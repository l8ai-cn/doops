package main

import "testing"

func TestGatewayTargetsURLUsesVersionedEndpoint(t *testing.T) {
	got, err := gatewayTargetsURL("https://gateway.example.com")
	if err != nil {
		t.Fatalf("build targets URL: %v", err)
	}
	want := "https://gateway.example.com/v1/targets"
	if got != want {
		t.Fatalf("targets URL mismatch\nwant: %s\n got: %s", want, got)
	}
}

func TestGatewayTargetsRejectsInsecureHTTPGatewayByDefault(t *testing.T) {
	t.Setenv("DOOPS_ALLOW_INSECURE_GATEWAY", "")
	if _, err := gatewayTargetsURL("http://203.0.113.10:42222"); err == nil {
		t.Fatal("expected insecure non-loopback gateway targets URL to be rejected by default")
	}
}

func TestGatewayTargetsAllowsInsecureHTTPGatewayWithOptIn(t *testing.T) {
	t.Setenv("DOOPS_ALLOW_INSECURE_GATEWAY", "1")
	got, err := gatewayTargetsURL("http://203.0.113.10:42222")
	if err != nil {
		t.Fatalf("opt-in should allow insecure gateway targets URL: %v", err)
	}
	want := "http://203.0.113.10:42222/v1/targets"
	if got != want {
		t.Fatalf("targets URL mismatch\nwant: %s\n got: %s", want, got)
	}
}

func TestGatewayTargetsAllowsInsecureLocalhostForDevelopment(t *testing.T) {
	got, err := gatewayTargetsURL("http://localhost:42222")
	if err != nil {
		t.Fatalf("local insecure gateway should be allowed: %v", err)
	}
	want := "http://localhost:42222/v1/targets"
	if got != want {
		t.Fatalf("targets URL mismatch\nwant: %s\n got: %s", want, got)
	}
}

func TestGatewayTargetUnlockURLUsesVersionedEndpoint(t *testing.T) {
	got, err := gatewayTargetUnlockURL("https://gateway.example.com", "dev", "node-1")
	if err != nil {
		t.Fatalf("build unlock URL: %v", err)
	}
	want := "https://gateway.example.com/v1/targets/unlock?cluster=dev&instance=node-1"
	if got != want {
		t.Fatalf("unlock URL mismatch\nwant: %s\n got: %s", want, got)
	}
}
