package main

import (
	"fmt"
	"strings"
	"time"
)

const reconciliationResultKind = "ReconciliationResult"

type ReconciliationStatus string

const (
	ReconciliationConverged ReconciliationStatus = "converged"
	ReconciliationBlocked   ReconciliationStatus = "blocked"
	ReconciliationFailed    ReconciliationStatus = "failed"
)

type ReconciliationResult struct {
	APIVersion         string                   `json:"apiVersion"`
	Kind               string                   `json:"kind"`
	PlanDigest         string                   `json:"planDigest"`
	Status             ReconciliationStatus     `json:"status"`
	Attempts           int                      `json:"attempts"`
	NoProgressAttempts int                      `json:"noProgressAttempts"`
	Evidence           []ReconciliationEvidence `json:"evidence"`
	FailureEvidence    []ReconciliationEvidence `json:"failureEvidence"`
}

type ReconciliationEvidence struct {
	Kind       string `json:"kind"`
	Subject    string `json:"subject"`
	ObservedAt string `json:"observedAt"`
	Value      string `json:"value"`
}

func validateReconciliationResult(plan DeploymentPlan, result ReconciliationResult) error {
	if result.APIVersion != deploymentAPIVersion || result.Kind != reconciliationResultKind {
		return fmt.Errorf("invalid reconciliation result type")
	}
	if strings.TrimSpace(plan.Digest) == "" || result.PlanDigest != plan.Digest {
		return fmt.Errorf("reconciliation result plan digest mismatch")
	}
	if result.Attempts <= 0 || result.Attempts > plan.Spec.Policy.MaxAttempts {
		return fmt.Errorf("reconciliation result attempts must be between 1 and %d", plan.Spec.Policy.MaxAttempts)
	}
	if result.NoProgressAttempts < 0 || result.NoProgressAttempts > plan.Spec.Policy.MaxNoProgress {
		return fmt.Errorf("reconciliation result noProgressAttempts must be between 0 and %d", plan.Spec.Policy.MaxNoProgress)
	}
	if err := validateReconciliationEvidence(result.Evidence); err != nil {
		return fmt.Errorf("validate reconciliation evidence: %w", err)
	}
	if err := validateReconciliationEvidence(result.FailureEvidence); err != nil {
		return fmt.Errorf("validate reconciliation failure evidence: %w", err)
	}

	switch result.Status {
	case ReconciliationConverged:
		return requireReconciliationEvidence(plan.Spec.Acceptance.RequiredEvidence, result.Evidence)
	case ReconciliationBlocked, ReconciliationFailed:
		return requireReconciliationEvidence(plan.Spec.Acceptance.RequiredFailureEvidence, result.FailureEvidence)
	default:
		return fmt.Errorf("unsupported reconciliation result status %q", result.Status)
	}
}

func validateReconciliationEvidence(evidence []ReconciliationEvidence) error {
	for _, item := range evidence {
		if strings.TrimSpace(item.Kind) == "" {
			return fmt.Errorf("kind is required")
		}
		if strings.TrimSpace(item.Subject) == "" {
			return fmt.Errorf("subject is required")
		}
		if _, err := time.Parse(time.RFC3339, item.ObservedAt); err != nil {
			return fmt.Errorf("observedAt must be RFC3339: %w", err)
		}
		if strings.TrimSpace(item.Value) == "" {
			return fmt.Errorf("value is required")
		}
	}
	return nil
}

func requireReconciliationEvidence(required []string, evidence []ReconciliationEvidence) error {
	present := make(map[string]bool, len(evidence))
	for _, item := range evidence {
		present[item.Kind] = true
	}
	for _, kind := range normalizeEvidenceKinds(required) {
		if !present[kind] {
			return fmt.Errorf("required reconciliation evidence %q is missing", kind)
		}
	}
	return nil
}
