package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

const (
	releaseRequestAPIVersion = "doops.sh/v3"
	releaseRequestKind       = "ReleaseRequest"
)

type ReleaseRequest struct {
	APIVersion   string            `json:"apiVersion"`
	Kind         string            `json:"kind"`
	RepositoryID string            `json:"repositoryId"`
	Revision     string            `json:"revision"`
	WorkflowPath string            `json:"workflowPath"`
	Inputs       map[string]string `json:"inputs,omitempty"`
	DryRun       bool              `json:"dryRun"`
	AllowMutate  bool              `json:"allowMutate"`
}

type ReleaseResult struct {
	ReleaseID string `json:"releaseId"`
	Status    string `json:"status"`
}

type CICDSubmitCommand struct {
	Target  string
	Request ReleaseRequest
}

type releaseSubmitter func(ReleaseRequest) (ReleaseResult, error)

type CICDAgenticRunRequest struct {
	SessionID     string
	DryRun        bool
	AllowMutate   bool
	MaxIterations int
	MaxNoProgress int
}
type CICDAgenticRun struct {
	PlanDigest string `json:"planDigest"`
	Target     string `json:"target"`
	Workspace  string `json:"workspace"`
	Outcome    string `json:"outcome"`
}
type deploymentAgenticExecutor interface {
	Run(context.Context, DeploymentPlan, CICDAgenticRunRequest) (CICDAgenticRun, error)
}
type agenticDeploymentRunner struct {
	server          Server
	sourceDirectory string
	sessionID       string
	pushWorkspace   func(Server, string, string) error
	ask             func(string) (string, error)
}

type cicdSetFlags map[string]string

func (f cicdSetFlags) String() string {
	if len(f) == 0 {
		return ""
	}
	parts := make([]string, 0, len(f))
	for key, value := range f {
		parts = append(parts, key+"="+value)
	}
	return strings.Join(parts, ",")
}

func (f cicdSetFlags) Set(value string) error {
	key, val, ok := strings.Cut(value, "=")
	if !ok || strings.TrimSpace(key) == "" {
		return fmt.Errorf("--set must be key=value")
	}
	f[strings.TrimSpace(key)] = val
	return nil
}

