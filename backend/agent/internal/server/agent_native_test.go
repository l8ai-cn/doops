package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
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

func TestValidateDoagentApplyInstructionRequiresCICDEnvelope(t *testing.T) {
	valid := `{
		"task":"execute-doops-cicd-workflow",
		"skill":"$doops-cicd",
		"executionMode":"apply"
	}`
	if err := validateDoagentApplyInstruction(valid); err != nil {
		t.Fatalf("valid apply envelope rejected: %v", err)
	}
	for name, instruction := range map[string]string{
		"plain prompt": "deploy this",
		"wrong task":   `{"task":"shell","skill":"$doops-cicd","executionMode":"apply"}`,
		"wrong skill":  `{"task":"execute-doops-cicd-workflow","skill":"$shell","executionMode":"apply"}`,
		"wrong mode":   `{"task":"execute-doops-cicd-workflow","skill":"$doops-cicd","executionMode":"dry-run"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateDoagentApplyInstruction(instruction); err == nil {
				t.Fatal("non-CI/CD apply envelope must be rejected")
			}
		})
	}
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
