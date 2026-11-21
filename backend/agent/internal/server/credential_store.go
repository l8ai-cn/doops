package server

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"
)

var (
	ErrSecretKeyUnavailable       = errors.New("DOOPS_GATEWAY_SECRET_KEY is required")
	ErrCredentialNotFound         = errors.New("credential not found")
	ErrCredentialVersionNotFound  = errors.New("credential version not found")
	ErrCredentialRevoked          = errors.New("credential is revoked")
	ErrCredentialGrantDenied      = errors.New("credential grant denied")
	ErrCredentialForbidden        = errors.New("credential operation forbidden")
	ErrCredentialPayloadInvalid   = errors.New("credential payload is invalid")
	ErrCredentialBundleNotFound   = errors.New("credential bundle not found")
	ErrCredentialAmbiguous        = errors.New("credential reference is ambiguous")
	ErrCredentialAuditUnavailable = errors.New("credential audit unavailable")
	// ErrCredentialCleanupPending refuses a revocation's terminal state while a
	// materialization record may still own something on a target.
	ErrCredentialCleanupPending = errors.New("credential cleanup pending")

	// Specific grant denials keep errors.Is(err, ErrCredentialGrantDenied) and its
	// 403 status, but report which binding precondition failed.
	ErrCredentialBindingTargetMismatch = fmt.Errorf("%w: credential is bound to another target", ErrCredentialGrantDenied)
	ErrCredentialBindingScopeInvalid   = fmt.Errorf("%w: target binding scope is invalid", ErrCredentialGrantDenied)
	ErrDoSQLDatabaseBindingMissing     = fmt.Errorf("%w: target has no database binding", ErrCredentialGrantDenied)
	ErrDoSQLCredentialNotBound         = fmt.Errorf("%w: named credential has no active database binding on this target", ErrCredentialGrantDenied)
)

// credentialGrantDenialCategory must run before the generic grant-denied case.
func credentialGrantDenialCategory(err error) (string, bool) {
	switch {
	case errors.Is(err, ErrCredentialBindingTargetMismatch):
		return "credential_binding_target_mismatch", true
	case errors.Is(err, ErrCredentialBindingScopeInvalid):
		return "credential_binding_scope_invalid", true
	case errors.Is(err, ErrDoSQLDatabaseBindingMissing):
		return "dosql_database_binding_missing", true
	case errors.Is(err, ErrDoSQLCredentialNotBound):
		return "dosql_credential_not_bound", true
	case errors.Is(err, ErrCredentialGrantDenied):
		return "credential_grant_denied", true
	}
	return "", false
}

type CredentialScope string

const (
	CredentialScopePersonal CredentialScope = "personal"
	CredentialScopePlatform CredentialScope = "platform"
)

type CredentialType string

const (
	CredentialTypeRegistry       CredentialType = "registry"
	CredentialTypeTLS            CredentialType = "tls"
	CredentialTypeOpaque         CredentialType = "opaque"
	CredentialTypeHelmRepository CredentialType = "helmRepository"
	CredentialTypeGitToken       CredentialType = "gitToken"
	// CredentialTypeModelProvider is the doagent model provider configuration a
	// target Agent receives from the Gateway instead of a mounted settings file.
	CredentialTypeModelProvider CredentialType = "modelProvider"
)

type CredentialState string

const (
	CredentialStateActive   CredentialState = "active"
	CredentialStateRevoking CredentialState = "revoking"
	CredentialStateRevoked  CredentialState = "revoked"
)

type CredentialVersionState string

const (
	CredentialVersionStaged     CredentialVersionState = "staged"
	CredentialVersionActive     CredentialVersionState = "active"
	CredentialVersionSuperseded CredentialVersionState = "superseded"
	CredentialVersionRevoked    CredentialVersionState = "revoked"
)

type CredentialUse string

const (
	CredentialUseImagePull     CredentialUse = "imagePull"
	CredentialUseGitCheckout   CredentialUse = "gitCheckout"
	CredentialUseHelmPull      CredentialUse = "helmPull"
	CredentialUseTLS           CredentialUse = "tls"
	CredentialUseOpaqueSecret  CredentialUse = "opaqueSecret"
	CredentialUseDatabase      CredentialUse = "database"
	CredentialUseModelProvider CredentialUse = "modelProvider"
)

