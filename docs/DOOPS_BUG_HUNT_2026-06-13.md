# doops 全链路缺陷猎杀记录（skill / cli / gateway / agent）

状态：fixed（L-15 legacy cleanup 待产品确认）
日期：2026-06-13
负责人：未指定
范围：doops 执行链路（SKILL.md → doops CLI → 公网 gateway → 内网 agent）
模式：batch（defect + security + permission + legacy-cleanup）
猎物键根：`doops/<layer>/<bug-source>`

> 约定：push **仅支持 git 模式**。CLI 推送统一走 `git push` → gateway `/v1/git/` → agent git-http-over-WS；
> 任何"WebSocket 分块上传 push"的描述或代码均视为历史残留，本文据此归类。

---

## 结论

整条链路存在一个决定信任模型的根因缺陷：**公网 gateway 不校验 agent 注册身份**，任何人可冒充/接管任意 target 并截获命令与文件内容。其次是**权限默认放行**（无 grant 即全开、`targets:list` 隐式提权），与 SKILL.md 的最小授权描述直接矛盾。再叠加隧道层的"单 op 超时断全连 + 背压丢帧"、agent 侧"session 路径穿越 / 文件无沙箱 / 安全 dispatcher 死代码"，以及 CLI 的"握手误判 + 无超时挂死"。push 既然 git-only，则 agent 的分块上传工具属于应清理的死路径，相关 SKILL 描述需改为 git 模式。

## 状态说明

- 当前状态：fixed（P0/P1/P2 代码缺陷已修；L-15 分块上传死路径清理待确认外部调用方）
- 证据强度：code-path proof 为主
- 是否已改代码：是（P0/P1/P2；另补 pull bare repo 自恢复）
- 是否已验证：是（`go test ./internal/server -count=1`、`go test ./cli/doops -count=1`）
- 去重检查：new（docs/ 下无既有 hunt 文档）
- 下一步：确认 L-15 分块上传工具是否仍有外部调用方；如无再做 legacy 删除

## 已修复

- 2026-06-13 `fix(doops-gateway): require agent auth and explicit grants`
  - P0-1：`/v1/agent/connect` 已在 WebSocket upgrade 前校验 agent token，且 token 绑定的 `cluster/instance` 必须与 query 一致；无 token / user token 返回 401，错配 agent token 返回 403。
  - P0-2：`UserCan` / `UserHasAction` 改为无 grant 默认拒绝；`targets:list` 不再隐式授予 exec/read/write/push 等默认操作。
  - 额外修复：pull bundle 生成时 `/tmp/repos/<session>.git` 若存在但不是合法 bare repo，会删除并重建，避免坏仓库状态导致 pull 卡死。
- 2026-06-13 `fix(doops): close bug hunt findings`
  - P1-3/P1-4：gateway 超时只发送 `tools/cancel`，不关闭 agent 物理连接；pending 投递改为阻塞背压，不再静默丢帧。
  - P1-5/P1-6/P1-7：agent 统一校验 `session_id`，file/bg 路径限制在 session workspace 内，`doops_shell` 接入 dispatcher 阻断危险命令。
  - P1-8：分块上传按 `upload_id` 加锁，修复 meta TOCTOU 竞态。
  - P1-9/P1-10：CLI initialize 校验 id/result/error，Call/CallAndCapture/ExecBg 增加调用超时。
  - P2：补 exec 超时、GC workspaceRoot、completed task 清理、docker 换行注入、WS read limit、clean/check session+Close、gateway path prefix、bash gateway-only 拒绝、push ready 去掉 legacy-ready。

---

## P0 — 严重（信任模型 / 权限）

