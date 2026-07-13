package main

import (
	"strings"
	"testing"
)

func TestReconciliationResultAcceptsRequiredEvidence(t *testing.T) {
	plan := reconciliationResultTestPlan()
	workspaceCommit := strings.Repeat("a", 40)
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
	attestReconciliationResultForTest(plan, workspaceCommit, &result)

	if err := validateReconciliationResult(plan, workspaceCommit, result); err != nil {
		t.Fatalf("validate reconciliation result: %v", err)
	}
}

func TestReconciliationResultRejectsUnattestedEvidence(t *testing.T) {
	plan := reconciliationResultTestPlan()
	result := ReconciliationResult{
		APIVersion: deploymentAPIVersion,
		Kind:       reconciliationResultKind,
		PlanDigest: plan.Digest,
		Status:     ReconciliationConverged,
		Attempts:   1,
		Evidence: []ReconciliationEvidence{
			reconciliationResultTestEvidence("source-identity"),
			reconciliationResultTestEvidence("runtime-state"),
		},
	}

	if err := validateReconciliationResult(plan, strings.Repeat("a", 40), result); err == nil {
		t.Fatal("Agent-authored evidence without bridge trace attestation must be rejected")
	}
}

func TestReconciliationResultRejectsTamperedTraceDigest(t *testing.T) {
	plan := reconciliationResultTestPlan()
	workspaceCommit := strings.Repeat("a", 40)
	result := ReconciliationResult{
		APIVersion: deploymentAPIVersion,
		Kind:       reconciliationResultKind,
		PlanDigest: plan.Digest,
		Status:     ReconciliationConverged,
		Attempts:   1,
		Evidence: []ReconciliationEvidence{
			reconciliationResultTestEvidence("source-identity"),
			reconciliationResultTestEvidence("runtime-state"),
		},
	}
	attestReconciliationResultForTest(plan, workspaceCommit, &result)
	result.ExecutionEvidence.TraceDigest = "sha256:" + strings.Repeat("f", 64)

	if err := validateReconciliationResult(plan, workspaceCommit, result); err == nil {
		t.Fatal("tampered bridge trace digest must be rejected")
	}
}

func TestReconciliationResultRejectsInvalidEvidenceToolCallBindings(t *testing.T) {
	plan := reconciliationResultTestPlan()
	workspaceCommit := strings.Repeat("a", 40)
	tests := []struct {
		name   string
		mutate func(*ReconciliationResult)
		want   string
	}{
		{
			name: "missing toolCallId",
			mutate: func(result *ReconciliationResult) {
				result.Evidence[0].ToolCallID = ""
			},
			want: "toolCallId is required",
		},
		{
			name: "nonexistent toolCallId",
			mutate: func(result *ReconciliationResult) {
				result.Evidence[0].ToolCallID = "call-missing"
			},
			want: `toolCallId "call-missing" is not present`,
		},
		{
			name: "failed toolCallId",
			mutate: func(result *ReconciliationResult) {
				result.ExecutionEvidence.ToolCalls[0].Status = "failed"
				result.ExecutionEvidence.ToolCalls = append(
					result.ExecutionEvidence.ToolCalls,
					completedObservationToolCallForTest("call-other"),
				)
				refreshReconciliationAttestationForTest(plan, result)
			},
			want: `toolCallId "call-observe" did not complete successfully`,
		},
		{
			name: "unrelated toolCallId",
			mutate: func(result *ReconciliationResult) {
				result.ExecutionEvidence.ToolCalls[0].Tool = "Bash"
				result.ExecutionEvidence.ToolCalls[0].Observation = false
				result.ExecutionEvidence.ToolCalls = append(
					result.ExecutionEvidence.ToolCalls,
					completedObservationToolCallForTest("call-other"),
				)
				refreshReconciliationAttestationForTest(plan, result)
			},
			want: `toolCallId "call-observe" is not an observation call`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := ReconciliationResult{
				APIVersion: deploymentAPIVersion,
				Kind:       reconciliationResultKind,
				PlanDigest: plan.Digest,
				Status:     ReconciliationConverged,
				Attempts:   1,
				Evidence: []ReconciliationEvidence{
					reconciliationResultTestEvidence("source-identity"),
					reconciliationResultTestEvidence("runtime-state"),
				},
			}
			attestReconciliationResultForTest(plan, workspaceCommit, &result)
			test.mutate(&result)

			err := validateReconciliationResult(plan, workspaceCommit, result)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("unexpected validation error: %v", err)
			}
		})
	}
}

func TestReconciliationResultRejectsTamperedEvidenceToolDigest(t *testing.T) {
	plan := reconciliationResultTestPlan()
	workspaceCommit := strings.Repeat("a", 40)
	result := ReconciliationResult{
		APIVersion: deploymentAPIVersion,
		Kind:       reconciliationResultKind,
		PlanDigest: plan.Digest,
		Status:     ReconciliationConverged,
		Attempts:   1,
		Evidence: []ReconciliationEvidence{
			reconciliationResultTestEvidence("source-identity"),
			reconciliationResultTestEvidence("runtime-state"),
		},
	}
	attestReconciliationResultForTest(plan, workspaceCommit, &result)
	result.Evidence[0].ToolDigest = "sha256:" + strings.Repeat("f", 64)

	err := validateReconciliationResult(plan, workspaceCommit, result)
	if err == nil || !strings.Contains(err.Error(), `toolDigest does not match toolCallId "call-observe"`) {
		t.Fatalf("unexpected validation error: %v", err)
	}
}

