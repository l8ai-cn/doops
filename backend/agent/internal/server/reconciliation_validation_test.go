package server

import (
	"strings"
	"testing"
)

func TestValidateAttestedReconciliationResultAcceptsRequiredEvidence(t *testing.T) {
	execution := agentPromptExecutionContext{
		PlanDigest:      "sha256:" + strings.Repeat("a", 64),
		SourceRevision:  strings.Repeat("c", 40),
		WorkspaceCommit: strings.Repeat("b", 40),
		RequiredEvidence: []string{
			"runtime-state",
			"source-identity",
		},
	}
	records := reconciliationValidationRecords(t)
	result := reconciliationValidationResult(execution.PlanDigest, "converged")
	result["evidence"] = []interface{}{
		reconciliationValidationEvidence("source-identity"),
		reconciliationValidationEvidence("runtime-state"),
	}
	if err := attestReconciliationResult(result, execution, "turn-release", records); err != nil {
		t.Fatalf("attest result: %v", err)
	}

	if err := validateAttestedReconciliationResult(result, execution); err != nil {
		t.Fatalf("validate attested result: %v", err)
	}
}

func TestValidateAttestedReconciliationResultRejectsInvalidCompletion(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]interface{})
		want   string
	}{
		{
			name: "missing required evidence",
			mutate: func(result map[string]interface{}) {
				result["evidence"] = []interface{}{
					reconciliationValidationEvidence("runtime-state"),
				}
			},
			want: `required reconciliation evidence "source-identity" is missing`,
		},
		{
			name: "textual success status",
			mutate: func(result map[string]interface{}) {
				result["status"] = "success"
			},
			want: "unsupported reconciliation result status",
		},
		{
			name: "invalid no progress count",
			mutate: func(result map[string]interface{}) {
				result["noProgressAttempts"] = float64(2)
			},
			want: "noProgressAttempts must be between 0 and attempts",
		},
		{
			name: "failed without failure evidence",
			mutate: func(result map[string]interface{}) {
				result["status"] = "failed"
				result["evidence"] = []interface{}{}
			},
			want: "blocked or failed reconciliation requires observed failure evidence",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			execution := agentPromptExecutionContext{
				PlanDigest:      "sha256:" + strings.Repeat("a", 64),
				SourceRevision:  strings.Repeat("c", 40),
				WorkspaceCommit: strings.Repeat("b", 40),
				RequiredEvidence: []string{
					"runtime-state",
					"source-identity",
				},
			}
			records := reconciliationValidationRecords(t)
			result := reconciliationValidationResult(execution.PlanDigest, "converged")
			result["evidence"] = []interface{}{
				reconciliationValidationEvidence("source-identity"),
				reconciliationValidationEvidence("runtime-state"),
			}
			test.mutate(result)
			if err := attestReconciliationResult(result, execution, "turn-release", records); err != nil {
				t.Fatalf("attest result: %v", err)
			}

			err := validateAttestedReconciliationResult(result, execution)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("unexpected validation error: %v", err)
			}
		})
	}
}

func reconciliationValidationRecords(t *testing.T) []doagentToolTraceRecord {
	t.Helper()
	record, ok, err := doagentToolTraceRecordFromUpdate(completedObservationUpdate("call-observe"))
	if err != nil || !ok {
		t.Fatalf("build observation record: ok=%v err=%v", ok, err)
	}
	return []doagentToolTraceRecord{record}
}

func reconciliationValidationResult(planDigest, status string) map[string]interface{} {
	return map[string]interface{}{
		"apiVersion":         "doops.sh/v2",
		"kind":               "ReconciliationResult",
		"planDigest":         planDigest,
		"status":             status,
		"attempts":           float64(1),
		"noProgressAttempts": float64(0),
		"evidence":           []interface{}{},
		"failureEvidence":    []interface{}{},
	}
}

func reconciliationValidationEvidence(kind string) map[string]interface{} {
	return map[string]interface{}{
		"kind":       kind,
		"subject":    "zhiyong",
		"observedAt": "2026-07-14T00:00:00Z",
		"value":      "verified",
		"toolRef": map[string]interface{}{
			"tool":    "WebFetch",
			"ordinal": float64(1),
		},
	}
}
