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

func TestCredentialMetadataAndTargetGrantPersistAcrossGatewayRestart(t *testing.T) {
	t.Setenv("DOOPS_GATEWAY_SECRET_KEY", strings.Repeat("p", 32))
	databasePath := filepath.Join(t.TempDir(), "gateway.db")
	store, err := OpenGatewayStore(databasePath)
	if err != nil {
		t.Fatalf("open credential store: %v", err)
	}
	owner := createCredentialTestUser(t, store, "persistent-owner")
	resource, err := store.CreateCredential(CredentialCreateRequest{
		Name: "persistent-registry", Scope: CredentialScopePersonal, Type: CredentialTypeRegistry,
		OwnerID: owner.ID, CreatedBy: owner.ID,
	})
	if err != nil {
		t.Fatalf("create persistent credential: %v", err)
	}
	if _, err := store.PutCredentialVersion(CredentialVersionPutRequest{
		CredentialID: resource.ID,
		Payload: json.RawMessage(
			`{"server":"registry.example.com","username":"owner","password":"synthetic"}`,
		),
		CreatedBy: owner.ID,
		Activate:  true,
	}); err != nil {
		t.Fatalf("activate persistent credential version: %v", err)
	}
	scope := credentialStoreImageScope("cluster-a", "instance-a", "apps")
	if _, err := store.CreateCredentialGrant(CredentialGrantCreateRequest{
		CredentialID: resource.ID,
		Scope:        scope,
		CreatedBy:    owner.ID,
	}); err != nil {
		t.Fatalf("create persistent credential grant: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close credential store: %v", err)
	}

	reopened, err := OpenGatewayStore(databasePath)
	if err != nil {
		t.Fatalf("reopen credential store: %v", err)
	}
	defer reopened.Close()
	managed, err := reopened.ListCredentials(CredentialListFilter{ActorID: owner.ID})
	if err != nil || len(managed) != 1 || managed[0].ID != resource.ID {
		t.Fatalf("managed credentials after restart = %#v, %v", managed, err)
	}
	visible, err := reopened.ListCredentialAccess(owner.ID, "cluster-a", "instance-a")
	if err != nil || len(visible) != 1 || visible[0].ID != resource.ID ||
		len(visible[0].Grants) != 1 || !credentialScopeEqual(visible[0].Grants[0].Scope, scope) {
		t.Fatalf("target credentials after restart = %#v, %v", visible, err)
	}
	otherTarget, err := reopened.ListCredentialAccess(owner.ID, "cluster-b", "instance-b")
	if err != nil || len(otherTarget) != 0 {
		t.Fatalf("other target credentials after restart = %#v, %v", otherTarget, err)
	}
}

