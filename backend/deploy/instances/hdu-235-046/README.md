# hdu-235 0.2.46 side-by-side cutover

Target `gw-hdu-235` is `doops-235/hdu-235`, namespace `ai`, node `1e07451bc39b`.
Same independent-Job pattern as the edu-coder cutover.

Result: success. Release `doops-agent` revision 1, Pod `doops-agent-58669846d5-97rrt`
(uid `f4e8da60-7ed1-4adc-9468-aa3b7b64e0f8`), `v0.3.12` commit
`e1d2ba7c24b4283efe7a1d2c0c1fa19bba960358`, do-agent `0.2.46`.

`cutover-job.yaml` mounts the k3s admin kubeconfig at `/root/.kube/config`
(`/etc/rancher/k3s/k3s.yaml`); the in-cluster Service address is
`10.43.0.1:443`, so the controller still rewrites the server before Helm runs.

The `exec` that started the formal install returned
`execution_outcome_unknown: agent disconnected before a terminal reply`. That is
expected: installing the formal release replaces the process answering the call.
Reconnecting showed `formal-ready`; it was not a failed install.
