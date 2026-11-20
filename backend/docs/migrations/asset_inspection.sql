-- Gateway SQLite. Apply explicitly through the audited migration process.
BEGIN IMMEDIATE;
CREATE TABLE IF NOT EXISTS inspection_plans (
 id INTEGER PRIMARY KEY, name TEXT NOT NULL, owner TEXT NOT NULL, executor TEXT NOT NULL REFERENCES users(id),
 timezone TEXT NOT NULL, weekday INTEGER NOT NULL CHECK(weekday BETWEEN 0 AND 6), clock TEXT NOT NULL,
 next_due INTEGER NOT NULL, active INTEGER NOT NULL DEFAULT 1
) STRICT;
CREATE INDEX IF NOT EXISTS inspection_schedule ON inspection_plans(active,next_due);
CREATE TABLE IF NOT EXISTS inspection_scope (
 plan_id INTEGER NOT NULL REFERENCES inspection_plans(id), asset_id TEXT NOT NULL, cluster TEXT NOT NULL,
 instance TEXT NOT NULL, cluster_id TEXT NOT NULL, check_id TEXT NOT NULL,
 PRIMARY KEY(plan_id,asset_id,cluster,instance,check_id)
) STRICT;
CREATE TABLE IF NOT EXISTS inspection_runs (
 id INTEGER PRIMARY KEY, plan_id INTEGER NOT NULL REFERENCES inspection_plans(id), due INTEGER NOT NULL,
 frozen INTEGER NOT NULL, trigger_kind TEXT NOT NULL CHECK(trigger_kind IN ('manual','scheduled','reconciled'))
) STRICT;
CREATE UNIQUE INDEX IF NOT EXISTS inspection_cycle ON inspection_runs(plan_id,due,(trigger_kind='manual'));
CREATE TABLE IF NOT EXISTS inspection_items (
 id INTEGER PRIMARY KEY, run_id INTEGER NOT NULL REFERENCES inspection_runs(id), asset_id TEXT NOT NULL,
 cluster TEXT NOT NULL, instance TEXT NOT NULL, cluster_id TEXT NOT NULL, check_id TEXT NOT NULL,
 generation INTEGER NOT NULL DEFAULT 1, opened INTEGER NOT NULL, deadline INTEGER NOT NULL,
 status TEXT NOT NULL, reason TEXT NOT NULL DEFAULT '', repair TEXT NOT NULL DEFAULT '', repair_by TEXT NOT NULL DEFAULT '', repair_at INTEGER NOT NULL DEFAULT 0,
 UNIQUE(run_id,asset_id,cluster,instance,check_id)
) STRICT;
CREATE INDEX IF NOT EXISTS inspection_work ON inspection_items(status,deadline,id);
CREATE TABLE IF NOT EXISTS inspection_receipts (
 item_id INTEGER NOT NULL REFERENCES inspection_items(id), generation INTEGER NOT NULL, actor TEXT NOT NULL,
 received INTEGER NOT NULL, status TEXT NOT NULL, reason TEXT NOT NULL, evidence TEXT NOT NULL, digest TEXT NOT NULL,
 PRIMARY KEY(item_id,generation)
) STRICT;
COMMIT;
