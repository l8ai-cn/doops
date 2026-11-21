package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const testPinnedDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const testUpgradeCommit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const testBaselineCommit = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func testPinnedImage() string {
	return "registry.example/doops@" + "sha256:" + testPinnedDigest
}

func TestParsePinnedImage(t *testing.T) {
	repository, digest, err := parsePinnedImage("registry.example.com/doops-agent@sha256:" + testPinnedDigest)
	if err != nil {
		t.Fatal(err)
	}
	if repository != "registry.example.com/doops-agent" || digest != "sha256:"+testPinnedDigest {
		t.Fatalf("parsed repository=%s digest=%s", repository, digest)
	}
	for _, image := range []string{
		"registry.example/doops:latest",
		"registry.example/doops@sha256:abc",
		"Registry.Example/doops@sha256:" + testPinnedDigest,
		"registry.example/doops@sha256:" + strings.Repeat("A", 64),
	} {
		if _, _, err := parsePinnedImage(image); err == nil {
			t.Fatalf("accepted unpinned image %s", image)
		}
	}
}

func TestSelfUpgradeScriptStaysOutsideTheAgentProcess(t *testing.T) {
	if strings.Contains(selfUpgradeApplyScript, "set image") || strings.Contains(selfUpgradeApplyScript, "upgrade --install") {
		t.Fatal("apply script must helm-upgrade the existing release without kubectl set image")
	}
	for _, part := range []string{
		"helm upgrade",
		"helm rollback",
		selfUpgradeChartPath,
		"/app/configure_kubeconfig.py",
		"selectorLabels",
		"observedGeneration",
		"availableReplicas",
		"unavailableReplicas",
		"--dry-run=server",
	} {
		if !strings.Contains(selfUpgradeApplyScript, part) {
			t.Fatalf("apply script missing %s", part)
		}
	}
	if strings.Contains(selfUpgradeApplyScript, "jsonpath=") {
		t.Fatal("apply script must verify one complete Deployment JSON")
	}
	if strings.Contains(selfUpgradeRollbackScript, `image["digest"]`) {
		t.Fatal("rollback script must not publish a new image")
	}
	if !strings.Contains(selfUpgradeRollbackScript, "helm rollback") {
		t.Fatal("rollback script missing helm rollback")
	}
	if !strings.Contains(selfUpgradeRollbackScript, `row["status"] == "superseded"`) ||
		!strings.Contains(selfUpgradeRollbackScript, "DOOPS_UPGRADE_REVISION") {
		t.Fatal("rollback script must recheck the admitted superseded revision")
	}
}

