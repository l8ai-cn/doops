package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestToolTraceCollectorPublishesCompletedRuntimeCallCatalog(t *testing.T) {
	catalogPath := filepath.Join(t.TempDir(), "runtime-tool-calls.json")
	collector := newDoagentToolTraceCollector(catalogPath)

	if _, _, err := collector.collect(map[string]interface{}{
		"sessionUpdate": "tool_call_update",
		"toolCallId":    "call-source",
		"toolName":      "Bash",
		"title":         "Observe source revision",
		"status":        "completed",
		"resultText":    `{"revision":"immutable"}`,
	}); err != nil {
		t.Fatalf("collect completed tool call: %v", err)
	}

	data, err := os.ReadFile(catalogPath)
	if err != nil {
		t.Fatalf("read runtime tool call catalog: %v", err)
	}
	var catalog struct {
		ToolCalls []doagentToolTrace `json:"toolCalls"`
	}
	if err := json.Unmarshal(data, &catalog); err != nil {
		t.Fatalf("decode runtime tool call catalog: %v", err)
	}
	if len(catalog.ToolCalls) != 1 {
		t.Fatalf("expected one completed tool call, got %#v", catalog.ToolCalls)
	}
	call := catalog.ToolCalls[0]
	if call.ToolCallID != "call-source" || call.ToolName != "Bash" ||
		call.Status != "completed" || call.ResultDigest == "" {
		t.Fatalf("unexpected runtime tool call catalog entry: %#v", call)
	}
}
