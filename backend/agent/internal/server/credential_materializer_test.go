package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMaterializeRegistryCredentialUsesStdinAndVerifiesManifest(t *testing.T) {
	allowCredentialPrivateEndpointsForTest(t)
	bin, argsLog := installCredentialTestCommands(t)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DOOPS_TEST_SECRET_TYPE", "kubernetes.io/dockerconfigjson")
	t.Setenv("DOOPS_TEST_SECRET_KEYS", ".dockerconfigjson")
	t.Setenv("DOOPS_TEST_GIT_SECRET", "")

	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/team/app/manifests/latest" {
			http.NotFound(w, r)
			return
		}
		username, password, ok := r.BasicAuth()
		if !ok || username != "deploy" || password != "materializer-canary-secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Docker-Content-Digest", "sha256:manifest")
		w.WriteHeader(http.StatusOK)
	}))
	defer registry.Close()

	result, err := materializeCredential(context.Background(), CredentialMaterializeRequest{
		CredentialID:       "cred_registry",
		VersionID:          "ver_registry",
		CredentialType:     CredentialTypeRegistry,
		Use:                CredentialUseImagePull,
		Namespace:          "kz-ops",
		Workload:           CredentialPlanWorkload{Kind: "Deployment", Name: "doops-agent-live"},
		RegistryRepository: "team/app",
		RegistryReference:  "latest",
		Payload: json.RawMessage(fmt.Sprintf(
			`{"server":%q,"username":"deploy","password":"materializer-canary-secret"}`,
			registry.URL,
		)),
	})
	if err != nil {
		t.Fatalf("materialize registry credential: %v", err)
	}
	if result.Status != "verified" ||
		result.SecretType != "kubernetes.io/dockerconfigjson" ||
		len(result.Keys) != 1 || result.Keys[0] != ".dockerconfigjson" ||
		result.ResourceVersion != "42" ||
		result.Digest != "sha256:manifest" {
		t.Fatalf("unexpected registry materialization: %#v", result)
	}
	assertCredentialCanaryNotInText(t, "materializer result", string(mustJSON(t, result)), "materializer-canary-secret")
	assertCredentialCanaryNotInText(t, "command arguments", readCredentialTestFile(t, argsLog), "materializer-canary-secret")
}

func TestMaterializeTLSCredentialValidatesKeyPair(t *testing.T) {
	bin, argsLog := installCredentialTestCommands(t)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DOOPS_TEST_SECRET_TYPE", "kubernetes.io/tls")
	t.Setenv("DOOPS_TEST_SECRET_KEYS", "tls.crt,tls.key")
	certificate, privateKey := credentialTestCertificate(t)

	result, err := materializeCredential(context.Background(), CredentialMaterializeRequest{
		CredentialID:   "cred_tls",
		VersionID:      "ver_tls",
		CredentialType: CredentialTypeTLS,
		Use:            CredentialUseTLS,
		Namespace:      "kz-ops",
		Payload: mustJSON(t, map[string]string{
			"certificate": certificate,
			"privateKey":  privateKey,
		}),
	})
	if err != nil {
		t.Fatalf("materialize TLS credential: %v", err)
	}
	if result.Status != "verified" ||
		result.SecretType != "kubernetes.io/tls" ||
		result.Fingerprint == "" ||
		result.ExpiresAt == "" {
		t.Fatalf("unexpected TLS materialization: %#v", result)
	}
	assertCredentialCanaryNotInText(t, "command arguments", readCredentialTestFile(t, argsLog), privateKey)

	_, wrongKey := credentialTestCertificate(t)
	_, err = materializeCredential(context.Background(), CredentialMaterializeRequest{
		CredentialID:   "cred_tls_bad",
		VersionID:      "ver_tls_bad",
		CredentialType: CredentialTypeTLS,
		Use:            CredentialUseTLS,
		Namespace:      "kz-ops",
		Payload: mustJSON(t, map[string]string{
			"certificate": certificate,
			"privateKey":  wrongKey,
		}),
	})
	if err == nil || !strings.Contains(err.Error(), "tls_key_pair_invalid") {
		t.Fatalf("mismatched TLS key error = %v", err)
	}
	assertCredentialCanaryNotInText(t, "TLS error", err.Error(), wrongKey)
}

