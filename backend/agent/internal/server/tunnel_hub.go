package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/user/doops/agent/api"
	"github.com/user/doops/agent/pkg/release"
)

type GatewayHubOptions struct {
	AgentLease            time.Duration
	OperationTimeout      time.Duration
	TargetQueueTimeout    time.Duration
	TargetReconnectGrace  time.Duration
	LoginTokenTTL         time.Duration
	LoginTokenMaxLifetime time.Duration
	LoginFailureLimit     int
	LoginFailureWindow    time.Duration
	MaxQueuedPerTarget    int
}

type GatewayHub struct {
	store        *GatewayStore
	opts         GatewayHubOptions
	registry     *AgentRegistry
	registration *AgentRegistrationHandler
	client       *ClientService
	edge         *EdgeRegistry

	credentialMutationLocks sync.Map
	loginFailures           *loginFailureLimiter
}

type contextMutex struct {
	ready chan struct{}
}

func newContextMutex() contextMutex {
	ready := make(chan struct{}, 1)
	ready <- struct{}{}
	return contextMutex{ready: ready}
}

func (m *contextMutex) Acquire(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-m.ready:
		return nil
	}
}

func (m *contextMutex) Release() {
	m.ready <- struct{}{}
}

var (
	errGatewayClientDisconnected = errors.New("gateway client disconnected")
	errGatewayAgentDisconnected  = errors.New("agent disconnected")
	errGatewayMessageQueueClosed = errors.New("gateway message queue closed")
	errGatewayMessageQueueFull   = errors.New("gateway message queue full")
)

const (
	gatewayMessageBudgetMaxBytes       = 128 << 20
	gatewayMessageQueueMaxBytes        = 64 << 20
	gatewayAgentTouchTimeout           = 2 * time.Second
	gatewayReadProgressAckInterval     = 5 * time.Second
	gatewayReadProgressAckWriteTimeout = 3 * time.Second
	credentialPlanProtocolV2           = "doops.sh/credential-plan/v2"
	credentialPlanDeclarationV3        = "doops.sh/v3"
)

func credentialPlanCapability() map[string]interface{} {
	return map[string]interface{}{
		"schemaVersion":            credentialPlanProtocolV2,
		"declarationSchemaVersion": credentialPlanDeclarationV3,
		"sourceRevision":           true,
		"targetBinding":            true,
		"sessionBinding":           true,
		"buildDefinition":          true,
	}
}

func credentialPlanCapabilityCompatible(value interface{}) bool {
	return capabilityMatches(value, credentialPlanCapability())
}

type GatewayActiveOperation struct {
	ID                string        `json:"id"`
	AuditID           int64         `json:"audit_id,omitempty"`
	State             string        `json:"state"`
	Resource          string        `json:"resource,omitempty"`
	RequestedResource string        `json:"requested_resource,omitempty"`
	EffectiveLock     string        `json:"effective_lock,omitempty"`
	BlockedBy         []string      `json:"blocked_by,omitempty"`
	UserID            string        `json:"user_id"`
	TokenID           string        `json:"token_id,omitempty"`
	Cluster           string        `json:"cluster"`
	Instance          string        `json:"instance"`
	Action            GatewayAction `json:"action"`
	Session           string        `json:"session,omitempty"`
	CommandSummary    string        `json:"command_summary,omitempty"`
	Kind              string        `json:"kind"`
	StartedAt         time.Time     `json:"started_at"`
	AgeSeconds        int64         `json:"age_seconds"`
}

type gatewayActiveOperation struct {
	GatewayActiveOperation
	cancel context.CancelFunc
	agent  *GatewayAgent
	lease  *operationLease
	finish *AuditFinish
}

// operationLease binds a resource release to the operation that acquired it.
// The release is idempotent so normal completion and operation finalization
// can safely converge on one terminal cleanup path.
type operationLease struct {
	once    sync.Once
	release func()
}

func newOperationLease(release func()) *operationLease {
	return &operationLease{release: release}
}

func (l *operationLease) Release() {
	if l == nil || l.release == nil {
		return
	}
	l.once.Do(l.release)
}

type GatewayAgent struct {
	Cluster                string    `json:"cluster"`
	Instance               string    `json:"instance"`
	Key                    string    `json:"key"`
	Remote                 string    `json:"remote"`
	TokenID                string    `json:"token_id,omitempty"`
	Generation             uint64    `json:"generation"`
	RuntimeID              string    `json:"runtime_id,omitempty"`
	ReleaseCommit          string    `json:"release_commit,omitempty"`
	CredentialPlanProtocol string    `json:"credential_plan_protocol,omitempty"`
	StructuredSSH          bool      `json:"structured_ssh,omitempty"`
	ExecutionLifecycle     bool      `json:"execution_lifecycle,omitempty"`
	UpgradeAudit           bool      `json:"upgrade_audit,omitempty"`
	RegistryAuthfile       bool      `json:"registry_authfile,omitempty"`
	COSSnapshotExecutable  bool      `json:"cos_snapshot_executable,omitempty"`
	ConnectedAt            time.Time `json:"connected_at"`
	LastSeen               time.Time `json:"last_seen"`
	HeartbeatAt            time.Time `json:"heartbeat_at,omitempty"`
	Status                 string    `json:"status,omitempty"`
	BusyReason             string    `json:"busy_reason,omitempty"`
	ActiveOps              int       `json:"active_ops,omitempty"`
	QueuedOps              int       `json:"queued_ops,omitempty"`

	stateMu          sync.RWMutex
	conn             *websocket.Conn
	writeMu          sync.Mutex
	dataWriteState   websocketDataWriteState
	heartbeatPayload string
	lastProgressAck  time.Time
	queueMu          sync.Mutex
	queued           int
	opsMu            sync.Mutex
	stateChanged     chan struct{}
	draining         bool
	writers          int
	sessionWriters   map[string]int
	readers          int
	resources        map[string]*agentResourceSlot
	pendingMu        sync.Mutex
	pending          map[int64]*gatewayMessageQueue
	activeByRequest  map[int64]*gatewayMessageQueue
	activeSessions   map[string]int
	messageBudget    *gatewayMessageBudget
	closed           chan struct{}
	reqID            int64
	modelVersionID   string
}

type agentResourceSlot struct {
	slot    chan struct{}
	queued  int
	session string
}

type GatewayTarget struct {
	Cluster       string    `json:"cluster"`
	Instance      string    `json:"instance"`
	Key           string    `json:"key"`
	Remote        string    `json:"remote"`
	TokenID       string    `json:"token_id,omitempty"`
	ReleaseCommit string    `json:"release_commit,omitempty"`
	ConnectedAt   time.Time `json:"connected_at"`
	LastSeen      time.Time `json:"last_seen"`
	Busy          bool      `json:"busy"`
	Status        string    `json:"status"`
	BusyReason    string    `json:"busy_reason,omitempty"`
	ActiveOps     int       `json:"active_ops"`
	QueuedOps     int       `json:"queued_ops"`
	Resources     []string  `json:"resources,omitempty"`
	Sessions      []string  `json:"sessions,omitempty"`
}

type gatewayWSMessage struct {
	Raw    []byte
	Parsed map[string]interface{}
}

type gatewayQueuedMessage struct {
	message gatewayWSMessage
	size    int64
}

type gatewayMessageBudget struct {
	mu       sync.Mutex
	used     int64
	maxBytes int64
}

type gatewayMessageQueue struct {
	mu              sync.Mutex
	items           []gatewayQueuedMessage
	queuedBytes     int64
	maxBytes        int64
	budget          *gatewayMessageBudget
	notify          chan struct{}
	requestID       int64
	cancelMethod    string
	cancelOnce      sync.Once
	cancelDone      chan struct{}
	terminal        chan struct{}
	terminalOnce    sync.Once
	terminalErr     error
	terminalMessage gatewayWSMessage
	closed          bool
	err             error
}

func newGatewayMessageBudget(maxBytes int64) *gatewayMessageBudget {
	return &gatewayMessageBudget{maxBytes: maxBytes}
}

func (b *gatewayMessageBudget) reserve(size int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if size <= 0 || b.used+size > b.maxBytes {
		return false
	}
	b.used += size
	return true
}

func (b *gatewayMessageBudget) release(size int64) {
	if size <= 0 {
		return
	}
	b.mu.Lock()
	b.used -= size
	b.mu.Unlock()
}

func newGatewayMessageQueue(budget *gatewayMessageBudget, maxBytes int64, requestID int64, cancelMethod string) *gatewayMessageQueue {
	if budget == nil {
		panic("gateway message budget is not initialized")
	}
	if maxBytes <= 0 {
		maxBytes = gatewayMessageQueueMaxBytes
	}
	return &gatewayMessageQueue{
		maxBytes:     maxBytes,
		budget:       budget,
		notify:       make(chan struct{}, 1),
		requestID:    requestID,
		cancelMethod: cancelMethod,
		cancelDone:   make(chan struct{}),
		terminal:     make(chan struct{}),
	}
}

func (q *gatewayMessageQueue) enqueue(message gatewayWSMessage) error {
	size := int64(len(message.Raw))*2 + 1024
	if len(message.Raw) == 0 {
		data, err := json.Marshal(message.Parsed)
		if err != nil {
			return fmt.Errorf("encode queued message: %w", err)
		}
		size = int64(len(data))*2 + 1024
	}

	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return errGatewayMessageQueueClosed
	}
	if q.queuedBytes+size > q.maxBytes {
		err := fmt.Errorf("%w: queued=%d incoming=%d limit=%d", errGatewayMessageQueueFull, q.queuedBytes, size, q.maxBytes)
		released := q.failLocked(err)
		q.mu.Unlock()
		q.budget.release(released)
		q.signal()
		return err
	}
	if !q.budget.reserve(size) {
		err := fmt.Errorf("%w: shared budget limit=%d", errGatewayMessageQueueFull, q.budget.maxBytes)
		released := q.failLocked(err)
		q.mu.Unlock()
		q.budget.release(released)
		q.signal()
		return err
	}
	q.items = append(q.items, gatewayQueuedMessage{message: message, size: size})
	q.queuedBytes += size
	q.mu.Unlock()
	q.signal()
	return nil
}

func (q *gatewayMessageQueue) signal() {
	select {
	case q.notify <- struct{}{}:
	default:
	}
}

func (q *gatewayMessageQueue) dequeue() (gatewayWSMessage, bool, error) {
	q.mu.Lock()
	if q.closed {
		err := q.err
		q.mu.Unlock()
		return gatewayWSMessage{}, false, err
	}
	if len(q.items) == 0 {
		q.mu.Unlock()
		return gatewayWSMessage{}, false, nil
	}
	item := q.items[0]
	q.items[0] = gatewayQueuedMessage{}
	if len(q.items) == 1 {
		q.items = nil
	} else {
		q.items = q.items[1:]
	}
	q.queuedBytes -= item.size
	q.budget.release(item.size)
	q.mu.Unlock()
	return item.message, true, nil
}

func (q *gatewayMessageQueue) failLocked(err error) int64 {
	q.closed = true
	q.err = err
	released := q.queuedBytes
	q.queuedBytes = 0
	for i := range q.items {
		q.items[i] = gatewayQueuedMessage{}
	}
	q.items = nil
	return released
}

func (q *gatewayMessageQueue) fail(err error) {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	released := q.failLocked(err)
	q.mu.Unlock()
	q.budget.release(released)
	q.signal()
}

func (q *gatewayMessageQueue) close() {
	q.fail(errGatewayMessageQueueClosed)
}

type targetResponse struct {
	Targets []GatewayTarget `json:"targets"`
}

type auditResponse struct {
	Events []auditRecord `json:"events"`
}

type auditRecord struct {
	ID              int64                 `json:"id"`
	UserID          string                `json:"user_id"`
	TokenID         string                `json:"token_id"`
	Cluster         string                `json:"cluster"`
	Instance        string                `json:"instance"`
	CredentialID    string                `json:"credential_id,omitempty"`
	VersionID       string                `json:"credential_version_id,omitempty"`
	GrantID         string                `json:"credential_grant_id,omitempty"`
	Scope           *CredentialGrantScope `json:"scope,omitempty"`
	ResourceVersion string                `json:"resource_version,omitempty"`
	Digest          string                `json:"digest,omitempty"`
	CorrelationID   string                `json:"correlation_id,omitempty"`
	Action          string                `json:"action"`
	Session         string                `json:"session"`
	CommandSummary  string                `json:"command_summary"`
	Status          string                `json:"status"`
	Error           string                `json:"error,omitempty"`
	Tail            string                `json:"tail,omitempty"`
	BytesIn         int64                 `json:"bytes_in"`
	BytesOut        int64                 `json:"bytes_out"`
	StartedAt       string                `json:"started_at"`
	EndedAt         string                `json:"ended_at,omitempty"`
}

type auditPurgeResponse struct {
	Deleted int64 `json:"deleted"`
}

type gatewayLoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Name     string `json:"name,omitempty"`
}

