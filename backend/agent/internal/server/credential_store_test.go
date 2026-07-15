package server

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCredentialPayloadRequiresExternalMasterKey(t *testing.T) {
	t.Setenv("DOOPS_GATEWAY_SECRET_KEY", "")
	root := t.TempDir()
	store, err := OpenGatewayStore(filepath.Join(root, "gateway.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	owner, err := store.CreateUser("owner")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	credential, err := store.CreateCredential(CredentialCreateRequest{
		Name:      "registry-main",
		Scope:     CredentialScopePersonal,
		Type:      CredentialTypeRegistry,
		OwnerID:   owner.ID,
		CreatedBy: owner.ID,
	})
	if err != nil {
		t.Fatalf("create credential metadata: %v", err)
	}
	_, err = store.PutCredentialVersion(CredentialVersionPutRequest{
		CredentialID: credential.ID,
		Payload:      json.RawMessage(`{"server":"registry.example.com","username":"owner","password":"canary-secret"}`),
		CreatedBy:    owner.ID,
		Activate:     true,
	})
	if !errors.Is(err, ErrSecretKeyUnavailable) {
		t.Fatalf("put credential without master key error = %v, want ErrSecretKeyUnavailable", err)
	}
	if _, err := os.Stat(filepath.Join(root, "gateway.secret")); !os.IsNotExist(err) {
		t.Fatalf("credential store must not generate a local gateway.secret, stat error: %v", err)
	}
}

