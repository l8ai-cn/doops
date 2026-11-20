# 集群盘点底册与覆盖率对账

在「资产 → 平台看板」中选择集群，打开「覆盖率」，将基线切换为「当前台账预览」。管理员填写盘点凭据和整份在役清单，每行用 Tab 分隔资产类型、稳定编号、名称。需要将未接入资产分配给已授权来源时，再填写目标的 `cluster`、`instance` 两列。保存后核对分母和缺口，再填写批次编号并冻结。下载当前明细 CSV 会沿用所选批次、对象类型及结论筛选。

底册归属与关系证据分别保存。未接入资产计入分母，没有可见且已核验的关系时计为缺口。同一类型和编号在同一底册内重复时，整批保存返回 400。平台汇总时，跨集群的同一资产只计一次，可从明细查看各集群归属。机器和系统分别计算 80% 门槛，普通账号仅能核对其可见范围。

## 父级接入

- 平台入口调用 `AssetCoverageSummary({ onOpen })` 和 `AssetCoveragePanel({ session })`。集群入口给两者传入相同的 `clusterId`，取集群台账的稳定 `asset_id`。
- Web 代理为 `/api/assets/coverage`，Gateway 为 `/v1/assets/coverage`。GET 使用 `cluster_id`、`baseline_id`、`kind`、`status` 和 `format=csv`。省略 `baseline_id` 读取最新批次，`live` 读取当前底册。看板使用 `summary=1` 和可选的 `cluster_id`。
- 管理员 POST `save_register` 提交整份底册。`register` 包含 `revision`、`document_ref` 和 `entries`，首次创建传版本 0，后续保存传读到的版本。空数组表示确认空底册。凭据和来源按条目写进快照。
- 管理员 POST `freeze` 提交 `baseline_id`。冻结单个集群时必传 `cluster_id` 和刚核对的 `expected_revision`。版本变化返回 409 `register_changed`，刷新并重新核对后才能冻结。平台冻结在一个事务内保存平台快照及每份已登记的集群底册，返回的批次编号可直接回读。重复冻结不会覆盖原批次。
- 历史明细和导出都按读取时的授权过滤。对仅在底册中登记的资产，来源授权和该条目当前分配都须有效。历史平台批次缺少当时的集群快照时，集群查询返回 `scope_unavailable`，导出返回 409。

## 迁移与验收

已有机器、集群、节点绑定及业务系统台账后，按顺序应用 [覆盖率基线迁移](migrations/asset_coverage.sql) 和 [底册与集群快照迁移](migrations/asset_coverage_registers.sql)。两张底册专用表缺任一张时，Gateway 对账接口返回 503 `coverage_register_migration_required`。实际环境迁移归父级发布流程。

在 `backend/agent` 下执行 `go test -count=1 ./internal/server -run '^TestAssetCoverage' -v`。用两个不同授权的账号打开同一冻结版本，核对 JSON、页面与 CSV 的资产编号；撤权后重新读取旧版本，确认记录与导出同时收回。空导出仍保留相同范围的完整表头。
