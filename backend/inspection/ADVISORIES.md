# 软件公告和部署态清单

V1 检查漏洞公告，V2 检查补丁公告，V3 使用已登记修复的历史发现和新的部署清单复检。执行器需要两份独立输入：由公告维护方签发的区间，以及在目标上实际观测的包版本。两份输入的 SHA-256 都随收据提交。

## 签发和同步

维护方审核适用的二进制包、发行版、架构和修复版本，为同一 `(component, advisory)` 提供一个区间。以下是格式示例，时间和标识仅用于说明。

```json
{
  "issued_at": "2026-11-20T00:00:00Z",
  "valid_until": "2026-11-21T00:00:00Z",
  "advisories": [{
    "kind": "vulnerability",
    "component": "pkg:deb/debian/example?arch=amd64&distro=debian-12",
    "advisory": "EXAMPLE-1",
    "introduced": "0",
    "fixed": "1:2.3-1+deb12u2"
  }]
}
```

用维护方的私钥对最终 JSON 原始字节签名，把文件和同名 `.sig` 放到获批 HTTPS 镜像。签名与 HTTPS 证书各自独立。签名后修改换行或重新序列化都会导致验签失败。

```sh
openssl dgst -sha256 -sign review-private.pem -out advisories.json.sig advisories.json
```

在执行机上把签名公钥和配置交由管理员只写，在 `/opt/doops/inspection/` 同时安装 `runner.py` 和 `advisory_sync.py`。同步账号为 `doops-advisories`，服务进程的主组为 `doops-inspection`。`/etc/doops/inspection/advisories.env` 示例如下。

```ini
INSPECTION_ADVISORY_URL=https://mirror.example.org/review/advisories.json
INSPECTION_ADVISORY_PUBLIC_KEY=/etc/doops/inspection/review-public.pem
```

私有 CA 可用 `INSPECTION_ADVISORY_CA` 指向 PEM 文件。同步只读取配置的 URL 及其 `.sig`，检查 TLS 证书、主机名、HTTP 200 和 SHA-256 分离签名，拒绝重定向。JSON 上限为 8 MiB，签名上限为 16 KiB。签发时间须满足 `issued_at <= 当前时间 < valid_until`。安装时在同一文件系统加锁，以现存文件作为回退比较基准。较早的签发时间、同一签发时间的不同内容及无效更新均保留原文件。相同签发时间和字节可安全重试。新文件原子替换后同步目录元数据。

`doops-advisories.timer` 每小时触发同步。`StateDirectory` 为 `/var/lib/doops-advisories`，目录 `0750`，公告 `0640`。同步账号拥有写权，`doops-inspection` 组有读权。runner 默认直接读取 `/var/lib/doops-advisories/advisories.json`，每次使用时重查有效期。更新失败且旧文件仍有效时可继续使用旧公告，过期或缺失产生证据缺口。防回退历史保存在已发布文件中，状态目录须持久化；删除或整体还原该目录会丢失之后的签发记录。

自定义路径时，在同步命令上使用 `--root <公告目录>`（或在同步服务自己的 `advisories.env` 中设置 `INSPECTION_EVIDENCE_ROOT`），在 `runner.env` 中设置 `INSPECTION_ADVISORY_FILE=<公告目录>/advisories.json` 的绝对路径。runner 的 `INSPECTION_EVIDENCE_ROOT` 始终指向资产证据目录。两个配置文件独立，runner 只读挂载公告和资产证据。首次安装旧公告目录时，先挂载持久状态并成功执行一次签名同步，再启用 runner。公钥轮换由维护方通过可信渠道交付。

## 本机 dpkg 清单

在被观测的 Debian 或 Ubuntu 环境中运行以下命令，使用现有的证据根目录和 Gateway 工单里的目标标识。Python 版本为 3.10 或更新。容器内运行得到该容器的安装清单。

```sh
python3 runner.py dpkg-sbom --root /var/lib/doops-evidence \
  --cluster lab --instance debian12 --asset-id node01
```

命令读取本机 `os-release` 和 `dpkg-query`，选取状态为 `installed` 的二进制包。输出 `<根>/<cluster>/<instance>/<asset_id>/sbom.json`，格式为 CycloneDX 1.5 JSON，包含观测时间、目标标识及每个包的名称、版本、架构和发行版 PURL。命令成功后打印包数、文件路径和摘要。根目录应由可信采集端维护，可用 setgid 的 `doops-inspection` 组目录向 runner 授予只读访问；新建子目录 `0750`、清单文件 `0640`。在本轮取证窗口开启后重新采集，修复后再采集一次。采集命令只读取本地包数据库，容器外的宿主包、应用自行下载的依赖和文件内容不在清单内。

## 比较范围与验收

公告的 `component` 为清单 PURL 移除 `@版本` 后的精确字符串，包括原顺序的限定符。只提供源包名或缺少发行版、架构的公告需要维护方先映射为对应的二进制包 PURL。V1/V2 命中区间为 `[introduced, fixed)`。`pkg:deb/` 使用 Debian 的 epoch、upstream 与 revision 顺序，支持 `~`、backport、Ubuntu 与 Debian 安全更新后缀，每个输入最多 128 字节，数字段须能用 Go `int` 表示。其他包只支持无前导零的三段数字版本，每段最多九位。无法比较、无适用公告、缺少原发现或复检时修复边界变化均记录为证据不足。复检须匹配原组件、公告和区间，并达到原 `fixed`。没有独立的发行商公告订阅、依赖解析或软件升级步骤。

本机可用 `python3 -B -m unittest discover -s backend/inspection -p '*_test.py'` 验证同步及 runner。签名用例使用临时密钥和回环 TLS 镜像。两个 Go 模块分别执行 `go build -mod=readonly ./...`，在 Agent 模块执行 `go test -mod=readonly ./internal/server -run '^TestInspectionDebian'` 验证区间与修复判定。
