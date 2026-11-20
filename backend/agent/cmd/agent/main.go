package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/user/doops/agent/internal/dispatcher"
	"github.com/user/doops/agent/internal/server"
	"github.com/user/doops/agent/pkg/release"
)

const defaultAgentListenAddress = "0.0.0.0"

var (
	reverseTunnelConnected int32
	reverseTunnelDial      = websocket.DefaultDialer.Dial
	reverseTunnelServeConn = func(gw *server.Gateway, conn *websocket.Conn, name string, onReady func()) {
		gw.ServeGatewayWebSocketConnWithReady(conn, name, onReady)
	}
	reverseTunnelContinue = func() bool { return true }
)

func main() {
	if err := dispatcher.ValidateConfig(); err != nil {
		log.Fatal(err)
	}
	if len(os.Args) > 1 && os.Args[1] == "database-runner" {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if len(os.Args) != 2 {
			log.Fatal("database-runner does not accept positional arguments")
		}
		if err := server.RunDatabaseRunnerContext(ctx, os.Stdin, os.Stdout, os.Stderr); err != nil {
			log.Fatal(err)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "doagent-settings" {
		// Run by the entrypoint before doagent starts: doagent reads only its
		// fixed settings file and refuses to start when it is missing. The
		// model itself arrives from the Gateway after the Agent connects.
		if len(os.Args) != 2 {
			log.Fatal("doagent-settings does not accept arguments")
		}
		if err := server.EnsureDoagentBaseSettings(); err != nil {
			log.Fatalf("write doagent settings: %v", err)
		}
		return
	}
	if err := server.InitializeMutationAuthority(); err != nil {
		log.Fatal(err)
	}
	if len(os.Args) > 1 && os.Args[1] == "mcp-stdio" {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		mcpFlags := flag.NewFlagSet("mcp-stdio", flag.ContinueOnError)
		domain := mcpFlags.String("domain", "doops", "MCP domain: doops or dosql")
		if err := mcpFlags.Parse(os.Args[2:]); err != nil {
			log.Fatal(err)
		}
		if mcpFlags.NArg() != 0 {
			log.Fatalf("unexpected mcp-stdio arguments: %s", strings.Join(mcpFlags.Args(), " "))
		}
		if err := server.RunK8SMCPStdioDomainContext(ctx, os.Stdin, os.Stdout, *domain); err != nil {
			log.Fatal(err)
		}
		return
	}

	port := flag.String("port", "42222", "Agent port")
	listen := flag.String("listen", defaultAgentListenAddress, "Bind address for health endpoints")
	gatewayURL := flag.String("gateway-url", "", "Public doops-gateway URL for reverse tunnel mode")
	cluster := flag.String("cluster", "", "Cluster name registered in reverse tunnel mode")
	instance := flag.String("instance", "", "Agent instance name registered in reverse tunnel mode")
	reconnectDelay := flag.Duration("reconnect-delay", time.Second, "Reverse tunnel reconnect delay")
	edge := flag.Bool("edge", false, "Enable Edge public reverse tunnel")
	edgeGatewayURL := flag.String("edge-gateway-url", "", "Edge gateway URL; defaults to -gateway-url with /v1/edge/connect")
	enrollTokenFlag := flag.String("enroll-token", "", "Optional enrollment token for this cluster and instance; defaults to DOOPS_AGENT_ENROLL_TOKEN. Required only after this target has connected once with an issued token")
	edgeEnrollToken := flag.String("edge-enroll-token", "", "Edge enrollment token")
	edgeBackendAllow := flag.String("edge-backend-allow", "", "Comma-separated host:port or cidr:port backends the Edge tunnel may dial")
	edgeConns := flag.Int("edge-conns", 1, "Parallel Edge WebSocket sessions")
	edgeMaxConns := flag.Int("edge-max-conns", 512, "Maximum concurrent Edge TCP connections on this agent")
	edgeDialTimeout := flag.Duration("edge-dial-timeout", 5*time.Second, "Timeout when dialing an Edge backend")
	printVersion := flag.Bool("version", false, "Print the DoOps release identity")
	flag.Parse()
	if *printVersion {
		fmt.Printf("release=%s commit=%s\n", release.Version, release.Commit)
		return
	}
	if err := validateStartupArgs(*gatewayURL, *cluster, *instance, flag.Args()); err != nil {
		log.Fatal(err)
	}

	enrollToken := strings.TrimSpace(*enrollTokenFlag)
	if enrollToken == "" {
		enrollToken = strings.TrimSpace(os.Getenv("DOOPS_AGENT_ENROLL_TOKEN"))
	}

	gw := server.NewGateway()

	// Start TTL GC running in the background routines
	// Max age: 1 day, Interval: every 1 hour
	go server.StartWorkspaceGC(24*time.Hour, 1*time.Hour)

	// doagent ACP HTTP 服务由 entrypoint.sh 在容器启动时拉起（port 9000）

	go runReverseTunnel(gw, *gatewayURL, *cluster, *instance, enrollToken, *reconnectDelay)
	if *edge {
		if strings.TrimSpace(*edgeEnrollToken) == "" || strings.TrimSpace(*edgeBackendAllow) == "" {
			log.Fatal("-edge requires -edge-enroll-token and -edge-backend-allow")
		}
		edgeURL := strings.TrimSpace(*edgeGatewayURL)
		if edgeURL == "" {
			edgeURL = *gatewayURL
		}
		runEdgeTunnels(edgeURL, *cluster, *instance, *edgeEnrollToken, *edgeBackendAllow, *edgeConns, *edgeMaxConns, *edgeDialTimeout, *reconnectDelay)
	}

	mux := http.NewServeMux()
	registerAgentHTTP(mux)

	log.Printf("🚦 health endpoints: /health/live (always 200), /health/ready (200 when tunnel connected)")
	gw.SetupGitHandler()

	bindHost := strings.TrimSpace(*listen)
	listenAddr := net.JoinHostPort(bindHost, *port)

	log.Printf("🚀 doops.sh agent health server starting on %s", listenAddr)

	if err := http.ListenAndServe(listenAddr, mux); err != nil {
		log.Fatal(err)
	}
}

func validateStartupArgs(gatewayURL, cluster, instance string, args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("unexpected positional arguments: %s", strings.Join(args, " "))
	}
	if strings.TrimSpace(gatewayURL) == "" ||
		strings.TrimSpace(cluster) == "" ||
		strings.TrimSpace(instance) == "" {
		return fmt.Errorf("-gateway-url, -cluster, and -instance are required")
	}
	return nil
}

func isReverseTunnelConnected() bool {
	return atomic.LoadInt32(&reverseTunnelConnected) == 1
}

func setReverseTunnelConnected(connected bool) {
	if connected {
		atomic.StoreInt32(&reverseTunnelConnected, 1)
		return
	}
	atomic.StoreInt32(&reverseTunnelConnected, 0)
}

func runReverseTunnel(gw *server.Gateway, rawURL, cluster, instance, enrollToken string, reconnectDelay time.Duration) {
	setReverseTunnelConnected(false)
	if reconnectDelay <= 0 {
		reconnectDelay = time.Second
	}
	cluster = strings.TrimSpace(cluster)
	instance = strings.TrimSpace(instance)
	var authBackoff time.Duration

	for {
		if reverseTunnelContinue != nil && !reverseTunnelContinue() {
			return
		}
		tunnelURL, err := buildGatewayAgentURL(rawURL, cluster, instance)
		if err != nil {
			log.Printf("❌ invalid gateway URL %q: %v", rawURL, err)
			time.Sleep(reconnectDelay)
			continue
		}

		log.Printf("🌉 connecting reverse tunnel to %s (cluster=%s instance=%s)", tunnelURL, cluster, instance)
		var header http.Header
		if token := strings.TrimSpace(enrollToken); token != "" {
			header = http.Header{"Authorization": []string{"Bearer " + token}}
		}
		conn, resp, err := reverseTunnelDial(tunnelURL, header)
		if err != nil {
			wait := reconnectDelay
			if resp != nil {
				log.Printf("❌ reverse tunnel dial failed: %v (HTTP %s)", err, resp.Status)
				if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
					authBackoff = nextReverseTunnelAuthBackoff(authBackoff, reconnectDelay)
					wait = authBackoff
					if strings.TrimSpace(enrollToken) == "" {
						log.Printf("🔑 gateway requires an enrollment token for cluster=%s instance=%s; set DOOPS_AGENT_ENROLL_TOKEN to the token issued by `doops-gateway enroll create` for this target, or revoke the target's enrollment on the Gateway. Retrying in %s", cluster, instance, wait)
					} else {
						log.Printf("🔑 gateway rejected the enrollment token for cluster=%s instance=%s; check DOOPS_AGENT_ENROLL_TOKEN matches an active enrollment for this exact target (`doops-gateway enroll list`). Retrying in %s", cluster, instance, wait)
					}
				}
			} else {
				log.Printf("❌ reverse tunnel dial failed: %v", err)
			}
			time.Sleep(wait)
			continue
		}
		authBackoff = 0

		log.Printf("✅ reverse tunnel connected: cluster=%s instance=%s", cluster, instance)
		func() {
			defer setReverseTunnelConnected(false)
			reverseTunnelServeConn(gw, conn, "doops-gateway:"+tunnelURL, func() {
				setReverseTunnelConnected(true)
			})
		}()
		log.Printf("⚠️ reverse tunnel disconnected; reconnecting in %s", reconnectDelay)
		time.Sleep(reconnectDelay)
	}
}

// reverseTunnelMaxAuthBackoff caps the retry interval after the Gateway
// refuses registration (401/403), so a misconfigured token does not hammer the
// Gateway while the Agent still recovers on its own once the token is fixed.
const reverseTunnelMaxAuthBackoff = 60 * time.Second

func nextReverseTunnelAuthBackoff(current, base time.Duration) time.Duration {
	if base <= 0 {
		base = time.Second
	}
	if current < base {
		return base
	}
	next := current * 2
	if next > reverseTunnelMaxAuthBackoff || next <= 0 {
		next = reverseTunnelMaxAuthBackoff
	}
	if next < base {
		next = base
	}
	return next
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
	} else if strings.TrimRight(u.Path, "/") == "/v1/agent/connect" {
		u.Path = "/v1/agent/connect"
	} else {
		return "", fmt.Errorf("invalid gateway URL path %q: use the doops-gateway base URL or /v1/agent/connect", u.Path)
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
