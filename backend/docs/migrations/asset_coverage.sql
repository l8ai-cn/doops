-- Apply once through the operator migration process, after machine_inventory.sql.
-- Agent connectivity does not determine whether a registered machine is in service.
BEGIN IMMEDIATE;
ALTER TABLE inventory_machines ADD COLUMN active INTEGER NOT NULL DEFAULT 1 CHECK (active IN (0,1));
CREATE TABLE asset_coverage_baselines (
    id TEXT PRIMARY KEY NOT NULL,
    frozen_at TEXT NOT NULL,
    frozen_by TEXT NOT NULL,
    snapshot TEXT NOT NULL CHECK (json_valid(snapshot))
) STRICT;
COMMIT;
