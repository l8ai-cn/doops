package server

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/user/doops/agent/api"
)

var testHelmDigest = "sha256:" + strings.Repeat("a", 64)

func testHelmParams() api.HelmParams {
	return api.HelmParams{
		SessionID:  "release",
		Operation:  "upgrade-install",
		Release:    "app",
		Chart:      "https://charts.example.test/app-1.0.0.tgz",
		Namespace:  "apps",
		ValueFiles: []string{"https://config.example.test/app-production.yaml"},
		SetStrings: map[string]api.HelmScalarValue{
			"image.repository": "registry.example/app",
			"image.digest":     api.HelmScalarValue(testHelmDigest),
		},
		Timeout: "180s",
	}
}

func testWorkspaceHelmParams() api.HelmParams {
	return api.HelmParams{
		SessionID:   "release",
		Operation:   "upgrade-install",
		Release:     "app",
		AssetSource: "workspace",
		Chart:       "deploy/helm/prod",
		Namespace:   "apps",
		ValueFiles:  []string{"deploy/helm/prod/values.yaml"},
		Timeout:     "180s",
	}
}

func writeWorkspaceHelmChart(t *testing.T, root string) {
	t.Helper()
	chart := filepath.Join(root, "release", "deploy", "helm", "prod")
	if err := os.MkdirAll(chart, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(chart, "Chart.yaml"), []byte("apiVersion: v2\nname: app\nversion: 1.0.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(chart, "values.yaml"), []byte("image:\n  repository: registry.example/app\n  digest: "+testHelmDigest+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBuildHelmUpgradeInstallUsesNativeValuesWithoutDoOpsPublicationGates(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DOOPS_WORKSPACE_ROOT", root)
	writeWorkspaceHelmChart(t, root)

	command, err := buildHelmCommand(testWorkspaceHelmParams())
	if err != nil {
		t.Fatalf("build workspace helm command: %v", err)
	}
	if command.cwd != filepath.Join(root, "release") {
		t.Fatalf("helm cwd = %q, want %q", command.cwd, filepath.Join(root, "release"))
	}
	if slices.Contains(command.args, "--reset-values") {
		t.Fatalf("DoOps must preserve Helm's native values semantics: %q", command.args)
	}
	for _, want := range []string{
		"upgrade", "app", "deploy/helm/prod", "--install",
		"--values", "deploy/helm/prod/values.yaml",
		"--namespace", "apps", "--wait", "--timeout", "180s", "--description",
	} {
		if !slices.Contains(command.args, want) {
			t.Fatalf("helm args missing %q: %#v", want, command.args)
		}
	}
	if command.mutationID == "" {
		t.Fatal("workspace upgrade must bind a mutation identity")
	}
}

func TestBuildHelmRollbackUsesSupportedHelmFlags(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DOOPS_WORKSPACE_ROOT", root)
	params := testHelmParams()
	params.Operation = "rollback"
	params.Chart = ""
	params.ValueFiles = nil
	params.SetStrings = nil
	params.Revision = 2

	command, err := buildHelmCommand(params)
	if err != nil {
		t.Fatalf("build rollback command: %v", err)
	}
	want := []string{"rollback", "app", "2", "--namespace", "apps", "--wait", "--timeout", "180s"}
	if !slices.Equal(command.args, want) {
		t.Fatalf("rollback args = %#v, want %#v", command.args, want)
	}
}

func TestHelmWorkspaceRootChartRendersWithRealHelm(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DOOPS_WORKSPACE_ROOT", root)
	workspace := filepath.Join(root, "release")
	for name, body := range map[string]string{
		"Chart.yaml":               "apiVersion: v2\nname: root-chart\nversion: 1.0.0\n",
		"values.yaml":              "message: default\n",
		"overlay.yaml":             "message: root-chart-accepted\n",
		"templates/configmap.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: root-chart\ndata:\n  message: {{ .Values.message | quote }}\n",
	} {
		path := filepath.Join(workspace, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, chart := range []string{".", "./"} {
		t.Run(chart, func(t *testing.T) {
			params := testWorkspaceHelmParams()
			params.Chart = chart
			params.ValueFiles = []string{"overlay.yaml"}
			command, err := buildHelmCommand(params)
			if err != nil {
				t.Fatal(err)
			}
			if command.cwd != workspace || command.args[2] != "." {
				t.Fatalf("root chart resolved incorrectly: cwd=%q args=%q", command.cwd, command.args)
			}
			args := []string{"template", command.release, command.args[2]}
			for index, arg := range command.args {
				if arg == "--values" {
					args = append(args, arg, command.args[index+1])
				}
			}
			render := exec.CommandContext(t.Context(), "helm", args...)
			render.Dir = command.cwd
			manifest, err := render.CombinedOutput()
			if err != nil || !strings.Contains(string(manifest), `message: "root-chart-accepted"`) {
				t.Fatalf("root chart rendering failed: %v: %s", err, manifest)
			}
		})
	}
}

func TestBuildHelmCommandRejectsOnlyTransportAndCredentialViolations(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(api.HelmParams, string) api.HelmParams
		wantError string
	}{
		{
			name: "values cannot be workspace directory",
			mutate: func(params api.HelmParams, _ string) api.HelmParams {
				params.ValueFiles = []string{"."}
				return params
			},
			wantError: "must be a regular file",
		},
		{
			name: "absolute chart path",
			mutate: func(params api.HelmParams, root string) api.HelmParams {
				params.Chart = filepath.Join(root, "release", "deploy", "helm", "prod")
				return params
			},
			wantError: "path must be relative",
		},
		{
			name: "parent traversal",
			mutate: func(params api.HelmParams, _ string) api.HelmParams {
				params.Chart = "deploy/helm/../prod"
				return params
			},
			wantError: "path traversal",
		},
		{
			name: "missing value overlay",
			mutate: func(params api.HelmParams, _ string) api.HelmParams {
				params.ValueFiles = []string{
					"deploy/helm/prod/values.yaml",
					"deploy/helm/prod/values-production.yaml",
				}
				return params
			},
			wantError: "does not exist",
		},
		{
			name: "credential override",
			mutate: func(params api.HelmParams, _ string) api.HelmParams {
				params.SetStrings = map[string]api.HelmScalarValue{"registry.password": "synthetic-secret"}
				return params
			},
			wantError: "forbidden credential material",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("DOOPS_WORKSPACE_ROOT", root)
			writeWorkspaceHelmChart(t, root)
			params := test.mutate(testWorkspaceHelmParams(), root)
			if _, err := buildHelmCommand(params); err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("error = %v, want %q", err, test.wantError)
			}
		})
	}
}

