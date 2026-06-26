package main

import "testing"

func TestDefaultInstallCapabilitiesIncludesCoreChecks(t *testing.T) {
	checks := defaultInstallCapabilities()
	if len(checks) < 4 {
		t.Fatalf("expected at least 4 install checks, got %d", len(checks))
	}

	required := map[string]bool{}
	for _, check := range checks {
		required[check.name] = check.required
	}

	if !required["agent process"] {
		t.Fatalf("agent process check must be required")
	}
	if !required["container runtime"] {
		t.Fatalf("container runtime check must be required")
	}
	if _, ok := required["kubectl"]; !ok {
		t.Fatalf("kubectl check missing")
	}
	if _, ok := required["buildkit"]; !ok {
		t.Fatalf("buildkit check missing")
	}
}

func TestParseCapabilityStatus(t *testing.T) {
	output := `
--- Capabilities ---
container-runtime: docker /usr/bin/docker
kubectl: MISSING
buildctl: OK /usr/local/bin/buildctl
buildkit-sock: OK /run/buildkit/buildkitd.sock
`

	ok, detail := parseCapabilityStatus(output, "container runtime")
	if !ok || detail == "" {
		t.Fatalf("expected container runtime success, got ok=%v detail=%q", ok, detail)
	}

	ok, detail = parseCapabilityStatus(output, "kubectl")
	if ok || detail != "kubectl: MISSING" {
		t.Fatalf("expected kubectl missing, got ok=%v detail=%q", ok, detail)
	}

	ok, detail = parseCapabilityStatus(output, "buildkit")
	if !ok || detail == "" {
		t.Fatalf("expected buildkit success, got ok=%v detail=%q", ok, detail)
	}
}
