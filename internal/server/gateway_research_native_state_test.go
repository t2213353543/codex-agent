package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// This barrier is after full gateway account selection and before native IPC.
// Existing owner/cookie tests replace an owner before starting the next request;
// this reproduces a completed authoritative replacement during cold startup.
func TestGatewayResearchNativeColdStartupOwnerRevocation(t *testing.T) {
	for _, change := range []string{"different_user_same_workspace", "owner_aba", "deleted", "disabled", "key_disabled", "key_groups", "account_groups", "same_owner_refresh", "unchanged"} {
		t.Run(change, func(t *testing.T) {
			p := newTestProxy(t, 2)
			pool, entered, release, starts := nativeStartupBarrierPool(t)
			p.NativeHTTP = pool
			p.BaseURL = "http://research-native.invalid"
			original, err := p.Store.Get("test")
			if err != nil {
				t.Fatal(err)
			}
			keyID := "admin"
			var key APIKey
			if change == "key_disabled" || change == "key_groups" || change == "account_groups" {
				key, original = gatewaySessionAuthorizationFixture(t, p)
				keyID = key.ID
			}
			const session = "native-research-cold-owner"
			request := proxyRequest(http.MethodPost, "/responses", `{"model":"fixture","stream":true,"input":"cold-start-owner-sentinel"}`)
			request.Header.Set("Session-Id", session)
			if keyID != "admin" {
				request.Header.Set("Authorization", "Bearer "+key.Key)
				if _, err := p.sessionAccount(t.Context(), keyID, session, gatewayAccountRef(original)); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
			defer cancel()
			request = request.WithContext(ctx)
			result := httptest.NewRecorder()
			done := make(chan struct{})
			go func() {
				defer close(done)
				p.ServeHTTP(result, request)
			}()
			nativeStartupWaitSignal(t, entered)
			// Reaching helper command creation proves selection and body/header
			// preparation have completed. No helper exists to receive IPC yet.
			if starts.Load() != 1 || p.inFlight(gatewayAdmissionKey(original)) != 1 {
				t.Fatal("fixture did not block one admitted request at native cold startup")
			}
			gatewaySessionAuthorizationAssertBinding(t, p, keyID, session, original)
			switch change {
			case "different_user_same_workspace", "owner_aba":
				if err := p.Store.UpdateAccount(original.ID, func(a *Account) error {
					a.Identity = &managedAccountIdentity{UserID: "replacement-user", WorkspaceID: a.AccountID}
					a.AccessToken = "replacement-user-token"
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				if change == "owner_aba" {
					if err := p.Store.UpdateAccount(original.ID, func(a *Account) error {
						a.Identity = &managedAccountIdentity{UserID: original.Identity.UserID, WorkspaceID: original.AccountID}
						a.AccessToken = original.AccessToken
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				}
				current, err := p.Store.Get(original.ID)
				if err != nil || current.ID != original.ID || current.AccountID != original.AccountID || current.OwnerRevision == original.OwnerRevision {
					t.Fatal("fixture did not preserve record/workspace while replacing ownership incarnation")
				}
			case "deleted":
				if err := p.Store.Delete(original.ID); err != nil {
					t.Fatal(err)
				}
			case "disabled":
				if err := p.Store.UpdateAccount(original.ID, func(a *Account) error { a.Enabled = false; return nil }); err != nil {
					t.Fatal(err)
				}
			case "account_groups":
				if err := p.Store.UpdateAccount(original.ID, func(a *Account) error { a.Groups = []string{"ungranted-group"}; return nil }); err != nil {
					t.Fatal(err)
				}
			case "key_disabled", "key_groups":
				if _, err := p.Keys.Update(key.ID, func(k *APIKey) error {
					if change == "key_disabled" {
						k.Enabled = false
					} else {
						k.Groups = []string{"ungranted-group"}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			case "same_owner_refresh":
				if err := p.Store.UpdateAccount(original.ID, func(a *Account) error {
					a.AccessToken, a.RefreshToken = "same-owner-new-at", "same-owner-new-rt"
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				current, err := p.Store.Get(original.ID)
				if err != nil || gatewayAccountRef(current) != gatewayAccountRef(original) {
					t.Fatal("same-owner refresh unexpectedly changed ownership incarnation")
				}
			}
			release()
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("controlled request did not finish after releasing helper startup")
			}
			allowed := change == "same_owner_refresh" || change == "unchanged"
			if allowed {
				if result.Code != http.StatusAccepted || !strings.Contains(result.Body.String(), "cold-start-owner-sentinel") {
					t.Errorf("current owner was rejected or request body changed: status=%d", result.Code)
				}
			} else if result.Code < 400 || strings.Contains(result.Body.String(), "cold-start-owner-sentinel") {
				t.Errorf("completed %s during cold helper startup still dispatched old-owner body: status=%d echoed=%t", change, result.Code, strings.Contains(result.Body.String(), "cold-start-owner-sentinel"))
			}
			records := p.Metrics.RecentForAccount(original.Name, 10)
			if len(records) != 1 {
				t.Fatal("completed/rejected request did not produce exactly one business record")
			}
			wantAttempts := 0
			if allowed {
				wantAttempts = 1
			}
			if len(records[0].Attempts) != wantAttempts {
				t.Errorf("pre-IPC rejection was incorrectly counted as an upstream send: attempts=%d want=%d", len(records[0].Attempts), wantAttempts)
			}
			if !allowed && strings.Contains(result.Body.String(), "after authentication recovery") {
				t.Error("initial dispatch rejection was misreported as OAuth recovery")
			}
			if p.inFlight(gatewayAdmissionKey(original)) != 0 {
				t.Fatal("completed/rejected request retained account admission")
			}
			nativeProxyDispatchAssertIdle(t, pool)
			gatewaySessionAuthorizationAssertBinding(t, p, keyID, session, original)
		})
	}
}

// Keep a native response open while a second request is cancelled and a fresh
// same-owner request is admitted. Only the cancelled IPC connection may close;
// its account scope/helper must remain usable by the independent requests.
func TestGatewayResearchNativeCancelAndReuseSameOwner(t *testing.T) {
	p := newTestProxy(t, 3)
	pool := nativeHTTPFixturePool(t, nil)
	p.NativeHTTP = pool
	account, err := p.Store.Get("test")
	if err != nil {
		t.Fatal(err)
	}
	request := func(ctx context.Context, path string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://research-native.invalid"+path, strings.NewReader("independent-request"))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+account.AccessToken)
		req.Header.Set("ChatGPT-Account-Id", account.AccountID)
		return p.doAccountRequest(p.clientFor(nil), req, account, doHTTPAccountRequest, nil)
	}
	first, err := request(t.Context(), "/hold")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Body.Close()
	firstPrefix := make([]byte, len("first"))
	if _, err := io.ReadFull(first.Body, firstPrefix); err != nil || string(firstPrefix) != "first" {
		t.Fatal("first response did not remain open after its own first chunk")
	}
	ctx, cancel := context.WithCancel(t.Context())
	second, err := request(ctx, "/hold")
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	defer second.Body.Close()
	if _, err := io.ReadFull(second.Body, firstPrefix); err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	_, _ = io.ReadAll(second.Body)
	_ = second.Body.Close()
	third, err := request(t.Context(), "/echo")
	if err != nil {
		t.Fatal("one request cancellation retired another active request's helper:", err)
	}
	defer third.Body.Close()
	got, err := io.ReadAll(third.Body)
	if err != nil || string(got) != "independent-request" || third.Header.Get("X-Native-Pid") != first.Header.Get("X-Native-Pid") {
		t.Fatal("same-owner independent request lost bytes or helper continuity after sibling cancellation")
	}
	_ = third.Body.Close()
	pool.mu.Lock()
	active := 0
	for _, child := range pool.entries {
		active += child.active
	}
	pool.mu.Unlock()
	if active != 1 {
		t.Fatalf("sibling cancellation/close disturbed first response ownership: active=%d", active)
	}
	_ = first.Body.Close()
	nativeProxyDispatchAssertIdle(t, pool)
}

// A synthetic protocol peer allows the Windows suite to prove actual WS IPC
// delivery and its absence. It makes no upstream connection or model request.
func TestGatewayResearchNativeHelperProcess(t *testing.T) {
	if os.Getenv("GATEWAY_RESEARCH_NATIVE_FIXTURE") != "1" {
		return
	}
	readJSON := func(reader io.Reader, value any) error {
		var size uint32
		if err := binary.Read(reader, binary.BigEndian, &size); err != nil {
			return err
		}
		if size > gatewayNativeHeadLimit {
			return errGatewayNativeProtocol
		}
		raw := make([]byte, size)
		if _, err := io.ReadFull(reader, raw); err != nil {
			return err
		}
		return json.Unmarshal(raw, value)
	}
	var config struct {
		Token     string `json:"ipc_token"`
		Lifecycle string `json:"http_client_lifecycle"`
	}
	if readJSON(os.Stdin, &config) != nil {
		os.Exit(2)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		os.Exit(2)
	}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"protocol": 1, "transport": "tcp", "listen": listener.Addr().String(),
		"max_body": gatewayNativeBodyLimit, "max_active": 0, "http_client_lifecycle": config.Lifecycle,
		"capabilities": []string{"request_zstd_v1", "websocket_message_v1"}})
	go func() { _, _ = io.Copy(io.Discard, os.Stdin); os.Exit(0) }()
	for {
		conn, err := listener.Accept()
		if err != nil {
			os.Exit(0)
		}
		go func() {
			defer conn.Close()
			var head struct {
				Token     string      `json:"ipc_token"`
				Operation string      `json:"operation"`
				Headers   [][2]string `json:"headers"`
			}
			if readJSON(conn, &head) != nil || head.Token != config.Token || head.Operation != "websocket_message_v1" {
				return
			}
			marker, err := os.OpenFile(os.Getenv("GATEWAY_RESEARCH_NATIVE_MARKER"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
			if err != nil {
				return
			}
			var authorizationHash string
			for _, header := range head.Headers {
				if strings.EqualFold(header[0], "Authorization") {
					decoded, _ := base64.StdEncoding.DecodeString(header[1])
					sum := sha256.Sum256(decoded)
					authorizationHash = hex.EncodeToString(sum[:])
				}
			}
			err = json.NewEncoder(marker).Encode(map[string]string{"event": "handshake-head-received", "authorization_sha256": authorizationHash})
			_ = marker.Close()
			if err != nil {
				return
			}
			if nativeWSTestPacket(conn, nativeWSHandshake, []byte(`{"status":101,"headers":[],"body_scope":"none","body_complete":null,"body_base64":"","tail_truncated":false}`)) != nil {
				return
			}
			_, _ = io.Copy(io.Discard, conn)
		}()
	}
}

func TestGatewayResearchNativeWSColdStartupRevocation(t *testing.T) {
	for _, change := range []string{"different_user_same_workspace", "deleted", "disabled", "key_disabled", "key_groups", "account_groups", "same_owner_refresh", "unchanged"} {
		t.Run(change, func(t *testing.T) {
			p := newTestProxy(t, 2)
			key, original := gatewaySessionAuthorizationFixture(t, p)
			p.BaseURL = "http://research-native-ws.invalid"
			marker := filepath.Join(t.TempDir(), "actual-ipc-received")
			entered, proceed := make(chan struct{}), make(chan struct{})
			var once sync.Once
			release := func() { once.Do(func() { close(proceed) }) }
			pool := nativeHTTPFixturePool(t, func(options *gatewayNativeOptions) {
				options.command = func(path string) *exec.Cmd {
					close(entered)
					<-proceed
					command := exec.Command(path, "-test.run=^TestGatewayResearchNativeHelperProcess$")
					command.Env = append(gatewayNativeEnvironment(), "GATEWAY_RESEARCH_NATIVE_FIXTURE=1", "GATEWAY_RESEARCH_NATIVE_MARKER="+marker)
					return command
				}
			})
			t.Cleanup(release)
			p.NativeHTTP, p.NativeWebSocket = pool, true
			enableGatewayWS(t, p, "")
			// Native availability and the runtime transport preference are
			// separate. This fixture must explicitly select its synthetic helper.
			settings, err := p.Settings.Get()
			if err != nil {
				t.Fatal(err)
			}
			settings.PreferWebsocket = true
			if err := p.Settings.Set(settings); err != nil {
				t.Fatal(err)
			}
			const session = "native-research-cold-ws-owner"
			if _, err := p.sessionAccount(t.Context(), key.ID, session, gatewayAccountRef(original)); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(p)
			t.Cleanup(server.Close)
			ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
			defer cancel()
			type handshake struct {
				conn *websocket.Conn
				res  *http.Response
				err  error
			}
			done := make(chan handshake, 1)
			go func() {
				conn, res, err := websocket.Dial(ctx, server.URL+"/responses", &websocket.DialOptions{HTTPHeader: http.Header{
					"Authorization": {"Bearer " + key.Key}, "Session-Id": {session},
				}})
				done <- handshake{conn, res, err}
			}()
			select {
			case <-entered:
			case early := <-done:
				if early.conn != nil {
					_ = early.conn.CloseNow()
				}
				status := 0
				if early.res != nil {
					status = early.res.StatusCode
					if early.res.Body != nil {
						_ = early.res.Body.Close()
					}
				}
				t.Fatalf("WS handshake finished before native startup: status=%d err=%v", status, early.err)
			case <-ctx.Done():
				t.Fatal("controlled WS startup synchronization not reached:", ctx.Err())
			}
			switch change {
			case "different_user_same_workspace":
				if err := p.Store.UpdateAccount(original.ID, func(a *Account) error {
					a.Identity = &managedAccountIdentity{UserID: "replacement-user", WorkspaceID: a.AccountID}
					a.AccessToken = "replacement-ws-owner-token"
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			case "deleted":
				if err := p.Store.Delete(original.ID); err != nil {
					t.Fatal(err)
				}
			case "disabled", "account_groups", "same_owner_refresh":
				if err := p.Store.UpdateAccount(original.ID, func(a *Account) error {
					switch change {
					case "disabled":
						a.Enabled = false
					case "account_groups":
						a.Groups = []string{"ungranted-group"}
					case "same_owner_refresh":
						a.AccessToken, a.RefreshToken = "same-owner-ws-new-at", "same-owner-ws-new-rt"
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			case "key_disabled", "key_groups":
				if _, err := p.Keys.Update(key.ID, func(k *APIKey) error {
					if change == "key_disabled" {
						k.Enabled = false
					} else {
						k.Groups = []string{"ungranted-group"}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			release()
			var got handshake
			select {
			case got = <-done:
			case <-ctx.Done():
				t.Fatal("controlled WS handshake did not finish")
			}
			if got.conn != nil {
				_ = got.conn.CloseNow()
			}
			if got.res != nil && got.res.Body != nil {
				_ = got.res.Body.Close()
			}
			allowed := change == "same_owner_refresh" || change == "unchanged"
			capture, readErr := os.ReadFile(marker)
			if allowed {
				var observed map[string]string
				sum := sha256.Sum256([]byte("Bearer " + original.AccessToken))
				validCapture := readErr == nil && json.Unmarshal(capture, &observed) == nil && observed["event"] == "handshake-head-received" && observed["authorization_sha256"] == hex.EncodeToString(sum[:])
				if got.err != nil || got.conn == nil || got.res == nil || got.res.StatusCode != http.StatusSwitchingProtocols || !validCapture {
					t.Errorf("authorized WS owner failed its one native handshake: err=%v ipc=%t", got.err, readErr == nil)
				}
			} else if got.err == nil || got.conn != nil || got.res == nil || got.res.StatusCode < 400 || readErr == nil {
				t.Errorf("completed %s during native startup reached WS IPC: accepted=%t actual_ipc=%t", change, got.conn != nil, readErr == nil)
			}
			waitGatewayWSReleased(t, p)
			gatewaySessionAuthorizationAssertBinding(t, p, key.ID, session, original)
		})
	}
}

func TestGatewayResearchNativeUnsentObservationPreservesOtherHandles(t *testing.T) {
	ctx, observer := withGatewayAttemptObservation(t.Context())
	first := observer.Begin("request", "/responses", time.Now())
	unsent := observer.Begin("websocket", "/responses", time.Now())
	last := observer.Begin("refresh", "/oauth/token", time.Now())
	guarded := withGatewayNativeDispatchGuard(ctx, func(context.Context) error {
		return &gatewayRecoveryAuthorizationError{status: http.StatusForbidden}
	})
	err := checkGatewayNativeDispatch(guarded)
	var notSent *gatewayNativeDispatchUnsentError
	var authorization *gatewayRecoveryAuthorizationError
	if !errors.As(err, &notSent) || !errors.As(err, &authorization) || !authorization.beforeNativeDispatch || gatewayTransportFailureStatus(err) != http.StatusForbidden {
		t.Fatal("native pre-write marker lost original authorization classification")
	}
	unsent.discardUnsent()
	first.Headers(http.StatusUnauthorized, time.Now())
	first.Finish("upstream", "http_status", time.Now())
	last.Headers(http.StatusOK, time.Now())
	last.Finish("", "", time.Now())
	// Attempting to discard a request with received headers cannot erase it.
	first.discardUnsent()
	got := observer.Snapshot().Attempts
	if len(got) != 2 || got[0].Attempt != 1 || got[1].Attempt != 2 || got[0].StatusCode == nil || *got[0].StatusCode != http.StatusUnauthorized || got[1].Phase != "refresh" || got[1].StatusCode == nil || *got[1].StatusCode != http.StatusOK {
		t.Fatal("discarding an unsent native candidate shifted another live observation handle")
	}
}

func TestGatewayResearchNativeDispatchAuthorizationUsesDerivedClientDeadline(t *testing.T) {
	p := newTestProxy(t, 1)
	p.NativeHTTP = nativeHTTPFixturePool(t, nil)
	account, err := p.Store.Get("test")
	if err != nil {
		t.Fatal(err)
	}
	parent, stopParent := context.WithCancel(t.Context())
	defer stopParent()
	entered := make(chan struct{})
	deadlineObserved := make(chan bool, 1)
	authorize := gatewayNativeDispatchAuthorizer(func(ctx context.Context, current Account) error {
		_, hasDeadline := ctx.Deadline()
		deadlineObserved <- hasDeadline && gatewayAccountRef(current) == gatewayAccountRef(account)
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	})
	ctx := context.WithValue(parent, gatewayNativeDispatchAuthorizerKey{}, authorize)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://research-native.invalid/echo", strings.NewReader("must-not-send-after-client-deadline"))
	if err != nil {
		t.Fatal(err)
	}
	client := p.clientFor(nil)
	// Start the same origin/scope/timeout helper before measuring the derived
	// deadline. A race-instrumented helper may take >300ms to start under CPU
	// limits; that startup speed is unrelated to propagation into the guard.
	warm, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://research-native.invalid/echo", strings.NewReader("warm-dispatch-deadline-helper"))
	if err != nil {
		t.Fatal(err)
	}
	warmResponse, err := p.doAccountRequest(client, warm, account, doHTTPAccountRequest, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, warmErr := io.Copy(io.Discard, warmResponse.Body)
	_ = warmResponse.Body.Close()
	if warmErr != nil {
		t.Fatal(warmErr)
	}
	client.Timeout = 300 * time.Millisecond
	done := make(chan error, 1)
	go func() {
		response, err := p.doAccountRequest(client, req, account, doHTTPAccountRequest, nil)
		if response != nil {
			_ = response.Body.Close()
		}
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("native request did not reach dispatch authorization")
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) || parent.Err() != nil || !<-deadlineObserved {
			t.Fatal("native authorization ignored the derived HTTP client deadline or cancelled its live parent")
		}
	case <-time.After(2 * time.Second):
		stopParent()
		<-done
		t.Fatal("authorization storage wait outlived the HTTP client's derived deadline")
	}
	nativeProxyDispatchAssertIdle(t, p.NativeHTTP)
}

func TestGatewayResearchNativeProxyRotationDuringDispatchGuard(t *testing.T) {
	for _, mode := range []string{"http", "websocket"} {
		t.Run(mode, func(t *testing.T) {
			p := newTestProxy(t, 1)
			p.NativeHTTP = nativeHTTPFixturePool(t, func(options *gatewayNativeOptions) {
				original := options.command
				options.command = func(path string) *exec.Cmd {
					command := original(path)
					command.Env = append(command.Env, "GATEWAY_NATIVE_FIXTURE_CAPABILITIES=websocket_message_v1")
					return command
				}
			})
			p.BaseURL = "http://research-native.invalid"
			entry := poolProxyEntry(t, p, "http://127.0.0.1:1")
			ctx := withGatewayNativeDispatchGuard(t.Context(), func(context.Context) error {
				_, err := p.Proxies.UpdateVersion(entry.ID, entry.Version, func(current *ProxyEntry) error {
					current.Password = "changed-during-owner-read"
					return nil
				})
				return err
			})
			var err error
			if mode == "http" {
				req, buildErr := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/echo", strings.NewReader("must-not-cross-rotated-proxy"))
				if buildErr != nil {
					t.Fatal(buildErr)
				}
				res, requestErr := p.clientFor(&entry).Do(req)
				err = requestErr
				if res != nil {
					_ = res.Body.Close()
				}
			} else {
				conn, res, dialErr := p.dialNativeGatewayWebSocket(ctx, nil, &entry)
				err = dialErr
				if conn != nil {
					_ = conn.CloseNow()
				}
				if res != nil && res.Body != nil {
					_ = res.Body.Close()
				}
			}
			if !errors.Is(err, errGatewayProxyUnavailable) {
				t.Fatalf("proxy edit during final owner authorization reached stale IPC instead of rejecting: %v", err)
			}
			nativeProxyDispatchAssertIdle(t, p.NativeHTTP)
		})
	}
}
