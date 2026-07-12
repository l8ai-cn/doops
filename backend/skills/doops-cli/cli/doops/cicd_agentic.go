package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
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
	pushWorkspace   func(Server, string, string) error
	ask             func(string) (ReconciliationResult, error)
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
		pushWorkspace: func(target Server, source, session string) error {
			return Push(target, source, "", false, nil, session)
		},
		ask: func(instruction string) (ReconciliationResult, error) {
			var result ReconciliationResult
			err := client.CallStructured("doops_agent_prompt", map[string]interface{}{
				"instruction":     instruction,
				"response_format": "json",
			}, &result)
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
	if err := r.pushWorkspace(r.server, r.sourceDirectory, r.sessionID); err != nil {
		return CICDAgenticRun{}, fmt.Errorf("push deployment workspace: %w", err)
	}
	request.SessionID = r.sessionID
	instruction, err := buildAgenticDeploymentInstruction(plan, request)
	if err != nil {
		return CICDAgenticRun{}, err
	}
	result, err := r.ask(instruction)
	if err != nil {
		return CICDAgenticRun{}, fmt.Errorf("ask deployment agent: %w", err)
	}
	run := CICDAgenticRun{
		PlanDigest: plan.Digest,
		Target:     plan.Spec.Target.ExecutionTarget,
		Workspace:  "/root/ws/" + r.sessionID,
		Result:     result,
	}
	if err := validateReconciliationResult(plan, result); err != nil {
		return run, err
	}
	if result.Status != ReconciliationConverged {
		return run, fmt.Errorf("deployment did not converge: %s", result.Status)
	}
	return run, nil
}

func buildAgenticDeploymentInstruction(plan DeploymentPlan, request CICDAgenticRunRequest) (string, error) {
	encodedPlan, err := json.Marshal(plan)
	if err != nil {
		return "", fmt.Errorf("encode DeploymentPlan for Ask: %w", err)
	}
	return fmt.Sprintf(
		"You own reconciliation for this DeploymentPlan. The synchronized workspace is /root/ws/%s. "+
			"Treat the resolved environment profile, desired state, acceptance criteria, and policy in the declaration as authoritative. "+
			"Use your available tools to inspect the workspace and actual target, then reconcile the declared desired state until it converges or the declared policy requires Blocked or Failed. "+
			"Never infer a different target or replace the declaration with a script, stage list, command list, or textual success claim. "+
			"Dry run: %t. Mutation is authorized: %t. "+
			"Return exactly one JSON object with apiVersion, kind=ReconciliationResult, planDigest, status, attempts, noProgressAttempts, evidence, and failureEvidence. "+
			"For converged, include every requiredEvidence item. For blocked or failed, include every requiredFailureEvidence item.\nDeploymentPlan:\n%s",
		request.SessionID, request.DryRun, request.AllowMutate, string(encodedPlan),
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
