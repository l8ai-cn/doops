package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// freeTextAuditTarget is a cluster value that is not a plain identifier: the
// audit target filters must leave it out of every audit column.
const freeTextAuditTarget = "dev-probe=request text"

// startClusterAgent runs a real in-process Agent for a target whose cluster is
// free text, so a verify or apply can actually reach it and succeed.
func startClusterAgent(t *testing.T, f *executionLifecycleFixture, cluster string) {
	t.Helper()
	gatewayAgent := &Gateway{
		tasks:      make(map[string]*bgTask),
		sessionMap: make(map[string]*sessionEntry),
	}
	conn := dialFakeGatewayAgent(t, f.server.URL, url.QueryEscape(cluster), "local")
	done := make(chan struct{})
	go func() {
		defer close(done)
		gatewayAgent.ServeGatewayWebSocketConnWithReady(conn, "credential-audit-target", nil)
	}()
	t.Cleanup(func() {
		_ = conn.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("audit target Agent did not stop")
		}
		requireEventually(t, 3*time.Second, func() bool { return f.hub.client.registry.Get(cluster, "local") == nil })
	})
	waitForGatewayAgent(t, f.hub, cluster, "local")
}

// createAuditTargetCredential creates a platform credential with a staged
// version and one active grant on scope, and returns all three.
func createAuditTargetCredential(
	t *testing.T,
	f *executionLifecycleFixture,
	scope CredentialGrantScope,
) (CredentialResource, CredentialVersion, CredentialGrant) {
	t.Helper()
	var resource CredentialResource
	if err := json.Unmarshal(f.http(t, f.admin, http.MethodPost, "/v1/credentials",
		`{"name":"audit-target","scope":"platform","type":"opaque"}`, http.StatusOK), &resource); err != nil {
		t.Fatal(err)
	}
	base := "/v1/credentials/" + resource.ID
	var version CredentialVersion
	if err := json.Unmarshal(f.http(t, f.admin, http.MethodPut, base+"/payload",
		`{"data":{"token":"`+revocationCanary+`-audit"}}`, http.StatusOK), &version); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{"scope": scope})
	if err != nil {
		t.Fatal(err)
	}
	var grant CredentialGrant
	if err := json.Unmarshal(f.http(t, f.admin, http.MethodPost, base+"/grants", string(body), http.StatusOK), &grant); err != nil {
		t.Fatal(err)
	}
	return resource, version, grant
}

// assertAuditEventHasNoRequestText fails when the request text or the payload
// canary appears in any column of the audit event.
func assertAuditEventHasNoRequestText(t *testing.T, event AuditEvent) {
	t.Helper()
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"audit event": string(encoded), "cluster": event.Cluster,
		"instance": event.Instance, "command summary": event.CommandSummary,
	} {
		if strings.Contains(value, freeTextAuditTarget) {
			t.Fatalf("audit %s carries the request text: %q", name, value)
		}
		if strings.Contains(value, revocationCanary) {
			t.Fatalf("audit %s carries the payload canary: %q", name, value)
		}
	}
}

// A platform admin can bind a credential to a cluster name that is not an
// identifier. Revoking that grant succeeds, and the success audit must not put
// the request text in the target or scope columns.
func TestCredentialGrantRevokeSuccessAuditFiltersRequestTextTarget(t *testing.T) {
	// Given a platform credential with a grant whose cluster is request text
	f := newRevocationFixture(t)
	grantResponse := f.wait(t, f.send(t, http.MethodPost, "/v1/credentials/"+f.resource.ID+"/grants",
		`{"scope":{"cluster":"`+freeTextAuditTarget+`","instance":"local","namespace":"apps"}}`))
	if grantResponse.code != http.StatusOK {
		t.Fatalf("grant = HTTP %d %q", grantResponse.code, grantResponse.body)
	}
	var grant CredentialGrant
	if err := json.Unmarshal([]byte(grantResponse.body), &grant); err != nil || grant.ID == "" {
		t.Fatalf("grant response = %q, %v", grantResponse.body, err)
	}
	// When the grant is revoked and reaches revoked
	revokeBody, err := json.Marshal(map[string]string{"grant_id": grant.ID})
	if err != nil {
		t.Fatal(err)
	}
	revoke := f.wait(t, f.send(t, http.MethodDelete, "/v1/credentials/"+f.resource.ID+"/grants", string(revokeBody)))
	if revoke.code != http.StatusOK {
		t.Fatalf("revoke = HTTP %d %q", revoke.code, revoke.body)
	}
	// Then the revoke success audit keeps no target and no stored scope
	events, err := f.store.ListAuditFiltered(AuditFilter{Action: ActionCredentialGrant, Limit: 1})
	if err != nil || len(events) != 1 {
		t.Fatalf("grant audit: %v, %#v", err, events)
	}
	event := events[0]
	if event.Status != "success" || event.Cluster != "" || event.Instance != "" || event.Scope != nil {
		t.Fatalf("revoke success audit = %#v", event)
	}
	assertAuditEventHasNoRequestText(t, event)
}

