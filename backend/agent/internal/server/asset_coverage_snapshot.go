package server

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

type coverageTarget struct {
	Cluster  string `json:"cluster"`
	Instance string `json:"instance"`
}
type coverageEvidence struct {
	Type         string         `json:"type"`
	Reference    string         `json:"reference"`
	Source       coverageTarget `json:"source"`
	ClusterAsset string         `json:"cluster_asset,omitempty"`
	PeerMachine  string         `json:"peer_machine,omitempty"`
	Verified     bool           `json:"verified"`
	VerifiedAt   string         `json:"verified_at,omitempty"`
	Revision     int64          `json:"revision,omitempty"`
}
type coverageAsset struct {
	Memberships  []coverageMembership `json:"memberships,omitempty"`
	RegisterOnly bool                 `json:"register_only,omitempty"`
	Kind         string               `json:"kind"`
	AssetID      string               `json:"asset_id"`
	Name         string               `json:"name"`
	Target       coverageTarget       `json:"target"`
	Evidence     []coverageEvidence   `json:"evidence"`
}
type coverageHost struct {
	MachineID string `json:"machine_id"`
	HostID    string `json:"host_id"`
	Revision  int64  `json:"revision"`
}

// Machine and business identities live in separate inventories. Snapshot both ends
// of each host edge so either asset can be reconciled without exposing a hidden peer.
func captureCoverage(ctx context.Context, tx *sql.Tx, auth TokenAuth, at time.Time, hosts []coverageHost) ([]coverageAsset, error) {
	machines, err := listInventoryMachines(tx, auth)
	if err != nil {
		return nil, err
	}
	inactive, err := tx.QueryContext(ctx, `SELECT asset_id FROM inventory_machines WHERE active=0`)
	if err != nil {
		return nil, err
	}
	retired := map[string]bool{}
	for inactive.Next() {
		var id string
		if err := inactive.Scan(&id); err != nil {
			inactive.Close()
			return nil, err
		}
		retired[id] = true
	}
	err = inactive.Err()
	inactive.Close()
	if err != nil {
		return nil, err
	}
	confirmed := map[coverageHost]bool{}
	for _, pair := range hosts {
		confirmed[pair] = true
	}
	assets := []coverageAsset{}
	indexes := map[string]int{}
	for _, m := range machines {
		if retired[m.AssetID] {
			continue
		}
		indexes[m.AssetID] = len(assets)
		assets = append(assets, coverageAsset{Kind: "machines", AssetID: m.AssetID, Name: m.Name, Target: coverageTarget{m.Cluster, m.Instance}, Evidence: []coverageEvidence{}})
	}
	for _, vm := range machines {
		child, ok := indexes[vm.AssetID]
		host, exists := indexes[vm.HostAssetID]
		if !ok || !exists {
			continue
		}
		key := coverageHost{vm.AssetID, vm.HostAssetID, vm.Revision}
		verified := confirmed[key]
		delete(confirmed, key)
		e := coverageEvidence{Type: "host", Reference: vm.HostAssetID, PeerMachine: vm.HostAssetID, Source: assets[host].Target, Verified: verified, Revision: vm.Revision}
		if verified {
			e.VerifiedAt = formatTime(at)
		}
		assets[child].Evidence = append(assets[child].Evidence, e)
		e.Type = "hosts_vm"
		e.Reference = vm.AssetID
		e.PeerMachine = vm.AssetID
		e.Source = assets[child].Target
		assets[host].Evidence = append(assets[host].Evidence, e)
	}
	if len(confirmed) > 0 {
		return nil, machineInventoryProblem{409, "host_changed", "宿主关系或机器版本已变更，请重新核验"}
	}
	links, err := readInventoryNodeLinks(ctx, tx, auth)
	if err != nil {
		return nil, err
	}
	for _, l := range links {
		if i, ok := indexes[l.MachineID]; ok && l.Source == "manual" {
			assets[i].Evidence = append(assets[i].Evidence, coverageEvidence{Type: "node", Reference: l.NodeName, ClusterAsset: l.AssetID, Source: coverageTarget{l.Cluster, l.Instance}, Verified: true})
		}
	}
	systems, err := listBusinessInventory(tx, auth)
	if err != nil {
		return nil, err
	}
	for _, s := range systems {
		if !s.Active {
			continue
		}
		row := coverageAsset{Kind: "systems", AssetID: s.AssetID, Name: s.Name, Evidence: []coverageEvidence{}}
		for _, d := range s.Deployments {
			verified, err := time.Parse(time.RFC3339Nano, d.VerifiedAt)
			ref := d.Namespace + "/" + d.Application
			if d.Database != "" {
				ref += " → " + d.Database
			}
			row.Evidence = append(row.Evidence, coverageEvidence{Type: "deployment", Reference: ref, Source: coverageTarget{d.Cluster, d.Instance}, Verified: err == nil && !verified.After(at) && d.Application != "", VerifiedAt: d.VerifiedAt})
		}
		assets = append(assets, row)
	}
	return assets, nil
}

type coverageAccess struct {
	admin             bool
	known             map[coverageKey]bool
	assignments       map[coverageKey]map[coverageTarget]bool
	grants            map[coverageTarget]bool
	machines, systems map[string]bool
	clusters          map[coverageTarget]string
}

