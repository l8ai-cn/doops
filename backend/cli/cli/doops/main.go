package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"
)

const cliContractVersion = "doops.sh/cli-contract/v3"

var (
	cliBuildVersion = "dev"
	cliBuildCommit  = "unknown"
)

func main() {
	verbose := flag.Bool("verbose", false, "Enable verbose logging")
	sessionName := flag.String("session", "", "Session/Task ID for operation context; generated when omitted")
	printContract := flag.Bool("contract", false, "Print the machine-readable CLI contract version")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `
Doops 分布式服务器管理工具 (doops.sh CLI)
专为 AI Agent 设计的自然语言运维通道。发布请求须明确 target、release、namespace 和 Chart；其它业务信息按任务需要提供。

配置文件路径: ~/.agent/skills/doops/config.json（唯一默认配置；DOOPS_CONFIG 可显式覆盖）

发布对象:
  普通业务 workload: ask --apply，不要求本机 kubeconfig 或先执行 push
  已具备自升级控制器的 Helm Agent 镜像: upgrade，由集群内 Job 执行
  K8S 升级需 upgradeAudit v1；旧 Agent 首升须单独审查经 Gateway exec 提交的独立 Job，见 BUILD_DEPLOY_SOP
  已有独立 Docker Agent: backend/scripts/release-host-agent.sh，正式外部宿主控制，不是 upgrade fallback，不要求迁 K8S
    必须显式提供 SSH --port、不可变镜像 digest、CI source commit 和 release；脚本 --dry-run 是真实只读预检
    严格校验主机密钥；可引用已认证私有 SSH socket，不接收密码/token；保留配置和旧容器，失败外部回滚
    完整参数与验收: backend/docs/BUILD_DEPLOY_SOP.md；禁止当前 Agent exec/Helm 替换自己
  Gateway 控制面: scripts/deploy-gateway.sh，不通过 target 发布自身

用法:
  doops [选项] <命令> [参数]

可用命令:
  list        列出已配置的 Gateway target 路由
  sync        把当前登录能看到的在线 target 补进本机路由；不改已有路由，不删除离线路由
  targets     查看 Gateway 操作状态；--admission 回读 do-agent 内部名额与队列
  capabilities 检查 target Agent 的协议能力
  exec        在目标 Agent 进程目录执行 Shell 命令；工作目录请显式 cd
  ssh         通过结构化 argv 代理 SSH，避免多层命令引号被远端 shell 重解析
  ask         发送自然语言指令 (由边缘端智能体完成受控的通用运维协作)
  sql         通过统一 Gateway/Agent 入口使用 DoSql 数据库 skill
  web         通过统一 Gateway/Agent 入口使用 DoWeb 网站 skill
  image       通用 OCI 镜像构建与复制（目标 Agent 执行）
  read        查看目标节点上的小文本文件（不用于下载大文件/二进制）
  write       写入文件到目标服务器
  info        获取节点系统信息 (CPU/内存/磁盘)
  k8s         受限 Kubernetes 观察与运维入口
  credential  管理 Gateway 凭据元数据与版本
  registry    通用 OCI 镜像 registry sync（DoOps 目标机执行）
  version     查看 DoOps 发布身份（release tag 和精确提交）
  guide       按场景打印有序命令（onboard/diagnose/release/credential/...）
  history     查看本地操作历史，不含 ask 或 exec 正文
  session     生成并输出一个新的唯一 Session ID
  push        极速增量推送本地代码到远端沙盒 (固定至 /root/ws/$SESSION)
  pull        下载已上传的 COS 快照；--storage gateway 获取远端当前工作区
  clean       清理远端节点上的工作区隔离沙盒
  admin       gateway 管理命令：用户、target 授权和 API key
  upgrade     由独立 Job 预检、升级或收编 K8S Agent；记录操作编号与验收证据，--dry-run 只预览
  add         添加 gateway target
  remove      仅在 target 确认离线时删除本机 gateway target 配置
  login       登录 Gateway；--gateway <name|url> 指定 Gateway（登录属于 Gateway 而不是 target）；--target 仍可作为别名
  logout      仅删除本机 Gateway 登录记录（--target 或 --gateway 指定 Gateway），不撤销 token
  check       检查线上服务容器配置的镜像与最新构建是否一致

选项:
  -verbose    启用详细日志
  -session    会话/任务ID (可省略，省略时自动生成并打印到 stderr；复用同名即延续同一工作区)
  -contract   输出机器可读的 CLI 契约版本
  -help       显示此帮助信息

	自然语言发布示例:
	  # --apply 授权本次任务做变更；目标 Agent 按原生 Helm 处理请求。
	  doops -session release-<id> ask --target <target> --apply --msg "在 target <target> 的 namespace apps 安装或升级 release app，使用 chart https://charts.example/app.tgz。"
`)
	}
	flag.Parse()
	if *printContract {
		fmt.Println(cliContractVersion)
		return
	}

	args := flag.Args()
	if len(args) == 0 {
		flag.Usage()
		os.Exit(0)
	}

	command := args[0]
	cmdArgs := args[1:]

	ss := NewSessionStore()
	servers, _, configErr := LoadConfig()

	switch command {
	case "registry":
		if err := runRegistryCommand(cmdArgs, *sessionName, servers, configErr, os.Stdout); err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}
	case "version":
		fmt.Printf("release=%s commit=%s\n", cliBuildVersion, cliBuildCommit)

	case "capabilities":
		var target, required string
		capabilitiesFlag := flag.NewFlagSet("capabilities", flag.ExitOnError)
		capabilitiesFlag.StringVar(&target, "target", "", "Configured Gateway target")
		capabilitiesFlag.StringVar(&required, "require", "", "Required protocol: structuredSSH, executionLifecycle")
		capabilitiesFlag.Parse(cmdArgs)
		if target == "" || required == "" {
			fmt.Println("Error: --target and --require are required")
			capabilitiesFlag.Usage()
			os.Exit(1)
		}
		if err := validateRequiredProtocol(required); err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}
		*sessionName = resolveSession(*sessionName)
		requireConfig(configErr)
		server := findServer(servers, target)
		if server == nil {
			fmt.Println(targetNotFound(servers, target))
			os.Exit(1)
		}
		token := mustResolveToken(*server)
		client := NewMCPClient(*server, ss, *sessionName, *verbose)
		client.Token = token
		defer client.Close()
		if err := client.RequireProtocol(required); err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("target=%s protocol=%s status=ready\n", target, required)

	case "session":
		fmt.Println(newSessionID())
		os.Exit(0)

	case "sync":
		if err := runSync(cmdArgs, servers, configErr, os.Stdout); err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}

	case "list":
		if configErr != nil {
			if os.IsNotExist(configErr) {
				fmt.Println("No doops nodes configured.")
				fmt.Println("First login (discovers visible targets): doops login --gateway <gateway-url>")
				fmt.Println("To pin a route first: doops add --name <name> --gateway <gateway-url> --cluster <cluster> --instance <instance>")
				return
			}
			fmt.Printf("Error loading config: %v\n", configErr)
			os.Exit(1)
		}
		if err := printServerList(os.Stdout, servers); err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}

	case "targets":
		var target string
		var admission bool
		targetsFlag := flag.NewFlagSet("targets", flag.ExitOnError)
		targetsFlag.StringVar(&target, "target", "", "Configured target to query")
		targetsFlag.BoolVar(&admission, "admission", false, "Read live do-agent turn permits and queues")
		targetsFlag.Parse(cmdArgs)
		if target == "" {
			fmt.Println("Error: --target is required")
			targetsFlag.Usage()
			os.Exit(1)
		}
		requireConfig(configErr)
		server := findServer(servers, target)
		if server == nil {
			fmt.Println(targetNotFound(servers, target))
			os.Exit(1)
		}
		token := mustResolveToken(*server)
		targets, err := fetchGatewayTargets(server.Gateway, token)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}
		targets = filterGatewayTargets(targets, server.Cluster, server.Instance)
		if len(targets) == 0 {
			fmt.Println(gatewayTargetUnavailableMessage(server.Cluster, server.Instance))
			os.Exit(1)
		}
		fmt.Printf("%-18s %-18s %-8s %-16s %-7s %-7s %-40s %-28s %-24s\n", "CLUSTER", "INSTANCE", "BUSY", "STATUS", "ACTIVE", "QUEUED", "RELEASE_COMMIT", "RESOURCES", "LAST_SEEN")
		fmt.Println(strings.Repeat("-", 178))
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
			releaseCommit := item.ReleaseCommit
			if releaseCommit == "" {
				releaseCommit = "<unknown>"
			}
			fmt.Printf("%-18s %-18s %-8v %-16s %-7d %-7d %-40s %-28s %-24s\n", item.Cluster, item.Instance, item.Busy, status, item.ActiveOps, item.QueuedOps, releaseCommit, resources, item.LastSeen.Format(time.RFC3339))
		}
		fmt.Println("ACTIVE/QUEUED count Gateway operations, not do-agent turn permits or prompt queues.")
		if admission {
			client := NewMCPClient(*server, ss, resolveSession(*sessionName), *verbose)
			client.Token = token
			result, err := client.CallAndCapture("doops_node_info", map[string]interface{}{"section": "admission"})
			client.Close()
			if err == nil {
				err = printAdmissionStatus(os.Stdout, result)
			}
			if err != nil {
				fmt.Printf("Error reading do-agent admission: %v\n", err)
				os.Exit(1)
			}
		}

	case "admin":
		if len(cmdArgs) >= 1 && cmdArgs[0] == "user" {
			runAdminUsers(cmdArgs[1:], servers, configErr)
			return
		}
		if len(cmdArgs) >= 1 && cmdArgs[0] == "grant" {
			runAdminGrants(cmdArgs[1:], servers, configErr)
			return
		}
		if len(cmdArgs) >= 1 && cmdArgs[0] == "proxy" {
			runAdminProxyRoutes(cmdArgs[1:], servers, configErr)
			return
		}
		if len(cmdArgs) < 2 {
			fmt.Println("Usage: doops admin user <list|create|password|disable> ...")
			fmt.Println("       doops admin grant <list|add|delete> ...")
			fmt.Println("       doops admin proxy <list|set|delete> ...")
			fmt.Println("Usage: doops admin token create --target <gateway-target> --user <username> [--name label] [--expires 720h] [--save-as target]")
			fmt.Println("       doops admin token list --target <gateway-target>")
			fmt.Println("       doops admin token revoke --target <gateway-target> --id <token-id>")
			os.Exit(1)
		}
		if cmdArgs[0] == "token" && cmdArgs[1] == "list" {
			runAdminTokensList(cmdArgs[2:], servers, configErr)
			return
		}
		if cmdArgs[0] == "token" && cmdArgs[1] == "revoke" {
			runAdminTokenRevoke(cmdArgs[2:], servers, configErr)
			return
		}
		if cmdArgs[0] != "token" || cmdArgs[1] != "create" {
			fmt.Println("Usage: doops admin user <list|create|password|disable> ...")
			fmt.Println("       doops admin grant <list|add|delete> ...")
			fmt.Println("       doops admin proxy <list|set|delete> ...")
			fmt.Println("Usage: doops admin token create --target <gateway-target> --user <username> [--name label] [--expires 720h] [--save-as target]")
			fmt.Println("       doops admin token list --target <gateway-target>")
			fmt.Println("       doops admin token revoke --target <gateway-target> --id <token-id>")
			os.Exit(1)
		}
		var target, user, label, expires, saveAs string
		adminFlag := flag.NewFlagSet("admin token create", flag.ExitOnError)
		adminFlag.StringVar(&target, "target", "", "Configured gateway target whose token/gateway should be used")
		adminFlag.StringVar(&user, "user", "", "Gateway user name to issue a token for")
		adminFlag.StringVar(&label, "name", "", "Token label")
		adminFlag.StringVar(&expires, "expires", "", "Optional token TTL, e.g. 720h")
		adminFlag.StringVar(&saveAs, "save-as", "", "Optional configured target name to cache the issued token for")
		adminFlag.Parse(cmdArgs[2:])
		if strings.TrimSpace(target) == "" {
			fmt.Println("Error: --target is required")
			adminFlag.Usage()
			os.Exit(1)
		}
		requireConfig(configErr)
		server := findServer(servers, target)
		if server == nil || strings.TrimSpace(server.Gateway) == "" {
			fmt.Println(targetNotFound(servers, target))
			os.Exit(1)
		}
		gateway := server.Gateway
		token := mustResolveToken(*server)
		if strings.TrimSpace(user) == "" {
			fmt.Println("Error: --user is required")
			adminFlag.Usage()
			os.Exit(1)
		}
		saveGateway := ""
		if strings.TrimSpace(saveAs) != "" {
			saveServer := findServer(servers, saveAs)
			if saveServer == nil || strings.TrimSpace(saveServer.Gateway) == "" {
				fmt.Println(targetNotFound(servers, saveAs))
				os.Exit(1)
			}
			if gatewayAuthKey(saveServer.Gateway) != gatewayAuthKey(gateway) {
				fmt.Println("Error: --save-as target belongs to a different gateway")
				os.Exit(1)
			}
			saveGateway = saveServer.Gateway
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
		fmt.Printf("token_type=%s\n", issued.TokenType)
		if issued.Username != "" {
			fmt.Printf("username=%s\n", issued.Username)
		}
		fmt.Printf("token_id=%s\n", issued.TokenID)
		fmt.Printf("token=%s\n", issued.Token)
		fmt.Println("warning=store this token now; gateway only keeps its hash")
		if saveGateway != "" {
			auth, err := LoadAuth()
			if err != nil {
				fmt.Printf("Error loading credentials: %v\n", err)
				os.Exit(1)
			}
			credential, err := issued.credential(issued.Token, "admin-token")
			if err != nil {
				fmt.Printf("Error validating issued token metadata: %v\n", err)
				os.Exit(1)
			}
			auth.SetGatewayRecord(saveGateway, credential)
			if err := auth.Save(); err != nil {
				fmt.Printf("Error saving issued token: %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("Cached issued token for gateway %s\n", gatewayAuthKey(saveGateway))
		}

	case "k8s":
		req, err := buildK8SRequest(cmdArgs)
		if errors.Is(err, errK8SHelp) {
			fmt.Println(k8sUsage)
			os.Exit(0)
		}
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}
		requireConfig(configErr)
		server := findServer(servers, req.Target)
		if server == nil {
			fmt.Println(targetNotFound(servers, req.Target))
			os.Exit(1)
		}

		*sessionName = resolveSession(*sessionName)
		token := mustResolveToken(*server)
		client := NewMCPClient(*server, ss, *sessionName, *verbose)
		client.Token = token
		defer client.Close()

		msg, err := runK8SRequest(client, req)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}
		if msg != "" {
			fmt.Println(msg)
		}
		RecordHistory(server.Name, *sessionName, fmt.Sprintf("k8s %s", req.Payload["operation"]))

	case "credential":
		if err := runCredentialCommand(cmdArgs, *sessionName, servers, configErr, os.Stdin, os.Stdout); err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}

	case "sql":
		if err := runSQLCommand(cmdArgs, *sessionName, servers, configErr, ss, *verbose); err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}

	case "web":
		if err := runWebCommand(cmdArgs, *sessionName, servers, configErr, ss, *verbose); err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}

	case "image":
		if err := runImageCommand(cmdArgs, *sessionName, servers, configErr, os.Stdout); err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}

	case "exec", "ssh", "ask", "write", "read", "info":
		var target, cmdStr, msgStr, path, modelStr, contentStr, fileStr, sshHost, storageMode, credentialName string
		var credentialRefs stringListFlag
		var databaseRefs stringListFlag
		var repositoryRefs stringListFlag
		var sshOptions stringListFlag
		var subSession string
		var apply, background bool
		subFlag := flag.NewFlagSet(command, flag.ExitOnError)
		subFlag.StringVar(&target, "target", "", "Target server name")
		subFlag.StringVar(&subSession, "session", "", "Session ID (can also be set before subcommand)")

		switch command {
		case "exec":
			subFlag.StringVar(&cmdStr, "cmd", "", "Command to execute")
			subFlag.BoolVar(&background, "bg", false, "Run through one streaming background task RPC until the process stops; requires executionLifecycle; no status polling or task retries")
		case "ssh":
			subFlag.StringVar(&sshHost, "host", "", "SSH destination, for example ops@app.internal")
			subFlag.Var(&sshOptions, "option", "Allowed SSH flag or key=value (repeatable, e.g. --option=BatchMode=yes)")
		case "ask":
			subFlag.StringVar(&msgStr, "msg", "", "Instruction")
			subFlag.StringVar(&modelStr, "model", "", "Model to use for this instruction (optional)")
			subFlag.BoolVar(&apply, "apply", false, "Allow the instruction to make the changes it requests")
			subFlag.Var(&credentialRefs, "credential", "Managed credential resource name (repeatable; requires --apply). A Helm credential bound to the target is used without this flag; name one to pick among several, or to add a registry, TLS or opaque credential")
			subFlag.Var(&databaseRefs, "database-ref", "Database asset reference for the conversation (repeatable)")
			subFlag.Var(&repositoryRefs, "repo-ref", "Code repository reference, optionally ID@branch (repeatable)")
		case "write":
			subFlag.StringVar(&path, "path", "", "Destination path")
			subFlag.StringVar(&contentStr, "content", "", "Content to write; for multi-line content prefer --file or stdin")
			subFlag.StringVar(&fileStr, "file", "", "Read content from local file; use - to read stdin")
			subFlag.StringVar(&storageMode, "storage", "cos", "Storage mode: cos (default, Gateway COS config) or gateway (local relay)")
			subFlag.StringVar(&credentialName, "credential", "", "Ignored for cos; COS settings come from the Gateway")
		case "read":
			subFlag.StringVar(&path, "path", "", "Source path")
			subFlag.StringVar(&storageMode, "storage", "cos", "Storage mode: cos (default, Gateway COS config) or gateway (local relay)")
			subFlag.StringVar(&credentialName, "credential", "", "Ignored for cos; COS settings come from the Gateway")
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

		*sessionName = resolveSession(*sessionName)

		if target == "" {
			fmt.Println("Error: --target 必传，请指定目标服务器名称。")
			os.Exit(1)
		}
		requireConfig(configErr)

		// 指令/命令校验 (支持位置参数提取后)
		if command == "ask" && strings.TrimSpace(msgStr) == "" {
			fmt.Println(withNext("Error: ask needs an instruction", "doops ask --target "+target+" --msg '<what to do>'", "doops guide diagnose"))
			os.Exit(1)
		}
		if command == "ask" && len(credentialRefs) != 0 && !apply {
			fmt.Println(withNext("Error: --credential is materialized only for a change task; add --apply", "doops ask --target "+target+" --apply --credential <name> --msg '<change>'"))
			os.Exit(1)
		}
		if command == "exec" && strings.TrimSpace(cmdStr) == "" {
			fmt.Println(withNext("Error: exec needs a command", "doops exec --target "+target+" --cmd '<cmd>'"))
			os.Exit(1)
		}
		if command == "exec" && looksLikeSSHCommand(cmdStr) {
			fmt.Println(withNext("Error: run nested SSH through doops ssh so remote quoting survives", sshCommandSuggestion(target, cmdStr)))
			os.Exit(1)
		}
		if command == "ssh" && strings.TrimSpace(sshHost) == "" {
			fmt.Println("Error: --host 必传。用法: doops ssh --target <t> --host <user@host> -- bash -lc '...'")
			os.Exit(1)
		}
		if command == "ssh" {
			normalizedOptions, err := normalizeSSHOptions(sshOptions)
			if err != nil {
				fmt.Printf("Error: %v\n", err)
				os.Exit(1)
			}
			sshOptions = normalizedOptions
		}
		if command == "read" && strings.TrimSpace(path) == "" {
			fmt.Println(withNext("Error: read needs --path (small text files; use pull for large or binary files)", "doops read --target "+target+" --path <remote-file>"))
			os.Exit(1)
		}
		if command == "write" && strings.TrimSpace(path) == "" {
			fmt.Println(withNext("Error: write needs --path", "doops write --target "+target+" --path <remote-file> --file <local-file>"))
			os.Exit(1)
		}
		storageMode, storageErr := normalizeStorageMode(storageMode)
		if storageErr != nil && (command == "read" || command == "write") {
			fmt.Printf("Error: %v\n", storageErr)
			os.Exit(1)
		}

		server := findServer(servers, target)
		if server == nil {
			fmt.Println(targetNotFound(servers, target))
			os.Exit(1)
		}
		token := mustResolveToken(*server)

		client := NewMCPClient(*server, ss, *sessionName, *verbose)
		client.Token = token
		defer client.Close() // 确保持久 WebSocket 连接在命令结束后释放

		endpoint := fmt.Sprintf("%s -> %s/%s", server.Gateway, server.Cluster, server.Instance)
		fmt.Printf("\033[93m[TARGETING]\033[0m Server: %s (%s), Use: %s\n",
			server.Name, endpoint, server.Use)

		if command == "exec" && background {
			if err := client.ExecBg(cmdStr); err != nil {
				fmt.Printf("Error: %v\n", err)
				os.Exit(1)
			}
			RecordHistory(server.Name, *sessionName, "exec --bg")
			break
		}

		var toolName string
		arguments := make(map[string]interface{})

		switch command {
		case "exec":
			toolName = "doops_shell"
			arguments["command"] = cmdStr
		case "ssh":
			toolName = "doops_ssh"
			arguments["host"] = sshHost
			arguments["options"] = []string(sshOptions)
			arguments["command"] = positionalArgs
		case "ask":
			toolName = "doops_agent_prompt"
			instruction, err := appendConversationReferences(msgStr, []string(databaseRefs), []string(repositoryRefs))
			if err != nil {
				fmt.Printf("Error: %v\n", err)
				os.Exit(1)
			}
			arguments["instruction"] = instruction
			if apply {
				arguments["operation"] = "apply"
			}
			if len(credentialRefs) != 0 {
				arguments["credential_refs"] = []string(credentialRefs)
			}
			if modelStr != "" {
				arguments["model"] = modelStr
			}
		case "write":
			content, err := resolveWriteContent(contentStr, fileStr, subFlag.Args(), os.Stdin)
			if err != nil {
				fmt.Printf("Error: %v\n", err)
				os.Exit(1)
			}
			if storageMode == "cos" {
				result, err := runCOSWrite(*server, *sessionName, credentialName, token, path, []byte(content), nil)
				if err != nil {
					fmt.Printf("Error: %v\n", err)
					os.Exit(1)
				}
				fmt.Printf("storage: cos\nbucket: %s\nkey: %s\npresigned_get: %s\n", result.Bucket, result.Key, result.URL)
				RecordHistory(server.Name, *sessionName, "doops_cos write "+path)
				break
			}
			toolName = "doops_file_write"
			arguments["path"] = path
			arguments["content"] = content
		case "read":
			if storageMode == "cos" {
				result, err := runCOSRead(*server, *sessionName, credentialName, token, path, nil)
				if err != nil {
					fmt.Printf("Error: %v\n", err)
					os.Exit(1)
				}
				fmt.Printf("storage: cos\nbucket: %s\nkey: %s\npresigned_get: %s\n", result.Bucket, result.Key, result.URL)
				RecordHistory(server.Name, *sessionName, "doops_cos read "+path)
				break
			}
			toolName = "doops_file_read"
			arguments["path"] = path
		case "info":
			toolName = "doops_node_info"
		}

		if (command == "read" || command == "write") && storageMode == "cos" {
			break
		}

		if toolName == "doops_ssh" {
			if err := client.RequireProtocol("structuredSSH"); err != nil {
				fmt.Printf("Error: %v\n", err)
				os.Exit(1)
			}
		}
		if err := client.Call(toolName, arguments); err != nil {
			if command == "ask" && apply {
				err = askCredentialNext(err, target)
			}
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}

		RecordHistory(server.Name, *sessionName, command)

	case "guide":
		if err := runGuide(cmdArgs, os.Stdout); err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}

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
		var target, gateway, username, label string
		var tokenStdin bool
		loginFlag := flag.NewFlagSet("login", flag.ExitOnError)
		loginFlag.StringVar(&gateway, "gateway", "", "Gateway URL (also accepts a configured route name). When used alone, discovers visible online targets")
		loginFlag.StringVar(&target, "target", "", "Backward-compatible alias: resolve this target's Gateway and log in there")
		loginFlag.BoolVar(&tokenStdin, "token-stdin", false, "Validate and import a Gateway token from stdin without issuing another token")
		loginFlag.StringVar(&username, "username", "", "Gateway username")
		loginFlag.StringVar(&label, "name", "", "Token label for password login")
		loginFlag.Parse(cmdArgs)

		resolvedGateway, server, err := resolveLoginGateway(target, gateway, servers, configErr)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			loginFlag.Usage()
			os.Exit(1)
		}
		gateway = resolvedGateway
		auth, err := LoadAuth()
		if err != nil {
			fmt.Printf("Error loading credentials: %v\n", err)
			os.Exit(1)
		}
		previous := auth.GetGatewayRecord(gateway)
		var credential GatewayAuth
		if tokenStdin {
			if strings.TrimSpace(username) != "" {
				fmt.Println("Error: --token-stdin cannot be combined with --username")
				os.Exit(1)
			}
			data, err := io.ReadAll(io.LimitReader(os.Stdin, 16*1024))
			if err != nil {
				fmt.Printf("Error reading gateway token from stdin: %v\n", err)
				os.Exit(1)
			}
			token := strings.TrimSpace(string(data))
			if token == "" {
				fmt.Println("Error: gateway token from stdin is empty")
				os.Exit(1)
			}
			credential, err = GatewayCurrentAuth(gateway, token, "token-stdin")
			if err != nil {
				fmt.Printf("Error validating gateway token: %v\n", err)
				os.Exit(1)
			}
		} else {
			if strings.TrimSpace(username) == "" {
				fmt.Print("Gateway username: ")
				fmt.Scanln(&username)
			}
			fmt.Printf("Enter gateway password for %s: ", username)
			bytePassword, err := term.ReadPassword(int(syscall.Stdin))
			fmt.Println()
			if err != nil {
				fmt.Printf("Error reading password: %v\n", err)
				os.Exit(1)
			}
			if label == "" {
				label = "doops login"
			}
			issued, err := GatewayLogin(gateway, username, string(bytePassword), label)
			if err != nil {
				fmt.Printf("Error logging in to gateway: %v\n", err)
				os.Exit(1)
			}
			credential = issued
		}
		auth.SetGatewayRecord(gateway, credential)
		if err := auth.Save(); err != nil {
			fmt.Printf("Error saving credentials: %v\n", err)
			os.Exit(1)
		}
		if server == nil {
			live, err := fetchGatewayTargets(gateway, credential.Token)
			if err != nil {
				fmt.Printf("Error: gateway login succeeded but target discovery failed: %v\n", err)
				fmt.Println("Gateway login is saved. Check Gateway target discovery and retry once the target list is available.")
				os.Exit(1)
			}
			routes, err := persistVisibleGatewayRoutes(servers, gateway, live)
			if err != nil {
				fmt.Printf("Error: gateway login succeeded but saving discovered targets failed: %v\n", err)
				os.Exit(1)
			}
			discovered := len(routes) - len(servers)
			fmt.Printf("Successfully logged in to Gateway %s; discovered %d target(s)\n", gatewayAuthKey(gateway), discovered)
			if len(live) == 0 {
				fmt.Println("No online targets are visible for this account.")
			} else if discovered == 0 {
				fmt.Printf("All %d visible target(s) are already configured.\n", len(live))
			}
		} else {
			fmt.Printf("Successfully cached gateway token for %s\n", gatewayAuthKey(gateway))
		}
		if previous.TokenID != "" && previous.TokenID != credential.TokenID {
			fmt.Printf("replaced_token_id=%s\n", previous.TokenID)
			fmt.Printf("replaced_username=%s\n", previous.Username)
		}
		if credential.TokenID != "" {
			fmt.Printf("cached_token_id=%s\n", credential.TokenID)
		}
		fmt.Printf("username=%s source=%s\n", credential.Username, credential.Source)
		if credential.ExpiresAt != "" {
			fmt.Printf("expires_at=%s\n", credential.ExpiresAt)
		} else {
			fmt.Println("expires_at=never")
		}

	case "logout":
		var target, gateway string
		logoutFlag := flag.NewFlagSet("logout", flag.ExitOnError)
		logoutFlag.StringVar(&target, "target", "", "Configured target name")
		logoutFlag.StringVar(&gateway, "gateway", "", "Gateway URL when no local target is configured")
		logoutFlag.Parse(cmdArgs)

		resolvedGateway, _, err := resolveLoginGateway(target, gateway, servers, configErr)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			logoutFlag.Usage()
			os.Exit(1)
		}
		auth, err := LoadAuth()
		if err != nil {
			fmt.Printf("Error loading credentials: %v\n", err)
			os.Exit(1)
		}
		auth.RemoveGateway(resolvedGateway)
		if err := auth.Save(); err != nil {
			fmt.Printf("Error saving credentials: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Removed local login for gateway %s; Gateway token was not revoked\n", gatewayAuthKey(resolvedGateway))

	case "add":
		var name, aliases, use, gateway, cluster, instance string
		addFlag := flag.NewFlagSet("add", flag.ExitOnError)
		addFlag.StringVar(&name, "name", "", "Node name (required)")
		addFlag.StringVar(&aliases, "aliases", "", "Comma separated aliases, e.g. demo,jy,oilan")
		addFlag.StringVar(&aliases, "alias", "", "Comma separated aliases, e.g. demo,jy,oilan")
		addFlag.StringVar(&use, "use", "Manually added node", "Description")
		addFlag.StringVar(&gateway, "gateway", "", "Public Gateway URL")
		addFlag.StringVar(&cluster, "cluster", "", "Gateway cluster name (required)")
		addFlag.StringVar(&instance, "instance", "", "Gateway agent instance name (required)")

		addFlag.Parse(cmdArgs)

		name = strings.TrimSpace(name)
		gateway = strings.TrimSpace(gateway)
		cluster = strings.TrimSpace(cluster)
		instance = strings.TrimSpace(instance)
		if name == "" || gateway == "" || cluster == "" || instance == "" {
			fmt.Println("Error: --name, --gateway, --cluster, and --instance are all required and must be non-empty.")
			addFlag.Usage()
			os.Exit(1)
		}
		if configErr != nil {
			if !os.IsNotExist(configErr) {
				fmt.Printf("Error loading config: %v\n", configErr)
				os.Exit(1)
			}
			servers = nil
		}

		newServer := Server{
			Name:     name,
			Aliases:  normalizeAliases([]string{aliases}),
			Use:      use,
			Gateway:  gateway,
			Cluster:  cluster,
			Instance: instance,
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
		if conflict := findRouteConflict(servers, newServer); conflict != "" {
			fmt.Printf("Error: gateway route %s/%s is already configured as target '%s'.\n", newServer.Cluster, newServer.Instance, conflict)
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

	case "remove", "rm":
		var target string
		removeFlag := flag.NewFlagSet("remove", flag.ExitOnError)
		removeFlag.StringVar(&target, "target", "", "Configured Gateway target to remove")
		removeFlag.Parse(cmdArgs)
		if strings.TrimSpace(target) == "" {
			fmt.Println("Error: --target is required")
			removeFlag.Usage()
			os.Exit(1)
		}
		*sessionName = resolveSession(*sessionName)
		requireConfig(configErr)
		index := -1
		for i, candidate := range servers {
			if serverMatchesTarget(candidate, target) {
				index = i
				break
			}
		}
		if index < 0 {
			fmt.Println(targetNotFound(servers, target))
			os.Exit(1)
		}
		server := servers[index]
		token := mustResolveToken(server)
		online, err := probeGatewayTarget(server, token, *sessionName)
		if err != nil {
			fmt.Printf("Error: cannot verify target is offline; refusing to remove '%s': %v\n", target, err)
			os.Exit(1)
		}
		if online {
			fmt.Printf("Error: target %s/%s is online; refusing to remove '%s'.\n", server.Cluster, server.Instance, target)
			os.Exit(1)
		}
		remaining, removed, err := removeServer(servers, target)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}
		if err := saveServers(remaining); err != nil {
			fmt.Printf("Error saving config: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Removed offline Gateway target '%s' (%s/%s).\n", removed.Name, removed.Cluster, removed.Instance)

	case "push":
		var target, src, dest string
		var dryRun bool
		var pushSession string
		var storageMode, credentialName string
		pushFlag := flag.NewFlagSet("push", flag.ExitOnError)
		pushFlag.StringVar(&target, "target", "", "Target server name")
		pushFlag.StringVar(&src, "src", ".", "Local source directory")
		pushFlag.StringVar(&dest, "dest", "", "Remote destination (用于推导 session 隔离标识)")
		pushFlag.BoolVar(&dryRun, "dry-run", false, "Preview only, don't actually sync")
		pushFlag.StringVar(&pushSession, "session", "", "Session ID (can also be set before subcommand)")
		pushFlag.StringVar(&storageMode, "storage", "cos", "Storage mode: cos (default, preserves Git executable bits; requires snapshot-capable Gateway/Agent) or gateway (local relay)")
		pushFlag.StringVar(&credentialName, "credential", "", "Ignored for cos; COS settings come from the Gateway")
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
			fmt.Println(targetNotFound(servers, target))
			os.Exit(1)
		}

		storageMode, storageErr := normalizeStorageMode(storageMode)
		if storageErr != nil {
			fmt.Printf("Error: %v\n", storageErr)
			os.Exit(1)
		}
		if storageMode == "cos" {
			token := mustResolveToken(*server)
			if _, err := pushCOS(*server, src, dest, dryRun, *sessionName, credentialName, token, nil); err != nil {
				fmt.Printf("Error: %v\n", err)
				os.Exit(1)
			}
			RecordHistory(server.Name, *sessionName, fmt.Sprintf("cos push %s -> %s", src, dest))
			break
		}
		if err := Push(*server, src, dest, dryRun, nil, *sessionName); err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}
		RecordHistory(server.Name, *sessionName, fmt.Sprintf("push %s -> %s", src, dest))

	case "pull":
		var target, dest string
		var pullSession string
		var storageMode, credentialName, commit string
		pullFlag := flag.NewFlagSet("pull", flag.ExitOnError)
		pullFlag.StringVar(&target, "target", "", "Target server name")
		pullFlag.StringVar(&dest, "dest", "", "Local destination directory (default: ./<session>)")
		pullFlag.StringVar(&pullSession, "session", "", "Session ID (can also be set before subcommand)")
		pullFlag.StringVar(&storageMode, "storage", "cos", "Storage mode: cos (default, uploaded snapshot with executable metadata) or gateway (current remote workspace)")
		pullFlag.StringVar(&credentialName, "credential", "", "Ignored for cos; COS settings come from the Gateway")
		pullFlag.StringVar(&commit, "commit", "", "Snapshot commit to pull; only used by the default cos mode")
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
			fmt.Println(targetNotFound(servers, target))
			os.Exit(1)
		}
		storageMode, storageErr := normalizeStorageMode(storageMode)
		if storageErr != nil {
			fmt.Printf("Error: %v\n", storageErr)
			os.Exit(1)
		}
		if storageMode == "gateway" && strings.TrimSpace(commit) != "" {
			fmt.Println("Error: --commit is only valid with --storage cos")
			os.Exit(1)
		}
		if storageMode == "cos" {
			token := mustResolveToken(*server)
			if err := pullWorkspace(*server, dest, *sessionName, "cos", credentialName, token, commit); err != nil {
				fmt.Printf("Error: %v\n", err)
				os.Exit(1)
			}
			RecordHistory(server.Name, *sessionName, fmt.Sprintf("cos pull -> %s", dest))
			break
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
		*sessionName = resolveSession(*sessionName)
		requireConfig(configErr)

		server := findServer(servers, target)
		if server == nil {
			fmt.Println(targetNotFound(servers, target))
			os.Exit(1)
		}

		token := mustResolveToken(*server)
		client := NewMCPClient(*server, ss, *sessionName, *verbose)
		client.Token = token
		defer client.Close()

		if err := Clean(client, workspace); err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}
		RecordHistory(server.Name, *sessionName, fmt.Sprintf("clean workspace=%s", workspace))

	case "upgrade":
		var target, image, mode, namespace, workload, container, release, upgradeSession, sourceCommit, ciURL string
		var dryRun, rollback, allowMissingRollbackBaseline bool
		upgradeFlag := flag.NewFlagSet("upgrade", flag.ExitOnError)
		upgradeFlag.StringVar(&target, "target", "", "Configured Gateway target")
		upgradeFlag.StringVar(&image, "image", "", "doops-agent image, repository:tag or repository@sha256:<digest>")
		upgradeFlag.StringVar(&mode, "mode", "auto", "Managed Helm Job mode: auto or k8s; standalone Docker uses backend/scripts/release-host-agent.sh")
		upgradeFlag.StringVar(&namespace, "namespace", "", "Optional namespace; must match the agent namespace")
		upgradeFlag.StringVar(&workload, "workload", "", "Optional deployment name; must match the Helm release")
		upgradeFlag.StringVar(&container, "container", "", "Container name; managed upgrades use doops-agent")
		upgradeFlag.StringVar(&release, "release", "", "Optional Helm release; discovered when omitted")
		upgradeFlag.StringVar(&upgradeSession, "session", "", "Session ID")
		upgradeFlag.StringVar(&sourceCommit, "source-commit", "", "Optional full CI source commit recorded with the upgrade audit")
		upgradeFlag.StringVar(&ciURL, "ci-url", "", "Optional HTTPS CI record URL without credentials, query or fragment")
		upgradeFlag.BoolVar(&dryRun, "dry-run", false, "Preview parameters only; does not verify live release, mounts or permissions, or create a Job")
		upgradeFlag.BoolVar(&rollback, "rollback", false, "Restore the previous verified revision and check its recorded release commit")
		upgradeFlag.BoolVar(&allowMissingRollbackBaseline, "allow-missing-rollback-baseline", false, "Proceed when the image an automatic rollback would restore is gone from the registry; the automatic rollback can then not restore the backup")
		upgradeFlag.Parse(cmdArgs)
		if upgradeSession != "" && *sessionName == "" {
			*sessionName = upgradeSession
		}
		*sessionName = resolveSession(*sessionName)
		if target == "" || (image == "" && !rollback) {
			fmt.Println("Error: --target is required; also provide --image unless using --rollback")
			upgradeFlag.Usage()
			os.Exit(1)
		}
		requireConfig(configErr)
		server := findServer(servers, target)
		if server == nil {
			fmt.Println(targetNotFound(servers, target))
			os.Exit(1)
		}
		if err := UpgradeAgents(*server, UpgradeOptions{
			Image:                        image,
			Mode:                         mode,
			Namespace:                    namespace,
			Workload:                     workload,
			Container:                    container,
			Release:                      release,
			DryRun:                       dryRun,
			Rollback:                     rollback,
			Session:                      *sessionName,
			SourceCommit:                 sourceCommit,
			CIURL:                        ciURL,
			AllowMissingRollbackBaseline: allowMissingRollbackBaseline,
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
		*sessionName = resolveSession(*sessionName)
		requireConfig(configErr)

		server := findServer(servers, target)
		if server == nil {
			fmt.Println(targetNotFound(servers, target))
			os.Exit(1)
		}

		token := mustResolveToken(*server)
		client := NewMCPClient(*server, ss, *sessionName, *verbose)
		client.Token = token
		defer client.Close()

		if err := CheckDeployment(client, namespace, deployment, image); err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}
		RecordHistory(server.Name, *sessionName, fmt.Sprintf("check %s/%s", namespace, deployment))

	default:
		fmt.Println(unknownCommand(command))
		os.Exit(1)
	}
}