func TestSelfUpgradeJobManifestPreservesReleaseTag(t *testing.T) {
	pod := selfUpgradePod{
		image:              "registry.example/doops:old",
		nodeName:           "node-a",
		serviceAccountName: "default",
		mount:              jobVolumeMount{Name: "kube", MountPath: "/root/.kube", ReadOnly: true},
		volume:             json.RawMessage(`{"name":"kube","secret":{"secretName":"cluster-admin"}}`),
	}
	manifest, err := selfUpgradeJobManifest("doops-self-upgrade-aaaaaaaa", "kz-ops", pod, selfUpgradePlan{
		image:      "registry.example/doops:v1.3.40",
		repository: "registry.example/doops",
		tag:        "v1.3.40",
		release:    "doops-agent-live",
		workload:   "doops-agent",
		script:     selfUpgradeApplyScript,
	})
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Image           string              `json:"image"`
						ImagePullPolicy string              `json:"imagePullPolicy"`
						Env             []map[string]string `json:"env"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(manifest, &decoded); err != nil {
		t.Fatal(err)
	}
	container := decoded.Spec.Template.Spec.Containers[0]
	if container.Image != "registry.example/doops:v1.3.40" || container.ImagePullPolicy != "Always" {
		t.Fatalf("tagged upgrade container = %#v", container)
	}
	for _, env := range container.Env {
		if env["name"] == "DOOPS_UPGRADE_TAG" && env["value"] == "v1.3.40" {
			return
		}
	}
	t.Fatal("tagged upgrade Job did not preserve DOOPS_UPGRADE_TAG")
}

func TestSelfUpgradeApplyScriptVerifiesCompleteDeploymentTwice(t *testing.T) {
	callLog := filepath.Join(t.TempDir(), "calls.log")
	env := selfUpgradeScriptEnv(callLog)
	out, err := runSelfUpgradeScript(t, selfUpgradeApplyScript, selfUpgradeFakeCommands(), env)
	if err != nil {
		t.Fatalf("self-upgrade script failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "self-upgrade complete image="+testPinnedImage()) {
		t.Fatalf("success evidence = %s", out)
	}
	logBytes, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatal(err)
	}
	log := string(logBytes)
	lines := strings.Split(strings.TrimSpace(log), "\n")
	dryRunLine := -1
	mutationLine := -1
	deploymentGets := 0
	for index, line := range lines {
		if strings.Contains(line, "get deployment") {
			deploymentGets++
		}
		if strings.Contains(line, "helm upgrade") && strings.Contains(line, "--dry-run=server") {
			dryRunLine = index
		}
		if strings.Contains(line, "helm upgrade") && !strings.Contains(line, "--dry-run=server") {
			mutationLine = index
		}
	}
	if dryRunLine < 0 || mutationLine < 0 || dryRunLine >= mutationLine {
		t.Fatalf("expected server-side dry-run before mutation, calls:\n%s", log)
	}
	if deploymentGets != 5 {
		t.Fatalf("expected strategy, selector, baseline, and two complete verification reads, got %d:\n%s", deploymentGets, log)
	}
	if strings.Contains(log, "jsonpath=") {
		t.Fatalf("script used partial jsonpath evidence:\n%s", log)
	}
}

func TestSelfUpgradeApplyAllowsRollingUpdateSurge(t *testing.T) {
	env := selfUpgradeScriptEnv(filepath.Join(t.TempDir(), "calls.log"))
	env["SURGE_REPLICAS"] = "1"
	out, err := runSelfUpgradeScript(t, selfUpgradeApplyScript, selfUpgradeFakeCommands(), env)
	if err != nil {
		t.Fatalf("rolling update surge was rejected: %v\n%s", err, out)
	}
	if !strings.Contains(out, "self-upgrade complete image="+testPinnedImage()) {
		t.Fatalf("rolling update surge lost success evidence: %s", out)
	}
}

func TestSelfUpgradeApplyScriptRejectsIncompleteDeploymentEvidence(t *testing.T) {
	for _, field := range []string{
		"observedGeneration",
		"replicas",
		"desiredReplicas",
		"statusReplicas",
		"updatedReplicas",
		"readyReplicas",
		"availableReplicas",
		"unavailableReplicas",
		"image",
	} {
		t.Run(field, func(t *testing.T) {
			callLog := filepath.Join(t.TempDir(), "calls.log")
			env := selfUpgradeScriptEnv(callLog)
			env["BAD_FIELD"] = field
			env["DATE_CALLS"] = filepath.Join(t.TempDir(), "date-calls")
			out, err := runSelfUpgradeScript(t, selfUpgradeApplyScript, selfUpgradeFakeCommands(), env)
			if err == nil {
				t.Fatalf("incomplete %s evidence was accepted:\n%s", field, out)
			}
			if !strings.Contains(out, "rolling back doops-agent-live to 5") ||
				!strings.Contains(out, "automatic rollback complete revision=5") {
				t.Fatalf("failed evidence did not complete a visible rollback:\n%s", out)
			}
			logBytes, readErr := os.ReadFile(callLog)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !strings.Contains(string(logBytes), "helm rollback doops-agent-live 5") {
				t.Fatalf("rollback revision was not the previous deployed revision:\n%s", logBytes)
			}
		})
	}
}

func TestSelfUpgradeVerifiesRuntimeIdentityFromPinnedController(t *testing.T) {
	env := selfUpgradeScriptEnv(filepath.Join(t.TempDir(), "calls.log"))
	env["RUNTIME_DIGEST"] = "sha256:" + strings.Repeat("c", 64)
	out, err := runSelfUpgradeScript(t, selfUpgradeApplyScript, selfUpgradeFakeCommands(), env)
	if err != nil || !strings.Contains(out, "self-upgrade complete image="+testPinnedImage()) {
		t.Fatalf("a shared runtime imageID must be proven by the independently pinned controller: %v\n%s", err, out)
	}
	if !strings.Contains(out, env["RUNTIME_DIGEST"]) {
		t.Fatalf("runtime imageID evidence is missing: %s", out)
	}
}

func TestSelfUpgradeRejectsUnverifiedControllerIdentity(t *testing.T) {
	for _, field := range []string{"image", "imageID", "node", "ready"} {
		t.Run(field, func(t *testing.T) {
			env := selfUpgradeScriptEnv(filepath.Join(t.TempDir(), "calls.log"))
			env["BAD_CONTROLLER_FIELD"] = field
			out, err := runSelfUpgradeScript(t, selfUpgradeApplyScript, selfUpgradeFakeCommands(), env)
			if err == nil || strings.Contains(out, "self-upgrade complete") {
				t.Fatalf("invalid controller %s was accepted: %v\n%s", field, err, out)
			}
			if !strings.Contains(out, "automatic rollback complete") {
				t.Fatalf("invalid controller must retain verified rollback: %s", out)
			}
		})
	}
}

// The controller Job runs on the old Agent's node. A digest-pinned candidate
// that the scheduler puts on another node is checked against the pinned digest
// (or the controller's runtime digest); a tag can only be proven on one node.
func TestSelfUpgradeVerifiesCandidateNodeAndDigest(t *testing.T) {
	pinned := "sha256:" + testPinnedDigest
	controllerRuntime := "sha256:" + strings.Repeat("c", 64)
	other := "sha256:" + strings.Repeat("e", 64)
	oldPinned := "sha256:" + strings.Repeat("b", 64)
	for _, tc := range []struct {
		name string
		env  map[string]string
		// want is the outcome: "complete", "rolled back", or "rollback failed".
		want   string
		output []string
	}{
		{"same node, pinned digest", nil, "complete", []string{"node=node-a"}},
		{"other node, pinned digest", map[string]string{"AGENT_NODE": "node-b"}, "complete", []string{"node=node-b"}},
		{"other node, controller runtime digest",
			map[string]string{"AGENT_NODE": "node-b", "RUNTIME_DIGEST": controllerRuntime}, "complete", []string{"node=node-b", controllerRuntime}},
		{"other node reports the pinned digest while the controller runtime differs",
			map[string]string{"AGENT_NODE": "node-b", "RUNTIME_DIGEST": controllerRuntime, "CANDIDATE_IMAGE_ID": "registry.example/doops@" + pinned},
			"complete", []string{"node=node-b", pinned}},
		{"other node, different content",
			map[string]string{"AGENT_NODE": "node-b", "CANDIDATE_IMAGE_ID": "registry.example/doops@" + other},
			"rolled back", []string{"matches neither the pinned image nor the controller runtime"}},
		{"same node must match the controller runtime exactly",
			map[string]string{"RUNTIME_DIGEST": controllerRuntime, "CANDIDATE_IMAGE_ID": "registry.example/doops@" + pinned},
			"rolled back", []string{"does not match the expected digest"}},
		{"same node, tag", map[string]string{
			"DOOPS_UPGRADE_IMAGE": "registry.example/doops:v2", "DOOPS_UPGRADE_DIGEST": "", "DOOPS_UPGRADE_TAG": "v2",
			"RUNTIME_DIGEST": controllerRuntime, "OLD_RUNTIME_DIGEST": oldPinned}, "complete", []string{"node=node-a"}},
		{"other node, tag", map[string]string{
			"DOOPS_UPGRADE_IMAGE": "registry.example/doops:v2", "DOOPS_UPGRADE_DIGEST": "", "DOOPS_UPGRADE_TAG": "v2",
			"RUNTIME_DIGEST": controllerRuntime, "OLD_RUNTIME_DIGEST": oldPinned, "AGENT_NODE": "node-b"}, "rolled back", []string{"same node runtime"}},
		{"rollback restored on another node, pinned baseline", map[string]string{
			"BAD_FIELD": "readyReplicas", "RESTORED_NODE": "node-b",
			"OLD_IMAGE": "registry.example/doops@" + oldPinned, "OLD_RUNTIME_DIGEST": oldPinned}, "rolled back", []string{"node=node-b"}},
		{"rollback restored on another node, tag baseline", map[string]string{
			"BAD_FIELD": "readyReplicas", "RESTORED_NODE": "node-b"}, "rollback failed", []string{"same node runtime"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := selfUpgradeScriptEnv(filepath.Join(t.TempDir(), "calls.log"))
			for key, value := range tc.env {
				env[key] = value
			}
			out, err := runSelfUpgradeScript(t, selfUpgradeApplyScript, selfUpgradeFakeCommands(), env)
			switch tc.want {
			case "complete":
				if err != nil || !strings.Contains(out, "self-upgrade complete image="+env["DOOPS_UPGRADE_IMAGE"]) || strings.Contains(out, "rolling back") {
					t.Fatalf("upgrade was not accepted: %v\n%s", err, out)
				}
			case "rolled back":
				if err == nil || strings.Contains(out, "self-upgrade complete") || !strings.Contains(out, "automatic rollback complete revision=5") {
					t.Fatalf("candidate was not rejected and restored: %v\n%s", err, out)
				}
			case "rollback failed":
				if err == nil || strings.Contains(out, "self-upgrade complete") || !strings.Contains(out, "automatic rollback failed") {
					t.Fatalf("unverifiable restore was accepted: %v\n%s", err, out)
				}
			}
			for _, want := range tc.output {
				if !strings.Contains(out, want) {
					t.Fatalf("output lacks %q:\n%s", want, out)
				}
			}
		})
	}
}

func TestSelfUpgradeAutomaticRollbackUsesCapturedRuntimeIdentity(t *testing.T) {
	env := selfUpgradeScriptEnv(filepath.Join(t.TempDir(), "calls.log"))
	env["OLD_IMAGE"] = "registry.example/doops@sha256:" + strings.Repeat("b", 64)
	env["OLD_RUNTIME_DIGEST"] = "sha256:" + strings.Repeat("d", 64)
	env["RUNTIME_DIGEST"] = "sha256:" + strings.Repeat("c", 64)
	env["BAD_FIELD"] = "readyReplicas"
	out, err := runSelfUpgradeScript(t, selfUpgradeApplyScript, selfUpgradeFakeCommands(), env)
	if err == nil || !strings.Contains(out, "automatic rollback complete revision=5") {
		t.Fatalf("failed update must restore the captured artifact identity: %v\n%s", err, out)
	}
	if !strings.Contains(out, env["OLD_RUNTIME_DIGEST"]) {
		t.Fatalf("captured rollback runtime identity was not verified: %s", out)
	}
}

func TestSelfUpgradeManualRollbackUsesHistoricalControllerIdentity(t *testing.T) {
	env := selfUpgradeScriptEnv(filepath.Join(t.TempDir(), "calls.log"))
	env["OLD_IMAGE"] = "registry.example/doops@sha256:" + strings.Repeat("b", 64)
	env["DOOPS_UPGRADE_IMAGE"] = env["OLD_IMAGE"]
	env["OLD_RUNTIME_DIGEST"] = "sha256:" + strings.Repeat("c", 64)
	env["RUNTIME_DIGEST"] = env["OLD_RUNTIME_DIGEST"]
	out, err := runSelfUpgradeScript(t, selfUpgradeRollbackScript, selfUpgradeFakeCommands(), env)
	if err != nil || !strings.Contains(out, "self-upgrade rolled back to 4") {
		t.Fatalf("historical manifest/index identity must remain verifiable: %v\n%s", err, out)
	}
}

func TestSelfUpgradeRollbackImageIsPinnedToSuccessfulHistory(t *testing.T) {
	wanted := "registry.example/doops@sha256:" + strings.Repeat("b", 64)
	manifest := `kind: Deployment
metadata: {name: doops-agent, namespace: kz-ops}
spec:
  template:
    spec:
      containers:
        - {name: doops-agent, image: "` + wanted + `"}
`
	for _, tc := range []struct {
		name     string
		manifest string
		wantErr  bool
	}{
		{"pinned", manifest, false},
		{"mutable", strings.ReplaceAll(manifest, wanted, "registry.example/doops:old"), true},
		{"wrong namespace", strings.ReplaceAll(manifest, "namespace: kz-ops", "namespace: other"), true},
		{"duplicate workload", manifest + "---\n" + manifest, true},
		{"missing evidence", manifest, true},
		{"evidence image mismatch", manifest, true},
		{"missing verified commit", manifest, true},
		{"unverified commit", manifest, true},
		{"candidate without successful receipt", manifest, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := upgradeProcess{run: func(ctx context.Context, stdin, name string, args ...string) ([]byte, error) {
				joined := strings.Join(args, " ")
				if name == "kubectl" && joined == "-n kz-ops get configmap doops-upgrade-verified-doops-agent-live --ignore-not-found -o json" {
					if tc.name == "missing evidence" {
						return nil, nil
					}
					image := wanted
					if tc.name == "evidence image mismatch" {
						image = testPinnedImage()
					}
					commit, outcome := testBaselineCommit, "succeeded"
					if tc.name == "missing verified commit" {
						commit = ""
					}
					if tc.name == "unverified commit" {
						outcome = "failed"
					}
					entry := map[string]string{"image": image, "release_commit": commit, "outcome": outcome}
					if tc.name == "candidate without successful receipt" {
						entry["phase"], entry["verification_stage"] = "apply", "controller"
						entry["upgrade_operation_id"] = "upgrade-review"
						entry["job"] = selfUpgradeJobName(entry["upgrade_operation_id"], "apply")
					}
					evidence, _ := json.Marshal(map[string]interface{}{
						"schemaVersion": 1, "release": "doops-agent-live", "workload": "doops-agent",
						"revisions": map[string]interface{}{"7": entry},
					})
					raw, _ := json.Marshal(map[string]interface{}{"data": map[string]string{"evidence.json": string(evidence)}})
					return raw, nil
				}
				if tc.name == "candidate without successful receipt" && name == "kubectl" && strings.Contains(joined, "doops-upgrade-receipt-") {
					return nil, nil
				}
				if name != "helm" {
					t.Fatalf("unexpected mutation or command: %s %s", name, joined)
				}
				if strings.HasPrefix(joined, "history ") {
					return []byte(`[{"revision":7,"status":"superseded"},{"revision":8,"status":"deployed"},{"revision":9,"status":"failed"}]`), nil
				}
				if joined != "get manifest doops-agent-live -n kz-ops --revision 7" {
					t.Fatalf("rollback selected the wrong revision: %s", joined)
				}
				return []byte(tc.manifest), nil
			}}
			got, revision, commit, err := p.rollbackImage(t.Context(), "kz-ops", "doops-agent-live", "doops-agent")
			if (err != nil) != tc.wantErr || (!tc.wantErr && (got != wanted || revision != 7 || commit != testBaselineCommit)) {
				t.Fatalf("rollback artifact = %q, %v", got, err)
			}
		})
	}
}

func TestSelfUpgradeReceiptFailurePreventsMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "calls.log")
	env := selfUpgradeScriptEnv(path)
	env["RECEIPT_FAIL"] = "1"
	out, err := runSelfUpgradeScript(t, selfUpgradeApplyScript, selfUpgradeFakeCommands(), env)
	if err == nil || !strings.Contains(out, "category=verification_receipt_failed") {
		t.Fatalf("missing durable baseline must fail: %v\n%s", err, out)
	}
	log, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(log), "helm upgrade") || strings.Contains(string(log), "helm rollback") {
		t.Fatalf("mutation dispatched without baseline receipt: %s", log)
	}
}

func TestSelfUpgradeApplyScriptReportsRollbackFailure(t *testing.T) {
	callLog := filepath.Join(t.TempDir(), "calls.log")
	env := selfUpgradeScriptEnv(callLog)
	env["BAD_FIELD"] = "availableReplicas"
	env["DATE_CALLS"] = filepath.Join(t.TempDir(), "date-calls")
	env["ROLLBACK_FAIL"] = "1"
	out, err := runSelfUpgradeScript(t, selfUpgradeApplyScript, selfUpgradeFakeCommands(), env)
	if err == nil {
		t.Fatalf("script reported success after rollback failure:\n%s", out)
	}
	for _, part := range []string{
		"helm rollback failed",
		"Error: automatic rollback failed: Helm rollback command failed",
		"Error: automatic rollback failed; manual intervention is required",
	} {
		if !strings.Contains(out, part) {
			t.Fatalf("rollback failure evidence missing %q:\n%s", part, out)
		}
	}
	if strings.Contains(out, "self-upgrade complete") {
		t.Fatalf("failed rollback was reported as upgrade success:\n%s", out)
	}
}

func TestSelfUpgradeApplyScriptReportsRollbackVerificationFailure(t *testing.T) {
	callLog := filepath.Join(t.TempDir(), "calls.log")
	env := selfUpgradeScriptEnv(callLog)
	env["BAD_FIELD"] = "availableReplicas"
	env["DATE_CALLS"] = filepath.Join(t.TempDir(), "date-calls")
	env["ROLLBACK_VERIFY_FAIL"] = "1"
	out, err := runSelfUpgradeScript(t, selfUpgradeApplyScript, selfUpgradeFakeCommands(), env)
	if err == nil {
		t.Fatalf("script reported success after rollback verification failure:\n%s", out)
	}
	if !strings.Contains(out, "Error: automatic rollback failed: rolled-back Deployment final state is not verified") ||
		!strings.Contains(out, "Error: automatic rollback failed; manual intervention is required") {
		t.Fatalf("rollback verification failure was not visible:\n%s", out)
	}
	if strings.Contains(out, "automatic rollback complete") {
		t.Fatalf("rollback verification failure was reported as complete:\n%s", out)
	}
}

func TestSelfUpgradeApplyScriptRejectsStableWindowDrift(t *testing.T) {
	callLog := filepath.Join(t.TempDir(), "calls.log")
	env := selfUpgradeScriptEnv(callLog)
	env["DATE_CALLS"] = filepath.Join(t.TempDir(), "date-calls")
	env["DRIFT_ON_SECOND_VERIFY"] = "1"
	out, err := runSelfUpgradeScript(t, selfUpgradeApplyScript, selfUpgradeFakeCommands(), env)
	if err == nil {
		t.Fatalf("stable-window drift was accepted:\n%s", out)
	}
	if strings.Contains(out, "self-upgrade complete") ||
		!strings.Contains(out, "automatic rollback complete revision=5") {
		t.Fatalf("stable-window drift did not fail and roll back:\n%s", out)
	}
}

func TestSelfUpgradeApplyScriptDryRunFailureDoesNotMutate(t *testing.T) {
	callLog := filepath.Join(t.TempDir(), "calls.log")
	env := selfUpgradeScriptEnv(callLog)
	env["DRY_RUN_FAIL"] = "1"
	out, err := runSelfUpgradeScript(t, selfUpgradeApplyScript, selfUpgradeFakeCommands(), env)
	if err == nil {
		t.Fatalf("dry-run failure was accepted:\n%s", out)
	}
	logBytes, readErr := os.ReadFile(callLog)
	if readErr != nil {
		t.Fatal(readErr)
	}
	log := string(logBytes)
	if !strings.Contains(log, "--dry-run=server") {
		t.Fatalf("dry-run command was not attempted:\n%s", log)
	}
	for _, line := range strings.Split(strings.TrimSpace(log), "\n") {
		if strings.Contains(line, "helm upgrade") && !strings.Contains(line, "--dry-run=server") {
			t.Fatalf("mutation ran after dry-run failure:\n%s", log)
		}
	}
	if strings.Contains(log, "helm rollback") || strings.Contains(log, "rollout status") {
		t.Fatalf("dry-run failure unexpectedly entered rollback:\n%s", log)
	}
}

func TestSelfUpgradeRejectsInvalidPodEvidence(t *testing.T) {
	for _, field := range []string{"missing", "uid", "node", "ready", "image", "imageID", "digest"} {
		t.Run(field, func(t *testing.T) {
			env := selfUpgradeScriptEnv(filepath.Join(t.TempDir(), "calls.log"))
			env["BAD_POD_FIELD"] = field
			out, err := runSelfUpgradeScript(t, selfUpgradeApplyScript, selfUpgradeFakeCommands(), env)
			if err == nil || strings.Contains(out, "self-upgrade complete") {
				t.Fatalf("invalid Pod %s accepted: %v\n%s", field, err, out)
			}
			if !strings.Contains(out, "automatic rollback complete revision=5") {
				t.Fatalf("invalid Pod did not restore the old release:\n%s", out)
			}
		})
	}
}

func TestSelfUpgradeReplacesALiveStrategyTheChartWillCorrect(t *testing.T) {
	// A legacy Deployment with maxSurge 0 is exactly what the Chart handoff fixes.
	for _, surge := range []string{"0", "2", `"100%"`} {
		t.Run(surge, func(t *testing.T) {
			logPath := filepath.Join(t.TempDir(), "calls.log")
			env := selfUpgradeScriptEnv(logPath)
			env["MAX_SURGE"] = surge
			out, err := runSelfUpgradeScript(t, selfUpgradeApplyScript, selfUpgradeFakeCommands(), env)
			if strings.Contains(out, "maxSurge must be 1") || strings.Contains(out, "maxUnavailable must be 0") {
				t.Fatalf("live strategy still gates the upgrade: %v\n%s", err, out)
			}
		})
	}
}

func TestAgentChartHandsOffWithSurgeAndNoUnavailable(t *testing.T) {
	raw, err := os.ReadFile("../../../deploy/helm/doops-agent/templates/deployment.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"maxUnavailable: 0", "maxSurge: 1", "hostNetwork: false"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("Chart template lost %q", want)
		}
	}
	if !strings.Contains(string(raw), "hostPID: {{ $enterNode }}") {
		t.Fatal("node entry must stay behind hostAccess.enabled")
	}
	if !strings.Contains(string(raw), "mountPath: /host") || !strings.Contains(string(raw), "readOnly: true") {
		t.Fatal("enabled node entry must mount the node root read-only at /host")
	}
}

func TestSelfUpgradeRollbackRejectsDriftAfterHelmSuccess(t *testing.T) {
	env := selfUpgradeScriptEnv(filepath.Join(t.TempDir(), "calls.log"))
	env["DOOPS_UPGRADE_IMAGE"] = "registry.example/doops:old"
	env["ROLLBACK_VERIFY_FAIL"] = "1"
	out, err := runSelfUpgradeScript(t, selfUpgradeRollbackScript, selfUpgradeFakeCommands(), env)
	if err == nil || strings.Contains(out, "self-upgrade rolled back to") {
		t.Fatalf("rollback accepted a different live image: %v\n%s", err, out)
	}
	if !strings.Contains(out, "image mismatch") {
		t.Fatalf("rollback did not report final image mismatch:\n%s", out)
	}
}

func TestSelfUpgradeRollbackUsesSuccessfulSupersededRevision(t *testing.T) {
	callLog := filepath.Join(t.TempDir(), "calls.log")
	env := selfUpgradeScriptEnv(callLog)
	env["DOOPS_UPGRADE_IMAGE"] = "registry.example/doops:old"
	env["DOOPS_UPGRADE_REVISION"] = "7"
	commands := selfUpgradeFakeCommands()
	commands["helm"] = `#!/bin/sh
set -eu
printf 'helm %s\n' "$*" >> "$CALL_LOG"
	case "${1:-}" in
  history)
    printf '%s\n' '[{"revision":6,"status":"failed"},{"revision":7,"status":"superseded"},{"revision":8,"status":"deployed"}]'
    ;;
  get)
    printf '%s\n' '{"kind":"Deployment","metadata":{"name":"doops-agent"},"spec":{"template":{"spec":{"containers":[{"name":"doops-agent","image":"registry.example/doops:old"}]}}}}'
    ;;
  rollback)
    printf 'rolled back\n' >"$ROLLBACK_MARKER"
    ;;
  *)
    echo "unexpected helm command: $*" >&2
    exit 2
    ;;
