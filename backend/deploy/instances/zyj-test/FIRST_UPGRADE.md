# ZYJ Test Gateway Recovery And First Upgrade

Completed on 2026-11-15 at Helm revision 21. The candidate's Gateway-routed
Pod, exact digest and commit were verified, and the Job reached Complete.
Both attempts' temporary Jobs, code ConfigMaps and controller identities
were removed; backup Secrets and receipt ConfigMaps remain. This is an
executed one-time procedure and must not be replayed against revision 21.

This is only for `gw-test` -> `doops-zyj/zyj-test`,
`agents-test/doops-agent-test`, deployed revision **20**. Submit and observe
through the independent `gw-zyj` management target in the same cluster.
It is not a general
upgrade command. The old Agent cannot use `upgradeAudit v1`; it only submits
an independent Job through the audited Gateway `exec` channel.

Confirmed on 2026-11-15: one Ready Agent Pod on `vm-0-11`, ServiceAccount
`doops-agent-test`, pull Secret reference `cnb-regcred`, old release commit
`76abae08a36d920c0058486d2d8ab86880c4c121`, DoAgent `0.2.42`, image digest
`sha256:778312b10cf925c74bd943d54b6f9509948d0a33ad414cebf87622edee76b8af`.
The old repository is read from the live Deployment and matched against
revision 20. Its manifest must still be retrievable before any upgrade.

The revision-18 attempt upgraded to 19 but did not receive Gateway acceptance,
then rolled back to revision 20. The original live Deployment used
`https://doops.aiedulab.cn`; Helm values retained `https://doops.l8ai.cn`.
The immutable `zyj-test-first-upgrade-r18-backup` proves that configuration
drift. This attempt validates that snapshot's UID, image and Gateway identity,
and writes the original Gateway URL into the new Helm values. Keep the first
attempt's failed Job, receipt and backup as evidence until recovery completes.

The candidate must be the verified **linux/amd64 platform image digest** for
DoOps **v0.3.3** and its full source commit after the release merge. Both are
required parameters; neither is inferred from the CLI or a mutable tag.
The candidate must embed DoAgent `0.2.45`, source
`64f075e37ccf06122ee4f88646b0ebd1548351da`, digest
`sha256:14e753702568bc2ed8bcc213c7a23a2c600788b3a0b989d84676c7a172ccef06`.
The controller checks these image labels and the actual binaries. This
DoAgent digest is **not** the DoOps image digest.

## Reuse And Recovery

The Job mounts `first-upgrade.py` through an immutable, code-only ConfigMap.
It uses the candidate image's `/app/deploy/helm/doops-agent`,
`configure_kubeconfig.py` and `adopt_agent.py` (`verify`,
`controller_runtime_digest`, `write_verified_receipt`). The image does not
need to contain this instance script. The original revision's values
are retained, changing the image and reconciling the proven Gateway URL drift.
The checked-in `values.yaml` now records the verified image and Gateway
identity; it was not an input to this upgrade.

Before mutation it stores history, user/all values, manifest, Deployment and
verified versions in immutable Secret `zyj-test-first-upgrade-r20-backup`.
No Secret payload, environment payload, Helm output or traceback is logged or
stored in a ConfigMap. The receipt ConfigMap contains only explicit metadata.
The dedicated Job SA/CRB are outside the Helm release and survive rollback.
They temporarily have the same cluster-admin authority as this Agent.

Revision, image, chart resources, target, selector and Deployment drift fail
closed. A legacy mount needing migration also stops before upgrade; this Job
does not perform mount migrations. The server-side Helm dry run
uses the same values as the actual upgrade. No concurrent Helm/RBAC/Agent
changes are permitted during this one-time operation.

After mutation, a failure, interruption or missing Gateway acceptance triggers
`helm rollback ... 20`, then restores the original Deployment spec from the
revision-18 backup with a resource-version-guarded patch over stdin. This
restores the actual prior Gateway route rather than the stale Helm value.
It then verifies old Pod Ready/imageID, commit, DoAgent version and
Gateway-connected readiness verification. It exits nonzero even after a
successful rollback. Its terminal receipt identifies the restored Pod, image
and source commit and clears the candidate's Gateway acceptance. A changed concurrent revision blocks destructive
rollback. Hard node loss/SIGKILL or loss of Kubernetes access cannot guarantee
recovery; the retained backup and an independently authorized management
target are then required. Do not replay an unknown result.

