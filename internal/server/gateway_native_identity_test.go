package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGatewayNativeAccountScopeKnownOwnerAndFallback(t *testing.T) {
	ctx := t.Context()
	headers := cookieIdentityHeaders(t, "workspace-a", "user-a", "at-1")
	a := cookieIdentityAccount(headers, "user-a")
	trusted := gatewayNativeAccountScope(withGatewayNativeAccountScope(ctx, a), headers)
	if trusted == ([32]byte{}) || trusted == gatewayNativeAccountScope(ctx, headers) {
		t.Fatal("trusted ownership scope missing or not separated from credentials fallback")
	}
	refreshed := a
	refreshed.AccessToken, refreshed.RefreshToken = "refreshed-at", "refreshed-rt"
	if gatewayNativeAccountScope(withGatewayNativeAccountScope(ctx, refreshed), nil) != trusted {
		t.Fatal("known-owner credential rotation lost native state continuity")
	}
	for _, alter := range []func(*Account){
		func(a *Account) { a.AccountID = "workspace-b" },
		func(a *Account) {
			a.Identity = &managedAccountIdentity{UserID: "different-user", WorkspaceID: a.AccountID}
		},
		func(a *Account) { a.OwnerRevision = "replacement" },
		func(a *Account) { a.ID = "other-record" },
		func(a *Account) { a.Identity = nil },
	} {
		changed := a
		alter(&changed)
		if gatewayNativeAccountScope(withGatewayNativeAccountScope(ctx, changed), headers) == trusted {
			t.Fatal("ownership boundary reused native state")
		}
	}
	unknown := a
	unknown.Identity = nil
	unknownBefore := gatewayNativeAccountScope(withGatewayNativeAccountScope(ctx, unknown), headers)
	unknown.AccessToken = "unknown-replacement"
	if gatewayNativeAccountScope(withGatewayNativeAccountScope(ctx, unknown), headers) == unknownBefore {
		t.Fatal("unknown-owner credential replacement reused native state")
	}
	fallback := gatewayNativeAccountScope(ctx, headers)
	caseChanged := http.Header{"authorization": {headers.Get("Authorization")}, "CHATGPT-ACCOUNT-ID": {headers.Get("ChatGPT-Account-Id")}}
	if gatewayNativeAccountScope(ctx, caseChanged) != fallback {
		t.Fatal("header casing alone changed fallback identity")
	}
	for _, changed := range []http.Header{
		cookieIdentityHeaders(t, "workspace-a", "user-a", "at-2"),
		{"Authorization": {headers.Get("Authorization")}, "Chatgpt-Account-Id": {"other-workspace"}},
		{"Authorization": {headers.Get("Authorization"), "additional"}, "Chatgpt-Account-Id": {"workspace-a"}},
	} {
		if gatewayNativeAccountScope(ctx, changed) == fallback {
			t.Fatal("fallback inferred identity instead of separating exact credentials")
		}
	}
	for _, anonymous := range []http.Header{nil, {}, {"Chatgpt-Account-Id": {"workspace-a"}}, {"Authorization": {""}}} {
		if gatewayNativeAccountScope(withGatewayNativeAccountScope(ctx, Account{}), anonymous) != ([32]byte{}) {
			t.Fatal("anonymous request unexpectedly acquired an owner scope")
		}
	}
}

func TestGatewayNativeAccountScopeDispatchRefreshAndManagement(t *testing.T) {
	p := newTestProxy(t, 1)
	a := setGatewayOwnerFixture(t, p, "scope-user")
	want := gatewayNativeAccountScope(withGatewayNativeAccountScope(t.Context(), a), nil)
	var requests, refreshes int
	client := &http.Client{Transport: sourceRefreshRoundTripper(func(r *http.Request) (*http.Response, error) {
		if got := gatewayNativeAccountScope(r.Context(), r.Header); got != want {
			t.Error("request/refresh lost the selected trusted owner scope")
		}
		if r.Body != nil {
			_, _ = io.Copy(io.Discard, r.Body)
			_ = r.Body.Close()
		}
		if r.URL.Path == "/token" {
			refreshes++
			if r.Header.Get("Authorization") != "" {
				t.Error("refresh should carry scope without bearer")
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(gatewayKnownOwnerRefreshFixture(`{"access_token":"rotated-scope-at","refresh_token":"rotated-scope-rt"}`, a.AccountID, a.Identity.UserID)))}, nil
		}
		requests++
		status := http.StatusUnauthorized
		if requests == 2 {
			status = http.StatusOK
			if r.Header.Get("Authorization") != "Bearer rotated-scope-at" {
				t.Error("retry did not use refreshed credentials")
			}
		}
		return &http.Response{StatusCode: status, Body: http.NoBody}, nil
	})}
	p.TokenURL = "http://synthetic.invalid/token"
	req := httptest.NewRequest(http.MethodPost, "http://synthetic.invalid/responses", strings.NewReader(`{}`))
	req.RequestURI = ""
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(`{}`)), nil }
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	// A stale inherited scope must be replaced at each account dispatch.
	other := a
	other.OwnerRevision = "stale-owner"
	req = req.WithContext(withGatewayNativeAccountScope(req.Context(), other))
	res, err := p.doAccountRequest(client, req, a, doHTTPAccountRequest, func(Account) error { return nil })
	if err != nil || res == nil || res.StatusCode != 200 || requests != 2 || refreshes != 1 {
		t.Fatalf("scope-aware 401 dispatch failed: err=%v requests=%d refreshes=%d", err, requests, refreshes)
	}
	_ = res.Body.Close()
	if gatewayNativeAccountScope(req.Context(), req.Header) == want {
		t.Fatal("dispatch mutated original caller request context")
	}
	managed := httptest.NewRequest(http.MethodGet, "http://synthetic.invalid/models", nil)
	if err := p.managedUpstreamHeaders(managed, a); err != nil {
		t.Fatal(err)
	}
	if gatewayNativeAccountScope(managed.Context(), managed.Header) != want {
		t.Fatal("managed upstream bypassed trusted account scope")
	}
	cancelCtx, cancel := context.WithCancel(context.Background())
	scoped := withGatewayNativeAccountScope(cancelCtx, a)
	cancel()
	if scoped.Err() != context.Canceled {
		t.Fatal("scope detached request cancellation")
	}
}
