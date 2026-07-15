package main

import "testing"

func TestReconciliationResultAcceptsRequiredEvidence(t *testing.T) {
	plan := reconciliationResultTestPlan()
	result := ReconciliationResult{
		APIVersion: deploymentAPIVersion,
		Kind:       reconciliationResultKind,
		PlanDigest: plan.Digest,
		Status:     ReconciliationConverged,
		Attempts:   2,
		Evidence: []ReconciliationEvidence{
			reconciliationResultTestEvidence("source-identity"),
			reconciliationResultTestEvidence("runtime-state"),
		},
	}

	if err := validateReconciliationResult(plan, result); err != nil {
		t.Fatalf("validate reconciliation result: %v", err)
	}
}

func TestReconciliationResultRejectsTextualSuccess(t *testing.T) {
	plan := reconciliationResultTestPlan()
	result := ReconciliationResult{
		APIVersion: deploymentAPIVersion,
		Kind:       reconciliationResultKind,
		PlanDigest: plan.Digest,
		Status:     ReconciliationStatus("success"),
		Attempts:   1,
		Evidence: []ReconciliationEvidence{
			reconciliationResultTestEvidence("source-identity"),
			reconciliationResultTestEvidence("runtime-state"),
		},
	}

	if err := validateReconciliationResult(plan, result); err == nil {
		t.Fatal("expected textual success to be rejected")
	}
}

func TestReconciliationResultRejectsMismatchedPlanDigest(t *testing.T) {
	plan := reconciliationResultTestPlan()
	result := ReconciliationResult{
		APIVersion: deploymentAPIVersion,
		Kind:       reconciliationResultKind,
		PlanDigest: "sha256:other-plan",
		Status:     ReconciliationConverged,
		Attempts:   1,
		Evidence: []ReconciliationEvidence{
			reconciliationResultTestEvidence("source-identity"),
			reconciliationResultTestEvidence("runtime-state"),
		},
	}

	if err := validateReconciliationResult(plan, result); err == nil {
		t.Fatal("expected plan digest mismatch to be rejected")
	}
}

func TestReconciliationResultRejectsMissingEvidence(t *testing.T) {
	plan := reconciliationResultTestPlan()
	result := ReconciliationResult{
		APIVersion: deploymentAPIVersion,
		Kind:       reconciliationResultKind,
		PlanDigest: plan.Digest,
		Status:     ReconciliationConverged,
		Attempts:   1,
		Evidence: []ReconciliationEvidence{
			reconciliationResultTestEvidence("source-identity"),
		},
	}

	if err := validateReconciliationResult(plan, result); err == nil {
		t.Fatal("expected missing success evidence to be rejected")
	}
}

func TestReconciliationResultRequiresFailureEvidence(t *testing.T) {
	plan := reconciliationResultTestPlan()
	result := ReconciliationResult{
		APIVersion: deploymentAPIVersion,
		Kind:       reconciliationResultKind,
		PlanDigest: plan.Digest,
		Status:     ReconciliationFailed,
		Attempts:   3,
	}

	if err := validateReconciliationResult(plan, result); err == nil {
		t.Fatal("expected failed reconciliation without rollback evidence to be rejected")
	}
}

func TestReconciliationResultRejectsTooManyNoProgressAttempts(t *testing.T) {
	plan := reconciliationResultTestPlan()
	result := ReconciliationResult{
		APIVersion:         deploymentAPIVersion,
		Kind:               reconciliationResultKind,
		PlanDigest:         plan.Digest,
		Status:             ReconciliationBlocked,
		Attempts:           2,
		NoProgressAttempts: 2,
		FailureEvidence: []ReconciliationEvidence{
			reconciliationResultTestEvidence("rollback-state"),
		},
	}

	if err := validateReconciliationResult(plan, result); err == nil {
		t.Fatal("expected no-progress policy violation to be rejected")
	}
}

func reconciliationResultTestPlan() DeploymentPlan {
	return DeploymentPlan{
		Digest: "sha256:expected-plan",
		Spec: DeploymentPlanSpec{
			Acceptance: CICDAcceptance{
				RequiredEvidence:        []string{"source-identity", "runtime-state"},
				RequiredFailureEvidence: []string{"rollback-state"},
			},
			Policy: CICDDeploymentPolicy{
				MaxAttempts:   3,
				MaxNoProgress: 1,
			},
		},
	}
}

func reconciliationResultTestEvidence(kind string) ReconciliationEvidence {
	return ReconciliationEvidence{
		Kind:       kind,
		Subject:    "zhiyong",
		ObservedAt: "2026-07-12T12:00:00Z",
		Value:      "verified",
	}
}
