-- SQLite Gateway DB. Apply explicitly before enabling the monitor route.
BEGIN IMMEDIATE;
CREATE TABLE IF NOT EXISTS asset_monitor_series (
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
-- Detect an incompatible pre-release schema before completing this migration.
SELECT device_id FROM asset_monitor_series LIMIT 0;
CREATE TABLE IF NOT EXISTS asset_monitor_samples (
 series_id INTEGER NOT NULL REFERENCES asset_monitor_series(id), at_ms INTEGER NOT NULL,
 status TEXT NOT NULL CHECK(status IN ('ok','missing','not_applicable')), value REAL,
 CHECK((status='ok' AND value IS NOT NULL AND value>=0) OR (status<>'ok' AND value IS NULL)),
 PRIMARY KEY(series_id,at_ms)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS asset_monitor_rules (
 series_id INTEGER PRIMARY KEY REFERENCES asset_monitor_series(id), threshold REAL NOT NULL CHECK(threshold>=0),
 duration_seconds INTEGER NOT NULL CHECK(duration_seconds BETWEEN 1 AND 604800), effective_ms INTEGER NOT NULL,
 first_above_ms INTEGER
);
CREATE TABLE IF NOT EXISTS asset_monitor_alerts (
 series_id INTEGER NOT NULL REFERENCES asset_monitor_series(id), opened_ms INTEGER NOT NULL,
 threshold REAL NOT NULL, duration_seconds INTEGER NOT NULL, ended_ms INTEGER,
 state TEXT NOT NULL CHECK(state IN ('open','recovered','interrupted','reconfigured')),
 PRIMARY KEY(series_id,opened_ms)
) WITHOUT ROWID;
CREATE UNIQUE INDEX IF NOT EXISTS asset_monitor_one_open ON asset_monitor_alerts(series_id) WHERE state='open';
COMMIT;
