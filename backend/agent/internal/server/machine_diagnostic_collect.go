package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"time"

	"github.com/user/doops/agent/api"
)

const diagnosticReadTool = "doops_machine_diagnostic_sample"
const diagnosticFreshness = 5 * time.Minute

var diagFingerprint = regexp.MustCompile(`^[0-9a-f]{64}$`)

type diagnosticTarget struct {
	AssetID  string `json:"asset_id"`
	Cluster  string `json:"cluster"`
	Instance string `json:"instance"`
}

func (t diagnosticTarget) valid() bool {
	return safeMachineID(t.AssetID) && safeMachineID(t.Cluster) && safeMachineID(t.Instance)
}

// Only the Agent operator selects the read-only evidence root. The RPC has no path or command argument.
func agentDiagnosticSample(raw json.RawMessage) (*diagnosticSample, error) {
	var t diagnosticTarget
	if decodeStrictCredentialPayload(raw, &t) != nil || !t.valid() {
		return nil, errors.New("invalid_diagnostic_target")
	}
	now := time.Now().UnixMilli()
	v, err := readDiagnosticSample(os.Getenv("DOOPS_AGENT_MACHINE_DIAGNOSTICS_ROOT"), t.AssetID, t.Cluster, t.Instance, now-diagnosticFreshness.Milliseconds(), now, 0)
	if err != nil || v == nil {
		return nil, errors.New("fresh_host_sample_unavailable")
	}
	return v, nil
}

type diagnosticRequest struct {
	diagnosticTarget
	Action      string  `json:"action"`
	Fingerprint *string `json:"host_sha256,omitempty"`
}

func (h *GatewayHub) collectMachineDiagnostic(r *http.Request, auth TokenAuth) (any, error) {
	var req diagnosticRequest
	raw, err := io.ReadAll(io.LimitReader(r.Body, 4097))
	if err != nil || len(raw) > 4096 || r.URL.RawQuery != "" || decodeStrictCredentialPayload(raw, &req) != nil || !req.valid() {
		return nil, monitorErr(400, "invalid_diagnostic_request")
	}
	switch req.Action {
	case "bind":
		if req.Fingerprint == nil || !diagFingerprint.MatchString(*req.Fingerprint) {
			return nil, monitorErr(400, "invalid_host_fingerprint")
		}
	case "refresh":
		if req.Fingerprint != nil {
			return nil, monitorErr(400, "invalid_diagnostic_request")
		}
	default:
		return nil, monitorErr(400, "invalid_diagnostic_request")
	}
	audit, err := h.store.StartAudit(AuditEvent{UserID: auth.UserID, TokenID: auth.TokenID, Cluster: req.Cluster, Instance: req.Instance, Session: "machine-diagnostic", Action: GatewayAction("machine:diagnostic:" + req.Action), CommandSummary: req.AssetID})
	if err != nil {
		return nil, err
	}
	result, err := h.pullMachineDiagnostic(r.Context(), auth, req, audit)
	if err != nil {
		failure := inventoryFailure{503, "machine_diagnostics_unavailable"}
		errors.As(err, &failure)
		if finishErr := h.store.FinishAudit(audit, AuditFinish{Status: "error", Error: failure.code}); finishErr != nil {
			return nil, finishErr
		}
	}
	return result, err
}

type diagnosticBinding struct {
	fp       string
	revision int64
}