esac
`
	out, err := runSelfUpgradeScript(t, selfUpgradeRollbackScript, commands, env)
	if err != nil {
		t.Fatalf("manual rollback failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "self-upgrade rolled back to 7") {
		t.Fatalf("manual rollback evidence = %s", out)
	}
	logBytes, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatal(err)
	}
	log := string(logBytes)
	if !strings.Contains(log, "helm rollback doops-agent-live 7") {
		t.Fatalf("manual rollback did not use the prior superseded revision:\n%s", log)
	}
	if strings.Contains(log, "helm rollback doops-agent-live 9") {
		t.Fatalf("manual rollback selected a failed revision:\n%s", log)
	}
}

func selfUpgradeScriptEnv(callLog string) map[string]string {
	return map[string]string{
		"DOOPS_UPGRADE_REVISION":                "4",
		"RECEIPT_FILE":                          callLog + ".receipt",
		"OPERATION_RECEIPT_FILE":                callLog + ".operation-receipt",
		"HOSTNAME":                              "upgrade-controller",
		"CALL_LOG":                              callLog,
		"KUBECTL_CALLS":                         callLog + ".kubectl",
		"DATE_CALLS":                            callLog + ".date",
		"ROLLBACK_MARKER":                       callLog + ".rollback",
		"DOOPS_UPGRADE_NAMESPACE":               "kz-ops",
		"DOOPS_UPGRADE_RELEASE":                 "doops-agent-live",
		"DOOPS_UPGRADE_WORKLOAD":                "doops-agent",
		"DOOPS_UPGRADE_REPOSITORY":              "registry.example/doops",
		"DOOPS_UPGRADE_DIGEST":                  "sha256:" + testPinnedDigest,
		"DOOPS_UPGRADE_IMAGE":                   testPinnedImage(),
		"DOOPS_UPGRADE_OPERATION_ID":            "test-operation",
		"DOOPS_UPGRADE_JOB":                     selfUpgradeJobName("test-operation", "apply"),
		"DOOPS_UPGRADE_EXPECTED_RELEASE_COMMIT": testUpgradeCommit,
		"DOOPS_UPGRADE_PHASE":                   "apply",
		"DOOPS_UPGRADE_REQUESTER_ID":            "operator-1",
		"DOOPS_UPGRADE_SESSION_ID":              "session-1",
		"DOOPS_UPGRADE_AUDIT_ID":                "42",
		"DOOPS_UPGRADE_SOURCE_COMMIT":           testUpgradeCommit,
		"DOOPS_UPGRADE_CI_URL":                  "https://ci.example/build/42",
	}
}

func runSelfUpgradeScript(t *testing.T, script string, commands map[string]string, extraEnv map[string]string) (string, error) {
	t.Helper()
	binDir := t.TempDir()
	for name, contents := range commands {
		path := filepath.Join(binDir, name)
		if err := os.WriteFile(path, []byte(contents), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	body := strings.TrimPrefix(script, selfUpgradeKubeconfigPrelude)
	if body == script {
		t.Fatal("script does not contain the kubeconfig prelude")
	}
	if script == selfUpgradeRollbackScript {
		extraEnv["DOOPS_UPGRADE_EXPECTED_RELEASE_COMMIT"] = testBaselineCommit
		extraEnv["DOOPS_UPGRADE_PHASE"] = "rollback"
		entry := map[string]string{
			"image":         firstNonEmptyString(extraEnv["OLD_IMAGE"], "registry.example/doops:old"),
			"runtimeDigest": firstNonEmptyString(extraEnv["OLD_RUNTIME_DIGEST"], "sha256:"+testPinnedDigest),
			"node":          "node-a", "release_commit": testBaselineCommit, "outcome": "succeeded",
		}
		evidence, _ := json.Marshal(map[string]interface{}{
			"schemaVersion": 1, "release": "doops-agent-live", "workload": "doops-agent",
			"revisions": map[string]interface{}{extraEnv["DOOPS_UPGRADE_REVISION"]: entry},
		})
		receipt, _ := json.Marshal(map[string]interface{}{"data": map[string]string{"evidence.json": string(evidence)}})
		if err := os.WriteFile(extraEnv["RECEIPT_FILE"], receipt, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if script == selfUpgradePreflightScript || script == selfUpgradePullOnlyScript {
		extraEnv["DOOPS_UPGRADE_PHASE"] = "preflight"
	}
	extraEnv["DOOPS_UPGRADE_JOB"] = selfUpgradeJobName(extraEnv["DOOPS_UPGRADE_OPERATION_ID"], extraEnv["DOOPS_UPGRADE_PHASE"])
	prelude := ""
	if script == selfUpgradeApplyScript || script == selfUpgradePreflightScript || script == selfUpgradePullOnlyScript || script == selfUpgradeRollbackScript {
		prelude = selfUpgradeEvidencePrelude
	}
	command := prelude + `
