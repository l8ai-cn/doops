package server

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/user/doops/agent/api"
)

type GatewayHubOptions struct {
	AgentLease              time.Duration
	OperationTimeout        time.Duration
	TargetQueueTimeout      time.Duration
	TargetReconnectGrace    time.Duration
	LoginTokenTTL           time.Duration
	MaxConcurrentOperations int
	MaxConcurrentPerUser    int
	MaxQueuedPerTarget      int
}

type GatewayHub struct {
	store        *GatewayStore
	opts         GatewayHubOptions
	registry     *AgentRegistry
	registration *AgentRegistrationHandler
	client       *ClientService

	scheduler            *Scheduler
	credentialMutationMu sync.Mutex
}

// AttachScheduler 注入定时巡检调度器，供 /v1/admin/jobs run-now 等接口使用。
func (h *GatewayHub) AttachScheduler(sched *Scheduler) {
	h.scheduler = sched
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
)

type GatewayActiveOperation struct {
	ID             string        `json:"id"`
	UserID         string        `json:"user_id"`
	TokenID        string        `json:"token_id,omitempty"`
	Cluster        string        `json:"cluster"`
	Instance       string        `json:"instance"`
	Action         GatewayAction `json:"action"`
	Session        string        `json:"session,omitempty"`
	CommandSummary string        `json:"command_summary,omitempty"`
	Kind           string        `json:"kind"`
	StartedAt      time.Time     `json:"started_at"`
	AgeSeconds     int64         `json:"age_seconds"`
}

type gatewayActiveOperation struct {
	GatewayActiveOperation
	cancel context.CancelFunc
}

type GatewayAgent struct {
	Cluster     string    `json:"cluster"`
	Instance    string    `json:"instance"`
	Key         string    `json:"key"`
	Remote      string    `json:"remote"`
	TokenID     string    `json:"token_id,omitempty"`
	Generation  uint64    `json:"generation"`
	RuntimeID   string    `json:"runtime_id,omitempty"`
	ConnectedAt time.Time `json:"connected_at"`
	LastSeen    time.Time `json:"last_seen"`
	HeartbeatAt time.Time `json:"heartbeat_at,omitempty"`
	Busy        bool      `json:"busy"`
	Status      string    `json:"status,omitempty"`
	BusyReason  string    `json:"busy_reason,omitempty"`
	ActiveOps   int       `json:"active_ops,omitempty"`
	QueuedOps   int       `json:"queued_ops,omitempty"`

	stateMu         sync.RWMutex
	conn            *websocket.Conn
	writeMu         sync.Mutex
	lastProgressAck time.Time
	opSlot          chan struct{}
	queueMu         sync.Mutex
	queued          int
	opsMu           sync.Mutex
	writers         int
	readers         int
	resources       map[string]*agentResourceSlot
	pendingMu       sync.Mutex
	pending         map[int64]*gatewayMessageQueue
	active          *gatewayMessageQueue
	activeBySession map[string]*gatewayMessageQueue
	messageBudget   *gatewayMessageBudget
	closed          chan struct{}
	reqID           int64
}

type agentResourceSlot struct {
	slot   chan struct{}
	queued int
}

type GatewayTarget struct {
	Cluster     string    `json:"cluster"`
	Instance    string    `json:"instance"`
	Key         string    `json:"key"`
	Remote      string    `json:"remote"`
	TokenID     string    `json:"token_id,omitempty"`
	ConnectedAt time.Time `json:"connected_at"`
	LastSeen    time.Time `json:"last_seen"`
	Busy        bool      `json:"busy"`
	Status      string    `json:"status"`
	BusyReason  string    `json:"busy_reason,omitempty"`
	ActiveOps   int       `json:"active_ops"`
	QueuedOps   int       `json:"queued_ops"`
	Resources   []string  `json:"resources,omitempty"`
	Sessions    []string  `json:"sessions,omitempty"`
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
	mu           sync.Mutex
	items        []gatewayQueuedMessage
	queuedBytes  int64
	maxBytes     int64
	budget       *gatewayMessageBudget
	notify       chan struct{}
	requestID    int64
	cancelMethod string
	cancelOnce   sync.Once
	cancelDone   chan struct{}
	closed       bool
	err          error
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

type targetUnlockResponse struct {
	Cluster      string `json:"cluster"`
	Instance     string `json:"instance"`
	Disconnected bool   `json:"disconnected"`
}

type auditResponse struct {
	Events []auditRecord `json:"events"`
}

type auditRecord struct {
	ID             int64  `json:"id"`
	UserID         string `json:"user_id"`
	TokenID        string `json:"token_id"`
	Cluster        string `json:"cluster"`
	Instance       string `json:"instance"`
	Action         string `json:"action"`
	Session        string `json:"session"`
	CommandSummary string `json:"command_summary"`
	Status         string `json:"status"`
	Error          string `json:"error,omitempty"`
	Tail           string `json:"tail,omitempty"`
	BytesIn        int64  `json:"bytes_in"`
	BytesOut       int64  `json:"bytes_out"`
	StartedAt      string `json:"started_at"`
	EndedAt        string `json:"ended_at,omitempty"`
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
	Token     string `json:"token"`
	TokenID   string `json:"token_id"`
	TokenType string `json:"token_type"`
	Username  string `json:"username"`
}

type gatewayAdminTokenCreateRequest struct {
	Kind     string `json:"kind,omitempty"`
	User     string `json:"user"`
	Name     string `json:"name,omitempty"`
	Cluster  string `json:"cluster,omitempty"`
	Instance string `json:"instance,omitempty"`
	Expires  string `json:"expires,omitempty"`
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
		opts.OperationTimeout = 30 * time.Minute
	}
	if opts.TargetQueueTimeout <= 0 {
		opts.TargetQueueTimeout = 2 * time.Minute
	}
	if opts.TargetReconnectGrace <= 0 {
		opts.TargetReconnectGrace = 10 * time.Second
	}
	if opts.LoginTokenTTL <= 0 {
		opts.LoginTokenTTL = 24 * time.Hour
	}
	if opts.MaxConcurrentOperations <= 0 {
		opts.MaxConcurrentOperations = 64
	}
	if opts.MaxConcurrentPerUser <= 0 {
		opts.MaxConcurrentPerUser = 8
	}
	if opts.MaxQueuedPerTarget <= 0 {
		opts.MaxQueuedPerTarget = 0
	}
	hub := &GatewayHub{
		store:    store,
		opts:     opts,
		registry: registry,
	}
	hub.registration = NewAgentRegistrationHandler(
		store,
		registry,
		opts.AgentLease,
		newGatewayMessageBudget(gatewayMessageBudgetMaxBytes),
	)
	hub.client = NewClientService(store, registry, opts)
	if err := hub.client.upgrades.ResumePending(opts.OperationTimeout); err != nil {
		panic(err)
	}
	return hub
}

