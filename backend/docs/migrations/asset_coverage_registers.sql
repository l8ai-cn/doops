-- Apply after asset_coverage.sql. Existing platform snapshots retain their original format.
BEGIN IMMEDIATE;
CREATE TABLE IF NOT EXISTS asset_coverage_registers (
    scope TEXT PRIMARY KEY NOT NULL,
    revision INTEGER NOT NULL CHECK(revision > 0),
    document_ref TEXT NOT NULL,
    entries TEXT NOT NULL CHECK(json_valid(entries)),
    updated_by TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;
CREATE TABLE IF NOT EXISTS asset_coverage_cluster_versions (
    cluster_id TEXT NOT NULL,
    id TEXT NOT NULL,
    frozen_at TEXT NOT NULL,
    frozen_by TEXT NOT NULL,
    snapshot TEXT NOT NULL CHECK(json_valid(snapshot)),
    document_ref TEXT NOT NULL,
    register_revision INTEGER NOT NULL,
    PRIMARY KEY(cluster_id,id)
) STRICT;
COMMIT;
