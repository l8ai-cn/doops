package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestGatewayPromptUnknownKeepsCredentialPreparation(t *testing.T) {
	t.Setenv("DOOPS_GATEWAY_SECRET_KEY", strings.Repeat("u", 32))
	f := newExecutionLifecycleFixture(t, GatewayHubOptions{})
	auth, err := f.store.VerifyUserToken(f.admin.Plaintext)
	if err != nil {
		t.Fatal(err)
	}
	resource, err := f.store.CreateCredential(CredentialCreateRequest{
		Name: "prompt-unknown-credential", Scope: CredentialScopePersonal, Type: CredentialTypeOpaque,
		OwnerID: auth.UserID, CreatedBy: auth.UserID,
	})
	if err != nil {
		t.Fatal(err)
	}
	version, err := f.store.PutCredentialVersion(CredentialVersionPutRequest{
		CredentialID: resource.ID, CreatedBy: auth.UserID, Activate: true,
		Payload: json.RawMessage(`{"data":{"dsn":"synthetic-prompt-unknown-payload-canary"}}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	scope := CredentialGrantScope{
		Cluster: "dev", Instance: "local", Namespace: "apps", Use: CredentialUseOpaqueSecret,
		Workload: CredentialPlanWorkload{Kind: "deployment", Name: "api"}, RequiredKeys: []string{"dsn"},
	}
	grant, err := f.store.CreateCredentialGrant(CredentialGrantCreateRequest{
		CredentialID: resource.ID, Scope: scope, CreatedBy: auth.UserID,
	})
	if err != nil {
		t.Fatal(err)
	}
	materialization, err := json.Marshal(CredentialMaterialization{
		CredentialID: resource.ID, GrantID: grant.ID, VersionID: version.ID,
		ResourceName: credentialSecretName(resource.ID, grant.ID), Namespace: scope.Namespace,
		SecretType: "Opaque", Keys: scope.RequiredKeys, ResourceVersion: "1", Status: "verified",
	})
	if err != nil {
		t.Fatal(err)
	}
	const helmUnknown = "execution_outcome_unknown: doops_helm=execution_outcome_unknown"
	for _, category := range []string{
		doagentPromptCleanupUnknown, "execution_outcome_unknown: prompt_transport_failed", helmUnknown, doagentPromptQueued,
	} {
		for _, operation := range []string{"ask", "apply"} {
			t.Run(category+"/"+operation, func(t *testing.T) {
				remote, client := f.agent(t, "prompt-unknown-credentials"), f.client(t, f.admin)
				t.Cleanup(func() {
					_ = remote.conn.Close()
					requireEventually(t, time.Second, func() bool { return f.hub.getAgent("dev", "local") == nil })
				})
				const session = "prompt-unknown-credentials"
				args := map[string]interface{}{"operation": operation, "instruction": "synthetic credential-bound prompt"}
				if operation == "apply" {
					args["credential_refs"] = []string{resource.Name}
				}
				lifecycleSend(t, client, 2, "doops_agent_prompt", session, args)
				if operation == "apply" {
					tokenLifecycleReplyText(t, remote, remote.call(t, "doops_credential_materialize", session), string(materialization), false)
				}
				call := remote.call(t, "doops_agent_prompt", session)
				op := f.operation(t, session, "running")
				const denial = "blocked_command(maintenance): next: contact the release owner"
				rpcError := map[string]interface{}{"code": -32603, "message": category}
				withReceipt := category == helmUnknown && operation == "apply"
				if withReceipt {
					mutation := map[string]any{"tool": "doops_helm", "result": map[string]any{"content": []map[string]string{{"type": "text", "text": denial}}}}
					rpcError["data"] = map[string]interface{}{
						"structuredContent": map[string]any{"kind": "doops.verified-mutation", "outcome": "unknown", "mutations": []map[string]any{mutation}},
						"rawOutput":         "synthetic-prompt-unknown-payload-canary",
					}
				}
				if err := remote.write(map[string]interface{}{
					"jsonrpc": "2.0", "id": call.id,
					"error": rpcError,
				}); err != nil {
					t.Fatal(err)
				}
				if err := client.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
					t.Fatal(err)
				}
				type result struct {
					message map[string]interface{}
					err     error
				}
				finished := make(chan result, 1)
				go func() {
					var response map[string]interface{}
					err := client.ReadJSON(&response)
					finished <- result{response, err}
				}()
				var response map[string]interface{}
				rollbacks := 0
			waitResponse:
				for {
					select {
					case extra := <-remote.calls:
						if extra.name != "doops_credential_cleanup" {
							t.Fatalf("unexpected dispatch after terminal: %#v", extra)
						}
						// Answer a mistaken rollback too, so the defect fails an
						// assertion instead of leaving the Gateway waiting.
						rollbacks++
						tokenLifecycleReplyText(t, remote, extra, "synthetic cleanup complete", false)
					case got := <-finished:
						if got.err != nil {
							t.Fatal(got.err)
						}
						response = got.message
						break waitResponse
					}
				}
				unknown := category != doagentPromptQueued
				wantStatus, wantMessage, wantCode := "error", category, float64(-32603)
				if unknown {
					wantStatus, wantCode = "outcome_unknown", -32008
					if category == doagentPromptCleanupUnknown {
						wantMessage = "execution_outcome_unknown: " + category
					}
				}
				rpcErr, _ := response["error"].(map[string]interface{})
				if withReceipt {
					data, _ := rpcErr["data"].(map[string]interface{})
					report, _ := json.Marshal(data["structuredContent"])
					if len(data) != 1 || !strings.Contains(string(report), denial) {
						t.Fatalf("expected only the public receipt with the original policy hint: %s", report)
					}
				}
				if response["id"] != float64(2) || rpcErr["code"] != wantCode || rpcErr["message"] != wantMessage {
					t.Errorf("terminal category or reason changed: %#v, want %q", response, wantMessage)
				}
				event := executionAuditTestEvent(t, f.store, op.AuditID)
				wantAuditMessage := wantMessage
				if category == helmUnknown {
					wantAuditMessage = "execution_outcome_unknown"
				}
				if event.Status != wantStatus || event.Error != wantAuditMessage {
					t.Errorf("audit lost unknown evidence: status=%q reason=%q", event.Status, event.Error)
				}
				wantRollbacks := 0
				if !unknown && operation == "apply" {
					wantRollbacks = 1
				}
				if rollbacks != wantRollbacks {
					t.Errorf("credential rollbacks=%d, want %d", rollbacks, wantRollbacks)
				}
				if operation == "apply" {
					records, err := f.store.CredentialMaterializations(resource.ID, version.ID, grant.ID)
					if err != nil {
						t.Fatal(err)
					}
					if unknown {
						if len(records) != 1 || records[0].Status != "verified" {
							t.Errorf("unknown prompt retired prepared credential: %#v", records)
						}
					} else if len(records) != 0 {
						t.Errorf("confirmed refusal did not clean its credential: %#v", records)
					}
				}
				if len(f.hub.listActiveOperations()) != 0 || len(remote.calls) != 0 || len(remote.cancels) != 0 {
					t.Fatal("terminal left an active operation or replayed the request")
				}
				followup, forwarded := tokenLifecycleShellCall(t, client, remote, 3, session)
				if !forwarded || followup["error"] != nil || followup["result"] == nil {
					t.Fatalf("historical unknown blocked the next task: %#v", followup)
				}
				retained := executionAuditTestEvent(t, f.store, op.AuditID)
				if retained.Status != event.Status || retained.Error != event.Error {
					t.Fatal("follow-up changed the original outcome")
				}
				audits, err := f.store.ListAudit(100)
				if err != nil {
					t.Fatal(err)
				}
				raw, err := json.Marshal(audits)
				if err != nil || strings.Contains(string(raw), "synthetic-prompt-unknown-payload-canary") ||
					strings.Contains(string(raw), f.admin.Plaintext) {
					t.Fatal("audit leaked credential payload or token")
				}
			})
		}
	}
}
