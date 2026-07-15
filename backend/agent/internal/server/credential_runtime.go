package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"
)

type CredentialMaterialization struct {
	CredentialID      string   `json:"credentialId"`
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
}

type CredentialRun struct {
	APIVersion       string                      `json:"apiVersion"`
	Kind             string                      `json:"kind"`
	ID               string                      `json:"id"`
	Mode             string                      `json:"mode"`
	Cluster          string                      `json:"cluster"`
	Instance         string                      `json:"instance"`
	Template         string                      `json:"template"`
	Project          string                      `json:"project"`
	Environment      string                      `json:"environment"`
	MutationCount    int                         `json:"mutationCount"`
	Materializations []CredentialMaterialization `json:"materializations"`
	CreatedAt        time.Time                   `json:"createdAt"`
}

type credentialPrepareHTTPBody struct {
	Cluster         string `json:"cluster"`
	Instance        string `json:"instance"`
	SessionID       string `json:"session_id"`
	WorkflowPath    string `json:"workflow_path"`
	WorkspaceCommit string `json:"workspace_commit"`
	Mode            string `json:"mode"`
}

type credentialVerifyHTTPBody struct {
	credentialPrepareHTTPBody
	VersionID string `json:"version_id"`
}

type authorizedCredentialMaterialization struct {
	Reference  CredentialPlanReference
	Authorized AuthorizedCredentialUse
}

