package server

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/user/doops/agent/api"
)

func TestHelmDataOverrideAcceptsOrdinaryContent(t *testing.T) {
	t.Setenv("DOOPS_WORKSPACE_ROOT", t.TempDir())
	for _, value := range []string{
		"hello",
		"hello, ordinary business data",
		`{"LOG_LEVEL":"info","PORT":8080}`,
		"LOG_LEVEL: info\nPORT: 8080\n",
	} {
		t.Run(value, func(t *testing.T) {
			params := testHelmParams()
			params.SetStrings = map[string]api.HelmScalarValue{"data": api.HelmScalarValue(value)}
			command, err := buildHelmCommand(params)
			if err != nil {
				t.Fatalf("ordinary Chart data rejected: %v", err)
			}
			if !slices.Contains(command.args, "data="+escapeHelmSetString(value)) {
				t.Fatalf("Chart data was not preserved: %#v", command.args)
			}
		})
	}
	if !credentialSecretLikeKey("data") {
		t.Fatal("the Helm exception must not change global credential parsing")
	}
}

func TestHelmDataOverrideRejectsCredentialsBeforeExecution(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DOOPS_WORKSPACE_ROOT", root)
	writeWorkspaceHelmChart(t, root)
	bin := t.TempDir()
	started := filepath.Join(bin, "started")
	writeExecutable(t, filepath.Join(bin, "helm"), "#!/bin/sh\ntouch '"+started+"'\nexit 1\n")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	const canary = "synthetic-helm-private-canary"
	tests := []struct {
		name, key, value string
	}{
		{"password field", "password", canary},
		{"nested password field", "registry.password", canary},
		{"auth field", "auth", canary},
		{"PAT field", "githubPat", canary},
		{"private key field", "tls.privateKey", canary},
		{"JSON data", "data", `{"password":"` + canary + `"}`},
		{"nested JSON data", "data", `{"database":{"password":"` + canary + `"}}`},
		{"YAML data", "data", "database:\n  password: " + canary},
		{"assignment data", "data", "PASSWORD=" + canary},
		{"quoted assignment", "data", `{"password": "` + canary + ` with spaces"}`},
		{"malformed private JSON", "data", `{"credentials":{"password":"` + canary + `"`},
		{"bearer data", "data", "Authorization: Bearer " + canary},
		{"URL data", "data", "https://user:" + canary + "@example.test"},
		{"token data", "data", "ghp_" + strings.Repeat("a", 30)},
		{"private key data", "data", "-----BEGIN PRIVATE KEY-----\n" + canary + "\n-----END PRIVATE KEY-----"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			params := testWorkspaceHelmParams()
			params.SetStrings = map[string]api.HelmScalarValue{test.key: api.HelmScalarValue(test.value)}
			raw, err := json.Marshal(params)
			if err != nil {
				t.Fatal(err)
			}
			var output strings.Builder
			result, err := handleHelmTool(t.Context(), raw, func(event notificationEvent) {
				output.WriteString(event.Data)
			})
			if err == nil || !strings.Contains(err.Error(), "forbidden credential material") {
				t.Fatalf("credential input was not rejected: result=%q err=%v", result, err)
			}
			if strings.Contains(result+output.String()+err.Error(), canary) {
				t.Fatal("credential rejection disclosed the input")
			}
			if _, err := os.Stat(started); !os.IsNotExist(err) {
				t.Fatalf("Helm started before credential validation: %v", err)
			}
		})
	}
}

