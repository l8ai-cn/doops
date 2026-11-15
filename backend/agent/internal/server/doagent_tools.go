package server

import "strings"

// doagentAllowedTools describes the tools that DoOps makes visible to the
// embedded doagent session. The PermissionEngine remains the authority for
// whether a visible tool may run; this list only selects the execution surface
// for the current task.
//
// Auto ask sessions include the native Bash tool so diagnostics that are not
// covered by a structured DoOps reader can run in the target Agent context.
// Workload mutations stay behind the explicit apply path and its mutation
// authority, so ordinary ask cannot silently turn a Helm or site operation
// into an unverified mutation.
func doagentAllowedTools(apply bool, skill string) []string {
	tools := []string{"Read", "glob", "grep", "Skill"}
	if !apply {
		tools = append(tools, "Bash")
	}
	tools = append(tools, "mcp_doops_k8s_read", "mcp_doops_web_read")
	if strings.TrimSpace(skill) == "dosql" {
		tools = append(tools, "mcp_dosql_read")
	}
	if apply {
		switch skill {
		case "", "k8s", "image-build":
			tools = append(tools, "mcp_doops_helm")
		case "doweb":
			tools = append(tools, "mcp_doops_web")
		}
	}
	return tools
}
