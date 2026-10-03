package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const shutdownTestAdminKey = "synthetic-shutdown-admin-key-at-least-32-bytes"

// This helper runs the exact production serve/drain/final-persistence lifecycle
// in another OS process. It is selected only by its parent test, never by a
// normal test run; no credentials are printed in readiness or process output.
func TestPGShutdownProcessHelper(t *testing.T) {
	if os.Getenv("CODEX_TEST_SHUTDOWN_CHILD") != "1" {
		return
	}
	ctx := context.Background()
	db, err := OpenDatabase(ctx, os.Getenv("CODEX_TEST_SHUTDOWN_DSN"), base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)), "account-test")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.ConfigureRedis(os.Getenv("TEST_REDIS_URL")); err != nil {
		t.Fatal("configure isolated shutdown Redis:", err)
	}
	db.Namespace = os.Getenv("CODEX_TEST_SHUTDOWN_NAMESPACE")
	if !strings.HasPrefix(db.Namespace, "codex-agent:integration:shutdown:") {
		t.Fatal("shutdown helper requires an isolated Redis namespace")
	}
	metrics, err := NewPGMetrics(db)
	if err != nil {
		t.Fatal(err)
	}
	defer metrics.Close()
	keys, err := NewPGKeyStore(db)
	if err != nil {
		t.Fatal(err)
	}
	proxies, err := NewPGProxyStore(db)
	if err != nil {
		t.Fatal(err)
	}
	p := &Proxy{Store: Store{DB: db}, Groups: NewPGGroupStore(db), Keys: keys, Proxies: proxies, Settings: NewPGSettingsStore(db), Metrics: metrics, AdminKey: shutdownTestAdminKey, Client: newHTTPClient(), BaseURL: os.Getenv("CODEX_TEST_SHUTDOWN_UPSTREAM"), TokenURL: os.Getenv("CODEX_TEST_SHUTDOWN_UPSTREAM") + "/oauth/token", startedAt: time.Now()}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stop, stopSignals := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	if err := os.WriteFile(os.Getenv("CODEX_TEST_SHUTDOWN_READY"), []byte("http://"+listener.Addr().String()), 0600); err != nil {
		listener.Close()
		t.Fatal(err)
	}
	if err := serveManaged(stop, p, listener, t.TempDir()); err != nil {
		t.Fatal(err)
	}
}

