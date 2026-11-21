package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// guideScenario is one task an operator asks for, written as the commands to run
// in order. `doops guide` prints these from the installed binary, so the chain
// always matches the commands this binary accepts.
type guideScenario struct {
	summary string
	steps   []string
	notes   []string
}

var guideScenarios = map[string]guideScenario{
	"onboard": {
		summary: "connect this machine to a Gateway target",
		steps: []string{
			"doops login --gateway <gateway-url> --username <user>  # enter the password at the hidden terminal prompt",
			"doops list                                              # choose an actual configured route name for <t>",
			"doops targets --target <t>",
			"doops exec --target <t> --cmd hostname            # online heartbeat is not enough; this proves the command channel",
		},
		notes: []string{
			`For automation, substitute the following for the login step: printf '%s' "$DOOPS_GATEWAY_TOKEN" | doops login --gateway <gateway-url> --token-stdin`,
			"Login is shared by routes on this Gateway. Login by URL adds visible online routes without renaming existing ones.",
			"To discover routes later with the saved login, use doops sync --target <t>. If no route exists yet, use doops login --gateway <gateway-url> again.",
		},
	},
	"diagnose": {
		summary: "find out what is wrong on a target",
		steps: []string{
			"doops targets --target <t>",
			"doops k8s --target <t> get pods -n <ns>",
			"doops k8s --target <t> events -n <ns>",
			"doops k8s --target <t> logs <pod> -n <ns> --previous --since 10m",
			"doops k8s --target <t> helm history <release> -n <ns>",
			"doops ask --target <t> --msg '<what to investigate>'   # let the edge agent reason over the cluster",
		},
	},
	"release": {
		summary: "install or upgrade a Helm release",
		steps: []string{
			"doops k8s --target <t> helm history <release> -n <ns>   # current revision = rollback target",
			"doops ask --target <t> --apply --msg 'release <release> in <ns>, chart <url or session path>'",
			"doops k8s --target <t> helm history <release> -n <ns>   # new revision is deployed",
		},
		notes: []string{
			"the Agent runs native Helm with --wait and verifies this call's deployed revision; Charts need no workload, Ingress or image",
			"Chart download credentials are selected by HTTPS origin; --credential <name> selects among bindings for the same origin",
			"exec runs helm/kubectl as given when you need a lower-level command",
		},
	},
	"credential": {
		summary: "store a secret once and bind it to a target",
		steps: []string{
			"printf '%s' '{\"data\":{\"token\":\"...\"}}' | doops credential configure --target <t> --name <n> --type <type> --scope personal --namespace <ns>",
			"doops credential configure --target <t> --name <n> --type opaque --scope personal --namespace <ns> --from-secret <secret> --keys <k1,k2>   # import an existing Secret",
			"doops credential list --target <t>",
		},
		notes: []string{"payloads go through stdin only; runtime use is --credential <name>"},
	},
	"database": {
		summary: "query or change a database bound to the target",
		steps: []string{
			"doops credential list --target <t>                 # a database credential must be bound; if none, do step 2",
			"doops credential configure --target <t> --name <n> --type opaque --scope personal --namespace <ns> --use database --from-secret <workload-secret> --keys <engine-specific-keys>   # choose the keys below for the discovered engine",
			"doops sql --target <t> --mode query --sql 'select ...'",
			"doops sql --target <t> --mode apply --file change.sql --verify 'select ...' --dry-run   # rollback-capable batches only; skip for MySQL/Oracle implicit-commit DDL or MongoDB non-transactional operations",
			"doops sql --target <t> --mode apply --file change.sql --verify 'select ...'   # within the user's authorized target, database and change scope",
			"doops sql --target <t> --mode query --sql 'select ...'   # read back the expected schema/data and compare with acceptance criteria",
		},
		notes: []string{
			"several databases on one target: add --database <name>",
			"MySQL jdbc:mysql source: --keys SPRING_DATASOURCE_URL,SPRING_DATASOURCE_USERNAME,SPRING_DATASOURCE_PASSWORD",
			"PostgreSQL, MongoDB, Oracle, SQL Server or MySQL URI source: --keys connection_uri; key names may be mapped with --key-map",
			"password-only Secret: use --connection-uri plus --password-key as described by doops credential configure --help; never print the password",
			"binding errors do not authorize switching databases; preserve the original --database when retrying",
			"natural-language --request supports query, plan and discover; apply uses --sql/--file (or the separate --copy-from table-copy operation)",
			"MySQL/Oracle implicit-commit DDL: execute separately from DML without --dry-run, check implicit_commits/partially_committed and read back schema; verification errors cannot undo committed DDL",
			"--verify returns query results; compare them with the task's expected values, including after commit",
			"reuse the user's explicit authorization; ask only when the target, database or change scope is unclear or exceeds it",
			"find <workload-secret> from the workload's secretKeyRef (doops k8s --target <t> get deployment <name> -n <ns> -o yaml); Secret values are never readable or printed",
			"the user's database need is the requirement: identify the database from the workload, then bind it with --from-secret instead of asking the user for a password",
		},
	},
	"upgrade": {
		summary: "upgrade the Agent that serves a target",
		steps: []string{
			"doops targets --target <t>                         # note RELEASE_COMMIT",
			"doops upgrade --target <t> --image <repo>@sha256:<digest> --dry-run",
			"doops upgrade --target <t> --image <repo>@sha256:<digest>",
			"doops targets --target <t>                         # RELEASE_COMMIT is the new commit",
		},
		notes: []string{"the in-cluster Job rolls back automatically when the new Agent does not come up; later: doops upgrade --target <t> --rollback"},
	},
	"user": {
		summary: "give someone access to one target",
		steps: []string{
			"doops admin user create --target <admin-t> --user <u> --generate-password --grant-target <t>",
			"doops admin grant list --target <admin-t> --user-id <id>",
		},
	},
	"files": {
		summary: "move code or files to and from a target",
		steps: []string{
			"doops push --target <t> --src <dir>                # lands in /root/ws/<session>",
			"doops exec --target <t> --cmd 'cd /root/ws/<session> && <cmd>'",
			"doops pull --target <t> --dest <dir> -session <session>",
		},
	},
}

