package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	buildrelease "github.com/user/doops/agent/pkg/release"
	"gopkg.in/yaml.v3"
)

// Self-upgrade is a Kubernetes Job, not a Helm command in the agent process.
// The Job uses the kubeconfig volume the agent already mounts, so the operator
// does not pass a kubeconfig. Chart handoff stays maxSurge=1 and maxUnavailable=0.
// If the new Pod does not stay ready, the Job itself rolls back; that controller
// is not the process the rollout deletes.

const (
	selfUpgradeLabelKey    = "app.kubernetes.io/name"
	selfUpgradeLabelValue  = "doops-self-upgrade"
	selfUpgradeChartPrefix = "doops-agent"
	selfUpgradeContainer   = "doops-agent"
	selfUpgradeChartPath   = "/app/deploy/helm/doops-agent"
)

var errNoManagedRelease = errors.New("no managed doops-agent Helm release")

var (
	imageRepositoryPattern = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?::[0-9]{1,5})?(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)+$`)
	imageTagPattern        = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	imageDigestPattern     = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	selfUpgradeJobPattern  = regexp.MustCompile(`^doops-self-upgrade-[a-f0-9]{24}-(preflight|apply|rollback)$`)
)

type upgradeExec func(ctx context.Context, stdin string, name string, args ...string) ([]byte, error)

type upgradeProcess struct {
	namespace string
	hostname  string
	cluster   string
	instance  string
	run       upgradeExec
}

func currentUpgradeProcess() upgradeProcess {
	host, _ := os.Hostname()
	return upgradeProcess{
		namespace: readNamespaceFile(),
		hostname:  host,
		cluster:   os.Getenv("DOOPS_GATEWAY_CLUSTER"),
		instance:  os.Getenv("DOOPS_GATEWAY_INSTANCE"),
		run:       defaultUpgradeExec,
	}
}

func defaultUpgradeExec(ctx context.Context, stdin string, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	return cmd.CombinedOutput()
}

func (p upgradeProcess) command(ctx context.Context, stdin string, name string, args ...string) ([]byte, error) {
	run := p.run
	if run == nil {
		run = defaultUpgradeExec
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return run(cctx, stdin, name, args...)
}

func handleAgentUpgrade(argBytes json.RawMessage) (string, error) {
	var args agentUpgradeParams
	if err := json.Unmarshal(argBytes, &args); err != nil {
		return "", fmt.Errorf("invalid agent upgrade params: %w", err)
	}
	return upgradeAgent(context.Background(), args, currentUpgradeProcess())
}

func validateAgentUpgradeDispatch(args agentUpgradeParams) error {
	if strings.TrimSpace(args.ObserveJob) != "" && args.Rollback {
		return fmt.Errorf("observe_job cannot be combined with rollback")
	}
	if strings.TrimSpace(args.ObserveJob) != "" || args.Rollback {
		return nil
	}
	_, _, _, err := parseUpgradeImage(args.Image)
	return err
}

func upgradeAgent(ctx context.Context, args agentUpgradeParams, proc upgradeProcess) (text string, err error) {
	defer func() {
		if err != nil && selfUpgradeFailureCategory(err.Error()) == "" {
			err = fmt.Errorf("category=upgrade_%s_failed %w", agentUpgradePhase(args), err)
		}
	}()
	if err := validateAgentUpgradeDispatch(args); err != nil {
		return "", err
	}
	if err := validateUpgradeEvidenceParams(args); err != nil {
		return "", err
	}
	if strings.TrimSpace(args.ObserveJob) != "" && args.Rollback {
		return "", fmt.Errorf("observe_job cannot be combined with rollback")
	}
	if args.DryRun && (args.Rollback || strings.TrimSpace(args.ObserveJob) != "") {
		return "", fmt.Errorf("dry_run cannot be combined with observe_job or rollback")
	}
	switch {
	case args.DryRun:
		return dryRunAgentUpgrade(args)
	case strings.TrimSpace(args.ObserveJob) != "":
		return proc.observeSelfUpgrade(ctx, args)
	case args.Rollback:
		return proc.startRollbackJob(ctx, args)
	default:
		return proc.startSelfUpgradeJob(ctx, args)
	}
}

func dryRunAgentUpgrade(args agentUpgradeParams) (string, error) {
	image := strings.TrimSpace(args.Image)
	if image == "" {
		return "", fmt.Errorf("image is required")
	}
	if _, _, _, err := parseUpgradeImage(image); err != nil {
		return "", err
	}
	mode := strings.TrimSpace(args.Mode)
	if mode == "" || mode == "auto" {
		mode = detectAgentUpgradeMode()
	}
	switch mode {
	case "k8s":
		namespace := firstNonEmptyString(args.Namespace, readNamespaceFile(), "oilan-system")
		workload := firstNonEmptyString(args.Workload, "deployment/doops-agent")
		container := firstNonEmptyString(args.Container, selfUpgradeContainer)
		return fmt.Sprintf(
			"dry-run: image=%s runtime=k8s namespace=%s workload=%s container=%s; an in-cluster Job will helm-upgrade that workload and roll back if the new Pod does not stay ready",
			image, namespace, workload, container,
		), nil
	case "daemonset", "docker", "container", "unsupported", "bare":
		return "", fmt.Errorf("doops upgrade replaces a Helm-managed Deployment; bring this workload under Helm first (see BUILD_DEPLOY_SOP.md, unmanaged Agent adoption)")
	default:
		return "", fmt.Errorf("unsupported agent upgrade mode %q", mode)
	}
}

func (p upgradeProcess) startSelfUpgradeJob(ctx context.Context, args agentUpgradeParams) (string, error) {
	mode := strings.TrimSpace(args.Mode)
	if mode == "" || mode == "auto" {
		mode = detectAgentUpgradeMode()
	}
	if mode == "docker" || mode == "container" || mode == "daemonset" || strings.HasPrefix(strings.TrimSpace(args.Workload), "daemonset/") {
		return "", fmt.Errorf("self-upgrade supports the managed Helm Deployment only")
	}
	if mode != "k8s" {
		return "", fmt.Errorf("unsupported agent upgrade mode %q", mode)
	}
	repository, tag, digest, err := parseUpgradeImage(args.Image)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(args.Container) != "" && strings.TrimSpace(args.Container) != selfUpgradeContainer {
		return "", fmt.Errorf("self-upgrade container must be %s", selfUpgradeContainer)
	}
	namespace, err := p.agentNamespace(args.Namespace)
	if err != nil {
		return "", err
	}
	script := selfUpgradeApplyScript
	if args.Preflight {
		script = selfUpgradePreflightScript
	}
	release, workload, err := p.managedRelease(ctx, namespace, args.Release, args.Workload)
	if errors.Is(err, errNoManagedRelease) {
		// No Helm release yet: the same controller Job brings the Deployment under Helm.
		release, workload, err = adoptionTarget(args.Release, args.Workload)
		if err == nil && digest == "" {
			err = fmt.Errorf("adopting an unmanaged Agent requires a repository@sha256:<digest> image")
		}
		script = selfUpgradeAdoptScript
		if args.Preflight {
			script = selfUpgradePullOnlyScript
		}
	}
	if err != nil {
		return "", err
	}
	pod, err := p.podSpec(ctx, namespace)
	if err != nil {
		return "", err
	}
	phase := agentUpgradePhase(args)
	jobName := selfUpgradeJobName(args.UpgradeOperationID, phase)
	if err := p.requireNoActiveUpgrade(ctx, namespace); err != nil {
		return "", err
	}
	manifest, err := selfUpgradeJobManifest(jobName, namespace, pod, selfUpgradePlan{
		image:                        strings.TrimSpace(args.Image),
		repository:                   repository,
		tag:                          tag,
		digest:                       digest,
		release:                      release,
		workload:                     workload,
		script:                       script,
		cluster:                      p.cluster,
		instance:                     p.instance,
		preflight:                    args.Preflight,
		allowMissingRollbackBaseline: args.AllowMissingRollbackBaseline,
		evidence:                     args,
		phase:                        phase,
	})
	if err != nil {
		return "", err
	}
	if err := p.applyJob(ctx, namespace, manifest); err != nil {
		return "", fmt.Errorf("%w job=%s namespace=%s", err, jobName, namespace)
	}
	return fmt.Sprintf(
		"self-upgrade-accepted job=%s namespace=%s release=%s workload=%s image=%s digest=%s expected_release_commit=%s upgrade_operation_id=%s phase=%s",
		jobName, namespace, release, workload, strings.TrimSpace(args.Image), digest, args.ExpectedReleaseCommit, args.UpgradeOperationID, phase,
	), nil
}

func (p upgradeProcess) startRollbackJob(ctx context.Context, args agentUpgradeParams) (string, error) {
	if strings.TrimSpace(args.Image) != "" {
		return "", fmt.Errorf("rollback does not take an image")
	}
	namespace, err := p.agentNamespace(args.Namespace)
	if err != nil {
		return "", err
	}
	release, workload, err := p.managedRelease(ctx, namespace, args.Release, args.Workload)
	if err != nil {
		return "", err
	}
	pod, err := p.podSpec(ctx, namespace)
	if err != nil {
		return "", err
	}
	jobName := selfUpgradeJobName(args.UpgradeOperationID, "rollback")
	rollbackImage, revision, commit, err := p.rollbackImage(ctx, namespace, release, workload)
	if err != nil {
		return "", err
	}
	repository, digest, err := parsePinnedImage(rollbackImage)
	if err != nil {
		return "", err
	}
	if err := p.requireNoActiveUpgrade(ctx, namespace); err != nil {
		return "", err
	}
	args.ExpectedReleaseCommit = commit
	manifest, err := selfUpgradeJobManifest(jobName, namespace, pod, selfUpgradePlan{
		image:           rollbackImage,
		controllerImage: pod.image,
		repository:      repository,
		digest:          digest,
		release:         release,
		workload:        workload,
		revision:        revision,
		script:          selfUpgradeRollbackScript,
		evidence:        args,
		phase:           "rollback",
	})
	if err != nil {
		return "", err
	}
	if err := p.applyJob(ctx, namespace, manifest); err != nil {
		return "", fmt.Errorf("%w job=%s namespace=%s", err, jobName, namespace)
	}
	return fmt.Sprintf(
		"self-upgrade-accepted job=%s namespace=%s release=%s workload=%s image=%s digest=%s expected_release_commit=%s upgrade_operation_id=%s phase=rollback",
		jobName, namespace, release, workload, rollbackImage, digest, commit, args.UpgradeOperationID,
	), nil
}

func (p upgradeProcess) rollbackImage(ctx context.Context, namespace, release, workload string) (string, int, string, error) {
	out, err := p.command(ctx, "", "helm", "history", release, "-n", namespace, "-o", "json")
	if err != nil {
		return "", 0, "", commandError("read rollback history", out, err)
	}
	var history []struct {
		Revision int    `json:"revision"`
		Status   string `json:"status"`
	}
	if err := json.Unmarshal(out, &history); err != nil {
		return "", 0, "", fmt.Errorf("category=rollback_evidence_invalid parse rollback history: %w", err)
	}
	out, err = p.command(ctx, "", "kubectl", "-n", namespace, "get", "configmap", "doops-upgrade-verified-"+release, "--ignore-not-found", "-o", "json")
	if err != nil {
		return "", 0, "", commandError("read verified rollback evidence", out, err)
	}
	var stored struct {
		Data map[string]string `json:"data"`
	}
	var evidence struct {
		SchemaVersion int    `json:"schemaVersion"`
		Release       string `json:"release"`
		Workload      string `json:"workload"`
		Revisions     map[string]struct {
			Image         string `json:"image"`
			ReleaseCommit string `json:"release_commit"`
			Outcome       string `json:"outcome"`
			OperationID   string `json:"upgrade_operation_id"`
			Job           string `json:"job"`
			Phase         string `json:"phase"`
			Stage         string `json:"verification_stage"`
		} `json:"revisions"`
	}
	if json.Unmarshal(out, &stored) != nil || json.Unmarshal([]byte(stored.Data["evidence.json"]), &evidence) != nil ||
		evidence.SchemaVersion != 1 || evidence.Release != release || evidence.Workload != workload {
		return "", 0, "", fmt.Errorf("category=rollback_evidence_invalid verified rollback evidence is missing or invalid; use the authorized external controller")
	}
	current, previous := 0, 0
	for _, row := range history {
		if row.Status == "deployed" && row.Revision > current {
			current = row.Revision
		}
	}
	for _, row := range history {
		entry := evidence.Revisions[fmt.Sprint(row.Revision)]
		if row.Status != "superseded" || row.Revision >= current || row.Revision <= previous || entry.Image == "" {
			continue
		}
		// Upgrade candidates may have live verification before the controller
		// commits its terminal outcome. A failed apply cannot certify that revision.
		if entry.Phase == "apply" && entry.Stage != "baseline" && entry.Stage != "rollback" {
			if entry.OperationID == "" || entry.Job != selfUpgradeJobName(entry.OperationID, "apply") {
				continue
			}
			receipt, err := p.readUpgradeReceipt(ctx, namespace, entry.Job, entry.OperationID)
			if err != nil || receipt["outcome"] != "succeeded" || receipt["release"] != release || receipt["workload"] != workload ||
				receipt["revision"] != row.Revision || receipt["verified_image"] != entry.Image || receipt["release_commit"] != entry.ReleaseCommit {
				continue
			}
		}
		previous = row.Revision
	}
	if previous == 0 {
		return "", 0, "", fmt.Errorf("category=rollback_evidence_invalid no previously verified superseded revision")
	}
	verified := evidence.Revisions[fmt.Sprint(previous)]
	if !buildrelease.ValidCommit(verified.ReleaseCommit) || verified.Outcome != "succeeded" {
		return "", 0, "", fmt.Errorf("category=rollback_commit_unknown historical revision %d has no verified release_commit", previous)
	}
	out, err = p.command(ctx, "", "helm", "get", "manifest", release, "-n", namespace, "--revision", fmt.Sprint(previous))
	if err != nil {
		return "", 0, "", commandError("read rollback manifest", out, err)
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(out)))
	var images []string
	for {
		var doc struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name      string `yaml:"name"`
				Namespace string `yaml:"namespace"`
			} `yaml:"metadata"`
			Spec struct {
				Template struct {
					Spec struct {
						Containers []struct {
							Name  string `yaml:"name"`
							Image string `yaml:"image"`
						} `yaml:"containers"`
					} `yaml:"spec"`
				} `yaml:"template"`
			} `yaml:"spec"`
		}
		if err := decoder.Decode(&doc); err == io.EOF {
			break
		} else if err != nil {
			return "", 0, "", fmt.Errorf("category=rollback_evidence_invalid parse rollback manifest: %w", err)
		}
		if doc.Kind != "Deployment" || doc.Metadata.Name != workload {
			continue
		}
		if doc.Metadata.Namespace != "" && doc.Metadata.Namespace != namespace {
			return "", 0, "", fmt.Errorf("category=rollback_evidence_invalid rollback workload namespace mismatch")
		}
		for _, container := range doc.Spec.Template.Spec.Containers {
			if container.Name == selfUpgradeContainer {
				images = append(images, container.Image)
			}
		}
	}
	if len(images) != 1 {
		return "", 0, "", fmt.Errorf("category=rollback_evidence_invalid rollback manifest must contain exactly one Agent image")
	}
	if _, _, err := parsePinnedImage(images[0]); err != nil {
		return "", 0, "", fmt.Errorf("category=rollback_evidence_invalid rollback requires an immutable historical image: %w", err)
	}
	if evidence.Revisions[fmt.Sprint(previous)].Image != images[0] {
		return "", 0, "", fmt.Errorf("category=rollback_evidence_invalid rollback manifest does not match verified evidence")
	}
	return images[0], previous, verified.ReleaseCommit, nil
}

func (p upgradeProcess) observeSelfUpgrade(ctx context.Context, args agentUpgradeParams) (string, error) {
	jobName := strings.TrimSpace(args.ObserveJob)
	if !selfUpgradeJobPattern.MatchString(jobName) {
		return "", fmt.Errorf("category=invalid_upgrade_request observe_job must name a doops self-upgrade job")
	}
	namespace, err := p.agentNamespace(args.Namespace)
	if err != nil {
		return "", err
	}
	job, found, err := p.readUpgradeJob(ctx, namespace, jobName)
	if err != nil {
		return "", err
	}
	if found && (job.Metadata.Annotations["doops.sh/upgrade-operation-id"] != args.UpgradeOperationID ||
		selfUpgradeJobName(args.UpgradeOperationID, job.Metadata.Annotations["doops.sh/phase"]) != jobName) {
		return "", fmt.Errorf("category=upgrade_operation_mismatch Job does not belong to upgrade_operation_id")
	}
	header := fmt.Sprintf("job=%s namespace=%s phase=observe upgrade_operation_id=%s", jobName, namespace, args.UpgradeOperationID)
	for _, field := range []string{"release", "image", "digest", "expected_release_commit", "phase"} {
		value := job.Metadata.Annotations["doops.sh/"+strings.ReplaceAll(field, "_", "-")]
		if field == "phase" {
			field = "job_phase"
		}
		if value != "" {
			header += " " + field + "=" + value
		}
	}
	if found && job.phase() == "running" {
		return fmt.Sprintf("self-upgrade-running %s", header), nil
	}
	receipt, receiptErr := p.readUpgradeReceipt(ctx, namespace, jobName, args.UpgradeOperationID)
	if receiptErr == nil {
		if found && receipt["outcome"] != job.phase() {
			return "", fmt.Errorf("category=upgrade_receipt_conflict Job and durable receipt disagree")
		}
		for _, key := range []string{"release", "workload", "image", "expected_release_commit"} {
			expected := job.Metadata.Annotations["doops.sh/"+strings.ReplaceAll(key, "_", "-")]
			if found && expected != "" && receipt[key] != expected {
				return "", fmt.Errorf("category=upgrade_receipt_conflict Job and receipt identity disagree")
			}
		}
		evidence, _ := json.Marshal(receipt)
		return fmt.Sprintf("self-upgrade-%s %s\n%s", receipt["outcome"], header, evidence), nil
	}
	if !found || job.phase() == "succeeded" {
		return "", receiptErr
	}
	// A deadline or killed controller may never have written a receipt. Preserve
	// that explicit evidence gap; arbitrary command/Pod output is not audit data.
	category := firstNonEmptyString(selfUpgradeFailureCategory(p.jobLogs(ctx, namespace, jobName)), "upgrade_job_failed")
	for _, condition := range job.Status.Conditions {
		if condition.Status == "True" && condition.Reason == "DeadlineExceeded" {
			category = "upgrade_job_deadline_exceeded"
		}
	}
	evidence, _ := json.Marshal(map[string]string{
		"category": category, "receipt_error": selfUpgradeFailureCategory(receiptErr.Error()),
	})
	return fmt.Sprintf("self-upgrade-failed %s category=%s\n%s", header, category, evidence), nil
}

func (p upgradeProcess) agentNamespace(requested string) (string, error) {
	namespace := strings.TrimSpace(p.namespace)
	if namespace == "" {
		return "", fmt.Errorf("self-upgrade requires the agent pod namespace")
	}
	if err := validateK8sName("namespace", namespace, 63); err != nil {
		return "", err
	}
	requested = strings.TrimSpace(requested)
	if requested != "" && requested != namespace {
		return "", fmt.Errorf("self-upgrade namespace must be the agent namespace %s", namespace)
	}
	return namespace, nil
}

func (p upgradeProcess) managedRelease(ctx context.Context, namespace, requested, workload string) (string, string, error) {
	if strings.TrimSpace(p.cluster) == "" || strings.TrimSpace(p.instance) == "" {
		return "", "", fmt.Errorf("self-upgrade requires DOOPS_GATEWAY_CLUSTER and DOOPS_GATEWAY_INSTANCE")
	}
	requested = strings.TrimSpace(requested)
	if requested != "" {
		if err := validateK8sName("release", requested, 53); err != nil {
			return "", "", err
		}
	}
	out, err := p.command(ctx, "", "helm", "list", "-n", namespace, "-o", "json")
	if err != nil {
		return "", "", commandError("list helm releases", out, err)
	}
	var listed []struct {
		Name  string `json:"name"`
		Chart string `json:"chart"`
	}
	if err := json.Unmarshal(out, &listed); err != nil {
		return "", "", fmt.Errorf("parse helm releases: %w", err)
	}
	matches := make([]string, 0, 1)
	for _, release := range listed {
		chart := strings.TrimSpace(release.Chart)
		if chart != selfUpgradeChartPrefix && !strings.HasPrefix(chart, selfUpgradeChartPrefix+"-") {
			continue
		}
		if requested != "" && release.Name != requested {
			continue
		}
		matches = append(matches, release.Name)
	}
	if len(matches) == 0 {
		return "", "", errNoManagedRelease
	}
	if len(matches) != 1 {
		return "", "", fmt.Errorf("expected exactly one managed doops-agent Helm release in %s, found %d", namespace, len(matches))
	}
	release := matches[0]
	if err := validateK8sName("release", release, 53); err != nil {
		return "", "", err
	}
	valuesOut, err := p.command(ctx, "", "helm", "get", "values", release, "-n", namespace, "-o", "json")
	if err != nil {
		return "", "", commandError("read helm values", valuesOut, err)
	}
	var values struct {
		Gateway struct {
			Cluster  string `json:"cluster"`
			Instance string `json:"instance"`
		} `json:"gateway"`
		Workload struct {
			Name string `json:"name"`
		} `json:"workload"`
	}
	if err := json.Unmarshal(valuesOut, &values); err != nil {
		return "", "", fmt.Errorf("parse helm values: %w", err)
	}
	if values.Gateway.Cluster != p.cluster || values.Gateway.Instance != p.instance {
		return "", "", fmt.Errorf("helm release %s identity does not match this agent", release)
	}
	liveWorkload := strings.TrimSpace(values.Workload.Name)
	if err := validateK8sName("workload", liveWorkload, 63); err != nil {
		return "", "", err
	}
	if err := workloadMatches(workload, liveWorkload); err != nil {
		return "", "", err
	}
	return release, liveWorkload, nil
}

func workloadMatches(requested, live string) error {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return nil
	}
	requested = strings.TrimPrefix(requested, "deployment/")
	if strings.Contains(requested, "/") {
		return fmt.Errorf("self-upgrade supports the managed Helm Deployment only")
	}
	if requested != live {
		return fmt.Errorf("workload %s does not match the Helm release workload %s", requested, live)
	}
	return nil
}

type selfUpgradePod struct {
	image              string
	serviceAccountName string
	runtimeClassName   string
	nodeName           string
	imagePullSecrets   []string
	mount              jobVolumeMount
	volume             json.RawMessage
}

func (p upgradeProcess) podSpec(ctx context.Context, namespace string) (selfUpgradePod, error) {
	if strings.TrimSpace(p.hostname) == "" {
		return selfUpgradePod{}, fmt.Errorf("self-upgrade requires the agent pod name")
	}
	if err := validateK8sName("pod", p.hostname, 253); err != nil {
		return selfUpgradePod{}, err
	}
	out, err := p.command(ctx, "", "kubectl", "-n", namespace, "get", "pod", p.hostname, "-o", "json")
	if err != nil {
		return selfUpgradePod{}, commandError("read agent pod", out, err)
	}
	var pod struct {
		Spec struct {
			NodeName           string `json:"nodeName"`
			ServiceAccountName string `json:"serviceAccountName"`
			RuntimeClassName   string `json:"runtimeClassName"`
			ImagePullSecrets   []struct {
				Name string `json:"name"`
			} `json:"imagePullSecrets"`
			Containers []struct {
				Name         string `json:"name"`
				Image        string `json:"image"`
				VolumeMounts []struct {
					Name      string `json:"name"`
					MountPath string `json:"mountPath"`
					SubPath   string `json:"subPath"`
					ReadOnly  bool   `json:"readOnly"`
				} `json:"volumeMounts"`
			} `json:"containers"`
			Volumes []json.RawMessage `json:"volumes"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(out, &pod); err != nil {
		return selfUpgradePod{}, fmt.Errorf("parse agent pod: %w", err)
	}
	var containerImage string
	var selectedMount jobVolumeMount
	foundMount := false
	for _, container := range pod.Spec.Containers {
		if container.Name != selfUpgradeContainer {
			continue
		}
		containerImage = container.Image
		for _, mount := range container.VolumeMounts {
			if mount.MountPath != "/root/.kube" && mount.MountPath != "/root/.kube/config" {
				continue
			}
			selectedMount = jobVolumeMount{
				Name:      mount.Name,
				MountPath: mount.MountPath,
				SubPath:   mount.SubPath,
				ReadOnly:  mount.ReadOnly,
			}
			foundMount = true
			if mount.MountPath == "/root/.kube/config" {
				break
			}
		}
	}
	if containerImage == "" {
		return selfUpgradePod{}, fmt.Errorf("agent pod has no %s container", selfUpgradeContainer)
	}
	var volume json.RawMessage
	if foundMount {
		volume, err = volumeByName(pod.Spec.Volumes, selectedMount.Name)
		if err != nil {
			return selfUpgradePod{}, err
		}
		if _, err = nodeNameForVolume(volume, pod.Spec.NodeName); err != nil {
			return selfUpgradePod{}, err
		}
	} else if sa := strings.TrimSpace(pod.Spec.ServiceAccountName); sa == "" || sa == "default" {
		// Without a kubeconfig volume the Job authenticates as the Agent's own
		// ServiceAccount; the namespace default account carries no Agent authority.
		return selfUpgradePod{}, fmt.Errorf("agent pod has no /root/.kube volume and no dedicated ServiceAccount")
	}
	if strings.TrimSpace(pod.Spec.NodeName) == "" {
		return selfUpgradePod{}, fmt.Errorf("self-upgrade requires the agent pod node name")
	}
	secrets := make([]string, 0, len(pod.Spec.ImagePullSecrets))
	for _, secret := range pod.Spec.ImagePullSecrets {
		if strings.TrimSpace(secret.Name) != "" {
			secrets = append(secrets, secret.Name)
		}
	}
	return selfUpgradePod{
		image:              containerImage,
		serviceAccountName: pod.Spec.ServiceAccountName,
		runtimeClassName:   pod.Spec.RuntimeClassName,
		nodeName:           pod.Spec.NodeName,
		imagePullSecrets:   secrets,
		mount:              selectedMount,
		volume:             volume,
	}, nil
}

