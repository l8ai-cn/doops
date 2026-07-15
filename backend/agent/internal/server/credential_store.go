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
	"sort"
	"strings"
	"time"
)

var (
	ErrSecretKeyUnavailable      = errors.New("DOOPS_GATEWAY_SECRET_KEY is required")
	ErrCredentialNotFound        = errors.New("credential not found")
	ErrCredentialVersionNotFound = errors.New("credential version not found")
	ErrCredentialRevoked         = errors.New("credential is revoked")
	ErrCredentialGrantDenied     = errors.New("credential grant denied")
	ErrCredentialForbidden       = errors.New("credential operation forbidden")
	ErrCredentialPayloadInvalid  = errors.New("credential payload is invalid")
	ErrCredentialBundleNotFound  = errors.New("credential bundle not found")
	ErrCredentialAmbiguous       = errors.New("credential reference is ambiguous")
)

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
	CredentialUseImagePull    CredentialUse = "imagePull"
	CredentialUseGitCheckout  CredentialUse = "gitCheckout"
	CredentialUseHelmPull     CredentialUse = "helmPull"
	CredentialUseTLS          CredentialUse = "tls"
	CredentialUseOpaqueSecret CredentialUse = "opaqueSecret"
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
	ID           string          `json:"id"`
	CredentialID string          `json:"credential_id"`
	GranteeID    string          `json:"grantee_id"`
	Cluster      string          `json:"cluster"`
	Instance     string          `json:"instance"`
	Project      string          `json:"project"`
	Environment  string          `json:"environment"`
	Template     string          `json:"template"`
	Namespace    string          `json:"namespace"`
	Uses         []CredentialUse `json:"uses"`
	State        CredentialState `json:"state"`
	CreatedBy    string          `json:"created_by"`
	CreatedAt    time.Time       `json:"created_at"`
	RevokedAt    *time.Time      `json:"revoked_at,omitempty"`
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
	CredentialID       string                 `json:"credential_id"`
	Use                CredentialUse          `json:"use"`
	Namespace          string                 `json:"namespace"`
	Workload           CredentialPlanWorkload `json:"workload,omitempty"`
	RegistryRepository string                 `json:"registry_repository,omitempty"`
	RegistryReference  string                 `json:"registry_reference,omitempty"`
	RequiredKeys       []string               `json:"required_keys,omitempty"`
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

type CredentialVersionPutRequest struct {
	CredentialID string
	Payload      json.RawMessage
	CreatedBy    string
	Activate     bool
}

type CredentialGrantCreateRequest struct {
	CredentialID string
	GranteeID    string
	Cluster      string
	Instance     string
	Project      string
	Environment  string
	Template     string
	Namespace    string
	Uses         []CredentialUse
	CreatedBy    string
}

