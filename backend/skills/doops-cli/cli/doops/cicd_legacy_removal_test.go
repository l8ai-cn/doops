package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCICDSubmitHasNoClientSigningKeyPath(t *testing.T) {
	t.Helper()
	forbidden := "DOOPS_CICD_PLAN_" + "SIGNING_KEY"
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("list CLI source files: %v", err)
	}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		source, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		if strings.Contains(string(source), forbidden) {
			t.Fatalf("client CI/CD source must not require a signing key: %s", file)
		}
	}
}

func TestCICDSubmitHasNoLocalPlanCompiler(t *testing.T) {
	forbidden := []string{
		"type Deployment" + "Plan struct",
		"func buildDeployment" + "Plan",
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("list CLI source files: %v", err)
	}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		source, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		for _, marker := range forbidden {
			if strings.Contains(string(source), marker) {
				t.Fatalf("client CI/CD source must not compile local deployment plans: %s", file)
			}
		}
	}
}
