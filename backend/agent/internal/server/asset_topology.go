package server

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

type topologyNode struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Ref       string `json:"ref"`
	Namespace string `json:"namespace,omitempty"`
}

type topologyEvidence struct {
	Record     string `json:"record"`
	Cluster    string `json:"cluster,omitempty"`
	Instance   string `json:"instance,omitempty"`
	Namespace  string `json:"namespace,omitempty"`
	Status     string `json:"status,omitempty"`
	VerifiedAt string `json:"verified_at,omitempty"`
}

type topologyEdge struct {
	From     string             `json:"from"`
	To       string             `json:"to"`
	Kind     string             `json:"kind"`
	Layer    string             `json:"layer"`
	Evidence []topologyEvidence `json:"evidence"`
}

type assetTopology struct {
	Nodes   []topologyNode `json:"nodes"`
	Edges   []topologyEdge `json:"edges"`
	Runtime []runtimeScope `json:"runtime"`
	CanBind bool           `json:"can_bind"`
}

func (h *GatewayHub) HandleAssetTopology(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	auth, err := h.authenticateUser(r)
	if err != nil {
		h.writeUserAuthError(w, r, err)
		return
	}
	fail := func(status int, code string) {
		writeDoopsError(w, status, "", doopsErrorResponse{Error: code, Code: code})
	}
	if r.Method == http.MethodPost {
		h.handleRuntimeTopology(w, r, auth)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET, POST")
		fail(405, "method_not_allowed")
		return
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	id := q.Get("cluster_id")
	if err != nil || len(q) > 0 && (len(q) != 1 || len(q["cluster_id"]) != 1 || !inventoryKeyPattern.MatchString(id) || !inventoryMetadata(id)) {
		fail(400, "invalid_topology_scope")
		return
	}
	result, err := h.store.readAssetTopology(r.Context(), auth, id)
	if errors.Is(err, errTopologyScope) {
		fail(404, "topology_scope_not_found")
		return
	}
	if err != nil {
		fail(503, "asset_topology_unavailable")
		return
	}
	writeJSONHTTP(w, result)
}

var errTopologyScope = errors.New("topology scope not visible")

func (s *GatewayStore) readAssetTopology(ctx context.Context, auth TokenAuth, scope string) (assetTopology, error) {
	var empty assetTopology
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return empty, err
	}
	defer tx.Rollback()
	clusters, err := readInventoryClusters(ctx, tx, auth)
	if err != nil {
		return empty, err
	}
	if scope != "" {
		visible := false
		for _, source := range clusters {
			if source.AssetID == scope {
				visible = true
				break
			}
		}
		if !visible {
			return empty, errTopologyScope
		}
	}
	machines, err := listInventoryMachines(tx, auth)
	if err != nil {
		return empty, err
	}
	links, err := readInventoryNodeLinks(ctx, tx, auth)
	if err != nil {
		return empty, err
	}
	systems, err := listBusinessInventory(tx, auth)
	if err != nil {
		return empty, err
	}
	g := buildAssetTopology(scope, clusters, machines, links, systems)
	scopes, err := readRuntimeGraph(tx, auth, scope, &g, systems)
	if err != nil {
		return empty, err
	}
	out := g.result()
	out.Runtime = scopes
	err = tx.QueryRow(inventoryGrants+`SELECT EXISTS(SELECT 1 FROM permitted WHERE is_admin=1)`, auth.UserID, auth.TokenID).Scan(&out.CanBind)
	return out, err
}

// The key encodes component boundaries so names and references stay opaque to callers.
func topologyID(parts ...string) string {
	encoded := make([]string, len(parts))
	for i, p := range parts {
		encoded[i] = url.PathEscape(p)
	}
	return strings.Join(encoded, "/")
}

type topologyBuilder struct {
	nodes map[string]topologyNode
	edges map[[3]string]*topologyEdge
}

func (g *topologyBuilder) node(kind, name, ref, ns string, identity ...string) string {
	id := kind + "/" + topologyID(identity...)
	g.nodes[id] = topologyNode{id, kind, name, ref, ns}
	return id
}
func (g *topologyBuilder) edge(from, to, kind, layer string, proof topologyEvidence) {
	key := [3]string{from, to, kind}
	edge := g.edges[key]
	if edge == nil {
		edge = &topologyEdge{From: from, To: to, Kind: kind, Layer: layer}
		g.edges[key] = edge
	}
	for _, prior := range edge.Evidence {
		if prior == proof {
			return
		}
	}
	edge.Evidence = append(edge.Evidence, proof)
}
func (g *topologyBuilder) result() assetTopology {
	out := assetTopology{Nodes: []topologyNode{}, Edges: []topologyEdge{}}
	for _, n := range g.nodes {
		out.Nodes = append(out.Nodes, n)
	}
	for _, e := range g.edges {
		out.Edges = append(out.Edges, *e)
	}
	sort.Slice(out.Nodes, func(i, j int) bool { return out.Nodes[i].ID < out.Nodes[j].ID })
	sort.Slice(out.Edges, func(i, j int) bool {
		a, b := out.Edges[i], out.Edges[j]
		if a.From != b.From {
			return a.From < b.From
		}
		if a.To != b.To {
			return a.To < b.To
		}
		return a.Kind < b.Kind
	})
	return out
}

