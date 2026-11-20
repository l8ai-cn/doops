package server

import (
	"context"
	"errors"
	"sort"
	"strings"
)

// Project on the Agent. No Pod data, Namespace annotations, or credentials are
// returned over the tunnel. The delimiters distinguish an empty list from lost
// output and the terminal tool receipt from streamed kubectl output.
const inventoryListOutput = `jsonpath={"namespaces-begin\n"}{range .items[*]}{.metadata.name}{"\t"}{.metadata.uid}{"\n"}{end}{"namespaces-end\n"}`
const inventoryPodsOutput = `jsonpath={"pods-listed\n"}`

func (h *GatewayHub) readInventoryNamespaces(ctx context.Context, req inventoryRequest, session string) ([]inventoryNamespace, error) {
	args := map[string]any{"session_id": session, "operation": "get", "kind": "namespaces", "output": inventoryListOutput}
	if req.Namespace != "" {
		args["kind"], args["namespace"], args["output"] = "pods", string(req.Namespace), inventoryPodsOutput
	}
	output, err := h.RunInternalToolCall(ctx, req.Cluster, req.Instance, "doops_k8s", args)
	if err != nil {
		switch {
		case errors.Is(err, errGatewayAgentDisconnected), strings.HasPrefix(err.Error(), "target offline:"):
			return nil, inventoryFailure{503, "target_offline"}
		case errors.Is(ctx.Err(), context.DeadlineExceeded), errors.Is(err, context.DeadlineExceeded), err.Error() == "operation timed out":
			return nil, inventoryFailure{504, "inventory_read_timeout"}
		case strings.Contains(output, "(Forbidden)"):
			return nil, inventoryFailure{403, "kubernetes_read_forbidden"}
		default:
			return nil, inventoryFailure{502, "inventory_read_failed"}
		}
	}
	return parseInventoryOutput(output, req.Namespace)
}

func parseInventoryOutput(output string, namespace inventoryNamespaceName) ([]inventoryNamespace, error) {
	invalid := inventoryFailure{502, "invalid_inventory_evidence"}
	data, ok := strings.CutSuffix(output, "\nKubernetes operation complete.")
	if !ok {
		return nil, invalid
	}
	// kubectl emits server warnings on stderr, multiplexed with stdout by
	// doops_k8s. Only its standard Warning: lines can be ignored.
	var lines []string
	for line := range strings.SplitSeq(strings.TrimSuffix(data, "\n"), "\n") {
		if !strings.HasPrefix(line, "Warning: ") {
			lines = append(lines, line)
		}
	}
	if namespace != "" {
		if len(lines) != 1 || lines[0] != "pods-listed" {
			return nil, invalid
		}
		return []inventoryNamespace{{Name: string(namespace)}}, nil
	}
	if len(lines) < 2 || lines[0] != "namespaces-begin" || lines[len(lines)-1] != "namespaces-end" {
		return nil, invalid
	}
	namespaces := make([]inventoryNamespace, 0, len(lines)-2)
	seen := make(map[string]bool, len(lines)-2)
	for _, line := range lines[1 : len(lines)-1] {
		name, uid, ok := strings.Cut(line, "\t")
		if !ok || !inventoryNamespacePattern.MatchString(name) || !inventoryKeyPattern.MatchString(uid) || seen[name] {
			return nil, invalid
		}
		seen[name] = true
		namespaces = append(namespaces, inventoryNamespace{Name: name, UID: uid})
	}
	sort.Slice(namespaces, func(i, j int) bool { return namespaces[i].Name < namespaces[j].Name })
	return namespaces, nil
}
