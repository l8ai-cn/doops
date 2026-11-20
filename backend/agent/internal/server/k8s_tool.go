package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/user/doops/agent/api"
	"github.com/user/doops/agent/internal/dispatcher"
)

var (
	k8sNamePattern      = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`)
	k8sKindPattern      = regexp.MustCompile(`^[a-z][a-z0-9.-]*$`)
	k8sContainerPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]*[a-z0-9])?$`)
	k8sTimeoutPattern   = regexp.MustCompile(`^[1-9][0-9]*(s|m|h)$`)
	// Label selectors: key[=|==|!=]value, key in (a,b), !key; comma-separated.
	k8sSelectorPattern = regexp.MustCompile(`^[A-Za-z0-9_./!=(), -]{1,512}$`)
	// jsonpath output is kubectl's own read-only template language.
	k8sJSONPathPattern = regexp.MustCompile(`^(jsonpath|jsonpath-as-json|custom-columns)=[^\x00\r\n]+$`)
	k8sReadableKinds   = map[string]bool{
		"pod": true, "pods": true,
		"deployment": true, "deployments": true, "deploy": true,
		"statefulset": true, "statefulsets": true, "sts": true,
		"daemonset": true, "daemonsets": true, "ds": true,
		"replicaset": true, "replicasets": true, "rs": true,
		"service": true, "services": true, "svc": true,
		"endpoint": true, "endpoints": true,
		"endpointslice": true, "endpointslices": true,
		"ingress": true, "ingresses": true, "ing": true,
		"job": true, "jobs": true,
		"cronjob": true, "cronjobs": true,
		"node": true, "nodes": true,
		"namespace": true, "namespaces": true, "ns": true,
		"event": true, "events": true,
		"persistentvolumeclaim": true, "persistentvolumeclaims": true, "pvc": true,
		"persistentvolume": true, "persistentvolumes": true, "pv": true,
		"configmap": true, "configmaps": true, "cm": true,
		"networkpolicy": true, "networkpolicies": true, "netpol": true,
		"horizontalpodautoscaler": true, "horizontalpodautoscalers": true, "hpa": true,
		"poddisruptionbudget": true, "poddisruptionbudgets": true, "pdb": true,
		"role": true, "roles": true, "rolebinding": true, "rolebindings": true,
		"clusterrole": true, "clusterroles": true, "clusterrolebinding": true, "clusterrolebindings": true,
		"storageclass": true, "storageclasses": true, "sc": true,
		"ingressclass": true, "ingressclasses": true,
		"priorityclass": true, "priorityclasses": true,
		"limitrange": true, "limitranges": true, "resourcequota": true, "resourcequotas": true, "quota": true,
		"customresourcedefinition": true, "customresourcedefinitions": true, "crd": true, "crds": true,
		"lease": true, "leases": true,
	}
	// Secrets and ServiceAccount tokens carry credentials and stay out of the read tool.
	k8sCredentialKinds = map[string]bool{
		"secret": true, "secrets": true, "serviceaccount": true, "serviceaccounts": true, "sa": true,
		"token": true, "tokenrequest": true, "tokenrequests": true,
	}
	k8sLoggableKinds = map[string]bool{
		"pod": true, "pods": true,
		"deployment": true, "deployments": true, "deploy": true,
		"statefulset": true, "statefulsets": true, "sts": true,
		"daemonset": true, "daemonsets": true, "ds": true,
		"job": true, "jobs": true,
	}
	k8sRolloutKinds = map[string]bool{
		"deployment": true, "deployments": true, "deploy": true,
		"statefulset": true, "statefulsets": true, "sts": true,
		"daemonset": true, "daemonsets": true, "ds": true,
	}
	k8sScalableKinds = map[string]bool{
		"deployment": true, "deployments": true, "deploy": true,
		"statefulset": true, "statefulsets": true, "sts": true,
		"replicaset": true, "replicasets": true, "rs": true,
	}
)

