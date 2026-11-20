# 从宿主机采样到机器详情

一台机器有三个不同的标识。`asset_id` 表示台账中的机器，`cluster/instance` 表示 Gateway 可授权的 Agent 目标，`host_sha256` 表示需要核验的宿主身份。采样服务运行在实际 Linux 宿主上。Agent 通过只读目录获取该机 JSON，沿主动建立的连接向 Gateway 返回；浏览器使用当前用户的 Gateway 会话读写。

1. 在「资产 → 机器」登记机器，绑定已领取、在线的 Agent 目标。通过目标宿主控制台或现有受信运维渠道核对 `/etc/machine-id`。指纹是十六进制 machine-id 去除末尾换行后计算的 SHA-256。保留完整 machine-id 在主机上。主机克隆时需按系统规范分配独立的 machine-id。
2. 在宿主为采样服务准备 `doops-sampler` 本地账号和同名组，使其能只读访问系统的 `/proc` 和 `/etc/os-release`。部署 Python 3.10+、`systemd-detect-virt` 和[宿主采样脚本](../scripts/machine_diagnostic_collector.py)。把核验过的资产编号、目标、指纹写入 `/etc/doops/host.json`，由管理员维护，采样账号可读。应用映射按已核验的服务 cgroup 填写。
3. 可用 [systemd 服务](../deploy/systemd/doops-host-sample.service) 和 [定时器](../deploy/systemd/doops-host-sample.timer) 每分钟运行采样。服务把脚本读到的数据原子写入 `/var/lib/doops-host-samples/<cluster>/<instance>/<asset_id>/`。用 `systemctl list-timers doops-host-sample.timer` 查看时钟，`journalctl -u doops-host-sample.service` 查看取样成功与失败。采样依赖查看真实宿主进程及命名空间；宿主的 `/proc` 隐藏策略或安全沙箱会影响可见范围。
4. 通过目标 Agent 的部署配置将该主机证据目录只读挂载，并将 **Agent 侧**的 `DOOPS_AGENT_MACHINE_DIAGNOSTICS_ROOT` 指向挂载根。例如挂载 `/var/lib/doops-host-samples:/var/lib/doops-host-samples:ro`。Agent 的服务身份须能读取文件，检查所在容器的 UID 映射和主机文件权限。采用反向通道接入时，Gateway 使用 SQLite 保存已经校验的样本。原有 Gateway 侧 `DOOPS_MACHINE_DIAGNOSTICS_ROOT` 用于只读查询已挂载证据的部署。
5. 平台管理员从机器详情输入从独立渠道取得的指纹，执行首次核验。核验后，具备该目标授权的用户可刷新并查看采样时间、系统、内核和进程。目标断线或没有新样本时，检查页面显示的时间与原因。变更机器来源后重新核验绑定。

配置示例如下。请将目标和指纹换成实际核验的值。

```json
{
  "asset_id": "srv-01",
  "cluster": "edge",
  "instance": "node-01",
  "host_sha256": "<已核对的64位小写十六进制指纹>",
  "applications": {}
}
```

机器详情的 JSON 是带有来源和采样时间的主机快照。持续监控使用时序样本，网卡、挂载点和 GPU 的采集与写入见[设备指标说明](ASSET_MONITOR_DEVICES.md)。