func (h *GatewayHub) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/v1/auth/login", h.HandleAuthLogin)
	mux.HandleFunc("/v1/agent/connect", h.HandleAgentConnect)
	mux.HandleFunc("/v1/rpc", h.HandleClientRPC)
	mux.HandleFunc("/v1/git/", h.HandleGitHTTP)
	mux.HandleFunc("/v1/targets", h.HandleTargets)
	mux.HandleFunc("/v1/targets/unlock", h.HandleTargetUnlock)
	mux.HandleFunc("/v1/audit", h.HandleAudit)
	mux.HandleFunc("/v1/credentials/prepare", h.HandleCredentialPrepare)
	mux.HandleFunc("/v1/credential-bundles", h.HandleCredentialBundles)
	mux.HandleFunc("/v1/credentials", h.HandleCredentials)
	mux.HandleFunc("/v1/credentials/", h.HandleCredential)
	mux.HandleFunc("/v1/admin/tokens", h.HandleAdminTokens)
	mux.HandleFunc("/v1/admin/users", h.HandleAdminUsers)
	mux.HandleFunc("/v1/admin/users/password", h.HandleAdminUserPassword)
	mux.HandleFunc("/v1/admin/users/disable", h.HandleAdminUserDisable)
	mux.HandleFunc("/v1/admin/grants", h.HandleAdminGrants)
	mux.HandleFunc("/v1/admin/instances", h.HandleAdminInstances)
	mux.HandleFunc("/v1/admin/operations", h.HandleAdminOperations)
	mux.HandleFunc("/v1/admin/repos", h.HandleAdminRepos)
	mux.HandleFunc("/v1/admin/repos/test", h.HandleAdminRepoTest)
	mux.HandleFunc("/v1/admin/repos/clone", h.HandleAdminRepoClone)
	mux.HandleFunc("/v1/admin/jobs", h.HandleAdminJobs)
	mux.HandleFunc("/v1/admin/jobs/run", h.HandleAdminJobRun)
	mux.HandleFunc("/v1/admin/jobs/issues", h.HandleAdminJobIssues)
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
	user, err := h.store.VerifyUserPassword(req.Username, req.Password)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = "password login"
	}
	_, _ = h.store.DeleteExpiredTokens(time.Now().UTC())
	token, err := h.store.CreateToken(CreateTokenRequest{
		Kind:      TokenKindUser,
		UserID:    user.ID,
		Name:      name,
		ExpiresAt: time.Now().UTC().Add(h.opts.LoginTokenTTL),
	})
	if err != nil {
		http.Error(w, "failed to create token", http.StatusInternalServerError)
		return
	}
	writeJSONHTTP(w, gatewayLoginResponse{
		Token:     token.Plaintext,
		TokenID:   token.ID,
		TokenType: string(TokenKindUser),
		Username:  user.Name,
	})
}

func (h *GatewayHub) HandleAdminTokens(w http.ResponseWriter, r *http.Request) {
	auth, err := h.authenticateUser(r)
	if err != nil {
		h.writeUserAuthError(w, r)
		return
	}
	if !h.store.UserHasAction(auth.UserID, ActionAdmin) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	switch r.Method {
	case http.MethodGet:
		tokens, err := h.store.ListTokens(TokenKind(strings.TrimSpace(r.URL.Query().Get("kind"))))
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
	auditID, err := h.store.StartAudit(AuditEvent{
		UserID:         auth.UserID,
		TokenID:        auth.TokenID,
		Cluster:        "*",
		Instance:       "*",
		Action:         ActionAdmin,
		Session:        "admin-token-create",
		CommandSummary: "admin token create",
		StartedAt:      time.Now().UTC(),
	})
	finishAudit := func(status, errMsg string) {
		_ = h.store.FinishAudit(auditID, AuditFinish{
			Status:  status,
			Error:   errMsg,
			EndedAt: time.Now().UTC(),
		})
	}
	var req gatewayAdminTokenCreateRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		finishAudit("error", "invalid token request")
		http.Error(w, "invalid token request", http.StatusBadRequest)
		return
	}
	kind := TokenKindUser
	if strings.EqualFold(strings.TrimSpace(req.Kind), string(TokenKindAgent)) {
		kind = TokenKindAgent
	}
	createReq := CreateTokenRequest{Kind: kind, Name: req.Name}
	var username string
	if kind == TokenKindAgent {
		if strings.TrimSpace(req.Cluster) == "" || strings.TrimSpace(req.Instance) == "" {
			finishAudit("error", "agent token requires cluster and instance")
			http.Error(w, "agent token requires cluster and instance", http.StatusBadRequest)
			return
		}
		createReq.Cluster = req.Cluster
		createReq.Instance = req.Instance
	} else {
		user, err := h.store.FindUserByName(req.User)
		if err != nil {
			finishAudit("not_found", "user not found")
			http.Error(w, "user not found", http.StatusNotFound)
			return
		}
		createReq.UserID = user.ID
		username = user.Name
	}
	if strings.TrimSpace(req.Expires) != "" {
		ttl, err := time.ParseDuration(strings.TrimSpace(req.Expires))
		if err != nil {
			finishAudit("error", "invalid expires duration")
			http.Error(w, "invalid expires duration", http.StatusBadRequest)
			return
		}
		if ttl <= 0 {
			finishAudit("error", "expires duration must be positive")
			http.Error(w, "expires duration must be positive", http.StatusBadRequest)
			return
		}
		createReq.ExpiresAt = time.Now().UTC().Add(ttl)
	}
	token, err := h.store.CreateToken(createReq)
	if err != nil {
		finishAudit("error", "failed to create token")
		http.Error(w, "failed to create token", http.StatusInternalServerError)
		return
	}
	finishAudit("success", "")
	writeJSONHTTP(w, gatewayLoginResponse{
		Token:     token.Plaintext,
		TokenID:   token.ID,
		TokenType: string(kind),
		Username:  username,
	})
}

