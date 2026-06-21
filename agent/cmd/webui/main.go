// doops-webui 是一个轻量 Web 控制台后端：
//   - 托管内置的静态前端（embed）
//   - 反向代理浏览器到 doops-gateway 的 /v1/auth/login、/v1/targets
//   - 把浏览器的 WebSocket 转发到 gateway /v1/rpc，并注入 Authorization 头
//
// 之所以需要这个后端：浏览器原生 WebSocket 无法设置 Authorization 请求头，
// gateway 又只认 Bearer token，因此由本服务在服务端注入 token 并双向转发帧。
package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

//go:embed static
var embeddedStatic embed.FS

var (
	upgrader = websocket.Upgrader{
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
		// 同源页面发起，放开 CheckOrigin 由部署侧用反代/防火墙约束。
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	httpClient = &http.Client{Timeout: 30 * time.Second}
)

func main() {
	port := flag.String("port", "8088", "Web UI listen port")
	defaultGateway := flag.String("gateway", "", "Default gateway base URL prefilled in the UI (optional)")
	staticDir := flag.String("static", "", "Serve static assets from this directory instead of the embedded copy (dev)")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/login", handleLogin)
	mux.HandleFunc("/api/targets", handleTargets)
	mux.HandleFunc("/api/rpc", handleRPC)
	mux.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"default_gateway": *defaultGateway})
	})
	mux.Handle("/", staticHandler(*staticDir))

	log.Printf("🖥  doops-webui listening on :%s", *port)
	if *defaultGateway != "" {
		log.Printf("   default gateway: %s", *defaultGateway)
	}
	if err := http.ListenAndServe(":"+*port, mux); err != nil {
		log.Fatal(err)
	}
}

func staticHandler(dir string) http.Handler {
	if strings.TrimSpace(dir) != "" {
		return http.FileServer(http.Dir(dir))
	}
	sub, err := fs.Sub(embeddedStatic, "static")
	if err != nil {
		log.Fatalf("failed to load embedded static assets: %v", err)
	}
	return http.FileServer(http.FS(sub))
}

type loginRequest struct {
	Gateway  string `json:"gateway"`
	Username string `json:"username"`
	Password string `json:"password"`
	Name     string `json:"name,omitempty"`
}

func handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	var req loginRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid login request"})
		return
	}
	base, err := normalizeHTTPBase(req.Gateway)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	payload, _ := json.Marshal(map[string]string{
		"username": req.Username,
		"password": req.Password,
		"name":     req.Name,
	})
	upstream, err := http.NewRequestWithContext(r.Context(), http.MethodPost, base+"/v1/auth/login", bytes.NewReader(payload))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	upstream.Header.Set("Content-Type", "application/json")
	proxyJSON(w, upstream)
}

func handleTargets(w http.ResponseWriter, r *http.Request) {
	base, err := normalizeHTTPBase(r.URL.Query().Get("gateway"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	upstream, err := http.NewRequestWithContext(r.Context(), http.MethodGet, base+"/v1/targets", nil)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	if token := bearerFromRequest(r); token != "" {
		upstream.Header.Set("Authorization", "Bearer "+token)
	}
	proxyJSON(w, upstream)
}

// handleRPC 把浏览器 WS 双向桥接到 gateway /v1/rpc，并注入 Bearer token。
func handleRPC(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	base, err := normalizeWSBase(q.Get("gateway"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	cluster := strings.TrimSpace(q.Get("cluster"))
	instance := strings.TrimSpace(q.Get("instance"))
	token := strings.TrimSpace(q.Get("token"))
	if instance == "" {
		http.Error(w, "missing instance", http.StatusBadRequest)
		return
	}
	if cluster == "" {
		cluster = "default"
	}

	target, err := url.Parse(base + "/v1/rpc")
	if err != nil {
		http.Error(w, "invalid gateway URL", http.StatusBadRequest)
		return
	}
	tq := target.Query()
	tq.Set("cluster", cluster)
	tq.Set("instance", instance)
	target.RawQuery = tq.Encode()

	header := http.Header{}
	if token != "" {
		header.Set("Authorization", "Bearer "+token)
		header.Set("X-Doops-Key", token)
	}

	upstreamConn, resp, err := websocket.DefaultDialer.DialContext(r.Context(), target.String(), header)
	if err != nil {
		status := http.StatusBadGateway
		msg := err.Error()
		if resp != nil {
			status = resp.StatusCode
			if body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096)); len(body) > 0 {
				msg = strings.TrimSpace(string(body))
			}
		}
		http.Error(w, "gateway connect failed: "+msg, status)
		return
	}
	defer upstreamConn.Close()

	clientConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer clientConn.Close()

	bridge(clientConn, upstreamConn)
}

// bridge 在两个 WS 连接之间双向转发消息，任一方向出错即结束。
func bridge(a, b *websocket.Conn) {
	var once sync.Once
	done := make(chan struct{})
	stop := func() { once.Do(func() { close(done) }) }

	copyFrames := func(dst, src *websocket.Conn) {
		defer stop()
		for {
			msgType, data, err := src.ReadMessage()
			if err != nil {
				_ = dst.WriteControl(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
					time.Now().Add(time.Second))
				return
			}
			if err := dst.WriteMessage(msgType, data); err != nil {
				return
			}
		}
	}

	go copyFrames(a, b)
	go copyFrames(b, a)
	<-done
}

func proxyJSON(w http.ResponseWriter, upstream *http.Request) {
	resp, err := httpClient.Do(upstream)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/json"
	}
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(resp.StatusCode)
	if len(body) == 0 && resp.StatusCode >= 400 {
		body = []byte(fmt.Sprintf(`{"error":%q}`, http.StatusText(resp.StatusCode)))
	}
	_, _ = w.Write(body)
}

func bearerFromRequest(r *http.Request) string {
	token := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.HasPrefix(strings.ToLower(token), "bearer ") {
		return strings.TrimSpace(token[len("bearer "):])
	}
	if token == "" {
		token = strings.TrimSpace(r.URL.Query().Get("token"))
	}
	return token
}

func normalizeHTTPBase(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("missing gateway URL")
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid gateway URL: %w", err)
	}
	switch u.Scheme {
	case "http", "https":
	default:
		return "", fmt.Errorf("unsupported gateway scheme %q", u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("invalid gateway URL: missing host")
	}
	return strings.TrimRight(u.Scheme+"://"+u.Host+u.Path, "/"), nil
}

func normalizeWSBase(raw string) (string, error) {
	base, err := normalizeHTTPBase(raw)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(base, "https://") {
		return "wss://" + strings.TrimPrefix(base, "https://"), nil
	}
	return "ws://" + strings.TrimPrefix(base, "http://"), nil
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
