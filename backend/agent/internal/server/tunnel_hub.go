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
	store *GatewayStore
	opts  GatewayHubOptions

	mu      sync.RWMutex
	agents  map[string]*GatewayAgent
	opsMu   sync.Mutex
	active  int
	userOps map[string]int

	activeOpsMu sync.Mutex
	activeOps   map[string]*gatewayActiveOperation
	activeSeq   uint64

	scheduler *Scheduler
}

// AttachScheduler 注入定时巡检调度器，供 /v1/admin/jobs run-now 等接口使用。
func (h *GatewayHub) AttachScheduler(sched *Scheduler) {
	h.scheduler = sched
}

var errGatewayClientDisconnected = errors.New("gateway client disconnected")

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
	ConnectedAt time.Time `json:"connected_at"`
	LastSeen    time.Time `json:"last_seen"`
	Busy        bool      `json:"busy"`
	Status      string    `json:"status,omitempty"`
	BusyReason  string    `json:"busy_reason,omitempty"`
	ActiveOps   int       `json:"active_ops,omitempty"`
	QueuedOps   int       `json:"queued_ops,omitempty"`

	stateMu         sync.RWMutex
	conn            *websocket.Conn
	writeMu         sync.Mutex
	opSlot          chan struct{}
	queueMu         sync.Mutex
	queued          int
	opsMu           sync.Mutex
	writers         int
	readers         int
	resources       map[string]*agentResourceSlot
	pendingMu       sync.Mutex
	pending         map[int64]chan gatewayWSMessage
	active          chan gatewayWSMessage
	activeBySession map[string]chan gatewayWSMessage
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
	return &GatewayHub{
		store:     store,
		opts:      opts,
		agents:    make(map[string]*GatewayAgent),
		userOps:   make(map[string]int),
		activeOps: make(map[string]*gatewayActiveOperation),
	}
}

func (h *GatewayHub) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/v1/auth/login", h.HandleAuthLogin)
	mux.HandleFunc("/v1/agent/connect", h.HandleAgentConnect)
	mux.HandleFunc("/v1/rpc", h.HandleClientRPC)
	mux.HandleFunc("/v1/git/", h.HandleGitHTTP)
	mux.HandleFunc("/v1/targets", h.HandleTargets)
	mux.HandleFunc("/v1/targets/unlock", h.HandleTargetUnlock)
	mux.HandleFunc("/v1/audit", h.HandleAudit)
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
	auditID, _ := h.store.StartAudit(AuditEvent{
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

func (h *GatewayHub) HandleAgentConnect(w http.ResponseWriter, r *http.Request) {
	cluster := strings.TrimSpace(r.URL.Query().Get("cluster"))
	instance := strings.TrimSpace(r.URL.Query().Get("instance"))
	if cluster == "" || instance == "" {
		http.Error(w, "cluster and instance are required", http.StatusBadRequest)
		return
	}
	auth, err := h.authenticateAgent(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if auth.Cluster != cluster || auth.Instance != instance {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[gateway] agent upgrade failed: %v", err)
		return
	}
	agent := &GatewayAgent{
		Cluster:         cluster,
		Instance:        instance,
		Key:             tunnelKey(cluster, instance),
		Remote:          r.RemoteAddr,
		ConnectedAt:     time.Now().UTC(),
		LastSeen:        time.Now().UTC(),
		conn:            conn,
		opSlot:          make(chan struct{}, 1),
		pending:         make(map[int64]chan gatewayWSMessage),
		resources:       make(map[string]*agentResourceSlot),
		activeBySession: make(map[string]chan gatewayWSMessage),
		closed:          make(chan struct{}),
	}
	agent.opSlot <- struct{}{}

	h.registerAgent(agent)
	log.Printf("[gateway] agent online: %s remote=%s", agent.Key, agent.Remote)
	go agent.readLoop(h)
	go agent.pingLoop(h)

	if err := agent.initialize(); err != nil {
		log.Printf("[gateway] agent initialize failed: %s: %v", agent.Key, err)
		conn.Close()
		h.unregisterAgent(agent)
		return
	}

	<-agent.closed
	h.unregisterAgent(agent)
	log.Printf("[gateway] agent offline: %s", agent.Key)
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
					"capabilities": map[string]interface{}{"tools": map[string]interface{}{}},
				},
			})
		case "tools/call":
			if err := h.handleGatewayToolCall(auth, cluster, instance, req, writeRaw, writeJSON); err != nil {
				writeJSON(buildErrorResponse(req.ID, -32603, err.Error()))
			}
		default:
			writeJSON(buildErrorResponse(req.ID, -32601, "unknown method: "+req.Method))
		}
	}
}