ns="$DOOPS_UPGRADE_NAMESPACE"
release="$DOOPS_UPGRADE_RELEASE"
workload="$DOOPS_UPGRADE_WORKLOAD"
` + body
	cmd := exec.Command("sh", "-c", command)
	env := append([]string{}, os.Environ()...)
	env = append(env, "PATH="+binDir+":"+os.Getenv("PATH"))
	env = append(env, "TMPDIR="+t.TempDir())
	pythonPath, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	env = append(env, "PYTHONPATH="+pythonPath)
	for key, value := range extraEnv {
		env = append(env, key+"="+value)
	}
	cmd.Env = env
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func selfUpgradeFakeCommands() map[string]string {
	return map[string]string{
		"sleep": `#!/bin/sh
exit 0
`,
		// skopeo answers with the registry's own wording, taken from the live
		// registry responses: a missing digest is "artifact ... not found",
		// a refused credential is "unauthorized: requested access ... denied".
		"skopeo": `#!/bin/sh
set -eu
printf 'skopeo %s\n' "$*" >> "$CALL_LOG"
authfile=""
previous=""
for arg in "$@"; do
  if [ "$previous" = "--authfile" ]; then authfile="$arg"; fi
  previous="$arg"
done
if [ -n "$authfile" ]; then
  stat -c '%a' "$authfile" 2>/dev/null >"$CALL_LOG.authmode" || stat -f '%Lp' "$authfile" >"$CALL_LOG.authmode"
  cat "$authfile" >"$CALL_LOG.authfile"
  printf '%s\n' "$authfile" >"$CALL_LOG.authpath"
