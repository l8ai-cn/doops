-- Apply once to the Gateway SQLite database after machine_inventory.sql and
-- cluster_inventory.sql. Each Node name belongs to one machine per source.
BEGIN IMMEDIATE;
CREATE TABLE inventory_node_links (
    cluster TEXT NOT NULL,
    instance TEXT NOT NULL,
    node_name TEXT NOT NULL CHECK (length(node_name) BETWEEN 1 AND 253),
    machine_id TEXT NOT NULL REFERENCES inventory_machines(asset_id),
    PRIMARY KEY (cluster, instance, node_name),
    FOREIGN KEY (cluster, instance) REFERENCES cluster_inventory_sources(cluster, instance)
) STRICT;
CREATE INDEX inventory_node_links_machine ON inventory_node_links(machine_id);

-- GatewayStore opens SQLite connections with foreign_keys off. Keep these
-- references enforced across restarts and other writers on the same database
-- without changing constraint behavior for unrelated Gateway tables.
CREATE TRIGGER inventory_node_links_check_insert BEFORE INSERT ON inventory_node_links
WHEN NOT EXISTS (SELECT 1 FROM inventory_machines WHERE asset_id = NEW.machine_id)
  OR NOT EXISTS (SELECT 1 FROM cluster_inventory_sources WHERE (cluster, instance) = (NEW.cluster, NEW.instance))
BEGIN SELECT RAISE(ABORT, 'missing node link parent'); END;
CREATE TRIGGER inventory_node_links_check_update BEFORE UPDATE OF machine_id, cluster, instance ON inventory_node_links
WHEN NOT EXISTS (SELECT 1 FROM inventory_machines WHERE asset_id = NEW.machine_id)
  OR NOT EXISTS (SELECT 1 FROM cluster_inventory_sources WHERE (cluster, instance) = (NEW.cluster, NEW.instance))
BEGIN SELECT RAISE(ABORT, 'missing node link parent'); END;
CREATE TRIGGER inventory_node_links_machine_delete BEFORE DELETE ON inventory_machines
WHEN EXISTS (SELECT 1 FROM inventory_node_links WHERE machine_id = OLD.asset_id)
BEGIN SELECT RAISE(ABORT, 'machine has node links'); END;
CREATE TRIGGER inventory_node_links_machine_rekey BEFORE UPDATE OF asset_id ON inventory_machines
WHEN NEW.asset_id <> OLD.asset_id AND EXISTS (SELECT 1 FROM inventory_node_links WHERE machine_id = OLD.asset_id)
BEGIN SELECT RAISE(ABORT, 'machine has node links'); END;
CREATE TRIGGER inventory_node_links_source_delete BEFORE DELETE ON cluster_inventory_sources
WHEN EXISTS (SELECT 1 FROM inventory_node_links WHERE (cluster, instance) = (OLD.cluster, OLD.instance))
BEGIN SELECT RAISE(ABORT, 'cluster source has node links'); END;
CREATE TRIGGER inventory_node_links_source_rekey BEFORE UPDATE OF asset_id, cluster, instance ON cluster_inventory_sources
WHEN (NEW.asset_id, NEW.cluster, NEW.instance) <> (OLD.asset_id, OLD.cluster, OLD.instance)
 AND EXISTS (SELECT 1 FROM inventory_node_links WHERE (cluster, instance) = (OLD.cluster, OLD.instance))
BEGIN SELECT RAISE(ABORT, 'cluster source has node links'); END;
COMMIT;
