package server

import (
	"log"
	"net/http"
	"strings"
	"time"
)

type AgentRegistrationHandler struct {
	store       *GatewayStore
	registry    *AgentRegistry
	lease       time.Duration
	acknowledge func(*GatewayAgent) error
}

func NewAgentRegistrationHandler(store *GatewayStore, registry *AgentRegistry, lease time.Duration) *AgentRegistrationHandler {
	return &AgentRegistrationHandler{
		store:    store,
		registry: registry,
		lease:    lease,
		acknowledge: func(session *GatewayAgent) error {
			return session.writeJSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"method":  "gateway/registered",
			})
		},
	}
}

func (h *AgentRegistrationHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	cluster := strings.TrimSpace(r.URL.Query().Get("cluster"))
	instance := strings.TrimSpace(r.URL.Query().Get("instance"))
	if cluster == "" || instance == "" {
		http.Error(w, "cluster and instance are required", http.StatusBadRequest)
		return
	}
	auth, err := h.store.VerifyAgentToken(bearerToken(r))
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
		log.Printf("[gateway] agent websocket upgrade failed: %v", err)
		return
	}
	session := newGatewayAgentSession(cluster, instance, auth.TokenID, r.RemoteAddr, conn)
	go session.readLoop(h.registry, h.lease)
	if err := session.initialize(); err != nil {
		log.Printf("[gateway] agent initialize failed: %s: %v", session.Key, err)
		_ = conn.Close()
		<-session.closed
		return
	}
	if err := h.acknowledge(session); err != nil {
		log.Printf("[gateway] agent registration acknowledgement failed: %s: %v", session.Key, err)
		_ = conn.Close()
		<-session.closed
		return
	}
	if err := h.registry.Register(session); err != nil {
		log.Printf("[gateway] agent registration failed: %s: %v", session.Key, err)
		_ = conn.Close()
		<-session.closed
		return
	}
	log.Printf("[gateway] agent online: %s generation=%d remote=%s", session.Key, session.Generation, session.Remote)
	go session.pingLoop(h.registry, h.lease)

	<-session.closed
	if err := h.registry.Unregister(session); err != nil {
		log.Printf("[gateway] agent offline persistence failed: %s generation=%d: %v", session.Key, session.Generation, err)
	}
	log.Printf("[gateway] agent offline: %s generation=%d", session.Key, session.Generation)
}
