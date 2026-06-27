package server

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"golang.org/x/crypto/bcrypt"
)

type GatewayAction string

const (
	ActionAll          GatewayAction = "*"
	ActionExec         GatewayAction = "exec"
	ActionAsk          GatewayAction = "ask"
	ActionRead         GatewayAction = "read"
	ActionWrite        GatewayAction = "write"
	ActionPush         GatewayAction = "push"
	ActionPull         GatewayAction = "pull"
	ActionInfo         GatewayAction = "info"
	ActionCheck        GatewayAction = "check"
	ActionClean        GatewayAction = "clean"
	ActionAgentUpgrade GatewayAction = "agent:upgrade"
	ActionTargetsList  GatewayAction = "targets:list"
	ActionAdmin        GatewayAction = "admin"
)

var defaultGatewayUserActions = []GatewayAction{
	ActionTargetsList,
	ActionExec,
	ActionAsk,
	ActionRead,
	ActionWrite,
	ActionPush,
	ActionPull,
	ActionInfo,
	ActionCheck,
	ActionClean,
	ActionAgentUpgrade,
}

type TokenKind string

const (
	TokenKindUser  TokenKind = "user"
	TokenKindAgent TokenKind = "agent"
)

var (
	ErrUnauthorized = errors.New("unauthorized")
	ErrForbidden    = errors.New("forbidden")
)

type GatewayStore struct {
	db *sql.DB
}

type GatewayUser struct {
	ID           string
	Name         string
	PasswordHash string
	CreatedAt    time.Time
}

type CreateUserRequest struct {
	Name     string
	Password string
}

type CreateTokenRequest struct {
	Kind      TokenKind
	UserID    string
	Name      string
	Cluster   string
	Instance  string
	ExpiresAt time.Time
}

type CreatedToken struct {
	ID        string
	Prefix    string
	Plaintext string
}

type TokenAuth struct {
	TokenID  string
	UserID   string
	Kind     TokenKind
	Cluster  string
	Instance string
}

type ScopeGrant struct {
	Cluster  string
	Instance string
	Actions  []GatewayAction
}

type AuditEvent struct {
	ID             int64
	UserID         string
	TokenID        string
	Cluster        string
	Instance       string
	Action         GatewayAction
	Session        string
	CommandSummary string
	Status         string
	Error          string
	Tail           string
	BytesIn        int64
	BytesOut       int64
	StartedAt      time.Time
	EndedAt        time.Time
}

type AgentStatus struct {
	Cluster     string
	Instance    string
	TokenID     string
	Remote      string
	Status      string
	ConnectedAt time.Time
	LastSeen    time.Time
	UpdatedAt   time.Time
}

type AuditFinish struct {
	Status   string
	Error    string
	Tail     string
	BytesIn  int64
	BytesOut int64
	EndedAt  time.Time
}

type AuditFilter struct {
	UserID   string
	Cluster  string
	Instance string
	Session  string
	Action   GatewayAction
	Status   string
	Limit    int
}

