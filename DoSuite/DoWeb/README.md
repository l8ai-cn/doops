# DoWeb

DoWeb is the frontend deployment and Nginx operations product.

The first implementation focuses on deployment only. The customer builds the
frontend on their own computer or in CI, then DoWeb receives the customer-built
dist artifact and publishes it into an Nginx-backed Web container. DoWeb does
not run `npm install`, `npm run build`, or application process management in the
MVP.

Target scope:

- connect to an Nginx or Nginx-managed gateway target;
- publish a new static frontend website from `dist/`;
- configure server blocks, custom domains, reverse proxy routes, load balancing,
  TLS certificate paths and static artifact deployment;
- support managed Nginx edits through `config-set` and `snippet-set`;
- validate every change with `nginx -t`;
- inspect site availability, certificate status and access/error logs;
- rollback site configuration and deployments through doagent.

Current status: Skill-side MVP script and knowledge base.

Resource-control design:

- `docs/superpowers/specs/2026-07-05-doweb-agent-nginx-resource-control-design.md`

```text
DoWeb/
├─ Server/   Future UI/API/runtime for website operations.
└─ Skill/    Nginx/site publishing skill, scripts, references and runbooks.
```

## MVP Command

```bash
export DOWEB_SSH_PASSWORD='...'
node DoSuite/DoWeb/Skill/scripts/doweb.mjs install-nginx \
  --ssh-host 114.55.61.32 \
  --ssh-user root \
  --password-env DOWEB_SSH_PASSWORD

node DoSuite/DoWeb/Skill/scripts/doweb.mjs domain-set \
  --domain aihub.org.cn \
  --host-ip 114.55.61.32 \
  --provider aliyun

node DoSuite/DoWeb/Skill/scripts/doweb.mjs deploy \
  --site-id demo \
  --domain-base aihub.org.cn \
  --dist /path/to/dist \
  --proxy /api/=https://api.example.com
```

DoWeb converts natural-language publishing requests into a SiteSpec, renders a
candidate Nginx config, runs `nginx -t`, reloads Nginx, records state, and
returns evidence. There is no silent fallback: failed validation, reload or
health check stops the change and surfaces the root cause.

`install-nginx` supports username/password bootstrap without storing the
password in the repository. The default mode is Docker: DoWeb installs or starts
Docker when needed, runs the `doweb-nginx` container, and returns the deploy
flags needed for later `nginx -t` and reload calls.

`domain-set` maintains base-domain metadata. After `aihub.org.cn` is registered,
`deploy --domain-base aihub.org.cn --site-id demo` automatically publishes
`demo.aihub.org.cn` and rejects conflicts before changing Nginx.
