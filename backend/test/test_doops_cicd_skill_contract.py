from pathlib import Path

import yaml


ROOT = Path(__file__).resolve().parents[1]
SKILL = ROOT / "agent" / "skills" / "doops-cicd" / "SKILL.md"
TEMPLATE_REFERENCE = (
    ROOT / "agent" / "skills" / "doops-cicd" / "references" / "deployment-template.md"
)
EXECUTION_REFERENCE = (
    ROOT / "agent" / "skills" / "doops-cicd" / "references" / "execution-contract.md"
)
EXISTING_WORKFLOW = ROOT / "deploy" / "workflows" / "oilan-agent-bootstrap.yaml"
SYSTEM_PROMPT = ROOT / "agent" / "skills" / "system_prompt.md"
LEGACY_SKILL = ROOT / "agent" / "skills" / "semantic-deployment" / "SKILL.md"


def test_doops_cicd_skill_is_generic_and_agent_native():
    text = SKILL.read_text(encoding="utf-8")
    lower = text.lower()

    assert "name: doops-cicd" in text
    assert "generate" in lower
    assert "execute" in lower
    assert "dry-run" in lower
    assert "apply" in lower
    assert "multi-agent" in lower
    assert "runtime" in lower
    assert "module" in lower
    assert "deploymenttemplate" in lower

    for forbidden in (
        "deploymentplan",
        "reconciliationresult",
        "doops_plan",
        "reconcilehelmrelease",
        "oilan",
    ):
        assert forbidden not in lower


def test_skill_references_define_compatible_yaml_and_execution_evidence():
    workflow = yaml.safe_load(EXISTING_WORKFLOW.read_text(encoding="utf-8"))
    template_contract = TEMPLATE_REFERENCE.read_text(encoding="utf-8")
    execution_contract = EXECUTION_REFERENCE.read_text(encoding="utf-8")

    assert workflow["apiVersion"] == "doops.sh/v2"
    assert workflow["kind"] == "DeploymentTemplate"
    assert "doops.sh/v2" in template_contract
    assert "DeploymentTemplate" in template_contract
    assert "${inputs." in template_contract
    assert "configurationSource" in template_contract
    assert "module" in execution_contract.lower()
    assert "evidence" in execution_contract.lower()
    assert "mutation" in execution_contract.lower()
    assert "fallback" in execution_contract.lower()


def test_system_prompt_routes_yaml_to_doops_cicd_only():
    text = SYSTEM_PROMPT.read_text(encoding="utf-8")

    assert "doops-cicd" in text
    assert "DeploymentTemplate" in text
    assert "DeploymentPlan" not in text
    assert "ReconciliationResult" not in text
    assert "semantic-deployment" not in text
    assert not LEGACY_SKILL.exists()