func OpenGatewayStore(path string) (*GatewayStore, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		path = "/var/lib/doops-gateway/gateway.db"
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	store := &GatewayStore{db: db}
	if err := store.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (s *GatewayStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *GatewayStore) migrate() error {
	stmts := []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA busy_timeout=5000`,
		`CREATE TABLE IF NOT EXISTS users (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL UNIQUE,
			password_hash TEXT NOT NULL DEFAULT '',
			disabled INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS tokens (
			id TEXT PRIMARY KEY,
			kind TEXT NOT NULL,
			user_id TEXT,
			name TEXT NOT NULL,
			prefix TEXT NOT NULL,
			hash TEXT NOT NULL,
			cluster TEXT NOT NULL DEFAULT '',
			instance TEXT NOT NULL DEFAULT '',
			expires_at TEXT NOT NULL DEFAULT '',
			revoked_at TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			FOREIGN KEY(user_id) REFERENCES users(id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_tokens_kind ON tokens(kind)`,
		`CREATE TABLE IF NOT EXISTS grants (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id TEXT NOT NULL,
			cluster TEXT NOT NULL,
			instance TEXT NOT NULL,
			actions_json TEXT NOT NULL,
			created_at TEXT NOT NULL,
			FOREIGN KEY(user_id) REFERENCES users(id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_grants_user ON grants(user_id)`,
		`CREATE TABLE IF NOT EXISTS agent_status (
			cluster TEXT NOT NULL,
			instance TEXT NOT NULL,
			token_id TEXT NOT NULL DEFAULT '',
			remote TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'offline',
			connected_at TEXT NOT NULL DEFAULT '',
			last_seen TEXT NOT NULL DEFAULT '',
			updated_at TEXT NOT NULL,
			PRIMARY KEY(cluster, instance)
		)`,
		`CREATE TABLE IF NOT EXISTS audit_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id TEXT NOT NULL DEFAULT '',
			token_id TEXT NOT NULL DEFAULT '',
			cluster TEXT NOT NULL DEFAULT '',
			instance TEXT NOT NULL DEFAULT '',
			action TEXT NOT NULL DEFAULT '',
			session TEXT NOT NULL DEFAULT '',
			command_summary TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'running',
			error TEXT NOT NULL DEFAULT '',
			tail TEXT NOT NULL DEFAULT '',
			bytes_in INTEGER NOT NULL DEFAULT 0,
			bytes_out INTEGER NOT NULL DEFAULT 0,
			started_at TEXT NOT NULL,
			ended_at TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS scheduler_jobs (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			cluster_glob TEXT NOT NULL DEFAULT '*',
			instance_glob TEXT NOT NULL DEFAULT '*',
			interval_sec INTEGER NOT NULL DEFAULT 3600,
			scan_mode TEXT NOT NULL DEFAULT 'ask',
			scan_config TEXT NOT NULL DEFAULT '{}',
			platform TEXT NOT NULL DEFAULT 'cnb',
			repo_slug TEXT NOT NULL DEFAULT '',
			labels TEXT NOT NULL DEFAULT '',
			token_env TEXT NOT NULL DEFAULT '',
			api_base TEXT NOT NULL DEFAULT '',
			dedup_window_sec INTEGER NOT NULL DEFAULT 86400,
			enabled INTEGER NOT NULL DEFAULT 1,
			last_run_at TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS scheduler_issues (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			job_id TEXT NOT NULL,
			fingerprint TEXT NOT NULL,
			repo_slug TEXT NOT NULL,
			cluster TEXT NOT NULL DEFAULT '',
			instance TEXT NOT NULL DEFAULT '',
			issue_url TEXT NOT NULL DEFAULT '',
			title TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'created',
			created_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_sched_issues_dedup ON scheduler_issues(repo_slug, fingerprint, created_at)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return err
		}
	}
	if err := s.ensureColumn("users", "password_hash", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	return nil
}

func (s *GatewayStore) CreateUser(name string) (GatewayUser, error) {
	return s.CreateUserWithPassword(CreateUserRequest{Name: name})
}

func (s *GatewayStore) CreateUserWithPassword(req CreateUserRequest) (GatewayUser, error) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return GatewayUser{}, fmt.Errorf("user name is required")
	}
	passwordHash := ""
	if strings.TrimSpace(req.Password) != "" {
		hash, err := hashPassword(req.Password)
		if err != nil {
			return GatewayUser{}, err
		}
		passwordHash = hash
	}
	user := GatewayUser{
		ID:           "usr_" + randomHex(12),
		Name:         name,
		PasswordHash: passwordHash,
		CreatedAt:    time.Now().UTC(),
	}
	_, err := s.db.Exec(`INSERT INTO users (id, name, password_hash, created_at) VALUES (?, ?, ?, ?)`,
		user.ID, user.Name, user.PasswordHash, formatTime(user.CreatedAt))
	if err != nil {
		return GatewayUser{}, err
	}
	return user, nil
}

func (s *GatewayStore) FindUserByName(name string) (GatewayUser, error) {
	var user GatewayUser
	var created string
	err := s.db.QueryRow(`SELECT id, name, password_hash, created_at FROM users WHERE name = ? AND disabled = 0`,
		strings.TrimSpace(name)).Scan(&user.ID, &user.Name, &user.PasswordHash, &created)
	if err != nil {
		return GatewayUser{}, err
	}
	user.CreatedAt, _ = parseTime(created)
	return user, nil
}

func (s *GatewayStore) SetUserPassword(userID, password string) error {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return fmt.Errorf("user id is required")
	}
	hash, err := hashPassword(password)
	if err != nil {
		return err
	}
	res, err := s.db.Exec(`UPDATE users SET password_hash = ? WHERE id = ? AND disabled = 0`, hash, userID)
	if err != nil {
		return err
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *GatewayStore) VerifyUserPassword(name, password string) (GatewayUser, error) {
	user, err := s.FindUserByName(name)
	if err != nil || user.PasswordHash == "" {
		return GatewayUser{}, ErrUnauthorized
	}
	if bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)) != nil {
		return GatewayUser{}, ErrUnauthorized
	}
	return user, nil
}

func (s *GatewayStore) CreateToken(req CreateTokenRequest) (CreatedToken, error) {
	if req.Kind != TokenKindUser && req.Kind != TokenKindAgent {
		return CreatedToken{}, fmt.Errorf("unsupported token kind %q", req.Kind)
	}
	if req.Kind == TokenKindUser && strings.TrimSpace(req.UserID) == "" {
		return CreatedToken{}, fmt.Errorf("user token requires user id")
	}
	if req.Kind == TokenKindAgent && (strings.TrimSpace(req.Cluster) == "" || strings.TrimSpace(req.Instance) == "") {
		return CreatedToken{}, fmt.Errorf("agent token requires cluster and instance")
	}
	id := "tok_" + randomHex(12)
	prefix := "dgw_" + string(req.Kind) + "_" + randomHex(4)
	secret := randomHex(32)
	plain := prefix + "_" + id + "_" + secret
	hash, err := bcrypt.GenerateFromPassword(tokenPassword(plain), bcrypt.DefaultCost)
	if err != nil {
		return CreatedToken{}, err
	}
	expires := ""
	if !req.ExpiresAt.IsZero() {
		expires = formatTime(req.ExpiresAt.UTC())
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = string(req.Kind) + " token"
	}
	_, err = s.db.Exec(`INSERT INTO tokens
		(id, kind, user_id, name, prefix, hash, cluster, instance, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, string(req.Kind), req.UserID, name, prefix, string(hash),
		strings.TrimSpace(req.Cluster), strings.TrimSpace(req.Instance), expires, formatTime(time.Now().UTC()))
	if err != nil {
		return CreatedToken{}, err
	}
	return CreatedToken{ID: id, Prefix: prefix, Plaintext: plain}, nil
}

func (s *GatewayStore) VerifyUserToken(token string) (TokenAuth, error) {
	auth, err := s.verifyToken(token, TokenKindUser)
	if err != nil {
		return TokenAuth{}, err
	}
	return auth, nil
}

func (s *GatewayStore) VerifyAgentToken(token string) (TokenAuth, error) {
	auth, err := s.verifyToken(token, TokenKindAgent)
	if err != nil {
		return TokenAuth{}, err
	}
	return auth, nil
}

func (s *GatewayStore) verifyToken(token string, kind TokenKind) (TokenAuth, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return TokenAuth{}, ErrUnauthorized
	}
	if tokenID := tokenIDFromPlaintext(token); tokenID != "" {
		return s.verifyTokenByID(token, kind, tokenID)
	}
	rows, err := s.db.Query(`SELECT id, user_id, hash, cluster, instance, expires_at
		FROM tokens WHERE kind = ? AND revoked_at = ''`, string(kind))
	if err != nil {
		return TokenAuth{}, err
	}
	defer rows.Close()
	now := time.Now().UTC()
	for rows.Next() {
		var id, userID, hash, cluster, instance, expires string
		if err := rows.Scan(&id, &userID, &hash, &cluster, &instance, &expires); err != nil {
			return TokenAuth{}, err
		}
		if expires != "" {
			exp, err := parseTime(expires)
			if err == nil && now.After(exp) {
				continue
			}
		}
		if bcrypt.CompareHashAndPassword([]byte(hash), tokenPassword(token)) == nil {
			return TokenAuth{TokenID: id, UserID: userID, Kind: kind, Cluster: cluster, Instance: instance}, nil
		}
	}
	if err := rows.Err(); err != nil {
		return TokenAuth{}, err
	}
	return TokenAuth{}, ErrUnauthorized
}

func (s *GatewayStore) verifyTokenByID(token string, kind TokenKind, tokenID string) (TokenAuth, error) {
	var id, userID, hash, cluster, instance, expires string
	err := s.db.QueryRow(`SELECT id, user_id, hash, cluster, instance, expires_at
		FROM tokens WHERE id = ? AND kind = ? AND revoked_at = ''`, tokenID, string(kind)).
		Scan(&id, &userID, &hash, &cluster, &instance, &expires)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return TokenAuth{}, ErrUnauthorized
		}
		return TokenAuth{}, err
	}
	if expires != "" {
		exp, err := parseTime(expires)
		if err == nil && time.Now().UTC().After(exp) {
			return TokenAuth{}, ErrUnauthorized
		}
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), tokenPassword(token)) != nil {
		return TokenAuth{}, ErrUnauthorized
	}
	return TokenAuth{TokenID: id, UserID: userID, Kind: kind, Cluster: cluster, Instance: instance}, nil
}

func tokenIDFromPlaintext(token string) string {
	parts := strings.Split(strings.TrimSpace(token), "_")
	if len(parts) < 6 || parts[0] != "dgw" || parts[3] != "tok" || parts[4] == "" {
		return ""
	}
	return "tok_" + parts[4]
}

func (s *GatewayStore) DeleteExpiredTokens(now time.Time) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM tokens WHERE expires_at != '' AND expires_at <= ?`, formatTime(now.UTC()))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *GatewayStore) GrantUser(userID string, grant ScopeGrant) error {
	if strings.TrimSpace(userID) == "" {
		return fmt.Errorf("user id is required")
	}
	if grant.Cluster == "" {
		grant.Cluster = "*"
	}
	if grant.Instance == "" {
		grant.Instance = "*"
	}
	if len(grant.Actions) == 0 {
		grant.Actions = append([]GatewayAction(nil), defaultGatewayUserActions...)
	}
	actions, err := json.Marshal(grant.Actions)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO grants (user_id, cluster, instance, actions_json, created_at)
		VALUES (?, ?, ?, ?, ?)`, userID, grant.Cluster, grant.Instance, string(actions), formatTime(time.Now().UTC()))
	return err
}

func (s *GatewayStore) UserCan(userID, cluster, instance string, action GatewayAction) bool {
	if userID == "" || action == "" {
		return false
	}
	rows, err := s.db.Query(`SELECT cluster, instance, actions_json FROM grants WHERE user_id = ?`, userID)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var clusterPattern, instancePattern, rawActions string
		if err := rows.Scan(&clusterPattern, &instancePattern, &rawActions); err != nil {
			return false
		}
		if !scopeMatches(clusterPattern, cluster) || !scopeMatches(instancePattern, instance) {
			continue
		}
		var actions []GatewayAction
		if json.Unmarshal([]byte(rawActions), &actions) != nil {
			continue
		}
		for _, a := range actions {
			if gatewayActionAllows(a, action) {
				return true
			}
		}
	}
	return false
}

func (s *GatewayStore) UserHasAction(userID string, action GatewayAction) bool {
	if userID == "" || action == "" {
		return false
	}
	rows, err := s.db.Query(`SELECT actions_json FROM grants WHERE user_id = ?`, userID)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var rawActions string
		if err := rows.Scan(&rawActions); err != nil {
			return false
		}
		var actions []GatewayAction
		if json.Unmarshal([]byte(rawActions), &actions) != nil {
			continue
		}
		for _, a := range actions {
			if gatewayActionAllows(a, action) {
				return true
			}
		}
	}
	return false
}

func gatewayActionAllows(granted, requested GatewayAction) bool {
	if granted == requested || granted == ActionAdmin || granted == ActionAll {
		return true
	}
	return false
}

func defaultGatewayUserActionAllows(action GatewayAction) bool {
	for _, allowed := range defaultGatewayUserActions {
		if allowed == action {
			return true
		}
	}
	return false
}

func (s *GatewayStore) MarkAgentOnline(agent AgentStatus) error {
	if strings.TrimSpace(agent.Cluster) == "" || strings.TrimSpace(agent.Instance) == "" {
		return fmt.Errorf("cluster and instance are required")
	}
	now := time.Now().UTC()
	if agent.ConnectedAt.IsZero() {
		agent.ConnectedAt = now
	}
	if agent.LastSeen.IsZero() {
		agent.LastSeen = now
	}
	_, err := s.db.Exec(`INSERT INTO agent_status
		(cluster, instance, token_id, remote, status, connected_at, last_seen, updated_at)
		VALUES (?, ?, ?, ?, 'online', ?, ?, ?)
		ON CONFLICT(cluster, instance) DO UPDATE SET
			token_id = excluded.token_id,
			remote = excluded.remote,
			status = 'online',
			connected_at = excluded.connected_at,
			last_seen = excluded.last_seen,
			updated_at = excluded.updated_at`,
		agent.Cluster, agent.Instance, agent.TokenID, agent.Remote,
		formatTime(agent.ConnectedAt.UTC()), formatTime(agent.LastSeen.UTC()), formatTime(now))
	return err
}

func (s *GatewayStore) MarkAgentOffline(cluster, instance string) error {
	now := time.Now().UTC()
	_, err := s.db.Exec(`UPDATE agent_status
		SET status = 'offline', updated_at = ?
		WHERE cluster = ? AND instance = ?`,
		formatTime(now), cluster, instance)
	return err
}

func (s *GatewayStore) TouchAgent(cluster, instance string, lastSeen time.Time) error {
	if lastSeen.IsZero() {
		lastSeen = time.Now().UTC()
	}
	_, err := s.db.Exec(`UPDATE agent_status
		SET last_seen = ?, updated_at = ?
		WHERE cluster = ? AND instance = ?`,
		formatTime(lastSeen.UTC()), formatTime(time.Now().UTC()), cluster, instance)
	return err
}

func (s *GatewayStore) ListAgentStatus() ([]AgentStatus, error) {
	rows, err := s.db.Query(`SELECT cluster, instance, token_id, remote, status, connected_at, last_seen, updated_at
		FROM agent_status ORDER BY cluster, instance`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var agents []AgentStatus
	for rows.Next() {
		var agent AgentStatus
		var connected, lastSeen, updated string
		if err := rows.Scan(&agent.Cluster, &agent.Instance, &agent.TokenID, &agent.Remote,
			&agent.Status, &connected, &lastSeen, &updated); err != nil {
			return nil, err
		}
		if connected != "" {
			agent.ConnectedAt, _ = parseTime(connected)
		}
		if lastSeen != "" {
			agent.LastSeen, _ = parseTime(lastSeen)
		}
		if updated != "" {
			agent.UpdatedAt, _ = parseTime(updated)
		}
		agents = append(agents, agent)
	}
	return agents, rows.Err()
}

func (s *GatewayStore) StartAudit(event AuditEvent) (int64, error) {
	if event.StartedAt.IsZero() {
		event.StartedAt = time.Now().UTC()
	}
	res, err := s.db.Exec(`INSERT INTO audit_events
		(user_id, token_id, cluster, instance, action, session, command_summary, status, started_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		event.UserID, event.TokenID, event.Cluster, event.Instance, string(event.Action),
		event.Session, event.CommandSummary, firstNonEmpty(event.Status, "running"), formatTime(event.StartedAt.UTC()))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *GatewayStore) FinishAudit(id int64, finish AuditFinish) error {
	if id == 0 {
		return nil
	}
	if finish.EndedAt.IsZero() {
		finish.EndedAt = time.Now().UTC()
	}
	if finish.Status == "" {
		if finish.Error != "" {
			finish.Status = "error"
		} else {
			finish.Status = "success"
		}
	}
	_, err := s.db.Exec(`UPDATE audit_events
		SET status = ?, error = ?, tail = ?, bytes_in = ?, bytes_out = ?, ended_at = ?
		WHERE id = ?`,
		finish.Status, finish.Error, trimTail(finish.Tail, 8192), finish.BytesIn, finish.BytesOut,
		formatTime(finish.EndedAt.UTC()), id)
	return err
}

func (s *GatewayStore) ListAudit(limit int) ([]AuditEvent, error) {
	return s.ListAuditFiltered(AuditFilter{Limit: limit})
}

func (s *GatewayStore) ListAuditFiltered(filter AuditFilter) ([]AuditEvent, error) {
	if filter.Limit <= 0 || filter.Limit > 500 {
		filter.Limit = 100
	}
	conditions := make([]string, 0, 6)
	args := make([]any, 0, 7)
	if v := strings.TrimSpace(filter.UserID); v != "" {
		conditions = append(conditions, "user_id = ?")
		args = append(args, v)
	}
	if v := strings.TrimSpace(filter.Cluster); v != "" {
		conditions = append(conditions, "cluster = ?")
		args = append(args, v)
	}
	if v := strings.TrimSpace(filter.Instance); v != "" {
		conditions = append(conditions, "instance = ?")
		args = append(args, v)
	}
	if v := strings.TrimSpace(filter.Session); v != "" {
		conditions = append(conditions, "session = ?")
		args = append(args, v)
	}
	if v := strings.TrimSpace(string(filter.Action)); v != "" {
		conditions = append(conditions, "action = ?")
		args = append(args, v)
	}
	if v := strings.TrimSpace(filter.Status); v != "" {
		conditions = append(conditions, "status = ?")
		args = append(args, v)
	}
	query := `SELECT id, user_id, token_id, cluster, instance, action, session,
		command_summary, status, error, tail, bytes_in, bytes_out, started_at, ended_at
		FROM audit_events`
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	query += " ORDER BY id DESC LIMIT ?"
	args = append(args, filter.Limit)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []AuditEvent
	for rows.Next() {
		var ev AuditEvent
		var action, started, ended string
		if err := rows.Scan(&ev.ID, &ev.UserID, &ev.TokenID, &ev.Cluster, &ev.Instance,
			&action, &ev.Session, &ev.CommandSummary, &ev.Status, &ev.Error, &ev.Tail,
			&ev.BytesIn, &ev.BytesOut, &started, &ended); err != nil {
			return nil, err
		}
		ev.Action = GatewayAction(action)
		ev.StartedAt, _ = parseTime(started)
		if ended != "" {
			ev.EndedAt, _ = parseTime(ended)
		}
		events = append(events, ev)
	}
	return events, rows.Err()
}

func (s *GatewayStore) DeleteAuditBefore(before time.Time) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM audit_events WHERE started_at < ?`, formatTime(before.UTC()))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func scopeMatches(pattern, value string) bool {
	pattern = strings.TrimSpace(pattern)
	value = strings.TrimSpace(value)
	return pattern == "*" || pattern == value
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func trimTail(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[len(s)-max:]
}

func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func parseTime(s string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, s)
}

func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	return hex.EncodeToString(buf)
}

func tokenPassword(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

func hashPassword(password string) (string, error) {
	if strings.TrimSpace(password) == "" {
		return "", fmt.Errorf("password is required")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

func (s *GatewayStore) ensureColumn(table, column, definition string) error {
	rows, err := s.db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, pk int
		var name, typ string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = s.db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, definition))
	return err
}
