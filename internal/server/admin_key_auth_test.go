package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestAdminKeyExplicitBearerNeverFallsBack(t *testing.T) {
	p, handler := webTestSetup(t)
	for _, tc := range []struct {
		name    string
		headers http.Header
		want    int
	}{
		{"valid", http.Header{"Authorization": {"Bearer " + webTestKey}}, 200},
		{"case_insensitive", http.Header{"authorization": {"bEaReR " + webTestKey}}, 200},
		{"empty", http.Header{"Authorization": {""}}, 401},
		{"no_values", http.Header{"Authorization": nil}, 401},
		{"wrong_key", http.Header{"Authorization": {"Bearer incorrect"}}, 401},
		{"wrong_scheme", http.Header{"Authorization": {"Basic " + webTestKey}}, 401},
		{"missing_token", http.Header{"Authorization": {"Bearer"}}, 401},
		{"duplicate", http.Header{"Authorization": {"Bearer " + webTestKey, "Bearer " + webTestKey}}, 401},
		{"different_case_duplicate", http.Header{"Authorization": {"Bearer " + webTestKey}, "authorization": {"Bearer incorrect"}}, 401},
		{"comma_joined", http.Header{"Authorization": {"Bearer " + webTestKey + ",Bearer incorrect"}}, 401},
		{"non_ascii_separator", http.Header{"Authorization": {"Bearer\u00a0" + webTestKey}}, 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/api/session", nil)
			r.Header = tc.headers.Clone()
			r.Header.Set("Origin", "https://unrelated.invalid")
			r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "ambient-stale-session"})
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("status %d, want %d", w.Code, tc.want)
			}
			if tc.want == 200 && !strings.Contains(w.Body.String(), `"expires_at":0`) {
				t.Fatal("bearer session response used cookie expiry")
			}
		})
	}
	// An ambient cookie must neither force Redis availability nor turn explicit
	// bearer logout into deletion of an unrelated browser session.
	r := httptest.NewRequest(http.MethodDelete, "/api/session", nil)
	r.Header.Set("Authorization", "Bearer "+webTestKey)
	r.Header.Set("Origin", "https://unrelated.invalid")
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "ambient-stale-session"})
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 200 || len(w.Result().Cookies()) != 1 || w.Result().Cookies()[0].MaxAge >= 0 {
		t.Fatal("bearer logout failed to clear browser cookie")
	}
	settings, err := p.Settings.Latest()
	if err != nil {
		t.Fatal(err)
	}
	settings.AdminAPI = false
	if err := p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
	if w := webTestRequest(handler, http.MethodGet, "/api/settings", "", "Bearer "+webTestKey); w.Code != 200 {
		t.Fatal("gateway admin_api switch disabled management")
	}
}

type adminAuthRedisHook struct {
	expires       int64
	gets, deletes int
}

func (h *adminAuthRedisHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h *adminAuthRedisHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (h *adminAuthRedisHook) ProcessHook(_ redis.ProcessHook) redis.ProcessHook {
	return func(_ context.Context, cmd redis.Cmder) error {
		switch c := cmd.(type) {
		case *redis.StringCmd:
			h.gets++
			raw, _ := json.Marshal(adminSession{ExpiresAt: h.expires})
			c.SetVal(string(raw))
		case *redis.IntCmd:
			h.deletes++
			c.SetVal(1)
		default:
			return errors.New("unexpected Redis command")
		}
		return nil
	}
}

func adminAuthCookieFixture(t *testing.T) (*Proxy, *adminAuthRedisHook) {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	t.Cleanup(func() { _ = rdb.Close() })
	hook := &adminAuthRedisHook{expires: time.Now().Add(time.Hour).Unix()}
	rdb.AddHook(hook)
	return &Proxy{Store: Store{DB: &Database{Redis: rdb, Namespace: "synthetic:"}}}, hook
}

func TestAdminKeyCookieIdentityAndOrigin(t *testing.T) {
	p, hook := adminAuthCookieFixture(t)
	r := httptest.NewRequest(http.MethodGet, "https://admin.example/api/session", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "valid-session"})
	w := httptest.NewRecorder()
	if !p.authenticateAdmin(w, r) {
		t.Fatal("valid synthetic session rejected")
	}
	p.handleSessionGet(w, r)
	var session adminSession
	if json.Unmarshal(w.Body.Bytes(), &session) != nil || session.ExpiresAt != hook.expires || hook.gets != 1 {
		t.Fatal("session identity was re-read or lost")
	}
	r.Header.Set("Authorization", "Basic invalid")
	w = httptest.NewRecorder()
	if p.authenticateAdmin(w, r) || w.Code != 401 || hook.gets != 1 {
		t.Fatal("invalid explicit credentials fell back to a valid cookie")
	}
	for _, tc := range []struct {
		name   string
		origin http.Header
		want   int
	}{
		{"absent", nil, 200},
		{"same_host", http.Header{"Origin": {"https://ADMIN.example"}}, 200},
		{"cross_host", http.Header{"Origin": {"https://other.example"}}, 403},
		{"null", http.Header{"Origin": {"null"}}, 403},
		{"empty", http.Header{"Origin": {""}}, 403},
		{"no_values", http.Header{"Origin": nil}, 403},
		{"duplicate", http.Header{"Origin": {"https://admin.example", "https://admin.example"}}, 403},
		{"mixed_case_duplicate", http.Header{"Origin": {"https://admin.example"}, "origin": {"https://other.example"}}, 403},
		{"path", http.Header{"Origin": {"https://admin.example/"}}, 403},
		{"query", http.Header{"Origin": {"https://admin.example?"}}, 403},
		{"fragment", http.Header{"Origin": {"https://admin.example#"}}, 403},
		{"userinfo", http.Header{"Origin": {"https://someone@admin.example"}}, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodDelete, "https://admin.example/api/session", nil)
			if tc.origin != nil {
				r.Header = tc.origin.Clone()
			}
			r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "valid-session"})
			w := httptest.NewRecorder()
			if p.authenticateAdmin(w, r) {
				p.handleSessionLogout(w, r)
			}
			if w.Code != tc.want {
				t.Fatalf("status %d, want %d", w.Code, tc.want)
			}
		})
	}
	if hook.deletes != 2 {
		t.Fatal("logout deleted an unauthenticated session")
	}
	for _, cookies := range []string{sessionCookie + "=one; " + sessionCookie + "=two", sessionCookie + "="} {
		r := httptest.NewRequest(http.MethodGet, "/api/session", nil)
		r.Header.Set("Cookie", cookies)
		w := httptest.NewRecorder()
		if p.authenticateAdmin(w, r) || w.Code != 401 {
			t.Fatal("ambiguous or empty cookie accepted")
		}
	}
}

