package gatewaycmd

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/user/doops/agent/internal/server"
)

const defaultGatewayDB = "/var/lib/doops-gateway/gateway.db"

func Run(args []string) {
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd = args[0]
		args = args[1:]
	}

	switch cmd {
	case "serve":
		serve(args)
	case "user":
		user(args)
	case "token":
		userToken(args)
	case "agent-token":
		agentToken(args)
	case "grant":
		grant(args)
	case "audit":
		audit(args)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", cmd)
		os.Exit(2)
	}
}

func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	port := fs.String("port", "42222", "Public doops-gateway port")
	dbPath := fs.String("db", defaultGatewayDB, "SQLite database path")
	loginTokenTTL := fs.Duration("login-token-ttl", 24*time.Hour, "Password-login token TTL")
	operationTimeout := fs.Duration("operation-timeout", 30*time.Minute, "Maximum runtime for one forwarded operation")
	maxConcurrentOperations := fs.Int("max-concurrent-operations", 64, "Maximum concurrent forwarded operations across all users")
	maxConcurrentPerUser := fs.Int("max-concurrent-per-user", 8, "Maximum concurrent forwarded operations per user")
	targetQueueTimeout := fs.Duration("target-queue-timeout", 2*time.Minute, "How long an operation waits for a busy target")
	maxQueuedPerTarget := fs.Int("max-queued-per-target", 8, "Maximum queued operations per target; negative disables queueing")
	schedulerTick := fs.Duration("scheduler-tick", time.Minute, "How often the issue-scheduler checks for due jobs; 0 disables the scheduler")
	fs.Parse(args)

	store, err := server.OpenGatewayStore(*dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()

	hub := server.NewGatewayHub(store, server.GatewayHubOptions{
		LoginTokenTTL:           *loginTokenTTL,
		OperationTimeout:        *operationTimeout,
		MaxConcurrentOperations: *maxConcurrentOperations,
		MaxConcurrentPerUser:    *maxConcurrentPerUser,
		TargetQueueTimeout:      *targetQueueTimeout,
		MaxQueuedPerTarget:      *maxQueuedPerTarget,
	})
	if *schedulerTick > 0 {
		scheduler := server.NewScheduler(hub, store, *schedulerTick)
		hub.AttachScheduler(scheduler)
		scheduler.Start()
	}

	mux := http.NewServeMux()
	hub.RegisterRoutes(mux)

	log.Printf("🌉 doops-gateway starting on :%s", *port)
	log.Printf("   database: %s", *dbPath)
	log.Printf("   agent endpoint: ws://127.0.0.1:%s/v1/agent/connect", *port)
	log.Printf("   client endpoint: ws://127.0.0.1:%s/v1/rpc?cluster=<cluster>&instance=<instance>", *port)
	if err := http.ListenAndServe(":"+*port, mux); err != nil {
		log.Fatal(err)
	}
}

func user(args []string) {
	if len(args) == 0 || (args[0] != "create" && args[0] != "passwd") {
		fmt.Fprintln(os.Stderr, "usage: doops-gateway user create -name <name> [-password pass] [-db path]")
		fmt.Fprintln(os.Stderr, "       doops-gateway user passwd -name <name> -password <pass> [-db path]")
		os.Exit(2)
	}
	if args[0] == "passwd" {
		userPasswd(args[1:])
		return
	}
	fs := flag.NewFlagSet("user create", flag.ExitOnError)
	name := fs.String("name", "", "User name")
	password := fs.String("password", "", "Optional login password")
	dbPath := fs.String("db", defaultGatewayDB, "SQLite database path")
	fs.Parse(args[1:])
	store := mustStore(*dbPath)
	defer store.Close()
	u, err := store.CreateUserWithPassword(server.CreateUserRequest{Name: *name, Password: *password})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("user_id=%s name=%s\n", u.ID, u.Name)
	if *password != "" {
		fmt.Println("password=enabled")
	}
}

func userPasswd(args []string) {
	fs := flag.NewFlagSet("user passwd", flag.ExitOnError)
	name := fs.String("name", "", "User name")
	password := fs.String("password", "", "New login password")
	dbPath := fs.String("db", defaultGatewayDB, "SQLite database path")
	fs.Parse(args)
	store := mustStore(*dbPath)
	defer store.Close()
	u, err := store.FindUserByName(*name)
	if err != nil {
		log.Fatal(err)
	}
	if err := store.SetUserPassword(u.ID, *password); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("password=updated user=%s\n", u.Name)
}

func userToken(args []string) {
	if len(args) == 0 || args[0] != "create" {
		fmt.Fprintln(os.Stderr, "usage: doops-gateway token create -user <name> [-name label] [-db path]")
		os.Exit(2)
	}
	fs := flag.NewFlagSet("token create", flag.ExitOnError)
	userName := fs.String("user", "", "User name")
	name := fs.String("name", "", "Token label")
	dbPath := fs.String("db", defaultGatewayDB, "SQLite database path")
	expires := fs.Duration("expires", 0, "Optional token TTL, e.g. 720h")
	fs.Parse(args[1:])
	store := mustStore(*dbPath)
	defer store.Close()
	u, err := store.FindUserByName(*userName)
	if err != nil {
		log.Fatal(err)
	}
	req := server.CreateTokenRequest{Kind: server.TokenKindUser, UserID: u.ID, Name: *name}
	if *expires > 0 {
		req.ExpiresAt = time.Now().Add(*expires)
	}
	tok, err := store.CreateToken(req)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("token=%s\n", tok.Plaintext)
	fmt.Println("warning=store this token now; only its hash is saved")
}

