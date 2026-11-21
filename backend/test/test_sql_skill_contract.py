"""Execute operator examples against the CLI, Gateway and model tool contracts."""

import itertools
import json
import os
from pathlib import Path
import re
import shlex
import subprocess

import pytest


ROOT = Path(__file__).resolve().parents[2]
DOCS = (
    "backend/skills/SKILL.md",
    "backend/docs/DOOPS_USAGE_GUIDE.md",
    "backend/docs/DOOPS_API_GUIDE.md",
    "backend/docs/UNIFIED_OPERATIONS_WORKBENCH.md",
    "README.md",
    "backend/README.md",
)


def command_variants(command):
    """Expand the command tables' optional flags and alternative arguments."""
    command = command.replace(r"\|", "|")
    optional = re.search(r"\[(--[^\[\]]+)\]", command)
    if optional:
        for value in ("", optional[1]):
            yield from command_variants(
                command[: optional.start()] + value + command[optional.end() :]
            )
        return
    alternative = re.search(r"\((--[^()]+)\)", command)
    if alternative:
        for value in alternative[1].split("|"):
            yield from command_variants(
                command[: alternative.start()] + value + command[alternative.end() :]
            )
        return
    words = shlex.split(command, comments=True)
    yield from itertools.product(*(word.split("|") for word in words))


def documented_commands():
    for path in DOCS:
        text = (ROOT / path).read_text()
        commands = re.findall(r"`(doops[^`\n]+)`", text)
        for block in re.findall(r"```bash\n(.*?)```", text, re.S):
            commands.extend(block.replace("\\\n", " ").splitlines())
        for command in commands:
            if re.match(r"doops\b", command) and re.search(r"\bsql --target\b", command):
                for words in command_variants(command):
                    yield path, words


@pytest.fixture(scope="module")
def cli(tmp_path_factory):
    directory = tmp_path_factory.mktemp("sql-cli-contract")
    binary = directory / "doops"
    subprocess.run(
        ["go", "build", "-o", str(binary), "./cli/doops"],
        cwd=ROOT / "backend/cli", check=True, capture_output=True, text=True, timeout=120,
    )
    config = directory / "config.json"
    config.write_text('{"servers":[]}')
    sql_file = directory / "change.sql"
    sql_file.write_text("UPDATE orders SET status = 'paid' WHERE id = 1")
    return binary, sql_file, config


def assert_cli_accepts(cli, words):
    binary, sql_file, config = cli
    args = [re.sub(r"<[^>]+>", "contract", word.replace("<n>", "2")) for word in words[1:]]
    for flag in ("--file", "--verify-file"):
        if flag in args:
            args[args.index(flag) + 1] = str(sql_file)
    result = subprocess.run(
        [str(binary), *args], env={**os.environ, "DOOPS_CONFIG": str(config)},
        capture_output=True, text=True, timeout=15,
    )
    # An empty isolated config prevents any network/database access. Getting as
    # far as target resolution proves the real CLI accepted the whole combination.
    assert result.returncode != 0
    assert 'target "contract" is not configured' in result.stdout, (
        shlex.join(args), result.stdout, result.stderr
    )


@pytest.mark.parametrize("path,words", list(documented_commands()))
def test_documented_sql_combinations_pass_cli_validation(cli, path, words):
    assert_cli_accepts(cli, words)


def operator_examples():
    text = (ROOT / "backend/skills/SKILL.md").read_text()
    for example in re.findall(r"`(doops[^`\n]+)`", text):
        if re.search(r"\b(?:ssh|k8s|credential configure) --target\b", example):
            yield from command_variants(example.replace("<user@host>", "review@example.test"))


@pytest.mark.parametrize("words", list(operator_examples()))
def test_operator_examples_pass_cli_validation(cli, words):
    assert_cli_accepts(cli, words)


def test_documentation_command_coverage():
    assert {path for path, _ in documented_commands()} == {
        "backend/skills/SKILL.md", "backend/docs/DOOPS_USAGE_GUIDE.md",
        "README.md", "backend/README.md",
    }


def test_database_guide_and_apply_next_commands(cli):
    binary, _, config = cli
    guide = subprocess.run(
        [str(binary), "guide", "database"], env={**os.environ, "DOOPS_CONFIG": str(config)},
        check=True, capture_output=True, text=True, timeout=15,
    ).stdout
    rejected = subprocess.run(
        [str(binary), "-session", "contract", "sql", "--target", "contract",
         "--mode", "apply", "--request", "add a column"],
        env={**os.environ, "DOOPS_CONFIG": str(config)},
        capture_output=True, text=True, timeout=15,
    )
    assert rejected.returncode != 0
    sources = (guide, rejected.stdout)
    for source in sources:
        commands = re.findall(r"(?m)^(?:\d+\. |next: )(doops .+)$", source)
        sql_commands = [c for c in commands if re.search(r"\bsql --target\b", c)]
        assert sql_commands
        assert "MySQL/Oracle" in source and "DDL" in source
        for command in sql_commands:
            for words in command_variants(command):
                if "--dry-run" in words:
                    assert "rollback-capable" in command and "skip" in command
                assert_cli_accepts(cli, words)