### P0-1 `doops/gateway/agent-connect-unauthenticated` — 任意 target 劫持 / MITM
- 状态：fixed ｜ 维度：gateway ｜ 安全
- 证据：
  - `agent/internal/server/tunnel_hub.go:373-417` `HandleAgentConnect` 仅取 query 的 `cluster/instance`，升级 WS 后直接 `registerAgent`，**全程无 token 校验**。
  - `agent/internal/server/tunnel_hub.go:921` `authenticateAgent` 已定义但**无任何调用点**（仅 `writeUserAuthError` 用 `VerifyAgentToken` 区分 401/403）。
  - `agent/cmd/agent/main.go:106-108` agent 实际发送了 `Authorization: Bearer` 与 `X-Doops-Key`，但被 gateway 忽略；`main.go:37` flag 注释自承"current gateways do not require it"。
  - `agent/internal/server/tunnel_hub.go:855-857` `registerAgent` 同 key 重注册会关闭旧 agent 连接 → 接管。
- 滥用路径：可达公网 gateway 者连 `/v1/agent/connect?cluster=doops-oilan&instance=oilan-node` 即冒充该 target，踢掉真实 agent，截获后续所有 `tools/call`（含 `doops_file_read` 内容、`doops_shell` 命令），并回传伪造结果。
- 影响：跨租户接管、命令/文件窃取、运维结果伪造、对真实 agent 的持续 DoS。
- 修复：`HandleAgentConnect` 入口已调用 `authenticateAgent(r)`；校验 agent token 绑定的 `cluster/instance` 与 query 一致；不匹配返回 401/403。已补回归测试断言无 token / user token / 错配 token 被拒，匹配 token 可注册。
- 兼容/回滚：可加 `--require-agent-auth`（默认开）灰度；老 agent 已具备发 token 能力，风险低。

### P0-2 `doops/gateway/permission-default-allow` — 无 grant 默认全放行 + targets:list 隐式提权
- 状态：fixed ｜ 维度：gateway ｜ 权限完整性 / 安全
- 证据：
  - `agent/internal/server/gateway_store.go:515` `UserCan` 末行 `return !hasGrant && defaultGatewayUserActionAllows(action)`：用户**无任何 grant 行时默认放行**全部 default actions 到任意 cluster/instance（`UserHasAction` 同样 `:544`）。
  - `agent/internal/server/gateway_store.go:39-50` default actions 含 exec/ask/read/write/push/pull/info/check/clean。
  - `agent/internal/server/gateway_store.go:547-555` `gatewayActionAllows`：只授予 `targets:list` 的用户被隐式赋予全部 default actions。
- 后果：`doops-gateway user create` 后未 grant 即可 exec 所有在线 target；管理员"只读授权"误发 `targets:list` 实际等于发了 exec/write/clean。
- 与文档冲突：SKILL.md（`skills/SKILL.md`）"缺少对应 target/action 的 grant → 403"与 `grant ... -actions targets:list,exec,ask,read,info` 的最小授权意图，与该实现**直接矛盾**。
- 修复：已改为默认拒绝；已移除 `targets:list ⇒ 全 default` 隐式映射，改为显式 action 授权。已补测试：新建无 grant 用户无任何 target action；仅 `targets:list` 不再拥有 write 等权限。
- 兼容/回滚：属行为收紧，需在发布说明提示存量用户补 grant；可加迁移脚本为现有用户补默认 grant 行。

---

## P1 — 高（正确性 / 稳定性 / agent 安全边界）

### P1-3 `doops/gateway/op-timeout-closes-tunnel` — 单次操作超时断开整条 agent 隧道
- 状态：fixed ｜ 维度：gateway
- 证据：`agent/internal/server/tunnel_hub.go:1133-1135` `relayToolCall` 超时分支 `_ = a.conn.Close()`；`OperationTimeout` 默认 30min（`:188`）；一个 agent 连接被所有 user/session 共享。
- 影响：任一用户长任务超时 → target 掉线、其他并发会话全断、agent 重连。
- 修复：超时只取消该 op（已有 `tools/cancel` 协议，`:1109-1114`），不要关物理连接。

