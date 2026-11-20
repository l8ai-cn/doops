package server

import (
	"context"
	"database/sql"
	"net/http"
	"sort"
)

type dashboardCluster struct {
	AssetID    string `json:"asset_id"`
	Name       string `json:"name"`
	Sources    int    `json:"sources"`
	Online     int    `json:"online"`
	Namespaces int    `json:"namespaces"`
	Machines   int    `json:"machines"`
	Systems    int    `json:"systems"`
	VerifiedAt string `json:"verified_at"`
}

type dashboardMachine struct {
	AssetID string `json:"asset_id"`
	Name    string `json:"name"`
}

type assetDashboard struct {
	Machines   int                `json:"machines"`
	Systems    int                `json:"systems"`
	Clusters   []dashboardCluster `json:"clusters"`
	Unassigned []dashboardMachine `json:"unassigned"`
}

func (h *GatewayHub) HandleAssetDashboard(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	auth, err := h.authenticateUser(r)
	if err != nil {
		h.writeUserAuthError(w, r, err)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "Method Not Allowed", 405)
		return
	}
	result, err := h.store.assetDashboard(r.Context(), auth)
	if err != nil {
		writeDoopsError(w, 503, "", doopsErrorResponse{Error: "asset_dashboard_unavailable", Code: "asset_dashboard_unavailable", Message: "无法读取资产看板，请检查资产表迁移和 Gateway 状态。"})
		return
	}
	writeJSONHTTP(w, result)
}

func (s *GatewayStore) assetDashboard(ctx context.Context, auth TokenAuth) (assetDashboard, error) {
	var result assetDashboard
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	sources, err := readInventoryClusters(ctx, tx, auth)
	if err != nil {
		return result, err
	}
	machines, err := listInventoryMachines(tx, auth)
	if err != nil {
		return result, err
	}
	links, err := readInventoryNodeLinks(ctx, tx, auth)
	if err != nil {
		return result, err
	}
	systems, err := listBusinessInventory(tx, auth)
	if err != nil {
		return result, err
	}
	type members struct{ namespaces, machines, systems map[string]bool }
	groups := map[string]*dashboardCluster{}
	counts := map[string]members{}
	targets := map[[2]string]string{}
	for _, source := range sources {
		id := source.AssetID
		if groups[id] == nil {
			groups[id] = &dashboardCluster{AssetID: id, Name: source.Name}
			counts[id] = members{map[string]bool{}, map[string]bool{}, map[string]bool{}}
		}
		group := groups[id]
		group.Sources++
		if source.Status == "online" {
			group.Online++
		}
		if source.VerifiedAt > group.VerifiedAt {
			group.VerifiedAt = source.VerifiedAt
		}
		targets[[2]string{source.Cluster, source.Instance}] = id
		for _, ns := range source.Namespaces {
			counts[id].namespaces[ns.Name] = true
		}
	}
	linked := map[string]bool{}
	for _, link := range links {
		counts[link.AssetID].machines[link.MachineID] = true
		linked[link.MachineID] = true
	}
	result.Unassigned = []dashboardMachine{}
	for _, m := range machines {
		if id := targets[[2]string{m.Cluster, m.Instance}]; id != "" {
			counts[id].machines[m.AssetID] = true
		} else if !linked[m.AssetID] {
			result.Unassigned = append(result.Unassigned, dashboardMachine{m.AssetID, m.Name})
		}
	}
	result.Machines = len(machines)
	for _, system := range systems {
		if !system.Active {
			continue
		}
		result.Systems++
		for _, dep := range system.Deployments {
			if id := targets[[2]string{dep.Cluster, dep.Instance}]; id != "" {
				counts[id].systems[system.AssetID] = true
			}
		}
	}
	result.Clusters = []dashboardCluster{}
	for id, group := range groups {
		group.Namespaces = len(counts[id].namespaces)
		group.Machines = len(counts[id].machines)
		group.Systems = len(counts[id].systems)
		result.Clusters = append(result.Clusters, *group)
	}
	sort.Slice(result.Clusters, func(i, j int) bool { return result.Clusters[i].AssetID < result.Clusters[j].AssetID })
	return result, tx.Commit()
}
