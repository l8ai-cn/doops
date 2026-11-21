package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/user/doops/agent/api"
)

type CredentialMaterialization struct {
	CredentialID      string   `json:"credentialId"`
	GrantID           string   `json:"grantId,omitempty"`
	VersionID         string   `json:"versionId"`
	ResourceName      string   `json:"resourceName,omitempty"`
	Namespace         string   `json:"namespace"`
	SecretType        string   `json:"secretType,omitempty"`
	Keys              []string `json:"keys,omitempty"`
	ResourceVersion   string   `json:"resourceVersion,omitempty"`
	Digest            string   `json:"digest,omitempty"`
	Fingerprint       string   `json:"fingerprint,omitempty"`
	ExpiresAt         string   `json:"expiresAt,omitempty"`
	Status            string   `json:"status"`
	ErrorCategory     string   `json:"errorCategory,omitempty"`
	PreviousVersionID string   `json:"previousVersionId,omitempty"`
	ContextKind       string   `json:"contextKind,omitempty"`
	ContextSocket     string   `json:"contextSocket,omitempty"`
}

type credentialVerifyHTTPBody struct {
	VersionID string               `json:"version_id"`
	SessionID string               `json:"session_id"`
	Scope     CredentialGrantScope `json:"scope"`
}

type credentialVerificationEvidence struct {
	Status          string `json:"status"`
	ResourceVersion string `json:"resourceVersion,omitempty"`
	Digest          string `json:"digest,omitempty"`
	ErrorCategory   string `json:"errorCategory,omitempty"`
}

type authorizedCredentialMaterialization struct {
	Reference  CredentialPlanReference
	Authorized AuthorizedCredentialUse
}

type completedCredentialMaterialization struct {
	Materialization CredentialMaterialization
	Item            authorizedCredentialMaterialization
	SessionID       string
}

type credentialApplyPreparation struct {
	Bindings  []api.AgentCredentialBinding
	completed []completedCredentialMaterialization
}

func validateCredentialMaterializationMetadata(materialization CredentialMaterialization) error {
	if !validCredentialMetadataToken(materialization.CredentialID, 128) ||
		!validCredentialMetadataToken(materialization.GrantID, 128) ||
		!validCredentialMetadataToken(materialization.VersionID, 128) ||
		strings.TrimSpace(materialization.Namespace) != materialization.Namespace ||
		!validKubernetesCredentialName(materialization.Namespace) {
		return errors.New("credential_materialization_metadata_invalid")
	}
	if materialization.ResourceName != "" &&
		(strings.TrimSpace(materialization.ResourceName) != materialization.ResourceName ||
			!validKubernetesCredentialName(materialization.ResourceName)) {
		return errors.New("credential_materialization_metadata_invalid")
	}
	switch materialization.SecretType {
	case "", "Opaque", "kubernetes.io/dockerconfigjson", "kubernetes.io/tls":
	default:
		return errors.New("credential_materialization_metadata_invalid")
	}
	switch materialization.Status {
	case "planned", "verified", "outcome_unknown":
	default:
		return errors.New("credential_materialization_metadata_invalid")
	}
	if materialization.ResourceVersion != "" &&
		(!validCredentialMetadataToken(materialization.ResourceVersion, 256) ||
			credentialScalarContainsInlineMaterial(materialization.ResourceVersion)) {
		return errors.New("credential_materialization_metadata_invalid")
	}
	if materialization.Digest != "" &&
		(strings.TrimSpace(materialization.Digest) != materialization.Digest ||
			!validCredentialSHA256Digest(materialization.Digest)) {
		return errors.New("credential_materialization_metadata_invalid")
	}
	if materialization.Fingerprint != "" &&
		(strings.TrimSpace(materialization.Fingerprint) != materialization.Fingerprint ||
			!validCredentialSHA256Digest(materialization.Fingerprint)) {
		return errors.New("credential_materialization_metadata_invalid")
	}
	if materialization.ExpiresAt != "" {
		if _, err := time.Parse(time.RFC3339, materialization.ExpiresAt); err != nil {
			return errors.New("credential_materialization_metadata_invalid")
		}
	}
	if materialization.ContextKind != "" {
		if materialization.ContextKind != "helm-native" || !validNativeHelmSocketPath(materialization.ContextSocket) || materialization.SecretType != "" {
			return errors.New("credential_materialization_metadata_invalid")
		}
	}
	if materialization.ErrorCategory != "" && !validCredentialErrorCategory(materialization.ErrorCategory) {
		return errors.New("credential_materialization_metadata_invalid")
	}
	for _, key := range materialization.Keys {
		if !validCredentialMetadataToken(key, 253) || credentialScalarContainsInlineMaterial(key) {
			return errors.New("credential_materialization_metadata_invalid")
		}
	}
	if materialization.PreviousVersionID != "" &&
		(!strings.HasPrefix(materialization.PreviousVersionID, "credver_") ||
			!validLowerHex(strings.TrimPrefix(materialization.PreviousVersionID, "credver_"), 24)) {
		return errors.New("credential_materialization_metadata_invalid")
	}
	return nil
}

func validCredentialMetadataToken(value string, maxLength int) bool {
	if value == "" || len(value) > maxLength || strings.TrimSpace(value) != value {
		return false
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '_' || char == '-' ||
			char == '.' || char == ':' || char == '/' {
			continue
		}
		return false
	}
	return true
}

func validCredentialErrorCategory(category string) bool {
	if category == "" || len(category) > 128 {
		return false
	}
	for _, char := range category {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '_' {
			return false
		}
	}
	return true
}