### P1-4 `doops/gateway/backpressure-drop` — 背压下静默丢帧导致截断/挂死
- 状态：fixed ｜ 维度：gateway
- 证据：`agent/internal/server/tunnel_hub.go:1006-1011`、`1024-1028` 向容量 256 的 channel 投递消息时 `select { case ch<-msg: default: }`，满则丢；git/body 同路径（`git_ws.go` 回发带 `id`）。
- 影响：大输出/大 clone 时 git pack 损坏或 exec 输出截断；若终帧被丢，`relayToolCall` 等到 30min 超时再触发 P1-3 断链。
- 修复：改为带 ctx 的阻塞写，丢弃前必须反压上游 `ReadMessage`；或显式上限并向客户端报错而非静默丢。

### P1-5 `doops/agent/session-id-path-traversal` — session_id 路径穿越
- 状态：fixed ｜ 维度：agent ｜ 安全
- 证据：`agent/internal/server/handler_ws.go:314-318, 669, 904` 用客户端 `session_id` 直接拼 `/root/ws/<sid>` 与 doagent cwd、SSE `?sid=`，未调用已存在的 `workspace_upload.go:985` `validateSession`。
- 滥用：`sid=../../tmp/x` 在工作区外建目录/写审计日志、把 doagent cwd 指向任意目录。
- 修复：所有入口先 `validateSession`，路径用 `filepath.Join(root, sid)` 并校验前缀；SSE 用 `url.QueryEscape`。

### P1-6 `doops/agent/file-tools-no-sandbox` — file_read/file_write/bg 无路径沙箱
- 状态：fixed ｜ 维度：agent ｜ 安全
- 证据：`agent/internal/server/workspace_upload.go:102-147` 直接用 `args.Path`（测试故意写工作区外 `handler_ws_test.go:32-36`）；`doops_bg` 的 `log_path` 直接 `os.Create`（`gateway.go:184-198`）。
- 滥用：已认证用户读/写/截断 agent 用户可达的任意文件（`/etc/*`、kubeconfig、cron）。属越权/路径边界（非密钥存储）。
- 修复：限定在 `workspaceRoot/<session>/` 或显式 allowlist，拒绝绝对路径/`..`/逃逸软链。

### P1-7 `doops/agent/dispatcher-dead-code` — 命令黑名单从未生效
- 状态：fixed ｜ 维度：agent ｜ legacy / 安全
- 证据：`agent/internal/dispatcher/dispatcher.go:33-40` 危险命令分类构造在 `Gateway`（`gateway.go:38,61`），但 `handler_ws.go` `doops_shell` 直达 `executeRawCommand`（`:1059`），全仓 0 处 `Classify` 调用。
- 影响：文档宣称的安全闸门实际不存在。
- 修复：exec 前调用 `Classify` 拦截 `PathBlocked`；或删除 dispatcher 及相关文档承诺，避免误导。

### P1-8 `doops/agent/upload-chunk-race` — 分块上传 meta TOCTOU 竞态
- 状态：fixed ｜ 维度：agent
- 证据：`agent/internal/server/workspace_upload.go:572-618` meta 读→追加→写非原子，同 `upload_id` 并发 chunk 可都过 `Seq==NextSeq` → tar/SHA256 损坏。
- 修复：按 `upload_id` 加互斥锁或文件锁。
- 备注：push 为 git-only，此分块上传链路是否仍被任何调用方使用需复核（见 L-15 legacy）。

### P1-9 `doops/cli/mcp-handshake-false-positive` — 握手把任意消息当连接成功
- 状态：fixed ｜ 维度：cli
- 证据：`skills/doops-cli/cli/doops/client.go:166-174` `connect()` 只等"有消息到达"，不校验 `id==initID`、不看 `error/result`；`dispatchMessage`（`:286-306`）在 `len(pending)==1` 时把杂项 notification 投给 init channel。
- 影响：误判 connected，后续 tool 调用失败/挂起。
- 修复：解析校验 initialize 响应（匹配 id、无 error）；握手期排除 notifications。