func TestPGShutdownPersistsRefreshAfterClientDisconnectAndSIGTERM(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX SIGTERM; cross-compiled Linux test binary covers the production lifecycle")
	}
	if os.Getenv("TEST_REDIS_URL") == "" {
		t.Skip("TEST_REDIS_URL is required for isolated content-session routing")
	}
	s := accountPGTestStore(t)
	if err := s.DB.ConfigureRedis(os.Getenv("TEST_REDIS_URL")); err != nil {
		t.Fatal("configure isolated shutdown Redis:", err)
	}
	s.DB.Namespace = "codex-agent:integration:shutdown:" + newID("") + ":"
	raw, _ := json.Marshal(map[string]any{"access_token": "synthetic-before-shutdown", "refresh_token": "synthetic-refresh-before-shutdown", "expires_at": time.Now().Add(-time.Hour).Unix()})
	report, err := s.ImportDetailed([]ImportItem{{Name: "shutdown-account", Credentials: raw}}, AccountDefaults{}, ImportOptions{})
	if err != nil || report.Added != 1 {
		t.Fatal("prepare account:", err)
	}
	if _, err := s.DB.initializeAdminKey(context.Background(), shutdownTestAdminKey); err != nil {
		t.Fatal("cannot initialize synthetic admin key")
	}
	settings := defaultSettings()
	settings.AdminAPI = true
	if err := NewPGSettingsStore(s.DB).Set(settings); err != nil {
		t.Fatal(err)
	}
	// Headerless Responses now infer a sticky content route. Exercise that
	// production path with shared Redis instead of failing admission before the
	// OAuth exchange, and clean only this test's exact synthetic binding.
	const requestBody = `{"model":"shutdown-test","input":"test","stream":false}`
	_, session, err := applyGatewayPolicy(nil, []byte(requestBody), "admin", settings)
	if err != nil || !isGatewayContentSessionKey(session) {
		t.Fatal("shutdown request must exercise inferred content routing", err)
	}
	threadKey := fmt.Sprintf("%sgateway:thread:%x", s.DB.Namespace, sha256.Sum256([]byte("admin\x00"+session)))
	t.Cleanup(func() {
		if err := s.DB.Redis.Del(context.Background(), threadKey).Err(); err != nil {
			t.Error("cleanup isolated shutdown route:", err)
		}
	})
	started, release := make(chan struct{}), make(chan struct{})
	var startedOnce, releaseOnce sync.Once
	releaseRefresh := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseRefresh()
	oauthCanceled := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" {
			http.Error(w, "request canceled after refresh", http.StatusBadGateway)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		startedOnce.Do(func() { close(started) })
		select {
		case <-release:
		case <-r.Context().Done():
			select {
			case oauthCanceled <- struct{}{}:
			default:
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"synthetic-after-shutdown","refresh_token":"synthetic-refresh-after-shutdown","expires_in":3600}`)
	}))
	defer upstream.Close()
	readyPath := filepath.Join(t.TempDir(), "ready")
	cmd := exec.Command(os.Args[0], "-test.run=^TestPGShutdownProcessHelper$", "-test.v")
	cmd.Env = append(os.Environ(), "CODEX_TEST_SHUTDOWN_CHILD=1", "CODEX_TEST_SHUTDOWN_DSN="+s.DB.Pool.Config().ConnString(), "CODEX_TEST_SHUTDOWN_UPSTREAM="+upstream.URL, "CODEX_TEST_SHUTDOWN_READY="+readyPath, "CODEX_TEST_SHUTDOWN_NAMESPACE="+s.DB.Namespace)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exit := make(chan error, 1)
	go func() { exit <- cmd.Wait() }()
	processFinished := false
	defer func() {
		releaseRefresh()
		if !processFinished {
			_ = cmd.Process.Kill()
			<-exit
		}
	}()
	var address string
	deadline := time.NewTimer(20 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
ready:
	for {
		select {
		case err := <-exit:
			processFinished = true
			t.Fatalf("child exited before readiness: %v; %s", err, output.String())
		case <-deadline.C:
			t.Fatal("child readiness timeout")
		case <-ticker.C:
			if b, err := os.ReadFile(readyPath); err == nil && len(b) > 0 {
				address = string(b)
				break ready
			}
		}
	}
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	defer cancelRequest()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, address+"/v1/responses", strings.NewReader(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+shutdownTestAdminKey)
	request.Header.Set("Content-Type", "application/json")
	type clientResult struct {
		status int
		body   string
		err    error
	}
	clientDone := make(chan clientResult, 1)
	go func() {
		response, err := http.DefaultClient.Do(request)
		result := clientResult{err: err}
		if response != nil {
			result.status = response.StatusCode
			body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
			result.body = string(body)
			response.Body.Close()
		}
		clientDone <- result
	}()
	select {
	case <-started:
	case result := <-clientDone:
		t.Fatalf("gateway returned before refresh: status=%d err=%v body=%q", result.status, result.err, result.body)
	case <-time.After(20 * time.Second):
		t.Fatal("OAuth refresh did not start")
	}
	cancelRequest()
	select {
	case <-clientDone:
	case <-time.After(3 * time.Second):
		t.Fatal("client did not disconnect")
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-exit:
		processFinished = true
		t.Fatalf("shutdown exited with refresh in flight: %v; %s", err, output.String())
	case <-oauthCanceled:
		t.Fatal("client disconnect or SIGTERM canceled the rotating OAuth exchange")
	case <-time.After(200 * time.Millisecond):
	}
	// Shutdown must already stop new connections while preserving the in-flight
	// handler until the refreshed credentials and final event are persisted.
	probe, err := net.DialTimeout("tcp", strings.TrimPrefix(address, "http://"), time.Second)
	if err == nil {
		probe.Close()
		t.Fatal("SIGTERM did not stop accepting new connections")
	}
	releaseRefresh()
	select {
	case err := <-exit:
		processFinished = true
		if err != nil {
			t.Fatalf("managed shutdown failed: %v; %s", err, output.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatal("child did not finish draining")
	}
	select {
	case <-oauthCanceled:
		t.Fatal("rotating OAuth exchange was canceled")
	default:
	}
	// Reopening state after the process exits must recover both newly rotated
	// secrets and the request event which finished during the drain window.
	stored, err := (Store{DB: s.DB}).Get("shutdown-account")
	if err != nil {
		t.Fatal(err)
	}
	if stored.AccessToken != "synthetic-after-shutdown" || stored.RefreshToken != "synthetic-refresh-after-shutdown" {
		t.Fatal("shutdown lost rotated credentials")
	}
	var events, logs, total int64
	err = s.DB.Pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM usage_events),(SELECT count(*) FROM request_logs),COALESCE((SELECT requests FROM usage_totals WHERE scope='global' AND subject=''),0)`).Scan(&events, &logs, &total)
	if err != nil {
		t.Fatal(err)
	}
	if events != 1 || logs != 1 || total != 1 {
		t.Fatalf("final persistence incomplete: events=%d logs=%d cumulative=%d", events, logs, total)
	}
}