func buildAssetTopology(scope string, clusters []inventoryCluster, machines []inventoryMachine, links []inventoryNodeLink, systems []inventoryBusinessSystem) topologyBuilder {
	g := topologyBuilder{nodes: map[string]topologyNode{}, edges: map[[3]string]*topologyEdge{}}
	sources := map[[2]string]inventoryCluster{}
	for _, s := range clusters {
		if scope != "" && s.AssetID != scope {
			continue
		}
		sources[[2]string{s.Cluster, s.Instance}] = s
		cluster := g.node("cluster", s.Name, s.AssetID, "", s.AssetID)
		target := s.Cluster + "/" + s.Instance
		source := g.node("source", target, target, "", s.Cluster, s.Instance)
		g.edge(source, cluster, "observes", "deployment", topologyEvidence{Record: "cluster_inventory_sources", Cluster: s.Cluster, Instance: s.Instance, Status: s.Status, VerifiedAt: s.VerifiedAt})
	}
	machineByID := map[string]inventoryMachine{}
	included := map[string]bool{}
	machineNode := func(m inventoryMachine) string { return g.node("machine", m.Name, m.AssetID, "", m.AssetID) }
	for _, m := range machines {
		machineByID[m.AssetID] = m
		_, bound := sources[[2]string{m.Cluster, m.Instance}]
		if scope == "" || bound {
			included[m.AssetID] = true
			machine := machineNode(m)
			if m.Cluster != "" {
				target := m.Cluster + "/" + m.Instance
				source := g.node("source", target, target, "", m.Cluster, m.Instance)
				g.edge(machine, source, "bound_to", "deployment", topologyEvidence{Record: "inventory_machines.target", Cluster: m.Cluster, Instance: m.Instance, Status: m.TargetStatus})
			}
		}
	}
	// Source authorization and both endpoints of a manual Node mapping were checked in readInventoryNodeLinks.
	// Node names have per-source identity in inventory_node_links (there is no Kubernetes Node UID).
	for _, l := range links {
		s, ok := sources[[2]string{l.Cluster, l.Instance}]
		m, visible := machineByID[l.MachineID]
		if !ok || !visible {
			continue
		}
		included[m.AssetID] = true
		node := g.node("node", l.NodeName, l.NodeName, "", l.Cluster, l.Instance, l.NodeName)
		proof := topologyEvidence{Record: "inventory_node_links", Cluster: l.Cluster, Instance: l.Instance, Status: s.Status}
		g.edge(node, machineNode(m), "maps_to", "physical", proof)
		g.edge(node, topologyID("cluster", s.AssetID), "member_of", "deployment", proof)
	}
	for _, m := range machines {
		if !included[m.AssetID] || m.HostAssetID == "" {
			continue
		}
		if host, ok := machineByID[m.HostAssetID]; ok {
			g.edge(machineNode(m), machineNode(host), "hosted_on", "physical", topologyEvidence{Record: "inventory_machines.host_asset_id", Cluster: m.Cluster, Instance: m.Instance})
		}
	}
	for _, sys := range systems {
		systemNode := func() string { return g.node("system", sys.Name, sys.AssetID, "", sys.AssetID) }
		if scope == "" {
			systemNode()
		}
		for _, d := range sys.Deployments {
			if d.VerifiedAt == "" || d.Application == "" {
				continue
			}
			s, imported := sources[[2]string{d.Cluster, d.Instance}]
			if scope != "" && !imported {
				continue
			}
			// Without an imported cluster identity, the exact deployment target is the only known scope.
			domain := []string{"target", d.Cluster, d.Instance}
			if imported {
				domain = []string{"cluster", s.AssetID}
			}
			app := g.node("application", d.Application, d.Application, d.Namespace, append(domain, d.Namespace, d.Application)...)
			proof := topologyEvidence{Record: "business_deployment", Cluster: d.Cluster, Instance: d.Instance, Namespace: d.Namespace, VerifiedAt: d.VerifiedAt, Status: s.Status}
			g.edge(systemNode(), app, "contains", "business", proof)
			if imported {
				g.edge(app, topologyID("cluster", s.AssetID), "deployed_to", "deployment", proof)
			}
			if d.Database != "" {
				db := g.node("database", d.Database, d.Database, "", append(domain, d.Database)...)
				g.edge(app, db, "uses", "business", proof)
			}
		}
	}
	return g
}