### P1-10 `doops/cli/no-call-timeout` — Call/ExecBg 无超时永久挂起
- 状态：fixed ｜ 维度：cli
- 证据：`skills/doops-cli/cli/doops/client.go:416, 541` `for evt := range ch` 仅靠终帧退出、channel 永不关闭、无 deadline；`ExecBg`（`:668-710`）`for{ sleep 3s }` 无最大时长。
- 影响：配合 P1-3/P1-4，agent 不回终帧时 CLI/自动化无限阻塞。
- 修复：每调用加超时并在超时关闭/排空 channel；ExecBg 设总时长与失败预算。

---

## P2 — 中

| 猎物键 | 维度 | 问题 | 证据 | 修复方向 |
| --- | --- | --- | --- | --- |
| `doops/agent/no-exec-timeout` | agent | exec 无最大执行时长 | `handler_ws.go:334-416` | fixed：shell/docker/node_info 使用 `context.WithTimeout`；gateway 已有全局/用户/资源并发限制 |
| `doops/agent/gc-data-loss` | agent | GC 硬编码 `/root/ws` 且 bare repo 清理策略错误 | `cleanup.go:28, 58-87` | fixed：共享 `workspaceRoot()`；`/tmp/repos` 按 repo 目录整体删除，workspace 保留审计日志 |
| `doops/agent/disk-mem-leak` | agent | pull bundle、完成的 bg `tasks` map 永不清理 | `workspace_upload.go:185-209`；`gateway.go:213-245` | fixed：上传/bundle 已有 TTL 清理；完成 bg task 按 TTL 驱逐 |
| `doops/agent/docker-newline-injection` | agent | `doops_docker` 过滤拦 `;|&$\`` 不拦换行/`()`，`ps\nid` 注入 | `handler_ws.go:422-430` | fixed：严格拒绝 `;|&$\`()\r\n` |
| `doops/agent/ws-no-readlimit` | agent | 无 `SetReadLimit`，大帧/大 `data_b64` 全量入内存 → OOM | `handler_ws.go:145, 156-160` | fixed：`SetReadLimit`，默认 32MiB，可用 `DOOPS_MAX_WS_MESSAGE_BYTES` 配置 |
| `doops/cli/client-leak-no-session` | cli | `clean`/`check` 未 `defer Close()`；不强制 `-session` | `main.go:769-775, 858-864` vs `:352` | fixed：统一 `defer client.Close()` + 强制 session |
| `doops/cli/gateway-url-prefix` | cli | 网关带 path 前缀（`/api`）时 WS/targets/login 与 git 的 URL 构造不一致 | `client.go:204-206`、`gateway_targets.go:125`、`push.go:225` | fixed：统一 path join，保留 base path 并追加 `/v1/...` |
| `doops/cli/bash-gateway-empty-ip` | cli | `bash` 对 gateway-only target 用空 IP 直连 SSH，失败仍 `exit 0` | `main.go:456-469, 641` | fixed：gateway-only target 直接拒绝；无 IP 直接失败 |
| `doops/cli/push-legacy-ready` | cli | push 就绪判定 `legacy-ready` 跳过 commit 校验，残留文件误报"完成" | `push.go:316-321` | fixed：删除 `legacy-ready` 回退，必须校验 `.doops-ready` commit |

---

## 跨层 / skill 文档漂移

### L-13 `doops/skill/push-mode-doc-drift` — SKILL 称 push 走 WebSocket 分块上传（与 git-only 事实不符）
- 状态：confirmed-by-code-path ｜ 维度：skill / 文档
- 证据：CLI 实际 `git push -f gitURL HEAD:master`（`skills/doops-cli/cli/doops/push.go:108-113`），gateway `/v1/git/` 中转，agent git-http-over-WS（`git_ws.go`）。`skills/SKILL.md:330` 写"gateway 模式 push 走 WebSocket 分块上传"。
- 修复：将 SKILL.md 改为：**push 统一 git 模式**——直连走 agent git-http，gateway 走 `/v1/git/` 隧道；删除"分块上传"措辞。

