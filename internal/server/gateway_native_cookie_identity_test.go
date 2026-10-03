package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type cookieIdentityConnectionKey struct{}

type cookieIdentityObservation struct {
	path, authorization, account string
	cookies                      map[string]string
	connection                   int64
	protocol                     int
	tlsResumed                   bool
}

type cookieIdentityFixture struct {
	proxyURL string
	mu       sync.Mutex
	seen     []cookieIdentityObservation
}

// This is the real-CA/CONNECT boundary used by the existing infrastructure
// cookie test, extended with auth and connection observations. Only loopback is
// dialed: synthetic chatgpt.com is verified using this fixture's temporary CA.
func newCookieIdentityFixture(t *testing.T) *cookieIdentityFixture {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Synthetic cookie identity root"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	root, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "chatgpt.com"}, DNSNames: []string{"chatgpt.com"}, NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	caPath := filepath.Join(t.TempDir(), "root.pem")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root}), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_CA_CERTIFICATE", caPath)
	f := new(cookieIdentityFixture)
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed := cookieIdentityObservation{path: r.URL.Path, authorization: r.Header.Get("Authorization"), account: r.Header.Get("ChatGPT-Account-Id"), cookies: make(map[string]string), protocol: r.ProtoMajor}
		observed.connection, _ = r.Context().Value(cookieIdentityConnectionKey{}).(int64)
		if r.TLS != nil {
			observed.tlsResumed = r.TLS.DidResume
		}
		for _, cookie := range r.Cookies() {
			observed.cookies[cookie.Name] = cookie.Value
		}
		f.mu.Lock()
		f.seen = append(f.seen, observed)
		f.mu.Unlock()
		if marker := r.URL.Query().Get("set"); marker != "" {
			w.Header().Add("Set-Cookie", "__oailb="+marker+"; Path=/backend-api; Secure; Max-Age=3600")
			w.Header().Add("Set-Cookie", "__cf_bm="+marker+"; Path=/backend-api; Secure; Max-Age=3600")
			w.Header().Add("Set-Cookie", "session=never-store; Path=/; Secure")
		}
		if r.URL.Path == "/backend-api/responses" {
			// A WS handshake has a Set-Cookie response too. This must not mutate
			// the HTTP jar merely because HTTP and WS use the same process.
			w.Header().Add("Set-Cookie", "__oailb=ws-only; Path=/backend-api; Secure; Max-Age=3600")
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				t.Error("synthetic cookie WS handshake failed:", err)
				return
			}
			defer conn.CloseNow()
			if err := conn.Write(r.Context(), websocket.MessageText, []byte("synthetic-cookie-ws")); err != nil {
				return
			}
			_, _, _ = conn.Read(r.Context())
			return
		}
		if r.URL.Path == "/backend-api/oauth/token" {
			_, _ = io.Copy(io.Discard, r.Body)
			_, _ = io.WriteString(w, gatewayKnownOwnerRefreshFixture(`{"access_token":"synthetic-refreshed-at","refresh_token":"synthetic-refreshed-rt"}`, "workspace-a", "user-a"))
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	var nextConnection atomic.Int64
	upstream.Config.ConnContext = func(ctx context.Context, _ net.Conn) context.Context {
		return context.WithValue(ctx, cookieIdentityConnectionKey{}, nextConnection.Add(1))
	}
	upstream.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der, root}, PrivateKey: key}}, MinVersion: tls.VersionTLS12}
	upstream.EnableHTTP2 = true
	upstream.StartTLS()
	t.Cleanup(upstream.Close)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Host != "chatgpt.com:443" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		remote, err := (&net.Dialer{}).DialContext(r.Context(), "tcp", upstream.Listener.Addr().String())
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer remote.Close()
		local, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer local.Close()
		_, _ = buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		_ = buffered.Flush()
		done := make(chan struct{})
		go func() { _, _ = io.Copy(remote, buffered); _ = remote.Close(); close(done) }()
		_, _ = io.Copy(local, remote)
		_ = local.Close()
		<-done
	}))
	t.Cleanup(proxy.Close)
	f.proxyURL = proxy.URL
	return f
}

func (f *cookieIdentityFixture) newProxy(t *testing.T) (*Proxy, ProxyEntry) {
	t.Helper()
	p := newTestProxy(t, 1)
	p.BaseURL = "https://chatgpt.com/backend-api"
	// Match the native WS key's timeout so cookie observations cannot be
	// explained by HTTP and WS accidentally running in separate helpers.
	p.Client.Transport = &http.Transport{ResponseHeaderTimeout: 90 * time.Second}
	realGatewayNativePool(t, p)
	p.NativeWebSocket = true
	return p, poolProxyEntry(t, p, f.proxyURL)
}