# Overlay adds a test to the production package only for this subprocess. No
# Agent source or tests are written into the shared worktree.
GATEWAY_CONTRACT_TEST = r'''
package server

import (
    "encoding/json"
    "os"
    "reflect"
    "strings"
    "testing"
    "time"

    "github.com/user/doops/agent/api"
)

func TestSQLDocumentationContract(t *testing.T) {
    raw, err := os.ReadFile(os.Getenv("DOOPS_SQL_DOC_CASES"))
    if err != nil { t.Fatal(err) }
    var cases struct {
        Tools []string
        Requests []struct {
            Arguments map[string]interface{}
            WantError string
        }
    }
    if err := json.Unmarshal(raw, &cases); err != nil { t.Fatal(err) }
    t.Run("documented MCP tools are registered and model-visible", func(t *testing.T) {
        tools, err := mcpToolsForDomain(mcpDomainDoSQL)
        if err != nil { t.Fatal(err) }
        var names []string
        for _, tool := range tools {
            name := tool.(map[string]interface{})["name"].(string)
            names = append(names, name)
            for _, apply := range []bool{false, true} {
                if !sliceContainsString(doagentAllowedTools(apply, "dosql"), "mcp_dosql_"+name) {
                    t.Fatalf("registered tool %q unavailable to DoSql", name)
                }
            }
        }
        if !reflect.DeepEqual(names, cases.Tools) {
            t.Fatalf("registered tools %v differ from documentation %v", names, cases.Tools)
        }
    })
    for _, test := range cases.Requests {
        t.Run(test.Arguments["session_id"].(string)+test.WantError, func(t *testing.T) {
            t.Setenv("DOOPS_GATEWAY_SECRET_KEY", strings.Repeat("d", 32))
            f := newExecutionLifecycleFixture(t, GatewayHubOptions{OperationTimeout: 3*time.Second})
            scope := CredentialGrantScope{Cluster:"dev", Instance:"local", Namespace:"apps", Use:CredentialUseDatabase}
            app, _ := namedDatabaseCredential(t, f, f.admin, "orders-app", scope)
            migrate, _ := namedDatabaseCredential(t, f, f.admin, "orders-migrate", scope)
            user := f.user(t, "doc-operator", ScopeGrant{Cluster:"dev", Instance:"local"})
            agent, client := newCredentialTargetPermissionAgent(t, f), f.client(t, user)
            args := test.Arguments
            session := args["session_id"].(string)
            lifecycleSend(t, client, 2, "doops_agent_prompt", session, args)
            if test.WantError != "" {
                response := lifecycleResponse(t, client, 2, test.WantError)
                if response["error"].(map[string]interface{})["code"] != float64(-32602) {
                    t.Fatalf("expected parameter rejection: %#v", response)
                }
                agent.noCalls(t)
                return
            }
            expected := map[string]bool{app.ID:true, migrate.ID:true}
            if args["sql_credential"] != nil { delete(expected, app.ID) }
            for range len(expected) {
                call := agent.call(t, client, "doops_credential_materialize", session)
                var request CredentialMaterializeRequest
                if err := json.Unmarshal(call.arguments, &request); err != nil { t.Fatal(err) }
                if !expected[request.CredentialID] { t.Fatalf("unexpected materialization %s", request.CredentialID) }
                metadata, err := json.Marshal(CredentialMaterialization{
                    CredentialID:request.CredentialID, GrantID:request.GrantID, VersionID:request.VersionID,
                    Namespace:"apps", ResourceName:credentialSecretName(request.CredentialID, request.GrantID),
                    SecretType:"Opaque", Keys:[]string{"connection_uri"}, ResourceVersion:"7", Status:"verified",
                })
                if err != nil { t.Fatal(err) }
                tokenLifecycleReplyText(t, agent.executionLifecycleAgent, call.executionLifecycleCall, string(metadata), false)
            }
            final := agent.call(t, client, "doops_agent_prompt", session)
            var prompt api.AgentPromptParams
            if err := json.Unmarshal(final.arguments, &prompt); err != nil { t.Fatal(err) }
            if len(prompt.CredentialBindings) != len(expected) { t.Fatalf("wrong bindings: %#v", prompt.CredentialBindings) }
            for _, binding := range prompt.CredentialBindings {
                if !expected[binding.CredentialID] { t.Fatalf("unexpected binding %s", binding.CredentialID) }
                delete(expected, binding.CredentialID)
            }
            if strings.Contains(string(final.arguments), "synthetic-target-permission-payload") {
                t.Fatal("credential payload leaked into public prompt")
            }
            if prompt.SQL != "" {
                request, err := planDirectDoSQL("sqlite", prompt)
                if err != nil { t.Fatal(err) }
                request.Target = newDoSQLDirectTestTarget(t)
                result, err := executeDoSQLDirect(t.Context(), request)
                if err != nil || !result.Committed || result.Verification.Rows[0][0] != "paid" {
                    t.Fatalf("documented SQL did not execute and verify: %#v %v", result, err)
                }
                if countDoSQLRows(t, request.Target, "id = 1 AND status = 'paid'") != "1" {
                    t.Fatal("documented SQL did not commit")
                }
            }
            agent.complete(t, final.executionLifecycleCall, "")
            lifecycleResponse(t, client, 2, "")
            agent.noCalls(t)
        })
    }
    t.Run("implicit commit DDL is rejected before connecting", func(t *testing.T) {
        for _, engine := range []string{"mysql", "oracle"} {
            for _, dryRun := range []bool{false, true} {
                statements := []string{"ALTER TABLE orders ADD external_id VARCHAR(64)"}
                want := "dosql_dry_run_ddl_unsupported"
                if !dryRun {
                    statements = append(statements, "UPDATE orders SET status = 'paid' WHERE id = 1")
                    want = "dosql_mixed_ddl_dml_unsupported"
                }
                result, err := executeDoSQLDirect(t.Context(), doSQLDirectRequest{
                    Target:doSQLTarget{Engine:engine, Driver:"must-not-connect"},
                    Statements:statements, DryRun:dryRun,
                })
                if err == nil || !strings.Contains(err.Error(), want) || result.Status != "rejected" || result.Committed {
                    t.Fatalf("%s dry-run=%v: %#v %v", engine, dryRun, result, err)
                }
            }
        }
    })
}
'''


