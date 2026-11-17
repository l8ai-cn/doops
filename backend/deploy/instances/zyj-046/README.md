# zyj 0.2.46 side-by-side cutover

Target `gw-zyj` is `doops-zyj/zyj`, namespace `ai`, node `vm-0-11`.
Same independent-Job pattern as the edu-coder cutover.

Result: success. Release `doops-agent` revision 1, Pod `doops-agent-64ccf66559-llsnb`
(uid `a703c0a4-1e4d-453d-9870-e7db31d307b2`), `v0.3.12` commit
`e1d2ba7c24b4283efe7a1d2c0c1fa19bba960358`, do-agent `0.2.46`.

This target shares the cluster with the `agents-test` namespace, which holds the
`gw-test` instance plus backend, desktop-web, file, gitea, postgres, redis, relay
and web. `protected_test_releases()` records every Deployment UID in
`agents-test` at preflight and asserts they are unchanged before the old release
is retired and again before the next release is removed. The guard covers all
Deployments, not only the Agent, so a mis-scoped uninstall cannot pass.

`hostAccess.enabled` is true and the home directory moves from
`/data/doops-agent-home` to `/data/doops-agent-home-next` for the candidate.