func checkDiagnosticTarget(ctx context.Context, tx *sql.Tx, auth TokenAuth, req diagnosticRequest, agent *GatewayAgent) (diagnosticBinding, error) {
	var state diagnosticBinding
	if err := authorizeInventory(ctx, tx, auth, inventoryRequest{Cluster: req.Cluster, Instance: req.Instance}); err != nil {
		return state, err
	}
	if req.Action == "bind" {
		var admin bool
		if err := tx.QueryRow(`SELECT is_admin FROM users WHERE id=?`, auth.UserID).Scan(&admin); err != nil {
			return state, err
		}
		if !admin {
			return state, monitorErr(403, "inventory_forbidden")
		}
	}
	err := tx.QueryRow(`SELECT m.revision,COALESCE(b.host_sha256,'') FROM inventory_machines m LEFT JOIN machine_diagnostic_bindings b USING(asset_id,cluster,instance) WHERE m.asset_id=? AND m.cluster=? AND m.instance=?`, req.AssetID, req.Cluster, req.Instance).Scan(&state.revision, &state.fp)
	if errors.Is(err, sql.ErrNoRows) {
		return state, monitorErr(404, "machine_target_unavailable")
	}
	if err != nil {
		return state, err
	}
	if os.Getenv("DOOPS_MACHINE_DIAGNOSTICS_ROOT") != "" {
		return state, monitorErr(409, "diagnostic_mount_read_only")
	}
	if req.Action == "bind" && state.fp != "" {
		return state, monitorErr(409, "host_already_bound")
	}
	if req.Action == "refresh" && state.fp == "" {
		return state, monitorErr(409, "binding_missing")
	}
	if agent == nil || agent.TokenID == "" {
		return state, monitorErr(409, "claimed_agent_required")
	}
	var claimed bool
	err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM agent_status a JOIN agent_enrollments e ON e.id=a.token_id AND e.cluster=a.cluster AND e.instance=a.instance WHERE a.cluster=? AND a.instance=? AND a.generation=? AND a.token_id=? AND a.status='online' AND a.archived_at='' AND e.confirmed_at<>'' AND e.revoked_at='')`, req.Cluster, req.Instance, agent.Generation, agent.TokenID).Scan(&claimed)
	if err != nil {
		return state, err
	}
	if !claimed {
		return state, monitorErr(409, "claimed_agent_required")
	}
	return state, nil
}

func (h *GatewayHub) pullMachineDiagnostic(ctx context.Context, auth TokenAuth, req diagnosticRequest, audit int64) (any, error) {
	agent := h.getAgent(req.Cluster, req.Instance)
	tx, err := h.store.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	state, err := checkDiagnosticTarget(ctx, tx, auth, req, agent)
	tx.Rollback()
	if err != nil {
		return nil, err
	}
	lease, err := agent.acquireActionLease(ctx, ActionWrite, "machine-diagnostic:"+req.AssetID, h.opts.MaxQueuedPerTarget, h.opts.TargetQueueTimeout)
	if err != nil {
		return nil, monitorErr(409, "diagnostic_busy")
	}
	defer lease.Release()
	args, err := json.Marshal(req.diagnosticTarget)
	if err != nil {
		return nil, err
	}
	var sample diagnosticSample
	err = agent.relayToolCall(ctx, api.ToolCallParams{Name: diagnosticReadTool, Arguments: args}, min(h.opts.OperationTimeout, 15*time.Second), func(msg gatewayWSMessage) error {
		if _, ok := msg.Parsed["id"]; !ok {
			return nil
		}
		var reply struct {
			Result json.RawMessage `json:"result"`
		}
		if json.Unmarshal(msg.Raw, &reply) != nil || decodeStrictCredentialPayload(reply.Result, &sample) != nil {
			return monitorErr(503, "fresh_host_sample_unavailable")
		}
		return nil
	})
	if err != nil {
		return nil, monitorErr(503, "fresh_host_sample_unavailable")
	}
	fp := state.fp
	if req.Action == "bind" {
		fp = *req.Fingerprint
	}
	if !sample.valid(req.AssetID, req.Cluster, req.Instance, fp) {
		return nil, monitorErr(409, "host_verification_failed")
	}
	if h.getAgent(req.Cluster, req.Instance) != agent {
		return nil, monitorErr(409, "diagnostic_target_changed")
	}
	tx, err = h.store.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	current, err := checkDiagnosticTarget(ctx, tx, auth, req, agent)
	if err != nil {
		return nil, err
	}
	if current != state {
		return nil, monitorErr(409, "diagnostic_target_changed")
	}
	now := time.Now()
	if sample.At > now.UnixMilli() || sample.At < now.Add(-diagnosticFreshness).UnixMilli() {
		return nil, monitorErr(409, "host_sample_stale")
	}
	if req.Action == "bind" {
		res, err := tx.Exec(`INSERT INTO machine_diagnostic_bindings(asset_id,cluster,instance,host_sha256) VALUES(?,?,?,?) ON CONFLICT DO NOTHING`, req.AssetID, req.Cluster, req.Instance, fp)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return nil, monitorErr(409, "host_already_bound")
		}
	}
	payload, err := json.Marshal(sample)
	if err != nil {
		return nil, err
	}
	var latest int64
	var prior string
	err = tx.QueryRow(`SELECT at_ms,payload FROM machine_diagnostic_samples WHERE asset_id=? ORDER BY at_ms DESC LIMIT 1`, req.AssetID).Scan(&latest, &prior)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if sample.At < latest || (sample.At == latest && string(payload) != prior) {
		return nil, monitorErr(409, "host_sample_stale")
	}
	if sample.At > latest {
		_, err = tx.Exec(`INSERT INTO machine_diagnostic_samples(asset_id,cluster,instance,host_sha256,at_ms,generation,payload) VALUES(?,?,?,?,?,?,?)`, req.AssetID, req.Cluster, req.Instance, fp, sample.At, agent.Generation, string(payload))
		if err != nil {
			return nil, err
		}
	}
	if _, err = tx.Exec(`DELETE FROM machine_diagnostic_samples WHERE asset_id=? AND at_ms<?`, req.AssetID, now.Add(-8*24*time.Hour).UnixMilli()); err != nil {
		return nil, err
	}
	summary := fmt.Sprintf("%s generation=%d at_ms=%d", req.AssetID, agent.Generation, sample.At)
	if err = finishAuditWith(tx, audit, AuditFinish{Status: "success", CommandSummary: summary}); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return map[string]any{"at_ms": sample.At, "generation": agent.Generation, "new_sample": sample.At > latest}, nil
}

func cachedDiagnostic(tx *sql.Tx, id, cluster, instance, fp string, from, to, exact int64) (*diagnosticSample, uint64, error) {
	if exact != 0 {
		from = exact
		to = exact
	}
	var data string
	var generation uint64
	var at int64
	err := tx.QueryRow(`SELECT at_ms,generation,payload FROM machine_diagnostic_samples WHERE asset_id=? AND cluster=? AND instance=? AND host_sha256=? AND at_ms BETWEEN ? AND ? ORDER BY at_ms DESC LIMIT 1`, id, cluster, instance, fp, from, to).Scan(&at, &generation, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	var v diagnosticSample
	if decodeStrictCredentialPayload([]byte(data), &v) != nil || v.At != at {
		return nil, 0, monitorErr(503, "invalid_host_sample")
	}
	return &v, generation, nil
}
