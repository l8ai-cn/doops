package server

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/user/doops/agent/api"
)

const proxyTunnelChunkBytes = 256 << 10

type proxyHTTPRequestParams struct {
	Method        string              `json:"method"`
	Path          string              `json:"path"`
	RawQuery      string              `json:"raw_query"`
	Header        map[string][]string `json:"header,omitempty"`
	ContentLength int64               `json:"content_length"`
	Upstream      string              `json:"upstream"`
}

func proxyRequestIsReadOnly(method string) bool {
	switch strings.ToUpper(strings.TrimSpace(method)) {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	default:
		return false
	}
}

func proxyResourceKey(routeID, method string) string {
	if proxyRequestIsReadOnly(method) {
		return "query:proxy:" + routeID
	}
	return "proxy:" + routeID
}

func (h *GatewayHub) HandlePublicProxy(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/v1/") {
		http.NotFound(w, r)
		return
	}
	host, err := normalizeProxyHost(r.Host)
	if err != nil {
		http.Error(w, "proxy host is invalid", http.StatusBadRequest)
		return
	}
	route, err := h.store.ProxyRouteByHost(host)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "proxy route lookup failed", http.StatusInternalServerError)
		return
	}
	if !route.Enabled {
		http.NotFound(w, r)
		return
	}
	if strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade") ||
		strings.TrimSpace(r.Header.Get("Upgrade")) != "" {
		http.Error(w, "websocket proxy is not enabled", http.StatusUpgradeRequired)
		return
	}
	agent := h.registry.Wait(r.Context(), route.Cluster, route.Instance, h.opts.TargetReconnectGrace)
	if agent == nil {
		http.Error(w, fmt.Sprintf("target offline: %s/%s", route.Cluster, route.Instance), http.StatusBadGateway)
		return
	}
	opCtx, cancel := context.WithCancel(r.Context())
	defer cancel()
	resourceKey := proxyResourceKey(route.ID, r.Method)
	auditID, err := h.store.StartAudit(AuditEvent{
		UserID:  "proxy",
		Cluster: route.Cluster, Instance: route.Instance, Action: ActionProxy,
		CommandSummary: r.Method + " " + r.URL.RequestURI(), StartedAt: time.Now().UTC(),
	})
	if err != nil {
		http.Error(w, "start audit: "+err.Error(), http.StatusInternalServerError)
		return
	}
	opID, holderID := h.client.registerActiveOperationForAgent(GatewayActiveOperation{
		AuditID:        auditID,
		Resource:       resourceKey,
		UserID:         "proxy",
		Cluster:        route.Cluster,
		Instance:       route.Instance,
		Action:         ActionProxy,
		CommandSummary: r.Method + " " + r.URL.RequestURI(),
		Kind:           "proxy",
	}, cancel, agent)
	defer h.client.finishActiveOperation(opID)
	if holderID != "" {
		errMsg := "target busy: active operation " + holderID
		_ = h.client.finishExecutionAudit(auditID, AuditFinish{
			Status: "blocked", Error: errMsg, EndedAt: time.Now().UTC(),
		})
		http.Error(w, errMsg, http.StatusConflict)
		return
	}
	lease, err := agent.acquireActionLease(opCtx, ActionProxy, resourceKey, h.opts.MaxQueuedPerTarget, h.opts.TargetQueueTimeout)
	if err != nil {
		_ = h.client.finishExecutionAudit(auditID, AuditFinish{
			Status: "blocked", Error: err.Error(), EndedAt: time.Now().UTC(),
		})
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	h.client.attachOperationLease(opID, lease)
	h.client.markOperationRunning(opID)
	if !proxyRequestIsReadOnly(r.Method) {
		if err := h.client.beginExecution(agent, opID, auditID); err != nil {
			_ = h.client.finishExecutionAudit(auditID, AuditFinish{
				Status: "blocked", Error: err.Error(), EndedAt: time.Now().UTC(),
			})
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
	}
	response := &tunnelResponseWriter{ResponseWriter: w}
	bytesOut, relayErr := agent.relayProxyHTTPRequest(opCtx, response, r, proxyHTTPRequestParams{
		Method: r.Method, Path: r.URL.Path, RawQuery: r.URL.RawQuery,
		Header: cloneProxyHeader(r.Header), ContentLength: r.ContentLength, Upstream: route.Upstream,
	}, h.opts.OperationTimeout)
	status, errorText := "success", ""
	if relayErr != nil {
		status, errorText = "error", relayErr.Error()
		if errors.Is(relayErr, context.Canceled) {
			status, errorText = "canceled", "operation canceled"
		}
		if errors.Is(relayErr, errExecutionOutcomeUnknown) {
			status, errorText = "outcome_unknown", errExecutionOutcomeUnknown.Error()
		}
		if !response.committed {
			http.Error(w, relayErr.Error(), http.StatusBadGateway)
		}
	}
	_ = h.client.finishExecutionAudit(auditID, AuditFinish{
		Status: status, Error: errorText, BytesIn: r.ContentLength, BytesOut: bytesOut,
		EndedAt: time.Now().UTC(),
	})
	if relayErr != nil && response.committed {
		panic(http.ErrAbortHandler)
	}
}

// A failed HTTP stream can only change its status before the final header.
type tunnelResponseWriter struct {
	http.ResponseWriter
	committed bool
}

func (w *tunnelResponseWriter) WriteHeader(status int) {
	if status >= 200 || status == http.StatusSwitchingProtocols {
		w.committed = true
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *tunnelResponseWriter) Write(data []byte) (int, error) {
	w.committed = true
	return w.ResponseWriter.Write(data)
}

func (w *tunnelResponseWriter) Flush() {
	w.committed = true
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

func (a *GatewayAgent) relayProxyHTTPRequest(ctx context.Context, w http.ResponseWriter, r *http.Request, params proxyHTTPRequestParams, timeout time.Duration) (bytesOut int64, finalErr error) {
	id := atomicAddGatewayRequestID(&a.reqID)
	ch := a.registerPending(id, "proxy/cancel")
	defer a.unregisterPending(id)
	uncertainIfInterrupted := !proxyRequestIsReadOnly(params.Method)
	if err := a.writeJSON(map[string]interface{}{"jsonrpc": "2.0", "method": "proxy/http", "id": id, "params": params}); err != nil {
		if uncertainIfInterrupted {
			return 0, fmt.Errorf("%w: proxy dispatch failed: %v", errExecutionOutcomeUnknown, err)
		}
		return 0, err
	}
	bodyErrCh := make(chan error, 1)
	go func() { bodyErrCh <- a.forwardProxyRequestBody(id, r.Body) }()
	completed := false
	defer func() {
		if !completed {
			// A downstream write failure does not establish the upstream outcome.
			if err := a.waitPendingCancel(ch); err != nil && uncertainIfInterrupted {
				finalErr = err
			}
		}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var wroteHeader bool
	for {
		if msg, ok, queueErr := ch.dequeue(); queueErr != nil {
			if uncertainIfInterrupted {
				return bytesOut, errExecutionOutcomeUnknown
			}
			return bytesOut, queueErr
		} else if ok {
			method, _ := msg.Parsed["method"].(string)
			params, _ := msg.Parsed["params"].(map[string]interface{})
			switch method {
			case "proxy/response":
				for key, values := range headersFromJSON(params["headers"]) {
					for _, value := range values {
						w.Header().Add(key, value)
					}
				}
				w.WriteHeader(intFromJSON(params["status"], http.StatusOK))
				wroteHeader = true
			case "proxy/body":
				data, err := base64.StdEncoding.DecodeString(fmt.Sprint(params["data_b64"]))
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
			if _, ok := msg.Parsed["result"]; ok {
				completed = true
				return bytesOut, nil
			}
			if errObj, ok := msg.Parsed["error"]; ok && errObj != nil {
				completed = true
				if uncertainIfInterrupted && executionResponseUnknown(msg.Parsed) {
					return bytesOut, errExecutionOutcomeUnknown
				}
				return bytesOut, fmt.Errorf("%v", errObj)
			}
			continue
		}
		select {
		case err := <-bodyErrCh:
			bodyErrCh = nil
			if err != nil {
				if uncertainIfInterrupted {
					return bytesOut, errExecutionOutcomeUnknown
				}
				return bytesOut, err
			}
		case <-ctx.Done():
			if uncertainIfInterrupted {
				return bytesOut, errExecutionOutcomeUnknown
			}
			return bytesOut, ctx.Err()
		case <-a.closed:
			if uncertainIfInterrupted {
				return bytesOut, errExecutionOutcomeUnknown
			}
			return bytesOut, errGatewayAgentDisconnected
		case <-ch.notify:
		case <-timer.C:
			if uncertainIfInterrupted {
				return bytesOut, errExecutionOutcomeUnknown
			}
			return bytesOut, fmt.Errorf("proxy operation timed out")
		}
	}
}

func (a *GatewayAgent) forwardProxyRequestBody(id int64, body io.ReadCloser) error {
	defer body.Close()
	buf := make([]byte, proxyTunnelChunkBytes)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			if writeErr := a.writeJSON(map[string]interface{}{
				"jsonrpc": "2.0", "method": "proxy/body",
				"params": map[string]interface{}{"id": id, "data_b64": base64.StdEncoding.EncodeToString(buf[:n])},
			}); writeErr != nil {
				return writeErr
			}
		}
		if err == io.EOF {
			return a.writeJSON(map[string]interface{}{"jsonrpc": "2.0", "method": "proxy/body", "params": map[string]interface{}{"id": id, "eof": true}})
		}
		if err != nil {
			return err
		}
	}
}

func cloneProxyHeader(in http.Header) map[string][]string {
	out := make(map[string][]string, len(in))
	for key, values := range in {
		switch {
		case strings.EqualFold(key, "Authorization"),
			strings.EqualFold(key, "Connection"),
			strings.EqualFold(key, "Keep-Alive"),
			strings.EqualFold(key, "Proxy-Authenticate"),
			strings.EqualFold(key, "Proxy-Authorization"),
			strings.EqualFold(key, "TE"),
			strings.EqualFold(key, "Trailer"),
			strings.EqualFold(key, "Transfer-Encoding"),
			strings.EqualFold(key, "Upgrade"),
			strings.EqualFold(key, "Host"),
			strings.EqualFold(key, "Forwarded"),
			strings.EqualFold(key, "X-Forwarded-Host"):
			continue
		}
		out[key] = append([]string(nil), values...)
	}
	return out
}

func proxyUpstreamURL(raw, requestPath, rawQuery string) (*url.URL, error) {
	base, err := normalizeProxyUpstream(raw)
	if err != nil {
		return nil, err
	}
	parsed, _ := url.Parse(base)
	if requestPath == "" {
		requestPath = "/"
	}
	if !strings.HasPrefix(requestPath, "/") {
		return nil, fmt.Errorf("proxy path must start with /")
	}
	parsed.Path = path.Join(parsed.Path, requestPath)
	if strings.HasSuffix(requestPath, "/") {
		parsed.Path += "/"
	}
	parsed.RawQuery = rawQuery
	return parsed, nil
}

type wsProxyResponseWriter struct {
	id        interface{}
	writeJSON func(interface{})
	header    http.Header
	wrote     bool
	mu        sync.Mutex
}

func (w *wsProxyResponseWriter) Header() http.Header { return w.header }
func (w *wsProxyResponseWriter) WriteHeader(status int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.wrote {
		return
	}
	w.wrote = true
	w.writeJSON(map[string]interface{}{"jsonrpc": "2.0", "id": w.id, "method": "proxy/response",
		"params": map[string]interface{}{"status": status, "headers": map[string][]string(w.header.Clone())}})
}
func (w *wsProxyResponseWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	written := len(data)
	if !w.wrote {
		w.wrote = true
		w.writeJSON(map[string]interface{}{"jsonrpc": "2.0", "id": w.id, "method": "proxy/response",
			"params": map[string]interface{}{"status": http.StatusOK, "headers": map[string][]string(w.header.Clone())}})
	}
	for len(data) > 0 {
		n := len(data)
		if n > proxyTunnelChunkBytes {
			n = proxyTunnelChunkBytes
		}
		w.writeJSON(map[string]interface{}{"jsonrpc": "2.0", "id": w.id, "method": "proxy/body",
			"params": map[string]interface{}{"data_b64": base64.StdEncoding.EncodeToString(data[:n])}})
		data = data[n:]
	}
	return written, nil
}
func (w *wsProxyResponseWriter) Flush() {}

func (gw *Gateway) handleHTTPProxyOverWS(ctx context.Context, reqID interface{}, params proxyHTTPRequestParams, body io.Reader, writeJSON func(interface{})) {
	target, err := proxyUpstreamURL(params.Upstream, params.Path, params.RawQuery)
	if err != nil {
		writeJSON(api.JSONRPCResponse{JSONRPC: "2.0", ID: reqID, Error: &api.RPCError{Code: -32602, Message: err.Error()}})
		return
	}
	if !gw.proxyHostAllowed(target.Hostname()) {
		writeJSON(api.JSONRPCResponse{JSONRPC: "2.0", ID: reqID, Error: &api.RPCError{Code: -32003, Message: "proxy upstream host is not allowlisted"}})
		return
	}
	req, err := http.NewRequestWithContext(ctx, params.Method, target.String(), io.NopCloser(body))
	if err != nil {
		writeJSON(api.JSONRPCResponse{JSONRPC: "2.0", ID: reqID, Error: &api.RPCError{Code: -32602, Message: err.Error()}})
		return
	}
	req.Header = headersFromJSON(params.Header)
	req.Header.Del("Authorization")
	req.Host = target.Host
	req.Header.Set("X-Forwarded-Host", target.Host)
	req.ContentLength = params.ContentLength
	writeUpstreamError := func(err error) {
		// A transport failure after Do cannot prove the upstream mutation stopped.
		if !proxyRequestIsReadOnly(params.Method) {
			err = fmt.Errorf("%w: %v", errExecutionOutcomeUnknown, err)
		}
		writeJSON(api.JSONRPCResponse{JSONRPC: "2.0", ID: reqID, Error: &api.RPCError{Code: -32004, Message: err.Error()}})
	}
	response, err := (&http.Client{
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}).Do(req)
	if err != nil {
		writeUpstreamError(err)
		return
	}
	defer response.Body.Close()
	rw := &wsProxyResponseWriter{id: reqID, writeJSON: writeJSON, header: response.Header.Clone()}
	rw.WriteHeader(response.StatusCode)
	buf := make([]byte, proxyTunnelChunkBytes)
	if _, err := io.CopyBuffer(rw, response.Body, buf); err != nil {
		writeUpstreamError(err)
		return
	}
	writeJSON(map[string]interface{}{"jsonrpc": "2.0", "id": reqID, "result": map[string]interface{}{"ok": true}})
}