func cookieIdentityHeaders(t *testing.T, workspace, user, generation string) http.Header {
	t.Helper()
	claims, err := json.Marshal(map[string]any{"https://api.openai.com/auth": map[string]string{"chatgpt_account_id": workspace, "chatgpt_user_id": user}, "jti": generation})
	if err != nil {
		t.Fatal(err)
	}
	// Synthetic, unsigned credentials only; the loopback fixture does not
	// authenticate. Token bytes differ on owner change and on AT refresh.
	token := "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(claims) + ".synthetic"
	return http.Header{"Authorization": {"Bearer " + token}, "Chatgpt-Account-Id": {workspace}}
}

func (f *cookieIdentityFixture) request(t *testing.T, p *Proxy, entry *ProxyEntry, path string, headers http.Header, owners ...Account) cookieIdentityObservation {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://chatgpt.com/backend-api/"+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header = headers.Clone()
	// Deliberately obtain a fresh Go client for every turn. Only the helper
	// process, and for pooled lifecycle its HTTP client, can preserve cookies.
	var res *http.Response
	if len(owners) == 0 {
		res, err = p.clientFor(entry).Do(req)
	} else {
		res, err = p.doAccountRequest(p.clientFor(entry), req, owners[0], doHTTPAccountRequest, nil)
	}
	if err != nil {
		t.Fatal("synthetic identity cookie request failed:", err)
	}
	body, readErr := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if readErr != nil || res.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Fatal("synthetic identity cookie response failed")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	got := f.seen[len(f.seen)-1]
	if got.authorization != headers.Get("Authorization") || got.account != headers.Get("ChatGPT-Account-Id") {
		t.Fatal("transport changed synthetic authorization/account headers")
	}
	return got
}

func assertIdentityCookies(t *testing.T, got cookieIdentityObservation, want string) {
	t.Helper()
	if got.cookies["__oailb"] != want || got.cookies["__cf_bm"] != want {
		t.Fatalf("infrastructure cookie scope differs: path=%s route_match=%t cf_match=%t", got.path, got.cookies["__oailb"] == want, got.cookies["__cf_bm"] == want)
	}
	wantCount := 2
	if want == "" {
		wantCount = 0
	}
	if len(got.cookies) != wantCount {
		t.Fatal("unexpected cookie name persisted across synthetic requests")
	}
}

// No trusted Account context: even credentials with identical unverified JWT
// ownership claims are isolated when their bearer changes.
func TestGatewayNativeActualCookieCredentialIsolation(t *testing.T) {
	for _, lifecycle := range []string{"cli_request", "pooled"} {
		t.Run(lifecycle, func(t *testing.T) {
			t.Setenv("CODEX_NATIVE_HTTP_TEST_LIFECYCLE", lifecycle)
			f := newCookieIdentityFixture(t)
			p, entry := f.newProxy(t)
			a := cookieIdentityHeaders(t, "workspace-a", "user-a", "at-1")
			b := cookieIdentityHeaders(t, "workspace-b", "user-b", "at-1")
			refreshed := cookieIdentityHeaders(t, "workspace-a", "user-a", "at-2")
			teammate := cookieIdentityHeaders(t, "workspace-a", "user-c", "at-1")
			var connections []int64
			for _, step := range []struct {
				name, path, want string
				headers          http.Header
			}{
				{"first_A", "seed-a?set=route-a", "", a},
				{"same_A", "same-a", "route-a", a},
				{"B_after_A", "seed-b?set=route-b", "", b},
				{"A_after_B", "return-a", "route-a", a},
				{"A_new_unverified_AT", "refresh-a?set=route-refreshed", "", refreshed},
				{"same_new_unverified_AT", "repeat-refreshed-a", "route-refreshed", refreshed},
				{"same_workspace_other_user", "seed-teammate?set=route-c", "", teammate},
				{"original_user_after_teammate", "return-original-user", "route-a", a},
				{"anonymous_after_auth", "anonymous", "", make(http.Header)},
			} {
				got := f.request(t, p, &entry, step.path, step.headers)
				assertIdentityCookies(t, got, step.want)
				connections = append(connections, got.connection)
				t.Logf("boundary=%s cookie_isolation_correct=true connection=%d protocol=HTTP/%d tls_resumed=%t", step.name, got.connection, got.protocol, got.tlsResumed)
			}
			unique := make(map[int64]bool)
			for _, connection := range connections {
				unique[connection] = true
			}
			wantConnections := len(connections)
			if lifecycle == "pooled" {
				wantConnections = 5
			}
			if len(unique) != wantConnections {
				t.Fatalf("connection lifecycle differs: got %d want %d", len(unique), wantConnections)
			}
			p.NativeHTTP.mu.Lock()
			processes := len(p.NativeHTTP.entries)
			p.NativeHTTP.mu.Unlock()
			if processes != 5 {
				t.Fatalf("four credentials and anonymous require five helpers: got %d", processes)
			}
		})
	}
}

