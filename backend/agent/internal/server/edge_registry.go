package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

const defaultEdgeMaxSessionsPerTarget = 8

type EdgeRegistry struct {
	store *GatewayStore

	mu          sync.Mutex
	sessions    map[string][]*edgeGatewaySession
	generations map[string]uint64
	routes      atomic.Pointer[edgeRouteSnapshot]
}

type edgeGatewaySession struct {
	cluster    string
	instance   string
	remote     string
	generation uint64
	ws         *edgeWSSession
	mu         sync.Mutex
	streams    map[uint32]net.Conn
}

func NewEdgeRegistry(store *GatewayStore) (*EdgeRegistry, error) {
	if store == nil {
		return nil, errors.New("gateway store is required")
	}
	registry := &EdgeRegistry{
		store:       store,
		sessions:    make(map[string][]*edgeGatewaySession),
		generations: make(map[string]uint64),
	}
	if err := registry.Reload(); err != nil {
		return nil, err
	}
	return registry, nil
}

func (r *EdgeRegistry) Reload() error {
	snapshot, err := r.store.LoadEdgeRouteSnapshot()
	if err != nil {
		return err
	}
	r.routes.Store(snapshot)
	r.mu.Lock()
	sessions := make([]*edgeGatewaySession, 0)
	for _, list := range r.sessions {
		sessions = append(sessions, list...)
	}
	r.mu.Unlock()
	for _, session := range sessions {
		session.pushRoutes(snapshot)
	}
	return nil
}

func (r *EdgeRegistry) Snapshot() *edgeRouteSnapshot {
	return r.routes.Load()
}

func (r *EdgeRegistry) SessionCount(cluster, instance string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sessions[tunnelKey(cluster, instance)])
}

func (r *EdgeRegistry) Pick(cluster, instance string) *edgeGatewaySession {
	r.mu.Lock()
	defer r.mu.Unlock()
	list := r.sessions[tunnelKey(cluster, instance)]
	var best *edgeGatewaySession
	for _, session := range list {
		select {
		case <-session.ws.closed:
			continue
		default:
		}
		if best == nil || session.ws.sched.BudgetUsed() < best.ws.sched.BudgetUsed() {
			best = session
		}
	}
	return best
}

func (h *GatewayHub) HandleEdgeConnect(w http.ResponseWriter, r *http.Request) {
	cluster := strings.TrimSpace(r.URL.Query().Get("cluster"))
	instance := strings.TrimSpace(r.URL.Query().Get("instance"))
	if cluster == "" || instance == "" {
		http.Error(w, "cluster and instance are required", http.StatusBadRequest)
		return
	}
	enrollment, err := h.store.VerifyEdgeEnrollment(bearerToken(r), cluster, instance)
	if err != nil {
		writeDoopsError(w, http.StatusUnauthorized, `Bearer realm="doops-edge"`, doopsErrorResponse{
			Error:      "unauthorized",
			Code:       "edge_enrollment_auth_failed",
			AuthDomain: "edge_enrollment",
			Message:    "Edge enrollment token is missing, invalid, expired, revoked, or bound to another target",
			NextAction: "Use the active enrollment token issued for this exact cluster and instance; do not use a Gateway user token or managed credential",
		})
		return
	}
	if h.edge.SessionCount(cluster, instance) >= defaultEdgeMaxSessionsPerTarget {
		http.Error(w, "too many edge sessions", http.StatusConflict)
		return
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[gateway] edge websocket upgrade failed: %v", err)
		return
	}
	session := newEdgeGatewaySession(cluster, instance, r.RemoteAddr, conn)
	if err := h.edge.register(session); err != nil {
		log.Printf("[gateway] edge register failed: %s: %v", tunnelKey(cluster, instance), err)
		session.ws.close()
		return
	}
	_ = enrollment
	session.pushRoutes(h.edge.Snapshot())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go session.ws.writer(ctx)
	go session.ws.pingLoop(ctx)
	go session.expireOpening(ctx)
	session.ws.readLoop(session.handleFrame)
	cancel()
	h.edge.unregister(session)
}

func newEdgeGatewaySession(cluster, instance, remote string, conn *websocket.Conn) *edgeGatewaySession {
	return &edgeGatewaySession{
		cluster:  cluster,
		instance: instance,
		remote:   remote,
		ws:       newEdgeWSSession(conn),
		streams:  make(map[uint32]net.Conn),
	}
}

func (s *edgeGatewaySession) pushRoutes(snapshot *edgeRouteSnapshot) {
	if snapshot == nil {
		return
	}
	payload, err := json.Marshal(struct {
		Version int64       `json:"version"`
		Routes  []EdgeRoute `json:"routes"`
	}{Version: snapshot.version, Routes: snapshot.forTarget(s.cluster, s.instance)})
	if err != nil || len(payload) == 0 {
		return
	}
	_ = s.ws.sched.EnqueueControl(edgeFrame{Type: edgeFrameRoutes, ConnID: edgeControlConnID, Payload: payload})
}

func (s *edgeGatewaySession) attach(id uint32, conn net.Conn) {
	s.mu.Lock()
	s.streams[id] = conn
	s.mu.Unlock()
}

func (s *edgeGatewaySession) detach(id uint32) net.Conn {
	s.mu.Lock()
	conn := s.streams[id]
	delete(s.streams, id)
	s.mu.Unlock()
	return conn
}

