package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/user/doops/agent/api"
)

type doSQLDirectReport struct {
	OperationID          string            `json:"operation_id"`
	Mode                 string            `json:"mode"`
	Engine               string            `json:"engine"`
	Database             string            `json:"database"`
	Credential           string            `json:"credential"`
	StatementFingerprint string            `json:"statement_fingerprint"`
	Version              int64             `json:"version,omitempty"`
	VersionRecordError   string            `json:"version_record_error,omitempty"`
	JournalPath          string            `json:"journal_path,omitempty"`
	EvidencePath         string            `json:"evidence_path,omitempty"`
	Result               doSQLDirectResult `json:"result"`
}

func doSQLVersionSuffix(report doSQLDirectReport) string {
	if report.Version == 0 {
		return ""
	}
	suffix := fmt.Sprintf(" version=%d", report.Version)
	if report.VersionRecordError != "" {
		suffix += " version_record_error=" + report.VersionRecordError
	}
	return suffix
}

func doSQLDirectRequested(args api.AgentPromptParams) bool {
	return strings.TrimSpace(args.Skill) == "dosql" && strings.TrimSpace(args.SQL) != ""
}

func handleDirectDoSQLPrompt(ctx context.Context, sessionID string, args api.AgentPromptParams) (string, error) {
	candidate, err := resolveDirectDoSQLBinding(ctx, args.CredentialBindings, args.SQLDatabase, readDoSQLSecretData)
	if err != nil {
		return "", err
	}
	binding, data, engine, database := candidate.Binding, candidate.Data, candidate.Engine, candidate.Database
	target, err := doSQLConnectionTarget(engine, database, data)
	if err != nil {
		return "", err
	}
	request, err := planDirectDoSQL(engine, args)
	if err != nil {
		return "", err
	}
	request.Target = target
	report := doSQLDirectReport{
		OperationID: "dbop_" + randomHex(8), Mode: directDoSQLMode(request), Engine: engine,
		Database: database, Credential: binding.Name,
		StatementFingerprint: "sha256:" + doSQLStatementFingerprint(request.Statements),
	}
	// A real apply takes its version before touching the database; if the
	// journal cannot record it, nothing runs. Queries and dry-runs are not versions.
	var journalPath string
	if !request.ReadOnly && !request.DryRun {
		journalPath, err = doSQLVersionJournalPath(target)
		if err != nil {
			return "", fmt.Errorf("%w (operation_id=%s): nothing was executed", err, report.OperationID)
		}
		version, err := beginDoSQLVersion(journalPath, report.OperationID, binding.Name, report.StatementFingerprint, request.Statements)
		if err != nil {
			return "", fmt.Errorf("%w (operation_id=%s): nothing was executed", err, report.OperationID)
		}
		report.Version = version
		report.JournalPath = journalPath
	}
	result, execErr := executeDoSQLDirect(ctx, request)
	report.Result = result
	if report.Version != 0 {
		if err := endDoSQLVersion(journalPath, report.Version, report.OperationID, binding.Name, report.StatementFingerprint, result, execErr); err != nil {
			// The change already ran; say the record is missing instead of hiding it.
			report.VersionRecordError = err.Error()
		}
	}
	report.EvidencePath = writeDirectDoSQLEvidence(sessionID, report, execErr)
	if execErr != nil {
		if ctx.Err() != nil && result.ImplicitCommits > 0 {
			return "", fmt.Errorf("%w: %s (operation_id=%s)", errExecutionOutcomeUnknown, strings.TrimSpace(partialCommitNote(&result)), report.OperationID)
		}
		return "", fmt.Errorf("%w (operation_id=%s%s)", execErr, report.OperationID, doSQLVersionSuffix(report))
	}
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return "", errors.New("dosql_result_encode_failed")
	}
	return string(encoded), nil
}