func TestCredentialGrantChangesInvalidateVerification(t *testing.T) {
	t.Setenv("DOOPS_GATEWAY_SECRET_KEY", strings.Repeat("c", 32))
	store := openCredentialTestStore(t)
	owner := createCredentialTestUser(t, store, "verification-owner")
	resource, err := store.CreateCredential(CredentialCreateRequest{
		Name: "verification-registry", Scope: CredentialScopePersonal, Type: CredentialTypeRegistry,
		OwnerID: owner.ID, CreatedBy: owner.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	version, err := store.PutCredentialVersion(CredentialVersionPutRequest{
		CredentialID: resource.ID, CreatedBy: owner.ID,
		Payload: json.RawMessage(`{"server":"registry.example.com","username":"owner","password":"synthetic"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	grantRequest := CredentialGrantCreateRequest{
		CredentialID: resource.ID, CreatedBy: owner.ID,
		Scope: credentialStoreImageScope("cluster-a", "instance-a", "apps"),
	}
	grant, err := store.CreateCredentialGrant(grantRequest)
	if err != nil {
		t.Fatal(err)
	}
	verify := func() {
		t.Helper()
		if err := store.RecordCredentialVerification(CredentialVerification{
			CredentialID: resource.ID, VersionID: version.ID, GrantID: grant.ID, Use: CredentialUseImagePull,
			Request:  CredentialMaterializeRequest{Scope: grant.Scope},
			Evidence: CredentialMaterialization{Status: "verified"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	assertInvalidated := func() {
		t.Helper()
		verifications, err := store.CredentialVerifications(resource.ID, version.ID)
		if err != nil || len(verifications) != 0 {
			t.Fatalf("expected no current verification, got %d, %v", len(verifications), err)
		}
	}
	verify()
	grantRequest.Scope.Namespace = "other-apps"
	updated, err := store.CreateCredentialGrant(grantRequest)
	if err != nil || updated.ID != grant.ID {
		t.Fatalf("update binding: %v", err)
	}
	assertInvalidated()
	grant = updated
	verify()
	if _, err := store.RevokeCredentialGrant(resource.ID, grant.ID, owner.ID); err != nil {
		t.Fatal(err)
	}
	regranted, err := store.CreateCredentialGrant(grantRequest)
	if err != nil || regranted.ID != grant.ID {
		t.Fatalf("reactivate binding: %v", err)
	}
	assertInvalidated()
}

func TestCredentialTargetListRequiresActiveVersion(t *testing.T) {
	t.Setenv("DOOPS_GATEWAY_SECRET_KEY", strings.Repeat("v", 32))
	store := openCredentialTestStore(t)
	owner := createCredentialTestUser(t, store, "version-owner")
	resource, err := store.CreateCredential(CredentialCreateRequest{
		Name: "version-gated", Scope: CredentialScopePersonal, Type: CredentialTypeRegistry,
		OwnerID: owner.ID, CreatedBy: owner.ID,
	})
	if err != nil {
		t.Fatalf("create credential: %v", err)
	}
	if _, err := store.CreateCredentialGrant(CredentialGrantCreateRequest{
		CredentialID: resource.ID,
		Scope:        credentialStoreImageScope("cluster-a", "instance-a", "apps"),
		CreatedBy:    owner.ID,
	}); err != nil {
		t.Fatalf("create grant: %v", err)
	}
	visible, err := store.ListCredentialAccess(owner.ID, "cluster-a", "instance-a")
	if err != nil || len(visible) != 0 {
		t.Fatalf("credential without version was target-visible: %#v, %v", visible, err)
	}
	if _, err := store.PutCredentialVersion(CredentialVersionPutRequest{
		CredentialID: resource.ID,
		Payload: json.RawMessage(
			`{"server":"registry.example.com","username":"owner","password":"synthetic"}`,
		),
		CreatedBy: owner.ID,
	}); err != nil {
		t.Fatalf("put staged version: %v", err)
	}
	visible, err = store.ListCredentialAccess(owner.ID, "cluster-a", "instance-a")
	if err != nil || len(visible) != 0 {
		t.Fatalf("credential with staged version was target-visible: %#v, %v", visible, err)
	}
	if _, err := store.PutCredentialVersion(CredentialVersionPutRequest{
		CredentialID: resource.ID,
		Payload: json.RawMessage(
			`{"server":"registry.example.com","username":"owner","password":"synthetic-active"}`,
		),
		CreatedBy: owner.ID,
		Activate:  true,
	}); err != nil {
		t.Fatalf("activate version: %v", err)
	}
	visible, err = store.ListCredentialAccess(owner.ID, "cluster-a", "instance-a")
	if err != nil || len(visible) != 1 || visible[0].ID != resource.ID {
		t.Fatalf("credential with active version not target-visible: %#v, %v", visible, err)
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
	if err := store.GrantUser(deployer.ID, ScopeGrant{
		Cluster: "cluster", Instance: "instance",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetUserAdmin(admin.ID, true); err != nil {
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
			Scope:        credentialStoreImageScope("cluster", "instance", "apps"),
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
			CredentialID: credentials[0].ID,
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
		ActorID: deployer.ID, Credential: "registry",
		Scope: credentialStoreImageScope("cluster", "instance", "apps"),
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
		Scope:        credentialStoreImageScope("cluster", "instance", "apps"),
		CreatedBy:    admin.ID,
	})
	if err != nil || restored.State != CredentialStateActive {
		t.Fatalf("restore revoked grant = %#v, %v", restored, err)
	}
	if _, err := store.AuthorizeCredentialUse(request); err != nil {
		t.Fatalf("authorization after grant restore: %v", err)
	}
}

func TestCredentialUseInheritsTargetAuthorityWithoutDeploymentScope(t *testing.T) {
	t.Setenv("DOOPS_GATEWAY_SECRET_KEY", strings.Repeat("g", 32))
	store := openCredentialTestStore(t)
	owner := createCredentialTestUser(t, store, "owner")
	deployer := createCredentialTestUser(t, store, "deployer")
	if err := store.GrantUser(deployer.ID, ScopeGrant{
		Cluster: "doops-edu", Instance: "edu-coder",
	}); err != nil {
		t.Fatal(err)
	}
	outsider := createCredentialTestUser(t, store, "outsider")
	if err := store.SetUserAdmin(owner.ID, true); err != nil {
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
		Scope:        credentialStoreImageScope("doops-edu", "edu-coder", "kz-ops"),
		CreatedBy:    owner.ID,
	})
	if err != nil {
		t.Fatalf("create credential grant: %v", err)
	}

	request := CredentialUseRequest{
		ActorID: deployer.ID, Credential: credential.Name, Scope: grant.Scope,
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
		{name: "actor", mutate: func(r *CredentialUseRequest) { r.ActorID = outsider.ID }},
		{name: "cluster", mutate: func(r *CredentialUseRequest) { r.Scope.Cluster = "other" }},
		{name: "instance", mutate: func(r *CredentialUseRequest) { r.Scope.Instance = "other" }},
		{name: "namespace", mutate: func(r *CredentialUseRequest) { r.Scope.Namespace = "other" }},
		{name: "use", mutate: func(r *CredentialUseRequest) { r.Scope.Use = CredentialUseGitCheckout }},
		{name: "workload kind", mutate: func(r *CredentialUseRequest) { r.Scope.Workload.Kind = "job" }},
		{name: "workload name", mutate: func(r *CredentialUseRequest) { r.Scope.Workload.Name = "other" }},
		{name: "image repository", mutate: func(r *CredentialUseRequest) { r.Scope.ImageRepository = "team/other" }},
		{name: "image digest", mutate: func(r *CredentialUseRequest) { r.Scope.ImageDigest = "sha256:" + strings.Repeat("b", 64) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changed := request
			test.mutate(&changed)
			wantErr := ErrCredentialGrantDenied
			if test.name == "actor" || test.name == "cluster" || test.name == "instance" {
				wantErr = ErrCredentialForbidden
			}
			if test.name != "actor" && test.name != "cluster" && test.name != "instance" {
				wantErr = nil
			}
			if _, err := store.AuthorizeCredentialUse(changed); !errors.Is(err, wantErr) {
				t.Fatalf("mismatched %s error = %v, want %v", test.name, err, wantErr)
			}
		})
	}
	request.Scope = CredentialGrantScope{Cluster: "doops-edu", Instance: "edu-coder", Use: CredentialUseImagePull}
	if _, err := store.AuthorizeCredentialUse(request); err != nil {
		t.Fatalf("target/use alone must resolve the configured credential: %v", err)
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
	if err := store.SetUserAdmin(operator.ID, true); err != nil {
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
		Scope:        credentialStoreImageScope("doops-edu", "edu-coder", "kz-ops"),
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

func TestCredentialAdministratorCanRecoverAnotherUsersPersonalCredential(t *testing.T) {
	t.Setenv("DOOPS_GATEWAY_SECRET_KEY", strings.Repeat("m", 32))
	store := openCredentialTestStore(t)
	owner := createCredentialTestUser(t, store, "owner")
	operator := createCredentialTestUser(t, store, "operator")
	if err := store.SetUserAdmin(operator.ID, true); err != nil {
		t.Fatalf("grant credential administration: %v", err)
	}

	credential, err := store.CreateCredential(CredentialCreateRequest{
		Name: "personal-registry", Scope: CredentialScopePersonal, Type: CredentialTypeRegistry,
		OwnerID: owner.ID, CreatedBy: owner.ID,
	})
	if err != nil {
		t.Fatalf("create personal credential: %v", err)
	}
	version, err := store.PutCredentialVersion(CredentialVersionPutRequest{
		CredentialID: credential.ID,
		Payload:      json.RawMessage(`{"server":"registry.example.com","username":"operator","password":"synthetic-canary"}`),
		CreatedBy:    operator.ID,
		Activate:     true,
	})
	if err != nil || version.State != CredentialVersionActive {
		t.Fatalf("operator rotate personal credential = %#v, %v", version, err)
	}
	grant, err := store.CreateCredentialGrant(CredentialGrantCreateRequest{
		CredentialID: credential.ID,
		Scope:        credentialStoreImageScope("doops-edu", "edu-coder", "kz-ops"),
		CreatedBy:    operator.ID,
	})
	if err != nil || grant.State != CredentialStateActive {
		t.Fatalf("operator grant personal credential = %#v, %v", grant, err)
	}
	if err := store.RevokeCredential(credential.ID, operator.ID); err != nil {
		t.Fatalf("operator revoke personal credential: %v", err)
	}
}

func TestCredentialGrantScopeRejectsUnsupportedWorkloadKinds(t *testing.T) {
	for _, kind := range []string{"job", "cronjob"} {
		t.Run(kind, func(t *testing.T) {
			scope := credentialStoreImageScope("doops-edu", "edu-coder", "kz-ops")
			scope.Workload.Kind = kind
			if err := validateCredentialGrantScope(scope); err == nil ||
				err.Error() != "credential_scope_invalid" {
				t.Fatalf("validate %s workload scope error = %v", kind, err)
			}
		})
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

func credentialStoreImageScope(cluster, instance, namespace string) CredentialGrantScope {
	return CredentialGrantScope{
		Cluster: cluster, Instance: instance, Namespace: namespace,
		Use: CredentialUseImagePull,
		Workload: CredentialPlanWorkload{
			Kind: "deployment", Name: "api",
		},
		ImageRepository: "team/api",
		ImageDigest:     "sha256:" + strings.Repeat("a", 64),
	}
}

func createCredentialTestUser(t *testing.T, store *GatewayStore, name string) GatewayUser {
	t.Helper()
	user, err := store.CreateUser(name)
	if err != nil {
		t.Fatalf("create user %s: %v", name, err)
	}
	return user
}
