package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

type trustedReconciliationAdmission struct {
	AllowedTools []string                     `json:"allowedTools"`
	Context      trustedReconciliationContext `json:"context"`
}

const trustedReconciliationContextSchema = "doops.reconciliation-context/v1"

type trustedReconciliationContext struct {
	SchemaVersion      string                                     `json:"schemaVersion"`
	OperationID        string                                     `json:"operationId"`
	ExecutionMode      string                                     `json:"executionMode"`
	MutationAuthorized bool                                       `json:"mutationAuthorized"`
	PlanDigest         string                                     `json:"planDigest"`
	PlanBindingDigest  string                                     `json:"planBindingDigest"`
	ContextDigest      string                                     `json:"contextDigest,omitempty"`
	Source             trustedReconciliationSource                `json:"source"`
	Capabilities       map[string]trustedReconciliationCapability `json:"capabilities"`
}

type trustedReconciliationSource struct {
	Repository      string `json:"repository"`
	Revision        string `json:"revision"`
	Branch          string `json:"branch,omitempty"`
	WorkspaceCommit string `json:"workspaceCommit"`
}

type trustedReconciliationCapability struct {
	Tool            string                 `json:"tool"`
	EvidenceKind    string                 `json:"evidenceKind"`
	EvidenceSubject string                 `json:"evidenceSubject"`
	CanonicalScope  map[string]interface{} `json:"canonicalScope"`
	ScopeDigest     string                 `json:"scopeDigest"`
}

func buildDoagentPromptParams(sessionID, prompt string, admission *trustedReconciliationAdmission, structured bool) map[string]interface{} {
	params := map[string]interface{}{
		"sessionId":    sessionID,
		"prompt":       prompt,
		"allowedTools": []string{},
	}
	if !structured || admission == nil {
		return params
	}
	params["allowedTools"] = append([]string(nil), admission.AllowedTools...)
	params["trustedReconciliationContext"] = admission.Context
	return params
}

func (c trustedReconciliationContext) ValidateDigest() error {
	copy := c
	copy.ContextDigest = ""
	digest, err := digestJSON(copy)
	if err != nil {
		return err
	}
	if c.ContextDigest != digest {
		return fmt.Errorf("trusted reconciliation context digest mismatch")
	}
	for key, capability := range c.Capabilities {
		scopeDigest, err := digestJSON(capability.CanonicalScope)
		if err != nil {
			return fmt.Errorf("digest capability %q scope: %w", key, err)
		}
		if capability.ScopeDigest != scopeDigest {
			return fmt.Errorf("trusted reconciliation capability %q scope digest mismatch", key)
		}
	}
	return nil
}