fi
case "${SKOPEO_RESULT:-ok}" in
  ok)
    printf '{"schemaVersion":2}\n'
    ;;
  missing)
    echo 'time="2026-11-01T01:22:58Z" level=fatal msg="Error parsing image name: reading manifest sha256:0000 in registry.example/doops: unknown: artifact doops@sha256:0000 not found"' >&2
    exit 1
    ;;
  manifest-unknown)
    echo 'level=fatal msg="reading manifest old in registry.example/doops: manifest unknown: manifest unknown"' >&2
    exit 1
    ;;
  refused)
    echo 'level=fatal msg="Error parsing image name: unable to retrieve auth token: invalid username/password: unauthorized: requested access to the resource is denied"' >&2
    exit 1
    ;;
  refused-not-found)
    echo 'level=fatal msg="reading manifest: unauthorized: repository not found or access denied"' >&2
    exit 1
    ;;
  unreachable)
    echo 'level=fatal msg="pinging container registry registry.example: Get \"https://registry.example/v2/\": dial tcp: i/o timeout"' >&2
    exit 1
    ;;
esac
`,
		"date": `#!/bin/sh
set -eu
calls=0
if [ -f "$DATE_CALLS" ]; then
  calls="$(cat "$DATE_CALLS")"
fi
calls=$((calls + 1))
printf '%s\n' "$calls" >"$DATE_CALLS"
if [ "$calls" -ge 3 ]; then
  printf '601\n'
else
  printf '0\n'
fi
`,
		"helm": `#!/bin/sh
