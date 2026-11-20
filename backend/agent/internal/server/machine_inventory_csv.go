package server

import (
	"crypto/sha256"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

const (
	machineLoginScope     = "machine_and_credentials"
	machineMetadataScope  = "machine_metadata"
	inventoryCSVBytes     = 1 << 20
	inventoryCSVRows      = 2000
	inventoryHostRequired = "host_asset_id must reference a registered or valid in-batch physical machine"
)

var errInventoryInsertSkipped = errors.New("inventory insert did not create a row")

var errMachineLoginCredentials = machineInventoryProblem{http.StatusUnprocessableEntity, "machine_login_credentials_not_supported", "machine_metadata accepts metadata only; use machine_and_credentials with username and password columns"}

// Match entire header names only. Other unknown columns keep the CSV header error.
var inventoryLoginHeader = regexp.MustCompile(`(?i)^(?:(?:(?:ssh|server|host|machine)[ _-]?)?(?:(?:login|initial)[ _-]?)?(?:(?:user|account)(?:[ _-]?name)?|password|passwd|pwd)|login(?:[ _-]?name)?|` +
	`(?:服务器|主机|机器)?(?:初始|登录|登陆)?(?:[账帐][户号]|用户名|用户|登[录陆]名|密码|口令)(?:[/／](?:密码|口令))?)$`)

func inventoryBatchID(text string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(text))) }

var inventoryCSVColumns = []string{"asset_id", "name", "kind", "address", "cpu_count", "memory_gib", "disk_gib", "host_asset_id"}

type inventoryRowResult struct {
	Line             int    `json:"line"`
	AssetID          string `json:"asset_id"`
	Status           string `json:"status"`
	Message          string `json:"message"`
	Code             string `json:"code"`
	Field            string `json:"field,omitempty"`
	CredentialStatus string `json:"credential_status,omitempty"`
}
type inventoryCSVRow struct {
	machine            machineInventoryInput
	result             inventoryRowResult
	username, password string
	referenceOnly      bool
}
type inventoryBatchContent struct {
	PreviewToken string               `json:"preview_token,omitempty"`
	BatchID      string               `json:"batch_id"`
	AttemptID    int64                `json:"attempt_id,omitempty"`
	Rows         []inventoryRowResult `json:"rows"`
}

type inventoryCredentialResult struct {
	Status   string `json:"status"`
	Saved    int    `json:"saved"` // Number of credentials written by this batch.
	Expected *int   `json:"expected,omitempty"`
	Existing *int   `json:"existing,omitempty"`
}

type inventoryBatch struct {
	inventoryBatchContent
	Scope            string                    `json:"scope"`
	CredentialResult inventoryCredentialResult `json:"credential_result"`
}

func metadataBatch(content inventoryBatchContent) inventoryBatch {
	return inventoryBatch{
		inventoryBatchContent: content,
		Scope:                 machineMetadataScope,
		CredentialResult:      inventoryCredentialResult{Status: "not_imported", Saved: 0},
	}
}

func decodeInventoryReceipt(receipt, batchID string, attemptID int64) (inventoryBatch, error) {
	var stored struct {
		inventoryBatchContent
		Scope            json.RawMessage `json:"scope"`
		CredentialResult json.RawMessage `json:"credential_result"`
	}
	if err := json.Unmarshal([]byte(receipt), &stored); err != nil {
		return inventoryBatch{}, err
	}
	if stored.BatchID != batchID || stored.AttemptID != attemptID || len(stored.Rows) == 0 {
		return inventoryBatch{}, errors.New("invalid stored inventory receipt")
	}
	batch := metadataBatch(stored.inventoryBatchContent)
	if stored.Scope == nil && stored.CredentialResult == nil && !hasInventoryCredentialRows(stored.Rows) {
		return batch, nil // Older receipts predate both fields. Their writer only inserted machines.
	}
	var scope string
	result := inventoryCredentialResult{Saved: -1}
	if json.Unmarshal(stored.Scope, &scope) == nil && json.Unmarshal(stored.CredentialResult, &result) == nil {
		switch scope {
		case machineLoginScope:
			if validMachineLoginReceipt(result, stored.Rows) {
				batch.Scope, batch.CredentialResult = scope, result
				return batch, nil
			}
		case machineMetadataScope:
			if result == batch.CredentialResult && !hasInventoryCredentialRows(stored.Rows) {
				return batch, nil
			}
		}
	}
	return inventoryBatch{}, errors.New("invalid stored machine import scope or credential result")
}