func buildTrustedReconciliationAdmission(sessionID, instruction, workspaceCommit string) (trustedReconciliationAdmission, error) {
	sessionID = strings.TrimSpace(sessionID)
	workspaceCommit = strings.TrimSpace(workspaceCommit)
	if sessionID == "" || !validCredentialWorkspaceCommit(workspaceCommit) {
		return trustedReconciliationAdmission{}, fmt.Errorf("trusted reconciliation identity is invalid")
	}

	var envelope struct {
		Task            string            `json:"task"`
		Skill           string            `json:"skill"`
		ExecutionMode   string            `json:"executionMode"`
		RunID           string            `json:"runId"`
		WorkflowPath    string            `json:"workflowPath"`
		WorkspaceCommit string            `json:"workspaceCommit"`
		Inputs          map[string]string `json:"inputs"`
		CredentialRun   json.RawMessage   `json:"credentialRun"`
		ResultContract  struct {
			APIVersion      string `json:"apiVersion"`
			Kind            string `json:"kind"`
			RequireEvidence bool   `json:"requireEvidence"`
		} `json:"resultContract"`
	}
	decoder := json.NewDecoder(strings.NewReader(instruction))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return trustedReconciliationAdmission{}, fmt.Errorf("invalid reconciliation instruction")
	}
	if envelope.Task != "execute-doops-cicd-workflow" || envelope.Skill != "$doops-cicd" ||
		(envelope.ExecutionMode != "dry-run" && envelope.ExecutionMode != "apply") ||
		strings.TrimSpace(envelope.RunID) == "" || strings.TrimSpace(envelope.WorkflowPath) == "" ||
		envelope.WorkspaceCommit != workspaceCommit ||
		envelope.ResultContract.APIVersion != "doops.sh/v2" ||
		envelope.ResultContract.Kind != "DeploymentRun" ||
		!envelope.ResultContract.RequireEvidence {
		return trustedReconciliationAdmission{}, fmt.Errorf("reconciliation instruction binding is invalid")
	}
	for key := range envelope.Inputs {
		lower := strings.ToLower(strings.TrimSpace(key))
		if credentialSecretLikeKeys[lower] {
			return trustedReconciliationAdmission{}, fmt.Errorf("sensitive workflow input %q is forbidden", key)
		}
	}

	root, err := workspacePath(sessionID)
	if err != nil {
		return trustedReconciliationAdmission{}, err
	}
	workflowData, err := readCredentialDeclarationFile(root, envelope.WorkflowPath)
	if err != nil {
		return trustedReconciliationAdmission{}, err
	}
	var workflow credentialDeploymentTemplate
	if err := strictCredentialYAMLDecode(workflowData, &workflow); err != nil {
		return trustedReconciliationAdmission{}, invalidCredentialDeclaration(err)
	}
	if err := validateTrustedWorkflowInputs(workflow.Spec.Parameters, envelope.Inputs); err != nil {
		return trustedReconciliationAdmission{}, err
	}
	revision, err := resolveTrustedRevision(workflow.Spec.Release.Source.Revision, envelope.Inputs)
	if err != nil {
		return trustedReconciliationAdmission{}, err
	}
	plan, err := parseCredentialPlan(root, envelope.WorkflowPath)
	if err != nil {
		return trustedReconciliationAdmission{}, err
	}
	source := trustedReconciliationSource{
		Repository:      strings.TrimSpace(workflow.Spec.Release.Source.Repository),
		Revision:        revision,
		Branch:          strings.TrimSpace(workflow.Spec.Release.Source.Branch),
		WorkspaceCommit: workspaceCommit,
	}
	if source.Repository == "" || source.Branch == "" || !validLowerHex(source.Revision, 40) {
		return trustedReconciliationAdmission{}, fmt.Errorf("release source identity is incomplete")
	}

	planBinding := map[string]interface{}{
		"template": plan.Template, "project": plan.Project, "environment": plan.Environment,
		"target": plan.Target, "cluster": plan.Cluster, "instance": plan.Instance,
		"workflowPath": envelope.WorkflowPath, "inputs": envelope.Inputs,
	}
	planDigest, err := digestJSON(plan)
	if err != nil {
		return trustedReconciliationAdmission{}, err
	}
	planBinding["planDigest"] = planDigest
	planBindingDigest, err := digestJSON(planBinding)
	if err != nil {
		return trustedReconciliationAdmission{}, err
	}

	tools := []string{
		"mcp_doops_plan_ObserveAuthorizationState",
		"mcp_doops_plan_ObserveHTTPContract",
		"mcp_doops_plan_ObserveHelmRelease",
		"mcp_doops_plan_ObserveKubernetesLogs",
		"mcp_doops_plan_ObserveKubernetesWorkload",
		"mcp_doops_plan_ObserveSourceRegistryImageSet",
		"mcp_doops_plan_ObserveTargetRegistryImageSet",
		"mcp_doops_plan_ObserveWorkspaceSource",
		"mcp_doops_plan_RenderHelmRelease",
	}
	if envelope.ExecutionMode == "apply" {
		tools = append(tools,
			"mcp_doops_plan_SyncImageSetToTarget",
			"mcp_doops_plan_ReconcileHelmRelease",
		)
	}
	capabilities := make(map[string]trustedReconciliationCapability, len(tools))
	for _, tool := range tools {
		key := strings.TrimPrefix(tool, "mcp_doops_plan_")
		scope := map[string]interface{}{
			"cluster": plan.Cluster, "instance": plan.Instance, "project": plan.Project,
			"environment": plan.Environment, "template": plan.Template, "tool": tool,
		}
		scopeDigest, err := digestJSON(scope)
		if err != nil {
			return trustedReconciliationAdmission{}, err
		}
		evidenceKind, evidenceSubject := trustedToolEvidence(key)
		capabilities[key] = trustedReconciliationCapability{
			Tool:            key,
			EvidenceKind:    evidenceKind,
			EvidenceSubject: evidenceSubject,
			CanonicalScope:  scope,
			ScopeDigest:     scopeDigest,
		}
	}

	context := trustedReconciliationContext{
		SchemaVersion:      trustedReconciliationContextSchema,
		OperationID:        trustedOperationID(envelope.RunID, workspaceCommit),
		ExecutionMode:      envelope.ExecutionMode,
		MutationAuthorized: envelope.ExecutionMode == "apply",
		PlanDigest:         planDigest,
		PlanBindingDigest:  planBindingDigest,
		Source:             source,
		Capabilities:       capabilities,
	}
	context.ContextDigest, err = digestJSON(context)
	if err != nil {
		return trustedReconciliationAdmission{}, err
	}
	return trustedReconciliationAdmission{AllowedTools: tools, Context: context}, nil
}

