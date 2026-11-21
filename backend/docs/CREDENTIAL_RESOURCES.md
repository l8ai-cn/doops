# DoOps Credential Resources

本文说明受管凭据资源的 CLI、权限、持久化、验证、轮换、吊销和发布引用方式。

凭据资源 ID 的互斥与操作 session 的互斥是不同边界：同一凭据的变更仍串行；不同
session 的目标执行可以并行。取消或审计收尾不得把其中一个 session 的状态扩展为
整个 target 的发布屏障。凭据使用只要求该 target 的 grant；workload 不是独立权限维度。

## 对象模型

| 对象 | 含义 |
| :--- | :--- |
| Credential Resource | 稳定名称、personal/platform 归属、类型和资源状态 |
| Credential Version | 加密 payload 的不可变版本，保存 payload digest，不返回 payload |
| Credential Grant | 凭据的 target 绑定及运行位置元数据；不要求每个操作员重复授权 |
| Credential Verification | staged 版本在某个 target grant 上的实际物化和验证证据 |
| Credential Bundle | 一组 credential ID 的静态元数据集合，当前不参与 `ask --apply` 展开 |
| Materialization | 目标环境中的 Kubernetes Secret 或临时 Helm/Git 凭据上下文 |

Gateway 使用 SQLite 持久化元数据、加密 payload、grant、verification、materialization
inventory 和 audit。`DOOPS_GATEWAY_SECRET_KEY` 是必需的加密主密钥；Gateway 不自动生成
替代 key。

## 登录和隔离

```bash
doops login --gateway <name|url> --username <user>
```

登录态按规范化 Gateway URL 保存。同一 Gateway 下所有 configured target 共用一次登录，
凭据使用继承当前用户的 target 操作权限，按 target 的 `cluster/instance` 隔离；凭据管理仍检查归属。

Web 控制台和 CLI 共享 Gateway 身份与授权模型：Web 使用 HttpOnly session cookie，CLI
使用 `doops` 的 Gateway token 缓存。两者都只能提交 credential resource name；DoSql
数据库 skill 不读取本地连接文件，也不把数据库密码放进 prompt、浏览器状态或审计文本。

```bash
doops credential list --target <target>
doops credential list --target <target> --view managed
```

- 有该 target 的 grant 时，默认 `target` 视图返回绑定该 target 且存在 active version
  的 active resource，不再要求给每个操作员重复授予凭据权限。
- `managed` 视图返回当前用户拥有的 personal resource；platform admin 可查看平台资源。
- `managed` 视图只返回 resource metadata，不加载 target binding 详情。
- target 视图不返回 owner、creator 或 payload 等管理/秘密字段。
- 未授权详情使用 404，避免泄露资源是否存在。

## Web 控制台管理

凭据页面的 `我管理的` 视图使用 `credential list --view managed` 的资源元数据，并行读取
`GET /v1/credentials/<id>/versions` 和 `GET /v1/credentials/<id>/grants`。页面展示服务端
返回的 staged、active 和历史版本，以及所有授权的完整 metadata scope；不会从本地状态、
更新时间或 resource ID 推断版本。页面不会缓存、回显或把 payload 放入任务上下文。

切换到 `当前目标可用` 后，页面使用该 target 返回的实际 binding 元数据展示可用状态。对
active target binding 的 staged version 执行验证时，页面提交 target/use 和物化 namespace；
workload、image、Helm 或 required keys 只在本次执行确实需要时作为可选元数据传入，不作为
Gateway 授权条件。Promote 请求仍由 Gateway 检查所有 active target binding 的验证状态；
重新授予已吊销的 grant 或修改影响物化的绑定元数据后，需要重新验证 staged 版本。grant 吊销和 resource 吊销都沿用 Gateway 的清理流程。

target 视图只读取 target 接口，不并行请求 managed 列表或 versions；管理接口的权限状态
不能清空成功返回的目标凭据。该视图仅展示可用凭据与目标绑定，管理操作在 `我管理的`
视图执行。查询接口只返回 metadata，不返回 payload。

`我管理的` 视图中，`opaque` 和 `modelProvider` 凭据有 `从 Secret 导入` 入口，对应
`credential configure --from-secret <name> --keys <k,...> [--key-map src=dst]` 的导入步骤，调用
`POST /v1/credentials/<id>/import-secret`。表单只提交当前 target、namespace、Secret 名、键名列表和
可选键映射（`modelProvider` 固定一个键并使用 `format=doagent_settings`，不允许映射）；浏览器端先做与
Gateway 相同的格式校验，校验失败不发请求。值由 Agent 读取后经私有控制通道交给 Gateway 加密保存，
响应只保留新 staged version 的 ID、状态和 digest。权限沿用 Gateway：需是资源 owner 或平台管理员，
且对当前 target 有授权，否则返回 `credential_import_forbidden`。页面将已知的稳定错误类别映射为中文原因和
下一步；未知正文按 HTTP 状态显示通用提示，不展示上游原文或值。导入后仍需在页面上绑定目标、验证并激活，
与 CLI 的后续阶段相同；页面不自动执行这些步骤。

