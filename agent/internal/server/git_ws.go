package server

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/user/doops/agent/api"
)

func (gw *Gateway) handleGitHTTPOverWS(reqID interface{}, params gitHTTPRequestParams, body io.Reader, writeJSON func(v interface{})) {
	if gw.gitHandler == nil {
		writeJSON(api.JSONRPCResponse{
			JSONRPC: "2.0",
			ID:      reqID,
			Error:   &api.RPCError{Code: -32603, Message: "git handler is not initialized"},
		})
		return
	}
	if !strings.HasPrefix(params.Path, "/git/") {
		writeJSON(api.JSONRPCResponse{
			JSONRPC: "2.0",
			ID:      reqID,
			Error:   &api.RPCError{Code: -32602, Message: "git path must start with /git/"},
		})
		return
	}
	if params.Method == "" {
		params.Method = http.MethodGet
	}

	u := &url.URL{Path: params.Path, RawQuery: params.RawQuery}
	req := &http.Request{
		Method:        params.Method,
		URL:           u,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header(params.Header).Clone(),
		Body:          io.NopCloser(body),
		ContentLength: params.ContentLength,
		Host:          "doops-agent",
		RemoteAddr:    "doops-gateway-tunnel",
		RequestURI:    u.RequestURI(),
	}
	defer req.Body.Close()
	req.Header.Del("Authorization")
	req.Header.Del("X-Doops-Key")
	if gw.Token != "" {
		req.SetBasicAuth("doops", gw.Token)
	}

	rw := newWSGitResponseWriter(reqID, writeJSON)
	defer func() {
		if recovered := recover(); recovered != nil {
			writeJSON(api.JSONRPCResponse{
				JSONRPC: "2.0",
				ID:      reqID,
				Error:   &api.RPCError{Code: -32603, Message: fmt.Sprintf("git handler panic: %v", recovered)},
			})
			return
		}
		rw.finish()
	}()
	gw.gitHandler.ServeHTTP(rw, req)
}

type wsGitResponseWriter struct {
	id          interface{}
	writeJSON   func(v interface{})
	header      http.Header
	wroteHeader bool
	mu          sync.Mutex
}

func newWSGitResponseWriter(id interface{}, writeJSON func(v interface{})) *wsGitResponseWriter {
	return &wsGitResponseWriter{
		id:        id,
		writeJSON: writeJSON,
		header:    http.Header{},
	}
}

func (w *wsGitResponseWriter) Header() http.Header {
	return w.header
}

func (w *wsGitResponseWriter) WriteHeader(statusCode int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writeHeaderLocked(statusCode)
}

func (w *wsGitResponseWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	written := len(data)
	if !w.wroteHeader {
		w.writeHeaderLocked(http.StatusOK)
	}
	for len(data) > 0 {
		n := len(data)
		if n > gitTunnelChunkBytes {
			n = gitTunnelChunkBytes
		}
		w.writeJSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      w.id,
			"method":  "git/body",
			"params": map[string]interface{}{
				"data_b64": base64.StdEncoding.EncodeToString(data[:n]),
			},
		})
		data = data[n:]
	}
	return written, nil
}

func (w *wsGitResponseWriter) Flush() {}

func (w *wsGitResponseWriter) finish() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.wroteHeader {
		w.writeHeaderLocked(http.StatusOK)
	}
	w.writeJSON(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      w.id,
		"method":  "git/body",
		"params":  map[string]interface{}{"eof": true},
	})
	w.writeJSON(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      w.id,
		"result":  map[string]interface{}{"ok": true},
	})
}

func (w *wsGitResponseWriter) writeHeaderLocked(statusCode int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.writeJSON(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      w.id,
		"method":  "git/response",
		"params": map[string]interface{}{
			"status":  statusCode,
			"headers": map[string][]string(w.header.Clone()),
		},
	})
}
