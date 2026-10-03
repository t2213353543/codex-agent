package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestGatewayRuntimeAuditConfigurationRejectsInvalidOwners(t *testing.T) {
	empty := func(string) string { return "" }
	for _, tc := range []struct {
		name   string
		p      *Proxy
		getenv func(string) string
	}{
		{"nil_proxy", nil, empty},
		{"nil_environment", &Proxy{}, nil},
		{"already_configured", &Proxy{NativeHTTP: &gatewayNativePool{}}, empty},
		{"orphan_websocket_mode", &Proxy{NativeWebSocket: true}, empty},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := configureGatewayNativeHTTP(tc.p, tc.getenv); err == nil {
				t.Fatal("invalid configuration was accepted")
			}
		})
	}
}

func TestGatewayRuntimeAuditNativeTimeoutSnapshotsReachHelper(t *testing.T) {
	pool := nativeHTTPFixturePool(t, nil)
	base := newHTTPClient()
	base.Transport.(*http.Transport).ResponseHeaderTimeout = 11 * time.Second
	p := &Proxy{Client: base, NativeHTTP: pool}
	custom, disabled := 17*time.Second, time.Duration(0)
	for _, tc := range []struct {
		name     string
		mode     gatewayClientMode
		override *time.Duration
		want     time.Duration
	}{
		{"base_http", gatewayClientHTTP, nil, 11 * time.Second},
		{"custom_http", gatewayClientHTTP, &custom, custom},
		{"disabled_http", gatewayClientHTTP, &disabled, 0},
		{"default_compact", gatewayClientCompact, nil, upstreamCompactTimeout},
		{"custom_compact", gatewayClientCompact, &custom, custom},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := p.gatewayNativeClientForConfigured(nil, tc.mode, tc.override)
			res, err := client.Get("http://runtime-audit.invalid/echo")
			if err != nil {
				t.Fatal(err)
			}
			_, readErr := io.Copy(io.Discard, res.Body)
			closeErr := res.Body.Close()
			if readErr != nil || closeErr != nil {
				t.Fatalf("body: %v / %v", readErr, closeErr)
			}
			if got := res.Header.Get("X-Native-Timeout"); got != strconv.FormatInt(tc.want.Milliseconds(), 10) {
				t.Fatalf("helper loaded timeout %s, want %s", got, tc.want)
			}
			if tc.mode == gatewayClientCompact && client.Timeout != tc.want {
				t.Fatal("compact total timeout differs from configured header deadline")
			}
		})
	}
	if base.Transport.(*http.Transport).ResponseHeaderTimeout != 11*time.Second || base.Timeout != 10*time.Minute {
		t.Fatal("native request variant mutated shared client")
	}
}

