package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

type K8SRequest struct {
	Target  string
	Payload map[string]interface{}
}

type k8sCaller interface {
	Call(toolName string, arguments map[string]interface{}) error
}

// errK8SHelp tells the caller to print k8sUsage and exit successfully.
var errK8SHelp = errors.New("k8s help requested")

const k8sUsage = `用法: doops -session <s> k8s --target <t> <command> [flags]

这是常用 Kubernetes 操作的结构化便利入口。CLI 解析参数；Gateway 检查目标授权，Agent 校验操作和值。

只读（可与发布并行）:
  get <kind> [name]               [-n|--namespace ns | -A|--all-namespaces] [-l|--selector sel] [-o|--output json|yaml|wide|name|jsonpath=...|custom-columns=...]
  describe <kind> <name>          [-n|--namespace ns]
  logs <resource>                 [-n|--namespace ns] [-c|--container c] [--tail N] [-p|--previous] [--since 10m]
  top pods|nodes                  [-n|--namespace ns | -A] [-l|--selector sel]
  events                          [-n|--namespace ns | -A] [--for kind/name]
  rollout status <resource>       [-n|--namespace ns] [--timeout 120s]
  helm list                       [-n|--namespace ns | -A]
  helm history|status <release>   -n|--namespace ns
  helm values <release>           -n|--namespace ns   (只返回 key 路径，不返回值)
  Agent 不允许通过此入口读取 Secret 与 ServiceAccount。

写操作（选定操作即执行）:
  rollout restart <resource>      [-n ns]
  scale <resource> --replicas N   [-n ns]
  node cordon|uncordon <node>
  delete pod <name>               [-n ns]

目标已授权、命令已确定时，可用 doops -session <s> exec --target <t> --cmd '<命令>' 执行原生 kubectl 或 helm。
已有脚本可继续使用上述 k8s 子命令。探索性诊断可用 ask，业务发布也可用 ask --apply。
遇到授权或策略拒绝时停止执行，按返回的原因处理。`

func buildK8SRequest(args []string) (K8SRequest, error) {
	for _, arg := range args {
		if arg == "-h" || arg == "-help" || arg == "--help" {
			return K8SRequest{}, errK8SHelp
		}
	}
	req, err := parseK8SRequest(args)
	if err != nil {
		return K8SRequest{}, fmt.Errorf("%w\nnext: doops k8s -h", err)
	}
	return req, nil
}