func (h *GatewayHub) handleGatewayToolCall(auth TokenAuth, cluster, instance string, req api.JSONRPCRequest, writeRaw func([]byte) error, writeJSON func(interface{}) error) error {
	var params api.ToolCallParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		writeJSON(buildErrorResponse(req.ID, -32602, "invalid tools/call params"))
		return nil
	}
	action := actionForTool(params.Name, params.Arguments)
	if action == "" {
		writeJSON(buildErrorResponse(req.ID, -32601, "unknown doops action for tool: "+params.Name))
		return nil
	}
	auditID, _ := h.store.StartAudit(AuditEvent{
		UserID:         auth.UserID,
		TokenID:        auth.TokenID,
		Cluster:        cluster,
		Instance:       instance,
		Action:         action,
		Session:        extractSession(params.Arguments),
		CommandSummary: summarizeToolCall(params.Name, params.Arguments),
		StartedAt:      time.Now().UTC(),
	})
	finishAudit := func(status, errMsg, tail string, bytesOut int64) {
		_ = h.store.FinishAudit(auditID, AuditFinish{
			Status:   status,
			Error:    errMsg,
			Tail:     tail,
			BytesIn:  int64(len(params.Arguments)),
			BytesOut: bytesOut,
			EndedAt:  time.Now().UTC(),
		})
	}
	if !h.store.UserCan(auth.UserID, cluster, instance, action) {
		errMsg := fmt.Sprintf("forbidden: %s on %s/%s", action, cluster, instance)
		finishAudit("forbidden", errMsg, "", 0)
		writeJSON(buildErrorResponse(req.ID, -32003, errMsg))
		return nil
	}
	agent := h.getAgent(cluster, instance)
	if agent == nil {
		errMsg := fmt.Sprintf("target offline: %s/%s", cluster, instance)
		finishAudit("offline", errMsg, "", 0)
		writeJSON(buildErrorResponse(req.ID, -32004, errMsg))
		return nil
	}
	releaseLimit, err := h.acquireOperationSlot(auth.UserID)
	if err != nil {
		finishAudit("rate_limited", err.Error(), "", 0)
		writeJSON(buildErrorResponse(req.ID, -32006, err.Error()))
		return nil
	}
	defer releaseLimit()
	opCtx, cancelOp := context.WithCancel(context.Background())
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

	tail := newAuditTailBuffer(8192)
	var bytesOut int64
	finalStatus := "success"
	finalErr := ""

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
			raw, _ := json.Marshal(msg.Parsed)
			if err := writeRaw(raw); err != nil {
				return err
			}
			if result, ok := msg.Parsed["result"].(map[string]interface{}); ok {
				if isErr, ok := result["isError"]; ok && fmt.Sprintf("%v", isErr) == "true" {
					finalStatus = "error"
				}
			}
			if rpcErr, ok := msg.Parsed["error"]; ok && rpcErr != nil {
				finalStatus = "error"
				finalErr = fmt.Sprintf("%v", rpcErr)
			}
			_ = id
		}
		return nil
	})
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

func (h *GatewayHub) acquireOperationSlot(userID string) (func(), error) {
	h.opsMu.Lock()
	defer h.opsMu.Unlock()
	if h.active >= h.opts.MaxConcurrentOperations {
		return nil, fmt.Errorf("global operation limit exceeded")
	}
	if h.userOps[userID] >= h.opts.MaxConcurrentPerUser {
		return nil, fmt.Errorf("user operation limit exceeded")
	}
	h.active++
	h.userOps[userID]++
	return func() {
		h.opsMu.Lock()
		defer h.opsMu.Unlock()
		h.active--
		h.userOps[userID]--
		if h.userOps[userID] <= 0 {
			delete(h.userOps, userID)
		}
	}, nil
}

