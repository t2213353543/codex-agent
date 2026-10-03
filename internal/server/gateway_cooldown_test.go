package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/redis/go-redis/v9"
)

func cooldownRequest(p *Proxy, model, session string) *httptest.ResponseRecorder {
	r := proxyRequest(http.MethodPost, "/v1/responses", fmt.Sprintf(`{"model":%q,"input":[],"stream":true}`, model))
	if session != "" {
		r.Header.Set("Session-Id", session)
	}
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	return w
}

func assertCooldownResponse(t *testing.T, w *httptest.ResponseRecorder, minimum, maximum int) {
	t.Helper()
	seconds, err := strconv.Atoi(w.Header().Get("Retry-After"))
	if w.Code != http.StatusTooManyRequests || err != nil || seconds < minimum || seconds > maximum {
		t.Fatalf("cooldown status=%d Retry-After=%q", w.Code, w.Header().Get("Retry-After"))
	}
}

func TestGatewayCooldownRoutesIndependentRequestsButKeepsSession(t *testing.T) {
	var aCalls, bCalls atomic.Int32
	p := cooldownProxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer test-upstream-token" {
			aCalls.Add(1)
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(429)
			_, _ = io.WriteString(w, `{"error":{"code":"rate_limit_exceeded"}}`)
			return
		}
		bCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"ok\",\"status\":\"completed\"}}\n\n")
	})
	if err := p.Store.ImportWith("other", []byte(`{"access_token":"other-token"}`), AccountDefaults{}); err != nil {
		t.Fatal(err)
	}
	if err := p.Store.UpdateAccount("test", func(a *Account) error { a.Priority = 100; return nil }); err != nil {
		t.Fatal(err)
	}
	p.invalidateAccounts()
	assertCooldownResponse(t, cooldownRequest(p, "model-a", "bound"), 30, 30)
	assertCooldownResponse(t, cooldownRequest(p, "model-a", "bound"), 29, 30)
	if aCalls.Load() != 1 || bCalls.Load() != 0 {
		t.Fatal("429 was replayed or pinned session changed account")
	}
	if got := cooldownRequest(p, "model-a", ""); got.Code != 200 {
		t.Fatalf("independent request did not use other account: %d", got.Code)
	}
	if got := cooldownRequest(p, "model-a", "new-session"); got.Code != 200 {
		t.Fatalf("new session did not use other account: %d", got.Code)
	}
	assertCooldownResponse(t, cooldownRequest(p, "model-b", "bound"), 30, 30)
	if aCalls.Load() != 2 || bCalls.Load() != 2 {
		t.Fatal("model scope or account selection incorrect")
	}
	// Expiry releases the original account, without sleeping or changing a binding.
	p.cooldownMu.Lock()
	for key := range p.cooldowns {
		p.cooldowns[key] = time.Now().Add(-time.Second)
	}
	p.cooldownMu.Unlock()
	assertCooldownResponse(t, cooldownRequest(p, "model-a", "bound"), 30, 30)
	if aCalls.Load() != 3 || bCalls.Load() != 2 {
		t.Fatal("expired original account did not become selectable")
	}
}

