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

type businessInventoryRequest struct {
	Action           string               `json:"action"`
	System           *businessSystemInput `json:"system"`
	ExpectedRevision *int64               `json:"expected_revision"`
}

func readBusinessRequest(w http.ResponseWriter, r *http.Request) (businessInventoryRequest, error) {
	var req businessInventoryRequest
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, inventoryJSONBytes))
	if err != nil {
		var limit *http.MaxBytesError
		if errors.As(err, &limit) {
			return req, machineInventoryProblem{413, "request_too_large", "请求超过 2 MiB"}
		}
		return req, machineBadRequest("invalid_inventory_request", "无法读取请求")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if !utf8.Valid(body) || dec.Decode(&req) != nil || dec.Decode(new(any)) != io.EOF || req.System == nil || (req.Action != "create" && req.Action != "update") {
		return businessInventoryRequest{}, machineBadRequest("invalid_inventory_request", "请提交 create 或 update 以及完整的系统信息")
	}
	if (req.Action == "create" && req.ExpectedRevision != nil) || (req.Action == "update" && (req.ExpectedRevision == nil || *req.ExpectedRevision < 1)) {
		return req, machineBadRequest("invalid_revision", "更新系统时须提供已读取的 expected_revision")
	}
	return req, nil
}

func businessProblem(err error) machineInventoryProblem {
	var p machineInventoryProblem
	if errors.As(err, &p) {
		return p
	}
	return machineInventoryProblem{503, "business_inventory_unavailable", "业务系统台账暂不可用，请检查 Gateway 及数据库迁移"}
}

func (h *GatewayHub) HandleBusinessInventory(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	auth, err := h.authenticateUser(r)
	if err != nil {
		h.writeUserAuthError(w, r, err)
		return
	}
	var result any
	switch r.Method {
	case http.MethodGet:
		var tx *sql.Tx
		tx, err = h.store.db.BeginTx(r.Context(), &sql.TxOptions{ReadOnly: true})
		if err == nil {
			defer tx.Rollback()
			var systems []inventoryBusinessSystem
			systems, err = listBusinessInventory(tx, auth)
			result = map[string]any{"systems": systems}
		}
	case http.MethodPost:
		req, parseErr := readBusinessRequest(w, r)
		result, err = h.store.postBusinessInventory(r.Context(), auth, req, parseErr)
	default:
		w.Header().Set("Allow", "GET, POST")
		err = machineInventoryProblem{405, "method_not_allowed", "此接口支持 GET 和 POST"}
	}
	if err != nil {
		p := businessProblem(err)
		writeDoopsError(w, p.status, "", doopsErrorResponse{Error: p.code, Code: p.code, Message: p.message})
		return
	}
	writeJSONHTTP(w, result)
}

func (s *GatewayStore) postBusinessInventory(ctx context.Context, auth TokenAuth, req businessInventoryRequest, parseErr error) (any, error) {
	action := "request"
	if req.Action == "create" || req.Action == "update" {
		action = req.Action
	}
	event := AuditEvent{Action: GatewayAction("inventory:system:" + action), UserID: auth.UserID, TokenID: auth.TokenID, Session: "business-inventory-" + randomHex(10)}
	if req.System != nil && safeMachineID(req.System.AssetID) {
		event.CommandSummary = "asset_id=" + req.System.AssetID
	}
	auditFailure := machineInventoryProblem{503, "audit_unavailable", "审计暂不可用"}
	auditID, err := s.StartAudit(event)
	if err != nil {
		return nil, auditFailure
	}
	revision, err := s.applyBusinessInventory(ctx, auth, req, parseErr, event.Session, auditID)
	if err != nil {
		p := businessProblem(err)
		status := "error"
		if p.status == 403 {
			status = "forbidden"
		}
		if err := s.FinishAudit(auditID, AuditFinish{Status: status, Error: p.code}); err != nil {
			return nil, auditFailure
		}
		return nil, p
	}
	return map[string]any{"asset_id": req.System.AssetID, "revision": revision}, nil
}

func (s *GatewayStore) applyBusinessInventory(ctx context.Context, auth TokenAuth, req businessInventoryRequest, parseErr error, session string, auditID int64) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	// Reserve the writer before checking the current role, revision and sources.
	if err := tx.QueryRow(`UPDATE audit_events SET status='executing' WHERE id=? AND session=? AND status='running' RETURNING id`, auditID, session).Scan(&auditID); err != nil {
		return 0, machineInventoryProblem{503, "audit_unavailable", "无法锁定本次审计记录"}
	}
	if err := requireInventoryAdmin(tx, auth); err != nil {
		return 0, err
	}
	if parseErr != nil {
		return 0, parseErr
	}
	rev, err := saveBusinessSystem(tx, auth, *req.System, req.ExpectedRevision)
	if err != nil {
		return 0, err
	}
	finish := AuditFinish{Status: "success", CommandSummary: fmt.Sprintf("asset_id=%s revision=%d relation_source=manual", req.System.AssetID, rev)}
	if err := finishAuditWith(tx, auditID, finish); err != nil {
		return 0, machineInventoryProblem{503, "audit_unavailable", "审计写入失败"}
	}
	return rev, tx.Commit()
}