func TestBuildHelmCommandAcceptsChartSpecificValueKeys(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DOOPS_WORKSPACE_ROOT", root)
	params := testHelmParams()
	params.SetStrings = map[string]api.HelmScalarValue{
		"service[0].image": "registry.example/app:v1",
		"compat":           "true",
		"global.compat":    "enabled",
		"data":             "hello",
	}
	command, err := buildHelmCommand(params)
	if err != nil {
		t.Fatalf("chart-specific key was rejected: %v", err)
	}
	for key, value := range params.SetStrings {
		if !slices.Contains(command.args, key+"="+string(value)) {
			t.Fatalf("chart-specific override %s missing: %#v", key, command.args)
		}
	}
}

func TestBuildHelmCommandRejectsPATFieldsWithoutRejectingCompat(t *testing.T) {
	t.Setenv("DOOPS_WORKSPACE_ROOT", t.TempDir())
	for _, key := range []string{"pat", "PAT", "git.pat", "githubPat", "githubPAT", "github_pat", "githubpat", "gitlabpat"} {
		t.Run(key, func(t *testing.T) {
			params := testHelmParams()
			params.SetStrings = map[string]api.HelmScalarValue{key: "synthetic-secret"}
			if _, err := buildHelmCommand(params); err == nil || !strings.Contains(err.Error(), "forbidden credential material") {
				t.Fatalf("credential field %q was accepted: %v", key, err)
			}
		})
	}
}

func TestBuildHelmCommandRejectsLocalAssetsAndInlineSecrets(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DOOPS_WORKSPACE_ROOT", root)
	tests := []struct {
		name      string
		mutate    func(*api.HelmParams)
		wantError string
	}{
		{
			name: "local chart in remote mode",
			mutate: func(params *api.HelmParams) {
				params.Chart = "deploy/helm/app"
			},
			wantError: "explicit https://",
		},
		{
			name: "unencrypted chart",
			mutate: func(params *api.HelmParams) {
				params.Chart = "http://charts.example.test/app-1.0.0.tgz"
			},
			wantError: "explicit https://",
		},
		{
			name: "local values file",
			mutate: func(params *api.HelmParams) {
				params.ValueFiles = []string{"deploy/values/production.yaml"}
			},
			wantError: "explicit http(s)://",
		},
		{
			name: "credential URL",
			mutate: func(params *api.HelmParams) {
				params.SetStrings = map[string]api.HelmScalarValue{"upstream.url": "https://user:secret@example.test/api"}
			},
			wantError: "forbidden credential material",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			params := testHelmParams()
			test.mutate(&params)
			if _, err := buildHelmCommand(params); err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("error = %v, want %q", err, test.wantError)
			}
		})
	}
}

