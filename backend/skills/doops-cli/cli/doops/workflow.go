package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type WorkspaceSourceIdentity struct {
	Repository string `json:"repository"`
	Revision   string `json:"revision"`
	Branch     string `json:"branch,omitempty"`
	TreeDigest string `json:"treeDigest,omitempty"`
}

const workspaceManifestAPIVersion = "doops.sh/v2"

type workflowCommand struct {
	File        string
	Target      string
	Inputs      map[string]string
	DryRun      bool
	AllowMutate bool
}

type workflowAgentCaller interface {
	CallAndCapture(toolName string, arguments map[string]interface{}) (string, error)
}

var pushWorkflowWorkspace = pushWorkspaceSnapshot

var openWorkflowAgent = func(
	server Server,
	sessionStore *SessionStore,
	sessionID string,
	verbose bool,
) (workflowAgentCaller, func()) {
	client := NewMCPClient(server, sessionStore, sessionID, verbose)
	client.Token = ResolveToken(server.Name, server.Token)
	return client, client.Close
}

func runCICDCommand(
	ctx context.Context,
	args []string,
	servers []Server,
	configErr error,
	sessionStore *SessionStore,
	sessionID string,
	verbose bool,
) error {
	if len(args) == 0 || strings.TrimSpace(args[0]) != "run" {
		return fmt.Errorf("only `cicd run` is supported; workflow validation and orchestration belong to doops-cicd")
	}
	request := workflowCommand{Inputs: map[string]string{}}
	sets := workflowSetFlags(request.Inputs)
	flags := flag.NewFlagSet("cicd run", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.StringVar(&request.File, "f", "", "workflow YAML file")
	flags.StringVar(&request.File, "file", "", "workflow YAML file")
	flags.StringVar(&request.Target, "target", "", "configured DoOps target")
	flags.Var(sets, "set", "workflow input override in key=value form")
	flags.BoolVar(&request.DryRun, "dry-run", false, "observe without mutation")
	flags.BoolVar(&request.AllowMutate, "allow-mutate", false, "authorize mutation")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if strings.TrimSpace(request.File) == "" {
		return fmt.Errorf("-f workflow YAML file is required")
	}
	if strings.TrimSpace(request.Target) == "" {
		return fmt.Errorf("-target is required; the CLI must not infer a deployment target")
	}
	if request.DryRun == request.AllowMutate {
		return fmt.Errorf("exactly one of --dry-run or --allow-mutate is required")
	}
	if strings.TrimSpace(sessionID) == "" {
		return fmt.Errorf("-session is required for `cicd run`")
	}
	if configErr != nil {
		return configErr
	}
	server := findServer(servers, request.Target)
	if server == nil {
		return fmt.Errorf("target %q not found; configure it with `doops add`", request.Target)
	}
	sourceDirectory, workflowPath, err := workflowPathInGitRepository(request.File)
	if err != nil {
		return err
	}
	if err := validateWorkflowInputs(request.Inputs); err != nil {
		return err
	}
	if strings.TrimSpace(server.Gateway) == "" {
		return fmt.Errorf("target %q must use a configured DoOps gateway", server.Name)
	}

	client, closeClient := openWorkflowAgent(*server, sessionStore, sessionID, verbose)
	defer closeClient()

	workspaceCommit, err := pushWorkflowWorkspace(*server, sourceDirectory, "", false, nil, sessionID)
	if err != nil {
		return fmt.Errorf("push workflow workspace: %w", err)
	}
	if !validWorkspaceCommit(workspaceCommit) {
		return fmt.Errorf("push workflow workspace returned an invalid commit")
	}

	executionMode := "dry-run"
	if request.AllowMutate {
		executionMode = "apply"
	}
	instruction, err := json.Marshal(map[string]interface{}{
		"task":               "execute-doops-cicd-workflow",
		"skill":              "$doops-cicd",
		"executionMode":      executionMode,
		"mutationAuthorized": request.AllowMutate,
		"workflowPath":       workflowPath,
		"workspaceCommit":    workspaceCommit,
		"inputs":             request.Inputs,
		"resultContract": map[string]interface{}{
			"apiVersion":      workspaceManifestAPIVersion,
			"kind":            "DeploymentRun",
			"requireEvidence": true,
		},
	})
	if err != nil {
		return fmt.Errorf("encode doops-cicd instruction: %w", err)
	}
	output, err := client.CallAndCapture("doops_agent_prompt", map[string]interface{}{
		"instruction":      string(instruction),
		"response_format":  "json",
		"workspace_commit": workspaceCommit,
	})
	if err != nil {
		return fmt.Errorf("ask doops-cicd: %w", err)
	}
	if err := validateWorkflowResult(output, executionMode); err != nil {
		return fmt.Errorf("doops-cicd returned invalid execution evidence: %w", err)
	}
	if strings.TrimSpace(output) != "" {
		fmt.Print(output)
		if !strings.HasSuffix(output, "\n") {
			fmt.Println()
		}
	}
	return nil
}

type workflowSetFlags map[string]string

func (f workflowSetFlags) String() string {
	if len(f) == 0 {
		return ""
	}
	parts := make([]string, 0, len(f))
	for key, value := range f {
		parts = append(parts, key+"="+value)
	}
	return strings.Join(parts, ",")
}

func (f workflowSetFlags) Set(value string) error {
	key, val, ok := strings.Cut(value, "=")
	if !ok || strings.TrimSpace(key) == "" {
		return fmt.Errorf("--set must be key=value")
	}
	f[strings.TrimSpace(key)] = val
	return nil
}

func validateWorkflowInputs(inputs map[string]string) error {
	for key := range inputs {
		if strings.TrimSpace(key) == "" || strings.TrimSpace(key) != key {
			return fmt.Errorf("workflow input name must be non-empty and trimmed")
		}
	}
	return nil
}

func workflowPathInGitRepository(path string) (root, relative string, err error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", "", fmt.Errorf("resolve workflow path: %w", err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return "", "", fmt.Errorf("read workflow file: %w", err)
	}
	if info.IsDir() {
		return "", "", fmt.Errorf("workflow path must be a file")
	}
	for directory := filepath.Dir(absolute); ; directory = filepath.Dir(directory) {
		if _, statErr := os.Stat(filepath.Join(directory, ".git")); statErr == nil {
			relative, relErr := filepath.Rel(directory, absolute)
			if relErr != nil || relative == "." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || relative == ".." {
				return "", "", fmt.Errorf("workflow path escapes Git repository")
			}
			return directory, filepath.ToSlash(relative), nil
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return "", "", fmt.Errorf("workflow file %q is not inside a Git repository", path)
		}
	}
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

func validateWorkflowResult(output, executionMode string) error {
	var result struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Spec       struct {
			Mode string `json:"mode"`
		} `json:"spec"`
		Status struct {
			Phase         string          `json:"phase"`
			MutationCount *int            `json:"mutationCount"`
			Evidence      json.RawMessage `json:"evidence"`
		} `json:"status"`
	}
	decoder := json.NewDecoder(strings.NewReader(output))
	if err := decoder.Decode(&result); err != nil {
		return fmt.Errorf("decode DeploymentRun: %w", err)
	}
	var trailing interface{}
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("DeploymentRun must be exactly one JSON object")
	}
	if result.APIVersion != workspaceManifestAPIVersion || result.Kind != "DeploymentRun" {
		return fmt.Errorf("result must be doops.sh/v2 DeploymentRun")
	}
	if result.Spec.Mode != executionMode {
		return fmt.Errorf("result mode %q does not match requested mode %q", result.Spec.Mode, executionMode)
	}
	switch result.Status.Phase {
	case "planned", "converged", "blocked", "failed", "outcome-unknown":
	default:
		return fmt.Errorf("unsupported result phase %q", result.Status.Phase)
	}
	if result.Status.MutationCount == nil || *result.Status.MutationCount < 0 {
		return fmt.Errorf("result mutationCount is required and must be non-negative")
	}
	if executionMode == "dry-run" && *result.Status.MutationCount != 0 {
		return fmt.Errorf("dry-run result must have mutationCount=0")
	}
	if len(result.Status.Evidence) == 0 || string(result.Status.Evidence) == "null" {
		return fmt.Errorf("result evidence is required")
	}
	var evidence []struct {
		Subject    string          `json:"subject"`
		Module     string          `json:"module"`
		ObservedAt string          `json:"observedAt"`
		Result     json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(result.Status.Evidence, &evidence); err != nil {
		return fmt.Errorf("decode result evidence: %w", err)
	}
	for index, item := range evidence {
		if strings.TrimSpace(item.Subject) == "" ||
			strings.TrimSpace(item.Module) == "" ||
			strings.TrimSpace(item.ObservedAt) == "" ||
			len(item.Result) == 0 ||
			string(item.Result) == "null" {
			return fmt.Errorf("evidence[%d] must bind subject, module, observedAt and result", index)
		}
	}
	return nil
}
