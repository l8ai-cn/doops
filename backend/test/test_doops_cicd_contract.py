import platform
import subprocess
from pathlib import Path

import yaml


ROOT = Path(__file__).parents[1]
SKILL = ROOT / "agent" / "skills" / "doops-cicd"
WORKFLOW = ROOT / "deploy" / "workflows" / "oilan-agent-bootstrap.yaml"
CLI_BIN = ROOT / "skills" / "doops-cli" / "bin"


def test_doops_cicd_skill_is_generic_and_agent_native():
    text = (SKILL / "SKILL.md").read_text()
    assert "doops.sh/v2" in text
    assert "DeploymentTemplate" in text
    assert "native doagent multi-agent engine" in text
    assert "$doops-cicd" in text
    assert "DeploymentReview" in text
    assert "exactly one `DeploymentTemplate` artifact" in text
    assert "imperative plan" in text
    assert "SemanticRelease" in text
    assert "ServiceRelease" in text
    assert "selected-services" in text
    assert "preserve-observed" in text
    assert "converge-shared-release" in text
    assert "pipeline" in text
    assert "runtime attestation" in text.lower()
    for forbidden in (
        "DeploymentPlan",
        "ReconciliationResult",
        "doops_plan",
        "reconcilehelmrelease",
    ):
        assert forbidden not in text


def test_existing_workflow_and_execution_contract_define_one_result():
    workflow = yaml.safe_load(WORKFLOW.read_text())
    contract = (SKILL / "references" / "execution-contract.md").read_text()

    assert workflow["apiVersion"] == "doops.sh/v2"
    assert workflow["kind"] == "DeploymentTemplate"
    assert workflow["spec"]["parameters"]["releaseId"]["required"] is True
    assert "${inputs.releaseId}" in str(workflow["spec"]["release"]["source"])
    assert workflow["spec"]["configurationSource"] == "backend/deploy/environments.yaml"
    assert "DeploymentRun" in contract
    assert "mutationCount" in contract
    assert "evidence" in contract
    assert "fallback" in contract.lower()
    assert "exactly one shared-release convergence" in contract
    assert "zero deployment executor" in contract
    assert "invocations" in contract
    assert "unselected service" in contract
    assert "toolCallId" in contract
    assert "Gateway" in contract


def test_system_prompt_routes_templates_to_doops_cicd():
    prompt = (ROOT / "agent" / "skills" / "system_prompt.md").read_text()
    assert "DeploymentTemplate" in prompt
    assert "SemanticRelease" in prompt
    assert "ServiceRelease" in prompt
    assert "doops-cicd" in prompt
    assert "DeploymentPlan" not in prompt
    assert "ReconciliationResult" not in prompt


def test_prebuilt_cli_exposes_only_agent_native_cicd_run():
    system = platform.system().lower()
    machine = {"x86_64": "amd64", "arm64": "arm64", "aarch64": "arm64"}[
        platform.machine().lower()
    ]
    binary = CLI_BIN / f"doops-{system}-{machine}"

    help_result = subprocess.run(
        [str(binary), "-help"],
        text=True,
        capture_output=True,
        check=False,
    )
    help_text = help_result.stdout + help_result.stderr
    assert help_result.returncode == 0
    assert "workflow 入口 (run)" in help_text
    assert "Agent 原生声明式 CI/CD 示例" in help_text
    assert "兼容适配" not in help_text
    assert "--allow-mutate" not in help_text
    assert "cicd lint" not in help_text
    assert "cicd plan" not in help_text

    retired_result = subprocess.run(
        [str(binary), "cicd", "plan"],
        text=True,
        capture_output=True,
        check=False,
    )
    retired_text = retired_result.stdout + retired_result.stderr
    assert retired_result.returncode != 0
    assert "only `cicd run` is supported" in retired_text