func TestGatewayNativeActualCookieSeparateProcessesControl(t *testing.T) {
	for _, lifecycle := range []string{"cli_request", "pooled"} {
		t.Run(lifecycle, func(t *testing.T) {
			t.Setenv("CODEX_NATIVE_HTTP_TEST_LIFECYCLE", lifecycle)
			f := newCookieIdentityFixture(t)
			pa, entryA := f.newProxy(t)
			pb, entryB := f.newProxy(t)
			a := cookieIdentityHeaders(t, "workspace-a", "user-a", "at-1")
			b := cookieIdentityHeaders(t, "workspace-b", "user-b", "at-1")
			assertIdentityCookies(t, f.request(t, pa, &entryA, "seed-a?set=route-a", a), "")
			assertIdentityCookies(t, f.request(t, pb, &entryB, "seed-b?set=route-b", b), "")
			assertIdentityCookies(t, f.request(t, pa, &entryA, "return-a", a), "route-a")
			assertIdentityCookies(t, f.request(t, pb, &entryB, "return-b", b), "route-b")
			t.Log("two helper processes preserve independent A/B infrastructure cookies")
		})
	}
}

func cookieIdentityAccount(headers http.Header, user string) Account {
	return Account{ID: "managed-" + user, Name: "name-" + user, AccountID: headers.Get("ChatGPT-Account-Id"), AccessToken: strings.TrimPrefix(headers.Get("Authorization"), "Bearer "), RefreshToken: "synthetic-refresh-" + user,
		Identity: &managedAccountIdentity{UserID: user, WorkspaceID: headers.Get("ChatGPT-Account-Id")}}
}

func logCookieIdentityResources(t *testing.T, p *Proxy, label string) {
	t.Helper()
	p.NativeHTTP.mu.Lock()
	var pids []int
	for _, process := range p.NativeHTTP.entries {
		if process.command != nil && process.command.Process != nil {
			pids = append(pids, process.command.Process.Pid)
		}
	}
	p.NativeHTTP.mu.Unlock()
	var rss, anonymous, threads, descriptors int64
	for _, pid := range pids {
		status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
		if err != nil {
			t.Log("native helper resource snapshot unavailable")
			return
		}
		for _, line := range strings.Split(string(status), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			value, _ := strconv.ParseInt(fields[1], 10, 64)
			switch fields[0] {
			case "VmRSS:":
				rss += value
			case "RssAnon:":
				anonymous += value
			case "Threads:":
				threads += value
			}
		}
		fds, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
		if err == nil {
			descriptors += int64(len(fds))
		}
	}
	t.Logf("resource_snapshot=%s helpers=%d summed_rss_kib=%d summed_anon_kib=%d threads=%d descriptors=%d", label, len(pids), rss, anonymous, threads, descriptors)
}

