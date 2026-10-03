package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func refreshIdentityJWT(user, plan string) string {
	claims, _ := json.Marshal(map[string]any{"exp": 1900000000, "email": "fixture@example.test", "https://api.openai.com/auth": map[string]string{
		"chatgpt_account_id": "token-workspace", "chatgpt_user_id": user, "chatgpt_plan_type": plan,
	}})
	return "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".synthetic"
}

func TestGatewayRefreshIdentityStored(t *testing.T) {
	runGatewayRefreshIdentityStored(t, accountTestStore)
}

func TestPGGatewayRefreshIdentityStored(t *testing.T) {
	runGatewayRefreshIdentityStored(t, accountPGTestStore)
}

func runGatewayRefreshIdentityStored(t *testing.T, setup func(*testing.T) Store) {
	for _, tc := range []struct {
		name      string
		response  map[string]any
		wantUser  string
		wantPlan  string
		unchanged bool
	}{
		{"same_user_access", map[string]any{"access_token": refreshIdentityJWT("user-a", "pro")}, "user-a", "pro", false},
		{"same_user_opaque_with_id", map[string]any{"access_token": "new-opaque", "id_token": refreshIdentityJWT("user-a", "pro")}, "user-a", "pro", false},
		{"new_user_access", map[string]any{"access_token": refreshIdentityJWT("user-b", "plus")}, "user-b", "plus", false},
		{"new_user_both_tokens", map[string]any{"access_token": refreshIdentityJWT("user-b", "plus"), "id_token": refreshIdentityJWT("user-b", "pro")}, "user-b", "pro", false},
		{"conflicting_tokens", map[string]any{"access_token": refreshIdentityJWT("user-a", "pro"), "id_token": refreshIdentityJWT("user-b", "pro")}, "", "", false},
		{"opaque_access", map[string]any{"access_token": "new-opaque"}, "", "", false},
		{"missing_user_claim", map[string]any{"access_token": refreshIdentityJWT("", "pro")}, "", "", false},
		{"malformed_access_claims", map[string]any{"access_token": "e30.not-json.synthetic"}, "", "", false},
		{"malformed_identity_token", map[string]any{"access_token": refreshIdentityJWT("user-a", "pro"), "id_token": "not-a-jwt"}, "", "", false},
		{"invalid_identity_field_type", map[string]any{"access_token": refreshIdentityJWT("user-a", "pro"), "id_token": map[string]string{"invalid": "metadata"}}, "", "", false},
		{"id_only_same_user", map[string]any{"id_token": refreshIdentityJWT("user-a", "pro")}, "user-a", "pro", false},
		{"id_only_conflicts_with_retained_access", map[string]any{"id_token": refreshIdentityJWT("user-b", "pro")}, "", "", false},
		{"same_access_token", map[string]any{"access_token": refreshIdentityJWT("user-a", "plus")}, "user-a", "plus", true},
		{"refresh_only", map[string]any{}, "user-a", "plus", true},
		{"refresh_only_null_identity", map[string]any{"id_token": nil}, "user-a", "plus", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := setup(t)
			raw, _ := json.Marshal(map[string]string{"access_token": refreshIdentityJWT("user-a", "plus"), "refresh_token": "old-refresh", "account_id": "selected-workspace"})
			if err := store.Import("refresh-identity", raw); err != nil {
				t.Fatal(err)
			}
			seed, err := store.Get("refresh-identity")
			if err != nil || seed.Identity == nil {
				t.Fatal("cannot load fixture credential identity")
			}
			seed.Identity.Verified = true
			// Identity is encrypted credential state in PG, not a management
			// field. Exercise the credential writer for both store backends.
			if err := store.UpdateTokens(seed.ID, seed.AccessToken, seed); err != nil {
				t.Fatal(err)
			}
			before, err := store.Get("refresh-identity")
			if err != nil {
				t.Fatal(err)
			}
			// Every response rotates RT; even unusable identity metadata must
			// not discard an already committed upstream credential rotation.
			tc.response["refresh_token"] = "rotated-refresh"
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				_ = json.NewEncoder(w).Encode(tc.response)
			}))
			defer server.Close()
			updated, err := store.RefreshContext(context.Background(), before.ID, before.AccessToken, func(current Account) (Account, error) {
				return refreshAccount(context.Background(), server.Client(), server.URL, current)
			})
			if err != nil {
				t.Fatal(err)
			}
			stored, err := store.Get(before.ID)
			if err != nil {
				t.Fatal(err)
			}
			wantAccess := before.AccessToken
			if access, ok := tc.response["access_token"].(string); ok {
				wantAccess = access
			}
			for _, account := range []Account{updated, stored} {
				if account.AccessToken != wantAccess || account.RefreshToken != "rotated-refresh" || account.AccountID != before.AccountID || calls.Load() != 1 {
					t.Fatal("rotation not saved or explicitly selected workspace changed")
				}
				if account.Identity == nil || account.Identity.UserID != tc.wantUser || account.Identity.PlanType != tc.wantPlan {
					t.Fatal("refreshed identity not reconstructed from current credential claims")
				}
				if !tc.unchanged && account.Identity.Verified {
					t.Fatal("inherited verification for new unverified JWT claims")
				}
				if tc.unchanged && !reflect.DeepEqual(account.Identity, before.Identity) {
					t.Fatal("refresh-only rotation altered the unchanged access-token identity")
				}
				wantSameOwner := tc.wantUser == "user-a"
				if (gatewayAccountRef(account) == gatewayAccountRef(before)) != wantSameOwner || (account.OwnerRevision == before.OwnerRevision) != wantSameOwner {
					t.Fatal("persisted owner revision does not reflect refreshed identity continuity")
				}
			}
			if before.Identity.UserID != "user-a" || before.Identity.PlanType != "plus" || !before.Identity.Verified {
				t.Fatal("refresh mutated the previous ownership snapshot")
			}
		})
	}
}

