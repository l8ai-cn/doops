package main

import (
	"context"
	"crypto/rand"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"
)

func main() {
	verbose := flag.Bool("verbose", false, "Enable verbose logging")
	sessionName := flag.String("session", "", "Session/Task ID for isolation (REQUIRED)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `
Doops 分布式服务器管理工具 (doops.sh CLI)
专为 AI Agent 设计的声明式运维工具。遵循 GitOps 原则：声明目标状态，由执行器持续协调至可验证结果。

配置文件路径: ~/.agent/skills/doops/config.json（唯一默认配置；DOOPS_CONFIG 可显式覆盖）

用法:
  doops [选项] <命令> [参数]

可用命令:
  bash        执行交互式 Shell (通过 SSH 转发 PTY)
  list        列出所有已知服务器 (查看名称、IP、用途)
  targets     查看 gateway 当前在线的 cluster/instance
  exec        在目标节点工作区执行 Shell 命令 (用于确定性的日常流水线执行)
  ask         发送自然语言指令 (由边缘端智能体完成受控的通用运维协作)
  read        查看目标节点上的小文本文件（不用于下载大文件/二进制）
  write       写入文件到目标服务器
  info        获取节点系统信息 (CPU/内存/磁盘)
  k8s         受限 Kubernetes 运维入口 (get/logs/rollout/scale/deploy-image/plan/apply-plan)
  cicd        声明式 CI/CD workflow 入口 (lint/plan/run)
  session     生成并输出一个新的唯一 Session ID
  push        极速增量推送本地代码到远端沙盒 (固定至 /root/ws/$SESSION)
  pull        基于 Git 拉取远端 session 工作区到本地目录
  clean       清理远端节点上的工作区隔离沙盒
  unlock      管理员强制释放 gateway 上卡住的 target busy 状态
  admin       gateway 管理命令，例如给用户签发 token
  upgrade     通过 gateway 广播升级在线 doops-agent
  add         添加 gateway target
  install     通过 SSH 首次自举 Agent 到新节点 (仅安装阶段使用 SSH)
  login       缓存 gateway user token
  logout      登出远程节点
  check       检查线上服务容器配置的镜像与最新构建是否一致

选项:
  -verbose    启用详细日志
  -session    会话/任务ID (无默认值，涉及远程调用的命令必须提供以严格隔离工作空间)
  -help       显示此帮助信息

声明式 CI/CD 闭环示例:
  # 标准入口只有 Gateway 模式：doops add --name prod --gateway https://gw.example.com --cluster prod --instance master-1 --token <gateway-user-token>
  # 查看 gateway 在线目标：doops targets --target prod

  # 1. 模板是版本化的期望状态；模板输入形成不可变 DeploymentPlan。
  doops cicd lint -f deploy/workflows/test.yaml
  doops cicd plan -f deploy/workflows/test.yaml --set releaseId=<immutable-release> --set reason=smoke

  # 2. dry-run 只计算协调目标；真实变更必须显式授权。
  doops -session test_ops cicd run -f deploy/workflows/test.yaml --dry-run --set releaseId=<immutable-release> --set reason=smoke
  doops -session test_ops cicd run -f deploy/workflows/test.yaml --allow-mutate --set releaseId=<immutable-release> --set reason=release
`)
	}
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		flag.Usage()
		os.Exit(0)
	}

	command := args[0]
	cmdArgs := args[1:]

	ss := NewSessionStore()
	servers, llmConfig, configErr := LoadConfig()

	switch command {
	case "session":
		b := make([]byte, 8)
		rand.Read(b)
		fmt.Printf("ses_%x\n", b)
		os.Exit(0)

	case "list":
		if configErr != nil {
			fmt.Println("No doops nodes configured.")
			fmt.Println("Add a gateway target with: doops add --name <name> --gateway <gateway-url> --cluster <cluster> --instance <instance> --token <gateway-user-token>")
			return
		}
		fmt.Printf("%-15s %-18s %-15s %-6s %-22s %-20s %-30s\n", "NAME", "ALIASES", "IP", "PORT", "GATEWAY", "CLUSTER/INSTANCE", "USE")
		fmt.Println(strings.Repeat("-", 132))
		for _, s := range servers {
			port := s.Port
			if port == "" {
				port = "42222"
			}
			cluster := s.Cluster
			if cluster == "" {
				cluster = "default"
			}
			instance := s.Instance
			if instance == "" {
				instance = s.Name
			}
			fmt.Printf("%-15s %-18s %-15s %-6s %-22s %-20s %-30s\n", s.Name, strings.Join(s.Aliases, ","), s.IP, port, s.Gateway, cluster+"/"+instance, s.Use)
		}

	case "targets":
		var gateway, token, target string
		targetsFlag := flag.NewFlagSet("targets", flag.ExitOnError)
		targetsFlag.StringVar(&gateway, "gateway", "", "Gateway URL")
		targetsFlag.StringVar(&token, "token", "", "User token")
		targetsFlag.StringVar(&target, "target", "", "Configured target whose gateway/token should be used")
		targetsFlag.Parse(cmdArgs)
		if target != "" {
			requireConfig(configErr)
			server := findServer(servers, target)
			if server == nil {
				fmt.Printf("Error: Server '%s' not found.\n", target)
				os.Exit(1)
			}
			gateway = server.Gateway
			if token == "" {
				token = ResolveToken(server.Name, server.Token)
			}
		}
		if gateway == "" {
			fmt.Println("Error: --gateway or --target is required")
			os.Exit(1)
		}
		targets, err := fetchGatewayTargets(gateway, token)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("%-18s %-18s %-8s %-16s %-7s %-7s %-28s %-24s\n", "CLUSTER", "INSTANCE", "BUSY", "STATUS", "ACTIVE", "QUEUED", "RESOURCES", "LAST_SEEN")
		fmt.Println(strings.Repeat("-", 132))
		for _, item := range targets {
			status := item.Status
			if status == "" {
				if item.Busy {
					status = "busy"
				} else {
					status = "idle"
				}
			}
			if item.BusyReason != "" {
				status += ":" + item.BusyReason
			}
			resources := strings.Join(item.Resources, ",")
			if len(resources) > 27 {
				resources = resources[:24] + "..."
			}
			fmt.Printf("%-18s %-18s %-8v %-16s %-7d %-7d %-28s %-24s\n", item.Cluster, item.Instance, item.Busy, status, item.ActiveOps, item.QueuedOps, resources, item.LastSeen.Format(time.RFC3339))
		}

	case "unlock":
		var target, gateway, token, cluster, instance string
		unlockFlag := flag.NewFlagSet("unlock", flag.ExitOnError)
		unlockFlag.StringVar(&target, "target", "", "Configured gateway target whose token/gateway should be used")
		unlockFlag.StringVar(&gateway, "gateway", "", "Gateway URL")
		unlockFlag.StringVar(&token, "token", "", "Gateway user token")
		unlockFlag.StringVar(&cluster, "cluster", "", "Target cluster")
		unlockFlag.StringVar(&instance, "instance", "", "Target instance")
		unlockFlag.Parse(cmdArgs)
		if target != "" {
			requireConfig(configErr)
			server := findServer(servers, target)
			if server == nil {
				fmt.Printf("Error: Server '%s' not found.\n", target)
				os.Exit(1)
			}
			if gateway == "" {
				gateway = server.Gateway
			}
			if token == "" {
				token = ResolveToken(server.Name, server.Token)
			}
		}
		if gateway == "" || cluster == "" || instance == "" {
			fmt.Println("Error: --gateway/--target, --cluster and --instance are required")
			unlockFlag.Usage()
			os.Exit(1)
		}
		if err := unlockGatewayTarget(gateway, token, cluster, instance); err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Unlocked %s/%s\n", cluster, instance)

	case "admin":
		if len(cmdArgs) >= 1 && cmdArgs[0] == "jobs" {
			runAdminJobs(cmdArgs[1:], servers, configErr)
			return
		}
		if len(cmdArgs) < 2 {
			fmt.Println("Usage: doops admin token create --target <gateway-target> --user <username> [--name label] [--expires 720h] [--save-as target]")
			fmt.Println("       doops admin operations list --target <gateway-target>")
			fmt.Println("       doops admin operations cancel --target <gateway-target> --id <operation-id>")
			fmt.Println("       doops admin jobs <list|add|run|enable|disable|rm|issues> --target <gateway-target> [...]")
			os.Exit(1)
		}
		if cmdArgs[0] == "operations" && cmdArgs[1] == "list" {
			runAdminOperationsList(cmdArgs[2:], servers, configErr)
			return
		}
		if cmdArgs[0] == "operations" && cmdArgs[1] == "cancel" {
			runAdminOperationCancel(cmdArgs[2:], servers, configErr)
			return
		}
		if cmdArgs[0] != "token" || cmdArgs[1] != "create" {
			fmt.Println("Usage: doops admin token create --target <gateway-target> --user <username> [--name label] [--expires 720h] [--save-as target]")
			fmt.Println("       doops admin operations list --target <gateway-target>")
			fmt.Println("       doops admin operations cancel --target <gateway-target> --id <operation-id>")
			os.Exit(1)
		}
		var target, gateway, token, user, label, expires, saveAs string
		adminFlag := flag.NewFlagSet("admin token create", flag.ExitOnError)
		adminFlag.StringVar(&target, "target", "", "Configured gateway target whose token/gateway should be used")
		adminFlag.StringVar(&gateway, "gateway", "", "Gateway URL")
		adminFlag.StringVar(&token, "token", "", "Gateway admin user token")
		adminFlag.StringVar(&user, "user", "", "Gateway user name to issue a token for")
		adminFlag.StringVar(&label, "name", "", "Token label")
		adminFlag.StringVar(&expires, "expires", "", "Optional token TTL, e.g. 720h")
		adminFlag.StringVar(&saveAs, "save-as", "", "Optional configured target name to cache the issued token for")
		adminFlag.Parse(cmdArgs[2:])
		if target != "" {
			requireConfig(configErr)
			server := findServer(servers, target)
			if server == nil {
				fmt.Printf("Error: Server '%s' not found.\n", target)
				os.Exit(1)
			}
			if gateway == "" {
				gateway = server.Gateway
			}
			if token == "" {
				token = ResolveToken(server.Name, server.Token)
			}
		}
		if gateway == "" || user == "" {
			fmt.Println("Error: --gateway/--target and --user are required")
			adminFlag.Usage()
			os.Exit(1)
		}
		issued, err := GatewayAdminTokenCreate(gateway, token, gatewayAdminTokenCreateRequest{
			User:    user,
			Name:    label,
			Expires: expires,
		})
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}
		if strings.TrimSpace(saveAs) != "" {
			auth, _ := LoadAuth()
			auth.Set(saveAs, issued.Token)
			if err := auth.Save(); err != nil {
				fmt.Printf("Error saving issued token: %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("Cached issued token for %s\n", saveAs)
		}
		fmt.Printf("username=%s\n", issued.Username)
		fmt.Printf("token_id=%s\n", issued.TokenID)
		fmt.Printf("token=%s\n", issued.Token)
		fmt.Println("warning=store this token now; gateway only keeps its hash")

	case "k8s":
		req, err := buildK8SRequest(cmdArgs)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}
		requireConfig(configErr)
		server := findServer(servers, req.Target)
		if server == nil {
			fmt.Printf("Error: Server '%s' not found.\n", req.Target)
			os.Exit(1)
		}

		if req.Payload["operation"] == "plan-set-image" {
			msg, err := runK8SRequest(nil, req, time.Now())
			if err != nil {
				fmt.Printf("Error: %v\n", err)
				os.Exit(1)
			}
			fmt.Println(msg)
			RecordHistory(server.Name, "", fmt.Sprintf("k8s plan %s", req.PlanOut))
			return
		}

		if *sessionName == "" {
			fmt.Println("Error: -session 必传，请指定会话 ID 以隔离 K8S 运维操作")
			os.Exit(1)
		}
		if strings.TrimSpace(server.Gateway) == "" && strings.TrimSpace(server.IP) == "" {
			fmt.Printf("Error: target '%s' has neither a gateway nor an SSH IP configured.\n", req.Target)
			os.Exit(1)
		}
		token := ResolveToken(server.Name, server.Token)
		client := NewMCPClient(*server, ss, *sessionName, *verbose)
		client.Token = token
		defer client.Close()

		msg, err := runK8SRequest(client, req, time.Now())
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}
		if msg != "" {
			fmt.Println(msg)
		}
		RecordHistory(server.Name, *sessionName, fmt.Sprintf("k8s %s", req.Payload["operation"]))

	case "cicd":
		// CICD submits one immutable DeploymentPlan to the target controller.
		// The controller reconciles the declared state; this CLI does not
		// execute workflow steps or build command strings.
		newReconciler := func(plan DeploymentPlan) (deploymentReconciler, func(), error) {
			requireConfig(configErr)
			server := findServer(servers, plan.Spec.Target.ExecutionTarget)
			if server == nil {
				return nil, nil, fmt.Errorf("target '%s' not found; configure it with `doops add`", plan.Spec.Target.ExecutionTarget)
			}
			if err := validateCICDServerBinding(*server, plan); err != nil {
				return nil, nil, err
			}
			if *sessionName == "" {
				return nil, nil, fmt.Errorf("-session is required for `cicd run` (isolates the reconciliation session)")
			}
			if strings.TrimSpace(server.Gateway) == "" {
				return nil, nil, fmt.Errorf("target '%s' must use a configured DoOps gateway for target-bound reconciliation", server.Name)
			}
			client := NewMCPClient(*server, ss, *sessionName, *verbose)
			client.Token = ResolveToken(server.Name, server.Token)
			return client, func() { client.Close() }, nil
		}
		if err := runCICDCommand(context.Background(), cmdArgs, newReconciler); err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}

	case "exec", "ask", "write", "read", "info":
		var target, cmdStr, msgStr, path, modelStr, contentStr, fileStr string
		var subSession string
		var background bool
		subFlag := flag.NewFlagSet(command, flag.ExitOnError)
		subFlag.StringVar(&target, "target", "", "Target server name")
		subFlag.StringVar(&subSession, "session", "", "Session ID (can also be set before subcommand)")

		switch command {
		case "exec":
			subFlag.StringVar(&cmdStr, "cmd", "", "Command to execute")
			subFlag.BoolVar(&background, "bg", false, "Run through the agent-managed background task service and wait for completion")
		case "ask":
			subFlag.StringVar(&msgStr, "msg", "", "Instruction")
			subFlag.StringVar(&modelStr, "model", "", "Model to use for this instruction (optional)")
		case "write":
			subFlag.StringVar(&path, "path", "", "Destination path")
			subFlag.StringVar(&contentStr, "content", "", "Content to write; for multi-line content prefer --file or stdin")
			subFlag.StringVar(&fileStr, "file", "", "Read content from local file; use - to read stdin")
		case "read":
			subFlag.StringVar(&path, "path", "", "Source path")
		}

		subFlag.Parse(cmdArgs)

		// 智能指令补充：支持位置参数直接作为 msg/cmd
		positionalArgs := subFlag.Args()
		if len(positionalArgs) > 0 {
			combined := strings.Join(positionalArgs, " ")
			if command == "ask" && msgStr == "" {
				msgStr = combined
			} else if command == "exec" && cmdStr == "" {
				cmdStr = combined
			}
		}

		// 子命令中的 -session 回填全局值
		if subSession != "" && *sessionName == "" {
			*sessionName = subSession
		}

		if *sessionName == "" {
			fmt.Println("Error: -session 必传，请指定会话 ID 以隔离工作区 (例如: doops exec -session prod -target node1 -cmd \"...\")")
			os.Exit(1)
		}

		if target == "" {
			fmt.Println("Error: --target 必传，请指定目标服务器名称。")
			os.Exit(1)
		}
		requireConfig(configErr)

		// 指令/命令校验 (支持位置参数提取后)
		if command == "ask" && strings.TrimSpace(msgStr) == "" {
			fmt.Println("Error: 指令内容不能为空。用法: doops ask \"你的指令\"")
			os.Exit(1)
		}
		if command == "exec" && strings.TrimSpace(cmdStr) == "" {
			fmt.Println("Error: 命令内容不能为空。用法: doops exec \"ls -la\"")
			os.Exit(1)
		}
		if command == "read" && strings.TrimSpace(path) == "" {
			fmt.Println("Error: path 不能为空。doops read 只用于查看远端小文本文件。")
			os.Exit(1)
		}
		if command == "write" && strings.TrimSpace(path) == "" {
			fmt.Println("Error: path 不能为空。用法: doops write --path /remote/file --file ./local 或 echo data | doops write --path /remote/file")
			os.Exit(1)
		}

		server := findServer(servers, target)
		if server == nil {
			fmt.Printf("Error: Server '%s' not found.\n", target)
			os.Exit(1)
		}
		// Gateway targets are the standard targets and route exec/ask/read/write/info
		// over the gateway WebSocket. Only legacy direct targets need an SSH IP.
		if strings.TrimSpace(server.Gateway) == "" && strings.TrimSpace(server.IP) == "" {
			fmt.Printf("Error: target '%s' has neither a gateway nor an SSH IP configured.\n", target)
			os.Exit(1)
		}

		token := ResolveToken(server.Name, server.Token)

		client := NewMCPClient(*server, ss, *sessionName, *verbose)
		client.Token = token
		defer client.Close() // 确保持久 WebSocket 连接在命令结束后释放

		endpoint := fmt.Sprintf("%s:%s", server.IP, func() string {
			if server.Port == "" {
				return "42222"
			}
			return server.Port
		}())
		if server.Gateway != "" {
			cluster := server.Cluster
			if cluster == "" {
				cluster = "default"
			}
			instance := server.Instance
			if instance == "" {
				instance = server.Name
			}
			endpoint = fmt.Sprintf("%s -> %s/%s", server.Gateway, cluster, instance)
		}
		fmt.Printf("\033[93m[TARGETING]\033[0m Server: %s (%s), Use: %s\n",
			server.Name, endpoint, server.Use)

		if command == "exec" && background {
			if err := client.ExecBg(cmdStr); err != nil {
				fmt.Printf("Error: %v\n", err)
				os.Exit(1)
			}
			RecordHistory(server.Name, *sessionName, "bg "+cmdStr)
			break
		}

		var toolName string
		arguments := make(map[string]interface{})

		switch command {
		case "exec":
			toolName = "doops_shell"
			arguments["command"] = cmdStr
		case "ask":
			toolName = "doops_agent_prompt"
			arguments["instruction"] = msgStr
			if modelStr != "" {
				arguments["model"] = modelStr
			}
		case "write":
			toolName = "doops_file_write"
			arguments["path"] = path
			content, err := resolveWriteContent(contentStr, fileStr, subFlag.Args(), os.Stdin)
			if err != nil {
				fmt.Printf("Error: %v\n", err)
				os.Exit(1)
			}
			arguments["content"] = content
		case "read":
			toolName = "doops_file_read"
			arguments["path"] = path
		case "info":
			toolName = "doops_node_info"
		}

		if err := client.Call(toolName, arguments); err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}

		// Log History
		cmdLog := toolName
		if toolName == "doops_shell" {
			cmdLog = cmdStr
		} else if toolName == "doops_agent_prompt" {
			cmdLog = msgStr
		} else if toolName == "doops_file_write" || toolName == "doops_file_read" {
			cmdLog = fmt.Sprintf("%s %s", toolName, path)
		}
		RecordHistory(server.Name, *sessionName, cmdLog)

	case "bash":
		var target string
		subFlag := flag.NewFlagSet("bash", flag.ExitOnError)
		subFlag.StringVar(&target, "target", "", "Target server name")
		subFlag.Parse(cmdArgs)

		if target == "" {
			subFlag.Usage()
			os.Exit(1)
		}
		requireConfig(configErr)

		server := findServer(servers, target)
		if server == nil {
			fmt.Printf("Error: Server '%s' not found.\n", target)
			os.Exit(1)
		}
		if strings.TrimSpace(server.Gateway) != "" {
			fmt.Printf("Error: target '%s' is gateway-only; interactive SSH bash is not supported. Use doops -session <id> exec --target %s --cmd '<command>'.\n", target, target)
			os.Exit(1)
		}
		if strings.TrimSpace(server.IP) == "" {
			fmt.Printf("Error: target '%s' has no SSH IP configured.\n", target)
			os.Exit(1)
		}

		fmt.Printf("\033[93m[TARGETING]\033[0m Interactive Bash on %s (%s)\n", server.Name, server.IP)
		fmt.Println("Attempting SSH-based interactive shell...")

		// 使用 Server 配置中的 SSH 参数
		sshUser := server.GetSSHUser()
		sshPort := server.GetSSHPort()
		// Secure by default: accept-new records unknown host keys but refuses
		// changed keys (MITM). DOOPS_SSH_INSECURE=1 fully disables verification.
		strictOpt := "StrictHostKeyChecking=accept-new"
		if strings.TrimSpace(os.Getenv("DOOPS_SSH_INSECURE")) == "1" {
			strictOpt = "StrictHostKeyChecking=no"
		}
		sshCmd := exec.Command("ssh", "-o", strictOpt, "-p", sshPort, fmt.Sprintf("%s@%s", sshUser, server.IP))
		sshCmd.Stdin = os.Stdin
		sshCmd.Stdout = os.Stdout
		sshCmd.Stderr = os.Stderr
		if err := sshCmd.Run(); err != nil {
			fmt.Printf("SSH session failed or not available: %v\n", err)
			fmt.Println("Fallback to doops_shell is not yet fully interactive. Please use 'exec --cmd /bin/bash' for one-off commands.")
			os.Exit(1)
		}

	case "install":
		var name, ip, sshUser, sshPassword, sshPort, agentPort, binaryPath, agentToken string
		installFlag := flag.NewFlagSet("install", flag.ExitOnError)
		installFlag.StringVar(&name, "name", "", "Node name")
		installFlag.StringVar(&ip, "ip", "", "Node IP")
		installFlag.StringVar(&sshUser, "ssh-user", "", "SSH user for one-time bootstrap")
		installFlag.StringVar(&sshPassword, "ssh-password", "", "SSH password for one-time bootstrap")
		installFlag.StringVar(&sshPort, "ssh-port", "22", "SSH port for one-time bootstrap")
		installFlag.StringVar(&agentToken, "agent-token", "", "Legacy local /ws token for SSH bootstrap only; not a gateway credential")
		installFlag.StringVar(&agentPort, "agent-port", "42222", "Agent port")
		installFlag.StringVar(&binaryPath, "binary", "", "Path to agent binary (for manual deploy)")
		localFlag := installFlag.Bool("local", false, "Run agent in local/bare-metal mode (skip nsenter docker isolation)")

		installFlag.Parse(cmdArgs)

		if name == "" || ip == "" || sshUser == "" || sshPassword == "" {
			installFlag.Usage()
			os.Exit(1)
		}

		if err := InstallAgent(name, ip, sshUser, sshPassword, sshPort, agentPort, binaryPath, *localFlag, agentToken, llmConfig); err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}
		RecordHistory(name, "system", "install-agent")

	case "history":
		historyPath := historyLogPath()

		data, err := os.ReadFile(historyPath)
		if err != nil {
			fmt.Println("No history found.")
			return
		}
		fmt.Println("--- Command History ---")
		fmt.Print(string(data))

	case "login":
		var target, token, gateway, username, password, label string
		loginFlag := flag.NewFlagSet("login", flag.ExitOnError)
		loginFlag.StringVar(&target, "target", "", "Target server name")
		loginFlag.StringVar(&token, "token", "", "Gateway user token (optional; username/password login can issue one)")
		loginFlag.StringVar(&gateway, "gateway", "", "Gateway URL")
		loginFlag.StringVar(&username, "username", "", "Gateway username")
		loginFlag.StringVar(&password, "password", "", "Gateway password")
		loginFlag.StringVar(&label, "name", "", "Token label for gateway login")
		loginFlag.Parse(cmdArgs)

		if target == "" {
			loginFlag.Usage()
			os.Exit(1)
		}
		requireConfig(configErr)

		server := findServer(servers, target)
		if server == nil {
			fmt.Printf("Error: Server '%s' not found.\n", target)
			os.Exit(1)
		}

		isGateway := strings.TrimSpace(server.Gateway) != "" || strings.TrimSpace(gateway) != ""
		if !isGateway {
			fmt.Printf("Error: target '%s' is a legacy direct target. Standard login only supports gateway targets.\n", target)
			os.Exit(1)
		}
		if isGateway {
			if gateway == "" {
				gateway = server.Gateway
			}
			if token == "" {
				if strings.TrimSpace(username) == "" {
					fmt.Print("Gateway username: ")
					fmt.Scanln(&username)
				}
				if strings.TrimSpace(password) == "" {
					fmt.Printf("Enter gateway password for %s: ", username)
					bytePassword, err := term.ReadPassword(int(syscall.Stdin))
					fmt.Println()
					if err != nil {
						fmt.Printf("Error reading password: %v\n", err)
						os.Exit(1)
					}
					password = string(bytePassword)
				}
				if label == "" {
					label = target
				}
				newToken, err := GatewayLogin(gateway, username, password, label)
				if err != nil {
					fmt.Printf("Error logging in to gateway: %v\n", err)
					os.Exit(1)
				}
				token = newToken
			}
		}

		auth, _ := LoadAuth()
		auth.Set(server.Name, token)
		if err := auth.Save(); err != nil {
			fmt.Printf("Error saving credentials: %v\n", err)
			os.Exit(1)
		}
		if isGateway {
			fmt.Printf("Successfully cached gateway token for %s\n", server.Name)
		}

	case "logout":
		var target string
		logoutFlag := flag.NewFlagSet("logout", flag.ExitOnError)
		logoutFlag.StringVar(&target, "target", "", "Target server name")
		logoutFlag.Parse(cmdArgs)

		if target == "" {
			logoutFlag.Usage()
			os.Exit(1)
		}

		if configErr == nil {
			if server := findServer(servers, target); server != nil {
				target = server.Name
			}
		}
		auth, _ := LoadAuth()
		auth.Remove(target)
		auth.Save()
		fmt.Printf("Logged out from %s\n", target)

	case "add":
		var name, aliases, ip, port, use, token, gateway, cluster, instance string
		addFlag := flag.NewFlagSet("add", flag.ExitOnError)
		addFlag.StringVar(&name, "name", "", "Node name (required)")
		addFlag.StringVar(&aliases, "aliases", "", "Comma separated aliases, e.g. jm,jy,oilan")
		addFlag.StringVar(&aliases, "alias", "", "Comma separated aliases, e.g. jm,jy,oilan")
		addFlag.StringVar(&ip, "ip", "", "Legacy direct node IP; do not use for standard gateway targets")
		addFlag.StringVar(&port, "port", "42222", "Agent port")
		addFlag.StringVar(&use, "use", "Manually added node", "Description")
		addFlag.StringVar(&token, "token", "", "Gateway user token")
		addFlag.StringVar(&gateway, "gateway", "", "Public gateway URL for reverse tunnel mode")
		addFlag.StringVar(&cluster, "cluster", "default", "Cluster name for reverse tunnel mode")
		addFlag.StringVar(&instance, "instance", "", "Agent instance name for reverse tunnel mode")

		addFlag.Parse(cmdArgs)

		if strings.TrimSpace(ip) != "" {
			fmt.Println("Error: --ip is legacy direct mode and is not allowed for standard config. Use --gateway --cluster --instance.")
			os.Exit(1)
		}
		if name == "" || gateway == "" {
			addFlag.Usage()
			os.Exit(1)
		}
		if strings.TrimSpace(gateway) == "" {
			fmt.Println("Error: --gateway is required. Standard targets must use gateway mode.")
			os.Exit(1)
		}
		if strings.TrimSpace(token) == "" {
			if strings.TrimSpace(gateway) != "" {
				fmt.Println("Error: --token is required. For gateway mode, use a gateway user token.")
			} else {
				fmt.Println("Error: --token is required. Standard targets use a gateway user token.")
			}
			os.Exit(1)
		}
		if configErr != nil {
			servers = nil
		}

		newServer := Server{
			Name:     name,
			Aliases:  normalizeAliases([]string{aliases}),
			IP:       ip,
			Port:     port,
			Use:      use,
			Token:    token,
			Gateway:  strings.TrimSpace(gateway),
			Cluster:  strings.TrimSpace(cluster),
			Instance: strings.TrimSpace(instance),
		}
		if newServer.Instance == "" {
			newServer.Instance = name
		}

		updated := false
		for i, s := range servers {
			if serverMatchesTarget(s, name) {
				servers[i] = newServer
				updated = true
				break
			}
		}
		if conflict := findAliasConflict(servers, newServer); conflict != "" {
			fmt.Printf("Error: alias/name conflict with existing target '%s'.\n", conflict)
			os.Exit(1)
		}
		if !updated {
			servers = append(servers, newServer)
		}
		if err := saveServers(servers); err != nil {
			fmt.Printf("Error saving config: %v\n", err)
			os.Exit(1)
		}
		if updated {
			fmt.Printf("✅ Node '%s' updated successfully.\n", name)
		} else {
			fmt.Printf("✅ Gateway target '%s' (%s/%s via %s) added successfully.\n", name, newServer.Cluster, newServer.Instance, gateway)
		}

	case "push":
		var target, src, dest string
		var dryRun bool
		var pushSession string
		pushFlag := flag.NewFlagSet("push", flag.ExitOnError)
		pushFlag.StringVar(&target, "target", "", "Target server name")
		pushFlag.StringVar(&src, "src", ".", "Local source directory")
		pushFlag.StringVar(&dest, "dest", "", "Remote destination (用于推导 session 隔离标识)")
		pushFlag.BoolVar(&dryRun, "dry-run", false, "Preview only, don't actually sync")
		pushFlag.StringVar(&pushSession, "session", "", "Session ID (can also be set before subcommand)")
		pushFlag.Parse(cmdArgs)

		// 子命令中的 -session 回填全局值
		if pushSession != "" && *sessionName == "" {
			*sessionName = pushSession
		}

		if target == "" {
			fmt.Println("Error: --target is required")
			pushFlag.Usage()
			os.Exit(1)
		}
		requireConfig(configErr)

		server := findServer(servers, target)
		if server == nil {
			fmt.Printf("Error: Server '%s' not found.\n", target)
			os.Exit(1)
		}

		if err := Push(*server, src, dest, dryRun, nil, *sessionName); err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}
		RecordHistory(server.Name, *sessionName, fmt.Sprintf("push %s -> %s", src, dest))

	case "pull":
		var target, dest string
		var pullSession string
		pullFlag := flag.NewFlagSet("pull", flag.ExitOnError)
		pullFlag.StringVar(&target, "target", "", "Target server name")
		pullFlag.StringVar(&dest, "dest", "", "Local destination directory (default: ./<session>)")
		pullFlag.StringVar(&pullSession, "session", "", "Session ID (can also be set before subcommand)")
		pullFlag.Parse(cmdArgs)

		if pullSession != "" && *sessionName == "" {
			*sessionName = pullSession
		}
		if target == "" {
			fmt.Println("Error: --target is required")
			pullFlag.Usage()
			os.Exit(1)
		}
		if *sessionName == "" {
			fmt.Println("Error: -session 必传，请指定要拉取的远端工作区")
			os.Exit(1)
		}
		requireConfig(configErr)

		server := findServer(servers, target)
		if server == nil {
			fmt.Printf("Error: Server '%s' not found.\n", target)
			os.Exit(1)
		}
		if err := Pull(*server, dest, *sessionName); err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}
		RecordHistory(server.Name, *sessionName, fmt.Sprintf("pull -> %s", dest))

	case "clean":
		var target, workspace, cleanSession string
		cleanFlag := flag.NewFlagSet("clean", flag.ExitOnError)
		cleanFlag.StringVar(&target, "target", "", "Target server name")
		cleanFlag.StringVar(&workspace, "workspace", "", "Workspace name to clean")
		cleanFlag.StringVar(&cleanSession, "session", "", "Session ID (can also be set before subcommand)")
		cleanFlag.Parse(cmdArgs)
		if cleanSession != "" && *sessionName == "" {
			*sessionName = cleanSession
		}

		if target == "" || workspace == "" {
			fmt.Println("Error: --target and --workspace are required")
			cleanFlag.Usage()
			os.Exit(1)
		}
		if *sessionName == "" {
			fmt.Println("Error: -session 必传，请指定会话 ID")
			os.Exit(1)
		}
		requireConfig(configErr)

		server := findServer(servers, target)
		if server == nil {
			fmt.Printf("Error: Server '%s' not found.\n", target)
			os.Exit(1)
		}

		token := ResolveToken(server.Name, server.Token)
		client := NewMCPClient(*server, ss, *sessionName, *verbose)
		client.Token = token
		defer client.Close()

		if err := Clean(client, workspace); err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}
		RecordHistory(server.Name, *sessionName, fmt.Sprintf("clean workspace=%s", workspace))

	case "upgrade":
		var target, gateway, token, cluster, instance, image, mode, namespace, workload, container, upgradeSession string
		var dryRun bool
		upgradeFlag := flag.NewFlagSet("upgrade", flag.ExitOnError)
		upgradeFlag.StringVar(&target, "target", "", "Configured gateway target whose token/gateway should be used")
		upgradeFlag.StringVar(&gateway, "gateway", "", "Gateway URL")
		upgradeFlag.StringVar(&token, "token", "", "Gateway user token")
		upgradeFlag.StringVar(&cluster, "cluster", "", "Target cluster; defaults to the configured --target")
		upgradeFlag.StringVar(&instance, "instance", "", "Target instance; defaults to the configured --target")
		upgradeFlag.StringVar(&image, "image", "", "New doops-agent image")
		upgradeFlag.StringVar(&mode, "mode", "auto", "Upgrade mode: auto,k8s,docker")
		upgradeFlag.StringVar(&namespace, "namespace", "", "Kubernetes namespace for DaemonSet mode")
		upgradeFlag.StringVar(&workload, "workload", "", "Kubernetes workload, e.g. daemonset/doops-agent")
		upgradeFlag.StringVar(&container, "container", "", "Container name")
		upgradeFlag.StringVar(&upgradeSession, "session", "", "Session ID")
		upgradeFlag.BoolVar(&dryRun, "dry-run", false, "Preview upgrade command only")
		upgradeFlag.Parse(cmdArgs)
		if upgradeSession != "" && *sessionName == "" {
			*sessionName = upgradeSession
		}
		if *sessionName == "" {
			*sessionName = fmt.Sprintf("upgrade_%d", time.Now().Unix())
		}
		if image == "" {
			fmt.Println("Error: --image is required")
			upgradeFlag.Usage()
			os.Exit(1)
		}
		var base Server
		if target != "" {
			requireConfig(configErr)
			server := findServer(servers, target)
			if server == nil {
				fmt.Printf("Error: Server '%s' not found.\n", target)
				os.Exit(1)
			}
			base = *server
		} else {
			base = Server{Name: "gateway", Gateway: gateway, Token: token}
		}
		if err := UpgradeAgents(base, UpgradeOptions{
			Gateway:   gateway,
			Token:     token,
			Cluster:   cluster,
			Instance:  instance,
			Image:     image,
			Mode:      mode,
			Namespace: namespace,
			Workload:  workload,
			Container: container,
			DryRun:    dryRun,
			Session:   *sessionName,
		}, *verbose); err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}

	case "check":
		var target, deployment, namespace, image, checkSession string
		checkFlag := flag.NewFlagSet("check", flag.ExitOnError)
		checkFlag.StringVar(&target, "target", "", "Target server name")
		checkFlag.StringVar(&deployment, "deployment", "", "Deployment name (e.g. openwork)")
		checkFlag.StringVar(&namespace, "namespace", "default", "Kubernetes namespace (e.g. oilan-system)")
		checkFlag.StringVar(&image, "image", "", "Image name (e.g. registry.example.com/oilan-system/openwork)")
		checkFlag.StringVar(&checkSession, "session", "", "Session ID (can also be set before subcommand)")
		checkFlag.Parse(cmdArgs)
		if checkSession != "" && *sessionName == "" {
			*sessionName = checkSession
		}

		if target == "" || deployment == "" || image == "" {
			fmt.Println("Error: --target, --deployment and --image are required")
			checkFlag.Usage()
			os.Exit(1)
		}
		if *sessionName == "" {
			fmt.Println("Error: -session 必传，请指定会话 ID")
			os.Exit(1)
		}
		requireConfig(configErr)

		server := findServer(servers, target)
		if server == nil {
			fmt.Printf("Error: Server '%s' not found.\n", target)
			os.Exit(1)
		}

		token := ResolveToken(server.Name, server.Token)
		client := NewMCPClient(*server, ss, *sessionName, *verbose)
		client.Token = token
		defer client.Close()

		if err := CheckDeployment(client, namespace, deployment, image); err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}
		RecordHistory(server.Name, *sessionName, fmt.Sprintf("check %s/%s", namespace, deployment))

	default:
		fmt.Printf("Unknown command: %s\n", command)
		flag.Usage()
		os.Exit(1)
	}
}

func findServer(servers []Server, name string) *Server {
	for _, s := range servers {
		if serverMatchesTarget(s, name) {
			return &s
		}
	}
	return nil
}

func serverMatchesTarget(server Server, target string) bool {
	target = strings.TrimSpace(target)
	if target == "" {
		return false
	}
	if server.Name == target {
		return true
	}
	for _, alias := range server.Aliases {
		if alias == target {
			return true
		}
	}
	return false
}

func findAliasConflict(servers []Server, candidate Server) string {
	names := append([]string{candidate.Name}, candidate.Aliases...)
	for _, server := range servers {
		if server.Name == candidate.Name {
			continue
		}
		for _, name := range names {
			if serverMatchesTarget(server, name) {
				return server.Name
			}
		}
	}
	return ""
}

func runAdminOperationsList(args []string, servers []Server, configErr error) {
	gateway, token := resolveAdminGatewayFlags("admin operations list", args, servers, configErr, nil)
	ops, err := GatewayAdminOperationsList(gateway, token)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("%-12s %-8s %-18s %-18s %-8s %-16s %-8s %-8s %s\n", "ID", "KIND", "CLUSTER", "INSTANCE", "ACTION", "SESSION", "AGE", "USER", "SUMMARY")
	fmt.Println(strings.Repeat("-", 132))
	for _, op := range ops {
		summary := op.CommandSummary
		if len(summary) > 48 {
			summary = summary[:45] + "..."
		}
		fmt.Printf("%-12s %-8s %-18s %-18s %-8s %-16s %-8ds %-8s %s\n", op.ID, op.Kind, op.Cluster, op.Instance, op.Action, op.Session, op.AgeSeconds, op.UserID, summary)
	}
}

func runAdminOperationCancel(args []string, servers []Server, configErr error) {
	var opID string
	gateway, token := resolveAdminGatewayFlags("admin operations cancel", args, servers, configErr, &opID)
	if err := GatewayAdminOperationCancel(gateway, token, opID); err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Canceled operation %s\n", opID)
}

func resolveAdminGatewayFlags(name string, args []string, servers []Server, configErr error, opID *string) (string, string) {
	var target, gateway, token string
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	fs.StringVar(&target, "target", "", "Configured gateway target whose token/gateway should be used")
	fs.StringVar(&gateway, "gateway", "", "Gateway URL")
	fs.StringVar(&token, "token", "", "Gateway admin user token")
	if opID != nil {
		fs.StringVar(opID, "id", "", "Active operation id")
	}
	fs.Parse(args)
	if target != "" {
		requireConfig(configErr)
		server := findServer(servers, target)
		if server == nil {
			fmt.Printf("Error: Server '%s' not found.\n", target)
			os.Exit(1)
		}
		if gateway == "" {
			gateway = server.Gateway
		}
		if token == "" {
			token = ResolveToken(server.Name, server.Token)
		}
	}
	if gateway == "" {
		fmt.Println("Error: --gateway or --target is required")
		fs.Usage()
		os.Exit(1)
	}
	if opID != nil && strings.TrimSpace(*opID) == "" {
		fmt.Println("Error: --id is required")
		fs.Usage()
		os.Exit(1)
	}
	return gateway, token
}

func requireConfig(err error) {
	if err == nil {
		return
	}
	fmt.Printf("Error: no doops node config found: %v\n", err)
	fmt.Println("Add a gateway target with: doops add --name <name> --gateway <gateway-url> --cluster <cluster> --instance <instance> --token <gateway-user-token>")
	os.Exit(1)
}

func ResolveToken(targetName string, serverToken string) string {
	// 1. Load from authStore (Check if logged in)
	auth, _ := LoadAuth()
	if p := auth.Get(targetName); p != "" {
		return p
	}
	// 2. If token is in config, use it
	if serverToken != "" {
		return serverToken
	}
	// 3. Prompt
	fmt.Printf("Gateway user token required for %s. Enter token: ", targetName)
	byteToken, err := term.ReadPassword(int(syscall.Stdin))
	fmt.Println()
	if err != nil {
		return ""
	}
	return string(byteToken)
}