// planDirectDoSQL turns the verbatim SQL text into an executable request; a
// query prompt only accepts read statements so ask authority never mutates.
func planDirectDoSQL(engine string, args api.AgentPromptParams) (doSQLDirectRequest, error) {
	apply := strings.ToLower(strings.TrimSpace(args.Operation)) == "apply"
	statements, err := splitDirectStatements(engine, args.SQL)
	if err != nil {
		return doSQLDirectRequest{}, err
	}
	request := doSQLDirectRequest{Statements: statements, ReadOnly: !apply, DryRun: apply && args.SQLDryRun}
	if !apply {
		if args.SQLDryRun || strings.TrimSpace(args.SQLVerification) != "" {
			return doSQLDirectRequest{}, errors.New("dosql_query_options_invalid: --verify and --dry-run require --mode apply")
		}
		for index, statement := range statements {
			if !directReadOnlyStatement(engine, statement) {
				return doSQLDirectRequest{}, fmt.Errorf("dosql_query_requires_read_only: statement %d is not a read statement; use --mode apply", index+1)
			}
		}
		return request, nil
	}
	if strings.TrimSpace(args.SQLVerification) != "" {
		verification, err := splitDirectStatements(engine, args.SQLVerification)
		if err != nil {
			return doSQLDirectRequest{}, err
		}
		if len(verification) != 1 || !directReadOnlyStatement(engine, verification[0]) {
			return doSQLDirectRequest{}, errors.New("dosql_verification_invalid: verification must be exactly one read statement")
		}
		request.Verification = verification[0]
	}
	return request, nil
}

func splitDirectStatements(engine, text string) ([]string, error) {
	if engine == "mongodb" {
		return parseMongoStatements(text)
	}
	return splitDoSQLStatements(engine, text)
}

func directReadOnlyStatement(engine, statement string) bool {
	if engine == "mongodb" {
		return mongoOperationKind(statement) == "read"
	}
	if !doSQLReadOnlyStatement(statement) {
		return false
	}
	// A SELECT-like prefix alone does not establish that the statement has
	// no side effects. Reject ambiguous forms before any database work begins.
	if engine == "mysql" {
		words, err := doSQLMySQLStatementWords(statement)
		if err != nil || !doSQLMySQLDryRunSafe(words) {
			return false
		}
	}
	code := statement
	if engine == "postgres" || engine == "mysql" {
		var unquoted strings.Builder
		if _, err := scanDoSQLStatements(engine, statement, &unquoted); err != nil {
			return false
		}
		code = unquoted.String()
	}
	words := directReadOnlyTokens.FindAllString(strings.ToUpper(code), -1)
	for _, word := range words {
		switch word {
		case "INSERT", "UPDATE", "DELETE", "MERGE", "REPLACE", "UPSERT", "INTO", "NEXTVAL", "SETVAL":
			return false
		}
	}
	return true
}

var directReadOnlyTokens = regexp.MustCompile(`[A-Z_][A-Z0-9_]*`)

func directDoSQLMode(request doSQLDirectRequest) string {
	switch {
	case request.ReadOnly:
		return "query"
	case request.DryRun:
		return "dry_run"
	default:
		return "apply"
	}
}

type doSQLBindingCandidate struct {
	Binding  api.AgentCredentialBinding
	Data     map[string]string
	Engine   string
	Database string
}

type doSQLSecretReader func(context.Context, api.AgentCredentialBinding) (map[string]string, error)