func printServerList(w io.Writer, servers []Server) error {
	if err := validateGatewayServers(servers); err != nil {
		return err
	}
	fmt.Fprintf(w, "%-20s %-18s %-28s %-30s %-30s\n", "NAME", "ALIASES", "GATEWAY", "CLUSTER/INSTANCE", "USE")
	fmt.Fprintln(w, strings.Repeat("-", 130))
	for _, s := range servers {
		fmt.Fprintf(w, "%-20s %-18s %-28s %-30s %-30s\n",
			s.Name,
			strings.Join(s.Aliases, ","),
			s.Gateway,
			strings.TrimSpace(s.Cluster)+"/"+strings.TrimSpace(s.Instance),
			s.Use,
		)
	}
	return nil
}

func gatewayTargetUnavailableMessage(cluster, instance string) string {
	return fmt.Sprintf(
		"Error: target unavailable: %s/%s is not visible through this Gateway login; it may be offline or the user may lack an explicit target grant. Check the Gateway token, exact target route, and grant before concluding the cluster is down.",
		strings.TrimSpace(cluster), strings.TrimSpace(instance),
	)
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

func findRouteConflict(servers []Server, candidate Server) string {
	candidateGateway := strings.TrimRight(strings.TrimSpace(candidate.Gateway), "/")
	candidateCluster := strings.TrimSpace(candidate.Cluster)
	candidateInstance := strings.TrimSpace(candidate.Instance)
	if candidateGateway == "" || candidateCluster == "" || candidateInstance == "" {
		return ""
	}
	for _, server := range servers {
		if server.Name == candidate.Name {
			continue
		}
		if strings.TrimRight(strings.TrimSpace(server.Gateway), "/") == candidateGateway &&
			strings.TrimSpace(server.Cluster) == candidateCluster &&
			strings.TrimSpace(server.Instance) == candidateInstance {
			return server.Name
		}
	}
	return ""
}

func runAdminTokensList(args []string, servers []Server, configErr error) {
	fs := flag.NewFlagSet("admin token list", flag.ExitOnError)
	target := fs.String("target", "", "Configured gateway target whose token/gateway should be used")
	fs.Parse(args)
	if strings.TrimSpace(*target) == "" {
		fmt.Println("Error: --target is required")
		fs.Usage()
		os.Exit(1)
	}
	requireConfig(configErr)
	server := findServer(servers, *target)
	if server == nil || strings.TrimSpace(server.Gateway) == "" {
		fmt.Println(targetNotFound(servers, *target))
		os.Exit(1)
	}
	tokens, err := GatewayAdminTokensList(server.Gateway, mustResolveToken(*server), "user")
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}
	for _, token := range tokens {
		fmt.Printf("token_id=%s kind=%s user=%s name=%s prefix=%s revoked=%t expires_at=%s created_at=%s\n",
			token.ID, token.Kind, token.UserName, token.Name, token.Prefix,
			token.Revoked, token.ExpiresAt, token.CreatedAt)
	}
}

