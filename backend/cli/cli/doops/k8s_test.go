package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestBuildK8SRequestGetPods(t *testing.T) {
	req, err := buildK8SRequest([]string{"get", "pods", "--target", "prod", "--namespace", "oilan-system", "--output", "json"})
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if req.Target != "prod" {
		t.Fatalf("target mismatch: %q", req.Target)
	}
	want := map[string]interface{}{
		"operation": "get",
		"kind":      "pods",
		"namespace": "oilan-system",
		"output":    "json",
	}
	if !reflect.DeepEqual(req.Payload, want) {
		t.Fatalf("payload mismatch:\n got: %#v\nwant: %#v", req.Payload, want)
	}
}

func TestBuildK8SRequestLogs(t *testing.T) {
	req, err := buildK8SRequest([]string{"logs", "deploy/exam-api", "--target", "prod", "--namespace", "oilan-system", "--tail", "100"})
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	want := map[string]interface{}{
		"operation": "logs",
		"resource":  "deploy/exam-api",
		"namespace": "oilan-system",
		"tail":      100,
	}
	if !reflect.DeepEqual(req.Payload, want) {
		t.Fatalf("payload mismatch:\n got: %#v\nwant: %#v", req.Payload, want)
	}
}

func TestBuildK8SRequestTopNodes(t *testing.T) {
	req, err := buildK8SRequest([]string{"top", "nodes", "--target", "prod"})
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	want := map[string]interface{}{
		"operation": "top",
		"kind":      "nodes",
	}
	if !reflect.DeepEqual(req.Payload, want) {
		t.Fatalf("payload mismatch:\n got: %#v\nwant: %#v", req.Payload, want)
	}
}

func TestBuildK8SRequestRolloutRestart(t *testing.T) {
	req, err := buildK8SRequest([]string{"rollout", "restart", "deploy/exam-api", "--target", "prod", "--namespace", "oilan-system"})
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	want := map[string]interface{}{
		"operation": "rollout-restart",
		"resource":  "deploy/exam-api",
		"namespace": "oilan-system",
	}
	if !reflect.DeepEqual(req.Payload, want) {
		t.Fatalf("payload mismatch:\n got: %#v\nwant: %#v", req.Payload, want)
	}
}

func TestRunK8SMutationsWithoutConfirm(t *testing.T) {
	for _, tc := range []struct {
		args      []string
		operation string
	}{
		{[]string{"rollout", "restart", "deployment/api"}, "rollout-restart"},
		{[]string{"scale", "deployment/api", "--replicas", "2"}, "scale"},
		{[]string{"node", "cordon", "node-a"}, "cordon"},
		{[]string{"node", "uncordon", "node-a"}, "uncordon"},
		{[]string{"delete", "pod", "pod-a"}, "delete-pod"},
	} {
		t.Run(tc.operation, func(t *testing.T) {
			req, err := buildK8SRequest(append(tc.args, "--target", "prod"))
			if err != nil {
				t.Fatalf("build explicit mutation: %v", err)
			}
			caller := &fakeK8SCaller{}
			if _, err := runK8SRequest(caller, req); err != nil {
				t.Fatalf("run explicit mutation: %v", err)
			}
			if len(caller.calls) != 1 || caller.calls[0].tool != "doops_k8s" || caller.calls[0].args["operation"] != tc.operation {
				t.Fatalf("mutation was not dispatched exactly once: %#v", caller.calls)
			}
			if _, exists := caller.calls[0].args["confirm"]; exists {
				t.Fatalf("removed confirm field sent: %#v", caller.calls)
			}
		})
	}
}

func TestBuildK8SRequestRejectsRemovedConfirmFlag(t *testing.T) {
	_, err := buildK8SRequest([]string{"node", "cordon", "node-a", "--target", "prod", "--confirm"})
	if err == nil || !strings.Contains(err.Error(), "unknown k8s flag --confirm") {
		t.Fatalf("removed flag accepted: %v", err)
	}
}

func TestBuildK8SRequestScaleRequiresReplicas(t *testing.T) {
	_, err := buildK8SRequest([]string{
		"scale", "deployment/api", "--target", "prod",
	})
	if err == nil {
		t.Fatal("expected missing replicas to fail")
	}
}

func TestBuildK8SRequestRejectsExtraPositionals(t *testing.T) {
	_, err := buildK8SRequest([]string{
		"delete", "pod", "pod-a", "pod-b", "--target", "prod",
	})
	if err == nil {
		t.Fatal("expected extra positional argument to fail")
	}
}

func TestBuildK8SRequestRejectsMissingTarget(t *testing.T) {
	if _, err := buildK8SRequest([]string{"get", "pods", "--namespace", "oilan-system"}); err == nil {
		t.Fatal("expected missing target to fail")
	}
}

func TestRunK8SRequestDirectCallsDoopsK8S(t *testing.T) {
	req, err := buildK8SRequest([]string{"get", "pods", "--target", "prod", "--namespace", "oilan-system"})
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	caller := &fakeK8SCaller{}
	msg, err := runK8SRequest(caller, req)
	if err != nil {
		t.Fatalf("run request: %v", err)
	}
	if msg != "" {
		t.Fatalf("unexpected message: %q", msg)
	}
	if len(caller.calls) != 1 || caller.calls[0].tool != "doops_k8s" || caller.calls[0].args["operation"] != "get" {
		t.Fatalf("remote call mismatch: %#v", caller.calls)
	}
}

type fakeK8SCaller struct {
	calls []fakeK8SCall
}

type fakeK8SCall struct {
	tool string
	args map[string]interface{}
}

func (f *fakeK8SCaller) Call(toolName string, arguments map[string]interface{}) error {
	copied := make(map[string]interface{}, len(arguments))
	for k, v := range arguments {
		copied[k] = v
	}
	f.calls = append(f.calls, fakeK8SCall{tool: toolName, args: copied})
	return nil
}

func (f *fakeK8SCaller) CallAndCapture(toolName string, arguments map[string]interface{}) (string, error) {
	if err := f.Call(toolName, arguments); err != nil {
		return "", err
	}
	return "verified", nil
}
