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

type cicdAgenticRunnerFactory func(plan DeploymentPlan, sourceDirectory string) (deploymentAgenticExecutor, func(), error)

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

type CICDCommand struct {
	Command           string
	File              string
	Inputs            map[string]string
	DryRun            bool
	AllowMutate       bool
	MaxIterations     int
	MaxNoProgressRuns int
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
	flags.IntVar(&request.MaxIterations, "max-iterations", 12, "Maximum agent attempts")
	flags.IntVar(&request.MaxNoProgressRuns, "max-no-progress", 3, "Maximum unchanged agent attempts")
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
		DryRun:        request.DryRun,
		AllowMutate:   request.AllowMutate,
		MaxIterations: request.MaxIterations,
		MaxNoProgress: request.MaxNoProgressRuns,
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
