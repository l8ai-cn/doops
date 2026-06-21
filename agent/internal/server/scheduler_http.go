package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type createJobRequest struct {
	Name           string          `json:"name"`
	ClusterGlob    string          `json:"cluster_glob"`
	InstanceGlob   string          `json:"instance_glob"`
	IntervalSec    int64           `json:"interval_sec"`
	ScanMode       string          `json:"scan_mode"`
	ScanConfig     json.RawMessage `json:"scan_config,omitempty"`
	Platform       string          `json:"platform"`
	RepoSlug       string          `json:"repo_slug"`
	Labels         string          `json:"labels"`
	TokenEnv       string          `json:"token_env"`
	APIBase        string          `json:"api_base"`
	DedupWindowSec int64           `json:"dedup_window_sec"`
	Enabled        *bool           `json:"enabled,omitempty"`
}

// HandleAdminJobs 管理定时巡检任务：GET 列表 / POST 创建 / DELETE 删除。
func (h *GatewayHub) HandleAdminJobs(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		jobs, err := h.store.ListSchedulerJobs()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if jobs == nil {
			jobs = []SchedulerJob{}
		}
		writeJSONHTTP(w, map[string]interface{}{"jobs": jobs})
	case http.MethodPost:
		var req createJobRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			http.Error(w, "invalid job request", http.StatusBadRequest)
			return
		}
		enabled := true
		if req.Enabled != nil {
			enabled = *req.Enabled
		}
		scanConfig := strings.TrimSpace(string(req.ScanConfig))
		if scanConfig == "" || scanConfig == "null" {
			scanConfig = "{}"
		}
		job, err := h.store.CreateSchedulerJob(SchedulerJob{
			Name:           req.Name,
			ClusterGlob:    req.ClusterGlob,
			InstanceGlob:   req.InstanceGlob,
			IntervalSec:    req.IntervalSec,
			ScanMode:       req.ScanMode,
			ScanConfig:     scanConfig,
			Platform:       req.Platform,
			RepoSlug:       req.RepoSlug,
			Labels:         req.Labels,
			TokenEnv:       req.TokenEnv,
			APIBase:        req.APIBase,
			DedupWindowSec: req.DedupWindowSec,
			Enabled:        enabled,
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSONHTTP(w, job)
	case http.MethodDelete:
		id := strings.TrimSpace(r.URL.Query().Get("id"))
		if id == "" {
			http.Error(w, "missing id", http.StatusBadRequest)
			return
		}
		if err := h.store.DeleteSchedulerJob(id); err != nil {
			if err == sql.ErrNoRows {
				http.Error(w, "job not found", http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSONHTTP(w, map[string]interface{}{"id": id, "deleted": true})
	default:
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

// HandleAdminJobRun 立即执行一个任务，或开关启用状态。
//
//	POST /v1/admin/jobs/run?id=<id>                -> 立即执行
//	POST /v1/admin/jobs/run?id=<id>&enabled=true   -> 启用/停用
func (h *GatewayHub) HandleAdminJobRun(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	if enabledRaw := strings.TrimSpace(r.URL.Query().Get("enabled")); enabledRaw != "" {
		enabled, err := strconv.ParseBool(enabledRaw)
		if err != nil {
			http.Error(w, "invalid enabled value", http.StatusBadRequest)
			return
		}
		if err := h.store.SetSchedulerJobEnabled(id, enabled); err != nil {
			if err == sql.ErrNoRows {
				http.Error(w, "job not found", http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSONHTTP(w, map[string]interface{}{"id": id, "enabled": enabled})
		return
	}
	if h.scheduler == nil {
		http.Error(w, "scheduler not running", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	summary, err := h.scheduler.RunJobNow(ctx, id)
	if err != nil {
		if err == sql.ErrNoRows {
			http.Error(w, "job not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSONHTTP(w, map[string]interface{}{"id": id, "summary": summary})
}

// HandleAdminJobIssues 列出某任务（或全部）已提交的 issue 去重记录。
func (h *GatewayHub) HandleAdminJobIssues(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	jobID := strings.TrimSpace(r.URL.Query().Get("id"))
	limit, _ := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("limit")))
	issues, err := h.store.ListSchedulerIssues(jobID, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if issues == nil {
		issues = []SchedulerIssue{}
	}
	writeJSONHTTP(w, map[string]interface{}{"issues": issues})
}

func (h *GatewayHub) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	auth, err := h.authenticateUser(r)
	if err != nil {
		h.writeUserAuthError(w, r)
		return false
	}
	if !h.store.UserHasAction(auth.UserID, ActionAdmin) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return false
	}
	return true
}