func volumeByName(volumes []json.RawMessage, name string) (json.RawMessage, error) {
	for _, raw := range volumes {
		var meta struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &meta); err != nil {
			return nil, fmt.Errorf("parse pod volume: %w", err)
		}
		if meta.Name == name {
			return raw, nil
		}
	}
	return nil, fmt.Errorf("pod volume %s was not found", name)
}

func nodeNameForVolume(volume json.RawMessage, nodeName string) (string, error) {
	var kinds map[string]json.RawMessage
	if err := json.Unmarshal(volume, &kinds); err != nil {
		return "", fmt.Errorf("parse kubeconfig volume: %w", err)
	}
	if _, ok := kinds["emptyDir"]; ok {
		return "", fmt.Errorf("kubeconfig volume cannot be shared with the upgrade Job")
	}
	if _, ok := kinds["hostPath"]; ok {
		if strings.TrimSpace(nodeName) == "" {
			return "", fmt.Errorf("hostPath kubeconfig requires the agent pod node name")
		}
		return nodeName, nil
	}
	return "", nil
}

func (p upgradeProcess) requireNoActiveUpgrade(ctx context.Context, namespace string) error {
	active, err := p.activeSelfUpgrade(ctx, namespace)
	if err != nil {
		return err
	}
	if active != "" {
		return fmt.Errorf("category=upgrade_busy self-upgrade job %s is already running", active)
	}
	return nil
}