type gatewayLoginResponse struct {
	Token     string     `json:"token"`
	TokenID   string     `json:"token_id"`
	TokenType string     `json:"token_type"`
	Username  string     `json:"username"`
	Name      string     `json:"name,omitempty"`
	IssuedAt  *time.Time `json:"issued_at,omitempty"`
	CreatedAt *time.Time `json:"created_at,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

type gatewayCurrentTokenResponse struct {
	TokenID   string     `json:"token_id"`
	UserID    string     `json:"user_id"`
	Username  string     `json:"username"`
	IsAdmin   bool       `json:"is_admin"`
	Name      string     `json:"name"`
	IssuedAt  time.Time  `json:"issued_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

type gatewayAdminTokenCreateRequest struct {
	User    string `json:"user"`
	Name    string `json:"name,omitempty"`
	Expires string `json:"expires,omitempty"`
}

func NewGatewayHub(store *GatewayStore, opts GatewayHubOptions) *GatewayHub {
	registry, err := NewAgentRegistry(store)
	if err != nil {
		panic(err)
	}
	if opts.AgentLease <= 0 {
		opts.AgentLease = 90 * time.Second
	}
	if opts.OperationTimeout <= 0 {
		opts.OperationTimeout = 60 * time.Minute
	}
	if opts.TargetQueueTimeout <= 0 {
		opts.TargetQueueTimeout = 2 * time.Minute
	}
	if opts.TargetReconnectGrace <= 0 {
		opts.TargetReconnectGrace = 10 * time.Second
	}
	opts.LoginTokenTTL, opts.LoginTokenMaxLifetime = normalizeLoginTokenLifetimes(opts.LoginTokenTTL, opts.LoginTokenMaxLifetime)
	if opts.LoginFailureLimit <= 0 {
		opts.LoginFailureLimit = 10
	}
	if opts.LoginFailureWindow <= 0 {
		opts.LoginFailureWindow = time.Minute
	}
	if opts.MaxQueuedPerTarget <= 0 {
		opts.MaxQueuedPerTarget = 0
	}
	hub := &GatewayHub{
		store:         store,
		opts:          opts,
		registry:      registry,
		loginFailures: newLoginFailureLimiter(),
	}
	hub.registration = NewAgentRegistrationHandler(
		registry,
		opts.AgentLease,
		newGatewayMessageBudget(gatewayMessageBudgetMaxBytes),
	)
	hub.client = NewClientService(store, registry, opts)
	hub.client.hub = hub
	hub.registration.onRegistered = hub.deliverAgentCredentials
	edge, err := NewEdgeRegistry(store)
	if err != nil {
		panic(err)
	}
	hub.edge = edge
	if err := store.RecoverInterruptedExecutions(); err != nil {
		panic(fmt.Errorf("recover interrupted executions: %w", err))
	}
	return hub
}

// StartOfflineInstanceReaper removes offline instance records past the
// retention window once immediately and then on interval. It is started by
// the gateway process, not by tests that only construct a hub.
func (h *GatewayHub) StartOfflineInstanceReaper(interval time.Duration) {
	if interval <= 0 {
		interval = time.Hour
	}
	go func() {
		h.pruneOfflineInstances()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			h.pruneOfflineInstances()
		}
	}()
	log.Printf("[gateway] offline instance cleanup started (active=%s, retention=%s, interval=%s)",
		ActiveInstanceWindow, OfflineInstanceRetention, interval)
}

func (h *GatewayHub) pruneOfflineInstances() {
	deleted, err := h.store.PruneOfflineAgentStatus(time.Now().UTC())
	if err != nil {
		log.Printf("[gateway] offline instance cleanup failed: %v", err)
		return
	}
	if deleted > 0 {
		log.Printf("[gateway] removed %d offline instance records older than %s", deleted, OfflineInstanceRetention)
	}
}

func (h *GatewayHub) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/ws", HandleRetiredInboundWS)
	mux.HandleFunc("/v1/auth/login", h.HandleAuthLogin)
	mux.HandleFunc("/v1/auth/me", h.HandleAuthMe)
	mux.HandleFunc("/v1/agent/connect", h.HandleAgentConnect)
	mux.HandleFunc("/v1/edge/connect", h.HandleEdgeConnect)
	mux.HandleFunc("/v1/rpc", h.HandleClientRPC)
	mux.HandleFunc("/v1/git/", h.HandleGitHTTP)
	mux.HandleFunc("/", h.HandlePublicProxy)
	mux.HandleFunc("/v1/targets", h.HandleTargets)
	mux.HandleFunc("/v1/assets/machines", h.HandleMachineInventory)
	mux.HandleFunc("/v1/assets/clusters", h.HandleClusterInventory)
	mux.HandleFunc("/v1/assets/node-links", h.HandleAssetNodeLinks)
	mux.HandleFunc("/v1/assets/dashboard", h.HandleAssetDashboard)
	mux.HandleFunc("/v1/assets/machine-diagnostics", h.HandleMachineDiagnostics)
	mux.HandleFunc("/v1/assets/systems", h.HandleBusinessInventory)
	mux.HandleFunc("/v1/assets/topology", h.HandleAssetTopology)
	mux.HandleFunc("/v1/assets/coverage", h.HandleAssetCoverage)
	mux.HandleFunc("/v1/assets/monitor", h.HandleAssetMonitor)
	mux.HandleFunc("/v1/assets/inspections", h.HandleAssetInspection)
	mux.HandleFunc("/v1/audit", h.HandleAudit)
	mux.HandleFunc("/v1/credential-bundles", h.HandleCredentialBundles)
	mux.HandleFunc("/v1/credentials", h.HandleCredentials)
	mux.HandleFunc("/v1/credentials/", h.HandleCredential)
	admin := func(path string, handler http.HandlerFunc) {
		mux.HandleFunc(path, h.auditAdminMutation(path, handler))
	}
	mux.HandleFunc("/v1/admin/tokens", h.HandleAdminTokens)
	admin("/v1/admin/users", h.HandleAdminUsers)
	admin("/v1/admin/users/password", h.HandleAdminUserPassword)
	admin("/v1/admin/users/disable", h.HandleAdminUserDisable)
	admin("/v1/admin/grants", h.HandleAdminGrants)
	admin("/v1/admin/instances", h.HandleAdminInstances)
	admin("/v1/admin/instances/archive", h.HandleAdminInstanceArchive)
	admin("/v1/admin/instances/unarchive", h.HandleAdminInstanceUnarchive)
	admin("/v1/admin/proxy-routes", h.HandleAdminProxyRoutes)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
}

func (h *GatewayHub) HandleAuthLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	var req gatewayLoginRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "invalid login request", http.StatusBadRequest)
		return
	}
	clientIP := loginClientIP(r)
	if h.loginFailures.limited(clientIP, req.Username, time.Now(), h.opts.LoginFailureLimit, h.opts.LoginFailureWindow) {
		w.Header().Set("Retry-After", "60")
		writeDoopsError(w, http.StatusTooManyRequests, "", doopsErrorResponse{
			Error:      "rate_limited",
			Code:       "gateway_login_rate_limited",
			AuthDomain: "gateway",
			Message:    "Gateway password login is temporarily unavailable for this client",
			NextAction: "Wait and retry, or use an existing Gateway user token",
		})
		return
	}
	user, err := h.store.VerifyUserPassword(req.Username, req.Password)
	if err != nil {
		if !errors.Is(err, ErrUnauthorized) {
			writeGatewayAuthError(w, `Bearer realm="doops-gateway"`, err)
			return
		}
		h.loginFailures.record(clientIP, req.Username, time.Now(), h.opts.LoginFailureWindow)
		writeDoopsError(w, http.StatusUnauthorized, "", doopsErrorResponse{
			Error:      "unauthorized",
			Code:       "gateway_login_invalid_credentials",
			AuthDomain: "gateway",
			Message:    "Gateway username or password was rejected",
			NextAction: "Verify the Gateway username/password or provide a valid Gateway user token",
		})
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = "password login"
	}
	token, err := h.store.CreateToken(CreateTokenRequest{
		Kind:      TokenKindUser,
		UserID:    user.ID,
		Name:      name,
		ExpiresAt: time.Now().UTC().Add(h.opts.LoginTokenTTL),
		IssuedVia: tokenIssuedViaPasswordLogin,
	})
	if err != nil {
		http.Error(w, "failed to create token", http.StatusInternalServerError)
		return
	}
	if revoked, err := h.store.RevokeExcessPasswordLoginTokens(user.ID, token.ID, passwordLoginTokenCap, time.Now().UTC()); err != nil {
		log.Printf("[gateway] failed to cap password-login tokens for user %s: %v", user.ID, err)
	} else if revoked > 0 {
		log.Printf("[gateway] revoked %d older password-login token(s) for user %s", revoked, user.ID)
	}
	writeJSONHTTP(w, gatewayLoginResponse{
		Token:     token.Plaintext,
		TokenID:   token.ID,
		TokenType: string(TokenKindUser),
		Username:  user.Name,
		Name:      token.Name,
		IssuedAt:  tokenTimePtr(token.CreatedAt),
		CreatedAt: tokenTimePtr(token.CreatedAt),
		ExpiresAt: tokenTimePtr(token.ExpiresAt),
	})
}

func (h *GatewayHub) HandleAuthMe(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	auth, err := h.authenticateUser(r)
	if err != nil {
		h.writeUserAuthError(w, r, err)
		return
	}
	info, isAdmin, err := h.store.CurrentUserTokenInfo(auth)
	if err != nil {
		h.writeUserAuthError(w, r, err)
		return
	}
	writeJSONHTTP(w, gatewayCurrentTokenResponse{
		TokenID: info.ID, UserID: info.UserID, Username: info.UserName, IsAdmin: isAdmin, Name: info.Name,
		IssuedAt: info.CreatedAt, ExpiresAt: info.ExpiresAt,
	})
}

func (h *GatewayHub) HandleAdminTokens(w http.ResponseWriter, r *http.Request) {
	h.auditAdminMutation("/v1/admin/tokens", h.handleAdminTokens)(w, r)
}

func (h *GatewayHub) handleAdminTokens(w http.ResponseWriter, r *http.Request) {
	auth, err := h.authenticateUser(r)
	if err != nil {
		h.writeUserAuthError(w, r, err)
		return
	}
	if !h.store.UserIsPlatformAdmin(auth.UserID) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	switch r.Method {
	case http.MethodGet:
		tokens, err := h.store.ListTokens(TokenKindUser)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSONHTTP(w, map[string]interface{}{"tokens": tokens})
		return
	case http.MethodDelete:
		id := strings.TrimSpace(r.URL.Query().Get("id"))
		if id == "" {
			http.Error(w, "missing id", http.StatusBadRequest)
			return
		}
		if err := h.store.RevokeToken(id); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				http.Error(w, "token not found", http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSONHTTP(w, map[string]interface{}{"id": id, "revoked": true})
		return
	case http.MethodPost:
		// 继续向下执行创建逻辑
	default:
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	var req gatewayAdminTokenCreateRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "invalid token request", http.StatusBadRequest)
		return
	}
	user, err := h.store.FindUserByName(req.User)
	if err != nil {
		http.Error(w, "user not found", http.StatusNotFound)
		return
	}
	createReq := CreateTokenRequest{Kind: TokenKindUser, UserID: user.ID, Name: req.Name}
	if strings.TrimSpace(req.Expires) != "" {
		ttl, err := time.ParseDuration(strings.TrimSpace(req.Expires))
		if err != nil {
			http.Error(w, "invalid expires duration", http.StatusBadRequest)
			return
		}
		if ttl <= 0 {
			http.Error(w, "expires duration must be positive", http.StatusBadRequest)
			return
		}
		createReq.ExpiresAt = time.Now().UTC().Add(ttl)
	}
	token, err := h.store.CreateToken(createReq)
	if err != nil {
		http.Error(w, "failed to create token", http.StatusInternalServerError)
		return
	}
	writeJSONHTTP(w, gatewayLoginResponse{
		Token:     token.Plaintext,
		TokenID:   token.ID,
		TokenType: string(TokenKindUser),
		Username:  user.Name,
		Name:      token.Name,
		IssuedAt:  tokenTimePtr(token.CreatedAt),
		CreatedAt: tokenTimePtr(token.CreatedAt),
		ExpiresAt: tokenTimePtr(token.ExpiresAt),
	})
}

func tokenTimePtr(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	utc := value.UTC()
	return &utc
}

func (h *GatewayHub) listActiveOperations() []GatewayActiveOperation {
	return h.client.listActiveOperations()
}

func (h *GatewayHub) cancelActiveOperation(id string) bool {
	return h.client.cancelActiveOperation(id)
}

func (h *GatewayHub) HandleAgentConnect(w http.ResponseWriter, r *http.Request) {
	h.registration.ServeHTTP(w, r)
}

func newGatewayAgentSession(
	cluster,
	instance,
	tokenID,
	remote string,
	conn *websocket.Conn,
	messageBudget *gatewayMessageBudget,
) *GatewayAgent {
	agent := &GatewayAgent{
		Cluster:         cluster,
		Instance:        instance,
		Key:             tunnelKey(cluster, instance),
		Remote:          remote,
		TokenID:         tokenID,
		ConnectedAt:     time.Now().UTC(),
		LastSeen:        time.Now().UTC(),
		conn:            conn,
		pending:         make(map[int64]*gatewayMessageQueue),
		activeByRequest: make(map[int64]*gatewayMessageQueue),
		resources:       make(map[string]*agentResourceSlot),
		sessionWriters:  make(map[string]int),
		activeSessions:  make(map[string]int),
		messageBudget:   messageBudget,
		closed:          make(chan struct{}),
		stateChanged:    make(chan struct{}),
	}
	return agent
}

