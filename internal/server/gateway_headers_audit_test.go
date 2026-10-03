package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func TestGatewayHeadersAuditCanonicalListsAndSingletons(t *testing.T) {
	source := http.Header{
		"User-Agent": {"codex_exec/0.155.0 (Linux 6.8; x86_64) unknown", "curl/8.0.0"},
		"user-agent": {"lowercase-conflict"}, "Originator": {"codex_exec", "other"},
		"originator": {"lowercase-conflict"}, "Version": {"0.155.0", "second-version"},
		"Openai-Beta": {"responses=experimental, canonical=v1", "future=v2"},
		"openai-beta": {"RESPONSES=EXPERIMENTAL, lower=v3"},
		"Tracestate":  {"first=one"}, "tracestate": {"second=two"},
		"If-None-Match": {`"first"`}, "if-none-match": {`"second"`},
		"X-Codex-Turn-State":  {"canonical-state", "conflicting-state"},
		"x-codex-turn-state":  {"lowercase-state"},
		"X-Oai-Attestation":   {"native-proof", "conflicting-proof"},
		"X-Codex-Future-List": {"first"}, "x-codex-future-list": {"second"},
	}
	original := source.Clone()
	for attempt := 0; attempt < 50; attempt++ {
		target := make(http.Header)
		copyRequestHeaders(target, source)
		for name, want := range map[string][]string{
			"User-Agent": {source.Get("User-Agent")}, "Originator": {"codex_exec"},
			"Version": {"0.155.0"}, "X-Codex-Turn-State": {"canonical-state"},
			"X-Oai-Attestation":   {"native-proof"},
			"X-Codex-Future-List": {"first", "second"},
			"Openai-Beta":         {"canonical=v1", "future=v2", "lower=v3"},
			"Tracestate":          {"first=one", "second=two"}, "If-None-Match": {`"first"`, `"second"`},
		} {
			if got := target.Values(name); !reflect.DeepEqual(got, want) {
				t.Fatalf("attempt %d: %s = %q, want %q", attempt, name, got, want)
			}
		}
		for name := range target {
			if name != http.CanonicalHeaderKey(name) {
				t.Fatalf("noncanonical alias survived: %s", name)
			}
		}
		target["Tracestate"][0] = "mutated=destination"
	}
	if !reflect.DeepEqual(source, original) {
		t.Fatal("header copying mutated source slices")
	}
}

func TestGatewayHeadersAuditConnectionNominationsOnWire(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, name := range []string{"Connection", "Traceparent", "X-Codex-Private-Hop", "X-Oai-Attestation", "Authorization", "Cookie"} {
			if got := r.Header.Get(name); got != "" {
				t.Errorf("hop or client credential %s reached upstream: %q", name, got)
			}
		}
		if got := r.Header.Get("X-Codex-Turn-State"); got != "retained" {
			t.Errorf("end-to-end state lost: %q", got)
		}
		w.Header().Set("Connection", "X-Codex-Private-Hop, X-Request-Id, Retry-After")
		w.Header().Set("X-Codex-Private-Hop", "private")
		w.Header().Set("X-Request-Id", "hop-request")
		w.Header().Set("Retry-After", "10")
		w.Header().Set("X-Codex-Turn-State", "next")
		w.Header().Set("X-Other-Meter-Primary-Used-Percent", "42")
		w.Header().Set("Set-Cookie", "private=true")
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()
	source := http.Header{
		"User-Agent": {"codex_exec/0.155.0"}, "Originator": {"codex_exec"},
		"connection":  {"Traceparent, x-codex-private-hop", "X-Oai-Attestation"},
		"Traceparent": {"private-trace"}, "X-Codex-Private-Hop": {"private"},
		"X-Oai-Attestation": {"hop-proof"}, "X-Codex-Turn-State": {"retained"},
		"Authorization": {"Bearer client-secret"}, "Cookie": {"client-secret=true"},
	}
	request, err := http.NewRequest(http.MethodGet, upstream.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	copyRequestHeaders(request.Header, source)
	response, err := upstream.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	target := make(http.Header)
	copyResponseHeaders(target, response.Header)
	for _, name := range []string{"Connection", "X-Codex-Private-Hop", "X-Request-Id", "Retry-After", "Set-Cookie", "Content-Length"} {
		if target.Get(name) != "" {
			t.Errorf("hop/private response header %s reached downstream", name)
		}
	}
	if target.Get("X-Codex-Turn-State") != "next" || target.Get("X-Other-Meter-Primary-Used-Percent") != "42" {
		t.Fatalf("protocol response metadata lost: %v", target)
	}
}

func TestGatewayHeadersAuditIdentityAndAttestationPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, userAgent, originator string
		unchanged                   bool
	}{
		{"default", "codex_cli_rs/0.155.0 (Linux 6.8; x86_64) xterm", "codex_cli_rs", true},
		{"custom_process", "custom_cli/0.155.0", "custom_cli", true},
		{"custom_thread", "custom_cli/0.155.0-alpha.3 (Linux 6.8; x86_64) xterm", "other_thread", true},
		{"browser_mentions_codex", "Mozilla/5.0 codex integration", "browser", false},
		{"curl", "curl/8.0.0", "browser", false},
		{"missing_identity", "", "", false},
		{"missing_originator", "codex_exec/0.155.0", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := http.Header{"User-Agent": {tc.userAgent}, "Originator": {tc.originator}, "X-Oai-Attestation": {"opaque-proof"}}
			target := make(http.Header)
			copyRequestHeaders(target, source)
			if tc.unchanged {
				if target.Get("User-Agent") != tc.userAgent || target.Get("Originator") != tc.originator || target.Get("X-Oai-Attestation") != "opaque-proof" || target.Get("Version") != "" {
					t.Fatalf("native identity, optional version, or proof changed: %v", target)
				}
			} else if target.Get("X-Oai-Attestation") != "" || target.Get("Originator") != clientOriginator {
				t.Fatalf("rewritten identity retained caller attestation: %v", target)
			}
		})
	}
}

