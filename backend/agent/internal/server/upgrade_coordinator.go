package server

import (
	"context"
	"fmt"
	"log"
	"time"
)

type UpgradeCoordinator struct {
	store    *GatewayStore
	registry *AgentRegistry
}

func (c *UpgradeCoordinator) ResumePending(timeout time.Duration) error {
	operations, err := c.store.ListRunningUpgradeOperations()
	if err != nil {
		return fmt.Errorf("list pending upgrade operations: %w", err)
	}
	for _, operation := range operations {
		op := operation
		if op.Phase != "waiting_reconnect" {
			if err := c.store.UpdateUpgradeOperation(op.ID, "gateway_restarted", "error", "gateway restarted before upgrade entered reconnect phase"); err != nil {
				return fmt.Errorf("mark interrupted upgrade %s: %w", op.ID, err)
			}
			continue
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			if _, err := c.WaitForReplacementPrepared(ctx, op); err != nil {
				log.Printf("[gateway] resumed upgrade failed: id=%s target=%s/%s: %v", op.ID, op.Cluster, op.Instance, err)
			}
		}()
	}
	return nil
}

func NewUpgradeCoordinator(store *GatewayStore, registry *AgentRegistry) *UpgradeCoordinator {
	return &UpgradeCoordinator{store: store, registry: registry}
}

func (c *UpgradeCoordinator) Begin(cluster, instance, image string, oldGeneration uint64) (UpgradeOperation, error) {
	return c.store.CreateUpgradeOperation(UpgradeOperation{
		Cluster:       cluster,
		Instance:      instance,
		Image:         image,
		OldGeneration: oldGeneration,
	})
}

func (c *UpgradeCoordinator) WaitForReplacement(ctx context.Context, op UpgradeOperation) (*GatewayAgent, error) {
	if err := c.MarkWaiting(op); err != nil {
		return nil, fmt.Errorf("persist upgrade waiting phase: %w", err)
	}
	return c.WaitForReplacementPrepared(ctx, op)
}

func (c *UpgradeCoordinator) MarkWaiting(op UpgradeOperation) error {
	return c.store.UpdateUpgradeOperation(op.ID, "waiting_reconnect", "running", "")
}

func (c *UpgradeCoordinator) WaitForReplacementPrepared(ctx context.Context, op UpgradeOperation) (*GatewayAgent, error) {
	replacement := c.registry.WaitForNewerHeartbeat(ctx, op.Cluster, op.Instance, op.OldGeneration)
	if replacement == nil {
		errMsg := "replacement agent did not register and heartbeat before deadline"
		if err := c.store.UpdateUpgradeOperation(op.ID, "replacement_timeout", "error", errMsg); err != nil {
			return nil, fmt.Errorf("%s; persist timeout: %w", errMsg, err)
		}
		return nil, fmt.Errorf("%s", errMsg)
	}
	if err := c.store.UpdateUpgradeOperation(op.ID, "replacement_healthy", "success", ""); err != nil {
		return nil, fmt.Errorf("persist upgrade completion: %w", err)
	}
	return replacement, nil
}

func (c *UpgradeCoordinator) Fail(op UpgradeOperation, phase string, cause error) error {
	errMsg := ""
	if cause != nil {
		errMsg = cause.Error()
	}
	if err := c.store.UpdateUpgradeOperation(op.ID, phase, "error", errMsg); err != nil {
		return fmt.Errorf("persist upgrade failure: %w", err)
	}
	return nil
}