func TestReconciliationResultRejectsTextualSuccess(t *testing.T) {
	plan := reconciliationResultTestPlan()
	workspaceCommit := strings.Repeat("a", 40)
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
	attestReconciliationResultForTest(plan, workspaceCommit, &result)

	if err := validateReconciliationResult(plan, workspaceCommit, result); err == nil {
		t.Fatal("expected textual success to be rejected")
	}
}

func TestReconciliationResultRejectsMismatchedPlanDigest(t *testing.T) {
	plan := reconciliationResultTestPlan()
	workspaceCommit := strings.Repeat("a", 40)
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
	attestReconciliationResultForTest(plan, workspaceCommit, &result)

	if err := validateReconciliationResult(plan, workspaceCommit, result); err == nil {
		t.Fatal("expected plan digest mismatch to be rejected")
	}
}

func TestReconciliationResultRejectsMissingEvidence(t *testing.T) {
	plan := reconciliationResultTestPlan()
	workspaceCommit := strings.Repeat("a", 40)
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
	attestReconciliationResultForTest(plan, workspaceCommit, &result)

	if err := validateReconciliationResult(plan, workspaceCommit, result); err == nil {
		t.Fatal("expected missing success evidence to be rejected")
	}
}

func TestReconciliationResultRequiresFailureEvidence(t *testing.T) {
	plan := reconciliationResultTestPlan()
	workspaceCommit := strings.Repeat("a", 40)
	result := ReconciliationResult{
		APIVersion: deploymentAPIVersion,
		Kind:       reconciliationResultKind,
		PlanDigest: plan.Digest,
		Status:     ReconciliationFailed,
		Attempts:   3,
	}
	attestReconciliationResultForTest(plan, workspaceCommit, &result)

	if err := validateReconciliationResult(plan, workspaceCommit, result); err == nil {
		t.Fatal("expected failed reconciliation without observed failure evidence to be rejected")
	}
}

func TestReconciliationResultRejectsNoProgressGreaterThanAttempts(t *testing.T) {
	plan := reconciliationResultTestPlan()
	workspaceCommit := strings.Repeat("a", 40)
	result := ReconciliationResult{
		APIVersion:         deploymentAPIVersion,
		Kind:               reconciliationResultKind,
		PlanDigest:         plan.Digest,
		Status:             ReconciliationBlocked,
		Attempts:           1,
		NoProgressAttempts: 2,
		FailureEvidence: []ReconciliationEvidence{
			reconciliationResultTestEvidence("access-denied"),
		},
	}
	attestReconciliationResultForTest(plan, workspaceCommit, &result)

	if err := validateReconciliationResult(plan, workspaceCommit, result); err == nil {
		t.Fatal("expected self-inconsistent no-progress count to be rejected")
	}
}

func reconciliationResultTestPlan() DeploymentPlan {
	return DeploymentPlan{
		Digest: "sha256:expected-plan",
		Spec: DeploymentPlanSpec{
			Acceptance: CICDAcceptance{
				RequiredEvidence: []string{"source-identity", "runtime-state"},
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

func attestReconciliationResultForTest(plan DeploymentPlan, workspaceCommit string, result *ReconciliationResult) {
	result.ExecutionEvidence = ReconciliationExecutionEvidence{
		TurnID:          "turn-test",
		WorkspaceCommit: workspaceCommit,
		ToolCalls: []ReconciliationToolEvidence{
			completedObservationToolCallForTest("call-observe"),
		},
	}
	result.ExecutionEvidence.TraceDigest = reconciliationTraceDigest(
		plan.Digest,
		result.ExecutionEvidence,
	)
	for index := range result.Evidence {
		result.Evidence[index].ToolCallID = result.ExecutionEvidence.ToolCalls[0].CallID
		result.Evidence[index].ToolDigest = result.ExecutionEvidence.ToolCalls[0].Digest
		result.Evidence[index].TraceDigest = result.ExecutionEvidence.TraceDigest
	}
	for index := range result.FailureEvidence {
		result.FailureEvidence[index].ToolCallID = result.ExecutionEvidence.ToolCalls[0].CallID
		result.FailureEvidence[index].ToolDigest = result.ExecutionEvidence.ToolCalls[0].Digest
		result.FailureEvidence[index].TraceDigest = result.ExecutionEvidence.TraceDigest
	}
}

func completedObservationToolCallForTest(callID string) ReconciliationToolEvidence {
	return ReconciliationToolEvidence{
		CallID:      callID,
		Tool:        "WebFetch",
		Status:      "completed",
		Observation: true,
		Digest:      "sha256:" + strings.Repeat("b", 64),
	}
}

func refreshReconciliationAttestationForTest(plan DeploymentPlan, result *ReconciliationResult) {
	result.ExecutionEvidence.TraceDigest = reconciliationTraceDigest(plan.Digest, result.ExecutionEvidence)
	for index := range result.Evidence {
		result.Evidence[index].ToolDigest = result.ExecutionEvidence.ToolCalls[0].Digest
		result.Evidence[index].TraceDigest = result.ExecutionEvidence.TraceDigest
	}
	for index := range result.FailureEvidence {
		result.FailureEvidence[index].ToolDigest = result.ExecutionEvidence.ToolCalls[0].Digest
		result.FailureEvidence[index].TraceDigest = result.ExecutionEvidence.TraceDigest
	}
}
