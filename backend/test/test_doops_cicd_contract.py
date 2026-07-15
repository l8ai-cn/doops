from pathlib import Path

import yaml


ROOT = Path(__file__).parents[1]
SKILL = ROOT / "agent" / "skills" / "doops-cicd"
WORKFLOW = ROOT / "deploy" / "workflows" / "oilan-agent-bootstrap.yaml"


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
