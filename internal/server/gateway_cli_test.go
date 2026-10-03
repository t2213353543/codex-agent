package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestNativeCLICompactJSON(t *testing.T) {
	const result = `{"object":"response.compaction","output":[{"type":"compaction","encrypted_content":"opaque-history"}],"usage":{"input_tokens":81,"output_tokens":9}}`
	for _, flags := range []string{"", `,"stream":false`, `,"stream":true,"store":true`} {
		t.Run(flags, func(t *testing.T) {
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				var body map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body["store"] != nil || body["stream"] != nil {
					t.Error("compact includes streaming flags")
				}
				if r.Header.Get("Accept") != "application/json" {
					t.Error("incorrect Accept")
				}
				w.Header().Set("X-Codex-Turn-State", "compact-route")
				_, _ = io.WriteString(w, result)
			})
			w := httptest.NewRecorder()
			p.ServeHTTP(w, proxyRequest(http.MethodPost, "/v1/responses/compact", `{"model":"test","input":[]`+flags+`}`))
			if w.Code != 200 || w.Body.String() != result || w.Header().Get("Content-Type") != "application/json" || w.Header().Get("X-Codex-Turn-State") != "compact-route" {
				t.Fatalf("compact response: %d %v %s", w.Code, w.Header(), w.Body.String())
			}
		})
	}
}

func TestNativeCLIWebsocketFallback(t *testing.T) {
	p := newTestProxy(t, 1)
	for _, authenticated := range []bool{false, true} {
		r := proxyRequest(http.MethodGet, "/v1/responses", "")
		r.Header.Set("Connection", "Upgrade")
		r.Header.Set("Upgrade", "websocket")
		if !authenticated {
			r.Header.Del("Authorization")
		}
		w := httptest.NewRecorder()
		p.ServeHTTP(w, r)
		want := 401
		if authenticated {
			want = 426
		}
		if w.Code != want {
			t.Fatalf("handshake status=%d want=%d", w.Code, want)
		}
	}
}

func TestGatewayFailedResponseRetainsUsage(t *testing.T) {
	p := proxyFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"message\":\"test failure\",\"code\":\"server_error\"},\"usage\":{\"input_tokens\":13,\"output_tokens\":7}}}\n\n")
	})
	w := httptest.NewRecorder()
	p.ServeHTTP(w, proxyRequest(http.MethodPost, "/responses", `{"model":"test","stream":false}`))
	rec, ok := p.Metrics.LastFor("test")
	if w.Code != 502 || !ok || rec.Status != "error" || rec.InputTokens != 13 || rec.OutputTokens != 7 || rec.Error != "test failure" {
		t.Fatalf("failed response lost usage: status=%d record=%+v", w.Code, rec)
	}
}

func TestGatewayModelsNotModified(t *testing.T) {
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != `"catalog-1"` {
			t.Error("missing model catalog validator")
		}
		w.Header().Set("ETag", `"catalog-1"`)
		w.WriteHeader(http.StatusNotModified)
	})
	r := proxyRequest(http.MethodGet, "/v1/models", "")
	r.Header.Set("If-None-Match", `"catalog-1"`)
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	rec, ok := p.Metrics.LastFor("test")
	if w.Code != 304 || w.Body.Len() != 0 || w.Header().Get("ETag") != `"catalog-1"` || !ok || rec.Status != "ok" {
		t.Fatalf("catalog validator result: %d %v %+v", w.Code, w.Header(), rec)
	}
}

func TestGatewaySessionIdentity(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/responses", nil)
	body := []byte(`{"prompt_cache_key":"cache"}`)
	for _, tc := range []struct{ header, value string }{
		{"", "cache"}, {"conversation_id", "legacy-thread"}, {"thread-id", "thread"}, {"session_id", "legacy-session"}, {"session-id", "root-session"},
	} {
		if tc.header != "" {
			r.Header.Set(tc.header, tc.value)
		}
		if got := gatewaySessionKey(r, body); got != tc.value {
			t.Fatalf("identity=%q want=%q", got, tc.value)
		}
	}
}

