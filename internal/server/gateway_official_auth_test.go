package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestOfficialAuthRefreshOwnsReplyAndPreservesAccount(t *testing.T) {
	account := Account{ID: "account", AccountID: "workspace", AccessToken: "old-access", RefreshToken: "private-refresh", Enabled: true, Identity: &managedAccountIdentity{UserID: "user"}}
	for _, test := range []string{"ok", "wrong-account", "wrong-reason", "canceled", "changed-owner"} {
		t.Run(test, func(t *testing.T) {
			responses := make(chan []byte, 1)
			calls := 0
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			callback := officialAuthRecovery(func(ctx context.Context, a Account) (Account, error) {
				calls++
				a.AccessToken = "new-access"
				if test == "changed-owner" {
					a.AccountID = "other"
				}
				return a, nil
			})
			ctx = context.WithValue(ctx, officialAuthRecoveryKey{}, callback)
			rpc := &officialRPC{authAccount: account, stdin: officialResponsesFixtureWriter{write: func(raw []byte) (int, error) { responses <- append([]byte(nil), raw...); return len(raw), nil }}}
			rpc.setAuthContext(ctx)
			if test == "canceled" {
				cancel()
			}
			reason, previous := "unauthorized", "workspace"
			if test == "wrong-account" {
				previous = "another"
			}
			if test == "wrong-reason" {
				reason = "arbitrary"
			}
			raw, _ := json.Marshal(map[string]any{"id": "refresh-request", "method": "account/chatgptAuthTokens/refresh", "params": map[string]string{"reason": reason, "previousAccountId": previous}})
			if !rpc.handleAuthRequest(raw) {
				t.Fatal("refresh request not intercepted")
			}
			select {
			case raw := <-responses:
				if strings.Contains(string(raw), "private-refresh") {
					t.Fatal("refresh token leaked into official process reply")
				}
				var reply map[string]json.RawMessage
				_ = json.Unmarshal(raw, &reply)
				if test == "ok" {
					if calls != 1 || !strings.Contains(string(reply["result"]), `"accessToken":"new-access"`) || !strings.Contains(string(reply["result"]), `"chatgptAccountId":"workspace"`) {
						t.Fatalf("wrong external auth reply: %s", raw)
					}
				} else if len(reply["error"]) == 0 {
					t.Fatalf("invalid recovery returned credentials: %s", raw)
				}
				if (test == "wrong-account" || test == "wrong-reason" || test == "canceled") && calls != 0 {
					t.Fatal("unauthorized recovery callback ran")
				}
			case <-time.After(time.Second):
				t.Fatal("official refresh reply deadlocked")
			}
		})
	}
}

func TestOfficialAuthRefreshReusesGatewaySerializedExchange(t *testing.T) {
	var exchanges atomic.Int32
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		exchanges.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, gatewayKnownOwnerRefreshFixture(`{"access_token":"replacement-access","refresh_token":"replacement-refresh","expires_in":3600}`, "workspace", "user"))
	})
	if err := p.Store.UpdateAccount("test", func(a *Account) error {
		a.AccountID = "workspace"
		a.Identity = &managedAccountIdentity{UserID: "user"}
		a.ExpiresAt = time.Now().Add(time.Hour).Unix()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	account, err := p.Store.Get("test")
	if err != nil {
		t.Fatal(err)
	}
	ctx := p.officialAuthContext(t.Context(), "test-client-key", "admin")
	callback := ctx.Value(officialAuthRecoveryKey{}).(officialAuthRecovery)
	var group sync.WaitGroup
	for i := 0; i < 6; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			updated, err := callback(ctx, account)
			if err != nil || updated.AccessToken != "replacement-access" {
				t.Errorf("recovery failed: %v", err)
			}
		}()
	}
	group.Wait()
	if exchanges.Load() != 1 {
		t.Fatalf("concurrent callbacks exchanged rotating refresh token %d times", exchanges.Load())
	}
	settings, _ := p.Settings.Get()
	settings.Upstream401RecoveryEnabled = false
	_ = p.Settings.Set(settings)
	if _, err := callback(ctx, account); err == nil || !strings.Contains(fmt.Sprint(err), "disabled") {
		t.Fatal("disabled recovery setting ignored")
	}
}

func TestOfficialAuthRefreshWhileModelRPCPending(t *testing.T) {
	for _, flag := range []string{"--refresh-before-models", "--catalog-unauthorized"} {
		t.Run(flag, func(t *testing.T) {
			p, account := officialManagedFixture(t, false)
			account.Identity = &managedAccountIdentity{UserID: "user"}
			original := p.Official.command
			p.Official.command = func(ctx context.Context, name string, args ...string) *exec.Cmd {
				cmd := original(ctx, name, args...)
				cmd.Args = append(cmd.Args, flag)
				return cmd
			}
			var calls atomic.Int32
			ctx := context.WithValue(t.Context(), officialAuthRecoveryKey{}, officialAuthRecovery(func(ctx context.Context, a Account) (Account, error) {
				calls.Add(1)
				a.AccessToken = "SENSITIVE_FIXTURE_TOKEN_REFRESHED"
				return a, nil
			}))
			if flag == "--catalog-unauthorized" {
				_, catalog, err := p.Official.clientToolModelCatalog(ctx, account, nil, "model-a")
				if err != nil || len(catalog) == 0 {
					t.Fatalf("catalog HTTP401 recovery failed: %v", err)
				}
			} else {
				rpc, err := p.Official.startRPC(ctx, account, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer rpc.close()
				if _, err := rpc.call(ctx, "model/list", map[string]any{"limit": 100, "includeHidden": true}); err != nil {
					t.Fatalf("refresh deadlocked outstanding model RPC: %v", err)
				}
			}
			if calls.Load() != 1 {
				t.Fatalf("recovery count %d", calls.Load())
			}
		})
	}
}

func TestOfficialModelAuthDiagnosticRequiresExplicitCatalog401(t *testing.T) {
	for _, test := range []struct {
		line string
		want bool
	}{
		{"ERROR codex_models_manager::manager: failed to refresh available models: unexpected status 401 Unauthorized\n", true},
		{"ERROR codex_models_manager::manager: failed to refresh available models: unexpected status 503 Service Unavailable\n", false},
		{"ERROR another_component: failed to refresh available models: 401 Unauthorized\n", false},
		{"ERROR codex_models_manager::manager: failed to write models cache: 401 Unauthorized\n", false},
	} {
		var diagnostic officialModelAuthDiagnostic
		for _, part := range strings.SplitAfter(test.line, " ") {
			_, _ = diagnostic.Write([]byte(part))
		}
		if diagnostic.unauthorized.Load() != test.want || len(diagnostic.line) != 0 {
			t.Fatalf("bad catalog diagnostic classification: %+v", test)
		}
	}
	var diagnostic officialModelAuthDiagnostic
	_, _ = diagnostic.Write([]byte(strings.Repeat("sensitive", 10000)))
	if len(diagnostic.line) > 8192 {
		t.Fatal("stderr observation is unbounded")
	}
}