func TestGatewayCooldownRetryAfterAndCredentialIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, retry string
		min, max    time.Duration
	}{
		{"seconds", "30", 29 * time.Second, 30 * time.Second},
		{"long_delay", "1800", 1799 * time.Second, 1800 * time.Second},
		{"http_date", time.Now().Add(30 * time.Second).UTC().Format(http.TimeFormat), 28 * time.Second, 30 * time.Second},
		{"missing", "", 3 * time.Second, 4 * time.Second},
		{"invalid", "bad", 3 * time.Second, 4 * time.Second},
		{"bounded", "9999999999999999999999999", time.Hour - time.Second, time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &Proxy{Store: Store{DB: &Database{}}} // No Redis: local cooldown with persisted-style credential generation.
			enableRateLimitCooldownForTest(t, p)
			account := Account{ID: "stable", Name: "original", CredentialVersion: 3, AccessToken: "synthetic-private-token"}
			ctx := withGatewayCooldownModel(context.Background(), "model")
			p.recordGatewayCooldown(ctx, account, tc.retry)
			var cooldown *gatewayCooldownError
			if err := p.checkGatewayCooldown(ctx, account); !errors.As(err, &cooldown) {
				t.Fatalf("missing cooldown: %v", err)
			}
			if delay := time.Until(cooldown.until); delay < tc.min || delay > tc.max {
				t.Fatalf("delay=%s", delay)
			}
			renamed := account
			renamed.Name = "renamed"
			renamed.Version++
			if p.checkGatewayCooldown(ctx, renamed) == nil {
				t.Fatal("management rename cleared cooldown")
			}
			rotated := account
			rotated.AccessToken = "replacement-token"
			if p.checkGatewayCooldown(ctx, rotated) != nil {
				t.Fatal("new actual token inherited cooldown")
			}
			reimported := account
			reimported.CredentialVersion++
			if p.checkGatewayCooldown(ctx, reimported) != nil {
				t.Fatal("new credential generation inherited cooldown")
			}
			if p.checkGatewayCooldown(withGatewayCooldownModel(ctx, "other-model"), account) != nil {
				t.Fatal("unrelated model cooled")
			}
			for key := range p.cooldowns {
				if len(key) != 64 || strings.Contains(key, "synthetic") {
					t.Fatal("cooldown key contains credentials")
				}
			}
			// Concurrent/late short responses cannot shorten the original delay.
			before := cooldown.until
			p.recordGatewayCooldown(ctx, account, "1")
			if err := p.checkGatewayCooldown(ctx, account); !errors.As(err, &cooldown) || cooldown.until.Before(before) {
				t.Fatal("short response shortened active cooldown")
			}
		})
	}
	for _, retry := range []string{"0", time.Now().Add(-time.Minute).UTC().Format(http.TimeFormat)} {
		p := &Proxy{}
		enableRateLimitCooldownForTest(t, p)
		p.recordGatewayCooldown(context.Background(), Account{ID: "zero"}, retry)
		if p.checkGatewayCooldown(context.Background(), Account{ID: "zero"}) != nil {
			t.Fatal("explicit zero or past date became default delay")
		}
	}
}

func TestGatewayCooldownBoundedStateDoesNotEvictActiveDeadline(t *testing.T) {
	p := &Proxy{cooldowns: make(map[string]time.Time)}
	enableRateLimitCooldownForTest(t, p)
	until := time.Now().Add(30 * time.Minute)
	for i := range maxGatewayCooldowns {
		p.cooldowns[fmt.Sprint(i)] = until
	}
	p.recordGatewayCooldown(withGatewayCooldownModel(context.Background(), "excess"), Account{ID: "additional"}, "30")
	if len(p.cooldowns) != maxGatewayCooldowns || p.cooldowns["0"] != until {
		t.Fatal("capacity dropped active deadline or exceeded bound")
	}
	var limited *gatewayCooldownError
	if err := p.checkGatewayCooldown(context.Background(), Account{ID: "unknown"}); !errors.As(err, &limited) {
		t.Fatalf("capacity did not fail closed: %v", err)
	}
	p.cooldownMu.Lock()
	p.cooldownOverflowUntil = time.Now().Add(-time.Second)
	for key := range p.cooldowns {
		p.cooldowns[key] = time.Now().Add(-time.Second)
	}
	p.cooldownMu.Unlock()
	p.recordGatewayCooldown(context.Background(), Account{ID: "fresh"}, "10")
	if len(p.cooldowns) != 1 {
		t.Fatal("expired state not pruned")
	}
}

func TestGatewayCooldownModelsAndBusyUseTyped429(t *testing.T) {
	var calls atomic.Int32
	p := cooldownProxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(429) // Missing header uses bounded 4-second default on next request.
	})
	request := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		p.ServeHTTP(w, proxyRequest("GET", "/v1/models", ""))
		return w
	}
	if request().Code != 429 {
		t.Fatal("original models status lost")
	}
	assertCooldownResponse(t, request(), 3, 4)
	if calls.Load() != 1 {
		t.Fatal("models request replayed during cooldown")
	}
	account, err := p.Store.Get("test")
	if err != nil {
		t.Fatal(err)
	}
	if p.checkGatewayCooldown(withGatewayCooldownModel(context.Background(), "model"), account) != nil {
		t.Fatal("catalog cooldown leaked into model inference")
	}
	busy := cooldownNewTestProxy(t, 1)
	account, _ = busy.Store.Get("test")
	release, ok := busy.acquireGatewayAccount(account)
	if !ok {
		t.Fatal("cannot occupy slot")
	}
	defer release()
	w := httptest.NewRecorder()
	busy.ServeHTTP(w, proxyRequest("GET", "/v1/models", ""))
	assertCooldownResponse(t, w, 1, 1)
	assertCooldownResponse(t, cooldownRequest(busy, "model", ""), 1, 1)
}