type k8sCommand struct {
	args              []string
	helm              bool // run helm instead of kubectl
	valueKeysOnly     bool // print only the key paths of a values document
	statusOnly        bool // print only release metadata, not values, manifest or notes
	configMapKeysOnly bool // print only the data key names of ConfigMaps
}

// configMapKeysOutputLimit bounds the kubectl JSON held in memory while its
// values are discarded; a larger listing must be narrowed to a namespace.
var configMapKeysOutputLimit = 64 << 20

func handleK8STool(ctx context.Context, raw json.RawMessage, push notificationSender) (string, error) {
	var params api.K8SParams
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&params); err != nil {
		return "", fmt.Errorf("invalid k8s request: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return "", fmt.Errorf("invalid k8s request")
	}

	commands, err := buildK8SCommands(params)
	if err != nil {
		return "", err
	}
	for _, command := range commands {
		if command.configMapKeysOnly {
			if err := runConfigMapKeysCommand(ctx, command.args, push); err != nil {
				return "", err
			}
			continue
		}
		if command.helm {
			if err := runHelmReadCommand(ctx, command, push); err != nil {
				return "", err
			}
			continue
		}
		if err := runK8SCommand(ctx, command.args, push, !gatewayK8SReadOnly(params.Operation)); err != nil {
			return "", err
		}
	}
	return "Kubernetes operation complete.", nil
}