func (h *GatewayHub) resolveExplicitCredentials(
	auth TokenAuth,
	cluster string,
	instance string,
	instruction string,
	names []string,
	action GatewayAction,
	skill string,
	credentialMode string,
	sqlCredential string,
) ([]authorizedCredentialMaterialization, error) {
	autoDatabase := skill == "dosql" && strings.TrimSpace(credentialMode) == "auto"
	autoHelm := !autoDatabase && automaticHelmCredentialEligible(action, skill)
	if len(names) == 0 && !autoDatabase && !autoHelm {
		return nil, nil
	}
	if !h.store.UserCanUseTarget(auth.UserID, cluster, instance) {
		return nil, ErrCredentialForbidden
	}
	// A change task pulls its Chart with the Helm credential already bound to
	// the target, so the operator does not name again what authorization has
	// already settled. Naming a registry, TLS or opaque credential does not
	// withdraw that: those write a Secret into the namespace and so must be
	// asked for by name, while the Chart download keeps the target's own Helm
	// credential. Discovery runs before the checks below, because a target with
	// no Helm credential must keep downloading a public Chart anonymously.
	if autoHelm {
		entries, err := h.store.ListCredentialAccess(auth.UserID, cluster, instance)
		if err != nil {
			return nil, err
		}
		// Explicit Helm references narrow the candidates. Otherwise the Agent
		// selects among all target bindings once the actual Chart origin is known.
		if !credentialReferencesIncludeHelm(names, entries) {
			names = append(names, helmCredentialIDs(entries)...)
			if len(names) == 0 {
				return nil, nil
			}
		}
	}
	if credentialScalarContainsInlineMaterial(instruction) {
		return nil, ErrInlineCredentialMaterial
	}
	// DoSql binds the target's database credentials by stable ID, so same-named
	// resources owned by different users cannot make the selection ambiguous.
	// A named selection (doops sql --credential) narrows that set to one
	// credential, e.g. a migration account beside the application account.
	if autoDatabase {
		entries, err := h.store.ListCredentialAccess(auth.UserID, cluster, instance)
		if err != nil {
			return nil, err
		}
		if selected := strings.TrimSpace(sqlCredential); selected != "" {
			names = databaseCredentialIDs(credentialEntriesNamed(entries, selected))
			switch len(names) {
			case 0:
				return nil, ErrDoSQLCredentialNotBound
			case 1:
			default:
				// Two owners use the same name; the credential ID tells them apart.
				return nil, ErrCredentialAmbiguous
			}
		} else {
			names = databaseCredentialIDs(entries)
		}
		if len(names) == 0 {
			// Discovery only lists promoted credentials; report an unpromoted
			// database binding as a lifecycle gap, not a missing binding.
			unpromoted, err := h.store.HasUnpromotedDatabaseBinding(cluster, instance)
			if err != nil {
				return nil, err
			}
			if unpromoted {
				return nil, ErrCredentialVersionNotFound
			}
			return nil, ErrDoSQLDatabaseBindingMissing
		}
	}
	names = uniqueCredentialReferences(names)
	authorized := make([]authorizedCredentialMaterialization, 0, len(names))
	seenHelmIDs := make(map[string]struct{}, len(names))
	for _, name := range names {
		request := CredentialUseRequest{
			ActorID: auth.UserID,
			Scope:   CredentialGrantScope{Cluster: cluster, Instance: instance},
		}
		if isCredentialResourceIDReference(name) {
			request.CredentialID = name
		} else {
			request.Credential = name
		}
		resolved, err := h.store.AuthorizeCredentialUse(request)
		if err != nil {
			return nil, err
		}
		if resolved.Resource.Type == CredentialTypeHelmRepository {
			if _, seen := seenHelmIDs[resolved.Resource.ID]; seen {
				continue
			}
			seenHelmIDs[resolved.Resource.ID] = struct{}{}
		}
		use := credentialUseForType(resolved.Resource.Type)
		if skill == "dosql" {
			use = CredentialUseDatabase
		}
		if err := credentialTypeAllowsUse(resolved.Resource.Type, use); err != nil {
			return nil, err
		}
		effectiveScope := normalizeCredentialGrantScope(resolved.Grant.Scope)
		effectiveScope.Use = use
		authorized = append(authorized, authorizedCredentialMaterialization{
			Reference:  CredentialPlanReference{Name: resolved.Resource.Name, Scope: effectiveScope},
			Authorized: resolved,
		})
	}
	return authorized, nil
}

func uniqueCredentialReferences(names []string) []string {
	seen := make(map[string]struct{}, len(names))
	unique := make([]string, 0, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, exists := seen[name]; exists {
			continue
		}
		seen[name] = struct{}{}
		unique = append(unique, name)
	}
	return unique
}

// automaticHelmCredentialEligible reports whether this operation may use the
// target's bound Helm credential without the caller naming it.
//
// Only a change task qualifies (ActionExec is what an apply prompt resolves
// to), and only the deployment form: dosql owns its own automatic database
// selection, and doweb reaches apply for website changes that pull no Chart.
// The automatic set is limited to helmPull because materializing one opens a
// process-local broker socket and touches no cluster object, while a registry,
// TLS or opaque credential writes a Secret into the namespace; creating one of
// those still requires the operator to ask for it by name.
func automaticHelmCredentialEligible(action GatewayAction, skill string) bool {
	switch strings.TrimSpace(skill) {
	case "dosql", "doweb":
		return false
	}
	return action == ActionExec
}