func currentCoverageAccess(ctx context.Context, tx *sql.Tx, auth TokenAuth) (coverageAccess, error) {
	a := coverageAccess{grants: map[coverageTarget]bool{}, machines: map[string]bool{}, systems: map[string]bool{}, clusters: map[coverageTarget]string{}}
	rows, err := tx.QueryContext(ctx, inventoryGrants+`SELECT a.is_admin,p.cluster,p.instance FROM actor a LEFT JOIN permitted p ON TRUE`, auth.UserID, auth.TokenID)
	if err != nil {
		return a, err
	}
	valid := false
	for rows.Next() {
		var c, i sql.NullString
		if err := rows.Scan(&a.admin, &c, &i); err != nil {
			rows.Close()
			return a, err
		}
		valid = true
		if c.Valid {
			a.grants[coverageTarget{c.String, i.String}] = true
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return a, err
	}
	if !valid {
		return a, machineInventoryProblem{403, "inventory_forbidden", "身份已失效"}
	}
	if a.admin {
		return a, nil
	}
	machines, err := listInventoryMachines(tx, auth)
	if err != nil {
		return a, err
	}
	for _, m := range machines {
		a.machines[m.AssetID] = true
	}
	systems, err := listBusinessInventory(tx, auth)
	if err != nil {
		return a, err
	}
	for _, s := range systems {
		a.systems[s.AssetID] = true
	}
	clusters, err := readInventoryClusters(ctx, tx, auth)
	if err != nil {
		return a, err
	}
	for _, c := range clusters {
		a.clusters[coverageTarget{c.Cluster, c.Instance}] = c.AssetID
	}
	return a, nil
}
func (a coverageAccess) allows(t coverageTarget) bool {
	return a.admin || (t.Cluster != "" && t.Instance != "" && (a.grants[t] || a.grants[coverageTarget{t.Cluster, "*"}] || a.grants[coverageTarget{"*", t.Instance}] || a.grants[coverageTarget{"*", "*"}]))
}
func (a coverageAccess) visible(row coverageAsset) (coverageAsset, bool) {
	if a.admin {
		return row, true
	}
	key := coverageKey{row.Kind, row.AssetID}
	visibleMemberships := []coverageMembership{}
	assigned := false
	for _, m := range row.Memberships {
		if m.ClusterID == "" || !a.clusterVisible(m.ClusterID) {
			continue
		}
		if row.RegisterOnly && !a.assignments[key][m.Source] {
			continue
		}
		if m.Source != (coverageTarget{}) {
			if a.clusters[m.Source] != m.ClusterID || !a.allows(m.Source) {
				continue
			}
			if a.assignments[key][m.Source] {
				assigned = true
				if row.RegisterOnly {
					row.Name = m.Name
				}
			}
		}
		visibleMemberships = append(visibleMemberships, m)
	}
	row.Memberships = visibleMemberships
	if row.RegisterOnly {
		return row, assigned && (!a.known[key] || row.Kind == "machines" && a.machines[row.AssetID] || row.Kind == "systems" && a.systems[row.AssetID])
	}
	if row.Kind == "machines" && (!a.machines[row.AssetID] || !a.allows(row.Target)) {
		return coverageAsset{}, false
	}
	if row.Kind == "systems" && !a.systems[row.AssetID] {
		return coverageAsset{}, false
	}
	evidence := []coverageEvidence{}
	for _, e := range row.Evidence {
		if a.allows(e.Source) && (e.PeerMachine == "" || a.machines[e.PeerMachine]) && (e.ClusterAsset == "" || a.clusters[e.Source] == e.ClusterAsset) {
			evidence = append(evidence, e)
		}
	}
	row.Evidence = evidence
	return row, row.Kind == "machines" || len(evidence) > 0 || assigned
}

// The machine API shares revision with this action but never depends on the coverage migration.
func setCoverageMachineActive(tx *sql.Tx, req coverageRequest) (string, error) {
	var active bool
	var revision int64
	err := tx.QueryRow(`SELECT active,revision FROM inventory_machines WHERE asset_id=?`, req.MachineID).Scan(&active, &revision)
	if err == sql.ErrNoRows {
		return "", machineInventoryProblem{404, "machine_not_found", "机器不存在"}
	}
	if err != nil {
		return "", err
	}
	if revision != *req.ExpectedRevision {
		return "", machineInventoryProblem{409, "revision_conflict", "机器版本已变化，请刷新后重新核对"}
	}
	if active == *req.Active {
		return "", machineInventoryProblem{409, "lifecycle_unchanged", "机器已处于所选状态，请刷新后核对"}
	}
	var next int64
	err = tx.QueryRow(`UPDATE inventory_machines SET active=?,revision=revision+1 WHERE asset_id=? AND revision=? RETURNING revision`, *req.Active, req.MachineID, revision).Scan(&next)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("machine_id=%s active=%t->%t revision=%d->%d", req.MachineID, active, *req.Active, revision, next), nil
}

func listCoverageMachineStates(tx *sql.Tx) ([]coverageMachineState, error) {
	rows, err := tx.Query(`SELECT asset_id,name,active,revision FROM inventory_machines ORDER BY asset_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	states := []coverageMachineState{}
	for rows.Next() {
		var m coverageMachineState
		if err := rows.Scan(&m.AssetID, &m.Name, &m.Active, &m.Revision); err != nil {
			return nil, err
		}
		states = append(states, m)
	}
	return states, rows.Err()
}