func TestGatewayRuntimeAuditPersistedSettingsValidation(t *testing.T) {
	for _, tc := range []struct{ name, document string }{
		{"negative_version", `{"version":-1}`},
		{"negative_idle", `{"upstream_stream_idle_timeout_seconds":-1}`},
		{"zero_header", `{"upstream_response_header_timeout_seconds":0}`},
		{"overflow_header", `{"upstream_response_header_timeout_seconds":9223372036854775807}`},
		{"overflow_compact", `{"upstream_compact_timeout_seconds":9223372036854775807}`},
		{"overflow_idle", `{"upstream_stream_idle_timeout_seconds":9223372036854775807}`},
		{"invalid_ttl", `{"sticky_session_ttl_seconds":2592001}`},
		{"invalid_mode", `{"builtin_prompt_mode":"replace"}`},
		{"invalid_fingerprint", `{"codex_fingerprint_profile":"unknown"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.json")
			if err := os.WriteFile(path, []byte(tc.document), 0600); err != nil {
				t.Fatal(err)
			}
			store := NewSettingsStore(path)
			if _, err := store.Get(); !errors.Is(err, errInvalid) {
				t.Fatalf("invalid persisted runtime settings accepted: %v", err)
			}
			if store.loaded {
				t.Fatal("invalid settings cached")
			}
			// A corrected durable document must be recoverable without restart.
			if err := os.WriteFile(path, []byte(`{"version":3,"admin_api":true}`), 0600); err != nil {
				t.Fatal(err)
			}
			got, err := store.Get()
			if err != nil || got.Version != 3 || got.UpstreamStreamIdleTimeoutSeconds != 300 {
				t.Fatalf("legacy defaults or recovery failed: %+v, %v", got, err)
			}
		})
	}
}

func TestGatewayRuntimeAuditSettingsVersionsDoNotOverflow(t *testing.T) {
	for _, version := range []int64{-1, math.MaxInt64} {
		t.Run(strconv.FormatInt(version, 10), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.json")
			state := defaultSettings().state()
			state.Version = version
			raw, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			store := NewSettingsStore(path)
			value := defaultSettings()
			value.Version = 0 // Compatibility callers still cannot overflow storage.
			if err := store.Set(value); !errors.Is(err, errInvalid) {
				t.Fatalf("invalid stored version incremented: %v", err)
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != string(raw) || store.loaded {
				t.Fatal("failed update mutated durable state or cache")
			}
		})
	}
	store := NewSettingsStore(filepath.Join(t.TempDir(), "settings.json"))
	value := defaultSettings()
	value.Version = -1
	if err := store.Set(value); !errors.Is(err, errInvalid) {
		t.Fatalf("negative requested version accepted: %v", err)
	}
}

func TestGatewayRuntimeAuditStartupFailureClosesListener(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := serveManaged(context.Background(), &Proxy{}, listener, t.TempDir()); err == nil {
		t.Fatal("missing storage was accepted")
	}
	_ = listener.(*net.TCPListener).SetDeadline(time.Now().Add(100 * time.Millisecond))
	if conn, err := listener.Accept(); !errors.Is(err, net.ErrClosed) {
		if conn != nil {
			_ = conn.Close()
		}
		t.Fatalf("failed startup retained its bound listener: %v", err)
	}
}

type runtimeAuditHeaderWriter struct {
	header   http.Header
	statuses []int
	flushes  int
	err      error
}

func (w *runtimeAuditHeaderWriter) Header() http.Header         { return w.header }
func (w *runtimeAuditHeaderWriter) WriteHeader(code int)        { w.statuses = append(w.statuses, code) }
func (w *runtimeAuditHeaderWriter) Write(p []byte) (int, error) { return len(p), nil }
func (w *runtimeAuditHeaderWriter) FlushError() error           { w.flushes++; return w.err }

func TestGatewayRuntimeAuditResponseWriterStatusBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name  string
		codes []int
		want  int
	}{
		{"early_hints_then_success", []int{103, 100, 201}, 201},
		{"switching_protocols", []int{101}, 101},
		{"first_final_wins", []int{204, 500}, 204},
		{"switch_is_final", []int{101, 502}, 101},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink := &runtimeAuditHeaderWriter{header: make(http.Header)}
			record := &RequestRecord{}
			w := &gatewayResponseWriter{ResponseWriter: sink, record: record}
			for _, code := range tc.codes {
				w.WriteHeader(code)
			}
			if !w.wroteHeader || record.HTTPCode != tc.want {
				t.Fatalf("recorded status=%d, want %d", record.HTTPCode, tc.want)
			}
		})
	}
	w := &gatewayResponseWriter{ResponseWriter: httptest.NewRecorder()}
	if _, err := w.Write([]byte("optional record")); err != nil {
		t.Fatal(err)
	}
}

func TestGatewayRuntimeAuditResponseWriterFlushPropagatesFailure(t *testing.T) {
	want := errors.New("synthetic flush failure")
	sink := &runtimeAuditHeaderWriter{header: make(http.Header), err: want}
	record := &RequestRecord{}
	w := &gatewayResponseWriter{ResponseWriter: sink, record: record}
	w.WriteHeader(http.StatusEarlyHints)
	if err := http.NewResponseController(w).Flush(); !errors.Is(err, want) {
		t.Fatalf("lost flush failure: %v", err)
	}
	if record.HTTPCode != 200 || sink.flushes != 1 || len(sink.statuses) != 2 || sink.statuses[1] != 200 {
		t.Fatal("flush after 1xx did not commit 200 exactly once")
	}
}

func TestGatewayRuntimeAuditResponseWriterInvalidStatusIsNotRecorded(t *testing.T) {
	record := &RequestRecord{}
	w := &gatewayResponseWriter{ResponseWriter: httptest.NewRecorder(), record: record}
	defer func() {
		if recover() == nil {
			t.Error("invalid status did not panic")
		}
		if w.wroteHeader || record.HTTPCode != 0 {
			t.Error("unsent invalid status was recorded")
		}
	}()
	w.WriteHeader(1000)
}