func TestGatewayNativeActualCookieTrustedOwnerContinuity(t *testing.T) {
	for _, lifecycle := range []string{"cli_request", "pooled"} {
		t.Run(lifecycle, func(t *testing.T) {
			t.Setenv("CODEX_NATIVE_HTTP_TEST_LIFECYCLE", lifecycle)
			f := newCookieIdentityFixture(t)
			p, entry := f.newProxy(t)
			headersA := cookieIdentityHeaders(t, "workspace-a", "user-a", "at-1")
			headersB := cookieIdentityHeaders(t, "workspace-b", "user-b", "at-1")
			headersTeam := cookieIdentityHeaders(t, "workspace-a", "user-c", "at-1")
			a := cookieIdentityAccount(headersA, "user-a")
			b := cookieIdentityAccount(headersB, "user-b")
			team := cookieIdentityAccount(headersTeam, "user-c")
			// Hold management record and workspace constant so this case can
			// only be isolated by the actual user ownership boundary.
			team.ID, team.Name = a.ID, a.Name
			assertIdentityCookies(t, f.request(t, p, &entry, "seed-a?set=route-a", headersA, a), "")
			assertIdentityCookies(t, f.request(t, p, &entry, "seed-b?set=route-b", headersB, b), "")
			logCookieIdentityResources(t, p, "two_owners")
			assertIdentityCookies(t, f.request(t, p, &entry, "return-a", headersA, a), "route-a")
			assertIdentityCookies(t, f.request(t, p, &entry, "seed-team?set=route-c", headersTeam, team), "")
			assertIdentityCookies(t, f.request(t, p, &entry, "original-user", headersA, a), "route-a")

			updated, err := refreshAccountWithSettings(t.Context(), p.clientFor(&entry), "https://chatgpt.com/backend-api/oauth/token", a, defaultSettings())
			if err != nil || updated.AccessToken == a.AccessToken || gatewayAccountRef(updated) != gatewayAccountRef(a) {
				t.Fatal("synthetic known-owner OAuth refresh failed")
			}
			f.mu.Lock()
			refresh := f.seen[len(f.seen)-1]
			f.mu.Unlock()
			assertIdentityCookies(t, refresh, "route-a")
			if refresh.authorization != "" || refresh.account != "" {
				t.Fatal("OAuth fixture unexpectedly sent account auth headers")
			}
			headersUpdated := headersA.Clone()
			headersUpdated.Set("Authorization", "Bearer "+updated.AccessToken)
			assertIdentityCookies(t, f.request(t, p, &entry, "after-refresh", headersUpdated, updated), "route-a")

			// Prepared WS is the production account-aware boundary. A fresh
			// bearer must still share the account's HTTP helper, not a new jar.
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			downstream := httptest.NewRequest(http.MethodGet, "/responses", nil)
			ws, _, err := p.dialGatewayWebSocketPrepared(ctx, downstream, updated, &entry, defaultSettings(), nil)
			if err != nil {
				cancel()
				t.Fatal("trusted owner WS failed:", err)
			}
			_, data, readErr := ws.Read(ctx)
			_ = ws.CloseNow()
			cancel()
			if readErr != nil || string(data) != "synthetic-cookie-ws" {
				t.Fatal("trusted owner WS failed to deliver fixture message")
			}
			f.mu.Lock()
			wsSeen := f.seen[len(f.seen)-1]
			f.mu.Unlock()
			assertIdentityCookies(t, wsSeen, "")
			assertIdentityCookies(t, f.request(t, p, &entry, "after-prepared-ws", headersUpdated, updated), "route-a")
			p.NativeHTTP.mu.Lock()
			processes := len(p.NativeHTTP.entries)
			for key := range p.NativeHTTP.entries {
				if key.proxyID != entry.ID || key.fingerprint != gatewayProxyFingerprint(entry) {
					t.Error("owner isolation changed the selected proxy")
				}
			}
			p.NativeHTTP.mu.Unlock()
			if processes != 3 {
				t.Fatalf("three trusted owners require three helpers despite refresh and WS: got %d", processes)
			}
			changed := updated
			changed.OwnerRevision = "replacement-owner"
			assertIdentityCookies(t, f.request(t, p, &entry, "owner-revision", headersUpdated, changed), "")
			logCookieIdentityResources(t, p, "four_owners")
			t.Log("trusted A/B/workspace-user isolation, OAuth continuity, HTTP/WS scope and owner revision verified")
		})
	}
}

func TestGatewayNativeActualCookieHTTPWSBoundary(t *testing.T) {
	for _, lifecycle := range []string{"cli_request", "pooled"} {
		t.Run(lifecycle, func(t *testing.T) {
			t.Setenv("CODEX_NATIVE_HTTP_TEST_LIFECYCLE", lifecycle)
			f := newCookieIdentityFixture(t)
			p, entry := f.newProxy(t)
			a := cookieIdentityHeaders(t, "workspace-a", "user-a", "at-1")
			b := cookieIdentityHeaders(t, "workspace-b", "user-b", "at-1")
			assertIdentityCookies(t, f.request(t, p, &entry, "seed-a?set=route-a", a), "")
			for index, headers := range []http.Header{a, b, a} {
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				ws, response, err := p.dialNativeGatewayWebSocket(ctx, headers, &entry)
				if err != nil {
					cancel()
					t.Fatal("synthetic native cookie WS dial failed:", err)
				}
				_, data, readErr := ws.Read(ctx)
				_ = ws.CloseNow()
				cancel()
				if readErr != nil || string(data) != "synthetic-cookie-ws" || response.StatusCode != http.StatusSwitchingProtocols {
					t.Fatal("synthetic native cookie WS message failed")
				}
				f.mu.Lock()
				got := f.seen[len(f.seen)-1]
				f.mu.Unlock()
				assertIdentityCookies(t, got, "")
				if got.account != headers.Get("ChatGPT-Account-Id") || got.authorization != headers.Get("Authorization") || got.protocol != 1 {
					t.Fatal("WS synthetic authorization/account/upgrade changed")
				}
				t.Logf("WS turn=%d cookie_empty=true protocol=HTTP/%d tls_resumed=%t", index, got.protocol, got.tlsResumed)
				// HTTP proves the cookie remained present, and WS Set-Cookie did
				// not replace it. The HTTP request uses the WS turn's identity.
				want := "route-a"
				if index == 1 {
					want = ""
				}
				assertIdentityCookies(t, f.request(t, p, &entry, fmt.Sprintf("after-ws-%d", index), headers), want)
			}
			p.NativeHTTP.mu.Lock()
			processes := len(p.NativeHTTP.entries)
			p.NativeHTTP.mu.Unlock()
			if processes != 2 {
				t.Fatalf("HTTP and WS must share one helper per credential: got %d", processes)
			}
		})
	}
}
