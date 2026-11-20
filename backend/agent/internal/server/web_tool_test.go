package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/user/doops/agent/api"
)

func TestWebReadCannotMutateOrChooseSession(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want string
	}{
		{`{"name":"web_read","arguments":{"operation":"deploy","site_id":"demo"}}`, "web_read tool requires a supported read operation"},
		{`{"name":"web_read","arguments":{"operation":"check","site_id":"demo","session_id":"other"}}`, "session_id is runtime-bound and must not be supplied"},
		{`{"name":"web","arguments":{"operation":"deploy","site_id":"demo","session_id":"other"}}`, "session_id is runtime-bound and must not be supplied"},
		{`{"name":"web","arguments":{"operation":"unsupported","site_id":"demo"}}`, "web tool requires a supported mutation"},
	} {
		response := callK8SMCPTool(t.Context(), k8sMCPRequest{ID: 1, Params: json.RawMessage(tc.raw)}, "web-test")
		if response.Error == nil || response.Error.Code != -32602 || !strings.Contains(response.Error.Message, tc.want) {
			t.Fatalf("invalid request did not return %q: %#v", tc.want, response)
		}
	}
}

func TestWebMCPMutationsExecuteWithoutConfirm(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DOOPS_WORKSPACE_ROOT", root)
	if err := os.Mkdir(filepath.Join(root, "web-test"), 0755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(root, "executor.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOWEB_NODE", "/bin/sh")
	t.Setenv("DOWEB_SCRIPT", script)
	for _, operation := range webMutationOperations {
		t.Run(operation, func(t *testing.T) {
			raw, _ := json.Marshal(map[string]interface{}{
				"name": "web",
				"arguments": map[string]interface{}{
					"operation": operation, "site_id": "demo", "runtime_mode": "host",
				},
			})
			response := callK8SMCPTool(t.Context(), k8sMCPRequest{ID: 1, Params: raw}, "web-test")
			result, ok := response.Result.(map[string]interface{})
			if response.Error != nil || !ok || result["isError"] != false ||
				!strings.Contains(fmt.Sprint(result["content"]), operation+" --site-id demo") {
				t.Fatalf("explicit web mutation did not execute: %#v", response)
			}
		})
	}
}

func TestK8SAndWebRejectRemovedConfirmField(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(context.Context, json.RawMessage, notificationSender) (string, error)
		raw  string
	}{
		{"k8s", handleK8STool, `{"session_id":"test","operation":"cordon","name":"node-a","confirm":true}`},
		{"web", handleWebTool, `{"session_id":"test","operation":"deploy","site_id":"demo","confirm":true}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.call(t.Context(), json.RawMessage(tc.raw), func(notificationEvent) {})
			if err == nil || !strings.Contains(err.Error(), `unknown field "confirm"`) {
				t.Fatalf("removed confirm field accepted: %v", err)
			}
		})
	}
}

func TestWebInputConfinedToSession(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DOOPS_WORKSPACE_ROOT", root)
	workspace := filepath.Join(root, "web-test")
	if err := os.MkdirAll(filepath.Join(workspace, "dist"), 0755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(workspace, "escape")); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"..", "dist/..", "../other", outside, "escape"} {
		if _, err := resolveWebWorkspaceInput("web-test", value, "dist"); err == nil {
			t.Fatalf("input escaped workspace: %q", value)
		}
	}
	expected, _ := filepath.EvalSymlinks(filepath.Join(workspace, "dist"))
	if got, err := resolveWebWorkspaceInput("web-test", "dist", "dist"); err != nil || got != expected {
		t.Fatalf("valid workspace asset rejected: %s, %v", got, err)
	}
	if _, err := resolveWebSession("missing"); err == nil {
		t.Fatal("missing session silently changed workspace")
	}
}

func TestWebSiteListBeforeFirstSessionWorkspace(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspaces")
	t.Setenv("DOOPS_WORKSPACE_ROOT", workspace)
	script, err := filepath.Abs("../../skills/doweb/scripts/doweb.mjs")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOWEB_SCRIPT", script)
	layout := filepath.Join(root, "runtime.json")
	config, err := json.Marshal(map[string]string{"base-dir": filepath.Join(root, "published-sites")})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout, config, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOWEB_RUNTIME_CONFIG", layout)
	result, err := handleWebTool(t.Context(), json.RawMessage(`{"operation":"site-list","session_id":"first-visit"}`), func(notificationEvent) {})
	if err != nil || result != `{"sites":[]}` {
		t.Fatalf("first visit: %q, %v", result, err)
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatalf("unexpected workspace access: %v", err)
	}
}

func TestWebExecutionUsesRealNginxValidationHealthAndRollback(t *testing.T) {
	nginx, err := exec.LookPath("nginx")
	if err != nil {
		t.Skip("real Nginx is required for DoWeb functional validation")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("Node is required for bundled DoWeb execution")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(root, "workspaces", "web-functional")
	available, enabled, snippets := filepath.Join(root, "available"), filepath.Join(root, "enabled"), filepath.Join(root, "snippets")
	for _, path := range []string{workspace, available, enabled, snippets, filepath.Join(root, "logs")} {
		if err := os.MkdirAll(path, 0755); err != nil {
			t.Fatal(err)
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	config := filepath.Join(root, "nginx.conf")
	user := ""
	if os.Geteuid() == 0 {
		user = "user root;\n"
	}
	configBody := fmt.Sprintf("%spid %s/nginx.pid;\nerror_log %s/logs/error.log;\nevents {}\nhttp { access_log off; include %s/*.conf; }\n", user, root, root, enabled)
	if err := os.WriteFile(config, []byte(configBody), 0600); err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(root, "nginx-wrapper")
	reloadFailure := filepath.Join(root, "reject-reload")
	if err := os.WriteFile(wrapper, []byte(fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = '-s' ] && [ -f '%s' ]; then exit 7; fi\nexec '%s' -p '%s' -c '%s' \"$@\"\n", reloadFailure, nginx, root, config)), 0700); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command(wrapper).CombinedOutput(); err != nil {
		t.Fatalf("start disposable Nginx: %v: %s", err, output)
	}
	t.Cleanup(func() {
		if output, err := exec.Command(wrapper, "-s", "quit").CombinedOutput(); err != nil {
			t.Errorf("stop disposable Nginx: %v: %s", err, output)
		}
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(filepath.Join(root, "nginx.pid")); os.IsNotExist(err) {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Error("disposable Nginx did not stop")
	})
	layout := map[string]interface{}{
		"base-dir": filepath.Join(root, "state"), "nginx-available-dir": available,
		"nginx-enabled-dir": enabled, "nginx-snippet-dir": snippets,
		"nginx-bin": wrapper, "reload-argv": []string{wrapper, "-s", "reload"}, "listen-port": port,
		"listen-address": "127.0.0.1",
	}
	layoutPath := filepath.Join(root, "runtime.json")
	encoded, _ := json.Marshal(layout)
	if err := os.WriteFile(layoutPath, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs("../../skills/doweb/scripts/doweb.mjs")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOWEB_SCRIPT", script)
	t.Setenv("DOWEB_NODE", node)
	t.Setenv("DOWEB_RUNTIME_CONFIG", layoutPath)
	t.Setenv("DOOPS_WORKSPACE_ROOT", filepath.Join(root, "workspaces"))
	url := fmt.Sprintf("http://127.0.0.1:%d/", port)
	writeDist := func(name, body string) {
		path := filepath.Join(workspace, name)
		if err := os.MkdirAll(path, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "index.html"), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	call := func(params api.WebParams) (string, error) {
		params.SessionID = "web-functional"
		raw, _ := json.Marshal(params)
		return handleWebTool(context.Background(), raw, func(notificationEvent) {})
	}
	listSites := func() []map[string]interface{} {
		t.Helper()
		result, err := call(api.WebParams{Operation: "site-list"})
		var inventory struct {
			Sites []map[string]interface{} `json:"sites"`
		}
		if err != nil || json.Unmarshal([]byte(result), &inventory) != nil || inventory.Sites == nil {
			t.Fatalf("list published sites: %s: %v", result, err)
		}
		return inventory.Sites
	}
	if sites := listSites(); len(sites) != 0 {
		t.Fatalf("unpublished target has sites: %v", sites)
	}
	assertBody := func(expected string) {
		deadline := time.Now().Add(3 * time.Second)
		client := &http.Client{Timeout: time.Second}
		for time.Now().Before(deadline) {
			response, err := client.Get(url)
			if err == nil {
				body, _ := io.ReadAll(response.Body)
				response.Body.Close()
				if response.StatusCode == 200 && string(body) == expected {
					return
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("Nginx did not serve expected artifact: %q", expected)
	}
	writeDist("dist-a", "release-a")
	result, err := call(api.WebParams{Operation: "deploy", SiteID: "demo", Domain: "localhost", Dist: "dist-a", HealthURLs: []string{url}})
	if err != nil || !strings.Contains(result, "validation") || !strings.Contains(result, "deployed") {
		t.Fatalf("first publication: %s: %v", result, err)
	}
	sites := listSites()
	if len(sites) != 1 || sites[0]["site_id"] != "demo" || sites[0]["domain"] != "localhost" || sites[0]["previous_release"] != nil || sites[0]["current_release"] == nil {
		t.Fatalf("first publication missing from inventory: %v", sites)
	}
	configText, err := call(api.WebParams{Operation: "config-get", SiteID: "demo"})
	if err != nil || !strings.Contains(configText, fmt.Sprintf("listen 127.0.0.1:%d;", port)) {
		t.Fatalf("loopback binding missing: %s: %v", configText, err)
	}
	firstRelease := sites[0]["current_release"]
	assertBody("release-a")
	writeDist("dist-b", "release-b")
	statePath := filepath.Join(root, "state", "state", "demo.json")
	previousState, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	evidenceDir := filepath.Join(root, "state", "evidence", "demo")
	if err := os.Rename(evidenceDir, evidenceDir+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(evidenceDir, []byte("synthetic evidence write failure"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := call(api.WebParams{Operation: "deploy", SiteID: "demo", Domain: "localhost", Dist: "dist-b", HealthURLs: []string{url}}); err == nil {
		t.Fatal("evidence persistence failure became success")
	}
	assertBody("release-a")
	restoredState, err := os.ReadFile(statePath)
	if err != nil || string(restoredState) != string(previousState) {
		t.Fatalf("failed publication changed the release index: %s: %v", restoredState, err)
	}
	if err := os.Remove(evidenceDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(evidenceDir+".saved", evidenceDir); err != nil {
		t.Fatal(err)
	}
	if _, err := call(api.WebParams{Operation: "deploy", SiteID: "demo", Domain: "localhost", Dist: "dist-b", HealthURLs: []string{url}}); err != nil {
		t.Fatal(err)
	}
	assertBody("release-b")
	writeDist("dist-c", "release-c")
	if _, err := call(api.WebParams{Operation: "deploy", SiteID: "demo", Domain: "new.example.test", Dist: "dist-c", HealthURLs: []string{url}}); err != nil {
		t.Fatal(err)
	}
	assertBody("release-c")
	if _, err := call(api.WebParams{Operation: "rollback", SiteID: "demo", HealthURLs: []string{url}}); err != nil {
		t.Fatal(err)
	}
	assertBody("release-b")
	if _, err := call(api.WebParams{Operation: "rollback", SiteID: "demo", HealthURLs: []string{url}}); err != nil {
		t.Fatal(err)
	}
	restored := listSites()
	if len(restored) != 1 || restored[0]["current_release"] != firstRelease || restored[0]["domain"] != "localhost" || restored[0]["previous_release"] != nil {
		t.Fatalf("inventory did not follow rollback: %v", restored)
	}
	assertBody("release-a")
	if _, err := call(api.WebParams{Operation: "rollback", SiteID: "demo", HealthURLs: []string{url}}); err == nil {
		t.Fatal("rollback from oldest release selected a newer release")
	}
	assertBody("release-a")
	failedHealth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "synthetic unhealthy site", http.StatusServiceUnavailable)
	}))
	defer failedHealth.Close()
	if _, err := call(api.WebParams{Operation: "deploy", SiteID: "demo", Domain: "localhost", Dist: "dist-b", HealthURLs: []string{failedHealth.URL}}); err == nil {
		t.Fatal("failed HTTP health became success")
	}
	assertBody("release-a")
	badConfig := filepath.Join(workspace, "invalid.conf")
	if err := os.WriteFile(badConfig, []byte("server { this_is_not_an_nginx_directive; }"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := call(api.WebParams{Operation: "config-set", SiteID: "demo", ConfigFile: "invalid.conf"}); err == nil {
		t.Fatal("invalid Nginx config became success")
	}
	assertBody("release-a")
	if _, err := call(api.WebParams{Operation: "check", SiteID: "demo", HealthURLs: []string{url}}); err != nil {
		t.Fatalf("site did not recover: %v", err)
	}
	if _, err := call(api.WebParams{Operation: "snippet-set", SiteID: "demo", SnippetName: "upload", Snippet: "client_max_body_size 20m;", HealthURLs: []string{url}}); err != nil {
		t.Fatalf("natural-language snippet could not be written: %v", err)
	}
	snippetBody, err := os.ReadFile(filepath.Join(snippets, "demo", "upload.conf"))
	if err != nil || string(snippetBody) != "client_max_body_size 20m;" {
		t.Fatalf("candidate snippet was not persisted: %s: %v", snippetBody, err)
	}
	if _, err := call(api.WebParams{Operation: "snippet-set", SiteID: "demo", SnippetName: "upload", Snippet: "client_max_body_size 50m;", HealthURLs: []string{failedHealth.URL}}); err == nil {
		t.Fatal("snippet update ignored failed HTTP health")
	}
	snippetBody, err = os.ReadFile(filepath.Join(snippets, "demo", "upload.conf"))
	if err != nil || string(snippetBody) != "client_max_body_size 20m;" {
		t.Fatalf("failed snippet update was not restored: %s: %v", snippetBody, err)
	}
	activeConfig, err := os.ReadFile(filepath.Join(available, "doweb-demo.conf"))
	if err != nil {
		t.Fatal(err)
	}
	include := "  include " + filepath.Join(snippets, "demo", "*.conf") + ";\n"
	withoutSnippets := strings.Replace(string(activeConfig), include, "", 1)
	if withoutSnippets == string(activeConfig) {
		t.Fatal("managed snippet include missing from functional fixture")
	}
	if _, err := call(api.WebParams{Operation: "config-set", SiteID: "demo", Config: withoutSnippets, HealthURLs: []string{url}}); err != nil {
		t.Fatal(err)
	}
	if _, err := call(api.WebParams{Operation: "snippet-set", SiteID: "demo", SnippetName: "upload", Snippet: "client_max_body_size 50m;", HealthURLs: []string{url}}); err == nil || !strings.Contains(err.Error(), "does not include the managed snippet") {
		t.Fatalf("unreferenced snippet reported success: %v", err)
	}
	if _, err := call(api.WebParams{Operation: "config-set", SiteID: "demo", Config: string(activeConfig), HealthURLs: []string{url}}); err != nil {
		t.Fatal(err)
	}
	assertBody("release-a")
	if err := os.WriteFile(reloadFailure, []byte("synthetic reload failure"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = call(api.WebParams{Operation: "deploy", SiteID: "demo", Domain: "localhost", Dist: "dist-b", HealthURLs: []string{url}})
	if !errors.Is(err, errExecutionOutcomeUnknown) {
		t.Fatalf("failed recovery did not report unknown outcome: %v", err)
	}
	if err := os.Remove(reloadFailure); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command(wrapper, "-s", "reload").CombinedOutput(); err != nil {
		t.Fatalf("restore disposable runtime after failure injection: %v: %s", err, output)
	}
	assertBody("release-a")
}

func TestApplyWebUsesSignedToolEvidenceAndCannotHideFailure(t *testing.T) {
	collector := newDoagentToolTraceCollector(false)
	bindApplyCollector(collector, "apply-test")
	input := map[string]interface{}{"operation": "deploy", "site_id": "demo", "dist": "dist"}
	trace := attestedMutationTrace(t, "mcp_doops_web", input)
	collector.calls["publish"] = trace
	if err := collector.requireCompletedToolEvidence(); err != nil {
		t.Fatalf("signed web publication rejected: %v", err)
	}
	if collector.hasUnresolvedMutation() {
		t.Fatal("completed web mutation marked unknown")
	}
	collector.calls["unfinished"] = doagentToolTrace{ToolName: "mcp_doops_web", Status: "running"}
	if !collector.hasUnresolvedMutation() {
		t.Fatal("unfinished website mutation disappeared")
	}
}

func TestApplyWebWithoutConfirmStillRequiresMutationAuthority(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*doagentToolTrace)
		reason string
	}{
		{"missing authority", func(trace *doagentToolTrace) { trace.MutationAuthorized = nil }, "mutation_not_authorized"},
		{"denied authority", func(trace *doagentToolTrace) { trace.MutationAuthorized = boolPointer(false) }, "mutation_not_authorized"},
		{"wrong authority", func(trace *doagentToolTrace) { trace.MutationAuthorityDigest = "wrong" }, "mutation_authority_binding_mismatch"},
		{"read-only mode", func(trace *doagentToolTrace) { trace.ExecutionMode = "ask" }, "execution_mode_invalid"},
		{"forged result", func(trace *doagentToolTrace) { trace.AttestedResultDigest = "wrong" }, "public_result_invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			collector := newDoagentToolTraceCollector(false)
			bindApplyCollector(collector, "apply-test")
			trace := attestedMutationTrace(t, "mcp_doops_web", map[string]interface{}{"operation": "deploy", "site_id": "demo"})
			tc.mutate(&trace)
			collector.calls["publish"] = trace
			if err := collector.requireCompletedToolEvidence(); err == nil || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("invalid web evidence accepted: %v, want %s", err, tc.reason)
			}
		})
	}
}
