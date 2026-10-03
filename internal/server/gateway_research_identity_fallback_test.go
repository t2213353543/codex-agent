package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func researchIdentityFallbackJWT(auth map[string]any) string {
	claims, _ := json.Marshal(map[string]any{"exp": 1900000000, "https://api.openai.com/auth": auth})
	return "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".synthetic"
}

func TestResearchIdentityFallbackCredentialEntrypoints(t *testing.T) {
	for _, tc := range []struct {
		name      string
		access    map[string]any
		identity  map[string]any
		workspace string
		wantUser  string
		wantError bool
	}{
		{name: "primary_conflict", access: map[string]any{"chatgpt_user_id": "user-a"}, identity: map[string]any{"chatgpt_user_id": "user-b"}, wantError: true},
		{name: "access_fallback_conflict", access: map[string]any{"user_id": "user-a"}, identity: map[string]any{"chatgpt_user_id": "user-b"}, wantError: true},
		{name: "id_token_fallback_conflict", access: map[string]any{"chatgpt_user_id": "user-a"}, identity: map[string]any{"user_id": "user-b"}, wantError: true},
		{name: "both_fallback_conflict", access: map[string]any{"user_id": "user-a"}, identity: map[string]any{"user_id": "user-b"}, wantError: true},
		{name: "null_primary_uses_fallback", access: map[string]any{"chatgpt_user_id": nil, "user_id": "user-a"}, identity: map[string]any{"chatgpt_user_id": "user-b"}, wantError: true},
		{name: "both_fallback_same_user", access: map[string]any{"user_id": "user-a"}, identity: map[string]any{"user_id": "user-a"}, wantUser: "user-a"},
		{name: "mixed_fields_same_user", access: map[string]any{"user_id": "user-a"}, identity: map[string]any{"chatgpt_user_id": "user-a"}, wantUser: "user-a"},
		{name: "primary_has_precedence", access: map[string]any{"chatgpt_user_id": "user-a", "user_id": "fallback-a"}, identity: map[string]any{"chatgpt_user_id": "user-a", "user_id": "fallback-b"}, wantUser: "user-a"},
		{name: "matching_fallback_cannot_hide_primary_conflict", access: map[string]any{"chatgpt_user_id": "user-a", "user_id": "same-fallback"}, identity: map[string]any{"chatgpt_user_id": "user-b", "user_id": "same-fallback"}, wantError: true},
		{name: "selected_workspace_is_preserved", access: map[string]any{"user_id": "user-a"}, identity: map[string]any{"chatgpt_user_id": "user-a"}, workspace: "selected-workspace", wantUser: "user-a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, auth := range []map[string]any{tc.access, tc.identity} {
				auth["chatgpt_account_id"] = "token-workspace"
				auth["chatgpt_plan_type"] = "plus"
			}
			workspace := tc.workspace
			if workspace == "" {
				workspace = "token-workspace"
			}
			access, identity := researchIdentityFallbackJWT(tc.access), researchIdentityFallbackJWT(tc.identity)
			raw, _ := json.Marshal(map[string]string{"access_token": access, "id_token": identity, "account_id": workspace})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/oauth/token" {
					t.Error("unexpected synthetic exchange route")
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(raw)
			}))
			defer server.Close()
			for _, entrypoint := range []string{"direct_import", "session_import", "oauth_exchange"} {
				t.Run(entrypoint, func(t *testing.T) {
					var account Account
					var err error
					switch entrypoint {
					case "direct_import":
						account, err = parseAccount("identity-fallback", raw)
					case "session_import":
						session, _ := json.Marshal(map[string]any{"tokens": map[string]string{"accessToken": access, "idToken": identity, "accountId": workspace}})
						account, err = parseManagedSessionCredentials(session)
					case "oauth_exchange":
						p := &Proxy{TokenURL: server.URL + "/oauth/token"}
						account, err = p.managedOAuthRequest(context.Background(), managedOAuthSession{}, strings.NewReader("grant_type=authorization_code"), "application/x-www-form-urlencoded")
					}
					if tc.wantError {
						if err == nil {
							t.Fatal("accepted conflicting access-token and identity-token users")
						}
						if strings.Contains(err.Error(), access) || strings.Contains(err.Error(), identity) {
							t.Fatal("credential appeared in error")
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					if account.Identity == nil || account.Identity.UserID != tc.wantUser || account.Identity.WorkspaceID != workspace || account.AccountID != workspace {
						t.Fatal("lost official user claim precedence or selected workspace")
					}
					if account.Identity.Verified || account.Identity.StableKey != managedCRSHash(workspace+"\x00"+tc.wantUser) {
						t.Fatal("incorrect identity provenance or owner key")
					}
				})
			}
		})
	}
}
