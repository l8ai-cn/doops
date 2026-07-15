package server

import (
	"net/http"
	"strings"
)

type credentialBundleHTTPBody struct {
	Name  string                 `json:"name"`
	Scope CredentialScope        `json:"scope"`
	Items []CredentialBundleItem `json:"items"`
}

func (h *GatewayHub) HandleCredentialBundles(w http.ResponseWriter, r *http.Request) {
	auth, err := h.authenticateUser(r)
	if err != nil {
		h.writeUserAuthError(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		if !h.store.UserHasAction(auth.UserID, ActionCredentialMetadata) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		name := strings.TrimSpace(r.URL.Query().Get("name"))
		if name == "" {
			http.Error(w, "credential bundle name is required", http.StatusBadRequest)
			return
		}
		bundle, err := h.store.ResolveCredentialBundle(auth.UserID, name)
		if err != nil {
			http.Error(w, credentialErrorCategory(err), credentialHTTPStatus(err))
			return
		}
		writeJSONHTTP(w, bundle)
	case http.MethodPost:
		if !h.store.UserHasAction(auth.UserID, ActionCredentialCreate) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		var body credentialBundleHTTPBody
		if err := decodeCredentialJSON(w, r, &body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if body.Scope == CredentialScopePlatform && !h.store.UserHasAction(auth.UserID, ActionCredentialGrant) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		ownerID := ""
		if body.Scope == CredentialScopePersonal {
			ownerID = auth.UserID
		}
		auditID := h.startCredentialAudit(auth, ActionCredentialCreate, "", "credential bundle create "+strings.TrimSpace(body.Name))
		bundle, err := h.store.CreateCredentialBundle(CredentialBundleCreateRequest{
			Name: body.Name, Scope: body.Scope, OwnerID: ownerID, CreatedBy: auth.UserID, Items: body.Items,
		})
		if err != nil {
			h.finishCredentialAudit(auditID, "error", credentialErrorCategory(err))
			http.Error(w, credentialErrorCategory(err), credentialHTTPStatus(err))
			return
		}
		h.finishCredentialAudit(auditID, "success", "")
		writeJSONHTTP(w, bundle)
	default:
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}
