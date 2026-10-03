package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestAuditSessionIdentityMatchesForwardedHeaders(t *testing.T) {
	for _, test := range []struct {
		name   string
		header http.Header
		want   string
	}{
		{"native_root_before_thread", http.Header{"Session-Id": {"root"}, "Thread-Id": {"child"}}, "root"},
		{"raw_lowercase", http.Header{"session-id": {"root"}}, "root"},
		{"raw_uppercase", http.Header{"SESSION-ID": {"root"}}, "root"},
		{"canonical_scalar_wins", http.Header{"Session-Id": {"root", "second"}, "session-id": {"shadow"}}, "root"},
		{"deterministic_noncanonical_scalar", http.Header{"SESSION-ID": {"upper"}, "session-id": {"lower"}}, "upper"},
		{"hop_session_uses_thread", http.Header{"Session-Id": {"hop-only"}, "Thread-Id": {"thread"}, "Connection": {"keep-alive, Session-ID"}}, "thread"},
		{"hop_all_uses_body", http.Header{"Session-Id": {"hop-only"}, "Thread-Id": {"thread"}, "cOnNeCtIoN": {"session-id", "THREAD-ID"}}, "body-cache"},
		{"legacy_noncanonical", http.Header{"session_id": {"legacy"}, "conversation_id": {"conversation"}}, "legacy"},
		{"blank_first_scalar_uses_thread", http.Header{"Session-Id": {" ", "shadow"}, "Thread-Id": {"thread"}}, "thread"},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/responses", nil)
			r.Header = test.header.Clone()
			body := []byte(`{"prompt_cache_key":"body-cache"}`)
			forwarded := make(http.Header)
			copyRequestHeaders(forwarded, r.Header)
			wire := r.Clone(t.Context())
			wire.Header = forwarded
			if got := gatewaySessionKey(r, body); got != test.want {
				t.Fatalf("route identity=%q, want actual upstream identity=%q; forwarded=%v", got, test.want, forwarded)
			}
			if original, wireKey := gatewaySessionKey(r, body), gatewaySessionKey(wire, body); original != wireKey {
				t.Fatalf("routing before and after header forwarding differs: %q != %q", original, wireKey)
			}
		})
	}
}

func auditSessionMapKey(keyID, session string) string {
	sum := sha256.Sum256([]byte(keyID + "\x00" + session))
	return hex.EncodeToString(sum[:])
}

func TestAuditSessionCanceledDoesNotClaimOrRenew(t *testing.T) {
	for _, existing := range []bool{false, true} {
		for _, proposed := range []string{"", "new-account"} {
			t.Run(fmt.Sprintf("existing=%v/proposed=%s", existing, proposed), func(t *testing.T) {
				p := newTestProxy(t, 1)
				key := auditSessionMapKey("key", "session")
				before := gatewaySessionRoute{Account: "original", Expires: time.Now().Add(time.Minute)}
				if existing {
					p.sessionRoutes = map[string]gatewaySessionRoute{key: before}
				}
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				value, err := p.sessionAccount(ctx, "key", "session", proposed)
				if value != "" || !errors.Is(err, context.Canceled) {
					t.Errorf("canceled request returned value=%q err=%v", value, err)
				}
				if existing {
					if p.sessionRoutes[key] != before {
						t.Fatal("canceled request renewed an existing account binding")
					}
				} else if len(p.sessionRoutes) != 0 {
					t.Fatal("canceled request established an account binding")
				}
			})
		}
	}
}

// A Redis hook isolates the cancellation boundary without an external server.
// It cancels after command dispatch, like a caller abandoning an in-flight read.
type auditSessionRedisCancelHook struct {
	cancel context.CancelFunc
	calls  int
}

func (h *auditSessionRedisCancelHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h *auditSessionRedisCancelHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (h *auditSessionRedisCancelHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		h.calls++
		h.cancel()
		return context.Canceled
	}
}

func TestAuditSessionRedisPreservesCallerCancellation(t *testing.T) {
	for _, proposed := range []string{"", "account"} {
		t.Run("proposed="+proposed, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			client := redis.NewClient(&redis.Options{Addr: "unused.invalid:6379"})
			t.Cleanup(func() { _ = client.Close() })
			hook := &auditSessionRedisCancelHook{cancel: cancel}
			client.AddHook(hook)
			p := &Proxy{Store: Store{DB: &Database{Redis: client}}}
			value, err := p.sessionAccount(ctx, "key", "session", proposed)
			if value != "" || !errors.Is(err, context.Canceled) || hook.calls != 1 {
				t.Fatalf("in-flight cancellation masked: value=%q err=%v calls=%d", value, err, hook.calls)
			}
			if len(p.sessionRoutes) != 0 {
				t.Fatal("Redis cancellation installed a local fallback")
			}
		})
	}
}

func TestAuditSessionCanceledDoesNotDispatchRedis(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	client := redis.NewClient(&redis.Options{Addr: "unused.invalid:6379"})
	t.Cleanup(func() { _ = client.Close() })
	hook := &auditSessionRedisCancelHook{cancel: cancel}
	client.AddHook(hook)
	p := &Proxy{Store: Store{DB: &Database{Redis: client}}}
	for _, proposed := range []string{"", "account"} {
		if got, err := p.sessionAccount(ctx, "key", "session", proposed); got != "" || !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled Redis request returned value=%q err=%v", got, err)
		}
	}
	if hook.calls != 0 {
		t.Fatal("canceled request dispatched a Redis claim/renew command")
	}
	p.Store.DB.Redis = nil
	if _, err := p.sessionAccount(ctx, "key", "session", "account"); !errors.Is(err, context.Canceled) {
		t.Fatal("missing Redis masked a caller cancellation")
	}
}

