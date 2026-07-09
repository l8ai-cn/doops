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
	Command     string
	File        string
	Inputs      map[string]string
	DryRun      bool
	AllowMutate bool
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
	cmd := strings.TrimSpace(args[0])
	switch cmd {
	case "lint", "plan", "run":
	default:
		return CICDCommand{}, fmt.Errorf("unsupported cicd command %q", cmd)
	}
	req := CICDCommand{
		Command: cmd,
		Inputs:  map[string]string{},
	}
	sets := cicdSetFlags(req.Inputs)
	fs := flag.NewFlagSet("cicd "+cmd, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&req.File, "f", "", "Workflow YAML file")
	fs.StringVar(&req.File, "file", "", "Workflow YAML file")
	fs.Var(sets, "set", "Workflow input override in key=value form")
	fs.BoolVar(&req.DryRun, "dry-run", false, "Skip mutating stages")
	fs.BoolVar(&req.AllowMutate, "allow-mutate", false, "Allow confirmed mutating stages")
	if err := fs.Parse(args[1:]); err != nil {
		return CICDCommand{}, err
	}
	if strings.TrimSpace(req.File) == "" {
		return CICDCommand{}, fmt.Errorf("-f workflow file is required")
	}
	return req, nil
}

// cicdExecutorFactory builds a live gateway executor for the resolved target.
// It returns the executor, a cleanup function, and an error. main.go supplies
// the real (gateway-backed) factory; tests may pass nil to stay offline.
type cicdExecutorFactory func(target string) (cicdExecutor, func(), error)

// cicdSourceSyncFactory builds a source syncer that pushes the local source
// tree into the remote session workspace before agent-native stages run.
type cicdSourceSyncFactory func(target, session string) (func(src string) error, error)

func runCICDCommand(ctx context.Context, args []string, newExecutor cicdExecutorFactory) error {
	return runCICDCommandWithSync(ctx, args, newExecutor, nil, "")
}

func runCICDCommandWithSync(ctx context.Context, args []string, newExecutor cicdExecutorFactory, newSourceSync cicdSourceSyncFactory, session string) error {
	req, err := buildCICDCommand(args)
	if err != nil {
		return err
	}
	workflow, err := loadCICDWorkflow(req.File)
	if err != nil {
		return err
	}
	switch req.Command {
	case "lint":
		fmt.Printf("workflow ok: %s\n", workflow.Metadata.Name)
	case "plan":
		plan, err := buildCICDPlan(workflow, req.Inputs)
		if err != nil {
			return err
		}
		return writeCICDJSON(plan)
	case "run":
		opts := CICDRunOptions{
			Inputs:      req.Inputs,
			DryRun:      req.DryRun,
			AllowMutate: req.AllowMutate,
			Session:     strings.TrimSpace(session),
		}
		// CICD is agent-driven: hand agent-native stages to the doagent. Build a
		// live executor whenever one is available; the agent decides how to
		// honor dry-run/mutation. If no executor can be built (e.g. no -session)
		// a dry-run still works offline (stages are recorded as planned), while
		// an apply run surfaces the executor error instead of silently no-op'ing.
		if newExecutor != nil {
			plan, err := buildCICDPlan(workflow, req.Inputs)
			if err != nil {
				return err
			}
			target := strings.TrimSpace(plan.Inputs["target"])
			if target == "" {
				return fmt.Errorf("input target is required to execute agent-native stages")
			}
			executor, cleanup, execErr := newExecutor(target)
			if execErr != nil {
				if !req.DryRun {
					return execErr
				}
			} else {
				if cleanup != nil {
					defer cleanup()
				}
				opts.Executor = executor
				if newSourceSync != nil && strings.TrimSpace(session) != "" {
					syncer, syncErr := newSourceSync(target, session)
					if syncErr != nil {
						if !req.DryRun {
							return syncErr
						}
					} else {
						opts.SourceSync = syncer
					}
				}
			}
		}
		result, err := runCICDWorkflow(ctx, workflow, opts)
		if writeErr := writeCICDJSON(result); writeErr != nil {
			return writeErr
		}
		return err
	default:
		return fmt.Errorf("unsupported cicd command %q", req.Command)
	}
	return nil
}

func writeCICDJSON(value interface{}) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(value)
}
