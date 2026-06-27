package server

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const gitTunnelChunkBytes = 256 << 10

type gitHTTPRequestParams struct {
	Method        string              `json:"method"`
	Path          string              `json:"path"`
	RawQuery      string              `json:"raw_query"`
	Header        map[string][]string `json:"header,omitempty"`
	ContentLength int64               `json:"content_length"`
}

func (h *GatewayHub) HandleGitHTTP(w http.ResponseWriter, r *http.Request) {
	cluster, instance, session, rest, ok := parseGatewayGitPath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	auth, err := h.authenticateGitUser(r)
	if err != nil {
		writeGitAuthError(w)
		return
	}
	action := gitActionForRequest(r)
	if action == "" {
		http.Error(w, "unsupported git service", http.StatusBadRequest)
		return
	}
	if !h.store.UserCan(auth.UserID, cluster, instance, action) {
		http.Error(w, fmt.Sprintf("forbidden: %s on %s/%s", action, cluster, instance), http.StatusForbidden)
		return
	}
	agent := h.waitForAgent(r.Context(), cluster, instance)
	if agent == nil {
		http.Error(w, fmt.Sprintf("target offline: %s/%s", cluster, instance), http.StatusBadGateway)
		return
	}

	releaseLimit, err := h.acquireOperationSlot(auth.UserID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusTooManyRequests)
		return
	}
	defer releaseLimit()
	opCtx, cancelOp := context.WithCancel(r.Context())
	defer cancelOp()
	opID := h.registerActiveOperation(GatewayActiveOperation{
		UserID:         auth.UserID,
		TokenID:        auth.TokenID,
		Cluster:        cluster,
		Instance:       instance,
		Action:         action,
		Session:        session,
		CommandSummary: r.Method + " " + r.URL.RequestURI(),
		Kind:           "git",
	}, cancelOp)
	defer h.finishActiveOperation(opID)

	resourceKey := "workspace:" + session
	if err := agent.acquireForAction(opCtx, action, resourceKey, h.opts.MaxQueuedPerTarget, h.opts.TargetQueueTimeout); err != nil {
		if errors.Is(err, context.Canceled) {
			http.Error(w, "operation canceled", http.StatusConflict)
			return
		}
		http.Error(w, fmt.Sprintf("%v: %s/%s", err, cluster, instance), http.StatusConflict)
		return
	}
	defer agent.releaseForAction(action, resourceKey)

	auditID, _ := h.store.StartAudit(AuditEvent{
		UserID:         auth.UserID,
		TokenID:        auth.TokenID,
		Cluster:        cluster,
		Instance:       instance,
		Action:         action,
		Session:        session,
		CommandSummary: r.Method + " " + r.URL.RequestURI(),
		StartedAt:      time.Now().UTC(),
	})
	status := "success"
	errMsg := ""
	bytesOut, relayErr := agent.relayGitHTTPRequest(opCtx, w, r, gitHTTPRequestParams{
		Method:        r.Method,
		Path:          "/git/" + session + ".git" + rest,
		RawQuery:      r.URL.RawQuery,
		Header:        cloneHTTPHeader(r.Header),
		ContentLength: r.ContentLength,
	}, h.opts.OperationTimeout)
	if relayErr != nil {
		status = "error"
		errMsg = relayErr.Error()
		if errors.Is(relayErr, context.Canceled) {
			status = "canceled"
			errMsg = "operation canceled"
		}
		if bytesOut == 0 {
			http.Error(w, relayErr.Error(), http.StatusBadGateway)
		}
	}
	_ = h.store.FinishAudit(auditID, AuditFinish{
		Status:   status,
		Error:    errMsg,
		BytesIn:  r.ContentLength,
		BytesOut: bytesOut,
		EndedAt:  time.Now().UTC(),
	})
}

func (h *GatewayHub) authenticateGitUser(r *http.Request) (TokenAuth, error) {
	if _, password, ok := r.BasicAuth(); ok && strings.TrimSpace(password) != "" {
		return h.store.VerifyUserToken(password)
	}
	return h.authenticateUser(r)
}