func TestCredentialPayloadSchemaIsStrictAndRedacted(t *testing.T) {
	t.Setenv("DOOPS_GATEWAY_SECRET_KEY", strings.Repeat("s", 32))
	store := openCredentialTestStore(t)
	owner := createCredentialTestUser(t, store, "owner")

	tests := []struct {
		name           string
		credentialType CredentialType
		payload        string
	}{
		{
			name:           "registry unknown field",
			credentialType: CredentialTypeRegistry,
			payload:        `{"server":"https://registry.example.com","username":"owner","password":"schema-canary","extra":"forbidden"}`,
		},
		{
			name:           "tls missing private key",
			credentialType: CredentialTypeTLS,
			payload:        `{"certificate":"schema-canary"}`,
		},
		{
			name:           "opaque empty data",
			credentialType: CredentialTypeOpaque,
			payload:        `{"data":{}}`,
		},
		{
			name:           "helm invalid URL",
			credentialType: CredentialTypeHelmRepository,
			payload:        `{"url":"file:///schema-canary","username":"owner","password":"secret"}`,
		},
		{
			name:           "git invalid URL",
			credentialType: CredentialTypeGitToken,
			payload:        `{"url":"file:///schema-canary","username":"owner","token":"secret"}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			credential, err := store.CreateCredential(CredentialCreateRequest{
				Name:      strings.ReplaceAll(test.name, " ", "-"),
				Scope:     CredentialScopePersonal,
				Type:      test.credentialType,
				OwnerID:   owner.ID,
				CreatedBy: owner.ID,
			})
			if err != nil {
				t.Fatalf("create credential: %v", err)
			}
			_, err = store.PutCredentialVersion(CredentialVersionPutRequest{
				CredentialID: credential.ID,
				Payload:      json.RawMessage(test.payload),
				CreatedBy:    owner.ID,
			})
			if !errors.Is(err, ErrCredentialPayloadInvalid) {
				t.Fatalf("payload validation error = %v, want ErrCredentialPayloadInvalid", err)
			}
			if strings.Contains(err.Error(), "schema-canary") {
				t.Fatalf("payload validation error exposed credential material: %v", err)
			}
		})
	}
}

func TestCredentialVersionIsEncryptedBoundAndNeverReturnedFromMetadata(t *testing.T) {
	t.Setenv("DOOPS_GATEWAY_SECRET_KEY", strings.Repeat("k", 32))
	store := openCredentialTestStore(t)
	owner := createCredentialTestUser(t, store, "owner")

	credential, err := store.CreateCredential(CredentialCreateRequest{
		Name:      "registry-main",
		Scope:     CredentialScopePersonal,
		Type:      CredentialTypeRegistry,
		OwnerID:   owner.ID,
		CreatedBy: owner.ID,
	})
	if err != nil {
		t.Fatalf("create credential: %v", err)
	}
	payload := json.RawMessage(`{"server":"registry.example.com","username":"owner","password":"canary-secret"}`)
	version, err := store.PutCredentialVersion(CredentialVersionPutRequest{
		CredentialID: credential.ID,
		Payload:      payload,
		CreatedBy:    owner.ID,
		Activate:     true,
	})
	if err != nil {
		t.Fatalf("put credential version: %v", err)
	}
	if version.State != CredentialVersionActive || version.PayloadDigest == "" {
		t.Fatalf("unexpected active version metadata: %#v", version)
	}

	var ciphertext string
	if err := store.db.QueryRow(`SELECT payload_ciphertext FROM credential_versions WHERE id = ?`, version.ID).Scan(&ciphertext); err != nil {
		t.Fatalf("read ciphertext: %v", err)
	}
	if ciphertext == "" || strings.Contains(ciphertext, "canary-secret") {
		t.Fatalf("credential payload was not encrypted")
	}

	listed, err := store.ListCredentials(CredentialListFilter{ActorID: owner.ID})
	if err != nil {
		t.Fatalf("list credentials: %v", err)
	}
	serialized, err := json.Marshal(listed)
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}
	if strings.Contains(string(serialized), "canary-secret") || strings.Contains(string(serialized), "payload_ciphertext") {
		t.Fatalf("credential metadata exposed payload material: %s", serialized)
	}

	resolved, err := store.ResolveCredentialPayload(credential.ID, version.ID)
	if err != nil {
		t.Fatalf("resolve payload: %v", err)
	}
	if string(resolved) != string(payload) {
		t.Fatalf("resolved payload mismatch")
	}

	other, err := store.CreateCredential(CredentialCreateRequest{
		Name:      "registry-other",
		Scope:     CredentialScopePersonal,
		Type:      CredentialTypeRegistry,
		OwnerID:   owner.ID,
		CreatedBy: owner.ID,
	})
	if err != nil {
		t.Fatalf("create second credential: %v", err)
	}
	if _, err := store.db.Exec(`UPDATE credential_versions SET credential_id = ? WHERE id = ?`, other.ID, version.ID); err != nil {
		t.Fatalf("transplant ciphertext: %v", err)
	}
	if _, err := store.ResolveCredentialPayload(other.ID, version.ID); err == nil {
		t.Fatal("ciphertext transplanted to another credential must fail AAD authentication")
	}
}

func TestCredentialBundleResolvesMetadataAndNamedAuthorizationRejectsAmbiguity(t *testing.T) {
	t.Setenv("DOOPS_GATEWAY_SECRET_KEY", strings.Repeat("b", 32))
	store := openCredentialTestStore(t)
	admin := createCredentialTestUser(t, store, "admin")
	deployer := createCredentialTestUser(t, store, "deployer")
	if err := store.GrantUser(admin.ID, ScopeGrant{
		Cluster: "*", Instance: "*",
		Actions: []GatewayAction{ActionCredentialGrant, ActionCredentialRotate, ActionCredentialRevoke},
	}); err != nil {
		t.Fatalf("grant credential administration: %v", err)
	}

	var credentials []CredentialResource
	for index := 0; index < 2; index++ {
		createRequest := CredentialCreateRequest{
			Name: "registry", Scope: CredentialScopePlatform, Type: CredentialTypeRegistry, CreatedBy: admin.ID,
		}
		versionActor := admin.ID
		grantActor := admin.ID
		if index == 1 {
			createRequest.Scope = CredentialScopePersonal
			createRequest.OwnerID = deployer.ID
			createRequest.CreatedBy = deployer.ID
			versionActor = deployer.ID
			grantActor = deployer.ID
		}
		credential, err := store.CreateCredential(createRequest)
		if err != nil {
			t.Fatalf("create credential %d: %v", index, err)
		}
		if _, err := store.PutCredentialVersion(CredentialVersionPutRequest{
			CredentialID: credential.ID,
			Payload: json.RawMessage(
				`{"server":"registry.example.com","username":"owner","password":"canary-secret"}`,
			),
			CreatedBy: versionActor,
			Activate:  true,
		}); err != nil {
			t.Fatalf("put credential version %d: %v", index, err)
		}
		grant, err := store.CreateCredentialGrant(CredentialGrantCreateRequest{
			CredentialID: credential.ID,
			GranteeID:    deployer.ID,
			Cluster:      "cluster",
			Instance:     "instance",
			Project:      "project",
			Environment:  "production",
			Template:     "release",
			Namespace:    "apps",
			Uses:         []CredentialUse{CredentialUseImagePull},
			CreatedBy:    grantActor,
		})
		if err != nil {
			t.Fatalf("create grant %d: %v", index, err)
		}
		if index == 0 {
			t.Cleanup(func() {
				_, _ = store.RevokeCredentialGrant(credential.ID, grant.ID, admin.ID)
			})
		}
		credentials = append(credentials, credential)
	}

	bundle, err := store.CreateCredentialBundle(CredentialBundleCreateRequest{
		Name: "release-shared", Scope: CredentialScopePlatform, CreatedBy: admin.ID,
		Items: []CredentialBundleItem{{
			CredentialID:       credentials[0].ID,
			Use:                CredentialUseImagePull,
			Namespace:          "apps",
			Workload:           CredentialPlanWorkload{Kind: "Deployment", Name: "api"},
			RegistryRepository: "team/api",
			RegistryReference:  "sha256:manifest",
		}},
	})
	if err != nil {
		t.Fatalf("create bundle: %v", err)
	}
	resolved, err := store.ResolveCredentialBundle(deployer.ID, bundle.Name)
	if err != nil {
		t.Fatalf("resolve bundle: %v", err)
	}
	serialized, _ := json.Marshal(resolved)
	if len(resolved.Items) != 1 || resolved.Items[0].CredentialID != credentials[0].ID ||
		strings.Contains(string(serialized), "canary-secret") {
		t.Fatalf("unexpected bundle metadata: %s", serialized)
	}

	request := CredentialUseRequest{
		ActorID: deployer.ID, Credential: "registry", Cluster: "cluster", Instance: "instance",
		Project: "project", Environment: "production", Template: "release", Namespace: "apps",
		Use: CredentialUseImagePull,
	}
	if _, err := store.AuthorizeCredentialUse(request); !errors.Is(err, ErrCredentialAmbiguous) {
		t.Fatalf("named authorization error = %v, want ErrCredentialAmbiguous", err)
	}
	request.CredentialID = credentials[0].ID
	if authorized, err := store.AuthorizeCredentialUse(request); err != nil || authorized.Resource.ID != credentials[0].ID {
		t.Fatalf("ID-bound authorization = %#v, %v", authorized, err)
	}
	grants, err := store.ActiveCredentialGrants(credentials[0].ID)
	if err != nil || len(grants) != 1 {
		t.Fatalf("active grants = %#v, %v", grants, err)
	}
	if _, err := store.RevokeCredentialGrant(credentials[0].ID, grants[0].ID, admin.ID); err != nil {
		t.Fatalf("revoke grant: %v", err)
	}
	if _, err := store.AuthorizeCredentialUse(request); !errors.Is(err, ErrCredentialGrantDenied) {
		t.Fatalf("authorization after grant revoke = %v, want ErrCredentialGrantDenied", err)
	}
	restored, err := store.CreateCredentialGrant(CredentialGrantCreateRequest{
		CredentialID: credentials[0].ID,
		GranteeID:    deployer.ID,
		Cluster:      "cluster",
		Instance:     "instance",
		Project:      "project",
		Environment:  "production",
		Template:     "release",
		Namespace:    "apps",
		Uses:         []CredentialUse{CredentialUseImagePull},
		CreatedBy:    admin.ID,
	})
	if err != nil || restored.State != CredentialStateActive {
		t.Fatalf("restore revoked grant = %#v, %v", restored, err)
	}
	if _, err := store.AuthorizeCredentialUse(request); err != nil {
		t.Fatalf("authorization after grant restore: %v", err)
	}
}

func TestCredentialGrantMustMatchEveryDeploymentDimension(t *testing.T) {
	t.Setenv("DOOPS_GATEWAY_SECRET_KEY", strings.Repeat("g", 32))
	store := openCredentialTestStore(t)
	owner := createCredentialTestUser(t, store, "owner")
	deployer := createCredentialTestUser(t, store, "deployer")
	if err := store.GrantUser(owner.ID, ScopeGrant{
		Cluster: "*", Instance: "*", Actions: []GatewayAction{ActionCredentialGrant, ActionCredentialRotate},
	}); err != nil {
		t.Fatalf("grant credential administration: %v", err)
	}

	credential, err := store.CreateCredential(CredentialCreateRequest{
		Name:      "platform-registry",
		Scope:     CredentialScopePlatform,
		Type:      CredentialTypeRegistry,
		CreatedBy: owner.ID,
	})
	if err != nil {
		t.Fatalf("create platform credential: %v", err)
	}
	version, err := store.PutCredentialVersion(CredentialVersionPutRequest{
		CredentialID: credential.ID,
		Payload:      json.RawMessage(`{"server":"registry.example.com","username":"platform","password":"canary-secret"}`),
		CreatedBy:    owner.ID,
		Activate:     true,
	})
	if err != nil {
		t.Fatalf("put active version: %v", err)
	}
	grant, err := store.CreateCredentialGrant(CredentialGrantCreateRequest{
		CredentialID: credential.ID,
		GranteeID:    deployer.ID,
		Cluster:      "doops-edu",
		Instance:     "edu-coder",
		Project:      "oilan",
		Environment:  "production",
		Template:     "oilan-agent-release",
		Namespace:    "kz-ops",
		Uses:         []CredentialUse{CredentialUseImagePull},
		CreatedBy:    owner.ID,
	})
	if err != nil {
		t.Fatalf("create credential grant: %v", err)
	}

	request := CredentialUseRequest{
		ActorID:     deployer.ID,
		Credential:  credential.Name,
		Cluster:     grant.Cluster,
		Instance:    grant.Instance,
		Project:     grant.Project,
		Environment: grant.Environment,
		Template:    grant.Template,
		Namespace:   grant.Namespace,
		Use:         CredentialUseImagePull,
	}
	resolved, err := store.AuthorizeCredentialUse(request)
	if err != nil {
		t.Fatalf("authorize exact credential use: %v", err)
	}
	if resolved.Version.ID != version.ID || resolved.Grant.ID != grant.ID {
		t.Fatalf("unexpected credential resolution: %#v", resolved)
	}

	tests := []struct {
		name   string
		mutate func(*CredentialUseRequest)
	}{
		{name: "actor", mutate: func(r *CredentialUseRequest) { r.ActorID = owner.ID }},
		{name: "cluster", mutate: func(r *CredentialUseRequest) { r.Cluster = "other" }},
		{name: "instance", mutate: func(r *CredentialUseRequest) { r.Instance = "other" }},
		{name: "project", mutate: func(r *CredentialUseRequest) { r.Project = "other" }},
		{name: "environment", mutate: func(r *CredentialUseRequest) { r.Environment = "staging" }},
		{name: "template", mutate: func(r *CredentialUseRequest) { r.Template = "other" }},
		{name: "namespace", mutate: func(r *CredentialUseRequest) { r.Namespace = "other" }},
		{name: "use", mutate: func(r *CredentialUseRequest) { r.Use = CredentialUseGitCheckout }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changed := request
			test.mutate(&changed)
			if _, err := store.AuthorizeCredentialUse(changed); !errors.Is(err, ErrCredentialGrantDenied) {
				t.Fatalf("mismatched %s error = %v, want ErrCredentialGrantDenied", test.name, err)
			}
		})
	}
}

func TestCredentialStagedVersionRequiresPromotionAndRevocationFailsClosed(t *testing.T) {
	t.Setenv("DOOPS_GATEWAY_SECRET_KEY", strings.Repeat("r", 32))
	store := openCredentialTestStore(t)
	owner := createCredentialTestUser(t, store, "owner")

	credential, err := store.CreateCredential(CredentialCreateRequest{
		Name:      "personal-git",
		Scope:     CredentialScopePersonal,
		Type:      CredentialTypeGitToken,
		OwnerID:   owner.ID,
		CreatedBy: owner.ID,
	})
	if err != nil {
		t.Fatalf("create credential: %v", err)
	}
	active, err := store.PutCredentialVersion(CredentialVersionPutRequest{
		CredentialID: credential.ID,
		Payload:      json.RawMessage(`{"url":"https://example.com/repo.git","username":"owner","token":"old-canary"}`),
		CreatedBy:    owner.ID,
		Activate:     true,
	})
	if err != nil {
		t.Fatalf("put active version: %v", err)
	}
	staged, err := store.PutCredentialVersion(CredentialVersionPutRequest{
		CredentialID: credential.ID,
		Payload:      json.RawMessage(`{"url":"https://example.com/repo.git","username":"owner","token":"new-canary"}`),
		CreatedBy:    owner.ID,
	})
	if err != nil {
		t.Fatalf("put staged version: %v", err)
	}
	if staged.State != CredentialVersionStaged {
		t.Fatalf("new version state = %q, want staged", staged.State)
	}
	current, err := store.ActiveCredentialVersion(credential.ID)
	if err != nil {
		t.Fatalf("get active version: %v", err)
	}
	if current.ID != active.ID {
		t.Fatalf("staged version became active before promotion: %#v", current)
	}
	if err := store.PromoteCredentialVersion(credential.ID, staged.ID, owner.ID); err != nil {
		t.Fatalf("promote staged version: %v", err)
	}
	current, err = store.ActiveCredentialVersion(credential.ID)
	if err != nil {
		t.Fatalf("get promoted version: %v", err)
	}
	if current.ID != staged.ID {
		t.Fatalf("active version = %s, want %s", current.ID, staged.ID)
	}
	if err := store.RevokeCredential(credential.ID, owner.ID); err != nil {
		t.Fatalf("revoke credential: %v", err)
	}
	if _, err := store.ActiveCredentialVersion(credential.ID); !errors.Is(err, ErrCredentialRevoked) {
		t.Fatalf("active version after revoke error = %v, want ErrCredentialRevoked", err)
	}
}

func TestPlatformCredentialAdministratorCanManageResourceCreatedByAnotherAdmin(t *testing.T) {
	t.Setenv("DOOPS_GATEWAY_SECRET_KEY", strings.Repeat("m", 32))
	store := openCredentialTestStore(t)
	creator := createCredentialTestUser(t, store, "creator")
	operator := createCredentialTestUser(t, store, "operator")
	deployer := createCredentialTestUser(t, store, "deployer")
	if err := store.GrantUser(operator.ID, ScopeGrant{
		Cluster: "*", Instance: "*",
		Actions: []GatewayAction{ActionCredentialGrant, ActionCredentialRotate, ActionCredentialRevoke},
	}); err != nil {
		t.Fatalf("grant credential administration: %v", err)
	}

	credential, err := store.CreateCredential(CredentialCreateRequest{
		Name: "shared-registry", Scope: CredentialScopePlatform, Type: CredentialTypeRegistry, CreatedBy: creator.ID,
	})
	if err != nil {
		t.Fatalf("create platform credential: %v", err)
	}
	if _, err := store.PutCredentialVersion(CredentialVersionPutRequest{
		CredentialID: credential.ID,
		Payload:      json.RawMessage(`{"server":"registry.example.com","username":"creator","password":"denied"}`),
		CreatedBy:    creator.ID,
		Activate:     true,
	}); !errors.Is(err, ErrCredentialForbidden) {
		t.Fatalf("platform creator without current rotate permission = %v, want ErrCredentialForbidden", err)
	}
	version, err := store.PutCredentialVersion(CredentialVersionPutRequest{
		CredentialID: credential.ID,
		Payload:      json.RawMessage(`{"server":"registry.example.com","username":"operator","password":"admin-canary"}`),
		CreatedBy:    operator.ID,
		Activate:     true,
	})
	if err != nil {
		t.Fatalf("operator put platform credential: %v", err)
	}
	if _, err := store.CreateCredentialGrant(CredentialGrantCreateRequest{
		CredentialID: credential.ID,
		GranteeID:    deployer.ID,
		Cluster:      "doops-edu",
		Instance:     "edu-coder",
		Project:      "oilan",
		Environment:  "production",
		Template:     "oilan-agent-release",
		Namespace:    "kz-ops",
		Uses:         []CredentialUse{CredentialUseImagePull},
		CreatedBy:    operator.ID,
	}); err != nil {
		t.Fatalf("operator grant platform credential: %v", err)
	}
	if err := store.RevokeCredential(credential.ID, operator.ID); err != nil {
		t.Fatalf("operator revoke platform credential: %v", err)
	}
	if _, err := store.ResolveCredentialPayload(credential.ID, version.ID); !errors.Is(err, ErrCredentialRevoked) {
		t.Fatalf("revoked platform credential resolution error = %v", err)
	}
}

func openCredentialTestStore(t *testing.T) *GatewayStore {
	t.Helper()
	store, err := OpenGatewayStore(filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatalf("open gateway store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func createCredentialTestUser(t *testing.T, store *GatewayStore, name string) GatewayUser {
	t.Helper()
	user, err := store.CreateUser(name)
	if err != nil {
		t.Fatalf("create user %s: %v", name, err)
	}
	return user
}