func TestMaterializeOpaqueCredentialRequiresExactKeySet(t *testing.T) {
	bin, _ := installCredentialTestCommands(t)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DOOPS_TEST_SECRET_TYPE", "Opaque")
	t.Setenv("DOOPS_TEST_SECRET_KEYS", "API_KEY,ENDPOINT")

	result, err := materializeCredential(context.Background(), CredentialMaterializeRequest{
		CredentialID:   "cred_opaque",
		VersionID:      "ver_opaque",
		CredentialType: CredentialTypeOpaque,
		Use:            CredentialUseOpaqueSecret,
		Namespace:      "app",
		RequiredKeys:   []string{"API_KEY", "ENDPOINT"},
		Payload:        json.RawMessage(`{"data":{"API_KEY":"opaque-canary-secret","ENDPOINT":"https://service.example.com"}}`),
	})
	if err != nil {
		t.Fatalf("materialize opaque credential: %v", err)
	}
	if result.Status != "verified" || strings.Join(result.Keys, ",") != "API_KEY,ENDPOINT" {
		t.Fatalf("unexpected opaque materialization: %#v", result)
	}

	_, err = materializeCredential(context.Background(), CredentialMaterializeRequest{
		CredentialID:   "cred_opaque_bad",
		VersionID:      "ver_opaque_bad",
		CredentialType: CredentialTypeOpaque,
		Use:            CredentialUseOpaqueSecret,
		Namespace:      "app",
		RequiredKeys:   []string{"API_KEY", "ENDPOINT"},
		Payload:        json.RawMessage(`{"data":{"API_KEY":"opaque-canary-secret","EXTRA":"not-allowed"}}`),
	})
	if err == nil || !strings.Contains(err.Error(), "credential_key_mismatch") {
		t.Fatalf("opaque key mismatch error = %v", err)
	}
	assertCredentialCanaryNotInText(t, "opaque error", err.Error(), "opaque-canary-secret")
}

func TestMaterializeHelmAndGitCredentialsVerifyRemoteWithoutLeaking(t *testing.T) {
	allowCredentialPrivateEndpointsForTest(t)
	bin, argsLog := installCredentialTestCommands(t)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DOOPS_TEST_SECRET_TYPE", "Opaque")
	t.Setenv("DOOPS_TEST_SECRET_KEYS", "password,url,username")
	t.Setenv("DOOPS_TEST_GIT_SECRET", "git-canary-secret")

	helmRepo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/index.yaml" {
			http.NotFound(w, r)
			return
		}
		username, password, ok := r.BasicAuth()
		if !ok || username != "helm" || password != "helm-canary-secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write([]byte("apiVersion: v1\nentries: {}\n"))
	}))
	defer helmRepo.Close()

	helmResult, err := materializeCredential(context.Background(), CredentialMaterializeRequest{
		CredentialID:   "cred_helm",
		VersionID:      "ver_helm",
		CredentialType: CredentialTypeHelmRepository,
		Use:            CredentialUseHelmPull,
		Namespace:      "kz-ops",
		Payload: mustJSON(t, map[string]string{
			"url":      helmRepo.URL,
			"username": "helm",
			"password": "helm-canary-secret",
		}),
	})
	if err != nil {
		t.Fatalf("materialize Helm credential: %v", err)
	}
	if helmResult.Status != "verified" || helmResult.Digest == "" {
		t.Fatalf("unexpected Helm materialization: %#v", helmResult)
	}

	gitResult, err := materializeCredential(context.Background(), CredentialMaterializeRequest{
		CredentialID:   "cred_git",
		VersionID:      "ver_git",
		CredentialType: CredentialTypeGitToken,
		Use:            CredentialUseGitCheckout,
		Namespace:      "kz-ops",
		Payload:        json.RawMessage(`{"url":"https://127.0.0.1/repo.git","username":"git","token":"git-canary-secret"}`),
	})
	if err != nil {
		t.Fatalf("materialize Git credential: %v", err)
	}
	if gitResult.Status != "verified" || gitResult.Digest == "" {
		t.Fatalf("unexpected Git materialization: %#v", gitResult)
	}
	serialized := string(mustJSON(t, []CredentialMaterialization{helmResult, gitResult}))
	assertCredentialCanaryNotInText(t, "Helm/Git results", serialized, "helm-canary-secret")
	assertCredentialCanaryNotInText(t, "Helm/Git results", serialized, "git-canary-secret")
	args := readCredentialTestFile(t, argsLog)
	assertCredentialCanaryNotInText(t, "Helm/Git command arguments", args, "helm-canary-secret")
	assertCredentialCanaryNotInText(t, "Helm/Git command arguments", args, "git-canary-secret")
}

