# Install And Management

DoSql is bundled inside the DoOps Agent runtime. Users install and authenticate
one product, `doops`; they do not install a second database gateway, target
registry or token store.

## User Installation

Install the versioned DoOps CLI and skill through the DoOps release installer:

```bash
bash backend/scripts/install.sh \
  --version <release-tag> \
  --project /absolute/path/to/business-project
```

Then log in once. Login belongs to the Gateway, not a target. The first
login discovers the visible targets and writes the local routes. Add a route
by hand only when that target cannot be discovered. The login command is
documented in `backend/docs/DOOPS_USAGE_GUIDE.md`.

```bash
doops login --gateway <gateway-url>
```

The discovered target routes and this Gateway login are the only user-facing
connection configuration. DoSql does not read `~/.config/dosql/connections.json`, a skill
local default target, or credentials passed through command-line arguments.

## Unified Entry Points

Use the same `doops` CLI for all skills:

```bash
doops -session db-read sql --target <target> --mode query \
  --database <logical-database> \
  --request '统计最近 24 小时的失败订单，只读执行'

doops -session db-plan sql --target <target> --mode plan \
  --request '为订单表增加 external_id 生成变更计划，不执行'

doops -session db-change sql --target <target> --mode apply \
  --database <logical-database> \
  --file migrations/0001_add_external_id.sql
```

All modes declare `skill=dosql`. Natural-language `query`, `plan` and `discover`
use Gateway `ask`; only `query` connects to the database. `apply` runs the
verbatim `--sql/--file` statements, using the user's existing authorization for
the target, database and change scope.

Gateway checks the user's grant on the exact target. Database requests use
`credential_mode=auto` to resolve its active database bindings; `--database`
selects the database, and direct `--sql/--file` requests may add
`--credential <resource-name-or-id>` to select a bound credential. This maps to
`sql_credential`, not `credential_refs`. Credential payloads reach only the
private materialization channel; the model receives metadata.

DoSql applies statement classification, journal and verification rules.
Dry-run requires rollback-capable statements. MySQL implicit-commit DDL
must run separately from DML without `--dry-run`; verification cannot undo
already committed DDL. Read back the resulting structure or data and compare it
with the task's acceptance criteria.

## Agent Runtime

The bundled executable skill lives at:

```text
backend/agent/skills/dosql/
```

Its entrypoint is `scripts/dosql-agent.mjs`. The Agent may call it for
deterministic database operations, but all user-facing admission still comes
through the authenticated DoOps Gateway operation. Direct execution against a
business database without a Gateway target and DoSql operation record is not a
supported path.
