# edu-coder 0.2.46 side-by-side cutover

Target `gw-edu-coder` is `doops-edu/edu-coder`, namespace `kz-ops`, node
`192.168.0.24`. The Agent could not run `doops upgrade` itself (`upgrade_audit_unsupported`),
so the replacement ran in an independent Job started through `doops exec`.

Result: success. Release `doops-agent` revision 1, Pod `doops-agent-5bd4c9688-zhl2g`
(uid `dfaf1030-0546-43dd-b441-c105ae7c0dce`), `v0.3.12` commit
`e1d2ba7c24b4283efe7a1d2c0c1fa19bba960358`, do-agent `0.2.46`.

`cutover.py` backs up the live Helm values, manifest and Deployment, installs
`doops-agent-next` with a separate home, and only removes the old release after
the new Pod is Ready on the pinned digest, the Gateway reports the expected
commit, and a read-only `ask` returns the function marker. Failure before old
release removal uninstalls only the next release.

The first attempt (`doops-edu-046-cutover`, kept as evidence in the cluster)
failed because `KUBECONFIG=/root/.kube/config` points at
`apiserver.cluster.local`, which is not resolvable from inside a Pod. The final
Job rewrites it with `/app/configure_kubeconfig.py` before calling Helm.

Expected during the change: while both Agents run, the old Pod reads `0/1 Ready`
because the single-process probe sees both Agents when host PID is shared. The
new Pod stays Ready. Do not read that as a failed old release.