func runAdminTokenRevoke(args []string, servers []Server, configErr error) {
	var target, id string
	fs := flag.NewFlagSet("admin token revoke", flag.ExitOnError)
	fs.StringVar(&target, "target", "", "Configured gateway target whose token/gateway should be used")
	fs.StringVar(&id, "id", "", "Token id to revoke")
	fs.Parse(args)
	if strings.TrimSpace(target) == "" || strings.TrimSpace(id) == "" {
		fmt.Println("Error: --target and --id are required")
		fs.Usage()
		os.Exit(1)
	}
	requireConfig(configErr)
	server := findServer(servers, target)
	if server == nil || strings.TrimSpace(server.Gateway) == "" {
		fmt.Println(targetNotFound(servers, target))
		os.Exit(1)
	}
	if err := GatewayAdminTokenRevoke(server.Gateway, mustResolveToken(*server), id); err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("token_id=%s revoked=true\n", strings.TrimSpace(id))
}

func resolveAdminTargetFlags(name string, args []string, servers []Server, configErr error, opID *string) (Server, string) {
	var target string
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	fs.StringVar(&target, "target", "", "Configured gateway target whose token/gateway should be used")
	if opID != nil {
		fs.StringVar(opID, "id", "", "Active operation id to request cancellation for; acknowledgment does not confirm it has stopped")
	}
	fs.Parse(args)
	if strings.TrimSpace(target) == "" {
		fmt.Println("Error: --target is required")
		fs.Usage()
		os.Exit(1)
	}
	requireConfig(configErr)
	server := findServer(servers, target)
	if server == nil || strings.TrimSpace(server.Gateway) == "" {
		fmt.Println(targetNotFound(servers, target))
		os.Exit(1)
	}
	if opID != nil && strings.TrimSpace(*opID) == "" {
		fmt.Println("Error: --id is required")
		fs.Usage()
		os.Exit(1)
	}
	return *server, mustResolveToken(*server)
}

func requireConfig(err error) {
	if err == nil {
		return
	}
	fmt.Printf("Error: no doops node config found: %v\n", err)
	fmt.Println("First login (discovers visible targets): doops login --gateway <gateway-url>")
	fmt.Println("To pin a route first: doops add --name <name> --gateway <gateway-url> --cluster <cluster> --instance <instance>")
	os.Exit(1)
}

func ResolveToken(server Server) (string, error) {
	if strings.TrimSpace(server.Gateway) == "" {
		return "", fmt.Errorf("target %q has no Gateway route", server.Name)
	}
	auth, err := LoadAuth()
	if err != nil {
		return "", err
	}
	if p := auth.GetGateway(server.Gateway); p != "" {
		return p, nil
	}
	return "", fmt.Errorf("no login for Gateway %s; run `doops login --gateway %s`",
		gatewayAuthKey(server.Gateway), gatewayAuthKey(server.Gateway))
}

func mustResolveToken(server Server) string {
	token, err := ResolveToken(server)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}
	return token
}
