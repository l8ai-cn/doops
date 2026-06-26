package server

import (
	"os"
	"strings"
)

// agentAutoApproveEnabled 仅在显式设置 DOOPS_AGENT_AUTO_APPROVE 为真值时返回 true。
// 真值包括 1/true/yes/on（大小写不敏感）。默认关闭，保持 doagent 的安全默认模式，
// 工具调用需逐项确认，避免无人值守地自动放行任意工具。
func agentAutoApproveEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("DOOPS_AGENT_AUTO_APPROVE"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