func (p upgradeProcess) activeSelfUpgrade(ctx context.Context, namespace string) (string, error) {
	out, err := p.command(ctx, "", "kubectl", "-n", namespace, "get", "jobs", "-l", selfUpgradeLabelKey+"="+selfUpgradeLabelValue, "-o", "json")
	if err != nil {
		return "", commandError("list self-upgrade jobs", out, err)
	}
	var listed struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Status selfUpgradeJobStatus `json:"status"`
		} `json:"items"`
	}
	if len(strings.TrimSpace(string(out))) == 0 {
		return "", nil
	}
	if err := json.Unmarshal(out, &listed); err != nil {
		return "", fmt.Errorf("parse self-upgrade jobs: %w", err)
	}
	for _, item := range listed.Items {
		if (&selfUpgradeJob{Status: item.Status}).phase() == "running" {
			return item.Metadata.Name, nil
		}
	}
	return "", nil
}

type selfUpgradeJobStatus struct {
	Active     int `json:"active"`
	Succeeded  int `json:"succeeded"`
	Failed     int `json:"failed"`
	Conditions []struct {
		Type   string `json:"type"`
		Status string `json:"status"`
		Reason string `json:"reason"`
	} `json:"conditions"`
}

type selfUpgradeJob struct {
	Metadata struct {
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
	Status selfUpgradeJobStatus `json:"status"`
}

func (job *selfUpgradeJob) phase() string {
	for _, condition := range job.Status.Conditions {
		if condition.Status == "True" {
			if condition.Type == "Failed" {
				return "failed"
			}
			if condition.Type == "Complete" {
				return "succeeded"
			}
		}
	}
	switch {
	case job.Status.Succeeded > 0:
		return "succeeded"
	case job.Status.Failed > 0:
		return "failed"
	default:
		return "running"
	}
}

func (p upgradeProcess) readUpgradeJob(ctx context.Context, namespace, jobName string) (selfUpgradeJob, bool, error) {
	var job selfUpgradeJob
	out, err := p.command(ctx, "", "kubectl", "-n", namespace, "get", "job", jobName, "-o", "json")
	if err != nil {
		if isNotFound(out, err) {
			return job, false, nil
		}
		return job, false, commandError("read self-upgrade job", out, err)
	}
	if err := json.Unmarshal(out, &job); err != nil {
		return job, false, fmt.Errorf("category=upgrade_job_invalid parse self-upgrade job: %w", err)
	}
	return job, true, nil
}

func (p upgradeProcess) jobLogs(ctx context.Context, namespace, jobName string) string {
	out, err := p.command(ctx, "", "kubectl", "-n", namespace, "logs", "job/"+jobName, "--tail=80")
	text := strings.TrimSpace(string(out))
	if err != nil && text == "" {
		text = strings.TrimSpace(err.Error())
	}
	return tailText(text, 4000)
}

func (p upgradeProcess) applyJob(ctx context.Context, namespace string, manifest []byte) error {
	out, err := p.command(ctx, string(manifest), "kubectl", "-n", namespace, "create", "-f", "-")
	if err != nil {
		if strings.Contains(strings.ToLower(string(out)), "alreadyexists") || strings.Contains(strings.ToLower(string(out)), "already exists") {
			return fmt.Errorf("category=upgrade_job_exists this operation phase already has a Job; observe the existing Job")
		}
		return fmt.Errorf("category=upgrade_job_create_unknown %w", commandError("create self-upgrade job", out, err))
	}
	return nil
}

type selfUpgradePlan struct {
	image           string
	controllerImage string
	repository      string
	tag             string
	digest          string
	release         string
	workload        string
	revision        int
	script          string
	cluster         string
	instance        string
	preflight       bool
	evidence        agentUpgradeParams
	phase           string
	// allowMissingRollbackBaseline lets the operator accept an upgrade whose
	// rollback baseline image can no longer be pulled.
	allowMissingRollbackBaseline bool
}

type jobVolumeMount struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
	SubPath   string `json:"subPath,omitempty"`
	ReadOnly  bool   `json:"readOnly"`
}