func (h *GatewayHub) HandleTargets(w http.ResponseWriter, r *http.Request) {
	auth, err := h.authenticateUser(r)
	if err != nil {
		h.writeUserAuthError(w, r, err)
		return
	}
	targets := h.ListTargets()
	filtered := targets[:0]
	for _, target := range targets {
		if h.store.UserCanUseTarget(auth.UserID, target.Cluster, target.Instance) {
			filtered = append(filtered, target)
		}
	}
	writeJSONHTTP(w, targetResponse{Targets: filtered})
}

func (h *GatewayHub) HandleAudit(w http.ResponseWriter, r *http.Request) {
	auth, err := h.authenticateUser(r)
	if err != nil {
		h.writeUserAuthError(w, r, err)
		return
	}
	isAdmin := h.store.UserIsPlatformAdmin(auth.UserID)

	switch r.Method {
	case http.MethodGet:
		events, err := h.visibleAuditEvents(auth, r.URL.Query())
		if err != nil {
			if errors.Is(err, ErrUnauthorized) {
				http.Error(w, "Forbidden", http.StatusForbidden)
			} else if errors.Is(err, errInvalidAuditFilter) {
				http.Error(w, err.Error(), http.StatusBadRequest)
			} else {
				http.Error(w, "audit_unavailable", http.StatusServiceUnavailable)
			}
			return
		}
		records := make([]auditRecord, 0, len(events))
		for _, event := range events {
			record := auditRecord{
				ID:              event.ID,
				UserID:          event.UserID,
				TokenID:         event.TokenID,
				Cluster:         event.Cluster,
				Instance:        event.Instance,
				CredentialID:    event.CredentialID,
				VersionID:       event.VersionID,
				GrantID:         event.GrantID,
				Scope:           event.Scope,
				ResourceVersion: event.ResourceVersion,
				Digest:          event.Digest,
				CorrelationID:   event.CorrelationID,
				Action:          string(event.Action),
				Session:         event.Session,
				CommandSummary:  event.CommandSummary,
				Status:          event.Status,
				Error:           event.Error,
				Tail:            event.Tail,
				BytesIn:         event.BytesIn,
				BytesOut:        event.BytesOut,
				StartedAt:       event.StartedAt.UTC().Format(time.RFC3339),
			}
			if !event.EndedAt.IsZero() {
				record.EndedAt = event.EndedAt.UTC().Format(time.RFC3339)
			}
			records = append(records, record)
		}
		writeJSONHTTP(w, auditResponse{Events: records})
	case http.MethodDelete:
		if !isAdmin {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		beforeRaw := strings.TrimSpace(r.URL.Query().Get("before"))
		if beforeRaw == "" {
			http.Error(w, "missing before", http.StatusBadRequest)
			return
		}
		before, err := time.Parse(time.RFC3339, beforeRaw)
		if err != nil {
			http.Error(w, "invalid before; use RFC3339", http.StatusBadRequest)
			return
		}
		deleted, err := h.store.PurgeAuditBefore(auth, before)
		if err != nil {
			http.Error(w, "audit_purge_failed", http.StatusServiceUnavailable)
			return
		}
		writeJSONHTTP(w, auditPurgeResponse{Deleted: deleted})
	default:
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

func (h *GatewayHub) HandleClientRPC(w http.ResponseWriter, r *http.Request) {
	h.client.HandleClientRPC(w, r)
}

func (h *ClientService) HandleClientRPC(w http.ResponseWriter, r *http.Request) {
	auth, err := h.authenticateUser(r)
	if err != nil {
		h.writeUserAuthError(w, r, err)
		return
	}
	cluster := strings.TrimSpace(r.URL.Query().Get("cluster"))
	instance := strings.TrimSpace(r.URL.Query().Get("instance"))
	if cluster == "" {
		http.Error(w, "missing cluster", http.StatusBadRequest)
		return
	}
	if instance == "" {
		http.Error(w, "missing instance", http.StatusBadRequest)
		return
	}
	if !h.store.UserCanUseTarget(auth.UserID, cluster, instance) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[gateway] client upgrade failed: %v", err)
		return
	}
	defer conn.Close()

	var connMu sync.Mutex
	var dataWriteState websocketDataWriteState
	writeJSON := func(v interface{}) error {
		if err := writeWebSocketJSON(conn, &connMu, &dataWriteState, v, agentWSDataWriteTimeout); err != nil {
			log.Printf("[gateway] client write failed: %v", err)
			return fmt.Errorf("%w: %v", errGatewayClientDisconnected, err)
		}
		return nil
	}
	writeRaw := func(raw []byte) error {
		return writeJSON(json.RawMessage(raw))
	}

	connCtx, cancelConn := context.WithCancel(r.Context())
	defer cancelConn()
	messages := make(chan []byte, 16)
	go func() {
		defer close(messages)
		defer cancelConn()
		for {
			msgType, data, err := conn.ReadMessage()
			if err != nil {
				if !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
					log.Printf("[gateway] client read failed: %v", err)
				}
				return
			}
			if msgType != websocket.TextMessage {
				continue
			}
			select {
			case messages <- data:
			case <-connCtx.Done():
				return
			}
		}
	}()

	var callWG sync.WaitGroup
	var callsMu sync.Mutex
	calls := make(map[string]context.CancelFunc)
	for data := range messages {
		var req api.JSONRPCRequest
		if err := json.Unmarshal(data, &req); err != nil {
			continue
		}
		switch req.Method {
		case "initialize":
			agent := h.registry.Get(cluster, instance)
			if failure := h.auditUpgradeVerification(auth, cluster, instance, req, agent); failure != nil {
				writeJSON(*failure)
				continue
			}
			writeJSON(api.JSONRPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result: map[string]interface{}{
					"protocolVersion": "2024-11-05",
					"serverInfo": map[string]string{
						"name":          "doops-gateway",
						"version":       release.Version,
						"releaseCommit": release.Commit,
					},
					"capabilities": clientCapabilities(agent),
				},
			})
		case "tools/call":
			key, ok := clientRPCRequestKey(req.ID)
			if !ok {
				writeJSON(buildErrorResponse(req.ID, -32600, "tools/call requires a string or numeric request id"))
				continue
			}
			callsMu.Lock()
			if _, exists := calls[key]; exists {
				callsMu.Unlock()
				writeJSON(buildErrorResponse(req.ID, -32600, "request id is already active"))
				continue
			}
			callCtx, cancelCall := context.WithCancel(connCtx)
			calls[key] = cancelCall
			callsMu.Unlock()
			callWG.Add(1)
			go func(req api.JSONRPCRequest) {
				defer callWG.Done()
				defer func() {
					cancelCall()
					callsMu.Lock()
					delete(calls, key)
					callsMu.Unlock()
				}()
				if err := h.handleGatewayToolCall(callCtx, auth, cluster, instance, req, writeRaw, writeJSON); err != nil {
					writeJSON(buildErrorResponse(req.ID, -32603, err.Error()))
				}
			}(req)
		case "tools/cancel":
			var params struct {
				ID interface{} `json:"id"`
			}
			if err := json.Unmarshal(req.Params, &params); err != nil {
				writeJSON(buildErrorResponse(req.ID, -32602, "invalid tools/cancel params"))
				continue
			}
			key, ok := clientRPCRequestKey(params.ID)
			if !ok {
				writeJSON(buildErrorResponse(req.ID, -32602, "tools/cancel requires the original request id"))
				continue
			}
			callsMu.Lock()
			cancel := calls[key]
			callsMu.Unlock()
			if cancel != nil {
				cancel()
			}
			writeJSON(api.JSONRPCResponse{
				JSONRPC: "2.0", ID: req.ID,
				Result: map[string]interface{}{"canceled": cancel != nil},
			})
		default:
			writeJSON(buildErrorResponse(req.ID, -32601, "unknown method: "+req.Method))
		}
	}
	callWG.Wait()
}

func clientRPCRequestKey(id interface{}) (string, bool) {
	switch id.(type) {
	case string, float64:
		encoded, err := json.Marshal(id)
		return string(encoded), err == nil
	default:
		return "", false
	}
}

