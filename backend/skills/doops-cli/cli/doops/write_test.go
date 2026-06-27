package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveWriteContentFromContentFlag(t *testing.T) {
	got, err := resolveWriteContent("hello", "", nil, os.Stdin)
	if err != nil {
		t.Fatalf("resolve content: %v", err)
	}
	if got != "hello" {
		t.Fatalf("content mismatch: %q", got)
	}
}

func TestResolveWriteContentFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pod.yaml")
	want := "apiVersion: v1\nkind: Pod\n"
	if err := os.WriteFile(path, []byte(want), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	got, err := resolveWriteContent("", path, nil, os.Stdin)
	if err != nil {
		t.Fatalf("resolve file: %v", err)
	}
	if got != want {
		t.Fatalf("content mismatch: %q", got)
	}
}

func TestResolveWriteContentRejectsAmbiguousSources(t *testing.T) {
	if _, err := resolveWriteContent("hello", "fixture.txt", nil, os.Stdin); err == nil {
		t.Fatal("expected ambiguous source error")
	}
}
