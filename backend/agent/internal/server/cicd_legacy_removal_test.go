package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGatewayHasNoLegacyCICDReconcileProtocol(t *testing.T) {
	forbiddenTool := "doops_cicd_" + "reconcile"
	forbiddenKey := "DOOPS_CICD_PLAN_" + "PUBLIC_KEY"
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("list Gateway source files: %v", err)
	}
	apiFiles, err := filepath.Glob(filepath.Join("..", "..", "api", "*.go"))
	if err != nil {
		t.Fatalf("list Gateway API files: %v", err)
	}
	files = append(files, apiFiles...)
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		source, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		if strings.Contains(string(source), forbiddenTool) || strings.Contains(string(source), forbiddenKey) {
			t.Fatalf("Gateway must accept only remote CI/CD submissions, found legacy protocol in %s", file)
		}
	}
}