func (h *ClientService) handleGatewayToolCall(ctx context.Context, auth TokenAuth, cluster, instance string, req api.JSONRPCRequest, writeRaw func([]byte) error, writeJSON func(interface{}) error) error {
	if err := h.store.validateUserTokenAuth(auth); err != nil {
		if errors.Is(err, ErrUnauthorized) {
			return writeJSON(buildErrorResponse(req.ID, -32001, authFailureCode(err)))
		}
		return writeJSON(buildErrorResponse(req.ID, -32603, "gateway_auth_unavailable"))
	}
	ctx = contextWithInternalAuditAuth(ctx, auth)
	var params api.ToolCallParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		writeJSON(buildErrorResponse(req.ID, -32602, "invalid tools/call params"))
		return nil
	}
	if internalCredentialTool(params.Name) {
		writeJSON(buildErrorResponse(req.ID, -32003, "credential control-plane tools are internal only"))
		return nil
	}
	if params.Name == "doops_cos_sync" {
		writeJSON(buildErrorResponse(req.ID, -32003, "cos_sync_internal"))
		return nil
	}
	if cosToolName(params.Name) {
		return h.handleCOSToolCall(ctx, auth, cluster, instance, req, params, writeJSON)
	}
	var promptArgs api.AgentPromptParams
	if params.Name == "doops_agent_prompt" {
		if err := json.Unmarshal(params.Arguments, &promptArgs); err != nil {
			writeJSON(buildErrorResponse(req.ID, -32602, "invalid agent prompt arguments"))
			return nil
		}
		operation := extractStringArg(params.Arguments, "operation")
		if operation != "" && operation != "ask" && operation != "apply" {
			writeJSON(buildErrorResponse(req.ID, -32602, "unsupported agent prompt operation: "+operation))
			return nil
		}
		if err := validateExplicitCredentialNames(promptArgs.CredentialRefs); err != nil {
			writeJSON(buildErrorResponse(req.ID, -32602, err.Error()))
			return nil
		}
		if promptArgs.CredentialMode != "" && promptArgs.CredentialMode != "auto" {
			writeJSON(buildErrorResponse(req.ID, -32602, "unsupported credential mode"))
			return nil
		}
		if promptArgs.CredentialMode == "auto" && promptArgs.Skill != "dosql" {
			writeJSON(buildErrorResponse(req.ID, -32602, "automatic credential mode is only supported by dosql"))
			return nil
		}
		// DoSql only uses the target's database bindings; a caller-chosen
		// reference would reintroduce the second selection path.
		if promptArgs.Skill == "dosql" && len(promptArgs.CredentialRefs) != 0 {
			writeJSON(buildErrorResponse(req.ID, -32602, "dosql_credential_refs_unsupported: pick one bound database credential with sql_credential (doops sql --credential <name>)"))
			return nil
		}
		// sql_credential narrows the automatic set to one bound credential by
		// name or ID; it never adds a credential the target has not bound.
		if promptArgs.SQLCredential != "" {
			if promptArgs.Skill != "dosql" || promptArgs.CredentialMode != "auto" {
				writeJSON(buildErrorResponse(req.ID, -32602, "sql_credential requires skill dosql with automatic credential mode"))
				return nil
			}
			if !validCredentialMetadataToken(promptArgs.SQLCredential, 128) {
				writeJSON(buildErrorResponse(req.ID, -32602, "credential reference name is invalid"))
				return nil
			}
		}
		if err := validatePromptCredentialAccess(operation, len(promptArgs.CredentialRefs) != 0 || promptArgs.CredentialMode == "auto"); err != nil {
			writeJSON(buildErrorResponse(req.ID, -32602, err.Error()))
			return nil
		}
		if len(promptArgs.CredentialBindings) != 0 {
			writeJSON(buildErrorResponse(req.ID, -32602, "credential_bindings are gateway-managed"))
			return nil
		}
	}
	action := actionForTool(params.Name, params.Arguments)
	if action == "" {
		writeJSON(buildErrorResponse(req.ID, -32601, "unknown doops action for tool: "+params.Name))
		return nil
	}
	if action == ActionAgentUpgrade {
		arguments, err := prepareUpgradeAuditArguments(params.Arguments, auth, 0)
		if err != nil {
			return writeJSON(buildErrorResponse(req.ID, -32602, err.Error()))
		}
		params.Arguments = arguments
	}
	startAudit := h.store.StartAudit
	if params.Name == "doops_file_write" || action == ActionAgentUpgrade {
		startAudit = h.store.startMutationAudit
	}
	correlationID, digest := "", ""
	if action == ActionAgentUpgrade {
		correlationID = extractStringArg(params.Arguments, "upgrade_operation_id")
		_, digest, _ = strings.Cut(extractStringArg(params.Arguments, "image"), "@")
	}
	auditID, auditErr := startAudit(AuditEvent{
		UserID:         auth.UserID,
		TokenID:        auth.TokenID,
		Cluster:        cluster,
		Instance:       instance,
		Action:         action,
		Session:        extractSession(params.Arguments),
		CommandSummary: summarizeToolCall(params.Name, params.Arguments),
		CorrelationID:  correlationID,
		Digest:         digest,
		StartedAt:      time.Now().UTC(),
	})
	if auditErr != nil {
		writeJSON(buildErrorResponse(req.ID, -32603, "audit_unavailable"))
		return nil
	}
	if action == ActionAgentUpgrade {
		params.Arguments, _ = prepareUpgradeAuditArguments(params.Arguments, auth, auditID)
	}
	var opID string
	var pendingFinish *AuditFinish
	defer func() {
		if opID != "" || pendingFinish == nil {
			return
		}
		// Rejected admission has no execution lease, but still owns its audit.
		id := h.registerActiveOperation(GatewayActiveOperation{
			AuditID: auditID, UserID: auth.UserID, TokenID: auth.TokenID,
			Cluster: cluster, Instance: instance, Action: action,
			Session: extractSession(params.Arguments), Kind: "rpc",
			CommandSummary: summarizeToolCall(params.Name, params.Arguments),
		}, func() {})
		_ = h.finishExecutionAudit(auditID, *pendingFinish)
		h.finishActiveOperation(id)
	}()
	finishAudit := func(status, errMsg, tail string, bytesOut int64) error {
		if action != ActionAgentUpgrade {
			errMsg, tail = sanitizeAgentPromptAudit(params.Name, errMsg, tail)
		}
		errMsg = sanitizeSecretText(errMsg)
		if action != ActionAgentUpgrade {
			tail = sanitizeSecretText(tail)
		}
		finish := AuditFinish{
			Status:   status,
			Error:    errMsg,
			Tail:     tail,
			BytesIn:  int64(len(params.Arguments)),
			BytesOut: bytesOut,
			EndedAt:  time.Now().UTC(),
		}
		if err := h.finishExecutionAudit(auditID, finish); err != nil {
			pendingFinish = &finish
			log.Printf("[gateway] audit_completion_failed: id=%d", auditID)
			return err
		}
		if action == ActionAgentUpgrade && status != "observing" {
			log.Printf("[gateway] agent upgrade audit=%d operation=%s phase=%s status=%s category=%q",
				auditID, correlationID, upgradeAuditPhase(params.Arguments), status, errMsg)
		}
		return nil
	}
	if !h.store.UserCanUseTarget(auth.UserID, cluster, instance) {
		errMsg := fmt.Sprintf("forbidden: %s on %s/%s", action, cluster, instance)
		finishAudit("forbidden", errMsg, "", 0)
		writeJSON(buildErrorResponse(req.ID, -32003, errMsg))
		return nil
	}
	agent := h.registry.Get(cluster, instance)
	if agent == nil || agent.Cluster != cluster || agent.Instance != instance {
		errMsg := fmt.Sprintf("target offline: %s/%s", cluster, instance)
		finishAudit("offline", errMsg, "", 0)
		writeJSON(buildErrorResponse(req.ID, -32004, errMsg))
		return nil
	}
	// A model that was missing or rotated after connect is fetched from the
	// Gateway before doagent runs. The same active version is not probed again.
	if params.Name == "doops_agent_prompt" && h.hub != nil {
		if category := h.hub.deliverModelProvider(ctx, cluster, instance); category != "" {
			finishAudit("error", category, "", 0)
			writeJSON(buildErrorResponse(req.ID, -32603, category))
			return nil
		}
	}
	if params.Name == "doops_ssh" && !agent.StructuredSSH {
		errMsg := fmt.Sprintf(
			"target Agent does not advertise protocol %q; upgrade the Agent before using structured doops ssh",
			structuredSSHProtocol,
		)
		finishAudit("blocked", errMsg, "", 0)
		writeJSON(buildErrorResponse(req.ID, -32003, errMsg))
		return nil
	}
	if err := agent.requireExecutionLifecycle(params.Name); err != nil {
		finishAudit("blocked", err.Error(), "", 0)
		writeJSON(buildErrorResponse(req.ID, -32003, err.Error()))
		return nil
	}
	if action == ActionAgentUpgrade {
		if !agent.UpgradeAudit {
			errMsg := "upgrade_audit_protocol_required: target Agent must advertise upgradeAudit v1 before an upgrade can be submitted"
			finishAudit("blocked", errMsg, "", 0)
			writeJSON(buildErrorResponse(req.ID, -32003, errMsg))
			return nil
		}
		var args agentUpgradeParams
		if err := json.Unmarshal(params.Arguments, &args); err != nil {
			errMsg := fmt.Sprintf("invalid agent upgrade params: %v", err)
			finishAudit("error", errMsg, "", 0)
			writeJSON(buildErrorResponse(req.ID, -32602, errMsg))
			return nil
		}
		if err := validateAgentUpgradeDispatch(args); err != nil {
			finishAudit("error", err.Error(), "", 0)
			writeJSON(buildErrorResponse(req.ID, -32003, err.Error()))
			return nil
		}
	}
	var err error
	var authorizedCredentials []authorizedCredentialMaterialization
	if params.Name == "doops_agent_prompt" && (len(promptArgs.CredentialRefs) != 0 ||
		(promptArgs.Skill == "dosql" && promptArgs.CredentialMode == "auto") ||
		// A change task resolves the Helm credential already bound to the target
		// without naming it; resolution returns nothing when none is bound.
		automaticHelmCredentialEligible(action, promptArgs.Skill)) {
		authorizedCredentials, err = h.hub.resolveExplicitCredentials(
			auth, cluster, instance,
			promptArgs.Instruction, promptArgs.CredentialRefs, action, promptArgs.Skill, promptArgs.CredentialMode,
			promptArgs.SQLCredential,
		)
		if err != nil {
			category := credentialErrorCategory(err)
			finishAudit("blocked", category, "", 0)
			writeJSON(buildErrorResponse(req.ID, -32003, category))
			return nil
		}
	}
	opCtx, cancelOp := context.WithCancel(ctx)
	defer cancelOp()
	resourceKey := resourceKeyForTool(action, params.Name, params.Arguments, cluster, instance)
	operation := GatewayActiveOperation{
		AuditID:        auditID,
		Resource:       resourceKey,
		UserID:         auth.UserID,
		TokenID:        auth.TokenID,
		Cluster:        cluster,
		Instance:       instance,
		Action:         action,
		Session:        extractSession(params.Arguments),
		CommandSummary: summarizeToolCall(params.Name, params.Arguments),
		Kind:           "rpc",
	}
	if h.hasCancellationBarrier(operation) {
		finishAudit("busy", "target has a cancellation pending; wait for terminal evidence", "", 0)
		writeJSON(buildErrorResponse(req.ID, -32005, "target busy: cancellation pending"))
		return nil
	}
	var holderID string
	opID, holderID = h.registerActiveOperationForAgent(operation, cancelOp, agent)
	operationFinished := false
	defer func() {
		if !operationFinished {
			h.finishActiveOperation(opID)
		}
	}()
	if holderID != "" {
		errMsg := "session has an active operation; wait for terminal evidence: " + holderID
		finishAudit("busy", errMsg, "", 0)
		writeJSON(buildErrorResponse(req.ID, -32005, errMsg))
		return nil
	}

	// Credential use does not take the credential mutation lock: the grant is
	// re-read right before materialization, so a concurrent promotion only
	// decides which version this operation materializes.
	lease, err := agent.acquireActionLeaseForSession(opCtx, action, resourceKey, extractSession(params.Arguments), h.opts.MaxQueuedPerTarget, h.opts.TargetQueueTimeout)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			finishAudit("canceled", "operation_canceled", "", 0)
			writeJSON(buildErrorResponse(req.ID, -32007, "operation canceled"))
			return nil
		}
		if errors.Is(err, context.DeadlineExceeded) {
			finishAudit("timeout", "operation_timeout", "", 0)
			writeJSON(buildErrorResponse(req.ID, -32603, "operation_timeout"))
			return nil
		}
		errMsg := fmt.Sprintf("%v: %s/%s", err, cluster, instance)
		finishAudit("busy", errMsg, "", 0)
		writeJSON(buildErrorResponse(req.ID, -32005, errMsg))
		return nil
	}
	h.attachOperationLease(opID, lease)
	opCtx = withInternalCredentialLock(opCtx, agent, resourceKey)
	if err := opCtx.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			finishAudit("timeout", "operation_timeout", "", 0)
			writeJSON(buildErrorResponse(req.ID, -32603, "operation_timeout"))
		} else {
			finishAudit("canceled", "operation_canceled", "", 0)
			writeJSON(buildErrorResponse(req.ID, -32007, "operation canceled before dispatch"))
		}
		return nil
	}
	guarded := requiresExecutionOutcomeGuard(action, params.Name, params.Arguments, resourceKey)
	h.markOperationRunning(opID)
	opCtx = h.withExecutionOwner(opCtx, agent, resourceKey, opID)
	if guarded {
		if err := h.beginExecution(agent, opID, auditID); err != nil {
			_ = finishAudit("blocked", err.Error(), "", 0)
			writeJSON(buildErrorResponse(req.ID, -32005, err.Error()))
			return nil
		}
	}
	var credentialPreparation credentialApplyPreparation
	if len(authorizedCredentials) != 0 {
		for index := range authorizedCredentials {
			item := &authorizedCredentials[index]
			resolved, refreshErr := h.hub.store.AuthorizeCredentialUse(CredentialUseRequest{
				ActorID: auth.UserID, CredentialID: item.Authorized.Resource.ID, Scope: item.Reference.Scope,
			})
			if refreshErr != nil || h.store.validateUserTokenAuth(auth) != nil ||
				!h.store.UserCanUseTarget(auth.UserID, cluster, instance) ||
				resolved.Grant.ID != item.Authorized.Grant.ID ||
				!credentialScopeEqual(resolved.Grant.Scope, item.Authorized.Grant.Scope) {
				finishAudit("blocked", "credential_authorization_changed", "", 0)
				writeJSON(buildErrorResponse(req.ID, -32003, "credential_authorization_changed"))
				return nil
			}
			item.Authorized = resolved
		}
		credentialPreparation, err = h.hub.materializeExplicitCredentialApply(
			opCtx, auth, promptArgs.SessionID, authorizedCredentials,
		)
		if err != nil {
			category := credentialOperationErrorCategory(err)
			finishAudit("error", category, "", 0)
			writeJSON(buildErrorResponse(req.ID, -32603, category))
			return nil
		}
		promptArgs.CredentialRefs = nil
		if h.store.validateUserTokenAuth(auth) != nil || !h.store.UserCanUseTarget(auth.UserID, cluster, instance) {
			if cleanupErr := h.hub.rollbackCredentialPrepare(opCtx, credentialPreparation.completed); cleanupErr != nil {
				category := credentialOperationErrorCategory(cleanupErr)
				finishAudit("error", category, "", 0)
				writeJSON(buildErrorResponse(req.ID, -32603, category))
				return nil
			}
			finishAudit("blocked", "credential_authorization_changed", "", 0)
			writeJSON(buildErrorResponse(req.ID, -32003, "credential_authorization_changed"))
			return nil
		}
		promptArgs.CredentialBindings = credentialPreparation.Bindings
		params.Arguments, err = json.Marshal(promptArgs)
		if err != nil {
			_ = h.hub.rollbackCredentialPrepare(opCtx, credentialPreparation.completed)
			finishAudit("error", "credential_binding_encode_failed", "", 0)
			writeJSON(buildErrorResponse(req.ID, -32603, "credential_binding_encode_failed"))
			return nil
		}
	}
	tail := newAuditTailBuffer(8192)
	var bytesOut int64
	finalStatus := "success"
	finalErr := ""
	knownApplyFailure := false
	var fileWriteResult []byte
	var finalResponse []byte
	var upgradeEvidence string

	err = agent.relayToolCall(opCtx, params, h.opts.OperationTimeout, func(msg gatewayWSMessage) error {
		if method, _ := msg.Parsed["method"].(string); method == "notifications/message" {
			if p, ok := msg.Parsed["params"].(map[string]interface{}); ok {
				if chunk, ok := p["data"].(string); ok {
					bytesOut += int64(len(chunk))
					if params.Name != "doops_file_write" {
						tail.WriteString(chunk)
					}
				}
			}
			forwarded := msg.Parsed
			if p, ok := forwarded["params"].(map[string]interface{}); ok {
				p["request_id"] = req.ID
			}
			raw, marshalErr := json.Marshal(forwarded)
			if marshalErr != nil {
				return marshalErr
			}
			return writeRaw(raw)
		}
		if id, ok := msg.Parsed["id"]; ok {
			msg.Parsed["id"] = req.ID
			if result, ok := msg.Parsed["result"].(map[string]interface{}); ok {
				if action == ActionAgentUpgrade {
					finalStatus, finalErr, upgradeEvidence = upgradeAuditResult(result, params.Arguments)
					bytesOut += int64(len(resultContentText(result)))
					knownApplyFailure = finalStatus == "error" || finalStatus == "busy"
				}
				if isErr, ok := result["isError"]; ok && fmt.Sprintf("%v", isErr) == "true" {
					if action != ActionAgentUpgrade {
						finalStatus = "error"
						finalErr = resultContentText(result)
						if action == ActionAsk && params.Name == "doops_agent_prompt" {
							switch code := stringField(result, "errorCode"); code {
							case askToolTimeout:
								finalStatus, finalErr = "timeout", code
							case askToolCancelled:
								finalStatus, finalErr = "canceled", code
							case askToolFailed:
								finalErr = code
							}
						}
					}
					knownApplyFailure = true
				}
			}
			if rpcErr, ok := msg.Parsed["error"]; ok && rpcErr != nil {
				finalStatus = "error"
				finalErr = relayErrorAuditText(params.Name, rpcErr)
				if executionResponseCanceled(msg.Parsed) {
					finalStatus = "canceled"
					if finalErr == "operation canceled" {
						finalErr = "operation_canceled"
					}
				} else if params.Name == "doops_agent_prompt" {
					switch finalErr {
					case "operation_timeout", doagentPromptAdmissionTimeout, "doagent_prompt_queue_timeout":
						finalStatus = "timeout"
					}
				}
				knownApplyFailure = true
			}
			raw, _ := json.Marshal(msg.Parsed)
			if params.Name == "doops_file_write" {
				fileWriteResult = raw
				return nil
			}
			finalResponse = raw
			_ = id
		}
		return nil
	})
	if knownApplyFailure && len(credentialPreparation.completed) != 0 {
		if rollbackErr := h.hub.rollbackCredentialPrepare(context.WithoutCancel(opCtx), credentialPreparation.completed); rollbackErr != nil {
			err = fmt.Errorf("%w: credential_apply_outcome_unknown", errExecutionOutcomeUnknown)
		}
	}
	if err != nil {
		if action == ActionAgentUpgrade && upgradeAuditMutation(params.Arguments) && len(finalResponse) == 0 {
			finalStatus = "outcome_unknown"
			finalErr = "upgrade outcome unknown; the controller Job may have been created: " + sanitizeSecretText(err.Error())
			finalResponse, _ = json.Marshal(buildErrorResponse(req.ID, -32008, finalErr))
		} else if errors.Is(err, errExecutionOutcomeUnknown) {
			finalStatus = "outcome_unknown"
			// Keep the reason: the code alone cannot say which check failed.
			finalErr = sanitizeSecretText(err.Error())
			response := buildErrorResponse(req.ID, -32008, finalErr)
			var receipt *unknownMutationReceipt
			if params.Name == "doops_agent_prompt" && errors.As(err, &receipt) {
				response.Error.Data = map[string]interface{}{"structuredContent": receipt.report}
			}
			finalResponse, _ = json.Marshal(response)
		} else if errors.Is(err, errDoSQLReadCanceled) {
			finalStatus = "canceled"
			finalErr = doSQLReadCanceledCategory
			finalResponse, _ = json.Marshal(buildErrorResponse(req.ID, -32007, errDoSQLReadCanceled.Error()))
		} else if errors.Is(err, errGatewayClientDisconnected) {
			finalStatus = "canceled"
			finalErr = "gateway_client_disconnected"
			finalResponse, _ = json.Marshal(buildErrorResponse(req.ID, -32603, errGatewayClientDisconnected.Error()))
		} else if errors.Is(err, context.Canceled) {
			finalStatus = "canceled"
			finalErr = "operation_canceled"
			finalResponse, _ = json.Marshal(buildErrorResponse(req.ID, -32007, "operation canceled"))
		} else if errors.Is(err, errGatewayOperationTimeout) || errors.Is(err, context.DeadlineExceeded) {
			finalStatus, finalErr = "timeout", "operation_timeout"
			finalResponse, _ = json.Marshal(buildErrorResponse(req.ID, -32603, err.Error()))
		} else {
			finalStatus = "error"
			finalErr = sanitizeSecretText(err.Error())
			finalResponse, _ = json.Marshal(buildErrorResponse(req.ID, -32603, finalErr))
		}
	}
	auditTail := tail.String()
	if action == ActionAgentUpgrade {
		// Upgrade evidence is redacted field by field before JSON encoding.
		// Text redaction here can consume JSON quotes and corrupt the receipt.
		auditTail = upgradeEvidence
	}
	if err := finishAudit(finalStatus, finalErr, auditTail, bytesOut); err != nil {
		h.finishActiveOperation(opID)
		operationFinished = true
		response := buildErrorResponse(req.ID, -32603, "audit_completion_failed; execution outcome requires verification")
		if action == ActionAgentUpgrade {
			response = upgradeAuditCompletionError(req.ID, auditID, params.Arguments, finalResponse)
			log.Printf("[gateway] upgrade audit completion pending: audit=%d operation=%s evidence=%s",
				auditID, correlationID, upgradeEvidence)
		}
		failure, _ := json.Marshal(response)
		return writeRaw(failure)
	}
	// Persist the terminal audit state before releasing the resource lease. The
	// client response is sent last so a follow-up mutation cannot race cleanup.
	h.finishActiveOperation(opID)
	operationFinished = true
	if len(fileWriteResult) != 0 {
		return writeRaw(fileWriteResult)
	}
	if len(finalResponse) != 0 {
		return writeRaw(finalResponse)
	}
	return nil
}