func selfUpgradeJobManifest(jobName, namespace string, pod selfUpgradePod, plan selfUpgradePlan) ([]byte, error) {
	if strings.TrimSpace(pod.image) == "" && plan.image == "" {
		return nil, fmt.Errorf("self-upgrade job image is required")
	}
	image := firstNonEmptyString(plan.controllerImage, plan.image)
	if image == "" {
		image = pod.image
	}
	env := []map[string]string{
		{"name": "DOOPS_UPGRADE_NAMESPACE", "value": namespace},
		{"name": "DOOPS_UPGRADE_RELEASE", "value": plan.release},
		{"name": "DOOPS_UPGRADE_WORKLOAD", "value": plan.workload},
	}
	annotations := map[string]string{
		"doops.sh/release": plan.release, "doops.sh/workload": plan.workload,
		"doops.sh/image": plan.image, "doops.sh/digest": plan.digest,
		"doops.sh/upgrade-operation-id":    plan.evidence.UpgradeOperationID,
		"doops.sh/expected-release-commit": plan.evidence.ExpectedReleaseCommit,
		"doops.sh/source-commit":           plan.evidence.SourceCommit, "doops.sh/ci-url": plan.evidence.CIURL,
		"doops.sh/requester-id": plan.evidence.RequesterID, "doops.sh/audit-id": fmt.Sprint(plan.evidence.AuditID),
		"doops.sh/session-id": plan.evidence.SessionID, "doops.sh/phase": plan.phase,
		"doops.sh/created-at":                      time.Now().UTC().Format(time.RFC3339Nano),
		"doops.sh/job":                             jobName,
		"doops.sh/allow-missing-rollback-baseline": fmt.Sprint(plan.allowMissingRollbackBaseline),
	}
	for _, key := range []string{"upgrade-operation-id", "expected-release-commit", "source-commit", "ci-url",
		"requester-id", "audit-id", "session-id", "phase", "created-at", "job"} {
		name := strings.ToUpper(strings.ReplaceAll(key, "-", "_"))
		if key == "upgrade-operation-id" {
			name = "OPERATION_ID"
		}
		env = append(env, map[string]string{
			"name":  "DOOPS_UPGRADE_" + name,
			"value": annotations["doops.sh/"+key],
		})
	}
	if plan.allowMissingRollbackBaseline {
		env = append(env, map[string]string{"name": "DOOPS_UPGRADE_ALLOW_MISSING_ROLLBACK_BASELINE", "value": "1"})
	}
	if plan.revision != 0 {
		env = append(env, map[string]string{"name": "DOOPS_UPGRADE_REVISION", "value": fmt.Sprint(plan.revision)})
	}
	if plan.cluster != "" {
		env = append(env, map[string]string{"name": "DOOPS_GATEWAY_CLUSTER", "value": plan.cluster},
			map[string]string{"name": "DOOPS_GATEWAY_INSTANCE", "value": plan.instance})
	}
	if plan.repository != "" {
		env = append(env, map[string]string{"name": "DOOPS_UPGRADE_REPOSITORY", "value": plan.repository})
		if plan.tag != "" {
			env = append(env, map[string]string{"name": "DOOPS_UPGRADE_TAG", "value": plan.tag})
		}
		if plan.digest != "" {
			env = append(env, map[string]string{"name": "DOOPS_UPGRADE_DIGEST", "value": plan.digest})
		}
		env = append(env, map[string]string{"name": "DOOPS_UPGRADE_IMAGE", "value": plan.image})
	}
	pullPolicy := "Always"
	if strings.Contains(image, "@sha256:") {
		pullPolicy = "IfNotPresent"
	}
	pullSecrets := make([]map[string]string, 0, len(pod.imagePullSecrets))
	for _, name := range pod.imagePullSecrets {
		pullSecrets = append(pullSecrets, map[string]string{"name": name})
	}
	podSpec := map[string]interface{}{
		"restartPolicy": "Never",
		"containers": []map[string]interface{}{
			{
				"name":            "controller",
				"image":           image,
				"imagePullPolicy": pullPolicy,
				"command":         []string{"/bin/sh", "-c", plan.script},
				"env":             env,
				"securityContext": map[string]bool{"privileged": false},
			},
		},
	}
	container := podSpec["containers"].([]map[string]interface{})[0]
	if pod.volume != nil {
		container["volumeMounts"] = []jobVolumeMount{pod.mount}
		podSpec["volumes"] = []json.RawMessage{pod.volume}
	} else {
		env = append(env, map[string]string{"name": "DOOPS_UPGRADE_KUBECONFIG_SOURCE", "value": "serviceAccount"})
		container["env"] = env
	}
	if pod.serviceAccountName != "" {
		podSpec["serviceAccountName"] = pod.serviceAccountName
	}
	if pod.runtimeClassName != "" {
		podSpec["runtimeClassName"] = pod.runtimeClassName
	}
	if len(pullSecrets) > 0 {
		podSpec["imagePullSecrets"] = pullSecrets
	}
	if pod.nodeName != "" {
		podSpec["nodeName"] = pod.nodeName
	}
	manifest := map[string]interface{}{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"metadata": map[string]interface{}{
			"name":      jobName,
			"namespace": namespace,
			"labels": map[string]string{
				selfUpgradeLabelKey: selfUpgradeLabelValue,
			},
			"annotations": annotations,
		},
		"spec": map[string]interface{}{
			"backoffLimit":            0,
			"ttlSecondsAfterFinished": 7 * 24 * 60 * 60,
			"activeDeadlineSeconds":   1200,
			"template": map[string]interface{}{
				"metadata": map[string]interface{}{
					"annotations": annotations,
					"labels": map[string]string{
						selfUpgradeLabelKey: selfUpgradeLabelValue,
					},
				},
				"spec": podSpec,
			},
		},
	}
	if plan.preflight {
		// An unpullable image never fails the pod, it just waits; bound that wait.
		manifest["spec"].(map[string]interface{})["activeDeadlineSeconds"] = 600
	}
	return json.Marshal(manifest)
}

func parseUpgradeImage(image string) (string, string, string, error) {
	image = strings.TrimSpace(image)
	at := strings.LastIndex(image, "@sha256:")
	if at > 0 {
		repository := image[:at]
		digest := image[at+1:]
		if !imageDigestPattern.MatchString(digest) {
			return "", "", "", fmt.Errorf("agent upgrade image must be repository:tag or repository@sha256:<64 hex digits>")
		}
		if !imageRepositoryPattern.MatchString(repository) {
			return "", "", "", fmt.Errorf("agent upgrade image repository is invalid")
		}
		return repository, "", digest, nil
	}
	colon := strings.LastIndex(image, ":")
	slash := strings.LastIndex(image, "/")
	if colon <= slash {
		return "", "", "", fmt.Errorf("agent upgrade image must be repository:tag or repository@sha256:<64 hex digits>")
	}
	repository := image[:colon]
	tag := image[colon+1:]
	if !imageRepositoryPattern.MatchString(repository) {
		return "", "", "", fmt.Errorf("agent upgrade image repository is invalid")
	}
	if !imageTagPattern.MatchString(tag) {
		return "", "", "", fmt.Errorf("agent upgrade image tag is invalid")
	}
	return repository, tag, "", nil
}

func parsePinnedImage(image string) (string, string, error) {
	repository, _, digest, err := parseUpgradeImage(image)
	if err != nil {
		return "", "", err
	}
	if digest == "" {
		return "", "", fmt.Errorf("rollback requires an immutable historical image")
	}
	return repository, digest, nil
}

func selfUpgradeJobName(operationID, phase string) string {
	digest := sha256.Sum256([]byte(operationID))
	return "doops-self-upgrade-" + hex.EncodeToString(digest[:])[:24] + "-" + phase
}

func validateK8sName(kind, name string, max int) error {
	if len(name) == 0 || len(name) > max || !k8sNamePattern.MatchString(name) {
		return fmt.Errorf("invalid %s %q", kind, name)
	}
	return nil
}

func isNotFound(out []byte, err error) bool {
	text := strings.ToLower(string(out))
	if err != nil {
		text += " " + strings.ToLower(err.Error())
	}
	return strings.Contains(text, "notfound") || strings.Contains(text, "not found")
}

func commandError(action string, _ []byte, _ error) error {
	// Kubernetes/Helm diagnostics can echo submitted objects and credentials.
	return fmt.Errorf("%s failed", action)
}

func tailText(text string, limit int) string {
	text = strings.TrimSpace(text)
	if limit <= 0 {
		return text
	}
	return utf8Tail(text, limit)
}

const selfUpgradeEvidencePrelude = `set -eu
umask 077
failure_category=upgrade_preflight_failed
emit_upgrade_event() {
  python3 - "$1" "$2" <<'PY'
import json, os, sys
sys.path.insert(0, "/app")
from adopt_agent import evidence_context, evidence_exception_hook, write_operation_receipt
sys.excepthook = evidence_exception_hook
record = evidence_context(os.environ["DOOPS_UPGRADE_NAMESPACE"], os.environ["DOOPS_UPGRADE_WORKLOAD"],
                          os.environ["DOOPS_UPGRADE_RELEASE"], os.environ.get("DOOPS_UPGRADE_IMAGE", ""))
record.update(outcome=sys.argv[1], category=sys.argv[2])
if sys.argv[1] in ("succeeded", "failed"):
    record = write_operation_receipt(sys.argv[1], sys.argv[2])
print(json.dumps(record, sort_keys=True))
PY
}
finish_upgrade() {
  status="$1"
  trap - EXIT
  if [ -s "${TMPDIR:-/tmp}/doops-upgrade-terminal.json" ]; then exit "$status"; fi
  if [ "$status" -eq 0 ]; then
    emit_upgrade_event succeeded "" || exit 1
  else
    category_file="${TMPDIR:-/tmp}/doops-upgrade-category"
    if [ -s "$category_file" ]; then failure_category="$(cat "$category_file")"; fi
    echo "category=${failure_category}" >&2
    emit_upgrade_event failed "$failure_category" || exit 1
  fi
  exit "$status"
}
trap 'finish_upgrade $?' EXIT
emit_upgrade_event running ""
`

