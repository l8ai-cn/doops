package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const inventoryPath = "/v1/assets/clusters"
const inventoryImport = `{"action":"import","asset_id":"k-1","name":"Test cluster","cluster":"dev","instance":"local"}`

func applyInventoryMigration(t *testing.T, f *executionLifecycleFixture) {
	t.Helper()
	sql, err := os.ReadFile("../../../docs/migrations/cluster_inventory.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.Exec(string(sql)); err != nil {
		t.Fatal(err)
	}
}

func inventoryTestAgent(t *testing.T, f *executionLifecycleFixture, instance string) func() {
	t.Helper()
	gw := &Gateway{tasks: make(map[string]*bgTask), sessionMap: make(map[string]*sessionEntry)}
	conn := dialFakeGatewayAgent(t, f.server.URL, "dev", instance)
	done := make(chan struct{})
	go func() {
		defer close(done)
		gw.ServeGatewayWebSocketConnWithReady(conn, "inventory-"+instance, nil)
	}()
	closeAgent := func() {
		_ = conn.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("Agent did not stop")
		}
		requireEventually(t, 3*time.Second, func() bool { return f.hub.getAgent("dev", instance) == nil })
	}
	t.Cleanup(closeAgent)
	waitForGatewayAgent(t, f.hub, "dev", instance)
	return closeAgent
}

func getTestInventory(t *testing.T, f *executionLifecycleFixture, user CreatedToken) []inventoryCluster {
	t.Helper()
	data := f.http(t, user, "GET", inventoryPath, "", 200)
	var result struct {
		Clusters []inventoryCluster `json:"clusters"`
	}
	if err := json.Unmarshal(data, &result); err != nil || result.Clusters == nil {
		t.Fatalf("invalid list %s: %v", data, err)
	}
	return result.Clusters
}

func inventoryTestError(t *testing.T, data []byte, code string) {
	t.Helper()
	var failure struct {
		Code, Error string
		Namespaces  []inventoryNamespace
	}
	if err := json.Unmarshal(data, &failure); err != nil || failure.Code != code || failure.Error != code || len(failure.Namespaces) != 0 || strings.Contains(string(data), "synthetic-secret") {
		t.Fatalf("expected sanitized error %s, got %s", code, data)
	}
}