func parseInventoryCSV(text string, withLogins bool) ([]inventoryCSVRow, error) {
	if len(text) > inventoryCSVBytes {
		return nil, machineInventoryProblem{http.StatusRequestEntityTooLarge, "csv_too_large", "csv exceeds 1 MiB"}
	}
	reader := csv.NewReader(strings.NewReader(strings.TrimPrefix(text, "\uFEFF")))
	header, err := reader.Read()
	if err != nil {
		return nil, machineBadRequest("invalid_csv_header", "csv requires a valid header")
	}
	for _, col := range header {
		if !withLogins && inventoryLoginHeader.MatchString(strings.TrimSpace(col)) {
			return nil, errMachineLoginCredentials
		}
	}
	seen := map[string]bool{}
	for _, col := range header {
		loginColumn := withLogins && (col == "username" || col == "password")
		if (!slices.Contains(inventoryCSVColumns, col) && !loginColumn) || seen[col] {
			return nil, machineBadRequest("invalid_csv_header", "csv header contains an unknown or repeated column")
		}
		seen[col] = true
	}
	required := inventoryCSVColumns[:3]
	if withLogins {
		required = []string{"asset_id"}
		if !seen["username"] || !seen["password"] {
			return nil, machineInventoryProblem{http.StatusUnprocessableEntity, "machine_login_required", "machine_and_credentials requires username and password columns"}
		}
	}
	for _, col := range required {
		if !seen[col] {
			return nil, machineBadRequest("invalid_csv_header", "csv header requires column "+col)
		}
	}
	passwordColumn := slices.Index(header, "password")
	var sourceLines []string
	var rows []inventoryCSVRow
	for {
		record, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			var parse *csv.ParseError
			if errors.As(err, &parse) {
				return nil, machineBadRequest("invalid_csv", fmt.Sprintf("invalid csv at line %d; check quoting and column count", parse.Line))
			}
			return nil, err
		}
		if len(rows) == inventoryCSVRows {
			return nil, machineInventoryProblem{http.StatusRequestEntityTooLarge, "too_many_csv_rows", "csv exceeds 2000 data rows"}
		}
		line, _ := reader.FieldPos(0)
		row := inventoryCSVFields(header, record, withLogins)
		row.result.Line = line
		// encoding/csv normalizes CRLF inside quoted fields. A password must
		// retain the original bytes even when the CSV uses mixed line endings.
		if strings.Contains(row.password, "\n") {
			if sourceLines == nil {
				sourceLines = strings.Split(text, "\n")
			}
			start, _ := reader.FieldPos(passwordColumn)
			parts := strings.Split(row.password, "\n")
			for i := 0; i < len(parts)-1; i++ {
				if strings.HasSuffix(sourceLines[start-1+i], "\r") {
					parts[i] += "\r"
				}
			}
			row.password = strings.Join(parts, "\n")
		}
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		return nil, machineBadRequest("invalid_csv", "csv requires at least one data row")
	}
	return rows, nil
}

func inventoryCSVFields(header, record []string, withLogins bool) inventoryCSVRow {
	row := inventoryCSVRow{referenceOnly: withLogins}
	m := &row.machine
	var issue *machineInventoryProblem
	for i, col := range header {
		v := record[i]
		if col != "asset_id" && col != "username" && col != "password" && v != "" {
			row.referenceOnly = false
		}
		switch col {
		case "username":
			row.username = v
		case "password":
			row.password = v
		case "asset_id":
			m.AssetID = v
		case "name":
			m.Name = v
		case "kind":
			m.Kind = v
		case "address":
			m.Address = v
		case "host_asset_id":
			m.HostAssetID = v
		case "cpu_count":
			if v != "" {
				n, err := strconv.ParseInt(v, 10, 64)
				if err != nil {
					issue = &machineInventoryProblem{400, "invalid_cpu_count", "cpu_count must be a positive integer"}
				}
				m.CPUCount = &n
			}
		case "memory_gib", "disk_gib":
			if v != "" {
				n, err := strconv.ParseFloat(v, 64)
				if err != nil {
					issue = &machineInventoryProblem{400, "invalid_" + col, col + " must be a finite positive number"}
				}
				if col == "memory_gib" {
					m.MemoryGiB = &n
				} else {
					m.DiskGiB = &n
				}
			}
		}
	}
	if safeMachineID(m.AssetID) {
		row.result.AssetID = m.AssetID
	}
	if issue == nil && (!row.referenceOnly || row.result.AssetID == "") {
		issue = m.validate()
	}
	if issue != nil {
		row.result.Status, row.result.Code, row.result.Message = "error", issue.code, issue.message
	}
	return row
}

