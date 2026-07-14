package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type adminUserCreateRequest struct {
	Name     string `json:"name"`
	Password string `json:"password"`
	Admin    bool   `json:"admin"`
}

type adminUserPasswordRequest struct {
	UserID   string `json:"user_id"`
	User     string `json:"user"`
	Password string `json:"password"`
}

type adminUserDisableRequest struct {
	UserID   string `json:"user_id"`
	Disabled bool   `json:"disabled"`
}

type adminGrantRequest struct {
	UserID   string   `json:"user_id"`
	User     string   `json:"user"`
	Cluster  string   `json:"cluster"`
	Instance string   `json:"instance"`
	Actions  []string `json:"actions"`
}

type adminRepoCloneRequest struct {
	Cluster   string `json:"cluster"`
	Instance  string `json:"instance"`
	SessionID string `json:"session_id"`
	Directory string `json:"directory"`
}

// HandleAdminUsers 处理 GET(列出) 与 POST(创建) 用户。
func (h *GatewayHub) HandleAdminUsers(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		users, err := h.store.ListUsers()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSONHTTP(w, map[string]interface{}{"users": users})
	case http.MethodPost:
		var req adminUserCreateRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(req.Name) == "" {
			http.Error(w, "name is required", http.StatusBadRequest)
			return
		}
		user, err := h.store.CreateUserWithPassword(CreateUserRequest{Name: req.Name, Password: req.Password})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// 新用户默认授予标准操作权限；如指定 admin 则追加 admin。
		grant := ScopeGrant{Cluster: "*", Instance: "*"}
		if req.Admin {
			grant.Actions = []GatewayAction{ActionAdmin}
		}
		if err := h.store.GrantUser(user.ID, grant); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSONHTTP(w, map[string]interface{}{
			"id":       user.ID,
			"name":     user.Name,
			"is_admin": req.Admin,
		})
	default:
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

// HandleAdminUserPassword 处理设置用户密码。
func (h *GatewayHub) HandleAdminUserPassword(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	var req adminUserPasswordRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	userID, err := h.resolveUserID(req.UserID, req.User)
	if err != nil {
		http.Error(w, "user not found", http.StatusNotFound)
		return
	}
	if err := h.store.SetUserPassword(userID, req.Password); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSONHTTP(w, map[string]interface{}{"user_id": userID, "updated": true})
}

// HandleAdminUserDisable 启用/停用用户。
func (h *GatewayHub) HandleAdminUserDisable(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	var req adminUserDisableRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if err := h.store.SetUserDisabled(req.UserID, req.Disabled); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "user not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSONHTTP(w, map[string]interface{}{"user_id": req.UserID, "disabled": req.Disabled})
}

// HandleAdminGrants 处理 GET(列出)、POST(新增)、DELETE(删除) 授权。
func (h *GatewayHub) HandleAdminGrants(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		grants, err := h.store.ListGrants(strings.TrimSpace(r.URL.Query().Get("user")))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSONHTTP(w, map[string]interface{}{"grants": grants})
	case http.MethodPost:
		var req adminGrantRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		userID, err := h.resolveUserID(req.UserID, req.User)
		if err != nil {
			http.Error(w, "user not found", http.StatusNotFound)
			return
		}
		actions, err := parseActions(req.Actions)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		grant := ScopeGrant{Cluster: req.Cluster, Instance: req.Instance, Actions: actions}
		if err := h.store.GrantUser(userID, grant); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSONHTTP(w, map[string]interface{}{"user_id": userID, "granted": true})
	case http.MethodDelete:
		id, err := strconv.ParseInt(strings.TrimSpace(r.URL.Query().Get("id")), 10, 64)
		if err != nil {
			http.Error(w, "invalid id", http.StatusBadRequest)
			return
		}
		if err := h.store.DeleteGrant(id); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				http.Error(w, "grant not found", http.StatusNotFound)
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

// HandleAdminInstances 列出全部已知实例（在线/离线），合并实时连接状态。
func (h *GatewayHub) HandleAdminInstances(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	statuses, err := h.store.ListAgentStatus()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// 合并当前在线连接的繁忙信息。
	live := make(map[string]GatewayTarget)
	for _, t := range h.ListTargets() {
		live[tunnelKey(t.Cluster, t.Instance)] = t
	}
	type instanceInfo struct {
		Cluster     string    `json:"cluster"`
		Instance    string    `json:"instance"`
		Status      string    `json:"status"`
		Remote      string    `json:"remote,omitempty"`
		Busy        bool      `json:"busy"`
		ActiveOps   int       `json:"active_ops"`
		QueuedOps   int       `json:"queued_ops"`
		ConnectedAt time.Time `json:"connected_at,omitempty"`
		LastSeen    time.Time `json:"last_seen,omitempty"`
	}
	out := make([]instanceInfo, 0, len(statuses))
	for _, st := range statuses {
		info := instanceInfo{
			Cluster:     st.Cluster,
			Instance:    st.Instance,
			Status:      st.Status,
			Remote:      st.Remote,
			ConnectedAt: st.ConnectedAt,
			LastSeen:    st.LastSeen,
		}
		if t, ok := live[tunnelKey(st.Cluster, st.Instance)]; ok {
			info.Status = "online"
			info.Busy = t.Busy
			info.ActiveOps = t.ActiveOps
			info.QueuedOps = t.QueuedOps
		}
		out = append(out, info)
	}
	writeJSONHTTP(w, map[string]interface{}{"instances": out})
}

// HandleAdminRepos 管理控制台可选择的代码仓库配置。
func (h *GatewayHub) HandleAdminRepos(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		repos, err := h.store.ListGitRepos()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if repos == nil {
			repos = []GitRepo{}
		}
		writeJSONHTTP(w, map[string]interface{}{"repos": repos})
	case http.MethodPost:
		var req GitRepoInput
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			http.Error(w, "invalid repo request", http.StatusBadRequest)
			return
		}
		repo, err := h.store.CreateGitRepo(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSONHTTP(w, repo)
	case http.MethodPatch, http.MethodPut:
		id := strings.TrimSpace(r.URL.Query().Get("id"))
		if id == "" {
			http.Error(w, "missing id", http.StatusBadRequest)
			return
		}
		var req GitRepoInput
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			http.Error(w, "invalid repo request", http.StatusBadRequest)
			return
		}
		repo, err := h.store.UpdateGitRepo(id, req)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				http.Error(w, "repo not found", http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSONHTTP(w, repo)
	case http.MethodDelete:
		id := strings.TrimSpace(r.URL.Query().Get("id"))
		if id == "" {
			http.Error(w, "missing id", http.StatusBadRequest)
			return
		}
		if err := h.store.DeleteGitRepo(id); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				http.Error(w, "repo not found", http.StatusNotFound)
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

// HandleAdminRepoTest validates that the saved repository record can reach the
// configured remote branch from the gateway process. Target-side deploys still
// run through doops RPC/agent tools; this endpoint does not open SSH sessions.
func (h *GatewayHub) HandleAdminRepoTest(w http.ResponseWriter, r *http.Request) {
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
	repo, err := h.store.GetGitRepo(id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "repo not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	result, err := h.store.TestGitRepoConnection(r.Context(), repo)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	repo, err = h.store.MarkGitRepoUsed(id, time.Now().UTC())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSONHTTP(w, map[string]interface{}{
		"ok":      true,
		"message": fmt.Sprintf("仓库连接成功：%s (%s @ %s)", repo.Name, result.Branch, shortGitRef(result.Ref)),
	})
}

func (h *GatewayHub) HandleAdminRepoClone(w http.ResponseWriter, r *http.Request) {
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
	var req adminRepoCloneRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "invalid clone request", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Cluster) == "" || strings.TrimSpace(req.Instance) == "" || strings.TrimSpace(req.SessionID) == "" {
		http.Error(w, "missing cluster / instance / session_id", http.StatusBadRequest)
		return
	}
	repo, err := h.store.GetGitRepo(id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "repo not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	password, err := h.store.GitRepoPassword(repo.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	directory := strings.TrimSpace(req.Directory)
	if directory == "" {
		directory = "repo"
	}
	out, err := h.RunInternalToolCall(r.Context(), req.Cluster, req.Instance, "doops_git_clone", map[string]interface{}{
		"session_id": req.SessionID,
		"url":        repo.URL,
		"branch":     repo.Branch,
		"username":   repo.Username,
		"password":   password,
		"directory":  directory,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	_, _ = h.store.MarkGitRepoUsed(id, time.Now().UTC())
	writeJSONHTTP(w, map[string]interface{}{
		"ok":      true,
		"message": out,
	})
}

func shortGitRef(ref string) string {
	ref = strings.TrimSpace(ref)
	if len(ref) > 12 {
		return ref[:12]
	}
	return ref
}

// resolveUserID 优先用 user_id，否则按用户名查找。
func (h *GatewayHub) resolveUserID(userID, userName string) (string, error) {
	if id := strings.TrimSpace(userID); id != "" {
		if _, err := h.store.FindUserByID(id); err != nil {
			return "", err
		}
		return id, nil
	}
	user, err := h.store.FindUserByName(userName)
	if err != nil {
		return "", err
	}
	return user.ID, nil
}

var knownActions = map[string]GatewayAction{
	"*":             ActionAll,
	"admin":         ActionAdmin,
	"exec":          ActionExec,
	"ask":           ActionAsk,
	"read":          ActionRead,
	"write":         ActionWrite,
	"push":          ActionPush,
	"pull":          ActionPull,
	"info":          ActionInfo,
	"check":         ActionCheck,
	"clean":         ActionClean,
	"agent:upgrade": ActionAgentUpgrade,
	"targets:list":  ActionTargetsList,
}

func parseActions(raw []string) ([]GatewayAction, error) {
	if len(raw) == 0 {
		return nil, nil // store 会填充默认动作集合
	}
	actions := make([]GatewayAction, 0, len(raw))
	for _, a := range raw {
		key := strings.TrimSpace(strings.ToLower(a))
		act, ok := knownActions[key]
		if !ok {
			return nil, errors.New("unknown action: " + a)
		}
		actions = append(actions, act)
	}
	return actions, nil
}
