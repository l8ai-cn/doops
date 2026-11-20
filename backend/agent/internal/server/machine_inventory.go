package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"unicode/utf8"
)

const inventoryJSONBytes = 2 << 20

type machineInventoryRequest struct {
	Action           string                 `json:"action"`
	Scope            *string                `json:"scope"`
	Machine          *machineInventoryInput `json:"machine"`
	ExpectedRevision *int64                 `json:"expected_revision"`
	PreviewToken     *string                `json:"preview_token"`
	CSV              *string                `json:"csv"`
}

type machineInventoryProblem struct {
	status  int
	code    string
	message string
}

func (e machineInventoryProblem) Error() string { return e.message }
func machineBadRequest(code, msg string) machineInventoryProblem {
	return machineInventoryProblem{http.StatusBadRequest, code, msg}
}
func publicInventoryProblem(err error) machineInventoryProblem {
	var problem machineInventoryProblem
	if errors.As(err, &problem) {
		return problem
	}
	return machineInventoryProblem{http.StatusServiceUnavailable, "machine_inventory_unavailable", "machine inventory unavailable"}
}

func readInventoryRequest(w http.ResponseWriter, r *http.Request) (machineInventoryRequest, error) {
	var req machineInventoryRequest
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, inventoryJSONBytes))
	if err != nil {
		var limit *http.MaxBytesError
		if errors.As(err, &limit) {
			return req, machineInventoryProblem{http.StatusRequestEntityTooLarge, "request_too_large", "request exceeds 2 MiB"}
		}
		return req, machineBadRequest("invalid_inventory_request", "invalid request body")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if !utf8.Valid(body) || dec.Decode(&req) != nil || dec.Decode(new(any)) != io.EOF {
		return machineInventoryRequest{}, machineBadRequest("invalid_inventory_request", "invalid JSON or unsupported inventory field")
	}
	switch req.Action {
	case "create", "update":
		if req.Machine == nil || req.CSV != nil || req.PreviewToken != nil || req.Scope != nil {
			return req, machineBadRequest("invalid_inventory_request", "expected a machine record")
		}
		if req.Action == "update" && (req.ExpectedRevision == nil || *req.ExpectedRevision < 1) {
			return req, machineBadRequest("invalid_revision", "update requires a positive expected_revision")
		}
		if req.Action == "create" && req.ExpectedRevision != nil {
			return req, machineBadRequest("invalid_revision", "expected_revision is only valid for update")
		}
	case "preview", "import":
		if req.Scope != nil && *req.Scope != machineMetadataScope && *req.Scope != machineLoginScope {
			return req, machineBadRequest("invalid_inventory_request", "unknown machine import scope")
		}
		if req.CSV == nil || req.Machine != nil || req.ExpectedRevision != nil {
			return req, machineBadRequest("invalid_inventory_request", "expected a csv string")
		}
	default:
		return machineInventoryRequest{}, machineBadRequest("invalid_inventory_request", "unsupported inventory action")
	}
	return req, nil
}