type CredentialUseRequest struct {
	ActorID      string
	CredentialID string
	Credential   string
	Cluster      string
	Instance     string
	Project      string
	Environment  string
	Template     string
	Namespace    string
	Use          CredentialUse
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
	_, err := s.db.Exec(`INSERT INTO credentials
		(id, name, scope, owner_id, type, state, created_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		resource.ID, resource.Name, resource.Scope, resource.OwnerID, resource.Type, resource.State,
		resource.CreatedBy, formatTime(now), formatTime(now))
	if err != nil {
		return CredentialResource{}, err
	}
	return resource, nil
}

func (s *GatewayStore) ListCredentials(filter CredentialListFilter) ([]CredentialResource, error) {
	args := make([]any, 0, 4)
	where := []string{"1 = 1"}
	if actorID := strings.TrimSpace(filter.ActorID); actorID != "" {
		where = append(where, `(owner_id = ? OR created_by = ? OR id IN (
			SELECT credential_id FROM credential_grants WHERE grantee_id = ? AND state = 'active'
		))`)
		args = append(args, actorID, actorID, actorID)
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

func (s *GatewayStore) PutCredentialVersion(req CredentialVersionPutRequest) (CredentialVersion, error) {
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
			return CredentialVersion{}, err
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
		WHERE c.id = ? AND v.id = ?`, credentialID, versionID).Scan(
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
	req.CredentialID = strings.TrimSpace(req.CredentialID)
	req.GranteeID = strings.TrimSpace(req.GranteeID)
	req.CreatedBy = strings.TrimSpace(req.CreatedBy)
	if req.CredentialID == "" || req.GranteeID == "" || req.CreatedBy == "" {
		return CredentialGrant{}, fmt.Errorf("credential, grantee, and creator are required")
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
		GranteeID:    req.GranteeID,
		Cluster:      strings.TrimSpace(req.Cluster),
		Instance:     strings.TrimSpace(req.Instance),
		Project:      strings.TrimSpace(req.Project),
		Environment:  strings.TrimSpace(req.Environment),
		Template:     strings.TrimSpace(req.Template),
		Namespace:    strings.TrimSpace(req.Namespace),
		Uses:         normalizedCredentialUses(req.Uses),
		State:        CredentialStateActive,
		CreatedBy:    req.CreatedBy,
		CreatedAt:    time.Now().UTC(),
	}
	if grant.Cluster == "" || grant.Instance == "" || grant.Project == "" || grant.Environment == "" ||
		grant.Template == "" || grant.Namespace == "" || len(grant.Uses) == 0 {
		return CredentialGrant{}, fmt.Errorf("credential grant requires every deployment dimension and at least one use")
	}
	usesJSON, err := json.Marshal(grant.Uses)
	if err != nil {
		return CredentialGrant{}, err
	}
	_, err = s.db.Exec(`INSERT INTO credential_grants
		(id, credential_id, grantee_id, cluster, instance, project, environment, template, namespace, uses_json, state, created_by, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(credential_id, grantee_id, cluster, instance, project, environment, template, namespace, uses_json)
		DO UPDATE SET
			state = excluded.state,
			created_by = excluded.created_by,
			created_at = excluded.created_at,
			revoked_at = ''`,
		grant.ID, grant.CredentialID, grant.GranteeID, grant.Cluster, grant.Instance, grant.Project,
		grant.Environment, grant.Template, grant.Namespace, string(usesJSON), grant.State,
		grant.CreatedBy, formatTime(grant.CreatedAt))
	if err != nil {
		return CredentialGrant{}, err
	}
	existing, err := s.credentialGrantByIdentity(grant, string(usesJSON))
	if err != nil {
		return CredentialGrant{}, err
	}
	return existing, nil
}

func (s *GatewayStore) RevokeCredentialGrant(credentialID, grantID, actorID string) (CredentialGrant, error) {
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
	now := time.Now().UTC()
	result, err := s.db.Exec(`UPDATE credential_grants
		SET state = ?, revoked_at = ?
		WHERE id = ? AND credential_id = ? AND state = ?`,
		CredentialStateRevoked, formatTime(now), grant.ID, resource.ID, CredentialStateActive)
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
	req.Cluster = strings.TrimSpace(req.Cluster)
	req.Instance = strings.TrimSpace(req.Instance)
	req.Project = strings.TrimSpace(req.Project)
	req.Environment = strings.TrimSpace(req.Environment)
	req.Template = strings.TrimSpace(req.Template)
	req.Namespace = strings.TrimSpace(req.Namespace)
	if req.ActorID == "" || (req.CredentialID == "" && req.Credential == "") || req.Cluster == "" || req.Instance == "" || req.Project == "" ||
		req.Environment == "" || req.Template == "" || req.Namespace == "" || !validCredentialUse(req.Use) {
		return CredentialResource{}, CredentialGrant{}, ErrCredentialGrantDenied
	}
	credentialSelector := "c.name = ?"
	credentialValue := req.Credential
	if req.CredentialID != "" {
		credentialSelector = "c.id = ?"
		credentialValue = req.CredentialID
	}
	rows, err := s.db.Query(`SELECT c.id, c.name, c.scope, c.owner_id, c.type, c.state, c.created_by, c.created_at, c.updated_at, c.revoked_at,
			g.id, g.credential_id, g.grantee_id, g.cluster, g.instance, g.project, g.environment, g.template, g.namespace,
			g.uses_json, g.state, g.created_by, g.created_at, g.revoked_at
		FROM credential_grants g JOIN credentials c ON c.id = g.credential_id
		WHERE `+credentialSelector+` AND c.state = ? AND g.grantee_id = ? AND g.cluster = ? AND g.instance = ? AND g.project = ?
			AND g.environment = ? AND g.template = ? AND g.namespace = ? AND g.state = ?`,
		credentialValue, CredentialStateActive, req.ActorID, req.Cluster, req.Instance, req.Project,
		req.Environment, req.Template, req.Namespace, CredentialStateActive)
	if err != nil {
		return CredentialResource{}, CredentialGrant{}, err
	}
	defer rows.Close()
	var matches []AuthorizedCredentialUse
	for rows.Next() {
		resource, grant, err := scanCredentialAuthorization(rows)
		if err != nil {
			return CredentialResource{}, CredentialGrant{}, err
		}
		if !containsCredentialUse(grant.Uses, req.Use) {
			continue
		}
		matches = append(matches, AuthorizedCredentialUse{Resource: resource, Grant: grant})
	}
	if err := rows.Err(); err != nil {
		return CredentialResource{}, CredentialGrant{}, err
	}
	if len(matches) == 0 {
		return CredentialResource{}, CredentialGrant{}, ErrCredentialGrantDenied
	}
	if len(matches) > 1 {
		return CredentialResource{}, CredentialGrant{}, ErrCredentialAmbiguous
	}
	return matches[0].Resource, matches[0].Grant, nil
}

func (s *GatewayStore) CreateCredentialBundle(req CredentialBundleCreateRequest) (CredentialBundle, error) {
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
		item.Namespace = strings.TrimSpace(item.Namespace)
		item.RegistryRepository = strings.TrimSpace(item.RegistryRepository)
		item.RegistryReference = strings.TrimSpace(item.RegistryReference)
		item.RequiredKeys = normalizedCredentialKeys(item.RequiredKeys)
		resource, err := s.credentialByID(item.CredentialID)
		if err != nil {
			return CredentialBundle{}, err
		}
		if !s.canManageCredential(resource, req.CreatedBy, ActionCredentialGrant) {
			return CredentialBundle{}, ErrCredentialForbidden
		}
		if err := validateCredentialBundleItem(resource.Type, item); err != nil {
			return CredentialBundle{}, err
		}
		identity := item.CredentialID + "\x00" + string(item.Use)
		if _, exists := seen[identity]; exists {
			return CredentialBundle{}, fmt.Errorf("credential bundle contains a duplicate item")
		}
		seen[identity] = struct{}{}
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
		workloadJSON, _ := json.Marshal(item.Workload)
		requiredKeysJSON, _ := json.Marshal(item.RequiredKeys)
		if _, err := tx.Exec(`INSERT INTO credential_bundle_items
			(bundle_id, credential_id, use, namespace, workload_json, registry_repository, registry_reference, required_keys_json)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			bundle.ID, item.CredentialID, item.Use, item.Namespace, string(workloadJSON),
			item.RegistryRepository, item.RegistryReference, string(requiredKeysJSON)); err != nil {
			return CredentialBundle{}, err
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
	itemRows, err := s.db.Query(`SELECT credential_id, use, namespace, workload_json,
			registry_repository, registry_reference, required_keys_json
		FROM credential_bundle_items WHERE bundle_id = ? ORDER BY credential_id, use`, bundle.ID)
	if err != nil {
		return CredentialBundle{}, err
	}
	defer itemRows.Close()
	for itemRows.Next() {
		var item CredentialBundleItem
		var workloadJSON, requiredKeysJSON string
		if err := itemRows.Scan(&item.CredentialID, &item.Use, &item.Namespace, &workloadJSON,
			&item.RegistryRepository, &item.RegistryReference, &requiredKeysJSON); err != nil {
			return CredentialBundle{}, err
		}
		if err := json.Unmarshal([]byte(workloadJSON), &item.Workload); err != nil {
			return CredentialBundle{}, err
		}
		if err := json.Unmarshal([]byte(requiredKeysJSON), &item.RequiredKeys); err != nil {
			return CredentialBundle{}, err
		}
		bundle.Items = append(bundle.Items, item)
	}
	return bundle, itemRows.Err()
}

func validateCredentialBundleItem(credentialType CredentialType, item CredentialBundleItem) error {
	if item.Namespace == "" || !validCredentialUse(item.Use) {
		return fmt.Errorf("credential bundle item is incomplete")
	}
	switch credentialType {
	case CredentialTypeRegistry:
		if item.Use != CredentialUseImagePull || strings.TrimSpace(item.Workload.Kind) == "" ||
			strings.TrimSpace(item.Workload.Name) == "" || item.RegistryRepository == "" || item.RegistryReference == "" {
			return fmt.Errorf("registry bundle item requires imagePull materialization context")
		}
	case CredentialTypeTLS:
		if item.Use != CredentialUseTLS {
			return fmt.Errorf("TLS bundle item use is invalid")
		}
	case CredentialTypeOpaque:
		if item.Use != CredentialUseOpaqueSecret || len(item.RequiredKeys) == 0 {
			return fmt.Errorf("opaque bundle item requires exact keys")
		}
	case CredentialTypeHelmRepository:
		if item.Use != CredentialUseHelmPull {
			return fmt.Errorf("Helm bundle item use is invalid")
		}
	case CredentialTypeGitToken:
		if item.Use != CredentialUseGitCheckout {
			return fmt.Errorf("Git bundle item use is invalid")
		}
	default:
		return fmt.Errorf("credential bundle item type is invalid")
	}
	return nil
}

func (s *GatewayStore) PromoteCredentialVersion(credentialID, versionID, actorID string) error {
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
	if verification.VerifiedAt.IsZero() {
		verification.VerifiedAt = time.Now().UTC()
	}
	_, err = s.db.Exec(`INSERT INTO credential_verifications
		(id, credential_id, version_id, grant_id, use, request_json, evidence_json, status, verified_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 'verified', ?)
		ON CONFLICT(version_id, grant_id, use) DO UPDATE SET
			id = excluded.id,
			request_json = excluded.request_json,
			evidence_json = excluded.evidence_json,
			status = excluded.status,
			verified_at = excluded.verified_at`,
		verification.ID, verification.CredentialID, verification.VersionID, verification.GrantID,
		verification.Use, string(requestJSON), string(evidenceJSON), formatTime(verification.VerifiedAt))
	return err
}

func (s *GatewayStore) CredentialVerifications(credentialID, versionID string) ([]CredentialVerification, error) {
	rows, err := s.db.Query(`SELECT id, credential_id, version_id, grant_id, use, request_json, evidence_json, status, verified_at
		FROM credential_verifications WHERE credential_id = ? AND version_id = ? AND status = 'verified'
		ORDER BY grant_id, use`, strings.TrimSpace(credentialID), strings.TrimSpace(versionID))
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
	rows, err := s.db.Query(`SELECT id, credential_id, grantee_id, cluster, instance, project, environment, template, namespace,
			uses_json, state, created_by, created_at, revoked_at
		FROM credential_grants WHERE credential_id = ? AND state = ? ORDER BY id`,
		strings.TrimSpace(credentialID), CredentialStateActive)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var grants []CredentialGrant
	for rows.Next() {
		grant, err := scanCredentialGrant(rows)
		if err != nil {
			return nil, err
		}
		grants = append(grants, grant)
	}
	return grants, rows.Err()
}

func (s *GatewayStore) CredentialGrantsForContext(credentialID string, ref CredentialPlanReference, plan CredentialPlan) ([]CredentialGrant, error) {
	grants, err := s.ActiveCredentialGrants(credentialID)
	if err != nil {
		return nil, err
	}
	matches := make([]CredentialGrant, 0, len(grants))
	for _, grant := range grants {
		if grant.Cluster == plan.Cluster && grant.Instance == plan.Instance &&
			grant.Project == plan.Project && grant.Environment == plan.Environment &&
			grant.Template == plan.Template && grant.Namespace == ref.Namespace &&
			containsCredentialUse(grant.Uses, ref.Use) {
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
	if resource.State == CredentialStateActive {
		now := time.Now().UTC()
		if _, err := s.db.Exec(`UPDATE credentials SET state = ?, updated_at = ? WHERE id = ? AND state = ?`,
			CredentialStateRevoking, formatTime(now), resource.ID, CredentialStateActive); err != nil {
			return CredentialResource{}, err
		}
		resource.State = CredentialStateRevoking
		resource.UpdatedAt = now
	}
	return resource, nil
}

func (s *GatewayStore) FinalizeCredentialRevocation(credentialID string) error {
	resource, err := s.credentialByID(credentialID)
	if err != nil {
		return err
	}
	if resource.State == CredentialStateRevoked {
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

func (s *GatewayStore) credentialByID(id string) (CredentialResource, error) {
	row := s.db.QueryRow(`SELECT id, name, scope, owner_id, type, state, created_by, created_at, updated_at, revoked_at
		FROM credentials WHERE id = ?`, strings.TrimSpace(id))
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

func (s *GatewayStore) credentialGrantByIdentity(grant CredentialGrant, usesJSON string) (CredentialGrant, error) {
	row := s.db.QueryRow(`SELECT id, credential_id, grantee_id, cluster, instance, project, environment, template, namespace,
			uses_json, state, created_by, created_at, revoked_at
		FROM credential_grants WHERE credential_id = ? AND grantee_id = ? AND cluster = ? AND instance = ? AND project = ?
			AND environment = ? AND template = ? AND namespace = ? AND uses_json = ?`,
		grant.CredentialID, grant.GranteeID, grant.Cluster, grant.Instance, grant.Project, grant.Environment,
		grant.Template, grant.Namespace, usesJSON)
	return scanCredentialGrant(row)
}

func (s *GatewayStore) credentialGrantByID(credentialID, grantID string) (CredentialGrant, error) {
	row := s.db.QueryRow(`SELECT id, credential_id, grantee_id, cluster, instance, project, environment, template, namespace,
			uses_json, state, created_by, created_at, revoked_at
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
	var usesJSON, createdAt, revokedAt string
	if err := scanner.Scan(&grant.ID, &grant.CredentialID, &grant.GranteeID, &grant.Cluster, &grant.Instance, &grant.Project,
		&grant.Environment, &grant.Template, &grant.Namespace, &usesJSON, &grant.State, &grant.CreatedBy, &createdAt, &revokedAt); err != nil {
		return CredentialGrant{}, err
	}
	if err := json.Unmarshal([]byte(usesJSON), &grant.Uses); err != nil {
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
	var resourceCreatedAt, resourceUpdatedAt, resourceRevokedAt, usesJSON, grantCreatedAt, grantRevokedAt string
	if err := scanner.Scan(
		&resource.ID, &resource.Name, &resource.Scope, &resource.OwnerID, &resource.Type, &resource.State,
		&resource.CreatedBy, &resourceCreatedAt, &resourceUpdatedAt, &resourceRevokedAt,
		&grant.ID, &grant.CredentialID, &grant.GranteeID, &grant.Cluster, &grant.Instance, &grant.Project,
		&grant.Environment, &grant.Template, &grant.Namespace, &usesJSON, &grant.State, &grant.CreatedBy,
		&grantCreatedAt, &grantRevokedAt,
	); err != nil {
		return CredentialResource{}, CredentialGrant{}, err
	}
	if err := json.Unmarshal([]byte(usesJSON), &grant.Uses); err != nil {
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
		return resource.OwnerID == actorID || s.UserHasAction(actorID, action)
	}
	return s.UserHasAction(actorID, action)
}

func validCredentialScope(scope CredentialScope) bool {
	return scope == CredentialScopePersonal || scope == CredentialScopePlatform
}

func validCredentialType(credentialType CredentialType) bool {
	switch credentialType {
	case CredentialTypeRegistry, CredentialTypeTLS, CredentialTypeOpaque, CredentialTypeHelmRepository, CredentialTypeGitToken:
		return true
	default:
		return false
	}
}

func validCredentialUse(use CredentialUse) bool {
	switch use {
	case CredentialUseImagePull, CredentialUseGitCheckout, CredentialUseHelmPull, CredentialUseTLS, CredentialUseOpaqueSecret:
		return true
	default:
		return false
	}
}

func normalizedCredentialUses(uses []CredentialUse) []CredentialUse {
	seen := make(map[CredentialUse]struct{}, len(uses))
	out := make([]CredentialUse, 0, len(uses))
	for _, use := range uses {
		if !validCredentialUse(use) {
			continue
		}
		if _, exists := seen[use]; exists {
			continue
		}
		seen[use] = struct{}{}
		out = append(out, use)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func normalizedCredentialKeys(keys []string) []string {
	seen := make(map[string]struct{}, len(keys))
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func containsCredentialUse(uses []CredentialUse, want CredentialUse) bool {
	for _, use := range uses {
		if use == want {
			return true
		}
	}
	return false
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
