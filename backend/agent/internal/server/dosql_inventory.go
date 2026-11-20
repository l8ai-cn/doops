package server

import (
	"sort"
	"strings"
)

// Database inventory is a view of discovered objects. Existing IDs remain addressable
// by the execution resolver even when a controller and its members share a row.
func databaseInventoryRows(objects []map[string]interface{}, cluster, instance string) []doSQLDatabaseCandidate {
	type resource struct {
		row        doSQLDatabaseCandidate
		meta, spec map[string]interface{}
	}
	var resources []*resource
	controllers := map[string]*resource{}
	services := map[string]*resource{}
	pods := map[string][]*resource{}
	for _, obj := range objects {
		meta, _ := obj["metadata"].(map[string]interface{})
		spec, _ := obj["spec"].(map[string]interface{})
		row, identified := databaseDiscoveryCandidate(obj, cluster, instance)
		r := &resource{row, meta, spec}
		ns := stringValue(meta["namespace"])
		name := stringValue(meta["name"])
		switch stringValue(obj["kind"]) {
		case "Pod":
			pods[ns] = append(pods[ns], r)
		case "Service":
			services[ns+"/"+name] = r
			r.row.Host = name + "." + ns + ".svc"
		case "StatefulSet":
			if uid := stringValue(meta["uid"]); identified && uid != "" {
				controllers[uid] = r
			}
		}
		if identified {
			resources = append(resources, r)
		}
	}
	grouped := map[*resource]*resource{}
	for ns, members := range pods {
		for _, p := range members {
			refs, _ := p.meta["ownerReferences"].([]interface{})
			for _, raw := range refs {
				ref, _ := raw.(map[string]interface{})
				owner := controllers[stringValue(ref["uid"])]
				if ref["controller"] == true && ref["kind"] == "StatefulSet" && owner != nil && owner.row.Namespace == ns && owner.row.Service == ref["name"] && p.row.Engine == owner.row.Engine && (p.row.DatabaseName == "" || p.row.DatabaseName == owner.row.DatabaseName) {
					grouped[p] = owner
					break
				}
			}
		}
	}
	for _, owner := range controllers {
		svc := services[owner.row.Namespace+"/"+stringValue(owner.spec["serviceName"])]
		if svc == nil {
			continue
		}
		selector, _ := svc.spec["selector"].(map[string]interface{})
		if len(selector) == 0 {
			continue
		}
		matched := false
		exclusive := true
		for _, pod := range pods[owner.row.Namespace] {
			labels, _ := pod.meta["labels"].(map[string]interface{})
			if !databaseSelectorMatches(selector, labels) {
				continue
			}
			if grouped[pod] != owner {
				exclusive = false
				break
			}
			matched = true
		}
		if !exclusive || !matched || (svc.row.Engine != "" && svc.row.Engine != owner.row.Engine) {
			continue
		}
		owner.row.Host = svc.row.Host
		owner.row.Port = databaseServicePort(svc.spec, owner.row.Engine)
		grouped[svc] = owner
	}
	for member, owner := range grouped {
		if id := member.row.DatabaseAssetID; id != "" && id != owner.row.DatabaseAssetID {
			owner.row.RelatedAssetIDs = append(owner.row.RelatedAssetIDs, id)
		}
	}
	rows := make([]doSQLDatabaseCandidate, 0, len(resources))
	for _, r := range resources {
		if grouped[r] != nil {
			continue
		}
		if r.row.Host == "" {
			r.row.Port = 0
		} else if r.row.Source == "Service" {
			r.row.Port = databaseServicePort(r.spec, r.row.Engine)
		}
		sort.Strings(r.row.RelatedAssetIDs)
		rows = append(rows, r.row)
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.DatabaseAssetID != b.DatabaseAssetID {
			return a.DatabaseAssetID < b.DatabaseAssetID
		}
		return a.Source < b.Source
	})
	return rows
}

func databaseSelectorMatches(selector, labels map[string]interface{}) bool {
	for key, value := range selector {
		if labels[key] != value {
			return false
		}
	}
	return true
}

func databaseServicePort(spec map[string]interface{}, engine string) int {
	ports, _ := spec["ports"].([]interface{})
	var tcp []int
	var named []int
	for _, raw := range ports {
		port, _ := raw.(map[string]interface{})
		protocol := stringValue(port["protocol"])
		number, _ := port["port"].(float64)
		if (protocol != "" && protocol != "TCP") || number <= 0 {
			continue
		}
		tcp = append(tcp, int(number))
		if strings.Contains(strings.ToLower(stringValue(port["name"])), engine) {
			named = append(named, int(number))
		}
	}
	if len(named) == 1 {
		return named[0]
	}
	if len(tcp) == 1 {
		return tcp[0]
	}
	return 0
}