func TestClusterInventoryHTTP(t *testing.T) {
	f := newExecutionLifecycleFixture(t, GatewayHubOptions{})
	inventoryTestError(t, f.http(t, f.admin, "GET", inventoryPath, "", 503), "cluster_inventory_unavailable")
	applyInventoryMigration(t, f)
	applyInventoryMigration(t, f)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "kubectl"), []byte(`#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$INVENTORY_CALLS"
case "$1 $2" in
'get namespaces')
  test "$#" = 4 && test "$3" = --output && test "$4" = "$INVENTORY_LIST" || exit 6
  cat "$INVENTORY_FIXTURE" ;;
'get pods')
  test "$#" = 6 && test "$3" = --namespace && test "$5" = --output && test "$6" = "$INVENTORY_PODS" || exit 7
  case "$4" in
  denied) echo 'Error from server (Forbidden): synthetic-secret' >&2; exit 1 ;;
  reports|private) printf 'pods-listed\n' ;;
  *) exit 8 ;;
  esac ;;
*) exit 9 ;;
esac
`), 0755); err != nil {
		t.Fatal(err)
	}
	calls := filepath.Join(root, "calls")
	fixture := filepath.Join(root, "namespaces")
	t.Setenv("INVENTORY_CALLS", calls)
	t.Setenv("INVENTORY_FIXTURE", fixture)
	t.Setenv("INVENTORY_LIST", inventoryListOutput)
	t.Setenv("INVENTORY_PODS", inventoryPodsOutput)
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	setList := func(lines string) {
		t.Helper()
		if err := os.WriteFile(fixture, []byte("namespaces-begin\n"+lines+"namespaces-end\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	setList("reports\tr-1\nkube-system\tsystem-1\n")
	closeAgent := inventoryTestAgent(t, f, "local")
	closeOther := inventoryTestAgent(t, f, "other")
	operator := f.user(t, "inventory-reader", ScopeGrant{Cluster: "*", Instance: "local"})
	admin, err := f.store.VerifyUserToken(f.admin.Plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.Exec(`DELETE FROM grants WHERE user_id=?`, admin.UserID); err != nil {
		t.Fatal(err)
	}
	if !f.store.UserCanUseTarget(admin.UserID, "dev", "local") {
		t.Fatal("zero-grant admin failed standard target authorization")
	}
	post := func(token CreatedToken, body string, want int) []byte {
		t.Helper()
		return f.http(t, token, "POST", inventoryPath, body, want)
	}
	scoped := func(body, ns string) string { return strings.TrimSuffix(body, "}") + `,"namespace":"` + ns + `"}` }
	discover := strings.Replace(inventoryImport, `"import"`, `"discover"`, 1)
	for _, extra := range []string{`"namespaces":[{"name":"fabricated"}]`, `"kubeconfig":"synthetic-secret"`, `"namespace":""`, `"namespace":null`, `"namespace":"../bad"`} {
		inventoryTestError(t, post(f.admin, strings.TrimSuffix(inventoryImport, "}")+","+extra+"}", 400), "invalid_inventory_request")
	}
	inventoryTestError(t, post(operator, inventoryImport, 403), "inventory_forbidden")
	inventoryTestError(t, post(operator, strings.Replace(discover, "local", "other", 1), 403), "inventory_forbidden")
	if _, err := os.Stat(calls); !os.IsNotExist(err) {
		t.Fatal("invalid or unauthorized request reached kubectl")
	}
	preview := post(operator, scoped(discover, "reports"), 200)
	if !strings.Contains(string(preview), `"scope":"list_pods"`) || len(getTestInventory(t, f, operator)) != 0 {
		t.Fatalf("preview: %s", preview)
	}
	invocations, err := os.ReadFile(calls)
	if err != nil || string(invocations) != "get pods --namespace reports --output "+inventoryPodsOutput+"\n" {
		t.Fatalf("scoped discovery issued unexpected commands: %s, %v", invocations, err)
	}
	var whole inventoryCluster
	if err := json.Unmarshal(post(operator, discover, 200), &whole); err != nil || len(whole.Namespaces) != 2 || whole.Namespaces[0].UID != "system-1" {
		t.Fatalf("whole preview %#v: %v", whole, err)
	}
	if len(getTestInventory(t, f, operator)) != 0 {
		t.Fatal("discover persisted assets")
	}
	post(f.admin, inventoryImport, 200)
	// Replay the same import concurrently. Both verify, both are audited, and
	// the primary keys still identify a single source and namespace.
	var imports sync.WaitGroup
	start := make(chan struct{})
	for range 2 {
		imports.Add(1)
		go func() {
			defer imports.Done()
			<-start
			req := httptest.NewRequest("POST", inventoryPath, strings.NewReader(scoped(inventoryImport, "reports")))
			req.Header.Set("Authorization", "Bearer "+f.admin.Plaintext)
			w := httptest.NewRecorder()
			f.hub.HandleClusterInventory(w, req)
			if w.Code != 200 {
				t.Errorf("concurrent import: HTTP %d: %s", w.Code, w.Body)
			}
		}()
	}
	close(start)
	imports.Wait()
	post(f.admin, scoped(strings.Replace(inventoryImport, "local", "other", 1), "private"), 200)
	if got := getTestInventory(t, f, f.admin); len(got) != 2 || len(got[1].Namespaces) != 1 || got[1].Namespaces[0].Name != "private" {
		t.Fatalf("admin sources %#v", got)
	}
	before := getTestInventory(t, f, operator)
	if len(before) != 1 || len(before[0].Namespaces) != 2 {
		t.Fatalf("source isolation %#v", before)
	}
	ns := before[0].Namespaces[1]
	if ns.Name != "reports" || ns.UID != "r-1" || ns.Scope != "list_pods" || ns.PodsListedAt != ns.VerifiedAt || ns.NamespaceObservedAt == "" || ns.VerifiedBy != admin.UserID {
		t.Fatalf("namespace evidence %#v", ns)
	}
	inventoryTestError(t, post(f.admin, scoped(inventoryImport, "denied"), 403), "kubernetes_read_forbidden")
	inventoryTestError(t, post(f.admin, strings.Replace(inventoryImport, "k-1", "k-2", 1), 409), "cluster_identity_conflict")
	setList("kube-system\tsystem-2\n")
	inventoryTestError(t, post(f.admin, inventoryImport, 409), "cluster_identity_conflict")
	setList("bad record\n")
	inventoryTestError(t, post(f.admin, inventoryImport, 502), "invalid_inventory_evidence")
	if !reflect.DeepEqual(before, getTestInventory(t, f, operator)) {
		t.Fatal("failed refresh changed snapshot")
	}
	if _, err := f.store.db.Exec(`CREATE TRIGGER fail_inventory_audit BEFORE UPDATE OF status ON audit_events WHEN NEW.action='cluster_inventory:import' AND NEW.status='success' BEGIN SELECT RAISE(ABORT,'test failure'); END`); err != nil {
		t.Fatal(err)
	}
	setList("reports\tr-2\n")
	inventoryTestError(t, post(f.admin, inventoryImport, 503), "cluster_inventory_unavailable")
	if !reflect.DeepEqual(before, getTestInventory(t, f, operator)) {
		t.Fatal("audit failure committed inventory")
	}
	if _, err := f.store.db.Exec(`DROP TRIGGER fail_inventory_audit`); err != nil {
		t.Fatal(err)
	}
	post(f.admin, inventoryImport, 200)
	before = getTestInventory(t, f, operator)
	if len(before[0].Namespaces) != 1 || before[0].Namespaces[0].UID != "r-2" || before[0].Namespaces[0].PodsListedAt != "" {
		t.Fatalf("recreated namespace retained obsolete evidence: %#v", before)
	}
	closeAgent()
	inventoryTestError(t, post(f.admin, inventoryImport, 503), "target_offline")
	before[0].Status = "offline"
	if !reflect.DeepEqual(before, getTestInventory(t, f, operator)) {
		t.Fatal("offline snapshot changed")
	}
	if _, err := f.store.db.Exec(`DELETE FROM grants WHERE user_id=(SELECT user_id FROM tokens WHERE id=?)`, operator.ID); err != nil {
		t.Fatal(err)
	}
	if len(getTestInventory(t, f, operator)) != 0 {
		t.Fatal("revoked source remained visible")
	}
	events, err := f.store.ListAuditFiltered(AuditFilter{Action: "cluster_inventory:import", Status: "success", UserID: admin.UserID})
	if err != nil || len(events) != 5 || events[0].TokenID != f.admin.ID || events[0].EndedAt.IsZero() || !strings.Contains(events[0].CommandSummary, "asset_id=k-1") {
		t.Fatalf("import audit %#v: %v", events, err)
	}
	closeOther()
	persisted := getTestInventory(t, f, f.admin)
	f.server.Close()
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	f.open(t)
	if got := getTestInventory(t, f, f.admin); !reflect.DeepEqual(got, persisted) {
		t.Fatalf("reopened inventory %#v, want %#v", got, persisted)
	}

}

// Exercise kubectl's REST mapper and JSONPath printer with a local-only API.
// The HTTP/Agent tests above also run on hosts without kubectl installed.
func TestClusterInventoryRealKubectl(t *testing.T) {
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl executable required for REST/JSONPath integration")
	}
	f := newExecutionLifecycleFixture(t, GatewayHubOptions{})
	applyInventoryMigration(t, f)
	var mu sync.Mutex
	var resourcePaths []string
	kube := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.Header.Get("Authorization") != "" {
			t.Error("expected a read using the Agent's anonymous local kubeconfig")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api":
			fmt.Fprint(w, `{"kind":"APIVersions","apiVersion":"v1","versions":["v1"]}`)
			return
		case "/apis":
			fmt.Fprint(w, `{"kind":"APIGroupList","apiVersion":"v1","groups":[]}`)
			return
		case "/api/v1":
			fmt.Fprint(w, `{"kind":"APIResourceList","apiVersion":"v1","groupVersion":"v1","resources":[{"name":"namespaces","kind":"Namespace","namespaced":false,"verbs":["list"]},{"name":"pods","kind":"Pod","namespaced":true,"verbs":["list"]}]}`)
			return
		}
		mu.Lock()
		resourcePaths = append(resourcePaths, r.URL.Path)
		mu.Unlock()
		switch r.URL.Path {
		case "/api/v1/namespaces":
			fmt.Fprint(w, `{"apiVersion":"v1","kind":"NamespaceList","items":[{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"reports","uid":"unique-real-printer","annotations":{"secret":"synthetic-secret"}}}]}`)
		case "/api/v1/namespaces/reports/pods":
			fmt.Fprint(w, `{"apiVersion":"v1","kind":"PodList","items":[]}`)
		case "/api/v1/namespaces/revoked/pods":
			if err := f.store.RevokeToken(f.admin.ID); err != nil {
				t.Error(err)
			}
			fmt.Fprint(w, `{"apiVersion":"v1","kind":"PodList","items":[]}`)
		default:
			t.Errorf("unexpected Kubernetes request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(kube.Close)
	root := t.TempDir()
	config := filepath.Join(root, "kubeconfig")
	if err := os.WriteFile(config, []byte(fmt.Sprintf(`{"apiVersion":"v1","kind":"Config","current-context":"fixture","contexts":[{"name":"fixture","context":{"cluster":"fixture"}}],"clusters":[{"name":"fixture","cluster":{"server":%q}}]}`, kube.URL)), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", config)
	t.Setenv("HOME", root) // Isolate kubectl's discovery cache from the user's configuration.
	inventoryTestAgent(t, f, "local")
	got := f.http(t, f.admin, "POST", inventoryPath, strings.TrimSuffix(inventoryImport, "}")+`,"namespace":"reports"}`, 200)
	if !strings.Contains(string(got), `"pods_listed_at"`) {
		t.Fatalf("empty PodList: %s", got)
	}
	got = f.http(t, f.admin, "POST", inventoryPath, inventoryImport, 200)
	if !strings.Contains(string(got), `"uid":"unique-real-printer"`) || strings.Contains(string(got), "synthetic-secret") {
		t.Fatalf("projected NamespaceList: %s", got)
	}
	reader := f.user(t, "real-kubectl-reader", ScopeGrant{Cluster: "dev", Instance: "local"})
	before := getTestInventory(t, f, reader)
	if len(before) != 1 || len(before[0].Namespaces) != 1 || before[0].Namespaces[0].PodsListedAt == "" || before[0].Namespaces[0].NamespaceObservedAt == "" {
		t.Fatalf("independent proof timestamps: %#v", before)
	}
	inventoryTestError(t, f.http(t, f.admin, "POST", inventoryPath, strings.TrimSuffix(inventoryImport, "}")+`,"namespace":"revoked"}`, 403), "inventory_forbidden")
	if !reflect.DeepEqual(before, getTestInventory(t, f, reader)) {
		t.Fatal("token revocation during Agent read changed stored evidence")
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"/api/v1/namespaces/reports/pods", "/api/v1/namespaces", "/api/v1/namespaces/revoked/pods"}
	if !reflect.DeepEqual(resourcePaths, want) {
		t.Fatalf("resource reads %v, want %v", resourcePaths, want)
	}
}