const selfUpgradeKubeconfigPrelude = selfUpgradeEvidencePrelude + `failure_category=kubeconfig_unavailable
sleep 5
source=/root/.kube/config
export KUBECONFIG=/tmp/doops-kubeconfig
if [ "${DOOPS_UPGRADE_KUBECONFIG_SOURCE:-mounted}" = "serviceAccount" ]; then
  python3 /app/configure_kubeconfig.py --service-account "$KUBECONFIG" "$KUBERNETES_SERVICE_HOST" "${KUBERNETES_SERVICE_PORT_HTTPS:-443}"
elif [ ! -f "$source" ]; then
  echo "kubeconfig volume is missing" >&2
  exit 1
elif [ -n "${KUBERNETES_SERVICE_HOST:-}" ]; then
  python3 /app/configure_kubeconfig.py "$source" "$KUBECONFIG" "$KUBERNETES_SERVICE_HOST" "${KUBERNETES_SERVICE_PORT_HTTPS:-443}"
else
  cp "$source" "$KUBECONFIG"
  chmod 600 "$KUBECONFIG"
fi
ns="$DOOPS_UPGRADE_NAMESPACE"
release="$DOOPS_UPGRADE_RELEASE"
workload="$DOOPS_UPGRADE_WORKLOAD"
failure_category=upgrade_preflight_failed
`

const selfUpgradeDeploymentVerifier = `verify_deployment() {
  python3 - "$ns" "$workload" "$1" "${2:-controller}" "${3:-}" <<'PY'
import json
import os
import re
import subprocess
import sys
sys.path.insert(0, "/app")
from adopt_agent import evidence_context, evidence_exception_hook, verified_release_commit, write_verified_receipt
sys.excepthook = evidence_exception_hook

namespace, expected_workload, expected_image, identity_source, record = sys.argv[1:6]
deployment = json.loads(subprocess.check_output(
    ["kubectl", "-n", namespace, "get", "deployment", expected_workload, "-o", "json"],
    timeout=30,
))

metadata = deployment.get("metadata") or {}
spec = deployment.get("spec") or {}
status = deployment.get("status") or {}
name = str(metadata.get("name") or "")
if name != expected_workload:
    raise SystemExit(
        f"Deployment identity changed: {name or '<unknown>'} != {expected_workload}"
    )

generation = int(metadata.get("generation") or 0)
observed_generation = int(status.get("observedGeneration") or 0)
if generation <= 0 or observed_generation != generation:
    raise SystemExit("Deployment controller has not observed the current generation")

desired = int(spec.get("replicas") if spec.get("replicas") is not None else 1)
if desired <= 0:
    raise SystemExit(f"Deployment desiredReplicas={desired} must be positive")
for field in ("updatedReplicas", "readyReplicas", "availableReplicas"):
    actual = int(status.get(field) or 0)
    if actual < desired:
        raise SystemExit(
            f"Deployment {field}={actual}, desiredReplicas={desired}"
        )
if int(status.get("replicas") or 0) < desired:
    raise SystemExit(
        f"Deployment replicas={int(status.get('replicas') or 0)}, desiredReplicas={desired}"
    )
if int(status.get("unavailableReplicas") or 0) != 0:
    raise SystemExit("Deployment still has unavailable replicas")

containers = ((spec.get("template") or {}).get("spec") or {}).get("containers") or []
actual_image = next(
    (container.get("image") for container in containers
     if container.get("name") == "doops-agent"),
    None,
)
if actual_image != expected_image:
    raise SystemExit(
        f"Deployment doops-agent image mismatch: {actual_image or '<none>'} != {expected_image}"
    )

selector = ((spec.get("selector") or {}).get("matchLabels") or {})
if not selector:
    raise SystemExit("Deployment selector.matchLabels is empty")
selector_text = ",".join(f"{key}={value}" for key, value in sorted(selector.items()))
pods = json.loads(subprocess.check_output(
    ["kubectl", "-n", namespace, "get", "pods", "-l", selector_text, "-o", "json"],
    timeout=30,
)).get("items") or []
pods = [pod for pod in pods if not (pod.get("metadata") or {}).get("deletionTimestamp")]
matching_pods = []
for pod in pods:
    containers = (pod.get("spec") or {}).get("containers") or []
    container = next((item for item in containers if item.get("name") == "doops-agent"), None)
    if not container or container.get("image") != expected_image:
        ready = any(
            condition.get("type") == "Ready" and condition.get("status") == "True"
            for condition in (pod.get("status") or {}).get("conditions") or []
        )
        if ready:
            raise SystemExit("Active Ready Agent Pod still uses a different image")
        continue
    matching_pods.append(pod)
if len(matching_pods) < desired:
    raise SystemExit(f"Active Agent Pods={len(matching_pods)}, desiredReplicas={desired}")
pods = matching_pods
# A digest-pinned reference names its content, so a Pod on any node can be
# checked against it. A tag only names what one node's runtime pulled.
pinned_digest = expected_image.rsplit("@", 1)[1] if "@sha256:" in expected_image else ""
if not re.fullmatch(r"sha256:[0-9a-f]{64}", pinned_digest):
    pinned_digest = ""
expected_digest = pinned_digest
controller_node = ""
runtime_class = ""
expected_commit = os.environ.get("DOOPS_UPGRADE_EXPECTED_RELEASE_COMMIT", "")
if identity_source == "baseline":
    baseline = json.loads(os.environ["DOOPS_UPGRADE_BASELINE"])
    if expected_image != baseline.get("image"):
        raise SystemExit("Rollback image does not match the captured baseline")
    expected_digest = baseline.get("digest") or ""
    controller_node = baseline.get("node") or ""
    runtime_class = baseline.get("runtime_class") or ""
    expected_commit = baseline.get("release_commit") or ""
    if not controller_node or not re.fullmatch(r"sha256:[0-9a-f]{64}", expected_digest):
        raise SystemExit("Rollback baseline runtime identity is missing")
elif identity_source == "controller":
    if expected_image != os.environ.get("DOOPS_UPGRADE_IMAGE"):
        raise SystemExit("Controller image does not match the requested image")
    controller = json.loads(subprocess.check_output(
        ["kubectl", "-n", namespace, "get", "pod", os.environ["HOSTNAME"], "-o", "json"],
        timeout=30,
    ))
    controller_spec = controller.get("spec") or {}
    controller_node = controller_spec.get("nodeName") or ""
    runtime_class = controller_spec.get("runtimeClassName") or ""
    pinned = next((item for item in controller_spec.get("containers") or []
                   if item.get("name") == "controller"), None)
    running = next((item for item in (controller.get("status") or {}).get("containerStatuses") or []
                    if item.get("name") == "controller"), None)
    if not controller_node or not pinned or pinned.get("image") != expected_image:
        raise SystemExit("Controller Pod does not match the requested image and node")
    if not running or not running.get("ready"):
        raise SystemExit("Controller container is not Ready")
    expected_digest = str(running.get("imageID") or "").split("://")[-1].rsplit("@", 1)[-1]
    if not re.fullmatch(r"sha256:[0-9a-f]{64}", expected_digest):
        raise SystemExit("Controller Pod imageID has no valid sha256 digest")
else:
    raise SystemExit("Unknown runtime identity source")
verified_pods = []
for pod in pods:
    metadata = pod.get("metadata") or {}
    uid = str(metadata.get("uid") or "")
    if not uid:
        raise SystemExit("Agent Pod is missing metadata.uid")
    status = pod.get("status") or {}
    if status.get("phase") != "Running" or not any(
        condition.get("type") == "Ready" and condition.get("status") == "True"
        for condition in status.get("conditions") or []
    ):
        raise SystemExit("Agent Pod is not Running and Ready")
    containers = (pod.get("spec") or {}).get("containers") or []
    container = next((item for item in containers if item.get("name") == "doops-agent"), None)
    if not container or container.get("image") != expected_image:
        raise SystemExit("Agent Pod spec image does not match the expected image")
    pod_node = (pod.get("spec") or {}).get("nodeName") or ""
    if not pod_node:
        raise SystemExit("Agent Pod is not bound to a node")
    same_node = pod_node == controller_node
    if not same_node and not pinned_digest:
        raise SystemExit("Agent and Controller Pods must use the same node runtime")
    if ((pod.get("spec") or {}).get("runtimeClassName") or "") != runtime_class:
        raise SystemExit("Agent and Controller Pods must use the same runtime class")
    statuses = status.get("containerStatuses") or []
    agent = next((item for item in statuses if item.get("name") == "doops-agent"), None)
    if not agent or not agent.get("ready"):
        raise SystemExit("Agent container is not Ready")
    image_id = str(agent.get("imageID") or "")
    actual_digest = image_id.split("://")[-1].rsplit("@", 1)[-1]
    if not re.fullmatch(r"sha256:[0-9a-f]{64}", actual_digest):
        raise SystemExit("Agent Pod imageID has no valid sha256 digest")
    if same_node:
        if expected_digest and actual_digest != expected_digest:
            raise SystemExit("Agent Pod imageID does not match the expected digest")
    elif actual_digest not in (pinned_digest, expected_digest):
        # Another node pulled the pinned reference itself. Its runtime may report
        # the pinned digest or the platform digest the controller saw; any other
        # digest is different content.
        raise SystemExit(
            "Agent Pod on node " + pod_node + " reports a digest that matches neither the pinned image"
            " nor the controller runtime on node " + controller_node
        )
    actual_commit = verified_release_commit(namespace, metadata["name"], expected_commit)
    verified_pods.append({"podUID": uid, "runtimeDigest": actual_digest, "node": pod_node,
                          "runtimeClass": runtime_class, "release_commit": actual_commit})
    print("pod_verified uid=" + uid + " imageID=" + image_id + " node=" + pod_node)
print("deployment_verified=" + name + " generation=" + str(generation) + " release_commit=" + actual_commit)
if record == "record":
    release = os.environ["DOOPS_UPGRADE_RELEASE"]
    evidence = evidence_context(namespace, expected_workload, release, expected_image)
    evidence.update(verified_pods[0], pods=verified_pods, outcome="succeeded", category="",
                    verification_stage=os.environ.get("DOOPS_UPGRADE_VERIFICATION_STAGE", identity_source),
                    expected_release_commit=expected_commit)
    write_verified_receipt(namespace, release, expected_workload, evidence)
PY
}
`

// The stable categories an upgrade preflight can report. They name why an
// upgrade was refused, so an operator reads the reason instead of guessing
// whether the Agent or the release is broken.
const (
	selfUpgradeCategoryRollbackBaselineUnavailable   = "rollback_baseline_image_unavailable"
	selfUpgradeCategoryRollbackBaselineUnresolved    = "rollback_baseline_image_unresolved"
	selfUpgradeCategoryRollbackBaselineProbeFailed   = "rollback_baseline_probe_failed"
	selfUpgradeCategoryStaleVolumeMountsUnresolved   = "stale_volume_mounts_unresolved"
	selfUpgradeCategoryRBACControllerIdentityChanged = "rbac_controller_identity_changed"
)

