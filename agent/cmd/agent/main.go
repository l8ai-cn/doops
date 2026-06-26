package main

import (
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/user/doops/agent/internal/server"
)

const defaultAgentTokenPath = "/root/.doops/agent-token"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "token" {
		token, err := loadOrCreateAgentToken("")
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(token)
		return
	}

	port := flag.String("port", "42222", "Agent port")
	listen := flag.String("listen", "0.0.0.0", "Bind address for the agent HTTP/WS listener (host or IP). Default 0.0.0.0 preserves host-network deployments; use 127.0.0.1 to restrict to loopback")
	tokenFlag := flag.String("token", "", "Agent authentication token")
	gatewayURL := flag.String("gateway-url", "", "Public doops-gateway URL for reverse tunnel mode")
	cluster := flag.String("cluster", "", "Cluster name registered in reverse tunnel mode")
	instance := flag.String("instance", "", "Agent instance name registered in reverse tunnel mode")
	agentToken := flag.String("agent-token", "", "Legacy doops-gateway agent registration token; current gateways do not require it")
	reconnectDelay := flag.Duration("reconnect-delay", time.Second, "Reverse tunnel reconnect delay")
	insecureGit := flag.Bool("insecure-git", false, "Allow anonymous git push/pull when no token is configured (default-deny otherwise)")
	flag.Parse()

	token := *tokenFlag
	token, err := loadOrCreateAgentToken(token)
	if err != nil {
		log.Fatal(err)
	}

	gw := server.NewGateway(*port)
	gw.Token = token
	gw.InsecureGit = *insecureGit

	// Start TTL GC running in the background routines
	// Max age: 1 day, Interval: every 1 hour
	go server.StartWorkspaceGC(24*time.Hour, 1*time.Hour)

	// doagent ACP HTTP 服务由 entrypoint.sh 在容器启动时拉起（port 9000）

	if strings.TrimSpace(*gatewayURL) != "" {
		tunnelToken := strings.TrimSpace(*agentToken)
		if tunnelToken == "" {
			tunnelToken = token
		}
		go runReverseTunnel(gw, *gatewayURL, tunnelToken, *cluster, *instance, *reconnectDelay)
	}

	http.HandleFunc("/ws", gw.HandleWebSocket) // 新增 WebSocket 端点
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	gw.SetupGitHandler()

	bindHost := strings.TrimSpace(*listen)
	listenAddr := net.JoinHostPort(bindHost, *port)

	log.Printf("🚀 doops.sh agent gateway starting on %s", listenAddr)
	if token != "" {
		log.Printf("   🔒 Token authentication enabled")
	}
	logHost := bindHost
	if logHost == "" || logHost == "0.0.0.0" || logHost == "::" {
		logHost = "<host>"
	}
	log.Printf("   WS endpoint: ws://%s:%s/ws", logHost, *port)

	if err := http.ListenAndServe(listenAddr, nil); err != nil {
		log.Fatal(err)
	}
}

func runReverseTunnel(gw *server.Gateway, rawURL, token, cluster, instance string, reconnectDelay time.Duration) {
	if reconnectDelay <= 0 {
		reconnectDelay = time.Second
	}
	if cluster = strings.TrimSpace(cluster); cluster == "" {
		cluster = "default"
	}
	if instance = strings.TrimSpace(instance); instance == "" {
		if hostname, err := os.Hostname(); err == nil && strings.TrimSpace(hostname) != "" {
			instance = hostname
		} else {
			instance = "agent"
		}
	}

	for {
		tunnelURL, err := buildGatewayAgentURL(rawURL, cluster, instance)
		if err != nil {
			log.Printf("❌ invalid gateway URL %q: %v", rawURL, err)
			time.Sleep(reconnectDelay)
			continue
		}

		header := http.Header{}
		if token != "" {
			header.Set("Authorization", "Bearer "+token)
			header.Set("X-Doops-Key", token)
		}

		log.Printf("🌉 connecting reverse tunnel to %s (cluster=%s instance=%s)", tunnelURL, cluster, instance)
		conn, resp, err := websocket.DefaultDialer.Dial(tunnelURL, header)
		if err != nil {
			if resp != nil {
				log.Printf("❌ reverse tunnel dial failed: %v (HTTP %s)", err, resp.Status)
			} else {
				log.Printf("❌ reverse tunnel dial failed: %v", err)
			}
			time.Sleep(reconnectDelay)
			continue
		}

		log.Printf("✅ reverse tunnel connected: cluster=%s instance=%s", cluster, instance)
		gw.ServeWebSocketConn(conn, "doops-gateway:"+tunnelURL)
		log.Printf("⚠️ reverse tunnel disconnected; reconnecting in %s", reconnectDelay)
		time.Sleep(reconnectDelay)
	}
}

func buildGatewayAgentURL(rawURL, cluster, instance string) (string, error) {
	rawURL = strings.TrimSpace(rawURL)
	if !strings.Contains(rawURL, "://") {
		rawURL = "wss://" + rawURL
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	case "ws", "wss":
	default:
		return "", fmt.Errorf("unsupported scheme %q", u.Scheme)
	}
	if err := enforceSecureGatewayURL(rawURL, u); err != nil {
		return "", err
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = "/v1/agent/connect"
	}
	q := u.Query()
	q.Set("cluster", cluster)
	q.Set("instance", instance)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// enforceSecureGatewayURL fails closed on insecure reverse-tunnel URLs.
//
// In tunnel mode we require wss:// (TLS). Plain ws:// (or the http:// that
// normalizes to it) is only permitted when the target is loopback (local
// development) or when the operator explicitly opts in via
// DOOPS_ALLOW_INSECURE_GATEWAY=1.
func enforceSecureGatewayURL(raw string, u *url.URL) error {
	if u.Scheme == "wss" {
		return nil
	}
	if insecureGatewayAllowed() {
		return nil
	}
	if isLoopbackHost(u.Hostname()) {
		return nil
	}
	return fmt.Errorf("refusing insecure gateway URL %q: use wss:// or set DOOPS_ALLOW_INSECURE_GATEWAY=1", raw)
}

func insecureGatewayAllowed() bool {
	return strings.TrimSpace(os.Getenv("DOOPS_ALLOW_INSECURE_GATEWAY")) == "1"
}

func isLoopbackHost(host string) bool {
	host = strings.TrimSpace(host)
	if host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

func agentTokenPath() string {
	if path := strings.TrimSpace(os.Getenv("DOOPS_AGENT_TOKEN_PATH")); path != "" {
		return path
	}
	return defaultAgentTokenPath
}

func loadOrCreateAgentToken(explicit string) (string, error) {
	explicit = strings.TrimSpace(explicit)
	path := agentTokenPath()

	if explicit != "" {
		if err := writeAgentToken(path, explicit); err != nil {
			return "", err
		}
		return explicit, nil
	}

	if data, err := os.ReadFile(path); err == nil {
		token := strings.TrimSpace(string(data))
		if token != "" {
			return token, nil
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}

	token, err := generateAgentToken()
	if err != nil {
		return "", err
	}
	if err := writeAgentToken(path, token); err != nil {
		return "", err
	}
	return token, nil
}

func generateAgentToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func writeAgentToken(path, token string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(token+"\n"), 0600)
}
