package server

import (
	"database/sql"
	"encoding/json"
	"sort"
	"strings"
)

// Join each snapshot to a currently managed namespace and its exact authorized source.
// Aggregate after filtering so a second observer cannot reveal a revoked source's evidence.
func readRuntimeGraph(tx *sql.Tx, auth TokenAuth, scope string, g *topologyBuilder, systems []inventoryBusinessSystem) ([]runtimeScope, error) {
	rows, err := tx.Query(inventoryGrants+`SELECT s.asset_id,s.cluster,s.instance,n.name,COALESCE(a.status,'offline'),COALESCE(x.revision,''),COALESCE(x.observed_at,''),COALESCE(x.objects,'{}') FROM cluster_inventory_sources s JOIN cluster_inventory_namespaces n USING(cluster,instance) LEFT JOIN agent_status a USING(cluster,instance) LEFT JOIN asset_runtime_snapshots x ON (x.cluster,x.instance,x.namespace,x.asset_id)=(s.cluster,s.instance,n.name,s.asset_id) WHERE (?='' OR s.asset_id=?) AND n.verified_at<>'' AND EXISTS(SELECT 1 FROM permitted p WHERE p.cluster IN ('*',s.cluster) AND p.instance IN ('*',s.instance)) ORDER BY s.asset_id,s.cluster,s.instance,n.name`, auth.UserID, auth.TokenID, scope, scope)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type key struct{ cluster, instance, ns string }
	apps := map[key]map[string]bool{}
	for _, sys := range systems {
		for _, d := range sys.Deployments {
			if d.VerifiedAt != "" {
				k := key{d.Cluster, d.Instance, d.Namespace}
				if apps[k] == nil {
					apps[k] = map[string]bool{}
				}
				apps[k][d.Application] = true
			}
		}
	}
	result := []runtimeScope{}
	for rows.Next() {
		var s runtimeScope
		var raw string
		if err := rows.Scan(&s.AssetID, &s.Cluster, &s.Instance, &s.Namespace, &s.Status, &s.Revision, &s.At, &raw); err != nil {
			return nil, err
		}
		objects := map[string]runtimeObject{}
		if err := json.Unmarshal([]byte(raw), &objects); err != nil {
			return nil, err
		}
		s.Workloads = []runtimeObject{}
		s.Applications = []string{}
		s.Bindings = []runtimeBinding{}
		visibleApps := apps[key{s.Cluster, s.Instance, s.Namespace}]
		for name := range visibleApps {
			s.Applications = append(s.Applications, name)
		}
		sort.Strings(s.Applications)
		s.addObjects(g, objects)
		if s.Revision != "" {
			bindings, err := tx.Query(`SELECT application,workload_uid,verified_at FROM asset_runtime_bindings WHERE cluster=? AND instance=? AND namespace=? ORDER BY application,workload_uid`, s.Cluster, s.Instance, s.Namespace)
			if err != nil {
				return nil, err
			}
			for bindings.Next() {
				var b runtimeBinding
				if err := bindings.Scan(&b.Application, &b.UID, &b.At); err != nil {
					bindings.Close()
					return nil, err
				}
				o, ok := objects[b.UID]
				if !ok || o.Kind == "Pod" || !visibleApps[b.Application] {
					continue
				}
				s.Bindings = append(s.Bindings, b)
				proof := s.proof("runtime_binding", b.At)
				g.edge(topologyID("application", "cluster", s.AssetID, s.Namespace, b.Application), s.objectID(o), "bound_workload", "deployment", proof)
			}
			err = bindings.Err()
			bindings.Close()
			if err != nil {
				return nil, err
			}
		}
		result = append(result, s)
	}
	return result, rows.Err()
}
func (s runtimeScope) proof(record, at string) topologyEvidence {
	return topologyEvidence{Record: record, Cluster: s.Cluster, Instance: s.Instance, Namespace: s.Namespace, VerifiedAt: at, Status: s.Status}
}
func (s runtimeScope) objectID(o runtimeObject) string {
	kind := "workload"
	if o.Kind == "Pod" {
		kind = "pod"
	}
	return topologyID(kind, s.AssetID, s.Namespace, o.UID)
}
func (s *runtimeScope) addObjects(g *topologyBuilder, objects map[string]runtimeObject) {
	for _, o := range objects {
		id := s.objectID(o)
		kind := "workload"
		if o.Kind == "Pod" {
			kind = "pod"
		} else {
			s.Workloads = append(s.Workloads, runtimeObject{UID: o.UID, Kind: o.Kind, Name: o.Name})
		}
		g.nodes[id] = topologyNode{id, kind, o.Kind + "/" + o.Name, o.UID, s.Namespace}
		if o.Kind == "Pod" && o.Node != "" {
			node := g.node("node", o.Node, o.Node, "", s.Cluster, s.Instance, o.Node)
			g.edge(id, node, "scheduled_on", "deployment", s.proof("pod_node_name", s.At))
		}
		fields := strings.Split(o.Owner, "/")
		if len(fields) != 5 {
			continue
		}
		parent, found := objects[fields[4]]
		if !found || parent.Name != fields[3] || parent.Kind != fields[2] {
			continue
		}
		if controllerPair(fields[0]+"/"+fields[1], parent.Kind, o.Kind) {
			g.edge(s.objectID(parent), id, "controls", "deployment", s.proof("controller_uid", s.At))
		}
	}
	sort.Slice(s.Workloads, func(i, j int) bool {
		a, b := s.Workloads[i], s.Workloads[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.UID < b.UID
	})
}
func controllerPair(version, parent, child string) bool {
	switch version {
	case "apps/v1":
		return (parent == "Deployment" && child == "ReplicaSet") || ((parent == "StatefulSet" || parent == "DaemonSet" || parent == "ReplicaSet") && child == "Pod")
	case "batch/v1":
		return (parent == "CronJob" && child == "Job") || (parent == "Job" && child == "Pod")
	}
	return false
}
