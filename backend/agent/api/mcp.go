package api

import "encoding/json"

// --- JSON-RPC / MCP Protocol Types ---

type JSONRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
	ID      interface{}     `json:"id,omitempty"`
}

type JSONRPCResponse struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      interface{} `json:"id,omitempty"`
	Result  interface{} `json:"result,omitempty"`
	Error   *RPCError   `json:"error,omitempty"`
}

type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ShellParams 用于 doops_shell 命令执行。
type ShellParams struct {
	SessionID string `json:"session_id"`
	Command   string `json:"command"`
}

// ToolCallParams 用于 MCP tools/call 标准入参。
type ToolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// FileWriteParams 用于 doops_file_write 远程文件写入。
type FileWriteParams struct {
	SessionID string `json:"session_id"`
	Path      string `json:"path"`
	Content   string `json:"content"` // Base64 or raw string
}

// FileReadParams 用于 doops_file_read 查看远程小文本文件。
type FileReadParams struct {
	SessionID string `json:"session_id"`
	Path      string `json:"path"`
}

// DockerParams 用于 doops_docker 容器管理。
type DockerParams struct {
	SessionID string `json:"session_id"`
	Command   string `json:"command"` // Docker subcommand, e.g. "ps -a", "logs nginx"
}

// KubectlParams 用于 doops_kubectl K8s 管理。
type KubectlParams struct {
	SessionID string `json:"session_id"`
	Command   string `json:"command"` // kubectl subcommand, e.g. "get pods -A"
}

// AgentPromptParams 用于 doops_agent_prompt 代理执行。
type AgentPromptParams struct {
	SessionID   string `json:"session_id"`
	Instruction string `json:"instruction"`
	Model       string `json:"model,omitempty"`
}

// GitCloneParams clones a configured repository into a session workspace.
type GitCloneParams struct {
	SessionID string `json:"session_id"`
	URL       string `json:"url"`
	Branch    string `json:"branch"`
	Username  string `json:"username,omitempty"`
	Password  string `json:"password,omitempty"`
	Directory string `json:"directory,omitempty"`
}

// --- 异步任务相关类型 (doops_bg / doops_task_status) ---

// BgExecParams 用于 doops_bg 后台异步任务提交。
// 命令通过 setsid 在独立进程组中运行，不受 PTY session 生命周期影响。
type BgExecParams struct {
	SessionID string `json:"session_id"` // 工作区会话
	Command   string `json:"command"`    // Shell 命令
	LogPath   string `json:"log_path"`   // 可选: 自定义日志路径，默认在 session 工作区内
}

// TaskStatusParams 用于 doops_task_status 查询异步任务状态。
type TaskStatusParams struct {
	TaskID string `json:"task_id"` // 任务 ID
	Lines  int    `json:"lines"`   // 可选: 返回最后 N 行日志，默认 30
}

// TaskInfo 描述一个异步任务的状态。
type TaskInfo struct {
	TaskID   string `json:"task_id"`
	PID      int    `json:"pid"`
	Status   string `json:"status"` // "running" | "done" | "failed"
	ExitCode int    `json:"exit_code"`
	LogPath  string `json:"log_path"`
	LogTail  string `json:"log_tail"` // 最后 N 行日志
}

// NodeInfoParams 用于 doops_node_info 查看节点信息
type NodeInfoParams struct {
	SessionID string `json:"session_id,omitempty"`
}
