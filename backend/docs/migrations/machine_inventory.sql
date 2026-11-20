-- Apply to the Gateway SQLite database through the operator's migration process.
BEGIN IMMEDIATE;
CREATE TABLE IF NOT EXISTS inventory_machines (
    asset_id TEXT PRIMARY KEY NOT NULL,
    name TEXT NOT NULL CHECK (length(name) > 0),
    kind TEXT NOT NULL CHECK (kind IN ('physical', 'vm')),
    address TEXT NOT NULL DEFAULT '',
    cpu_count INTEGER CHECK (cpu_count > 0),
    memory_gib REAL CHECK (memory_gib > 0),
    disk_gib REAL CHECK (disk_gib > 0),
    host_asset_id TEXT REFERENCES inventory_machines(asset_id),
    cluster TEXT NOT NULL DEFAULT '',
    instance TEXT NOT NULL DEFAULT '',
    revision INTEGER NOT NULL DEFAULT 1 CHECK (revision > 0),
    CHECK (host_asset_id IS NULL OR (kind = 'vm' AND host_asset_id <> asset_id)),
    CHECK ((cluster = '') = (instance = ''))
) STRICT;
CREATE INDEX IF NOT EXISTS inventory_machines_target ON inventory_machines(cluster, instance);
CREATE INDEX IF NOT EXISTS inventory_machines_host ON inventory_machines(host_asset_id) WHERE host_asset_id IS NOT NULL;

-- Keep each confirmation's line results without retaining uploaded CSV or machine descriptions.
CREATE TABLE IF NOT EXISTS inventory_import_attempts (
    attempt_id INTEGER PRIMARY KEY,
    batch_id TEXT NOT NULL,
    receipt TEXT NOT NULL
) STRICT;
CREATE INDEX IF NOT EXISTS inventory_import_attempts_batch ON inventory_import_attempts(batch_id, attempt_id);
-- Reapply this migration to an existing inventory before importing logins.
-- The first successful login import pins the key so a misconfigured gateway
-- cannot create accounts encrypted under another key in the same vault.
CREATE TABLE IF NOT EXISTS inventory_machine_login_key (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    key_mac TEXT NOT NULL
) STRICT;
CREATE TABLE IF NOT EXISTS inventory_machine_login_accounts (
    asset_id TEXT NOT NULL REFERENCES inventory_machines(asset_id),
    username TEXT NOT NULL,
    credential_id TEXT NOT NULL UNIQUE REFERENCES credentials(id),
    version_id TEXT NOT NULL UNIQUE REFERENCES credential_versions(id),
    PRIMARY KEY (asset_id, username)
) STRICT;
COMMIT;