func writeGitAuthError(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="doops-gateway"`)
	http.Error(w, "Unauthorized", http.StatusUnauthorized)
}

func parseGatewayGitPath(rawPath string) (cluster, instance, session, rest string, ok bool) {
	trimmed := strings.TrimPrefix(rawPath, "/v1/git/")
	if trimmed == rawPath {
		return "", "", "", "", false
	}
	parts := strings.SplitN(trimmed, "/", 4)
	if len(parts) < 3 {
		return "", "", "", "", false
	}
	cluster = strings.TrimSpace(parts[0])
	instance = strings.TrimSpace(parts[1])
	repo := strings.TrimSpace(parts[2])
	if cluster == "" || instance == "" || !strings.HasSuffix(repo, ".git") {
		return "", "", "", "", false
	}
	session = strings.TrimSuffix(repo, ".git")
	if session == "" || strings.Contains(session, "/") || strings.Contains(session, "..") {
		return "", "", "", "", false
	}
	if len(parts) == 4 {
		rest = "/" + parts[3]
	}
	if rest != "" {
		rest = path.Clean(rest)
		if !strings.HasPrefix(rest, "/") {
			rest = "/" + rest
		}
	}
	return cluster, instance, session, rest, true
}

func gitActionForRequest(r *http.Request) GatewayAction {
	service := strings.TrimSpace(r.URL.Query().Get("service"))
	if service == "" {
		switch {
		case strings.HasSuffix(r.URL.Path, "/git-receive-pack"):
			service = "git-receive-pack"
		case strings.HasSuffix(r.URL.Path, "/git-upload-pack"):
			service = "git-upload-pack"
		}
	}
	switch service {
	case "git-receive-pack":
		return ActionPush
	case "git-upload-pack":
		return ActionPull
	default:
		return ""
	}
}

func (a *GatewayAgent) relayGitHTTPRequest(ctx context.Context, w http.ResponseWriter, r *http.Request, params gitHTTPRequestParams, timeout time.Duration) (int64, error) {
	id := atomicAddGatewayRequestID(&a.reqID)
	ch := a.registerPending(id)
	defer a.unregisterPending(id)

	if err := a.writeJSON(map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "git/http",
		"id":      id,
		"params":  params,
	}); err != nil {
		return 0, err
	}

	bodyErrCh := make(chan error, 1)
	go func() {
		bodyErrCh <- a.forwardGitRequestBody(id, r.Body)
	}()

	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	var wroteHeader bool
	var bytesOut int64
	for {
		select {
		case <-ctx.Done():
			return bytesOut, ctx.Err()
		case bodyErr := <-bodyErrCh:
			bodyErrCh = nil
			if bodyErr != nil {
				return bytesOut, bodyErr
			}
		case msg := <-ch:
			if msg.Parsed == nil {
				continue
			}
			if method, _ := msg.Parsed["method"].(string); method != "" {
				params, _ := msg.Parsed["params"].(map[string]interface{})
				switch method {
				case "git/response":
					status := intFromJSON(params["status"], http.StatusOK)
					headers := headersFromJSON(params["headers"])
					for key, values := range headers {
						for _, value := range values {
							w.Header().Add(key, value)
						}
					}
					w.WriteHeader(status)
					wroteHeader = true
				case "git/body":
					dataB64, _ := params["data_b64"].(string)
					if dataB64 == "" {
						continue
					}
					data, err := base64.StdEncoding.DecodeString(dataB64)
					if err != nil {
						return bytesOut, err
					}
					if !wroteHeader {
						w.WriteHeader(http.StatusOK)
						wroteHeader = true
					}
					if len(data) > 0 {
						n, err := w.Write(data)
						bytesOut += int64(n)
						if flusher, ok := w.(http.Flusher); ok {
							flusher.Flush()
						}
						if err != nil {
							return bytesOut, err
						}
					}
				}
				continue
			}
			if errObj, ok := msg.Parsed["error"]; ok && errObj != nil {
				return bytesOut, fmt.Errorf("%v", errObj)
			}
			if _, ok := msg.Parsed["result"]; ok {
				return bytesOut, nil
			}
		case <-a.closed:
			return bytesOut, fmt.Errorf("agent disconnected")
		case <-timer.C:
			return bytesOut, fmt.Errorf("git operation timed out")
		}
	}
}

func (a *GatewayAgent) forwardGitRequestBody(id int64, body io.ReadCloser) error {
	defer body.Close()
	buf := make([]byte, gitTunnelChunkBytes)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			if writeErr := a.writeJSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"method":  "git/body",
				"params": map[string]interface{}{
					"id":       id,
					"data_b64": base64.StdEncoding.EncodeToString(buf[:n]),
				},
			}); writeErr != nil {
				return writeErr
			}
		}
		if err == io.EOF {
			return a.writeJSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"method":  "git/body",
				"params":  map[string]interface{}{"id": id, "eof": true},
			})
		}
		if err != nil {
			return err
		}
	}
}

func cloneHTTPHeader(in http.Header) map[string][]string {
	out := make(map[string][]string, len(in))
	for key, values := range in {
		if strings.EqualFold(key, "Authorization") {
			continue
		}
		out[key] = append([]string(nil), values...)
	}
	return out
}

func headersFromJSON(v interface{}) http.Header {
	out := http.Header{}
	switch typed := v.(type) {
	case map[string]interface{}:
		for key, rawValues := range typed {
			switch values := rawValues.(type) {
			case []interface{}:
				for _, value := range values {
					out.Add(key, fmt.Sprint(value))
				}
			case []string:
				for _, value := range values {
					out.Add(key, value)
				}
			case string:
				out.Add(key, values)
			}
		}
	case map[string][]string:
		for key, values := range typed {
			for _, value := range values {
				out.Add(key, value)
			}
		}
	}
	return out
}

func intFromJSON(v interface{}, fallback int) int {
	switch typed := v.(type) {
	case float64:
		return int(typed)
	case int:
		return typed
	case string:
		if parsed, err := strconv.Atoi(typed); err == nil {
			return parsed
		}
	}
	return fallback
}

func atomicAddGatewayRequestID(id *int64) int64 {
	return atomic.AddInt64(id, 1)
}
