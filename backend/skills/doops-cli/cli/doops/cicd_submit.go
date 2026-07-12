package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path"
	"regexp"
	"strings"
)

const (
	releaseRequestAPIVersion = "doops.sh/v3"
	releaseRequestKind       = "ReleaseRequest"
	releaseStatusAccepted    = "Accepted"
)

var immutableGitCommitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

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
	if err := validateReleaseResult(result); err != nil {
		return err
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
}

func validateReleaseResult(result ReleaseResult) error {
	if strings.TrimSpace(result.ReleaseID) == "" || strings.TrimSpace(result.Status) == "" {
		return fmt.Errorf("remote release submission returned an incomplete result")
	}
	if result.Status != releaseStatusAccepted {
		return fmt.Errorf("remote release submission returned non-accepted status %q", result.Status)
	}
	return nil
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
