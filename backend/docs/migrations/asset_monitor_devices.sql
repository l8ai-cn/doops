-- Upgrade an existing GPU-aware asset_monitor.sql database on a dedicated connection.
-- Preserve series IDs referenced by samples, rules and alerts. Abort on SQL errors.
PRAGMA foreign_keys=OFF;
BEGIN IMMEDIATE;
CREATE TABLE asset_monitor_series_new (
 id INTEGER PRIMARY KEY, cluster TEXT NOT NULL, instance TEXT NOT NULL, namespace TEXT NOT NULL,
 type TEXT NOT NULL CHECK(type IN ('machine','os','kernel','process','app','database')),
 asset_id TEXT NOT NULL, origin TEXT NOT NULL CHECK(origin IN ('k8s_top','exporter')),
 metric TEXT NOT NULL CHECK(metric IN ('cpu','memory','gpu','network','requests','storage','storage_total','storage_available')),
 device_id TEXT NOT NULL DEFAULT '',
 CHECK((metric='gpu' AND length(device_id)=40 AND substr(device_id,1,4)='GPU-')
    OR (metric='network' AND length(device_id) BETWEEN 1 AND 15 AND device_id GLOB '[a-zA-Z0-9]*' AND device_id NOT GLOB '*[^a-zA-Z0-9_.-]*')
    OR (metric IN ('storage','storage_total','storage_available') AND substr(device_id,1,1)='/')
    OR (metric IN ('cpu','memory','requests','storage','network') AND device_id='')),
 UNIQUE(cluster,instance,type,asset_id,namespace,origin,metric,device_id)
);
INSERT INTO asset_monitor_series_new(id,cluster,instance,namespace,type,asset_id,origin,metric,device_id)
 SELECT id,cluster,instance,namespace,type,asset_id,origin,metric,device_id FROM asset_monitor_series;
DROP TABLE asset_monitor_series;
ALTER TABLE asset_monitor_series_new RENAME TO asset_monitor_series;
CREATE TEMP TABLE monitor_fk_check (violations INTEGER CHECK(violations=0));
INSERT INTO monitor_fk_check SELECT count(*) FROM pragma_foreign_key_check WHERE parent='asset_monitor_series';
DROP TABLE monitor_fk_check;
COMMIT;
PRAGMA foreign_keys=ON;
