# 从授权目标的宿主采样到设备曲线

| 操作 | Gateway 权限与取数范围 |
| --- | --- |
| `GET /v1/assets/monitor` | 有效用户凭据和对应 `cluster`、`instance` 的读取授权。指定 `cluster_id` 时还会检查集群台账绑定。 |
| `POST`，`action=collect` | 普通账号可对获授的在线目标指定一个命名空间。Gateway 通过该目标的 Agent 执行固定的 `kubectl top pods --namespace …`，需要指标服务可用且 Agent 身份能读取该命名空间的 Pod 指标。结果为 Pod CPU 和内存。用户的目标授权本身不细分 namespace。 |
| `POST`，`action=ingest` 或 `rule` | 要求有效的平台管理员用户凭据和已登记、未归档的目标。普通账号即使能读取该目标，写入仍返回 403。管理员身份在 Gateway 中涵盖所有目标。 |

**1. 在台账对应的可信宿主采样。** 在宿主上用已有受控任务执行下列单次命令，将标准输出作为该次任务的 JSON 结果。宿主不需要 Gateway token。将示例标识、网卡和挂载路径换成该任务绑定的实际值。

```sh
python3 backend/scripts/asset_monitor_collector.py \
  --cluster east --instance node01 --type machine --asset-id SRV-001 \
  --interface eth0 --interface eth1 --mount / --mount /data
```

Linux 主机采集要求宿主根目录、可访问的 PID 1 根目录与命名空间、`/proc`、`/sys` 和 `/usr/bin/systemd-detect-virt`。请直接在宿主运行。Agent 容器内请求 `machine`、`os`、`kernel` 观测会在输出前拒绝。容器应用上下文的指标只能按其实际应用来源解释。网卡速率为各接口 RX 与 TX 字节增量之和除以观测秒数。挂载点要求规范绝对路径，每次从同一文件系统快照取得 `storage_total`（总字节）、`storage`（已用字节）和 `storage_available`（普通用户可用字节）。预留块可能使三者无法简单相加。指定设备读不到会返回 `missing` 和 `null`，仍保留设备标识。

**2. 在已有受控中心验证来源并提交。** 中心按已认证的采样任务或只读证据通道取得 JSON，将该来源绑定到预先批准的 `cluster/instance/asset_id` 和设备清单。示例为单次任务，`snapshot.json` 是刚从该任务收到的只读结果。中心本地只读密钥文件 `MONITOR_AUTH_HEADER` 含一行 `Authorization: Bearer …`，仅提交进程可读；`MONITOR_GATEWAY` 由中心配置为固定 HTTPS origin。以下命令一次验证并提交同一份内容，校验失败时终止。

```sh
set -e
body=$(jq -ces '
  if length != 1 then error("expected one observation") else .[0] end
  | select(.action == "ingest" and .cluster == "east" and .instance == "node01")
  | select((.samples | length) == 8 and all(.samples[];
      .type == "machine" and .asset_id == "SRV-001" and .namespace == ""))
  | select(([.samples[] | [.metric, .device_id]] | sort) ==
      ([["network","eth0"],["network","eth1"]] +
       [(["storage","storage_total","storage_available"][]) as $m |
        (["/","/data"][]) as $d | [$m,$d]] | sort))
  | {action,cluster,instance,samples}' snapshot.json)
printf '%s' "$body" | curl --fail-with-body --silent --show-error --proto '=https' \
  --header "@$MONITOR_AUTH_HEADER" --header 'Content-Type: application/json' \
  --data-binary @- "$MONITOR_GATEWAY/v1/assets/monitor"
```

返回 `{"action":"ingest","accepted":8}` 表示 Gateway 接收了该批 8 项记录，含缺测记录。需要曲线时按同一来源绑定多次执行。采集器的 `--gateway` 会读取 stdin 的 bearer 并直接 POST；它仅适用于执行位置本身已获准持有对应账号凭据的情况。普通用户 token 可用于 `--pod-namespace` 的 `collect`。外部主机指标使用以上中心提交路径，管理员密钥只驻留中心。普通用户的目标授权可精确到单一 `cluster/instance`。

**3. 用获授该目标的普通账号验收。** 在「资产 → 监控」选择采集目标、机器和资产编号。两张网卡各有一条 bytes/s 曲线，两个挂载点各有三条 bytes 曲线。管理员可在具体曲线保存阈值和持续时间；现有规则按持续超过阈值触发。回读示例为 `GET /v1/assets/monitor?cluster=east&instance=node01&type=machine&asset_id=SRV-001&origin=exporter&metric=storage&device_id=%2Fdata`。省略 `device_id` 返回匹配的所有序列。`metric=storage&device_id=` 或 `metric=network&device_id=` 可精确回读旧的无设备维度序列。

新建数据库使用 [监控基础表](migrations/asset_monitor.sql)。已经运行带 GPU `device_id` 版本的数据库，由受控数据库变更流程在专用 SQLite 连接上执行 [设备维度升级](migrations/asset_monitor_devices.sql)。升级保留序列 ID、样本、规则、告警及旧空设备标识。SQL 应在首个错误处终止并关闭连接。
