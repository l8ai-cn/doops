package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/user/doops/agent/api"
)

// RunInternalToolCall 让 gateway 内部组件（如调度器）在服务端直接对某个在线 agent
// 发起一次 tool call，并把流式输出 + 最终结果文本聚合返回。它复用了与客户端 RPC
// 相同的目标串行/资源锁语义（acquireForAction），因此不会和用户操作互相踩踏。
//
// 注意：这是受信任的内部路径，不做 user RBAC 校验；调用方需自行保证来源可信。
func (h *GatewayHub) RunInternalToolCall(ctx context.Context, cluster, instance, tool string, args map[string]interface{}) (string, error) {
	agent := h.getAgent(cluster, instance)
	if agent == nil {
		return "", fmt.Errorf("target offline: %s/%s", cluster, instance)
	}

	rawArgs, err := json.Marshal(args)
	if err != nil {
		return "", fmt.Errorf("invalid tool args: %w", err)
	}
	action := actionForTool(tool, rawArgs)
	if action == "" {
		return "", fmt.Errorf("unknown doops action for tool: %s", tool)
	}
	resourceKey := resourceKeyForTool(action, tool, rawArgs, cluster, instance)
	if err := agent.acquireForAction(ctx, action, resourceKey, h.opts.MaxQueuedPerTarget, h.opts.TargetQueueTimeout); err != nil {
		return "", err
	}
	defer agent.releaseForAction(action, resourceKey)

	var out strings.Builder
	relayErr := agent.relayToolCall(ctx, api.ToolCallParams{Name: tool, Arguments: rawArgs}, h.opts.OperationTimeout, func(msg gatewayWSMessage) error {
		if method, _ := msg.Parsed["method"].(string); method == "notifications/message" {
			if p, ok := msg.Parsed["params"].(map[string]interface{}); ok {
				if chunk, ok := p["data"].(string); ok {
					out.WriteString(chunk)
				}
			}
			return nil
		}
		if _, ok := msg.Parsed["id"]; ok {
			if result, ok := msg.Parsed["result"].(map[string]interface{}); ok {
				if text := resultContentText(result); text != "" {
					if out.Len() > 0 {
						out.WriteString("\n")
					}
					out.WriteString(text)
				}
				if isErr, ok := result["isError"]; ok && fmt.Sprintf("%v", isErr) == "true" {
					return fmt.Errorf("tool returned an error")
				}
			} else if rpcErr, ok := msg.Parsed["error"]; ok && rpcErr != nil {
				return fmt.Errorf("%v", rpcErr)
			}
		}
		return nil
	})
	return out.String(), relayErr
}

func resultContentText(result map[string]interface{}) string {
	content, _ := result["content"].([]interface{})
	var sb strings.Builder
	for _, item := range content {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		if m["type"] == "text" {
			if text, ok := m["text"].(string); ok && text != "" {
				if sb.Len() > 0 {
					sb.WriteString("\n")
				}
				sb.WriteString(text)
			}
		}
	}
	return sb.String()
}
