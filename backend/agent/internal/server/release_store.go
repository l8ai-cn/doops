package server

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

type ReleaseStatus string

const (
	ReleaseStatusQueued     ReleaseStatus = "queued"
	ReleaseStatusRunning    ReleaseStatus = "running"
	ReleaseStatusCompleted  ReleaseStatus = "completed"
	ReleaseStatusFailed     ReleaseStatus = "failed"
	ReleaseStatusSuperseded ReleaseStatus = "superseded"
)

type ReleaseEventType string

const (
	ReleaseEventCreated    ReleaseEventType = "created"
	ReleaseEventClaimed    ReleaseEventType = "claimed"
	ReleaseEventProgress   ReleaseEventType = "progress"
	ReleaseEventCompleted  ReleaseEventType = "completed"
	ReleaseEventFailed     ReleaseEventType = "failed"
	ReleaseEventSuperseded ReleaseEventType = "superseded"
	ReleaseEventRecovered  ReleaseEventType = "recovered"
)

type CreateReleaseTicketRequest struct {
	UserID               string
	TokenID              string
	SessionID            string
	Requirement          string
	Cluster              string
	Instance             string
	Scope                string
	Application          string
	Environment          string
	Namespace            string
	ReleaseName          string
	PlanDigest           string
	SourceRevision       string
	WorkspaceCommit      string
	ExecutionMode        string
	Instruction          string
	RequiredEvidenceJSON string
}

type ReleaseTicket struct {
	Number               int64          `json:"number"`
	UserID               string         `json:"user_id"`
	TokenID              string         `json:"-"`
	SessionID            string         `json:"session_id"`
	Requirement          string         `json:"requirement"`
	Cluster              string         `json:"cluster"`
	Instance             string         `json:"instance"`
	Scope                string         `json:"scope"`
	Application          string         `json:"application"`
	Environment          string         `json:"environment"`
	Namespace            string         `json:"namespace"`
	ReleaseName          string         `json:"release_name"`
	PlanDigest           string         `json:"plan_digest"`
	SourceRevision       string         `json:"source_revision,omitempty"`
	WorkspaceCommit      string         `json:"workspace_commit"`
	ExecutionMode        string         `json:"execution_mode"`
	Instruction          string         `json:"-"`
	RequiredEvidenceJSON string         `json:"required_evidence_json,omitempty"`
	Status               ReleaseStatus  `json:"status"`
	Stage                string         `json:"stage,omitempty"`
	SupersededBy         *int64         `json:"superseded_by,omitempty"`
	ResultJSON           string         `json:"result,omitempty"`
	Error                string         `json:"error,omitempty"`
	CreatedAt            time.Time      `json:"created_at"`
	UpdatedAt            time.Time      `json:"updated_at"`
	StartedAt            *time.Time     `json:"started_at,omitempty"`
	FinishedAt           *time.Time     `json:"finished_at,omitempty"`
	Events               []ReleaseEvent `json:"events,omitempty"`
}

type ReleaseEvent struct {
	ID            int64            `json:"id"`
	ReleaseNumber int64            `json:"release_number"`
	Type          ReleaseEventType `json:"type"`
	Status        ReleaseStatus    `json:"status"`
	Stage         string           `json:"stage,omitempty"`
	Message       string           `json:"message,omitempty"`
	DataJSON      string           `json:"data,omitempty"`
	CreatedAt     time.Time        `json:"created_at"`
}

