package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func managedIdentityTestJWT(workspace, user string) string {
	raw, _ := json.Marshal(map[string]any{"email": user + "@example.test", "https://api.openai.com/auth": map[string]string{"chatgpt_account_id": workspace, "chatgpt_user_id": user, "chatgpt_plan_type": "team"}})
	return "e30." + base64.RawURLEncoding.EncodeToString(raw) + ".synthetic"
}
func TestManagedAccountIdentityPrivacyBatch(t *testing.T) {
	runManagedAccountIdentityPrivacyBatch(t, webTestSetup)
}
func TestPGManagedAccountIdentityPrivacyBatch(t *testing.T) {
	runManagedAccountIdentityPrivacyBatch(t, adminPGTestProxy)
}
func runManagedAccountIdentityPrivacyBatch(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	p, h := setup(t)
	importIdentity := func(name, user string) Account {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"access_token": managedIdentityTestJWT("workspace", user), "refresh_token": "secret-refresh-" + user, "account_id": "workspace", "agent_identity": map[string]any{"agent_runtime_id": "agent-" + user, "agent_private_key": "never-return-private-key", "account_id": "workspace", "chatgpt_user_id": user, "plan_type": "team", "email": user + "@example.test"}})
		if err := p.Store.Import(name, raw); err != nil {
			t.Fatal(err)
		}
		a, err := p.Store.Get(name)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	a := importIdentity("identity-one", "user-one")
	b := importIdentity("identity-two", "user-two")
	if a.Identity == nil || b.Identity == nil || a.Identity.StableKey == b.Identity.StableKey || a.ID == b.ID {
		t.Fatal("same workspace merged distinct users")
	}
	r := managedTestRequest(t, h, "GET", "/api/accounts/"+a.ID+"/metadata", nil, 200)
	if strings.Contains(r.Body.String(), "never-return-private-key") || strings.Contains(r.Body.String(), a.AccessToken) || !strings.Contains(r.Body.String(), `"verified":false`) || !strings.Contains(r.Body.String(), `"collected_at":`) {
		t.Fatal("metadata provenance or secrecy incorrect")
	}
	onlyAgent := []byte(`{"agent_identity":{"agent_runtime_id":"agent","agent_private_key":"private","account_id":"workspace","chatgpt_user_id":"user"}}`)
	if _, err := parseAccount("unsupported", onlyAgent); err == nil || !strings.Contains(err.Error(), "task signing") {
		t.Fatal("pure agent private key silently became bearer")
	}
	mismatch, _ := json.Marshal(map[string]any{"access_token": managedIdentityTestJWT("workspace", "user-one"), "agent_identity": map[string]string{"agent_runtime_id": "agent", "agent_private_key": "private", "account_id": "workspace", "chatgpt_user_id": "other"}})
	if _, err := parseAccount("mismatch", mismatch); err == nil {
		t.Fatal("agent metadata mismatched OAuth user")
	}
	var wrapper map[string]json.RawMessage
	_ = json.Unmarshal(mismatch, &wrapper)
	sessionRaw, _ := json.Marshal(map[string]any{"tokens": map[string]string{"access_token": managedIdentityTestJWT("workspace", "user-one")}, "agent_identity": wrapper["agent_identity"]})
	if _, err := parseManagedSessionCredentials(sessionRaw); err == nil {
		t.Fatal("session normalization discarded agent identity mismatch")
	}
	incoming := map[string]any{"access_token": managedIdentityTestJWT("workspace", "user-three"), "refresh_token": "new-secret", "account_id": "workspace"}
	body := map[string]any{"items": []any{map[string]any{"id": a.ID, "version": a.Version, "credentials": incoming}, map[string]any{"id": b.ID, "version": b.Version + 1, "credentials": incoming}}}
	r = managedTestRequest(t, h, "POST", "/api/accounts/reauthorize-batch", body, 200)
	if !strings.Contains(r.Body.String(), "identity_change_requires_confirmation") || !strings.Contains(r.Body.String(), "version_conflict") {
		t.Fatalf("batch identity/CAS guards failed %s", r.Body.String())
	}
	version := a.CredentialVersion
	if p.Store.DB == nil {
		version = a.Version
	}
	if err := p.cacheManagedUpstream(context.Background(), managedUpstreamSnapshot{ID: a.ID, Kind: "privacy", OK: true, CheckedAt: time.Now().Unix(), CredentialVersion: version, Source: "upstream_privacy"}); err != nil {
		t.Fatal(err)
	}
	r = managedTestRequest(t, h, "GET", "/api/accounts/manage?privacy=disabled", nil, 200)
	var list struct {
		Accounts []accountSummary
		Total    int
	}
	_ = json.Unmarshal(r.Body.Bytes(), &list)
	if list.Total != 1 || len(list.Accounts) != 1 || list.Accounts[0].ID != a.ID {
		t.Fatalf("privacy full filter wrong: %s", r.Body.String())
	}
	r = managedTestRequest(t, h, "POST", "/api/accounts/selection", map[string]any{"filters": map[string]any{"privacy": "disabled"}}, 200)
	var selection struct {
		Items []managedAccountItem
		Total int
	}
	_ = json.Unmarshal(r.Body.Bytes(), &selection)
	if selection.Total != 1 || selection.Items[0].ID != a.ID {
		t.Fatal("selection disagrees with privacy list")
	}
	body = map[string]any{"items": []any{map[string]any{"id": a.ID, "version": a.Version, "credentials": incoming, "allow_identity_change": true}}}
	managedTestRequest(t, h, "POST", "/api/accounts/reauthorize-batch", body, 200)
	r = managedTestRequest(t, h, "GET", "/api/accounts/manage?privacy=disabled", nil, 200)
	_ = json.Unmarshal(r.Body.Bytes(), &list)
	if list.Total != 0 {
		t.Fatal("old-identity privacy proof remained current")
	}
	r = managedTestRequest(t, h, "POST", "/api/account-oauth/import", map[string]any{"method": "session", "credentials": map[string]any{"access_token": "synthetic-created-token"}}, 200)
	var staged struct {
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal(r.Body.Bytes(), &staged)
	r = managedTestRequest(t, h, "POST", "/api/account-oauth/apply", map[string]any{"session_id": staged.SessionID, "name": "created-with-management", "notes": "creation notes", "tags": []string{"created"}, "enabled": false}, 200)
	created, err := p.Store.Get("created-with-management")
	if err != nil || created.Enabled || created.Notes != "creation notes" || len(created.Tags) != 1 {
		t.Fatal("OAuth create omitted general management fields")
	}
}
