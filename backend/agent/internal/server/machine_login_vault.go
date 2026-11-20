package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// This scope cannot be created or managed through the generic credential API.
const machineVaultScope CredentialScope = "machine_login"

var ErrMachineLoginConflict = errors.New("a different password is already stored for this account")

const machineLoginAccountsJSON = `(SELECT json_group_array(json_object('username', a.username, 'credential_id', a.credential_id) ORDER BY a.username)
 FROM inventory_machine_login_accounts a WHERE a.asset_id=m.asset_id)`

func (s *GatewayStore) machineLoginMAC(purpose, text string) (string, error) {
	key, err := s.secretKey()
	if err != nil {
		return "", err
	}
	derive := hmac.New(sha256.New, key)
	derive.Write([]byte("doops/machine-login/v1/" + purpose))
	mac := hmac.New(sha256.New, derive.Sum(nil))
	mac.Write([]byte(text))
	return hex.EncodeToString(mac.Sum(nil)), nil
}

// Check the installation key even when planning a previously unseen account.
// Only a write transaction may initialize the marker on an empty vault.
func (s *GatewayStore) checkMachineLoginKey(tx *sql.Tx, initialize bool) error {
	mac, err := s.machineLoginMAC("vault-key", "v1")
	if err != nil {
		return err
	}
	var stored string
	err = tx.QueryRow(`SELECT key_mac FROM inventory_machine_login_key WHERE singleton=1`).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		var hasAccounts bool
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM inventory_machine_login_accounts)`).Scan(&hasAccounts); err != nil {
			return err
		}
		if hasAccounts {
			return errors.New("machine login vault key marker missing")
		}
		if initialize {
			_, err := tx.Exec(`INSERT INTO inventory_machine_login_key(singleton,key_mac) VALUES(1,?)`, mac)
			return err
		}
		return nil
	}
	if err != nil {
		return err
	}
	if !hmac.Equal([]byte(stored), []byte(mac)) {
		return ErrSecretKeyUnavailable
	}
	return nil
}

// Bind the row's identity as well as the managed resource/version to its ciphertext.
func machineLoginAAD(credID, versionID, digest, assetID, username string) []byte {
	return append(credentialPayloadAAD(credID, versionID, CredentialTypeOpaque, digest), "\x00"+assetID+"\x00"+username...)
}

func (s *GatewayStore) planMachineLogin(tx *sql.Tx, assetID, username, password string) (bool, error) {
	if err := s.checkMachineLoginKey(tx, false); err != nil {
		return false, err
	}
	return s.matchMachineLogin(tx, assetID, username, password)
}

func (s *GatewayStore) matchMachineLogin(tx *sql.Tx, assetID, username, password string) (bool, error) {
	// Preserve a broken mapping as a scan error instead of treating it as a new account.
	var credID, versionID, ciphertext, digest string
	err := tx.QueryRow(`SELECT c.id,v.id,v.payload_ciphertext,v.payload_digest
 FROM inventory_machine_login_accounts a
 LEFT JOIN credentials c ON c.id=a.credential_id AND c.scope=? AND c.type='opaque' AND c.state='active'
 LEFT JOIN credential_versions v ON v.id=a.version_id AND v.credential_id=c.id AND v.state='active'
 WHERE a.asset_id=? AND a.username=?`, machineVaultScope, assetID, username).Scan(&credID, &versionID, &ciphertext, &digest)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	plain, err := s.decryptSecretWithAAD(ciphertext, machineLoginAAD(credID, versionID, digest, assetID, username))
	if err != nil {
		return false, err
	}
	var payload struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal([]byte(plain), &payload); err != nil || payload.Data["username"] != username || payload.Data["password"] == "" {
		return false, ErrCredentialPayloadInvalid
	}
	if !hmac.Equal([]byte(payload.Data["password"]), []byte(password)) {
		return false, ErrMachineLoginConflict
	}
	return true, nil
}

// The inventory importer holds the writer reservation and has authorized the admin.
// Its transaction also contains the machine insert and the import receipt.
func (s *GatewayStore) storeMachineLogin(tx *sql.Tx, assetID, username, password string, auth TokenAuth) error {
	if !safeMachineID(assetID) || username == "" || username != strings.TrimSpace(username) || password == "" || auth.UserID == "" {
		return ErrCredentialPayloadInvalid
	}
	if err := s.checkMachineLoginKey(tx, true); err != nil {
		return err
	}
	if stored, err := s.matchMachineLogin(tx, assetID, username, password); err != nil || stored {
		return err
	}
	credID, versionID := "cred_"+randomHex(12), "credver_"+randomHex(12)
	payload, err := json.Marshal(map[string]map[string]string{"data": {"username": username, "password": password}})
	if err != nil {
		return err
	}
	digest, err := s.machineLoginMAC("payload/"+credID, string(payload))
	if err != nil {
		return err
	}
	ciphertext, err := s.encryptSecretWithAAD(string(payload), machineLoginAAD(credID, versionID, digest, assetID, username))
	if err != nil {
		return err
	}
	now := formatTime(time.Now().UTC())
	if _, err := tx.Exec(`INSERT INTO credentials(id,name,scope,owner_id,type,state,created_by,created_at,updated_at)
 VALUES(?,?,?,?, 'opaque','active',?,?,?)`, credID, credID, machineVaultScope, auth.UserID, auth.UserID, now, now); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO credential_versions(id,credential_id,payload_ciphertext,payload_digest,state,created_by,created_at,promoted_at)
 VALUES(?,?,?,?,'active',?,?,?)`, versionID, credID, ciphertext, digest, auth.UserID, now, now); err != nil {
		return err
	}
	// Foreign-key enforcement is connection-specific in SQLite. Require the asset
	// in this transaction even on gateway connections with foreign_keys disabled.
	res, err := tx.Exec(`INSERT INTO inventory_machine_login_accounts(asset_id,username,credential_id,version_id)
 SELECT asset_id,?,?,? FROM inventory_machines WHERE asset_id=?`, username, credID, versionID, assetID)
	if err != nil {
		return err
	}
	count, err := res.RowsAffected()
	if err == nil && count != 1 {
		return errors.New("machine login asset missing")
	}
	return err
}
