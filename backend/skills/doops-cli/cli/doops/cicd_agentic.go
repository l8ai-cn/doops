package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type CICDAgenticRunRequest struct {
	SessionID   string
	DryRun      bool
	AllowMutate bool
}
type CICDAgenticRun struct {
	PlanDigest string               `json:"planDigest"`
	Target     string               `json:"target"`
	Workspace  string               `json:"workspace"`
	Result     ReconciliationResult `json:"result"`
}
type deploymentAgenticExecutor interface {
	Run(context.Context, DeploymentPlan, CICDAgenticRunRequest) (CICDAgenticRun, error)
}

type cicdAgenticRunnerFactory func(plan DeploymentPlan, sourceDirectory string) (deploymentAgenticExecutor, func(), error)

type agenticDeploymentRunner struct {
	server          Server
	sourceDirectory string
	sessionID       string
	pushWorkspace   func(Server, string, string, *CICDSourceRelease) (string, error)
	ask             func(map[string]interface{}) (ReconciliationResult, error)
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

type CICDCommand struct {
	Command     string
	File        string
	Inputs      map[string]string
	DryRun      bool
	AllowMutate bool
}

func runCICDCommand(ctx context.Context, args []string, newRunner cicdAgenticRunnerFactory) error {
	if len(args) == 0 {
		return fmt.Errorf("cicd subcommand is required")
	}
	command := strings.TrimSpace(args[0])
	if command != "lint" && command != "plan" && command != "run" {
		return fmt.Errorf("unsupported cicd command %q", command)
	}
	request := CICDCommand{Command: command, Inputs: map[string]string{}}
	sets := cicdSetFlags(request.Inputs)
	flags := flag.NewFlagSet("cicd "+command, flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.StringVar(&request.File, "f", "", "DeploymentTemplate YAML file")
	flags.StringVar(&request.File, "file", "", "DeploymentTemplate YAML file")
	flags.Var(sets, "set", "Template parameter override in key=value form")
	flags.BoolVar(&request.DryRun, "dry-run", false, "Observe and validate without mutation")
	flags.BoolVar(&request.AllowMutate, "allow-mutate", false, "Approve a mutating deployment")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if strings.TrimSpace(request.File) == "" {
		return fmt.Errorf("-f deployment template file is required")
	}
	template, err := loadDeploymentTemplate(request.File)
	if err != nil {
		return err
	}
	if request.Command == "lint" {
		fmt.Printf("deployment template ok: %s\n", template.Metadata.Name)
		return nil
	}
	plan, err := buildDeploymentPlan(template, request.Inputs)
	if err != nil {
		return err
	}
	if request.Command == "plan" {
		return writeCICDJSON(plan)
	}
	if request.DryRun && request.AllowMutate {
		return fmt.Errorf("--dry-run and --allow-mutate are mutually exclusive")
	}
	if !request.DryRun && !request.AllowMutate {
		return fmt.Errorf("mutating deployment requires --allow-mutate")
	}
	if newRunner == nil {
		return fmt.Errorf("Agent-native deployment runner factory is required")
	}
	sourceDirectory, err := findCICDSourceDirectory(request.File)
	if err != nil {
		return err
	}
	if err := validateCICDSourceWorkspace(plan, sourceDirectory); err != nil {
		return err
	}
	runner, cleanup, err := newRunner(plan, sourceDirectory)
	if err != nil {
		return err
	}
	if cleanup != nil {
		defer cleanup()
	}
	run, runErr := runner.Run(ctx, plan, CICDAgenticRunRequest{
		DryRun:      request.DryRun,
		AllowMutate: request.AllowMutate,
	})
	if writeErr := writeCICDJSON(run); writeErr != nil {
		return writeErr
	}
	return runErr
}

func validateCICDSourceWorkspace(plan DeploymentPlan, sourceDirectory string) error {
	if plan.Spec.Release.Source == nil {
		return nil
	}
	runGit := func(args ...string) (string, error) {
		command := exec.Command("git", append([]string{"-C", sourceDirectory}, args...)...)
		output, err := command.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
		}
		return strings.TrimSpace(string(output)), nil
	}
	head, err := runGit("rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("resolve deployment workspace revision: %w", err)
	}
	if head != plan.Spec.Release.Source.Revision {
		return fmt.Errorf(
			"deployment workspace HEAD %s does not match declared release revision %s",
			head,
			plan.Spec.Release.Source.Revision,
		)
	}
	status, err := runGit("status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return fmt.Errorf("inspect deployment workspace: %w", err)
	}
	if status != "" {
		return fmt.Errorf("deployment workspace has uncommitted changes")
	}
	return nil
}

func writeCICDJSON(value interface{}) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func newAgenticDeploymentRunner(server Server, sourceDirectory, sessionID string, client *MCPClient) (*agenticDeploymentRunner, error) {
	if strings.TrimSpace(sourceDirectory) == "" || strings.TrimSpace(sessionID) == "" || client == nil {
		return nil, fmt.Errorf("deployment source directory, session ID, and Ask client are required")
	}
	return &agenticDeploymentRunner{server: server, sourceDirectory: sourceDirectory, sessionID: sessionID,
		pushWorkspace: func(target Server, source, session string, identity *CICDSourceRelease) (string, error) {
			return pushWorkspaceSnapshotWithSource(target, source, "", false, nil, session, identity)
		},
		ask: func(arguments map[string]interface{}) (ReconciliationResult, error) {
			var result ReconciliationResult
			err := client.CallStructured("doops_agent_prompt", arguments, &result)
			return result, err
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
	request.SessionID = r.sessionID
	instruction, err := buildAgenticDeploymentInstruction(plan, request)
	if err != nil {
		return CICDAgenticRun{}, err
	}
	arguments, err := buildAgenticDeploymentArguments(plan, request, instruction)
	if err != nil {
		return CICDAgenticRun{}, err
	}
	workspaceCommit, err := r.pushWorkspace(
		r.server,
		r.sourceDirectory,
		r.sessionID,
		plan.Spec.Release.Source,
	)
	if err != nil {
		return CICDAgenticRun{}, fmt.Errorf("push deployment workspace: %w", err)
	}
	if !validWorkspaceCommit(workspaceCommit) {
		return CICDAgenticRun{}, fmt.Errorf("push deployment workspace returned an invalid commit")
	}
	arguments["workspace_commit"] = workspaceCommit
	result, err := r.ask(arguments)
	if err != nil {
		return CICDAgenticRun{}, fmt.Errorf("ask deployment agent: %w", err)
	}
	run := CICDAgenticRun{
		PlanDigest: plan.Digest,
		Target:     plan.Spec.Target.ExecutionTarget,
		Workspace:  "/root/ws/" + r.sessionID,
		Result:     result,
	}
	if err := validateReconciliationResult(plan, workspaceCommit, result); err != nil {
		return run, err
	}
	if result.Status != ReconciliationConverged {
		return run, fmt.Errorf("deployment did not converge: %s", result.Status)
	}
	return run, nil
}

func validWorkspaceCommit(value string) bool {
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

func buildAgenticDeploymentArguments(plan DeploymentPlan, request CICDAgenticRunRequest, instruction string) (map[string]interface{}, error) {
	if request.DryRun == request.AllowMutate {
		return nil, fmt.Errorf("deployment execution requires exactly one of dry-run or mutation authorization")
	}
	if !ociDigestPattern.MatchString(plan.Digest) {
		return nil, fmt.Errorf("deployment plan digest must be a sha256 digest")
	}
	if strings.TrimSpace(instruction) == "" {
		return nil, fmt.Errorf("deployment instruction is required")
	}
	executionMode := "dry-run"
	if request.AllowMutate {
		executionMode = "apply"
	}
	arguments := map[string]interface{}{
		"instruction":     instruction,
		"response_format": "json",
		"operation":       "reconcile",
		"plan_digest":     plan.Digest,
		"execution_mode":  executionMode,
	}
	if plan.Spec.Release.Source != nil {
		arguments["source_revision"] = plan.Spec.Release.Source.Revision
	}
	return arguments, nil
}

func buildAgenticDeploymentInstruction(plan DeploymentPlan, request CICDAgenticRunRequest) (string, error) {
	if request.DryRun == request.AllowMutate {
		return "", fmt.Errorf("deployment execution requires exactly one of dry-run or mutation authorization")
	}
	executionMode := "dry-run"
	if request.AllowMutate {
		executionMode = "apply"
	}
	envelope := struct {
		Task           string         `json:"task"`
		Skill          string         `json:"skill"`
		ExecutionMode  string         `json:"executionMode"`
		DeploymentPlan DeploymentPlan `json:"deploymentPlan"`
	}{
		Task:           "reconcile-deployment-plan",
		Skill:          "semantic-deployment",
		ExecutionMode:  executionMode,
		DeploymentPlan: plan,
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return "", fmt.Errorf("encode DeploymentPlan for Ask: %w", err)
	}
	return string(encoded), nil
}

func findCICDSourceDirectory(templatePath string) (string, error) {
	path, err := filepath.Abs(templatePath)
	if err != nil {
		return "", fmt.Errorf("resolve deployment template path: %w", err)
	}
	for directory := filepath.Dir(path); ; directory = filepath.Dir(directory) {
		if _, err := os.Stat(filepath.Join(directory, ".git")); err == nil {
			return directory, nil
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return "", fmt.Errorf("deployment template %q is not inside a Git repository", templatePath)
		}
	}
}
