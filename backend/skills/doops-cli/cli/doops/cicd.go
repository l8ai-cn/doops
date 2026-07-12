package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
)

type CICDCommand struct {
	Command           string
	File              string
	Inputs            map[string]string
	DryRun            bool
	AllowMutate       bool
	MaxIterations     int
	MaxNoProgressRuns int
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

func buildCICDCommand(args []string) (CICDCommand, error) {
	if len(args) == 0 {
		return CICDCommand{}, fmt.Errorf("cicd subcommand is required")
	}
	command := strings.TrimSpace(args[0])
	switch command {
	case "lint", "plan", "run":
	default:
		return CICDCommand{}, fmt.Errorf("unsupported cicd command %q", command)
	}

	request := CICDCommand{
		Command: command,
		Inputs:  map[string]string{},
	}
	sets := cicdSetFlags(request.Inputs)
	flags := flag.NewFlagSet("cicd "+command, flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.StringVar(&request.File, "f", "", "DeploymentTemplate YAML file")
	flags.StringVar(&request.File, "file", "", "DeploymentTemplate YAML file")
	flags.Var(sets, "set", "Template parameter override in key=value form")
	flags.BoolVar(&request.DryRun, "dry-run", false, "Observe and reconcile without mutation")
	flags.BoolVar(&request.AllowMutate, "allow-mutate", false, "Approve a mutating reconciliation")
	flags.IntVar(&request.MaxIterations, "max-iterations", 12, "Maximum reconciliation iterations")
	flags.IntVar(&request.MaxNoProgressRuns, "max-no-progress", 3, "Stop after this many unchanged reconciliation iterations")
	if err := flags.Parse(args[1:]); err != nil {
		return CICDCommand{}, err
	}
	if strings.TrimSpace(request.File) == "" {
		return CICDCommand{}, fmt.Errorf("-f deployment template file is required")
	}
	return request, nil
}

type cicdReconcilerFactory func(plan DeploymentPlan) (deploymentReconciler, func(), error)

func runCICDCommand(ctx context.Context, args []string, newReconciler cicdReconcilerFactory) error {
	request, err := buildCICDCommand(args)
	if err != nil {
		return err
	}
	template, err := loadDeploymentTemplate(request.File)
	if err != nil {
		return err
	}

	switch request.Command {
	case "lint":
		fmt.Printf("deployment template ok: %s\n", template.Metadata.Name)
		return nil
	case "plan":
		plan, err := buildDeploymentPlan(template, request.Inputs)
		if err != nil {
			return err
		}
		return writeCICDJSON(plan)
	case "run":
		if !request.DryRun && !request.AllowMutate {
			return fmt.Errorf("mutating reconciliation requires --allow-mutate")
		}
		if newReconciler == nil {
			return fmt.Errorf("deployment reconciler factory is required")
		}
		plan, err := buildDeploymentPlan(template, request.Inputs)
		if err != nil {
			return err
		}
		if err := attestDeploymentPlanFromEnvironment(&plan); err != nil {
			return err
		}
		reconciler, cleanup, err := newReconciler(plan)
		if err != nil {
			return err
		}
		if cleanup != nil {
			defer cleanup()
		}
		run, reconcileErr := reconcileDeploymentPlan(ctx, plan, reconciler, DeploymentReconcileOptions{
			DryRun:            request.DryRun,
			MaxIterations:     request.MaxIterations,
			MaxNoProgressRuns: request.MaxNoProgressRuns,
		})
		if writeErr := writeCICDJSON(run); writeErr != nil {
			return writeErr
		}
		return reconcileErr
	default:
		return fmt.Errorf("unsupported cicd command %q", request.Command)
	}
}

func writeCICDJSON(value interface{}) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