func buildK8SCommands(params api.K8SParams) ([]k8sCommand, error) {
	params.Operation = strings.ToLower(strings.TrimSpace(params.Operation))
	params.Kind = strings.ToLower(strings.TrimSpace(params.Kind))
	params.Name = strings.TrimSpace(params.Name)
	params.Resource = normalizeK8SResource(params.Resource)
	params.Namespace = strings.TrimSpace(params.Namespace)
	params.Container = strings.TrimSpace(params.Container)
	params.Output = strings.TrimSpace(params.Output)
	params.Timeout = strings.TrimSpace(params.Timeout)
	params.Selector = strings.TrimSpace(params.Selector)
	params.Since = strings.TrimSpace(params.Since)

	if params.Operation == "" {
		return nil, fmt.Errorf("k8s operation is required")
	}
	if err := validateSession(strings.TrimSpace(params.SessionID)); err != nil {
		return nil, err
	}
	if params.Namespace != "" && !k8sNamePattern.MatchString(params.Namespace) {
		return nil, fmt.Errorf("invalid kubernetes namespace")
	}
	if params.Kind != "" && !k8sKindPattern.MatchString(params.Kind) {
		return nil, fmt.Errorf("invalid kubernetes kind")
	}
	if params.Name != "" && !k8sNamePattern.MatchString(params.Name) {
		return nil, fmt.Errorf("invalid kubernetes name")
	}
	if params.Container != "" && !k8sContainerPattern.MatchString(params.Container) {
		return nil, fmt.Errorf("invalid kubernetes container")
	}
	if params.Resource != "" &&
		(params.Operation == "logs" || params.Operation == "events" ||
			params.Operation == "rollout-status") &&
		!validK8SResource(params.Resource) {
		if params.Operation == "events" {
			return nil, fmt.Errorf("invalid kubernetes resource for events; omit resource for namespace-wide events or use kind/name")
		}
		return nil, fmt.Errorf("invalid kubernetes resource")
	}
	if params.Timeout != "" && !k8sTimeoutPattern.MatchString(params.Timeout) {
		return nil, fmt.Errorf("invalid kubernetes timeout")
	}
	if params.Selector != "" && !k8sSelectorPattern.MatchString(params.Selector) {
		return nil, fmt.Errorf("invalid kubernetes label selector")
	}
	if params.Since != "" && !k8sTimeoutPattern.MatchString(params.Since) {
		return nil, fmt.Errorf("invalid logs since duration; use e.g. 10m or 2h")
	}
	if params.AllNamespaces && params.Namespace != "" {
		return nil, fmt.Errorf("all_namespaces and namespace are mutually exclusive")
	}
	for _, kind := range []string{params.Kind, k8sResourceKind(params.Resource)} {
		if k8sCredentialKinds[kind] {
			return nil, fmt.Errorf("kubernetes kind %q is not available through the read tool: it carries credentials", kind)
		}
	}
	if params.Tail < 0 || params.Tail > 10000 {
		return nil, fmt.Errorf("tail must be between 0 and 10000")
	}
	if params.Replicas != nil && (*params.Replicas < 0 || *params.Replicas > 10000) {
		return nil, fmt.Errorf("replicas must be between 0 and 10000")
	}

	namespace := func(args []string) []string {
		if params.Namespace != "" {
			return append(args, "--namespace", params.Namespace)
		}
		if params.AllNamespaces {
			return append(args, "--all-namespaces")
		}
		return args
	}
	selector := func(args []string) []string {
		if params.Selector != "" {
			return append(args, "--selector", params.Selector)
		}
		return args
	}
	timeout := func(args []string) []string {
		if params.Timeout != "" {
			return append(args, "--timeout", params.Timeout)
		}
		return args
	}
	switch params.Operation {
	case "get":
		if params.Kind == "" {
			return nil, fmt.Errorf("kind is required for get")
		}
		if !k8sReadableKinds[params.Kind] {
			return nil, fmt.Errorf("kubernetes kind %q is not available through the read tool", params.Kind)
		}
		args := namespace([]string{"get", params.Kind})
		if params.Name != "" {
			args = append(args, params.Name)
		} else {
			args = selector(args)
		}
		if params.Output != "" {
			if !validK8SOutput(params.Output) {
				return nil, fmt.Errorf("unsupported kubernetes output; use json, yaml, wide, name, jsonpath=..., or custom-columns=...")
			}
			args = append(args, "--output", params.Output)
		}
		return []k8sCommand{{args: args}}, nil
	case "describe":
		if params.Kind == "" || params.Name == "" {
			return nil, fmt.Errorf("kind and name are required for describe")
		}
		if !k8sReadableKinds[params.Kind] {
			return nil, fmt.Errorf("kubernetes kind %q is not available through the read tool", params.Kind)
		}
		return []k8sCommand{{args: namespace([]string{"describe", params.Kind, params.Name})}}, nil
	case "logs":
		if params.Resource == "" {
			return nil, fmt.Errorf("resource is required for logs")
		}
		if !k8sLoggableKinds[k8sResourceKind(params.Resource)] {
			return nil, fmt.Errorf("kubernetes resource %q is not available for logs", params.Resource)
		}
		args := namespace([]string{"logs", params.Resource})
		if params.Container != "" {
			args = append(args, "--container", params.Container)
		}
		if params.Tail > 0 {
			args = append(args, "--tail", strconv.Itoa(params.Tail))
		}
		if params.Previous {
			args = append(args, "--previous")
		}
		if params.Since != "" {
			args = append(args, "--since", params.Since)
		}
		return []k8sCommand{{args: args}}, nil
	case "top":
		if params.Kind != "pods" && params.Kind != "nodes" {
			return nil, fmt.Errorf("top kind must be pods or nodes")
		}
		return []k8sCommand{{args: selector(namespace([]string{"top", params.Kind}))}}, nil
	case "events":
		args := namespace([]string{"events"})
		if params.Resource != "" {
			if !k8sReadableKinds[k8sResourceKind(params.Resource)] {
				return nil, fmt.Errorf("kubernetes resource %q is not available for events; use kind/name or omit resource", params.Resource)
			}
			args = append(args, "--for", params.Resource)
		}
		return []k8sCommand{{args: args}}, nil
	case "configmap-keys":
		// describe and get -o json print ConfigMap values, so this read keeps
		// only the key names, the way helm-values keeps only key paths.
		if params.Output != "" {
			return nil, fmt.Errorf("configmap-keys has a fixed output; omit output")
		}
		args := namespace([]string{"get", "configmaps"})
		if params.Name != "" {
			args = append(args, params.Name)
		} else {
			args = selector(args)
		}
		return []k8sCommand{{configMapKeysOnly: true, args: append(args, "--output", "json")}}, nil
	case "helm-list":
		return []k8sCommand{{helm: true, args: namespace([]string{"list", "--output", "json"})}}, nil
	case "helm-history", "helm-status", "helm-values":
		if params.Name == "" || !k8sNamePattern.MatchString(params.Name) {
			return nil, fmt.Errorf("name (the Helm release) is required for %s", params.Operation)
		}
		if params.Namespace == "" {
			return nil, fmt.Errorf("namespace is required for %s", params.Operation)
		}
		switch params.Operation {
		case "helm-history":
			return []k8sCommand{{helm: true, args: []string{"history", params.Name, "--namespace", params.Namespace, "--max", "20", "--output", "json"}}}, nil
		case "helm-status":
			// helm status -o json carries the release values, rendered manifest and
			// NOTES, any of which can hold plaintext secrets; only metadata is kept.
			return []k8sCommand{{helm: true, statusOnly: true, args: []string{"status", params.Name, "--namespace", params.Namespace, "--output", "json"}}}, nil
		default:
			// Values can hold plaintext secrets, so only their key paths are returned.
			return []k8sCommand{{helm: true, valueKeysOnly: true, args: []string{"get", "values", params.Name, "--namespace", params.Namespace, "--all", "--output", "json"}}}, nil
		}
	case "rollout-status":
		if params.Resource == "" {
			return nil, fmt.Errorf("resource is required for rollout status")
		}
		if !k8sRolloutKinds[k8sResourceKind(params.Resource)] {
			return nil, fmt.Errorf("kubernetes resource %q does not support rollout status", params.Resource)
		}
		return []k8sCommand{{args: timeout(namespace([]string{"rollout", "status", params.Resource}))}}, nil
	case "rollout-restart":
		if params.Resource == "" {
			return nil, fmt.Errorf("resource is required for rollout restart")
		}
		if !k8sRolloutKinds[k8sResourceKind(params.Resource)] {
			return nil, fmt.Errorf("kubernetes resource %q does not support rollout restart", params.Resource)
		}
		return []k8sCommand{{args: namespace([]string{"rollout", "restart", params.Resource})}}, nil
	case "scale":
		if params.Resource == "" {
			return nil, fmt.Errorf("resource is required for scale")
		}
		if !k8sScalableKinds[k8sResourceKind(params.Resource)] {
			return nil, fmt.Errorf("kubernetes resource %q does not support scale", params.Resource)
		}
		if params.Replicas == nil {
			return nil, fmt.Errorf("replicas is required for scale")
		}
		return []k8sCommand{{args: namespace([]string{"scale", params.Resource, "--replicas", strconv.Itoa(*params.Replicas)})}}, nil
	case "cordon", "uncordon":
		if params.Name == "" {
			return nil, fmt.Errorf("node name is required for %s", params.Operation)
		}
		return []k8sCommand{{args: []string{params.Operation, params.Name}}}, nil
	case "delete-pod":
		if params.Name == "" {
			return nil, fmt.Errorf("pod name is required for delete")
		}
		return []k8sCommand{{args: namespace([]string{"delete", "pod", params.Name})}}, nil
	default:
		return nil, fmt.Errorf("unsupported k8s operation %q; next: doops k8s -h", params.Operation)
	}
}

