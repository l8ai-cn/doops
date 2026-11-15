package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDoagentModeForPromptUsesOnlyNativeAskMode(t *testing.T) {
	tests := []struct {
		name          string
		operation     string
		executionMode string
		want          string
		wantErr       bool
	}{
		{name: "general", want: "auto"},
		{name: "ask", operation: "ask", want: "auto"},
		{name: "apply", operation: "apply", want: "build"},
		{name: "mode is not accepted", executionMode: "apply", wantErr: true},
		{name: "unknown operation", operation: "custom", wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := doagentModeForPrompt(test.operation, test.executionMode)
			if test.wantErr {
				if err == nil {
					t.Fatalf("expected mode mapping error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("map native mode: %v", err)
			}
			if got != test.want {
				t.Fatalf("native mode mismatch: got %q want %q", got, test.want)
			}
		})
	}
}

func TestDoagentAllowedToolsUsesPermissionEngineForDiagnostics(t *testing.T) {
	ordinary := doagentAllowedTools(false, "")
	wantOrdinary := []string{"Read", "glob", "grep", "Skill", "Bash", "mcp_doops_k8s_read", "mcp_doops_web_read"}
	if !reflect.DeepEqual(ordinary, wantOrdinary) {
		t.Fatalf("ordinary prompt tool directory = %#v, want %#v", ordinary, wantOrdinary)
	}

	apply := doagentAllowedTools(true, "")
	wantApply := []string{"Read", "glob", "grep", "Skill", "mcp_doops_k8s_read", "mcp_doops_web_read", "mcp_doops_helm"}
	if !reflect.DeepEqual(apply, wantApply) {
		t.Fatalf("apply prompt tool directory = %#v, want %#v", apply, wantApply)
	}
	for _, skill := range []string{"k8s", "image-build"} {
		tools := doagentAllowedTools(true, skill)
		if !sliceContainsString(tools, "mcp_doops_helm") || sliceContainsString(tools, "mcp_doops_web") ||
			sliceContainsString(tools, "Bash") {
			t.Fatalf("Helm skill %q cannot execute its authorized task: %#v", skill, tools)
		}
	}
	website := doagentAllowedTools(true, "doweb")
	if !sliceContainsString(website, "mcp_doops_web") || sliceContainsString(website, "mcp_doops_helm") {
		t.Fatalf("website prompt did not retain its domain boundary: %#v", website)
	}
	databaseRead := doagentAllowedTools(false, "dosql")
	if !sliceContainsString(databaseRead, "mcp_dosql_read") {
		t.Fatalf("database read prompt boundary is unsafe: %#v", databaseRead)
	}
	databaseApply := doagentAllowedTools(true, "dosql")
	if !sliceContainsString(databaseApply, "mcp_dosql_read") ||
		sliceContainsString(databaseApply, "mcp_doops_helm") || sliceContainsString(databaseApply, "mcp_doops_web") {
		t.Fatalf("database apply prompt boundary is unsafe: %#v", databaseApply)
	}
	for _, skill := range []string{"dosql", "unknown", " doweb "} {
		for _, tool := range []string{"mcp_doops_web", "mcp_doops_helm"} {
			if sliceContainsString(doagentAllowedTools(true, skill), tool) {
				t.Fatalf("skill %q gained unrelated mutation tool %q", skill, tool)
			}
		}
	}
}

func sliceContainsString(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func TestDoagentPermissionRequestFailsClosedWithoutReply(t *testing.T) {
	rpcCalled := make(chan struct{}, 1)
	doagent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/events":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintln(w, `data: {"jsonrpc":"2.0","method":"permission.updated","params":{"sessionId":"permission-session","permission":{"id":"permission-1","title":"run mutating tool","toolName":"shell"}}}`)
			fmt.Fprintln(w)
		case "/rpc":
			rpcCalled <- struct{}{}
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","result":{}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer doagent.Close()

	err := subscribeDoagentSSEWithCollector(
		context.Background(),
		doagent.URL,
		"permission-session",
		func(notificationEvent) {},
		nil,
	)
	if err == nil || !strings.Contains(err.Error(), "permission required") {
		t.Fatalf("unexpected permission result: %v", err)
	}
	select {
	case <-rpcCalled:
		t.Fatal("DoOps bridge must never reply to doagent permission requests")
	case <-time.After(100 * time.Millisecond):
	}
}