func inventoryEqualNumber[T int64 | float64](a, b *T) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

// Compare only columns in the CSV vocabulary. Reimporting an asset never
// changes the binding established separately by its administrator.
func inventoryDifference(a, b machineInventoryInput) string {
	switch {
	case a.Name != b.Name:
		return "name"
	case a.Kind != b.Kind:
		return "kind"
	case a.Address != b.Address:
		return "address"
	case !inventoryEqualNumber(a.CPUCount, b.CPUCount):
		return "cpu_count"
	case !inventoryEqualNumber(a.MemoryGiB, b.MemoryGiB):
		return "memory_gib"
	case !inventoryEqualNumber(a.DiskGiB, b.DiskGiB):
		return "disk_gib"
	case a.HostAssetID != b.HostAssetID:
		return "host_asset_id"
	}
	return ""
}

func classifyInventoryRows(tx *sql.Tx, rows []inventoryCSVRow) error {
	first := map[string]int{}
	conflicts := map[string]string{}
	for i, row := range rows {
		id := row.result.AssetID
		if id == "" || row.referenceOnly {
			continue
		}
		j, ok := first[id]
		if !ok {
			first[id] = i
			continue
		}
		field := inventoryDifference(rows[j].machine, row.machine)
		if field != "" {
			conflicts[id] = field
		} else if (row.result.Status == "error") != (rows[j].result.Status == "error") {
			conflicts[id] = "asset_id"
		}
	}
	// A valid physical host in this CSV can satisfy a VM on any earlier line.
	availableHosts := map[string]bool{}
	for _, kind := range []string{"physical", "vm"} {
		for i := range rows {
			row := &rows[i]
			m := row.machine
			result := &row.result
			if field := conflicts[result.AssetID]; !row.referenceOnly && field != "" {
				result.Status, result.Code, result.Field, result.Message = "error", "batch_asset_conflict", field, "conflicting "+field+" for this asset_id; resolve all rows for this identifier"
			}
			if m.Kind != kind || result.Status == "error" {
				continue
			}
			if j := first[m.AssetID]; j != i {
				if rows[j].result.Status == "error" {
					result.Status, result.Code, result.Field, result.Message = "error", rows[j].result.Code, rows[j].result.Field, rows[j].result.Message
				} else {
					result.Status, result.Code, result.Message = "duplicate", "duplicate_row", "machine metadata matches an earlier row"
				}
				continue
			}
			existing, err := getInventoryMachine(tx, m.AssetID)
			switch {
			case err == nil:
				if field := inventoryDifference(existing.machineInventoryInput, m); field != "" {
					result.Status, result.Code, result.Field, result.Message = "error", "registered_asset_conflict", field, field+" conflicts with the registered asset; edit it using its current revision"
				} else {
					result.Status, result.Code, result.Message = "duplicate", "already_registered", "asset already registered"
				}
				continue
			case !errors.Is(err, sql.ErrNoRows):
				return err
			}
			if m.HostAssetID != "" && !availableHosts[m.HostAssetID] {
				host, err := getInventoryMachine(tx, m.HostAssetID)
				if errors.Is(err, sql.ErrNoRows) || (err == nil && host.Kind != "physical") {
					result.Status, result.Code, result.Message = "error", "invalid_host_asset_id", inventoryHostRequired
					continue
				}
				if err != nil {
					return err
				}
			}
			result.Status, result.Code, result.Message = "new", "ready_to_import", "ready to import"
			if kind == "physical" && result.Status != "error" {
				availableHosts[m.AssetID] = true
			}
		}
	}
	return nil
}

// All inputs here are fixed-format identifiers or server-generated classifications.
// Exclude presentation text so rewording a message does not change a preview.
func inventoryPreviewToken(batchID string, rows []inventoryCSVRow) string {
	digest := sha256.New()
	fmt.Fprintln(digest, "machine-inventory-preview-v1", batchID)
	for _, row := range rows {
		r := row.result
		fmt.Fprintf(digest, "%d\t%s\t%s\t%s\t%s\n", r.Line, r.AssetID, r.Status, r.Code, r.Field)
	}
	return fmt.Sprintf("%x", digest.Sum(nil))
}

