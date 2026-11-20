-- Apply explicitly to Gateway SQLite after cluster_inventory.sql and business_inventory.sql.
-- Existing static topology records need no conversion. Runtime GET/POST requires this migration.
BEGIN IMMEDIATE;
CREATE TABLE IF NOT EXISTS asset_runtime_snapshots (
 cluster TEXT NOT NULL, instance TEXT NOT NULL, namespace TEXT NOT NULL, asset_id TEXT NOT NULL,
 revision TEXT NOT NULL, observed_at TEXT NOT NULL, objects TEXT NOT NULL CHECK(json_valid(objects)),
 PRIMARY KEY(cluster,instance,namespace)
) STRICT;
CREATE TABLE IF NOT EXISTS asset_runtime_bindings (
 cluster TEXT NOT NULL, instance TEXT NOT NULL, namespace TEXT NOT NULL, application TEXT NOT NULL,
 workload_uid TEXT NOT NULL, verified_at TEXT NOT NULL,
 PRIMARY KEY(cluster,instance,namespace,application,workload_uid)
) STRICT;
CREATE TRIGGER IF NOT EXISTS asset_runtime_ns_drop AFTER DELETE ON cluster_inventory_namespaces BEGIN
 DELETE FROM asset_runtime_snapshots WHERE (cluster,instance,namespace)=(OLD.cluster,OLD.instance,OLD.name);
 DELETE FROM asset_runtime_bindings WHERE (cluster,instance,namespace)=(OLD.cluster,OLD.instance,OLD.name);
END;
CREATE TRIGGER IF NOT EXISTS asset_runtime_ns_recreated AFTER UPDATE OF uid ON cluster_inventory_namespaces WHEN OLD.uid<>'' AND OLD.uid<>NEW.uid BEGIN
 DELETE FROM asset_runtime_snapshots WHERE (cluster,instance,namespace)=(OLD.cluster,OLD.instance,OLD.name);
 DELETE FROM asset_runtime_bindings WHERE (cluster,instance,namespace)=(OLD.cluster,OLD.instance,OLD.name);
END;
CREATE TRIGGER IF NOT EXISTS asset_runtime_source_drop AFTER DELETE ON cluster_inventory_sources BEGIN
 DELETE FROM asset_runtime_snapshots WHERE (cluster,instance)=(OLD.cluster,OLD.instance);
 DELETE FROM asset_runtime_bindings WHERE (cluster,instance)=(OLD.cluster,OLD.instance);
END;
CREATE TRIGGER IF NOT EXISTS asset_runtime_deployment_drop AFTER DELETE ON inventory_business_deployments WHEN NOT EXISTS (
 SELECT 1 FROM inventory_business_deployments WHERE (cluster,instance,namespace,application)=(OLD.cluster,OLD.instance,OLD.namespace,OLD.application) AND verified_at<>'') BEGIN
 DELETE FROM asset_runtime_bindings WHERE (cluster,instance,namespace,application)=(OLD.cluster,OLD.instance,OLD.namespace,OLD.application);
END;
COMMIT;