func runGuide(args []string, w io.Writer) error {
	if len(args) == 0 {
		names := make([]string, 0, len(guideScenarios))
		for name := range guideScenarios {
			names = append(names, name)
		}
		sort.Strings(names)
		fmt.Fprintln(w, "doops guide <scenario> prints the commands for a task, in order.")
		fmt.Fprintln(w, "-session is optional everywhere; one is generated and printed when omitted.")
		fmt.Fprintln(w)
		for _, name := range names {
			fmt.Fprintf(w, "  %-11s %s\n", name, guideScenarios[name].summary)
		}
		return nil
	}
	name := strings.TrimSpace(args[0])
	scenario, ok := guideScenarios[name]
	if !ok {
		names := make([]string, 0, len(guideScenarios))
		for known := range guideScenarios {
			names = append(names, known)
		}
		if closest := closestWord(name, names); closest != "" {
			return fmt.Errorf("%s", withNext(fmt.Sprintf("no guide %q; did you mean %q?", name, closest), "doops guide "+closest))
		}
		return fmt.Errorf("%s", withNext(fmt.Sprintf("no guide %q", name), "doops guide"))
	}
	fmt.Fprintf(w, "%s: %s\n\n", name, scenario.summary)
	for index, step := range scenario.steps {
		fmt.Fprintf(w, "%d. %s\n", index+1, step)
	}
	for _, note := range scenario.notes {
		fmt.Fprintf(w, "\n- %s", note)
	}
	if len(scenario.notes) != 0 {
		fmt.Fprintln(w)
	}
	return nil
}