// helmCredentialIDs returns the stable IDs of the active Helm repository
// credentials among entries, so two resources sharing a name cannot make the
// automatic selection ambiguous.
func helmCredentialIDs(entries []CredentialListEntry) []string {
	seen := make(map[string]struct{}, len(entries))
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.Type != CredentialTypeHelmRepository || entry.State != CredentialStateActive {
			continue
		}
		for _, binding := range entry.Grants {
			use := binding.Scope.Use
			if use == "" {
				use = credentialUseForType(entry.Type)
			}
			if use != CredentialUseHelmPull {
				continue
			}
			if _, exists := seen[entry.ID]; !exists {
				ids = append(ids, entry.ID)
				seen[entry.ID] = struct{}{}
			}
			break
		}
	}
	return ids
}

// credentialReferencesIncludeHelm reports whether names already pick a Helm
// repository credential, by stable ID or by name. A task that names its own
// Helm credential has settled which repository the Chart comes from, so
// discovery must neither add a second one nor report ambiguity.
func credentialReferencesIncludeHelm(names []string, entries []CredentialListEntry) bool {
	helm := make(map[string]struct{}, len(entries))
	for _, id := range helmCredentialIDs(entries) {
		helm[id] = struct{}{}
	}
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		for _, entry := range credentialEntriesNamed(entries, name) {
			if _, ok := helm[entry.ID]; ok {
				return true
			}
		}
	}
	return false
}

func databaseCredentialIDs(entries []CredentialListEntry) []string {
	seen := make(map[string]struct{}, len(entries))
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.Type != CredentialTypeOpaque || entry.State != CredentialStateActive {
			continue
		}
		for _, binding := range entry.Grants {
			use := binding.Scope.Use
			if use == "" {
				use = credentialUseForType(entry.Type)
			}
			if use != CredentialUseDatabase {
				continue
			}
			if _, exists := seen[entry.ID]; !exists {
				ids = append(ids, entry.ID)
				seen[entry.ID] = struct{}{}
			}
			break
		}
	}
	return ids
}

// credentialEntriesNamed keeps the entries whose resource name or ID is reference.
func credentialEntriesNamed(entries []CredentialListEntry, reference string) []CredentialListEntry {
	var matches []CredentialListEntry
	for _, entry := range entries {
		if entry.ID == reference || entry.Name == reference {
			matches = append(matches, entry)
		}
	}
	return matches
}

func isCredentialResourceIDReference(value string) bool {
	const prefix = "cred_"
	return strings.HasPrefix(value, prefix) && validLowerHex(strings.TrimPrefix(value, prefix), 24)
}

func (h *GatewayHub) materializeExplicitCredentialApply(
	ctx context.Context,
	auth TokenAuth,
	sessionID string,
	authorized []authorizedCredentialMaterialization,
) (credentialApplyPreparation, error) {
	preparation := credentialApplyPreparation{
		Bindings:  make([]api.AgentCredentialBinding, 0, len(authorized)),
		completed: make([]completedCredentialMaterialization, 0, len(authorized)),
	}
	for _, item := range authorized {
		cluster := item.Reference.Scope.Cluster
		instance := item.Reference.Scope.Instance
		// The audit carries only validated identifiers: a scope whose
		// cluster/instance is request text keeps them and its scope JSON out.
		target := credentialTargetAudit(item.Reference.Scope, item.Authorized.Grant.ID)
		auditID, err := h.store.StartAudit(AuditEvent{
			UserID: auth.UserID, TokenID: auth.TokenID, Cluster: target.Cluster, Instance: target.Instance,
			CredentialID: item.Authorized.Resource.ID, VersionID: item.Authorized.Version.ID,
			GrantID: item.Authorized.Grant.ID, Scope: target.Scope,
			CorrelationID: "credapply_" + randomHex(12), Action: ActionCredentialUse,
			Session: sessionID, CommandSummary: "credential materialize use=" + string(item.Reference.Scope.Use),
			StartedAt: time.Now().UTC(),
		})
		if err != nil {
			_ = h.rollbackCredentialPrepare(ctx, preparation.completed)
			return credentialApplyPreparation{}, ErrCredentialAuditUnavailable
		}
		materialization, request, err := h.materializeCredentialVersion(
			ctx, cluster, instance, sessionID, item.Authorized.Resource, item.Authorized.Version,
			item.Authorized.Version.ID, item.Authorized.Grant.ID, item.Reference,
		)
		if err != nil {
			_ = h.store.FinishAudit(auditID, AuditFinish{
				Status: "error", Error: credentialOperationErrorCategory(err), EndedAt: time.Now().UTC(),
			})
			_ = h.rollbackCredentialPrepare(ctx, preparation.completed)
			return credentialApplyPreparation{}, err
		}
		completed := completedCredentialMaterialization{
			Materialization: materialization, Item: item, SessionID: sessionID,
		}
		preparation.completed = append(preparation.completed, completed)
		if err := h.store.RecordCredentialVerification(CredentialVerification{
			CredentialID: item.Authorized.Resource.ID, VersionID: item.Authorized.Version.ID,
			GrantID: item.Authorized.Grant.ID, Use: item.Reference.Scope.Use,
			Request: request, Evidence: materialization,
		}); err != nil {
			_ = h.rollbackCredentialPrepare(ctx, preparation.completed)
			return credentialApplyPreparation{}, err
		}
		if err := h.store.FinishAudit(auditID, AuditFinish{
			Status: "success", Scope: target.Scope, ResourceVersion: materialization.ResourceVersion,
			Digest: materialization.Digest, EndedAt: time.Now().UTC(),
		}); err != nil {
			_ = h.rollbackCredentialPrepare(ctx, preparation.completed)
			return credentialApplyPreparation{}, ErrCredentialAuditUnavailable
		}
		preparation.Bindings = append(preparation.Bindings, api.AgentCredentialBinding{
			Name: referenceName(item.Reference), Use: string(item.Reference.Scope.Use),
			Namespace: item.Reference.Scope.Namespace, ResourceName: materialization.ResourceName,
			DatabaseAssetID: item.Reference.Scope.DatabaseAssetID, EnvironmentID: item.Reference.Scope.EnvironmentID,
			DatabaseEngine: item.Reference.Scope.DatabaseEngine, DatabaseName: item.Reference.Scope.DatabaseName,
			SecretType: materialization.SecretType, Keys: append([]string(nil), materialization.Keys...),
			ResourceVersion: materialization.ResourceVersion, Digest: materialization.Digest,
			CredentialID: item.Authorized.Resource.ID, GrantID: item.Authorized.Grant.ID,
			VersionID: item.Authorized.Version.ID, HelmRelease: item.Reference.Scope.HelmRelease,
			HelmChart:   item.Reference.Scope.HelmChart,
			ContextKind: materialization.ContextKind, ContextSocket: materialization.ContextSocket,
		})
	}
	return preparation, nil
}

