package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

var registrySyncTokenPattern = regexp.MustCompile(`^[A-Za-z0-9._:/-]+$`)

func runRegistryCommand(args []string, sessionName string, servers []Server, configErr error, stdout io.Writer) error {
	if len(args) > 0 && (args[0] == "-h" || args[0] == "-help" || args[0] == "--help" || args[0] == "help") {
		fmt.Fprintln(stdout, "用法: doops -session <s> registry sync --target <t> ...\n查看参数: doops registry sync -h")
		return nil
	}
	if len(args) == 0 || args[0] != "sync" {
		return fmt.Errorf("用法: doops registry sync --target <target> --source-registry <registry/path> --target-registry <registry/path> --tag <tag> [--source-authfile <remote-path>] [--target-authfile <remote-path>] --image <name> (可重复)")
	}
	registryFlags := flag.NewFlagSet("registry sync", flag.ContinueOnError)
	registryFlags.SetOutput(os.Stdout)
	var target, sourceRegistry, targetRegistry, tag, sourceAuthfile, targetAuthfile string
	var timeoutSeconds int
	var images stringListFlag
	registryFlags.StringVar(&target, "target", "", "Configured Gateway target")
	registryFlags.StringVar(&sourceRegistry, "source-registry", "", "Source registry/repository prefix")
	registryFlags.StringVar(&targetRegistry, "target-registry", "", "Target registry/repository prefix")
	registryFlags.StringVar(&tag, "tag", "", "Immutable release tag")
	registryFlags.StringVar(&sourceAuthfile, "source-authfile", "", "Remote source registry authfile path (default: the authfile the Gateway delivered to the target)")
	registryFlags.StringVar(&targetAuthfile, "target-authfile", "", "Remote target registry authfile path (default: the authfile the Gateway delivered to the target)")
	registryFlags.IntVar(&timeoutSeconds, "timeout", 600, "Timeout per copy/inspect operation in seconds")
	registryFlags.Var(&images, "image", "Image name without registry/tag (repeatable)")
	if err := registryFlags.Parse(args[1:]); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	// Help must work without a session or config, so these checks follow parsing.
	sessionName = resolveSession(sessionName)
	if configErr != nil {
		return configErr
	}
	if target == "" || sourceRegistry == "" || targetRegistry == "" || tag == "" || len(images) == 0 {
		return fmt.Errorf("registry sync requires --target, --source-registry, --target-registry, --tag and at least one --image")
	}
	if timeoutSeconds <= 0 {
		return fmt.Errorf("--timeout must be positive")
	}
	for name, value := range map[string]string{
		"--source-registry": sourceRegistry,
		"--target-registry": targetRegistry,
		"--tag":             tag,
	} {
		if !registrySyncTokenPattern.MatchString(value) {
			return fmt.Errorf("%s contains unsupported characters", name)
		}
	}
	for _, image := range images {
		if !registrySyncTokenPattern.MatchString(image) {
			return fmt.Errorf("--image contains unsupported characters: %s", image)
		}
	}

	server := findServer(servers, target)
	if server == nil {
		return errTargetNotFound(servers, target)
	}
	client := NewMCPClient(*server, NewSessionStore(), sessionName, false)
	client.Token = mustResolveToken(*server)
	defer client.Close()

	command := buildRegistrySyncCommand(sourceRegistry, targetRegistry, tag, sourceAuthfile, targetAuthfile, images, timeoutSeconds)
	if err := client.Call("doops_shell", map[string]interface{}{"command": command}); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "registry_sync=complete target=%s images=%d tag=%s\n", target, len(images), tag)
	return nil
}

func buildRegistrySyncCommand(sourceRegistry, targetRegistry, tag, sourceAuthfile, targetAuthfile string, images []string, timeoutSeconds int) string {
	parts := []string{"set -eu", "command -v timeout >/dev/null 2>&1", "command -v sha256sum >/dev/null 2>&1", "command -v skopeo >/dev/null 2>&1"}
	// Keep inspect as a standalone command so its failure propagates under POSIX sh.
	parts = append(parts, `manifest=$(mktemp)`, `trap 'rm -f -- "$manifest"' EXIT`)
	// An authfile path is optional. Without one, skopeo reads the target's own
	// registry authfile, which the Gateway delivers to the Agent for the
	// credentials granted on it.
	srcAuthArgs := []string{}
	if sourceAuthfile != "" {
		srcAuthArgs = append(srcAuthArgs, "--src-authfile", shellQuote(sourceAuthfile))
	}
	dstAuthArgs := []string{}
	if targetAuthfile != "" {
		dstAuthArgs = append(dstAuthArgs, "--dest-authfile", shellQuote(targetAuthfile))
	}
	srcInspectArgs := []string{}
	if sourceAuthfile != "" {
		srcInspectArgs = append(srcInspectArgs, "--authfile", shellQuote(sourceAuthfile))
	}
	dstInspectArgs := []string{}
	if targetAuthfile != "" {
		dstInspectArgs = append(dstInspectArgs, "--authfile", shellQuote(targetAuthfile))
	}
	for _, image := range images {
		sourceRef := sourceRegistry + "/" + image + ":" + tag
		targetRef := targetRegistry + "/" + image + ":" + tag
		copyArgs := append([]string{"skopeo", "copy", "--all", "--retry-times", "3"}, srcAuthArgs...)
		copyArgs = append(copyArgs, dstAuthArgs...)
		copyArgs = append(copyArgs, shellQuote("docker://"+sourceRef), shellQuote("docker://"+targetRef))
		parts = append(parts, fmt.Sprintf("timeout %ds %s", timeoutSeconds, strings.Join(copyArgs, " ")))
		srcInspect := append([]string{"skopeo", "inspect", "--raw"}, srcInspectArgs...)
		srcInspect = append(srcInspect, shellQuote("docker://"+sourceRef))
		dstInspect := append([]string{"skopeo", "inspect", "--raw"}, dstInspectArgs...)
		dstInspect = append(dstInspect, shellQuote("docker://"+targetRef))
		parts = append(parts,
			fmt.Sprintf(`timeout %ds %s > "$manifest"`, timeoutSeconds, strings.Join(srcInspect, " ")),
			`test -s "$manifest"`,
			`src_digest=$(sha256sum < "$manifest")`,
			fmt.Sprintf(`timeout %ds %s > "$manifest"`, timeoutSeconds, strings.Join(dstInspect, " ")),
			`test -s "$manifest"`,
			`dst_digest=$(sha256sum < "$manifest")`,
			fmt.Sprintf("test -n \"${src_digest}\" && test \"${src_digest}\" = \"${dst_digest}\" || { echo 'registry manifest digest mismatch for %s' >&2; exit 1; }", image),
		)
	}
	return strings.Join(parts, "\n")
}
