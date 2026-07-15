package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

type doagentToolTrace struct {
	ToolCallID   string `json:"toolCallId"`
	ToolName     string `json:"toolName"`
	Title        string `json:"title,omitempty"`
	Status       string `json:"status"`
	ResultDigest string `json:"resultDigest"`
}

type doagentToolTraceCollector struct {
	mu          sync.Mutex
	calls       map[string]doagentToolTrace
	catalogPath string
}

func newDoagentToolTraceCollector(catalogPath string) *doagentToolTraceCollector {
	return &doagentToolTraceCollector{
		calls:       make(map[string]doagentToolTrace),
		catalogPath: catalogPath,
	}
}

func (c *doagentToolTraceCollector) collect(update map[string]interface{}) (bool, bool, error) {
	updateType, _ := update["sessionUpdate"].(string)
	if updateType != "tool_call" && updateType != "tool_call_update" {
		return false, false, nil
	}
	toolCallID, _ := update["toolCallId"].(string)
	if strings.TrimSpace(toolCallID) == "" {
		return false, false, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	trace := c.calls[toolCallID]
	trace.ToolCallID = toolCallID
	if toolName, _ := update["toolName"].(string); strings.TrimSpace(toolName) != "" {
		trace.ToolName = toolName
	}
	if title, _ := update["title"].(string); strings.TrimSpace(title) != "" {
		trace.Title = title
	}
	if trace.ToolName == "" {
		trace.ToolName = trace.Title
	}
	if status, _ := update["status"].(string); strings.TrimSpace(status) != "" {
		trace.Status = status
	}
	if trace.Status == "completed" {
		resultText, _ := update["resultText"].(string)
		if resultText != "" {
			sum := sha256.Sum256([]byte(resultText))
			trace.ResultDigest = "sha256:" + hex.EncodeToString(sum[:])
		}
	}
	c.calls[toolCallID] = trace
	return false, false, c.publishLocked()
}

func (c *doagentToolTraceCollector) completed() []doagentToolTrace {
	c.mu.Lock()
	defer c.mu.Unlock()
	traces := make([]doagentToolTrace, 0, len(c.calls))
	for _, trace := range c.calls {
		if trace.Status == "completed" && trace.ResultDigest != "" {
			traces = append(traces, trace)
		}
	}
	sort.Slice(traces, func(i, j int) bool {
		return traces[i].ToolCallID < traces[j].ToolCallID
	})
	return traces
}

func (c *doagentToolTraceCollector) publishLocked() error {
	if strings.TrimSpace(c.catalogPath) == "" {
		return nil
	}
	traces := make([]doagentToolTrace, 0, len(c.calls))
	for _, trace := range c.calls {
		if trace.Status == "completed" && trace.ResultDigest != "" {
			traces = append(traces, trace)
		}
	}
	sort.Slice(traces, func(i, j int) bool {
		return traces[i].ToolCallID < traces[j].ToolCallID
	})
	return writeDoagentToolTraceCatalog(c.catalogPath, traces)
}

func writeDoagentToolTraceCatalog(path string, traces []doagentToolTrace) error {
	data, err := json.Marshal(map[string]interface{}{"toolCalls": traces})
	if err != nil {
		return fmt.Errorf("encode runtime tool call catalog: %w", err)
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".runtime-tool-calls-*.tmp")
	if err != nil {
		return fmt.Errorf("create runtime tool call catalog: %w", err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return fmt.Errorf("secure runtime tool call catalog: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return fmt.Errorf("write runtime tool call catalog: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close runtime tool call catalog: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("publish runtime tool call catalog: %w", err)
	}
	return nil
}

func attestDeploymentRun(
	result map[string]interface{}, traces []doagentToolTrace,
) (string, map[string]interface{}, error) {
	if result["kind"] != "DeploymentRun" {
		data, err := json.Marshal(result)
		return string(data), result, err
	}
	status, ok := result["status"].(map[string]interface{})
	if !ok {
		return "", nil, fmt.Errorf("DeploymentRun.status must be an object")
	}
	evidence, ok := status["evidence"].([]interface{})
	if !ok || len(evidence) == 0 {
		return "", nil, fmt.Errorf("DeploymentRun evidence must be a non-empty array")
	}
	completed := make(map[string]doagentToolTrace, len(traces))
	for _, trace := range traces {
		completed[trace.ToolCallID] = trace
	}
	for index, item := range evidence {
		entry, ok := item.(map[string]interface{})
		if !ok {
			return "", nil, fmt.Errorf("DeploymentRun evidence[%d] must be an object", index)
		}
		toolCallID, _ := entry["toolCallId"].(string)
		trace, ok := completed[toolCallID]
		if !ok {
			return "", nil, fmt.Errorf(
				"DeploymentRun evidence[%d] must reference a completed runtime tool call",
				index,
			)
		}
		module, _ := entry["module"].(string)
		if trace.ToolName != "" && module != trace.ToolName {
			return "", nil, fmt.Errorf(
				"DeploymentRun evidence[%d] module does not match runtime tool call",
				index,
			)
		}
	}
	status["runtimeAttestation"] = map[string]interface{}{"toolCalls": traces}
	delete(status, "resultDigest")
	payload, err := json.Marshal(result)
	if err != nil {
		return "", nil, fmt.Errorf("encode DeploymentRun for digest: %w", err)
	}
	sum := sha256.Sum256(payload)
	status["resultDigest"] = "sha256:" + hex.EncodeToString(sum[:])
	payload, err = json.Marshal(result)
	if err != nil {
		return "", nil, fmt.Errorf("encode attested DeploymentRun: %w", err)
	}
	return string(payload), result, nil
}
