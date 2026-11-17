# Zheyin campus DNS: stop shadowing aiedulab.cn

The cluster resolvers `172.16.55.3` and `172.16.55.4` are the `zheyin-bind`
DaemonSet on `docker11` and `docker12` (namespace `dns-system`). It ran BIND with
`aiedulab.cn` as a local `type master` zone.

That zone was a stale copy of a domain the campus does not own: public
authoritative DNS (`dns31`/`dns32.hichina.com`) had moved on, but the local copy
froze at SOA serial `2026071503` and ended with `* A 47.114.103.22`. Because a
local master zone answers authoritatively for the whole name, every
`aiedulab.cn` lookup inside the campus was pinned to that retired address, and
`doops.aiedulab.cn` resolved to a host presenting a `*.l8ai.cn` certificate.

Zone transfer is not an option: `AXFR` against the public authoritative servers
returns `rcode5` (REFUSED), so a complete, current copy cannot be obtained at
all. The only sound fix is to stop holding a copy.

## Change

`zheyin-bind-config.before.yaml` and `zheyin-bind-config.after.yaml` are the
ConfigMap before and after. The change removes `zone "aiedulab.cn"` from
`named.conf`, leaves the zone file as a comment stub, and keeps
`zjcm.edu.cn` authoritative because that domain is genuinely campus-internal.

Every `aiedulab.cn` name now resolves through the forwarders, so public DNS
changes propagate without any sync job. `zjcm.edu.cn` is untouched: those records
point at campus systems and the campus WAF at `172.16.99.247`.

`zheyin-bind-ds.after.yaml` is the DaemonSet with the two dead mounts removed:
`db.zhengfang.l8ai.cn.default` (no such ConfigMap key, it mounted as an empty
directory and nothing referenced it) and `db.aiedulab.cn.default`.

## Notes

Files are mounted with `subPath`, which does not refresh on ConfigMap change, so
the rollout deletes Pods. `maxUnavailable: 1` keeps one resolver serving
throughout; do one node at a time and verify before the second.

Verify from inside the cluster:

```sh
~/.local/bin/doops -session <s> exec --target gw-zy --cmd \
  'python3 -c "import socket;print(sorted({i[4][0] for i in socket.getaddrinfo(\"doops.aiedulab.cn\",443)}))"'
```

Expected the current gateway address, read from
`kubectl -n default get deploy doops-agent-239 -o jsonpath='{.spec.template.spec.hostAliases}'`
before the cleanup removed it.

`zheyin-bind-config.before.yaml` was regenerated from the recorded ConfigMap
contents after a script truncated the working copy; the `zjcm` zone in it is the
live one, unchanged by this work.

## Redaction

Production addresses are not committed (see `.gitleaks.toml`). In
`zheyin-bind-config.before.yaml` the `oilan` and `git` records carry
`203.0.113.11` instead of the real address; the unredacted copy of this file and
of the cleanup job output is kept locally under `.local/`, which is not tracked.
Read the live values from the ConfigMap before using either as a rollback source.