func TestCleanupCredentialDeletesOnlyMatchingDoOpsOwnedSecret(t *testing.T) {
	bin := t.TempDir()
	logPath := filepath.Join(bin, "kubectl.log")
	kubectl := `#!/bin/sh
printf '%s\n' "$*" >> "$DOOPS_TEST_ARGS_LOG"
if [ "$1" = "get" ] && [ "$2" = "secret" ]; then
  printf '%s\n' "$DOOPS_TEST_SECRET_JSON"
elif [ "$1" = "get" ]; then
  printf '{"spec":{"template":{"spec":{"imagePullSecrets":[{"name":"existing-pull"},{"name":"doops-cred-PLACEHOLDER"}]}}}}\n'
elif [ "$1" = "patch" ]; then
  cat >/dev/null
fi
`
	if err := os.WriteFile(filepath.Join(bin, "kubectl"), []byte(kubectl), 0755); err != nil {
		t.Fatalf("write fake kubectl: %v", err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DOOPS_TEST_ARGS_LOG", logPath)
	credentialID := "cred_cleanup"
	versionID := "credver_cleanup"
	t.Setenv("DOOPS_TEST_SECRET_JSON", fmt.Sprintf(
		`{"metadata":{"labels":{"app.kubernetes.io/managed-by":"doops","doops.sh/credential-id":%q},"annotations":{"doops.sh/credential-version":%q}}}`,
		credentialID, versionID,
	))
	if err := cleanupCredential(context.Background(), CredentialCleanupRequest{
		SessionID:    "cleanup-session",
		CredentialID: credentialID,
		VersionID:    versionID,
		Namespace:    "apps",
		Workload:     CredentialPlanWorkload{Kind: "Deployment", Name: "app"},
	}); err != nil {
		t.Fatalf("cleanup credential: %v", err)
	}
	logged := readCredentialTestFile(t, logPath)
	if !strings.Contains(logged, "delete secret "+credentialSecretName(credentialID)) {
		t.Fatalf("cleanup did not delete the owned Secret: %s", logged)
	}

	t.Setenv("DOOPS_TEST_SECRET_JSON", fmt.Sprintf(
		`{"metadata":{"labels":{"app.kubernetes.io/managed-by":"doops","doops.sh/credential-id":%q},"annotations":{"doops.sh/credential-version":"other"}}}`,
		credentialID,
	))
	if err := cleanupCredential(context.Background(), CredentialCleanupRequest{
		SessionID:    "cleanup-session",
		CredentialID: credentialID,
		VersionID:    versionID,
		Namespace:    "apps",
	}); err == nil || !strings.Contains(err.Error(), "ownership_mismatch") {
		t.Fatalf("mismatched ownership cleanup error = %v", err)
	}
}

func installCredentialTestCommands(t *testing.T) (string, string) {
	t.Helper()
	bin := t.TempDir()
	argsLog := filepath.Join(bin, "args.log")
	kubectl := `#!/bin/sh
printf 'kubectl' >> "$DOOPS_TEST_ARGS_LOG"
for arg in "$@"; do printf ' [%s]' "$arg" >> "$DOOPS_TEST_ARGS_LOG"; done
printf '\n' >> "$DOOPS_TEST_ARGS_LOG"
case "$1" in
  apply|patch|replace) cat >/dev/null ;;
  delete) ;;
  get)
    if [ "$2" = "secret" ]; then
      for arg in "$@"; do
        if [ "$arg" = "--ignore-not-found" ]; then
          exit 0
        fi
      done
      printf '%s\n' "$DOOPS_TEST_SECRET_TYPE"
      printf '42\n'
      printf '%s\n' "$DOOPS_TEST_SECRET_KEYS"
    else
      printf '{"spec":{"template":{"spec":{"imagePullSecrets":[{"name":"existing-pull"}]}}}}\n'
    fi
    ;;
esac
`
	git := `#!/bin/sh
printf 'git' >> "$DOOPS_TEST_ARGS_LOG"
for arg in "$@"; do printf ' [%s]' "$arg" >> "$DOOPS_TEST_ARGS_LOG"; done
printf '\n' >> "$DOOPS_TEST_ARGS_LOG"
test -n "$GIT_ASKPASS"
test -x "$GIT_ASKPASS"
test "$DOOPS_GIT_SECRET" = "$DOOPS_TEST_GIT_SECRET"
printf '0123456789abcdef\trefs/heads/main\n'
`
	if err := os.WriteFile(filepath.Join(bin, "kubectl"), []byte(kubectl), 0755); err != nil {
		t.Fatalf("write fake kubectl: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(git), 0755); err != nil {
		t.Fatalf("write fake git: %v", err)
	}
	t.Setenv("DOOPS_TEST_ARGS_LOG", argsLog)
	return bin, argsLog
}

func credentialTestCertificate(t *testing.T) (string, string) {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate private key: %v", err)
	}
	now := time.Now().UTC()
	template := x509.Certificate{
		SerialNumber: big.NewInt(now.UnixNano()),
		Subject:      pkix.Name{CommonName: "credential.test"},
		DNSNames:     []string{"credential.test"},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatalf("marshal private key: %v", err)
	}
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	key := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return string(certificate), string(key)
}

func readCredentialTestFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func assertCredentialCanaryNotInText(t *testing.T, subject, text, canary string) {
	t.Helper()
	if canary != "" && strings.Contains(text, canary) {
		t.Fatalf("%s exposed credential canary", subject)
	}
}

func allowCredentialPrivateEndpointsForTest(t *testing.T) {
	t.Helper()
	previous := credentialPrivateEndpointAllowed
	credentialPrivateEndpointAllowed = func(string) bool { return true }
	t.Cleanup(func() {
		credentialPrivateEndpointAllowed = previous
	})
}
