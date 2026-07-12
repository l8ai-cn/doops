package main

import (
	"context"
	"fmt"
)

type DeploymentReconcileRequest struct {
	DryRun bool `json:"dryRun,omitempty"`
}

type deploymentReconciler interface {
	Reconcile(DeploymentPlan, DeploymentReconcileRequest) (CICDReconcileResult, error)
}

type DeploymentReconcileOptions struct {
	DryRun            bool
	MaxIterations     int
	MaxNoProgressRuns int
}

type DeploymentReconcileRun struct {
	PlanDigest string                `json:"planDigest"`
	State      CICDReconcileStatus   `json:"state"`
	Iterations int                   `json:"iterations"`
	Results    []CICDReconcileResult `json:"results"`
}

func (c *MCPClient) Reconcile(plan DeploymentPlan, request DeploymentReconcileRequest) (CICDReconcileResult, error) {
	var result CICDReconcileResult
	if err := validateDeploymentPlan(plan); err != nil {
		return result, err
	}
	if plan.Attestation == nil {
		return result, fmt.Errorf("deployment plan attestation is required for reconciliation")
	}
	if err := c.requireSemanticDeploymentCapability(); err != nil {
		return result, err
	}
	err := c.CallStructured("doops_cicd_reconcile", map[string]interface{}{
		"plan":    plan,
		"dry_run": request.DryRun,
	}, &result)
	return result, err
}

func reconcileDeploymentPlan(ctx context.Context, plan DeploymentPlan, executor deploymentReconciler, options DeploymentReconcileOptions) (DeploymentReconcileRun, error) {
	if executor == nil {
		return DeploymentReconcileRun{}, fmt.Errorf("deployment reconciler is required")
	}
	if options.MaxIterations <= 0 {
		options.MaxIterations = 12
	}
	if options.MaxNoProgressRuns <= 0 {
		options.MaxNoProgressRuns = 3
	}
	run := DeploymentReconcileRun{
		PlanDigest: plan.Digest,
		State:      CICDReconcilePending,
		Results:    make([]CICDReconcileResult, 0, options.MaxIterations),
	}
	lastFingerprint := ""
	noProgressRuns := 0

	for iteration := 1; iteration <= options.MaxIterations; iteration++ {
		if err := ctx.Err(); err != nil {
			run.State = CICDReconcileBlocked
			return run, err
		}
		result, err := executor.Reconcile(plan, DeploymentReconcileRequest{DryRun: options.DryRun})
		run.Iterations = iteration
		if err != nil {
			run.State = CICDReconcileFailed
			return run, fmt.Errorf("reconcile iteration %d: %w", iteration, err)
		}
		run.Results = append(run.Results, result)
		state, err := evaluateDeploymentReconcile(plan, result)
		if err != nil {
			run.State = CICDReconcileFailed
			return run, fmt.Errorf("validate reconcile iteration %d: %w", iteration, err)
		}
		run.State = state
		switch state {
		case CICDReconcileConverged:
			return run, nil
		case CICDReconcileBlocked:
			return run, fmt.Errorf("reconciliation blocked")
		case CICDReconcileFailed:
			return run, fmt.Errorf("reconciliation failed")
		}

		fingerprint, err := digestDeploymentValue(struct {
			Evidence   []CICDEvidence  `json:"evidence"`
			Violations []CICDViolation `json:"violations"`
		}{
			Evidence:   result.Evidence,
			Violations: result.Violations,
		})
		if err != nil {
			run.State = CICDReconcileFailed
			return run, err
		}
		if fingerprint == lastFingerprint {
			noProgressRuns++
		} else {
			lastFingerprint = fingerprint
			noProgressRuns = 0
		}
		if noProgressRuns >= options.MaxNoProgressRuns {
			run.State = CICDReconcileBlocked
			return run, fmt.Errorf("reconciliation made no progress for %d iterations", noProgressRuns)
		}
	}
	run.State = CICDReconcileBlocked
	return run, fmt.Errorf("reconciliation exceeded %d iterations", options.MaxIterations)
}