func importInventoryRows(tx *sql.Tx, rows []inventoryCSVRow) error {
	failed := map[string]inventoryRowResult{}
	for _, kind := range []string{"physical", "vm"} {
		for i := range rows {
			row := &rows[i]
			m, result := row.machine, &row.result
			if m.Kind != kind {
				continue
			}
			if previous, ok := failed[m.AssetID]; ok && result.Status == "duplicate" {
				result.Status, result.Code, result.Message = "error", previous.Code, previous.Message
			}
			if result.Status != "new" {
				continue
			}
			if _, ok := failed[m.HostAssetID]; ok {
				result.Status, result.Code, result.Message = "error", "invalid_host_asset_id", inventoryHostRequired
			} else {
				// A constraint can reject one machine; roll back that row's side effects.
				if _, err := tx.Exec(`SAVEPOINT inventory_row`); err != nil {
					return err
				}
				err := insertInventoryMachine(tx, m)
				if err != nil {
					var dbErr *sqlite.Error
					if !errors.Is(err, errInventoryInsertSkipped) && (!errors.As(err, &dbErr) || dbErr.Code()&0xff != sqlite3.SQLITE_CONSTRAINT) {
						return err
					}
					if _, err := tx.Exec(`ROLLBACK TO inventory_row`); err != nil {
						return err
					}
					result.Status, result.Code, result.Message = "error", "insert_rejected", "asset_id could not be inserted; check registration constraints"
				} else {
					result.Status, result.Code, result.Message = "imported", "asset_imported", "asset registered"
				}
				if _, err := tx.Exec(`RELEASE inventory_row`); err != nil {
					return err
				}
			}
			if result.Status == "error" {
				failed[m.AssetID] = *result
			}
		}
	}
	return nil
}

func (s *GatewayStore) runInventoryBatch(tx *sql.Tx, req machineInventoryRequest, auditID int64, auth TokenAuth) (inventoryBatch, error) {
	withLogins := req.Scope != nil && *req.Scope == machineLoginScope
	rows, err := parseInventoryCSV(*req.CSV, withLogins)
	if err != nil {
		return inventoryBatch{}, err
	}
	confirmation := req.PreviewToken
	if req.Action == "import" && (confirmation == nil || *confirmation == "") {
		return inventoryBatch{}, machineBadRequest("preview_token_required", "preview the CSV before confirming the import")
	}
	if req.Action == "preview" && confirmation != nil {
		return inventoryBatch{}, machineBadRequest("invalid_inventory_request", "preview_token is only accepted on import")
	}
	if err := classifyInventoryRows(tx, rows); err != nil {
		return inventoryBatch{}, err
	}
	var batch inventoryBatch
	var token string
	if withLogins {
		batch, token, err = s.planMachineLoginBatch(tx, *req.CSV, rows)
		if err != nil {
			return inventoryBatch{}, err
		}
	} else {
		batch = metadataBatch(inventoryBatchContent{BatchID: inventoryBatchID(*req.CSV)})
		token = inventoryPreviewToken(batch.BatchID, rows)
	}
	if confirmation == nil {
		batch.PreviewToken = token
	} else {
		if *confirmation != token {
			return inventoryBatch{}, machineInventoryProblem{http.StatusConflict, "preview_stale", "preview changed; preview the CSV again before confirming"}
		}
		if withLogins {
			if err := s.importMachineLoginBatch(tx, rows, &batch.CredentialResult, auth); err != nil {
				return inventoryBatch{}, err
			}
		} else if err := importInventoryRows(tx, rows); err != nil {
			return inventoryBatch{}, err
		}
	}
	batch.Rows = make([]inventoryRowResult, len(rows))
	for i, row := range rows {
		batch.Rows[i] = row.result
	}
	if confirmation != nil {
		batch.AttemptID = auditID
		receipt, err := json.Marshal(batch)
		if err != nil {
			return inventoryBatch{}, err
		}
		insert, err := tx.Exec(`INSERT INTO inventory_import_attempts(attempt_id,batch_id,receipt) VALUES(?,?,?)`, auditID, batch.BatchID, string(receipt))
		if err != nil {
			return inventoryBatch{}, err
		}
		if n, err := insert.RowsAffected(); err != nil || n != 1 {
			return inventoryBatch{}, errors.New("inventory receipt was not inserted")
		}
	}
	return batch, nil
}
