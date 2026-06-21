package main

import (
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
专为 AI Agent 设计的声明式运维工具。遵循 DevOps 原则：无缝推流工作区、意图智能探索部署，并将成果固化在本地。

配置文件路径: ~/.agent/skills/doops/config.json（唯一默认配置；DOOPS_CONFIG 可显式覆盖）

用法:
  doops [选项] <命令> [参数]

可用命令:
  bash        执行交互式 Shell (通过 SSH 转发 PTY)
  list        列出所有已知服务器 (查看名称、IP、用途)
  targets     查看 gateway 当前在线的 cluster/instance
  exec        在目标节点工作区执行 Shell 命令 (用于确定性的日常流水线执行)
  ask         发送自然语言指令 (由边缘端智能体自行分析底层环境、重试排障，并要求留下固化脚本)
  read        查看目标节点上的小文本文件（不用于下载大文件/二进制）
  write       写入文件到目标服务器
  info        获取节点系统信息 (CPU/内存/磁盘)
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

标准 DevOps 闭环示例 (探索式部署与固化):
  # 标准入口只有 Gateway 模式：doops add --name prod --gateway https://gw.example.com --cluster prod --instance master-1 --token <gateway-user-token>
  # 查看 gateway 在线目标：doops targets --target prod

  # 1. 增量输送代码与物料入远端沙盒
  doops -session test_ops push --target api-node --src .
  
  # 2. 意图驱动：让远端 Agent 摸索适配环境，并要求留下固化版 deploy.sh
  doops -session test_ops ask --target api-node --msg "帮我把这里的代码用 BuildKit 打包推送，重启 deployment/app-gw。遇到报错自己解决。成功后留下 deploy.sh 供日后使用。"
  
  # 3. 小文本查看：将远端验证跑通的部署脚本查看/回传本地库并 Commit 固化 (Single Source of Truth)
  doops -session test_ops read --target api-node --path /root/ws/test_ops/deploy.sh > ./deploy.sh
  
  # 4. 闭环验证：未来的日常部署，只需无脑触发确定流水线
  doops -session test_ops exec --target api-node --cmd "cd /root/ws/test_ops && ./deploy.sh"
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

	case "exec", "ask", "write", "read", "info":
		var target, cmdStr, msgStr, path, modelStr, contentStr, fileStr string
		var subSession string
		subFlag := flag.NewFlagSet(command, flag.ExitOnError)
		subFlag.StringVar(&target, "target", "", "Target server name")
		subFlag.StringVar(&subSession, "session", "", "Session ID (can also be set before subcommand)")

		switch command {
		case "exec":
			subFlag.StringVar(&cmdStr, "cmd", "", "Command to execute")
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

		// [P2 自动固化 Post-Hook] exec/ask 成功后, 自动检查远端是否生成了 deploy.sh 并回传本地
		if (command == "exec" || command == "ask") && *sessionName != "" {
			remoteDeploy := fmt.Sprintf("/root/ws/%s/deploy.sh", *sessionName)
			pullClient := NewMCPClient(*server, ss, *sessionName, false)
			pullClient.Token = token

			readArgs := map[string]interface{}{"path": remoteDeploy}
			// 使用静默读取：如果文件不存在则跳过
			content, readErr := pullClient.CallAndCapture("doops_file_read", readArgs)
			pullClient.Close()

			if readErr == nil && len(content) > 10 && strings.Contains(content, "#!/bin/bash") {
				localPath := "./deploy.sh"
				if writeErr := os.WriteFile(localPath, []byte(content), 0755); writeErr == nil {
					fmt.Printf("\n\033[92m✅ [自动固化] deploy.sh 已从远端回传至 %s\033[0m\n", localPath)
				}
			}
		}

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
		upgradeFlag.StringVar(&cluster, "cluster", "*", "Target cluster or *")
		upgradeFlag.StringVar(&instance, "instance", "*", "Target instance or *")
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