set -eu
printf 'helm %s\n' "$*" >> "$CALL_LOG"
# volumes_json turns "a,b" into [{"name":"a"},{"name":"b"}].
volumes_json() {
  out=""
  for name in $(printf '%s' "$1" | tr ',' ' '); do
    out="${out}${out:+,}{\"name\":\"${name}\"}"
  done
  printf '[%s]' "$out"
}
case "${1:-}" in
  template)
    if [ "${HELM_TEMPLATE_EMPTY:-0}" = "1" ]; then exit 0; fi
    printf '{"kind":"Deployment","metadata":{"name":"doops-agent"},"spec":{"template":{"spec":{"containers":[{"name":"doops-agent","image":"%s"}],"volumes":%s}}}}\n' "$DOOPS_UPGRADE_IMAGE" "$(volumes_json "${RENDERED_VOLUMES:-}")"
    ;;
  history)
    if [ -n "${HELM_HISTORY:-}" ]; then
      printf '%s\n' "$HELM_HISTORY"
    elif [ -f "$ROLLBACK_MARKER" ]; then
      printf '%s\n' '[{"revision":5,"status":"superseded"},{"revision":6,"status":"failed"},{"revision":7,"status":"deployed"}]'
    elif [ -f "$CALL_LOG.mutation" ]; then
      printf '%s\n' '[{"revision":4,"status":"superseded"},{"revision":5,"status":"superseded"},{"revision":6,"status":"deployed"}]'
    else
      printf '%s\n' '[{"revision":4,"status":"superseded"},{"revision":5,"status":"deployed"},{"revision":6,"status":"failed"}]'
    fi
    ;;
  get)
    if [ "${2:-}" = "manifest" ]; then
      if [ -n "${REVISION_MANIFEST:-}" ]; then
        printf '%s\n' "$REVISION_MANIFEST"
      else
        printf '{"kind":"Deployment","metadata":{"name":"doops-agent"},"spec":{"template":{"spec":{"containers":[{"name":"doops-agent","image":"%s"}],"volumes":%s}}}}\n' "${OLD_IMAGE:-registry.example/doops:old}" "$(volumes_json "${PREVIOUS_VOLUMES:-}")"
      fi
    else
      printf '%s\n' '{"image":{"repository":"registry.example/doops","tag":"old"},"workload":{"name":"doops-agent"}}'
    fi
    ;;
  upgrade)
    case "$*" in
      *"--dry-run=server"*)
        if [ "${DRY_RUN_FAIL:-0}" = "1" ]; then
          echo "helm dry-run failed" >&2
          exit 19
        fi
        exit 0
        ;;
    esac
    : >"$CALL_LOG.mutation"
    exit 0
    ;;
  rollback)
    if [ "${ROLLBACK_FAIL:-0}" = "1" ]; then
      echo "helm rollback failed" >&2
      exit 17
    fi
    printf 'rolled back\n' >"$ROLLBACK_MARKER"
    ;;
  *)
    echo "unexpected helm command: $*" >&2
    exit 2
    ;;
esac
`,
		"kubectl": `#!/bin/sh
