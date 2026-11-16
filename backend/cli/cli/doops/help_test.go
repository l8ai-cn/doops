package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// Help must print usage without a session or config so callers can discover flags.
func TestSubcommandHelpWorksWithoutSessionOrConfig(t *testing.T) {
	configErr := errors.New("no config")
	cases := []struct {
		name string
		run  func(*bytes.Buffer) error
		want string
	}{
		{"image", func(out *bytes.Buffer) error { return runImageCommand([]string{"-h"}, "", nil, configErr, out) }, "image build|copy"},
		{"registry", func(out *bytes.Buffer) error { return runRegistryCommand([]string{"-h"}, "", nil, configErr, out) }, "registry sync"},
	}
	for _, tc := range cases {
		var out bytes.Buffer
		if err := tc.run(&out); err != nil {
			t.Fatalf("%s -h returned error: %v", tc.name, err)
		}
		if !strings.Contains(out.String(), tc.want) {
			t.Fatalf("%s -h output missing %q: %s", tc.name, tc.want, out.String())
		}
	}
}

func TestBuildK8SRequestReportsHelp(t *testing.T) {
	for _, args := range [][]string{{"-h"}, {"get", "--help"}} {
		if _, err := buildK8SRequest(args); !errors.Is(err, errK8SHelp) {
			t.Fatalf("args %v: expected errK8SHelp, got %v", args, err)
		}
	}
	if strings.Contains(k8sUsage, "--confirm") {
		t.Fatal("k8s usage must not advertise the removed --confirm flag")
	}
}