func buildCICDSubmitCommand(args []string) (CICDSubmitCommand, error) {
	if len(args) == 0 || strings.TrimSpace(args[0]) != "submit" {
		return CICDSubmitCommand{}, fmt.Errorf("cicd submit is required")
	}

	command := CICDSubmitCommand{
		Request: ReleaseRequest{
			APIVersion: releaseRequestAPIVersion,
			Kind:       releaseRequestKind,
			Inputs:     map[string]string{},
		},
	}
	sets := cicdSetFlags(command.Request.Inputs)
	flags := flag.NewFlagSet("cicd submit", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.StringVar(&command.Target, "target", "", "Configured remote CI/CD control-plane target")
	flags.StringVar(&command.Request.RepositoryID, "repository-id", "", "Registered source repository ID")
	flags.StringVar(&command.Request.Revision, "revision", "", "Immutable 40-character Git commit")
	flags.StringVar(&command.Request.WorkflowPath, "workflow", "", "Repository-relative deployment workflow path")
	flags.Var(sets, "set", "Workflow parameter override in key=value form")
	flags.BoolVar(&command.Request.DryRun, "dry-run", false, "Validate remotely without mutation")
	flags.BoolVar(&command.Request.AllowMutate, "allow-mutate", false, "Approve a mutating release")
	var localFile string
	flags.StringVar(&localFile, "f", "", "Unsupported: local deployment files are not accepted")
	flags.StringVar(&localFile, "file", "", "Unsupported: local deployment files are not accepted")
	if err := flags.Parse(args[1:]); err != nil {
		return CICDSubmitCommand{}, err
	}
	if strings.TrimSpace(localFile) != "" {
		return CICDSubmitCommand{}, fmt.Errorf("cicd submit does not accept local deployment files; use --workflow with a repository-relative path")
	}
	if strings.TrimSpace(command.Target) == "" {
		return CICDSubmitCommand{}, fmt.Errorf("--target remote CI/CD control-plane target is required")
	}
	if err := validateReleaseRequest(command.Request); err != nil {
		return CICDSubmitCommand{}, err
	}
	return command, nil
}

func validateReleaseRequest(request ReleaseRequest) error {
	if request.APIVersion != releaseRequestAPIVersion || request.Kind != releaseRequestKind {
		return fmt.Errorf("invalid release request type")
	}
	if strings.TrimSpace(request.RepositoryID) == "" {
		return fmt.Errorf("--repository-id is required")
	}
	if !immutableGitCommitPattern.MatchString(strings.TrimSpace(request.Revision)) {
		return fmt.Errorf("--revision must be an immutable 40-character Git commit")
	}
	workflow := strings.TrimSpace(request.WorkflowPath)
	if workflow == "" {
		return fmt.Errorf("--workflow is required")
	}
	if path.IsAbs(workflow) || workflow != path.Clean(workflow) || workflow == "." || strings.HasPrefix(workflow, "../") {
		return fmt.Errorf("--workflow must be a repository-relative path")
	}
	if !strings.HasPrefix(workflow, "deploy/workflows/") {
		return fmt.Errorf("--workflow must be under deploy/workflows/")
	}
	if !request.DryRun && !request.AllowMutate {
		return fmt.Errorf("mutating release submission requires --allow-mutate")
	}
	return nil
}

func runCICDSubmitCommand(ctx context.Context, args []string, submit releaseSubmitter) error {
	command, err := buildCICDSubmitCommand(args)
	if err != nil {
		return err
	}
	return executeCICDSubmitCommand(ctx, command, submit)
}

func executeCICDSubmitCommand(ctx context.Context, command CICDSubmitCommand, submit releaseSubmitter) error {
	if submit == nil {
		return fmt.Errorf("remote release submitter is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	result, err := submit(command.Request)
	if err != nil {
		return err
	}
	if strings.TrimSpace(result.ReleaseID) == "" || strings.TrimSpace(result.Status) == "" {
		return fmt.Errorf("remote release submission returned an incomplete result")
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
}

func (c *MCPClient) SubmitRelease(request ReleaseRequest) (ReleaseResult, error) {
	var result ReleaseResult
	if err := validateReleaseRequest(request); err != nil {
		return result, err
	}
	err := c.CallStructured("doops_cicd_submit", map[string]interface{}{
		"request": request,
	}, &result)
	return result, err
}

func newAgenticDeploymentRunner(server Server, sourceDirectory, sessionID string, client *MCPClient) (*agenticDeploymentRunner, error) {
	if strings.TrimSpace(sourceDirectory) == "" || strings.TrimSpace(sessionID) == "" || client == nil {
		return nil, fmt.Errorf("deployment source directory, session ID, and Ask client are required")
	}
	return &agenticDeploymentRunner{server: server, sourceDirectory: sourceDirectory, sessionID: sessionID,
		pushWorkspace: func(target Server, source, session string) error {
			return Push(target, source, "", false, nil, session)
		},
		ask: func(instruction string) (string, error) {
			return client.CallAndCapture("doops_agent_prompt", map[string]interface{}{"instruction": instruction})
		},
	}, nil
}

func (r agenticDeploymentRunner) Run(ctx context.Context, plan DeploymentPlan, request CICDAgenticRunRequest) (CICDAgenticRun, error) {
	if err := ctx.Err(); err != nil {
		return CICDAgenticRun{}, err
	}
	if r.pushWorkspace == nil || r.ask == nil {
		return CICDAgenticRun{}, fmt.Errorf("agentic deployment runner is incomplete")
	}
	if err := r.pushWorkspace(r.server, r.sourceDirectory, r.sessionID); err != nil {
		return CICDAgenticRun{}, fmt.Errorf("push deployment workspace: %w", err)
	}
	request.SessionID = r.sessionID
	instruction, err := buildAgenticDeploymentInstruction(plan, request)
	if err != nil {
		return CICDAgenticRun{}, err
	}
	outcome, err := r.ask(instruction)
	if err != nil {
		return CICDAgenticRun{}, fmt.Errorf("ask deployment agent: %w", err)
	}
	outcome = strings.TrimSpace(outcome)
	if outcome == "" {
		return CICDAgenticRun{}, fmt.Errorf("deployment agent returned no outcome")
	}
	return CICDAgenticRun{PlanDigest: plan.Digest, Target: plan.Spec.Target.ExecutionTarget, Workspace: "/root/ws/" + r.sessionID, Outcome: outcome}, nil
}

func buildAgenticDeploymentInstruction(plan DeploymentPlan, request CICDAgenticRunRequest) (string, error) {
	encodedPlan, err := json.Marshal(plan)
	if err != nil {
		return "", fmt.Errorf("encode DeploymentPlan for Ask: %w", err)
	}
	return fmt.Sprintf(
		"Act as the deployment owner for this DeploymentPlan. The synchronized workspace is /root/ws/%s. "+
			"Treat the resolved target profile and artifact contract as authoritative. Inspect the workspace and actual target, then use your available tools to reach the declared desired state. "+
			"Validate every requiredEvidence item before reporting success. If the goal cannot be reached, preserve the failure evidence, restore the last known good revision, and explain the blocking fact. "+
			"Do not generate deployment scripts, replay a stage list, or infer another target. Dry run: %t. Mutation is authorized: %t. "+
			"Bounded reconciliation guidance: stop after %d unchanged attempts or %d total attempts. Return the final status and concrete evidence observed.\nDeploymentPlan:\n%s",
		request.SessionID, request.DryRun, request.AllowMutate, request.MaxNoProgress, request.MaxIterations, string(encodedPlan),
	), nil
}

func findCICDSourceDirectory(templatePath string) (string, error) {
	path, err := filepath.Abs(templatePath)
	if err != nil {
		return "", fmt.Errorf("resolve deployment template path: %w", err)
	}
	for directory := filepath.Dir(path); ; directory = filepath.Dir(directory) {
		if info, err := os.Stat(filepath.Join(directory, ".git")); err == nil && info.IsDir() {
			return directory, nil
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return "", fmt.Errorf("deployment template %q is not inside a Git repository", templatePath)
		}
	}
}
