package server

import (
	"context"
	"testing"
	"time"
)

func TestUpgradeCoordinatorCompletesAfterNewGenerationHeartbeat(t *testing.T) {
	store, err := OpenGatewayStore(t.TempDir() + "/gateway.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	registry, err := NewAgentRegistry(store)
	if err != nil {
		t.Fatalf("new registry: %v", err)
	}
	old := &GatewayAgent{Cluster: "dev", Instance: "local", Key: "dev/local", ConnectedAt: time.Now().UTC(), LastSeen: time.Now().UTC()}
	if err := registry.Register(old); err != nil {
		t.Fatalf("register old session: %v", err)
	}
	coordinator := NewUpgradeCoordinator(store, registry)
	op, err := coordinator.Begin("dev", "local", "registry.example/doops@sha256:abc", old.Generation)
	if err != nil {
		t.Fatalf("begin upgrade: %v", err)
	}

	go func() {
		time.Sleep(20 * time.Millisecond)
		now := time.Now().UTC()
		replacement := &GatewayAgent{
			Cluster:     "dev",
			Instance:    "local",
			Key:         "dev/local",
			ConnectedAt: now,
			LastSeen:    now,
			HeartbeatAt: now,
		}
		_ = registry.Register(replacement)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	replacement, err := coordinator.WaitForReplacement(ctx, op)
	if err != nil {
		t.Fatalf("wait for replacement: %v", err)
	}
	if replacement.Generation <= old.Generation {
		t.Fatalf("replacement generation did not advance: old=%d new=%d", old.Generation, replacement.Generation)
	}
	stored, err := store.GetUpgradeOperation(op.ID)
	if err != nil {
		t.Fatalf("get upgrade operation: %v", err)
	}
	if stored.Status != "success" || stored.Phase != "replacement_healthy" {
		t.Fatalf("unexpected completed operation: %#v", stored)
	}
}