func TestGatewaySessionStaysOnAccount(t *testing.T) {
	p := newTestProxy(t, 1)
	ctx := context.Background()
	first, release, err := p.pickSessionAccount(ctx, "test-client-key", "admin", "native-session")
	if err != nil {
		t.Fatal(err)
	}
	release()
	if err := p.Store.ImportWith("other", []byte(`{"access_token":"other-token"}`), AccountDefaults{Concurrency: 2}); err != nil {
		t.Fatal(err)
	}
	if err := p.Store.UpdateAccount("other", func(a *Account) error { a.Priority = 100; return nil }); err != nil {
		t.Fatal(err)
	}
	p.invalidateAccounts()
	second, release, err := p.pickSessionAccount(ctx, "test-client-key", "admin", "native-session")
	if err != nil {
		t.Fatal(err)
	}
	if second.Name != first.Name {
		t.Fatal("session switched after priorities changed")
	}
	_, busyRelease, err := p.pickSessionAccount(ctx, "test-client-key", "admin", "native-session")
	if busyRelease != nil {
		busyRelease()
	}
	if err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("busy pinned account migrated: %v", err)
	}
	release()
	newAccount, newRelease, err := p.pickSessionAccount(ctx, "test-client-key", "admin", "new-session")
	if err != nil {
		t.Fatal(err)
	}
	newRelease()
	if newAccount.Name != "other" {
		t.Fatal("new session ignored priority")
	}
	if err := p.Store.UpdateAccount(first.Name, func(a *Account) error { a.Enabled = false; return nil }); err != nil {
		t.Fatal(err)
	}
	p.invalidateAccounts()
	_, disabledRelease, err := p.pickSessionAccount(ctx, "test-client-key", "admin", "native-session")
	if disabledRelease != nil {
		disabledRelease()
	}
	if err == nil || !strings.Contains(err.Error(), "no longer enabled") {
		t.Fatalf("disabled pinned account migrated: %v", err)
	}
}

func TestGatewaySessionAtomicClaimAndKeyIsolation(t *testing.T) {
	p := newTestProxy(t, 1)
	var wg sync.WaitGroup
	results := make(chan string, 40)
	for i := 0; i < 40; i++ {
		wg.Go(func() {
			value, err := p.sessionAccount(context.Background(), "key-a", "session", fmt.Sprintf("account-%d", i))
			if err != nil {
				t.Error(err)
			}
			results <- value
		})
	}
	wg.Wait()
	close(results)
	winner := ""
	for result := range results {
		if winner == "" {
			winner = result
		}
		if result != winner {
			t.Fatal("concurrent first requests selected different accounts")
		}
	}
	got, err := p.sessionAccount(context.Background(), "key-b", "session", "isolated")
	if err != nil || got != "isolated" {
		t.Fatalf("key isolation: %q %v", got, err)
	}
}

func TestGatewaySessionRejectsReplacedUpstreamIdentity(t *testing.T) {
	p := newTestProxy(t, 1)
	ctx := context.Background()
	original, release, err := p.pickSessionAccount(ctx, "test-client-key", "admin", "session")
	if err != nil {
		t.Fatal(err)
	}
	release()
	if err := p.Store.UpdateAccount(original.Name, func(a *Account) error { a.AccessToken = "refreshed-token"; return nil }); err != nil {
		t.Fatal(err)
	}
	p.invalidateAccounts()
	_, release, err = p.pickSessionAccount(ctx, "test-client-key", "admin", "session")
	if err != nil {
		t.Fatalf("token rotation broke affinity: %v", err)
	}
	release()
	if err := p.Store.UpdateAccount(original.Name, func(a *Account) error { a.AccountID = "different-chatgpt-account"; return nil }); err != nil {
		t.Fatal(err)
	}
	p.invalidateAccounts()
	_, release, err = p.pickSessionAccount(ctx, "test-client-key", "admin", "session")
	if release != nil {
		release()
	}
	if err == nil {
		t.Fatal("old encrypted history would be routed to a replaced ChatGPT account")
	}
}