func referenceName(reference CredentialPlanReference) string {
	return strings.TrimSpace(reference.Name)
}

func credentialTypeAllowsUse(credentialType CredentialType, use CredentialUse) error {
	valid := (credentialType == CredentialTypeRegistry && use == CredentialUseImagePull) ||
		(credentialType == CredentialTypeTLS && use == CredentialUseTLS) ||
		(credentialType == CredentialTypeOpaque && (use == CredentialUseOpaqueSecret || use == CredentialUseDatabase)) ||
		(credentialType == CredentialTypeHelmRepository && use == CredentialUseHelmPull) ||
		(credentialType == CredentialTypeGitToken && use == CredentialUseGitCheckout) ||
		(credentialType == CredentialTypeModelProvider && use == CredentialUseModelProvider)
	if !valid {
		return errors.New("credential_use_mismatch")
	}
	return nil
}

func credentialUseForType(credentialType CredentialType) CredentialUse {
	switch credentialType {
	case CredentialTypeRegistry:
		return CredentialUseImagePull
	case CredentialTypeTLS:
		return CredentialUseTLS
	case CredentialTypeHelmRepository:
		return CredentialUseHelmPull
	case CredentialTypeGitToken:
		return CredentialUseGitCheckout
	case CredentialTypeOpaque:
		return CredentialUseOpaqueSecret
	case CredentialTypeModelProvider:
		return CredentialUseModelProvider
	default:
		return ""
	}
}

func (h *GatewayHub) rollbackCredentialPrepare(ctx context.Context, completed []completedCredentialMaterialization) error {
	var rollbackErr error
	for index := len(completed) - 1; index >= 0; index-- {
		entry := completed[index]
		request := credentialMaterializeRequest(entry.SessionID, entry.Item.Authorized, entry.Item.Reference)
		request.VersionID = entry.Materialization.VersionID
		if entry.Materialization.PreviousVersionID == "" {
			if err := h.cleanupCredentialVersion(ctx, request); err != nil {
				rollbackErr = err
			}
			continue
		}
		previous, err := h.store.CredentialVersionByID(request.CredentialID, entry.Materialization.PreviousVersionID)
		if err != nil {
			rollbackErr = err
			continue
		}
		if _, _, err := h.materializeCredentialVersion(
			ctx, request.Cluster, request.Instance, request.SessionID,
			entry.Item.Authorized.Resource, previous, request.VersionID,
			request.GrantID, entry.Item.Reference,
		); err != nil {
			rollbackErr = err
		}
	}
	return rollbackErr
}

// materializeOption adjusts one materialization without changing the call
// sites that do not need it.
type materializeOption func(*CredentialMaterializeRequest)

// materializeAuthfileOnly asks the Agent to restore only its OCI authfile and
// leave the Kubernetes Secret alone.
func materializeAuthfileOnly(request *CredentialMaterializeRequest) {
	request.AuthfileOnly = true
}

func (h *GatewayHub) materializeCredentialVersion(
	ctx context.Context,
	cluster string,
	instance string,
	sessionID string,
	resource CredentialResource,
	version CredentialVersion,
	expectedPreviousVersionID string,
	grantID string,
	ref CredentialPlanReference,
	options ...materializeOption,
) (CredentialMaterialization, CredentialMaterializeRequest, error) {
	return h.materializeCredentialVersionAttempt(
		ctx, cluster, instance, sessionID, resource, version,
		expectedPreviousVersionID, grantID, ref, true, options...,
	)
}