func adminAuthRotate(t *testing.T, handler http.Handler, endpoint, body, key string, status int) string {
	t.Helper()
	w := webTestRequest(handler, http.MethodPost, endpoint, body, "Bearer "+key)
	if w.Code != status {
		t.Fatalf("rotation status %d, want %d", w.Code, status)
	}
	var reply struct {
		AdminKey string `json:"admin_key"`
	}
	if json.Unmarshal(w.Body.Bytes(), &reply) != nil {
		t.Fatal("non-JSON rotation response")
	}
	if status == 200 && reply.AdminKey == "" {
		t.Fatal("successful rotation omitted key")
	}
	return reply.AdminKey
}

func TestAdminKeyFileRotationReplayScope(t *testing.T) {
	p, handler := webTestSetup(t)
	endpoint := "/api/settings/rotate-key"
	key := adminAuthRotate(t, handler, endpoint, `{"request_id":"first"}`, webTestKey, 200)
	if key == webTestKey {
		t.Fatal("rotation did not change key")
	}
	for _, bearer := range []string{webTestKey, key} {
		if replay := adminAuthRotate(t, handler, endpoint, `{"request_id":"first"}`, bearer, 200); replay != key {
			t.Fatal("replay generated a second key")
		}
	}
	persisted, err := os.ReadFile(p.KeyFile)
	if err != nil || strings.TrimSpace(string(persisted)) != key {
		t.Fatal("active key was not persisted")
	}
	for _, body := range []string{
		`{"request_id":"other"}`, `{"request_id":"first","request_id":"first"}`,
		`{"Request_ID":"first"}`, `{"request_id":"first"} {}`,
		`{"request_id":"first","regenerate":true}`, `{"request_id":"first","admin_api":false}`,
		`{"request_id":"first","unknown":null}`,
	} {
		adminAuthRotate(t, handler, endpoint, body, webTestKey, 401)
	}
	adminAuthRotate(t, handler, "/api/settings", `{"request_id":"first","regenerate":true}`, webTestKey, 401)
	adminAuthRotate(t, handler, "/api/settings", `{"request_id":"first","regenerate":true}`, key, 409)
	if w := webTestRequest(handler, http.MethodGet, "/api/settings", "", "Bearer "+webTestKey); w.Code != 401 {
		t.Fatal("old key retained general access")
	}
	// Restarted file-mode instances can recover a lost response from the sidecar.
	restarted := &Proxy{Store: p.Store, AdminKey: key, KeyFile: p.KeyFile}
	r := httptest.NewRequest(http.MethodPost, endpoint, strings.NewReader(`{"request_id":"first"}`))
	r.Header.Set("Authorization", "Bearer "+webTestKey)
	if !restarted.authenticateAdmin(httptest.NewRecorder(), r) {
		t.Fatal("restart lost latest rotation replay")
	}
	if got, err := restarted.rotateAdminKey(r.Context(), "first"); err != nil || got != key {
		t.Fatal("restart replay changed key", err)
	}
	key2 := adminAuthRotate(t, handler, "/api/settings", `{"request_id":"second","regenerate_key":true}`, key, 200)
	if replay := adminAuthRotate(t, handler, "/api/settings", `{"request_id":"second","regenerate_key":true}`, key, 200); replay != key2 {
		t.Fatal("legacy replay changed key")
	}
	adminAuthRotate(t, handler, endpoint, `{"request_id":"second"}`, key, 401)
	adminAuthRotate(t, handler, endpoint, `{"request_id":"first"}`, webTestKey, 401)
	if p.adminKey() != key2 {
		t.Fatal("rejected replays changed active key")
	}
}

