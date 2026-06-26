package main

import "testing"

func TestFindServerMatchesNameAndAliases(t *testing.T) {
	servers := []Server{
		{Name: "jm", Aliases: []string{"jy", "oilan"}, Gateway: "https://gateway.example.com", Cluster: "doops-jm", Instance: "jm-228"},
		{Name: "ali", Aliases: []string{"aliyun"}, Gateway: "https://gateway.example.com", Cluster: "doops-ali", Instance: "master"},
	}

	if got := findServer(servers, "jm"); got == nil || got.Name != "jm" {
		t.Fatalf("expected canonical jm target, got %#v", got)
	}
	if got := findServer(servers, "jy"); got == nil || got.Name != "jm" {
		t.Fatalf("expected alias jy to resolve to jm, got %#v", got)
	}
	if got := findServer(servers, "oilan"); got == nil || got.Name != "jm" {
		t.Fatalf("expected alias oilan to resolve to jm, got %#v", got)
	}
	if got := findServer(servers, "aliyun"); got == nil || got.Name != "ali" {
		t.Fatalf("expected alias aliyun to resolve to ali, got %#v", got)
	}
}

func TestNormalizeAliasesSplitsCommasAndDeduplicates(t *testing.T) {
	got := normalizeAliases([]string{"jm, jy", "jm", " oilan "})
	want := []string{"jm", "jy", "oilan"}
	if len(got) != len(want) {
		t.Fatalf("alias count mismatch: %#v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("alias mismatch at %d: want %q got %q", i, want[i], got[i])
		}
	}
}

func TestAliasConflictDetection(t *testing.T) {
	servers := []Server{
		{Name: "jm", Aliases: []string{"jy", "oilan"}},
	}
	if got := findAliasConflict(servers, Server{Name: "ali", Aliases: []string{"jy"}}); got != "jm" {
		t.Fatalf("expected alias conflict with jm, got %q", got)
	}
	if got := findAliasConflict(servers, Server{Name: "jm", Aliases: []string{"new"}}); got != "" {
		t.Fatalf("updating canonical target should not conflict with itself, got %q", got)
	}
}
