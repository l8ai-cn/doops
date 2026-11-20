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
	"strings"
	"time"
	"unicode"
)

type inventoryRequest struct {
	Action    string                 `json:"action"`
	AssetID   string                 `json:"asset_id"`
	Name      string                 `json:"name"`
	Cluster   string                 `json:"cluster"`
	Instance  string                 `json:"instance"`
	Namespace inventoryNamespaceName `json:"namespace"`
}

// Only omission requests a cluster-wide list. Invalid, null or blank values
// cannot turn a scoped probe into a broader one.
type inventoryNamespaceName string

var inventoryNamespacePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
var inventoryKeyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,252}$`)

func (n *inventoryNamespaceName) UnmarshalJSON(raw []byte) error {
	var name string
	if json.Unmarshal(raw, &name) != nil || !inventoryNamespacePattern.MatchString(name) {
		return errors.New("invalid namespace")
	}
	*n = inventoryNamespaceName(name)
	return nil
}

type inventoryNamespace struct {
	Name                string `json:"name"`
	UID                 string `json:"uid,omitempty"`
	NamespaceObservedAt string `json:"namespace_observed_at,omitempty"`
	PodsListedAt        string `json:"pods_listed_at,omitempty"`
	Scope               string `json:"scope"`
	VerifiedAt          string `json:"verified_at"`
	VerifiedBy          string `json:"verified_by"`
}

type inventoryCluster struct {
	AssetID    string               `json:"asset_id"`
	Name       string               `json:"name"`
	Cluster    string               `json:"cluster"`
	Instance   string               `json:"instance"`
	Status     string               `json:"status"`
	Scope      string               `json:"scope"`
	VerifiedAt string               `json:"verified_at"`
	VerifiedBy string               `json:"verified_by"`
	Namespaces []inventoryNamespace `json:"namespaces"`
}

type inventoryFailure struct {
	status int
	code   string
}

func (e inventoryFailure) Error() string { return e.code }

func inventoryHTTPError(w http.ResponseWriter, err error) {
	var failure inventoryFailure
	if !errors.As(err, &failure) {
		failure = inventoryFailure{503, "cluster_inventory_unavailable"}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(failure.status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": failure.code, "code": failure.code, "namespaces": []inventoryNamespace{}})
}

func (h *GatewayHub) HandleClusterInventory(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	auth, err := h.authenticateUser(r)
	if err != nil {
		h.writeUserAuthError(w, r, err)
		return
	}
	switch r.Method {
	case http.MethodGet:
		clusters, err := h.store.listInventoryClusters(r.Context(), auth)
		if err != nil {
			inventoryHTTPError(w, err)
			return
		}
		writeJSONHTTP(w, map[string]any{"clusters": clusters})
	case http.MethodPost:
		var req inventoryRequest
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&req) != nil || decoder.Decode(&struct{}{}) != io.EOF || !req.valid() {
			inventoryHTTPError(w, inventoryFailure{400, "invalid_inventory_request"})
			return
		}
		row, err := h.observeClusterInventory(r.Context(), auth, req)
		if err != nil {
			inventoryHTTPError(w, err)
			return
		}
		writeJSONHTTP(w, row)
	default:
		w.Header().Set("Allow", "GET, POST")
		inventoryHTTPError(w, inventoryFailure{405, "method_not_allowed"})
	}
}

func (req inventoryRequest) valid() bool {
	return (req.Action == "discover" || req.Action == "import") &&
		inventoryKeyPattern.MatchString(req.Cluster) && inventoryKeyPattern.MatchString(req.Instance) &&
		(req.AssetID == "" || inventoryKeyPattern.MatchString(req.AssetID)) &&
		len(req.Name) <= 256 && strings.TrimSpace(req.Name) == req.Name && strings.IndexFunc(req.Name, unicode.IsControl) < 0 &&
		(req.Action != "import" || (req.AssetID != "" && req.Name != ""))
}

func (h *GatewayHub) observeClusterInventory(ctx context.Context, auth TokenAuth, req inventoryRequest) (inventoryCluster, error) {
	scope := "list_namespaces"
	if req.Namespace != "" {
		scope = "list_pods"
	}
	row := inventoryCluster{AssetID: req.AssetID, Name: req.Name, Cluster: req.Cluster, Instance: req.Instance,
		Scope: scope, VerifiedBy: auth.UserID, Status: "online"}
	session := "inventory-" + randomHex(10)
	auditID, err := h.store.StartAudit(AuditEvent{UserID: auth.UserID, TokenID: auth.TokenID, Cluster: req.Cluster, Instance: req.Instance,
		Action: GatewayAction("cluster_inventory:" + req.Action), Session: session,
		CommandSummary: fmt.Sprintf("asset_id=%s scope=%s namespace=%s", req.AssetID, scope, req.Namespace)})
	if err != nil {
		return row, err
	}
	err = h.verifyInventoryObservation(ctx, auth, req, &row, session, auditID)
	if err != nil {
		failure := inventoryFailure{503, "cluster_inventory_unavailable"}
		var classified inventoryFailure
		if errors.As(err, &classified) {
			failure = classified
		}
		if finishErr := h.store.FinishAudit(auditID, AuditFinish{Status: "error", Error: failure.code}); finishErr != nil {
			return row, finishErr
		}
	}
	return row, err
}

func (h *GatewayHub) verifyInventoryObservation(ctx context.Context, auth TokenAuth, req inventoryRequest, row *inventoryCluster, session string, auditID int64) error {
	if err := authorizeInventory(ctx, h.store.db, auth, req); err != nil {
		return err
	}
	agent := h.getAgent(req.Cluster, req.Instance)
	if agent == nil || agent.Cluster != req.Cluster || agent.Instance != req.Instance {
		return inventoryFailure{503, "target_offline"}
	}
	readCtx, cancel := context.WithTimeout(contextWithInternalAuditAuth(ctx, auth), 30*time.Second)
	defer cancel()
	namespaces, err := h.readInventoryNamespaces(readCtx, req, session)
	if err != nil {
		return err
	}
	// Check the same token, grant, role and Agent generation in the transaction
	// that publishes the result. Never hold the DB connection during the RPC.
	tx, err := h.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := authorizeInventory(ctx, tx, auth, req); err != nil {
		return err
	}
	var online bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM agent_status WHERE cluster=? AND instance=? AND status='online' AND generation=?)`, req.Cluster, req.Instance, agent.Generation).Scan(&online); err != nil {
		return err
	}
	if !online {
		return inventoryFailure{503, "target_connection_changed"}
	}
	row.VerifiedAt = formatTime(time.Now())
	for i := range namespaces {
		namespaces[i].Scope, namespaces[i].VerifiedAt, namespaces[i].VerifiedBy = row.Scope, row.VerifiedAt, row.VerifiedBy
		if row.Scope == "list_namespaces" {
			namespaces[i].NamespaceObservedAt = row.VerifiedAt
		} else {
			namespaces[i].PodsListedAt = row.VerifiedAt
		}
	}
	row.Namespaces = namespaces
	if req.Action == "import" {
		if err := saveInventoryObservation(tx, *row); err != nil {
			return err
		}
	}
	if err := finishAuditWith(tx, auditID, AuditFinish{Status: "success"}); err != nil {
		return err
	}
	return tx.Commit()
}