func (s *edgeGatewaySession) lookup(id uint32) net.Conn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.streams[id]
}

func (s *edgeGatewaySession) closeAll() {
	s.mu.Lock()
	conns := make([]net.Conn, 0, len(s.streams))
	for id, conn := range s.streams {
		conns = append(conns, conn)
		delete(s.streams, id)
	}
	s.mu.Unlock()
	for _, conn := range conns {
		_ = conn.Close()
	}
}

func (s *edgeGatewaySession) expireOpening(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.ws.closed:
			return
		case now := <-ticker.C:
			s.ws.sched.ExpireOpening(now)
		}
	}
}

func (s *edgeGatewaySession) handleFrame(frame edgeFrame) error {
	switch frame.Type {
	case edgeFrameRoutes, edgeFrameStats:
		return nil
	case edgeFrameData:
		if _, err := s.ws.sched.HandleRemote(frame); err != nil {
			return err
		}
		if conn := s.lookup(frame.ConnID); conn != nil {
			if _, err := conn.Write(frame.Payload); err != nil {
				_ = s.ws.sched.LocalClose(frame.ConnID, edgeCloseUpstreamFailure, err.Error())
				if detached := s.detach(frame.ConnID); detached != nil {
					_ = detached.Close()
				}
			}
		}
		return nil
	case edgeFrameShutdown:
		action, err := s.ws.sched.HandleRemote(frame)
		if err != nil {
			return err
		}
		if conn := s.lookup(frame.ConnID); conn != nil && action == edgeActionCloseWrite {
			closeWrite(conn)
		}
		if action == edgeActionClose {
			if conn := s.detach(frame.ConnID); conn != nil {
				_ = conn.Close()
			}
		}
		return nil
	case edgeFrameClose:
		action, err := s.ws.sched.HandleRemote(frame)
		if err != nil {
			return err
		}
		if action == edgeActionClose {
			if conn := s.detach(frame.ConnID); conn != nil {
				_ = conn.Close()
			}
		}
		return nil
	default:
		_, err := s.ws.sched.HandleRemote(frame)
		return err
	}
}

func (s *edgeGatewaySession) pipePublic(ctx context.Context, id uint32, conn net.Conn) {
	defer func() {
		if detached := s.detach(id); detached != nil {
			_ = detached.Close()
		}
	}()
	pipeEdgeTCP(ctx, s.ws, id, conn)
}

// Keep the socket and its admission reservation until both FINs have drained.
// A scheduler close or tunnel shutdown must also interrupt a blocked TCP Read.
func pipeEdgeTCP(ctx context.Context, ws *edgeWSSession, id uint32, conn net.Conn) {
	stream, status := ws.sched.Lookup(id)
	if status != edgeIDLive {
		return
	}
	defer ws.sched.LocalClose(id, edgeCloseNormal, "")
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		select {
		case <-stream.done:
		case <-ws.closed:
		case <-ctx.Done():
		case <-finished:
			return
		}
		_ = conn.Close()
	}()
	buf := make([]byte, int(edgeFramePayloadBytes()))
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			if waitErr := ws.sched.WaitEnqueue(ctx, id, buf[:n]); waitErr != nil {
				_ = ws.sched.LocalClose(id, edgeCloseUpstreamFailure, waitErr.Error())
				return
			}
		}
		if errors.Is(err, io.EOF) {
			if err := ws.sched.LocalShutdown(id); err == nil {
				select {
				case <-stream.done:
				case <-ws.closed:
				case <-ctx.Done():
				}
			}
			return
		}
		if err != nil {
			_ = ws.sched.LocalClose(id, edgeCloseUpstreamFailure, err.Error())
			return
		}
	}
}

func (r *EdgeRegistry) register(session *edgeGatewaySession) error {
	r.mu.Lock()
	key := tunnelKey(session.cluster, session.instance)
	defer r.mu.Unlock()
	count := len(r.sessions[key])
	if count >= defaultEdgeMaxSessionsPerTarget {
		return errors.New("too many edge sessions")
	}
	generation := r.generations[key] + 1
	if err := r.store.MarkEdgeAgentOnline(session.cluster, session.instance, session.remote, count+1, generation); err != nil {
		return err
	}
	r.generations[key] = generation
	session.generation = generation
	r.sessions[key] = append(r.sessions[key], session)
	return nil
}

func (r *EdgeRegistry) unregister(session *edgeGatewaySession) {
	session.ws.close()
	session.closeAll()
	r.mu.Lock()
	key := tunnelKey(session.cluster, session.instance)
	list := r.sessions[key]
	filtered := list[:0]
	for _, item := range list {
		if item != session {
			filtered = append(filtered, item)
		}
	}
	if len(filtered) == 0 {
		delete(r.sessions, key)
	} else {
		r.sessions[key] = filtered
	}
	if len(filtered) == 0 {
		_ = r.store.MarkEdgeAgentOffline(session.cluster, session.instance, 0)
	} else {
		latest := filtered[len(filtered)-1]
		_ = r.store.MarkEdgeAgentOnline(latest.cluster, latest.instance, latest.remote, len(filtered), latest.generation)
	}
	r.mu.Unlock()
}

func closeWrite(conn net.Conn) {
	type closer interface{ CloseWrite() error }
	if cw, ok := conn.(closer); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = conn.Close()
}