func TestGatewayHeadersAuditResponseSingletonsAndErrorContentType(t *testing.T) {
	source := http.Header{
		"Content-Type": {"text/event-stream", "application/octet-stream"}, "content-type": {"text/plain"},
		"Retry-After": {"20", "30"}, "retry-after": {"40"},
		"X-Codex-Turn-State": {"next", "conflict"}, "x-codex-turn-state": {"lower"},
	}
	target := http.Header{"content-type": {"stale"}}
	copyResponseHeaders(target, source)
	if !reflect.DeepEqual(target.Values("Content-Type"), []string{"text/event-stream"}) ||
		!reflect.DeepEqual(target.Values("Retry-After"), []string{"20"}) ||
		!reflect.DeepEqual(target.Values("X-Codex-Turn-State"), []string{"next"}) {
		t.Fatalf("ambiguous response singleton values: %v", target)
	}
	if _, exists := target["content-type"]; exists {
		t.Fatal("destination alias retained stale content type")
	}
	w := httptest.NewRecorder()
	copyResponseHeaders(w.Header(), source)
	apiError(w, http.StatusBadGateway, "upstream failed")
	var payload map[string]json.RawMessage
	if w.Code != http.StatusBadGateway || w.Header().Get("Content-Type") != "application/json" || json.Unmarshal(w.Body.Bytes(), &payload) != nil || payload["error"] == nil {
		t.Fatalf("API error used stale response framing: %d %v %s", w.Code, w.Header(), w.Body.String())
	}
}

func TestGatewayHeadersAuditClientIP(t *testing.T) {
	for remote, want := range map[string]string{
		"127.0.0.1:1234": "127.0.0.1", "127.0.0.1": "127.0.0.1",
		"[2001:db8::1]:1234": "2001:db8::1", "2001:db8::1": "2001:db8::1",
		"[::1]": "::1", "[fe80::1%zone]:90": "fe80::1%zone", "fe80::1%zone": "fe80::1%zone",
		"": "",
	} {
		request := &http.Request{RemoteAddr: remote, Header: http.Header{"X-Forwarded-For": {"spoofed"}}}
		if got := clientIP(request); got != want {
			t.Errorf("clientIP(%q) = %q, want %q", remote, got, want)
		}
	}
}

