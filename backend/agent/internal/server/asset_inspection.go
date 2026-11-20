package server

import (
	"context"

	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"
)

var inspectionChecks = map[string]string{"M1": "运行与重启", "M2": "CPU 与内存", "M3": "可疑进程", "M4": "配置合规", "M5": "异常登录与权限变更", "V1": "组件公告匹配", "V2": "待更新补丁", "V3": "修复版本验证", "A1": "服务可用性", "A2": "数据库状态", "A3": "流量异常", "A4": "Web 安全配置", "A5": "指定 API"}

type inspectionScope struct {
	Asset    string   `json:"asset_id"`
	Cluster  string   `json:"cluster"`
	Instance string   `json:"instance"`
	Checks   []string `json:"checks"`
}
type inspectionRequest struct {
	Due        int64               `json:"due"`
	Action     string              `json:"action"`
	Name       string              `json:"name"`
	Owner      string              `json:"owner"`
	Executor   string              `json:"executor"`
	Zone       string              `json:"timezone"`
	Weekday    int                 `json:"weekday"`
	Clock      string              `json:"clock"`
	Scope      []inspectionScope   `json:"scope"`
	Plan       int64               `json:"plan_id"`
	Item       int64               `json:"item_id"`
	Generation int                 `json:"generation"`
	Memo       string              `json:"memo"`
	Evidence   *inspectionEvidence `json:"evidence"`
}

func inspectErr(status int, msg string) error {
	return machineInventoryProblem{status, "inspection_" + strconv.Itoa(status), msg}
}
func inspectText(s string, n int) bool {
	return strings.TrimSpace(s) != "" && len(s) <= n && inventoryMetadata(s)
}

const inspectAllowed = `EXISTS(SELECT 1 FROM permitted g WHERE g.cluster IN ('*',s.cluster) AND g.instance IN ('*',s.instance))`

// The selected target and asset must still have a verified inventory relationship at collection time.
const inspectCatalog = `(SELECT asset_id,cluster,instance FROM inventory_machines WHERE cluster<>'' UNION SELECT asset_id,cluster,instance FROM cluster_inventory_sources UNION SELECT d.asset_id,d.cluster,d.instance FROM inventory_business_deployments d JOIN inventory_business_systems b ON b.asset_id=d.asset_id WHERE b.active=1)`
const inspectExecAllowed = `EXISTS(SELECT 1 FROM users u WHERE u.id=p.executor AND u.disabled=0 AND (u.is_admin=1 OR EXISTS(SELECT 1 FROM grants g WHERE g.user_id=u.id AND g.cluster IN ('*',s.cluster) AND g.instance IN ('*',s.instance))))`
const inspectBinding = `EXISTS(SELECT 1 FROM ` + inspectCatalog + ` c WHERE c.asset_id=s.asset_id AND c.cluster=s.cluster AND c.instance=s.instance)`
const inspectOnline = `EXISTS(SELECT 1 FROM agent_status a WHERE a.cluster=s.cluster AND a.instance=s.instance AND a.status='online' AND a.archived_at='')`
const inspectBlock = `CASE WHEN NOT ` + inspectExecAllowed + ` THEN 'executor_grant_revoked' WHEN NOT ` + inspectBinding + ` THEN 'asset_binding_changed' WHEN NOT ` + inspectOnline + ` THEN 'target_offline' ELSE '' END`

func (h *GatewayHub) HandleAssetInspection(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	auth, err := h.authenticateUser(r)
	if err != nil {
		h.writeUserAuthError(w, r, err)
		return
	}
	var result any
	switch r.Method {
	case "GET":
		result, err = h.store.inspectionRead(r, auth)
	case "POST":
		var req inspectionRequest
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 131072))
		d.DisallowUnknownFields()
		if d.Decode(&req) != nil || d.Decode(new(any)) != io.EOF {
			err = inspectErr(400, "invalid inspection JSON")
		} else {
			result, err = h.store.inspectionWrite(r.Context(), auth, req)
		}
	default:
		w.Header().Set("Allow", "GET, POST")
		err = inspectErr(405, "method not allowed")
	}
	if err != nil {
		p := machineInventoryProblem{503, "inspection_unavailable", "inspection store unavailable"}
		var known machineInventoryProblem
		if errors.As(err, &known) {
			p = known
		}
		writeDoopsError(w, p.status, "", doopsErrorResponse{Error: p.code, Code: p.code, Message: p.message})
		return
	}
	if r.Method == "GET" && r.URL.Query().Get("view") == "download" {
		w.Header().Set("Content-Disposition", `attachment; filename="inspection-`+r.URL.Query().Get("run_id")+`.json"`)
	}
	writeJSONHTTP(w, result)
}
func (s *GatewayStore) inspectionWrite(ctx context.Context, auth TokenAuth, req inspectionRequest) (any, error) {
	switch req.Action {
	case "preview", "create", "pause", "resume", "reconcile", "tick", "run", "makeup", "gap", "repair", "recheck", "evidence":
	default:
		return nil, inspectErr(400, "unknown inspection action")
	}
	id, err := s.StartAudit(AuditEvent{UserID: auth.UserID, TokenID: auth.TokenID, Session: "inspection-" + randomHex(8), Action: GatewayAction("inspection:" + req.Action)})
	if err != nil {
		return nil, err
	}
	apply := func() (any, error) {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return nil, err
		}
		defer tx.Rollback()
		if _, err = tx.Exec(`UPDATE audit_events SET status='executing' WHERE id=?`, id); err != nil {
			return nil, err
		}
		now := time.Now().UTC().Truncate(time.Second)
		var out any
		var finish AuditFinish
		if req.Action != "tick" && req.Action != "evidence" && req.Action != "repair" && req.Action != "recheck" {
			if err = requireInventoryAdmin(tx, auth); err != nil {
				return nil, err
			}
		}
		switch req.Action {
		case "create", "preview":
			out, err = inspectionPlan(tx, req, now)
		case "reconcile", "run", "tick", "pause", "resume":
			out, err = inspectionSchedule(tx, auth, req, now)
		default:
			out, finish, err = inspectionItemAction(tx, auth, req, now)
		}
		if err != nil {
			return nil, err
		}
		if finish.CommandSummary == "" {
			finish.CommandSummary = "plan=" + strconv.FormatInt(req.Plan, 10)
			if req.Action == "reconcile" {
				finish.CommandSummary += " due=" + strconv.FormatInt(req.Due, 10)
			}
		}
		if err = finishAuditWith(tx, id, finish); err != nil {
			return nil, err
		}
		return out, tx.Commit()
	}
	out, err := apply()
	if err != nil {
		status := "error"
		var p machineInventoryProblem
		if errors.As(err, &p) && p.status == 403 {
			status = "forbidden"
		}
		if e := s.FinishAudit(id, AuditFinish{Status: status, Error: "inspection request rejected"}); e != nil {
			return nil, e
		}
	}
	return out, err
}
