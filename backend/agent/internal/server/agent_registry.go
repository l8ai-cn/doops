package server

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// AgentRegistry is the single owner of live agent sessions and their persisted
// connectivity state.
type AgentRegistry struct {
	store *GatewayStore

	mu          sync.RWMutex
	sessions    map[string]*GatewayAgent
	generations map[string]uint64
	changed     map[string]chan struct{}
	retired     map[string][]string
}

func NewAgentRegistry(store *GatewayStore) (*AgentRegistry, error) {
	if store == nil {
		return nil, fmt.Errorf("gateway store is required")
	}
	if err := store.MarkAllAgentsOffline(); err != nil {
		return nil, fmt.Errorf("reconcile gateway agent status: %w", err)
	}
	statuses, err := store.ListAgentStatus()
	if err != nil {
		return nil, fmt.Errorf("load gateway agent generations: %w", err)
	}
	registry := &AgentRegistry{
		store:       store,
		sessions:    make(map[string]*GatewayAgent),
		generations: make(map[string]uint64),
		changed:     make(map[string]chan struct{}),
		retired:     make(map[string][]string),
	}
	for _, status := range statuses {
		registry.generations[tunnelKey(status.Cluster, status.Instance)] = status.Generation
	}
	return registry, nil
}

func (r *AgentRegistry) Register(agent *GatewayAgent) error {
	r.mu.Lock()
	key := agent.Key
	if runtimeIDRetired(r.retired[key], agent.RuntimeID) {
		if r.sessions[key] != nil {
			r.mu.Unlock()
			return fmt.Errorf("agent runtime %q was superseded for %s", agent.RuntimeID, key)
		}
		r.retired[key] = removeRetiredRuntimeID(r.retired[key], agent.RuntimeID)
	}
	old := r.sessions[key]
	generation := r.generations[key] + 1
	agent.Generation = generation
	if err := r.store.MarkAgentOnline(AgentStatus{
		Cluster:     agent.Cluster,
		Instance:    agent.Instance,
		TokenID:     agent.TokenID,
		Remote:      agent.Remote,
		Generation:  agent.Generation,
		ConnectedAt: agent.ConnectedAt,
		LastSeen:    agent.LastSeen,
	}); err != nil {
		r.mu.Unlock()
		return fmt.Errorf("persist agent online: %w", err)
	}
	if old != nil && old.RuntimeID != "" && old.RuntimeID != agent.RuntimeID {
		r.retired[key] = appendRetiredRuntimeID(r.retired[key], old.RuntimeID)
	}
	r.generations[key] = generation
	r.sessions[key] = agent
	r.notifyLocked(key)
	r.mu.Unlock()

	if old != nil && old.conn != nil {
		_ = old.conn.Close()
	}
	return nil
}

func runtimeIDRetired(retired []string, runtimeID string) bool {
	if runtimeID == "" {
		return false
	}
	for _, candidate := range retired {
		if candidate == runtimeID {
			return true
		}
	}
	return false
}

func removeRetiredRuntimeID(retired []string, runtimeID string) []string {
	filtered := retired[:0]
	for _, candidate := range retired {
		if candidate != runtimeID {
			filtered = append(filtered, candidate)
		}
	}
	return filtered
}

func appendRetiredRuntimeID(retired []string, runtimeID string) []string {
	if runtimeID == "" || runtimeIDRetired(retired, runtimeID) {
		return retired
	}
	retired = append(retired, runtimeID)
	const retainedRuntimeLimit = 8
	if len(retired) > retainedRuntimeLimit {
		retired = append([]string(nil), retired[len(retired)-retainedRuntimeLimit:]...)
	}
	return retired
}

func (r *AgentRegistry) Unregister(agent *GatewayAgent) error {
	r.mu.Lock()
	current := r.sessions[agent.Key]
	if current != agent || current.Generation != agent.Generation {
		r.mu.Unlock()
		return nil
	}
	delete(r.sessions, agent.Key)
	r.notifyLocked(agent.Key)
	r.mu.Unlock()

	agent.stateMu.RLock()
	lastSeen := agent.LastSeen
	agent.stateMu.RUnlock()
	if err := r.store.MarkAgentOffline(agent.Cluster, agent.Instance, agent.Generation, lastSeen); err != nil {
		return fmt.Errorf("persist agent offline: %w", err)
	}
	return nil
}

func (r *AgentRegistry) Touch(agent *GatewayAgent, lastSeen time.Time) error {
	return r.TouchContext(context.Background(), agent, lastSeen)
}

func (r *AgentRegistry) TouchContext(ctx context.Context, agent *GatewayAgent, lastSeen time.Time) error {
	r.mu.RLock()
	current := r.sessions[agent.Key]
	if current != agent || current.Generation != agent.Generation {
		r.mu.RUnlock()
		return nil
	}
	r.mu.RUnlock()
	err := r.store.TouchAgentContext(ctx, agent.Cluster, agent.Instance, agent.Generation, lastSeen)
	r.mu.Lock()
	if current := r.sessions[agent.Key]; current == agent && current.Generation == agent.Generation {
		r.notifyLocked(agent.Key)
	}
	r.mu.Unlock()
	if err != nil {
		return fmt.Errorf("persist agent heartbeat: %w", err)
	}
	return nil
}

func (r *AgentRegistry) NotifyHeartbeat(agent *GatewayAgent) {
	r.mu.Lock()
	if current := r.sessions[agent.Key]; current == agent && current.Generation == agent.Generation {
		r.notifyLocked(agent.Key)
	}
	r.mu.Unlock()
}

func (r *AgentRegistry) Get(cluster, instance string) *GatewayAgent {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.sessions[tunnelKey(cluster, instance)]
}

func (r *AgentRegistry) Wait(ctx context.Context, cluster, instance string, grace time.Duration) *GatewayAgent {
	key := tunnelKey(cluster, instance)
	if grace <= 0 {
		return r.Get(cluster, instance)
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	for {
		r.mu.Lock()
		if agent := r.sessions[key]; agent != nil {
			r.mu.Unlock()
			return agent
		}
		ch := r.changed[key]
		if ch == nil {
			ch = make(chan struct{})
			r.changed[key] = ch
		}
		r.mu.Unlock()

		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
			return nil
		case <-ch:
		}
	}
}

func (r *AgentRegistry) WaitForReplacementHeartbeat(ctx context.Context, cluster, instance string, generation uint64, oldRuntimeID string) *GatewayAgent {
	key := tunnelKey(cluster, instance)
	for {
		r.mu.Lock()
		if session := r.sessions[key]; session != nil &&
			session.Generation > generation &&
			session.RuntimeID != "" &&
			session.RuntimeID != oldRuntimeID {
			session.stateMu.RLock()
			heartbeatAt := session.HeartbeatAt
			session.stateMu.RUnlock()
			if !heartbeatAt.IsZero() {
				r.mu.Unlock()
				return session
			}
		}
		ch := r.changed[key]
		if ch == nil {
			ch = make(chan struct{})
			r.changed[key] = ch
		}
		r.mu.Unlock()

		select {
		case <-ctx.Done():
			return nil
		case <-ch:
		}
	}
}

func (r *AgentRegistry) List() []GatewayTarget {
	r.mu.RLock()
	out := make([]GatewayTarget, 0, len(r.sessions))
	for _, session := range r.sessions {
		out = append(out, session.snapshot())
	}
	r.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func (r *AgentRegistry) notifyLocked(key string) {
	if ch := r.changed[key]; ch != nil {
		close(ch)
	}
	r.changed[key] = make(chan struct{})
}