func (h *GatewayHub) HandleCredentialPrepare(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	auth, err := h.authenticateUser(r)
	if err != nil {
		h.writeUserAuthError(w, r)
		return
	}
	var body credentialPrepareHTTPBody
	if err := decodeCredentialJSON(w, r, &body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	body.Cluster = strings.TrimSpace(body.Cluster)
	body.Instance = strings.TrimSpace(body.Instance)
	body.SessionID = strings.TrimSpace(body.SessionID)
	body.WorkflowPath = strings.TrimSpace(body.WorkflowPath)
	body.WorkspaceCommit = strings.TrimSpace(body.WorkspaceCommit)
	body.Mode = strings.ToLower(strings.TrimSpace(body.Mode))
	if body.Cluster == "" || body.Instance == "" || body.SessionID == "" ||
		body.WorkflowPath == "" || !validCredentialWorkspaceCommit(body.WorkspaceCommit) ||
		(body.Mode != "dry-run" && body.Mode != "apply") {
		http.Error(w, "invalid credential prepare request", http.StatusBadRequest)
		return
	}
	if !h.store.UserCan(auth.UserID, body.Cluster, body.Instance, ActionAsk) ||
		!h.store.UserCan(auth.UserID, body.Cluster, body.Instance, ActionCredentialUse) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	if body.Mode == "apply" {
		h.credentialMutationMu.Lock()
		defer h.credentialMutationMu.Unlock()
	}

	auditID, _ := h.store.StartAudit(AuditEvent{
		UserID:         auth.UserID,
		TokenID:        auth.TokenID,
		Cluster:        body.Cluster,
		Instance:       body.Instance,
		Action:         ActionCredentialUse,
		Session:        body.SessionID,
		CommandSummary: "credential prepare " + body.WorkflowPath + " mode=" + body.Mode,
		StartedAt:      time.Now().UTC(),
	})
	finishAudit := func(status, category string) {
		_ = h.store.FinishAudit(auditID, AuditFinish{
			Status:  status,
			Error:   category,
			EndedAt: time.Now().UTC(),
		})
	}

	plan, err := h.loadCredentialPlan(r.Context(), body)
	if err != nil {
		category := err.Error()
		finishAudit("error", category)
		http.Error(w, category, http.StatusBadGateway)
		return
	}
	plan.CredentialRefs, err = h.resolveCredentialPlanReferences(auth.UserID, plan)
	if err != nil {
		category := credentialErrorCategory(err)
		finishAudit("error", category)
		http.Error(w, category, credentialHTTPStatus(err))
		return
	}

	authorized := make([]authorizedCredentialMaterialization, 0, len(plan.CredentialRefs))
	for _, ref := range plan.CredentialRefs {
		resolved, err := h.store.AuthorizeCredentialUse(CredentialUseRequest{
			ActorID:      auth.UserID,
			CredentialID: ref.CredentialID,
			Credential:   ref.Name,
			Cluster:      plan.Cluster,
			Instance:     plan.Instance,
			Project:      plan.Project,
			Environment:  plan.Environment,
			Template:     plan.Template,
			Namespace:    ref.Namespace,
			Use:          ref.Use,
		})
		if err != nil {
			category := credentialErrorCategory(err)
			finishAudit("forbidden", category)
			http.Error(w, category, credentialHTTPStatus(err))
			return
		}
		authorized = append(authorized, authorizedCredentialMaterialization{Reference: ref, Authorized: resolved})
	}

	run := CredentialRun{
		APIVersion:       "doops.sh/v2",
		Kind:             "CredentialRun",
		ID:               "credrun_" + randomHex(12),
		Mode:             body.Mode,
		Cluster:          plan.Cluster,
		Instance:         plan.Instance,
		Template:         plan.Template,
		Project:          plan.Project,
		Environment:      plan.Environment,
		Materializations: make([]CredentialMaterialization, 0, len(authorized)),
		CreatedAt:        time.Now().UTC(),
	}
	completed := make([]completedCredentialMaterialization, 0, len(authorized))
	for _, item := range authorized {
		materialization := CredentialMaterialization{
			CredentialID: item.Authorized.Resource.ID,
			VersionID:    item.Authorized.Version.ID,
			Namespace:    item.Reference.Namespace,
			Status:       "planned",
		}
		if body.Mode == "apply" {
			var request CredentialMaterializeRequest
			materialization, request, err = h.materializeCredentialVersion(
				r.Context(), body.Cluster, body.Instance, body.SessionID,
				item.Authorized.Resource, item.Authorized.Version, item.Reference,
			)
			if err != nil {
				category := "credential_materialization_failed"
				if rollbackErr := h.rollbackCredentialPrepare(r.Context(), completed); rollbackErr != nil {
					category = "credential_materialization_outcome_unknown"
				}
				finishAudit("error", category)
				http.Error(w, category, http.StatusBadGateway)
				return
			}
			if err := h.store.RecordCredentialVerification(CredentialVerification{
				CredentialID: item.Authorized.Resource.ID,
				VersionID:    item.Authorized.Version.ID,
				GrantID:      item.Authorized.Grant.ID,
				Use:          item.Reference.Use,
				Request:      request,
				Evidence:     materialization,
			}); err != nil {
				completed = append(completed, completedCredentialMaterialization{
					Materialization: materialization,
					Item:            item,
					SessionID:       body.SessionID,
				})
				category := "credential_verification_persist_failed"
				if rollbackErr := h.rollbackCredentialPrepare(r.Context(), completed); rollbackErr != nil {
					category = "credential_materialization_outcome_unknown"
				}
				finishAudit("error", category)
				http.Error(w, category, http.StatusInternalServerError)
				return
			}
			completed = append(completed, completedCredentialMaterialization{
				Materialization: materialization,
				Item:            item,
				SessionID:       body.SessionID,
			})
			run.MutationCount++
		}
		sort.Strings(materialization.Keys)
		materialization.PreviousVersionID = ""
		run.Materializations = append(run.Materializations, materialization)
	}
	finishAudit("success", "")
	writeJSONHTTP(w, run)
}

type completedCredentialMaterialization struct {
	Materialization CredentialMaterialization
	Item            authorizedCredentialMaterialization
	SessionID       string
}

func (h *GatewayHub) rollbackCredentialPrepare(ctx context.Context, completed []completedCredentialMaterialization) error {
	var rollbackErr error
	for index := len(completed) - 1; index >= 0; index-- {
		entry := completed[index]
		materialization := entry.Materialization
		request := CredentialMaterializeRequest{
			SessionID:          entry.SessionID,
			Cluster:            entry.Item.Authorized.Grant.Cluster,
			Instance:           entry.Item.Authorized.Grant.Instance,
			CredentialID:       entry.Item.Authorized.Resource.ID,
			VersionID:          materialization.VersionID,
			CredentialType:     entry.Item.Authorized.Resource.Type,
			Use:                entry.Item.Reference.Use,
			Namespace:          entry.Item.Reference.Namespace,
			Workload:           entry.Item.Reference.Workload,
			RegistryRepository: entry.Item.Reference.RegistryRepository,
			RegistryReference:  entry.Item.Reference.RegistryReference,
			RequiredKeys:       entry.Item.Reference.RequiredKeys,
		}
		if materialization.PreviousVersionID == "" {
			if err := h.cleanupCredentialVersion(ctx, request); err != nil {
				rollbackErr = err
			}
			continue
		}
		previous, err := h.store.CredentialVersionByID(request.CredentialID, materialization.PreviousVersionID)
		if err != nil {
			rollbackErr = err
			continue
		}
		if _, _, err := h.materializeCredentialVersion(
			ctx,
			request.Cluster,
			request.Instance,
			request.SessionID,
			entry.Item.Authorized.Resource,
			previous,
			entry.Item.Reference,
		); err != nil {
			rollbackErr = err
		}
	}
	return rollbackErr
}

func (h *GatewayHub) materializeCredentialVersion(
	ctx context.Context,
	cluster string,
	instance string,
	sessionID string,
	resource CredentialResource,
	version CredentialVersion,
	ref CredentialPlanReference,
) (CredentialMaterialization, CredentialMaterializeRequest, error) {
	payload, err := h.store.ResolveCredentialPayload(resource.ID, version.ID)
	if err != nil {
		return CredentialMaterialization{}, CredentialMaterializeRequest{}, err
	}
	defer clear(payload)
	requiredKeys := append([]string(nil), ref.RequiredKeys...)
	if len(requiredKeys) == 0 {
		requiredKeys, err = credentialRequiredKeys(resource.Type, payload)
	}
	if err != nil {
		return CredentialMaterialization{}, CredentialMaterializeRequest{}, err
	}
	request := CredentialMaterializeRequest{
		SessionID:          sessionID,
		Cluster:            cluster,
		Instance:           instance,
		CredentialID:       resource.ID,
		VersionID:          version.ID,
		CredentialType:     resource.Type,
		Use:                ref.Use,
		Namespace:          ref.Namespace,
		Workload:           ref.Workload,
		RegistryRepository: ref.RegistryRepository,
		RegistryReference:  ref.RegistryReference,
		RequiredKeys:       requiredKeys,
		Payload:            json.RawMessage(payload),
	}
	toolArgs, err := credentialToolArguments(request)
	if err != nil {
		return CredentialMaterialization{}, CredentialMaterializeRequest{}, err
	}
	resultText, err := h.RunInternalToolCall(ctx, cluster, instance, "doops_credential_materialize", toolArgs)
	if err != nil {
		return CredentialMaterialization{}, CredentialMaterializeRequest{}, err
	}
	var materialization CredentialMaterialization
	if err := json.Unmarshal([]byte(strings.TrimSpace(resultText)), &materialization); err != nil {
		return CredentialMaterialization{}, CredentialMaterializeRequest{}, err
	}
	if materialization.CredentialID != resource.ID ||
		materialization.VersionID != version.ID ||
		materialization.Namespace != ref.Namespace ||
		materialization.Status != "verified" {
		return CredentialMaterialization{}, CredentialMaterializeRequest{}, ErrCredentialPayloadInvalid
	}
	request.Payload = nil
	return materialization, request, nil
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
	if !h.canRotateCredential(auth.UserID, resource) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	h.credentialMutationMu.Lock()
	defer h.credentialMutationMu.Unlock()
	var body credentialVerifyHTTPBody
	if err := decodeCredentialJSON(w, r, &body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	body.VersionID = strings.TrimSpace(body.VersionID)
	body.Cluster = strings.TrimSpace(body.Cluster)
	body.Instance = strings.TrimSpace(body.Instance)
	body.SessionID = strings.TrimSpace(body.SessionID)
	body.WorkflowPath = strings.TrimSpace(body.WorkflowPath)
	body.WorkspaceCommit = strings.TrimSpace(body.WorkspaceCommit)
	if body.VersionID == "" || body.Cluster == "" || body.Instance == "" || body.SessionID == "" ||
		body.WorkflowPath == "" || !validCredentialWorkspaceCommit(body.WorkspaceCommit) {
		http.Error(w, "invalid credential verify request", http.StatusBadRequest)
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
	plan, err := h.loadCredentialPlan(r.Context(), body.credentialPrepareHTTPBody)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	plan.CredentialRefs, err = h.resolveCredentialPlanReferences(auth.UserID, plan)
	if err != nil {
		http.Error(w, credentialErrorCategory(err), credentialHTTPStatus(err))
		return
	}
	var reference *CredentialPlanReference
	for index := range plan.CredentialRefs {
		if plan.CredentialRefs[index].CredentialID == resource.ID ||
			(plan.CredentialRefs[index].CredentialID == "" && plan.CredentialRefs[index].Name == resource.Name) {
			if reference != nil {
				http.Error(w, "credential_reference_ambiguous", http.StatusConflict)
				return
			}
			reference = &plan.CredentialRefs[index]
		}
	}
	if reference == nil {
		http.Error(w, "credential_reference_missing", http.StatusConflict)
		return
	}
	grants, err := h.store.CredentialGrantsForContext(resource.ID, *reference, plan)
	if err != nil {
		http.Error(w, credentialErrorCategory(err), credentialHTTPStatus(err))
		return
	}
	evidence, request, err := h.materializeCredentialVersion(
		r.Context(), body.Cluster, body.Instance, body.SessionID, resource, version, *reference,
	)
	if err != nil {
		http.Error(w, "credential_verification_failed", http.StatusBadGateway)
		return
	}
	active, activeErr := h.store.ActiveCredentialVersion(resource.ID)
	switch {
	case activeErr == nil:
		if _, _, err := h.materializeCredentialVersion(
			r.Context(), body.Cluster, body.Instance, body.SessionID, resource, active, *reference,
		); err != nil {
			http.Error(w, "credential_verification_restore_failed", http.StatusBadGateway)
			return
		}
	case errors.Is(activeErr, ErrCredentialVersionNotFound):
		if err := h.cleanupCredentialVersion(r.Context(), request); err != nil {
			http.Error(w, "credential_verification_cleanup_failed", http.StatusBadGateway)
			return
		}
	default:
		http.Error(w, credentialErrorCategory(activeErr), credentialHTTPStatus(activeErr))
		return
	}
	for _, grant := range grants {
		if err := h.store.RecordCredentialVerification(CredentialVerification{
			CredentialID: resource.ID,
			VersionID:    version.ID,
			GrantID:      grant.ID,
			Use:          reference.Use,
			Request:      request,
			Evidence:     evidence,
		}); err != nil {
			http.Error(w, "credential_verification_persist_failed", http.StatusInternalServerError)
			return
		}
	}
	writeJSONHTTP(w, map[string]any{
		"credential_id": resource.ID,
		"version_id":    version.ID,
		"verified":      true,
		"evidence":      evidence,
	})
}

func (h *GatewayHub) loadCredentialPlan(ctx context.Context, body credentialPrepareHTTPBody) (CredentialPlan, error) {
	planText, err := h.RunInternalToolCall(ctx, body.Cluster, body.Instance, "doops_credential_plan", map[string]any{
		"session_id":       body.SessionID,
		"workflow_path":    body.WorkflowPath,
		"workspace_commit": body.WorkspaceCommit,
	})
	if err != nil {
		return CredentialPlan{}, errors.New("credential_plan_failed")
	}
	var plan CredentialPlan
	if err := json.Unmarshal([]byte(strings.TrimSpace(planText)), &plan); err != nil {
		return CredentialPlan{}, errors.New("credential_plan_invalid")
	}
	if plan.Cluster != body.Cluster || plan.Instance != body.Instance {
		return CredentialPlan{}, errors.New("credential_target_mismatch")
	}
	return plan, nil
}

func (h *GatewayHub) resolveCredentialPlanReferences(actorID string, plan CredentialPlan) ([]CredentialPlanReference, error) {
	references := append([]CredentialPlanReference(nil), plan.CredentialRefs...)
	for _, bundleName := range plan.BundleRefs {
		bundle, err := h.store.ResolveCredentialBundle(actorID, bundleName)
		if err != nil {
			return nil, err
		}
		for _, item := range bundle.Items {
			resource, err := h.store.CredentialByID(item.CredentialID)
			if err != nil {
				return nil, err
			}
			references = append(references, CredentialPlanReference{
				CredentialID:       resource.ID,
				Name:               resource.Name,
				Use:                item.Use,
				Namespace:          item.Namespace,
				Workload:           item.Workload,
				RegistryRepository: item.RegistryRepository,
				RegistryReference:  item.RegistryReference,
				RequiredKeys:       append([]string(nil), item.RequiredKeys...),
			})
		}
	}
	return references, nil
}

func (h *GatewayHub) cleanupCredentialVersion(ctx context.Context, request CredentialMaterializeRequest) error {
	arguments := map[string]any{
		"session_id":    request.SessionID,
		"credential_id": request.CredentialID,
		"version_id":    request.VersionID,
		"namespace":     request.Namespace,
		"workload":      request.Workload,
	}
	_, err := h.RunInternalToolCall(ctx, request.Cluster, request.Instance, "doops_credential_cleanup", arguments)
	return err
}

func credentialReferenceFromRequest(request CredentialMaterializeRequest) CredentialPlanReference {
	return CredentialPlanReference{
		CredentialID:       request.CredentialID,
		Use:                request.Use,
		Namespace:          request.Namespace,
		Workload:           request.Workload,
		RegistryRepository: request.RegistryRepository,
		RegistryReference:  request.RegistryReference,
	}
}

func credentialMaterializationContextKey(request CredentialMaterializeRequest) string {
	return strings.Join([]string{
		request.Cluster,
		request.Instance,
		request.Namespace,
		string(request.Use),
		request.Workload.Kind,
		request.Workload.Name,
		request.RegistryRepository,
		request.RegistryReference,
	}, "\x00")
}

func (h *GatewayHub) rollbackCredentialPromotion(
	ctx context.Context,
	resource CredentialResource,
	previous CredentialVersion,
	previousErr error,
	applied []CredentialVerification,
) error {
	for index := len(applied) - 1; index >= 0; index-- {
		request := applied[index].Request
		if previousErr == nil {
			if _, _, err := h.materializeCredentialVersion(
				ctx,
				request.Cluster,
				request.Instance,
				request.SessionID,
				resource,
				previous,
				credentialReferenceFromRequest(request),
			); err != nil {
				return err
			}
			continue
		}
		if err := h.cleanupCredentialVersion(ctx, request); err != nil {
			return err
		}
	}
	return nil
}

func credentialRequiredKeys(credentialType CredentialType, payload json.RawMessage) ([]string, error) {
	if credentialType != CredentialTypeOpaque {
		return nil, nil
	}
	var value struct {
		Data map[string]string `json:"data"`
	}
	if err := decodeStrictCredentialPayload(payload, &value); err != nil || len(value.Data) == 0 {
		return nil, ErrCredentialPayloadInvalid
	}
	return mapKeys(value.Data), nil
}

func validCredentialWorkspaceCommit(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}
