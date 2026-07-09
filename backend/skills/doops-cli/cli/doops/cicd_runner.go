package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type CICDRunOptions struct {
	Inputs      map[string]string
	DryRun      bool
	AllowMutate bool
	// Session isolates the remote agent workspace at /root/ws/<session>.
	Session string
	// SourceSync pushes the local source tree into the remote session workspace
	// before the first agent-native stage. Local git.clone alone is not visible
	// on the target node.
	SourceSync func(src string) error
	// Executor dispatches agent-native stages (agent.task / doops.k8s /
	// doops.exec) to the real DoOps gateway tools. When nil, those stages are
	// only planned (kept for lint/plan/dry-run and offline use).
	Executor cicdExecutor
}

type CICDRunResult struct {
	Name       string              `json:"name"`
	StartedAt  string              `json:"startedAt"`
	FinishedAt string              `json:"finishedAt"`
	Steps      []CICDRunStepResult `json:"steps"`
}

type CICDRunStepResult struct {
	ID      string `json:"id"`
	Uses    string `json:"uses"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
	Stdout  string `json:"stdout,omitempty"`
	Stderr  string `json:"stderr,omitempty"`
}

func runCICDWorkflow(ctx context.Context, workflow CICDWorkflow, opts CICDRunOptions) (CICDRunResult, error) {
	plan, err := buildCICDPlan(workflow, opts.Inputs)
	if err != nil {
		return CICDRunResult{}, err
	}
	result := CICDRunResult{
		Name:      plan.Name,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}
	sourceSynced := false
	for _, stage := range plan.Stages {
		step := CICDRunStepResult{ID: stage.ID, Uses: stage.Uses}
		if !isCICDAgentDrivenStage(stage) {
			if stage.Mutates && opts.DryRun {
				step.Status = "skipped"
				step.Message = "dry-run skipped mutating stage"
				result.Steps = append(result.Steps, step)
				continue
			}
			if stage.Mutates && !opts.AllowMutate {
				step.Status = "failed"
				step.Message = "mutating stage requires --allow-mutate"
				result.Steps = append(result.Steps, step)
				result.FinishedAt = time.Now().UTC().Format(time.RFC3339)
				return result, fmt.Errorf("stage %s requires --allow-mutate", stage.ID)
			}
		}
		switch stage.Uses {
		case "git.clone":
			stdout, stderr, err := runCICDGitClone(ctx, plan.Source)
			step.Stdout = stdout
			step.Stderr = stderr
			if err != nil {
				step.Status = "failed"
				step.Message = err.Error()
				result.Steps = append(result.Steps, step)
				result.FinishedAt = time.Now().UTC().Format(time.RFC3339)
				return result, fmt.Errorf("stage %s failed: %w", stage.ID, err)
			}
			step.Status = "success"
		case "git.update":
			stdout, stderr, err := runCICDGitUpdate(ctx, plan.Source)
			step.Stdout = stdout
			step.Stderr = stderr
			if err != nil {
				step.Status = "failed"
				step.Message = err.Error()
				result.Steps = append(result.Steps, step)
				result.FinishedAt = time.Now().UTC().Format(time.RFC3339)
				return result, fmt.Errorf("stage %s failed: %w", stage.ID, err)
			}
			step.Status = "success"
		case "shell":
			stdout, stderr, err := runCICDShellStage(ctx, plan.Source.Path, stage, plan.Inputs)
			step.Stdout = stdout
			step.Stderr = stderr
			if err != nil {
				step.Status = "failed"
				step.Message = err.Error()
				result.Steps = append(result.Steps, step)
				result.FinishedAt = time.Now().UTC().Format(time.RFC3339)
				return result, fmt.Errorf("stage %s failed: %w", stage.ID, err)
			}
			step.Status = "success"
		case "agent.task", "doops.k8s", "doops.exec":
			if strings.TrimSpace(stage.Run) != "" {
				stdout, stderr, err := runCICDShellStage(ctx, plan.Source.Path, stage, plan.Inputs)
				step.Stdout = stdout
				step.Stderr = stderr
				if err != nil {
					step.Status = "failed"
					step.Message = err.Error()
					result.Steps = append(result.Steps, step)
					result.FinishedAt = time.Now().UTC().Format(time.RFC3339)
					return result, fmt.Errorf("stage %s failed: %w", stage.ID, err)
				}
				step.Status = "success"
				break
			}
			// Agent-driven execution: hand the stage's intent to the doagent.
			// Dry-run is dispatched with mode=dry-run. Mutating apply runs must
			// pass --allow-mutate before any executor.Call.
			if opts.Executor != nil {
				if stage.Mutates && !opts.DryRun && !opts.AllowMutate {
					step.Status = "failed"
					step.Message = "mutating stage requires --allow-mutate"
					result.Steps = append(result.Steps, step)
					result.FinishedAt = time.Now().UTC().Format(time.RFC3339)
					return result, fmt.Errorf("stage %s requires --allow-mutate", stage.ID)
				}
				if !sourceSynced && opts.SourceSync != nil {
					src := strings.TrimSpace(plan.Source.Path)
					if src == "" {
						step.Status = "failed"
						step.Message = "source.path is required before agent-native stages"
						result.Steps = append(result.Steps, step)
						result.FinishedAt = time.Now().UTC().Format(time.RFC3339)
						return result, fmt.Errorf("stage %s: source.path is required before agent-native stages", stage.ID)
					}
					if err := opts.SourceSync(src); err != nil {
						step.Status = "failed"
						step.Message = "source sync to remote session failed: " + err.Error()
						result.Steps = append(result.Steps, step)
						result.FinishedAt = time.Now().UTC().Format(time.RFC3339)
						return result, fmt.Errorf("stage %s: sync source to remote session: %w", stage.ID, err)
					}
					sourceSynced = true
				}
				mode := "apply"
				if opts.DryRun {
					mode = "dry-run"
				}
				if err := runCICDAgentStage(opts.Executor, plan, stage, mode, opts.Session); err != nil {
					step.Status = "failed"
					step.Message = err.Error()
					result.Steps = append(result.Steps, step)
					result.FinishedAt = time.Now().UTC().Format(time.RFC3339)
					return result, fmt.Errorf("stage %s failed: %w", stage.ID, err)
				}
				step.Status = "success"
				step.Message = "dispatched to agent-native executor (mode=" + mode + ")"
				break
			}
			// No executor wired (offline lint/plan): record the stage as planned
			// for a later agent-native run. This is not a failure.
			step.Status = "planned"
			step.Message = "agent-native stage planned (no executor wired; run with -session against a target)"
		default:
			step.Status = "skipped"
			step.Message = "stage type is planned but not executable by local v1 runner"
		}
		result.Steps = append(result.Steps, step)
	}
	result.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	return result, nil
}

func runCICDGitClone(ctx context.Context, source CICDSource) (string, string, error) {
	repo := strings.TrimSpace(source.Repo)
	if repo == "" {
		return "", "", fmt.Errorf("source.repo is required for git.clone")
	}
	path := strings.TrimSpace(source.Path)
	if path == "" {
		return "", "", fmt.Errorf("source.path is required for git.clone")
	}
	pathAbs, err := filepath.Abs(path)
	if err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(filepath.Dir(pathAbs), 0o755); err != nil {
		return "", "", err
	}
	args := []string{"clone", "--no-hardlinks"}
	if strings.TrimSpace(source.Branch) != "" {
		args = append(args, "--branch", strings.TrimSpace(source.Branch))
	}
	args = append(args, repo, pathAbs)
	return runCICDCommandOutput(ctx, "", nil, "git", args...)
}

func runCICDGitUpdate(ctx context.Context, source CICDSource) (string, string, error) {
	path := strings.TrimSpace(source.Path)
	if path == "" {
		return "", "", fmt.Errorf("source.path is required for git.update")
	}
	pathAbs, err := filepath.Abs(path)
	if err != nil {
		return "", "", err
	}
	var stdout strings.Builder
	var stderr strings.Builder
	for _, args := range [][]string{
		{"rev-parse", "--is-inside-work-tree"},
		{"fetch", "--all", "--prune"},
	} {
		out, errOut, err := runCICDCommandOutput(ctx, pathAbs, nil, "git", args...)
		if out != "" {
			stdout.WriteString(out + "\n")
		}
		if errOut != "" {
			stderr.WriteString(errOut + "\n")
		}
		if err != nil {
			return strings.TrimSpace(stdout.String()), strings.TrimSpace(stderr.String()), err
		}
	}
	if strings.TrimSpace(source.Branch) != "" {
		for _, args := range [][]string{
			{"checkout", strings.TrimSpace(source.Branch)},
			{"pull", "--ff-only"},
		} {
			out, errOut, err := runCICDCommandOutput(ctx, pathAbs, nil, "git", args...)
			if out != "" {
				stdout.WriteString(out + "\n")
			}
			if errOut != "" {
				stderr.WriteString(errOut + "\n")
			}
			if err != nil {
				return strings.TrimSpace(stdout.String()), strings.TrimSpace(stderr.String()), err
			}
		}
	}
	return strings.TrimSpace(stdout.String()), strings.TrimSpace(stderr.String()), nil
}

func runCICDShellStage(ctx context.Context, sourcePath string, stage CICDPlanStage, inputs map[string]string) (string, string, error) {
	workdir, err := resolveCICDWorkdir(sourcePath, stage.Workdir)
	if err != nil {
		return "", "", err
	}
	return runCICDCommandOutput(ctx, workdir, cicdInputEnv(inputs), "bash", "-lc", stage.Run)
}

func runCICDCommandOutput(ctx context.Context, dir string, env []string, name string, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if strings.TrimSpace(dir) != "" {
		cmd.Dir = dir
	}
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return strings.TrimSpace(stdout.String()), strings.TrimSpace(stderr.String()), err
}

func cicdInputEnv(inputs map[string]string) []string {
	if len(inputs) == 0 {
		return nil
	}
	env := make([]string, 0, len(inputs))
	for key, value := range inputs {
		envKey := "DOOPS_CICD_INPUT_" + cicdEnvKey(key)
		env = append(env, envKey+"="+value)
	}
	return env
}

func cicdEnvKey(key string) string {
	var out strings.Builder
	for _, r := range strings.ToUpper(key) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			out.WriteRune(r)
		} else {
			out.WriteByte('_')
		}
	}
	return out.String()
}

func resolveCICDWorkdir(sourcePath string, stageWorkdir string) (string, error) {
	sourcePath = strings.TrimSpace(sourcePath)
	if sourcePath == "" {
		return "", fmt.Errorf("source.path is required for local shell execution")
	}
	sourceAbs, err := filepath.Abs(sourcePath)
	if err != nil {
		return "", err
	}
	sourceReal, err := filepath.EvalSymlinks(sourceAbs)
	if err != nil {
		return "", err
	}
	workdir := strings.TrimSpace(stageWorkdir)
	if workdir == "" {
		return sourceReal, nil
	}
	if !filepath.IsAbs(workdir) {
		workdir = filepath.Join(sourceReal, workdir)
	}
	workdirAbs, err := filepath.Abs(workdir)
	if err != nil {
		return "", err
	}
	workdirReal, err := filepath.EvalSymlinks(workdirAbs)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(sourceReal, workdirReal)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("workdir %s is outside source.path %s", workdirReal, sourceReal)
	}
	return workdirReal, nil
}
