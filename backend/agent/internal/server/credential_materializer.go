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
	"fmt"
	"io"
	"mime"
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
	SessionID      string               `json:"session_id"`
	Cluster        string               `json:"cluster,omitempty"`
	Instance       string               `json:"instance,omitempty"`
	CredentialID   string               `json:"credential_id"`
	GrantID        string               `json:"grant_id"`
	VersionID      string               `json:"version_id"`
	CredentialType CredentialType       `json:"credential_type"`
	Scope          CredentialGrantScope `json:"scope"`
	RequiredKeys   []string             `json:"required_keys,omitempty"`
	Payload        json.RawMessage      `json:"payload"`
	// AuthfileOnly delivers the credential to the Agent's own OCI tool
	// configuration file and leaves the Kubernetes Secret alone. The Gateway
	// uses it to restore that file after an Agent restart, where the Secret
	// still exists but the file the Agent wrote is gone with the Pod.
	AuthfileOnly bool `json:"authfile_only,omitempty"`
}

type CredentialCleanupRequest struct {
	CredentialType CredentialType         `json:"credential_type,omitempty"`
	SessionID      string                 `json:"session_id"`
	CredentialID   string                 `json:"credential_id"`
	GrantID        string                 `json:"grant_id"`
	VersionID      string                 `json:"version_id"`
	AnyVersion     bool                   `json:"any_version,omitempty"`
	Namespace      string                 `json:"namespace"`
	Workload       CredentialPlanWorkload `json:"workload,omitempty"`
	// AuthfileOnly limits the cleanup to the Agent's own OCI authfile. The
	// Kubernetes Secret is the kubelet's pull credential and outlives the Pod,
	// so a failed reconnect delivery must not remove it: a workload pulling
	// with that Secret would lose its registry access.
	AuthfileOnly bool `json:"authfile_only,omitempty"`
}

type credentialTargetSnapshot struct {
	Secret            []byte
	PreviousVersionID string
	WorkloadKind      string
	WorkloadName      string
	ImagePullNames    []string
}

type credentialSecretInspection struct {
	UID             string
	SecretType      string
	ResourceVersion string
	CredentialID    string
	GrantID         string
	VersionID       string
	Data            map[string]string
}