func validK8SResource(resource string) bool {
	parts := strings.Split(resource, "/")
	return len(parts) == 2 &&
		k8sKindPattern.MatchString(parts[0]) &&
		k8sNamePattern.MatchString(parts[1])
}

func normalizeK8SResource(resource string) string {
	parts := strings.Split(strings.TrimSpace(resource), "/")
	if len(parts) != 2 {
		return strings.TrimSpace(resource)
	}
	return strings.ToLower(strings.TrimSpace(parts[0])) + "/" + strings.TrimSpace(parts[1])
}

func k8sResourceKind(resource string) string {
	kind, _, _ := strings.Cut(resource, "/")
	return strings.ToLower(kind)
}

func validK8SOutput(output string) bool {
	switch output {
	case "json", "yaml", "wide", "name":
		return true
	default:
		return len(output) <= 1024 && k8sJSONPathPattern.MatchString(output)
	}
}

func runK8SCommand(ctx context.Context, args []string, push notificationSender, mutation bool) error {
	// waitCommandOutput owns cancellation of the entire process group.
	cmd, err := dispatcher.CommandContext(context.WithoutCancel(ctx), "kubectl", args...)
	if err != nil {
		return err
	}
	_, stdout, stderr, err := startRawCommand(ctx, cmd)
	if err != nil {
		return fmt.Errorf("start kubectl: %w", err)
	}
	// Redact complete diagnostics before exposing any part of them. Never
	// retain an arbitrary suffix of a possibly multiline credential.
	const limit = 8 << 10
	var diagnostic strings.Builder
	oversized := false
	err = waitCommandOutput(ctx, cmd, stdout, stderr, push, func(event notificationEvent) {
		if len(event.Data) > limit-diagnostic.Len() {
			oversized = true
		} else if !oversized {
			diagnostic.WriteString(event.Data)
		}
	}, false)
	reason, redactErr := redactDoagentUIText(strings.TrimSpace(diagnostic.String()))
	if oversized || redactErr != nil {
		reason = "[kubectl diagnostic withheld]"
	}
	if reason != "" {
		push(rawNotification(reason + "\n"))
	}
	if err != nil {
		failure := fmt.Errorf("kubectl %s failed: %w", strings.Join(args, " "), err)
		if mutation {
			// The native exit status describes the command, not the API write.
			if reason != "" {
				failure = fmt.Errorf("%w: %s", failure, reason)
			}
			return fmt.Errorf("%w: %w", errExecutionOutcomeUnknown, failure)
		}
		return failure
	}
	return nil
}