func clientCapabilities(agent *GatewayAgent) map[string]interface{} {
	protocols := map[string]interface{}{}
	if agent != nil && agent.StructuredSSH {
		protocols[structuredSSHProtocol] = structuredSSHCapability()
	}
	if agent != nil && agent.ExecutionLifecycle {
		protocols[executionLifecycleProtocol] = executionLifecycleCapability()
	}
	if agent != nil && agent.UpgradeAudit {
		protocols[upgradeAuditProtocol] = upgradeAuditCapability()
	}
	capabilities := map[string]interface{}{
		"tools": map[string]interface{}{},
		// Gateway-owned protocols apply even when the selected Agent is offline.
		"gatewayProtocols": map[string]interface{}{
			cosSnapshotExecutableProtocol: cosSnapshotExecutableCapability(),
		},
	}
	if agent != nil && release.ValidCommit(agent.ReleaseCommit) {
		capabilities["releaseCommit"] = agent.ReleaseCommit
	}
	if len(protocols) != 0 {
		capabilities["protocols"] = protocols
	}
	return capabilities
}

func validateExplicitCredentialNames(names []string) error {
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if !validCredentialMetadataToken(name, 128) {
			return errors.New("credential reference name is invalid")
		}
		if _, duplicate := seen[name]; duplicate {
			return errors.New("credential reference name is duplicated")
		}
		seen[name] = struct{}{}
	}
	return nil
}

func sanitizeAgentPromptAudit(toolName, errMsg, tail string) (string, string) {
	if toolName == "doops_file_write" {
		if errMsg != "" {
			errMsg = "file_write_failed"
		}
		return errMsg, ""
	}
	if toolName != "doops_agent_prompt" {
		return sanitizeSecretText(errMsg), sanitizeSecretText(tail)
	}
	if errMsg != "" {
		errMsg = agentPromptAuditCategory(errMsg)
	}
	return errMsg, ""
}

// relayErrorAuditText keeps the audit text stable. A prompt error message is
// already a stable category, so recording the formatted envelope would make
// every prompt failure sanitize to credential_operation_failed and lose the
// category the operator and the audit need.
func relayErrorAuditText(tool string, rpcErr interface{}) string {
	if tool == "doops_agent_prompt" {
		if errorObject, ok := rpcErr.(map[string]interface{}); ok {
			if message, _ := errorObject["message"].(string); strings.TrimSpace(message) != "" {
				return strings.TrimSpace(message)
			}
		}
	}
	return fmt.Sprintf("%v", rpcErr)
}

// agentPromptAuditCategory keeps only stable categories for prompt audits. An
// unknown outcome keeps its code, and its reason only when the reason is itself
// a stable category, so the audit still says the outcome must be verified.
func agentPromptAuditCategory(errMsg string) string {
	prefix := errExecutionOutcomeUnknown.Error()
	message := strings.TrimSpace(errMsg)
	if message == prefix {
		return prefix
	}
	if strings.HasPrefix(message, prefix+":") {
		reason := strings.TrimSpace(strings.TrimPrefix(message, prefix+":"))
		if category := sanitizeCredentialCategory(reason); category == reason {
			return prefix + ": " + category
		}
		return prefix
	}
	return sanitizeCredentialCategory(message)
}

func internalCredentialTool(tool string) bool {
	return tool == "doops_credential_import" || tool == "doops_credential_materialize" ||
		tool == "doops_credential_cleanup"
}

func (h *GatewayHub) registerAgent(agent *GatewayAgent) error {
	return h.registry.Register(agent)
}

func (h *GatewayHub) unregisterAgent(agent *GatewayAgent) error {
	return h.registry.Unregister(agent)
}

func (h *GatewayHub) getAgent(cluster, instance string) *GatewayAgent {
	return h.registry.Get(cluster, instance)
}

func (h *GatewayHub) ListTargets() []GatewayTarget {
	return h.registry.List()
}

func (h *GatewayHub) authenticateUser(r *http.Request) (TokenAuth, error) {
	return authenticateGatewayUser(h.store, h.opts, r)
}

func (h *ClientService) authenticateUser(r *http.Request) (TokenAuth, error) {
	return authenticateGatewayUser(h.store, h.opts, r)
}

func authenticateGatewayUser(store *GatewayStore, opts GatewayHubOptions, r *http.Request) (TokenAuth, error) {
	token := bearerToken(r)
	if token == "" && strings.TrimSpace(r.Header.Get("Authorization")) != "" {
		return TokenAuth{}, authFailure(AuthFailureInvalid)
	}
	return verifyAndRenewUserToken(store, opts, token)
}

func verifyAndRenewUserToken(store *GatewayStore, opts GatewayHubOptions, token string) (TokenAuth, error) {
	auth, err := store.VerifyUserToken(token)
	if err != nil {
		return auth, err
	}
	if err := store.MaybeRenewPasswordLoginToken(auth, opts.LoginTokenTTL, opts.LoginTokenMaxLifetime, time.Now().UTC()); err != nil {
		logPasswordLoginRenewalError(auth.TokenID, err)
	}
	return auth, nil
}

func (h *ClientService) writeUserAuthError(w http.ResponseWriter, _ *http.Request, err error) {
	writeGatewayAuthError(w, `Bearer realm="doops-gateway"`, err)
}

func (h *GatewayHub) writeUserAuthError(w http.ResponseWriter, _ *http.Request, err error) {
	writeGatewayAuthError(w, `Bearer realm="doops-gateway"`, err)
}

func bearerToken(r *http.Request) string {
	token := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.HasPrefix(strings.ToLower(token), "bearer ") {
		return strings.TrimSpace(token[len("bearer "):])
	}
	return ""
}

type doopsErrorResponse struct {
	Error      string `json:"error"`
	Code       string `json:"code"`
	AuthDomain string `json:"auth_domain,omitempty"`
	Message    string `json:"message"`
	NextAction string `json:"next_action,omitempty"`
}

func writeGatewayAuthError(w http.ResponseWriter, challenge string, authErr error) {
	response := gatewayAuthErrorResponse(authErr)
	status := http.StatusUnauthorized
	if response.Code == "gateway_auth_unavailable" {
		status = http.StatusServiceUnavailable
		response.Error = "service_unavailable"
		challenge = ""
	}
	writeDoopsError(w, status, challenge, response)
}

func authFailureCode(err error) string {
	var failure *TokenAuthError
	if errors.As(err, &failure) {
		switch failure.Reason {
		case AuthFailureMissing:
			return "gateway_auth_missing"
		case AuthFailureExpired:
			return "gateway_auth_expired"
		case AuthFailureRevoked:
			return "gateway_auth_revoked"
		case AuthFailureUserDisabled:
			return "gateway_auth_user_disabled"
		case AuthFailureInvalidExpiry:
			return "gateway_auth_invalid_expiry"
		case AuthFailureInvalid:
			return "gateway_auth_invalid"
		}
	}
	return "gateway_auth_unavailable"
}

func gatewayAuthErrorResponse(err error) doopsErrorResponse {
	code := authFailureCode(err)
	message := "Gateway authentication could not be verified"
	nextAction := "Retry the request; if the Gateway is unavailable, inspect Gateway health and logs"
	if code != "gateway_auth_unavailable" {
		nextAction = "Run doops login --gateway <gateway-url> for this Gateway; do not replace the Gateway token with a managed credential or business API key"
	}
	switch code {
	case "gateway_auth_missing":
		message = "Gateway user token is missing"
	case "gateway_auth_invalid":
		message = "Gateway user token is invalid"
	case "gateway_auth_expired":
		message = "Gateway user token has expired"
	case "gateway_auth_revoked":
		message = "Gateway user token was revoked"
	case "gateway_auth_user_disabled":
		message = "Gateway user account is disabled"
		nextAction = "Contact a Gateway administrator to enable this account"
	case "gateway_auth_invalid_expiry":
		message = "Gateway user token has invalid expiry metadata"
		nextAction = "Contact a Gateway administrator to repair the token expiry metadata and reissue the token"
	}
	return doopsErrorResponse{
		Error:      "unauthorized",
		Code:       code,
		AuthDomain: "gateway",
		Message:    message,
		NextAction: nextAction,
	}
}

