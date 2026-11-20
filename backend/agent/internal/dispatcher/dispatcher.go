package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

// rule is one command shape the Agent refuses. Every rule names the supported
// alternative so the caller can switch paths instead of guessing why it failed.
type rule struct {
	name  string
	hint  string
	match *regexp.Regexp
}

var blockRules = []rule{
	{
		name:  "delete_root_filesystem",
		hint:  "deleting the root filesystem is never allowed; name the exact path to remove",
		match: regexp.MustCompile(`(?i)\brm\s+(-[a-z]*\s+)*-[a-z]*[rf][a-z]*\s+(-[a-z]*\s+)*/(\*)?(\s|;|&|\||$)`),
	},
}

// BlockedError reports which rule refused a command and what to do instead.
type BlockedError struct {
	Rule string
	Hint string
}

func (e *BlockedError) Error() string {
	return fmt.Sprintf("blocked_command(%s): next: %s", e.Rule, e.Hint)
}

const RulesEnv = "DOOPS_COMMAND_DENY_RULES"

var ruleName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
var plainArgument = regexp.MustCompile(`^[a-zA-Z0-9_./,:=@%+-]+$`)

func configuredRules() ([]rule, error) {
	raw, ok := os.LookupEnv(RulesEnv)
	if !ok {
		return blockRules, nil
	}
	var entries []struct{ Name, Match, Hint string }
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&entries); err != nil || entries == nil {
		return nil, fmt.Errorf("%s must be a JSON array of {name, match, hint}", RulesEnv)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return nil, fmt.Errorf("%s contains trailing data", RulesEnv)
	}
	rules := append([]rule(nil), blockRules...)
	names := make(map[string]bool, len(rules)+len(entries))
	for _, existing := range rules {
		names[existing.name] = true
	}
	for index, entry := range entries {
		if !ruleName.MatchString(entry.Name) || names[entry.Name] || strings.TrimSpace(entry.Match) == "" || strings.TrimSpace(entry.Hint) == "" {
			return nil, fmt.Errorf("%s[%d] requires a unique rule name, match and hint", RulesEnv, index)
		}
		pattern, err := regexp.Compile(entry.Match)
		if err != nil {
			return nil, fmt.Errorf("%s[%d]: invalid regex for %s: %w", RulesEnv, index, entry.Name, err)
		}
		names[entry.Name] = true
		rules = append(rules, rule{entry.Name, strings.TrimSpace(entry.Hint), pattern})
	}
	return rules, nil
}

func ValidateConfig() error {
	_, err := configuredRules()
	return err
}

func Check(command string) error {
	rules, err := configuredRules()
	if err != nil {
		return err
	}
	for _, r := range rules {
		if r.match.MatchString(strings.TrimSpace(command)) {
			return &BlockedError{Rule: r.name, Hint: r.hint}
		}
	}
	return nil
}

// ReportedDenial extracts a public diagnostic from an already-failed native
// tool call. Only return locally configured text, never arbitrary tool output.
func ReportedDenial(output string) *BlockedError {
	rules, err := configuredRules()
	if err != nil {
		return nil
	}
	for _, r := range rules {
		denied := &BlockedError{Rule: r.name, Hint: r.hint}
		if strings.Contains(output, denied.Error()) {
			return denied
		}
	}
	return nil
}

// ShellCommand checks the submitted text before either caller starts Bash.
// doagent uses a login shell, while the gateway exec contract uses bash -c.
func ShellCommand(command string, login bool) (*exec.Cmd, error) {
	if err := Check(command); err != nil {
		return nil, err
	}
	if login {
		return exec.Command("/bin/bash", "-lc", command), nil
	}
	return exec.Command("bash", "-c", command), nil
}

func CommandContext(ctx context.Context, name string, args ...string) (*exec.Cmd, error) {
	words := make([]string, 1, len(args)+1)
	words[0] = name
	for _, arg := range args {
		if !plainArgument.MatchString(arg) {
			arg = "'" + strings.ReplaceAll(arg, "'", "'\\''") + "'"
		}
		words = append(words, arg)
	}
	if err := Check(strings.Join(words, " ")); err != nil {
		return nil, err
	}
	return exec.CommandContext(ctx, name, args...), nil
}
