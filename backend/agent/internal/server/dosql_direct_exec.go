package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgconn/ctxwatch"
	"github.com/jackc/pgx/v5/stdlib"
	_ "github.com/microsoft/go-mssqldb"
	_ "github.com/sijms/go-ora/v2"
)

const (
	doSQLReadCanceledCategory = "dosql_read_canceled"
	doSQLDirectRowLimit       = 200
	doSQLDirectLockWait       = 5 * time.Second
	doSQLDirectStatementT     = 60 * time.Second
)

var errDoSQLReadCanceled = errors.New(doSQLReadCanceledCategory + ": PostgreSQL confirmed query cancellation (SQLSTATE 57014)")

func openDoSQLDirectDB(request doSQLDirectRequest) (*sql.DB, error) {
	if !request.ReadOnly || request.Target.Driver != "pgx" {
		return sql.Open(request.Target.Driver, request.Target.DSN)
	}
	config, err := pgx.ParseConfig(request.Target.DSN)
	if err != nil {
		return nil, err
	}
	// Let the original connection receive the server error after CancelRequest.
	// Bound the wait even if that connection or the cancel channel is lost.
	config.BuildContextWatcherHandler = func(conn *pgconn.PgConn) ctxwatch.Handler {
		return &pgconn.CancelRequestContextWatcherHandler{Conn: conn, DeadlineDelay: 5 * time.Second}
	}
	return stdlib.OpenDB(*config), nil
}

type doSQLDirectRequest struct {
	Target       doSQLTarget
	Statements   []string
	Verification string
	ReadOnly     bool
	DryRun       bool
}

type doSQLStatementResult struct {
	Statement    string     `json:"statement"`
	AffectedRows int64      `json:"affected_rows,omitempty"`
	Columns      []string   `json:"columns,omitempty"`
	Rows         [][]string `json:"rows,omitempty"`
	Truncated    bool       `json:"truncated,omitempty"`
}

type doSQLDirectResult struct {
	Status    string `json:"status"`
	Committed bool   `json:"committed"`
	// ImplicitCommits counts statements the engine committed on its own (DDL on
	// MySQL/Oracle); those cannot be rolled back by this executor.
	ImplicitCommits int `json:"implicit_commits,omitempty"`
	// AuthSource is the MongoDB authentication database that accepted the login.
	AuthSource   string                 `json:"auth_source,omitempty"`
	Statements   []doSQLStatementResult `json:"statements"`
	Verification *doSQLStatementResult  `json:"verification,omitempty"`
	DurationMS   int64                  `json:"duration_ms"`
}

