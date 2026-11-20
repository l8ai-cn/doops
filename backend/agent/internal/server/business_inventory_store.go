package server

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

type inventoryBusinessDeployment struct {
	Cluster     string `json:"cluster"`
	Instance    string `json:"instance"`
	Namespace   string `json:"namespace"`
	Application string `json:"application"`
	Database    string `json:"database"`
	VerifiedAt  string `json:"verified_at"`
}

type inventoryBusinessSystem struct {
	AssetID     string                        `json:"asset_id"`
	Name        string                        `json:"name"`
	Owner       string                        `json:"owner"`
	Active      bool                          `json:"active"`
	Revision    int64                         `json:"revision"`
	Deployments []inventoryBusinessDeployment `json:"deployments"`
}

// Scope edges before joining their parent. Only an administrator may see a
// system with no permitted deployment, including one awaiting registration.
func listBusinessInventory(tx *sql.Tx, auth TokenAuth) ([]inventoryBusinessSystem, error) {
	rows, err := tx.Query(inventoryGrants+`, visible_deployments AS (
 SELECT d.* FROM inventory_business_deployments d WHERE EXISTS (
 SELECT 1 FROM permitted p WHERE p.cluster IN ('*',d.cluster) AND p.instance IN ('*',d.instance)))
 SELECT s.asset_id,s.name,s.owner,s.active,s.revision,COALESCE(d.cluster,''),COALESCE(d.instance,''),
 COALESCE(d.namespace,''),COALESCE(d.application,''),COALESCE(d.database_name,''),COALESCE(d.verified_at,'')
 FROM inventory_business_systems s LEFT JOIN visible_deployments d ON s.asset_id=d.asset_id
 WHERE d.asset_id IS NOT NULL OR EXISTS (SELECT 1 FROM permitted WHERE is_admin=1)
 ORDER BY s.asset_id,d.cluster,d.instance,d.namespace,d.application,d.database_name`, auth.UserID, auth.TokenID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	systems := []inventoryBusinessSystem{}
	for rows.Next() {
		var s inventoryBusinessSystem
		var d inventoryBusinessDeployment
		if err := rows.Scan(&s.AssetID, &s.Name, &s.Owner, &s.Active, &s.Revision, &d.Cluster, &d.Instance, &d.Namespace, &d.Application, &d.Database, &d.VerifiedAt); err != nil {
			return nil, err
		}
		if len(systems) == 0 || systems[len(systems)-1].AssetID != s.AssetID {
			s.Deployments = []inventoryBusinessDeployment{}
			systems = append(systems, s)
		}
		if d.Cluster != "" {
			last := &systems[len(systems)-1]
			last.Deployments = append(last.Deployments, d)
		}
	}
	return systems, rows.Err()
}

type businessSystemInput struct {
	AssetID     string                        `json:"asset_id"`
	Name        string                        `json:"name"`
	Owner       string                        `json:"owner"`
	Active      *bool                         `json:"active"`
	Deployments []inventoryBusinessDeployment `json:"deployments"`
}

func (in businessSystemInput) validate() error {
	if !safeMachineID(in.AssetID) {
		return machineBadRequest("invalid_asset_id", "请填写有效的业务系统编号")
	}
	for _, v := range []string{in.Name, in.Owner} {
		if strings.TrimSpace(v) == "" || strings.TrimSpace(v) != v || len(v) > 256 || !inventoryMetadata(v) {
			return machineBadRequest("invalid_system_metadata", "系统名称和负责人须为有效的简短文本")
		}
	}
	if in.Active == nil {
		return machineBadRequest("invalid_active", "请指定业务系统是否在役")
	}
	for _, d := range in.Deployments {
		if !inventoryKeyPattern.MatchString(d.Cluster) || !inventoryKeyPattern.MatchString(d.Instance) || !inventoryMetadata(d.Cluster) || !inventoryMetadata(d.Instance) || !inventoryNamespacePattern.MatchString(d.Namespace) || !safeMachineID(d.Application) || (d.Database != "" && !safeMachineID(d.Database)) {
			return machineBadRequest("invalid_deployment", "请核对目标、命名空间、应用及数据库标识")
		}
		if d.VerifiedAt != "" {
			return machineBadRequest("invalid_verified_at", "核验时间由服务端记录")
		}
	}
	return nil
}

func saveBusinessSystem(tx *sql.Tx, auth TokenAuth, in businessSystemInput, expected *int64) (int64, error) {
	if err := in.validate(); err != nil {
		return 0, err
	}
	var revision int64
	err := tx.QueryRow(`SELECT revision FROM inventory_business_systems WHERE asset_id=?`, in.AssetID).Scan(&revision)
	switch {
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return 0, err
	case expected == nil && err == nil:
		return 0, machineInventoryProblem{409, "asset_id_exists", "该系统编号已登记"}
	case expected != nil && errors.Is(err, sql.ErrNoRows):
		return 0, machineInventoryProblem{404, "system_not_found", "业务系统不存在"}
	case expected != nil && revision != *expected:
		return 0, machineInventoryProblem{409, "revision_conflict", "系统已被修改，请重新读取后再编辑"}
	}
	// The enclosing transaction already holds SQLite's write reservation.
	revision++
	if expected == nil {
		_, err = tx.Exec(`INSERT INTO inventory_business_systems(asset_id,name,owner,active,revision) VALUES(?,?,?,?,?)`, in.AssetID, in.Name, in.Owner, *in.Active, revision)
	} else {
		_, err = tx.Exec(`UPDATE inventory_business_systems SET name=?,owner=?,active=?,revision=? WHERE asset_id=?`, in.Name, in.Owner, *in.Active, revision, in.AssetID)
	}
	if err != nil {
		return 0, err
	}
	if in.Deployments != nil {
		if err := replaceBusinessDeployments(tx, in.AssetID, auth.UserID, in.Deployments); err != nil {
			return 0, err
		}
	}
	return revision, nil
}

func replaceBusinessDeployments(tx *sql.Tx, id, actor string, deployments []inventoryBusinessDeployment) error {
	rows, err := tx.Query(`SELECT cluster,instance,namespace,application,database_name FROM inventory_business_deployments WHERE asset_id=?`, id)
	if err != nil {
		return err
	}
	existing := map[inventoryBusinessDeployment]bool{}
	for rows.Next() {
		var d inventoryBusinessDeployment
		if err := rows.Scan(&d.Cluster, &d.Instance, &d.Namespace, &d.Application, &d.Database); err != nil {
			rows.Close()
			return err
		}
		existing[d] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	desired := map[inventoryBusinessDeployment]bool{}
	verifiedAt := formatTime(time.Now().UTC())
	for _, d := range deployments {
		if desired[d] {
			continue
		}
		desired[d] = true
		if existing[d] {
			continue
		}
		// The registered target is checked in the write transaction. The operator
		// attests the namespace, application and database relationship, not an Agent probe.
		res, err := tx.Exec(`INSERT INTO inventory_business_deployments(asset_id,cluster,instance,namespace,application,database_name,verified_at,verified_by)
   SELECT ?,cluster,instance,?,?,?,?,? FROM agent_status WHERE cluster=? AND instance=? AND archived_at=''`, id, d.Namespace, d.Application, d.Database, verifiedAt, actor, d.Cluster, d.Instance)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return machineBadRequest("target_not_registered", "部署来源必须是已登记且未归档的 Agent 目标")
		}
	}
	for d := range existing {
		if !desired[d] {
			if _, err := tx.Exec(`DELETE FROM inventory_business_deployments WHERE asset_id=? AND cluster=? AND instance=? AND namespace=? AND application=? AND database_name=?`, id, d.Cluster, d.Instance, d.Namespace, d.Application, d.Database); err != nil {
				return err
			}
		}
	}
	return nil
}