func TestGatewayAtomicAdmissionAndAvailableSelection(t *testing.T) {
	p := newTestProxy(t, 1)
	account, err := p.Store.Get("test")
	if err != nil {
		t.Fatal(err)
	}
	admissionKey := gatewayAdmissionKey(account)
	var wg sync.WaitGroup
	var acquired atomic.Int32
	start := make(chan struct{})
	for range 80 {
		wg.Go(func() {
			<-start
			if p.acquire(admissionKey, 1) {
				acquired.Add(1)
			}
		})
	}
	close(start)
	wg.Wait()
	if acquired.Load() != 1 {
		t.Fatalf("concurrency exceeded: %d", acquired.Load())
	}
	if err := p.Store.ImportWith("available", []byte(`{"access_token":"second"}`), AccountDefaults{Concurrency: 1}); err != nil {
		t.Fatal(err)
	}
	if err := p.Store.UpdateAccount("test", func(a *Account) error { a.Weight = 10000; return nil }); err != nil {
		t.Fatal(err)
	}
	p.invalidateAccounts()
	for range 10 {
		account, release, _, _, err := p.pickAccount(context.Background(), "test-client-key")
		if err != nil {
			t.Fatal(err)
		}
		release()
		if account.Name != "available" {
			t.Fatal("picked busy account")
		}
	}
	p.release(admissionKey)
}

func TestGatewayBoundHTTPProxy(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host != "upstream.invalid" {
			t.Errorf("not an HTTP proxy request: %s", r.URL)
		}
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[]}}\n\n")
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	port, _ := strconv.Atoi(u.Port())
	p := newTestProxy(t, 1)
	p.BaseURL = "http://upstream.invalid"
	// pickProxy used to panic when this lazy map was first used.
	entry, err := p.Proxies.Create(ProxyEntry{Name: "local", Type: "http", Host: u.Hostname(), Port: port})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Store.UpdateAccount("test", func(a *Account) error { a.Proxies = []string{entry.ID}; return nil }); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	p.ServeHTTP(w, proxyRequest(http.MethodPost, "/responses", `{"model":"test","input":[],"stream":true}`))
	if w.Code != 200 {
		t.Fatalf("proxy forwarding: %d %s", w.Code, w.Body.String())
	}
}

func gatewayRedisSessionFixture(t *testing.T) (*Database, string) {
	t.Helper()
	redisURL := os.Getenv("TEST_REDIS_URL")
	if redisURL == "" {
		t.Skip("TEST_REDIS_URL is not configured")
	}
	options, err := redis.ParseURL(redisURL)
	if err != nil {
		t.Fatal("invalid TEST_REDIS_URL")
	}
	client := redis.NewClient(options)
	t.Cleanup(func() { _ = client.Close() })
	prefix := os.Getenv("TEST_REDIS_PREFIX")
	if prefix == "" {
		prefix = "codex-agent:test:"
	}
	prefix = strings.TrimRight(prefix, ":") + ":gateway:" + newID("") + ":"
	db := &Database{Redis: client, Namespace: prefix}
	key := fmt.Sprintf("%sgateway:thread:%x", prefix, sha256.Sum256([]byte("key\x00session")))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := client.Del(ctx, key).Err(); err != nil {
			t.Errorf("remove test session binding: %v", err)
		}
	})
	return db, key
}

func TestGatewayRedisSessionSharing(t *testing.T) {
	db, key := gatewayRedisSessionFixture(t)
	p1, p2 := &Proxy{Store: Store{DB: db}}, &Proxy{Store: Store{DB: db}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a, err := p1.sessionAccount(ctx, "key", "session", "id:original")
	if err != nil {
		t.Fatal(err)
	}
	b, err := p2.sessionAccount(ctx, "key", "session", "id:different")
	if err != nil || a != b {
		t.Fatalf("shared binding: %s %s %v", a, b, err)
	}
	if got, err := db.Redis.Get(ctx, key).Result(); err != nil || got != a {
		t.Fatalf("persisted binding: %q %v", got, err)
	}
	if ttl := db.Redis.TTL(ctx, key).Val(); ttl < gatewaySessionTTL-time.Minute {
		t.Fatalf("binding TTL=%s", ttl)
	}
}
