package server

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

type ClientService struct {
	store    *GatewayStore
	registry *AgentRegistry
	opts     GatewayHubOptions
	upgrades *UpgradeCoordinator

	opsMu   sync.Mutex
	active  int
	userOps map[string]int

	activeOpsMu sync.Mutex
	activeOps   map[string]*gatewayActiveOperation
	activeSeq   uint64
}

func NewClientService(store *GatewayStore, registry *AgentRegistry, opts GatewayHubOptions) *ClientService {
	return &ClientService{
		store:     store,
		registry:  registry,
		opts:      opts,
		upgrades:  NewUpgradeCoordinator(store, registry),
		userOps:   make(map[string]int),
		activeOps: make(map[string]*gatewayActiveOperation),
	}
}

func (s *ClientService) acquireOperationSlot(userID string) (func(), error) {
	s.opsMu.Lock()
	defer s.opsMu.Unlock()
	if s.active >= s.opts.MaxConcurrentOperations {
		return nil, fmt.Errorf("global operation limit exceeded")
	}
	if s.userOps[userID] >= s.opts.MaxConcurrentPerUser {
		return nil, fmt.Errorf("user operation limit exceeded")
	}
	s.active++
	s.userOps[userID]++
	return func() {
		s.opsMu.Lock()
		defer s.opsMu.Unlock()
		s.active--
		s.userOps[userID]--
		if s.userOps[userID] <= 0 {
			delete(s.userOps, userID)
		}
	}, nil
}

func (s *ClientService) registerActiveOperation(op GatewayActiveOperation, cancel context.CancelFunc) string {
	id := fmt.Sprintf("op-%d", atomic.AddUint64(&s.activeSeq, 1))
	op.ID = id
	op.StartedAt = time.Now().UTC()
	s.activeOpsMu.Lock()
	s.activeOps[id] = &gatewayActiveOperation{
		GatewayActiveOperation: op,
		cancel:                 cancel,
	}
	s.activeOpsMu.Unlock()
	return id
}

func (s *ClientService) finishActiveOperation(id string) {
	if id == "" {
		return
	}
	s.activeOpsMu.Lock()
	delete(s.activeOps, id)
	s.activeOpsMu.Unlock()
}

func (s *ClientService) listActiveOperations() []GatewayActiveOperation {
	now := time.Now().UTC()
	s.activeOpsMu.Lock()
	defer s.activeOpsMu.Unlock()
	ops := make([]GatewayActiveOperation, 0, len(s.activeOps))
	for _, op := range s.activeOps {
		item := op.GatewayActiveOperation
		item.AgeSeconds = int64(now.Sub(item.StartedAt).Seconds())
		ops = append(ops, item)
	}
	sort.Slice(ops, func(i, j int) bool {
		return ops[i].StartedAt.Before(ops[j].StartedAt)
	})
	return ops
}

func (s *ClientService) cancelActiveOperation(id string) bool {
	s.activeOpsMu.Lock()
	op := s.activeOps[id]
	s.activeOpsMu.Unlock()
	if op == nil {
		return false
	}
	op.cancel()
	return true
}