func adminAuthContext(key string) context.Context {
	return context.WithValue(context.Background(), adminAuthenticationContextKey{}, adminAuthentication{BearerHash: adminKeyHash(key), Endpoint: "/api/settings/rotate-key"})
}

func testAdminKeyRotationRevalidation(t *testing.T, p *Proxy, original string) {
	t.Helper()
	stale := adminAuthContext(original)
	key1, err := p.rotateAdminKey(stale, "rotation-one")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.rotateAdminKey(stale, "rotation-racing"); !errors.Is(err, errConflict) {
		t.Fatal("revoked authenticated request rotated again", err)
	}
	if replay, err := p.rotateAdminKey(stale, "rotation-one"); err != nil || replay != key1 {
		t.Fatal("immediate prior key cannot recover response", err)
	}
	key2, err := p.rotateAdminKey(adminAuthContext(key1), "rotation-two")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.rotateAdminKey(stale, "rotation-two"); !errors.Is(err, errConflict) {
		t.Fatal("twice-revoked caller recovered a later rotation", err)
	}
	if current, err := p.currentAdminKey(context.Background()); err != nil || current != key2 {
		t.Fatal("rejected race changed current key", err)
	}
}

func TestAdminKeyFileRotationRevalidatesUnderLock(t *testing.T) {
	p, _ := webTestSetup(t)
	testAdminKeyRotationRevalidation(t, p, webTestKey)
}

func TestAdminKeyRotationReplayTimeAndInvalidConfiguration(t *testing.T) {
	p, _ := webTestSetup(t)
	key, err := p.rotateAdminKey(context.Background(), "bounded")
	if err != nil {
		t.Fatal(err)
	}
	for _, created := range []int64{time.Now().Add(time.Hour).Unix(), time.Now().Add(-25 * time.Hour).Unix()} {
		store := newFileStore[rotationResult](p.KeyFile + ".rotation.json")
		if err := store.Modify(func(v *rotationResult) error { v.CreatedAt = created; return nil }); err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodPost, "/api/settings/rotate-key", strings.NewReader(`{"request_id":"bounded"}`))
		r.Header.Set("Authorization", "Bearer "+webTestKey)
		if p.rotationReplayAuthorized(r) {
			t.Fatal("future or expired rotation accepted")
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"request_id":"bounded"}` {
			t.Fatal("replay check consumed request body")
		}
		if _, err := p.rotateAdminKey(adminAuthContext(key), "bounded"); !errors.Is(err, errConflict) {
			t.Fatal("expired result returned", err)
		}
	}
	p.KeyFile = ""
	if _, err := p.rotateAdminKey(context.Background(), "no-file"); err == nil || p.adminKey() != key {
		t.Fatal("missing key file did not fail safely")
	}
	for _, id := range []string{"", "white space", "newline\n", "nonbreaking\u00a0space", strings.Repeat("x", 129)} {
		if _, err := p.rotateAdminKey(context.Background(), id); !errors.Is(err, errInvalid) {
			t.Fatal("invalid request ID accepted")
		}
	}
}

func TestAdminKeyPGRevalidationAndCookiePrecedence(t *testing.T) {
	p, _ := adminPGTestProxy(t)
	// A deterministic Redis hook supplies a genuinely valid session without a
	// second external test service; PostgreSQL remains the authoritative key store.
	cookieProxy, hook := adminAuthCookieFixture(t)
	p.Store.DB.Redis = cookieProxy.Store.DB.Redis
	r := httptest.NewRequest(http.MethodGet, "/api/session", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "valid-session"})
	if !p.authenticateAdmin(httptest.NewRecorder(), r) {
		t.Fatal("valid cookie rejected")
	}
	r.Header.Set("Authorization", "Bearer incorrect")
	w := httptest.NewRecorder()
	if p.authenticateAdmin(w, r) || w.Code != 401 || hook.gets != 1 {
		t.Fatal("wrong bearer fell back to valid cookie")
	}
	testAdminKeyRotationRevalidation(t, p, webTestKey)
	// A second instance's stale in-memory copy must not authorize a revoked key.
	other := &Proxy{Store: p.Store, AdminKey: webTestKey}
	r = httptest.NewRequest(http.MethodGet, "/api/session", nil)
	r.Header.Set("Authorization", "Bearer "+webTestKey)
	w = httptest.NewRecorder()
	if other.authenticateAdmin(w, r) || w.Code != 401 {
		t.Fatal("stale instance accepted revoked key")
	}
}