func TestHelmChartSpecificInputsRenderWithRealHelm(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DOOPS_WORKSPACE_ROOT", root)
	workspace := filepath.Join(root, "release")
	files := map[string]string{
		"charts/platform/Chart.yaml":  "apiVersion: v2\nname: platform\nversion: 1.0.0\n",
		"charts/platform/values.yaml": "services:\n  api:\n    image: registry.example/api:v1\n  web:\n    image: registry.example/web:v1\ncors:\n  origins: https://default.example\n",
		"config/base.yaml":            "services:\n  api:\n    image: registry.example/api@" + testHelmDigest + "\n",
		"config/prod.yaml":            "services:\n  web:\n    image: registry.example/web:git-b87ab17\n",
		"charts/platform/templates/configmap.yaml": `apiVersion: v1
kind: ConfigMap
metadata:
  name: platform-config
data:
  message: {{ .Values.data | quote }}
`,
		"charts/platform/templates/deployments.yaml": `{{- range $name, $service := .Values.services }}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ $name }}
spec:
  template:
    spec:
      containers:
        - name: {{ $name }}
          image: {{ $service.image | quote }}
          env:
            - name: CORS_ORIGINS
              value: {{ $.Values.cors.origins | quote }}
{{- end }}
`,
	}
	for path, body := range files {
		fullPath := filepath.Join(workspace, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fullPath, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	params := testWorkspaceHelmParams()
	params.Chart = "charts/platform"
	params.ValueFiles = []string{"config/base.yaml", "config/prod.yaml"}
	params.SetStrings = map[string]api.HelmScalarValue{
		"cors.origins": "https://hdu.example,https://unicom.example\nhttps://third.example",
		"data":         "hello, ordinary business data",
	}
	command, err := buildHelmCommand(params)
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"template", command.release, command.args[2], "--namespace", command.namespace}
	for index := 3; index < len(command.args); index++ {
		if command.args[index] == "--values" || command.args[index] == "--set-string" {
			args = append(args, command.args[index], command.args[index+1])
			index++
		}
	}
	render := exec.CommandContext(t.Context(), "helm", args...)
	render.Dir = command.cwd
	manifest, err := render.CombinedOutput()
	if err != nil {
		t.Fatalf("real Helm rendering failed: %v: %s", err, manifest)
	}
	for _, expected := range []string{
		"registry.example/api@" + testHelmDigest,
		"registry.example/web:git-b87ab17",
		"https://hdu.example,https://unicom.example",
		`\nhttps://third.example`,
		`message: "hello, ordinary business data"`,
	} {
		if !strings.Contains(string(manifest), expected) {
			t.Fatalf("chart-specific input %q did not render: %s", expected, manifest)
		}
	}
}

func TestHelmTimeoutUsesPositiveDurationWithinCallBudget(t *testing.T) {
	t.Setenv("DOOPS_WORKSPACE_ROOT", t.TempDir())
	for _, test := range []struct {
		timeout string
		valid   bool
	}{
		{"", true}, {"1m30s", true}, {"10m", true}, {"600s", true}, {"1ms", true},
		{"25m", true}, {"0", false}, {"0s", false}, {"-1s", false}, {"25m1s", false}, {"1h", false}, {"forever", false},
	} {
		t.Run(test.timeout, func(t *testing.T) {
			params := testHelmParams()
			params.Timeout = test.timeout
			command, err := buildHelmCommand(params)
			if (err == nil) != test.valid {
				t.Fatalf("timeout %q: %v", test.timeout, err)
			}
			if test.valid && test.timeout != "" && !slices.Contains(command.args, test.timeout) {
				t.Fatalf("duration was not passed through to Helm: %#v", command.args)
			}
		})
	}
}

func TestHelmRemoteReferenceSchemes(t *testing.T) {
	if _, err := resolveRemoteHelmChartReference("https://charts.example.test/app.tgz"); err != nil {
		t.Fatalf("allowed chart was rejected: %v", err)
	}
	if _, err := resolveRemoteHelmChartReference("oci://registry.example.test/charts/app"); err == nil ||
		!strings.Contains(err.Error(), "explicit https://") {
		t.Fatalf("OCI chart was not rejected at the input boundary: %v", err)
	}
	if _, err := resolveRemoteHelmValueReference("http://config.example.test/values.yaml"); err != nil {
		t.Fatalf("values must retain their separate HTTP(S) contract: %v", err)
	}
}

func TestBuildHelmUpgradeInstallAtomicAndOwnershipAreCallerOptions(t *testing.T) {
	for _, tc := range []struct {
		name          string
		atomic        bool
		takeOwnership bool
		wantAtomic    bool
		wantOwnership bool
	}{
		{"defaults", false, false, false, false},
		{"explicit options", true, true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("DOOPS_WORKSPACE_ROOT", root)
			params := testHelmParams()
			params.Atomic = tc.atomic
			params.TakeOwnership = tc.takeOwnership
			command, err := buildHelmCommand(params)
			if err != nil {
				t.Fatal(err)
			}
			if slices.Contains(command.args, "--atomic") != tc.wantAtomic {
				t.Fatalf("--atomic mismatch: %q", command.args)
			}
			if slices.Contains(command.args, "--take-ownership") != tc.wantOwnership {
				t.Fatalf("--take-ownership mismatch: %q", command.args)
			}
		})
	}
}