// A model naturally expresses chart values in their native JSON shape, such as
// `loki.useTestSchema: true`. Preserve that native type through Helm so schema
// validation and templates see a boolean rather than the string "true".
func TestHelmSetStringsAcceptsNativeScalarValues(t *testing.T) {
	t.Setenv("DOOPS_WORKSPACE_ROOT", t.TempDir())
	raw := []byte(`{"session_id":"release","operation":"upgrade-install","release":"loki",
		"namespace":"test","chart":"https://charts.example.test/loki-6.55.0.tgz",
		"set_strings":{"loki.useTestSchema":true,"loki.storage.bucketNames.chunks":"loki-chunks","gateway.replicas":2}}`)
	var params api.HelmParams
	if err := json.Unmarshal(raw, &params); err != nil {
		t.Fatalf("native Helm scalar value rejected: %v", err)
	}
	command, err := buildHelmCommand(params)
	if err != nil {
		t.Fatalf("buildHelmCommand: %v", err)
	}
	for _, wanted := range [][]string{
		{"--set-json", "loki.useTestSchema=true"},
		{"--set-string", "loki.storage.bucketNames.chunks=loki-chunks"},
		{"--set-json", "gateway.replicas=2"},
	} {
		found := false
		for index := 0; index+1 < len(command.args); index++ {
			if command.args[index] == wanted[0] && command.args[index+1] == wanted[1] {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("scalar override %q %q missing: %#v", wanted[0], wanted[1], command.args)
		}
	}
}

func TestHelmNativeScalarTypesRenderWithRealHelm(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DOOPS_WORKSPACE_ROOT", root)
	writeWorkspaceHelmChart(t, root)
	chart := filepath.Join(root, "release", "deploy", "helm", "prod")
	if err := os.MkdirAll(filepath.Join(chart, "templates"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(chart, "values.schema.json"), []byte(`{"type":"object","properties":{"enabled":{"type":"boolean"},"replicas":{"type":"integer"},"ratio":{"type":"number"},"exponent":{"type":"number"},"label":{"type":"string"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(chart, "templates", "scalars.yaml"), []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: scalar-types
data:
  enabled: {{ .Values.enabled | quote }}
  replicas: {{ .Values.replicas | quote }}
  ratio: {{ .Values.ratio | quote }}
  exponent: {{ .Values.exponent | quote }}
  label: {{ .Values.label | quote }}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	var params api.HelmParams
	bound, err := bindK8SMCPSession([]byte(`{"operation":"upgrade-install","release":"app","namespace":"apps","asset_source":"workspace","chart":"deploy/helm/prod","set_strings":{"enabled":false,"replicas":2,"ratio":0.0001,"exponent":1e2,"label":"false"}}`), "release")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(bound, &params); err != nil {
		t.Fatal(err)
	}
	command, err := buildHelmCommand(params)
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"template", command.release, command.args[2], "--namespace", command.namespace}
	for index := 3; index < len(command.args); index++ {
		if command.args[index] == "--set-json" || command.args[index] == "--set-string" {
			args = append(args, command.args[index], command.args[index+1])
			index++
		}
	}
	render := exec.CommandContext(t.Context(), "helm", args...)
	render.Dir = command.cwd
	manifest, err := render.CombinedOutput()
	if err != nil {
		t.Fatalf("native scalar schema validation failed: %v: %s", err, manifest)
	}
	for _, expected := range []string{`enabled: "false"`, `replicas: "2"`, `ratio: "0.0001"`, `exponent: "100"`, `label: "false"`} {
		if !strings.Contains(string(manifest), expected) {
			t.Fatalf("missing scalar %q: %s", expected, manifest)
		}
	}
}

func TestHelmSessionBindingPreservesNumericInput(t *testing.T) {
	bound, err := bindK8SMCPSession([]byte(`{"set_strings":{"exact":9007199254740993,"ratio":1.234567890123456789}}`), "release")
	if err != nil {
		t.Fatal(err)
	}
	var params api.HelmParams
	if err := json.Unmarshal(bound, &params); err != nil {
		t.Fatal(err)
	}
	if params.SetStrings["exact"] != "9007199254740993" || params.SetStrings["ratio"] != "1.234567890123456789" {
		t.Fatalf("input changed before reaching Helm: %s", bound)
	}
}

func TestHelmSetStringsRejectsStructuredValues(t *testing.T) {
	t.Setenv("DOOPS_WORKSPACE_ROOT", t.TempDir())
	for _, values := range []string{
		`"loki.schema":["a","b"]`,
		`"loki.schema":{"a":1}`,
		`"loki.schema":null`,
	} {
		raw := []byte(`{"session_id":"release","operation":"upgrade-install","release":"loki",` +
			`"namespace":"test","chart":"https://charts.example.test/loki.tgz","set_strings":{` + values + `}}`)
		var params api.HelmParams
		if err := json.Unmarshal(raw, &params); err == nil {
			t.Fatalf("structured Helm value was accepted: %s", raw)
		}
	}
}
