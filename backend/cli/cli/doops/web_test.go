package main

import (
	"strings"
	"testing"
)

func TestParseWebMode(t *testing.T) {
	for _, value := range []string{"check", "plan", "apply", " APPLY "} {
		if _, err := parseWebMode(value); err != nil {
			t.Fatalf("parseWebMode(%q): %v", value, err)
		}
	}
	if _, err := parseWebMode("deploy"); err == nil {
		t.Fatal("expected unsupported web mode to fail")
	}
}

func TestBuildWebInstructionKeepsGatewayBoundary(t *testing.T) {
	message := buildWebInstruction(webModeApply, "home", "/root/ws/release/dist", "example.test", "publish the approved site")
	for _, expected := range []string{
		"使用 DoWeb skill",
		"site_id=home",
		"dist=/root/ws/release/dist",
		"domain=example.test",
		"无需再次请求用户确认",
		"执行前核对 SiteSpec、dist、域名、代理、TLS 和验收条件",
		"nginx -t",
		"不要读取或索取密码",
		"用户需求：publish the approved site",
	} {
		if !strings.Contains(message, expected) {
			t.Fatalf("instruction missing %q: %s", expected, message)
		}
	}
}

func TestWebGatewayOperation(t *testing.T) {
	if got := webGatewayOperation(webModeCheck); got != "ask" {
		t.Fatalf("check operation = %q, want ask", got)
	}
	if got := webGatewayOperation(webModePlan); got != "ask" {
		t.Fatalf("plan operation = %q, want ask", got)
	}
	if got := webGatewayOperation(webModeApply); got != "apply" {
		t.Fatalf("apply operation = %q, want apply", got)
	}
}