func (h *GatewayHub) materializeCredentialVersionAttempt(
	ctx context.Context,
	cluster string,
	instance string,
	sessionID string,
	resource CredentialResource,
	version CredentialVersion,
	expectedPreviousVersionID string,
	grantID string,
	ref CredentialPlanReference,
	recoverBindingFailure bool,
	options ...materializeOption,
) (CredentialMaterialization, CredentialMaterializeRequest, error) {
	payload, err := h.store.ResolveCredentialPayload(resource.ID, version.ID)
	if err != nil {
		return CredentialMaterialization{}, CredentialMaterializeRequest{}, err
	}
	defer clear(payload)
	requiredKeys := append([]string(nil), ref.Scope.RequiredKeys...)
	if len(requiredKeys) == 0 {
		requiredKeys, err = credentialRequiredKeys(resource.Type, payload)
	}
	if err != nil {
		return CredentialMaterialization{}, CredentialMaterializeRequest{}, err
	}
	request := CredentialMaterializeRequest{
		SessionID: sessionID, Cluster: cluster, Instance: instance,
		CredentialID: resource.ID, GrantID: grantID, VersionID: version.ID,
		CredentialType: resource.Type, Scope: ref.Scope, RequiredKeys: requiredKeys,
		Payload: json.RawMessage(payload),
	}
	for _, option := range options {
		if option != nil {
			option(&request)
		}
	}
	intentRequest := request
	intentRequest.Payload = nil
	intent, err := h.store.RecordCredentialMaterializationIntent(intentRequest)
	if err != nil {
		return CredentialMaterialization{}, intentRequest, err
	}
	toolArgs, err := credentialToolArguments(request)
	if err != nil {
		_ = h.store.MarkCredentialMaterializationFailed(intent, "credential_request_encode_failed")
		return CredentialMaterialization{}, intentRequest, err
	}
	resultText, err := h.RunInternalToolCall(ctx, cluster, instance, "doops_credential_materialize", toolArgs)
	if err != nil {
		_ = h.store.MarkCredentialMaterializationFailed(intent, credentialOperationErrorCategory(err))
		return CredentialMaterialization{}, intentRequest, err
	}
	var materialization CredentialMaterialization
	if err := json.Unmarshal([]byte(strings.TrimSpace(resultText)), &materialization); err != nil {
		_ = h.store.MarkCredentialMaterializationUnknown(intent.ID, "credential_materialization_result_invalid")
		return CredentialMaterialization{}, intentRequest, err
	}
	if materialization.CredentialID != resource.ID || materialization.VersionID != version.ID ||
		materialization.Namespace != ref.Scope.Namespace || materialization.Status != "verified" {
		_ = h.store.MarkCredentialMaterializationUnknown(intent.ID, "credential_materialization_result_mismatch")
		return CredentialMaterialization{}, intentRequest, ErrCredentialPayloadInvalid
	}
	materialization.GrantID = grantID
	if err := h.validateCredentialMaterializationResult(
		materialization, resource, grantID, ref, requiredKeys, expectedPreviousVersionID,
	); err != nil {
		_ = h.store.MarkCredentialMaterializationUnknown(intent.ID, err.Error())
		if recoverBindingFailure {
			if recoveryErr := h.recoverRejectedCredentialMaterialization(
				ctx, intentRequest, resource, version, expectedPreviousVersionID, grantID, ref,
			); recoveryErr != nil {
				return CredentialMaterialization{}, intentRequest, errors.New("credential_materialization_outcome_unknown")
			}
		}
		return CredentialMaterialization{}, intentRequest, err
	}
	if err := h.store.CompleteCredentialMaterialization(intent.ID, materialization); err != nil {
		_ = h.store.MarkCredentialMaterializationUnknown(intent.ID, "credential_materialization_inventory_failed")
		return CredentialMaterialization{}, intentRequest, err
	}
	return materialization, intentRequest, nil
}

func credentialMaterializeRequest(
	sessionID string,
	authorized AuthorizedCredentialUse,
	ref CredentialPlanReference,
) CredentialMaterializeRequest {
	return CredentialMaterializeRequest{
		SessionID: sessionID, Cluster: authorized.Grant.Scope.Cluster,
		Instance: authorized.Grant.Scope.Instance, CredentialID: authorized.Resource.ID,
		GrantID: authorized.Grant.ID, VersionID: authorized.Version.ID,
		CredentialType: authorized.Resource.Type, Scope: ref.Scope,
		RequiredKeys: append([]string(nil), ref.Scope.RequiredKeys...),
	}
}

func (h *GatewayHub) recoverRejectedCredentialMaterialization(
	ctx context.Context,
	request CredentialMaterializeRequest,
	resource CredentialResource,
	rejectedVersion CredentialVersion,
	expectedPreviousVersionID string,
	grantID string,
	ref CredentialPlanReference,
) error {
	if request.AuthfileOnly {
		// A restore changed no binding: the Kubernetes Secret is untouched and
		// the authfile entry it restores was already the Agent's. There is
		// nothing to roll back to and nothing to tear down.
		return nil
	}
	if expectedPreviousVersionID == "" {
		return h.cleanupCredentialVersion(ctx, request)
	}
	previous, err := h.store.CredentialVersionByID(resource.ID, expectedPreviousVersionID)
	if err != nil {
		return err
	}
	if _, _, err := h.materializeCredentialVersionAttempt(
		ctx, request.Cluster, request.Instance, request.SessionID, resource, previous,
		rejectedVersion.ID, grantID, ref, false,
	); err != nil {
		return err
	}
	return h.store.MarkCredentialMaterializationCleaned(request)
}

