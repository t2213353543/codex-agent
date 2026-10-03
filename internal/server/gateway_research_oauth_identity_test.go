package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func researchOAuthIdentityReply(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, gatewayKnownOwnerRefreshFixture(`{"access_token":"synthetic-new-at","refresh_token":"synthetic-new-rt","expires_in":3600}`, "test-account", "fixture-user"))
}

func TestResearchOAuthIdentityRefreshOmitsProviderVersion(t *testing.T) {
	runResearchOAuthIdentityRefresh(t, false)
}

func runResearchOAuthIdentityRefresh(t *testing.T, native bool) {
	for _, profile := range []string{"disabled", "linux", "macos"} {
		t.Run(profile, func(t *testing.T) {
			var calls atomic.Int32
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path != "/oauth/token" || r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
					t.Error("refresh request route/grant encoding changed")
				}
				if got := r.Header.Get("Version"); got != "" {
					t.Errorf("OAuth default auth client must not acquire model-provider Version: %q", got)
				}
				if got := r.Header.Get("Originator"); got != clientOriginator {
					t.Errorf("refresh originator=%q", got)
				}
				if profile != "disabled" {
					configured, _ := codexFingerprintProfile(profile)
					if r.UserAgent() != configured.UserAgent {
						t.Error("chosen OAuth UA profile changed")
					}
				} else if r.UserAgent() != clientOriginator+"/"+clientVersion {
					t.Error("disabled-profile compatibility UA unexpectedly changed")
				}
				for _, forbidden := range []string{"Authorization", "ChatGPT-Account-Id", "X-Codex-Routing-Hint", "Session-Id", "OpenAI-Beta", "X-Oai-Attestation", "Content-Encoding"} {
					if r.Header.Get(forbidden) != "" {
						t.Errorf("refresh leaked inference field %s", forbidden)
					}
				}
				var payload map[string]string
				if json.NewDecoder(r.Body).Decode(&payload) != nil || len(payload) != 3 || payload["client_id"] != "synthetic-issuer-client" || payload["grant_type"] != "refresh_token" || payload["refresh_token"] != "test-refresh-token" {
					t.Error("OAuth identity change altered its issuer or grant")
				}
				researchOAuthIdentityReply(w)
			})
			if native {
				realGatewayNativePool(t, p)
			}
			if profile != "disabled" {
				setFingerprintTestSettings(t, p, true, profile)
			}
			account, err := p.Store.Get("test")
			if err != nil {
				t.Fatal(err)
			}
			account.ClientID = "synthetic-issuer-client"
			updated, err := p.refreshAccountWithFingerprint(t.Context(), account)
			if err != nil || calls.Load() != 1 || updated.AccessToken != "synthetic-new-at" || updated.RefreshToken != "synthetic-new-rt" || gatewayAccountRef(updated) != gatewayAccountRef(account) {
				t.Fatalf("refresh failed or replaced ownership: err=%v calls=%d", err, calls.Load())
			}
		})
	}
}

func TestResearchOAuthIdentityManagedExchangeSeparatesRawAndDefaultClients(t *testing.T) {
	runResearchOAuthIdentityManagedExchange(t, false)
}

func runResearchOAuthIdentityManagedExchange(t *testing.T, native bool) {
	for _, profile := range []string{"disabled", "linux", "macos"} {
		for _, grant := range []string{"authorization_code", "refresh_token"} {
			t.Run(profile+"/"+grant, func(t *testing.T) {
				var calls atomic.Int32
				p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					if r.URL.Path != "/oauth/token" || r.Method != http.MethodPost {
						t.Error("managed exchange route changed")
					}
					if r.Header.Get("Authorization") != "" || r.Header.Get("ChatGPT-Account-Id") != "" || r.Header.Get("Version") != "" {
						t.Error("token exchange inherited model-provider authentication/version headers")
					}
					if grant == "authorization_code" {
						if _, hasUA := r.Header["User-Agent"]; hasUA || r.Header.Get("Originator") != "" {
							t.Error("authorization-code raw auth client inherited Codex default identity")
						}
						if err := r.ParseForm(); err != nil || r.Form.Get("grant_type") != grant || r.Form.Get("code") != "synthetic-code" || r.Form.Get("client_id") != oauthClientID || r.Form.Get("code_verifier") != "synthetic-verifier" {
							t.Error("authorization code form changed")
						}
					} else {
						if r.Header.Get("Originator") != clientOriginator || !strings.HasPrefix(r.UserAgent(), clientOriginator+"/") {
							t.Error("refresh import lost its default auth identity")
						}
						var payload map[string]string
						if json.NewDecoder(r.Body).Decode(&payload) != nil || payload["grant_type"] != grant || payload["refresh_token"] != "synthetic-import-rt" {
							t.Error("refresh import payload changed")
						}
					}
					researchOAuthIdentityReply(w)
				})
				if native {
					realGatewayNativePool(t, p)
				}
				if profile != "disabled" {
					setFingerprintTestSettings(t, p, true, profile)
				}
				var body, contentType string
				if grant == "authorization_code" {
					body = url.Values{"grant_type": {grant}, "code": {"synthetic-code"}, "client_id": {oauthClientID}, "code_verifier": {"synthetic-verifier"}, "redirect_uri": {managedOAuthRedirect}}.Encode()
					contentType = "application/x-www-form-urlencoded"
				} else {
					body = `{"grant_type":"refresh_token","client_id":"synthetic-issuer-client","refresh_token":"synthetic-import-rt"}`
					contentType = "application/json"
				}
				account, err := p.managedOAuthRequest(t.Context(), managedOAuthSession{}, strings.NewReader(body), contentType)
				if err != nil || calls.Load() != 1 || account.AccessToken != "synthetic-new-at" {
					t.Fatalf("managed exchange failed: err=%v calls=%d", err, calls.Load())
				}
			})
		}
	}
}

func TestResearchOAuthIdentityNativeActualHTTPContracts(t *testing.T) {
	t.Run("refresh", func(t *testing.T) { runResearchOAuthIdentityRefresh(t, true) })
	t.Run("managed_exchange", func(t *testing.T) { runResearchOAuthIdentityManagedExchange(t, true) })
}