set -eu
printf 'kubectl %s\n' "$*" >> "$CALL_LOG"
case "$*" in
  *"exec agent-pod -c doops-agent -- /app/doops-agent -version"*)
    commit="${BASELINE_COMMIT-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb}"
    if [ -f "$CALL_LOG.mutation" ] && [ ! -f "$ROLLBACK_MARKER" ]; then
      commit="${ACTUAL_COMMIT-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa}"
    fi
    printf 'release=v-test commit=%s\n' "$commit"
    ;;
  *"exec agent-pod -c doops-agent -- curl"*)
    if [ "${GATEWAY_NOT_READY:-0}" = "1" ] && [ -f "$CALL_LOG.mutation" ] && [ ! -f "$ROLLBACK_MARKER" ]; then exit 22; fi
    printf 'ready\n'
    ;;
  "auth whoami -o json")
    if [ "${AUTH_WHOAMI_DENIED:-0}" = "1" ]; then echo "auth whoami forbidden" >&2; exit 21; fi
    printf '{"status":{"userInfo":{"username":"%s"}}}\n' "${AUTH_USERNAME-kubernetes-admin}"
    ;;
  *"get configmap"*)
    case "$*" in
      *doops-upgrade-receipt-*) file="$OPERATION_RECEIPT_FILE" ;;
      *) file="$RECEIPT_FILE" ;;
    esac
    if [ -f "$file" ]; then cat "$file"; fi
    ;;
  *"apply -f -"*)
    if [ "${RECEIPT_FAIL:-0}" = "1" ]; then echo "verification receipt persistence failed" >&2; exit 17; fi
    payload="$(cat)"
    case "$payload" in
      *doops-upgrade-receipt-*)
        if [ "${OPERATION_RECEIPT_FAIL:-0}" = "1" ]; then exit 17; fi
        file="$OPERATION_RECEIPT_FILE"
        ;;
      *) file="$RECEIPT_FILE" ;;
    esac
    printf '%s' "$payload" >"$file"
    ;;
  *"apply -f "*)
    # The stale mount backup is applied from a file before the first change.
    if [ "${MOUNT_BACKUP_FAIL:-0}" = "1" ]; then echo "mount backup persistence failed" >&2; exit 18; fi
    file="$(printf '%s\n' "$*" | sed 's/.*apply -f //')"
    cp "$file" "$CALL_LOG.mountbackup"
    ;;
  *"patch deployment"*)
    # A remove patch takes the stale mounts out of the live Deployment; an add
    # patch puts them back.
    case "$*" in
      *'"op":"remove"'*)
        if [ "${REMOVE_PATCH_FAIL:-0}" = "1" ]; then echo "remove patch rejected" >&2; exit 22; fi
        : >"$CALL_LOG.mounts-removed"
        ;;
      *'"op":"add"'*)
        if [ "${RESTORE_PATCH_FAIL:-0}" = "1" ]; then echo "restore patch rejected" >&2; exit 23; fi
        rm -f "$CALL_LOG.mounts-removed"
        ;;
    esac
    ;;
  *"get secret"*)
    case "${PULL_SECRET:-}" in
      missing)
        echo 'Error from server (NotFound): secrets "registry-pull" not found' >&2
        exit 1
        ;;
      opaque)
        printf '{"type":"Opaque","data":{"token":"c3ludGhldGlj"}}\n'
        ;;
      *)
        # {"auths":{"registry.example":{"auth":"<synthetic canary>"}}}
        printf '{"type":"kubernetes.io/dockerconfigjson","data":{".dockerconfigjson":"%s"}}\n' \
          "$(printf '{"auths":{"registry.example":{"auth":"%s"}}}' "$PULL_SECRET_CANARY" | base64 | tr -d '\n')"
        ;;
    esac
    ;;
  *"-o jsonpath="*)
    echo "jsonpath is forbidden in this test" >&2
    exit 91
    ;;
  *"rollout status"*)
    exit 0
    ;;
  *"get pod upgrade-controller"*)
    image="$DOOPS_UPGRADE_IMAGE"
    image_id="registry.example/doops@${RUNTIME_DIGEST:-$DOOPS_UPGRADE_DIGEST}"
    node="node-a"
    ready=true
    case "${BAD_CONTROLLER_FIELD:-}" in
      image) image="registry.example/doops:old" ;;
      imageID) image_id="" ;;
      node) node="" ;;
      ready) ready=false ;;
    esac
    printf '{"metadata":{"name":"upgrade-controller","uid":"controller-uid"},"spec":{"nodeName":"%s","containers":[{"name":"controller","image":"%s"}]},"status":{"phase":"Running","containerStatuses":[{"name":"controller","ready":%s,"imageID":"%s"}]}}\n' "$node" "$image" "$ready" "$image_id"
    ;;
  *"get pods"*)
    image="$DOOPS_UPGRADE_IMAGE"
    image_id="registry.example/doops@${RUNTIME_DIGEST:-$DOOPS_UPGRADE_DIGEST}"
    uid="candidate-pod"
    ready=true
    node="${AGENT_NODE:-node-a}"
    calls=0
    if [ -f "$KUBECTL_CALLS" ]; then calls="$(cat "$KUBECTL_CALLS")"; fi
    if [ -f "$ROLLBACK_MARKER" ] || [ "$calls" -lt 4 ]; then
      image="${OLD_IMAGE:-registry.example/doops:old}"
      image_id="registry.example/doops@${OLD_RUNTIME_DIGEST:-$DOOPS_UPGRADE_DIGEST}"
      uid="restored-pod"
      node="${BASELINE_NODE:-node-a}"
      if [ -f "$ROLLBACK_MARKER" ] && [ -n "${RESTORED_NODE:-}" ]; then node="$RESTORED_NODE"; fi
    else
      if [ -n "${CANDIDATE_IMAGE_ID:-}" ]; then image_id="$CANDIDATE_IMAGE_ID"; fi
      case "${BAD_POD_FIELD:-}" in
        missing) printf '{"items":[]}\n'; exit 0 ;;
        uid) uid="" ;;
        node) node="" ;;
        ready) ready=false ;;
        image) image="registry.example/doops:old" ;;
        imageID) image_id="" ;;
        digest) image_id="registry.example/doops@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" ;;
      esac
    fi
    pull_secrets=""
    if [ -n "${PULL_SECRET:-}" ]; then pull_secrets=',"imagePullSecrets":[{"name":"registry-pull"}]'; fi
    printf '{"items":[{"metadata":{"name":"agent-pod","uid":"%s"},"spec":{"nodeName":"%s"%s,"containers":[{"name":"doops-agent","image":"%s"}]},"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}],"containerStatuses":[{"name":"doops-agent","ready":%s,"imageID":"%s"}]}}]}\n' "$uid" "$node" "$pull_secrets" "$image" "$ready" "$image_id"
    ;;
  *"get deployment"*)
    calls=0
    if [ -f "$KUBECTL_CALLS" ]; then
      calls="$(cat "$KUBECTL_CALLS")"
    fi
    calls=$((calls + 1))
    printf '%s\n' "$calls" >"$KUBECTL_CALLS"
    image="${OLD_IMAGE:-registry.example/doops:old}"
    if [ ! -f "$ROLLBACK_MARKER" ] && [ "$calls" -ge 4 ]; then
      image="$DOOPS_UPGRADE_IMAGE"
    fi
    if [ ! -f "$ROLLBACK_MARKER" ] && [ "${BAD_FIELD:-}" = "image" ] && [ "$calls" -ge 4 ]; then
      image="registry.example/doops@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
    fi
    if [ ! -f "$ROLLBACK_MARKER" ] && [ "${DRIFT_ON_SECOND_VERIFY:-0}" = "1" ] && [ "$calls" -ge 5 ]; then
      image="registry.example/doops@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
    fi
    generation=2
    observed=2
    replicas=1
    status_replicas=1
    updated=1
    ready=1
    available=1
    unavailable=0
    if [ ! -f "$ROLLBACK_MARKER" ] && [ "$calls" -ge 4 ]; then
      case "${BAD_FIELD:-}" in
        observedGeneration) observed=1 ;;
        replicas) replicas=2 ;;
        desiredReplicas) replicas=0 ;;
        statusReplicas) status_replicas=0 ;;
        updatedReplicas) updated=0 ;;
        readyReplicas) ready=0 ;;
        availableReplicas) available=0 ;;
        unavailableReplicas) unavailable=1 ;;
      esac
    fi
    if [ -f "$ROLLBACK_MARKER" ]; then
      status_replicas=1
      updated=1
      ready=1
      available=1
      unavailable=0
    fi
	if [ "${SURGE_REPLICAS:-0}" = "1" ] && [ ! -f "$ROLLBACK_MARKER" ] && [ "$calls" -ge 4 ]; then
		status_replicas=2
	fi
    if [ -f "$ROLLBACK_MARKER" ] && [ "${ROLLBACK_VERIFY_FAIL:-0}" = "1" ]; then
      image="registry.example/doops@sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
    fi
    # LIVE_MOUNTS is "name:path,name:path" on the doops-agent container; a
    # remove patch takes them out until an add patch restores them.
    mounts=""
    if [ ! -f "$CALL_LOG.mounts-removed" ]; then
      for item in $(printf '%s' "${LIVE_MOUNTS:-}" | tr ',' ' '); do
        mounts="${mounts}${mounts:+,}{\"name\":\"${item%%:*}\",\"mountPath\":\"${item#*:}\"}"
      done
    fi
    printf '{"metadata":{"name":"doops-agent","generation":%s},"spec":{"replicas":%s,"strategy":{"type":"RollingUpdate","rollingUpdate":{"maxUnavailable":0,"maxSurge":%s}},"selector":{"matchLabels":{"app":"doops"}},"template":{"spec":{"containers":[{"name":"sidecar","image":"registry.example/sidecar:old"},{"name":"doops-agent","image":"%s","volumeMounts":[%s]}]}}},"status":{"observedGeneration":%s,"replicas":%s,"updatedReplicas":%s,"readyReplicas":%s,"availableReplicas":%s,"unavailableReplicas":%s}}\n' "$generation" "$replicas" "${MAX_SURGE:-1}" "$image" "$mounts" "$observed" "$status_replicas" "$updated" "$ready" "$available" "$unavailable"
    ;;
  *)
    echo "unexpected kubectl command: $*" >&2
    exit 2
    ;;