绑定目标时，必须显式填写目标 Agent 已获 Kubernetes RBAC 授权、可创建临时 Secret 的 namespace。
Gateway 当前不提供可写 namespace 列表，控制台不会假定 `default` 可用。用途为 `opaqueSecret` 时，
绑定对话框另有可选的 required keys（逗号分隔的 Secret 键名，提交前去重排序），对应 CLI
`--required-keys`：填写后验证要求版本 payload 的键集合与之完全相等，不填则按版本内容推断。

凭据操作（写入版本、绑定、验证、激活、解除绑定、吊销）失败时，页面把 Gateway 返回的稳定类别映射为
中文原因和下一步，括号中保留原类别；未知类别显示通用说明和类别本身，非类别的错误正文按 HTTP 状态
显示固定提示，不展示上游原文。例如 `credential_secret_get_failed` 提示检查绑定的 namespace 是否在目标
Agent 的 Kubernetes RBAC 范围内，`credential_verification_required` 提示先对每个 active 绑定验证暂存版本。

页头的 `凭据组` 入口用于创建和按名称查看 bundle，步骤和限制见 [Bundle](#bundle)。

## 开发凭据 Preflight

开发项目可在 `.agent/doops-credentials.yaml` 声明需要使用的受管凭据引用：

```yaml
apiVersion: doops.sh/v1
kind: DevelopmentCredentialRequirements
target: gw-114-k8s
credentials:
  - credentialRef: dev-git
    use: gitCheckout
  - credentialRef: dev-registry
    use: imagePull
```

执行检查：

```bash
doops -session dev-preflight-<task> credential preflight \
  --manifest .agent/doops-credentials.yaml
```

Given 当前用户已经登录且精确 target grant 为 active，When preflight 查询 manifest 中的
每个 `credentialRef + use`，Then 全部匹配时返回 `credential_preflight=ready`；缺失、
use 不匹配、revoked grant 或同名资源不唯一时返回非零。

安全边界：

- manifest 只能包含 `apiVersion`、`kind`、`target`、`credentials[].credentialRef`
  和 `credentials[].use`；
- unknown field 会被严格拒绝，因此 password、token、cookie、payload 和本地 secret
  路径不能混入清单；
- `credentialRef` 只接受小写字母、数字、点和连字符组成的资源名，不接受 URL、文件路径、
  token 形态或 fallback 值；
- manifest 可以进入 Git，但它只是需求清单，不创建 grant、不授予权限、不物化 secret；
- preflight 不从项目名、目录、prompt 或历史命令推断 credentialRef；
- preflight 不为本机进程或远端 workspace 注入 payload。实际使用仍走 target binding、
  verification、promotion 和内部 materialization。

## Scope 和权限

### Resource scope

- `personal`：owner 是创建者。owner 可以写入版本、授权、验证、轮换和吊销。
- `platform`：只有 platform admin 可以创建、管理、授权、轮换和吊销。
- platform admin 是用户的 `is_admin` 标记，不由 target grant 推导。

personal owner 给 target 创建绑定前必须能操作该 target。运行时使用和验证只要求该 target 的 grant；验证、轮换、吊销的资源管理权限仍检查 owner/admin。

### Target binding

Gateway 授权只回答一个问题：当前用户能否使用哪个 target。凭据绑定只回答凭据在哪个
target 可用。权限匹配只使用 target 的 `cluster` 和 `instance`；`use` 只是凭据类型和
物化用途的元数据，不是第二套权限。凭据不再绑定某个用户。

以下字段是物化或执行元数据，不是凭据授权条件：

- Kubernetes `namespace`；
- workload kind/name；
- image repository/digest；
- Helm release/chart；
- required Secret keys。

workload kind 仅支持 `Deployment`、`StatefulSet`、`DaemonSet`。不支持 wildcard、前缀、
继承、Pod、Job 或 CronJob。grant 不绑定 session；session 只绑定一次运行和物化记录。

| use | Credential type | 额外必填字段 |
| :--- | :--- | :--- |
| `imagePull` | `registry` | 可选 image repository 和 64 位 `sha256:` digest，用于运行时镜像校验 |
| `tls` | `tls` | 无 |
| `opaqueSecret` | `opaque` | 可选 `required_keys`；填写时验证要求 payload key 集合与之完全相等，省略时按版本 payload 的 key 推断 |
| `database` | `opaque` | 目标 `cluster/instance`、物化 `namespace` 和 `use=database`；数据库资产、环境、引擎、数据库名由 DoSql/Agent 校验 |
| `helmPull` | `helmRepository` | target 上可用的 Helm 仓库凭据；实际 Chart 由任务和仓库来源决定 |
| `gitCheckout` | `gitToken` | 无；当前 scope 不包含 branch/ref |
| `modelProvider` | `modelProvider` | 无；一个 target 同时只能绑定一个 active `modelProvider` 凭据 |

数据库 grant 不绑定数据库资产、环境、引擎、数据库名或 workload；这些是 DoSql 的执行上下文，
DoSql 请求直接由 Gateway 读取保存的 target/database 绑定，物化使用绑定的 namespace；不调用模型生成凭据 scope。
Agent 会根据当前 target 的非 Secret discovery 校验。PostgreSQL、MySQL、Oracle、SQL Server 和 MongoDB 都可以用
`connection_uri`（`credential configure --from-secret ... --connection-uri` 由 Agent 注入密码，
不回传）；MySQL 也可以用
`required_keys=SPRING_DATASOURCE_URL,SPRING_DATASOURCE_USERNAME,SPRING_DATASOURCE_PASSWORD`。
`database_asset_id` 必须来自 DoSql 的 target discovery 或授权代码资产，不能由 CLI 根据
数据库名猜测；它不再进入 Gateway target binding equality。`credential grant`/`verify` 只验证
Gateway target、credential ownership、materialization namespace 和 payload usability；DoSql
query/apply 继续验证资产、引擎、数据库名、SQL policy、approval 和 evidence。

## Payload schema

payload 只能从 stdin 输入，最大 1 MiB，必须是单个 strict JSON；未知字段会被拒绝。

### OCI Registry

```json
{
  "server": "registry.example.com",
  "username": "deploy",
  "password": "<secret>",
  "email": "optional@example.com"
}
```

一份 registry 凭据同时物化两处，二者都由 Gateway 下发：

- Kubernetes Secret（`kubernetes.io/dockerconfigjson`）供 kubelet 拉取镜像使用。
- Agent 自己的 OCI 工具配置文件 `/root/.docker/config.json`（权限 0600），供
  `doops registry sync`、`doops image copy` 等目标侧命令使用。

Agent 的配置文件随 Pod 一起消失，因此 Agent 连上 Gateway 后，Gateway 会把该 target 上
active 的 registry 凭据重新物化到该文件，Agent 不需要、也不应该由运维手工写入。
重新物化只恢复这个文件，不重建 Kubernetes Secret，也不改 workload 的 imagePullSecrets，
因此正在用该 Secret 拉取的工作负载不受 Agent 重启影响。

配置文件是合并写入，不是整体替换：Agent 没有下发的条目（例如运维自己写的）保持不变。
Agent 另写一份只含元数据的归属记录（server、credential/grant/version ID），据此在
grant 吊销或轮换时只清理自己下发的条目。轮换到另一个 registry 时，旧的 registry 条目
一并移除。

`doops registry sync` 与 `doops image copy` 默认使用这个文件，不必再传 authfile 路径；
`doops registry sync` 的 `--source-authfile`/`--target-authfile` 和 `doops image copy` 的
`--source-authfile`/`--destination-authfile` 保留为覆盖项，用于目标把 registry 配置放在别处的情况。
凭据本身始终不进入命令参数。

重新物化只发给握手时声明 `registryAuthfile` 协议（`doops.sh/registry-authfile/v1`）的 Agent。
旧版本 Agent 严格解析物化参数，收到该请求会直接拒绝，因此 Gateway 不发，
它的 authfile 保持升级前的样子，直到该 target 的 Agent 升级完成。

### TLS

```json
{
  "certificate": "-----BEGIN CERTIFICATE-----\n...\n",
  "privateKey": "-----BEGIN PRIVATE KEY-----\n...\n"
}
```

验证会检查证书和私钥匹配；公开输出仍只包含状态、resourceVersion、digest 或错误类别，
不等价于 CA 信任、hostname 或完整证书链验证。

### Opaque Secret

```json
{
  "data": {
    "API_TOKEN": "<secret>",
    "ENDPOINT": "https://api.example.com"
  }
}
```

key 和 value 都不能为空。

### Helm Repository

```json
{
  "url": "https://charts.example.com",
  "username": "deploy",
  "password": "<secret>"
}
```

验证以本次请求的 Chart URL 和受管 payload 仓库 URL 为下载边界，带 HTTP Basic 读取真实 `.tgz`，
校验 gzip/tar 与 `Chart.yaml` 并返回包摘要，不读取 `index.yaml`。`values.yaml` 可缺省，
其业务 schema 和模板正确性由 Helm 校验，不在下载阶段增加限制。
带凭据的下载 URL 必须属于 payload URL 的 HTTPS origin；仓库路径不参与权限匹配。认证请求的重定向、私网地址、无效包和超限失败关闭，
不会改为匿名请求。当前 remote 仅支持 HTTPS `.tgz` 下载，不支持 `oci://`；调用方须在任务中指定
HTTPS Chart，不会猜测、自动替换仓库路径或 fallback。

Helm target binding 只绑定 target 的 cluster/instance 和 use=helmPull，不再把 namespace、workload、
release 或 Chart URL 写成 Gateway 授权条件。Agent broker 按凭据 payload 的仓库 URL
与本次 Chart 的 hostname、有效端口选择认证；不同源时不返回凭据 payload，
Chart 从自己的来源匿名下载，并通过相同的包内容校验。这保证凭据不会被发往无关 host，
同时允许同一 target 使用多个 namespace、release 和 Chart。

`ask --apply` 不必再用 `--credential` 点名 helmPull 凭据：授权已经由该 target 上的 active
grant 决定，Gateway 按 (user, cluster, instance) 发现候选并按稳定 ID 去重，
走的是与显式 `--credential` 相同的解析、物化与租约路径。Agent 按实际 Chart 的 HTTPS origin
选择：没有匹配时匿名下载，唯一匹配时使用该凭据；只有同一 origin 匹配多个不同凭据时才返回
`helm_credential_binding_ambiguous`，由任务用 `--credential <name>` 指明。
绑定多个不同来源的仓库不会阻断发布，公共 Chart 也不需要先选择无关的私有凭据。

公共 Chart 不需要新增凭据或授权记录。即使 target 已绑定私有仓库凭据，来源不同的公共
Chart 仍直接匿名请求，不会先携带私有凭据尝试下载。匿名请求支持至多 9 次 HTTPS 重定向，
以支持 GitHub Release 等公共 CDN；每跳独立校验并固定目标地址，拒绝私网、HTTP、
userinfo 与无效端口，不转发 Authorization 或 Cookie。CDN 返回的签名查询串仅用于下载，
不进入错误文本。认证失败不会转成匿名重试。

点名一个 helmPull 凭据时它始终优先进入候选绑定：发现不再参与，
也不会因为 target 上还绑着别的 Helm 仓库而报歧义。点名 `registry`、`tls`、`opaque` 不收回
Chart 下载的凭据：这些类型按下一段必须点名，而 Chart 仍用该 target 自己的 helmPull 凭据，
两者同时生效。所以「私有仓库 Chart + 运行期 Secret」是一条命令，不需要在点名运行期凭据和
拿到 Chart 之间二选一。

自动选择只覆盖 helmPull：物化一个 helmPull 凭据只在 Agent 进程内开一个临时 unix socket
broker，prompt 结束即删，不碰任何集群对象。`registry`、`tls`、`opaque` 的物化会在 namespace
里创建或替换 Kubernetes Secret，因此仍必须由调用方显式 `--credential` 点名。dosql 的自动
database 选择和 doweb 的网站变更不受影响。

仓库要求认证而该 target 没有绑定同源 helmPull 凭据时，匿名下载会被仓库拒绝；此时报
`helm_chart_requires_credential`（保留原始 `helm_artifact_http_401`/`_403` 便于排查），
CLI 随错误给出 `credential list`、`credential configure --use helmPull` 和重跑命令。

发布时 Gateway 仍只发送物化元数据。Agent 为本次授权 prompt 建立临时、无 payload 的
租约，结构化 Helm 内部匹配 session 与 credential/grant/version/resourceVersion；
namespace、release、Chart 不是授权条件。凭据物化不下载历史 Chart。实际发布下载当前
Chart，验证后只使用本次固定的 staging 文件；不读取、提交或回显 Secret 值。
受管发布保留认证、target 权限与固定下载文件，允许任务指定的零或多个有序
`value_files` 和非敏感 Chart-specific `set_strings`，不强制 bundled-only；覆盖资产不扩大
凭据授权，未授权的远程 values 不可使用。普通升级不再隐式保留历史 values；完整迁移和
发布校验契约见 [`NATURAL_LANGUAGE_DEPLOYMENT.md`](NATURAL_LANGUAGE_DEPLOYMENT.md)。

### Git Token

```json
{
  "url": "https://git.example.com/org/repo.git",
  "username": "deploy",
  "token": "<secret>"
}
```

验证执行 `git ls-remote --heads`；不验证特定 branch/ref。

### Model Provider

Agent 内置 doagent 的模型配置。Agent 部署不需要挂载模型 Secret，也不需要先配置模型：
Agent 连上 Gateway 后，以及之后每次 `doops_agent_prompt`（含控制台 metadata）之前，
Gateway 把该 target 上 active 的 `modelProvider` 凭据物化到 doagent 的固定配置文件
`/root/.agent/settings.json`。已连接的 Agent 如果已经持有这个版本，不再重复探测。
该 target 没有绑定，或绑定尚未 promote 时，不会从别的 target 取模型。

首次下发与重连直接走原生模型物化，不预先清除已下发的配置，也不要求
Kubernetes 或 kubeconfig。Agent 先执行模型探测，成功后才写入新配置；
探测失败会返回错误并保留现有配置，不将失败标记为已下发。

```json
{
  "model": "openai/gpt-5.4",
  "provider": {
    "openai": {
      "options": {
        "apiKey": "<secret>",
        "baseURL": "https://api.example.com/v1",
        "kind": "openai"
      },
      "models": { "gpt-5.4": { "type": "text", "features": { "toolCall": true } } }
    }
  },
  "image_provider": "openai",
  "image_model": "openai/gpt-image-2"
}
```

- `model` 必须是 `<provider>/<model>`，且该 provider 必须在 `provider` 中配置。
  `image_provider`、`image_model` 可选。
- `provider.<id>` 使用 doagent `settings.json` 的 provider 结构，原样写入。
  `apiKey` 必须是实值；`{file:...}`、`{env:...}` 等引用会被拒绝，因为 Gateway 是唯一密钥来源。
  `baseURL` 规则同 Helm/Git URL。
- verify 由目标 Agent 执行 `do-agent model probe`，payload 只走 stdin，真实调用一次模型。
  调用失败返回稳定类别（如 `model_auth_failed`、`model_unavailable`），不能 promote。
- 物化时 Agent 把模型字段与 Agent 自带的 doops/dosql MCP 服务和权限合并写入配置文件，
  权限 0600。doagent 每个新会话重新读配置，轮换后不用重启；进行中的会话不受影响。
- Agent 重启时只保留由 Gateway 下发记录的模型字段，其他来源（如旧挂载复制来的 key）一律丢弃。
  吊销 grant 或资源时，Agent 先从配置文件删掉该模型，再进入 revoked。
- 一个配置文件只放一个模型来源：同一 target 上第二个 `modelProvider` 绑定会被拒绝
  （`model_provider_already_bound`）。

doagent 在没有模型时也能启动，`/health` 正常；收到 prompt 时报告模型未配置并给出配置路径。

从旧的模型 settings Secret 迁移时，平台管理员用精确引用导入，Agent 只取 `model`、`provider`、
`image_provider`、`image_model`，其余字段丢弃，payload 不经过 stdin 或输出：

```bash
doops -session migrate credential configure --target <t> --namespace <secret-ns> \
  --name <name> --type modelProvider --scope platform \
  --from-secret <secret-name> --keys settings.json
```

导入后照常 grant、逐个 verify、promote；全部成功后再去掉 Agent 的旧挂载。

Helm/Git URL 必须为 HTTPS、不得包含 URL userinfo/query/fragment，并默认拒绝私网、
loopback 和 link-local 目标。Registry 可以使用无 scheme 的 registry host。

## 执行结果与资源所有权

凭据物化使用已有用户、target、session 和运行位置元数据。发布内部调用继承当前执行
所有权及 actor/token，不重复获取已持有的资源锁。取消 ACK 不表示物化或发布已停止；
缺少原始终态，或外部物化/清理/发布已被中断而无法确认副作用时，保留
`outcome_unknown` 并报告失败；本地命令退出不证明远端状态。不得自动接管、
重放或扩大凭据授权，人工声明不得改写审计或强制释放活动 lease。凭据锁按资源 ID 获取；多凭据按 ID
排序取得锁，等待取消时释放已取得的锁。不同凭据可并行，同一凭据的发布、授权、
轮换和吊销保持串行，避免执行期间权限或版本被改变。
内部子调用的终态审计失败必须传播失败，不得宣称成功；执行结束及清理完成后释放 lease，
原始终态证据独立重试落盘，不用审计重试阻塞后续执行。
历史未知审计不参与新任务准入，不改为 success，也不删除；实际活动任务仍按资源锁串行。
`executionLifecycle` 是后台执行准入条件，不是凭据物化、清理或前台 apply 的总开关；
这些调用继续要求真实终态并如实报告未知结果，不因缺少后台能力而拒绝。

## 一次配置（推荐）

`credential configure` 复用本文已有的 resource/version/grant/verification/promotion，
不是另一套凭据存储或授权系统。已有资源用 `--id`；新资源用
`--name <name> --scope <personal|platform> --type <type>`，名称和归属唯一匹配时复用，
不重复创建。同一 Gateway 沿用当前登录用户；`--target` 提供精确 cluster/instance。

已有目标 Kubernetes Secret 时，无须读取并粘贴密码：

```bash
doops -session db-setup credential configure \
  --target <target> --id <existing-credential-id> \
  --namespace <namespace> --use database \
  --from-secret <source-secret> --keys connection_uri
```

如果源 Secret 只有密码字段，可传不含密码的连接事实，由 Agent 内部组合：

```bash
doops -session db-setup credential configure \
  --target <target> --id <existing-credential-id> \
  --namespace <namespace> --use database \
  --from-secret <source-secret> --keys DB_PASSWORD --password-key DB_PASSWORD \
  --connection-uri 'postgresql://<user>@<host>:5432/<database>'
```

边界：

- `--from-secret` 仅用于 `opaque`，要求对应 target 的 grant、可管理资源、明确 namespace/Secret 和
  明确的 key 集合；源 Secret namespace 与本次物化 namespace 相同，不修改源 Secret。
- `--key-map source=runtime` 可将源 Secret key 映射为运行时 key；每个 source 必须同时出现在
  `--keys` 中，runtime key 不能重复。映射只改 key 名称，值仍通过 Agent 私有控制通道传输。
  例如 Test MySQL 将 `EXAM_MYSQL_JDBC_URL`、`EXAM_MYSQL_USERNAME`、`EXAM_MYSQL_PASSWORD`
  映射为 `SPRING_DATASOURCE_URL`、`SPRING_DATASOURCE_USERNAME`、`SPRING_DATASOURCE_PASSWORD`。
- URI scheme 支持 postgres/postgresql、mysql、oracle、sqlserver/mssql、mongodb，以及按同协议处理的
  mariadb、tidb（MySQL 协议）。URI 必须含用户名、host 和数据库路径，不允许密码、
  query 或 fragment；`--password-key` 必须是唯一选中的 key，输出 payload 只有 `connection_uri`。
  需要其它连接形式时由安全输入端通过 stdin 提交完整 payload，不自动改写或降级。
- 不带 `--from-secret` 时只读 stdin，格式与 `credential put` 相同；Secret 模式不读 stdin。
  导入值经内部 Agent 控制通道进入 Gateway 加密存储，不进入公开 RPC、prompt 或 CLI 输出。
- 导入失败返回真实类别，CLI 原样打印：`target_offline`（503）、`target_busy`、`target_queue_full`、
  `target_queue_timeout`、`operation_canceled`（409）、`credential_import_timeout`（504）、
  `credential_audit_unavailable`（500）、`credential_import_agent_unsupported`（502，Agent 太旧，先升级），
  以及 Agent 端的 `credential_import_source_unavailable`、`credential_import_key_missing` 等（502）。
  只有无法归类时才是 `credential_import_failed`。
- 自动为当前用户建立 target grant，读取并验证该资源全部 active target 后才 promote。
  不再要求 workload、镜像仓库、Helm release、Chart、数据库资产或其它业务绑定字段。
- 任一步失败即非零退出，不宣称就绪、不自动重试或清理已有资源。重跑会产生新的 staged version。
  该流程不是跨请求事务；失败前已完成的资源/版本/授权保留，输出仅含阶段 ID 和元数据。
- `phase=promoted` 表示受管凭据已激活；opaque/database 验证的是 Secret 物化与内容一致性，
  不是数据库登录或 SQL 执行成功。业务验收仍通过 DoSql 完成。

数据库凭据激活后，`doops -session <s> sql --target <t> ...` 自动使用当前 target 上全部 active database 绑定，
不再引用资源名；多个数据库凭据时用 `--database <name>` 选择目标库。其他 use 仍通过 `--credential <name>` 引用。
旧的分步命令保留给轮换、权限维护和故障诊断；Web 的版本/授权管理仍使用相同底层 API。
Secret 导入需要 Gateway 和目标 Agent 都发布此能力；旧端报错时应升级，不绕过到 shell 取密。

## 分步创建和首次激活

1. 创建 resource：

```bash
doops credential create \
  --target <target> \
  --name <resource-name> \
  --scope personal \
  --type registry
```

2. 从 stdin 写入 staged version：

```bash
printf '%s' '{"server":"registry.example.com","username":"deploy","password":"..."}' |
  doops credential put --target <target> --id <credential-id>
```

输出只包含 `credential_id`、`version_id`、`state` 和 `payload_digest`。

3. 创建 target grant：

```bash
doops credential grant \
  --target <target> \
  --id <credential-id> \
  --namespace <namespace>
```

该命令只建立凭据到 target 的绑定，不接收用户授权参数；谁能使用凭据由该用户的 target
操作权限决定。namespace、`--use`（省略时按凭据类型推断）、workload、image、Helm 和
`--required-keys` 等物化参数在这一步写入绑定，之后的 verify 和运行时物化都使用这里保存的值。

4. 验证 staged version：

```bash
doops -session credential-verify-<version> credential verify \
  --target <target> \
  --id <credential-id> \
  --version-id <version-id>
```

verify 只需要 target、凭据 ID 和版本，不再接受 namespace、use、keys 等参数；
Gateway 对该 target 上的每个 active 绑定，用绑定时保存的参数临时物化 staged version，
检查 Secret 类型/key 集合及类型特定外部能力，然后恢复当前 active version 或清理临时资源。
输出仅包含状态、resourceVersion、digest 和错误类别。要改物化参数，先吊销旧绑定再按新参数 grant。

5. 激活版本：

```bash
doops credential promote \
  --target <target> \
  --id <credential-id> \
  --version-id <version-id>
```

所有 active target binding 都必须存在该 staged version 的 verification。Promotion 只重新物化
当前 active target binding，旧 active version 变为 `superseded`。

## 轮换

轮换使用同一 resource：

1. `credential put` 创建新的 staged version。
2. 对每个 active target grant 执行 `credential verify`。
3. 检查验证输出只有 metadata。
4. 执行 `credential promote`。
5. 通过 `credential list --view managed` 和目标功能路径确认新版本生效。

不要创建同名临时 resource 替代轮换，不要通过参数、环境回显或 issue 传递 payload。

## 吊销

吊销单条 grant：

```bash
doops credential grant-revoke \
  --target <target> \
  --id <credential-id> \
  --grant-id <grant-id>
```

Gateway 先把 grant 置为 `revoking`，清理该 grant 的全部物化上下文，再置为 `revoked`。
清理不完时 grant 保持 `revoking`，重复执行同一命令会继续清理，已 `revoked` 的 grant 再次吊销直接返回成功。

物化记录决定哪些上下文需要清理：

- 物化成功（`verified`）、结果未知（`outcome_unknown`）或尚未结束（`pending`，例如 Gateway 在派发后重启）的上下文，
  必须由目标 Agent 清理；
- 确定没有改动目标的失败记为 `not_applied`，不需要清理。目标离线、请求编码失败等派发前的失败，
  以及 Agent 在写入前拒绝、或写入失败后已恢复快照并复核一致时返回的稳定错误类别，属于这一类；
  白名单和依据在 `credential_materialization_store.go` 的 `credentialMaterializationUnchangedCategories`；
- 同一上下文先前成功过的物化不会被后来一次「没有改动」的失败覆盖，记录保持先前的状态，仍需清理；
- 引入 `not_applied` 之前写下的 `outcome_unknown` 记录，无论错误类别是什么都按结果未知处理，
  因为当时的重试会覆盖先前成功的记录。升级前留下的这类记录不会被自动判为未改动，
  对应的绑定仍需目标在线、由 Agent 清理完才能进入 `revoked`。

一个上下文清理失败不会中断其余上下文。全部尝试后仍有上下文未清理时，按原因返回：

| 类别 | HTTP | 含义 |
| --- | --- | --- |
| `target_offline` | 503 | 需要清理的目标没有连接，清理调用没有发出 |
| `target_busy`、`target_queue_full`、`target_queue_timeout` | 409 | 目标正在执行其他操作，清理调用没有发出 |
| `credential_grant_revocation_outcome_unknown` | 502 | 清理已到达 Agent 但失败或结果未知 |

任何一个上下文结果未知时返回 `credential_grant_revocation_outcome_unknown`。
Gateway 不提供跳过清理或强制置为 `revoked` 的入口；目标恢复连接后重试即可。

吊销与 `ask --apply`、重连交付等使用并发时：

- resource 或 grant 一旦离开 `active`，新的物化记录就写不进去，使用会以 `credential_revoked` 失败，不会派发到目标；
- 进入终态前，Gateway 在同一事务里复查该 grant（resource 吊销时是该 resource 的全部 grant）还有没有
  `cleaned`、`not_applied` 以外的物化记录。有就不进入终态，保持 `revoking`，返回 502 结果未知类别；
  resource 吊销的返回体同样列出这些记录所在的 `cluster/instance`。重试会清理这些上下文；
- 重连交付在拿到凭据锁后复查 grant 状态，等锁期间被吊销的 grant 不再交付。

吊销整个 resource：

```bash
doops credential revoke \
  --target <target> \
  --id <credential-id>
```

Gateway 先把 resource 和它的全部 grant 一起置为 `revoking`，再按上面的同一套规则清理所有 grant 的物化上下文，
跳过 `not_applied` 记录。一个上下文失败不会中断其余上下文，全部清理完才把 resource、version 和 grant 置为终态。
清理不完时 resource 和 grant 都保持 `revoking`，返回体是类别加上仍未清理的 `cluster/instance` 列表，
例如 `target_offline: doops-zyj/test`；审计只记录类别。类别和状态码与 grant 吊销一致，
结果未知时为 `credential_revocation_outcome_unknown`（502）。重复执行同一命令会继续清理，
已 `revoked` 的 resource 再次吊销直接返回成功。Resource revoke 是终态，
当前没有恢复、删除、改名或改类型能力。

## Bundle

Bundle 当前只是 credential ID 的静态集合：

```bash
printf '%s' '{
  "name":"release",
  "scope":"personal",
  "items":[
    {"credential_id":"cred_..."},
    {"credential_id":"cred_..."}
  ]
}' | doops credential bundle-create --target <target>

doops credential bundle-get --target <target> --name release
```

当前限制：

- `ask --apply --credential` 只接受 resource name，不接受 bundle name；
- bundle 没有 update、list、revoke、grant、verify、promote 或自动物化；
- platform bundle 仅 platform admin 可读取；
- `bundle-get` CLI 只输出 bundle 基本元数据，不输出 items。

Web 控制台在「凭据」页的「凭据组」入口提供同样的创建和按名称查看，经 `/api/credential-bundles`
代理到 Gateway `/v1/credential-bundles`。操作步骤：

1. 选中 target，打开「凭据」页，点页头的「凭据组」；
2. 新建：切到「新建凭据组」，填写名称，选择归属（平台归属只有 platform admin 能创建成功），
   勾选要放入的凭据，点「创建凭据组」；创建成功后页面切回查看并显示该 bundle；
3. 查看：在「按名称查看」输入 bundle 名称，点「查看」或按回车。

界面行为：

- 创建时只能勾选当前账号「我管理的」有效凭据，请求体只含 name、scope 和 `credential_id` 列表；
  bundle 不包含凭据 payload，界面也没有 payload 输入框；
- 查看结果只展示 id、名称、归属、状态和所含 `credential_id`，响应中的其他字段在客户端丢弃；
- Gateway 错误按稳定类别（`credential_forbidden`、`credential_bundle_not_found`、
  `credential_audit_unavailable`、`credential_key_unavailable` 等）映射为中文提示，未知响应文本不显示；
- 演示模式拒绝创建，查看只返回本地合成的 `release-demo`。

## 发布引用

```bash
doops -session release-<commit> ask \
  --target <target> \
  --apply \
  --credential <resource-name> \
  --msg '在 <target> 的 namespace <namespace> 安装或升级 release <release>，使用 Chart <chart>；其它说明按任务需要提供。'
```

Gateway 不从 resource name 推断执行事实。release、namespace 和 Chart 必须来自任务或任务指定
的资产；凭据授权只绑定 target，事实读取不能扩大 target 权限。完整参数与终态契约见
[`NATURAL_LANGUAGE_DEPLOYMENT.md`](NATURAL_LANGUAGE_DEPLOYMENT.md)。

## 状态和可见输出

Resource：`active -> revoking -> revoked`。

Version：`staged -> active -> superseded`；resource 吊销后版本为 `revoked`。

Grant：`active -> revoking -> revoked`。

任何 CLI、Web、audit、verification 或 Agent 输出都不得出现 payload value。内部
Gateway-to-Agent 凭据控制面会把解密 payload 临时传给目标 doops-agent 物化工具；
payload 不进入公开 prompt、doagent 上下文或 metadata binding。

注意：Resource `state=active` 不等于存在 active Version。数据库执行必须同时满足：
active resource、active version、目标上的 active target 绑定和当前用户的目标操作权限，
以及该 version 在所有 active target binding 上完成 verification。标准顺序是
`credential put -> credential grant -> credential verify -> credential promote`。
缺少 active version 时，target credential list 会隐藏该 resource，DoSql 执行会被 Gateway
返回 `credential_version_not_found`（自动发现时目标上存在未 promote 的 database 绑定也返回此类别）。
绑定缺失按原因区分，均为 403，不把缺少配置说成 `credential_invalid`：

| 类别 | 含义 | 处理 |
| --- | --- | --- |
| `dosql_database_binding_missing` | DoSql 请求的当前 target 没有 active `use=database` 的 opaque 绑定 | `credential configure --use database` 绑定到当前 target |
| `dosql_credential_not_bound` | `doops sql --credential` 指定的凭据在当前 target 上没有 active `use=database` 绑定 | `credential list --target` 查看名称，或先 configure 该凭据 |
| `credential_binding_target_mismatch` | `--credential` 显式引用的凭据（发布、DoWeb 等非 DoSql 入口）只绑定在其他 `cluster/instance` | 在当前 target 上 grant/configure |
| `credential_binding_scope_invalid` | 当前 target 上的绑定 scope 不再通过校验（例如 namespace 无效） | grant-revoke 后用合法 namespace 重新 configure |
| `credential_grant_denied` | 其他绑定不存在的情况，例如绑定已吊销 | 检查 `credential list --target` |

`--credential` 引用的名称在当前 target 上不存在、属于他人、或只绑定在别的 target 时，
返回同一类别，不区分「不存在」与「无权访问」，避免泄露调用者看不到的凭据名称。
CLI 在该类别后给出 `next:` 步骤：先 `credential list --target` 查看本 target 的绑定，
再决定是改用 `--credential <name>` 还是去掉 `--credential`（单一绑定的 Helm 凭据会自动生效）。

其它稳定错误类别：

- `credential_scope_invalid`：凭据引用没有匹配到该 target 的 Gateway 授权，或物化
  namespace 无效。数据库资产与 SQL 规则由 DoSql/Agent 校验，不要把业务数据库字段塞回
  Gateway grant。
- `credential_verification_required`：staged 版本尚未覆盖全部 active target 的验证；
  对每个 active target 执行 `credential verify`。
- `credential preflight failed`：清单中的 credentialRef 缺失、use 不匹配、grant 已吊销
  或同名资源不唯一。修复受管凭据和精确授权，不得读取本地 secret 文件兜底。
- 物化失败按返回的错误类别定位目标或权限状态，不重试、不猜测，不把 payload 放入任务。

## 审计

所有 Credential mutation 在执行前创建 audit，审计不可用时 fail closed。记录包括：

- user/token ID；
- credential/version/grant ID；
- target、session、`cluster/instance` authorization scope 和执行元数据；
- action、时间、状态；
- resourceVersion、digest 或稳定错误类别。

审计不记录 payload。凭据 owner 和 platform admin 可以通过
Gateway `/v1/audit` 查询；控制台审计页的「按凭据」范围即 `session=<credential_id>` 查询，
详见 [DOOPS_API_GUIDE.md](DOOPS_API_GUIDE.md) 的审计查询。当前 CLI 尚未提供独立的
credential audit 子命令。

凭据审计分两类，查询方式不同：

| 类别 | 操作 | `cluster/instance` | 查询 |
| --- | --- | --- | --- |
| 资源级 | create、put、promote、resource revoke | 空 | `session=<credential_id>` |
| 目标级 | grant 创建和吊销、verify、Secret 导入、Gateway 内部的物化和清理调用 | 该次操作的目标，审计开始后的成功和失败都记录目标 | `cluster=<cluster>&instance=<instance>`，可加 `action=`；grant 和 verify 同时带 `session=<credential_id>`，按凭据也能查到 |

- 资源级操作的 `session` 和 `credential_id` 都是该凭据 ID。create 在成功后才有 ID，
  失败的 create 没有 credential ID；
- promote 和 resource revoke 可能影响多个目标，一条审计只有一组 `cluster/instance`，
  所以不填目标。它们在各目标上发出的物化和清理调用另有目标级审计，`session` 是物化记录里保存的会话；
- grant 吊销和 verify 失败时也记录目标；grant 已确定时同时记录 grant ID 和保存的 scope；
- verify 在审计开始前的校验失败（version 不存在、不是 staged、该目标没有 grant）不写审计；
  grant 吊销在 grant 确定之前的失败不带目标。审计只覆盖审计开始之后的成功和失败；
- resource revoke 失败时，返回体列出仍未清理的 `cluster/instance`，审计只记录稳定错误类别。