func materializeCredential(ctx context.Context, req CredentialMaterializeRequest) (CredentialMaterialization, error) {
	if strings.TrimSpace(req.CredentialID) == "" || strings.TrimSpace(req.GrantID) == "" ||
		strings.TrimSpace(req.VersionID) == "" ||
		!validCredentialType(req.CredentialType) ||
		validateCredentialGrantScope(req.Scope) != nil {
		return CredentialMaterialization{}, errors.New("credential_request_invalid")
	}
	if err := validateCredentialPayload(req.CredentialType, req.Payload); err != nil {
		return CredentialMaterialization{}, ErrCredentialPayloadInvalid
	}

	name := credentialSecretName(req.CredentialID, req.GrantID)
	result := CredentialMaterialization{
		CredentialID: req.CredentialID,
		VersionID:    req.VersionID,
		ResourceName: name,
		Namespace:    req.Scope.Namespace,
		Status:       "verified",
	}

	var secretType string
	var secretData map[string]string
	// pendingRegistryAuth holds a registry authfile delivery that must happen
	// only after the Secret verifies.
	var pendingRegistryAuth *registryAuthPendingDelivery
	switch req.CredentialType {
	case CredentialTypeRegistry:
		if req.Scope.Use != CredentialUseImagePull {
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
		if req.Scope.ImageRepository != "" || req.Scope.ImageDigest != "" {
			digest, err := verifyRegistryManifest(ctx, payload.Server, payload.Username, payload.Password,
				req.Scope.ImageRepository, req.Scope.ImageDigest)
			if err != nil {
				return CredentialMaterialization{}, err
			}
			result.Digest = digest
		}
		// The Secret is what the kubelet pulls with; the Agent's own OCI tools
		// read an authfile. The file is written after the Secret verifies, so a
		// failed materialization never leaves the Agent holding registry
		// access the Secret was not granted.
		authfileEntry := map[string]string{
			"username": payload.Username,
			"password": payload.Password,
			"email":    payload.Email,
			"auth":     auth,
		}
		if req.AuthfileOnly {
			// The Secret already exists and outlives the Pod; only the authfile
			// was lost. Restoring it must not rebuild the Secret, because
			// replacing it would break a workload that is pulling with it.
			if err := deliverRegistryAuthEntry(payload.Server, authfileEntry,
				req.CredentialID, req.GrantID, req.VersionID); err != nil {
				return CredentialMaterialization{}, err
			}
			return registryAuthOnlyMaterialization(req, payload.Server, auth, result.Digest), nil
		}
		pendingRegistryAuth = &registryAuthPendingDelivery{
			server: payload.Server, entry: authfileEntry,
			credentialID: req.CredentialID, grantID: req.GrantID, versionID: req.VersionID,
		}
	case CredentialTypeTLS:
		if req.Scope.Use != CredentialUseTLS {
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
		if req.Scope.Use != CredentialUseOpaqueSecret && req.Scope.Use != CredentialUseDatabase {
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
		if req.Scope.Use != CredentialUseHelmPull {
			return CredentialMaterialization{}, errors.New("credential_use_mismatch")
		}
		var payload struct {
			URL      string `json:"url"`
			Username string `json:"username"`
			Password string `json:"password"`
		}
		_ = json.Unmarshal(req.Payload, &payload)
		sum := sha256.Sum256(req.Payload)
		digest := "sha256:" + hex.EncodeToString(sum[:])
		return materializeNativeHelmCredential(ctx, req, nativeHelmAuth{
			URL: payload.URL, Username: payload.Username, Password: payload.Password,
		}, digest)
	case CredentialTypeModelProvider:
		// The model lives in doagent's fixed settings file, not a Kubernetes Secret.
		return materializeModelProviderCredential(ctx, req)
	case CredentialTypeGitToken:
		if req.Scope.Use != CredentialUseGitCheckout {
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
	applyErr := applyCredentialSecret(ctx, req, name, secretType, secretData)
	applied, inspectErr := inspectCredentialSecret(ctx, req.Scope.Namespace, name)
	if inspectErr != nil || !credentialSecretMatches(applied, req, secretType, secretData) {
		clear(applied.Data)
		if restoreErr := restoreCredentialTarget(ctx, req.Scope.Namespace, name, snapshot, applied); restoreErr != nil {
			return CredentialMaterialization{}, errors.New("credential_target_outcome_unknown")
		}
		if applyErr != nil {
			return CredentialMaterialization{}, applyErr
		}
		if inspectErr != nil {
			return CredentialMaterialization{}, inspectErr
		}
		return CredentialMaterialization{}, errors.New("credential_secret_verification_failed")
	}
	defer clear(applied.Data)
	if req.CredentialType == CredentialTypeRegistry && strings.TrimSpace(req.Scope.Workload.Name) != "" {
		if err := patchImagePullSecret(ctx, req.Scope.Namespace, req.Scope.Workload, name); err != nil {
			if restoreErr := restoreCredentialTarget(ctx, req.Scope.Namespace, name, snapshot, applied); restoreErr != nil {
				return CredentialMaterialization{}, errors.New("credential_target_outcome_unknown")
			}
			return CredentialMaterialization{}, err
		}
	}
	actual, err := inspectCredentialSecret(ctx, req.Scope.Namespace, name)
	if err != nil {
		if restoreErr := restoreCredentialTarget(ctx, req.Scope.Namespace, name, snapshot, applied); restoreErr != nil {
			return CredentialMaterialization{}, errors.New("credential_target_outcome_unknown")
		}
		return CredentialMaterialization{}, err
	}
	defer clear(actual.Data)
	keys := mapKeys(actual.Data)
	if actual.UID != applied.UID || !credentialSecretMatches(actual, req, secretType, secretData) {
		if restoreErr := restoreCredentialTarget(ctx, req.Scope.Namespace, name, snapshot, applied); restoreErr != nil {
			return CredentialMaterialization{}, errors.New("credential_target_outcome_unknown")
		}
		return CredentialMaterialization{}, errors.New("credential_secret_verification_failed")
	}
	result.SecretType = actual.SecretType
	result.ResourceVersion = actual.ResourceVersion
	result.Keys = keys
	if pendingRegistryAuth != nil {
		// The Secret is in place and verified, so the Agent may now use the
		// registry from its own tools.
		if err := deliverRegistryAuthEntry(pendingRegistryAuth.server, pendingRegistryAuth.entry,
			pendingRegistryAuth.credentialID, pendingRegistryAuth.grantID, pendingRegistryAuth.versionID); err != nil {
			if restoreErr := restoreCredentialTarget(ctx, req.Scope.Namespace, name, snapshot, actual); restoreErr != nil {
				return CredentialMaterialization{}, errors.New("credential_target_outcome_unknown")
			}
			return CredentialMaterialization{}, err
		}
	}
	return result, nil
}

func snapshotCredentialTarget(ctx context.Context, req CredentialMaterializeRequest, name string) (credentialTargetSnapshot, error) {
	snapshot := credentialTargetSnapshot{}
	cmd := exec.CommandContext(ctx, "kubectl", "get", "secret", name, "-n", req.Scope.Namespace, "--ignore-not-found", "-o", "json")
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
	if req.CredentialType == CredentialTypeRegistry && strings.TrimSpace(req.Scope.Workload.Name) != "" {
		kind := strings.ToLower(strings.TrimSpace(req.Scope.Workload.Kind))
		if (kind != "deployment" && kind != "statefulset" && kind != "daemonset") ||
			!validKubernetesCredentialName(req.Scope.Workload.Name) {
			return credentialTargetSnapshot{}, errors.New("credential_workload_invalid")
		}
		names, err := inspectImagePullSecrets(ctx, req.Scope.Namespace, kind, req.Scope.Workload.Name)
		if err != nil {
			return credentialTargetSnapshot{}, err
		}
		snapshot.WorkloadKind = kind
		snapshot.WorkloadName = req.Scope.Workload.Name
		snapshot.ImagePullNames = names
	}
	return snapshot, nil
}

func restoreCredentialTarget(
	ctx context.Context,
	namespace string,
	name string,
	snapshot credentialTargetSnapshot,
	applied credentialSecretInspection,
) error {
	if len(snapshot.Secret) == 0 {
		current, err := getCredentialSecret(ctx, namespace, name)
		if err != nil {
			return errors.New("credential_secret_restore_failed")
		}
		if len(bytes.TrimSpace(current)) != 0 {
			currentIdentity, identityErr := inspectCredentialSecretIdentity(current)
			if identityErr != nil || applied.UID == "" || currentIdentity.UID != applied.UID ||
				(applied.ResourceVersion != "" && currentIdentity.ResourceVersion != applied.ResourceVersion) {
				return errors.New("credential_secret_restore_failed")
			}
			deleteOptions, _ := json.Marshal(map[string]any{
				"apiVersion": "v1",
				"kind":       "DeleteOptions",
				"preconditions": map[string]string{
					"uid":             currentIdentity.UID,
					"resourceVersion": currentIdentity.ResourceVersion,
				},
			})
			_ = runCredentialCommand(ctx, deleteOptions, "kubectl", "delete",
				"--raw", "/api/v1/namespaces/"+namespace+"/secrets/"+name, "-f", "-")
			remaining, inspectErr := getCredentialSecret(ctx, namespace, name)
			if inspectErr != nil || len(bytes.TrimSpace(remaining)) != 0 {
				return errors.New("credential_secret_restore_failed")
			}
		}
	} else {
		current, err := getCredentialSecret(ctx, namespace, name)
		if err != nil || len(bytes.TrimSpace(current)) == 0 {
			return errors.New("credential_secret_restore_failed")
		}
		currentIdentity, identityErr := inspectCredentialSecretIdentity(current)
		if identityErr != nil || applied.UID == "" || currentIdentity.UID != applied.UID ||
			(applied.ResourceVersion != "" && currentIdentity.ResourceVersion != applied.ResourceVersion) {
			return errors.New("credential_secret_restore_failed")
		}
		manifest, sanitizeErr := credentialSecretRestoreManifest(snapshot.Secret, currentIdentity)
		if sanitizeErr != nil {
			return errors.New("credential_secret_restore_failed")
		}
		_ = runCredentialCommand(ctx, manifest, "kubectl", "replace", "-f", "-")
		restored, err := getCredentialSecret(ctx, namespace, name)
		restoredIdentity, identityErr := inspectCredentialSecretIdentity(restored)
		if err != nil || identityErr != nil || restoredIdentity.UID != currentIdentity.UID ||
			!sameCredentialSecretSnapshot(snapshot.Secret, restored) {
			return errors.New("credential_secret_restore_failed")
		}
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
	_ = runCredentialCommand(ctx, nil, "kubectl", "patch", snapshot.WorkloadKind, snapshot.WorkloadName,
		"-n", namespace, "--type=merge", "--patch", string(patch))
	observed, err := inspectImagePullSecrets(ctx, namespace, snapshot.WorkloadKind, snapshot.WorkloadName)
	if err != nil || !sameCredentialKeys(observed, snapshot.ImagePullNames) {
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
			"namespace": req.Scope.Namespace,
			"labels": map[string]string{
				"app.kubernetes.io/managed-by": "doops",
				"doops.sh/credential-id":       req.CredentialID,
				"doops.sh/credential-grant-id": req.GrantID,
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
	kind, err := credentialWorkloadKind(workload)
	if err != nil {
		return err
	}
	names, err := inspectImagePullSecrets(ctx, namespace, kind, workload.Name)
	if err != nil {
		return err
	}
	if !containsString(names, secretName) {
		names = append(names, secretName)
	}
	mutationErr := patchImagePullSecrets(ctx, namespace, kind, workload.Name, names)
	observed, inspectErr := inspectImagePullSecrets(ctx, namespace, kind, workload.Name)
	if inspectErr != nil {
		return errors.New("credential_target_outcome_unknown")
	}
	if sameCredentialKeys(observed, names) {
		return nil
	}
	if mutationErr != nil {
		return mutationErr
	}
	return errors.New("credential_workload_verification_failed")
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
	if strings.TrimSpace(req.CredentialID) == "" || strings.TrimSpace(req.GrantID) == "" ||
		(!req.AnyVersion && strings.TrimSpace(req.VersionID) == "") ||
		!validKubernetesCredentialName(req.Namespace) {
		return errors.New("credential_cleanup_invalid")
	}
	if req.AuthfileOnly {
		// A restore owns nothing new: the Kubernetes Secret belongs to the
		// workload pulling with it, and the authfile entry it restores was
		// already there before the restore. Removing either would take away
		// registry access the Agent had before the restore was attempted, so a
		// failed restore leaves both alone.
		return nil
	}
	if cleaned, err := cleanupNativeHelmCredential(req); err != nil {
		return err
	} else if cleaned {
		return nil
	}
	if cleaned, err := cleanupModelProviderCredential(req); err != nil {
		return err
	} else if cleaned {
		return nil
	}
	// A registry grant owns authfile entries as well as a Secret, so this
	// removes the entries and the Secret cleanup below still runs.
	if err := cleanupRegistryAuthEntries(req); err != nil {
		return err
	}
	name := credentialSecretName(req.CredentialID, req.GrantID)
	output, err := getCredentialSecret(ctx, req.Namespace, name)
	if err != nil {
		return err
	}
	secretExists := len(bytes.TrimSpace(output)) != 0
	var secretUID string
	var secretResourceVersion string
	if secretExists {
		var secret struct {
			Metadata struct {
				UID             string            `json:"uid"`
				ResourceVersion string            `json:"resourceVersion"`
				Labels          map[string]string `json:"labels"`
				Annotations     map[string]string `json:"annotations"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal(output, &secret); err != nil {
			return errors.New("credential_secret_verification_failed")
		}
		secretUID = strings.TrimSpace(secret.Metadata.UID)
		secretResourceVersion = strings.TrimSpace(secret.Metadata.ResourceVersion)
		if secret.Metadata.Labels["app.kubernetes.io/managed-by"] != "doops" ||
			secret.Metadata.Labels["doops.sh/credential-id"] != req.CredentialID ||
			secret.Metadata.Labels["doops.sh/credential-grant-id"] != req.GrantID ||
			secretUID == "" || secretResourceVersion == "" ||
			(!req.AnyVersion && secret.Metadata.Annotations["doops.sh/credential-version"] != req.VersionID) {
			return errors.New("credential_secret_ownership_mismatch")
		}
	}

	var workloadKind string
	var originalNames []string
	workloadChanged := false
	if strings.TrimSpace(req.Workload.Name) != "" {
		workloadKind, err = credentialWorkloadKind(req.Workload)
		if err != nil {
			return err
		}
		originalNames, err = inspectImagePullSecrets(ctx, req.Namespace, workloadKind, req.Workload.Name)
		if err != nil {
			return err
		}
		filtered := withoutString(originalNames, name)
		if !sameCredentialKeys(originalNames, filtered) {
			mutationErr := patchImagePullSecrets(ctx, req.Namespace, workloadKind, req.Workload.Name, filtered)
			observed, inspectErr := inspectImagePullSecrets(ctx, req.Namespace, workloadKind, req.Workload.Name)
			if inspectErr != nil {
				return errors.New("credential_target_outcome_unknown")
			}
			switch {
			case sameCredentialKeys(observed, filtered):
				workloadChanged = true
			case mutationErr != nil && sameCredentialKeys(observed, originalNames):
				return mutationErr
			default:
				return errors.New("credential_target_outcome_unknown")
			}
		}
	}

	if !secretExists {
		return nil
	}
	deleteOptions, _ := json.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "DeleteOptions",
		"preconditions": map[string]string{
			"uid":             secretUID,
			"resourceVersion": secretResourceVersion,
		},
	})
	_ = runCredentialCommand(ctx, deleteOptions, "kubectl", "delete",
		"--raw", "/api/v1/namespaces/"+req.Namespace+"/secrets/"+name, "-f", "-")
	remaining, inspectErr := getCredentialSecret(ctx, req.Namespace, name)
	if inspectErr != nil {
		return errors.New("credential_target_outcome_unknown")
	}
	if len(bytes.TrimSpace(remaining)) == 0 {
		return nil
	}
	var remainingSecret struct {
		Metadata struct {
			UID string `json:"uid"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(remaining, &remainingSecret); err != nil ||
		strings.TrimSpace(remainingSecret.Metadata.UID) != secretUID {
		return errors.New("credential_target_outcome_unknown")
	}
	if workloadChanged {
		_ = patchImagePullSecrets(ctx, req.Namespace, workloadKind, req.Workload.Name, originalNames)
		observed, verifyErr := inspectImagePullSecrets(ctx, req.Namespace, workloadKind, req.Workload.Name)
		if verifyErr != nil || !sameCredentialKeys(observed, originalNames) {
			return errors.New("credential_target_outcome_unknown")
		}
	}
	return errors.New("credential_secret_delete_failed")
}

func patchImagePullSecrets(ctx context.Context, namespace, kind, name string, names []string) error {
	imagePullSecrets := make([]map[string]string, 0, len(names))
	for _, item := range names {
		imagePullSecrets = append(imagePullSecrets, map[string]string{"name": item})
	}
	patch, _ := json.Marshal(map[string]any{
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"imagePullSecrets": imagePullSecrets,
		}}},
	})
	return runCredentialCommand(ctx, nil, "kubectl", "patch", kind, name, "-n", namespace, "--type=merge", "--patch", string(patch))
}

func credentialWorkloadKind(workload CredentialPlanWorkload) (string, error) {
	kind := strings.ToLower(strings.TrimSpace(workload.Kind))
	if (kind != "deployment" && kind != "statefulset" && kind != "daemonset") ||
		!validKubernetesCredentialName(workload.Name) {
		return "", errors.New("credential_workload_invalid")
	}
	return kind, nil
}

func withoutString(values []string, excluded string) []string {
	filtered := make([]string, 0, len(values))
	for _, value := range values {
		if value != excluded {
			filtered = append(filtered, value)
		}
	}
	return filtered
}

func getCredentialSecret(ctx context.Context, namespace, name string) ([]byte, error) {
	output, err := exec.CommandContext(ctx, "kubectl", "get", "secret", name, "-n", namespace, "--ignore-not-found", "-o", "json").Output()
	if err != nil {
		return nil, errors.New("credential_secret_get_failed")
	}
	return output, nil
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func inspectCredentialSecret(ctx context.Context, namespace, name string) (credentialSecretInspection, error) {
	cmd := exec.CommandContext(ctx, "kubectl", "get", "secret", name, "-n", namespace, "-o", "json")
	output, err := cmd.Output()
	if err != nil {
		return credentialSecretInspection{}, errors.New("credential_secret_get_failed")
	}
	var secret struct {
		Metadata struct {
			UID             string            `json:"uid"`
			ResourceVersion string            `json:"resourceVersion"`
			Labels          map[string]string `json:"labels"`
			Annotations     map[string]string `json:"annotations"`
		} `json:"metadata"`
		Type string            `json:"type"`
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(output, &secret); err != nil {
		return credentialSecretInspection{}, errors.New("credential_secret_verification_failed")
	}
	actual := credentialSecretInspection{
		UID:             strings.TrimSpace(secret.Metadata.UID),
		SecretType:      strings.TrimSpace(secret.Type),
		ResourceVersion: strings.TrimSpace(secret.Metadata.ResourceVersion),
		CredentialID:    strings.TrimSpace(secret.Metadata.Labels["doops.sh/credential-id"]),
		GrantID:         strings.TrimSpace(secret.Metadata.Labels["doops.sh/credential-grant-id"]),
		VersionID:       strings.TrimSpace(secret.Metadata.Annotations["doops.sh/credential-version"]),
		Data:            make(map[string]string, len(secret.Data)),
	}
	if actual.UID == "" ||
		strings.TrimSpace(secret.Type) == "" ||
		strings.TrimSpace(secret.Metadata.ResourceVersion) == "" {
		return actual, errors.New("credential_secret_verification_failed")
	}
	for key, encoded := range secret.Data {
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			clear(actual.Data)
			actual.Data = nil
			return actual, errors.New("credential_secret_verification_failed")
		}
		actual.Data[key] = string(decoded)
		clear(decoded)
	}
	return actual, nil
}

func inspectCredentialSecretIdentity(raw []byte) (credentialSecretInspection, error) {
	var secret struct {
		Metadata struct {
			UID             string `json:"uid"`
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &secret); err != nil {
		return credentialSecretInspection{}, err
	}
	identity := credentialSecretInspection{
		UID:             strings.TrimSpace(secret.Metadata.UID),
		ResourceVersion: strings.TrimSpace(secret.Metadata.ResourceVersion),
	}
	if identity.UID == "" || identity.ResourceVersion == "" {
		return credentialSecretInspection{}, errors.New("credential_secret_identity_invalid")
	}
	return identity, nil
}

func credentialSecretRestoreManifest(
	snapshot []byte,
	current credentialSecretInspection,
) ([]byte, error) {
	var manifest map[string]any
	if err := json.Unmarshal(snapshot, &manifest); err != nil {
		return nil, err
	}
	metadata, ok := manifest["metadata"].(map[string]any)
	if !ok {
		return nil, errors.New("credential_secret_metadata_invalid")
	}
	for _, key := range []string{
		"uid", "resourceVersion", "generation", "managedFields", "creationTimestamp",
		"deletionTimestamp", "deletionGracePeriodSeconds", "selfLink",
	} {
		delete(metadata, key)
	}
	metadata["uid"] = current.UID
	metadata["resourceVersion"] = current.ResourceVersion
	delete(manifest, "status")
	return json.Marshal(manifest)
}

func credentialSecretMatches(
	actual credentialSecretInspection,
	req CredentialMaterializeRequest,
	secretType string,
	secretData map[string]string,
) bool {
	return actual.SecretType == secretType &&
		actual.CredentialID == req.CredentialID &&
		actual.GrantID == req.GrantID &&
		actual.VersionID == req.VersionID &&
		sameCredentialData(secretData, actual.Data)
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
	request, err := registryManifestRequest(ctx, endpoint)
	if err != nil {
		return "", errors.New("registry_request_invalid")
	}
	request.SetBasicAuth(username, password)
	response, err := credentialHTTPClient(request.URL).Do(request)
	if err != nil {
		return "", errors.New("registry_manifest_unavailable")
	}
	if response.StatusCode == http.StatusUnauthorized {
		challenge := response.Header.Get("WWW-Authenticate")
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
		_ = response.Body.Close()
		token, err := exchangeRegistryBearerToken(
			ctx, request.URL, challenge, username, password,
		)
		if err != nil {
			return "", err
		}
		request, err = registryManifestRequest(ctx, endpoint)
		if err != nil {
			return "", errors.New("registry_request_invalid")
		}
		request.Header.Set("Authorization", "Bearer "+token)
		response, err = credentialHTTPClient(request.URL).Do(request)
		if err != nil {
			return "", errors.New("registry_manifest_unavailable")
		}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
		return "", errors.New("registry_manifest_unavailable")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(body) == 0 || len(body) > 1<<20 {
		return "", errors.New("registry_manifest_invalid")
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || !validRegistryManifestMediaType(mediaType) {
		return "", errors.New("registry_manifest_invalid")
	}
	var manifest struct {
		SchemaVersion int `json:"schemaVersion"`
	}
	if err := json.Unmarshal(body, &manifest); err != nil || manifest.SchemaVersion != 2 {
		return "", errors.New("registry_manifest_invalid")
	}
	digest := strings.TrimSpace(response.Header.Get("Docker-Content-Digest"))
	if digest == "" {
		return "", errors.New("registry_digest_missing")
	}
	if !validCredentialSHA256Digest(digest) {
		return "", errors.New("registry_digest_invalid")
	}
	computed := sha256.Sum256(body)
	if digest != "sha256:"+hex.EncodeToString(computed[:]) {
		return "", errors.New("registry_digest_mismatch")
	}
	if validCredentialSHA256Digest(reference) && digest != reference {
		return "", errors.New("registry_reference_digest_mismatch")
	}
	return digest, nil
}

func validRegistryManifestMediaType(value string) bool {
	switch strings.TrimSpace(value) {
	case "application/vnd.oci.image.manifest.v1+json",
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.docker.distribution.manifest.v2+json",
		"application/vnd.docker.distribution.manifest.list.v2+json":
		return true
	default:
		return false
	}
}

func registryManifestRequest(ctx context.Context, endpoint string) (*http.Request, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set(
		"Accept",
		"application/vnd.oci.image.manifest.v1+json, "+
			"application/vnd.oci.image.index.v1+json, "+
			"application/vnd.docker.distribution.manifest.v2+json, "+
			"application/vnd.docker.distribution.manifest.list.v2+json",
	)
	return request, nil
}

func exchangeRegistryBearerToken(
	ctx context.Context,
	registryURL *url.URL,
	challenge string,
	username string,
	password string,
) (string, error) {
	parameters, err := parseRegistryBearerChallenge(challenge)
	if err != nil {
		return "", errors.New("registry_auth_challenge_invalid")
	}
	realm, err := url.Parse(parameters["realm"])
	if err != nil || realm.User != nil || realm.Hostname() == "" || realm.Fragment != "" {
		return "", errors.New("registry_auth_challenge_invalid")
	}
	if realm.Scheme != "https" &&
		!(realm.Scheme == "http" && registryURL.Scheme == "http" && realm.Host == registryURL.Host) {
		return "", errors.New("registry_auth_challenge_invalid")
	}
	query := realm.Query()
	for _, key := range []string{"service", "scope"} {
		if value := parameters[key]; value != "" {
			query.Set(key, value)
		}
	}
	realm.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
	if err != nil {
		return "", errors.New("registry_auth_challenge_invalid")
	}
	request.SetBasicAuth(username, password)
	response, err := credentialHTTPClient(request.URL).Do(request)
	if err != nil {
		return "", errors.New("registry_auth_failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
		return "", errors.New("registry_auth_failed")
	}
	var payload struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	if err := decoder.Decode(&payload); err != nil {
		return "", errors.New("registry_auth_response_invalid")
	}
	token := strings.TrimSpace(payload.Token)
	if token == "" {
		token = strings.TrimSpace(payload.AccessToken)
	}
	if token == "" {
		return "", errors.New("registry_auth_response_invalid")
	}
	return token, nil
}

func parseRegistryBearerChallenge(header string) (map[string]string, error) {
	header = strings.TrimSpace(header)
	if len(header) < len("Bearer ") || !strings.EqualFold(header[:len("Bearer")], "Bearer") {
		return nil, errors.New("not a bearer challenge")
	}
	raw := strings.TrimSpace(header[len("Bearer"):])
	parts := make([]string, 0, 3)
	start := 0
	quoted := false
	escaped := false
	for index, char := range raw {
		switch {
		case escaped:
			escaped = false
		case char == '\\' && quoted:
			escaped = true
		case char == '"':
			quoted = !quoted
		case char == ',' && !quoted:
			parts = append(parts, raw[start:index])
			start = index + 1
		}
	}
	if quoted || escaped {
		return nil, errors.New("invalid bearer challenge quoting")
	}
	parts = append(parts, raw[start:])
	parameters := make(map[string]string, len(parts))
	for _, part := range parts {
		key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)
		if !ok || key == "" || len(value) < 2 || value[0] != '"' || value[len(value)-1] != '"' {
			return nil, errors.New("invalid bearer challenge parameter")
		}
		value = strings.ReplaceAll(value[1:len(value)-1], `\"`, `"`)
		if value == "" {
			return nil, errors.New("empty bearer challenge parameter")
		}
		parameters[key] = value
	}
	if parameters["realm"] == "" {
		return nil, errors.New("bearer challenge realm is required")
	}
	return parameters, nil
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
	signer, err := parseTLSPrivateKey(keyBlock)
	if err != nil {
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

func parseTLSPrivateKey(block *pem.Block) (crypto.Signer, error) {
	if block == nil {
		return nil, errors.New("private key is missing")
	}
	switch block.Type {
	case "PRIVATE KEY":
		privateKey, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		signer, ok := privateKey.(crypto.Signer)
		if !ok {
			return nil, errors.New("private key does not implement crypto.Signer")
		}
		return signer, nil
	case "RSA PRIVATE KEY":
		return x509.ParsePKCS1PrivateKey(block.Bytes)
	case "EC PRIVATE KEY":
		return x509.ParseECPrivateKey(block.Bytes)
	default:
		return nil, fmt.Errorf("unsupported private key PEM type %q", block.Type)
	}
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

func credentialSecretName(credentialID, grantID string) string {
	sum := sha256.Sum256([]byte(credentialID + "\x00" + grantID))
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

func sameCredentialData(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

func sameCredentialSecretSnapshot(left, right []byte) bool {
	var decode = func(raw []byte) (struct {
		Metadata struct {
			Labels      map[string]string `json:"labels"`
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
		Type string            `json:"type"`
		Data map[string]string `json:"data"`
	}, error) {
		var secret struct {
			Metadata struct {
				Labels      map[string]string `json:"labels"`
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
			Type string            `json:"type"`
			Data map[string]string `json:"data"`
		}
		err := json.Unmarshal(raw, &secret)
		return secret, err
	}
	expected, err := decode(left)
	if err != nil {
		return false
	}
	actual, err := decode(right)
	if err != nil {
		return false
	}
	return expected.Type == actual.Type &&
		sameCredentialData(expected.Data, actual.Data) &&
		expected.Metadata.Labels["app.kubernetes.io/managed-by"] == actual.Metadata.Labels["app.kubernetes.io/managed-by"] &&
		expected.Metadata.Labels["doops.sh/credential-id"] == actual.Metadata.Labels["doops.sh/credential-id"] &&
		expected.Metadata.Labels["doops.sh/credential-grant-id"] == actual.Metadata.Labels["doops.sh/credential-grant-id"] &&
		expected.Metadata.Annotations["doops.sh/credential-version"] == actual.Metadata.Annotations["doops.sh/credential-version"]
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