// resolveDirectDoSQLBinding picks the target's database binding without a
// caller-chosen credential: one binding is used as is, several are told apart
// by the database each credential points to. Errors carry names only.
func resolveDirectDoSQLBinding(ctx context.Context, bindings []api.AgentCredentialBinding, database string, read doSQLSecretReader) (doSQLBindingCandidate, error) {
	var databaseBindings []api.AgentCredentialBinding
	for _, binding := range bindings {
		if binding.Use == string(CredentialUseDatabase) {
			databaseBindings = append(databaseBindings, binding)
		}
	}
	if len(databaseBindings) == 0 {
		return doSQLBindingCandidate{}, errors.New("dosql_credential_missing: run credential configure --use database for this target")
	}
	database = strings.TrimSpace(database)
	var candidates []doSQLBindingCandidate
	var unusable []string
	for _, binding := range databaseBindings {
		candidate, err := loadDoSQLBindingCandidate(ctx, binding, read)
		if err != nil {
			if len(databaseBindings) == 1 {
				return doSQLBindingCandidate{}, err
			}
			// Keep the reason: one broken binding must not hide behind a generic miss.
			unusable = append(unusable, binding.Name+"("+doSQLErrorCategory(err)+")")
			continue
		}
		candidates = append(candidates, candidate)
	}
	if database == "" {
		if len(candidates) == 1 && len(unusable) == 0 {
			return candidates[0], nil
		}
		return doSQLBindingCandidate{}, fmt.Errorf("dosql_database_required: target binds %s (name=engine/database); pass --database <database>",
			describeDoSQLCandidates(candidates, unusable))
	}
	var matches []doSQLBindingCandidate
	for _, candidate := range candidates {
		if candidate.Database == database {
			matches = append(matches, candidate)
		}
	}
	if len(matches) == 0 {
		// Users copy the credential/binding name shown by "credential list".
		// Accept it as an alias for the database name when it is unique, so a
		// valid listed binding does not fail as if it were unbound.
		for _, candidate := range candidates {
			if candidate.Binding.Name == database || candidate.Binding.ResourceName == database {
				matches = append(matches, candidate)
			}
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return doSQLBindingCandidate{}, fmt.Errorf("dosql_database_not_bound: no database credential on this target points to %q; target binds %s (name=engine/database); pass --database <database>",
			database, describeDoSQLCandidates(candidates, unusable))
	}
	names := make([]string, 0, len(matches))
	for _, candidate := range matches {
		names = append(names, candidate.Binding.Name)
	}
	sort.Strings(names)
	return doSQLBindingCandidate{}, fmt.Errorf("dosql_credential_ambiguous: credentials %s all point to %q; revoke the duplicate binding",
		strings.Join(names, ","), database)
}

func loadDoSQLBindingCandidate(ctx context.Context, binding api.AgentCredentialBinding, read doSQLSecretReader) (doSQLBindingCandidate, error) {
	data, err := read(ctx, binding)
	if err != nil {
		return doSQLBindingCandidate{}, err
	}
	engine, database, err := inferDoSQLEngine(data)
	if err != nil {
		return doSQLBindingCandidate{}, err
	}
	return doSQLBindingCandidate{Binding: binding, Data: data, Engine: engine, Database: database}, nil
}

func describeDoSQLCandidates(candidates []doSQLBindingCandidate, unusable []string) string {
	parts := make([]string, 0, len(candidates)+len(unusable))
	for _, candidate := range candidates {
		parts = append(parts, candidate.Binding.Name+"="+candidate.Engine+"/"+candidate.Database)
	}
	sort.Strings(parts)
	sort.Strings(unusable)
	if len(unusable) != 0 {
		parts = append(parts, "unusable: "+strings.Join(unusable, ","))
	}
	return strings.Join(parts, ", ")
}

// doSQLErrorCategory keeps only the stable category prefix of an Agent error.
func doSQLErrorCategory(err error) string {
	category, _, _ := strings.Cut(err.Error(), ":")
	return strings.TrimSpace(category)
}

func inferDoSQLEngine(data map[string]string) (string, string, error) {
	if raw := strings.TrimSpace(data["SPRING_DATASOURCE_URL"]); raw != "" {
		parsed, err := url.Parse("mysql://" + strings.TrimPrefix(raw, "jdbc:mysql://"))
		if err != nil || !strings.HasPrefix(raw, "jdbc:mysql://") {
			return "", "", errors.New("dosql_connection_uri_invalid: SPRING_DATASOURCE_URL must be jdbc:mysql://host:port/database")
		}
		return "mysql", strings.Trim(parsed.Path, "/"), nil
	}
	parsed, err := url.Parse(strings.TrimSpace(data["connection_uri"]))
	if err != nil || parsed.Scheme == "" {
		return "", "", errors.New("dosql_connection_uri_invalid: credential needs connection_uri or SPRING_DATASOURCE_URL")
	}
	database := strings.Trim(parsed.Path, "/")
	if engine, ok := doSQLEngineForScheme(parsed.Scheme); ok {
		return engine, database, nil
	}
	return "", "", fmt.Errorf("dosql_engine_unsupported: %s is not supported for direct SQL", parsed.Scheme)
}

// Evidence is best effort: a full disk must not turn a committed change into an error.
func writeDirectDoSQLEvidence(sessionID string, report doSQLDirectReport, execErr error) string {
	workspace, err := workspacePath(sessionID)
	if err != nil {
		return ""
	}
	directory := filepath.Join(workspace, ".dosql", "operations")
	if os.MkdirAll(directory, 0700) != nil {
		return ""
	}
	evidence := map[string]interface{}{
		"schema": "dosql.direct-evidence.v1", "recorded_at": time.Now().UTC().Format(time.RFC3339Nano), "report": report,
	}
	if execErr != nil {
		evidence["error"] = sanitizeSecretText(execErr.Error())
	}
	encoded, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		return ""
	}
	path := filepath.Join(directory, report.OperationID+".evidence.json")
	if os.WriteFile(path, encoded, 0600) != nil {
		return ""
	}
	return path
}