func digestJSON(value interface{}) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func validateTrustedWorkflowInputs(parameters map[string]credentialTemplateParameter, inputs map[string]string) error {
	for name, parameter := range parameters {
		value, ok := inputs[name]
		if parameter.Required != nil && *parameter.Required && (!ok || strings.TrimSpace(value) == "") {
			return fmt.Errorf("required workflow input %q is missing", name)
		}
	}
	for name := range inputs {
		if _, ok := parameters[name]; !ok {
			return fmt.Errorf("workflow input %q is not declared", name)
		}
	}
	return nil
}

func resolveTrustedRevision(declared string, inputs map[string]string) (string, error) {
	declared = strings.TrimSpace(declared)
	const prefix = "${inputs."
	if strings.HasPrefix(declared, prefix) && strings.HasSuffix(declared, "}") {
		name := strings.TrimSuffix(strings.TrimPrefix(declared, prefix), "}")
		value, ok := inputs[name]
		if !ok || strings.TrimSpace(value) != value || value == "" {
			return "", fmt.Errorf("workflow revision input %q is missing or invalid", name)
		}
		return value, nil
	}
	if strings.Contains(declared, "${") {
		return "", fmt.Errorf("workflow revision must be one exact input reference or immutable revision")
	}
	return declared, nil
}

func trustedToolEvidence(tool string) (string, string) {
	switch tool {
	case "ObserveWorkspaceSource":
		return "source-identity", "source-identity"
	case "ObserveSourceRegistryImageSet":
		return "image-set", "source-image-set"
	case "ObserveTargetRegistryImageSet":
		return "release-manifest", "release-manifest"
	case "RenderHelmRelease":
		return "gitops-render", "gitops-render"
	case "ObserveHelmRelease":
		return "runtime-state", "helm-release"
	case "ObserveKubernetesWorkload":
		return "runtime-state", "kubernetes-workload"
	case "ObserveAuthorizationState":
		return "authorization-state", "authorization-state"
	case "ObserveHTTPContract":
		return "public-contract", "public-contract"
	case "ObserveKubernetesLogs":
		return "post-deploy-log-scan", "post-deploy-log-scan"
	case "SyncImageSetToTarget":
		return "image-set", "target-image-set"
	case "ReconcileHelmRelease":
		return "runtime-state", "helm-release"
	default:
		return "", ""
	}
}

func trustedOperationID(runID, workspaceCommit string) string {
	sum := sha256.Sum256([]byte(runID + "\x00" + workspaceCommit))
	return "op_" + hex.EncodeToString(sum[:16])
}

func validLowerHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}