var selfUpgradeFailureCategories = []string{
	selfUpgradeCategoryRollbackBaselineUnavailable,
	selfUpgradeCategoryRollbackBaselineUnresolved,
	selfUpgradeCategoryRollbackBaselineProbeFailed,
	selfUpgradeCategoryStaleVolumeMountsUnresolved,
	selfUpgradeCategoryRBACControllerIdentityChanged,
}

// selfUpgradeFailureCategory reports the stable category a failed upgrade Job
// carried, or "" when the failure has no category. The category is part of the
// Job log, so it survives the Job, the observation and the CLI report.
func selfUpgradeFailureCategory(logs string) string {
	matches := upgradeCategoryPattern.FindAllStringSubmatch(logs, -1)
	if len(matches) > 0 {
		return matches[len(matches)-1][1]
	}
	for _, category := range selfUpgradeFailureCategories {
		if strings.Contains(logs, category) {
			return category
		}
	}
	return ""
}

// selfUpgradeRevisionImage prints the doops-agent image of one Helm revision.
// The pre-change checks and the rollback script both read the rollback target
// through this one helper, so the two can never select different images.
const selfUpgradeRevisionImage = `revision_image() {
  python3 - "$release" "$ns" "$1" "$workload" <<'PY'
import subprocess, sys, yaml
release, namespace, revision, workload = sys.argv[1:5]
manifest = subprocess.check_output(
    ["helm", "get", "manifest", release, "-n", namespace, "--revision", revision],
    timeout=30,
)
images = [
    container["image"]
    for doc in yaml.safe_load_all(manifest) if doc and doc.get("kind") == "Deployment"
    and (doc.get("metadata") or {}).get("name") == workload
    and (doc.get("metadata") or {}).get("namespace", namespace) == namespace
    for container in ((doc.get("spec") or {}).get("template") or {}).get("spec", {}).get("containers") or []
    if container.get("name") == "doops-agent"
]
if len(images) != 1 or not images[0]:
    raise SystemExit("rollback_baseline_image_unresolved: the revision must contain exactly one doops-agent image")
print(images[0])
PY
}
`

// selfUpgradeRollbackBaselineProbe proves that the image an automatic rollback
// would restore is still in its registry.
//
// It reads the baseline image manifest with skopeo, authenticated with the
// Agent Pod's own imagePullSecrets, which are the credentials the kubelet uses
// for the rollback Pod. An Agent Pod without pull secrets is checked
// anonymously, as the kubelet would pull it. No new credential dimension and no
// new mount: the Secret is read into a 0600 file that only skopeo reads, and the
// payload never reaches an argument or the Job log.
//
// It asks the registry instead of the kubelet on purpose. A kubelet pull waits
// behind every other pull on the node, so on a busy node it can take minutes
// and proves nothing more about whether the manifest is gone.
//
// A manifest that is gone from the registry is therefore caught before the
// release changes, which is what makes the third upgrade guarantee (restore the
// backup when verification fails) real rather than aspirational. A refused
// credential or an unreachable registry is not reported as a missing image.
//
// The operator can still proceed on purpose with
// DOOPS_UPGRADE_ALLOW_MISSING_ROLLBACK_BASELINE=1; the default is fail closed.
const selfUpgradeRollbackBaselineProbe = `rollback_baseline_probe="$(python3 - "$ns" "$rollback_baseline_image" "$previous" "$baseline" <<'PY' 2>&1
import base64, json, os, subprocess, sys, tempfile

namespace, image, revision, baseline_json = sys.argv[1:5]
baseline = json.loads(baseline_json)
CATEGORY_UNAVAILABLE = "rollback_baseline_image_unavailable"
CATEGORY_PROBE_FAILED = "rollback_baseline_probe_failed"
# A refused credential is checked first: some registries answer a refused
# request for a repository with "not found", and that is not a missing image.
REFUSED_MARKERS = ("unauthorized", "denied", "forbidden", "authentication required")
MISSING_MARKERS = ("manifest unknown", "name unknown", "not found")


def fail(category, reason):
    raise SystemExit(category + ": image=" + image + " reason=" + reason)


def pull_secret_auths(name):
    read = subprocess.run(["kubectl", "-n", namespace, "get", "secret", name, "-o", "json"],
                          capture_output=True, timeout=30)
    if read.returncode != 0:
        fail(CATEGORY_PROBE_FAILED, "the pull secret " + name + " could not be read")
    data = (json.loads(read.stdout).get("data") or {})
    try:
        if data.get(".dockerconfigjson"):
            return json.loads(base64.b64decode(data[".dockerconfigjson"])).get("auths") or {}
        if data.get(".dockercfg"):
            return json.loads(base64.b64decode(data[".dockercfg"]))
    except (ValueError, TypeError, AttributeError):
        pass
    fail(CATEGORY_PROBE_FAILED, "the pull secret " + name + " is not a registry credential")


auths = {}
for ref in baseline.get("image_pull_secrets") or []:
    for server, entry in pull_secret_auths(ref.get("name") or "").items():
        auths.setdefault(server, entry)

command = ["skopeo", "inspect", "--raw", "--retry-times", "2"]
authfile = ""
try:
    if auths:
        fd, authfile = tempfile.mkstemp(prefix="doops-rollback-baseline-", suffix=".json")
        with os.fdopen(fd, "w", encoding="utf-8") as fh:
            json.dump({"auths": auths}, fh)
        command += ["--authfile", authfile]
    else:
        command.append("--no-creds")
    try:
        checked = subprocess.run(command + ["docker://" + image], capture_output=True, timeout=120)
    except subprocess.TimeoutExpired:
        fail(CATEGORY_PROBE_FAILED, "the registry did not answer in time")
    except OSError:
        fail(CATEGORY_PROBE_FAILED, "skopeo is not available in the upgrade image")
finally:
    if authfile:
        os.unlink(authfile)

if checked.returncode == 0:
    print("rollback_baseline_image_available image=" + image)
    raise SystemExit(0)
detail = checked.stderr.decode("utf-8", "replace").strip()
lowered = detail.lower()
if any(marker in lowered for marker in REFUSED_MARKERS):
    fail(CATEGORY_PROBE_FAILED, "the registry refused the pull credentials")
if any(marker in lowered for marker in MISSING_MARKERS):
    fail(CATEGORY_UNAVAILABLE, "the registry has no manifest for this image")
fail(CATEGORY_PROBE_FAILED, "the registry manifest check failed")
PY
)" || {
  case "$rollback_baseline_probe" in
    *rollback_baseline_image_unavailable*)
      if [ "${DOOPS_UPGRADE_ALLOW_MISSING_ROLLBACK_BASELINE:-0}" != "1" ]; then
        failure_category=rollback_baseline_image_unavailable
        echo "$rollback_baseline_probe" >&2
        exit 1
      fi
      echo "warning: $rollback_baseline_probe; the operator allowed this upgrade without a pullable rollback baseline" >&2
      ;;
    *)
      echo "rollback_baseline_probe_failed: $rollback_baseline_probe" >&2
      exit 1
      ;;
  esac
}
`

// selfUpgradeApplyChecks holds every check that runs before the first change, so
// the preflight Job and the apply Job cannot disagree about what is admissible.
const selfUpgradeApplyChecks = selfUpgradeRevisionImage + selfUpgradeRBACGuard + `repository="$DOOPS_UPGRADE_REPOSITORY"
failure_category=rollback_baseline_image_unresolved
tag="${DOOPS_UPGRADE_TAG:-}"
digest="${DOOPS_UPGRADE_DIGEST:-}"
target="$DOOPS_UPGRADE_IMAGE"
chart=` + selfUpgradeChartPath + `
live_deployment="${TMPDIR:-/tmp}/doops-upgrade-live-deployment.json"
previous_image="$(python3 - "$ns" "$workload" "$live_deployment" <<'PY'
import json, subprocess, sys
ns, workload, live_path = sys.argv[1:4]
raw = subprocess.check_output(["kubectl", "-n", ns, "get", "deployment", workload, "-o", "json"])
with open(live_path, "wb") as fh:
    fh.write(raw)
doc = json.loads(raw)
containers = ((doc.get("spec") or {}).get("template") or {}).get("spec", {}).get("containers") or []
actual_image = next(
    (container.get("image") for container in containers
     if container.get("name") == "doops-agent"),
    None,
)
if not actual_image:
    raise SystemExit("deployment must contain a doops-agent container")
print(actual_image)
PY
)"
previous="$(helm history "$release" -n "$ns" -o json | python3 -c 'import json,sys; rows=json.load(sys.stdin); deployed=[int(row["revision"]) for row in rows if row["status"]=="deployed"]; print(max(deployed) if len(deployed)==1 else "")')"
test -n "$previous" || { echo "rollback_baseline_image_unresolved: the release must have exactly one deployed revision" >&2; exit 1; }
selector_json="$(kubectl -n "$ns" get deployment "$workload" -o json | python3 -c 'import json,sys; d=json.load(sys.stdin); print(json.dumps(d["spec"]["selector"]["matchLabels"], separators=(",", ":"), sort_keys=True))')"
baseline="$(python3 - "$ns" "$previous_image" "$selector_json" <<'PY'
import json, re, subprocess, sys
sys.path.insert(0, "/app")
from adopt_agent import evidence_exception_hook, verified_release_commit
sys.excepthook = evidence_exception_hook
ns, image, labels = sys.argv[1:4]
selector = ",".join(f"{key}={value}" for key, value in sorted(json.loads(labels).items()))
pods = json.loads(subprocess.check_output(
    ["kubectl", "-n", ns, "get", "pods", "-l", selector, "-o", "json"], timeout=30,
)).get("items") or []
pods = [pod for pod in pods if not (pod.get("metadata") or {}).get("deletionTimestamp")]
if len(pods) != 1:
    raise SystemExit("Rollback baseline requires one active Agent Pod")
pod = pods[0]
spec = pod.get("spec") or {}
container = next((item for item in spec.get("containers") or [] if item.get("name") == "doops-agent"), None)
status = next((item for item in (pod.get("status") or {}).get("containerStatuses") or [] if item.get("name") == "doops-agent"), None)
if not (pod.get("metadata") or {}).get("uid") or not spec.get("nodeName") or not container or container.get("image") != image or not status or not status.get("ready"):
    raise SystemExit("Rollback baseline Pod identity is not verified")
digest = str(status.get("imageID") or "").split("://")[-1].rsplit("@", 1)[-1]
if not re.fullmatch(r"sha256:[0-9a-f]{64}", digest):
    raise SystemExit("Rollback baseline runtime digest is invalid")
commit = verified_release_commit(ns, pod["metadata"]["name"])
print(json.dumps({"image": image, "digest": digest, "node": spec["nodeName"],
                 "release_commit": commit,
                 "runtime_class": spec.get("runtimeClassName") or "",
                 "service_account": spec.get("serviceAccountName") or "",
                 "tolerations": spec.get("tolerations") or [],
                 "node_selector": spec.get("nodeSelector") or {},
                 "image_pull_secrets": spec.get("imagePullSecrets") or []}))
PY
)"
export DOOPS_UPGRADE_BASELINE="$baseline"
failure_category=rollback_baseline_verification_failed
verify_deployment "$previous_image" baseline ${verify_record-record}
failure_category=rollback_baseline_image_unresolved
rollback_baseline_image="$(revision_image "$previous")"
test "$rollback_baseline_image" = "$previous_image" || {
  echo "rollback_baseline_image_unresolved: the rollback revision image differs from the live Agent image" >&2
  exit 1
}
failure_category=rollback_baseline_probe_failed
` + selfUpgradeRollbackBaselineProbe + `failure_category=upgrade_values_failed
values="${TMPDIR:-/tmp}/doops-upgrade-values.json"
helm get values "$release" -n "$ns" -o json >"$values"
python3 - "$values" "$repository" "$tag" "$digest" "$selector_json" <<'PY'
import json, sys
path, repository, tag, digest, selector_json = sys.argv[1:6]
with open(path, encoding="utf-8") as fh:
    values = json.load(fh)
image = values.setdefault("image", {})
image.pop("tag", None)
image["repository"] = repository
image.pop("digest", None)
if digest:
    image["digest"] = digest
else:
    image["tag"] = tag
values.setdefault("workload", {})["selectorLabels"] = json.loads(selector_json)
with open(path, "w", encoding="utf-8") as fh:
    json.dump(values, fh)
PY
failure_category=stale_volume_mounts_unresolved
` + selfUpgradeStaleVolumeMounts + `failure_category=rbac_controller_identity_changed
verify_controller_rbac "$previous_manifest" "$rendered_manifest"
`

