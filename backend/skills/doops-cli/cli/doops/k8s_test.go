package main

import (
	"reflect"
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

func TestBuildK8SRequestDeployImage(t *testing.T) {
	req, err := buildK8SRequest([]string{
		"deploy-image",
		"deploy/exam-api",
		"--container", "app",
		"--image", "registry.example.com/example/exam-api:v2",
		"--target", "prod",
		"--namespace", "oilan-system",
	})
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	want := map[string]interface{}{
		"operation": "set-image",
		"resource":  "deploy/exam-api",
		"container": "app",
		"image":     "registry.example.com/example/exam-api:v2",
		"namespace": "oilan-system",
	}
	if !reflect.DeepEqual(req.Payload, want) {
		t.Fatalf("payload mismatch:\n got: %#v\nwant: %#v", req.Payload, want)
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
