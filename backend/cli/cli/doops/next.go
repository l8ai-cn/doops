package main

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// The CLI is driven by AI callers. Every failure names the command to run next,
// so the caller corrects itself from the output instead of from the docs.

// withNext appends the runnable next step to an error message.
func withNext(message string, next ...string) string {
	if len(next) == 0 {
		return message
	}
	return message + "\nnext: " + strings.Join(next, "\nnext: ")
}

// askCredentialNext turns a Helm credential failure from `ask --apply` into the
// commands that fix it. A target's bound Helm credential is used automatically,
// so these are the two cases automation cannot settle: no credential is bound
// for a repository that requires one, and several are bound.
func askCredentialNext(err error, target string) error {
	if err == nil {
		return nil
	}
	message := err.Error()
	configure := "doops credential configure --target " + target +
		" --name <helm-credential> --type helmRepository --scope personal --use helmPull" +
		" --namespace <ns>   # payload on stdin only; the URL is the repository origin"
	switch {
	case strings.Contains(message, "helm_chart_requires_credential"):
		return fmt.Errorf("%s", withNext(message,
			"doops credential list --target "+target+"   # a helmPull credential here is used automatically",
			configure,
			"rerun the same doops ask --target "+target+" --apply command   # no --credential needed"))
	case strings.Contains(message, "credential_reference_ambiguous"):
		return fmt.Errorf("%s", withNext(message,
			"doops credential list --target "+target+"   # several helmPull credentials are bound",
			"rerun with --credential <name> to pick the repository this Chart comes from"))
	case strings.Contains(message, "credential_version_not_found"):
		return fmt.Errorf("%s", withNext(message,
			"doops credential list --target "+target+"   # the credential is bound but never promoted",
			"doops credential configure --target "+target+" --id <id> --use helmPull --namespace <ns>"))
	case strings.Contains(message, "credential_grant_denied"),
		strings.Contains(message, "credential_forbidden"):
		// The named credential is not usable on this target. Whether it does
		// not exist, belongs to someone else, or is bound elsewhere, the
		// reply is the same, so it never reveals a name the caller may not see.
		return fmt.Errorf("%s", withNext(message,
			"doops credential list --target "+target+"   # the credentials bound to this target",
			"rerun without --credential   # a single bound Helm credential is used automatically"))
	}
	return err
}

// targetNotFound explains a missing target and lists the configured ones, with
// the closest name first.
func targetNotFound(servers []Server, name string) string {
	names := make([]string, 0, len(servers))
	for _, server := range servers {
		names = append(names, server.Name)
	}
	if len(names) == 0 {
		return withNext(fmt.Sprintf("Error: target %q is not configured and no targets exist yet", name),
			"doops login --gateway <gateway-url>   # discover visible online targets",
			"doops list   # use an actual configured route name")
	}
	if closest := closestWord(name, names); closest != "" {
		return withNext(fmt.Sprintf("Error: target %q is not configured; did you mean %q?", name, closest),
			fmt.Sprintf("rerun with --target %s", closest), "doops list")
	}
	sort.Strings(names)
	return withNext(fmt.Sprintf("Error: target %q is not configured; configured targets: %s", name, strings.Join(names, ", ")),
		"doops list")
}

// errTargetNotFound is targetNotFound as an error for callers that print "Error: %v".
func errTargetNotFound(servers []Server, name string) error {
	return errors.New(strings.TrimPrefix(targetNotFound(servers, name), "Error: "))
}

// closestWord returns the candidate within a small edit distance of word, or "".
func closestWord(word string, candidates []string) string {
	best, bestDistance := "", 3
	for _, candidate := range candidates {
		if distance := editDistance(strings.ToLower(word), strings.ToLower(candidate)); distance < bestDistance {
			best, bestDistance = candidate, distance
		}
	}
	return best
}

func editDistance(a, b string) int {
	previous := make([]int, len(b)+1)
	for j := range previous {
		previous[j] = j
	}
	for i := 1; i <= len(a); i++ {
		current := make([]int, len(b)+1)
		current[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			current[j] = min(previous[j]+1, current[j-1]+1, previous[j-1]+cost)
		}
		previous = current
	}
	return previous[len(b)]
}

var topLevelCommands = []string{
	"list", "targets", "capabilities", "exec", "ssh", "ask", "sql", "web", "image", "read", "write", "info",
	"k8s", "credential", "registry", "version", "session", "push", "pull", "clean", "admin", "upgrade",
	"add", "remove", "login", "logout", "check", "history", "guide",
}

// unknownCommand suggests the closest real command and the scenario guide.
func unknownCommand(command string) string {
	if closest := closestWord(command, topLevelCommands); closest != "" {
		return withNext(fmt.Sprintf("Error: unknown command %q; did you mean %q?", command, closest),
			"doops "+closest+" -h", "doops guide")
	}
	return withNext(fmt.Sprintf("Error: unknown command %q", command), "doops -h", "doops guide")
}
