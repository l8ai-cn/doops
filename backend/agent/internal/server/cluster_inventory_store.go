package server

import (
	"context"
	"database/sql"
	"encoding/json"
)

func (s *GatewayStore) listInventoryClusters(ctx context.Context, auth TokenAuth) ([]inventoryCluster, error) {
	return readInventoryClusters(ctx, s.db, auth)
}

type inventoryReader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func readInventoryClusters(ctx context.Context, db inventoryReader, auth TokenAuth) ([]inventoryCluster, error) {
	rows, err := db.QueryContext(ctx, inventoryGrants+`SELECT s.asset_id, s.name, s.cluster, s.instance,
 COALESCE(a.status, 'offline'), s.scope, s.verified_at, s.verified_by,
 n.name, n.uid, n.scope, n.verified_at, n.verified_by, n.namespace_observed_at, n.pods_listed_at FROM cluster_inventory_sources s
 LEFT JOIN cluster_inventory_namespaces n USING(cluster, instance)
 LEFT JOIN agent_status a USING(cluster, instance)
 WHERE EXISTS(SELECT 1 FROM permitted p WHERE p.cluster IN ('*', s.cluster) AND p.instance IN ('*', s.instance))
 ORDER BY s.asset_id, s.cluster, s.instance, n.name`, auth.UserID, auth.TokenID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	clusters := []inventoryCluster{}
	for rows.Next() {
		var row inventoryCluster
		var name, uid, scope, verifiedAt, actor, namespaceAt, podsAt sql.NullString
		if err := rows.Scan(&row.AssetID, &row.Name, &row.Cluster, &row.Instance, &row.Status, &row.Scope, &row.VerifiedAt,
			&row.VerifiedBy, &name, &uid, &scope, &verifiedAt, &actor, &namespaceAt, &podsAt); err != nil {
			return nil, err
		}
		if len(clusters) == 0 || clusters[len(clusters)-1].Cluster != row.Cluster || clusters[len(clusters)-1].Instance != row.Instance {
			row.Namespaces = []inventoryNamespace{}
			clusters = append(clusters, row)
		}
		if name.Valid {
			last := &clusters[len(clusters)-1]
			last.Namespaces = append(last.Namespaces, inventoryNamespace{Name: name.String, UID: uid.String, Scope: scope.String, VerifiedAt: verifiedAt.String, VerifiedBy: actor.String,
				NamespaceObservedAt: namespaceAt.String, PodsListedAt: podsAt.String})
		}
	}
	return clusters, rows.Err()
}

func saveInventoryObservation(tx *sql.Tx, row inventoryCluster) error {
	var systemUID string
	for _, ns := range row.Namespaces {
		if ns.Name == "kube-system" {
			systemUID = ns.UID
		}
	}
	// The administrator explicitly supplies the asset ID. Each route keeps its
	// own evidence. When both lists expose kube-system, its UID must agree.
	var conflict bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM cluster_inventory_sources
 WHERE (cluster=? AND instance=? AND asset_id<>?)
 OR (asset_id=? AND (name<>? OR (kube_system_uid<>'' AND ?<>'' AND kube_system_uid<>?))))`,
		row.Cluster, row.Instance, row.AssetID, row.AssetID, row.Name, systemUID, systemUID).Scan(&conflict); err != nil {
		return err
	}
	if conflict {
		return inventoryFailure{409, "cluster_identity_conflict"}
	}
	_, err := tx.Exec(`INSERT INTO cluster_inventory_sources
 (cluster,instance,asset_id,name,kube_system_uid,verified_at,verified_by,scope) VALUES (?,?,?,?,?,?,?,?)
 ON CONFLICT(cluster,instance) DO UPDATE SET verified_at=excluded.verified_at, verified_by=excluded.verified_by,
 scope=excluded.scope, kube_system_uid=COALESCE(NULLIF(excluded.kube_system_uid,''),cluster_inventory_sources.kube_system_uid)`,
		row.Cluster, row.Instance, row.AssetID, row.Name, systemUID, row.VerifiedAt, row.VerifiedBy, row.Scope)
	if err != nil {
		return err
	}
	if row.Scope == "list_namespaces" {
		// Keep both kinds of evidence for surviving names. Only a complete
		// Namespace list can retire names absent from this source.
		names := make([]string, 0, len(row.Namespaces))
		for _, ns := range row.Namespaces {
			names = append(names, ns.Name)
		}
		listed, err := json.Marshal(names)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM cluster_inventory_namespaces WHERE cluster=? AND instance=?
 AND name NOT IN (SELECT value FROM json_each(?))`, row.Cluster, row.Instance, string(listed)); err != nil {
			return err
		}
	}
	for _, ns := range row.Namespaces {
		_, err := tx.Exec(`INSERT INTO cluster_inventory_namespaces (cluster,instance,name,uid,verified_at,verified_by,scope,namespace_observed_at,pods_listed_at)
 VALUES (?,?,?,?,?,?,?,?,?) ON CONFLICT(cluster,instance,name) DO UPDATE SET
 uid=COALESCE(NULLIF(excluded.uid,''),cluster_inventory_namespaces.uid),
 namespace_observed_at=COALESCE(NULLIF(excluded.namespace_observed_at,''),cluster_inventory_namespaces.namespace_observed_at),
 pods_listed_at=CASE WHEN excluded.uid<>'' AND cluster_inventory_namespaces.uid<>'' AND excluded.uid<>cluster_inventory_namespaces.uid THEN ''
 ELSE COALESCE(NULLIF(excluded.pods_listed_at,''),cluster_inventory_namespaces.pods_listed_at) END,
 verified_at=excluded.verified_at, verified_by=excluded.verified_by, scope=excluded.scope`,
			row.Cluster, row.Instance, ns.Name, ns.UID, ns.VerifiedAt, ns.VerifiedBy, ns.Scope, ns.NamespaceObservedAt, ns.PodsListedAt)
		if err != nil {
			return err
		}
	}
	return nil
}