esac
`,
	}
}

func TestSelfUpgradeJobUsesSharedKubeconfigVolume(t *testing.T) {
	pod := selfUpgradePod{
		image:              "registry.example/doops:old",
		nodeName:           "node-a",
		serviceAccountName: "default",
		runtimeClassName:   "verified-runtime",
		imagePullSecrets:   []string{"registry-pull"},
		mount:              jobVolumeMount{Name: "kube", MountPath: "/root/.kube", ReadOnly: true},
		volume:             json.RawMessage(`{"name":"kube","secret":{"secretName":"cluster-admin"}}`),
	}
	manifest, err := selfUpgradeJobManifest("doops-self-upgrade-aaaaaaaa", "kz-ops", pod, selfUpgradePlan{
		image:      testPinnedImage(),
		repository: "registry.example/doops",
		digest:     "sha256:" + testPinnedDigest,
		release:    "doops-agent-live",
		workload:   "doops-agent",
		script:     selfUpgradeApplyScript,
	})
	if err != nil {
		t.Fatal(err)
	}
	var job map[string]interface{}
	if err := json.Unmarshal(manifest, &job); err != nil {
		t.Fatal(err)
	}
	text := string(manifest)
	if strings.Contains(text, "hostPath") || strings.Contains(text, "hostNetwork") || strings.Contains(text, "hostPID") {
		t.Fatalf("job copied node privileges: %s", text)
	}
	if !strings.Contains(text, `"privileged":false`) {
		t.Fatalf("controller must not be privileged: %s", text)
	}
	if !strings.Contains(text, testPinnedImage()) || strings.Contains(text, "registry.example/doops:old") {
		t.Fatalf("job image should be the pinned target: %s", text)
	}
	spec := job["spec"].(map[string]interface{})
	if spec["backoffLimit"].(float64) != 0 {
		t.Fatalf("backoffLimit = %v", spec["backoffLimit"])
	}
	podSpec := spec["template"].(map[string]interface{})["spec"].(map[string]interface{})
	if podSpec["restartPolicy"] != "Never" {
		t.Fatalf("restartPolicy = %v", podSpec["restartPolicy"])
	}
	if podSpec["nodeName"] != "node-a" {
		t.Fatal("the controller must use the Agent node runtime even with a Secret kubeconfig")
	}
	if podSpec["runtimeClassName"] != "verified-runtime" {
		t.Fatal("the controller must retain the Agent runtime class")
	}
	volumes := podSpec["volumes"].([]interface{})
	if len(volumes) != 1 {
		t.Fatalf("volumes = %#v", volumes)
	}
}

func TestSelfUpgradePinsHostPathJobToTheAgentNode(t *testing.T) {
	_, err := nodeNameForVolume(json.RawMessage(`{"name":"kube","emptyDir":{}}`), "node-a")
	if err == nil || !strings.Contains(err.Error(), "cannot be shared") {
		t.Fatalf("emptyDir error = %v", err)
	}
	node, err := nodeNameForVolume(json.RawMessage(`{"name":"kube","hostPath":{"path":"/root/.kube"}}`), "node-a")
	if err != nil || node != "node-a" {
		t.Fatalf("hostPath node = %q err=%v", node, err)
	}
}

func TestStartSelfUpgradeCreatesJobWithoutHelmMutation(t *testing.T) {
	var applied string
	proc := upgradeProcess{
		namespace: "kz-ops",
		hostname:  "doops-agent-abc",
		cluster:   "doops-edu",
		instance:  "edu-coder",
		run: func(ctx context.Context, stdin string, name string, args ...string) ([]byte, error) {
			joined := strings.Join(args, " ")
			if name == "helm" && strings.Contains(joined, "upgrade") {
				t.Fatalf("agent process ran helm upgrade: %s", joined)
			}
			switch {
			case strings.Contains(joined, "get jobs"):
				return []byte(`{"items":[]}`), nil
			case strings.Contains(joined, "get job "):
				return nil, fmt.Errorf("Error from server (NotFound): jobs.batch not found")
			case strings.Contains(joined, "get pod"):
				return []byte(selfUpgradePodFixture), nil
			case name == "helm" && strings.Contains(joined, "list"):
				return []byte(`[{"name":"doops-agent-live","chart":"doops-agent-0.1.0"}]`), nil
			case name == "helm" && strings.Contains(joined, "get values"):
				return []byte(`{"gateway":{"cluster":"doops-edu","instance":"edu-coder"},"workload":{"name":"doops-agent"}}`), nil
			case strings.Contains(joined, "create"):
				applied = stdin
				return []byte("job.batch/doops-self-upgrade-aaaaaaaa created\n"), nil
			default:
				t.Fatalf("unexpected command %s %s", name, joined)
				return nil, nil
			}
		},
	}
	out, err := proc.startSelfUpgradeJob(context.Background(), agentUpgradeParams{
		Image: testPinnedImage(), Mode: "k8s", UpgradeOperationID: "operation-123", ExpectedReleaseCommit: testUpgradeCommit,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "self-upgrade-accepted job="+selfUpgradeJobName("operation-123", "apply")+" namespace=kz-ops release=doops-agent-live") {
		t.Fatalf("acceptance = %s", out)
	}
	if !strings.Contains(applied, "helm rollback") || strings.Contains(applied, "set image") {
		t.Fatalf("job manifest lost the rollback controller: %s", applied)
	}
	if strings.Contains(applied, "emptyDir") || strings.Contains(applied, `"privileged":true`) {
		t.Fatalf("job copied the agent pod privileges or data volume: %s", applied)
	}
}

func TestStartSelfUpgradeRefusesForeignIdentity(t *testing.T) {
	proc := upgradeProcess{
		namespace: "kz-ops",
		hostname:  "doops-agent-abc",
		cluster:   "doops-edu",
		instance:  "edu-coder",
		run: func(ctx context.Context, stdin string, name string, args ...string) ([]byte, error) {
			joined := strings.Join(args, " ")
			switch {
			case strings.Contains(joined, "get jobs"):
				return []byte(`{"items":[]}`), nil
			case name == "helm" && strings.Contains(joined, "list"):
				return []byte(`[{"name":"doops-agent-live","chart":"doops-agent-0.1.0"}]`), nil
			case name == "helm" && strings.Contains(joined, "get values"):
				return []byte(`{"gateway":{"cluster":"other","instance":"node"},"workload":{"name":"doops-agent"}}`), nil
			default:
				t.Fatalf("identity mismatch continued to %s %s", name, joined)
				return nil, nil
			}
		},
	}
	_, err := proc.startSelfUpgradeJob(context.Background(), agentUpgradeParams{Image: testPinnedImage(), Mode: "k8s"})
	if err == nil || !strings.Contains(err.Error(), "does not match this agent") {
		t.Fatalf("identity error = %v", err)
	}
}

func TestObserveSelfUpgradeReportsTerminalPhase(t *testing.T) {
	proc := upgradeProcess{
		namespace: "kz-ops",
		hostname:  "doops-agent-abc",
		run: func(ctx context.Context, stdin string, name string, args ...string) ([]byte, error) {
			joined := strings.Join(args, " ")
			switch {
			case strings.Contains(joined, "get job"):
				return []byte(`{"metadata":{"annotations":{"doops.sh/upgrade-operation-id":"operation-123","doops.sh/phase":"apply"}},"status":{"succeeded":1}}`), nil
			case strings.Contains(joined, "get configmap"):
				return testOperationReceipt("operation-123", "kz-ops", "apply"), nil
			default:
				t.Fatalf("unexpected observe command %s", joined)
				return nil, nil
			}
		},
	}
	out, err := proc.observeSelfUpgrade(context.Background(), agentUpgradeParams{
		ObserveJob: selfUpgradeJobName("operation-123", "apply"), UpgradeOperationID: "operation-123",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "self-upgrade-succeeded") || !strings.Contains(out, `"release_commit":"`+testUpgradeCommit+`"`) {
		t.Fatalf("observe = %s", out)
	}
}

const selfUpgradePodFixture = `{
  "spec": {
    "nodeName": "node-a",
    "serviceAccountName": "default",
    "imagePullSecrets": [{"name": "registry-pull"}],
    "containers": [{
      "name": "doops-agent",
      "image": "registry.example/doops:old",
      "securityContext": {"privileged": true},
      "volumeMounts": [
        {"name": "kube", "mountPath": "/root/.kube", "readOnly": true},
        {"name": "data", "mountPath": "/var/lib/data"}
      ]
    }],
    "volumes": [
      {"name": "kube", "secret": {"secretName": "cluster-admin"}},
      {"name": "data", "emptyDir": {}}
    ]
  }
}`