func (h *GatewayHub) registerActiveOperation(op GatewayActiveOperation, cancel context.CancelFunc) string {
	id := fmt.Sprintf("op-%d", atomic.AddUint64(&h.activeSeq, 1))
	op.ID = id
	op.StartedAt = time.Now().UTC()
	h.activeOpsMu.Lock()
	h.activeOps[id] = &gatewayActiveOperation{
		GatewayActiveOperation: op,
		cancel:                 cancel,
	}
	h.activeOpsMu.Unlock()
	return id
}

func (h *GatewayHub) finishActiveOperation(id string) {
	if id == "" {
		return
	}
	h.activeOpsMu.Lock()
	delete(h.activeOps, id)
	h.activeOpsMu.Unlock()
}

func (h *GatewayHub) listActiveOperations() []GatewayActiveOperation {
	now := time.Now().UTC()
	h.activeOpsMu.Lock()
	defer h.activeOpsMu.Unlock()
	ops := make([]GatewayActiveOperation, 0, len(h.activeOps))
	for _, op := range h.activeOps {
		item := op.GatewayActiveOperation
		item.AgeSeconds = int64(now.Sub(item.StartedAt).Seconds())
		ops = append(ops, item)
	}
	sort.Slice(ops, func(i, j int) bool {
		return ops[i].StartedAt.Before(ops[j].StartedAt)
	})
	return ops
}

func (h *GatewayHub) cancelActiveOperation(id string) bool {
	h.activeOpsMu.Lock()
	op := h.activeOps[id]
	h.activeOpsMu.Unlock()
	if op == nil {
		return false
	}
	op.cancel()
	return true
}

func (h *GatewayHub) registerAgent(agent *GatewayAgent) {
	var old *GatewayAgent
	h.mu.Lock()
	old = h.agents[agent.Key]
	h.agents[agent.Key] = agent
	if h.store != nil {
		_ = h.store.MarkAgentOnline(AgentStatus{
			Cluster:     agent.Cluster,
			Instance:    agent.Instance,
			TokenID:     agent.TokenID,
			Remote:      agent.Remote,
			ConnectedAt: agent.ConnectedAt,
			LastSeen:    agent.LastSeen,
		})
	}
	h.mu.Unlock()

	if old != nil && old.conn != nil {
		_ = old.conn.Close()
	}
}

func (h *GatewayHub) unregisterAgent(agent *GatewayAgent) {
	removed := false
	h.mu.Lock()
	if current := h.agents[agent.Key]; current == agent {
		delete(h.agents, agent.Key)
		removed = true
	}
	h.mu.Unlock()

	if removed && h.store != nil {
		_ = h.store.MarkAgentOffline(agent.Cluster, agent.Instance)
	}
}

func (h *GatewayHub) getAgent(cluster, instance string) *GatewayAgent {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.agents[tunnelKey(cluster, instance)]
}

func (h *GatewayHub) waitForAgent(ctx context.Context, cluster, instance string) *GatewayAgent {
	if agent := h.getAgent(cluster, instance); agent != nil {
		return agent
	}
	grace := h.opts.TargetReconnectGrace
	if grace <= 0 {
		return nil
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
			return nil
		case <-ticker.C:
			if agent := h.getAgent(cluster, instance); agent != nil {
				return agent
			}
		}
	}
}

