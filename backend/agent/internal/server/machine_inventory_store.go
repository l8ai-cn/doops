package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"net"
	"net/http"
	"regexp"
	"strings"
	"unicode"
)

type machineInventoryInput struct {
	AssetID     string   `json:"asset_id"`
	Name        string   `json:"name"`
	Kind        string   `json:"kind"`
	Address     string   `json:"address"`
	CPUCount    *int64   `json:"cpu_count"`
	MemoryGiB   *float64 `json:"memory_gib"`
	DiskGiB     *float64 `json:"disk_gib"`
	HostAssetID string   `json:"host_asset_id,omitempty"`
	Cluster     string   `json:"cluster"`
	Instance    string   `json:"instance"`
}

type inventoryMachine struct {
	machineInventoryInput
	Revision      int64           `json:"revision"`
	TargetStatus  string          `json:"target_status"`
	LastSeen      string          `json:"last_seen,omitempty"`
	LoginAccounts json.RawMessage `json:"login_accounts,omitempty"`
}

var machineAssetID = regexp.MustCompile(`^[[:alnum:]][[:alnum:]_.-]{0,127}$`)
var machineSecretMarker = regexp.MustCompile(`(?i)\b(?:dgw_user_|dga_aen_|sk-[a-zA-Z0-9_-]{16,}|(?:password|token|api[_-]?key|secret|client-key-data|client-certificate-data)\s*[:=])`)

func inventoryMetadata(value string) bool {
	return !machineSecretMarker.MatchString(value) && !credentialScalarContainsInlineMaterial(value) && strings.IndexFunc(value, unicode.IsControl) < 0
}

func safeMachineID(id string) bool { return machineAssetID.MatchString(id) && inventoryMetadata(id) }

func (m machineInventoryInput) validate() *machineInventoryProblem {
	switch {
	case !safeMachineID(m.AssetID):
		return &machineInventoryProblem{http.StatusBadRequest, "invalid_asset_id", "asset_id must be a 1–128 character identifier using letters, digits, dots, underscores or hyphens"}
	case strings.TrimSpace(m.Name) == "" || len(m.Name) > 256 || !inventoryMetadata(m.Name):
		return &machineInventoryProblem{http.StatusBadRequest, "invalid_name", "name must be a short display name without credential material"}
	case m.Kind != "physical" && m.Kind != "vm":
		return &machineInventoryProblem{http.StatusBadRequest, "invalid_kind", "kind must be physical or vm"}
	case m.Address != "" && (len(m.Address) > 254 || !inventoryMetadata(m.Address) || (net.ParseIP(m.Address) == nil && !managedHelmHostnamePattern.MatchString(strings.TrimSuffix(m.Address, ".")))):
		return &machineInventoryProblem{http.StatusBadRequest, "invalid_address", "address must be an IP address or hostname"}
	case m.CPUCount != nil && *m.CPUCount <= 0:
		return &machineInventoryProblem{http.StatusBadRequest, "invalid_cpu_count", "cpu_count must be a positive integer"}
	case !validInventoryCapacity(m.MemoryGiB):
		return &machineInventoryProblem{http.StatusBadRequest, "invalid_memory_gib", "memory_gib must be a finite positive number"}
	case !validInventoryCapacity(m.DiskGiB):
		return &machineInventoryProblem{http.StatusBadRequest, "invalid_disk_gib", "disk_gib must be a finite positive number"}
	case m.HostAssetID != "" && (m.Kind != "vm" || m.HostAssetID == m.AssetID || !safeMachineID(m.HostAssetID)):
		return &machineInventoryProblem{http.StatusBadRequest, "invalid_host_asset_id", "host_asset_id must reference another physical machine and is only allowed for a VM"}
	case (m.Cluster == "") != (m.Instance == ""):
		return &machineInventoryProblem{http.StatusBadRequest, "invalid_target", "cluster and instance must be provided together"}
	case m.Cluster == "*" || m.Instance == "*" || !inventoryMetadata(m.Cluster) || !inventoryMetadata(m.Instance):
		return &machineInventoryProblem{http.StatusBadRequest, "invalid_target", "cluster and instance must identify a registered target"}
	}
	return nil
}

func validInventoryCapacity(n *float64) bool { return n == nil || (*n > 0 && !math.IsInf(*n, 0)) }

// A single read snapshot controls both membership and the host reference. An
// inventory record can outlive the gateway's offline agent-status retention.
const machineInventoryColumns = `m.asset_id,m.name,m.kind,m.address,m.cpu_count,m.memory_gib,m.disk_gib,
 COALESCE(host.asset_id,''),m.cluster,m.instance,m.revision,
 CASE WHEN m.cluster='' THEN 'unbound' ELSE COALESCE(a.status,'offline') END,COALESCE(a.last_seen,'')`
const machineStatusJoin = ` LEFT JOIN agent_status a ON a.cluster=m.cluster AND a.instance=m.instance `

func scanInventoryMachine(row interface{ Scan(...any) error }) (m inventoryMachine, err error) {
	err = row.Scan(&m.AssetID, &m.Name, &m.Kind, &m.Address, &m.CPUCount, &m.MemoryGiB, &m.DiskGiB, &m.HostAssetID, &m.Cluster, &m.Instance, &m.Revision, &m.TargetStatus, &m.LastSeen, (*[]byte)(&m.LoginAccounts))
	return
}