func (h *GatewayHub) HandleAdminOperations(w http.ResponseWriter, r *http.Request) {
	auth, err := h.authenticateUser(r)
	if err != nil {
		h.writeUserAuthError(w, r)
		return
	}
	if !h.store.UserHasAction(auth.UserID, ActionAdmin) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSONHTTP(w, map[string]interface{}{"operations": h.listActiveOperations()})
	case http.MethodDelete:
		id := strings.TrimSpace(r.URL.Query().Get("id"))
		if id == "" {
			http.Error(w, "missing id", http.StatusBadRequest)
			return
		}
		if !h.cancelActiveOperation(id) {
			http.Error(w, "operation not found", http.StatusNotFound)
			return
		}
		writeJSONHTTP(w, map[string]interface{}{"id": id, "canceled": true})
	default:
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
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
		opSlot:          make(chan struct{}, 1),
		pending:         make(map[int64]*gatewayMessageQueue),
		resources:       make(map[string]*agentResourceSlot),
		activeBySession: make(map[string]*gatewayMessageQueue),
		messageBudget:   messageBudget,
		closed:          make(chan struct{}),
	}
	agent.opSlot <- struct{}{}
	return agent
}

func (h *GatewayHub) HandleTargets(w http.ResponseWriter, r *http.Request) {
	auth, err := h.authenticateUser(r)
	if err != nil {
		h.writeUserAuthError(w, r)
		return
	}
	if !h.store.UserHasAction(auth.UserID, ActionTargetsList) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	targets := h.ListTargets()
	filtered := targets[:0]
	for _, target := range targets {
		if h.store.UserCan(auth.UserID, target.Cluster, target.Instance, ActionTargetsList) {
			filtered = append(filtered, target)
		}
	}
	writeJSONHTTP(w, targetResponse{Targets: filtered})
}

func (h *GatewayHub) HandleTargetUnlock(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	auth, err := h.authenticateUser(r)
	if err != nil {
		h.writeUserAuthError(w, r)
		return
	}
	if !h.store.UserHasAction(auth.UserID, ActionAdmin) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	cluster := strings.TrimSpace(r.URL.Query().Get("cluster"))
	instance := strings.TrimSpace(r.URL.Query().Get("instance"))
	if cluster == "" || instance == "" {
		http.Error(w, "missing cluster or instance", http.StatusBadRequest)
		return
	}
	agent := h.getAgent(cluster, instance)
	if agent == nil {
		http.Error(w, fmt.Sprintf("target offline: %s/%s", cluster, instance), http.StatusNotFound)
		return
	}
	agent.forceUnlock()
	writeJSONHTTP(w, targetUnlockResponse{Cluster: cluster, Instance: instance, Disconnected: true})
}

