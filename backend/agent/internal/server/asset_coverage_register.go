package server

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// A register establishes membership. A documentary reference is never a verified topology edge.
type coverageEntry struct {
	Kind    string         `json:"kind"`
	AssetID string         `json:"asset_id"`
	Name    string         `json:"name"`
	Source  coverageTarget `json:"source"`
}
type coverageRegister struct {
	Document string          `json:"document_ref"`
	Entries  []coverageEntry `json:"entries"`
	Revision int64           `json:"revision"`
}
type coverageMembership struct {
	Name      string         `json:"registered_name"`
	ClusterID string         `json:"cluster_id"`
	Document  string         `json:"document_ref"`
	Source    coverageTarget `json:"source"`
}
type coverageKey struct{ kind, id string }

func registerSchema(tx *sql.Tx) (bool, error) {
	var ok bool
	err := tx.QueryRow(`SELECT count(*)=2 FROM sqlite_schema WHERE type='table' AND name IN ('asset_coverage_registers','asset_coverage_cluster_versions')`).Scan(&ok)
	return ok, err
}
func coverageClusterSources(tx *sql.Tx) (map[coverageTarget]string, error) {
	rows, err := tx.Query(`SELECT cluster,instance,asset_id FROM cluster_inventory_sources`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sources := map[coverageTarget]string{}
	for rows.Next() {
		var t coverageTarget
		var id string
		if err = rows.Scan(&t.Cluster, &t.Instance, &id); err != nil {
			return nil, err
		}
		sources[t] = id
	}
	return sources, rows.Err()
}
func (a coverageAccess) clusterVisible(id string) bool {
	for _, cluster := range a.clusters {
		if cluster == id {
			return true
		}
	}
	return false
}
func coverageRegisters(tx *sql.Tx) (map[string]coverageRegister, error) {
	rows, err := tx.Query(`SELECT scope,revision,document_ref,entries FROM asset_coverage_registers ORDER BY scope`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]coverageRegister{}
	for rows.Next() {
		var id, raw string
		var reg coverageRegister
		if err := rows.Scan(&id, &reg.Revision, &reg.Document, &raw); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(raw), &reg.Entries); err != nil {
			return nil, err
		}
		result[id] = reg
	}
	return result, rows.Err()
}
func saveCoverageRegister(tx *sql.Tx, scope string, reg coverageRegister, sources map[coverageTarget]string, actor, at string) error {
	validText := func(s string) bool { return strings.TrimSpace(s) != "" && len(s) <= 256 && inventoryMetadata(s) }
	if reg.Revision < 0 || !validText(reg.Document) || reg.Entries == nil {
		return machineBadRequest("invalid_register", "请提供底册凭据编号、资产清单及当前版本")
	}
	seen := map[coverageKey]bool{}
	for _, e := range reg.Entries {
		k := coverageKey{e.Kind, e.AssetID}
		if seen[k] || !slices.Contains([]string{"machines", "systems"}, e.Kind) || !safeMachineID(e.AssetID) || !validText(e.Name) {
			return machineBadRequest("invalid_register_entry", "请核对重复编号、资产类型和名称")
		}
		seen[k] = true
		if e.Source != (coverageTarget{}) && (scope == "" || e.Source.Cluster == "" || e.Source.Instance == "" || sources[e.Source] != scope) {
			return machineBadRequest("invalid_register_source", "可见来源必须是此集群已登记的精确目标")
		}
	}
	data, err := json.Marshal(reg.Entries)
	if err != nil {
		return err
	}
	var result sql.Result
	if reg.Revision == 0 {
		result, err = tx.Exec(`INSERT INTO asset_coverage_registers(scope,revision,document_ref,entries,updated_by,updated_at) VALUES(?,1,?,?,?,?) ON CONFLICT(scope) DO NOTHING`, scope, reg.Document, string(data), actor, at)
	} else {
		result, err = tx.Exec(`UPDATE asset_coverage_registers SET revision=revision+1,document_ref=?,entries=?,updated_by=?,updated_at=? WHERE scope=? AND revision=?`, reg.Document, string(data), actor, at, scope, reg.Revision)
	}
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return machineInventoryProblem{409, "register_changed", "底册已变更，请刷新后核对"}
	}
	return nil
}