func (h *GatewayHub) ListTargets() []GatewayTarget {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]GatewayTarget, 0, len(h.agents))
	for _, a := range h.agents {
		out = append(out, a.snapshot())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func (h *GatewayHub) authenticateUser(r *http.Request) (TokenAuth, error) {
	return h.store.VerifyUserToken(bearerToken(r))
}

func (h *GatewayHub) authenticateAgent(r *http.Request) (TokenAuth, error) {
	return h.store.VerifyAgentToken(bearerToken(r))
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
	ch := a.registerPending(id)
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
	select {
	case <-ch:
		return nil
	case <-time.After(5 * time.Second):
		return fmt.Errorf("agent initialize timed out")
	case <-a.closed:
		return fmt.Errorf("agent connection closed")
	}
}

func (a *GatewayAgent) readLoop(h *GatewayHub) {
	defer close(a.closed)
	defer a.conn.Close()
	a.conn.SetReadDeadline(time.Now().Add(h.opts.AgentLease))
	a.conn.SetPongHandler(func(string) error {
		a.touch(h)
		return a.conn.SetReadDeadline(time.Now().Add(h.opts.AgentLease))
	})
	for {
		msgType, data, err := a.conn.ReadMessage()
		if err != nil {
			if !errors.Is(err, io.EOF) && !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				log.Printf("[gateway] agent read failed: %s: %v", a.Key, err)
			}
			return
		}
		a.touch(h)
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
			ch := a.pending[id]
			a.pendingMu.Unlock()
			if ch != nil {
				a.deliverPending(msg, ch)
				continue
			}
		}
		if method, _ := parsed["method"].(string); method == "notifications/message" {
			sessionID := sessionIDFromNotification(parsed)
			a.pendingMu.Lock()
			var ch chan gatewayWSMessage
			if sessionID != "" {
				ch = a.activeBySession[sessionID]
			} else {
				ch = a.active
			}
			a.pendingMu.Unlock()
			if ch != nil {
				a.deliverPending(msg, ch)
			}
		}
	}
}

func (a *GatewayAgent) deliverPending(msg gatewayWSMessage, ch chan gatewayWSMessage) {
	select {
	case ch <- msg:
	case <-a.closed:
	}
}

func (a *GatewayAgent) pingLoop(h *GatewayHub) {
	interval := h.opts.AgentLease / 3
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			deadline := time.Now().Add(10 * time.Second)
			a.writeMu.Lock()
			err := a.conn.WriteControl(websocket.PingMessage, []byte("ping"), deadline)
			a.writeMu.Unlock()
			if err != nil {
				_ = a.conn.Close()
				return
			}
		case <-a.closed:
			return
		}
	}
}

func (a *GatewayAgent) touch(h *GatewayHub) {
	lastSeen := time.Now().UTC()
	a.stateMu.Lock()
	a.LastSeen = lastSeen
	a.stateMu.Unlock()
	if h.store != nil {
		_ = h.store.TouchAgent(a.Cluster, a.Instance, lastSeen)
	}
}

func (a *GatewayAgent) relayToolCall(ctx context.Context, params api.ToolCallParams, timeout time.Duration, forward func(gatewayWSMessage) error) error {
	id := atomic.AddInt64(&a.reqID, 1)
	ch := a.registerPending(id)
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
		select {
		case <-ctx.Done():
			_ = a.writeJSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"method":  "tools/cancel",
				"id":      id,
				"params":  map[string]interface{}{"id": id},
			})
			return ctx.Err()
		case msg := <-ch:
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
		case <-timer.C:
			_ = a.writeJSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"method":  "tools/cancel",
				"id":      id,
				"params":  map[string]interface{}{"id": id},
			})
			return fmt.Errorf("operation timed out")
		case <-a.closed:
			return fmt.Errorf("agent disconnected")
		}
	}
}

func (a *GatewayAgent) registerPending(id int64) chan gatewayWSMessage {
	ch := make(chan gatewayWSMessage, 256)
	a.pendingMu.Lock()
	a.pending[id] = ch
	a.pendingMu.Unlock()
	return ch
}

func (a *GatewayAgent) unregisterPending(id int64) {
	a.pendingMu.Lock()
	delete(a.pending, id)
	a.pendingMu.Unlock()
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
	for id := range a.pending {
		delete(a.pending, id)
	}
	a.active = nil
	for sessionID := range a.activeBySession {
		delete(a.activeBySession, sessionID)
	}
	a.pendingMu.Unlock()
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
	case "doops_shell":
		return ActionExec
	case "doops_agent_prompt":
		return ActionAsk
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
	case "doops_shell":
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
