package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func managedOAuthTestJSON(t *testing.T, r *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(r.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	return value
}
func TestManagedOAuthFlows(t *testing.T)   { runManagedOAuthFlows(t, webTestSetup) }
func TestPGManagedOAuthFlows(t *testing.T) { runManagedOAuthFlows(t, adminPGTestProxy) }
func runManagedOAuthFlows(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	p, h := setup(t)
	var calls atomic.Int32
	var grantClient string
	var verifier string
	var grantMu sync.Mutex
	claims, _ := json.Marshal(map[string]any{"https://api.openai.com/auth": map[string]string{"chatgpt_account_id": "new-oauth-identity"}})
	idToken := "header." + base64.RawURLEncoding.EncodeToString(claims) + ".signature"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/accounts/v1/user-auth-credential/whoami" {
			if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer at-synthetic-pat" {
				t.Error("PAT did not use authenticated whoami")
			}
			fmt.Fprint(w, `{"chatgpt_account_id":"pat-account-id","chatgpt_user_id":"pat-user-id","chatgpt_plan_type":"pro","chatgpt_account_is_fedramp":false}`)
			return
		}
		if r.URL.Path != "/oauth/token" || r.Method != "POST" || r.Header.Get("Authorization") != "" {
			t.Error("invalid token exchange destination or headers")
		}
		grantMu.Lock()
		defer grantMu.Unlock()
		if strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
			_ = r.ParseForm()
			grantClient = r.Form.Get("client_id")
			verifier = r.Form.Get("code_verifier")
			if r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("redirect_uri") != managedOAuthRedirect || r.Form.Get("code") != "code-synthetic" {
				t.Error("authorization-code contract changed")
			}
		} else {
			var payload map[string]string
			if json.NewDecoder(r.Body).Decode(&payload) != nil || payload["grant_type"] != "refresh_token" || payload["scope"] != "" {
				t.Error("invalid refresh grant")
			}
			grantClient = payload["client_id"]
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": fmt.Sprintf("exchanged-secret-%d", calls.Load()), "refresh_token": fmt.Sprintf("rotated-secret-%d", calls.Load()), "id_token": idToken, "expires_in": 3600})
	}))
	defer upstream.Close()
	p.TokenURL = upstream.URL + "/oauth/token"

	t.Run("pkce_state_replay_target_apply", func(t *testing.T) {
		target := managedTestAccount(t, p, "oauth-target", func(a *Account) { a.Enabled = false; a.Notes = "keep original notes"; a.Priority = -5; a.Weight = 7 })
		start := managedOAuthTestJSON(t, managedTestRequest(t, h, "POST", "/api/account-oauth/start", map[string]any{"target_id": target.ID, "version": target.Version}, 200))
		id := start["session_id"].(string)
		authURL, err := url.Parse(start["auth_url"].(string))
		if err != nil {
			t.Fatal(err)
		}
		q := authURL.Query()
		if q.Get("client_id") != oauthClientID || q.Get("redirect_uri") != managedOAuthRedirect || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("state") == "" {
			t.Fatal("incomplete PKCE authorization URL")
		}
		if strings.Contains(start["auth_url"].(string), "code_verifier") {
			t.Fatal("verifier exposed in authorization URL")
		}
		managedTestRequest(t, h, "POST", "/api/account-oauth/exchange", map[string]any{"session_id": id, "code": "code-synthetic", "state": "wrong-state"}, 400)
		managedTestRequest(t, h, "POST", "/api/account-oauth/exchange", map[string]any{"session_id": id, "code": "https://attacker.invalid/auth/callback?code=x&state=y"}, 400)
		if calls.Load() != 0 {
			t.Fatal("invalid state made upstream request")
		}
		callback := managedOAuthRedirect + "?" + url.Values{"code": {"code-synthetic"}, "state": {q.Get("state")}}.Encode()
		ready := managedTestRequest(t, h, "POST", "/api/account-oauth/exchange", map[string]any{"session_id": id, "code": callback}, 200)
		status := managedOAuthTestJSON(t, ready)
		if status["phase"] != "ready" || status["identity_change"] != true || status["has_refresh_token"] != true || strings.Contains(ready.Body.String(), "exchanged-secret") || strings.Contains(ready.Body.String(), "rotated-secret") {
			t.Fatal("exchange did not stage redacted credentials")
		}
		grantMu.Lock()
		challenge := sha256.Sum256([]byte(verifier))
		actualClient := grantClient
		grantMu.Unlock()
		if base64.RawURLEncoding.EncodeToString(challenge[:]) != q.Get("code_challenge") || actualClient != oauthClientID {
			t.Fatal("PKCE verifier does not match challenge")
		}
		managedTestRequest(t, h, "POST", "/api/account-oauth/exchange", map[string]any{"session_id": id, "code": callback}, 409)
		if calls.Load() != 1 {
			t.Fatal("replayed code was exchanged twice")
		}
		managedTestRequest(t, h, "POST", "/api/account-oauth/apply", map[string]any{"session_id": id}, 409)
		applied := managedOAuthTestJSON(t, managedTestRequest(t, h, "POST", "/api/account-oauth/apply", map[string]any{"session_id": id, "allow_identity_change": true}, 200))
		if applied["id"] != target.ID {
			t.Fatal("target ID changed")
		}
		current, err := p.Store.Get(target.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.Name != target.Name || current.Enabled || current.Notes != target.Notes || current.Priority != target.Priority || current.Weight != target.Weight || current.AccountID != "new-oauth-identity" || current.ClientID != oauthClientID {
			t.Fatal("OAuth apply changed management fields or lost client identity")
		}
		managedTestRequest(t, h, "POST", "/api/account-oauth/apply", map[string]any{"session_id": id, "allow_identity_change": true}, 200)
		after, _ := p.Store.Get(target.ID)
		if after.Version != current.Version {
			t.Fatal("apply replay wrote credentials twice")
		}
		entry, err := p.managementState().Get(context.Background(), managedOAuthNamespace, id)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(entry.Payload), "exchanged-secret") || strings.Contains(string(entry.Payload), "rotated-secret") || strings.Contains(string(entry.Payload), verifier) {
			t.Fatal("applied session retained credentials or verifier")
		}
	})
	t.Run("refresh_mobile_session_and_pat_import", func(t *testing.T) {
		before := calls.Load()
		managedTestRequest(t, h, "POST", "/api/account-oauth/import", map[string]any{"method": "mobile_refresh_token", "refresh_token": "mobile-rt"}, 400)
		if calls.Load() != before {
			t.Fatal("mobile client ID was guessed")
		}
		for _, method := range []string{"refresh_token", "mobile_refresh_token", "session", "pat"} {
			body := map[string]any{"method": method}
			switch method {
			case "refresh_token":
				body["refresh_token"] = "original-rt"
			case "mobile_refresh_token":
				body["refresh_token"] = "mobile-rt"
				body["client_id"] = "original-mobile-client"
			case "session":
				body["credentials"] = map[string]any{"accessToken": "session-access-secret", "account": map[string]string{"id": "session-account"}, "sessionToken": "browser-cookie-secret", "expires": "2030-01-01T00:00:00Z"}
			case "pat":
				body["access_token"] = "at-synthetic-pat"
			}
			r := managedTestRequest(t, h, "POST", "/api/account-oauth/import", body, 200)
			status := managedOAuthTestJSON(t, r)
			id := status["session_id"].(string)
			if status["phase"] != "ready" || status["method"] != method {
				t.Fatalf("%s did not stage", method)
			}
			if strings.Contains(r.Body.String(), "secret") || strings.Contains(r.Body.String(), "at-synthetic-pat") {
				t.Fatal("import returned credentials")
			}
			if method == "mobile_refresh_token" {
				grantMu.Lock()
				actual := grantClient
				grantMu.Unlock()
				if actual != "original-mobile-client" {
					t.Fatal("mobile refresh used wrong issuing client")
				}
			}
			if method == "session" {
				entry, err := p.managementState().Get(context.Background(), managedOAuthNamespace, id)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(entry.Payload), "browser-cookie-secret") {
					t.Fatal("browser session cookie persisted as OAuth credential")
				}
				if status["expires_at_token"] != float64(0) {
					t.Fatal("browser session expiry misrepresented as OAuth expiry")
				}
			}
			name := "created-" + method
			managedTestRequest(t, h, "POST", "/api/account-oauth/apply", map[string]any{"session_id": id, "name": name, "defaults": map[string]any{"concurrency": 4, "weight": 3}}, 200)
			a, err := p.Store.Get(name)
			if err != nil {
				t.Fatal(err)
			}
			if a.Concurrency != 4 || a.Weight != 3 {
				t.Fatal("new OAuth account ignored defaults")
			}
			if method == "pat" && (a.RefreshToken != "" || a.ExpiresAt != 0 || a.AccountID != "pat-account-id") {
				t.Fatal("PAT inherited OAuth-only fields")
			}
		}
		if calls.Load() != before+3 {
			t.Fatal("session JSON import unexpectedly contacted upstream")
		}
		managedTestRequest(t, h, "POST", "/api/account-oauth/import", map[string]any{"method": "session", "credentials": map[string]string{"sessionToken": "cookie-only"}}, 400)
	})
	t.Run("cancel_expiry_and_owner_binding", func(t *testing.T) {
		start := managedOAuthTestJSON(t, managedTestRequest(t, h, "POST", "/api/account-oauth/start", map[string]any{}, 200))
		id := start["session_id"].(string)
		entry, err := p.managementState().Get(context.Background(), managedOAuthNamespace, id)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := decodeManagedOAuth(&entry, "different-owner"); err == nil {
			t.Fatal("OAuth session not bound to authenticated owner")
		}
		managedTestRequest(t, h, "DELETE", "/api/account-oauth/"+id, nil, 200)
		managedTestRequest(t, h, "POST", "/api/account-oauth/apply", map[string]any{"session_id": id, "name": "must-not-exist"}, 409)
		entry, _ = p.managementState().Get(context.Background(), managedOAuthNamespace, id)
		var state managedOAuthSession
		_ = json.Unmarshal(entry.Payload, &state)
		if state.Verifier != "" || state.State != "" || len(state.Credentials) > 0 {
			t.Fatal("cancellation retained secrets")
		}
		if err := p.managementState().Update(context.Background(), managedOAuthNamespace, id, func(row *managementStateEntry) error { row.ExpiresAt = time.Now().Unix() - 1; return nil }); err != nil {
			t.Fatal(err)
		}
		managedTestRequest(t, h, "GET", "/api/account-oauth/"+id, nil, 404)
	})
}

