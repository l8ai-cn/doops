package server

import (
	"bytes"
	"context"
	"crypto"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type CredentialMaterializeRequest struct {
	SessionID          string                 `json:"session_id"`
	Cluster            string                 `json:"cluster,omitempty"`
	Instance           string                 `json:"instance,omitempty"`
	CredentialID       string                 `json:"credential_id"`
	VersionID          string                 `json:"version_id"`
	CredentialType     CredentialType         `json:"credential_type"`
	Use                CredentialUse          `json:"use"`
	Namespace          string                 `json:"namespace"`
	Workload           CredentialPlanWorkload `json:"workload,omitempty"`
	RegistryRepository string                 `json:"registry_repository,omitempty"`
	RegistryReference  string                 `json:"registry_reference,omitempty"`
	RequiredKeys       []string               `json:"required_keys,omitempty"`
	Payload            json.RawMessage        `json:"payload"`
}

type CredentialCleanupRequest struct {
	SessionID    string                 `json:"session_id"`
	CredentialID string                 `json:"credential_id"`
	VersionID    string                 `json:"version_id"`
	Namespace    string                 `json:"namespace"`
	Workload     CredentialPlanWorkload `json:"workload,omitempty"`
}

type credentialTargetSnapshot struct {
	Secret            []byte
	PreviousVersionID string
	WorkloadKind      string
	WorkloadName      string
	ImagePullNames    []string
}

func materializeCredential(ctx context.Context, req CredentialMaterializeRequest) (CredentialMaterialization, error) {
	if strings.TrimSpace(req.CredentialID) == "" || strings.TrimSpace(req.VersionID) == "" ||
		strings.TrimSpace(req.Namespace) == "" || !validCredentialType(req.CredentialType) ||
		!validCredentialUse(req.Use) {
		return CredentialMaterialization{}, errors.New("credential_request_invalid")
	}
	if err := validateCredentialPayload(req.CredentialType, req.Payload); err != nil {
		return CredentialMaterialization{}, ErrCredentialPayloadInvalid
	}

	name := credentialSecretName(req.CredentialID)
	result := CredentialMaterialization{
		CredentialID: req.CredentialID,
		VersionID:    req.VersionID,
		ResourceName: name,
		Namespace:    req.Namespace,
		Status:       "verified",
	}

	var secretType string
	var secretData map[string]string
	switch req.CredentialType {
	case CredentialTypeRegistry:
		if req.Use != CredentialUseImagePull {
			return CredentialMaterialization{}, errors.New("credential_use_mismatch")
		}
		var payload struct {
			Server   string `json:"server"`
			Username string `json:"username"`
			Password string `json:"password"`
			Email    string `json:"email,omitempty"`
		}
		_ = json.Unmarshal(req.Payload, &payload)
		auth := base64.StdEncoding.EncodeToString([]byte(payload.Username + ":" + payload.Password))
		dockerConfig, _ := json.Marshal(map[string]any{"auths": map[string]any{
			payload.Server: map[string]string{
				"username": payload.Username,
				"password": payload.Password,
				"email":    payload.Email,
				"auth":     auth,
			},
		}})
		secretType = "kubernetes.io/dockerconfigjson"
		secretData = map[string]string{".dockerconfigjson": string(dockerConfig)}
		digest, err := verifyRegistryManifest(ctx, payload.Server, payload.Username, payload.Password, req.RegistryRepository, req.RegistryReference)
		if err != nil {
			return CredentialMaterialization{}, err
		}
		result.Digest = digest
	case CredentialTypeTLS:
		if req.Use != CredentialUseTLS {
			return CredentialMaterialization{}, errors.New("credential_use_mismatch")
		}
		var payload struct {
			Certificate string `json:"certificate"`
			PrivateKey  string `json:"privateKey"`
		}
		_ = json.Unmarshal(req.Payload, &payload)
		certificate, fingerprint, err := validateTLSCredential(payload.Certificate, payload.PrivateKey)
		if err != nil {
			return CredentialMaterialization{}, err
		}
		secretType = "kubernetes.io/tls"
		secretData = map[string]string{"tls.crt": payload.Certificate, "tls.key": payload.PrivateKey}
		result.Fingerprint = fingerprint
		result.ExpiresAt = certificate.NotAfter.UTC().Format(time.RFC3339)
	case CredentialTypeOpaque:
		if req.Use != CredentialUseOpaqueSecret {
			return CredentialMaterialization{}, errors.New("credential_use_mismatch")
		}
		var payload struct {
			Data map[string]string `json:"data"`
		}
		_ = json.Unmarshal(req.Payload, &payload)
		if !sameCredentialKeys(req.RequiredKeys, mapKeys(payload.Data)) {
			return CredentialMaterialization{}, errors.New("credential_key_mismatch")
		}
		secretType = "Opaque"
		secretData = payload.Data
	case CredentialTypeHelmRepository:
		if req.Use != CredentialUseHelmPull {
			return CredentialMaterialization{}, errors.New("credential_use_mismatch")
		}
		var payload struct {
			URL      string `json:"url"`
			Username string `json:"username"`
			Password string `json:"password"`
		}
		_ = json.Unmarshal(req.Payload, &payload)
		digest, err := verifyHelmRepository(ctx, payload.URL, payload.Username, payload.Password)
		if err != nil {
			return CredentialMaterialization{}, err
		}
		secretType = "Opaque"
		secretData = map[string]string{"url": payload.URL, "username": payload.Username, "password": payload.Password}
		result.Digest = digest
	case CredentialTypeGitToken:
		if req.Use != CredentialUseGitCheckout {
			return CredentialMaterialization{}, errors.New("credential_use_mismatch")
		}
		var payload struct {
			URL      string `json:"url"`
			Username string `json:"username"`
			Token    string `json:"token"`
		}
		_ = json.Unmarshal(req.Payload, &payload)
		digest, err := verifyGitRemote(ctx, payload.URL, payload.Username, payload.Token)
		if err != nil {
			return CredentialMaterialization{}, err
		}
		secretType = "Opaque"
		secretData = map[string]string{"url": payload.URL, "username": payload.Username, "password": payload.Token}
		result.Digest = digest
	default:
		return CredentialMaterialization{}, errors.New("credential_type_unsupported")
	}

	snapshot, err := snapshotCredentialTarget(ctx, req, name)
	if err != nil {
		return CredentialMaterialization{}, err
	}
	result.PreviousVersionID = snapshot.PreviousVersionID
	if err := applyCredentialSecret(ctx, req, name, secretType, secretData); err != nil {
		return CredentialMaterialization{}, err
	}
	if req.CredentialType == CredentialTypeRegistry && strings.TrimSpace(req.Workload.Name) != "" {
		if err := patchImagePullSecret(ctx, req.Namespace, req.Workload, name); err != nil {
			if restoreErr := restoreCredentialTarget(ctx, req.Namespace, name, snapshot); restoreErr != nil {
				return CredentialMaterialization{}, errors.New("credential_target_outcome_unknown")
			}
			return CredentialMaterialization{}, err
		}
	}
	actualType, resourceVersion, keys, err := inspectCredentialSecret(ctx, req.Namespace, name)
	if err != nil {
		if restoreErr := restoreCredentialTarget(ctx, req.Namespace, name, snapshot); restoreErr != nil {
			return CredentialMaterialization{}, errors.New("credential_target_outcome_unknown")
		}
		return CredentialMaterialization{}, err
	}
	expectedKeys := mapKeys(secretData)
	if actualType != secretType || !sameCredentialKeys(expectedKeys, keys) {
		if restoreErr := restoreCredentialTarget(ctx, req.Namespace, name, snapshot); restoreErr != nil {
			return CredentialMaterialization{}, errors.New("credential_target_outcome_unknown")
		}
		return CredentialMaterialization{}, errors.New("credential_secret_verification_failed")
	}
	result.SecretType = actualType
	result.ResourceVersion = resourceVersion
	result.Keys = keys
	return result, nil
}

func snapshotCredentialTarget(ctx context.Context, req CredentialMaterializeRequest, name string) (credentialTargetSnapshot, error) {
	snapshot := credentialTargetSnapshot{}
	cmd := exec.CommandContext(ctx, "kubectl", "get", "secret", name, "-n", req.Namespace, "--ignore-not-found", "-o", "json")
	output, err := cmd.Output()
	if err != nil {
		return credentialTargetSnapshot{}, errors.New("credential_secret_get_failed")
	}
	snapshot.Secret = append([]byte(nil), bytes.TrimSpace(output)...)
	if len(snapshot.Secret) != 0 {
		var existing struct {
			Metadata struct {
				Labels      map[string]string `json:"labels"`
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal(snapshot.Secret, &existing); err != nil {
			return credentialTargetSnapshot{}, errors.New("credential_secret_verification_failed")
		}
		if existing.Metadata.Labels["app.kubernetes.io/managed-by"] != "doops" ||
			existing.Metadata.Labels["doops.sh/credential-id"] != req.CredentialID {
			return credentialTargetSnapshot{}, errors.New("credential_secret_ownership_conflict")
		}
		snapshot.PreviousVersionID = strings.TrimSpace(existing.Metadata.Annotations["doops.sh/credential-version"])
		if snapshot.PreviousVersionID == "" {
			return credentialTargetSnapshot{}, errors.New("credential_secret_ownership_conflict")
		}
	}
	if req.CredentialType == CredentialTypeRegistry && strings.TrimSpace(req.Workload.Name) != "" {
		kind := strings.ToLower(strings.TrimSpace(req.Workload.Kind))
		if (kind != "deployment" && kind != "statefulset" && kind != "daemonset") ||
			!validKubernetesCredentialName(req.Workload.Name) {
			return credentialTargetSnapshot{}, errors.New("credential_workload_invalid")
		}
		names, err := inspectImagePullSecrets(ctx, req.Namespace, kind, req.Workload.Name)
		if err != nil {
			return credentialTargetSnapshot{}, err
		}
		snapshot.WorkloadKind = kind
		snapshot.WorkloadName = req.Workload.Name
		snapshot.ImagePullNames = names
	}
	return snapshot, nil
}

func restoreCredentialTarget(ctx context.Context, namespace, name string, snapshot credentialTargetSnapshot) error {
	if len(snapshot.Secret) == 0 {
		if err := exec.CommandContext(ctx, "kubectl", "delete", "secret", name, "-n", namespace, "--ignore-not-found", "--wait=true").Run(); err != nil {
			return errors.New("credential_secret_restore_failed")
		}
	} else if err := runCredentialCommand(ctx, snapshot.Secret, "kubectl", "replace", "-f", "-"); err != nil {
		return errors.New("credential_secret_restore_failed")
	}
	if snapshot.WorkloadName == "" {
		return nil
	}
	imagePullSecrets := make([]map[string]string, 0, len(snapshot.ImagePullNames))
	for _, item := range snapshot.ImagePullNames {
		imagePullSecrets = append(imagePullSecrets, map[string]string{"name": item})
	}
	patch, _ := json.Marshal(map[string]any{
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"imagePullSecrets": imagePullSecrets,
		}}},
	})
	if err := runCredentialCommand(ctx, patch, "kubectl", "patch", snapshot.WorkloadKind, snapshot.WorkloadName,
		"-n", namespace, "--type=merge", "--patch-file=-"); err != nil {
		return errors.New("credential_workload_restore_failed")
	}
	return nil
}