// A ServiceAccount controller cannot let Helm add, remove or rebind its own
// identity: a failed upgrade's rollback could delete the newly managed account
// or binding, and a rollback across that adoption point could do the same.
// Those transitions need a controller with an independent Kubernetes identity.
// A mounted kubeconfig may also authenticate as a ServiceAccount, so the API
// server's effective identity, not the configuration source, selects the check.
const selfUpgradeRBACGuard = `verify_controller_rbac() {
  python3 - "$ns" "$1" "$2" <<'PY'
import json, os, subprocess, sys, yaml

namespace, before_path, after_path = sys.argv[1:4]


def refuse(reason):
    raise SystemExit("rbac_controller_identity_changed: " + reason + "; use the authorized external controller")


def identities(path, account_namespace, account):
    result = {}
    with open(path, encoding="utf-8") as fh:
        for doc in yaml.safe_load_all(fh):
            if not doc:
                continue
            kind = doc.get("kind")
            metadata = doc.get("metadata") or {}
            name = metadata.get("name")
            resource_namespace = "" if kind == "ClusterRoleBinding" else metadata.get("namespace", namespace)
            identity = None
            if kind == "ServiceAccount" and name == account and resource_namespace == account_namespace:
                identity = {}
            elif kind in ("RoleBinding", "ClusterRoleBinding"):
                subjects = doc.get("subjects") or []
                if any(subject.get("kind") == "ServiceAccount" and subject.get("name") == account
                       and subject.get("namespace", resource_namespace) == account_namespace for subject in subjects):
                    identity = {"roleRef": doc.get("roleRef"),
                                "subjects": sorted(json.dumps(subject, sort_keys=True) for subject in subjects)}
            if identity is not None:
                key = (kind, resource_namespace, name)
                if key in result:
                    refuse("duplicate controller RBAC resource in the Helm manifest")
                result[key] = identity
    return result


try:
    authentication = json.loads(subprocess.check_output(
        ["kubectl", "auth", "whoami", "-o", "json"], timeout=30,
    ))
    username = ((authentication.get("status") or {}).get("userInfo") or {}).get("username")
    if not isinstance(username, str) or not username:
        refuse("Kubernetes did not return the controller authentication identity")
    source_is_account = os.environ.get("DOOPS_UPGRADE_KUBECONFIG_SOURCE") == "serviceAccount"
    if not username.startswith("system:serviceaccount:"):
        if source_is_account:
            refuse("the controller authentication does not match its ServiceAccount source")
        print("controller_rbac=independent")
        raise SystemExit(0)
    identity = username.split(":")
    if len(identity) != 4 or not identity[2] or not identity[3]:
        refuse("Kubernetes returned an invalid ServiceAccount identity")
    account_namespace, account = identity[2:4]
    if source_is_account:
        pod = json.loads(subprocess.check_output(
            ["kubectl", "-n", namespace, "get", "pod", os.environ["HOSTNAME"], "-o", "json"], timeout=30,
        ))
        if account_namespace != namespace or (pod.get("spec") or {}).get("serviceAccountName") != account:
            refuse("the controller Pod ServiceAccount does not match its authentication identity")
    if identities(before_path, account_namespace, account) != identities(after_path, account_namespace, account):
        refuse("Helm would change the controller ServiceAccount or its RBAC bindings")
except (OSError, subprocess.SubprocessError, ValueError, yaml.YAMLError) as exc:
    refuse("cannot verify controller RBAC: " + str(exc))
print("controller_rbac=unchanged")
PY
}
`

// selfUpgradeStaleVolumeMounts finds the volume mounts the Helm upgrade would
// leave pointing at a volume it deletes.
//
// Helm removes only what its own previous revision rendered. A volume the
// previous revision rendered and the new chart drops is deleted, but a mount of
// that volume added outside Helm (for example by the kubectl apply that first
// created the Agent) is not Helm's to remove. The API server then rejects the
// Deployment, because a mount names a volume that no longer exists.
//
// Only mounts of exactly those volumes are stale: a volume the previous
// revision never rendered is not Helm's either, so Helm keeps it and its mount
// stays valid. The check reads the Deployment snapshot the other checks
// already took and changes nothing; the apply Job removes the mounts it lists,
// after backing them up, and restores them if the upgrade is rolled back.
const selfUpgradeStaleVolumeMounts = `rendered_manifest="${TMPDIR:-/tmp}/doops-upgrade-rendered.yaml"
previous_manifest="${TMPDIR:-/tmp}/doops-upgrade-previous-manifest.yaml"
stale_mounts="${TMPDIR:-/tmp}/doops-upgrade-stale-mounts.json"
remove_mounts_patch="${TMPDIR:-/tmp}/doops-upgrade-remove-mounts.json"
mount_backup="${TMPDIR:-/tmp}/doops-upgrade-mount-backup.json"
helm template "$release" "$chart" -n "$ns" -f "$values" >"$rendered_manifest"
helm get manifest "$release" -n "$ns" --revision "$previous" >"$previous_manifest"
python3 - "$ns" "$release" "$workload" "$previous" "$rendered_manifest" "$previous_manifest" "$live_deployment" "$stale_mounts" "$remove_mounts_patch" "$mount_backup" <<'PY'
import json, os, sys, yaml

(namespace, release, workload, revision, rendered_path, previous_path, live_path,
 stale_path, patch_path, backup_path) = sys.argv[1:11]


def deployment_volumes(path, label):
    with open(path, encoding="utf-8") as fh:
        docs = [doc for doc in yaml.safe_load_all(fh) if doc and doc.get("kind") == "Deployment"
                and (doc.get("metadata") or {}).get("name") == workload]
    if len(docs) != 1:
        raise SystemExit("stale_volume_mounts_unresolved: the " + label + " must render exactly one " + workload + " Deployment")
    pod = ((docs[0].get("spec") or {}).get("template") or {}).get("spec") or {}
    return {volume.get("name") for volume in pod.get("volumes") or []}


deleted = deployment_volumes(previous_path, "previous revision") - deployment_volumes(rendered_path, "new chart")
with open(live_path, encoding="utf-8") as fh:
    live_pod = ((json.load(fh).get("spec") or {}).get("template") or {}).get("spec") or {}

stale = []
for group in ("initContainers", "containers"):
    for container_index, container in enumerate(live_pod.get(group) or []):
        for mount_index, mount in enumerate(container.get("volumeMounts") or []):
            if mount.get("name") in deleted:
                stale.append({"group": group, "container": container.get("name"),
                              "containerIndex": container_index, "mountIndex": mount_index, "mount": mount})

for path in (stale_path, patch_path, backup_path):
    if os.path.exists(path):
        os.unlink(path)
if not stale:
    print("stale_volume_mounts=none")
    raise SystemExit(0)

# Each removal is guarded by a test of the container and mount it expects, so a
# Deployment that changed since the snapshot rejects the whole patch instead of
# losing a different mount. Removals run from the highest index down so earlier
# indices stay valid.
ops = []
for item in stale:
    base = "/spec/template/spec/%s/%d" % (item["group"], item["containerIndex"])
    ops.append({"op": "test", "path": base + "/name", "value": item["container"]})
    ops.append({"op": "test", "path": "%s/volumeMounts/%d/name" % (base, item["mountIndex"]),
                "value": item["mount"].get("name")})
for item in sorted(stale, key=lambda item: (item["group"], item["containerIndex"], -item["mountIndex"])):
    ops.append({"op": "remove", "path": "/spec/template/spec/%s/%d/volumeMounts/%d"
                % (item["group"], item["containerIndex"], item["mountIndex"])})

with open(stale_path, "w", encoding="utf-8") as fh:
    json.dump(stale, fh)
with open(patch_path, "w", encoding="utf-8") as fh:
    json.dump(ops, fh, separators=(",", ":"))
with open(backup_path, "w", encoding="utf-8") as fh:
    json.dump({"apiVersion": "v1", "kind": "ConfigMap",
               "metadata": {"name": "doops-upgrade-mount-backup-" + release, "namespace": namespace},
               "data": {"workload": workload, "revision": revision,
                        "volumeMounts.json": json.dumps(stale, sort_keys=True)}}, fh)
print("stale_volume_mounts=" + ",".join(
    "%s:%s:%s" % (item["container"], item["mount"].get("name"), item["mount"].get("mountPath", ""))
    for item in stale))
PY
`