func (h *GatewayHub) validateCredentialMaterializationResult(
	materialization CredentialMaterialization,
	resource CredentialResource,
	grantID string,
	ref CredentialPlanReference,
	requiredKeys []string,
	expectedPreviousVersionID string,
) error {
	if err := validateCredentialMaterializationMetadata(materialization); err != nil {
		return err
	}
	if materialization.ResourceName != credentialSecretName(resource.ID, grantID) ||
		materialization.ResourceVersion == "" || materialization.ErrorCategory != "" ||
		!sameCredentialKeys(materialization.Keys, requiredKeys) {
		return errors.New("credential_materialization_metadata_invalid")
	}
	expectedSecretType := "Opaque"
	switch resource.Type {
	case CredentialTypeRegistry:
		expectedSecretType = "kubernetes.io/dockerconfigjson"
	case CredentialTypeTLS:
		expectedSecretType = "kubernetes.io/tls"
	case CredentialTypeHelmRepository, CredentialTypeModelProvider:
		expectedSecretType = ""
	case CredentialTypeOpaque, CredentialTypeGitToken:
	default:
		return errors.New("credential_materialization_metadata_invalid")
	}
	if materialization.SecretType != expectedSecretType {
		return errors.New("credential_materialization_metadata_invalid")
	}
	if resource.Type == CredentialTypeHelmRepository {
		if materialization.ContextKind != "helm-native" || !validNativeHelmSocketPath(materialization.ContextSocket) {
			return errors.New("credential_materialization_metadata_invalid")
		}
	} else if materialization.ContextKind != "" || materialization.ContextSocket != "" {
		return errors.New("credential_materialization_metadata_invalid")
	}
	switch resource.Type {
	case CredentialTypeRegistry, CredentialTypeHelmRepository, CredentialTypeGitToken, CredentialTypeModelProvider:
		if resource.Type != CredentialTypeRegistry && materialization.Digest == "" {
			return errors.New("credential_materialization_metadata_invalid")
		}
		if resource.Type == CredentialTypeRegistry &&
			(ref.Scope.ImageRepository != "" || ref.Scope.ImageDigest != "") && materialization.Digest == "" {
			return errors.New("credential_materialization_metadata_invalid")
		}
		if materialization.Fingerprint != "" || materialization.ExpiresAt != "" {
			return errors.New("credential_materialization_metadata_invalid")
		}
	case CredentialTypeTLS:
		if materialization.Digest != "" || materialization.Fingerprint == "" || materialization.ExpiresAt == "" {
			return errors.New("credential_materialization_metadata_invalid")
		}
	case CredentialTypeOpaque:
		if materialization.Digest != "" || materialization.Fingerprint != "" || materialization.ExpiresAt != "" {
			return errors.New("credential_materialization_metadata_invalid")
		}
	}
	if materialization.PreviousVersionID != "" &&
		materialization.PreviousVersionID != expectedPreviousVersionID {
		return errors.New("credential_materialization_metadata_invalid")
	}
	return nil
}

func credentialToolArguments(request CredentialMaterializeRequest) (map[string]any, error) {
	encoded, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	var arguments map[string]any
	if err := json.Unmarshal(encoded, &arguments); err != nil {
		return nil, err
	}
	return arguments, nil
}

func (h *GatewayHub) handleCredentialVerify(
	w http.ResponseWriter,
	r *http.Request,
	auth TokenAuth,
	resource CredentialResource,
) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	if !h.canVerifyCredential(auth.UserID, resource) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	var body credentialVerifyHTTPBody
	if err := decodeCredentialJSON(w, r, &body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	body.VersionID = strings.TrimSpace(body.VersionID)
	body.SessionID = strings.TrimSpace(body.SessionID)
	// Verification only selects the target; each grant is verified with the scope
	// stored when it was bound, so callers never restate namespace, use or keys.
	body.Scope = CredentialGrantScope{
		Cluster: strings.TrimSpace(body.Scope.Cluster), Instance: strings.TrimSpace(body.Scope.Instance),
	}
	if body.VersionID == "" || validateSession(body.SessionID) != nil ||
		body.Scope.Cluster == "" || body.Scope.Instance == "" {
		http.Error(w, "invalid credential verify request", http.StatusBadRequest)
		return
	}
	if !h.store.UserCanUseTarget(auth.UserID, body.Scope.Cluster, body.Scope.Instance) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	unlock, err := h.acquireCredentialMutationLocks(r.Context(), []string{resource.ID})
	if err != nil {
		http.Error(w, "operation canceled", http.StatusConflict)
		return
	}
	defer unlock()
	if h.store.validateUserTokenAuth(auth) != nil || !h.canVerifyCredential(auth.UserID, resource) ||
		!h.store.UserCanUseTarget(auth.UserID, body.Scope.Cluster, body.Scope.Instance) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	version, err := h.store.CredentialVersionByID(resource.ID, body.VersionID)
	if err != nil {
		http.Error(w, credentialErrorCategory(err), credentialHTTPStatus(err))
		return
	}
	if version.State != CredentialVersionStaged {
		http.Error(w, "credential_version_not_staged", http.StatusConflict)
		return
	}
	grants, err := h.store.CredentialGrantsForContext(resource.ID, body.Scope)
	if err != nil {
		http.Error(w, credentialErrorCategory(err), credentialHTTPStatus(err))
		return
	}
	active, activeErr := h.store.ActiveCredentialVersion(resource.ID)
	if activeErr != nil && !errors.Is(activeErr, ErrCredentialVersionNotFound) {
		http.Error(w, credentialErrorCategory(activeErr), credentialHTTPStatus(activeErr))
		return
	}
	auditID, ok := h.requireCredentialAudit(w, auth, ActionCredentialVerify, resource.ID, "credential verify")
	if !ok {
		return
	}
	// A verification runs on one target, so a failure is audited against that
	// target the same way a success is.
	failure := credentialTargetAudit(body.Scope, "")
	failure.VersionID = version.ID
	evidence := make([]credentialVerificationEvidence, 0, len(grants))
	verifications := make([]CredentialVerification, 0, len(grants))
	var auditScope CredentialGrantScope
	for index, grant := range grants {
		scope := grant.Scope
		if scope.Use == "" {
			scope.Use = credentialUseForType(resource.Type)
		}
		if index == 0 {
			auditScope = scope
		}
		failure = credentialTargetAudit(scope, grant.ID)
		failure.VersionID = version.ID
		ref := CredentialPlanReference{Name: resource.Name, Scope: scope}
		expectedPreviousVersionID := ""
		if activeErr == nil {
			expectedPreviousVersionID = active.ID
		}
		observed, request, err := h.materializeCredentialVersion(
			r.Context(), body.Scope.Cluster, body.Scope.Instance, body.SessionID,
			resource, version, expectedPreviousVersionID, grant.ID, ref,
		)
		if err != nil {
			h.writeCredentialVerificationError(w, auditID, failure, credentialOperationErrorCategory(err), http.StatusBadGateway)
			return
		}
		if activeErr == nil {
			if _, _, err := h.materializeCredentialVersion(
				r.Context(), body.Scope.Cluster, body.Scope.Instance, body.SessionID,
				resource, active, version.ID, grant.ID, ref,
			); err != nil {
				h.writeCredentialVerificationError(w, auditID, failure, "credential_verification_outcome_unknown", http.StatusBadGateway)
				return
			}
			if err := h.store.MarkCredentialMaterializationCleaned(request); err != nil {
				h.writeCredentialVerificationError(w, auditID, failure, "credential_verification_outcome_unknown", http.StatusBadGateway)
				return
			}
		} else if err := h.cleanupCredentialVersion(r.Context(), request); err != nil {
			h.writeCredentialVerificationError(w, auditID, failure, "credential_verification_outcome_unknown", http.StatusBadGateway)
			return
		}
		verifications = append(verifications, CredentialVerification{
			CredentialID: resource.ID, VersionID: version.ID, GrantID: grant.ID,
			Use: scope.Use, Request: request, Evidence: observed,
		})
		evidence = append(evidence, credentialVerificationEvidence{
			Status: observed.Status, ResourceVersion: observed.ResourceVersion,
			Digest: observed.Digest, ErrorCategory: observed.ErrorCategory,
		})
	}
	// The success audit keeps only validated identifiers, like the failure
	// target above: a verify whose request target is request text writes no
	// cluster, instance or scope column.
	success := credentialTargetAudit(body.Scope, "")
	success.Status = "success"
	success.VersionID = version.ID
	if success.Cluster != "" && success.Instance != "" && validateCredentialGrantScope(auditScope) == nil {
		success.Scope = &auditScope
	}
	success.EndedAt = time.Now().UTC()
	if err := h.store.RecordCredentialVerificationsAudited(verifications, auditID, success); err != nil {
		h.writeCredentialVerificationError(w, auditID, failure, "credential_verification_persist_failed", http.StatusInternalServerError)
		return
	}
	writeJSONHTTP(w, map[string]any{"status": "verified", "evidence": evidence})
}