func parseK8SRequest(args []string) (K8SRequest, error) {
	opts, positional, err := parseK8SFlags(args)
	if err != nil {
		return K8SRequest{}, err
	}
	if opts["target"] == "" {
		return K8SRequest{}, fmt.Errorf("--target is required")
	}
	if len(positional) == 0 {
		return K8SRequest{}, fmt.Errorf("k8s subcommand is required")
	}

	req := K8SRequest{
		Target:  opts["target"],
		Payload: map[string]interface{}{},
	}
	addString := func(key, value string) {
		if strings.TrimSpace(value) != "" {
			req.Payload[key] = strings.TrimSpace(value)
		}
	}
	addInt := func(key, value string) error {
		if strings.TrimSpace(value) == "" {
			return nil
		}
		n, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("--%s must be an integer", key)
		}
		req.Payload[key] = n
		return nil
	}
	addCommon := func() error {
		addString("namespace", opts["namespace"])
		addString("container", opts["container"])
		addString("output", opts["output"])
		addString("timeout", opts["timeout"])
		if err := addInt("tail", opts["tail"]); err != nil {
			return err
		}
		if err := addInt("replicas", opts["replicas"]); err != nil {
			return err
		}
		addString("selector", opts["selector"])
		addString("since", opts["since"])
		if opts["all-namespaces"] == "true" {
			req.Payload["all_namespaces"] = true
		}
		if opts["previous"] == "true" {
			req.Payload["previous"] = true
		}
		return nil
	}

	switch positional[0] {
	case "get":
		if len(positional) != 2 && len(positional) != 3 {
			return K8SRequest{}, fmt.Errorf("get requires kind and an optional name")
		}
		req.Payload["operation"] = "get"
		req.Payload["kind"] = positional[1]
		if len(positional) == 3 {
			req.Payload["name"] = positional[2]
		}
	case "helm":
		if len(positional) < 2 {
			return K8SRequest{}, fmt.Errorf("helm requires list|history|status|values")
		}
		switch positional[1] {
		case "list":
			if len(positional) != 2 {
				return K8SRequest{}, fmt.Errorf("helm list takes no release")
			}
			req.Payload["operation"] = "helm-list"
		case "history", "status", "values":
			if len(positional) != 3 {
				return K8SRequest{}, fmt.Errorf("helm %s requires a release", positional[1])
			}
			req.Payload["operation"] = "helm-" + positional[1]
			req.Payload["name"] = positional[2]
		default:
			return K8SRequest{}, fmt.Errorf("unsupported helm read %q", positional[1])
		}
	case "describe":
		if len(positional) != 3 {
			return K8SRequest{}, fmt.Errorf("describe requires kind and name")
		}
		req.Payload["operation"] = "describe"
		req.Payload["kind"] = positional[1]
		req.Payload["name"] = positional[2]
	case "logs":
		if len(positional) != 2 {
			return K8SRequest{}, fmt.Errorf("logs requires resource")
		}
		req.Payload["operation"] = "logs"
		req.Payload["resource"] = positional[1]
	case "top":
		if len(positional) != 2 {
			return K8SRequest{}, fmt.Errorf("top requires pods|nodes")
		}
		req.Payload["operation"] = "top"
		req.Payload["kind"] = positional[1]
	case "events":
		if len(positional) != 1 {
			return K8SRequest{}, fmt.Errorf("events does not accept positional arguments")
		}
		req.Payload["operation"] = "events"
		addString("resource", opts["for"])
	case "rollout":
		if len(positional) != 3 {
			return K8SRequest{}, fmt.Errorf("rollout requires status|restart and resource")
		}
		switch positional[1] {
		case "status":
			req.Payload["operation"] = "rollout-status"
		case "restart":
			req.Payload["operation"] = "rollout-restart"
		default:
			return K8SRequest{}, fmt.Errorf("unsupported rollout operation %q", positional[1])
		}
		req.Payload["resource"] = positional[2]
	case "scale":
		if len(positional) != 2 {
			return K8SRequest{}, fmt.Errorf("scale requires resource")
		}
		if strings.TrimSpace(opts["replicas"]) == "" {
			return K8SRequest{}, fmt.Errorf("scale requires --replicas")
		}
		req.Payload["operation"] = "scale"
		req.Payload["resource"] = positional[1]
	case "node":
		if len(positional) != 3 {
			return K8SRequest{}, fmt.Errorf("node requires cordon|uncordon and node name")
		}
		switch positional[1] {
		case "cordon":
			req.Payload["operation"] = "cordon"
		case "uncordon":
			req.Payload["operation"] = "uncordon"
		default:
			return K8SRequest{}, fmt.Errorf("unsupported node operation %q", positional[1])
		}
		req.Payload["name"] = positional[2]
	case "delete":
		if len(positional) != 3 || positional[1] != "pod" {
			return K8SRequest{}, fmt.Errorf("only delete pod is supported")
		}
		req.Payload["operation"] = "delete-pod"
		req.Payload["name"] = positional[2]
	default:
		return K8SRequest{}, fmt.Errorf("unsupported k8s command %q", positional[0])
	}
	if err := addCommon(); err != nil {
		return K8SRequest{}, err
	}
	return req, nil
}

func runK8SRequest(caller k8sCaller, req K8SRequest) (string, error) {
	if caller == nil {
		return "", fmt.Errorf("k8s caller is required")
	}
	if err := caller.Call("doops_k8s", req.Payload); err != nil {
		return "", err
	}
	return "", nil
}

func parseK8SFlags(args []string) (map[string]string, []string, error) {
	opts := map[string]string{}
	var positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "--target", "--namespace", "-n", "--container", "-c", "--output", "-o", "--tail", "--replicas",
			"--timeout", "--for", "--selector", "-l", "--since":
			if i+1 >= len(args) || strings.TrimSpace(args[i+1]) == "" {
				return nil, nil, fmt.Errorf("%s requires a value", arg)
			}
			value := args[i+1]
			if len(value) > 1 && value[0] == '-' && (value[1] < '0' || value[1] > '9') {
				return nil, nil, fmt.Errorf("%s requires a value before %s", arg, value)
			}
			opts[k8sFlagName(arg)] = value
			i++
		case "--all-namespaces", "-A":
			opts["all-namespaces"] = "true"
		case "--previous", "-p":
			opts["previous"] = "true"
		default:
			if strings.HasPrefix(arg, "-") {
				return nil, nil, fmt.Errorf("unknown k8s flag %s", arg)
			}
			positional = append(positional, arg)
		}
	}
	return opts, positional, nil
}

// k8sFlagName maps kubectl-style short flags to their long option name.
func k8sFlagName(arg string) string {
	switch arg {
	case "-n":
		return "namespace"
	case "-c":
		return "container"
	case "-o":
		return "output"
	case "-l":
		return "selector"
	}
	return strings.TrimPrefix(arg, "--")
}
