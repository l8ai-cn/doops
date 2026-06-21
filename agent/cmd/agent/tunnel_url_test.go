package main

import "testing"

func TestBuildGatewayAgentURLUsesVersionedEndpoint(t *testing.T) {
	got, err := buildGatewayAgentURL("https://gateway.example.com", "dev", "local")
	if err != nil {
		t.Fatalf("build url: %v", err)
	}
	want := "wss://gateway.example.com/v1/agent/connect?cluster=dev&instance=local"
	if got != want {
		t.Fatalf("gateway URL mismatch\nwant: %s\n got: %s", want, got)
	}
}

func TestBuildGatewayAgentURLRejectsInsecureRemoteByDefault(t *testing.T) {
	// Ensure the default-deny path is exercised regardless of any ambient
	// DOOPS_ALLOW_INSECURE_GATEWAY value in the developer's shell.
	t.Setenv("DOOPS_ALLOW_INSECURE_GATEWAY", "")
	if _, err := buildGatewayAgentURL("http://203.0.113.10:42222", "dev", "local"); err == nil {
		t.Fatal("insecure (ws://) gateway URL to a non-loopback host must be rejected by default")
	}
}

func TestBuildGatewayAgentURLAllowsInsecureRemoteWithOptIn(t *testing.T) {
	t.Setenv("DOOPS_ALLOW_INSECURE_GATEWAY", "1")
	got, err := buildGatewayAgentURL("http://203.0.113.10:42222", "dev", "local")
	if err != nil {
		t.Fatalf("insecure gateway URL should be allowed with opt-in: %v", err)
	}
	want := "ws://203.0.113.10:42222/v1/agent/connect?cluster=dev&instance=local"
	if got != want {
		t.Fatalf("gateway URL mismatch\nwant: %s\n got: %s", want, got)
	}
}

func TestBuildGatewayAgentURLDefaultsToSecureScheme(t *testing.T) {
	got, err := buildGatewayAgentURL("gateway.example.com", "dev", "local")
	if err != nil {
		t.Fatalf("bare host should default to wss: %v", err)
	}
	want := "wss://gateway.example.com/v1/agent/connect?cluster=dev&instance=local"
	if got != want {
		t.Fatalf("gateway URL mismatch\nwant: %s\n got: %s", want, got)
	}
}

func TestBuildGatewayAgentURLAllowsInsecureLocalhostForDevelopment(t *testing.T) {
	got, err := buildGatewayAgentURL("http://localhost:42222", "dev", "local")
	if err != nil {
		t.Fatalf("local insecure gateway should be allowed: %v", err)
	}
	want := "ws://localhost:42222/v1/agent/connect?cluster=dev&instance=local"
	if got != want {
		t.Fatalf("gateway URL mismatch\nwant: %s\n got: %s", want, got)
	}
}
