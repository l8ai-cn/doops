-- SQLite Gateway database. Apply explicitly through the operator's audited DoOps
-- migration session before enabling /v1/assets/clusters. Safe to reapply.
BEGIN IMMEDIATE;
CREATE TABLE IF NOT EXISTS cluster_inventory_sources (
    cluster TEXT NOT NULL,
    instance TEXT NOT NULL,
    asset_id TEXT NOT NULL,
    name TEXT NOT NULL,
    kube_system_uid TEXT NOT NULL DEFAULT '',
    verified_at TEXT NOT NULL,
    verified_by TEXT NOT NULL,
    scope TEXT NOT NULL CHECK (scope IN ('list_namespaces', 'list_pods')),
    PRIMARY KEY (cluster, instance)
);
CREATE INDEX IF NOT EXISTS cluster_inventory_asset_id ON cluster_inventory_sources(asset_id);
CREATE TABLE IF NOT EXISTS cluster_inventory_namespaces (
    cluster TEXT NOT NULL,
    instance TEXT NOT NULL,
    name TEXT NOT NULL,
    uid TEXT NOT NULL,
    namespace_observed_at TEXT NOT NULL,
    pods_listed_at TEXT NOT NULL,
    verified_at TEXT NOT NULL,
    verified_by TEXT NOT NULL,
    scope TEXT NOT NULL CHECK (scope IN ('list_namespaces', 'list_pods')),
    PRIMARY KEY (cluster, instance, name),
    FOREIGN KEY (cluster, instance) REFERENCES cluster_inventory_sources(cluster, instance)
);
COMMIT;
