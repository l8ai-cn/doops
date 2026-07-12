package server

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

type CICDOperationRole string

const (
	CICDOperationRoleSourceVerifier     CICDOperationRole = "source-verifier"
	CICDOperationRoleImageBuilder       CICDOperationRole = "image-builder"
	CICDOperationRoleArtifactAttestor   CICDOperationRole = "artifact-attestor"
	CICDOperationRoleDeploymentExecutor CICDOperationRole = "deployment-executor"
	CICDOperationRoleHealthObserver     CICDOperationRole = "health-observer"
	CICDOperationRoleRollbackController CICDOperationRole = "rollback-controller"
)

var cicdRequiredOperationRoles = []CICDOperationRole{
	CICDOperationRoleSourceVerifier,
	CICDOperationRoleImageBuilder,
	CICDOperationRoleArtifactAttestor,
	CICDOperationRoleDeploymentExecutor,
	CICDOperationRoleHealthObserver,
	CICDOperationRoleRollbackController,
}

var cicdOperationDependencies = map[CICDOperationRole][]string{
	CICDOperationRoleSourceVerifier:     {},
	CICDOperationRoleImageBuilder:       {"source"},
	CICDOperationRoleArtifactAttestor:   {"build"},
	CICDOperationRoleDeploymentExecutor: {"attest"},
	CICDOperationRoleHealthObserver:     {"deploy"},
	CICDOperationRoleRollbackController: {"deploy", "observe"},
}

var cicdOperationEvidence = map[CICDOperationRole][]string{
	CICDOperationRoleSourceVerifier:     {"source-identity"},
	CICDOperationRoleImageBuilder:       {"image-digest"},
	CICDOperationRoleArtifactAttestor:   {"release-manifest"},
	CICDOperationRoleDeploymentExecutor: {"runtime-state"},
	CICDOperationRoleHealthObserver:     {"public-contract", "post-deploy-log-scan"},
	CICDOperationRoleRollbackController: {"rollback-state"},
}

var forbiddenCICDOperationGraphFields = map[string]struct{}{
	"run":      {},
	"script":   {},
	"command":  {},
	"shell":    {},
	"uses":     {},
	"stages":   {},
	"task":     {},
	"fallback": {},
}

type CICDOperationGraph struct {
	Operations []CICDOperation `json:"operations"`
}

type CICDOperation struct {
	ID        string                `json:"id"`
	Role      CICDOperationRole     `json:"role"`
	DependsOn []string              `json:"dependsOn,omitempty"`
	Target    CICDOperationTarget   `json:"target"`
	Outputs   []CICDOperationOutput `json:"outputs"`
}

type CICDOperationTarget struct {
	Target   string `json:"target"`
	Cluster  string `json:"cluster"`
	Instance string `json:"instance"`
}

type CICDOperationOutput struct {
	Kind      string `json:"kind"`
	Immutable bool   `json:"immutable"`
}

func parseCICDOperationGraph(raw json.RawMessage) (CICDOperationGraph, error) {
	if field, found, err := findForbiddenCICDOperationGraphField(raw); err != nil {
		return CICDOperationGraph{}, err
	} else if found {
		return CICDOperationGraph{}, fmt.Errorf("operation graph contains forbidden command-driven field %q", field)
	}
	var graph CICDOperationGraph
	if err := json.Unmarshal(raw, &graph); err != nil {
		return CICDOperationGraph{}, fmt.Errorf("parse CI/CD operation graph: %w", err)
	}
	if err := validateCICDOperationGraph(graph); err != nil {
		return CICDOperationGraph{}, err
	}
	return graph, nil
}

