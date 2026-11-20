-- SQLite Gateway database. Apply explicitly in an audited DoOps migration session.
BEGIN IMMEDIATE;
CREATE TABLE IF NOT EXISTS inventory_business_systems (
    asset_id TEXT PRIMARY KEY NOT NULL,
    name TEXT NOT NULL,
    owner TEXT NOT NULL,
    active INTEGER NOT NULL CHECK (active IN (0,1)),
    revision INTEGER NOT NULL CHECK (revision > 0)
);
-- Each row is a manually verified source-specific relationship. An empty
-- database records a deployment without a database dependency.
CREATE TABLE IF NOT EXISTS inventory_business_deployments (
    asset_id TEXT NOT NULL REFERENCES inventory_business_systems(asset_id),
    cluster TEXT NOT NULL,
    instance TEXT NOT NULL,
    namespace TEXT NOT NULL,
    application TEXT NOT NULL,
    database_name TEXT NOT NULL DEFAULT '',
    verified_at TEXT NOT NULL,
    verified_by TEXT NOT NULL,
    PRIMARY KEY (asset_id, cluster, instance, namespace, application, database_name)
);
CREATE INDEX IF NOT EXISTS inventory_business_deployment_target
    ON inventory_business_deployments(cluster, instance, asset_id);
COMMIT;