// runHelmReadCommand runs a read-only helm command. Values output is reduced to
// its key paths so plaintext secrets in release values never reach the caller.
func runHelmReadCommand(ctx context.Context, command k8sCommand, push notificationSender) error {
	cmd, err := dispatcher.CommandContext(ctx, "helm", command.args...)
	if err != nil {
		return err
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("helm %s failed: %s", command.args[0], sanitizeSecretText(strings.TrimSpace(string(output))))
	}
	if command.statusOnly {
		return pushHelmStatusSummary(output, push)
	}
	if !command.valueKeysOnly {
		push(rawNotification(string(output)))
		return nil
	}
	var values interface{}
	if err := json.Unmarshal(output, &values); err != nil {
		return fmt.Errorf("decode helm values: %w", err)
	}
	keys := helmValueKeyPaths("", values)
	sort.Strings(keys)
	push(rawNotification("value keys (values are not shown):\n" + strings.Join(keys, "\n") + "\n"))
	return nil
}

// helmStatusSummary is the part of helm status -o json that holds no values:
// config, manifest, hooks and info.notes are dropped before anything is printed.
type helmStatusSummary struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Version   int    `json:"version"`
	Info      struct {
		Status        string `json:"status"`
		Description   string `json:"description,omitempty"`
		FirstDeployed string `json:"first_deployed,omitempty"`
		LastDeployed  string `json:"last_deployed,omitempty"`
		Deleted       string `json:"deleted,omitempty"`
	} `json:"info"`
}

