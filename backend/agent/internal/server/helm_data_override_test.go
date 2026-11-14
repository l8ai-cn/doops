package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
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
			params.SetStrings = map[string]string{"data": value}
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
			params.SetStrings = map[string]string{test.key: test.value}
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
