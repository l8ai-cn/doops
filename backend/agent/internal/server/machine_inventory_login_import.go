package server

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode"
)

// Each (asset_id, username) identifies one credential. A repeated CSV row
// counts neither as another pending write nor as another stored credential.
func (s *GatewayStore) planMachineLoginBatch(tx *sql.Tx, text string, rows []inventoryCSVRow) (inventoryBatch, string, error) {
	batchID, err := s.machineLoginMAC("machine-inventory-batch-v1", text)
	if err != nil {
		return inventoryBatch{}, "", machineLoginImportError(err)
	}
	first := make(map[[2]string]int)
	conflicts := make(map[[2]string]bool)
	for i, row := range rows {
		key := [2]string{row.machine.AssetID, row.username}
		if j, ok := first[key]; !ok {
			first[key] = i
		} else if rows[j].password != row.password {
			conflicts[key] = true
		}
	}
	var expected, existing int
	var plan strings.Builder
	fmt.Fprintln(&plan, batchID)
	for i := range rows {
		row := &rows[i]
		key := [2]string{row.machine.AssetID, row.username}
		if row.referenceOnly && row.result.Status != "error" {
			_, err := getInventoryMachine(tx, row.machine.AssetID)
			switch {
			case errors.Is(err, sql.ErrNoRows):
				row.rejectLogin("machine_not_registered", "asset_id", "credentials without machine metadata require a registered asset_id")
			case err != nil:
				return inventoryBatch{}, "", err
			default:
				row.result.Status, row.result.Code, row.result.Message = "duplicate", "already_registered", "asset already registered"
			}
		}
		switch {
		case row.result.Status == "error":
			row.result.CredentialStatus = "error"
		case row.username == "" || row.username != strings.TrimSpace(row.username) || strings.IndexFunc(row.username, unicode.IsControl) >= 0:
			row.rejectLogin("invalid_machine_username", "username", "username is required and must have no control characters or surrounding whitespace")
		case row.password == "":
			row.rejectLogin("machine_login_required", "password", "password is required")
		case conflicts[key]:
			row.rejectLogin("machine_login_conflict", "password", "different passwords for the same account in this batch")
		case first[key] != i:
			prior := rows[first[key]].result
			if prior.Status == "error" {
				row.rejectLogin(prior.Code, prior.Field, prior.Message)
			} else {
				row.result.CredentialStatus = "duplicate"
			}
		default:
			stored, err := s.planMachineLogin(tx, key[0], key[1], row.password)
			switch {
			case errors.Is(err, ErrMachineLoginConflict):
				row.rejectLogin("machine_login_conflict", "password", "a different password is already stored for this account")
			case err != nil:
				return inventoryBatch{}, "", machineLoginImportError(err)
			case stored:
				row.result.CredentialStatus = "existing"
				existing++
			default:
				row.result.CredentialStatus = "pending"
				expected++
			}
		}
		r := row.result
		fmt.Fprintf(&plan, "%d\t%s\t%s\t%s\t%s\n", r.Line, r.Status, r.Code, r.Field, r.CredentialStatus)
	}
	token, err := s.machineLoginMAC("machine-inventory-preview-v1", plan.String())
	if err != nil {
		return inventoryBatch{}, "", machineLoginImportError(err)
	}
	return inventoryBatch{
		inventoryBatchContent: inventoryBatchContent{BatchID: batchID}, Scope: machineLoginScope,
		CredentialResult: inventoryCredentialResult{Status: "preview", Expected: &expected, Existing: &existing},
	}, token, nil
}

func (row *inventoryCSVRow) rejectLogin(code, field, message string) {
	row.result.Status, row.result.CredentialStatus = "error", "error"
	row.result.Code, row.result.Field, row.result.Message = code, field, message
}

func machineLoginImportError(err error) error {
	if errors.Is(err, ErrSecretKeyUnavailable) {
		return machineInventoryProblem{http.StatusServiceUnavailable, "machine_login_key_unavailable", "machine login encryption key is unavailable"}
	}
	return machineInventoryProblem{http.StatusServiceUnavailable, "machine_login_unavailable", "machine login vault unavailable"}
}

func (s *GatewayStore) importMachineLoginBatch(tx *sql.Tx, rows []inventoryCSVRow, result *inventoryCredentialResult, auth TokenAuth) error {
	invalid := machineInventoryProblem{http.StatusUnprocessableEntity, "machine_login_batch_rejected", "the batch contains row errors; correct them and preview again"}
	for _, row := range rows {
		if row.result.Status == "error" {
			return invalid
		}
	}
	if err := importInventoryRows(tx, rows); err != nil {
		return err
	}
	for i := range rows {
		row := &rows[i]
		if row.result.Status == "error" { // A machine insert was rejected by the database.
			return invalid
		}
		if row.result.CredentialStatus == "pending" {
			if err := s.storeMachineLogin(tx, row.machine.AssetID, row.username, row.password, auth); err != nil {
				return machineLoginImportError(err)
			}
			// Count only credentials that can be read back in this transaction.
			stored, err := s.planMachineLogin(tx, row.machine.AssetID, row.username, row.password)
			if err != nil || !stored {
				return machineLoginImportError(err)
			}
			row.result.CredentialStatus = "saved"
			result.Saved++
		}
	}
	result.Status = "saved"
	return nil
}

func hasInventoryCredentialRows(rows []inventoryRowResult) bool {
	for _, row := range rows {
		if row.CredentialStatus != "" {
			return true
		}
	}
	return false
}

func validMachineLoginReceipt(result inventoryCredentialResult, rows []inventoryRowResult) bool {
	if result.Status != "saved" || result.Expected == nil || result.Existing == nil {
		return false
	}
	var saved, existing int
	for _, row := range rows {
		if row.Status != "imported" && row.Status != "duplicate" {
			return false
		}
		switch row.CredentialStatus {
		case "saved":
			saved++
		case "existing":
			existing++
		case "duplicate":
		default:
			return false
		}
	}
	return saved+existing > 0 && saved == result.Saved && saved == *result.Expected && existing == *result.Existing
}