func writeDoopsError(w http.ResponseWriter, status int, challenge string, response doopsErrorResponse) {
	if challenge != "" {
		w.Header().Set("WWW-Authenticate", challenge)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(response)
}

func writeJSONHTTP(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (a *GatewayAgent) initialize() error {
	id := atomic.AddInt64(&a.reqID, 1)
	ch := a.registerPending(id, "")
	defer a.unregisterPending(id)
	if err := a.writeJSON(map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "initialize",
		"id":      id,
		"params": map[string]interface{}{
			"protocolVersion": "2024-11-05",
			"clientInfo": map[string]string{
				"name":    "doops-gateway",
				"version": "1.0",
			},
		},
	}); err != nil {
		return err
	}
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		if msg, ok, queueErr := ch.dequeue(); queueErr != nil {
			return queueErr
		} else if ok {
			if result, ok := msg.Parsed["result"].(map[string]interface{}); ok {
				a.applyInitializeResult(result)
			}
			return nil
		}
		select {
		case <-ch.notify:
		case <-timer.C:
			return fmt.Errorf("agent initialize timed out")
		case <-a.closed:
			return fmt.Errorf("agent connection closed")
		}
	}
}

func (a *GatewayAgent) applyInitializeResult(result map[string]interface{}) {
	runtimeID, _ := result["runtimeId"].(string)
	a.RuntimeID = strings.TrimSpace(runtimeID)
	serverInfo, _ := result["serverInfo"].(map[string]interface{})
	releaseCommit, _ := serverInfo["releaseCommit"].(string)
	releaseCommit = strings.TrimSpace(releaseCommit)
	if !release.ValidCommit(releaseCommit) {
		releaseCommit = ""
	}
	a.ReleaseCommit = releaseCommit
	capabilities, _ := result["capabilities"].(map[string]interface{})
	protocols, ok := capabilities["protocols"].(map[string]interface{})
	a.ExecutionLifecycle = executionLifecycleCapabilityCompatible(protocols[executionLifecycleProtocol])
	a.UpgradeAudit = upgradeAuditCapabilityCompatible(protocols[upgradeAuditProtocol])
	// An Agent that does not advertise this rejects an authfile_only request
	// outright, because it decodes the materialization request strictly.
	a.RegistryAuthfile = registryAuthfileCapabilityCompatible(protocols[registryAuthfileProtocol])
	a.COSSnapshotExecutable = cosSnapshotExecutableCapabilityCompatible(protocols[cosSnapshotExecutableProtocol])
	if !ok {
		return
	}
	if credentialPlanCapabilityCompatible(protocols["credentialPlan"]) {
		a.CredentialPlanProtocol = credentialPlanProtocolV2
	}
	a.StructuredSSH = structuredSSHCapabilityCompatible(protocols[structuredSSHProtocol])
}

func (a *GatewayAgent) readLoop(lease, initialReadTimeout time.Duration) {
	defer close(a.closed)
	defer a.conn.Close()
	a.conn.SetReadDeadline(time.Now().Add(initialReadTimeout))
	a.conn.SetPongHandler(func(payload string) error {
		a.heartbeat(payload)
		return a.conn.SetReadDeadline(time.Now().Add(lease))
	})
	for {
		refreshActivity := func() {
			now := time.Now()
			_ = a.conn.SetReadDeadline(now.Add(lease))
			a.touchMemory(now.UTC())
			a.acknowledgeReadProgress(now)
		}
		msgType, data, err := readWebSocketMessage(a.conn, refreshActivity)
		if err != nil {
			if !errors.Is(err, io.EOF) && !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				log.Printf("[gateway] agent read failed: %s: %v", a.Key, err)
			}
			return
		}
		refreshActivity()
		if msgType != websocket.TextMessage {
			continue
		}
		var parsed map[string]interface{}
		if err := json.Unmarshal(data, &parsed); err != nil {
			continue
		}
		msg := gatewayWSMessage{Raw: append([]byte(nil), data...), Parsed: parsed}
		if id, ok := numericID(parsed["id"]); ok {
			a.pendingMu.Lock()
			queue := a.pending[id]
			a.pendingMu.Unlock()
			if queue != nil {
				queue.observeTerminal(msg)
				if err := queue.enqueue(msg); err != nil && !errors.Is(err, errGatewayMessageQueueClosed) {
					log.Printf("[gateway] agent message queue failed: %s id=%d: %v", a.Key, id, err)
					a.cancelPending(queue, true)
				}
				continue
			}
		}
		if method, _ := parsed["method"].(string); method == "notifications/message" {
			requestID, ok := requestIDFromNotification(parsed)
			if !ok {
				log.Printf("[gateway] agent protocol violation: notification missing request_id: %s", a.Key)
				return
			}
			a.pendingMu.Lock()
			queue := a.activeByRequest[requestID]
			a.pendingMu.Unlock()
			if queue != nil {
				if err := queue.enqueue(msg); err != nil && !errors.Is(err, errGatewayMessageQueueClosed) {
					log.Printf("[gateway] agent notification queue failed: %s request_id=%d: %v", a.Key, requestID, err)
					a.cancelPending(queue, true)
				}
			}
		}
	}
}

func (a *GatewayAgent) acknowledgeReadProgress(now time.Time) {
	if !a.lastProgressAck.IsZero() && now.Sub(a.lastProgressAck) < gatewayReadProgressAckInterval {
		return
	}
	a.lastProgressAck = now
	// An unsolicited Pong proves that the gateway consumed bytes before a large message finishes.
	writeHeartbeatControl(
		a.conn,
		&a.dataWriteState,
		websocket.PongMessage,
		[]byte("read-progress"),
		gatewayReadProgressAckWriteTimeout,
	)
}

func (a *GatewayAgent) cancelPending(queue *gatewayMessageQueue, async bool) <-chan struct{} {
	if queue == nil || queue.cancelMethod == "" {
		return nil
	}
	send := func() {
		defer close(queue.cancelDone)
		if err := a.writeJSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"method":  queue.cancelMethod,
			"id":      fmt.Sprintf("cancel-%d", queue.requestID),
			"params":  map[string]interface{}{"id": queue.requestID},
		}); err != nil {
			log.Printf("[gateway] cancel failed: %s id=%d method=%s: %v", a.Key, queue.requestID, queue.cancelMethod, err)
		}
	}
	queue.cancelOnce.Do(func() {
		if async {
			go send()
			return
		}
		send()
	})
	return queue.cancelDone
}

func (a *GatewayAgent) waitPendingCancel(queue *gatewayMessageQueue) error {
	select {
	case <-queue.terminal:
		return queue.terminalErr
	default:
	}
	a.cancelPending(queue, true)
	timer := time.NewTimer(executionCancellationTimeout)
	defer timer.Stop()
	select {
	case <-queue.terminal:
		return queue.terminalErr
	case <-a.closed:
		return fmt.Errorf("%w: agent disconnected before acknowledging cancellation", errExecutionOutcomeUnknown)
	case <-timer.C:
		return fmt.Errorf("%w: agent did not acknowledge cancellation within %s", errExecutionOutcomeUnknown, executionCancellationTimeout)
	}
}

func (a *GatewayAgent) pingLoop(registry *AgentRegistry, lease time.Duration) {
	interval := lease / 3
	if interval <= 0 {
		interval = 30 * time.Second
	}
	if !writeHeartbeatPing(a.conn, &a.dataWriteState, []byte("ping"), 10*time.Second) {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if !writeHeartbeatPing(a.conn, &a.dataWriteState, []byte("ping"), 10*time.Second) {
				return
			}
			if registry != nil {
				a.stateMu.RLock()
				lastSeen := a.LastSeen
				a.stateMu.RUnlock()
				ctx, cancel := context.WithTimeout(context.Background(), gatewayAgentTouchTimeout)
				err := registry.TouchContext(ctx, a, lastSeen)
				cancel()
				if err != nil {
					log.Printf("[gateway] persist agent last-seen failed: %s generation=%d: %v", a.Key, a.Generation, err)
				}
			}
		case <-a.closed:
			return
		}
	}
}

func (a *GatewayAgent) touchMemory(lastSeen time.Time) {
	a.stateMu.Lock()
	a.LastSeen = lastSeen
	a.stateMu.Unlock()
}

func (a *GatewayAgent) heartbeat(payload string) {
	now := time.Now().UTC()
	a.stateMu.Lock()
	a.HeartbeatAt = now
	a.heartbeatPayload = payload
	a.LastSeen = now
	a.stateMu.Unlock()
}

// A pre-admission category alone is not enough: cleanup_unknown can still run.
func promptReplyProvesNoExecution(tool string, response map[string]interface{}) bool {
	if tool != "doops_agent_prompt" || response["result"] != nil || !promptPreAdmissionReply(response) {
		return false
	}
	rpcError, _ := response["error"].(map[string]interface{})
	message, _ := rpcError["message"].(string)
	return strings.TrimSpace(message) != doagentPromptCleanupUnknown
}

func promptReplyUnknownError(tool string, response map[string]interface{}) error {
	if tool != "doops_agent_prompt" {
		return nil
	}
	if executionResponseUnknown(response) {
		return executionUnknownFromResponse(response)
	}
	rpcError, _ := response["error"].(map[string]interface{})
	message, _ := rpcError["message"].(string)
	if strings.TrimSpace(message) == doagentPromptCleanupUnknown {
		return fmt.Errorf("%w: %s", errExecutionOutcomeUnknown, doagentPromptCleanupUnknown)
	}
	return nil
}

var errGatewayOperationTimeout = errors.New("operation timed out")

