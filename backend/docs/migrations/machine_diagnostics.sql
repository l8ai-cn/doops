-- Explicit Gateway SQLite migration. Reapply to add tunnel evidence to an existing binding store.
-- The fingerprint is the
-- SHA-256 of the lowercase 32-character /etc/machine-id, without its newline.
BEGIN IMMEDIATE;
CREATE TABLE IF NOT EXISTS machine_diagnostic_bindings (
    asset_id TEXT PRIMARY KEY NOT NULL REFERENCES inventory_machines(asset_id),
    cluster TEXT NOT NULL,
    instance TEXT NOT NULL,
    host_sha256 TEXT NOT NULL UNIQUE CHECK(length(host_sha256)=64 AND host_sha256 NOT GLOB '*[^0-9a-f]*')
);
CREATE TABLE IF NOT EXISTS machine_diagnostic_samples (
    asset_id TEXT NOT NULL REFERENCES machine_diagnostic_bindings(asset_id),
    cluster TEXT NOT NULL,
    instance TEXT NOT NULL,
    host_sha256 TEXT NOT NULL,
    at_ms INTEGER NOT NULL,
    generation INTEGER NOT NULL,
    payload TEXT NOT NULL,
    PRIMARY KEY (asset_id, at_ms)
) STRICT;
-- A later rebind, even back to the same target, requires fresh verification.
CREATE TRIGGER IF NOT EXISTS machine_diagnostic_target_changed AFTER UPDATE OF cluster,instance ON inventory_machines
WHEN (OLD.cluster, OLD.instance) <> (NEW.cluster, NEW.instance)
BEGIN
    DELETE FROM machine_diagnostic_samples WHERE asset_id=OLD.asset_id;
    DELETE FROM machine_diagnostic_bindings WHERE asset_id=OLD.asset_id;
END;
CREATE TRIGGER IF NOT EXISTS machine_diagnostic_asset_deleted BEFORE DELETE ON inventory_machines
BEGIN
    DELETE FROM machine_diagnostic_samples WHERE asset_id=OLD.asset_id;
    DELETE FROM machine_diagnostic_bindings WHERE asset_id=OLD.asset_id;
END;
COMMIT;
