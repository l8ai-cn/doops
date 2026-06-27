package main

import (
	"fmt"
	"strings"
)

// CheckDeployment 在目标节点上通过 kubectl 校验 Deployment 主容器镜像是否与期望一致。
func CheckDeployment(client *MCPClient, namespace, deployment, wantImage string) error {
	out, err := client.CallAndCapture("doops_check_deployment", map[string]interface{}{
		"namespace":  namespace,
		"deployment": deployment,
		"image":      wantImage,
	})
	if err != nil {
		return err
	}
	fmt.Println(strings.TrimSpace(out))
	return nil
}

func stripJSONTextNoise(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, `"'`)
	return strings.TrimSpace(s)
}

func imageRefMatches(want, got string) bool {
	want = strings.TrimSpace(want)
	got = strings.TrimSpace(got)
	if want == got {
		return true
	}
	// 允许仅比对仓库路径（省略 tag）或一方带 digest
	if strings.HasPrefix(got, want+":") || strings.HasPrefix(got, want+"@") {
		return true
	}
	if strings.HasPrefix(want, got+":") || strings.HasPrefix(want, got+"@") {
		return true
	}
	return false
}