type CredentialResource struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Scope     CredentialScope `json:"scope"`
	OwnerID   string          `json:"owner_id,omitempty"`
	Type      CredentialType  `json:"type"`
	State     CredentialState `json:"state"`
	CreatedBy string          `json:"created_by"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
	RevokedAt *time.Time      `json:"revoked_at,omitempty"`
}

type CredentialVersion struct {
	ID            string                 `json:"id"`
	CredentialID  string                 `json:"credential_id"`
	PayloadDigest string                 `json:"payload_digest"`
	State         CredentialVersionState `json:"state"`
	CreatedBy     string                 `json:"created_by"`
	CreatedAt     time.Time              `json:"created_at"`
	PromotedAt    *time.Time             `json:"promoted_at,omitempty"`
	RevokedAt     *time.Time             `json:"revoked_at,omitempty"`
}

type CredentialGrant struct {
	ID           string               `json:"id"`
	CredentialID string               `json:"credential_id"`
	Scope        CredentialGrantScope `json:"scope"`
	State        CredentialState      `json:"state"`
	CreatedBy    string               `json:"created_by"`
	CreatedAt    time.Time            `json:"created_at"`
	RevokedAt    *time.Time           `json:"revoked_at,omitempty"`
}

type CredentialBundle struct {
	ID        string                 `json:"id"`
	Name      string                 `json:"name"`
	Scope     CredentialScope        `json:"scope"`
	OwnerID   string                 `json:"owner_id,omitempty"`
	State     CredentialState        `json:"state"`
	CreatedBy string                 `json:"created_by"`
	CreatedAt time.Time              `json:"created_at"`
	UpdatedAt time.Time              `json:"updated_at"`
	Items     []CredentialBundleItem `json:"items,omitempty"`
}

type CredentialBundleItem struct {
	CredentialID string `json:"credential_id"`
}

type CredentialBundleCreateRequest struct {
	Name      string
	Scope     CredentialScope
	OwnerID   string
	CreatedBy string
	Items     []CredentialBundleItem
}

type CredentialCreateRequest struct {
	Name      string
	Scope     CredentialScope
	Type      CredentialType
	OwnerID   string
	CreatedBy string
}

type CredentialListFilter struct {
	ActorID string
	Scope   CredentialScope
}

type CredentialListEntry struct {
	CredentialResource
	Grants []CredentialGrant `json:"grants"`
}

type CredentialVersionPutRequest struct {
	CredentialID string
	Payload      json.RawMessage
	CreatedBy    string
	Activate     bool
}

type CredentialGrantCreateRequest struct {
	CredentialID string
	Scope        CredentialGrantScope
	CreatedBy    string
}

type CredentialUseRequest struct {
	ActorID      string
	CredentialID string
	Credential   string
	Scope        CredentialGrantScope
}

type AuthorizedCredentialUse struct {
	Resource CredentialResource `json:"resource"`
	Version  CredentialVersion  `json:"version"`
	Grant    CredentialGrant    `json:"grant"`
}

type CredentialVerification struct {
	ID           string                       `json:"id"`
	CredentialID string                       `json:"credential_id"`
	VersionID    string                       `json:"version_id"`
	GrantID      string                       `json:"grant_id"`
	Use          CredentialUse                `json:"use"`
	Request      CredentialMaterializeRequest `json:"request"`
	Evidence     CredentialMaterialization    `json:"evidence"`
	Status       string                       `json:"status"`
	VerifiedAt   time.Time                    `json:"verified_at"`
}

func (s *GatewayStore) CreateCredential(req CredentialCreateRequest) (CredentialResource, error) {
	return s.createCredential(req, 0)
}

func (s *GatewayStore) CreateCredentialAudited(req CredentialCreateRequest, auditID int64) (CredentialResource, error) {
	return s.createCredential(req, auditID)
}

func (s *GatewayStore) createCredential(req CredentialCreateRequest, auditID int64) (CredentialResource, error) {
	req.Name = strings.TrimSpace(req.Name)
	req.OwnerID = strings.TrimSpace(req.OwnerID)
	req.CreatedBy = strings.TrimSpace(req.CreatedBy)
	if req.Name == "" || req.CreatedBy == "" {
		return CredentialResource{}, fmt.Errorf("credential name and creator are required")
	}
	if !validCredentialScope(req.Scope) || !validCredentialType(req.Type) {
		return CredentialResource{}, fmt.Errorf("invalid credential scope or type")
	}
	if req.Scope == CredentialScopePersonal && req.OwnerID == "" {
		return CredentialResource{}, fmt.Errorf("personal credential owner is required")
	}
	if req.Scope == CredentialScopePlatform {
		req.OwnerID = ""
	}
	now := time.Now().UTC()
	resource := CredentialResource{
		ID:        "cred_" + randomHex(12),
		Name:      req.Name,
		Scope:     req.Scope,
		OwnerID:   req.OwnerID,
		Type:      req.Type,
		State:     CredentialStateActive,
		CreatedBy: req.CreatedBy,
		CreatedAt: now,
		UpdatedAt: now,
	}
	tx, err := s.db.Begin()
	if err != nil {
		return CredentialResource{}, err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`INSERT INTO credentials
		(id, name, scope, owner_id, type, state, created_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		resource.ID, resource.Name, resource.Scope, resource.OwnerID, resource.Type, resource.State,
		resource.CreatedBy, formatTime(now), formatTime(now))
	if err != nil {
		return CredentialResource{}, err
	}
	if auditID != 0 {
		// The ID is only known now. Every other resource-level credential audit
		// carries it as credential and session, so the create is found the same way.
		if _, err := tx.Exec(`UPDATE audit_events SET credential_id = ?, session = ?
			WHERE id = ? AND status IN ('running', 'executing')`,
			resource.ID, resource.ID, auditID); err != nil {
			return CredentialResource{}, fmt.Errorf("%w: %v", ErrCredentialAuditUnavailable, err)
		}
		if err := finishAuditWith(tx, auditID, AuditFinish{Status: "success", EndedAt: now}); err != nil {
			return CredentialResource{}, fmt.Errorf("%w: %v", ErrCredentialAuditUnavailable, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return CredentialResource{}, err
	}
	return resource, nil
}

func (s *GatewayStore) ListCredentials(filter CredentialListFilter) ([]CredentialResource, error) {
	args := []any{machineVaultScope}
	where := []string{"scope <> ?"}
	if actorID := strings.TrimSpace(filter.ActorID); actorID != "" {
		where = append(where, "scope = ? AND owner_id = ?")
		args = append(args, CredentialScopePersonal, actorID)
	}
	if filter.Scope != "" {
		if !validCredentialScope(filter.Scope) {
			return nil, fmt.Errorf("invalid credential scope")
		}
		where = append(where, "scope = ?")
		args = append(args, filter.Scope)
	}
	rows, err := s.db.Query(`SELECT id, name, scope, owner_id, type, state, created_by, created_at, updated_at, revoked_at
		FROM credentials WHERE `+strings.Join(where, " AND ")+` ORDER BY created_at DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var resources []CredentialResource
	for rows.Next() {
		resource, err := scanCredentialResource(rows)
		if err != nil {
			return nil, err
		}
		resources = append(resources, resource)
	}
	return resources, rows.Err()
}

func (s *GatewayStore) ListCredentialAccess(
	actorID, cluster, instance string,
) ([]CredentialListEntry, error) {
	actorID = strings.TrimSpace(actorID)
	cluster = strings.TrimSpace(cluster)
	instance = strings.TrimSpace(instance)
	if actorID == "" || cluster == "" || instance == "" {
		return nil, fmt.Errorf("credential actor, cluster, and instance are required")
	}
	// Credential visibility follows the selected target permission. There is
	// no second per-user credential grant.
	args := []any{CredentialStateActive, CredentialStateActive, CredentialVersionActive}
	rows, err := s.db.Query(`SELECT
			c.id, c.name, c.scope, c.owner_id, c.type, c.state, c.created_by,
			c.created_at, c.updated_at, c.revoked_at,
			g.id, g.credential_id, g.grantee_id, g.scope_json, g.state,
			g.created_by, g.created_at, g.revoked_at
		FROM credentials c
		JOIN credential_grants g ON g.credential_id = c.id
		WHERE c.state = ? AND g.state = ?
		  AND EXISTS (
			SELECT 1 FROM credential_versions v
			WHERE v.credential_id = c.id AND v.state = ?
		  )
		ORDER BY c.created_at DESC, g.created_at DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	entries := make([]CredentialListEntry, 0)
	entryByID := make(map[string]int)
	for rows.Next() {
		var resource CredentialResource
		var grant CredentialGrant
		var resourceCreatedAt, resourceUpdatedAt, resourceRevokedAt string
		var grantScope, grantCreatedAt, grantRevokedAt string
		if err := rows.Scan(
			&resource.ID, &resource.Name, &resource.Scope, &resource.OwnerID, &resource.Type,
			&resource.State, &resource.CreatedBy, &resourceCreatedAt, &resourceUpdatedAt, &resourceRevokedAt,
			&grant.ID, &grant.CredentialID, new(string), &grantScope, &grant.State,
			&grant.CreatedBy, &grantCreatedAt, &grantRevokedAt,
		); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(grantScope), &grant.Scope); err != nil {
			return nil, err
		}
		grant.Scope = normalizeCredentialGrantScope(grant.Scope)
		if grant.Scope.Cluster != cluster || grant.Scope.Instance != instance {
			continue
		}
		resource.CreatedAt, _ = parseTime(resourceCreatedAt)
		resource.UpdatedAt, _ = parseTime(resourceUpdatedAt)
		if resourceRevokedAt != "" {
			parsed, _ := parseTime(resourceRevokedAt)
			resource.RevokedAt = &parsed
		}
		grant.CreatedAt, _ = parseTime(grantCreatedAt)
		if grantRevokedAt != "" {
			parsed, _ := parseTime(grantRevokedAt)
			grant.RevokedAt = &parsed
		}
		index, exists := entryByID[resource.ID]
		if !exists {
			index = len(entries)
			entryByID[resource.ID] = index
			entries = append(entries, CredentialListEntry{
				CredentialResource: resource,
				Grants:             []CredentialGrant{},
			})
		}
		entries[index].Grants = append(entries[index].Grants, grant)
	}
	return entries, rows.Err()
}

func (s *GatewayStore) PutCredentialVersion(req CredentialVersionPutRequest) (CredentialVersion, error) {
	return s.putCredentialVersion(req, 0)
}

func (s *GatewayStore) PutCredentialVersionAudited(req CredentialVersionPutRequest, auditID int64) (CredentialVersion, error) {
	return s.putCredentialVersion(req, auditID)
}

func (s *GatewayStore) putCredentialVersion(req CredentialVersionPutRequest, auditID int64) (CredentialVersion, error) {
	req.CredentialID = strings.TrimSpace(req.CredentialID)
	req.CreatedBy = strings.TrimSpace(req.CreatedBy)
	if req.CredentialID == "" || req.CreatedBy == "" {
		return CredentialVersion{}, fmt.Errorf("credential id and creator are required")
	}
	if len(req.Payload) == 0 || !json.Valid(req.Payload) {
		return CredentialVersion{}, ErrCredentialPayloadInvalid
	}
	resource, err := s.credentialByID(req.CredentialID)
	if err != nil {
		return CredentialVersion{}, err
	}
	if resource.State != CredentialStateActive {
		return CredentialVersion{}, ErrCredentialRevoked
	}
	if !s.canManageCredential(resource, req.CreatedBy, ActionCredentialRotate) {
		return CredentialVersion{}, ErrCredentialForbidden
	}
	if err := validateCredentialPayload(resource.Type, req.Payload); err != nil {
		return CredentialVersion{}, ErrCredentialPayloadInvalid
	}

	digest := credentialPayloadDigest(req.Payload)
	now := time.Now().UTC()
	version := CredentialVersion{
		ID:            "credver_" + randomHex(12),
		CredentialID:  resource.ID,
		PayloadDigest: digest,
		State:         CredentialVersionStaged,
		CreatedBy:     req.CreatedBy,
		CreatedAt:     now,
	}
	if req.Activate {
		version.State = CredentialVersionActive
		version.PromotedAt = &now
	}
	ciphertext, err := s.encryptSecretWithAAD(string(req.Payload), credentialPayloadAAD(resource.ID, version.ID, resource.Type, digest))
	if err != nil {
		return CredentialVersion{}, err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return CredentialVersion{}, err
	}
	defer tx.Rollback()
	if req.Activate {
		if _, err := tx.Exec(`UPDATE credential_versions SET state = ? WHERE credential_id = ? AND state = ?`,
			CredentialVersionSuperseded, resource.ID, CredentialVersionActive); err != nil {
			return CredentialVersion{}, fmt.Errorf("%w: %v", ErrCredentialAuditUnavailable, err)
		}
	}
	var promotedAt string
	if version.PromotedAt != nil {
		promotedAt = formatTime(*version.PromotedAt)
	}
	if _, err := tx.Exec(`INSERT INTO credential_versions
		(id, credential_id, payload_ciphertext, payload_digest, state, created_by, created_at, promoted_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		version.ID, version.CredentialID, ciphertext, version.PayloadDigest, version.State,
		version.CreatedBy, formatTime(version.CreatedAt), promotedAt); err != nil {
		return CredentialVersion{}, err
	}
	if _, err := tx.Exec(`UPDATE credentials SET updated_at = ? WHERE id = ?`, formatTime(now), resource.ID); err != nil {
		return CredentialVersion{}, err
	}
	if auditID != 0 {
		if err := finishAuditWith(tx, auditID, AuditFinish{
			Status: "success", VersionID: version.ID, EndedAt: now,
		}); err != nil {
			return CredentialVersion{}, fmt.Errorf("%w: %v", ErrCredentialAuditUnavailable, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return CredentialVersion{}, err
	}
	return version, nil
}

func (s *GatewayStore) ResolveCredentialPayload(credentialID, versionID string) (json.RawMessage, error) {
	credentialID = strings.TrimSpace(credentialID)
	versionID = strings.TrimSpace(versionID)
	var resource CredentialResource
	var version CredentialVersion
	var ciphertext string
	err := s.db.QueryRow(`SELECT c.id, c.name, c.scope, c.owner_id, c.type, c.state, c.created_by, c.created_at, c.updated_at, c.revoked_at,
			v.id, v.credential_id, v.payload_ciphertext, v.payload_digest, v.state, v.created_by, v.created_at, v.promoted_at, v.revoked_at
		FROM credentials c JOIN credential_versions v ON v.credential_id = c.id
		WHERE c.id = ? AND v.id = ? AND c.scope <> ?`, credentialID, versionID, machineVaultScope).Scan(
		&resource.ID, &resource.Name, &resource.Scope, &resource.OwnerID, &resource.Type, &resource.State,
		&resource.CreatedBy, new(string), new(string), new(string),
		&version.ID, &version.CredentialID, &ciphertext, &version.PayloadDigest, &version.State,
		&version.CreatedBy, new(string), new(string), new(string),
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrCredentialVersionNotFound
	}
	if err != nil {
		return nil, err
	}
	if resource.State == CredentialStateRevoked || version.State == CredentialVersionRevoked {
		return nil, ErrCredentialRevoked
	}
	plain, err := s.decryptSecretWithAAD(ciphertext, credentialPayloadAAD(resource.ID, version.ID, resource.Type, version.PayloadDigest))
	if err != nil {
		return nil, err
	}
	return json.RawMessage(plain), nil
}

func (s *GatewayStore) ActiveCredentialVersion(credentialID string) (CredentialVersion, error) {
	resource, err := s.credentialByID(credentialID)
	if err != nil {
		return CredentialVersion{}, err
	}
	if resource.State != CredentialStateActive {
		return CredentialVersion{}, ErrCredentialRevoked
	}
	version, err := s.credentialVersionByState(resource.ID, CredentialVersionActive)
	if errors.Is(err, sql.ErrNoRows) {
		return CredentialVersion{}, ErrCredentialVersionNotFound
	}
	return version, err
}

func (s *GatewayStore) CreateCredentialGrant(req CredentialGrantCreateRequest) (CredentialGrant, error) {
	return s.createCredentialGrant(req, 0)
}

func (s *GatewayStore) CreateCredentialGrantAudited(req CredentialGrantCreateRequest, auditID int64) (CredentialGrant, error) {
	return s.createCredentialGrant(req, auditID)
}

func (s *GatewayStore) createCredentialGrant(req CredentialGrantCreateRequest, auditID int64) (CredentialGrant, error) {
	req.CredentialID = strings.TrimSpace(req.CredentialID)
	req.CreatedBy = strings.TrimSpace(req.CreatedBy)
	if req.CredentialID == "" || req.CreatedBy == "" {
		return CredentialGrant{}, fmt.Errorf("credential and creator are required")
	}
	resource, err := s.credentialByID(req.CredentialID)
	if err != nil {
		return CredentialGrant{}, err
	}
	if resource.State != CredentialStateActive {
		return CredentialGrant{}, ErrCredentialRevoked
	}
	if !s.canManageCredential(resource, req.CreatedBy, ActionCredentialGrant) {
		return CredentialGrant{}, ErrCredentialForbidden
	}
	grant := CredentialGrant{
		ID:           "credgrant_" + randomHex(12),
		CredentialID: resource.ID,
		Scope:        normalizeCredentialGrantScope(req.Scope),
		State:        CredentialStateActive,
		CreatedBy:    req.CreatedBy,
		CreatedAt:    time.Now().UTC(),
	}
	if grant.Scope.Use == "" {
		grant.Scope.Use = credentialUseForType(resource.Type)
	}
	if err := validateCredentialGrantScope(grant.Scope); err != nil {
		return CredentialGrant{}, err
	}
	if resource.Type == CredentialTypeModelProvider {
		if grant.Scope.Use != CredentialUseModelProvider {
			return CredentialGrant{}, errors.New("credential_use_mismatch")
		}
		// doagent has one settings file, so a target takes one model provider.
		conflict, err := s.modelProviderGrantConflicts(resource.ID, grant.Scope)
		if err != nil {
			return CredentialGrant{}, err
		}
		if conflict {
			return CredentialGrant{}, errors.New("model_provider_already_bound")
		}
	}
	scopeJSON, err := json.Marshal(grant.Scope)
	if err != nil {
		return CredentialGrant{}, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return CredentialGrant{}, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT id, credential_id, grantee_id, scope_json, state, created_by, created_at, revoked_at
		FROM credential_grants WHERE credential_id = ? AND state = ? ORDER BY id`,
		grant.CredentialID, CredentialStateActive)
	if err != nil {
		return CredentialGrant{}, err
	}
	var semanticMatches []CredentialGrant
	for rows.Next() {
		existing, scanErr := scanCredentialGrant(rows)
		if scanErr != nil {
			rows.Close()
			return CredentialGrant{}, scanErr
		}
		if credentialScopeEqual(existing.Scope, grant.Scope) {
			semanticMatches = append(semanticMatches, existing)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return CredentialGrant{}, err
	}
	rows.Close()
	if len(semanticMatches) == 1 {
		existing := semanticMatches[0]
		if existingScopeJSON, marshalErr := json.Marshal(existing.Scope); marshalErr != nil {
			return CredentialGrant{}, marshalErr
		} else if string(existingScopeJSON) != string(scopeJSON) {
			if _, updateErr := tx.Exec(`UPDATE credential_grants SET scope_json = ?, created_by = ?, created_at = ?, revoked_at = '' WHERE id = ?`,
				string(scopeJSON), grant.CreatedBy, formatTime(grant.CreatedAt), existing.ID); updateErr != nil {
				return CredentialGrant{}, updateErr
			}
			if err := invalidateCredentialGrantVerifications(tx, existing.ID); err != nil {
				return CredentialGrant{}, err
			}
			existing.Scope = grant.Scope
			existing.CreatedBy = grant.CreatedBy
			existing.CreatedAt = grant.CreatedAt
		}
		if auditID != 0 {
			if err := finishAuditWith(tx, auditID, credentialGrantSuccessAudit(existing)); err != nil {
				return CredentialGrant{}, fmt.Errorf("%w: %v", ErrCredentialAuditUnavailable, err)
			}
		}
		if err := tx.Commit(); err != nil {
			return CredentialGrant{}, err
		}
		return existing, nil
	}
	if len(semanticMatches) > 1 {
		// Legacy rows may contain several execution-metadata variants for one
		// target/use authorization. Keep the stable first grant and refresh its
		// metadata instead of turning equivalent grants into an authorization block.
		existing := semanticMatches[0]
		if _, updateErr := tx.Exec(`UPDATE credential_grants SET scope_json = ?, created_by = ?, created_at = ?, revoked_at = '' WHERE id = ?`,
			string(scopeJSON), grant.CreatedBy, formatTime(grant.CreatedAt), existing.ID); updateErr != nil {
			return CredentialGrant{}, updateErr
		}
		if err := invalidateCredentialGrantVerifications(tx, existing.ID); err != nil {
			return CredentialGrant{}, err
		}
		existing.Scope = grant.Scope
		existing.CreatedBy = grant.CreatedBy
		existing.CreatedAt = grant.CreatedAt
		if auditID != 0 {
			if err := finishAuditWith(tx, auditID, credentialGrantSuccessAudit(existing)); err != nil {
				return CredentialGrant{}, fmt.Errorf("%w: %v", ErrCredentialAuditUnavailable, err)
			}
		}
		if err := tx.Commit(); err != nil {
			return CredentialGrant{}, err
		}
		return existing, nil
	}
	// Clear evidence from grants revoked by older Gateway versions before
	// their stable IDs are reused by the upsert below.
	if _, err := tx.Exec(`DELETE FROM credential_verifications WHERE grant_id IN (
		SELECT id FROM credential_grants WHERE credential_id = ? AND grantee_id = ? AND scope_json = ? AND state = ?
	)`, grant.CredentialID, grant.CreatedBy, string(scopeJSON), CredentialStateRevoked); err != nil {
		return CredentialGrant{}, err
	}
	_, err = tx.Exec(`INSERT INTO credential_grants
		(id, credential_id, grantee_id, scope_json, state, created_by, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(credential_id, grantee_id, scope_json)
		DO UPDATE SET
			state = excluded.state,
			created_by = excluded.created_by,
			created_at = excluded.created_at,
			revoked_at = ''`,
		grant.ID, grant.CredentialID, grant.CreatedBy, string(scopeJSON), grant.State,
		grant.CreatedBy, formatTime(grant.CreatedAt))
	if err != nil {
		return CredentialGrant{}, err
	}
	var existing CredentialGrant
	var storedScope, createdAt, revokedAt string
	err = tx.QueryRow(`SELECT id, credential_id, grantee_id, scope_json, state, created_by, created_at, revoked_at
		FROM credential_grants
		WHERE credential_id = ? AND grantee_id = ? AND scope_json = ?`,
		grant.CredentialID, grant.CreatedBy, string(scopeJSON),
	).Scan(
		&existing.ID, &existing.CredentialID, new(string), &storedScope,
		&existing.State, &existing.CreatedBy, &createdAt, &revokedAt,
	)
	if err != nil {
		return CredentialGrant{}, err
	}
	if err := json.Unmarshal([]byte(storedScope), &existing.Scope); err != nil {
		return CredentialGrant{}, err
	}
	existing.CreatedAt, _ = parseTime(createdAt)
	if auditID != 0 {
		if err := finishAuditWith(tx, auditID, credentialGrantSuccessAudit(existing)); err != nil {
			return CredentialGrant{}, fmt.Errorf("%w: %v", ErrCredentialAuditUnavailable, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return CredentialGrant{}, err
	}
	return existing, nil
}

// credentialGrantSuccessAudit is the success audit of a created grant. It keeps
// only the identifiers credentialTargetAudit accepts, so a request scope whose
// cluster or instance is free text leaves the target columns and the stored
// scope JSON empty.
func credentialGrantSuccessAudit(existing CredentialGrant) AuditFinish {
	target := credentialTargetAudit(existing.Scope, existing.ID)
	target.Status = "success"
	target.EndedAt = time.Now().UTC()
	return target
}

func (s *GatewayStore) RevokeCredentialGrant(credentialID, grantID, actorID string) (CredentialGrant, error) {
	grant, err := s.BeginCredentialGrantRevocation(credentialID, grantID, actorID)
	if err != nil {
		return CredentialGrant{}, err
	}
	if grant.State == CredentialStateRevoked {
		return grant, nil
	}
	return s.FinalizeCredentialGrantRevocation(credentialID, grantID, 0)
}

func (s *GatewayStore) BeginCredentialGrantRevocation(credentialID, grantID, actorID string) (CredentialGrant, error) {
	resource, err := s.credentialByID(strings.TrimSpace(credentialID))
	if err != nil {
		return CredentialGrant{}, err
	}
	if !s.canManageCredential(resource, strings.TrimSpace(actorID), ActionCredentialGrant) {
		return CredentialGrant{}, ErrCredentialForbidden
	}
	grant, err := s.credentialGrantByID(resource.ID, strings.TrimSpace(grantID))
	if err != nil {
		return CredentialGrant{}, err
	}
	if grant.State == CredentialStateRevoked {
		return grant, nil
	}
	if grant.State == CredentialStateRevoking {
		return grant, nil
	}
	result, err := s.db.Exec(`UPDATE credential_grants
		SET state = ?, revoked_at = ''
		WHERE id = ? AND credential_id = ? AND state = ?`,
		CredentialStateRevoking, grant.ID, resource.ID, CredentialStateActive)
	if err != nil {
		return CredentialGrant{}, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return CredentialGrant{}, err
	}
	if affected != 1 {
		return CredentialGrant{}, ErrCredentialGrantDenied
	}
	grant.State = CredentialStateRevoking
	grant.RevokedAt = nil
	return grant, nil
}

func invalidateCredentialGrantVerifications(tx *sql.Tx, grantID string) error {
	_, err := tx.Exec("DELETE FROM credential_verifications WHERE grant_id = ?", grantID)
	return err
}

func (s *GatewayStore) FinalizeCredentialGrantRevocation(
	credentialID string,
	grantID string,
	auditID int64,
) (CredentialGrant, error) {
	grant, err := s.credentialGrantByID(strings.TrimSpace(credentialID), strings.TrimSpace(grantID))
	if err != nil {
		return CredentialGrant{}, err
	}
	if grant.State == CredentialStateRevoked {
		if auditID != 0 {
			if err := s.FinishAudit(auditID, credentialGrantSuccessAudit(grant)); err != nil {
				return CredentialGrant{}, fmt.Errorf("%w: %v", ErrCredentialAuditUnavailable, err)
			}
		}
		return grant, nil
	}
	if grant.State != CredentialStateRevoking {
		return CredentialGrant{}, fmt.Errorf("credential grant is not revoking")
	}
	now := time.Now().UTC()
	tx, err := s.db.Begin()
	if err != nil {
		return CredentialGrant{}, err
	}
	defer tx.Rollback()
	// A use already under way when the revocation began can record or finish a
	// materialization after the cleanup list was read. The grant reaches revoked
	// only when none of its records may still own something on the target.
	if pending, err := uncleanedCredentialMaterializations(tx, grant.CredentialID, grant.ID); err != nil {
		return CredentialGrant{}, err
	} else if pending != 0 {
		return CredentialGrant{}, ErrCredentialCleanupPending
	}
	result, err := tx.Exec(`UPDATE credential_grants
		SET state = ?, revoked_at = ?
		WHERE id = ? AND credential_id = ? AND state = ?`,
		CredentialStateRevoked, formatTime(now), grant.ID, grant.CredentialID, CredentialStateRevoking)
	if err != nil {
		return CredentialGrant{}, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return CredentialGrant{}, err
	}
	if affected != 1 {
		return CredentialGrant{}, ErrCredentialGrantDenied
	}
	if err := invalidateCredentialGrantVerifications(tx, grant.ID); err != nil {
		return CredentialGrant{}, err
	}
	if auditID != 0 {
		if err := finishAuditWith(tx, auditID, credentialGrantSuccessAudit(grant)); err != nil {
			return CredentialGrant{}, fmt.Errorf("%w: %v", ErrCredentialAuditUnavailable, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return CredentialGrant{}, err
	}
	grant.State = CredentialStateRevoked
	grant.RevokedAt = &now
	return grant, nil
}

func (s *GatewayStore) AuthorizeCredentialUse(req CredentialUseRequest) (AuthorizedCredentialUse, error) {
	resource, grant, err := s.AuthorizeCredentialGrant(req)
	if err != nil {
		return AuthorizedCredentialUse{}, err
	}
	version, err := s.ActiveCredentialVersion(resource.ID)
	if err != nil {
		return AuthorizedCredentialUse{}, err
	}
	return AuthorizedCredentialUse{Resource: resource, Version: version, Grant: grant}, nil
}

func (s *GatewayStore) AuthorizeCredentialGrant(req CredentialUseRequest) (CredentialResource, CredentialGrant, error) {
	req.ActorID = strings.TrimSpace(req.ActorID)
	req.CredentialID = strings.TrimSpace(req.CredentialID)
	req.Credential = strings.TrimSpace(req.Credential)
	req.Scope = normalizeCredentialGrantScope(req.Scope)
	if req.ActorID == "" || (req.CredentialID == "" && req.Credential == "") ||
		req.Scope.Cluster == "" || req.Scope.Instance == "" {
		return CredentialResource{}, CredentialGrant{}, ErrCredentialGrantDenied
	}
	if !s.UserCanUseTarget(req.ActorID, req.Scope.Cluster, req.Scope.Instance) {
		return CredentialResource{}, CredentialGrant{}, ErrCredentialForbidden
	}
	if req.Scope.Use != "" && !validCredentialUse(req.Scope.Use) {
		return CredentialResource{}, CredentialGrant{}, ErrCredentialGrantDenied
	}
	credentialSelector := "c.name = ?"
	credentialValue := req.Credential
	if req.CredentialID != "" {
		credentialSelector = "c.id = ?"
		credentialValue = req.CredentialID
	}
	rows, err := s.db.Query(`SELECT c.id, c.name, c.scope, c.owner_id, c.type, c.state, c.created_by, c.created_at, c.updated_at, c.revoked_at,
			g.id, g.credential_id, g.grantee_id, g.scope_json, g.state, g.created_by, g.created_at, g.revoked_at
		FROM credential_grants g JOIN credentials c ON c.id = g.credential_id
		WHERE `+credentialSelector+` AND c.state = ? AND g.state = ? ORDER BY c.id, g.id`,
		credentialValue, CredentialStateActive, CredentialStateActive)
	if err != nil {
		return CredentialResource{}, CredentialGrant{}, err
	}
	defer rows.Close()
	var matches []AuthorizedCredentialUse
	otherTarget, invalidScope := false, false
	for rows.Next() {
		resource, grant, err := scanCredentialAuthorization(rows)
		if err != nil {
			return CredentialResource{}, CredentialGrant{}, err
		}
		if grant.Scope.Use == "" {
			grant.Scope.Use = credentialUseForType(resource.Type)
		}
		if grant.Scope.Cluster != req.Scope.Cluster || grant.Scope.Instance != req.Scope.Instance {
			otherTarget = true
			continue
		}
		if validateCredentialGrantScope(grant.Scope) != nil {
			invalidScope = true
			continue
		}
		if len(matches) != 0 && matches[len(matches)-1].Resource.ID == resource.ID &&
			credentialScopeEqual(matches[len(matches)-1].Grant.Scope, grant.Scope) {
			continue
		}
		matches = append(matches, AuthorizedCredentialUse{Resource: resource, Grant: grant})
	}
	if err := rows.Err(); err != nil {
		return CredentialResource{}, CredentialGrant{}, err
	}
	if len(matches) == 0 {
		switch {
		case invalidScope:
			return CredentialResource{}, CredentialGrant{}, ErrCredentialBindingScopeInvalid
		case otherTarget:
			return CredentialResource{}, CredentialGrant{}, ErrCredentialBindingTargetMismatch
		}
		return CredentialResource{}, CredentialGrant{}, ErrCredentialGrantDenied
	}
	if len(matches) > 1 {
		return CredentialResource{}, CredentialGrant{}, ErrCredentialAmbiguous
	}
	return matches[0].Resource, matches[0].Grant, nil
}

// HasUnpromotedDatabaseBinding reports whether the target has an active opaque
// database binding whose credential still lacks an active version. Auto DoSql
// discovery hides such credentials, so this separates "not promoted" from
// "never bound".
func (s *GatewayStore) HasUnpromotedDatabaseBinding(cluster, instance string) (bool, error) {
	rows, err := s.db.Query(`SELECT g.scope_json
		FROM credentials c JOIN credential_grants g ON g.credential_id = c.id
		WHERE c.state = ? AND c.type = ? AND g.state = ?
		  AND NOT EXISTS (
			SELECT 1 FROM credential_versions v
			WHERE v.credential_id = c.id AND v.state = ?
		  )`,
		CredentialStateActive, CredentialTypeOpaque, CredentialStateActive, CredentialVersionActive)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var scopeJSON string
		if err := rows.Scan(&scopeJSON); err != nil {
			return false, err
		}
		var scope CredentialGrantScope
		if err := json.Unmarshal([]byte(scopeJSON), &scope); err != nil {
			return false, err
		}
		scope = normalizeCredentialGrantScope(scope)
		if scope.Cluster == cluster && scope.Instance == instance && scope.Use == CredentialUseDatabase {
			return true, nil
		}
	}
	return false, rows.Err()
}

func (s *GatewayStore) CreateCredentialBundle(req CredentialBundleCreateRequest) (CredentialBundle, error) {
	return s.createCredentialBundle(req, 0)
}

func (s *GatewayStore) CreateCredentialBundleAudited(req CredentialBundleCreateRequest, auditID int64) (CredentialBundle, error) {
	return s.createCredentialBundle(req, auditID)
}

func (s *GatewayStore) createCredentialBundle(req CredentialBundleCreateRequest, auditID int64) (CredentialBundle, error) {
	req.Name = strings.TrimSpace(req.Name)
	req.OwnerID = strings.TrimSpace(req.OwnerID)
	req.CreatedBy = strings.TrimSpace(req.CreatedBy)
	if req.Name == "" || req.CreatedBy == "" || !validCredentialScope(req.Scope) || len(req.Items) == 0 {
		return CredentialBundle{}, fmt.Errorf("credential bundle metadata and items are required")
	}
	if req.Scope == CredentialScopePersonal {
		if req.OwnerID == "" || req.OwnerID != req.CreatedBy {
			return CredentialBundle{}, ErrCredentialForbidden
		}
	} else {
		req.OwnerID = ""
	}
	items := make([]CredentialBundleItem, 0, len(req.Items))
	seen := make(map[string]struct{}, len(req.Items))
	for _, item := range req.Items {
		item.CredentialID = strings.TrimSpace(item.CredentialID)
		resource, err := s.credentialByID(item.CredentialID)
		if err != nil {
			return CredentialBundle{}, fmt.Errorf("%w: %v", ErrCredentialAuditUnavailable, err)
		}
		if !s.canManageCredential(resource, req.CreatedBy, ActionCredentialGrant) {
			return CredentialBundle{}, ErrCredentialForbidden
		}
		if _, exists := seen[item.CredentialID]; exists {
			return CredentialBundle{}, fmt.Errorf("credential bundle contains a duplicate item")
		}
		seen[item.CredentialID] = struct{}{}
		items = append(items, item)
	}

	now := time.Now().UTC()
	bundle := CredentialBundle{
		ID: "credbundle_" + randomHex(12), Name: req.Name, Scope: req.Scope, OwnerID: req.OwnerID,
		State: CredentialStateActive, CreatedBy: req.CreatedBy, CreatedAt: now, UpdatedAt: now, Items: items,
	}
	tx, err := s.db.Begin()
	if err != nil {
		return CredentialBundle{}, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO credential_bundles
		(id, name, scope, owner_id, state, created_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		bundle.ID, bundle.Name, bundle.Scope, bundle.OwnerID, bundle.State, bundle.CreatedBy,
		formatTime(bundle.CreatedAt), formatTime(bundle.UpdatedAt)); err != nil {
		return CredentialBundle{}, err
	}
	for _, item := range bundle.Items {
		if _, err := tx.Exec(`INSERT INTO credential_bundle_items
			(bundle_id, credential_id) VALUES (?, ?)`,
			bundle.ID, item.CredentialID); err != nil {
			return CredentialBundle{}, err
		}
	}
	if auditID != 0 {
		if err := finishAuditWith(tx, auditID, AuditFinish{Status: "success", EndedAt: now}); err != nil {
			return CredentialBundle{}, fmt.Errorf("%w: %v", ErrCredentialAuditUnavailable, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return CredentialBundle{}, err
	}
	return bundle, nil
}

func (s *GatewayStore) ResolveCredentialBundle(actorID, name string) (CredentialBundle, error) {
	actorID = strings.TrimSpace(actorID)
	name = strings.TrimSpace(name)
	rows, err := s.db.Query(`SELECT id, name, scope, owner_id, state, created_by, created_at, updated_at
		FROM credential_bundles
		WHERE name = ? AND state = ? AND (owner_id = ? OR (scope = ? AND owner_id = ''))
		ORDER BY CASE WHEN owner_id = ? THEN 0 ELSE 1 END`,
		name, CredentialStateActive, actorID, CredentialScopePlatform, actorID)
	if err != nil {
		return CredentialBundle{}, err
	}
	defer rows.Close()
	var matches []CredentialBundle
	for rows.Next() {
		var bundle CredentialBundle
		var createdAt, updatedAt string
		if err := rows.Scan(&bundle.ID, &bundle.Name, &bundle.Scope, &bundle.OwnerID, &bundle.State,
			&bundle.CreatedBy, &createdAt, &updatedAt); err != nil {
			return CredentialBundle{}, err
		}
		bundle.CreatedAt, _ = parseTime(createdAt)
		bundle.UpdatedAt, _ = parseTime(updatedAt)
		matches = append(matches, bundle)
	}
	if err := rows.Err(); err != nil {
		return CredentialBundle{}, err
	}
	if len(matches) == 0 {
		return CredentialBundle{}, ErrCredentialBundleNotFound
	}
	if len(matches) > 1 {
		return CredentialBundle{}, ErrCredentialAmbiguous
	}
	bundle := matches[0]
	itemRows, err := s.db.Query(`SELECT credential_id
		FROM credential_bundle_items WHERE bundle_id = ? ORDER BY credential_id`, bundle.ID)
	if err != nil {
		return CredentialBundle{}, err
	}
	defer itemRows.Close()
	for itemRows.Next() {
		var item CredentialBundleItem
		if err := itemRows.Scan(&item.CredentialID); err != nil {
			return CredentialBundle{}, err
		}
		bundle.Items = append(bundle.Items, item)
	}
	return bundle, itemRows.Err()
}

func (s *GatewayStore) PromoteCredentialVersion(credentialID, versionID, actorID string) error {
	return s.promoteCredentialVersion(credentialID, versionID, actorID, 0)
}

func (s *GatewayStore) PromoteCredentialVersionAudited(credentialID, versionID, actorID string, auditID int64) error {
	return s.promoteCredentialVersion(credentialID, versionID, actorID, auditID)
}

func (s *GatewayStore) promoteCredentialVersion(credentialID, versionID, actorID string, auditID int64) error {
	resource, err := s.credentialByID(credentialID)
	if err != nil {
		return err
	}
	if resource.State != CredentialStateActive {
		return ErrCredentialRevoked
	}
	if !s.canManageCredential(resource, strings.TrimSpace(actorID), ActionCredentialRotate) {
		return ErrCredentialForbidden
	}
	version, err := s.credentialVersionByID(resource.ID, versionID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrCredentialVersionNotFound
	}
	if err != nil {
		return err
	}
	if version.State != CredentialVersionStaged {
		return fmt.Errorf("credential version is not staged")
	}
	now := time.Now().UTC()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE credential_versions SET state = ? WHERE credential_id = ? AND state = ?`,
		CredentialVersionSuperseded, resource.ID, CredentialVersionActive); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE credential_versions SET state = ?, promoted_at = ? WHERE id = ?`,
		CredentialVersionActive, formatTime(now), version.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE credentials SET updated_at = ? WHERE id = ?`, formatTime(now), resource.ID); err != nil {
		return err
	}
	if auditID != 0 {
		if err := finishAuditWith(tx, auditID, AuditFinish{
			Status: "success", VersionID: version.ID, EndedAt: now,
		}); err != nil {
			return fmt.Errorf("%w: %v", ErrCredentialAuditUnavailable, err)
		}
	}
	return tx.Commit()
}

func (s *GatewayStore) RevokeCredential(credentialID, actorID string) error {
	resource, err := s.credentialByID(credentialID)
	if err != nil {
		return err
	}
	if !s.canManageCredential(resource, strings.TrimSpace(actorID), ActionCredentialRevoke) {
		return ErrCredentialForbidden
	}
	if resource.State == CredentialStateRevoked {
		return nil
	}
	now := time.Now().UTC()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE credentials SET state = ?, revoked_at = ?, updated_at = ? WHERE id = ?`,
		CredentialStateRevoked, formatTime(now), formatTime(now), resource.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE credential_versions SET state = ?, revoked_at = ? WHERE credential_id = ? AND state != ?`,
		CredentialVersionRevoked, formatTime(now), resource.ID, CredentialVersionRevoked); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE credential_grants SET state = ?, revoked_at = ? WHERE credential_id = ? AND state != ?`,
		CredentialStateRevoked, formatTime(now), resource.ID, CredentialStateRevoked); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *GatewayStore) RecordCredentialVerification(verification CredentialVerification) error {
	return recordCredentialVerificationWith(s.db, verification)
}

func (s *GatewayStore) RecordCredentialVerificationsAudited(
	verifications []CredentialVerification,
	auditID int64,
	finish AuditFinish,
) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, verification := range verifications {
		if err := recordCredentialVerificationWith(tx, verification); err != nil {
			return err
		}
	}
	if err := finishAuditWith(tx, auditID, finish); err != nil {
		return fmt.Errorf("%w: %v", ErrCredentialAuditUnavailable, err)
	}
	return tx.Commit()
}

func recordCredentialVerificationWith(execer auditExecer, verification CredentialVerification) error {
	verification.CredentialID = strings.TrimSpace(verification.CredentialID)
	verification.VersionID = strings.TrimSpace(verification.VersionID)
	verification.GrantID = strings.TrimSpace(verification.GrantID)
	verification.Request.Payload = nil
	if verification.CredentialID == "" || verification.VersionID == "" || verification.GrantID == "" ||
		!validCredentialUse(verification.Use) || verification.Evidence.Status != "verified" {
		return fmt.Errorf("invalid credential verification")
	}
	requestJSON, err := json.Marshal(verification.Request)
	if err != nil {
		return err
	}
	evidenceJSON, err := json.Marshal(verification.Evidence)
	if err != nil {
		return err
	}
	if verification.ID == "" {
		verification.ID = "credverify_" + randomHex(12)
	}
	contextKey := credentialMaterializationContextKey(verification.Request)
	if verification.VerifiedAt.IsZero() {
		verification.VerifiedAt = time.Now().UTC()
	}
	_, err = execer.Exec(`INSERT INTO credential_verifications
		(id, credential_id, version_id, grant_id, use, context_key, request_json, evidence_json, status, verified_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'verified', ?)
		ON CONFLICT(version_id, grant_id, use, context_key) DO UPDATE SET
			id = excluded.id,
			request_json = excluded.request_json,
			evidence_json = excluded.evidence_json,
			status = excluded.status,
			verified_at = excluded.verified_at`,
		verification.ID, verification.CredentialID, verification.VersionID, verification.GrantID,
		verification.Use, contextKey, string(requestJSON), string(evidenceJSON), formatTime(verification.VerifiedAt))
	return err
}

func (s *GatewayStore) CredentialVerifications(credentialID, versionID string) ([]CredentialVerification, error) {
	rows, err := s.db.Query(`SELECT id, credential_id, version_id, grant_id, use, request_json, evidence_json, status, verified_at
		FROM credential_verifications WHERE credential_id = ? AND version_id = ? AND status = 'verified'
		ORDER BY grant_id, use, context_key`, strings.TrimSpace(credentialID), strings.TrimSpace(versionID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var verifications []CredentialVerification
	for rows.Next() {
		var verification CredentialVerification
		var requestJSON, evidenceJSON, verifiedAt string
		if err := rows.Scan(&verification.ID, &verification.CredentialID, &verification.VersionID,
			&verification.GrantID, &verification.Use, &requestJSON, &evidenceJSON, &verification.Status, &verifiedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(requestJSON), &verification.Request); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(evidenceJSON), &verification.Evidence); err != nil {
			return nil, err
		}
		verification.VerifiedAt, _ = parseTime(verifiedAt)
		verifications = append(verifications, verification)
	}
	return verifications, rows.Err()
}

func (s *GatewayStore) ActiveCredentialGrants(credentialID string) ([]CredentialGrant, error) {
	rows, err := s.db.Query(`SELECT id, credential_id, grantee_id, scope_json, state, created_by, created_at, revoked_at
		FROM credential_grants WHERE credential_id = ? AND state = ? ORDER BY id`,
		strings.TrimSpace(credentialID), CredentialStateActive)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var grants []CredentialGrant
	seenTargets := make(map[string]struct{})
	for rows.Next() {
		grant, err := scanCredentialGrant(rows)
		if err != nil {
			return nil, err
		}
		targetKey := grant.Scope.Cluster + "\x00" + grant.Scope.Instance
		if _, exists := seenTargets[targetKey]; exists {
			continue
		}
		seenTargets[targetKey] = struct{}{}
		grants = append(grants, grant)
	}
	return grants, rows.Err()
}

func (s *GatewayStore) CredentialGrantsForContext(credentialID string, scope CredentialGrantScope) ([]CredentialGrant, error) {
	grants, err := s.ActiveCredentialGrants(credentialID)
	if err != nil {
		return nil, err
	}
	matches := make([]CredentialGrant, 0, len(grants))
	seenTargets := make(map[string]struct{})
	for _, grant := range grants {
		if credentialScopeEqual(grant.Scope, scope) {
			targetKey := grant.Scope.Cluster + "\x00" + grant.Scope.Instance
			if _, exists := seenTargets[targetKey]; exists {
				continue
			}
			seenTargets[targetKey] = struct{}{}
			matches = append(matches, grant)
		}
	}
	if len(matches) == 0 {
		return nil, ErrCredentialGrantDenied
	}
	return matches, nil
}

func (s *GatewayStore) BeginCredentialRevocation(credentialID, actorID string) (CredentialResource, error) {
	resource, err := s.credentialByID(credentialID)
	if err != nil {
		return CredentialResource{}, err
	}
	if !s.canManageCredential(resource, strings.TrimSpace(actorID), ActionCredentialRevoke) {
		return CredentialResource{}, ErrCredentialForbidden
	}
	if resource.State == CredentialStateRevoked {
		return resource, nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return CredentialResource{}, err
	}
	defer tx.Rollback()
	if resource.State == CredentialStateActive {
		now := time.Now().UTC()
		if _, err := tx.Exec(`UPDATE credentials SET state = ?, updated_at = ? WHERE id = ? AND state = ?`,
			CredentialStateRevoking, formatTime(now), resource.ID, CredentialStateActive); err != nil {
			return CredentialResource{}, err
		}
		resource.State = CredentialStateRevoking
		resource.UpdatedAt = now
	}
	// Revoking a resource revokes all of its grants, so they follow it to
	// revoking. This also moves grants that a revocation started before this
	// rule left active, so no grant reads as active while it is being cleaned.
	if _, err := tx.Exec(`UPDATE credential_grants SET state = ?, revoked_at = '' WHERE credential_id = ? AND state = ?`,
		CredentialStateRevoking, resource.ID, CredentialStateActive); err != nil {
		return CredentialResource{}, err
	}
	if err := tx.Commit(); err != nil {
		return CredentialResource{}, err
	}
	return resource, nil
}

func (s *GatewayStore) FinalizeCredentialRevocation(credentialID string) error {
	return s.finalizeCredentialRevocation(credentialID, 0)
}

func (s *GatewayStore) FinalizeCredentialRevocationAudited(credentialID string, auditID int64) error {
	return s.finalizeCredentialRevocation(credentialID, auditID)
}

func (s *GatewayStore) finalizeCredentialRevocation(credentialID string, auditID int64) error {
	resource, err := s.credentialByID(credentialID)
	if err != nil {
		return err
	}
	if resource.State == CredentialStateRevoked {
		if auditID != 0 {
			if err := s.FinishAudit(auditID, AuditFinish{
				Status: "success", EndedAt: time.Now().UTC(),
			}); err != nil {
				return fmt.Errorf("%w: %v", ErrCredentialAuditUnavailable, err)
			}
		}
		return nil
	}
	if resource.State != CredentialStateRevoking {
		return fmt.Errorf("credential is not revoking")
	}
	now := time.Now().UTC()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// The same check as for a grant, over every grant of the resource.
	if pending, err := uncleanedCredentialMaterializations(tx, resource.ID, ""); err != nil {
		return err
	} else if pending != 0 {
		return ErrCredentialCleanupPending
	}
	if _, err := tx.Exec(`UPDATE credentials SET state = ?, revoked_at = ?, updated_at = ? WHERE id = ?`,
		CredentialStateRevoked, formatTime(now), formatTime(now), resource.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE credential_versions SET state = ?, revoked_at = ? WHERE credential_id = ? AND state != ?`,
		CredentialVersionRevoked, formatTime(now), resource.ID, CredentialVersionRevoked); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE credential_grants SET state = ?, revoked_at = ? WHERE credential_id = ? AND state != ?`,
		CredentialStateRevoked, formatTime(now), resource.ID, CredentialStateRevoked); err != nil {
		return err
	}
	if auditID != 0 {
		if err := finishAuditWith(tx, auditID, AuditFinish{Status: "success", EndedAt: now}); err != nil {
			return fmt.Errorf("%w: %v", ErrCredentialAuditUnavailable, err)
		}
	}
	return tx.Commit()
}

func (s *GatewayStore) credentialByID(id string) (CredentialResource, error) {
	row := s.db.QueryRow(`SELECT id, name, scope, owner_id, type, state, created_by, created_at, updated_at, revoked_at
		FROM credentials WHERE id = ? AND scope <> ?`, strings.TrimSpace(id), machineVaultScope)
	resource, err := scanCredentialResource(row)
	if errors.Is(err, sql.ErrNoRows) {
		return CredentialResource{}, ErrCredentialNotFound
	}
	return resource, err
}

func (s *GatewayStore) CredentialByID(id string) (CredentialResource, error) {
	return s.credentialByID(id)
}

func (s *GatewayStore) credentialVersionByID(credentialID, versionID string) (CredentialVersion, error) {
	row := s.db.QueryRow(`SELECT id, credential_id, payload_digest, state, created_by, created_at, promoted_at, revoked_at
		FROM credential_versions WHERE credential_id = ? AND id = ?`, strings.TrimSpace(credentialID), strings.TrimSpace(versionID))
	return scanCredentialVersion(row)
}

func (s *GatewayStore) CredentialVersionByID(credentialID, versionID string) (CredentialVersion, error) {
	version, err := s.credentialVersionByID(credentialID, versionID)
	if errors.Is(err, sql.ErrNoRows) {
		return CredentialVersion{}, ErrCredentialVersionNotFound
	}
	return version, err
}

func (s *GatewayStore) CredentialVersions(credentialID string) ([]CredentialVersion, error) {
	rows, err := s.db.Query(`SELECT id, credential_id, payload_digest, state, created_by, created_at, promoted_at, revoked_at
		FROM credential_versions WHERE credential_id = ? ORDER BY created_at DESC, id DESC`, strings.TrimSpace(credentialID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	versions := make([]CredentialVersion, 0)
	for rows.Next() {
		version, err := scanCredentialVersion(rows)
		if err != nil {
			return nil, err
		}
		versions = append(versions, version)
	}
	return versions, rows.Err()
}

func (s *GatewayStore) CredentialGrants(credentialID string) ([]CredentialGrant, error) {
	rows, err := s.db.Query(`SELECT id, credential_id, grantee_id, scope_json, state, created_by, created_at, revoked_at
		FROM credential_grants WHERE credential_id = ? ORDER BY created_at DESC, id DESC`, strings.TrimSpace(credentialID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	grants := make([]CredentialGrant, 0)
	for rows.Next() {
		grant, err := scanCredentialGrant(rows)
		if err != nil {
			return nil, err
		}
		grants = append(grants, grant)
	}
	return grants, rows.Err()
}

func (s *GatewayStore) credentialVersionByState(credentialID string, state CredentialVersionState) (CredentialVersion, error) {
	row := s.db.QueryRow(`SELECT id, credential_id, payload_digest, state, created_by, created_at, promoted_at, revoked_at
		FROM credential_versions WHERE credential_id = ? AND state = ?`, credentialID, state)
	return scanCredentialVersion(row)
}

func (s *GatewayStore) CredentialVersionByState(credentialID string, state CredentialVersionState) (CredentialVersion, error) {
	version, err := s.credentialVersionByState(strings.TrimSpace(credentialID), state)
	if errors.Is(err, sql.ErrNoRows) {
		return CredentialVersion{}, ErrCredentialVersionNotFound
	}
	return version, err
}

func (s *GatewayStore) credentialGrantByID(credentialID, grantID string) (CredentialGrant, error) {
	row := s.db.QueryRow(`SELECT id, credential_id, grantee_id, scope_json, state, created_by, created_at, revoked_at
		FROM credential_grants WHERE credential_id = ? AND id = ?`, credentialID, grantID)
	grant, err := scanCredentialGrant(row)
	if errors.Is(err, sql.ErrNoRows) {
		return CredentialGrant{}, ErrCredentialGrantDenied
	}
	return grant, err
}

type credentialScanner interface {
	Scan(dest ...any) error
}

func scanCredentialResource(scanner credentialScanner) (CredentialResource, error) {
	var resource CredentialResource
	var createdAt, updatedAt, revokedAt string
	if err := scanner.Scan(&resource.ID, &resource.Name, &resource.Scope, &resource.OwnerID, &resource.Type, &resource.State,
		&resource.CreatedBy, &createdAt, &updatedAt, &revokedAt); err != nil {
		return CredentialResource{}, err
	}
	resource.CreatedAt, _ = parseTime(createdAt)
	resource.UpdatedAt, _ = parseTime(updatedAt)
	if revokedAt != "" {
		if parsed, err := parseTime(revokedAt); err == nil {
			resource.RevokedAt = &parsed
		}
	}
	return resource, nil
}

func scanCredentialVersion(scanner credentialScanner) (CredentialVersion, error) {
	var version CredentialVersion
	var createdAt, promotedAt, revokedAt string
	if err := scanner.Scan(&version.ID, &version.CredentialID, &version.PayloadDigest, &version.State, &version.CreatedBy,
		&createdAt, &promotedAt, &revokedAt); err != nil {
		return CredentialVersion{}, err
	}
	version.CreatedAt, _ = parseTime(createdAt)
	if promotedAt != "" {
		if parsed, err := parseTime(promotedAt); err == nil {
			version.PromotedAt = &parsed
		}
	}
	if revokedAt != "" {
		if parsed, err := parseTime(revokedAt); err == nil {
			version.RevokedAt = &parsed
		}
	}
	return version, nil
}

func scanCredentialGrant(scanner credentialScanner) (CredentialGrant, error) {
	var grant CredentialGrant
	var scopeJSON, createdAt, revokedAt string
	if err := scanner.Scan(&grant.ID, &grant.CredentialID, new(string), &scopeJSON,
		&grant.State, &grant.CreatedBy, &createdAt, &revokedAt); err != nil {
		return CredentialGrant{}, err
	}
	if err := json.Unmarshal([]byte(scopeJSON), &grant.Scope); err != nil {
		return CredentialGrant{}, err
	}
	grant.CreatedAt, _ = parseTime(createdAt)
	if revokedAt != "" {
		if parsed, err := parseTime(revokedAt); err == nil {
			grant.RevokedAt = &parsed
		}
	}
	return grant, nil
}

func scanCredentialAuthorization(scanner credentialScanner) (CredentialResource, CredentialGrant, error) {
	var resource CredentialResource
	var grant CredentialGrant
	var resourceCreatedAt, resourceUpdatedAt, resourceRevokedAt, scopeJSON, grantCreatedAt, grantRevokedAt string
	if err := scanner.Scan(
		&resource.ID, &resource.Name, &resource.Scope, &resource.OwnerID, &resource.Type, &resource.State,
		&resource.CreatedBy, &resourceCreatedAt, &resourceUpdatedAt, &resourceRevokedAt,
		&grant.ID, &grant.CredentialID, new(string), &scopeJSON, &grant.State, &grant.CreatedBy,
		&grantCreatedAt, &grantRevokedAt,
	); err != nil {
		return CredentialResource{}, CredentialGrant{}, err
	}
	if err := json.Unmarshal([]byte(scopeJSON), &grant.Scope); err != nil {
		return CredentialResource{}, CredentialGrant{}, err
	}
	resource.CreatedAt, _ = parseTime(resourceCreatedAt)
	resource.UpdatedAt, _ = parseTime(resourceUpdatedAt)
	grant.CreatedAt, _ = parseTime(grantCreatedAt)
	return resource, grant, nil
}

func credentialPayloadDigest(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func credentialPayloadAAD(credentialID, versionID string, credentialType CredentialType, digest string) []byte {
	return []byte(strings.Join([]string{"credential", credentialID, versionID, string(credentialType), digest}, "\x00"))
}

func (s *GatewayStore) canManageCredential(resource CredentialResource, actorID string, action GatewayAction) bool {
	if resource.Scope == CredentialScopePersonal {
		return resource.OwnerID == actorID || s.UserIsPlatformAdmin(actorID)
	}
	return s.UserIsPlatformAdmin(actorID)
}

func validCredentialScope(scope CredentialScope) bool {
	return scope == CredentialScopePersonal || scope == CredentialScopePlatform
}

func validCredentialType(credentialType CredentialType) bool {
	switch credentialType {
	case CredentialTypeRegistry, CredentialTypeTLS, CredentialTypeOpaque, CredentialTypeHelmRepository, CredentialTypeGitToken,
		CredentialTypeModelProvider:
		return true
	default:
		return false
	}
}

func validCredentialUse(use CredentialUse) bool {
	switch use {
	case CredentialUseImagePull, CredentialUseGitCheckout, CredentialUseHelmPull, CredentialUseTLS, CredentialUseOpaqueSecret, CredentialUseDatabase,
		CredentialUseModelProvider:
		return true
	default:
		return false
	}
}

func validateCredentialPayload(credentialType CredentialType, payload json.RawMessage) error {
	switch credentialType {
	case CredentialTypeRegistry:
		var value struct {
			Server   string `json:"server"`
			Username string `json:"username"`
			Password string `json:"password"`
			Email    string `json:"email,omitempty"`
		}
		if err := decodeStrictCredentialPayload(payload, &value); err != nil ||
			!validCredentialEndpoint(value.Server, true) ||
			strings.TrimSpace(value.Username) == "" ||
			strings.TrimSpace(value.Password) == "" {
			return ErrCredentialPayloadInvalid
		}
	case CredentialTypeTLS:
		var value struct {
			Certificate string `json:"certificate"`
			PrivateKey  string `json:"privateKey"`
		}
		if err := decodeStrictCredentialPayload(payload, &value); err != nil ||
			strings.TrimSpace(value.Certificate) == "" ||
			strings.TrimSpace(value.PrivateKey) == "" {
			return ErrCredentialPayloadInvalid
		}
	case CredentialTypeOpaque:
		var value struct {
			Data map[string]string `json:"data"`
		}
		if err := decodeStrictCredentialPayload(payload, &value); err != nil || len(value.Data) == 0 {
			return ErrCredentialPayloadInvalid
		}
		for key, item := range value.Data {
			if strings.TrimSpace(key) == "" || item == "" {
				return ErrCredentialPayloadInvalid
			}
		}
	case CredentialTypeHelmRepository:
		var value struct {
			URL      string `json:"url"`
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if err := decodeStrictCredentialPayload(payload, &value); err != nil ||
			!validCredentialEndpoint(value.URL, false) ||
			strings.TrimSpace(value.Username) == "" ||
			strings.TrimSpace(value.Password) == "" {
			return ErrCredentialPayloadInvalid
		}
	case CredentialTypeGitToken:
		var value struct {
			URL      string `json:"url"`
			Username string `json:"username"`
			Token    string `json:"token"`
		}
		if err := decodeStrictCredentialPayload(payload, &value); err != nil ||
			!validCredentialEndpoint(value.URL, false) ||
			strings.TrimSpace(value.Username) == "" ||
			strings.TrimSpace(value.Token) == "" {
			return ErrCredentialPayloadInvalid
		}
	case CredentialTypeModelProvider:
		if _, err := decodeModelProviderPayload(payload); err != nil {
			return ErrCredentialPayloadInvalid
		}
	default:
		return ErrCredentialPayloadInvalid
	}
	return nil
}

func decodeStrictCredentialPayload(payload []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return ErrCredentialPayloadInvalid
	}
	return nil
}

func validCredentialEndpoint(raw string, allowRegistryHost bool) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	if allowRegistryHost && !strings.Contains(raw, "://") {
		return !strings.ContainsAny(raw, "/?#@ \t\r\n")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User != nil || parsed.Hostname() == "" {
		return false
	}
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && credentialPrivateEndpointAllowed(parsed.Hostname())) {
		return false
	}
	return parsed.RawQuery == "" && parsed.Fragment == ""
}