## Execute After Review

Run from this directory. Set the two non-secret parameters from the verified
v0.3.3 build receipt, then render locally:

```bash
IMAGE='<verified-doops-repository>@sha256:<platform-digest>'
SOURCE_COMMIT='<full-40-hex-v0.3.3-source-commit>'
SESSION=helm-runtime-inspect-20261115
python3 first-upgrade.py render --image "$IMAGE" --source-commit "$SOURCE_COMMIT" \
  > /tmp/zyj-test-first-upgrade.json

~/.local/bin/doops -session "$SESSION" write --target gw-zyj --storage gateway \
  --path zyj-test-first-upgrade.json --file /tmp/zyj-test-first-upgrade.json
~/.local/bin/doops -session "$SESSION" exec --target gw-zyj \
  --cmd "kubectl create -f /root/ws/$SESSION/zyj-test-first-upgrade.json"
```

Use `create`, not `apply`. Existing objects mean inspect and reconcile the
earlier attempt, never overwrite/recreate the Job. The old Agent does not run
Helm upgrade itself. Observe the Job and the metadata-only receipt:

```bash
~/.local/bin/doops -session "$SESSION" exec --target gw-zyj \
  --cmd 'kubectl -n agents-test get job zyj-test-first-upgrade-r20; kubectl -n agents-test get configmap zyj-test-first-upgrade-r20-receipt -o json'
```

When phase is `awaiting-gateway`, complete these steps within **300 seconds**.
The last command runs in the Pod that Gateway actually routes to, checks its
UID/imageID/commit/versions, then acknowledges that exact Pod to the Job:

```bash
~/.local/bin/doops -session "$SESSION" targets --target gw-test
~/.local/bin/doops -session "$SESSION" capabilities --target gw-test --require structuredSSH
~/.local/bin/doops -session "$SESSION" write --target gw-test --storage gateway \
  --path zyj-test-first-upgrade.py --file first-upgrade.py
~/.local/bin/doops -session "$SESSION" exec --target gw-test \
  --cmd "python3 /root/ws/$SESSION/zyj-test-first-upgrade.py accept --image '$IMAGE' --source-commit '$SOURCE_COMMIT'"
```

Only acknowledge after `targets` shows the expected full source commit and
the protocol check succeeds. The controller rechecks the same Pod after
15 seconds before recording `complete`, revision 21 and standard upgrade
evidence. Require Job `Complete=True`, receipt `phase=complete`, target commit
and actual functional smoke before declaring delivery. No automatic acceptance
is sent by the controller. Missing acceptance rolls back.

After either terminal outcome is verified, delete only this Job, its code
ConfigMap and temporary SA/CRB through Gateway `exec`. Retain the backup Secret,
receipt and standard `doops-upgrade-verified-doops-agent-test` evidence.

```bash
~/.local/bin/doops -session "$SESSION" exec --target gw-zyj \
  --cmd 'kubectl -n agents-test delete job/zyj-test-first-upgrade-r20 configmap/zyj-test-first-upgrade-r20-code serviceaccount/zyj-test-first-upgrade-r20'
~/.local/bin/doops -session "$SESSION" exec --target gw-zyj \
  --cmd 'kubectl delete clusterrolebinding agents-test-zyj-test-first-upgrade-r20'
```

Separately reconcile this instance's `values.yaml` image/release comment and
verified configuration through the normal GitOps review; never copy secret
values from the backup into Git. Do not use this revision-20 Job for subsequent
upgrades.

## Local Checks

`PYTHONDONTWRITEBYTECODE=1 pytest -q -p no:cacheprovider test_first_upgrade.py`
checks the rendered objects and the transaction with synthetic command
boundaries: accepted candidate, missing Gateway acknowledgement, unknown Helm
result, registry/dry-run failure, revision drift, rollback failure and a
Gateway request routed to the wrong Pod. It does not deploy or prove registry
access. The actual candidate digest/commit and live preflight remain mandatory.
