# S5 本机巡检证据

在目标 Linux 主机的宿主 PID 命名空间运行 `host_evidence.py`，使用 Python 3.10+。将 Gateway 工单对应的三项稳定标识传给采集器。以下为开发示例，策略值由该机器的负责人确定。

```sh
python3 host_evidence.py --cluster dev --instance local --asset-id dev-host \
  --policy /etc/doops/host-policy.json --evidence-root /var/lib/doops/evidence
```

`--policy` 指向管理员维护的本地普通 JSON 文件，所有者为 root 或运行账号，组和其他用户不可写。策略与证据根目录的上级目录也应由可信账号管理。证据根目录需已存在。采集器在 `<根>/<cluster>/<instance>/<asset_id>/` 内加目录锁、写临时文件并原子替换每个检查的 JSON。同一 UID 的 runner 可从只读挂载消费这些文件。每项收据的 `policy` 附该项策略内容的 SHA-256。

```json
{
  "M1": {"min_uptime_seconds": 600},
  "M3": {"denied_comm": ["example-worker"]},
  "M4": {"sysctl": {"kernel.randomize_va_space": 2, "fs.protected_symlinks": 1}},
  "M5": {"window_seconds": 3600, "max_gap_seconds": 120, "privileged_groups": ["sudo", "wheel"]}
}
```

| 检查 | 观测与判定 |
| --- | --- |
| M1 运行与重启 | 比对 `/proc/stat` 的 `btime` 与 `/proc/uptime`。开机时长低于本机下限记一次发现。两者推算的时间相差超过 5 秒时留缺口。 |
| M3 可疑进程 | 针对本次枚举的 PID 逐个读取 `comm`，精确匹配清单中的内核短名（最多 15 字节）。明细保留观测数和命中数。PID 条目消失、不可读或 `/proc` 使用隐藏 PID 的挂载选项时留缺口。 |
| M4 配置合规 | 逐项比对指定 sysctl 的内核实测值与期望整数。支持 `kernel.randomize_va_space`、`kernel.kptr_restrict`、`fs.protected_hardlinks`、`fs.protected_symlinks`、`net.ipv4.ip_forward`。缺项、未知配置名或空策略时留缺口。 |
| M5 异常登录与特权组变更 | 读取本机原始 Linux Audit `audit.log` 末尾最多 8 MiB。`USER_AUTH`、`USER_LOGIN` 拒绝记录及命中配置组名的成功组管理记录计为发现。输出按审计记录计数，一次登录可能产生多条记录。 |

M5 将观测截止时间定为采集时刻向下取整到秒，再减去 `max_gap_seconds`。明细给出 `[截止时间 − window_seconds, 截止时间)` 的 UTC 起止时间，收据 `observed_at` 取截止时间。日志必须覆盖两端，窗口内事件序号须连续，相邻记录的间隔须在配置上限内。为了避免把缺少审计源当作零异常，当前策略要求窗口内至少各有一条可解析的认证和组管理记录。安静窗口、日志轮转导致开头丢失、丢事件、审计重启、配置切换或不支持的日志格式会留下缺口。

M5 的组变更范围是 shadow 工具通过 Audit 记录的具名组操作，包括 `GRP_MGMT`、`ADD_GROUP`、`DEL_GROUP`、`GRP_CHAUTHTOK` 和带组操作的 `USER_MGMT`。手工直接改文件、目录服务变更以及未接入本机 Audit 的登录渠道超出此窗口的观测范围。收据只写时间和计数。

退出码 `0` 表示四项都已取得证据，发现项通过 `violations` 交给 runner。存在缺口时退出码为 `1`，标准输出列出原因。对缺少策略或观测失败的检查，采集器撤下原收据。SG 没有配置采集时，runner 领取该目标的工单后会提交 `source_missing`。在 `backend/inspection` 下运行 `python3 -B -m unittest -v host_evidence_test` 可用临时夹具复验。