func (h *GatewayHub) HandleAudit(w http.ResponseWriter, r *http.Request) {
	auth, err := h.authenticateUser(r)
	if err != nil {
		h.writeUserAuthError(w, r)
		return
	}
	if !h.store.UserHasAction(auth.UserID, ActionAdmin) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	switch r.Method {
	case http.MethodGet:
		query := r.URL.Query()
		limit, _ := strconv.Atoi(strings.TrimSpace(query.Get("limit")))
		events, err := h.store.ListAuditFiltered(AuditFilter{
			UserID:   query.Get("user_id"),
			Cluster:  query.Get("cluster"),
			Instance: query.Get("instance"),
			Session:  query.Get("session"),
			Action:   GatewayAction(query.Get("action")),
			Status:   query.Get("status"),
			Limit:    limit,
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		records := make([]auditRecord, 0, len(events))
		for _, event := range events {
			record := auditRecord{
				ID:             event.ID,
				UserID:         event.UserID,
				TokenID:        event.TokenID,
				Cluster:        event.Cluster,
				Instance:       event.Instance,
				Action:         string(event.Action),
				Session:        event.Session,
				CommandSummary: event.CommandSummary,
				Status:         event.Status,
				Error:          event.Error,
				Tail:           event.Tail,
				BytesIn:        event.BytesIn,
				BytesOut:       event.BytesOut,
				StartedAt:      event.StartedAt.UTC().Format(time.RFC3339),
			}
			if !event.EndedAt.IsZero() {
				record.EndedAt = event.EndedAt.UTC().Format(time.RFC3339)
			}
			records = append(records, record)
		}
		writeJSONHTTP(w, auditResponse{Events: records})
	case http.MethodDelete:
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
		deleted, err := h.store.DeleteAuditBefore(before)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
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
		h.writeUserAuthError(w, r)
		return
	}
	cluster := strings.TrimSpace(r.URL.Query().Get("cluster"))
	instance := strings.TrimSpace(r.URL.Query().Get("instance"))
	if cluster == "" {
		cluster = "default"
	}
	if instance == "" {
		http.Error(w, "missing instance", http.StatusBadRequest)
		return
	}
	if !h.store.UserCan(auth.UserID, cluster, instance, ActionInfo) &&
		!h.store.UserCan(auth.UserID, cluster, instance, ActionExec) &&
		!h.store.UserCan(auth.UserID, cluster, instance, ActionAsk) &&
		!h.store.UserCan(auth.UserID, cluster, instance, ActionRead) &&
		!h.store.UserCan(auth.UserID, cluster, instance, ActionWrite) &&
		!h.store.UserCan(auth.UserID, cluster, instance, ActionPush) &&
		!h.store.UserCan(auth.UserID, cluster, instance, ActionCheck) &&
		!h.store.UserCan(auth.UserID, cluster, instance, ActionClean) &&
		!h.store.UserCan(auth.UserID, cluster, instance, ActionAgentUpgrade) {
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
	writeJSON := func(v interface{}) error {
		connMu.Lock()
		defer connMu.Unlock()
		if err := conn.WriteJSON(v); err != nil {
			log.Printf("[gateway] client write failed: %v", err)
			return fmt.Errorf("%w: %v", errGatewayClientDisconnected, err)
		}
		return nil
	}
	writeRaw := func(raw []byte) error {
		connMu.Lock()
		defer connMu.Unlock()
		if err := conn.WriteMessage(websocket.TextMessage, raw); err != nil {
			log.Printf("[gateway] client raw write failed: %v", err)
			return fmt.Errorf("%w: %v", errGatewayClientDisconnected, err)
		}
		return nil
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

	for data := range messages {
		var req api.JSONRPCRequest
		if err := json.Unmarshal(data, &req); err != nil {
			continue
		}
		switch req.Method {
		case "initialize":
			writeJSON(api.JSONRPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result: map[string]interface{}{
					"protocolVersion": "2024-11-05",
					"serverInfo": map[string]string{
						"name":    "doops-gateway",
						"version": "1.0",
					},
					"capabilities": map[string]interface{}{
						"tools": map[string]interface{}{},
					},
				},
			})
		case "tools/call":
			if err := h.handleGatewayToolCall(connCtx, auth, cluster, instance, req, writeRaw, writeJSON); err != nil {
				writeJSON(buildErrorResponse(req.ID, -32603, err.Error()))
			}
		default:
			writeJSON(buildErrorResponse(req.ID, -32601, "unknown method: "+req.Method))
		}
	}
}

func (h *ClientService) handleGatewayToolCall(ctx context.Context, auth TokenAuth, cluster, instance string, req api.JSONRPCRequest, writeRaw func([]byte) error, writeJSON func(interface{}) error) error {
	var params api.ToolCallParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		writeJSON(buildErrorResponse(req.ID, -32602, "invalid tools/call params"))
		return nil
	}
	if internalCredentialTool(params.Name) {
		writeJSON(buildErrorResponse(req.ID, -32003, "credential control-plane tools are internal only"))
		return nil
	}
	if params.Name == "doops_agent_prompt" {
		operation := extractStringArg(params.Arguments, "operation")
		if operation == "apply" {
			instruction := extractStringArg(params.Arguments, "instruction")
			if err := validateDoagentApplyInstruction(instruction); err != nil {
				writeJSON(buildErrorResponse(req.ID, -32602, err.Error()))
				return nil
			}
		} else if operation != "" && operation != "ask" {
			writeJSON(buildErrorResponse(req.ID, -32602, "unsupported agent prompt operation: "+operation))
			return nil
		}
	}
	action := actionForTool(params.Name, params.Arguments)
	if action == "" {
		writeJSON(buildErrorResponse(req.ID, -32601, "unknown doops action for tool: "+params.Name))
		return nil
	}
	auditID, auditErr := h.store.StartAudit(AuditEvent{
		UserID:         auth.UserID,
		TokenID:        auth.TokenID,
		Cluster:        cluster,
		Instance:       instance,
		Action:         action,
		Session:        extractSession(params.Arguments),
		CommandSummary: summarizeToolCall(params.Name, params.Arguments),
		StartedAt:      time.Now().UTC(),
	})
	if auditErr != nil {
		writeJSON(buildErrorResponse(req.ID, -32603, "start audit: "+auditErr.Error()))
		return nil
	}
	finishAudit := func(status, errMsg, tail string, bytesOut int64) {
		if err := h.store.FinishAudit(auditID, AuditFinish{
			Status:   status,
			Error:    errMsg,
			Tail:     tail,
			BytesIn:  int64(len(params.Arguments)),
			BytesOut: bytesOut,
			EndedAt:  time.Now().UTC(),
		}); err != nil {
			log.Printf("[gateway] finish audit failed: id=%d: %v", auditID, err)
		}
	}
	if !h.store.UserCan(auth.UserID, cluster, instance, action) {
		errMsg := fmt.Sprintf("forbidden: %s on %s/%s", action, cluster, instance)
		finishAudit("forbidden", errMsg, "", 0)
		writeJSON(buildErrorResponse(req.ID, -32003, errMsg))
		return nil
	}
	agent := h.registry.Get(cluster, instance)
	if agent == nil {
		errMsg := fmt.Sprintf("target offline: %s/%s", cluster, instance)
		finishAudit("offline", errMsg, "", 0)
		writeJSON(buildErrorResponse(req.ID, -32004, errMsg))
		return nil
	}
	var upgradeOp UpgradeOperation
	waitForUpgradeReplacement := false
	upgradeImage := ""
	if action == ActionAgentUpgrade {
		var args agentUpgradeParams
		if err := json.Unmarshal(params.Arguments, &args); err != nil {
			errMsg := fmt.Sprintf("invalid agent upgrade params: %v", err)
			finishAudit("error", errMsg, "", 0)
			writeJSON(buildErrorResponse(req.ID, -32602, errMsg))
			return nil
		}
		mode := strings.TrimSpace(args.Mode)
		waitForUpgradeReplacement = !args.DryRun && (mode == "" || mode == "auto" || mode == "k8s" || mode == "daemonset")
		upgradeImage = strings.TrimSpace(args.Image)
	}
	releaseLimit, err := h.acquireOperationSlot(auth.UserID)
	if err != nil {
		finishAudit("rate_limited", err.Error(), "", 0)
		writeJSON(buildErrorResponse(req.ID, -32006, err.Error()))
		return nil
	}
	defer releaseLimit()
	opCtx, cancelOp := context.WithCancel(ctx)
	defer cancelOp()
	opID := h.registerActiveOperation(GatewayActiveOperation{
		UserID:         auth.UserID,
		TokenID:        auth.TokenID,
		Cluster:        cluster,
		Instance:       instance,
		Action:         action,
		Session:        extractSession(params.Arguments),
		CommandSummary: summarizeToolCall(params.Name, params.Arguments),
		Kind:           "rpc",
	}, cancelOp)
	defer h.finishActiveOperation(opID)

	resourceKey := resourceKeyForTool(action, params.Name, params.Arguments, cluster, instance)
	if err := agent.acquireForAction(opCtx, action, resourceKey, h.opts.MaxQueuedPerTarget, h.opts.TargetQueueTimeout); err != nil {
		if errors.Is(err, context.Canceled) {
			finishAudit("canceled", "operation canceled", "", 0)
			writeJSON(buildErrorResponse(req.ID, -32007, "operation canceled"))
			return nil
		}
		errMsg := fmt.Sprintf("%v: %s/%s", err, cluster, instance)
		finishAudit("busy", errMsg, "", 0)
		writeJSON(buildErrorResponse(req.ID, -32005, errMsg))
		return nil
	}
	defer agent.releaseForAction(action, resourceKey)
	if waitForUpgradeReplacement {
		var beginErr error
		upgradeOp, beginErr = h.upgrades.Begin(cluster, instance, upgradeImage, agent.Generation, agent.RuntimeID)
		if beginErr != nil {
			finishAudit("error", beginErr.Error(), "", 0)
			writeJSON(buildErrorResponse(req.ID, -32603, beginErr.Error()))
			return nil
		}
		if beginErr = h.upgrades.MarkWaiting(upgradeOp); beginErr != nil {
			_ = h.upgrades.Fail(upgradeOp, "persistence_failed", beginErr)
			finishAudit("error", beginErr.Error(), "", 0)
			writeJSON(buildErrorResponse(req.ID, -32603, beginErr.Error()))
			return nil
		}
	}

	tail := newAuditTailBuffer(8192)
	var bytesOut int64
	finalStatus := "success"
	finalErr := ""
	upgradeAccepted := false
	upgradeRejected := false
	var upgradeTerminalRaw []byte

	err = agent.relayToolCall(opCtx, params, h.opts.OperationTimeout, func(msg gatewayWSMessage) error {
		if method, _ := msg.Parsed["method"].(string); method == "notifications/message" {
			if p, ok := msg.Parsed["params"].(map[string]interface{}); ok {
				if chunk, ok := p["data"].(string); ok {
					bytesOut += int64(len(chunk))
					tail.WriteString(chunk)
				}
			}
			return writeRaw(msg.Raw)
		}
		if id, ok := msg.Parsed["id"]; ok {
			msg.Parsed["id"] = req.ID
			if result, ok := msg.Parsed["result"].(map[string]interface{}); ok {
				if isErr, ok := result["isError"]; ok && fmt.Sprintf("%v", isErr) == "true" {
					finalStatus = "error"
					upgradeRejected = waitForUpgradeReplacement
					finalErr = resultContentText(result)
					if waitForUpgradeReplacement {
						upgradeTerminalRaw, _ = json.Marshal(msg.Parsed)
						return nil
					}
				} else if waitForUpgradeReplacement {
					upgradeAccepted = true
					return nil
				}
			}
			if rpcErr, ok := msg.Parsed["error"]; ok && rpcErr != nil {
				finalStatus = "error"
				finalErr = fmt.Sprintf("%v", rpcErr)
				if waitForUpgradeReplacement {
					upgradeRejected = true
					upgradeTerminalRaw, _ = json.Marshal(msg.Parsed)
					return nil
				}
			}
			raw, _ := json.Marshal(msg.Parsed)
			if err := writeRaw(raw); err != nil {
				return err
			}
			_ = id
		}
		return nil
	})
	if waitForUpgradeReplacement && (upgradeAccepted || errors.Is(err, errGatewayAgentDisconnected)) {
		type upgradeResult struct {
			replacement *GatewayAgent
			err         error
		}
		resultCh := make(chan upgradeResult, 1)
		go func() {
			waitCtx, cancelWait := context.WithTimeout(context.Background(), h.opts.OperationTimeout)
			defer cancelWait()
			replacement, waitErr := h.upgrades.WaitForReplacementPrepared(waitCtx, upgradeOp)
			resultCh <- upgradeResult{replacement: replacement, err: waitErr}
		}()
		select {
		case result := <-resultCh:
			if result.err != nil {
				err = result.err
			} else {
				err = nil
				finalStatus = "success"
				finalErr = ""
				_ = writeJSON(buildSuccessResponse(req.ID, fmt.Sprintf(
					"agent upgrade completed: %s/%s generation %d image %s",
					cluster, instance, result.replacement.Generation, upgradeOp.Image,
				)))
			}
		case <-opCtx.Done():
			err = opCtx.Err()
		}
	} else if waitForUpgradeReplacement && (err != nil || upgradeRejected) {
		cause := err
		phase := "request_failed"
		if upgradeRejected {
			phase = "request_rejected"
			cause = fmt.Errorf("%s", firstNonEmptyString(finalErr, "agent rejected upgrade request"))
		}
		if persistErr := h.upgrades.Fail(upgradeOp, phase, cause); persistErr != nil {
			err = persistErr
		} else if len(upgradeTerminalRaw) > 0 {
			err = writeRaw(upgradeTerminalRaw)
		}
	}
	if err != nil {
		if errors.Is(err, errGatewayClientDisconnected) {
			finalStatus = "canceled"
			finalErr = errGatewayClientDisconnected.Error()
		} else if errors.Is(err, context.Canceled) {
			finalStatus = "canceled"
			finalErr = "operation canceled"
			_ = writeJSON(buildErrorResponse(req.ID, -32007, finalErr))
		} else {
			finalStatus = "error"
			finalErr = err.Error()
			_ = writeJSON(buildErrorResponse(req.ID, -32603, err.Error()))
		}
	}
	finishAudit(finalStatus, finalErr, tail.String(), bytesOut)
	return nil
}

func internalCredentialTool(tool string) bool {
	return tool == "doops_credential_plan" ||
		tool == "doops_credential_materialize" ||
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

func (h *GatewayHub) waitForAgent(ctx context.Context, cluster, instance string) *GatewayAgent {
	return h.registry.Wait(ctx, cluster, instance, h.opts.TargetReconnectGrace)
}

func (h *GatewayHub) ListTargets() []GatewayTarget {
	return h.registry.List()
}

func (h *GatewayHub) authenticateUser(r *http.Request) (TokenAuth, error) {
	return h.store.VerifyUserToken(bearerToken(r))
}

func (h *ClientService) authenticateUser(r *http.Request) (TokenAuth, error) {
	return h.store.VerifyUserToken(bearerToken(r))
}

func (h *ClientService) writeUserAuthError(w http.ResponseWriter, r *http.Request) {
	if _, err := h.store.VerifyAgentToken(bearerToken(r)); err == nil {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	http.Error(w, "Unauthorized", http.StatusUnauthorized)
}

func (h *GatewayHub) writeUserAuthError(w http.ResponseWriter, r *http.Request) {
	if _, err := h.store.VerifyAgentToken(bearerToken(r)); err == nil {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	http.Error(w, "Unauthorized", http.StatusUnauthorized)
}

// secureTokenEqual reports whether two tokens match using a constant-time
// comparison to avoid leaking the token through timing side channels. An empty
// expected token never matches.
func secureTokenEqual(provided, expected string) bool {
	if expected == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1
}

func bearerToken(r *http.Request) string {
	token := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.HasPrefix(strings.ToLower(token), "bearer ") {
		return strings.TrimSpace(token[len("bearer "):])
	}
	if token == "" {
		token = strings.TrimSpace(r.Header.Get("X-Doops-Key"))
	}
	return token
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
				a.RuntimeID, _ = result["runtimeId"].(string)
				a.RuntimeID = strings.TrimSpace(a.RuntimeID)
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

func (a *GatewayAgent) readLoop(registry *AgentRegistry, lease time.Duration) {
	defer close(a.closed)
	defer a.conn.Close()
	a.conn.SetReadDeadline(time.Now().Add(lease))
	a.conn.SetPongHandler(func(string) error {
		a.heartbeat(registry)
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
				if err := queue.enqueue(msg); err != nil && !errors.Is(err, errGatewayMessageQueueClosed) {
					log.Printf("[gateway] agent message queue failed: %s id=%d: %v", a.Key, id, err)
					a.cancelPending(queue, true)
				}
				continue
			}
		}
		if method, _ := parsed["method"].(string); method == "notifications/message" {
			sessionID := sessionIDFromNotification(parsed)
			a.pendingMu.Lock()
			var queue *gatewayMessageQueue
			if sessionID != "" {
				queue = a.activeBySession[sessionID]
			} else {
				queue = a.active
			}
			a.pendingMu.Unlock()
			if queue != nil {
				if err := queue.enqueue(msg); err != nil && !errors.Is(err, errGatewayMessageQueueClosed) {
					log.Printf("[gateway] agent notification queue failed: %s session=%q: %v", a.Key, sessionID, err)
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
			"id":      queue.requestID,
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

func (a *GatewayAgent) waitPendingCancel(queue *gatewayMessageQueue) {
	done := a.cancelPending(queue, false)
	if done == nil {
		return
	}
	select {
	case <-done:
	case <-a.closed:
	}
}

func (a *GatewayAgent) pingLoop(registry *AgentRegistry, lease time.Duration) {
	interval := lease / 3
	if interval <= 0 {
		interval = 30 * time.Second
	}
	if !writeHeartbeatPing(a.conn, []byte("ping"), 10*time.Second) {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if !writeHeartbeatPing(a.conn, []byte("ping"), 10*time.Second) {
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

func (a *GatewayAgent) heartbeat(registry *AgentRegistry) {
	now := time.Now().UTC()
	a.stateMu.Lock()
	a.HeartbeatAt = now
	a.LastSeen = now
	a.stateMu.Unlock()
	if registry != nil {
		registry.NotifyHeartbeat(a)
	}
}

func (a *GatewayAgent) relayToolCall(ctx context.Context, params api.ToolCallParams, timeout time.Duration, forward func(gatewayWSMessage) error) error {
	id := atomic.AddInt64(&a.reqID, 1)
	ch := a.registerPending(id, "tools/cancel")
	defer a.unregisterPending(id)
	sessionID := extractSession(params.Arguments)
	a.pendingMu.Lock()
	if sessionID != "" {
		a.activeBySession[sessionID] = ch
	} else {
		a.active = ch
	}
	a.pendingMu.Unlock()
	defer func() {
		a.pendingMu.Lock()
		if sessionID == "" && a.active == ch {
			a.active = nil
		}
		if sessionID != "" && a.activeBySession[sessionID] == ch {
			delete(a.activeBySession, sessionID)
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
		return err
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var forwardErr error
	for {
		if msg, ok, queueErr := ch.dequeue(); queueErr != nil {
			a.waitPendingCancel(ch)
			return queueErr
		} else if ok {
			if msg.Parsed == nil {
				continue
			}
			if _, ok := msg.Parsed["id"]; ok {
				if forwardErr == nil {
					if err := forward(msg); err != nil {
						forwardErr = err
					}
				}
				return forwardErr
			}
			if forwardErr == nil {
				if err := forward(msg); err != nil {
					forwardErr = err
				}
			}
			continue
		}

		select {
		case <-ctx.Done():
			a.waitPendingCancel(ch)
			return ctx.Err()
		case <-ch.notify:
		case <-timer.C:
			a.waitPendingCancel(ch)
			return fmt.Errorf("operation timed out")
		case <-a.closed:
			return errGatewayAgentDisconnected
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
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	return a.conn.WriteJSON(v)
}

func (a *GatewayAgent) acquire(ctx context.Context, maxQueued int, wait time.Duration) error {
	select {
	case <-a.opSlot:
		a.setBusy(true)
		return nil
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
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
	defer func() {
		a.queueMu.Lock()
		a.queued--
		a.queueMu.Unlock()
	}()

	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-a.opSlot:
		a.setBusy(true)
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return fmt.Errorf("target queue timeout")
	case <-a.closed:
		return fmt.Errorf("target offline")
	}
}

func (a *GatewayAgent) release() {
	select {
	case a.opSlot <- struct{}{}:
	default:
	}
	a.setBusy(false)
}

func (a *GatewayAgent) acquireForAction(ctx context.Context, action GatewayAction, resourceKey string, maxQueued int, wait time.Duration) error {
	if isConcurrentReadOnlyAction(action) {
		return a.acquireReadOnly()
	}
	if resourceKey != "" {
		return a.acquireResource(ctx, resourceKey, maxQueued, wait)
	}
	return a.acquireExclusive(ctx, maxQueued, wait)
}

func (a *GatewayAgent) releaseForAction(action GatewayAction, resourceKey string) {
	if isConcurrentReadOnlyAction(action) {
		a.releaseReadOnly()
		return
	}
	if resourceKey != "" {
		a.releaseResource(resourceKey)
		return
	}
	a.releaseExclusive()
}

func (a *GatewayAgent) acquireExclusive(ctx context.Context, maxQueued int, wait time.Duration) error {
	if err := a.acquire(ctx, maxQueued, wait); err != nil {
		return err
	}
	a.opsMu.Lock()
	a.writers++
	a.opsMu.Unlock()
	return nil
}

func (a *GatewayAgent) releaseExclusive() {
	a.opsMu.Lock()
	if a.writers > 0 {
		a.writers--
	}
	a.opsMu.Unlock()
	a.release()
}

func (a *GatewayAgent) acquireReadOnly() error {
	a.opsMu.Lock()
	if a.writers > 0 || len(a.opSlot) == 0 {
		a.opsMu.Unlock()
		return fmt.Errorf("target busy")
	}
	a.readers++
	a.opsMu.Unlock()
	a.setBusy(true)
	return nil
}

func (a *GatewayAgent) releaseReadOnly() {
	a.opsMu.Lock()
	if a.readers > 0 {
		a.readers--
	}
	idle := a.readers == 0 && a.writers == 0 && len(a.opSlot) > 0
	a.opsMu.Unlock()
	if idle {
		a.setBusy(false)
	}
}

func (a *GatewayAgent) acquireResource(ctx context.Context, key string, maxQueued int, wait time.Duration) error {
	a.opsMu.Lock()
	if a.writers > 0 || len(a.opSlot) == 0 {
		a.opsMu.Unlock()
		return fmt.Errorf("target busy")
	}
	slot := a.resources[key]
	if slot == nil {
		slot = &agentResourceSlot{slot: make(chan struct{}, 1)}
		slot.slot <- struct{}{}
		a.resources[key] = slot
	}
	select {
	case <-slot.slot:
		a.readers++
		a.opsMu.Unlock()
		a.setBusy(true)
		return nil
	default:
	}
	if maxQueued <= 0 {
		a.opsMu.Unlock()
		return fmt.Errorf("target busy")
	}
	if slot.queued >= maxQueued {
		a.opsMu.Unlock()
		return fmt.Errorf("target queue full")
	}
	slot.queued++
	a.opsMu.Unlock()
	defer func() {
		a.opsMu.Lock()
		if slot.queued > 0 {
			slot.queued--
		}
		a.opsMu.Unlock()
	}()

	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-slot.slot:
		a.opsMu.Lock()
		a.readers++
		a.opsMu.Unlock()
		a.setBusy(true)
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return fmt.Errorf("target queue timeout")
	case <-a.closed:
		return fmt.Errorf("target offline")
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
	idle := a.readers == 0 && a.writers == 0 && len(a.opSlot) > 0
	a.opsMu.Unlock()
	if idle {
		a.setBusy(false)
	}
}

func isConcurrentReadOnlyAction(action GatewayAction) bool {
	switch action {
	case ActionRead, ActionInfo, ActionCheck:
		return true
	default:
		return false
	}
}

func (a *GatewayAgent) forceUnlock() {
	a.pendingMu.Lock()
	queues := make(map[*gatewayMessageQueue]struct{}, len(a.pending)+len(a.activeBySession)+1)
	for id, queue := range a.pending {
		queues[queue] = struct{}{}
		delete(a.pending, id)
	}
	if a.active != nil {
		queues[a.active] = struct{}{}
	}
	a.active = nil
	for sessionID, queue := range a.activeBySession {
		queues[queue] = struct{}{}
		delete(a.activeBySession, sessionID)
	}
	a.pendingMu.Unlock()
	for queue := range queues {
		queue.close()
	}
	a.opsMu.Lock()
	a.writers = 0
	a.readers = 0
	a.resources = make(map[string]*agentResourceSlot)
	a.opsMu.Unlock()
	a.release()
	_ = a.conn.Close()
}

func (a *GatewayAgent) setBusy(busy bool) {
	a.stateMu.Lock()
	a.Busy = busy
	a.stateMu.Unlock()
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
	opSlotLocked := len(a.opSlot) == 0
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
	sessions := make([]string, 0, len(a.activeBySession))
	for sessionID := range a.activeBySession {
		sessions = append(sessions, sessionID)
	}
	hasLegacyActive := a.active != nil
	a.pendingMu.Unlock()
	sort.Strings(sessions)

	state.activeOps = writers + readers
	if hasLegacyActive && state.activeOps == 0 {
		state.activeOps = 1
	}
	state.queuedOps = queued + resourceQueued
	state.resources = resources
	state.sessions = sessions

	switch {
	case writers > 0 || opSlotLocked:
		state.busy = true
		state.status = "busy"
		state.reason = "exclusive_operation"
	case queued > 0:
		state.busy = true
		state.status = "busy"
		state.reason = "target_queue"
	case state.activeOps > 0 || len(resources) > 0 || state.queuedOps > 0:
		state.status = "active"
	}
	return state
}

func (a *GatewayAgent) busyState() bool {
	return a.runtimeState().busy
}

func (a *GatewayAgent) snapshot() GatewayTarget {
	a.stateMu.RLock()
	target := GatewayTarget{
		Cluster:     a.Cluster,
		Instance:    a.Instance,
		Key:         a.Key,
		Remote:      a.Remote,
		TokenID:     a.TokenID,
		ConnectedAt: a.ConnectedAt,
		LastSeen:    a.LastSeen,
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
	case "doops_shell", "doops_bg", "doops_task_status":
		return ActionExec
	case "doops_agent_prompt":
		switch extractStringArg(args, "mode") {
		case "metadata", "history":
			return ActionAsk
		default:
			return ActionAsk
		}
	case "doops_git_clone":
		return ActionPull
	case "doops_file_read":
		return ActionRead
	case "doops_file_write":
		return ActionWrite
	case "doops_node_info":
		return ActionInfo
	case "doops_check_deployment":
		return ActionCheck
	case "doops_clean_workspace":
		return ActionClean
	case "doops_agent_upgrade":
		return ActionAgentUpgrade
	case "doops_credential_plan":
		return ActionCredentialMetadata
	case "doops_credential_materialize":
		return ActionCredentialUse
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
	switch action {
	case ActionExec, ActionAsk:
		if sessionID := extractSession(args); sessionID != "" {
			return "session:" + sessionID
		}
	case ActionPush, ActionPull, ActionClean:
		if sessionID := extractSession(args); sessionID != "" {
			return "workspace:" + sessionID
		}
	case ActionWrite:
		if path := extractStringArg(args, "path"); path != "" {
			return "path:" + path
		}
	case ActionAgentUpgrade:
		return "target:" + cluster + "/" + instance
	case ActionCredentialMetadata:
		if sessionID := extractSession(args); sessionID != "" {
			return "workspace:" + sessionID
		}
	case ActionCredentialUse:
		if credentialID := extractStringArg(args, "credential_id"); credentialID != "" {
			return "credential:" + credentialID
		}
	}
	return ""
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

func sessionIDFromNotification(parsed map[string]interface{}) string {
	params, _ := parsed["params"].(map[string]interface{})
	for _, key := range []string{"sessionID", "session_id", "session"} {
		if v, _ := params[key].(string); v != "" {
			return v
		}
	}
	return ""
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
	case "doops_agent_prompt":
		if msg, _ := m["instruction"].(string); msg != "" {
			return trimTail(msg, 512)
		}
	case "doops_git_clone":
		repoURL, _ := m["url"].(string)
		branch, _ := m["branch"].(string)
		return trimTail("doops_git_clone "+repoURL+" "+branch, 512)
	case "doops_file_read", "doops_file_write":
		if p, _ := m["path"].(string); p != "" {
			return tool + " " + p
		}
	case "doops_workspace_pull_begin":
		if sessionID, _ := m["session_id"].(string); sessionID != "" {
			return tool + " " + sessionID
		}
	case "doops_agent_upgrade":
		if image, _ := m["image"].(string); image != "" {
			return tool + " " + image
		}
	}
	return tool
}

func tunnelKey(cluster, instance string) string {
	cluster = strings.TrimSpace(cluster)
	instance = strings.TrimSpace(instance)
	if cluster == "" {
		cluster = "default"
	}
	return cluster + "/" + instance
}