func TestGatewayCooldownAfter401UsesRefreshedCredentials(t *testing.T) {
	var oauthCalls, responseCalls atomic.Int32
	p := cooldownProxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" {
			oauthCalls.Add(1)
			_, _ = io.WriteString(w, gatewayKnownOwnerRefreshFixture(`{"access_token":"fresh-synthetic-token","refresh_token":"fresh-refresh","expires_in":3600}`, "test-account", "fixture-user"))
			return
		}
		responseCalls.Add(1)
		if r.Header.Get("Authorization") == "Bearer test-upstream-token" {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(429)
	})
	assertCooldownResponse(t, cooldownRequest(p, "model", "same"), 30, 30)
	assertCooldownResponse(t, cooldownRequest(p, "model", "same"), 29, 30)
	if responseCalls.Load() != 2 || oauthCalls.Load() != 1 {
		t.Fatalf("post-refresh 429 bypassed or retried: response=%d oauth=%d", responseCalls.Load(), oauthCalls.Load())
	}
}

func TestGatewayCooldownHTTPAcceptedRateLimitEvents(t *testing.T) {
	for _, tc := range []struct {
		name               string
		stream, recover401 bool
		event              string
	}{
		{"stream_failed", true, false, `{"type":"response.failed","headers":{"retry-after":"30"},"response":{"id":"limited","status":"failed","error":{"code":"rate_limit_exceeded"}}}`},
		{"collected_failed", false, false, `{"type":"response.failed","headers":{"retry-after":"30"},"response":{"id":"limited","status":"failed","error":{"code":"rate_limit_exceeded"}}}`},
		{"refreshed_stream_failed", true, true, `{"type":"response.failed","headers":{"retry-after":"30"},"response":{"id":"limited","status":"failed","error":{"code":"rate_limit_exceeded"}}}`},
		{"top_level_error", true, false, `{"type":"error","status":429,"code":"rate_limit_exceeded","headers":{"Retry-After":"30"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls, oauth atomic.Int32
			p := cooldownProxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/oauth/token" {
					oauth.Add(1)
					_, _ = io.WriteString(w, gatewayKnownOwnerRefreshFixture(`{"access_token":"fresh-event-token","refresh_token":"fresh-refresh","expires_in":3600}`, "test-account", "fixture-user"))
					return
				}
				calls.Add(1)
				if tc.recover401 && r.Header.Get("Authorization") == "Bearer test-upstream-token" {
					w.WriteHeader(401)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "data: %s\n\n", tc.event)
			})
			request := func() *httptest.ResponseRecorder {
				w := httptest.NewRecorder()
				p.ServeHTTP(w, proxyRequest("POST", "/v1/responses", fmt.Sprintf(`{"model":"model","input":[],"stream":%t}`, tc.stream)))
				return w
			}
			first := request()
			want := 502
			if tc.stream {
				want = 200
			}
			if first.Code != want {
				t.Fatalf("accepted event changed current response status: %d", first.Code)
			}
			assertCooldownResponse(t, request(), 29, 30)
			expectedCalls, expectedOAuth := int32(1), int32(0)
			if tc.recover401 {
				expectedCalls, expectedOAuth = 2, 1
			}
			if calls.Load() != expectedCalls || oauth.Load() != expectedOAuth {
				t.Fatal("accepted SSE rate limit replayed or stored under stale credentials")
			}
		})
	}
	t.Run("unrelated_failure", func(t *testing.T) {
		var calls atomic.Int32
		p := cooldownProxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			_, _ = io.WriteString(w, "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"invalid_request\",\"message\":\"mentions 429 rate limit but is not a rate limit\"}}}\n\n")
		})
		for range 2 {
			if cooldownRequest(p, "model", "").Code != 200 {
				t.Fatal("ordinary failure created cooldown")
			}
		}
		if calls.Load() != 2 {
			t.Fatal("message keywords affected rate-limit classification")
		}
	})
}

func TestGatewayCooldownWebSocketChecksEveryTurnAndRecordsRateLimit(t *testing.T) {
	var calls atomic.Int32
	p := cooldownProxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		for {
			_, raw, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			calls.Add(1)
			var body struct {
				Model    string `json:"model"`
				StreamID string `json:"stream_id"`
			}
			if json.Unmarshal(raw, &body) != nil {
				return
			}
			var event string
			if body.Model == "limited" {
				event = fmt.Sprintf(`{"type":"response.failed","stream_id":%q,"headers":{"Retry-After":"30"},"response":{"id":"failed","status":"failed","error":{"code":"rate_limit_exceeded","message":"controlled limit"}}}`, body.StreamID)
			} else {
				event = fmt.Sprintf(`{"type":"response.completed","stream_id":%q,"response":{"id":"ok","status":"completed"}}`, body.StreamID)
			}
			if conn.Write(r.Context(), websocket.MessageText, []byte(event)) != nil {
				return
			}
		}
	})
	enableGatewayWS(t, p, "")
	server := httptest.NewServer(p)
	defer server.Close()
	conn, _ := gatewayWSDial(t, server.URL, "ws-cooldown")
	write := func(model, stream string) {
		t.Helper()
		err := conn.Write(context.Background(), websocket.MessageText, []byte(fmt.Sprintf(`{"type":"response.create","model":%q,"stream_id":%q,"input":[]}`, model, stream)))
		if err != nil {
			t.Fatal(err)
		}
	}
	write("limited", "first")
	if !strings.Contains(string(gatewayWSRead(t, conn)), "response.failed") {
		t.Fatal("upstream failure not forwarded")
	}
	write("limited", "second")
	var blocked struct {
		Status  int               `json:"status"`
		Stream  string            `json:"stream_id"`
		Headers map[string]string `json:"headers"`
	}
	if json.Unmarshal(gatewayWSRead(t, conn), &blocked) != nil || blocked.Status != 429 || blocked.Stream != "second" || blocked.Headers["Retry-After"] != "30" {
		t.Fatalf("missing scoped cooldown: %+v", blocked)
	}
	write("other", "third")
	if !strings.Contains(string(gatewayWSRead(t, conn)), "response.completed") {
		t.Fatal("other model blocked by model-specific cooldown")
	}
	if calls.Load() != 2 {
		t.Fatalf("cooling WebSocket turn reached upstream: %d", calls.Load())
	}
}

func TestGatewayCooldownWebSocketRejectedUpgrade(t *testing.T) {
	var calls atomic.Int32
	p := cooldownProxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(429)
	})
	enableGatewayWS(t, p, "")
	server := httptest.NewServer(p)
	defer server.Close()
	for range 2 {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		conn, response, err := websocket.Dial(ctx, server.URL+"/v1/responses", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer test-client-key"}, "Session-Id": {"same"}}})
		cancel()
		if conn != nil {
			conn.CloseNow()
		}
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		if err == nil || response == nil || response.StatusCode != 429 || response.Header.Get("Retry-After") != "30" {
			t.Fatal("upgrade did not retain retry metadata")
		}
	}
	if calls.Load() != 1 {
		t.Fatal("rejected upgrade replayed during cooldown")
	}
}

type cooldownRedisFailureHook struct{}

func (cooldownRedisFailureHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (cooldownRedisFailureHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (cooldownRedisFailureHook) ProcessHook(redis.ProcessHook) redis.ProcessHook {
	return func(context.Context, redis.Cmder) error { return errors.New("synthetic Redis outage") }
}

func TestGatewayCooldownSharedReadFailureIsTypedAndFailsClosed(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	defer rdb.Close()
	rdb.AddHook(cooldownRedisFailureHook{})
	p := &Proxy{Store: Store{DB: &Database{Redis: rdb, Namespace: "synthetic:"}}}
	enableRateLimitCooldownForTest(t, p)
	err := p.checkGatewayCooldown(withGatewayCooldownModel(context.Background(), "model"), Account{ID: "bound"})
	status, retry, recognized := gatewaySelectionFailure(err)
	if !recognized || status != 503 || retry != "" {
		t.Fatalf("shared state failed open: %v", err)
	}
	if len(p.cooldowns) != 0 {
		t.Fatal("failed shared read invented local routing state")
	}
}

func TestGatewayCooldownRedisSharing(t *testing.T) {
	db, _ := gatewayRedisSessionFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	redisKey := db.Namespace + "gateway:cooldowns"
	t.Cleanup(func() {
		if err := db.Redis.Del(context.Background(), redisKey).Err(); err != nil {
			t.Error("cannot remove test cooldown key")
		}
	})
	p1, p2 := &Proxy{Store: Store{DB: db}}, &Proxy{Store: Store{DB: db}}
	enableRateLimitCooldownForTest(t, p1)
	enableRateLimitCooldownForTest(t, p2)
	a := Account{ID: "synthetic", CredentialVersion: 2, AccessToken: "secret-never-stored"}
	modelCtx := withGatewayCooldownModel(ctx, "model")
	p1.recordGatewayCooldown(modelCtx, a, "30")
	var limited *gatewayCooldownError
	if err := p2.checkGatewayCooldown(modelCtx, a); !errors.As(err, &limited) || time.Until(limited.until) < 29*time.Second {
		t.Fatalf("cross-instance cooldown unavailable: %v", err)
	}
	p2.recordGatewayCooldown(modelCtx, a, "1")
	if err := p1.checkGatewayCooldown(modelCtx, a); !errors.As(err, &limited) || time.Until(limited.until) < 29*time.Second {
		t.Fatal("shared short response shortened deadline")
	}
	if p2.checkGatewayCooldown(withGatewayCooldownModel(ctx, "other"), a) != nil {
		t.Fatal("shared model isolation failed")
	}
	a.CredentialVersion++
	if p2.checkGatewayCooldown(modelCtx, a) != nil {
		t.Fatal("shared credential version did not reset")
	}
	members, err := db.Redis.ZRange(ctx, redisKey, 0, -1).Result()
	if err != nil || len(members) != 1 || len(members[0]) != 64 {
		t.Fatal("shared state stores something other than hash keys")
	}
	// Expire the Redis entry from a second process; no local cache on p2 may
	// extend a first-process-only record after Redis has authoritatively expired.
	p3 := &Proxy{Store: Store{DB: db}}
	enableRateLimitCooldownForTest(t, p3)
	if err := db.Redis.ZAdd(ctx, redisKey, redis.Z{Score: float64(time.Now().Add(-time.Second).UnixMilli()), Member: members[0]}).Err(); err != nil {
		t.Fatal("cannot expire synthetic cooldown")
	}
	a.CredentialVersion--
	if err := p3.checkGatewayCooldown(modelCtx, a); err != nil {
		t.Fatalf("expired Redis cooldown survived: %v", err)
	}
	// Bound the shared set too: active entries are never evicted to make room.
	future := time.Now().Add(10 * time.Minute).UnixMilli()
	entries := make([]redis.Z, maxGatewayCooldowns)
	for i := range entries {
		entries[i] = redis.Z{Score: float64(future), Member: fmt.Sprintf("%064x", i)}
	}
	if err := db.Redis.ZAdd(ctx, redisKey, entries...).Err(); err != nil {
		t.Fatal("cannot fill synthetic cooldown set")
	}
	p3.recordGatewayCooldown(modelCtx, a, "30")
	if count, err := db.Redis.ZCard(ctx, redisKey).Result(); err != nil || count != maxGatewayCooldowns+1 {
		t.Fatalf("shared bound incorrect: count=%d err=%v", count, err)
	}
	if score, err := db.Redis.ZScore(ctx, redisKey, fmt.Sprintf("%064x", 0)).Result(); err != nil || int64(score) != future {
		t.Fatal("capacity evicted active shared deadline")
	}
	if err := p2.checkGatewayCooldown(withGatewayCooldownModel(ctx, "unseen"), a); !errors.As(err, &limited) {
		t.Fatal("shared capacity marker did not fail closed")
	}
}

func TestPGGatewayCooldownSelectionBindingAndReplacement(t *testing.T) {
	p1, p2, token := adminKeyGatewayPGProxies(t)
	enableRateLimitCooldownForTest(t, p1)
	enableRateLimitCooldownForTest(t, p2)
	redisDB, _ := gatewayRedisSessionFixture(t)
	p1.Store.DB.Redis, p1.Store.DB.Namespace = redisDB.Redis, redisDB.Namespace
	ctx := withGatewayCooldownModel(context.Background(), "limited-model")
	t.Cleanup(func() {
		thread := fmt.Sprintf("%sgateway:thread:%x", redisDB.Namespace, sha256.Sum256([]byte("admin\x00bound")))
		_ = redisDB.Redis.Del(context.Background(), redisDB.Namespace+"gateway:cooldowns", thread).Err()
	})
	initial := []byte(`{"access_token":"pg-preferred-token","refresh_token":"refresh-pg-preferred-token","chatgpt_account_id":"cooldown-workspace","chatgpt_user_id":"cooldown-user"}`)
	if report, err := p1.Store.ImportDetailed([]ImportItem{{Name: "preferred", Credentials: initial}}, AccountDefaults{}, ImportOptions{}); err != nil || report.Added != 1 {
		t.Fatalf("real PG known-owner fixture import failed: %v", err)
	}
	a, err := p1.Store.Get("preferred")
	if err != nil || !gatewayCompleteOwner(a) {
		t.Fatalf("cooldown replacement fixture requires complete ownership: %v", err)
	}
	_ = pgImportForTest(t, p1.Store, "fallback", "pg-fallback-token")
	if err := p1.Store.UpdateAccount(a.ID, func(a *Account) error { a.Priority = 100; return nil }); err != nil {
		t.Fatal(err)
	}
	first, release, err := p1.pickSessionAccount(ctx, token, "admin", "bound")
	if err != nil || first.ID != a.ID {
		t.Fatalf("cannot establish real PG account binding: %v", err)
	}
	release()
	p1.recordGatewayCooldown(ctx, first, "30")
	var cooldown *gatewayCooldownError
	_, release, err = p2.pickSessionAccount(ctx, token, "admin", "bound")
	if release != nil {
		release()
	}
	if !errors.As(err, &cooldown) {
		t.Fatalf("second process ignored shared pinned cooldown: %v", err)
	}
	other, release, _, _, err := p2.pickAccount(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if other.Name != "fallback" {
		t.Fatal("independent PG request ignored cooled priority account")
	}
	// A Redis outage must not create a file-store fallback or replace the owner.
	broken := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	defer broken.Close()
	broken.AddHook(cooldownRedisFailureHook{})
	p3 := &Proxy{Store: Store{DB: &Database{Redis: broken, Namespace: redisDB.Namespace}}}
	enableRateLimitCooldownForTest(t, p3)
	_, release, err = p3.pickSessionAccount(ctx, token, "admin", "bound")
	if release != nil {
		release()
	}
	if err == nil || len(p3.sessionRoutes) != 0 {
		t.Fatal("Redis outage silently migrated shared binding")
	}
	if bound, err := p1.sessionAccount(ctx, "admin", "bound", ""); err != nil || bound != gatewayAccountRef(first) {
		t.Fatal("shared binding changed during outage")
	}
	// The import API treats identical tokens as no-ops; a real replacement
	// increments the persisted generation and makes the same known owner eligible.
	raw := []byte(`{"access_token":"pg-replacement-token","refresh_token":"replacement-refresh","chatgpt_account_id":"cooldown-workspace","chatgpt_user_id":"cooldown-user"}`)
	if report, err := p1.Store.ImportDetailed([]ImportItem{{Name: first.Name, Credentials: raw}}, AccountDefaults{}, ImportOptions{Replace: true}); err != nil || report.Replaced != 1 {
		t.Fatalf("real PG credential replacement failed: %v", err)
	}
	p2.invalidateAccounts()
	updated, release, err := p2.pickSessionAccount(ctx, token, "admin", "bound")
	if err != nil {
		t.Fatal(err)
	}
	release()
	if updated.ID != first.ID || updated.CredentialVersion == first.CredentialVersion || !gatewayCompleteOwner(updated) || gatewayAccountRef(updated) != gatewayAccountRef(first) {
		t.Fatal("replacement lost identity or retained credential generation")
	}
}