func TestManagedOAuthConcurrentClaimAndUnsafeResponses(t *testing.T) {
	p, h := webTestSetup(t)
	start := managedOAuthTestJSON(t, managedTestRequest(t, h, "POST", "/api/account-oauth/start", map[string]any{}, 200))
	id := start["session_id"].(string)
	u, _ := url.Parse(start["auth_url"].(string))
	var won atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := p.claimManagedOAuth(context.Background(), id, "bearer:"+adminKeyHash(webTestKey), "oauth", u.Query().Get("state")); err == nil {
				won.Add(1)
			}
		}()
	}
	wg.Wait()
	if won.Load() != 1 {
		t.Fatalf("OAuth state claimed %d times", won.Load())
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		fmt.Fprint(w, `{"error":"private-token-should-not-escape"}`)
	}))
	defer server.Close()
	p.TokenURL = server.URL + "/oauth/token"
	r := managedTestRequest(t, h, "POST", "/api/account-oauth/import", map[string]any{"method": "refresh_token", "refresh_token": "private-token-should-not-escape"}, 502)
	if strings.Contains(r.Body.String(), "private-token-should-not-escape") {
		t.Fatal("OAuth error leaked upstream secret")
	}
	for _, path := range []string{"/api/account-oauth/start", "/api/account-oauth/exchange", "/api/account-oauth/import", "/api/account-oauth/apply"} {
		r := webTestRequest(h, "POST", path, `{}`, "")
		if r.Code != 401 {
			t.Fatal("OAuth route missing authentication")
		}
	}
}