func (h *GatewayHub) writeCredentialVerificationError(
	w http.ResponseWriter,
	auditID int64,
	target AuditFinish,
	category string,
	status int,
) {
	if err := h.finishCredentialTargetAudit(auditID, target, "error", category); err != nil {
		http.Error(w, "credential_audit_unavailable", http.StatusInternalServerError)
		return
	}
	http.Error(w, category, status)
}

func (h *GatewayHub) cleanupCredentialVersion(ctx context.Context, request CredentialMaterializeRequest) error {
	arguments := map[string]any{
		"session_id": request.SessionID, "credential_id": request.CredentialID,
		"grant_id": request.GrantID, "version_id": request.VersionID,
		"namespace": request.Scope.Namespace, "workload": request.Scope.Workload,
		"credential_type": request.CredentialType,
	}
	if request.AuthfileOnly {
		// The restore owns only the Agent's authfile. The Kubernetes Secret
		// belongs to the workload pulling with it, so a failed restore must
		// not remove it.
		arguments["authfile_only"] = true
	}
	if _, err := h.RunInternalToolCall(ctx, request.Cluster, request.Instance, "doops_credential_cleanup", arguments); err != nil {
		return err
	}
	return h.store.MarkCredentialMaterializationCleaned(request)
}

func (h *GatewayHub) cleanupCredentialMaterializationContext(
	ctx context.Context,
	request CredentialMaterializeRequest,
) error {
	arguments := map[string]any{
		"session_id": request.SessionID, "credential_id": request.CredentialID,
		"grant_id": request.GrantID, "version_id": request.VersionID, "any_version": true,
		"namespace": request.Scope.Namespace, "workload": request.Scope.Workload,
		"credential_type": request.CredentialType,
	}
	if _, err := h.RunInternalToolCall(ctx, request.Cluster, request.Instance, "doops_credential_cleanup", arguments); err != nil {
		return err
	}
	return h.store.MarkCredentialMaterializationContextCleaned(request)
}

func (h *GatewayHub) cleanupCredentialGrantMaterializations(
	ctx context.Context,
	credentialID string,
	grantID string,
) error {
	materializations, err := h.store.CredentialMaterializations(credentialID, "", grantID)
	if err != nil {
		return err
	}
	_, err = h.cleanupCredentialMaterializationContexts(ctx, materializations)
	return err
}

// cleanupCredentialMaterializationContexts cleans every context in records,
// once each. A failed context does not stop the others, so one unreachable
// target does not leave Secrets behind on the reachable ones. It returns the
// sorted cluster/instance of every target that still holds an uncleaned context.
func (h *GatewayHub) cleanupCredentialMaterializationContexts(
	ctx context.Context,
	records []CredentialMaterializationRecord,
) ([]string, error) {
	var failures []error
	var unfinished []string
	for _, request := range pendingCredentialCleanups(records) {
		if err := h.cleanupCredentialMaterializationContext(ctx, request); err != nil {
			failures = append(failures, err)
			if target := request.Cluster + "/" + request.Instance; !containsString(unfinished, target) {
				unfinished = append(unfinished, target)
			}
		}
	}
	sort.Strings(unfinished)
	return unfinished, errors.Join(failures...)
}