func (a *GatewayAgent) relayToolCall(ctx context.Context, params api.ToolCallParams, timeout time.Duration, forward func(gatewayWSMessage) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	id := atomic.AddInt64(&a.reqID, 1)
	ch := a.registerPending(id, "tools/cancel")
	defer a.unregisterPending(id)
	uncertainIfInterrupted := interruptionNeedsReconciliation(params.Name, params.Arguments)
	var prompt api.AgentPromptParams
	readSQL := params.Name == "doops_agent_prompt" && json.Unmarshal(params.Arguments, &prompt) == nil &&
		strings.EqualFold(strings.TrimSpace(prompt.Operation), "ask") && doSQLDirectRequested(prompt) && !doSQLCopyRequested(prompt)
	uncertainWithoutTerminal := readSQL || missingTerminalNeedsReconciliation(params.Name, params.Arguments)
	sessionID := extractSession(params.Arguments)
	routeNotifications := params.Name != "doops_task_status"
	a.pendingMu.Lock()
	if routeNotifications {
		if a.activeByRequest == nil {
			a.activeByRequest = make(map[int64]*gatewayMessageQueue)
		}
		a.activeByRequest[id] = ch
		if sessionID != "" {
			if a.activeSessions == nil {
				a.activeSessions = make(map[string]int)
			}
			a.activeSessions[sessionID]++
		}
	}
	a.pendingMu.Unlock()
	defer func() {
		a.pendingMu.Lock()
		if routeNotifications && a.activeByRequest[id] == ch {
			delete(a.activeByRequest, id)
		}
		if routeNotifications && sessionID != "" {
			if count := a.activeSessions[sessionID]; count <= 1 {
				delete(a.activeSessions, sessionID)
			} else {
				a.activeSessions[sessionID] = count - 1
			}
		}
		a.pendingMu.Unlock()
	}()

	if err := a.writeJSON(map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "tools/call",
		"id":      id,
		"params": map[string]interface{}{
			"name":      params.Name,
			"arguments": params.Arguments,
		},
	}); err != nil {
		if uncertainWithoutTerminal {
			return fmt.Errorf("%w: dispatch failed: %v", errExecutionOutcomeUnknown, err)
		}
		return fmt.Errorf("%w: dispatch failed: %v", errGatewayAgentDisconnected, err)
	}

	waitCtx, cancelWait := context.WithTimeout(ctx, timeout)
	defer cancelWait()
	var forwardErr error
	forwardMessage := func(msg gatewayWSMessage) (bool, error) {
		if msg.Parsed == nil {
			return false, nil
		}
		if _, ok := msg.Parsed["id"]; ok {
			// An unknown prompt must not enter the known-failure callback:
			// it can still be using the credentials prepared for this request.
			if err := promptReplyUnknownError(params.Name, msg.Parsed); err != nil {
				return true, err
			}
			if result, ok := msg.Parsed["result"].(map[string]interface{}); readSQL && ok {
				if _, ack := result["canceled"]; ack {
					return false, nil
				}
			}
			if readSQL && executionResponseCanceled(msg.Parsed) {
				rpcError, _ := msg.Parsed["error"].(map[string]interface{})
				data, _ := rpcError["data"].(map[string]interface{})
				if data["category"] == doSQLReadCanceledCategory && data["sqlstate"] == "57014" {
					return true, errDoSQLReadCanceled
				}
				return true, errExecutionOutcomeUnknown
			}
			if (uncertainWithoutTerminal && executionResponseUnknown(msg.Parsed)) ||
				remoteMutationFailed(params.Name, params.Arguments, msg.Parsed) {
				return true, executionUnknownFromResponse(msg.Parsed)
			}
			if uncertainIfInterrupted && !promptReplyProvesNoExecution(params.Name, msg.Parsed) &&
				(ctx.Err() != nil || executionResponseCanceled(msg.Parsed)) {
				return true, fmt.Errorf("%w: mutation was interrupted before it finished", errExecutionOutcomeUnknown)
			}
			if forwardErr == nil {
				if err := forward(msg); err != nil {
					forwardErr = err
				}
			}
			return true, forwardErr
		}
		if forwardErr == nil {
			if err := forward(msg); err != nil {
				forwardErr = err
			}
		}
		return false, nil
	}
	cancelAndWait := func() (bool, error) {
		err := a.waitPendingCancel(ch)
		// A terminal may race the disconnect selected by waitPendingCancel.
		// Read the saved reply only after its publication barrier has closed.
		select {
		case <-ch.terminal:
			if err := promptReplyUnknownError(params.Name, ch.terminalMessage.Parsed); err != nil {
				return true, err
			}
			if readSQL || promptReplyProvesNoExecution(params.Name, ch.terminalMessage.Parsed) {
				return forwardMessage(ch.terminalMessage)
			}
		default:
		}
		if err != nil {
			if !uncertainWithoutTerminal && errors.Is(err, errExecutionOutcomeUnknown) {
				return false, nil
			}
			return true, err
		}
		// Local termination cannot fence a remote controller or detached process.
		if uncertainIfInterrupted {
			return true, errExecutionOutcomeUnknown
		}
		return false, nil
	}
	interruptionResult := func() (bool, error) {
		if waitCtx.Err() == nil {
			return false, nil
		}
		if done, err := cancelAndWait(); done {
			return true, err
		}
		if err := ctx.Err(); err != nil {
			return true, err
		}
		return true, errGatewayOperationTimeout
	}
	for {
		// A busy output queue must not starve cancellation or the deadline.
		if done, err := interruptionResult(); done {
			return err
		}
		if msg, ok, queueErr := ch.dequeue(); queueErr != nil {
			if done, err := cancelAndWait(); done {
				return err
			}
			return queueErr
		} else if ok {
			if done, err := forwardMessage(msg); done {
				return err
			}
			continue
		}

		select {
		case <-waitCtx.Done():
		case <-ch.notify:
		case <-a.closed:
			for {
				if done, err := interruptionResult(); done {
					return err
				}
				msg, ok, queueErr := ch.dequeue()
				if queueErr != nil {
					if done, err := cancelAndWait(); done {
						return err
					}
					return queueErr
				}
				if !ok {
					break
				}
				if done, err := forwardMessage(msg); done {
					return err
				}
			}
			select {
			case <-ch.terminal:
				if ch.terminalErr != nil &&
					!(errors.Is(ch.terminalErr, errExecutionOutcomeUnknown) && !uncertainWithoutTerminal) {
					return ch.terminalErr
				}
				_, err := forwardMessage(ch.terminalMessage)
				return err
			default:
				if !uncertainWithoutTerminal {
					return errGatewayAgentDisconnected
				}
				return fmt.Errorf("%w: agent disconnected before a terminal reply", errExecutionOutcomeUnknown)
			}
		}
	}
}

func (a *GatewayAgent) registerPending(id int64, cancelMethod string) *gatewayMessageQueue {
	queue := newGatewayMessageQueue(a.messageBudget, gatewayMessageQueueMaxBytes, id, cancelMethod)
	a.pendingMu.Lock()
	a.pending[id] = queue
	a.pendingMu.Unlock()
	return queue
}

func (a *GatewayAgent) unregisterPending(id int64) {
	a.pendingMu.Lock()
	queue := a.pending[id]
	delete(a.pending, id)
	a.pendingMu.Unlock()
	if queue != nil {
		queue.close()
	}
}

func (a *GatewayAgent) writeJSON(v interface{}) error {
	return writeWebSocketJSON(a.conn, &a.writeMu, &a.dataWriteState, v, agentWSDataWriteTimeout)
}

func (a *GatewayAgent) acquireActionLease(ctx context.Context, action GatewayAction, resourceKey string, maxQueued int, wait time.Duration) (*operationLease, error) {
	return a.acquireActionLeaseForSession(ctx, action, resourceKey, "", maxQueued, wait)
}

func exclusiveLockKey(resourceKey, session string) string {
	if strings.HasPrefix(resourceKey, "target-mutation:") && strings.TrimSpace(session) != "" {
		return "session-mutation:" + strings.TrimPrefix(resourceKey, "target-mutation:") + "/" + strings.TrimSpace(session)
	}
	return resourceKey
}

func sessionForResourceKey(resourceKey string) string {
	for _, prefix := range []string{"session-mutation:", "workspace:"} {
		if strings.HasPrefix(resourceKey, prefix) {
			value := strings.TrimPrefix(resourceKey, prefix)
			if prefix == "session-mutation:" {
				if index := strings.LastIndexByte(value, '/'); index >= 0 {
					return value[index+1:]
				}
			}
			return value
		}
	}
	return ""
}

func (a *GatewayAgent) sessionResourceHeldLocked(lockKey string) bool {
	if strings.HasPrefix(lockKey, "target:") {
		// A workspace slot can contain only waiters. Every granted slot counts as a reader.
		return a.writers > 0 || a.readers > 0
	}
	session := sessionForResourceKey(lockKey)
	if session == "" {
		return false
	}
	shellLike := strings.HasPrefix(lockKey, "session-mutation:")
	for key, count := range a.sessionWriters {
		if count == 0 {
			continue
		}
		if strings.HasPrefix(key, "target:") {
			return true
		}
		// Shell, SSH and apply calls in one session run concurrently; only
		// workspace resources and Agent upgrade exclude them.
		if shellLike && strings.HasPrefix(key, "session-mutation:") {
			continue
		}
		if sessionForResourceKey(key) == session {
			return true
		}
	}
	for key, slot := range a.resources {
		if slot != nil && len(slot.slot) == 0 && (sessionForResourceKey(key) == session || slot.session == session) {
			return true
		}
	}
	return false
}

func (a *GatewayAgent) conflictingResourceLocked(resourceKey, session string) string {
	for key, count := range a.sessionWriters {
		if count > 0 && strings.HasPrefix(key, "target:") {
			return key
		}
	}
	if strings.HasPrefix(resourceKey, "target:") {
		if a.writers > 0 {
			return resourceKey
		}
		return ""
	}
	workspace := sessionForResourceKey(resourceKey)
	for key, count := range a.sessionWriters {
		holder := sessionForResourceKey(key)
		if count > 0 && ((session != "" && holder == session) || (workspace != "" && holder == workspace)) {
			return key
		}
	}
	for key, slot := range a.resources {
		if key != resourceKey && slot != nil && len(slot.slot) == 0 &&
			((session != "" && (slot.session == session || sessionForResourceKey(key) == session)) ||
				(workspace != "" && slot.session == workspace)) {
			return key
		}
	}
	return ""
}

func (a *GatewayAgent) acquireActionLeaseForSession(ctx context.Context, action GatewayAction, resourceKey, session string, maxQueued int, wait time.Duration) (*operationLease, error) {
	if (strings.HasPrefix(resourceKey, "target-mutation:") || strings.HasPrefix(resourceKey, "nginx:")) && validateSession(strings.TrimSpace(session)) != nil {
		return nil, fmt.Errorf("a valid session is required for mutation")
	}
	if isExclusiveOperation(action, resourceKey) {
		lockKey := exclusiveLockKey(resourceKey, session)
		if err := a.acquireExclusive(ctx, lockKey, maxQueued, wait); err != nil {
			return nil, err
		}
		return newOperationLease(func() { a.releaseExclusive(lockKey) }), nil
	}
	if isConcurrentQuery(action, resourceKey) {
		if err := a.acquireReadOnly(); err != nil {
			return nil, err
		}
		return newOperationLease(a.releaseReadOnly), nil
	}
	if resourceKey != "" {
		if err := a.acquireResource(ctx, action, resourceKey, session, maxQueued, wait); err != nil {
			return nil, err
		}
		return newOperationLease(func() { a.releaseResource(resourceKey) }), nil
	}
	if isConcurrentReadOnlyAction(action) {
		if err := a.acquireReadOnly(); err != nil {
			return nil, err
		}
		return newOperationLease(a.releaseReadOnly), nil
	}
	lockKey := exclusiveLockKey(resourceKey, session)
	if err := a.acquireExclusive(ctx, lockKey, maxQueued, wait); err != nil {
		return nil, err
	}
	return newOperationLease(func() { a.releaseExclusive(lockKey) }), nil
}

func (a *GatewayAgent) acquireExclusive(ctx context.Context, lockKey string, maxQueued int, wait time.Duration) error {
	if strings.TrimSpace(lockKey) == "" {
		return fmt.Errorf("operation resource is required")
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	queued := false
	defer func() {
		if !queued {
			return
		}
		a.queueMu.Lock()
		if a.queued > 0 {
			a.queued--
		}
		a.queueMu.Unlock()
	}()

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		a.opsMu.Lock()
		admissible := !a.draining && !a.sessionResourceHeldLocked(lockKey)
		if admissible {
			a.writers++
			if a.sessionWriters == nil {
				a.sessionWriters = make(map[string]int)
			}
			a.sessionWriters[lockKey]++
			a.opsMu.Unlock()
			return nil
		}
		changed := a.stateChangedLocked()
		a.opsMu.Unlock()

		if !queued {
			if maxQueued <= 0 {
				return fmt.Errorf("target busy")
			}
			a.queueMu.Lock()
			if a.queued >= maxQueued {
				a.queueMu.Unlock()
				return fmt.Errorf("target queue full")
			}
			a.queued++
			a.queueMu.Unlock()
			queued = true
		}

		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		case <-a.closed:
			return fmt.Errorf("target offline")
		case <-timer.C:
			return fmt.Errorf("target queue timeout")
		}
	}
}

func (a *GatewayAgent) releaseExclusive(lockKey string) {
	a.opsMu.Lock()
	if a.sessionWriters[lockKey] == 0 {
		a.opsMu.Unlock()
		return
	}
	a.writers--
	if a.sessionWriters[lockKey]--; a.sessionWriters[lockKey] <= 0 {
		delete(a.sessionWriters, lockKey)
	}
	a.opsMu.Unlock()
	a.signalStateChange()
}

func (a *GatewayAgent) acquireReadOnly() error {
	a.opsMu.Lock()
	if a.draining || a.targetExclusiveHeldLocked() {
		a.opsMu.Unlock()
		return fmt.Errorf("target busy")
	}
	a.readers++
	a.opsMu.Unlock()
	return nil
}

func (a *GatewayAgent) targetExclusiveHeldLocked() bool {
	for key, count := range a.sessionWriters {
		if count > 0 && strings.HasPrefix(key, "target:") {
			return true
		}
	}
	return false
}

func (a *GatewayAgent) releaseReadOnly() {
	a.opsMu.Lock()
	if a.readers > 0 {
		a.readers--
	}
	a.opsMu.Unlock()
	a.signalStateChange()
}

func (a *GatewayAgent) acquireResource(ctx context.Context, action GatewayAction, key, session string, maxQueued int, wait time.Duration) error {
	a.opsMu.Lock()
	defer a.opsMu.Unlock()
	slot := a.resources[key]
	if slot == nil {
		if a.resources == nil {
			a.resources = make(map[string]*agentResourceSlot)
		}
		slot = &agentResourceSlot{slot: make(chan struct{}, 1)}
		slot.slot <- struct{}{}
		a.resources[key] = slot
	}
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
			slot.queued--
		}
		if slot.queued == 0 && len(slot.slot) == 1 {
			delete(a.resources, key)
		}
	}()

	// One queue entry and deadline cover both the session mutation and the
	// workspace slot, even when another holder wins the race after a wakeup.
	waitOn := func(ready <-chan struct{}) error {
		if timer == nil {
			if maxQueued <= 0 {
				return fmt.Errorf("target busy")
			}
			if slot.queued >= maxQueued {
				return fmt.Errorf("target queue full")
			}
			slot.queued++
			timer = time.NewTimer(wait)
		}
		a.opsMu.Unlock()
		defer a.opsMu.Lock()
		select {
		case <-ready:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return fmt.Errorf("target queue timeout")
		case <-a.closed:
			return fmt.Errorf("target offline")
		}
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if a.draining {
			return fmt.Errorf("target busy")
		}
		if blocker := a.conflictingResourceLocked(key, session); blocker != "" {
			if action != ActionAsk || !strings.HasPrefix(key, "workspace:") || !strings.HasPrefix(blocker, "session-mutation:") {
				return fmt.Errorf("target busy")
			}
			if err := waitOn(a.stateChangedLocked()); err != nil {
				return err
			}
			continue
		}
		select {
		case <-slot.slot:
		default:
			if err := waitOn(slot.slot); err != nil {
				return err
			}
			if ctx.Err() != nil || a.draining || a.conflictingResourceLocked(key, session) != "" {
				slot.slot <- struct{}{}
				continue
			}
		}
		slot.session = session
		a.readers++
		return nil
	}
}

