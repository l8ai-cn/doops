package server

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestHandleAgentUpgradeRequiresImage(t *testing.T) {
	if _, err := handleAgentUpgrade(json.RawMessage(`{"mode":"k8s"}`)); err == nil {
		t.Fatal("expected image to be required")
	}
}

func TestHandleAgentUpgradeDryRunK8s(t *testing.T) {
	out, err := handleAgentUpgrade(json.RawMessage(`{
		"mode":"k8s",
		"image":"repo.example/doops-agent:latest",
		"namespace":"ai",
		"workload":"daemonset/doops-agent",
		"container":"doops-agent",
		"dry_run":true
	}`))
	if err != nil {
		t.Fatalf("dry-run k8s upgrade: %v", err)
	}
	if !strings.Contains(out, "kubectl -n ai set image daemonset/doops-agent doops-agent=repo.example/doops-agent:latest") {
		t.Fatalf("unexpected dry-run output: %s", out)
	}
	if !strings.Contains(out, "pull image before rollout") {
		t.Fatalf("dry-run should document pre-pull before rollout: %s", out)
	}
}
