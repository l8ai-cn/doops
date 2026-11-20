# A1–A5 应用周检证据

`application_evidence.py` 每次为一个资产产生一轮收据。由目标维护者提供本地策略，采集端写入证据目录，周检执行器以只读方式挂载同一目录。在执行器领取工单前采集，确保观测时间落在该工单的取证窗口内。

```sh
python3 application_evidence.py --config /etc/doops/orders-probes.json --root /var/lib/doops/evidence
```

生产器使用 POSIX 和 Python 3.10+ 标准库。证据根目录需已存在。策略文件及上级目录由受信维护者管理，文件所有者须为 root 或运行账号，组和其他用户均无写权限。以下配置中的地址与阈值为示例，部署时替换为资产负责人指定的端点和业务基线。

```json
{
  "cluster": "east", "instance": "orders-agent", "asset_id": "orders",
  "checks": {
    "A1": {"url": "https://orders.example.invalid/health", "status": 200},
    "A2": {"url": "https://metrics.example.invalid/orders/database-up"},
    "A3": {"url": "https://metrics.example.invalid/orders/request-rate", "min": 2, "max": 150},
    "A4": {"url": "https://orders.example.invalid/"},
    "A5": {"url": "https://orders.example.invalid/api/status", "status": 200, "field": ["service", "ready"], "equals": true}
  }
}
```

一次最多读取 32 KiB 策略，每个已配置检查最多发送一次固定 GET，单次总时限 5 秒。远端使用校验证书和主机名的 HTTPS，明文 HTTP 仅限回环地址。请求直连配置的精确路径，禁止 URL 嵌入凭据、查询参数及重定向。JSON 端点应返回 `application/json`，正文采用原始编码，最多 64 KiB。

| 检查 | 实际观测 | 发现条件 |
| --- | --- | --- |
| A1 服务健康 | 指定健康端点返回的 HTTP 状态 | 与配置的 2xx 状态不同 |
| A2 数据库状态 | 可信 exporter 的 `database_up`，单位 `1`，值为数值 0 或 1 | 值为 0 |
| A3 业务流量 | 可信 exporter 的 `requests_per_second`，单位 `req/s` | 单点值落在配置的闭区间之外 |
| A4 Web 安全 | HTTPS 证书校验、正数 HSTS `max-age`、`X-Content-Type-Options: nosniff` | 成功 HTTP 响应上有策略项未满足，或 TLS 对端证书校验失败。证书失败仅计一次已测发现 |
| A5 指定 API | 检查 HTTP 状态后，按对象键路径比较业务标量 | 状态不符、缺少必需字段、值或类型不符。布尔值与数值分别比较 |

A2、A3 的端点各返回一个具有资产身份和观测时间的 JSON 对象。`observed_at` 使用带时区的 RFC3339 时间，须位于采集时刻之前的 300 秒内。收据保留 exporter 的观测时间。单个对象的示例如下。

```json
{"cluster":"east","instance":"orders-agent","asset_id":"orders","metric":"requests_per_second","unit":"req/s","value":12.5,"observed_at":"2026-11-19T02:15:00Z"}
```

测量收据包含工单的四项标识、`observed_at`、`source`、`policy`、`samples`、`violations` 和脱敏 `detail`。401、403 或 407 记录 `permission_denied`，断连记录 `source_unreachable`，超时记录 `probe_timeout`。重定向、限流、过期标量、资产身份不符或不可解析的响应也产生证据缺口。缺口收据带 `gap`，省略测量计数。A1、A5 的指定端点返回 503 表明已观测到服务状态异常，A2、A3 的 exporter 返回 503 表明缺少资产观测。

每份收据通过同目录临时文件、`fsync` 和 `os.replace` 发布到 `<root>/<cluster>/<instance>/<asset_id>/A*.json`，权限为 `0640`。缺口覆盖该项上一份收据。运行账号与执行器需对目录和文件具备对应的写入、读取权限。配置仅列出本生产器负责的检查。移交或移除检查时需同步处理其旧收据。

命令输出发布数、发现数和缺口数。退出码 `0` 表示已取得所有配置项的测量，检查结果见 `violations`。`2` 表示采集轮次已有缺口，`1` 表示配置文件或发布失败。收据不含响应正文、业务字段原值或 Cookie。

```sh
python3 -B backend/inspection/application_evidence_test.py -v
```

测试仅使用回环 HTTP/HTTPS 服务。测试程序用本机 OpenSSL 生成临时 TLS 证书，并通过 `runner.py` 校验收据读取与缺口提交。
