package server

import (
	"bytes"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

type coverageVersion struct {
	ID       string `json:"id"`
	FrozenAt string `json:"frozen_at"`
}
type coverageMetric struct {
	Total     int      `json:"total"`
	Covered   int      `json:"covered"`
	Shortfall int      `json:"shortfall"`
	Percent   *float64 `json:"percent"`
	Passed    *bool    `json:"passed"`
}
type coverageLine struct {
	coverageAsset
	Covered bool `json:"covered"`
}
type coverageSummary struct {
	ClusterID string                     `json:"cluster_id,omitempty"`
	Scope     string                     `json:"scope"`
	State     string                     `json:"state"`
	Baseline  *coverageVersion           `json:"baseline"`
	Metrics   map[string]*coverageMetric `json:"metrics"`
	Accepted  *bool                      `json:"accepted"`
}
type coverageMachineState struct {
	AssetID  string `json:"asset_id"`
	Name     string `json:"name"`
	Active   bool   `json:"active"`
	Revision int64  `json:"revision"`
}
type coverageReport struct {
	coverageSummary
	Register         *coverageRegister      `json:"register,omitempty"`
	RegisterRevision int64                  `json:"register_revision,omitempty"`
	Document         string                 `json:"document_ref,omitempty"`
	Lifecycle        []coverageMachineState `json:"machine_lifecycle,omitempty"`
	Baselines        []coverageVersion      `json:"baselines"`
	Rows             []coverageLine         `json:"rows"`
}
type coverageRequest struct {
	ClusterID        *string           `json:"cluster_id,omitempty"`
	Register         *coverageRegister `json:"register,omitempty"`
	MachineID        string            `json:"machine_id"`
	Active           *bool             `json:"active"`
	ExpectedRevision *int64            `json:"expected_revision"`
	Action           string            `json:"action"`
	BaselineID       string            `json:"baseline_id"`
	VerifiedHosts    []coverageHost    `json:"verified_hosts"`
}

func coverageQuery(r *http.Request) (url.Values, error) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	bad := machineBadRequest("invalid_coverage_query", "不支持的对账参数、取值或重复参数")
	if err != nil || r.Method == http.MethodPost && len(q) > 0 {
		return nil, bad
	}
	for k, values := range q {
		if len(values) != 1 {
			return nil, bad
		}
		v := values[0]
		switch k {
		case "summary":
			if len(q) > 1 && !(len(q) == 2 && q.Has("cluster_id")) || v != "1" {
				return nil, bad
			}
		case "cluster_id":
			if !inventoryKeyPattern.MatchString(v) || !inventoryMetadata(v) {
				return nil, bad
			}
		case "baseline_id":
			if !safeMachineID(v) {
				return nil, bad
			}
		case "kind":
			if !slices.Contains([]string{"all", "machines", "systems"}, v) {
				return nil, bad
			}
		case "status":
			if !slices.Contains([]string{"all", "gap", "covered"}, v) {
				return nil, bad
			}
		case "format":
			if v != "csv" && v != "json" {
				return nil, bad
			}
		default:
			return nil, bad
		}
	}
	return q, nil
}
func (h *GatewayHub) HandleAssetCoverage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	auth, err := h.authenticateUser(r)
	if err != nil {
		h.writeUserAuthError(w, r, err)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		writeDoopsError(w, 405, "", doopsErrorResponse{Code: "method_not_allowed", Error: "method_not_allowed", Message: "仅支持 GET 和 POST"})
		return
	}
	q, parseErr := coverageQuery(r)
	var req coverageRequest
	if r.Method == http.MethodPost && parseErr == nil {
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, inventoryJSONBytes))
		dec.DisallowUnknownFields()
		if dec.Decode(&req) != nil || dec.Decode(new(any)) != io.EOF {
			parseErr = machineBadRequest("invalid_coverage_request", "盘点请求格式无效")
		} else {
			switch req.Action {
			case "freeze":
				if req.Register != nil || !safeMachineID(req.BaselineID) || req.BaselineID == "live" || req.MachineID != "" || req.Active != nil || req.ClusterID == nil && req.ExpectedRevision != nil {
					parseErr = machineBadRequest("invalid_coverage_freeze", "冻结请求需要唯一基线编号")
				}
			case "save_register":
				if req.Register == nil || req.MachineID != "" || req.BaselineID != "" || req.Active != nil || req.ExpectedRevision != nil || req.VerifiedHosts != nil {
					parseErr = machineBadRequest("invalid_register", "底册请求不完整")
				}
			case "set_machine_active":
				if req.ClusterID != nil || req.Register != nil || !safeMachineID(req.MachineID) || req.Active == nil || req.ExpectedRevision == nil || *req.ExpectedRevision < 1 || req.BaselineID != "" || req.VerifiedHosts != nil {
					parseErr = machineBadRequest("invalid_machine_lifecycle", "请提供机器编号、在役状态和当前版本")
				}
			default:
				parseErr = machineBadRequest("invalid_coverage_request", "未知盘点动作")
			}
		}
	}
	if req.ClusterID != nil && (!inventoryKeyPattern.MatchString(*req.ClusterID) || !inventoryMetadata(*req.ClusterID)) {
		parseErr = machineBadRequest("invalid_cluster_id", "集群编号无效")
	}
	s := h.store
	var audit int64
	var report coverageReport
	if r.Method == http.MethodPost || q.Get("format") == "csv" {
		action := "export"
		if r.Method == http.MethodPost {
			action = "request"
			if req.Action == "freeze" || req.Action == "set_machine_active" || req.Action == "save_register" {
				action = req.Action
			}
		}
		audit, err = s.StartAudit(AuditEvent{UserID: auth.UserID, TokenID: auth.TokenID, Session: "asset-coverage", Action: GatewayAction("inventory:coverage:" + action)})
	}
	if err == nil {
		report, err = s.coverageTransaction(r, auth, q, req, audit, parseErr)
	}
	if err != nil {
		problem := machineInventoryProblem{503, "coverage_unavailable", "覆盖率服务不可用，请检查数据库迁移和审计"}
		var known machineInventoryProblem
		if errors.As(err, &known) {
			problem = known
		}
		if audit != 0 {
			status := "error"
			if problem.status == 403 {
				status = "forbidden"
			}
			if s.FinishAudit(audit, AuditFinish{Status: status, Error: problem.code}) != nil {
				problem = machineInventoryProblem{503, "audit_unavailable", "审计不可用"}
			}
		}
		writeDoopsError(w, problem.status, "", doopsErrorResponse{Code: problem.code, Error: problem.code, Message: problem.message})
		return
	}
	if q.Get("format") == "csv" {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="asset-coverage.csv"`)
		_, _ = w.Write(coverageCSV(report))
		return
	}
	if q.Has("summary") {
		writeJSONHTTP(w, report.coverageSummary)
		return
	}
	writeJSONHTTP(w, report)
}
func (s *GatewayStore) coverageTransaction(r *http.Request, auth TokenAuth, q url.Values, req coverageRequest, audit int64, parseErr error) (coverageReport, error) {
	var report coverageReport
	ctx := r.Context()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: audit == 0})
	if err != nil {
		return report, err
	}
	defer tx.Rollback()
	if audit != 0 {
		if err := tx.QueryRow(`UPDATE audit_events SET status='executing' WHERE id=? AND status='running' RETURNING id`, audit).Scan(&audit); err != nil {
			return report, err
		}
	}
	if r.Method == http.MethodPost {
		if err := requireInventoryAdmin(tx, auth); err != nil {
			return report, err
		}
	}
	if parseErr != nil {
		return report, parseErr
	}
	id := q.Get("baseline_id")
	now := time.Now().UTC()
	scope := q.Get("cluster_id")
	if req.ClusterID != nil {
		scope = *req.ClusterID
	}
	access, err := currentCoverageAccess(ctx, tx, auth)
	if err != nil {
		return report, err
	}
	ready, err := registerSchema(tx)
	if err != nil {
		return report, err
	}
	if !ready {
		return report, machineInventoryProblem{503, "coverage_register_migration_required", "请先应用集群盘点底册迁移"}
	}
	sources, err := coverageClusterSources(tx)
	if err != nil {
		return report, err
	}
	if access.admin {
		access.clusters = sources
	}
	if scope != "" && !access.clusterVisible(scope) {
		return report, machineInventoryProblem{404, "cluster_not_visible", "集群不存在或当前无权访问"}
	}
	regs, err := coverageRegisters(tx)
	if err != nil {
		return report, err
	}
	if !access.admin {
		if err := access.indexRegisterAccess(tx, regs); err != nil {
			return report, err
		}
	}
	capture := func(hosts []coverageHost) ([]coverageAsset, error) {
		assets, err := captureCoverage(ctx, tx, auth, now, hosts)
		if err != nil {
			return nil, err
		}
		return mergeCoverageRegisters(tx, assets, regs, sources)
	}
	var lifecycleReceipt string
	if req.Action == "save_register" {
		if err := saveCoverageRegister(tx, scope, *req.Register, sources, auth.UserID, formatTime(now)); err != nil {
			return report, err
		}
		lifecycleReceipt = coverageRegisterReceipt(scope, *req.Register)
		reg := *req.Register
		reg.Revision++
		regs[scope] = reg
		id = "live"
	}
	if req.Action == "set_machine_active" {
		lifecycleReceipt, err = setCoverageMachineActive(tx, req)
		if err != nil {
			return report, err
		}
		id = "live"
	}
	if req.Action == "freeze" {
		assets, err := capture(req.VerifiedHosts)
		if err != nil {
			return report, err
		}
		if scope != "" {
			reg, ok := regs[scope]
			if !ok {
				return report, machineInventoryProblem{409, "register_required", "请先确认此集群的底册"}
			}
			if req.ExpectedRevision == nil || *req.ExpectedRevision < 1 {
				return report, machineBadRequest("invalid_coverage_freeze", "冻结集群需要提供已核对的底册版本")
			}
			if reg.Revision != *req.ExpectedRevision {
				return report, machineInventoryProblem{409, "register_changed", "底册已变更，请刷新后核对"}
			}
			var collision bool
			if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM asset_coverage_baselines WHERE id=?)`, req.BaselineID).Scan(&collision); err != nil {
				return report, err
			}
			if collision {
				return report, machineInventoryProblem{409, "baseline_exists", "此编号已用于平台基线"}
			}
			if err := freezeClusterCoverage(tx, scope, req.BaselineID, auth.UserID, formatTime(now), reg, assets); err != nil {
				return report, err
			}
		} else {
			var used bool
			if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM asset_coverage_cluster_versions WHERE id=?)`, req.BaselineID).Scan(&used); err != nil {
				return report, err
			}
			if used {
				return report, machineInventoryProblem{409, "baseline_exists", "编号已用于集群快照"}
			}
			payload, err := json.Marshal(assets)
			if err != nil {
				return report, err
			}
			result, err := tx.Exec(`INSERT INTO asset_coverage_baselines(id,frozen_at,frozen_by,snapshot) VALUES(?,?,?,?) ON CONFLICT(id) DO NOTHING`, req.BaselineID, formatTime(now), auth.UserID, string(payload))
			if err != nil {
				return report, err
			}
			n, err := result.RowsAffected()
			if err != nil {
				return report, err
			}
			if n != 1 {
				return report, machineInventoryProblem{409, "baseline_exists", "该基线编号已冻结，请读取既有快照"}
			}
			for cluster, reg := range regs {
				if cluster != "" {
					if err := freezeClusterCoverage(tx, cluster, req.BaselineID, auth.UserID, formatTime(now), reg, assets); err != nil {
						return report, err
					}
				}
			}
		}
		id = req.BaselineID
	}
	report = coverageReport{coverageSummary: coverageSummary{ClusterID: scope, Scope: "visible", State: "missing", Metrics: map[string]*coverageMetric{"machines": {}, "systems": {}}}, Baselines: []coverageVersion{}, Rows: []coverageLine{}}
	if access.admin {
		report.Scope = "platform"
	}
	versionsQuery := `SELECT id,frozen_at FROM asset_coverage_baselines ORDER BY rowid DESC`
	var params []any
	if scope != "" {
		versionsQuery = `SELECT id,frozen_at FROM asset_coverage_cluster_versions WHERE cluster_id=? UNION ALL SELECT b.id,b.frozen_at FROM asset_coverage_baselines b WHERE NOT EXISTS (SELECT 1 FROM asset_coverage_cluster_versions c WHERE c.cluster_id=? AND c.id=b.id) ORDER BY frozen_at DESC,id DESC`
		params = []any{scope, scope}
	}
	if q.Has("summary") {
		versionsQuery += " LIMIT 1"
	}
	versions, err := tx.Query(versionsQuery, params...)
	if err != nil {
		return report, err
	}
	for versions.Next() {
		var v coverageVersion
		if err := versions.Scan(&v.ID, &v.FrozenAt); err != nil {
			versions.Close()
			return report, err
		}
		report.Baselines = append(report.Baselines, v)
		if id == "" {
			id = v.ID
		}
		if v.ID == id {
			report.Baseline = &v
		}
	}
	err = versions.Err()
	versions.Close()
	if err != nil {
		return report, err
	}
	var assets []coverageAsset
	switch {
	case id == "live":
		report.State = "live"
		assets, err = capture(nil)
		reg, found := regs[scope]
		if access.admin && q.Get("format") != "csv" {
			if !found {
				reg.Entries = []coverageEntry{}
			}
			report.Register = &reg
		}
		if scope != "" {
			assets = projectCoverage(assets, scope)
			if !found {
				report.State = "register_missing"
			}
			report.RegisterRevision = reg.Revision
			if access.admin {
				report.Document = reg.Document
			}
		}
		if err == nil && scope == "" && access.admin && q.Get("format") != "csv" {
			report.Lifecycle, err = listCoverageMachineStates(tx)
		}
	case report.Baseline != nil:
		report.State = "frozen"
		if scope != "" {
			assets, report.Document, report.RegisterRevision, err = clusterCoveragePayload(tx, scope, id)
			if !access.admin {
				report.Document = ""
			}
			if errors.Is(err, sql.ErrNoRows) {
				report.State = "scope_unavailable"
				err = nil
			}
			break
		}
		var raw string
		err = tx.QueryRow(`SELECT snapshot FROM asset_coverage_baselines WHERE id=?`, id).Scan(&raw)
		if err == nil {
			err = json.Unmarshal([]byte(raw), &assets)
		}
	case id != "":
		return report, machineInventoryProblem{404, "baseline_not_found", "基线不存在"}
	}
	if err != nil {
		return report, err
	}
	if q.Get("format") == "csv" && (report.State == "scope_unavailable" || report.State == "register_missing") {
		return report, machineInventoryProblem{409, report.State, "此版本缺少已确认的集群底册范围"}
	}
	if report.State == "missing" && q.Get("format") == "csv" {
		return report, machineInventoryProblem{409, "baseline_required", "请先冻结盘点基线"}
	}
	report.calculate(assets, access, q)
	if audit != 0 {
		summary := lifecycleReceipt
		if summary == "" {
			summary = fmt.Sprintf("baseline_id=%s scope=%s kind=%s status=%s rows=%d", id, report.Scope, q.Get("kind"), q.Get("status"), len(report.Rows))
		}
		if scope != "" && req.Action != "save_register" {
			summary += " cluster_id=" + scope
		}
		if err := finishAuditWith(tx, audit, AuditFinish{Status: "success", CommandSummary: summary}); err != nil {
			return report, err
		}
	}
	return report, tx.Commit()
}
func (report *coverageReport) calculate(assets []coverageAsset, access coverageAccess, q url.Values) {
	kind, status := q.Get("kind"), q.Get("status")
	for _, asset := range assets {
		row, visible := access.visible(asset)
		if !visible || report.ClusterID != "" && len(row.Memberships) == 0 {
			continue
		}
		m := report.Metrics[row.Kind]
		m.Total++
		covered := slices.ContainsFunc(row.Evidence, func(e coverageEvidence) bool { return e.Verified })
		if covered {
			m.Covered++
		}
		if !q.Has("summary") && (kind == "" || kind == "all" || kind == row.Kind) && !(status == "gap" && covered || status == "covered" && !covered) {
			report.Rows = append(report.Rows, coverageLine{row, covered})
		}
	}
	passed := true
	for _, m := range report.Metrics {
		if m.Total == 0 {
			continue
		}
		m.Shortfall = max(0, (4*m.Total+4)/5-m.Covered)
		percent := math.Round(1000*float64(m.Covered)/float64(m.Total)) / 10
		ok := m.Shortfall == 0
		m.Percent = &percent
		m.Passed = &ok
		passed = passed && ok
	}
	if access.admin && report.State == "frozen" && report.Metrics["machines"].Total > 0 && report.Metrics["systems"].Total > 0 {
		report.Accepted = &passed
	}
}
func coverageCSV(report coverageReport) []byte {
	var buf bytes.Buffer
	buf.WriteString("\uFEFF")
	w := csv.NewWriter(&buf)
	w.UseCRLF = true
	header := []string{"scope", "baseline_id", "frozen_at", "kind", "asset_id", "name", "covered", "evidence"}
	if report.ClusterID != "" {
		header = append(header, "cluster_id", "document_ref", "register_revision")
	}
	header = append(header, "memberships")
	_ = w.Write(header)
	id, at := "live", ""
	if report.Baseline != nil {
		id, at = report.Baseline.ID, report.Baseline.FrozenAt
	}
	for _, row := range report.Rows {
		e, _ := json.Marshal(row.Evidence)
		fields := []string{report.Scope, id, at, row.Kind, row.AssetID, row.Name, fmt.Sprint(row.Covered), string(e)}
		if report.ClusterID != "" {
			doc := report.Document
			if len(row.Memberships) > 0 {
				doc = row.Memberships[0].Document
			}
			fields = append(fields, report.ClusterID, doc, fmt.Sprint(report.RegisterRevision))
		}
		memberships := row.Memberships
		if memberships == nil {
			memberships = []coverageMembership{}
		}
		data, _ := json.Marshal(memberships)
		fields = append(fields, string(data))
		for i, v := range fields {
			if strings.ContainsAny(firstCoverageRune(v), "=+@-") || strings.HasPrefix(v, "\t") || strings.HasPrefix(v, "\r") {
				fields[i] = "'" + v
			}
		}
		_ = w.Write(fields)
	}
	w.Flush()
	return buf.Bytes()
}
func firstCoverageRune(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return s[:1]
}