// uncleanedCredentialTargets lists, sorted, the cluster/instance of every
// target that still holds a context of the credential needing a cleanup. It is
// best effort: a failed read returns no targets and the caller still fails.
func (h *GatewayHub) uncleanedCredentialTargets(credentialID string) []string {
	records, err := h.store.CredentialMaterializations(credentialID, "", "")
	if err != nil {
		return nil
	}
	var targets []string
	for _, record := range records {
		if target := record.Request.Cluster + "/" + record.Request.Instance; !containsString(targets, target) {
			targets = append(targets, target)
		}
	}
	sort.Strings(targets)
	return targets
}

// pendingCredentialCleanups keeps one request per materialization context.
// The records come from CredentialMaterializations, which leaves out contexts
// that provably own nothing, so an offline target whose attempts all left it
// unchanged gets no cleanup call and does not block revocation.
func pendingCredentialCleanups(records []CredentialMaterializationRecord) []CredentialMaterializeRequest {
	var pending []CredentialMaterializeRequest
	seen := make(map[string]struct{})
	for _, record := range records {
		key := credentialMaterializationContextKey(record.Request)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		pending = append(pending, record.Request)
	}
	return pending
}

// credentialCleanupFailure names why a cleanup did not finish. A target that
// never received the call is reported as offline, busy or queued; anything that
// may have reached the Agent keeps the caller's outcome-unknown category, and
// one such failure among several decides the result.
func credentialCleanupFailure(err error, unknown string) (string, int) {
	failures := []error{err}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		failures = joined.Unwrap()
	}
	category, status := "", 0
	for _, failure := range failures {
		next, nextStatus := credentialCleanupFailureCategory(failure, unknown)
		if next == unknown {
			return unknown, nextStatus
		}
		if category == "" {
			category, status = next, nextStatus
		}
	}
	if category == "" {
		return unknown, http.StatusBadGateway
	}
	return category, status
}

func credentialCleanupFailureCategory(err error, unknown string) (string, int) {
	if errors.Is(err, errExecutionOutcomeUnknown) {
		return unknown, http.StatusBadGateway
	}
	message := strings.ToLower(err.Error())
	switch {
	case strings.HasPrefix(message, "target offline"):
		return "target_offline", http.StatusServiceUnavailable
	case strings.HasPrefix(message, "target busy"):
		return "target_busy", http.StatusConflict
	case strings.HasPrefix(message, "target queue full"):
		return "target_queue_full", http.StatusConflict
	case strings.HasPrefix(message, "target queue timeout"):
		return "target_queue_timeout", http.StatusConflict
	}
	return unknown, http.StatusBadGateway
}

func credentialReferenceFromRequest(request CredentialMaterializeRequest) CredentialPlanReference {
	return CredentialPlanReference{Scope: request.Scope}
}

func credentialMaterializationContextKey(request CredentialMaterializeRequest) string {
	scopeJSON, _ := json.Marshal(normalizeCredentialGrantScope(request.Scope))
	return strings.Join([]string{
		request.Cluster, request.Instance, request.GrantID, string(scopeJSON),
	}, "\x00")
}

func credentialOperationErrorCategory(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, ErrCredentialVersionNotFound):
		return "credential_version_not_found"
	case errors.Is(err, ErrCredentialGrantDenied):
		category, _ := credentialGrantDenialCategory(err)
		return category
	case errors.Is(err, ErrCredentialForbidden):
		return "credential_forbidden"
	case errors.Is(err, ErrCredentialRevoked):
		return "credential_revoked"
	case errors.Is(err, ErrCredentialPayloadInvalid):
		return "credential_payload_invalid"
	case errors.Is(err, ErrCredentialAuditUnavailable):
		return "credential_audit_unavailable"
	}
	category := sanitizeCredentialCategory(err.Error())
	if category != "credential_operation_failed" {
		return category
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return "verification_transport_failed"
	case errors.Is(err, errExecutionOutcomeUnknown):
		// The call may have reached the Agent, so its text is not evidence
		// that the target was offline before dispatch.
		return "credential_operation_failed"
	case strings.Contains(strings.ToLower(err.Error()), "target offline"):
		return "target_offline"
	default:
		return "credential_operation_failed"
	}
}

func (h *GatewayHub) rollbackCredentialPromotion(
	ctx context.Context,
	resource CredentialResource,
	previous CredentialVersion,
	previousErr error,
	applied []CredentialVerification,
) error {
	var rollbackErr error
	for index := len(applied) - 1; index >= 0; index-- {
		verification := applied[index]
		if previousErr == nil {
			ref := credentialReferenceFromRequest(verification.Request)
			if _, _, err := h.materializeCredentialVersion(
				ctx, verification.Request.Cluster, verification.Request.Instance,
				verification.Request.SessionID, resource, previous,
				verification.Request.VersionID, verification.Request.GrantID, ref,
			); err != nil {
				rollbackErr = err
			}
			continue
		}
		if err := h.cleanupCredentialVersion(ctx, verification.Request); err != nil {
			rollbackErr = err
		}
	}
	return rollbackErr
}

func credentialRequiredKeys(credentialType CredentialType, payload json.RawMessage) ([]string, error) {
	switch credentialType {
	case CredentialTypeRegistry:
		return []string{".dockerconfigjson"}, nil
	case CredentialTypeTLS:
		return []string{"tls.crt", "tls.key"}, nil
	case CredentialTypeHelmRepository, CredentialTypeModelProvider:
		return nil, nil
	case CredentialTypeGitToken:
		return []string{"password", "url", "username"}, nil
	case CredentialTypeOpaque:
		var value struct {
			Data map[string]string `json:"data"`
		}
		if err := json.Unmarshal(payload, &value); err != nil {
			return nil, ErrCredentialPayloadInvalid
		}
		return normalizedCredentialKeys(mapKeys(value.Data)), nil
	default:
		return nil, ErrCredentialPayloadInvalid
	}
}