// executeDoSQLDirect runs the whole batch plus its verification in one
// transaction on one connection, so a failed verification rolls the batch back.
// MySQL and Oracle commit DDL implicitly; for them only DML is atomic.
func executeDoSQLDirect(ctx context.Context, request doSQLDirectRequest) (doSQLDirectResult, error) {
	started := time.Now()
	result := doSQLDirectResult{Status: "failed"}
	if len(request.Statements) == 0 {
		return result, errors.New("dosql_statement_required")
	}
	if request.Target.Engine == "mongodb" {
		return executeDoSQLMongo(ctx, request)
	}
	if request.Target.Driver == doSQLExternalDriver {
		return executeDoSQLExternal(ctx, request)
	}
	if request.Target.Engine == "mysql" {
		statements := append([]string(nil), request.Statements...)
		if strings.TrimSpace(request.Verification) != "" {
			statements = append(statements, request.Verification)
		}
		for index, statement := range statements {
			words, err := doSQLMySQLStatementWords(statement)
			if err != nil {
				result.Status = "rejected"
				return result, fmt.Errorf("%w: statement %d", err, index+1)
			}
			if request.DryRun && !doSQLMySQLDryRunSafe(words) && !doSQLCommitsImplicitly("mysql", statement) {
				result.Status = "rejected"
				return result, fmt.Errorf("dosql_dry_run_statement_unsupported: statement %d cannot be guaranteed to roll back", index+1)
			}
			if request.DryRun && index == len(request.Statements) && doSQLCommitsImplicitly("mysql", statement) {
				result.Status = "rejected"
				return result, errors.New("dosql_verification_invalid: verification cannot commit the transaction")
			}
		}
	}
	if request.DryRun {
		// Checked before connecting: running the DDL is what would leak it.
		for index, statement := range request.Statements {
			if doSQLCommitsImplicitly(request.Target.Engine, statement) {
				result.Status = "rejected"
				return result, fmt.Errorf("dosql_dry_run_ddl_unsupported: statement %d is DDL and %s commits DDL implicitly, so a dry run cannot roll it back; keep the authorized target/database and execute DDL separately without --dry-run only within the authorized change scope", index+1, request.Target.Engine)
			}
		}
	}
	if !request.ReadOnly {
		// After an implicit commit the session is back in autocommit, so DML that
		// follows DDL would persist even if a later statement fails. Refuse the
		// mix before running anything; DDL-only and DML-only batches stay allowed.
		firstDDL, firstDML := 0, 0
		for index, statement := range request.Statements {
			switch {
			case doSQLCommitsImplicitly(request.Target.Engine, statement):
				if firstDDL == 0 {
					firstDDL = index + 1
				}
			case !doSQLReadOnlyStatement(statement):
				if firstDML == 0 {
					firstDML = index + 1
				}
			}
		}
		if firstDDL != 0 && firstDML != 0 {
			result.Status = "rejected"
			return result, fmt.Errorf("dosql_mixed_ddl_dml_unsupported: statement %d is DDL and statement %d is DML; %s commits DDL implicitly so the batch cannot be atomic, apply them as separate operations", firstDDL, firstDML, request.Target.Engine)
		}
	}
	db, err := openDoSQLDirectDB(request)
	if err != nil {
		return result, doSQLFailure("dosql_connect_failed", err.Error())
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		return result, doSQLFailure("dosql_connect_failed", err.Error())
	}
	defer conn.Close()
	if err := prepareDoSQLSession(ctx, conn, request.Target.Engine); err != nil {
		return result, doSQLFailure("dosql_session_setup_failed", err.Error())
	}
	tx, err := beginDoSQLTransaction(ctx, conn, request.Target.Engine, request.ReadOnly)
	if err != nil {
		return result, doSQLFailure("dosql_transaction_failed", err.Error())
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	for index, statement := range request.Statements {
		executed, err := runDoSQLStatement(ctx, tx, statement)
		if err != nil {
			var pgErr *pgconn.PgError
			if request.ReadOnly && request.Target.Driver == "pgx" && ctx.Err() == context.Canceled &&
				errors.As(err, &pgErr) && pgErr.Code == "57014" && pgErr.Routine == "ProcessInterrupts" &&
				pgErr.Message == "canceling statement due to user request" {
				result.Status = "canceled"
				return result, errDoSQLReadCanceled
			}
			// A DDL that failed may still have committed what ran before it.
			return result, doSQLFailure("dosql_statement_failed", fmt.Sprintf("statement %d: %v%s", index+1, err, partialCommitNote(&result)))
		}
		result.Statements = append(result.Statements, executed)
		if doSQLCommitsImplicitly(request.Target.Engine, statement) {
			result.ImplicitCommits++
			result.Committed = true
			result.Status = "partially_committed"
		}
	}
	if strings.TrimSpace(request.Verification) != "" {
		verified, err := runDoSQLStatement(ctx, tx, request.Verification)
		if err != nil {
			return result, doSQLFailure("dosql_verification_failed", err.Error()+partialCommitNote(&result))
		}
		result.Verification = &verified
	}
	result.DurationMS = time.Since(started).Milliseconds()
	if request.ReadOnly || request.DryRun {
		result.Status = "verified"
		if request.DryRun {
			result.Status = "dry_run_rolled_back"
		}
		return result, nil
	}
	if err := tx.Commit(); err != nil {
		return result, doSQLFailure("dosql_commit_failed", err.Error()+partialCommitNote(&result))
	}
	committed = true
	result.Committed = true
	result.Status = "verified"
	return result, nil
}

func partialCommitNote(result *doSQLDirectResult) string {
	if result.ImplicitCommits == 0 {
		return ""
	}
	return fmt.Sprintf(" (dosql_ddl_already_committed: %d DDL statement(s) committed implicitly and are not rolled back)", result.ImplicitCommits)
}

func prepareDoSQLSession(ctx context.Context, conn *sql.Conn, engine string) error {
	seconds := int(doSQLDirectLockWait / time.Second)
	var statements []string
	switch engine {
	case "postgres":
		statements = []string{
			fmt.Sprintf("SET lock_timeout = '%ds'", seconds),
			fmt.Sprintf("SET statement_timeout = '%ds'", int(doSQLDirectStatementT/time.Second)),
		}
	case "mysql":
		statements = []string{fmt.Sprintf("SET SESSION innodb_lock_wait_timeout = %d", seconds)}
	case "oracle":
		statements = []string{fmt.Sprintf("ALTER SESSION SET DDL_LOCK_TIMEOUT = %d", seconds)}
	case "mssql":
		statements = []string{fmt.Sprintf("SET LOCK_TIMEOUT %d", seconds*1000)}
	}
	for _, statement := range statements {
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

func beginDoSQLTransaction(ctx context.Context, conn *sql.Conn, engine string, readOnly bool) (*sql.Tx, error) {
	// go-ora rejects TxOptions.ReadOnly; Oracle gets the equivalent statement.
	if engine == "oracle" {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil || !readOnly {
			return tx, err
		}
		if _, err := tx.ExecContext(ctx, "SET TRANSACTION READ ONLY"); err != nil {
			_ = tx.Rollback()
			return nil, err
		}
		return tx, nil
	}
	return conn.BeginTx(ctx, doSQLTxOptions(engine, readOnly))
}

func runDoSQLStatement(ctx context.Context, tx *sql.Tx, statement string) (doSQLStatementResult, error) {
	executed := doSQLStatementResult{Statement: statement}
	if !doSQLReadOnlyStatement(statement) {
		outcome, err := tx.ExecContext(ctx, statement)
		if err != nil {
			return executed, err
		}
		executed.AffectedRows, _ = outcome.RowsAffected()
		return executed, nil
	}
	rows, err := tx.QueryContext(ctx, statement)
	if err != nil {
		return executed, err
	}
	defer rows.Close()
	if executed.Columns, err = rows.Columns(); err != nil {
		return executed, err
	}
	for rows.Next() {
		if len(executed.Rows) == doSQLDirectRowLimit {
			executed.Truncated = true
			break
		}
		values := make([]interface{}, len(executed.Columns))
		pointers := make([]interface{}, len(values))
		for index := range values {
			pointers[index] = &values[index]
		}
		if err := rows.Scan(pointers...); err != nil {
			return executed, err
		}
		row := make([]string, len(values))
		for index, value := range values {
			row[index] = doSQLCellText(value)
		}
		executed.Rows = append(executed.Rows, row)
	}
	return executed, rows.Err()
}

func doSQLCellText(value interface{}) string {
	switch typed := value.(type) {
	case nil:
		return "NULL"
	case []byte:
		return redactSecretText(string(typed))
	case time.Time:
		return typed.UTC().Format(time.RFC3339Nano)
	default:
		return redactSecretText(fmt.Sprint(typed))
	}
}
