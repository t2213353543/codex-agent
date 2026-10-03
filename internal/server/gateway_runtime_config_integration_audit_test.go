package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestGatewayRuntimeAuditActualCAConfiguration(t *testing.T) {
	bin := os.Getenv("CODEX_NATIVE_HTTP_TEST_BIN")
	if bin == "" || runtime.GOOS != "linux" {
		t.Skip("set CODEX_NATIVE_HTTP_TEST_BIN on Linux for real helper TLS configuration")
	}
	raw, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	// The fixture produces valid X.509 PEM locally. No upstream request is made.
	server := httptest.NewTLSServer(http.NotFoundHandler())
	cert := server.Certificate().Raw
	server.Close()
	dir := nativeStartupTempDirectory(t)
	valid, malformed := filepath.Join(dir, "valid.pem"), filepath.Join(dir, "malformed.pem")
	if err := os.WriteFile(valid, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(malformed, []byte("existing file with no certificate blocks\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, codexCA, sslCA string
		wantError            bool
	}{
		{"existing_malformed_primary", malformed, "", true},
		{"existing_malformed_fallback", "", malformed, true},
		{"bad_primary_does_not_fall_back", malformed, valid, true},
		{"valid_primary_ignores_bad_fallback", valid, malformed, false},
		{"empty_primary_uses_fallback", "", valid, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			private := nativeStartupTempDirectory(t)
			t.Setenv("TMPDIR", private)
			t.Setenv("CODEX_CA_CERTIFICATE", tc.codexCA)
			t.Setenv("SSL_CERT_FILE", tc.sslCA)
			t.Setenv("SSL_CERT_DIR", "")
			before := nativeStartupChildProcesses(t)
			p := &Proxy{}
			values := map[string]string{"CODEX_NATIVE_HTTP_HELPER": bin, "CODEX_NATIVE_HTTP_SHA256": hex.EncodeToString(digest[:]), "CODEX_NATIVE_WEBSOCKET": "1"}
			err := configureGatewayNativeHTTP(p, func(key string) string { return values[key] })
			if (err != nil) != tc.wantError {
				t.Fatalf("runtime CA load: got %v, want error %t", err, tc.wantError)
			}
			if tc.wantError {
				if p.NativeHTTP != nil || p.NativeWebSocket {
					t.Fatal("failed runtime TLS configuration published a pool")
				}
			} else {
				if p.NativeHTTP == nil || !p.NativeWebSocket {
					t.Fatal("valid HTTP/WS TLS configuration was not published")
				}
				if err := p.NativeHTTP.Close(); err != nil {
					t.Fatal(err)
				}
			}
			nativeStartupAssertClean(t, private, before)
		})
	}
}

func TestPGGatewayRuntimeAuditInvalidSettingsDoNotPoisonCache(t *testing.T) {
	accountStore := accountPGTestStore(t)
	store := NewPGSettingsStore(accountStore.DB)
	if err := store.Set(defaultSettings()); err != nil {
		t.Fatal(err)
	}
	before, err := store.Get()
	if err != nil {
		t.Fatal(err)
	}
	bad := -1
	if err := store.store.Modify(func(state *settingsState) error {
		state.UpstreamResponseHeaderTimeoutSeconds = &bad
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Latest bypasses a fresh cache; gateway Get must fail after its bounded TTL.
	if _, err := store.Latest(); !errors.Is(err, errInvalid) {
		t.Fatalf("Latest accepted invalid persisted timeout: %v", err)
	}
	store.mu.Lock()
	store.loadedAt = time.Now().Add(-settingsCacheTTL)
	store.mu.Unlock()
	if _, err := store.Get(); !errors.Is(err, errInvalid) {
		t.Fatalf("Get accepted invalid persisted timeout: %v", err)
	}
	if store.cached != before {
		t.Fatal("invalid refresh replaced last valid cached settings")
	}
	if err := store.Set(before); err != nil {
		t.Fatal(err)
	}
	after, err := store.Latest()
	if err != nil || after.Version != before.Version+1 || after.UpstreamResponseHeaderTimeoutSeconds != before.UpstreamResponseHeaderTimeoutSeconds {
		t.Fatalf("corrected settings not recoverable: %+v %v", after, err)
	}
}

func TestPGGatewayRuntimeAuditReadinessRejectsInvalidSettings(t *testing.T) {
	redisURL := os.Getenv("TEST_REDIS_URL")
	if redisURL == "" {
		t.Skip("TEST_REDIS_URL is required for managed readiness")
	}
	accounts := accountPGTestStore(t)
	if err := accounts.DB.ConfigureRedis(redisURL); err != nil {
		t.Fatal(err)
	}
	store := NewPGSettingsStore(accounts.DB)
	if err := store.Set(defaultSettings()); err != nil {
		t.Fatal(err)
	}
	before, err := store.Get()
	if err != nil {
		t.Fatal(err)
	}
	metrics, err := NewPGMetrics(accounts.DB)
	if err != nil {
		t.Fatal(err)
	}
	defer metrics.Close()
	p := &Proxy{Store: accounts, Settings: store, Metrics: metrics}
	handler := managementRuntimeHandler(p, http.NotFoundHandler())
	bad := -1
	if err := store.store.Modify(func(state *settingsState) error {
		state.UpstreamCompactTimeoutSeconds = &bad
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]int{"/health/ready": 503, "/health/live": 200} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != want {
			t.Fatalf("%s=%d, want %d", path, w.Code, want)
		}
	}
	if err := store.Set(before); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	if w.Code != 200 {
		t.Fatalf("corrected settings not ready: %d", w.Code)
	}
}
