package main

import (
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

func buildK8SRequest(args []string) (K8SRequest, error) {
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
		addString("image", opts["image"])
		addString("output", opts["output"])
		addString("timeout", opts["timeout"])
		if err := addInt("tail", opts["tail"]); err != nil {
			return err
		}
		if err := addInt("replicas", opts["replicas"]); err != nil {
			return err
		}
		if opts["confirm"] == "true" {
			req.Payload["confirm"] = true
		}
		return nil
	}

	switch positional[0] {
	case "get":
		if len(positional) < 2 {
			return K8SRequest{}, fmt.Errorf("get requires kind")
		}
		req.Payload["operation"] = "get"
		req.Payload["kind"] = positional[1]
	case "describe":
		if len(positional) < 3 {
			return K8SRequest{}, fmt.Errorf("describe requires kind and name")
		}
		req.Payload["operation"] = "describe"
		req.Payload["kind"] = positional[1]
		req.Payload["name"] = positional[2]
	case "logs":
		if len(positional) < 2 {
			return K8SRequest{}, fmt.Errorf("logs requires resource")
		}
		req.Payload["operation"] = "logs"
		req.Payload["resource"] = positional[1]
	case "top":
		if len(positional) < 2 {
			return K8SRequest{}, fmt.Errorf("top requires pods|nodes")
		}
		req.Payload["operation"] = "top"
		req.Payload["kind"] = positional[1]
	case "events":
		req.Payload["operation"] = "events"
		addString("resource", opts["for"])
	case "rollout":
		if len(positional) < 3 {
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
		if len(positional) < 2 {
			return K8SRequest{}, fmt.Errorf("scale requires resource")
		}
		req.Payload["operation"] = "scale"
		req.Payload["resource"] = positional[1]
	case "node":
		if len(positional) < 3 {
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
		if len(positional) < 3 || positional[1] != "pod" {
			return K8SRequest{}, fmt.Errorf("only delete pod is supported")
		}
		req.Payload["operation"] = "delete-pod"
		req.Payload["name"] = positional[2]
	case "deploy-image":
		if len(positional) < 2 {
			return K8SRequest{}, fmt.Errorf("deploy-image requires resource")
		}
		req.Payload["operation"] = "set-image"
		req.Payload["resource"] = positional[1]
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
	opts := map[string]string{
		"target":    "",
		"namespace": "",
		"container": "",
		"image":     "",
		"output":    "",
		"tail":      "",
		"replicas":  "",
		"timeout":   "",
		"for":       "",
		"confirm":   "",
	}
	var positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "--target", "--namespace", "--container", "--image", "--output", "--tail", "--replicas", "--timeout", "--for":
			if i+1 >= len(args) {
				return nil, nil, fmt.Errorf("%s requires a value", arg)
			}
			opts[strings.TrimPrefix(arg, "--")] = args[i+1]
			i++
		case "--confirm":
			opts["confirm"] = "true"
		default:
			if strings.HasPrefix(arg, "-") {
				return nil, nil, fmt.Errorf("unknown k8s flag %s", arg)
			}
			positional = append(positional, arg)
		}
	}
	return opts, positional, nil
}
