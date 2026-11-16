# Doops Usage Guide

This guide holds command-level usage details. The skill file is only the entry point.

`-session <s>` names the operation context and its `/root/ws/<s>` workspace. Any valid name
works and nothing has to be created first. When it is omitted, the CLI generates `ses_<16 hex>`,
prints it to stderr, and proceeds; pass that name again to continue in the same workspace.
`pull` is the exception: it needs the existing session whose workspace it downloads.

## Command Choice

| Need | Command | Notes |
| :--- | :--- | :--- |
| List configured targets | `doops list` | Local config only; not live gateway state. |
| Copy newly online targets into local routes | `doops sync [--target <gateway-target>]` | Adds live targets this login can use and that are missing locally. Does not rename existing routes or remove offline ones. |
| List live gateway agents | `doops -session <s> targets --target <gateway-target>` | Checks only that configured route. It does not list or save the Gateway's other agents. |
| Inspect prompt admission | `doops -session <s> targets --target <gateway-target> --admission` | Reads live do-agent permits, owning turns and queued sessions without a model. |
| Retire an offline target | `doops -session <s> remove --target <gateway-target>` | Removes only the local route after a successful Gateway query confirms the exact route is not currently visible; online, unreachable, or unauthorized targets are refused. |
| Check Agent protocol capability | `doops -session <s> capabilities --target <t> --require structuredSSH` | Fails closed when the online Agent is too old for structured SSH. |
| Add a Gateway target | `doops add --name <n> --gateway <url> --cluster <c> --instance <i>` | Stores routing only; never stores a target token. |
| Run deterministic shell | `doops -session <s> exec --target <t> --cmd '<cmd>'` | Runs explicitly requested commands, including native Helm releases; see [delivery contract](NATURAL_LANGUAGE_DEPLOYMENT.md). |
| Sync OCI images | `doops -session <s> registry sync --target <t> --source-registry <src> --target-registry <dst> --tag <release> --image <name>` | Generic target-side `skopeo copy --all`, bounded timeout/retry, and manifest digest verification; auth payloads never enter CLI arguments. `--source-authfile` / `--target-authfile` are optional overrides for the authfile the Gateway delivered to the target. |
| Build an OCI image | `doops -session <s> image build --target <t> --context <remote-dir> --output oci --output-path <remote-file>` | Generic BuildKit build; supports `--platform` and `--output image --image <ref> --push` for a registry push. |
| Copy an OCI image | `doops -session <s> image copy --target <t> --source <ref> --destination <ref>` | Generic Skopeo transport-to-transport copy with optional authfiles and raw manifest digest verification. |
| Run nested SSH safely | `doops -session <s> ssh --target <t> --host <user@host> -- <command> [args...]` | Preserves remote argv across nested shells. |
| Run a long build | `doops -session <s> exec --bg --target <t> --cmd '<cmd>'` | One RPC waits for the Agent task's terminal result; no task-status polling or command retries. |
| Ask the edge agent to reason | `doops -session <s> ask --target <t> --msg '<task>'` | Best for diagnostics and multi-step investigation. |
| Run a release | `doops -session <s> ask --target <t> --apply [--credential <name>...] --msg '<facts>'` | Natural-language release with mutation authorization. |
| DoSql read-only query or scan | `doops -session <s> sql --target <t> --mode query [--database <name>] --request '<request>'` | Automatically binds every active database credential on the selected Gateway target; `--database` picks among several. No local connection file is read. |
| DoSql live schema inspection | `doops -session <s> sql --target <t> --mode query [--database <name>] --request '读取指定 database_asset_id 的表、列或索引结构'` | Uses one read-only engine catalog/information-schema query. `discover` only finds Kubernetes database assets; it does not inspect live tables. |
| DoSql change plan | `doops -session <s> sql --target <t> --mode plan --request '<request>'` | Returns business summary, risk and verification evidence without mutation. |
| DoSql direct SQL | `doops -session <s> sql --target <t> --mode apply [--database <name>] [--credential <name>] --sql '<sql>' [--verify '<read sql>'] [--dry-run]` | `--credential` picks one database credential bound to the target by name or ID, e.g. a migration account for DDL beside a DML-only application account. Rollback-capable batches execute in one transaction. Postgres rolls back DDL and DML together. MySQL/Oracle commit DDL implicitly: `--dry-run` rejects DDL (`dosql_dry_run_ddl_unsupported`), and a batch mixing DDL and DML is refused (`dosql_mixed_ddl_dml_unsupported`). Successful DDL returns `verified` with `implicit_commits`; a later failure after DDL committed returns `partially_committed`. Each real apply gets a per-database `version` in a hash-chained journal on the Agent (`$(dirname workspace root)/.doops-dosql-versions`, persists only if that path is on a persistent volume); the version is reserved before execution and an apply is refused if the journal is unwritable or corrupt. Queries and dry-runs get no version. MongoDB takes JSON operation documents instead of SQL (`{"collection":"c","op":"updateMany","filter":{},"update":{"$set":{}}}`, array or consecutive documents): writes plus `--verify` share one transaction and roll back together, `--dry-run` rolls back (replica set required, else `dosql_transaction_unsupported`); `createCollection/createIndex/dropIndex/drop` are non-transactional and follow the same DDL rules as MySQL; `$where/$function/$accumulator`, `$out/$merge` and `system.*` are refused; auth retries once with `authSource=admin` and reports `auth_source`. |
| DoSql table copy | `doops -session <s> sql --target <t> --mode apply --copy-from <src-db> --database <dst-db> --tables a,b [--dry-run]` | The Agent copies whole tables between two databases bound to the same target, so rows never pass through the 200-row result limit or the caller. Both databases must use the same engine (mysql or postgres) and the destination tables must already exist. Source is read in a read-only transaction; all batches (500 rows) insert in one destination transaction, so any failure or `--dry-run` leaves the destination untouched. Counts as one versioned apply on the destination (`dosql_copy_*` errors name the cause). |
| DoSql target discovery | `doops -session <s> sql --target <t> --mode discover --request '<request>'` | Uses `mcp_dosql_read` to scan non-Secret target metadata; it never stores connection secrets. |
| DoSql repository/resource discovery | `doops -session <s> sql --target <t> --mode discover --root <repo> --resources-file <assets.json> --project <id> --environment <id> --dosql-script <path>/dosql-agent.mjs` | Scans the repository's code/config metadata and persists a metadata-only database inventory. |
| DoWeb read-only inspection | `doops -session <s> web --target <t> --mode check --request '<site/Nginx/TLS/health request>'` | Uses the selected Gateway target and DoWeb evidence path. |
| DoWeb site plan | `doops -session <s> web --target <t> --mode plan --request '<site publish request>'` | Produces SiteSpec, Nginx plan, checks and rollback without mutation. |
| DoWeb controlled publish | `doops -session <s> web --target <t> --mode apply --credential <web-resource> --request '<site, dist, domain, proxy, acceptance>'` | Requires the target grant and `nginx -t`/reload/health evidence. |
| Push a local directory | `doops -session <s> push --target <t> --src <dir>` | Syncs into `/root/ws/<session>`. |
| Pull a remote workspace | `doops -session <s> pull --target <t> --dest <dir>` | Use for large files and binary assets. |
| Read small text | `doops -session <s> read --target <t> --path <file>` | Small text only. Default COS mode prints a presigned GET; use `--storage gateway` to print content. Paths are relative to the session workspace. |
| Write small text | `doops -session <s> write --target <t> --path <file> --file <local>` | Also supports `--content` and stdin. Default COS mode stores the object and prints a presigned GET; use `--storage gateway` for a direct session-workspace write. Paths are relative. |
| Read system information | `doops -session <s> info --target <t>` | CPU, memory and disk metadata. |
| Use structured Kubernetes operations | `doops -session <s> k8s --target <t> ...` | Structured reads and operational changes; Helm delivery entries are in the [delivery contract](NATURAL_LANGUAGE_DEPLOYMENT.md). |
| Clean workspace | `doops -session <s> clean --target <t> --workspace <s>` | Cleans `/root/ws/<workspace>`. |
| Check an image deployment | `doops check --target <t> --deployment <d> --namespace <ns> --image <repo/image>` | Compares the Deployment's main container image with the exact image you pass; `--namespace` defaults to `default`. |
| Upgrade a managed Helm Agent | `doops -session <s> upgrade --target <t> --image <repository:tag 或 repository@sha256:<digest>` | In-cluster Job only. `--dry-run` previews parameters. |
| Release an existing standalone Docker Agent | External `scripts/release-host-agent.sh` | Explicit DockerHost controller, not CLI fallback; real read-only `--dry-run`. See [SOP](BUILD_DEPLOY_SOP.md#独立-dockerhost-发布). |
| Cache token | `doops login ...` | Gateway login can exchange username/password for user token. |
| List target credentials | `doops credential list --target <t>` | Uses the cached gateway login and returns only metadata granted to that user on the selected cluster/instance. |
| Preflight development credentials | `doops -session <s> credential preflight --manifest .agent/doops-credentials.yaml` | Fails closed unless every declared credentialRef is active and bound to the selected target. The manifest `use` describes the requested operation and does not create a second permission. |
| Configure a credential once | `doops -session <s> credential configure --target <t> --id <id> [--namespace <ns>] [--use <use>] <source-and-scope-flags>` | Reuses managed storage, stages, binds to the target, verifies and promotes. See `CREDENTIAL_RESOURCES.md`. |
| Manage credential lifecycle | `doops credential <create|put|grant|verify|promote|grant-revoke|revoke>` | See `CREDENTIAL_RESOURCES.md`. |
| Create or read a credential bundle | `doops credential bundle-create --target <t>` (JSON from stdin) / `doops credential bundle-get --target <t> --name <n>` | A static set of credential IDs, no payload. The Web console `凭据` page has the same actions under `凭据组`. See `CREDENTIAL_RESOURCES.md`. |
| Create Gateway user and API key | `doops admin user create --target <t> --user <u> (--password-stdin\|--generate-password)` | Removes bootstrap wildcard access and applies only explicit `--grant-target` scopes. |
| List Gateway users | `doops admin user list --target <t>` | Returns metadata only. |
| Manage target grants | `doops admin grant <list\|add\|delete> ...` | Grants use exact configured `cluster/instance` routes. |
| Issue token as admin | `doops admin token create ...` | Requires a gateway token with `admin` permission. |
| Remove cached token | `doops logout --target <t>` | Removes local token cache. |

DoSql 必须使用 `doops sql` 入口。普通 `doops ask --msg '使用 DoSql'` 不保证注入
`mcp_dosql_read`，也不能替代 DoSql 的 target discovery、credential scope 或 journal/evidence 链。
模型的 DoSql MCP 域只提供 `read`，自然语言 `--request` 只支持 query、plan、discover；
变更需求先整理为可审查的 SQL，再用 apply 的 `--sql/--file` 直接交给 Agent 执行。
用户已明确授权的 target、数据库和变更范围持续有效，不要求每次调用重复批准；只有范围
不清或超出已有授权时才补充确认。Gateway target grant 和执行器保护仍按实际请求检查。

DoSql 使用凭据只要求该 target 的 grant，不需要逐操作员绑定。
Gateway 直接解析保存的 target/database 绑定和 active version，不让模型生成凭据 scope。
不存在 active version 时先用 `credential configure` 安全导入；该错误明确为 `credential_version_not_found`。
不带 `--credential` 时，Gateway 按 ID 绑定当前 target 上全部 active 的
`use=database` 凭据。只有一个绑定时直接使用；有多个时按 `--database` 匹配凭据指向的数据库，
未指定时返回 `dosql_database_required` 并列出各绑定指向的 `engine/database`。
`--database` 的值取列表里的 **database 部分**（例如 `lab_management`）；
为兼容从 `credential list` 复制名称的习惯，也接受唯一的 credential/binding 名并解析到其 database。
同一数据库绑了多个账号时用 `--credential <name>` 选择账号。

真实 apply 返回 `version`、`journal_path` 和 `evidence_path`；query/dry-run 返回
`evidence_path` 但不占 version。`journal_path` 是目标上的 per-database hash 链
journal（begin/end + statement 指纹 + committed/verified），用于审计和恢复；
`evidence_path` 是本次 operation 的完整 statement/columns/rows 证据。
同一个库绑了多个账号（例如只做 DML 的应用账号和做 DDL 的迁移账号）时，用
`--credential <name|cred_id>` 指定本次使用哪一个；它只用于 `--sql/--file`，只能选中当前 target
上已绑定的 database 凭据，不新增任何授权。名字不在当前 target 的 database 绑定里时返回
`dosql_credential_not_bound`；两个 owner 的凭据同名时返回 `credential_reference_ambiguous`，改用凭据 ID。
当前 target 没有 `use=database` 绑定时返回 `dosql_database_binding_missing`；绑定 scope
失效时返回 `credential_binding_scope_invalid`。`--database` 没有凭据指向该库时返回
`dosql_database_not_bound`；多个凭据指向同一库时返回 `dosql_credential_ambiguous`，
直接 SQL 可用 `--credential` 收窄选择。完整对照见 `CREDENTIAL_RESOURCES.md`。

没有数据库绑定不是终点：用户的数据库需求就是任务，AI 应自己找到库并完成绑定，不向用户索要密码。
上述绑定类错误，CLI 的 `next:` 行和 `doops guide database` 会按顺序给出命令：

1. `doops sql --target <t> --mode discover --request '...'` 识别引擎、主机、库名。
2. `doops k8s --target <t> get deployment <workload> -n <ns> -o yaml` 读取环境变量的
   `valueFrom.secretKeyRef.name`，得到工作负载使用的 Secret 名（Secret 值不可读，也不会输出）。
3. `doops credential configure --target <t> --name <n> --type opaque --scope personal --namespace <ns> --use database --from-secret <secret> --keys <engine-specific-keys>`
   由 Agent 就地导入，走完整的 create、put、grant、verify、promote 生命周期。MySQL 的
   `jdbc:mysql` 来源选 `SPRING_DATASOURCE_URL,SPRING_DATASOURCE_USERNAME,SPRING_DATASOURCE_PASSWORD`；
   PostgreSQL、MongoDB、Oracle、SQL Server 或 MySQL URI 来源选 `connection_uri`。
   Secret 键映射及仅有密码时的 `--connection-uri/--password-key` 组合见
   [凭据导入说明](CREDENTIAL_RESOURCES.md)。
4. 保留原 target 和 `--database` 重跑原命令；绑定失败不代表可以改用另一数据库。

### DoSql 直接执行 SQL

已知确切 SQL 时用 `--sql` 或 `--file`，SQL 原样提交给 Agent，不经过模型，与 `--request`
二选一。只支持 `--mode query` 和 `--mode apply`，引擎为 postgres、mysql、oracle、sqlserver、mongodb；MariaDB、TiDB 走 mysql 路径，CockroachDB 走 postgres 路径，由凭据 `connection_uri` 的 scheme 决定。

其它数据库可由运维在 Agent 主机放置外置驱动：可执行文件 `dosql-driver-<scheme>`（默认目录 `<工作区根的上级>/.doops-dosql-drivers`，与版本日志同处，Agent 换 Pod 后随 agent-home 保留；可用 `DOOPS_DOSQL_DRIVER_DIR` 覆盖，不得 world-writable）。Agent 每批调用一次，DSN（含密码）只经 stdin 传入，helper 在进程内完成事务、回滚和 dry-run，并把 `doSQLDirectResult` JSON 写到 stdout；版本日志和 evidence 仍由 Agent 写。没有对应文件时返回 `dosql_engine_unsupported`。模板见 `backend/agent/cmd/dosql-driver`，Agent 不自动下载驱动。

```bash
# 只读：每条语句都必须是 SELECT/WITH/SHOW/DESCRIBE/EXPLAIN/VALUES/TABLE
doops -session db-check sql --target <t> --mode query --database orders \
  --sql 'SELECT count(*) FROM orders'

# 可回滚的 DML 批次：执行和验证后回滚，migrate.sql 只包含本次授权的 DML
doops -session db-change sql --target <t> --mode apply --database orders \
  --file migrate.sql --verify 'SELECT count(*) FROM orders WHERE status IS NULL' --dry-run

# 已授权的真实变更：核对 dry-run 返回值后执行
doops -session db-change sql --target <t> --mode apply --database orders \
  --file migrate.sql --verify 'SELECT count(*) FROM orders WHERE status IS NULL'

# MySQL/Oracle 隐式提交 DDL：单独执行，不加 --dry-run，不与 DML 混批
doops -session db-schema sql --target <t> --mode apply --database orders \
  --credential orders-migrate --sql 'ALTER TABLE orders ADD external_id VARCHAR(64)' \
  --verify 'SELECT external_id FROM orders WHERE 1 = 0'

# 提交后回读结构；DML 则回读数据并对照任务验收值
doops -session db-schema sql --target <t> --mode query --database orders \
  --credential orders-migrate --sql 'SELECT external_id FROM orders WHERE 1 = 0'
```

- 不要在 SQL 里写 `BEGIN/COMMIT/ROLLBACK/SAVEPOINT`，事务边界由 DoSql 持有；写了返回
  `dosql_statement_transaction_control_forbidden`。
- `--verify`/`--verify-file` 只能是一条只读语句，否则 `dosql_verification_invalid`；
  查询执行失败返回 `dosql_verification_failed`；返回的行和值须与任务验收条件比较，不会自动
  将非零计数判为失败。`--verify` 与 `--dry-run` 只用于 apply。
- query 中出现写语句返回 `dosql_query_requires_read_only`，改用 `--mode apply`。
- Postgres 和 SQL Server 的 DDL 与 DML 一起回滚。MySQL 和 Oracle 的 DDL 会隐式提交：`--dry-run` 含 DDL
  返回 `dosql_dry_run_ddl_unsupported`，DDL 与 DML 混批返回 `dosql_mixed_ddl_dml_unsupported`；
  全部执行及验证成功时返回 `status=verified`、`committed=true` 和 `implicit_commits`；
  已有 DDL 提交、后续语句或验证/提交失败时返回 `status=partially_committed`，不报成已回滚。
  此类 DDL 按已有明确授权独立执行，事先准备恢复方案，执行后回读结构并核对结果；
  验证查询失败也不能撤销已提交 DDL。不要为满足向导而更换数据库或绕过执行器保护。
- 真实 apply 在 Agent 上按库写带哈希链的版本日志（`.doops-dosql-versions/<db>.jsonl`），
  执行前先分配版本号；日志不可写返回 `dosql_version_journal_unavailable`，被篡改返回
  `dosql_version_journal_corrupt`，两者都拒绝执行。query 和 dry-run 不产生版本。日志只在
  Agent 的 `/root` 是持久卷时跨重启保留。
- 结果是带 `operation_id`、`statement_fingerprint` 的 JSON，证据写入目标 session 工作区
  `.dosql/operations/<operation_id>.evidence.json`，用 `pull --storage gateway` 取回当前远端工作区；
  默认 COS pull 只下载此前上传的快照，不包含远端执行后新生成的证据。
- query 和 apply 都只要求该 target 的 grant；数据库选择规则同上一节。

MongoDB 也走 `--sql/--file`，但语句是 JSON 操作，不是 SQL。一批可以是 JSON 数组，也可以是连续多个文档：

```bash
doops -session db-change sql --target <t> --mode apply --database <name> \
  --sql '{"collection":"orders","op":"updateMany","filter":{"status":null},"update":{"$set":{"status":"new"}}}' \
  --verify '{"collection":"orders","op":"count","filter":{"status":null}}'
```

- 读（`--mode query` 可用）：`find`、`count`、`aggregate`、`distinct`、`explain`（`verbosity`）、`listDatabases`、
  `listCollections`、`listIndexes`、`dbStats`、`collStats`、`currentOp`（只返回 opid/op/ns/耗时/client，不含命令文本）、
  `serverStatus`、`replSetStatus`、`buildInfo`。库级和服务级操作不需要 `collection`。
- 写：`insertOne/insertMany/updateOne/updateMany/replaceOne/deleteOne/deleteMany`，以及原子的
  `findOneAndUpdate/findOneAndReplace/findOneAndDelete`（`return` 取 `before` 或 `after`）。
- 运维（非事务，等同 DDL 规则）：`createCollection`、`createIndex`（`unique/sparse/expireAfterSeconds/partialFilter`）、
  `dropIndex`、`drop`、`renameCollection`（`to`）、`collMod`（`options`）、`compact`、`killOp`（`opid`）、
  `dropDatabase`（`confirmDatabase` 必须等于当前目标库名，否则返回 `dosql_mongo_confirm_mismatch`）。
- 刻意不支持：用户和角色管理（`createUser` 等需要密码，会进入语句、版本日志和证据）、`$where/$function`、
  `$out/$merge`。这些要用独立的受控流程。
- 管理类读取不在事务内，看不到同一批里尚未提交的写入。
- 一批写操作与 `--verify` 在同一事务内，失败整批回滚，`--dry-run` 回滚。事务需要副本集，单机返回
  `dosql_transaction_unsupported`。
- `createCollection/createIndex/dropIndex/drop` 不能进事务：`--dry-run` 含它们返回
  `dosql_dry_run_ddl_unsupported`，与写操作混批返回 `dosql_mixed_ddl_dml_unsupported`，执行后计入 `implicit_commits`。
- `$where/$function/$accumulator` 返回 `dosql_mongo_javascript_forbidden`；聚合 `$out/$merge` 返回
  `dosql_mongo_stage_unsupported`；`system.*` 集合、未知操作或结构错误返回 `dosql_mongo_operation_invalid` /
  `dosql_mongo_document_invalid`。`deleteOne/deleteMany` 必须显式给 `filter`（`{}` 表示全部）。
- 认证库默认取 URI 中的库，认证失败时用 `authSource=admin` 重试一次；实际使用的库见结果的 `auth_source`。

`registry sync` is a thin generic wrapper over the target Agent's audited exec path. The
standard DoOps Agent image provides `buildctl` for image builds and `skopeo` for OCI
copy/inspect. The command performs copy and source/destination manifest digest checks;
it has no AMP-specific path and never installs a missing tool at runtime. If the target
does not advertise `skopeo`, the operation fails with a missing-tool error; upgrade the
DoOps Agent image or use a target that already provides it.

`image build` and `image copy` are thin generic wrappers over the same audited exec path.
They use standard BuildKit and Skopeo transports, so the command does not depend on Docker,
nerdctl, Harbor, AMP image names, or a project-specific script. For private Registry output,
the target uses the authfile the Gateway delivered for the registry credentials granted on it;
credentials are never accepted inline in the command arguments. `--source-authfile` and
`--target-authfile` override that file when a target keeps its registry configuration somewhere
else, and both may be omitted.

Web 控制台的「镜像」标签提供同样的三项操作（构建、复制、Registry 同步）。表单字段与上面的
CLI 参数一一对应，生成与 CLI 相同的 `doops_shell` 命令；authfile 只填目标上的路径，不接收凭据内容。
点击执行直接提交该操作，结束后界面列出 Agent 输出中的 manifest digest。

## Basic Checks

```bash
doops list
doops version
doops -session smoke targets --target <gateway-target>
doops -session smoke exec --target <gateway-target> --cmd 'hostname && date -u +%FT%TZ'
doops -session smoke-capabilities capabilities --target <gateway-target> --require structuredSSH
doops -session image-toolchain-check info --target <gateway-target>
```

`info` 输出中的 `buildctl: OK`、`skopeo: OK` 和 `buildkit-sock: OK` 表示目标具备
DoOps 通用镜像工具链。镜像构建使用明确给出的 `exec` 命令调用 `buildctl`；镜像复制
和源/目标 manifest 校验使用 `image copy` 或 `registry sync`。两者都不需要 AMP 专用脚本或外置同步 Job。
目标缺少 `skopeo` 时直接报告工具缺失，不在线安装，也不静默切换到其它工具。私有 Registry
凭据由 Gateway 下发到目标的受控 authfile，命令参数里不出现任何凭据。

运行态文件与配置同目录：`auth.json`（按 Gateway URL 保存的登录态及 token 元数据）、`history.jsonl`
（本机命令历史）、`sessions.json`（本机会话缓存）。`~/.config/doops/config.json`、
`~/.doops/auth.json` 和项目 `.doops/` 是旧路径，不作为当前配置依据。

`doops version` 的 `release` 和 `commit` 标识已安装 CLI；
`doops targets` 的 `RELEASE_COMMIT` 标识目标 Agent。日常操作不要求两者同版，
所需能力按实际调用检查。Agent 自身升级的候选制品与版本验收见
[构建与部署 SOP](BUILD_DEPLOY_SOP.md)。`cli-contract/v3` 通过 `doops --contract`
检查协议兼容性，不是另一个发布版本。

`doops list` reads local config. `doops targets --target <t>` asks the Gateway whether that one configured route is visible. A target can be listed locally but unavailable in the Gateway because it is offline or because the logged-in user lacks the exact target grant; the CLI does not collapse these cases into a definitive offline diagnosis. A newly registered agent does not appear in `doops list` until its route is stored locally.

`doops sync` is the local update. It uses each configured Gateway login, or the Gateway selected by `--target`, and calls the same `/v1/targets` list. Every live target that login can use and that is not already routed locally is appended. The new route name prefers the instance name, then `cluster-instance`, and does not reuse an existing name or alias. Existing names, aliases and descriptions are not rewritten. Offline agents are absent from that list, so their local routes stay until `doops remove`. The command stores routing only and never a token. One failed Gateway does not discard routes already added from the others.

To remove a stale local route, use `doops -session <s> remove --target <gateway-target>`. The command first queries the configured Gateway and refuses to modify the config if the Gateway is unreachable, authentication fails, or the exact route is currently visible. It does not delete Gateway grants, audit records, proxy routes, or historical agent status.

`doops targets` 的状态：

这些状态只描述 Gateway 操作计数。`ACTIVE=0 / QUEUED=0` 不代表 do-agent 内部名额空闲。
增加 `--admission` 会通过节点信息 RPC 读取 `runtime/gcStats` 的 `admission.current`：
`limit`、`inUse`、`available`、`holders`（session/turn）及
`queues`（session、queued、oldestWaitSecs）。已取得但尚未归属 turn 的局部名额计入
`inUse`，不出现在 `holders`。该读取不运行模型、不创建 prompt、不重启运行时；
旧 Agent/运行时不支持或读取失败时明确报错，不能将缺失数据显示为零。

- `busy=true` 只表示 Agent 自升级占用整个 target；普通操作不会置位。
- `status=active` 且 `busy=false` 表示有 session 在跑。Shell/SSH/bg/apply 可并发，同 session 也不串行，
  但该 session 的 workspace 写入会等它们结束；session 只是并发归属，不是沙箱，不要从两个 session 同时改同一
  release 或数据库。
- `resources` 列出 `workspace:<session>` 等当前持有的锁。

`exec` 的 Shell 从 Agent 进程目录启动，需要 session 工作区时显式 `cd /root/ws/<session>`。
CLI 原样输出通知文本（含 JSON、空行、分块）；超时会请求取消并继续等原请求终态。

`capabilities --require structuredSSH|executionLifecycle` 只做握手，不证明 Agent 支持自升级；
只有 `exec --bg` 需要 `executionLifecycle`。其它名称在联网前即报参数错误。

执行生命周期（终态、`outcome_unknown`、取消返回 `202 cancel_requested` 不代表已停止、
`finalizing`/`audit_completion_failed`、SSH 退出码 `1..254` 与 `255` 的区别、并发与锁）见
[`DOOPS_GATEWAY_TUNNEL.md`](DOOPS_GATEWAY_TUNNEL.md#multi-user-behavior)。Gateway 控制面发布（`fetch-gateway-release.sh` +
`deploy-gateway.sh --no-build`）见 [`BUILD_DEPLOY_SOP.md`](BUILD_DEPLOY_SOP.md)。

## Gateway Admin Tokens

给用户签发 token 时，优先用 CLI 管理入口，不要 SSH 到 gateway 主机手工跑
`doops-gateway token create`：

```bash
doops admin token create \
  --target demo \
  --user gaojiaqi \
  --name maintenance \
  --expires 720h
```

`--target` 必须是本地配置里的 gateway target，并且它当前使用的 token 要有
`admin` 权限。新 token 只打印一次；需要写入本机缓存时显式使用：

```bash
doops admin token create \
  --target demo \
  --user gaojiaqi \
  --name maintenance \
  --expires 720h \
  --save-as demo
```

查看 token 元数据和撤销旧 token：

```bash
doops admin token list --target demo
doops admin token revoke --target demo --id <token-id>
```

列表只返回 ID、前缀、归属和状态，不返回 token 明文。管理员凭据只从
`--target` 对应的本地受限存储读取，不得通过命令行参数传入。
不带 `--expires` 的 token 不返回 `expires_at`，表示没有到期时间；不会显示为
`0001-01-01T00:00:00Z`。密码登录默认签发 30 天可续期 token（距签发最长 90 天），
每次登录都会产生新的 token ID；同一用户未过期的密码登录 token 超过 10 个时，
最早的旧 token 会被撤销。CLI 会在替换本机记录时打印旧、新 token ID。

## Gateway Users And Target Grants

CLI 已封装用户创建、API key 签发和精确 target grant 管理。管理员 token 只从统一
`auth.json` 读取并放入 Authorization header，不得出现在命令行参数、请求 URL、日志或
文档中。用户密码只通过 stdin、交互式终端或 `--generate-password` 输入，禁止放入命令
参数或自然语言 prompt。

创建新用户并签发 API key：

```bash
printf '%s' '<PASSWORD_FROM_SECURE_INPUT>' | \
  doops admin user create \
  --target <gateway-target> \
  --user <username> \
  --password-stdin \
  --grant-target <allowed-target> \
  --name user-create \
  --expires 720h
```

`--grant-target` 可以重复。没有指定时，CLI 会删除后端 bootstrap grant，不授予任何
target 权限，并明确输出 warning。`--generate-password` 可替代 `--password-stdin`。
CLI 会先查同名用户，拒绝重复创建；创建或授权、签发、最终复核任一步失败都会停用新用户，
若 API key 已签发则同时尝试撤销，命令以失败退出。

用户和授权的只读/维护命令：

```bash
doops admin user list --target <gateway-target>
doops admin grant list --target <gateway-target> --user-id <user-id>
doops admin grant add --target <gateway-target> \
  --scope-target <allowed-target> --user-id <user-id>
doops admin grant delete --target <gateway-target> --id <grant-id>
```

已有用户可以设置或轮换密码：

```bash
printf '%s' '<PASSWORD_FROM_SECURE_INPUT>' | \
  doops admin user password --target <gateway-target> \
  --user-id <user-id> --password-stdin
```

停用或恢复用户：

```bash
doops admin user disable --target <gateway-target> --user-id <user-id> --disabled=true
doops admin user disable --target <gateway-target> --user-id <user-id> --disabled=false
```

先确认请求对应的精确路由，并检查同名用户：

```bash
doops list
doops targets --target <allowed-target>
```

```http
GET /v1/admin/users
Authorization: Bearer <ADMIN_USER_TOKEN>
```

同名用户已存在时停止创建，转为核对或调整现有 grant。创建普通用户必须显式使用
`admin:false`：

```http
POST /v1/admin/users
Authorization: Bearer <ADMIN_USER_TOKEN>
Content-Type: application/json

{
  "name": "alice",
  "password": "<PASSWORD_FROM_SECURE_INPUT>",
  "admin": false
}
```

新用户没有任何 target grant。为每个允许的 target 创建一条 grant：

```http
POST /v1/admin/grants
Authorization: Bearer <ADMIN_USER_TOKEN>
Content-Type: application/json

{
  "user_id": "usr_xxx",
  "cluster": "doops-oilan",
  "instance": "oilan-node"
}
```

grant 覆盖该 target 的全部操作和它绑定的凭据，没有 action 维度。授权失败时停用用户再退出：

```http
POST /v1/admin/users/disable
Authorization: Bearer <ADMIN_USER_TOKEN>
Content-Type: application/json

{"user_id":"usr_xxx","disabled":true}
```

### End-to-End Verification

不要用新用户登录覆盖本机管理员的共享 Gateway 登录态。创建一个临时目录，将所需的
routing-only targets 写入其 `config.json`，并让 `DOOPS_CONFIG` 指向该文件；其同目录
`auth.json` 将保存隔离的验证登录态。

```bash
DOOPS_CONFIG=<temporary-dir>/config.json \
  doops login --gateway <gateway-url> --username alice --name user-create-smoke

DOOPS_CONFIG=<temporary-dir>/config.json \
  doops targets --target <allowed-target>

DOOPS_CONFIG=<temporary-dir>/config.json \
  doops -session user_create_smoke exec --target <allowed-target> --cmd 'hostname'

DOOPS_CONFIG=<temporary-dir>/config.json \
  doops -session user_create_smoke_denied exec --target <denied-sibling> --cmd 'hostname'

DOOPS_CONFIG=<temporary-dir>/config.json \
  doops -session user_create_smoke clean --target <allowed-target> --workspace user_create_smoke
```

允许 target 必须成功，未授权 sibling target 必须非零退出并被 Gateway 拒绝。最后用
`doops admin token list` 按 `user-create-smoke` 标签找到 token ID，再执行
`doops admin token revoke`。`doops logout` 只删除本地缓存，不能替代服务端撤销。安全删除
临时配置目录后，再交付账号。

## Exec

```bash
doops -session ops exec --target demo --cmd 'hostname && df -h && free -h'
```

Use `exec` for deterministic commands, read-only checks, and known deployment scripts.
标准 Agent 镜像提供出站 `ssh` 客户端。多层 SSH 必须使用结构化入口，不能把 SSH
命令拼进 `exec --cmd` 的单个字符串；这样可在 OpenSSH 边界保留每个远端 argv，避免
嵌套引号被远端 shell 重解析。这不会创建 SSH target，也不会启动 `sshd`。SSH 目标、
参数、远端命令和认证材料必须来自当前明确任务，禁止把私钥或密码内联到命令、prompt、
日志或工作区。

```bash
doops -session ops ssh --target demo --host ops@app.internal \
  --option=BatchMode=yes -- bash -lc 'hostname && systemctl status api'
```

`--option` 只接受 `-4`、`-6`、`-p`、`-i`，以及 `BatchMode`、`StrictHostKeyChecking`、
`ConnectTimeout`、`IdentitiesOnly`、`IdentityFile` 和 `Port`。`key=value` 简写会转换为
`-o<key>=<value>`。会改写目标或执行本地命令的选项会被拒绝。
CLI 只拦截直接以 `ssh`、`/usr/bin/ssh` 或 `/bin/ssh` 开头的 `exec` 命令，不是完整 Shell
分析器；包装命令仍须由调用者改用结构化入口。

`exec` 也可执行明确给出的原生 Helm `upgrade/install/rollback/uninstall`。该路径遵循
target 授权、session 隔离和完整退出审计，使用命令自身的退出状态与回读结果；它不替代
`ask --apply` 的可信结构化工具证明。Helm 资产、凭据和失败语义见
[`NATURAL_LANGUAGE_DEPLOYMENT.md`](NATURAL_LANGUAGE_DEPLOYMENT.md)。

## Ask

```bash
doops -session ops ask --target demo --msg '只读检查 Kubernetes 节点状态，给出结论和风险'
```

Use `ask` for diagnostics that require exploration. The target must have doagent/model configuration available.

`--database-ref <id>` 和 `--repo-ref <id>[@branch]` 可重复，只把引用 ID 作为
`[DoOps 任务引用]` 前缀拼进任务文本，不授予数据库或仓库权限，也不物化凭据。
`--model` 为本次指令选择模型。`--credential` 只允许与 `--apply` 一起使用。

## Kubernetes Operations

`doops k8s` 调用 Agent 的 `doops_k8s` 工具，不走 shell `kubectl`：

```bash
doops -session ops k8s --target <t> get pods --namespace apps
doops -session ops k8s --target <t> describe deployment api --namespace apps
doops -session ops k8s --target <t> logs deployment/api --namespace apps --tail 200
doops -session ops k8s --target <t> top pods --namespace apps
doops -session ops k8s --target <t> events --namespace apps --for deployment/api
doops -session ops k8s --target <t> rollout status deployment/api --namespace apps
doops -session ops k8s --target <t> get pods -A -l app=api
doops -session ops k8s --target <t> get networkpolicy worker -n apps -o yaml
doops -session ops k8s --target <t> get deploy -n apps -o 'jsonpath={.items[*].spec.template.spec.containers[0].image}'
doops -session ops k8s --target <t> logs pod/api-0 -n apps --previous --since 10m
doops -session ops k8s --target <t> helm list -A
doops -session ops k8s --target <t> helm history zhiyong -n zyj
doops -session ops k8s --target <t> helm status zhiyong -n zyj
doops -session ops k8s --target <t> helm values zhiyong -n zyj   # 只返回 key 路径
# 以下子命令直接执行指定变更
doops -session ops k8s --target <t> rollout restart deployment/api --namespace apps
doops -session ops k8s --target <t> scale deployment/api --replicas 3 --namespace apps
doops -session ops k8s --target <t> node cordon <node>
doops -session ops k8s --target <t> delete pod <pod> --namespace apps
```

可用参数：`--namespace`/`-n`、`--all-namespaces`/`-A`、`--selector`/`-l`、`--container`/`-c`、
`--output`/`-o json|yaml|wide|name|jsonpath=...|custom-columns=...`、`--tail`、`--previous`/`-p`、
`--since`、`--replicas`、`--timeout`、`--for`。可读 kind 包括工作负载、Service/Ingress、
ConfigMap、NetworkPolicy、HPA、PDB、RBAC、StorageClass、CRD、事件和存储；Secret 与
ServiceAccount 不开放。`helm values` 只返回 key 路径，`helm status` 只返回发布名、命名空间、修订和
`info` 里的状态与时间，不返回 values、manifest、hooks 和 notes，因为它们里可能有明文。
`get/describe/logs/top/events/rollout status/helm` 是只读查询，可与 target mutation 并行；`rollout restart/scale/cordon/uncordon/delete pod`
使用 target mutation lease。两类都只要求该 target 的 grant。版本发布不走这里，用
`ask --apply`。

Web 控制台的「Kubernetes」标签用同一个 `doops_k8s` 工具和同一组参数完成上述操作：选择操作、
填写 kind/名称/资源/命名空间等字段后执行，输出与 CLI 相同。写操作（滚动重启、扩缩容、
cordon/uncordon、删除 Pod）点击执行即提交，不再要求确认框或二次批准。
迁移旧调用时删除 CLI 的 `--confirm` 和 RPC 的 `confirm` 字段；只读与变更操作仍分别处理。
Web 另有只读的「ConfigMap 键名」，调用 Agent 的
`configmap-keys`（可选名称、命名空间、全部命名空间、标签选择器），只返回命名空间、名称和
data/binaryData 键名，不返回值；CLI 目前没有对应子命令。结果超过 64 MiB 时 Agent 拒绝返回，
需要缩小到单个命名空间或用选择器。

## Development Credential Preflight

需要 Git、Registry、Helm、TLS 或 API 凭据的项目，在仓库中提交 metadata-only 清单：

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

Agent 在相关开发操作前运行：

```bash
doops -session dev-preflight-<task> credential preflight \
  --manifest .agent/doops-credentials.yaml
```

CLI 严格解析单文档 YAML，拒绝未知字段、重复的 `credentialRef + use`、target 冲突和
没有 `-session` 的远端检查。它只查询当前登录用户在 manifest target 上可见的 active
resource 和 target binding，输出 `ready`、`missing`、`wrong_use` 或 `ambiguous` 元数据。

清单不是授权源，也不包含 payload、secret 文件路径、默认值或本地 fallback。preflight
不会把凭据注入本机命令或远端 workspace；实际使用仍必须经过 target binding 和受控
materialization 路径。

## CI/CD And Apply

自然语言发布用 `doops ask --apply`。任务写明 target、release、namespace、Chart 和选用的
values/Helm 选项；需要时再写明镜像事实、原因和业务验收条件。Helm 的原生 values 语义、
凭据预物化、成功回执和失败处理见
[`NATURAL_LANGUAGE_DEPLOYMENT.md`](NATURAL_LANGUAGE_DEPLOYMENT.md)。

```bash
doops -session release-<commit> ask \
  --target <configured-target> \
  --apply \
  --credential <resource-name> \
  --msg '在 <target> 的 namespace <namespace> 安装或升级 release <release>，使用 chart https://charts.example/<name>.tgz；原因：<reason>；如需业务验收：<acceptance-condition>。'
```

`--credential` 可重复，只传 resource name；Gateway 按 target grant 选择凭据，并只向匹配的
HTTPS origin 发送认证。CI 可以调用同一命令，但 CI 记录、镜像证明和 provenance 不是 Helm
发布的前置门禁；CLI、Agent 和 Helm 工具不自动证明构建来源。Agent 自身升级不走这条路径，
见下文 Upgrade。

## Push

```bash
doops -session build push --target demo --src .
doops -session build exec --target demo --cmd 'ls -la /root/ws/build | head'
```

`push` defaults to COS. The bucket, endpoint, region, and keys come from the Gateway process configuration, not from `--credential`. Pass `--storage gateway` to keep the Git workspace sync:

- Gateway: `/v1/git/<cluster>/<instance>/<session>.git`

The default upload uses the Gateway's COS configuration. A missing Gateway COS key or a failed bucket check stops in `cos` and does not fall back to Git. When the Agent is online the snapshot is also copied into `/root/ws/<session>`; otherwise the objects remain in COS and the result says the workspace was not synced.

COS snapshots preserve the executable bit recorded in the Git snapshot. New files and replacements restore as `0755` (executable) or `0644` (not executable); they do not inherit an old destination file's executable bit. Only regular Git files are supported: symbolic links and other tree types are rejected before upload instead of being flattened. Push checks the Gateway's snapshot capability on each connection; the Gateway checks the selected Agent before sending a sync. Upgrade the unsupported component when either capability is missing. Ordinary COS `read/write` do not require snapshot capabilities.

Literal dots in filenames, including Next.js `[...path]` and `[[...path]]` route
directories, are supported. A `..` path segment remains invalid; a filename
containing multiple dots is not a parent-directory reference.

Agent 的 Git handler 只接受 Gateway 反向隧道中的进程内调用，不暴露本地
HTTP `/git` 入口。

Do not reimplement push with tar/base64/chunk upload. `doops_workspace_*` chunk tools are low-level compatibility JSON-RPC tools, not the standard CLI/deploy path; seeing 512KB chunk logs means a script is bypassing the supported Git HTTP path. Push respects `.gitignore` and `.doopsignore`; do not hide deploy manifests or scripts behind ignore rules unless intentional.
Git evaluates `.gitignore` for both repository and ordinary-directory sources, including
nested rules, ordered `!` inclusions and parent-directory exclusions. The transport commit
preserves the selected files, including tracked files ignored for future additions.
DoOps default exclusions and `.doopsignore` still apply before Git filtering.

## Pull

取回远端执行后生成的文件，显式选择当前工作区：

```bash
doops -session build pull --storage gateway --target demo --dest ./doops-build-output
```

Use `pull --storage gateway` for files generated in the remote workspace, including archives, SQL dumps, images, and videos. It creates a snapshot of the current remote files before downloading. Do not use `read` or `exec base64` for downloads.

Default `pull` reads the snapshot manifest from the Gateway's COS bucket and downloads the previously uploaded commit into `--dest`. `--commit` selects an uploaded snapshot. COS pull does not export files created or changed on the Agent after the upload:

```bash
doops -session build pull --target demo --dest ./uploaded-snapshot
```

COS pull validates the entire manifest before writing files. A snapshot without executable metadata returns `cos_snapshot_permissions_missing`: push again from the original source using the current CLI to create a complete snapshot. Existing objects are not deleted or guessed into a new permission state. This is an executable-bit contract, not a backup of UID/GID, ACLs or all POSIX mode bits; see the [COS API contract](DOOPS_API_GUIDE.md#cos-存储模式).

## Read And Write

```bash
doops -session ops read --target demo --path deploy.sh
doops -session ops write --target demo --path notes.txt --content 'hello'
doops -session ops write --target demo --path pod.yaml --file ./pod.yaml
cat ./pod.yaml | doops -session ops write --target demo --path pod.yaml
```

File API paths are relative to the named session workspace. The Web file browser
uses the same paths for open, save, new-file and upload, and its navigation stays
inside that workspace. The one absolute form accepted is a knowledge-base file,
`<DOOPS_KB_ROOT>/<name>.md` (default `/root/kb`); any other absolute path,
including `/root/ws/<session>/...`, is rejected as unsafe. Use explicit Shell
operations for other host paths.

`read` and `write` are for small text files and short scripts. Use `push/pull` for directories and binary assets.

`read` prints a presigned GET and leaves the object in COS. `write` stores the file under the caller's prefix in the Gateway's COS bucket and prints the object key plus a presigned GET. Files of 16 MiB or less are uploaded by the Gateway; larger files are PUT directly to COS. `--storage gateway` keeps the old session-file commands.

## Clean

```bash
doops -session build clean --target demo --workspace build
```

Confirm the session name before cleaning. Do not share one session across unrelated tasks.

## Login And Logout

```bash
# 首次登录 / 尚无本地路由：直接给 Gateway URL，CLI 会自动发现并写回可见 target
doops login --gateway https://gateway.example.com --username alice
printf '%s' "$DOOPS_GATEWAY_TOKEN" | doops login --gateway https://gateway.example.com --token-stdin

# 已有路由后的常规登录
doops login --gateway https://gateway.example.com --username alice
printf '%s' "$DOOPS_GATEWAY_TOKEN" | doops login --gateway https://gateway.example.com --token-stdin
# --target <name> still works as an alias that resolves to that target's Gateway

doops logout --target demo
doops logout --gateway https://gateway.example.com
```

本地还没有 target 路由时，用 `--gateway <url>` 做首次登录：CLI 先交换或导入 token，再调用
`GET /v1/targets` 把该账号可见的在线 target 写回本机路由（已有路由的名称、别名和描述不改）。
`--gateway` 与 `--target` 同时给出时必须指向同一个 Gateway；`--target` 仍要求路由已存在。

CI 和自动化只用 `--token-stdin` 传入已有 Gateway user token，不能与 `--username` 混用。
密码登录使用终端隐藏输入。`login --token` 和 `login --password` 已移除，旧脚本须改为
上面的 token stdin 导入；不会保留明文 argv 兼容路径。不要把 token 或密码写进命令行参数。

Login and logout apply to the Gateway, not one target. `auth.json` stores one
credential record under the normalized Gateway URL, including the token ID,
username, source, issue time, and expiry. The
plaintext token is still required for requests, but the record makes a login
replacement auditable. Existing legacy `{"tokens": {"gateway": "token"}}`
files are migrated to the versioned `credentials` shape when the CLI next saves
the file; their unavailable metadata stays unknown until the token is explicitly
imported again. There is one runtime credential map. Mixed schemas, unknown
versions, empty tokens, and conflicting normalized Gateway records are rejected.
Saves replace the complete file atomically through a private `0600` temporary file.
Target configuration contains routing only; it must not contain a
Gateway user token. Gateway permissions still come from the user's grants.

Importing a token calls `GET /v1/auth/me` to validate it and read its authoritative
metadata; it does not issue a new token or infer the owner from CLI flags. Failed
validation leaves the cached credential unchanged. Password login issues a new
token and prints the replaced token ID/account plus the new account and expiry;
it does not renew or revoke the previous token. The CLI loads and validates the
local auth file before contacting the login endpoint.

`doops logout` only removes this local credential record. It does not revoke the
Gateway token. To invalidate a token at the Gateway, use `doops admin token revoke
--target <t> --id <token-id>`; an expired, revoked, disabled-user, or missing
token now returns a distinct structured authentication reason instead of one
generic sentence.

## Updating Metadata

没有通用的 `update` 命令。各类元信息按下表修改；“替换”表示先建新的再删旧的。

| 对象 | 如何修改 | 不能改的部分 |
| :--- | :--- | :--- |
| 本机 target 路由 | 同名再执行 `doops add --name <n> ...` 原地覆盖 gateway/cluster/instance/aliases/use；alias 与其它 target 冲突时拒绝。`doops sync` 只补当前登录可见、本地还没有的在线路由 | 只有确认离线才能 `remove`；`sync` 不删除、不改名 |
| Gateway 用户 | `admin user password` 改密码；`admin user disable --disabled=true\|false` 停用/恢复 | 用户名、`is_admin` 无修改入口 |
| target grant | 替换：`admin grant add` 新范围，再 `admin grant delete --id` 旧的 | grant 不支持原地改 scope |
| Gateway token | 替换：`admin token create` 新 token，确认后 `admin token revoke` 旧 token | token 不能续期或改名 |
| 凭据 payload | 轮换：`credential put` → 对每个 active 绑定 `verify` → `promote`；或对已有资源重跑 `credential configure --id <id>`（每次生成新版本） | 历史版本不可改 |
| 凭据 target 绑定 | 同一 scope 重新 `credential grant` 会复活已吊销绑定；换 scope 要新 `grant` 再 `grant-revoke` 旧的 | 绑定不支持原地改 scope |
| 凭据资源 | 只能 `credential revoke` | name、type、scope 无修改入口；要改就新建资源 |
| 网站代理路由 | `doops admin proxy set --id <id> [--host] [--scope-target] [--upstream] [--enabled]` 原地修改，只改传入字段；新建省略 `--id`，见 `DOOPS_GATEWAY_TUNNEL.md`；平台管理员也可在 Web 控制台「管理 → 代理路由」新建、编辑、启停和删除 | — |
| Edge 域名路由 / enrollment | 当前没有 HTTP 或 CLI 管理入口；存储层的增改删只被测试调用 | 不要直接改 Gateway SQLite |
| Agent 的 cluster/instance | 由 Agent 部署配置（Helm values）声明，改它就是重新发布 Agent，见 `BUILD_DEPLOY_SOP.md` | CLI 和 Gateway 都不能改在线 Agent 的身份 |

## Credential Resources

凭据模型、payload schema、权限、分步生命周期（create/put/grant/verify/promote/revoke）、轮换、
bundle、审计和错误类别见 [`CREDENTIAL_RESOURCES.md`](CREDENTIAL_RESOURCES.md)。

首次配置用一条命令完成写入、绑定、验证和激活：

```bash
# payload 从 stdin 输入
printf '%s' '<payload-json>' | doops -session setup credential configure \
  --target <t> --name <name> --type <type> --scope personal --namespace <ns> --use <use>

# 从目标上已有 Secret 导入，不回传密码
doops -session setup credential configure --target <t> --id <id> \
  --namespace <ns> --use database --from-secret <secret> --keys connection_uri

doops credential list --target <t>                 # target 可用的凭据
doops credential list --target <t> --view managed  # 本人管理的凭据
```

已有资源传 `--id`，避免重复创建；源 Secret key 与运行时契约不同时加 `--key-map source=runtime`。
`doops sql` 默认自动使用 target 上的 database 绑定；`--sql/--file` 可用 `--credential`
选择其中一个，详见本指南的 DoSql 参数说明。

## Upgrade

入口按运行方式选择，参数、验收和回滚见 [`BUILD_DEPLOY_SOP.md`](BUILD_DEPLOY_SOP.md)：

| 场景 | 入口 |
| :--- | :--- |
| 安装或更新 CLI / Skill（不动 Agent） | `bash scripts/install.sh --version <vX.Y.Z> --project "$PWD"` |
| 已声明 `upgradeAudit` v1 的 K8s Agent | `doops upgrade`（下方示例）；`release-managed-agent.sh` 只委托此入口 |
| K8S 首装、旧 Agent 首升或 Helm 配置变更 | 按 SOP 单独审查；旧脚本的本地变更入口已停用，升级必须经 Gateway 提交独立 Job |
| 已有独立 Docker Agent | `scripts/release-host-agent.sh`，见 SOP“独立 DockerHost 发布” |
| 空 Ubuntu/Debian 服务器 | `scripts/bootstrap-host-agent.sh`，见 [`CLUSTER_INSTALL_GUIDE.md`](CLUSTER_INSTALL_GUIDE.md#空服务器-docker-首装) |

安装器从 GitHub Release 下载带校验的 CLI 到 `~/.local/bin/doops`，回执写在
`~/.local/share/doops/install.txt`，其中 `commit` 与 `skill_commit` 一致。Skill/docs 取自 CLI
内嵌 commit，不取当前工作区；缺少该 commit 时安装在替换前停止。

```bash
doops -session agent-upgrade upgrade --target <t> --image <repository>@sha256:<digest> --dry-run
doops -session agent-upgrade upgrade --target <t> --image <repository>@sha256:<digest>
doops -session agent-upgrade upgrade --target <t> --rollback
```

- 镜像可以是 `repository:tag` 或 `repository@sha256:<64 hex>`；回滚只回到有验收证据的历史 revision。
- `--dry-run` 只预览参数，不检查 release、挂载或权限。`PREVIEW` 不是已部署，`ACCEPTED` 不是已完成。
- `OK` 表示 Job、`structuredSSH` 探针和 `release_commit` 已核验；之后仍要用同一 target 的
  `exec hostname` 确认实际服务的 Pod，并验证功能路径。
- `doops upgrade` 只换 Agent 镜像或回滚，不改 `proxy.allowedHosts`、`hostAliases` 等 Helm 配置。
- 每个目标生成一个 `upgrade_operation_id`，CLI 输出 operation/session 并把成功或失败结果写入
  本地 history。审计查询使用 `correlation_id=<operation>`，预检、执行、观察和自动回滚共享此编号。
  历史、终端和 verbose 输出中的凭据会脱敏；来源元数据在网络请求及历史写入前校验，拒绝值不回显。
- 可用 `--source-commit <40hex>` 和 `--ci-url <https-url>` 记录发布来源；URL 不接受凭据、
  query 或 fragment。这是操作者提交的来源元数据，不替代调用前核验 CI 与 digest。
- `--rollback` 要求历史回执包含已验证的 `release_commit`；缺失或实际 commit 不一致时不会打印 `OK`。
  审计收尾失败时 CLI 保留 `audit_completion_failed`，继续观察已受理 Job 并完成验收或必要回滚，
  最终仍返回非零，不重复提交升级。

## BuildKit Availability

Release images are built by the controlled build pipeline (CI or a designated build host) from the exact source commit. BuildKit in an
Agent is a runtime capability, not permission to build release images on a
business target or to make `push` a prerequisite for `ask --apply`.
Check availability without starting a build:

```bash
doops -session buildkit-check exec --target <target> --cmd 'buildctl --version'
```

Long-running non-release commands use `exec --bg`, not
`&`, `nohup`, or `setsid` inside a foreground `exec`. The native task service owns
the process, logs and exit status; it does not grant deployment authority.