func (a *GatewayAgent) releaseResource(key string) {
	a.opsMu.Lock()
	slot := a.resources[key]
	if slot != nil {
		select {
		case slot.slot <- struct{}{}:
		default:
		}
		if slot.queued == 0 && len(slot.slot) == 1 {
			delete(a.resources, key)
		}
	}
	if a.readers > 0 {
		a.readers--
	}
	a.opsMu.Unlock()
	a.signalStateChange()
}

func (a *GatewayAgent) stateChangedLocked() chan struct{} {
	if a.stateChanged == nil {
		a.stateChanged = make(chan struct{})
	}
	return a.stateChanged
}

func (a *GatewayAgent) signalStateChange() {
	a.opsMu.Lock()
	previous := a.stateChangedLocked()
	a.stateChanged = make(chan struct{})
	a.opsMu.Unlock()
	close(previous)
}

func isConcurrentReadOnlyAction(action GatewayAction) bool {
	switch action {
	case ActionRead, ActionInfo, ActionCheck:
		return true
	default:
		return false
	}
}

func isConcurrentQuery(action GatewayAction, resourceKey string) bool {
	return isConcurrentReadOnlyAction(action) ||
		strings.HasPrefix(resourceKey, "query:")
}

func isExclusiveOperation(action GatewayAction, resourceKey string) bool {
	return action == ActionAgentUpgrade || strings.HasPrefix(resourceKey, "target:") || strings.HasPrefix(resourceKey, "target-mutation:") ||
		(resourceKey == "" && !isConcurrentReadOnlyAction(action))
}

type targetRuntimeState struct {
	busy      bool
	status    string
	reason    string
	activeOps int
	queuedOps int
	resources []string
	sessions  []string
}

func (a *GatewayAgent) runtimeState() targetRuntimeState {
	state := targetRuntimeState{status: "idle"}
	a.opsMu.Lock()
	writers := a.writers
	readers := a.readers
	targetExclusive := false
	for lockKey, count := range a.sessionWriters {
		if count > 0 && strings.HasPrefix(lockKey, "target:") {
			targetExclusive = true
			break
		}
	}
	resources := make([]string, 0, len(a.resources))
	resourceQueued := 0
	for key, slot := range a.resources {
		resources = append(resources, key)
		if slot != nil {
			resourceQueued += slot.queued
		}
	}
	a.opsMu.Unlock()
	sort.Strings(resources)

	a.queueMu.Lock()
	queued := a.queued
	a.queueMu.Unlock()

	a.pendingMu.Lock()
	sessions := make([]string, 0, len(a.activeSessions))
	for sessionID := range a.activeSessions {
		sessions = append(sessions, sessionID)
	}
	a.pendingMu.Unlock()
	sort.Strings(sessions)

	state.activeOps = writers + readers
	state.queuedOps = queued + resourceQueued
	state.resources = resources
	state.sessions = sessions

	switch {
	case targetExclusive:
		state.busy = true
		state.status = "busy"
		state.reason = "exclusive_operation"
	case state.activeOps > 0 || len(resources) > 0 || state.queuedOps > 0:
		state.status = "active"
	}
	return state
}

func (a *GatewayAgent) snapshot() GatewayTarget {
	a.stateMu.RLock()
	target := GatewayTarget{
		Cluster:       a.Cluster,
		Instance:      a.Instance,
		Key:           a.Key,
		Remote:        a.Remote,
		TokenID:       a.TokenID,
		ReleaseCommit: a.ReleaseCommit,
		ConnectedAt:   a.ConnectedAt,
		LastSeen:      a.LastSeen,
	}
	a.stateMu.RUnlock()
	state := a.runtimeState()
	target.Busy = state.busy
	target.Status = state.status
	target.BusyReason = state.reason
	target.ActiveOps = state.activeOps
	target.QueuedOps = state.queuedOps
	target.Resources = state.resources
	target.Sessions = state.sessions
	return target
}

func numericID(v interface{}) (int64, bool) {
	switch id := v.(type) {
	case float64:
		return int64(id), true
	case int64:
		return id, true
	case int:
		return int64(id), true
	default:
		return 0, false
	}
}

func actionForTool(tool string, args json.RawMessage) GatewayAction {
	switch tool {
	case "doops_web":
		operation := strings.ToLower(strings.TrimSpace(extractStringArg(args, "operation")))
		if containsWebString(webReadOperations, operation) {
			return ActionAsk
		}
		if containsWebString(webMutationOperations, operation) {
			return ActionExec
		}
		return ""
	case "doops_shell", "doops_ssh", "doops_bg", "doops_task_status", "doops_k8s":
		return ActionExec
	case "doops_agent_prompt":
		if extractStringArg(args, "operation") == "apply" {
			return ActionExec
		}
		switch extractStringArg(args, "mode") {
		case "metadata", "history":
			return ActionAsk
		default:
			return ActionAsk
		}
	case "doops_file_read":
		return ActionRead
	case "doops_workspace_read":
		return ActionRead
	case "doops_file_write":
		return ActionWrite
	case "doops_cos_sync":
		return ActionWrite
	case "doops_node_info", "doops_database_inventory":
		return ActionInfo
	case "doops_check_deployment":
		return ActionCheck
	case "doops_clean_workspace":
		return ActionClean
	case "doops_agent_upgrade":
		return ActionAgentUpgrade
	case "doops_credential_materialize":
		return ActionCredentialUse
	case "doops_credential_import":
		return ActionCredentialRotate
	case "doops_credential_cleanup":
		return ActionCredentialRevoke
	case "doops_workspace_begin", "doops_workspace_chunk", "doops_workspace_commit":
		return ActionPush
	case "doops_workspace_pull_begin", "doops_workspace_pull_chunk":
		return ActionPull
	default:
		return ""
	}
}

func resourceKeyForTool(action GatewayAction, tool string, args json.RawMessage, cluster, instance string) string {
	targetMutation := "target-mutation:" + cluster + "/" + instance
	if tool == "doops_credential_import" {
		return "query:credential:" + cluster + "/" + instance + "/" +
			extractStringArg(args, "namespace") + "/" + extractStringArg(args, "secret_name")
	}
	if tool == "doops_agent_prompt" {
		var prompt api.AgentPromptParams
		if json.Unmarshal(args, &prompt) == nil &&
			(len(prompt.CredentialRefs) != 0 || len(prompt.CredentialBindings) != 0 ||
				(prompt.Skill == "dosql" && prompt.CredentialMode == "auto")) {
			return targetMutation
		}
	}
	if tool == "doops_web" || (tool == "doops_agent_prompt" && extractStringArg(args, "skill") == "doweb") {
		if action == ActionExec {
			// Nginx activation and reload are shared across website sessions.
			return "nginx:" + cluster + "/" + instance
		}
		if tool == "doops_web" {
			return "query:web:" + cluster + "/" + instance
		}
	}
	switch action {
	case ActionExec:
		if tool == "doops_agent_prompt" && extractStringArg(args, "operation") == "apply" {
			return targetMutation
		}
		if tool == "doops_shell" || tool == "doops_bg" || tool == "doops_ssh" {
			// A session selects a workspace, not an OS or remote-resource sandbox.
			return targetMutation
		}
		if tool == "doops_task_status" {
			return "query:task:" + extractStringArg(args, "task_id")
		}
		if tool == "doops_k8s" {
			if !gatewayK8SReadOnly(extractStringArg(args, "operation")) {
				return targetMutation
			}
			if resource := extractStringArg(args, "resource"); resource != "" {
				return "query:k8s:" + extractStringArg(args, "namespace") + "/" + canonicalK8SResource(resource)
			}
			if name := extractStringArg(args, "name"); name != "" {
				return "query:k8s:" + extractStringArg(args, "namespace") + "/" + extractStringArg(args, "kind") + "/" + name
			}
			return "query:k8s:" + cluster + "/" + instance
		}
		return targetMutation
	case ActionAsk:
		if sessionID := extractSession(args); sessionID != "" {
			return "workspace:" + sessionID
		}
	case ActionClean:
		workspace := strings.TrimSpace(extractStringArg(args, "workspace"))
		if validateSession(workspace) == nil {
			return "workspace:" + workspace
		}
		return targetMutation
	case ActionPush, ActionWrite:
		if sessionID := extractSession(args); sessionID != "" {
			return "workspace:" + sessionID
		}
		return targetMutation
	case ActionPull, ActionRead:
		if sessionID := extractSession(args); sessionID != "" {
			return "workspace:" + sessionID
		}
	case ActionCredentialUse, ActionCredentialRevoke:
		return targetMutation
	case ActionAgentUpgrade:
		return "target:" + cluster + "/" + instance
	case ActionCredentialMetadata:
		if sessionID := extractSession(args); sessionID != "" {
			return "workspace:" + sessionID
		}
	}
	return ""
}

func gatewayK8SReadOnly(operation string) bool {
	return slices.Contains(k8sReadOperations, strings.ToLower(strings.TrimSpace(operation)))
}

func extractSession(args json.RawMessage) string {
	var m map[string]interface{}
	if json.Unmarshal(args, &m) != nil {
		return ""
	}
	for _, key := range []string{"session_id", "session"} {
		if v, _ := m[key].(string); v != "" {
			return v
		}
	}
	return ""
}

func extractStringArg(args json.RawMessage, key string) string {
	var m map[string]interface{}
	if json.Unmarshal(args, &m) != nil {
		return ""
	}
	v, _ := m[key].(string)
	return strings.TrimSpace(v)
}

func requestIDFromNotification(parsed map[string]interface{}) (int64, bool) {
	params, _ := parsed["params"].(map[string]interface{})
	if params == nil {
		return 0, false
	}
	requestID, ok := params["request_id"]
	if !ok {
		return 0, false
	}
	return numericID(requestID)
}

func summarizeToolCall(tool string, args json.RawMessage) string {
	var m map[string]interface{}
	if json.Unmarshal(args, &m) != nil {
		return tool
	}
	switch tool {
	case "doops_shell", "doops_bg":
		if cmd, _ := m["command"].(string); cmd != "" {
			return trimTail(cmd, 512)
		}
	case "doops_ssh":
		host, _ := m["host"].(string)
		command, _ := m["command"].([]interface{})
		return fmt.Sprintf("doops_ssh host=%s remote_args=%d", strings.TrimSpace(host), len(command))
	case "doops_agent_prompt":
		operation, _ := m["operation"].(string)
		summary := "doops_agent_prompt operation=" + strings.ToLower(strings.TrimSpace(operation))
		if skill, _ := m["skill"].(string); skill == "doweb" || skill == "dosql" {
			summary += " skill=" + skill
		}
		return summary
	case "doops_web":
		return trimTail("doops_web "+extractStringArg(args, "operation")+" site="+extractStringArg(args, "site_id"), 512)
	case "doops_file_read", "doops_file_write":
		if p, _ := m["path"].(string); p != "" {
			return tool + " " + p
		}
	case "doops_workspace_pull_begin":
		if sessionID, _ := m["session_id"].(string); sessionID != "" {
			return tool + " " + sessionID
		}
	case "doops_agent_upgrade":
		return summarizeUpgradeAudit(args)
	case "doops_database_inventory":
		return "doops_database_inventory"
	case "doops_k8s":
		operation, _ := m["operation"].(string)
		resource, _ := m["resource"].(string)
		if resource == "" {
			kind, _ := m["kind"].(string)
			name, _ := m["name"].(string)
			resource = strings.Trim(strings.TrimSpace(kind)+"/"+strings.TrimSpace(name), "/")
		}
		namespace, _ := m["namespace"].(string)
		summary := "doops_k8s " + strings.TrimSpace(operation) + " namespace=" + strings.TrimSpace(namespace) + " resource=" + resource
		if replicas, ok := m["replicas"]; ok {
			summary += " replicas=" + fmt.Sprint(replicas)
		}
		if container, _ := m["container"].(string); container != "" {
			summary += " container=" + container
		}
		if image, _ := m["image"].(string); image != "" {
			summary += " image=" + image
		}
		return trimTail(summary, 512)
	}
	return tool
}

func canonicalK8SResource(resource string) string {
	parts := strings.SplitN(strings.TrimSpace(resource), "/", 2)
	if len(parts) != 2 {
		return strings.TrimSpace(resource)
	}
	aliases := map[string]string{
		"deploy": "deployment", "deployments": "deployment",
		"ds": "daemonset", "daemonsets": "daemonset",
		"sts": "statefulset", "statefulsets": "statefulset",
		"po": "pod", "pods": "pod",
	}
	kind := parts[0]
	if canonical := aliases[kind]; canonical != "" {
		kind = canonical
	}
	return kind + "/" + parts[1]
}

func tunnelKey(cluster, instance string) string {
	cluster = strings.TrimSpace(cluster)
	instance = strings.TrimSpace(instance)
	return cluster + "/" + instance
}