// Merge the entire in-service inventory and all signed-off registers by (kind,id).
// Current readers may have only part of the inventory. A restricted or retired
// asset must never reappear as a new, register-only asset via a second source.
func mergeCoverageRegisters(tx *sql.Tx, assets []coverageAsset, regs map[string]coverageRegister, sources map[coverageTarget]string) ([]coverageAsset, error) {
	known := map[coverageKey]bool{}
	rows, err := tx.Query(`SELECT 'machines',asset_id FROM inventory_machines UNION ALL SELECT 'systems',asset_id FROM inventory_business_systems`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var k coverageKey
		if err := rows.Scan(&k.kind, &k.id); err != nil {
			return nil, err
		}
		known[k] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	index := map[coverageKey]int{}
	for i, a := range assets {
		index[coverageKey{a.Kind, a.AssetID}] = i
		for j, e := range a.Evidence {
			if e.Type == "deployment" {
				assets[i].Evidence[j].ClusterAsset = sources[e.Source]
			}
		}
	}
	scopes := make([]string, 0, len(regs))
	for scope := range regs {
		scopes = append(scopes, scope)
	}
	sort.Strings(scopes)
	for _, scope := range scopes {
		reg := regs[scope]
		for _, e := range reg.Entries {
			k := coverageKey{e.Kind, e.AssetID}
			i, exists := index[k]
			if !exists {
				if known[k] {
					continue
				}
				i = len(assets)
				index[k] = i
				assets = append(assets, coverageAsset{Kind: e.Kind, AssetID: e.AssetID, Name: e.Name, RegisterOnly: true, Evidence: []coverageEvidence{}})
			}
			assets[i].Memberships = append(assets[i].Memberships, coverageMembership{e.Name, scope, reg.Document, e.Source})
		}
	}
	sort.Slice(assets, func(i, j int) bool {
		a, b := assets[i], assets[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.AssetID < b.AssetID
	})
	return assets, nil
}
func projectCoverage(assets []coverageAsset, scope string) []coverageAsset {
	members := map[string]bool{}
	selected := []coverageAsset{}
	for _, a := range assets {
		for _, m := range a.Memberships {
			if m.ClusterID == scope {
				a.Memberships = []coverageMembership{m}
				if a.RegisterOnly {
					a.Name = m.Name
				}
				selected = append(selected, a)
				if a.Kind == "machines" {
					members[a.AssetID] = true
				}
				break
			}
		}
	}
	for i, a := range selected {
		edges := []coverageEvidence{}
		for _, e := range a.Evidence {
			if e.ClusterAsset == scope || e.PeerMachine != "" && members[e.PeerMachine] {
				edges = append(edges, e)
			}
		}
		selected[i].Evidence = edges
	}
	return selected
}
func freezeClusterCoverage(tx *sql.Tx, scope, id, actor, at string, reg coverageRegister, assets []coverageAsset) error {
	data, err := json.Marshal(projectCoverage(assets, scope))
	if err != nil {
		return err
	}
	r, err := tx.Exec(`INSERT INTO asset_coverage_cluster_versions(cluster_id,id,frozen_at,frozen_by,snapshot,document_ref,register_revision) VALUES(?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`, scope, id, at, actor, string(data), reg.Document, reg.Revision)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err == nil && n != 1 {
		return machineInventoryProblem{409, "baseline_exists", "该范围已有同编号的冻结版本"}
	}
	return err
}
func clusterCoveragePayload(tx *sql.Tx, scope, id string) ([]coverageAsset, string, int64, error) {
	var raw, doc string
	var revision int64
	err := tx.QueryRow(`SELECT snapshot,document_ref,register_revision FROM asset_coverage_cluster_versions WHERE cluster_id=? AND id=?`, scope, id).Scan(&raw, &doc, &revision)
	if err != nil {
		return nil, "", 0, err
	}
	var assets []coverageAsset
	err = json.Unmarshal([]byte(raw), &assets)
	return assets, doc, revision, err
}
func coverageRegisterReceipt(scope string, reg coverageRegister) string {
	return fmt.Sprintf("cluster_id=%s register_revision=%d entries=%d", scope, reg.Revision+1, len(reg.Entries))
}

func (a *coverageAccess) indexRegisterAccess(tx *sql.Tx, regs map[string]coverageRegister) error {
	a.assignments = map[coverageKey]map[coverageTarget]bool{}
	for scope, reg := range regs {
		for _, e := range reg.Entries {
			if scope != "" && a.clusters[e.Source] == scope && a.allows(e.Source) {
				k := coverageKey{e.Kind, e.AssetID}
				if a.assignments[k] == nil {
					a.assignments[k] = map[coverageTarget]bool{}
				}
				a.assignments[k][e.Source] = true
			}
		}
	}
	rows, err := tx.Query(`SELECT 'machines',asset_id FROM inventory_machines UNION ALL SELECT 'systems',asset_id FROM inventory_business_systems`)
	if err != nil {
		return err
	}
	defer rows.Close()
	a.known = map[coverageKey]bool{}
	for rows.Next() {
		var k coverageKey
		if err := rows.Scan(&k.kind, &k.id); err != nil {
			return err
		}
		a.known[k] = true
	}
	return rows.Err()
}