func applyCredentialSecret(ctx context.Context, req CredentialMaterializeRequest, name, secretType string, values map[string]string) error {
	data := make(map[string]string, len(values))
	for key, value := range values {
		data[key] = base64.StdEncoding.EncodeToString([]byte(value))
	}
	manifest, err := json.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]any{
			"name":      name,
			"namespace": req.Namespace,
			"labels": map[string]string{
				"app.kubernetes.io/managed-by": "doops",
				"doops.sh/credential-id":       req.CredentialID,
			},
			"annotations": map[string]string{
				"doops.sh/credential-version": req.VersionID,
			},
		},
		"type": secretType,
		"data": data,
	})
	if err != nil {
		return errors.New("credential_manifest_invalid")
	}
	return runCredentialCommand(ctx, manifest, "kubectl", "apply", "--server-side", "--field-manager=doops-credential", "-f", "-")
}

func patchImagePullSecret(ctx context.Context, namespace string, workload CredentialPlanWorkload, secretName string) error {
	kind := strings.ToLower(strings.TrimSpace(workload.Kind))
	if (kind != "deployment" && kind != "statefulset" && kind != "daemonset") ||
		!validKubernetesCredentialName(workload.Name) {
		return errors.New("credential_workload_invalid")
	}
	names, err := inspectImagePullSecrets(ctx, namespace, kind, workload.Name)
	if err != nil {
		return err
	}
	if !containsString(names, secretName) {
		names = append(names, secretName)
	}
	imagePullSecrets := make([]map[string]string, 0, len(names))
	for _, name := range names {
		imagePullSecrets = append(imagePullSecrets, map[string]string{"name": name})
	}
	patch, _ := json.Marshal(map[string]any{
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"imagePullSecrets": imagePullSecrets,
		}}},
	})
	return runCredentialCommand(ctx, patch, "kubectl", "patch", kind, workload.Name, "-n", namespace, "--type=merge", "--patch-file=-")
}