// selfUpgradeRestoreVolumeMounts puts back the stale mounts the apply Job
// removed, once Helm has rolled the release back and the volume exists again.
// It reads the live Deployment and adds only the mounts that are missing, so it
// is safe to run whether or not the removal itself went through.
const selfUpgradeRestoreVolumeMounts = `restore_volume_mounts() {
  python3 - "$ns" "$workload" "$stale_mounts" <<'PY'
import json, subprocess, sys

namespace, workload, stale_path = sys.argv[1:4]
with open(stale_path, encoding="utf-8") as fh:
    stale = json.load(fh)
live = json.loads(subprocess.check_output(
    ["kubectl", "-n", namespace, "get", "deployment", workload, "-o", "json"], timeout=30,
))
pod = ((live.get("spec") or {}).get("template") or {}).get("spec") or {}
ops = []
for item in stale:
    containers = pod.get(item["group"]) or []
    index = next((i for i, container in enumerate(containers) if container.get("name") == item["container"]), None)
    if index is None:
        raise SystemExit("container " + str(item["container"]) + " is missing after the rollback")
    mount = item["mount"]
    mounts = containers[index].get("volumeMounts")
    if any(m.get("name") == mount.get("name") and m.get("mountPath") == mount.get("mountPath") for m in mounts or []):
        continue
    base = "/spec/template/spec/%s/%d" % (item["group"], index)
    ops.append({"op": "test", "path": base + "/name", "value": item["container"]})
    if mounts is None:
        ops.append({"op": "add", "path": base + "/volumeMounts", "value": [mount]})
        containers[index]["volumeMounts"] = [mount]
    else:
        ops.append({"op": "add", "path": base + "/volumeMounts/-", "value": mount})
        mounts.append(mount)
if ops:
    subprocess.run(["kubectl", "-n", namespace, "patch", "deployment", workload, "--type=json",
                    "-p", json.dumps(ops, separators=(",", ":"))], check=True, timeout=60)
print("volume_mounts_restored count=" + str(sum(1 for op in ops if op["op"] == "add")))
PY
}
`

// The preflight Job starts from the target image, so a successful start already
// means the image was pulled to the node. It changes nothing: it only repeats the
// apply checks and asks the API server to dry-run the same Helm upgrade.
const selfUpgradePreflightScript = selfUpgradeKubeconfigPrelude + selfUpgradeDeploymentVerifier + "verify_record=\n" + selfUpgradeApplyChecks + `failure_category=helm_dry_run_failed
helm upgrade "$release" "$chart" -n "$ns" -f "$values" --timeout 180s --wait=false --dry-run=server >/dev/null
echo "preflight-passed image=${target}"
`

// Adoption has no Helm release to dry-run yet; starting the Job still proves the pull.
const selfUpgradePullOnlyScript = selfUpgradeKubeconfigPrelude + `echo "preflight-passed image=${DOOPS_UPGRADE_IMAGE}"
`

const selfUpgradeApplyScript = selfUpgradeKubeconfigPrelude + selfUpgradeDeploymentVerifier + selfUpgradeApplyChecks + selfUpgradeRestoreVolumeMounts + `mutated=false
rollback_release() {
  echo "rolling back ${release} to ${previous}" >&2
  if ! helm rollback "$release" "$previous" -n "$ns" --timeout 300s --wait; then
    echo "Error: automatic rollback failed: Helm rollback command failed" >&2
    return 1
  fi
  if [ -s "$stale_mounts" ] && ! restore_volume_mounts; then
    echo "Error: automatic rollback failed: the removed volume mounts could not be restored" >&2
    return 1
  fi
  if ! kubectl -n "$ns" rollout status "deployment/${workload}" --timeout 300s; then
    echo "Error: automatic rollback failed: rolled-back Deployment did not become ready" >&2
    return 1
  fi
  if ! DOOPS_UPGRADE_VERIFICATION_STAGE=rollback verify_deployment "$previous_image" baseline record; then
    echo "Error: automatic rollback failed: rolled-back Deployment final state is not verified" >&2
    return 1
  fi
  echo "automatic rollback complete revision=${previous}" >&2
}
cleanup() {
  status=$?
  trap - EXIT
  if [ "$status" -ne 0 ] && [ "$mutated" = true ]; then
    if ! rollback_release; then
      echo "Error: automatic rollback failed; manual intervention is required" >&2
      printf '%s' automatic_rollback_failed >"${TMPDIR:-/tmp}/doops-upgrade-category"
      export DOOPS_UPGRADE_ROLLBACK_OUTCOME=failed
      finish_upgrade 1
    fi
    export DOOPS_UPGRADE_ROLLBACK_OUTCOME=succeeded
  fi
  finish_upgrade "$status"
}
trap cleanup EXIT
failure_category=helm_dry_run_failed
helm upgrade "$release" "$chart" -n "$ns" -f "$values" --timeout 180s --wait=false --dry-run=server >/dev/null
if [ -s "$stale_mounts" ]; then
  # Back the mounts up in the cluster before the first change, so the evidence
  # survives this Job even when the rollback cannot run.
  failure_category=mount_backup_failed
  kubectl -n "$ns" apply -f "$mount_backup"
fi
mutated=true
if [ -s "$stale_mounts" ]; then
  failure_category=mount_removal_failed
  kubectl -n "$ns" patch deployment "$workload" --type=json -p "$(cat "$remove_mounts_patch")"
  echo "stale_volume_mounts_removed workload=${workload}"
fi
failure_category=helm_upgrade_failed
helm upgrade "$release" "$chart" -n "$ns" -f "$values" --timeout 180s --wait=false >/dev/null
failure_category=release_verification_failed
deadline=$(( $(date +%s) + 600 ))
while [ "$(date +%s)" -lt "$deadline" ]; do
  if verify_deployment "$target"; then
    sleep 15
    if verify_deployment "$target" controller record; then
      emit_upgrade_event succeeded "" || { failure_category=verification_receipt_failed; exit 1; }
      echo "self-upgrade complete image=${target}"
      exit 0
    fi
  fi
  sleep 5
done
echo "new deployment did not stay ready" >&2
exit 1
`

const selfUpgradeRollbackScript = selfUpgradeKubeconfigPrelude + selfUpgradeDeploymentVerifier + selfUpgradeRevisionImage + selfUpgradeRBACGuard + `previous="$DOOPS_UPGRADE_REVISION"
failure_category=rollback_evidence_invalid
python3 - "$ns" "$release" "$previous" <<'PY'
import json, subprocess, sys
ns, release, revision = sys.argv[1:4]
rows = json.loads(subprocess.check_output(["helm", "history", release, "-n", ns, "-o", "json"], timeout=30))
current = [int(row["revision"]) for row in rows if row["status"] == "deployed"]
if len(current) != 1 or not any(int(row["revision"]) == int(revision) < current[0] and row["status"] == "superseded" for row in rows):
    raise SystemExit("admitted rollback revision is no longer eligible")
PY
expected="$(revision_image "$previous")"
test "$expected" = "$DOOPS_UPGRADE_IMAGE" || { echo "rollback revision image changed after admission" >&2; exit 1; }
DOOPS_UPGRADE_BASELINE="$(python3 - "$ns" "$release" "$workload" "$previous" <<'PY'
import json, os, re, subprocess, sys
sys.path.insert(0, "/app")
from adopt_agent import report_failure
ns, release, workload, revision = sys.argv[1:]
stored = json.loads(subprocess.check_output(
    ["kubectl", "-n", ns, "get", "configmap", "doops-upgrade-verified-" + release, "-o", "json"], timeout=30))
receipt = json.loads(stored["data"]["evidence.json"])
entry = receipt["revisions"][revision]
if receipt.get("release") != release or receipt.get("workload") != workload or receipt.get("schemaVersion") != 1:
    raise SystemExit("category=rollback_evidence_invalid rollback receipt scope changed")
if (entry.get("outcome") != "succeeded" or not re.fullmatch(r"[0-9a-f]{40}", entry.get("release_commit", ""))
        or entry["release_commit"] != os.environ["DOOPS_UPGRADE_EXPECTED_RELEASE_COMMIT"]
        or entry["image"] != os.environ["DOOPS_UPGRADE_IMAGE"]):
    report_failure("rollback_commit_unknown")
    raise SystemExit("category=rollback_commit_unknown historical verified commit changed or is unknown")
if not re.fullmatch(r"sha256:[0-9a-f]{64}", entry.get("runtimeDigest", "")) or not entry.get("node"):
    raise SystemExit("category=rollback_evidence_invalid historical runtime identity is missing")
print(json.dumps({"image": entry["image"], "digest": entry["runtimeDigest"], "node": entry["node"],
                  "runtime_class": entry.get("runtimeClass", ""), "release_commit": entry["release_commit"]}))
PY
)"
export DOOPS_UPGRADE_BASELINE
current_manifest="${TMPDIR:-/tmp}/doops-rollback-current-manifest.yaml"
rollback_manifest="${TMPDIR:-/tmp}/doops-rollback-target-manifest.yaml"
helm get manifest "$release" -n "$ns" >"$current_manifest"
helm get manifest "$release" -n "$ns" --revision "$previous" >"$rollback_manifest"
failure_category=rbac_controller_identity_changed
verify_controller_rbac "$current_manifest" "$rollback_manifest"
failure_category=helm_rollback_failed
helm rollback "$release" "$previous" -n "$ns" --timeout 300s --wait
failure_category=rollback_rollout_failed
kubectl -n "$ns" rollout status "deployment/${workload}" --timeout 300s
failure_category=rollback_verification_failed
verify_deployment "$expected" baseline record
echo "self-upgrade rolled back to ${previous}"
`

// adoptionTarget names the release and workload for an Agent Deployment that has no
// Helm release yet. The release defaults to the Deployment name.
func adoptionTarget(requestedRelease, requestedWorkload string) (string, string, error) {
	workload := strings.TrimPrefix(strings.TrimSpace(requestedWorkload), "deployment/")
	if workload == "" {
		workload = selfUpgradeContainer
	}
	if strings.Contains(workload, "/") {
		return "", "", fmt.Errorf("adoption supports a Deployment workload only")
	}
	release := firstNonEmptyString(strings.TrimSpace(requestedRelease), workload)
	if err := validateK8sName("workload", workload, 63); err != nil {
		return "", "", err
	}
	if err := validateK8sName("release", release, 53); err != nil {
		return "", "", err
	}
	return release, workload, nil
}

const selfUpgradeAdoptScript = selfUpgradeKubeconfigPrelude + `failure_category=adopt_failed
python3 /app/adopt_agent.py run \
  --namespace "$ns" --workload "$workload" --release "$release" \
  --cluster "$DOOPS_GATEWAY_CLUSTER" --instance "$DOOPS_GATEWAY_INSTANCE" \
  --image "$DOOPS_UPGRADE_IMAGE"
echo "self-upgrade complete image=${DOOPS_UPGRADE_IMAGE}"
`
