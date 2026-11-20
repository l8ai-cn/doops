package server

import (
	"context"
	"strconv"
	"strings"
	"time"
)

// kubectl top returns rounded usage and no upstream timestamp. at_ms is the
// Gateway observation time. No API object, Pod spec or log is requested.
func (h *GatewayHub) collectMonitorPods(ctx context.Context, auth TokenAuth, req monitorRequest, session string) ([]monitorInput, error) {
	read, cancel := context.WithTimeout(contextWithInternalAuditAuth(ctx, auth), 30*time.Second)
	defer cancel()
	out, err := h.RunInternalToolCall(read, req.Cluster, req.Instance, "doops_k8s", map[string]any{"operation": "top", "kind": "pods", "namespace": req.Namespace, "session_id": session})
	if err != nil {
		return nil, monitorErr(502, "monitor_k8s_top_failed")
	}
	return parseMonitorTop(out, req.Namespace, time.Now().UnixMilli())
}
func parseMonitorTop(out, ns string, at int64) ([]monitorInput, error) {
	bad := monitorErr(502, "invalid_monitor_top_evidence")
	text, ok := strings.CutSuffix(out, "\nKubernetes operation complete.")
	if !ok {
		return nil, bad
	}
	rows := []monitorInput{}
	seen := map[string]bool{}
	header := false
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		if strings.HasPrefix(line, "Warning: ") {
			continue
		}
		f := strings.Fields(line)
		if !header {
			if strings.Join(f, " ") != "NAME CPU(cores) MEMORY(bytes)" {
				if strings.TrimSpace(text) == "No resources found in "+ns+" namespace." {
					return rows, nil
				}
				return nil, bad
			}
			header = true
			continue
		}
		if len(f) != 3 || !inventoryNodeName.MatchString(f[0]) || !monitorID(f[0]) || seen[f[0]] || len(seen) >= 2048 {
			return nil, bad
		}
		seen[f[0]] = true
		for i, key := range []string{"cpu", "memory"} {
			p := monitorInput{monitorAsset: monitorAsset{Type: "app", AssetID: f[0], Namespace: ns}, Metric: key, Unit: monitorUnits[key], monitorPoint: monitorPoint{At: at, Status: "missing"}}
			if raw := f[i+1]; raw != "<unknown>" {
				suffix, factor := "m", 0.001
				if key == "memory" {
					suffix, factor = "Mi", 1048576
				}
				number, ok := strings.CutSuffix(raw, suffix)
				if !ok {
					return nil, bad
				}
				n, err := strconv.ParseUint(number, 10, 53)
				if err != nil {
					return nil, bad
				}
				v := float64(n) * factor
				p.Value = &v
				p.Status = "ok"
			}
			rows = append(rows, p)
		}
	}
	if !header {
		return nil, bad
	}
	return rows, nil
}
