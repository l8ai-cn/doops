# Zheyin 0.2.46 cutover and gateway hostAlias cleanup

Target `gw-zy` is `doops-zheyin/node-239`, namespace `default`, node
`cluster-node0014.172.16.50.239`. Release `doops-agent-239`.

Result: success on the third attempt. `v0.3.12` commit
`e1d2ba7c24b4283efe7a1d2c0c1fa19bba960358`, do-agent `0.2.46`,
`rejectWhenBusy=true`. Final Pod `doops-agent-239-7569785699-ckx7q`; the
hostAlias cleanup rolled it to `doops-agent-239-db66f887b-b29zq` at revision 2.

## Why three attempts

`cutover.py` (r1) and `cutover-r2.py` both failed at candidate readiness with
`x509: certificate is valid for *.l8ai.cn, not doops.aiedulab.cn`. Helm atomic
timed out and uninstalled only the next release; the old release stayed at
revision 12 and the receipt recorded `old-preserved`.

The cause was not the certificate: the live Deployment pins the gateway address
with a Pod `hostAlias`. The new Deployment was rendered from Helm values and had
no such alias, so it resolved `doops.aiedulab.cn` through cluster DNS to the
retired `47.114.103.22`, which serves the `*.l8ai.cn` certificate.

`cutover-r3.py` copies the live Deployment's `hostAliases` into the candidate
values and asserts the gateway pin exists before installing. It succeeded:
candidate Ready, smoke marker returned by a real read-only `ask`, old release
retired, formal release installed, next release removed.

## cleanup-hostalias.py

Once the campus DNS record was corrected (see `../zy-dns-20261117`), the alias
became redundant and would keep shadowing any future gateway address change.
The Agent cannot rebuild its own Pod, so this runs in an independent Job that
upgrades the release without `hostAliases`, using `--atomic --wait` so a failed
cleanup restores the previous revision.

The Job reported failure even though the cleanup succeeded: it counted both the
new Pod and the terminating old one. The script now counts only ready Pods that
are not being deleted; `cleanup-hostalias-job-output.txt` is the original run.

## Files

- `cutover.py`, `cutover-job.yaml` — first attempt, kept as the record of the
  certificate failure
- `cutover-r2.py`, `cutover-job-r2.yaml` — second attempt, same failure
- `cutover-r3.py`, `cutover-job-r3.yaml` — the successful run
- `cleanup-hostalias.py`, `cleanup-hostalias-job.yaml`,
  `cleanup-hostalias-job-output.txt`