func pushHelmStatusSummary(output []byte, push notificationSender) error {
	var summary helmStatusSummary
	if err := json.Unmarshal(output, &summary); err != nil || summary.Name == "" {
		// The decoder's message can quote input; the input holds values.
		return fmt.Errorf("decode helm status: helm output is not a release status")
	}
	// A failed release description can repeat rendered text.
	summary.Info.Description = redactSecretText(summary.Info.Description)
	encoded, err := json.Marshal(summary)
	if err != nil {
		return fmt.Errorf("encode helm status: %w", err)
	}
	push(rawNotification(string(encoded) + "\n"))
	return nil
}

// configMapKeysObject decodes one ConfigMap or a List of them. Values are kept
// only as raw JSON so they can be dropped; nothing here formats them.
type configMapKeysObject struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
	} `json:"metadata"`
	Data       map[string]json.RawMessage `json:"data"`
	BinaryData map[string]json.RawMessage `json:"binaryData"`
	Items      []configMapKeysObject      `json:"items"`
}

// runConfigMapKeysCommand runs kubectl get configmaps -o json and prints only
// namespace, name and the data/binaryData key names. Neither the output nor an
// error carries a value.
func runConfigMapKeysCommand(ctx context.Context, args []string, push notificationSender) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd, err := dispatcher.CommandContext(ctx, "kubectl", args...)
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("start kubectl: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start kubectl: %w", err)
	}
	output, readErr := io.ReadAll(io.LimitReader(stdout, int64(configMapKeysOutputLimit)+1))
	if len(output) > configMapKeysOutputLimit {
		cancel()
		_ = cmd.Wait()
		return fmt.Errorf("configmap listing exceeds %d bytes; narrow it to one namespace or a selector", configMapKeysOutputLimit)
	}
	if err := cmd.Wait(); err != nil {
		detail := sanitizeSecretText(strings.TrimSpace(stderr.String()))
		if detail == "" {
			detail = err.Error()
		}
		return fmt.Errorf("kubectl %s failed: %s", strings.Join(args, " "), detail)
	}
	if readErr != nil {
		return fmt.Errorf("read kubectl output: %w", readErr)
	}
	var document configMapKeysObject
	if err := json.Unmarshal(output, &document); err != nil {
		// The decoder's message can quote input; the input holds values.
		return fmt.Errorf("decode configmaps: kubectl output is not ConfigMap JSON")
	}
	objects := []configMapKeysObject{document}
	if document.Kind == "List" || document.Items != nil {
		objects = document.Items
	}
	var table bytes.Buffer
	writer := tabwriter.NewWriter(&table, 0, 0, 3, ' ', 0)
	fmt.Fprintln(writer, "NAMESPACE\tNAME\tKEYS")
	for _, object := range objects {
		if object.Metadata.Name == "" {
			return fmt.Errorf("decode configmaps: kubectl output is not ConfigMap JSON")
		}
		keys := make([]string, 0, len(object.Data)+len(object.BinaryData))
		for key := range object.Data {
			keys = append(keys, key)
		}
		for key := range object.BinaryData {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		// ConfigMap keys cannot contain commas, spaces or "<", so the list and
		// the empty marker stay unambiguous.
		joined := strings.Join(keys, ",")
		if joined == "" {
			joined = "<none>"
		}
		namespace := object.Metadata.Namespace
		if namespace == "" {
			namespace = "<none>"
		}
		fmt.Fprintf(writer, "%s\t%s\t%s\n", namespace, object.Metadata.Name, joined)
	}
	if err := writer.Flush(); err != nil {
		return fmt.Errorf("format configmap keys: %w", err)
	}
	push(rawNotification(table.String()))
	return nil
}

func helmValueKeyPaths(prefix string, value interface{}) []string {
	object, ok := value.(map[string]interface{})
	if !ok || len(object) == 0 {
		if prefix == "" {
			return nil
		}
		return []string{prefix}
	}
	var keys []string
	for key, child := range object {
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		keys = append(keys, helmValueKeyPaths(path, child)...)
	}
	return keys
}