func (s *GatewayStore) CreateReleaseTicket(req CreateReleaseTicketRequest) (ReleaseTicket, error) {
	req = normalizeReleaseTicketRequest(req)
	if req.Requirement == "" {
		return ReleaseTicket{}, fmt.Errorf("release requirement is required")
	}
	if req.Scope == "" {
		return ReleaseTicket{}, fmt.Errorf("release scope is required")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return ReleaseTicket{}, err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	nowText := formatTime(now)

	res, err := tx.Exec(`INSERT INTO release_tickets
		(user_id, token_id, session_id, requirement, cluster, instance, scope, application,
		 environment, namespace, release_name, plan_digest, source_revision, workspace_commit, execution_mode,
		 instruction, required_evidence_json, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		req.UserID, req.TokenID, req.SessionID, req.Requirement, req.Cluster, req.Instance, req.Scope,
		req.Application, req.Environment, req.Namespace, req.ReleaseName, req.PlanDigest,
		req.SourceRevision, req.WorkspaceCommit, req.ExecutionMode, req.Instruction, req.RequiredEvidenceJSON,
		string(ReleaseStatusQueued), nowText, nowText)
	if err != nil {
		return ReleaseTicket{}, err
	}
	number, err := res.LastInsertId()
	if err != nil {
		return ReleaseTicket{}, err
	}

	oldRows, err := tx.Query(`SELECT number FROM release_tickets
		WHERE scope = ? AND status = ? AND number <> ?
		ORDER BY number`, req.Scope, string(ReleaseStatusQueued), number)
	if err != nil {
		return ReleaseTicket{}, err
	}
	var oldNumbers []int64
	for oldRows.Next() {
		var oldNumber int64
		if err := oldRows.Scan(&oldNumber); err != nil {
			oldRows.Close()
			return ReleaseTicket{}, err
		}
		oldNumbers = append(oldNumbers, oldNumber)
	}
	if err := oldRows.Close(); err != nil {
		return ReleaseTicket{}, err
	}
	for _, oldNumber := range oldNumbers {
		res, err := tx.Exec(`UPDATE release_tickets
			SET status = ?, stage = ?, superseded_by = ?, updated_at = ?, finished_at = ?
			WHERE number = ? AND status = ?`,
			string(ReleaseStatusSuperseded), "superseded", number, nowText, nowText,
			oldNumber, string(ReleaseStatusQueued))
		if err != nil {
			return ReleaseTicket{}, err
		}
		if affected, _ := res.RowsAffected(); affected == 0 {
			return ReleaseTicket{}, sql.ErrNoRows
		}
		if err := insertReleaseEvent(tx, oldNumber, ReleaseEventSuperseded, ReleaseStatusSuperseded,
			"superseded", fmt.Sprintf("superseded by release %d", number), "", now); err != nil {
			return ReleaseTicket{}, err
		}
	}
	if err := insertReleaseEvent(tx, number, ReleaseEventCreated, ReleaseStatusQueued,
		"", "release ticket queued", "", now); err != nil {
		return ReleaseTicket{}, err
	}
	if err := tx.Commit(); err != nil {
		return ReleaseTicket{}, err
	}
	return s.GetReleaseTicket(number)
}

func (s *GatewayStore) GetReleaseTicket(number int64) (ReleaseTicket, error) {
	row := s.db.QueryRow(releaseTicketSelect()+` WHERE number = ?`, number)
	ticket, err := scanReleaseTicket(row)
	if err != nil {
		return ReleaseTicket{}, err
	}
	ticket.Events, err = s.listReleaseEvents(number)
	if err != nil {
		return ReleaseTicket{}, err
	}
	return ticket, nil
}

func (s *GatewayStore) ListQueuedReleaseTickets(limit int) ([]ReleaseTicket, error) {
	query := releaseTicketSelect() + ` WHERE status = ? ORDER BY number`
	args := []any{string(ReleaseStatusQueued)}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tickets []ReleaseTicket
	for rows.Next() {
		ticket, err := scanReleaseTicket(rows)
		if err != nil {
			return nil, err
		}
		tickets = append(tickets, ticket)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range tickets {
		events, err := s.listReleaseEvents(tickets[i].Number)
		if err != nil {
			return nil, err
		}
		tickets[i].Events = events
	}
	return tickets, nil
}

func (s *GatewayStore) listQueuedReleaseCandidates(afterNumber int64, limit int) ([]ReleaseTicket, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("release candidate page size must be positive")
	}
	rows, err := s.db.Query(releaseTicketSelect()+`
		WHERE status = ? AND number > ?
		ORDER BY number
		LIMIT ?`,
		string(ReleaseStatusQueued), afterNumber, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tickets []ReleaseTicket
	for rows.Next() {
		ticket, err := scanReleaseTicket(rows)
		if err != nil {
			return nil, err
		}
		tickets = append(tickets, ticket)
	}
	return tickets, rows.Err()
}

func (s *GatewayStore) ClaimReleaseTicket(number int64) (ReleaseTicket, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return ReleaseTicket{}, err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	nowText := formatTime(now)
	res, err := tx.Exec(`UPDATE release_tickets
		SET status = ?, updated_at = ?, started_at = ?
		WHERE number = ? AND status = ?
		AND NOT EXISTS (
			SELECT 1 FROM release_tickets
			WHERE status = ?
		)`,
		string(ReleaseStatusRunning), nowText, nowText,
		number, string(ReleaseStatusQueued), string(ReleaseStatusRunning))
	if err != nil {
		return ReleaseTicket{}, err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return ReleaseTicket{}, sql.ErrNoRows
	}
	if err := insertReleaseEvent(tx, number, ReleaseEventClaimed, ReleaseStatusRunning, "",
		"release ticket claimed", "", now); err != nil {
		return ReleaseTicket{}, err
	}
	if err := tx.Commit(); err != nil {
		return ReleaseTicket{}, err
	}
	return s.GetReleaseTicket(number)
}

func (s *GatewayStore) UpdateReleaseProgress(number int64, stage, message string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	res, err := tx.Exec(`UPDATE release_tickets
		SET stage = ?, updated_at = ?
		WHERE number = ? AND status = ?`,
		strings.TrimSpace(stage), formatTime(now), number, string(ReleaseStatusRunning))
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return sql.ErrNoRows
	}
	if err := insertReleaseEvent(tx, number, ReleaseEventProgress, ReleaseStatusRunning, strings.TrimSpace(stage),
		strings.TrimSpace(message), "", now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *GatewayStore) CompleteReleaseTicket(number int64, resultJSON string) (ReleaseTicket, error) {
	if err := s.transitionRelease(number, ReleaseStatusRunning, ReleaseStatusCompleted, "completed", strings.TrimSpace(resultJSON), "", ReleaseEventCompleted, "release ticket completed"); err != nil {
		return ReleaseTicket{}, err
	}
	return s.GetReleaseTicket(number)
}

func (s *GatewayStore) FailReleaseTicket(number int64, errorMessage, resultJSON string) (ReleaseTicket, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return ReleaseTicket{}, err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	res, err := tx.Exec(`UPDATE release_tickets
		SET status = ?, stage = ?, result_json = ?, error = ?, updated_at = ?, finished_at = ?
		WHERE number = ? AND status IN (?, ?)`,
		string(ReleaseStatusFailed), "failed", strings.TrimSpace(resultJSON), strings.TrimSpace(errorMessage),
		formatTime(now), formatTime(now), number, string(ReleaseStatusQueued), string(ReleaseStatusRunning))
	if err != nil {
		return ReleaseTicket{}, err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return ReleaseTicket{}, sql.ErrNoRows
	}
	if err := insertReleaseEvent(tx, number, ReleaseEventFailed, ReleaseStatusFailed, "",
		strings.TrimSpace(errorMessage), strings.TrimSpace(resultJSON), now); err != nil {
		return ReleaseTicket{}, err
	}
	if err := tx.Commit(); err != nil {
		return ReleaseTicket{}, err
	}
	return s.GetReleaseTicket(number)
}

func (s *GatewayStore) RecoverInterruptedReleases() (int64, error) {
	const outcomeUnknown = `{"outcome":"unknown"}`
	const message = "interrupted release recovered with outcome unknown"
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	nowText := formatTime(now)
	rows, err := tx.Query(`SELECT number FROM release_tickets WHERE status = ? ORDER BY number`, string(ReleaseStatusRunning))
	if err != nil {
		return 0, err
	}
	var numbers []int64
	for rows.Next() {
		var number int64
		if err := rows.Scan(&number); err != nil {
			rows.Close()
			return 0, err
		}
		numbers = append(numbers, number)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	for _, number := range numbers {
		res, err := tx.Exec(`UPDATE release_tickets
			SET status = ?, stage = ?, result_json = ?, error = ?, updated_at = ?, finished_at = ?
			WHERE number = ? AND status = ?`,
			string(ReleaseStatusFailed), "recovered", outcomeUnknown, message, nowText, nowText,
			number, string(ReleaseStatusRunning))
		if err != nil {
			return 0, err
		}
		if affected, _ := res.RowsAffected(); affected == 0 {
			return 0, sql.ErrNoRows
		}
		if err := insertReleaseEvent(tx, number, ReleaseEventRecovered, ReleaseStatusFailed,
			"recovered", message, outcomeUnknown, now); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int64(len(numbers)), nil
}

func (s *GatewayStore) transitionRelease(number int64, from, to ReleaseStatus, stage, resultJSON, errorMessage string, eventType ReleaseEventType, message string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	nowText := formatTime(now)
	query := `UPDATE release_tickets SET status = ?, updated_at = ?`
	args := []any{string(to), nowText}
	if to == ReleaseStatusRunning {
		query += `, started_at = ?`
		args = append(args, nowText)
	}
	if to == ReleaseStatusCompleted || to == ReleaseStatusFailed || to == ReleaseStatusSuperseded {
		query += `, finished_at = ?`
		args = append(args, nowText)
	}
	if strings.TrimSpace(stage) != "" {
		query += `, stage = ?`
		args = append(args, strings.TrimSpace(stage))
	}
	if strings.TrimSpace(resultJSON) != "" {
		query += `, result_json = ?`
		args = append(args, strings.TrimSpace(resultJSON))
	}
	if strings.TrimSpace(errorMessage) != "" {
		query += `, error = ?`
		args = append(args, strings.TrimSpace(errorMessage))
	}
	query += ` WHERE number = ? AND status = ?`
	args = append(args, number, string(from))
	res, err := tx.Exec(query, args...)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return sql.ErrNoRows
	}
	if err := insertReleaseEvent(tx, number, eventType, to, strings.TrimSpace(stage),
		strings.TrimSpace(message), strings.TrimSpace(resultJSON), now); err != nil {
		return err
	}
	return tx.Commit()
}

type releaseExec interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func insertReleaseEvent(exec releaseExec, number int64, eventType ReleaseEventType, status ReleaseStatus, stage, message, dataJSON string, at time.Time) error {
	_, err := exec.Exec(`INSERT INTO release_events
		(release_number, type, status, stage, message, data_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		number, string(eventType), string(status), strings.TrimSpace(stage),
		strings.TrimSpace(message), strings.TrimSpace(dataJSON), formatTime(at.UTC()))
	return err
}

func (s *GatewayStore) listReleaseEvents(number int64) ([]ReleaseEvent, error) {
	rows, err := s.db.Query(`SELECT id, release_number, type, status, stage, message, data_json, created_at
		FROM release_events WHERE release_number = ? ORDER BY id`, number)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []ReleaseEvent
	for rows.Next() {
		var event ReleaseEvent
		var eventType, status, created string
		if err := rows.Scan(&event.ID, &event.ReleaseNumber, &eventType, &status, &event.Stage,
			&event.Message, &event.DataJSON, &created); err != nil {
			return nil, err
		}
		event.Type = ReleaseEventType(eventType)
		event.Status = ReleaseStatus(status)
		parsed, err := parseTime(created)
		if err != nil {
			return nil, fmt.Errorf("parse release event %d created_at: %w", event.ID, err)
		}
		event.CreatedAt = parsed
		events = append(events, event)
	}
	return events, rows.Err()
}

func releaseTicketSelect() string {
	return `SELECT number, user_id, token_id, session_id, requirement, cluster, instance, scope,
		application, environment, namespace, release_name, plan_digest, workspace_commit,
		source_revision, execution_mode, instruction, required_evidence_json, status, stage, superseded_by, result_json,
		error, created_at, updated_at, started_at, finished_at FROM release_tickets`
}

type releaseTicketScanner interface {
	Scan(dest ...any) error
}

func scanReleaseTicket(scanner releaseTicketScanner) (ReleaseTicket, error) {
	var ticket ReleaseTicket
	var status, created, updated, started, finished string
	var supersededBy sql.NullInt64
	if err := scanner.Scan(&ticket.Number, &ticket.UserID, &ticket.TokenID, &ticket.SessionID,
		&ticket.Requirement, &ticket.Cluster, &ticket.Instance, &ticket.Scope, &ticket.Application,
		&ticket.Environment, &ticket.Namespace, &ticket.ReleaseName, &ticket.PlanDigest,
		&ticket.WorkspaceCommit, &ticket.SourceRevision, &ticket.ExecutionMode, &ticket.Instruction,
		&ticket.RequiredEvidenceJSON, &status,
		&ticket.Stage, &supersededBy, &ticket.ResultJSON, &ticket.Error, &created, &updated,
		&started, &finished); err != nil {
		return ReleaseTicket{}, err
	}
	ticket.Status = ReleaseStatus(status)
	if supersededBy.Valid {
		ticket.SupersededBy = &supersededBy.Int64
	}
	var err error
	ticket.CreatedAt, err = parseTime(created)
	if err != nil {
		return ReleaseTicket{}, fmt.Errorf("parse release %d created_at: %w", ticket.Number, err)
	}
	ticket.UpdatedAt, err = parseTime(updated)
	if err != nil {
		return ReleaseTicket{}, fmt.Errorf("parse release %d updated_at: %w", ticket.Number, err)
	}
	if started != "" {
		parsed, err := parseTime(started)
		if err != nil {
			return ReleaseTicket{}, fmt.Errorf("parse release %d started_at: %w", ticket.Number, err)
		}
		ticket.StartedAt = &parsed
	}
	if finished != "" {
		parsed, err := parseTime(finished)
		if err != nil {
			return ReleaseTicket{}, fmt.Errorf("parse release %d finished_at: %w", ticket.Number, err)
		}
		ticket.FinishedAt = &parsed
	}
	return ticket, nil
}

func normalizeReleaseTicketRequest(req CreateReleaseTicketRequest) CreateReleaseTicketRequest {
	req.UserID = strings.TrimSpace(req.UserID)
	req.TokenID = strings.TrimSpace(req.TokenID)
	req.SessionID = strings.TrimSpace(req.SessionID)
	req.Requirement = strings.TrimSpace(req.Requirement)
	req.Cluster = strings.TrimSpace(req.Cluster)
	req.Instance = strings.TrimSpace(req.Instance)
	req.Scope = strings.TrimSpace(req.Scope)
	req.Application = strings.TrimSpace(req.Application)
	req.Environment = strings.TrimSpace(req.Environment)
	req.Namespace = strings.TrimSpace(req.Namespace)
	req.ReleaseName = strings.TrimSpace(req.ReleaseName)
	req.PlanDigest = strings.TrimSpace(req.PlanDigest)
	req.SourceRevision = strings.TrimSpace(req.SourceRevision)
	req.WorkspaceCommit = strings.TrimSpace(req.WorkspaceCommit)
	req.ExecutionMode = strings.TrimSpace(req.ExecutionMode)
	req.Instruction = strings.TrimSpace(req.Instruction)
	req.RequiredEvidenceJSON = strings.TrimSpace(req.RequiredEvidenceJSON)
	return req
}