func validateCICDOperationGraph(graph CICDOperationGraph) error {
	byRole := make(map[CICDOperationRole]CICDOperation, len(graph.Operations))
	ids := make(map[string]bool, len(graph.Operations))
	targets := make(map[string]CICDOperationRole, len(graph.Operations))
	for _, operation := range graph.Operations {
		if strings.TrimSpace(operation.ID) == "" {
			return fmt.Errorf("CI/CD operation id is required")
		}
		if ids[operation.ID] {
			return fmt.Errorf("duplicate CI/CD operation id %q", operation.ID)
		}
		ids[operation.ID] = true
		if _, ok := cicdOperationDependencies[operation.Role]; !ok {
			return fmt.Errorf("unsupported CI/CD operation role %q", operation.Role)
		}
		if _, exists := byRole[operation.Role]; exists {
			return fmt.Errorf("duplicate CI/CD operation role %q", operation.Role)
		}
		if err := validateCICDOperationTarget(operation.Target); err != nil {
			return fmt.Errorf("%s target: %w", operation.Role, err)
		}
		targetKey := operation.Target.Target
		if previous, exists := targets[targetKey]; exists {
			return fmt.Errorf("CI/CD roles %q and %q must use independent targets", previous, operation.Role)
		}
		targets[targetKey] = operation.Role
		if !sameStringSet(operation.DependsOn, cicdOperationDependencies[operation.Role]) {
			return fmt.Errorf("%s dependencies must be %v", operation.Role, cicdOperationDependencies[operation.Role])
		}
		if err := validateCICDOperationOutputs(operation.Role, operation.Outputs); err != nil {
			return err
		}
		byRole[operation.Role] = operation
	}

	missing := make([]string, 0, len(cicdRequiredOperationRoles))
	for _, role := range cicdRequiredOperationRoles {
		if _, ok := byRole[role]; !ok {
			missing = append(missing, string(role))
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("CI/CD operation graph is missing required roles: %s", strings.Join(missing, ", "))
	}
	return nil
}

func validateCICDOperationTarget(target CICDOperationTarget) error {
	if strings.TrimSpace(target.Target) == "" {
		return fmt.Errorf("target is required")
	}
	if strings.TrimSpace(target.Cluster) == "" {
		return fmt.Errorf("cluster is required")
	}
	if strings.TrimSpace(target.Instance) == "" {
		return fmt.Errorf("instance is required")
	}
	return nil
}

func validateCICDOperationOutputs(role CICDOperationRole, outputs []CICDOperationOutput) error {
	actual := make(map[string]bool, len(outputs))
	nonImmutable := make([]string, 0, len(outputs))
	for _, output := range outputs {
		kind := strings.TrimSpace(output.Kind)
		if kind == "" {
			return fmt.Errorf("%s output kind is required", role)
		}
		if actual[kind] {
			return fmt.Errorf("%s output %q is duplicated", role, kind)
		}
		actual[kind] = output.Immutable
		if !output.Immutable {
			nonImmutable = append(nonImmutable, kind)
		}
	}
	for _, kind := range cicdOperationEvidence[role] {
		if !actual[kind] {
			return fmt.Errorf("%s requires immutable output %q", role, kind)
		}
	}
	if len(nonImmutable) > 0 {
		return fmt.Errorf("%s outputs must be immutable: %s", role, strings.Join(nonImmutable, ", "))
	}
	return nil
}

func sameStringSet(actual, expected []string) bool {
	if len(actual) != len(expected) {
		return false
	}
	actualCopy := append([]string(nil), actual...)
	expectedCopy := append([]string(nil), expected...)
	sort.Strings(actualCopy)
	sort.Strings(expectedCopy)
	for index := range actualCopy {
		if actualCopy[index] != expectedCopy[index] {
			return false
		}
	}
	return true
}

func findForbiddenCICDOperationGraphField(raw json.RawMessage) (string, bool, error) {
	var value interface{}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return "", false, fmt.Errorf("parse CI/CD operation graph: %w", err)
	}
	return findForbiddenCICDOperationGraphValue(value)
}

func findForbiddenCICDOperationGraphValue(value interface{}) (string, bool, error) {
	switch node := value.(type) {
	case map[string]interface{}:
		for key, nested := range node {
			if _, forbidden := forbiddenCICDOperationGraphFields[key]; forbidden {
				return key, true, nil
			}
			if field, found, err := findForbiddenCICDOperationGraphValue(nested); err != nil || found {
				return field, found, err
			}
		}
	case []interface{}:
		for _, nested := range node {
			if field, found, err := findForbiddenCICDOperationGraphValue(nested); err != nil || found {
				return field, found, err
			}
		}
	}
	return "", false, nil
}