func TestGatewayRefreshIdentityOnlyAndErrorResponses(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		wantError  bool
	}{
		{"identity_only", `{"id_token":"` + refreshIdentityJWT("user-a", "pro") + `"}`, 200, false},
		{"identity_only_bad_type", `{"id_token":{"not":"a token"}}`, 200, true},
		{"identity_only_malformed", `{"id_token":"not-a-jwt"}`, 200, true},
		{"identity_only_empty", `{"id_token":""}`, 200, true},
		{"identity_only_boolean", `{"id_token":true}`, 200, true},
		{"identity_only_array", `{"id_token":[]}`, 200, true},
		{"empty_response", `{}`, 200, true},
		{"http_error", `{"id_token":"` + refreshIdentityJWT("user-b", "pro") + `","refresh_token":"must-not-save"}`, 401, true},
		{"malformed_json", `{"refresh_token":"must-not-save"`, 200, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := Account{AccountID: "selected-workspace", AccessToken: refreshIdentityJWT("user-a", "plus"), RefreshToken: "old-refresh",
				Identity: &managedAccountIdentity{WorkspaceID: "selected-workspace", UserID: "user-a", PlanType: "plus", Verified: true}}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			updated, err := refreshAccount(context.Background(), server.Client(), server.URL, before)
			if (err != nil) != tc.wantError {
				t.Fatalf("unexpected refresh outcome: error=%v", err)
			}
			if tc.wantError {
				if !reflect.DeepEqual(updated, before) {
					t.Fatal("failed refresh changed credential or identity")
				}
			} else if updated.Identity == nil || updated.Identity.PlanType != "pro" || updated.AccessToken != before.AccessToken || updated.RefreshToken != before.RefreshToken {
				t.Fatal("optional ID-only response did not update identity independently")
			}
		})
	}
}

func TestGatewayRefreshIdentityUnchangedOpaqueAccess(t *testing.T) {
	before := Account{AccountID: "selected-workspace", AccessToken: "unchanged-opaque", RefreshToken: "old-refresh",
		Identity: &managedAccountIdentity{WorkspaceID: "selected-workspace", UserID: "user-a", Verified: true}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": before.AccessToken, "refresh_token": "rotated-refresh"})
	}))
	defer server.Close()
	updated, err := refreshAccount(context.Background(), server.Client(), server.URL, before)
	if err != nil || updated.AccessToken != before.AccessToken || updated.RefreshToken != "rotated-refresh" || !reflect.DeepEqual(updated.Identity, before.Identity) || gatewayAccountRef(updated) != gatewayAccountRef(before) {
		t.Fatal("unchanged access token incorrectly discarded its existing ownership evidence")
	}
}

func TestGatewayRefreshIdentityRejectsOldHTTPAndWSBindings(t *testing.T) {
	for _, newUser := range []string{"user-b", ""} {
		t.Run("known="+newUser, func(t *testing.T) {
			var refreshes atomic.Int32
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				refreshes.Add(1)
				access := "new-opaque"
				if newUser != "" {
					access = refreshIdentityJWT(newUser, "pro")
				}
				_ = json.NewEncoder(w).Encode(map[string]string{"access_token": access, "refresh_token": "rotated-refresh"})
			})
			if err := p.Store.UpdateAccount("test", func(a *Account) error {
				a.AccessToken = refreshIdentityJWT("user-a", "plus")
				a.Identity = &managedAccountIdentity{WorkspaceID: a.AccountID, UserID: "user-a"}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			p.invalidateAccounts()
			before, err := p.Store.Get("test")
			if err != nil {
				t.Fatal(err)
			}
			_, release, err := p.pickSessionAccount(t.Context(), "test-client-key", "admin", "refresh-owner-thread")
			if err != nil {
				t.Fatal(err)
			}
			release()
			if _, err := p.recoverUnauthorized(t.Context(), before, p.Client); err == nil {
				t.Fatal("401 recovery accepted a changed or unproven owner")
			}
			stored, err := p.Store.Get(before.ID)
			if err != nil || stored.RefreshToken != "rotated-refresh" || refreshes.Load() != 1 {
				t.Fatal("owner rejection lost rotated refresh credentials")
			}
			_, release, err = p.pickSessionAccount(t.Context(), "test-client-key", "admin", "refresh-owner-thread")
			if release != nil {
				release()
			}
			if err == nil {
				t.Fatal("existing HTTP session retained previous owner state")
			}
			if bound, err := p.sessionAccount(t.Context(), "admin", "refresh-owner-thread", ""); err != nil || bound != gatewayAccountRef(before) {
				t.Fatal("old HTTP session binding was migrated after refresh")
			}
			if _, err := p.authorizedGatewayWSAccount("test-client-key", "admin", before); err == nil {
				t.Fatal("existing WS authorization retained previous owner state")
			}
			_, release, err = p.pickSessionAccount(t.Context(), "test-client-key", "admin", "fresh-owner-thread")
			if err != nil {
				t.Fatal("new thread cannot use durably refreshed credentials")
			}
			release()
			if strings.Contains(gatewayAccountRef(stored), stored.AccessToken) {
				t.Fatal("ownership reference exposed credential")
			}
		})
	}
}
