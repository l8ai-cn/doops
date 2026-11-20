package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
)

type nodeLinkKey struct {
	AssetID   string `json:"asset_id"`
	Cluster   string `json:"cluster"`
	Instance  string `json:"instance"`
	NodeName  string `json:"node_name"`
	MachineID string `json:"machine_id"`
}

type inventoryNodeLink struct {
	nodeLinkKey
	Source string `json:"source"`
}

type nodeLinkRequest struct {
	Action string `json:"action"`
	nodeLinkKey
}

var inventoryNodeName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)

// HandleAssetNodeLinks serves manual inventory mappings on /v1/assets/node-links.
func (h *GatewayHub) HandleAssetNodeLinks(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	auth, err := h.authenticateUser(r)
	if err != nil {
		h.writeUserAuthError(w, r, err)
		return
	}
	var result any
	switch r.Method {
	case http.MethodGet:
		var links []inventoryNodeLink
		links, err = h.store.listNodeLinks(r.Context(), auth)
		result = map[string]any{"links": links}
	case http.MethodPost:
		var req nodeLinkRequest
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
		dec.DisallowUnknownFields()
		valid := dec.Decode(&req) == nil && dec.Decode(new(any)) == io.EOF && (req.Action == "bind" || req.Action == "unbind") &&
			safeMachineID(req.MachineID) && inventoryKeyPattern.MatchString(req.AssetID) && inventoryKeyPattern.MatchString(req.Cluster) &&
			inventoryKeyPattern.MatchString(req.Instance) && len(req.NodeName) <= 253 && inventoryNodeName.MatchString(req.NodeName) &&
			inventoryMetadata(req.AssetID) && inventoryMetadata(req.Cluster) && inventoryMetadata(req.Instance) && inventoryMetadata(req.NodeName)
		err = h.store.changeNodeLink(r.Context(), auth, req, valid)
		result = map[string]any{"action": req.Action, "link": inventoryNodeLink{req.nodeLinkKey, "manual"}}
	default:
		w.Header().Set("Allow", "GET, POST")
		err = machineInventoryProblem{405, "method_not_allowed", "method not allowed"}
	}
	if err != nil {
		problem := nodeLinkProblem(err)
		writeDoopsError(w, problem.status, "", doopsErrorResponse{Error: problem.code, Code: problem.code, Message: problem.message})
		return
	}
	writeJSONHTTP(w, result)
}

func nodeLinkProblem(err error) machineInventoryProblem {
	var p machineInventoryProblem
	if errors.As(err, &p) {
		return p
	}
	return machineInventoryProblem{503, "node_links_unavailable", "node link inventory unavailable"}
}

func (s *GatewayStore) listNodeLinks(ctx context.Context, auth TokenAuth) ([]inventoryNodeLink, error) {
	return readInventoryNodeLinks(ctx, s.db, auth)
}

func readInventoryNodeLinks(ctx context.Context, db inventoryReader, auth TokenAuth) ([]inventoryNodeLink, error) {
	// Match UserCanUseTarget independently for both endpoints. Only an admin
	// can see a machine without a target, even with an existing */* grant.
	rows, err := db.QueryContext(ctx, inventoryGrants+`
 SELECT s.asset_id,l.cluster,l.instance,l.node_name,l.machine_id FROM inventory_node_links l
 JOIN cluster_inventory_sources s USING(cluster,instance) JOIN inventory_machines m ON m.asset_id=l.machine_id
 WHERE EXISTS(SELECT 1 FROM permitted p WHERE p.cluster IN ('*',s.cluster) AND p.instance IN ('*',s.instance))
 AND EXISTS(SELECT 1 FROM permitted p WHERE p.is_admin=1 OR
   (m.cluster<>'' AND m.instance<>'' AND p.cluster IN ('*',m.cluster) AND p.instance IN ('*',m.instance)))
 ORDER BY s.asset_id,l.cluster,l.instance,l.node_name`, auth.UserID, auth.TokenID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	links := []inventoryNodeLink{}
	for rows.Next() {
		link := inventoryNodeLink{Source: "manual"}
		if err := rows.Scan(&link.AssetID, &link.Cluster, &link.Instance, &link.NodeName, &link.MachineID); err != nil {
			return nil, err
		}
		links = append(links, link)
	}
	return links, rows.Err()
}

func (s *GatewayStore) changeNodeLink(ctx context.Context, auth TokenAuth, req nodeLinkRequest, valid bool) error {
	event := AuditEvent{UserID: auth.UserID, TokenID: auth.TokenID, Session: "node-links-" + randomHex(10), Action: "inventory:node:request"}
	if valid {
		event.Action = GatewayAction("inventory:node:" + req.Action)
		event.Cluster, event.Instance = req.Cluster, req.Instance
		event.CommandSummary = fmt.Sprintf("asset_id=%s machine_id=%s node_name=%s source=manual", req.AssetID, req.MachineID, req.NodeName)
	}
	id, err := s.StartAudit(event)
	if err != nil {
		return machineInventoryProblem{503, "audit_unavailable", "audit unavailable"}
	}
	if err := s.applyNodeLink(ctx, auth, req, valid, id, event.Session); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return machineInventoryProblem{503, "audit_unavailable", "audit unavailable"}
		}
		p := nodeLinkProblem(err)
		status := "error"
		if p.status == http.StatusForbidden {
			status = "forbidden"
		}
		if err := s.FinishAudit(id, AuditFinish{Status: status, Error: p.message}); err != nil {
			return machineInventoryProblem{503, "audit_unavailable", "audit unavailable"}
		}
		return p
	}
	return nil
}

func (s *GatewayStore) applyNodeLink(ctx context.Context, auth TokenAuth, req nodeLinkRequest, valid bool, auditID int64, session string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Reserve the writer before reading the current role and the exact parents.
	if err := tx.QueryRow(`UPDATE audit_events SET status='executing' WHERE id=? AND session=? AND status='running' RETURNING id`, auditID, session).Scan(&auditID); err != nil {
		return err
	}
	if err := requireInventoryAdmin(tx, auth); err != nil {
		return err
	}
	if !valid {
		return machineBadRequest("invalid_node_link", "expected bind or unbind, exact asset_id, cluster, instance, machine_id and a valid node_name")
	}
	var parents bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM cluster_inventory_sources WHERE cluster=? AND instance=? AND asset_id=?) AND EXISTS(SELECT 1 FROM inventory_machines WHERE asset_id=?)`, req.Cluster, req.Instance, req.AssetID, req.MachineID).Scan(&parents); err != nil {
		return err
	}
	if !parents {
		return machineInventoryProblem{404, "asset_not_found", "machine or imported cluster source not found"}
	}
	query := `INSERT INTO inventory_node_links(cluster,instance,node_name,machine_id) VALUES(?,?,?,?) ON CONFLICT(cluster,instance,node_name) DO NOTHING`
	if req.Action == "unbind" {
		query = `DELETE FROM inventory_node_links WHERE cluster=? AND instance=? AND node_name=? AND machine_id=?`
	}
	res, err := tx.Exec(query, req.Cluster, req.Instance, req.NodeName, req.MachineID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		if req.Action == "bind" {
			return machineInventoryProblem{409, "node_link_conflict", "node name already bound on this source"}
		}
		return machineInventoryProblem{404, "node_link_not_found", "exact node link not found"}
	}
	if err := finishAuditWith(tx, auditID, AuditFinish{Status: "success"}); err != nil {
		return machineInventoryProblem{503, "audit_unavailable", "audit unavailable"}
	}
	return tx.Commit()
}
