# 机器详情的宿主试采

在「控制台 → 资产 → 机器」选中已关联目标的机器。首次绑定需要平台管理员。目标 Agent 须使用该 cluster / instance 的有效领取凭据连接 Gateway。后续有当前目标授权的账号可以查看记录和刷新。

## 准备宿主采样服务

在受信任的 Linux 宿主控制台执行 `tr -d '\n' </etc/machine-id | sha256sum`，保存第一列的 64 位小写 SHA-256 到独立的资产记录。虚拟机可在客体操作系统内执行。每台宿主应有唯一的 machine-id。

将 `backend/scripts/machine_diagnostic_collector.py` 安装在宿主，准备 Python 3.10+ 和 `/usr/bin/systemd-detect-virt`。创建仅允许运维账号写入的证据根目录和宿主绑定文件。例如 `/etc/doops/host.json` 的结构如下，将指纹替换为控制台取得的值。

```json
{"asset_id":"srv-01","cluster":"east","instance":"a","host_sha256":"<64 位小写十六进制>","applications":{}}
```

在宿主服务或 systemd timer 中每分钟运行一次下列命令。采集器检查宿主环境、machine-id 和绑定文件，原子发布当前分钟的 JSON。Agent 只需要对该目录及文件有读取权限。在已领取的 Agent 服务配置中设置 `DOOPS_AGENT_MACHINE_DIAGNOSTICS_ROOT=/var/lib/doops/diagnostics`。Agent 运行在容器时，向它只读挂载对应宿主的证据树；采样服务须运行在容器外。

```sh
python3 /opt/doops/machine_diagnostic_collector.py --binding /etc/doops/host.json --root /var/lib/doops/diagnostics
```

Gateway 运维人员通过受审计的迁移流程显式应用 `backend/docs/migrations/machine_diagnostics.sql`。已有绑定表可重跑同一脚本以建立隧道快照表和目标变更清理触发器。CPU 和内存保存在快照 JSON 中，无需迁移指标列。已有 Gateway 只读挂载模式继续使用 `DOOPS_MACHINE_DIAGNOSTICS_ROOT`、预置绑定和分钟证据树；配置该模式时，详情提供读取操作。

## 首采与第二次取样

1. 在机器详情输入独立资产记录中的指纹，点击「核验并绑定」。Gateway 经目标当前的反向连接向 Agent 读取固定位置最新的宿主快照，核对资产、目标、指纹和采集时间。成功后同一事务写入绑定、首条样本和成功审计。
2. 等待下一次宿主采样，点击「从宿主取样」。在「宿主 CPU 与内存」核对两条宿主采集时间、CPU 忙碌与总 ticks、已用与总内存 MiB，以及两条样本之间的整机 CPU 使用率。「重读记录」只读取当前时间窗口内已保存的快照。重复拉取同一条快照不会生成第二个时间点。
3. CPU 总 ticks 为 `/proc/stat` 的 user 至 steal 八项之和，忙碌 ticks 扣除 idle 与 iowait。区间使用率为忙碌增量除以总增量。内存已用为 `MemTotal − MemAvailable`。启动 ID 改变、采样间隔超过 5 分钟、CPU 计数回退或缺少字段时，页面说明原因并停止计算该区间。旧快照缺失的字段显示未知。

取样只接受 5 分钟内的宿主证据。Gateway 在取证前和落库时检查资产、授权、领取凭据和连接 generation。更新目标或删除资产会清除旧绑定和隧道样本。目标暂时离线时，仍有授权的用户可以按时间窗重读历史数据。成功与失败的操作可在 Gateway 审计中按 `machine-diagnostic` 会话查找。
