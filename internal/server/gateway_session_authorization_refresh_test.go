package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestGatewaySessionAuthorizationRechecksKeyAfterRefresh(t *testing.T) {
	var inference, refreshes atomic.Int32
	var p *Proxy
	var key APIKey
	p = proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if r.URL.Path == "/oauth/token" {
			refreshes.Add(1)
			if _, err := p.Keys.Update(key.ID, func(current *APIKey) error {
				current.Groups = []string{"group-two"}
				return nil
			}); err != nil {
				t.Error(err)
				http.Error(w, "synthetic key update failed", 500)
				return
			}
			_, _ = io.WriteString(w, gatewayKnownOwnerRefreshFixture(`{"access_token":"synthetic-refreshed-access","refresh_token":"synthetic-refreshed-token","expires_in":3600}`, "test-account", "fixture-user"))
			return
		}
		inference.Add(1)
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"authorization-refresh\",\"status\":\"completed\",\"output\":[]}}\n\n")
	})
	var account Account
	key, account = gatewaySessionAuthorizationFixture(t, p)
	const session = "group-authorization-refresh"
	send := func() *httptest.ResponseRecorder {
		r := proxyRequest(http.MethodPost, "/responses", `{"model":"fixture","stream":false,"input":[]}`)
		r.Header.Set("Authorization", "Bearer "+key.Key)
		r.Header.Set("Session-Id", session)
		w := httptest.NewRecorder()
		p.ServeHTTP(w, r)
		return w
	}
	if first := send(); first.Code != 200 || inference.Load() != 1 {
		t.Fatal("cannot establish bound session before refresh")
	}
	if err := p.Store.UpdateAccount(account.ID, func(current *Account) error {
		current.ExpiresAt = time.Now().Add(-time.Minute).Unix()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if denied := send(); denied.Code != http.StatusServiceUnavailable || inference.Load() != 1 || refreshes.Load() != 1 {
		t.Fatal("post-refresh key revocation forwarded the original model request")
	}
	current, err := p.Store.Get(account.ID)
	if err != nil || current.AccessToken != "synthetic-refreshed-access" || current.RefreshToken != "synthetic-refreshed-token" ||
		gatewayAccountRef(current) != gatewayAccountRef(account) || strings.Contains(current.LastError, "authorized") || strings.Contains(current.LastError, "allowed") {
		t.Fatal("authorization denial lost refreshed credentials or marked the account as an OAuth failure")
	}
	gatewaySessionAuthorizationAssertBinding(t, p, key.ID, session, account)
	if p.inFlight(account.ID) != 0 {
		t.Fatal("post-refresh authorization denial retained admission")
	}
}