func listInventoryMachines(tx *sql.Tx, auth TokenAuth) ([]inventoryMachine, error) {
	rows, err := tx.Query(inventoryGrants+`, visible AS (
 SELECT m.* FROM inventory_machines m WHERE EXISTS (SELECT 1 FROM permitted p
 WHERE p.is_admin=1 OR (m.cluster<>'' AND m.instance<>'' AND p.cluster IN ('*',m.cluster) AND p.instance IN ('*',m.instance))))
 SELECT `+machineInventoryColumns+`, CASE WHEN (SELECT is_admin FROM actor)=1 THEN `+machineLoginAccountsJSON+` END FROM visible m LEFT JOIN visible host ON host.asset_id=m.host_asset_id `+machineStatusJoin+` ORDER BY m.asset_id`, auth.UserID, auth.TokenID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []inventoryMachine{}
	for rows.Next() {
		m, err := scanInventoryMachine(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

const machineByIDFrom = ` FROM inventory_machines m LEFT JOIN inventory_machines host ON host.asset_id=m.host_asset_id ` + machineStatusJoin + ` WHERE m.asset_id=?`

func getInventoryMachine(tx *sql.Tx, id string) (inventoryMachine, error) {
	return scanInventoryMachine(tx.QueryRow(`SELECT `+machineInventoryColumns+`, NULL`+machineByIDFrom, id))
}

func insertInventoryMachine(tx *sql.Tx, m machineInventoryInput) error {
	result, err := tx.Exec(`INSERT INTO inventory_machines(asset_id,name,kind,address,cpu_count,memory_gib,disk_gib,host_asset_id,cluster,instance)
 VALUES(?,?,?,?,?,?,?,NULLIF(?,''),?,?)`, m.AssetID, m.Name, m.Kind, m.Address, m.CPUCount, m.MemoryGiB, m.DiskGiB, m.HostAssetID, m.Cluster, m.Instance)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n != 1 {
		return errInventoryInsertSkipped
	}
	return err
}

func saveInventoryMachine(tx *sql.Tx, m machineInventoryInput, revision *int64) (inventoryMachine, error) {
	var empty inventoryMachine
	if issue := m.validate(); issue != nil {
		return empty, *issue
	}
	existing, err := getInventoryMachine(tx, m.AssetID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return empty, err
	}
	switch {
	case revision == nil && err == nil:
		return empty, machineInventoryProblem{http.StatusConflict, "asset_id_exists", "asset_id already exists"}
	case revision != nil && err != nil:
		return empty, machineInventoryProblem{http.StatusNotFound, "machine_not_found", "machine not found"}
	case revision != nil && existing.Revision != *revision:
		return empty, machineInventoryProblem{http.StatusConflict, "revision_conflict", "revision changed; reload the machine"}
	}
	if m.HostAssetID != "" {
		host, err := getInventoryMachine(tx, m.HostAssetID)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && host.Kind != "physical") {
			return empty, machineBadRequest("invalid_host_asset_id", inventoryHostRequired)
		}
		if err != nil {
			return empty, err
		}
	}
	if m.Cluster != "" {
		var found bool
		err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM agent_status WHERE cluster=? AND instance=? AND archived_at='')`, m.Cluster, m.Instance).Scan(&found)
		if err != nil {
			return empty, err
		}
		if !found {
			return empty, machineBadRequest("target_not_registered", "cluster and instance must match a registered, unarchived target")
		}
	}
	if revision == nil {
		err = insertInventoryMachine(tx, m)
	} else {
		if existing.Kind == "physical" && m.Kind == "vm" {
			var hostsVM bool
			if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM inventory_machines WHERE host_asset_id=?)`, m.AssetID).Scan(&hostsVM); err != nil {
				return empty, err
			}
			if hostsVM {
				return empty, machineInventoryProblem{http.StatusConflict, "host_in_use", "kind cannot change while VMs reference this host"}
			}
		}
		var res sql.Result
		res, err = tx.Exec(`UPDATE inventory_machines SET name=?,kind=?,address=?,cpu_count=?,memory_gib=?,disk_gib=?,host_asset_id=NULLIF(?,''),cluster=?,instance=?,revision=revision+1 WHERE asset_id=? AND revision=?`, m.Name, m.Kind, m.Address, m.CPUCount, m.MemoryGiB, m.DiskGiB, m.HostAssetID, m.Cluster, m.Instance, m.AssetID, *revision)
		if err == nil {
			var changed int64
			changed, err = res.RowsAffected()
			if err == nil && changed != 1 {
				return empty, machineInventoryProblem{http.StatusConflict, "revision_conflict", "revision changed; reload the machine"}
			}
		}
	}
	if err != nil {
		return empty, err
	}
	// The mutation was authorized by requireInventoryAdmin in this transaction.
	return scanInventoryMachine(tx.QueryRow(`SELECT `+machineInventoryColumns+`, `+machineLoginAccountsJSON+machineByIDFrom, m.AssetID))
}