### L-14 `doops/skill/grant-doc-contradiction` — 文档最小授权描述与默认放行实现矛盾
- 状态：confirmed-by-code-path ｜ 维度：skill / 权限
- 证据：见 P0-2。SKILL.md 暗示需显式 grant、缺则 403，实现却默认全放行。
- 修复：随 P0-2 改默认拒绝后，文档与实现自然对齐；若暂不改代码，文档须显式说明"无 grant = 全放行"的真实风险。

### L-15 `doops/agent/legacy-chunked-upload` — 分块上传工具疑似死路径（push git-only 前提下）
- 状态：candidate（needs validation）｜ 维度：agent / legacy-cleanup
- 证据：agent 与 gateway 保留 `doops_workspace_begin/chunk/commit` 及 `pull_begin/pull_chunk` 的 action 映射与实现（`tunnel_hub.go:1508-1511`、`workspace_upload.go`），但 CLI push/pull 走 git；`push_test.go:75-77` 仍引用这些工具。
- 修复方向：确认无任何现网调用方后，删除分块上传/下载实现与 action 映射、相关测试与文档；同时消除 P1-8 竞态与 P2 bundle 泄漏的根因。
- 暂缓原因：需先确认 pull 是否也已全部 git 化、是否有外部脚本直接调这些 MCP 工具。

---

## 已识别但按猎人规则排除（非 prey，仅备注）

- git push/pull 把 `user:token@host` 嵌入 URL，git 报错原样打印 stderr 可能泄露 token（`push.go:133`、`pull.go:105`）。
- token/password 用 `==` 非常量时间比较（`handler_ws.go:47`、`gateway.go:172`）。
- `enforceSecureGatewayURL` 为空函数，允许明文 `ws://`（`cli gateway_targets.go:136`、`agent main.go:161`）。
- CheckOrigin 恒 true；agent 默认监听 `:port` 全网卡（`handler_ws.go:27-30`、`main.go:77`）。

> 以上涉及密钥/凭据处理或 TLS-secret，按 bug-hunter 规则标 `non-prey/out_of_scope`，不纳入主修复清单；如需可单独评估。

---

## 修复优先级（建议执行顺序）

1. P0-1 给 `/v1/agent/connect` 加 agent token 校验（决定整个信任模型）。
2. P0-2 默认拒绝 + 取消 `targets:list` 隐式提权。
3. P1-3 / P1-4 隧道层：超时只取消 op、不断连；背压不丢帧。
4. P1-5 / P1-6 / P1-7 agent：session 校验、文件路径沙箱、接回或删除 dispatcher。
5. P1-9 / P1-10 CLI：握手校验 + 调用超时。
6. L-13 / L-14 文档随代码同步；L-15 确认后清理分块上传死路径。

## 证据索引（关键）

- `agent/internal/server/tunnel_hub.go:373-417` — agent 注册无鉴权（P0-1）
- `agent/internal/server/tunnel_hub.go:921` — `authenticateAgent` 定义但无调用（P0-1）
- `agent/internal/server/gateway_store.go:515,544,547-555` — 默认放行 + 隐式提权（P0-2）
- `agent/internal/server/tunnel_hub.go:1133-1135` — 超时关整条连接（P1-3）
- `agent/internal/server/tunnel_hub.go:1006-1011,1024-1028` — 背压丢帧（P1-4）
- `agent/internal/server/handler_ws.go:314-318,669,904` — session_id 穿越（P1-5）
- `agent/internal/server/workspace_upload.go:102-147` — 文件无沙箱（P1-6）
- `agent/internal/dispatcher/dispatcher.go:33-40` + `handler_ws.go:1059` — 黑名单死代码（P1-7）
- `skills/doops-cli/cli/doops/client.go:166-174` — 握手误判（P1-9）
- `skills/doops-cli/cli/doops/push.go:108-113` — push git-only 事实（L-13）
