package main

import (
	"flag"
	"fmt"
	"strings"
)

type webMode string

const (
	webModeCheck webMode = "check"
	webModePlan  webMode = "plan"
	webModeApply webMode = "apply"
)

func runWebCommand(args []string, sessionName string, servers []Server, configErr error, ss *SessionStore, verbose bool) error {
	flags := flag.NewFlagSet("web", flag.ContinueOnError)
	flags.SetOutput(nil)
	target := flags.String("target", "", "Configured Gateway target")
	request := flags.String("request", "", "Website operation in natural language")
	mode := flags.String("mode", string(webModeCheck), "DoWeb mode: check, plan, or apply")
	site := flags.String("site", "", "Optional site id")
	dist := flags.String("dist", "", "Optional customer-built dist path on the target")
	domain := flags.String("domain", "", "Optional site domain")
	runtimeMode := flags.String("runtime", "host", "Nginx runtime: host or docker")
	subSession := flags.String("session", "", "Session ID (can also be set before subcommand)")
	var credentialRefs stringListFlag
	flags.Var(&credentialRefs, "credential", "Managed credential resource name; repeatable and only valid with --mode apply")
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	if *subSession != "" && sessionName == "" {
		sessionName = *subSession
	}
	sessionName = resolveSession(sessionName)
	if strings.TrimSpace(*target) == "" {
		return fmt.Errorf("--target 必传，请指定 Gateway target")
	}
	if strings.TrimSpace(*request) == "" {
		positional := strings.TrimSpace(strings.Join(flags.Args(), " "))
		if positional != "" {
			*request = positional
		}
	}
	if strings.TrimSpace(*request) == "" {
		return fmt.Errorf("%s", withNext("web needs --request", "doops web --target "+*target+" --mode check --request '<site>'"))
	}
	parsedMode, err := parseWebMode(*mode)
	if err != nil {
		return err
	}
	if *runtimeMode != "host" && *runtimeMode != "docker" {
		return fmt.Errorf("--runtime must be host or docker")
	}
	if len(credentialRefs) > 0 && parsedMode != webModeApply {
		return fmt.Errorf("--credential 仅允许与 --mode apply 一起使用")
	}
	instruction := buildWebInstruction(parsedMode, *site, *dist, *domain, *request) + "\nNginx runtime_mode=" + *runtimeMode
	return runSkillPrompt("doweb", webGatewayOperation(parsedMode), *target, sessionName, instruction, "", []string(credentialRefs), servers, configErr, ss, verbose)
}

func parseWebMode(value string) (webMode, error) {
	switch webMode(strings.ToLower(strings.TrimSpace(value))) {
	case webModeCheck:
		return webModeCheck, nil
	case webModePlan:
		return webModePlan, nil
	case webModeApply:
		return webModeApply, nil
	default:
		return "", fmt.Errorf("unsupported --mode %q; use check, plan, or apply", value)
	}
}

func webGatewayOperation(mode webMode) string {
	if mode == webModeApply {
		return "apply"
	}
	return "ask"
}

func buildWebInstruction(mode webMode, siteID, dist, domain, request string) string {
	purpose := "只读检查网站、Nginx、TLS、DNS 或健康状态，不修改目标。"
	if mode == webModePlan {
		purpose = "只生成 SiteSpec、Nginx 配置计划、验证步骤和回滚方案，不执行修改。"
	}
	if mode == webModeApply {
		purpose = "按用户选定操作执行网站变更，无需再次请求用户确认；执行前核对 SiteSpec、dist、域名、代理、TLS 和验收条件，执行 nginx -t、reload、健康检查并保留证据。"
	}
	facts := make([]string, 0, 3)
	if strings.TrimSpace(siteID) != "" {
		facts = append(facts, "site_id="+strings.TrimSpace(siteID))
	}
	if strings.TrimSpace(dist) != "" {
		facts = append(facts, "dist="+strings.TrimSpace(dist))
	}
	if strings.TrimSpace(domain) != "" {
		facts = append(facts, "domain="+strings.TrimSpace(domain))
	}
	context := ""
	if len(facts) > 0 {
		context = "\n已知参数：" + strings.Join(facts, "; ")
	}
	return strings.TrimSpace(fmt.Sprintf(
		"使用 DoWeb skill 处理当前 DoOps Gateway target。%s%s\n不要读取或索取密码、SSH 凭据或 Gateway token；不要绕过 DoWeb 脚本、nginx -t、reload、健康检查和回滚证据。\n用户需求：%s",
		purpose, context, strings.TrimSpace(request),
	))
}