// HandleMachineInventory serves /v1/assets/machines. Only the operator SQL
// creates inventory tables. Missing schema is a service error.
func (h *GatewayHub) HandleMachineInventory(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	auth, err := h.authenticateUser(r)
	if err != nil {
		h.writeUserAuthError(w, r, err)
		return
	}
	var result any
	switch r.Method {
	case http.MethodGet:
		result, err = h.store.readMachineInventory(r, auth)
	case http.MethodPost:
		req, parseErr := readInventoryRequest(w, r)
		result, err = h.store.postMachineInventory(r.Context(), auth, req, parseErr)
	default:
		w.Header().Set("Allow", "GET, POST")
		err = machineInventoryProblem{http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed"}
	}
	if err != nil {
		p := publicInventoryProblem(err)
		writeDoopsError(w, p.status, "", doopsErrorResponse{Error: p.code, Code: p.code, Message: p.message})
		return
	}
	writeJSONHTTP(w, result)
}

func requireInventoryAdmin(tx *sql.Tx, auth TokenAuth) error {
	var allowed bool
	if err := tx.QueryRow(inventoryGrants+`SELECT EXISTS(SELECT 1 FROM permitted WHERE is_admin=1)`, auth.UserID, auth.TokenID).Scan(&allowed); err != nil {
		return err
	}
	if !allowed {
		return machineInventoryProblem{http.StatusForbidden, "inventory_forbidden", "platform administrator required"}
	}
	return nil
}

func (s *GatewayStore) readMachineInventory(r *http.Request, auth TokenAuth) (any, error) {
	tx, err := s.db.BeginTx(r.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	query := r.URL.Query()
	if query.Has("batch_id") {
		if err := requireInventoryAdmin(tx, auth); err != nil {
			return nil, err
		}
		// A preview gives the caller the batch id before any write, allowing an
		// interrupted confirmation to be looked up after reconnect or restart.
		var receipt string
		var attemptID int64
		batchID := query.Get("batch_id")
		err := tx.QueryRow(`SELECT receipt, attempt_id FROM inventory_import_attempts WHERE batch_id=? AND (?='' OR attempt_id=?) ORDER BY attempt_id DESC LIMIT 1`, batchID, query.Get("attempt_id"), query.Get("attempt_id")).Scan(&receipt, &attemptID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, machineInventoryProblem{http.StatusNotFound, "batch_not_found", "batch attempt not found"}
		}
		if err != nil {
			return nil, err
		}
		return decodeInventoryReceipt(receipt, batchID, attemptID)
	}
	machines, err := listInventoryMachines(tx, auth)
	if err != nil {
		return nil, err
	}
	return map[string]any{"machines": machines}, nil
}

func (s *GatewayStore) postMachineInventory(ctx context.Context, auth TokenAuth, req machineInventoryRequest, parseErr error) (any, error) {
	// Action is from the decoder's allowlist. Invalid payloads get a fixed label.
	action := "request"
	switch req.Action {
	case "create", "update", "preview", "import":
		action = req.Action
	}
	event := AuditEvent{UserID: auth.UserID, TokenID: auth.TokenID, Action: GatewayAction("inventory:machine:" + action), Session: "machine-inventory"}
	if req.Machine != nil && safeMachineID(req.Machine.AssetID) {
		event.CommandSummary = "asset_id=" + req.Machine.AssetID
	}
	auditID, err := s.StartAudit(event)
	if err != nil {
		return nil, machineInventoryProblem{http.StatusServiceUnavailable, "audit_unavailable", "audit unavailable"}
	}
	result, err := s.applyMachineInventory(ctx, auth, auditID, req, parseErr)
	if err != nil {
		p := publicInventoryProblem(err)
		status := "error"
		if p.status == http.StatusForbidden {
			status = "forbidden"
		}
		if finishErr := s.FinishAudit(auditID, AuditFinish{Status: status, Error: p.code}); finishErr != nil {
			return nil, machineInventoryProblem{http.StatusServiceUnavailable, "audit_unavailable", "audit unavailable"}
		}
	}
	return result, err
}

func (s *GatewayStore) applyMachineInventory(ctx context.Context, auth TokenAuth, auditID int64, req machineInventoryRequest, parseErr error) (any, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// Acquire SQLite's writer reservation before reading permissions and asset
	// state. This also serializes independent Gateway connections using this DB.
	if _, err = tx.Exec(`UPDATE audit_events SET status='executing' WHERE id=? AND status='running'`, auditID); err != nil {
		return nil, err
	}
	if err := requireInventoryAdmin(tx, auth); err != nil {
		return nil, err
	}
	if parseErr != nil {
		return nil, parseErr
	}
	var result any
	finish := AuditFinish{Status: "success"}
	if req.Machine != nil {
		m, err := saveInventoryMachine(tx, *req.Machine, req.ExpectedRevision)
		if err != nil {
			return nil, err
		}
		finish.Cluster, finish.Instance = m.Cluster, m.Instance
		finish.CommandSummary = "asset_id=" + m.AssetID
		result = map[string]any{"machine": m}
	} else {
		batch, err := s.runInventoryBatch(tx, req, auditID, auth)
		if err != nil {
			return nil, err
		}
		// Only a validated scope and CSV can supply the audit correlation ID.
		if _, err := tx.Exec(`UPDATE audit_events SET correlation_id=? WHERE id=?`, batch.BatchID, auditID); err != nil {
			return nil, machineInventoryProblem{http.StatusServiceUnavailable, "audit_unavailable", "audit unavailable"}
		}
		counts := map[string]int{}
		for _, row := range batch.Rows {
			counts[row.Status]++
		}
		finish.CommandSummary = fmt.Sprintf("batch_id=%s new=%d imported=%d duplicate=%d error=%d", batch.BatchID, counts["new"], counts["imported"], counts["duplicate"], counts["error"])
		result = batch
	}
	if err := finishAuditWith(tx, auditID, finish); err != nil {
		return nil, machineInventoryProblem{http.StatusServiceUnavailable, "audit_unavailable", "audit unavailable"}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}