func TestAuditSessionPersistedTTLBoundsBeforeDurationConversion(t *testing.T) {
	for _, seconds := range []int64{60, 2592000, 59, -1, 2592001, 18446744134} {
		t.Run(strconv.FormatInt(seconds, 10), func(t *testing.T) {
			if strconv.IntSize == 32 && seconds > 1<<31-1 {
				t.Skip("persisted TTL exceeds the platform integer width")
			}
			path := filepath.Join(t.TempDir(), "settings.json")
			// Storage reads may contain old or manually edited settings which did
			// not pass through the current management API's range validation.
			raw := fmt.Sprintf(`{"version":1,"sticky_session_ttl_seconds":%d}`, seconds)
			if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			p := &Proxy{Settings: NewSettingsStore(path)}
			if seconds < 60 || seconds > 2592000 {
				if _, err := p.sessionAccount(t.Context(), "key", "session", "account"); err == nil {
					t.Fatal("invalid persisted TTL was accepted")
				}
				if len(p.sessionRoutes) != 0 {
					t.Fatal("invalid persisted settings created a session binding")
				}
				return
			}
			if _, err := p.sessionAccount(t.Context(), "key", "session", "account"); err != nil {
				t.Fatal(err)
			}
			want := time.Duration(seconds) * time.Second
			remaining := time.Until(p.sessionRoutes[auditSessionMapKey("key", "session")].Expires)
			if remaining > want || remaining < want-time.Second {
				t.Fatalf("persisted TTL=%d seconds produced %s, want %s", seconds, remaining, want)
			}
		})
	}
}

func TestAuditSessionInitialClaimChecksCurrentKeyIdentity(t *testing.T) {
	p := newTestProxy(t, 1)
	key, account := gatewaySessionAuthorizationFixture(t, p)
	_, release, err := p.pickSessionAccount(t.Context(), key.Key, "obsolete-key-id", "first-turn")
	if release != nil {
		release()
	}
	if err == nil {
		t.Fatal("current token was allowed to claim a session under a different key identity")
	}
	if len(p.sessionRoutes) != 0 || p.inFlight(gatewayAdmissionKey(account)) != 0 {
		t.Fatal("key identity mismatch retained a session claim or admission")
	}
}

func TestAuditSessionCapacityDoesNotEvictActiveHistory(t *testing.T) {
	p := &Proxy{sessionRoutes: make(map[string]gatewaySessionRoute, maxGatewaySessions)}
	for i := range maxGatewaySessions {
		p.sessionRoutes[fmt.Sprint(i)] = gatewaySessionRoute{Account: "account", Expires: time.Now().Add(time.Hour)}
	}
	if _, err := p.sessionAccount(t.Context(), "key", "new", "different"); err == nil || len(p.sessionRoutes) != maxGatewaySessions {
		t.Fatal("capacity pressure discarded active history or silently exceeded the limit")
	}
	expired := p.sessionRoutes["0"]
	expired.Expires = time.Now().Add(-time.Second)
	p.sessionRoutes["0"] = expired
	if got, err := p.sessionAccount(t.Context(), "key", "new", "different"); err != nil || got != "different" || len(p.sessionRoutes) != maxGatewaySessions {
		t.Fatalf("expired route could not reclaim capacity: value=%q err=%v routes=%d", got, err, len(p.sessionRoutes))
	}
	if _, exists := p.sessionRoutes["0"]; exists {
		t.Fatal("expired route survived capacity reclamation")
	}
}

func TestAuditSessionLeaseCanceledAndConcurrentStop(t *testing.T) {
	p := newTestProxy(t, 1)
	parent, cancel := context.WithCancel(t.Context())
	cancel()
	ctx, stop := p.keepGatewaySessionEvery(parent, "key", "session", Account{ID: "account"}, time.Nanosecond)
	var stopped sync.WaitGroup
	for range 8 {
		stopped.Go(stop)
	}
	done := make(chan struct{})
	go func() { stopped.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("concurrent lease stop did not finish")
	}
	if !errors.Is(ctx.Err(), context.Canceled) || len(p.sessionRoutes) != 0 {
		t.Fatal("canceled lease renewed a binding or failed to propagate cancellation")
	}
}

func TestAuditSessionKeyLookupDoesNotRetainNegativeCache(t *testing.T) {
	p := newTestProxy(t, 1)
	key, _ := gatewaySessionAuthorizationFixture(t, p)
	writer := gatewayKeyAuthorizationWriter(t, p, key)
	if _, err := writer.Update(key.ID, func(key *APIKey) error { key.Enabled = false; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, found, err := p.Keys.LookupCurrent(t.Context(), key.Key); found || err != nil {
		t.Fatal("disabled key was accepted")
	}
	if _, err := writer.Update(key.ID, func(key *APIKey) error { key.Enabled = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if current, found, err := p.Keys.LookupCurrent(t.Context(), key.Key); !found || err != nil || current.ID != key.ID {
		t.Fatal("authoritative re-enable retained a negative authorization result")
	}
}