// Resolve the same target authority as UserCanUseTarget within the caller's
// database snapshot. A platform admin resolves to the wildcard target grant.
const inventoryGrants = `WITH actor AS (
 SELECT u.id, u.is_admin FROM users u JOIN tokens t ON t.user_id=u.id
 WHERE u.id=? AND t.id=? AND t.kind='user' AND u.disabled=0 AND t.revoked_at=''
 AND (t.expires_at='' OR julianday(t.expires_at)>julianday('now'))
), permitted AS (
 SELECT '*' AS cluster, '*' AS instance, is_admin FROM actor WHERE is_admin=1
 UNION ALL SELECT g.cluster, g.instance, a.is_admin FROM actor a JOIN grants g ON g.user_id=a.id WHERE a.is_admin=0
) `

func authorizeInventory(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, auth TokenAuth, req inventoryRequest) error {
	var allowed bool
	err := q.QueryRowContext(ctx, inventoryGrants+`SELECT EXISTS(SELECT 1 FROM permitted
 WHERE cluster IN ('*', ?) AND instance IN ('*', ?) AND (? <> 'import' OR is_admin=1))`, auth.UserID, auth.TokenID, req.Cluster, req.Instance, req.Action).Scan(&allowed)
	if err != nil {
		return err
	}
	if !allowed {
		return inventoryFailure{403, "inventory_forbidden"}
	}
	return nil
}