// Verify binds a target by name; when that name is request text and the
// verification succeeds on a real Agent, the success audit must not carry it.
// The identifier path keeps recording the target and the stored scope.
func TestCredentialVerifySuccessAuditFiltersRequestTextTarget(t *testing.T) {
	for _, tc := range []struct {
		name         string
		cluster      string
		wantIdentifiers bool
	}{
		{name: "request text target", cluster: freeTextAuditTarget},
		{name: "identifier target", cluster: "dev", wantIdentifiers: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Given a credential bound to a target, with a real Agent there
			f, _ := credentialImportFixture(t)
			if !tc.wantIdentifiers {
				startClusterAgent(t, f, tc.cluster)
			}
			resource, version, _ := createAuditTargetCredential(t, f,
				CredentialGrantScope{Cluster: tc.cluster, Instance: "local", Namespace: "apps"})
			body, err := json.Marshal(map[string]any{
				"session_id": "audit-target-verify", "version_id": version.ID,
				"scope": map[string]string{"cluster": tc.cluster, "instance": "local"},
			})
			if err != nil {
				t.Fatal(err)
			}
			// When the verification succeeds
			f.http(t, f.admin, http.MethodPost, "/v1/credentials/"+resource.ID+"/verify", string(body), http.StatusOK)
			// Then the success audit filters the target when it is request text
			events, err := f.store.ListAuditFiltered(AuditFilter{Action: ActionCredentialVerify, Limit: 1})
			if err != nil || len(events) != 1 {
				t.Fatalf("verify audit: %v, %#v", err, events)
			}
			event := events[0]
			if event.Status != "success" {
				t.Fatalf("verify audit status = %q, want success: %#v", event.Status, event)
			}
			if tc.wantIdentifiers {
				if event.Cluster != "dev" || event.Instance != "local" ||
					event.Scope == nil || event.Scope.Namespace != "apps" {
					t.Fatalf("identifier verify audit = %#v", event)
				}
				return
			}
			if event.Cluster != "" || event.Instance != "" || event.Scope != nil {
				t.Fatalf("request-text verify audit = %#v", event)
			}
			assertAuditEventHasNoRequestText(t, event)
		})
	}
}

// materializeExplicitCredentialApply opens the audit that ask --apply writes
// for each credential. A scope whose cluster is request text must not reach
// that audit at its start or when it succeeds.
func TestCredentialApplyStartAuditFiltersRequestTextTarget(t *testing.T) {
	for _, tc := range []struct {
		name            string
		cluster         string
		wantIdentifiers bool
	}{
		{name: "request text target", cluster: freeTextAuditTarget},
		{name: "identifier target", cluster: "dev", wantIdentifiers: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Given a credential bound to a target, with a real Agent there
			f, _ := credentialImportFixture(t)
			if !tc.wantIdentifiers {
				startClusterAgent(t, f, tc.cluster)
			}
			resource, version, grant := createAuditTargetCredential(t, f,
				CredentialGrantScope{Cluster: tc.cluster, Instance: "local", Namespace: "apps"})
			auth, err := f.store.VerifyUserToken(f.admin.Plaintext)
			if err != nil {
				t.Fatal(err)
			}
			// When the apply materializes the credential and finishes its audit
			preparation, err := f.hub.materializeExplicitCredentialApply(context.Background(), auth, "audit-target-apply",
				[]authorizedCredentialMaterialization{{
					Reference:  CredentialPlanReference{Name: resource.Name, Scope: grant.Scope},
					Authorized: AuthorizedCredentialUse{Resource: resource, Version: version, Grant: grant},
				}})
			if err != nil {
				t.Fatalf("apply preparation: %v", err)
			}
			if len(preparation.Bindings) != 1 {
				t.Fatalf("apply bindings = %#v", preparation.Bindings)
			}
			// The audit of the apply itself is the one the admin opened; the
			// internal materialize call writes its own target audit.
			events, err := f.store.ListAuditFiltered(AuditFilter{
				UserID: auth.UserID, Action: ActionCredentialUse, Limit: 1,
			})
			if err != nil || len(events) != 1 {
				t.Fatalf("apply audit: %v, %#v", err, events)
			}
			event := events[0]
			if event.Status != "success" {
				t.Fatalf("apply audit status = %q, want success: %#v", event.Status, event)
			}
			if tc.wantIdentifiers {
				if event.Cluster != "dev" || event.Instance != "local" ||
					event.Scope == nil || event.Scope.Namespace != "apps" {
					t.Fatalf("identifier apply audit = %#v", event)
				}
				return
			}
			if event.Cluster != "" || event.Instance != "" || event.Scope != nil {
				t.Fatalf("request-text apply audit = %#v", event)
			}
			assertAuditEventHasNoRequestText(t, event)
		})
	}
}