func agentToken(args []string) {
	if len(args) == 0 || args[0] != "create" {
		fmt.Fprintln(os.Stderr, "usage: doops-gateway agent-token create -cluster <cluster> -instance <instance> [-db path]")
		os.Exit(2)
	}
	fs := flag.NewFlagSet("agent-token create", flag.ExitOnError)
	cluster := fs.String("cluster", "", "Allowed cluster")
	instance := fs.String("instance", "", "Allowed instance")
	name := fs.String("name", "", "Token label")
	dbPath := fs.String("db", defaultGatewayDB, "SQLite database path")
	fs.Parse(args[1:])
	store := mustStore(*dbPath)
	defer store.Close()
	tok, err := store.CreateToken(server.CreateTokenRequest{
		Kind:     server.TokenKindAgent,
		Name:     *name,
		Cluster:  *cluster,
		Instance: *instance,
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("agent_token=%s\n", tok.Plaintext)
	fmt.Println("warning=store this token now; only its hash is saved")
}

func grant(args []string) {
	fs := flag.NewFlagSet("grant", flag.ExitOnError)
	userName := fs.String("user", "", "User name")
	cluster := fs.String("cluster", "*", "Cluster scope")
	instance := fs.String("instance", "*", "Instance scope")
	actions := fs.String("actions", "", "Comma separated actions; empty means full target access")
	dbPath := fs.String("db", defaultGatewayDB, "SQLite database path")
	fs.Parse(args)
	store := mustStore(*dbPath)
	defer store.Close()
	u, err := store.FindUserByName(*userName)
	if err != nil {
		log.Fatal(err)
	}
	var parsed []server.GatewayAction
	for _, raw := range strings.Split(*actions, ",") {
		raw = strings.TrimSpace(raw)
		if raw != "" {
			parsed = append(parsed, server.GatewayAction(raw))
		}
	}
	if err := store.GrantUser(u.ID, server.ScopeGrant{Cluster: *cluster, Instance: *instance, Actions: parsed}); err != nil {
		log.Fatal(err)
	}
	if strings.TrimSpace(*actions) == "" {
		*actions = "default-full-target-access"
	}
	fmt.Printf("granted user=%s scope=%s/%s actions=%s\n", u.Name, *cluster, *instance, *actions)
}

func audit(args []string) {
	if len(args) == 0 || (args[0] != "list" && args[0] != "purge") {
		fmt.Fprintln(os.Stderr, "usage: doops-gateway audit list [-db path] [-limit N] [-user-id ID] [-cluster C] [-instance I] [-session S] [-action A] [-status X]")
		fmt.Fprintln(os.Stderr, "       doops-gateway audit purge -before <RFC3339> [-db path]")
		os.Exit(2)
	}
	switch args[0] {
	case "list":
		auditList(args[1:])
	case "purge":
		auditPurge(args[1:])
	}
}

func auditList(args []string) {
	fs := flag.NewFlagSet("audit list", flag.ExitOnError)
	dbPath := fs.String("db", defaultGatewayDB, "SQLite database path")
	limit := fs.Int("limit", 50, "Rows to show")
	userID := fs.String("user-id", "", "Filter by user id")
	cluster := fs.String("cluster", "", "Filter by cluster")
	instance := fs.String("instance", "", "Filter by instance")
	session := fs.String("session", "", "Filter by session")
	action := fs.String("action", "", "Filter by action")
	status := fs.String("status", "", "Filter by status")
	fs.Parse(args)
	store := mustStore(*dbPath)
	defer store.Close()
	events, err := store.ListAuditFiltered(server.AuditFilter{
		UserID:   *userID,
		Cluster:  *cluster,
		Instance: *instance,
		Session:  *session,
		Action:   server.GatewayAction(strings.TrimSpace(*action)),
		Status:   *status,
		Limit:    *limit,
	})
	if err != nil {
		log.Fatal(err)
	}
	for _, ev := range events {
		fmt.Printf("%d %s user=%s token=%s %s/%s action=%s session=%s status=%s error=%s cmd=%q\n",
			ev.ID, ev.StartedAt.Format(time.RFC3339), ev.UserID, ev.TokenID, ev.Cluster, ev.Instance,
			ev.Action, ev.Session, ev.Status, ev.Error, ev.CommandSummary)
	}
}

func auditPurge(args []string) {
	fs := flag.NewFlagSet("audit purge", flag.ExitOnError)
	dbPath := fs.String("db", defaultGatewayDB, "SQLite database path")
	before := fs.String("before", "", "Delete audit rows before this RFC3339 timestamp")
	fs.Parse(args)
	if strings.TrimSpace(*before) == "" {
		log.Fatal("-before is required")
	}
	cutoff, err := time.Parse(time.RFC3339, strings.TrimSpace(*before))
	if err != nil {
		log.Fatalf("invalid -before timestamp: %v", err)
	}
	store := mustStore(*dbPath)
	defer store.Close()
	deleted, err := store.DeleteAuditBefore(cutoff)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("deleted=%d before=%s\n", deleted, cutoff.UTC().Format(time.RFC3339))
}

func mustStore(path string) *server.GatewayStore {
	store, err := server.OpenGatewayStore(path)
	if err != nil {
		log.Fatal(err)
	}
	return store
}