def test_api_examples_and_mcp_claims_against_gateway_and_executor(tmp_path):
    api_doc = (ROOT / "backend/docs/DOOPS_API_GUIDE.md").read_text()
    examples = [
        value for block in re.findall(r"```json\n(.*?)```", api_doc, re.S)
        if isinstance(value := json.loads(block), dict) and value.get("skill") == "dosql"
    ]
    assert {example["operation"] for example in examples} == {"ask", "apply"}
    requests = [{"Arguments": example, "WantError": ""} for example in examples]
    apply = next(example for example in examples if example["operation"] == "apply")
    for changes, error in (
        ({"credential_refs": ["orders-migrate"]}, "dosql_credential_refs_unsupported"),
        ({"credential_mode": ""}, "sql_credential requires skill dosql"),
        ({"skill": "doweb"}, "automatic credential mode is only supported by dosql"),
    ):
        requests.append({"Arguments": {**apply, **changes}, "WantError": error})
    tools = set()
    for path in DOCS:
        for claim in re.findall(r"mcp_dosql_([a-z_]+(?:/[a-z_]+)*)", (ROOT / path).read_text()):
            tools.update(claim.split("/"))
    assert tools
    cases = tmp_path / "cases.json"
    cases.write_text(json.dumps({"Tools": sorted(tools), "Requests": requests}))
    source = tmp_path / "sql_documentation_contract_test.go"
    source.write_text(GATEWAY_CONTRACT_TEST)
    overlay = tmp_path / "overlay.json"
    target = ROOT / "backend/agent/internal/server/sql_documentation_contract_test.go"
    overlay.write_text(json.dumps({"Replace": {str(target): str(source)}}))
    result = subprocess.run(
        ["go", "test", "-overlay", str(overlay), "./internal/server",
         "-run", "^TestSQLDocumentationContract$", "-count=1", "-timeout=90s", "-v"],
        cwd=ROOT / "backend/agent", env={**os.environ, "DOOPS_SQL_DOC_CASES": str(cases)},
        capture_output=True, text=True, timeout=180,
    )
    assert result.returncode == 0, result.stdout + result.stderr
    assert "--- PASS: TestSQLDocumentationContract" in result.stdout


def test_bundled_dosql_cli_and_audit_filesystem_contracts():
    tests = sorted((ROOT / "backend/agent/skills/dosql").rglob("*.test.mjs"))
    assert tests
    result = subprocess.run(
        ["node", "--test", *map(str, tests)],
        cwd=ROOT, capture_output=True, text=True, timeout=90,
    )
    assert result.returncode == 0, result.stdout + result.stderr
