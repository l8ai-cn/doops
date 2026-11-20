# 周检执行器

独立进程每次启动执行一轮。为计划绑定普通 Gateway 用户 ID，向该用户授予计划所需的精确 `cluster/instance`。在主机定时器或 CronJob 中每五分钟用 Python 3.10+ 启动 `runner.py`。一轮处理本账号最多 32 个到期周期，最多领取 256 个检查项，90 秒后停止领取；每次 HTTP 超时 8 秒。CronJob 使用 `concurrencyPolicy: Forbid`、`backoffLimit: 0` 和 `activeDeadlineSeconds: 120`。失败退出由作业监控处理，下一轮从 Gateway 持久化进度继续。

进程需要 `INSPECTION_GATEWAY`（HTTPS origin）、`INSPECTION_TOKEN_FILE`（挂载的执行账号 token）和 `INSPECTION_EVIDENCE_ROOT`（可信采集端的只读目录）。定时器命令示例为 `*/5 * * * * /usr/bin/python3 /opt/doops/inspection/runner.py`。Token 不进入计划、收据或日志。凭据轮换保留执行用户 ID。

## Linux 定时运行

仓库提供 [systemd 服务](systemd/doops-inspection.service) 和 [定时器](systemd/doops-inspection.timer)。执行机上使用专用本地账号 `doops-inspection` 读取 `/etc/doops/inspection/runner.env` 指定的 Gateway 地址、只读证据目录和该执行账号的 token 文件。把 `runner.py` 放在 `/opt/doops/inspection/`；部署这两个 unit 后启用 `doops-inspection.timer`。定时器每五分钟检查持久化计划，计划本身定义周几、时间及时区。可用 `systemctl list-timers doops-inspection.timer` 核对下次触发，`journalctl -u doops-inspection.service` 核对每轮完成或失败，控制台按计划查看周期报告。

## 数据源

机器可用 [Linux 本机证据生产器](HOST_EVIDENCE.md) 发布运行、进程、配置、登录与权限观测，业务服务可用 [应用探针](APPLICATION_EVIDENCE.md) 发布 A1–A5。两者应在采集侧按业务所需窗口运行，让执行器只读挂载产出的证据目录。报告中的采集缺口保留具体原因，包括服务无响应、访问被拒及数据过期。

- M2 从 Gateway `/v1/assets/monitor` 读取同目标、同资产的 `machine/exporter` CPU 和内存实测值，按已配置阈值判断瞬时值。两条序列都需要本次取证窗口内的新样本和阈值。
- V1、V2、V3 从 `<根>/<cluster>/<instance>/<asset_id>/sbom.json` 读取部署态 CycloneDX JSON。`metadata.timestamp` 是部署清单的实际观测时间。`metadata.properties` 中的 `doops:cluster`、`doops:instance`、`doops:asset_id` 必须与工单一致。读取组件 `purl`、`version` 和嵌套 `components`。
- V1、V2、V3 默认读取 `/var/lib/doops-advisories/advisories.json`，可用 `INSPECTION_ADVISORY_FILE` 指定同步器发布的独立文件。公告与清单按精确 PURL 匹配，保留平台限定符。Go 支持 Debian 包版本及三段数字版本；清单与公告摘要随收据保存。签名镜像配置、修复判定和本机 `dpkg-sbom` 命令见[公告与清单](ADVISORIES.md)。
- M1、M3–M5 和 A1–A5 读取相同资产目录下 `<检查编号>.json`。对接本机/应用已有监控、配置基线、脱敏审计或已配置业务探针，由可信采集程序在只读挂载的上游写入目录原子发布结果。每份 JSON 必须标明 `asset_id`、`cluster`、`instance`、`check_id`，并包含 RFC3339 `observed_at`、`source`、`policy`、正整数 `samples`、整数 `violations`（0 至 samples）及脱敏 `detail`（实际数值、单位、窗口或事件计数）。检查名称由 `view=overview` 的 `checks` 返回。部署时限制证据目录和公告供应者的写权限，保留原文件用于按 SHA-256 对账。采集器只消费可信挂载文件和 Gateway 只读指标，采样不足、离线、撤权及逾期都保留原因。

## 验收

创建时预览授权和在线状态。手动与定时运行各自在计划及周时隙内去重，提前手动运行不占用调度槽。执行账号停用或定时器中断且过期 24 小时后，管理员页面列出最早未对账周期。点击「冻结本期漏检清单」生成全缺口报告并推进一个周期，不领取工作或提交证据。重放同一 `(plan_id,due)` 返回原报告。固定清单记录目标、资产、检查编号和实际时间。修复登记后重新取证须使用新的 generation，比较同组件、同公告和同修复线。M2 与业务策略类收据保留逐次原始测量；软件类保留版本及版本区间。报告按当前登录者的目标授权过滤后生成与下载。周期和补检窗口各 24 小时。长期离线的执行器恢复后逐批补记到期周期。