func TestGatewayHeadersAuditUpstreamURL(t *testing.T) {
	for _, tc := range []struct{ base, path, want string }{
		{"https://example.test/backend-api/codex///", "/responses", "https://example.test/backend-api/codex/responses?client_version=0.155.0"},
		{"https://example.test/prefix%2Ftenant", "models", "https://example.test/prefix%2Ftenant/models?client_version=0.155.0"},
		{"http://[::1]:8080", "/models", "http://[::1]:8080/models?client_version=0.155.0"},
	} {
		got, err := gatewayUpstreamURL(tc.base, tc.path, url.Values{"client_version": {"0.155.0"}})
		if err != nil || got != tc.want {
			t.Errorf("gatewayUpstreamURL(%q) = %q, %v; want %q", tc.base, got, err, tc.want)
		}
	}
	for _, base := range []string{
		"https://example.test/prefix?secret=value", "https://example.test/prefix?", "https://example.test/prefix#fragment",
		"https://example.test/prefix#", "https://user:secret@example.test/prefix", "ftp://example.test", "/relative", "https:///missing-host",
	} {
		if got, err := gatewayUpstreamURL(base, "/responses", nil); err == nil || got != "" || strings.Contains(err.Error(), "secret") {
			t.Errorf("invalid base URL accepted or leaked: %q => %q, %v", base, got, err)
		}
	}
}

func TestGatewayHeadersAuditInvalidBaseURLNeverDispatches(t *testing.T) {
	for _, route := range []string{"/models", "/responses"} {
		for _, suffix := range []string{"?api_key=private", "?", "#private"} {
			t.Run(route+suffix, func(t *testing.T) {
				var requests atomic.Int32
				p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(http.StatusNoContent) })
				p.BaseURL += suffix
				method, body := http.MethodGet, ""
				if route == "/responses" {
					method, body = http.MethodPost, `{"model":"test-model","input":"hello"}`
				}
				w := httptest.NewRecorder()
				p.ServeHTTP(w, proxyRequest(method, route, body))
				if w.Code != http.StatusInternalServerError || requests.Load() != 0 || strings.Contains(w.Body.String(), "private") {
					t.Fatalf("invalid base dispatched/leaked: status %d requests %d body %s", w.Code, requests.Load(), w.Body.String())
				}
			})
		}
	}
}

func TestGatewayHeadersAuditEncodedPrefixOnWire(t *testing.T) {
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.EscapedPath(); got != "/prefix%2Ftenant/models" {
			t.Errorf("escaped endpoint prefix changed: %s", got)
		}
		if r.URL.Query().Get("client_version") != "custom+version" {
			t.Errorf("client_version not escaped correctly: %q", r.URL.RawQuery)
		}
		_, _ = io.WriteString(w, `{"models":[]}`)
	})
	p.BaseURL += "/prefix%2Ftenant/"
	w := httptest.NewRecorder()
	p.ServeHTTP(w, proxyRequest(http.MethodGet, "/models?client_version=custom%2Bversion", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("encoded prefix request failed: %d %s", w.Code, w.Body.String())
	}
}

func TestGatewayHeadersAuditErrorPathRedaction(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{`open C:\deploy\accounts.json: failed`, `open [path] failed`},
		{`open "C:\Program Files\Codex\accounts.json": failed`, `open "[path]": failed`},
		{`open '\\server\private share\accounts.json': failed`, `open '[path]': failed`},
		{`open /srv/codex/accounts.json: failed`, `open [path] failed`},
		{"open /srv/codex/accounts.json\r\nretry", "open [path]\r\nretry"},
		{`open '/srv/private files/accounts.json': failed`, `open '[path]': failed`},
		{`request /models failed`, `request /models failed`},
	} {
		if got := hidePaths(tc.input); got != tc.want {
			t.Errorf("hidePaths(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestGatewayHeadersAuditMissingTransportResponse(t *testing.T) {
	p := newTestProxy(t, 1)
	account, err := p.Store.Get("test")
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://fixture.invalid/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	var attempts, retries int
	response, err := p.doAccountRequest(p.Client, request, account, func(*http.Client, *http.Request) (*http.Response, error) {
		attempts++
		return nil, nil
	}, func(Account) error { retries++; return nil })
	if err == nil || response != nil || attempts != 1 || retries != 0 {
		t.Fatalf("missing transport response panic/replayed: response=%v err=%v attempts=%d retries=%d", response, err, attempts, retries)
	}
}