func inspectImagePullSecrets(ctx context.Context, namespace, kind, name string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "kubectl", "get", kind, name, "-n", namespace, "-o", "json")
	output, err := cmd.Output()
	if err != nil {
		return nil, errors.New("credential_workload_get_failed")
	}
	var workload struct {
		Spec struct {
			Template struct {
				Spec struct {
					ImagePullSecrets []struct {
						Name string `json:"name"`
					} `json:"imagePullSecrets"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(output, &workload); err != nil {
		return nil, errors.New("credential_workload_invalid")
	}
	names := make([]string, 0, len(workload.Spec.Template.Spec.ImagePullSecrets))
	for _, item := range workload.Spec.Template.Spec.ImagePullSecrets {
		if name := strings.TrimSpace(item.Name); name != "" && !containsString(names, name) {
			names = append(names, name)
		}
	}
	return names, nil
}

func cleanupCredential(ctx context.Context, req CredentialCleanupRequest) error {
	if strings.TrimSpace(req.CredentialID) == "" || strings.TrimSpace(req.VersionID) == "" ||
		!validKubernetesCredentialName(req.Namespace) {
		return errors.New("credential_cleanup_invalid")
	}
	name := credentialSecretName(req.CredentialID)
	cmd := exec.CommandContext(ctx, "kubectl", "get", "secret", name, "-n", req.Namespace, "--ignore-not-found", "-o", "json")
	output, err := cmd.Output()
	if err != nil {
		return errors.New("credential_secret_get_failed")
	}
	if len(bytes.TrimSpace(output)) == 0 {
		return nil
	}
	var secret struct {
		Metadata struct {
			Labels      map[string]string `json:"labels"`
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(output, &secret); err != nil {
		return errors.New("credential_secret_verification_failed")
	}
	if secret.Metadata.Labels["app.kubernetes.io/managed-by"] != "doops" ||
		secret.Metadata.Labels["doops.sh/credential-id"] != req.CredentialID ||
		secret.Metadata.Annotations["doops.sh/credential-version"] != req.VersionID {
		return errors.New("credential_secret_ownership_mismatch")
	}
	if strings.TrimSpace(req.Workload.Name) != "" {
		if err := removeImagePullSecret(ctx, req.Namespace, req.Workload, name); err != nil {
			return err
		}
	}
	if err := exec.CommandContext(ctx, "kubectl", "delete", "secret", name, "-n", req.Namespace, "--wait=true").Run(); err != nil {
		return errors.New("credential_secret_delete_failed")
	}
	return nil
}

func removeImagePullSecret(ctx context.Context, namespace string, workload CredentialPlanWorkload, secretName string) error {
	kind := strings.ToLower(strings.TrimSpace(workload.Kind))
	if (kind != "deployment" && kind != "statefulset" && kind != "daemonset") ||
		!validKubernetesCredentialName(workload.Name) {
		return errors.New("credential_workload_invalid")
	}
	names, err := inspectImagePullSecrets(ctx, namespace, kind, workload.Name)
	if err != nil {
		return err
	}
	filtered := make([]map[string]string, 0, len(names))
	for _, name := range names {
		if name != secretName {
			filtered = append(filtered, map[string]string{"name": name})
		}
	}
	patch, _ := json.Marshal(map[string]any{
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"imagePullSecrets": filtered,
		}}},
	})
	return runCredentialCommand(ctx, patch, "kubectl", "patch", kind, workload.Name, "-n", namespace, "--type=merge", "--patch-file=-")
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func inspectCredentialSecret(ctx context.Context, namespace, name string) (string, string, []string, error) {
	cmd := exec.CommandContext(ctx, "kubectl", "get", "secret", name, "-n", namespace,
		"-o", `jsonpath={.type}{"\n"}{.metadata.resourceVersion}{"\n"}{range $k,$v := .data}{$k}{","}{end}{"\n"}`)
	output, err := cmd.Output()
	if err != nil {
		return "", "", nil, errors.New("credential_secret_get_failed")
	}
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) < 3 || strings.TrimSpace(lines[0]) == "" || strings.TrimSpace(lines[1]) == "" {
		return "", "", nil, errors.New("credential_secret_verification_failed")
	}
	keys := strings.Split(strings.Trim(strings.TrimSpace(lines[2]), ","), ",")
	if len(keys) == 1 && keys[0] == "" {
		keys = nil
	}
	sort.Strings(keys)
	return strings.TrimSpace(lines[0]), strings.TrimSpace(lines[1]), keys, nil
}

func verifyRegistryManifest(ctx context.Context, server, username, password, repository, reference string) (string, error) {
	repository = strings.Trim(strings.TrimSpace(repository), "/")
	reference = strings.TrimSpace(reference)
	if repository == "" || reference == "" || strings.ContainsAny(repository+reference, " \t\r\n?#") {
		return "", errors.New("registry_verification_context_invalid")
	}
	if !strings.Contains(server, "://") {
		server = "https://" + server
	}
	endpoint := strings.TrimRight(server, "/") + "/v2/" + repository + "/manifests/" + reference
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", errors.New("registry_request_invalid")
	}
	request.SetBasicAuth(username, password)
	request.Header.Set("Accept", "application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json")
	response, err := credentialHTTPClient(request.URL).Do(request)
	if err != nil {
		return "", errors.New("registry_manifest_unavailable")
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", errors.New("registry_manifest_unavailable")
	}
	digest := strings.TrimSpace(response.Header.Get("Docker-Content-Digest"))
	if digest == "" {
		return "", errors.New("registry_digest_missing")
	}
	return digest, nil
}

func validateTLSCredential(certificatePEM, privateKeyPEM string) (*x509.Certificate, string, error) {
	certificateBlock, _ := pem.Decode([]byte(certificatePEM))
	keyBlock, _ := pem.Decode([]byte(privateKeyPEM))
	if certificateBlock == nil || keyBlock == nil {
		return nil, "", errors.New("tls_key_pair_invalid")
	}
	certificate, err := x509.ParseCertificate(certificateBlock.Bytes)
	if err != nil {
		return nil, "", errors.New("tls_key_pair_invalid")
	}
	privateKey, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, "", errors.New("tls_key_pair_invalid")
	}
	signer, ok := privateKey.(crypto.Signer)
	if !ok {
		return nil, "", errors.New("tls_key_pair_invalid")
	}
	certificatePublicKey, certificateErr := x509.MarshalPKIXPublicKey(certificate.PublicKey)
	privatePublicKey, privateErr := x509.MarshalPKIXPublicKey(signer.Public())
	if certificateErr != nil || privateErr != nil || !bytes.Equal(certificatePublicKey, privatePublicKey) {
		return nil, "", errors.New("tls_key_pair_invalid")
	}
	sum := sha256.Sum256(certificate.Raw)
	return certificate, "sha256:" + hex.EncodeToString(sum[:]), nil
}

func verifyHelmRepository(ctx context.Context, repositoryURL, username, password string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(repositoryURL, "/")+"/index.yaml", nil)
	if err != nil {
		return "", errors.New("helm_repository_invalid")
	}
	request.SetBasicAuth(username, password)
	response, err := credentialHTTPClient(request.URL).Do(request)
	if err != nil {
		return "", errors.New("helm_repository_unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", errors.New("helm_repository_unavailable")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil || len(body) == 0 {
		return "", errors.New("helm_repository_invalid")
	}
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func verifyGitRemote(ctx context.Context, repositoryURL, username, token string) (string, error) {
	parsed, err := credentialOutboundURL(repositoryURL)
	if err != nil {
		return "", errors.New("git_remote_invalid")
	}
	if _, err := resolveCredentialHost(ctx, parsed.Hostname()); err != nil {
		return "", errors.New("git_remote_invalid")
	}
	dir, err := os.MkdirTemp("", "doops-git-askpass-*")
	if err != nil {
		return "", errors.New("git_verification_setup_failed")
	}
	defer os.RemoveAll(dir)
	askpass := filepath.Join(dir, "askpass")
	script := "#!/bin/sh\ncase \"$1\" in\n  *sername*) printf '%s' \"$DOOPS_GIT_USERNAME\" ;;\n  *) printf '%s' \"$DOOPS_GIT_SECRET\" ;;\nesac\n"
	if err := os.WriteFile(askpass, []byte(script), 0700); err != nil {
		return "", errors.New("git_verification_setup_failed")
	}
	cmd := exec.CommandContext(ctx, "git", "ls-remote", "--heads", repositoryURL)
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ALLOW_PROTOCOL=https",
		"GIT_ASKPASS="+askpass,
		"DOOPS_GIT_USERNAME="+username,
		"DOOPS_GIT_SECRET="+token,
	)
	output, err := cmd.Output()
	if err != nil || len(bytes.TrimSpace(output)) == 0 {
		return "", errors.New("git_remote_unavailable")
	}
	sum := sha256.Sum256(bytes.TrimSpace(output))
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

var credentialPrivateEndpointAllowed = func(string) bool { return false }

func credentialHTTPClient(endpoint *url.URL) *http.Client {
	host := endpoint.Hostname()
	port := endpoint.Port()
	if port == "" {
		port = "443"
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		addresses, err := resolveCredentialHost(ctx, host)
		if err != nil {
			return nil, err
		}
		var dialer net.Dialer
		var lastErr error
		for _, address := range addresses {
			conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(address.String(), port))
			if err == nil {
				return conn, nil
			}
			lastErr = err
		}
		return nil, lastErr
	}
	return &http.Client{
		Transport: transport,
		Timeout:   20 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func credentialOutboundURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Hostname() == "" ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("credential_endpoint_invalid")
	}
	return parsed, nil
}

func resolveCredentialHost(ctx context.Context, host string) ([]net.IP, error) {
	addresses, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil || len(addresses) == 0 {
		return nil, errors.New("credential_endpoint_unresolvable")
	}
	for _, address := range addresses {
		if credentialPrivateEndpointAllowed(host) {
			continue
		}
		if address.IsLoopback() || address.IsPrivate() || address.IsLinkLocalUnicast() ||
			address.IsLinkLocalMulticast() || address.IsUnspecified() || address.IsMulticast() {
			return nil, errors.New("credential_endpoint_private")
		}
	}
	return addresses, nil
}

func runCredentialCommand(ctx context.Context, stdin []byte, command string, args ...string) error {
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Stdin = bytes.NewReader(stdin)
	if err := cmd.Run(); err != nil {
		return errors.New("credential_target_mutation_failed")
	}
	return nil
}

func credentialSecretName(credentialID string) string {
	sum := sha256.Sum256([]byte(credentialID))
	return "doops-cred-" + hex.EncodeToString(sum[:8])
}

func mapKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sameCredentialKeys(left, right []string) bool {
	a := append([]string(nil), left...)
	b := append([]string(nil), right...)
	sort.Strings(a)
	sort.Strings(b)
	return strings.Join(a, "\x00") == strings.Join(b, "\x00")
}

func validKubernetesCredentialName(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 253 {
		return false
	}
	for index, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '-' || char == '.' {
			continue
		}
		if index == 0 || index == len(value)-1 {
			return false
		}
		return false
	}
	return value[0] != '-' && value[0] != '.' && value[len(value)-1] != '-' && value[len(value)-1] != '.'
}
