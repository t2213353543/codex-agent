package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestGatewayNativeHTTPHelperProcess(t *testing.T) {
	if os.Getenv("GATEWAY_NATIVE_FIXTURE") != "1" {
		return
	}
	var length uint32
	if binary.Read(os.Stdin, binary.BigEndian, &length) != nil || length > gatewayNativeHeadLimit {
		os.Exit(2)
	}
	raw := make([]byte, length)
	if _, err := io.ReadFull(os.Stdin, raw); err != nil {
		os.Exit(2)
	}
	var config struct {
		Token      string `json:"ipc_token"`
		Origin     string `json:"upstream"`
		Timeout    int64  `json:"headers_timeout_ms"`
		Lifecycle  string `json:"http_client_lifecycle"`
		UnixSocket string `json:"unix_socket"`
	}
	if json.Unmarshal(raw, &config) != nil {
		os.Exit(2)
	}
	if os.Getenv("GATEWAY_NATIVE_FIXTURE_MODE") == "no-ready" {
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	}
	network, address := "tcp", "127.0.0.1:0"
	if config.UnixSocket != "" {
		network, address = "unix", config.UnixSocket
	}
	listener, err := net.Listen(network, address)
	if err != nil {
		os.Exit(2)
	}
	if network == "unix" {
		if os.Chmod(address, 0600) != nil {
			os.Exit(2)
		}
	}
	ready := map[string]any{"protocol": 1, "transport": "tcp", "listen": listener.Addr().String(),
		"max_body": gatewayNativeBodyLimit, "max_active": 0, "http_client_lifecycle": config.Lifecycle}
	if network == "unix" {
		ready["transport"], ready["unix_socket"] = "unix", address
	}
	if capabilities := os.Getenv("GATEWAY_NATIVE_FIXTURE_CAPABILITIES"); capabilities != "" {
		ready["capabilities"] = strings.Split(capabilities, ",")
	}
	if os.Getenv("GATEWAY_NATIVE_FIXTURE_MODE") == "bad-ready" {
		ready["protocol"] = 8
	}
	if os.Getenv("GATEWAY_NATIVE_FIXTURE_MODE") == "bound-no-ready" {
		for {
			time.Sleep(time.Hour)
		}
	}
	switch os.Getenv("GATEWAY_NATIVE_FIXTURE_MODE") {
	case "legacy-concurrency":
		ready["max_active"] = 32
	case "missing-concurrency":
		delete(ready, "max_active")
	case "legacy-lifecycle":
		delete(ready, "http_client_lifecycle")
	case "wrong-lifecycle":
		if config.Lifecycle == "cli_request" {
			ready["http_client_lifecycle"] = "pooled"
		} else {
			ready["http_client_lifecycle"] = "cli_request"
		}
	}
	_ = json.NewEncoder(os.Stdout).Encode(ready)
	go func() {
		_, _ = io.Copy(io.Discard, os.Stdin)
		switch os.Getenv("GATEWAY_NATIVE_FIXTURE_MODE") {
		case "bad-exit":
			os.Exit(7)
		case "ignore-eof":
			select {}
		}
		os.Exit(0)
	}()
	for {
		conn, err := listener.Accept()
		if err != nil {
			os.Exit(0)
		}
		go func() {
			defer conn.Close()
			var length uint32
			if binary.Read(conn, binary.BigEndian, &length) != nil || length > gatewayNativeHeadLimit {
				return
			}
			raw := make([]byte, length)
			if _, err := io.ReadFull(conn, raw); err != nil {
				return
			}
			var head struct {
				Token      string      `json:"ipc_token"`
				Method     string      `json:"method"`
				Path       string      `json:"path"`
				Headers    [][2]string `json:"headers"`
				BodyLength int         `json:"body_length"`
			}
			if json.Unmarshal(raw, &head) != nil || head.Token != config.Token {
				return
			}
			body := make([]byte, head.BodyLength)
			if _, err := io.ReadFull(conn, body); err != nil {
				return
			}
			frame := func(kind byte, payload []byte) {
				_, _ = conn.Write([]byte{kind})
				_ = binary.Write(conn, binary.BigEndian, uint32(len(payload)))
				_, _ = conn.Write(payload)
			}
			if head.Path == "/error" {
				frame(4, []byte("upstream_timeout"))
				return
			}
			if head.Path == "/unknown-error" {
				frame(4, []byte("private-route-secret"))
				return
			}
			if head.Path == "/bad-frame" {
				_, _ = conn.Write([]byte{1, 0xff, 0xff, 0xff, 0xff})
				return
			}
			if head.Path == "/wait-headers" {
				_, _ = io.Copy(io.Discard, conn)
				return
			}
			header := [][2]string{{"X-Native-Pid", base64.StdEncoding.EncodeToString([]byte(strconvNativePID()))},
				{"X-Native-Origin", base64.StdEncoding.EncodeToString([]byte(config.Origin))},
				{"X-Native-Timeout", base64.StdEncoding.EncodeToString([]byte(fmt.Sprint(config.Timeout)))},
				{"X-Native-Lifecycle", base64.StdEncoding.EncodeToString([]byte(config.Lifecycle))},
				{"X-Opaque", base64.StdEncoding.EncodeToString([]byte{'x', 0x80, 0xff})},
				{"Set-Cookie", base64.StdEncoding.EncodeToString([]byte("a=1"))}, {"Set-Cookie", base64.StdEncoding.EncodeToString([]byte("b=2"))}}
			if head.Path == "/length-short" || head.Method == "HEAD" {
				header = append(header, [2]string{"Content-Length", base64.StdEncoding.EncodeToString([]byte("10"))})
			}
			if head.Path == "/inspect" {
				body, _ = json.Marshal(head)
			}
			headBytes, _ := json.Marshal(map[string]any{"status": 202, "version": "HTTP/2", "headers": header})
			frame(1, headBytes)
			switch head.Path {
			case "/no-end":
				frame(2, []byte("partial"))
				return
			case "/hold":
				frame(2, []byte("first"))
				_, _ = io.Copy(io.Discard, conn)
				return
			case "/fragment":
				_, _ = conn.Write([]byte{2, 0, 0, 0, 5, 'x'})
				return
			}
			if head.Method != "HEAD" {
				frame(2, body)
			}
			frame(3, nil)
		}()
	}
}

func strconvNativePID() string { return fmt.Sprint(os.Getpid()) }

func nativeHTTPFixturePool(t *testing.T, configure func(*gatewayNativeOptions)) *gatewayNativePool {
	t.Helper()
	binaryPath, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(binaryPath)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	options := gatewayNativeOptions{network: "tcp", startup: 3 * time.Second, stop: time.Second,
		command: func(path string) *exec.Cmd {
			command := exec.Command(path, "-test.run=^TestGatewayNativeHTTPHelperProcess$")
			command.Env = append(gatewayNativeEnvironment(), "GATEWAY_NATIVE_FIXTURE=1")
			return command
		}}
	if configure != nil {
		configure(&options)
	}
	pool, err := newGatewayNativePoolWithOptions(binaryPath, hex.EncodeToString(digest[:]), options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	return pool
}

func nativeHTTPFixtureRequest(t *testing.T, rt http.RoundTripper, ctx context.Context, method, target, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, method, target, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	res, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestGatewayNativeHTTPRoundTripAndRouteIsolation(t *testing.T) {
	p := nativeHTTPFixturePool(t, nil)
	rt := p.RoundTripper(nil, 2*time.Second)
	var first string
	for i, target := range []string{"http://first.invalid/echo?q=a%20b", "http://first.invalid/echo", "https://oauth.invalid/echo"} {
		res := nativeHTTPFixtureRequest(t, rt, t.Context(), "POST", target, "raw\x00payload")
		body, err := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if err != nil || string(body) != "raw\x00payload" || res.StatusCode != 202 || res.ProtoMajor != 2 || res.Header.Get("X-Opaque") != string([]byte{'x', 0x80, 0xff}) || len(res.Header.Values("Set-Cookie")) != 2 {
			t.Fatalf("response bytes/status/headers changed: status=%d body=%q err=%v", res.StatusCode, body, err)
		}
		pid := res.Header.Get("X-Native-Pid")
		if i == 0 {
			first = pid
		} else if i == 1 && pid != first || i == 2 && pid == first {
			t.Fatal("process origin reuse/isolation failed")
		}
	}
	res := nativeHTTPFixtureRequest(t, p.RoundTripper(nil, 3*time.Second), t.Context(), "HEAD", "http://first.invalid/head", "")
	body, err := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if err != nil || len(body) != 0 || res.ContentLength != 10 || res.Header.Get("X-Native-Pid") == first || res.Header.Get("X-Native-Timeout") != "3000" {
		t.Fatal("HEAD framing or timeout isolation failed")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.entries) != 3 {
		t.Fatalf("process count=%d", len(p.entries))
	}
}

func TestGatewayNativeHTTPProtocolFailuresDoNotBecomeEOF(t *testing.T) {
	p := nativeHTTPFixturePool(t, nil)
	rt := p.RoundTripper(nil, time.Second)
	for _, path := range []string{"no-end", "fragment", "length-short", "bad-frame", "unknown-error", "error"} {
		t.Run(path, func(t *testing.T) {
			req, _ := http.NewRequest("POST", "http://fixture.invalid/"+path, strings.NewReader("short"))
			res, err := rt.RoundTrip(req)
			if err == nil {
				_, err = io.ReadAll(res.Body)
				_ = res.Body.Close()
			}
			if err == nil || err == io.EOF || strings.Contains(err.Error(), "private-route-secret") {
				t.Fatalf("invalid protocol ended successfully or leaked detail: %v", err)
			}
			if path == "error" {
				var timeout interface{ Timeout() bool }
				if !errors.As(err, &timeout) || !timeout.Timeout() {
					t.Fatal("helper timeout lost Timeout contract")
				}
			} else if path == "no-end" || path == "fragment" || path == "length-short" {
				if !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatalf("missing END/bytes did not return UnexpectedEOF: %v", err)
				}
			}
		})
	}
}

func TestGatewayNativeHTTPContextElasticRoutesAndRetirement(t *testing.T) {
	p := nativeHTTPFixturePool(t, func(o *gatewayNativeOptions) { o.idleProcessLimit = 1 })
	proxy := ProxyEntry{ID: "route", Type: "http", Host: "127.0.0.1", Port: 1234, Enabled: true, Version: 1}
	ctx, cancel := context.WithCancel(t.Context())
	rt := p.RoundTripper(&proxy, time.Second)
	first := nativeHTTPFixtureRequest(t, rt, ctx, "GET", "http://fixture.invalid/hold", "")
	defer first.Body.Close()
	buf := make([]byte, 5)
	if _, err := io.ReadFull(first.Body, buf); err != nil {
		t.Fatal(err)
	}
	extra := nativeHTTPFixtureRequest(t, rt, t.Context(), "GET", "http://fixture.invalid/echo", "")
	_ = extra.Body.Close()
	proxy.Version++
	newRoute := p.RoundTripper(&proxy, time.Second)
	second := nativeHTTPFixtureRequest(t, newRoute, t.Context(), "GET", "http://fixture.invalid/hold", "")
	defer second.Body.Close()
	_, _ = io.ReadFull(second.Body, buf)
	third := nativeHTTPFixtureRequest(t, newRoute, t.Context(), "GET", "http://third.invalid/echo", "")
	_ = third.Body.Close()
	readResult := make(chan error, 1)
	go func() { _, err := first.Body.Read(buf); readResult <- err }()
	cancel()
	select {
	case err := <-readResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel error=%v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("request cancellation did not unblock IPC")
	}
	_ = second.Body.Close()
	recovered := nativeHTTPFixtureRequest(t, newRoute, t.Context(), "GET", "http://third.invalid/echo", "")
	_, _ = io.ReadAll(recovered.Body)
	_ = recovered.Body.Close()
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest("GET", "http://third.invalid/echo", nil)
	if _, err := newRoute.RoundTrip(request); !errors.Is(err, errGatewayNativeClosed) {
		t.Fatalf("closed pool accepted another request: %v", err)
	}
}

func TestGatewayNativeHTTPIdleReaperStopsChild(t *testing.T) {
	p := nativeHTTPFixturePool(t, func(o *gatewayNativeOptions) { o.idle = 40 * time.Millisecond })
	res := nativeHTTPFixtureRequest(t, p.RoundTripper(nil, time.Second), t.Context(), "GET", "http://fixture.invalid/echo", "")
	_, _ = io.ReadAll(res.Body)
	_ = res.Body.Close()
	p.mu.Lock()
	var child *gatewayNativeProcess
	for _, entry := range p.entries {
		child = entry
	}
	p.mu.Unlock()
	select {
	case <-child.done:
	case <-time.After(3 * time.Second):
		t.Fatal("idle helper needed new traffic before it exited")
	}
	deadline := time.Now().Add(time.Second)
	for {
		p.mu.Lock()
		count := len(p.entries)
		p.mu.Unlock()
		if count == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("idle helper retained its process slot")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := os.Stat(child.dir); !os.IsNotExist(err) {
		t.Fatal("idle helper retained its private directory")
	}
}

func TestGatewayNativeHTTPStartupFailureAndValidation(t *testing.T) {
	for _, mode := range []string{"bad-ready", "no-ready"} {
		t.Run(mode, func(t *testing.T) {
			p := nativeHTTPFixturePool(t, func(o *gatewayNativeOptions) {
				o.startup = 150 * time.Millisecond
				original := o.command
				o.command = func(path string) *exec.Cmd {
					cmd := original(path)
					cmd.Env = append(cmd.Env, "GATEWAY_NATIVE_FIXTURE_MODE="+mode)
					return cmd
				}
			})
			req, _ := http.NewRequestWithContext(t.Context(), "GET", "http://fixture.invalid/", nil)
			if _, err := p.RoundTripper(nil, time.Second).RoundTrip(req); !errors.Is(err, errGatewayNativeUnavailable) {
				t.Fatalf("bad startup accepted: %v", err)
			}
			_ = p.Close()
			if len(p.entries) != 0 {
				t.Fatal("failed startup consumed a process slot")
			}
		})
	}
	if _, err := newGatewayNativePoolWithOptions("relative", strings.Repeat("a", 64), gatewayNativeOptions{}); err == nil {
		t.Fatal("relative helper accepted")
	}
	if runtime.GOOS != "linux" {
		if _, err := newGatewayNativePool(filepath.Join(t.TempDir(), "helper"), strings.Repeat("a", 64)); !errors.Is(err, errGatewayNativeUnsupported) {
			t.Fatal("non-Linux production native enabled")
		}
	}
	p := nativeHTTPFixturePool(t, nil)
	req, _ := http.NewRequestWithContext(t.Context(), "GET", "http://fixture.invalid/", nil)
	if _, err := p.RoundTripper(&ProxyEntry{Type: "unsupported", Host: "127.0.0.1", Port: 1234}, time.Second).RoundTrip(req); !errors.Is(err, errGatewayNativeProxy) {
		t.Fatalf("unsupported route did not fail explicitly: %v", err)
	}
}

type gatewayNativeTrackedBody struct {
	io.Reader
	closed atomic.Int32
}

func (b *gatewayNativeTrackedBody) Close() error { b.closed.Add(1); return nil }

func TestGatewayNativeHTTPRequestBoundsAndHeaderBytes(t *testing.T) {
	p := nativeHTTPFixturePool(t, nil)
	rt := p.RoundTripper(nil, time.Second)
	for _, length := range []int64{gatewayNativeBodyLimit + 1, 5} {
		body := &gatewayNativeTrackedBody{Reader: strings.NewReader("x")}
		req, _ := http.NewRequest("POST", "http://fixture.invalid/", body)
		req.ContentLength = length
		if _, err := rt.RoundTrip(req); err == nil || body.closed.Load() != 1 {
			t.Fatalf("invalid request length accepted or body not closed: %v", err)
		}
	}
	req, _ := http.NewRequest("POST", "http://fixture.invalid/inspect", strings.NewReader("{}"))
	req.Header["X-Bytes"] = []string{string([]byte{0x80, 0xff}), "second"}
	res, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var head struct {
		Headers    [][2]string `json:"headers"`
		BodyLength int         `json:"body_length"`
	}
	if json.NewDecoder(res.Body).Decode(&head) != nil || head.BodyLength != 2 || len(head.Headers) != 2 || head.Headers[0][1] != base64.StdEncoding.EncodeToString([]byte{0x80, 0xff}) {
		t.Fatal("request header bytes/repetition/body length changed")
	}
}

func TestGatewayNativeHTTPConcurrentPoolCreation(t *testing.T) {
	p := nativeHTTPFixturePool(t, nil)
	rt := p.RoundTripper(nil, time.Second)
	var workers sync.WaitGroup
	errors := make(chan error, 12)
	for range 12 {
		workers.Go(func() {
			req, _ := http.NewRequestWithContext(t.Context(), "GET", "http://fixture.invalid/echo", nil)
			res, err := rt.RoundTrip(req)
			if err == nil {
				_, err = io.Copy(io.Discard, res.Body)
				_ = res.Body.Close()
			}
			errors <- err
		})
	}
	workers.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.entries) != 1 {
		t.Fatalf("concurrent route creation spawned %d processes", len(p.entries))
	}
}

func TestGatewayNativeHTTPEnvironmentAllowlist(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "fixture-private")
	t.Setenv("HTTPS_PROXY", "http://private.invalid")
	t.Setenv("CODEX_CA_CERTIFICATE", "/synthetic/ca.pem")
	joined := strings.Join(gatewayNativeEnvironment(), "\n")
	if strings.Contains(joined, "fixture-private") || strings.Contains(joined, "private.invalid") || !strings.Contains(joined, "CODEX_CA_CERTIFICATE=/synthetic/ca.pem") {
		t.Fatal("helper inherited unrelated private environment")
	}
}

func TestGatewayNativeHTTPSlowStartupDoesNotBlockOtherRoutes(t *testing.T) {
	var starts atomic.Int32
	p := nativeHTTPFixturePool(t, func(o *gatewayNativeOptions) {
		o.idleProcessLimit, o.startup = 1, 3*time.Second
		original := o.command
		o.command = func(path string) *exec.Cmd {
			cmd := original(path)
			if starts.Add(1) == 2 {
				cmd.Env = append(cmd.Env, "GATEWAY_NATIVE_FIXTURE_MODE=no-ready")
			}
			return cmd
		}
	})
	rt := p.RoundTripper(nil, time.Second)
	active := nativeHTTPFixtureRequest(t, rt, t.Context(), "GET", "http://ready.invalid/hold", "")
	defer active.Body.Close()
	buf := make([]byte, 5)
	_, _ = io.ReadFull(active.Body, buf)
	result := make(chan error, 1)
	go func() {
		req, _ := http.NewRequestWithContext(t.Context(), "GET", "http://starting.invalid/echo", nil)
		_, err := rt.RoundTrip(req)
		result <- err
	}()
	deadline := time.Now().Add(2 * time.Second)
	for starts.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("slow fixture did not begin startup")
		}
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
	defer cancel()
	res := nativeHTTPFixtureRequest(t, rt, ctx, "POST", "http://ready.invalid/echo", "available")
	body, err := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if err != nil || string(body) != "available" {
		t.Fatalf("slow startup blocked a ready route: %v", err)
	}
	waitCtx, stopWait := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer stopWait()
	req, _ := http.NewRequestWithContext(waitCtx, "GET", "http://starting.invalid/echo", nil)
	started := time.Now()
	if _, err := rt.RoundTrip(req); !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > time.Second {
		t.Fatalf("startup waiter did not obey independent context: %v", err)
	}
	third := nativeHTTPFixtureRequest(t, rt, t.Context(), "GET", "http://third.invalid/echo", "")
	_ = third.Body.Close()
	started = time.Now()
	_ = p.Close()
	if time.Since(started) > 2*time.Second {
		t.Fatal("Close waited for the stalled readiness timeout")
	}
	if err := <-result; err == nil {
		t.Fatal("closed startup returned a usable client")
	}
}

func TestGatewayNativeHTTPShutdownDoesNotDeleteReplacementPath(t *testing.T) {
	p := nativeHTTPFixturePool(t, nil)
	res := nativeHTTPFixtureRequest(t, p.RoundTripper(nil, time.Second), t.Context(), "GET", "http://fixture.invalid/echo", "")
	_, _ = io.ReadAll(res.Body)
	_ = res.Body.Close()
	p.mu.Lock()
	var child *gatewayNativeProcess
	for _, value := range p.entries {
		child = value
	}
	p.mu.Unlock()
	path := filepath.Join(child.dir, "s")
	if err := os.WriteFile(path, []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(path); _ = os.Remove(child.dir) })
	_ = p.Close()
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != "replacement" {
		t.Fatalf("shutdown removed an unrelated replacement path: %v", err)
	}
}

func TestGatewayNativeHTTPAnonymousProxyDoesNotRetireOtherRoutes(t *testing.T) {
	p := nativeHTTPFixturePool(t, nil)
	direct := p.RoundTripper(nil, time.Second)
	first := nativeHTTPFixtureRequest(t, direct, t.Context(), "GET", "http://fixture.invalid/echo", "")
	_, _ = io.ReadAll(first.Body)
	_ = first.Body.Close()
	firstPID := first.Header.Get("X-Native-Pid")
	proxyA := &ProxyEntry{Type: "http", Host: "127.0.0.1", Port: 1234, Enabled: true}
	proxyB := &ProxyEntry{Type: "http", Host: "127.0.0.1", Port: 5678, Enabled: true}
	routes := []http.RoundTripper{direct, p.RoundTripper(proxyA, time.Second), p.RoundTripper(proxyB, time.Second)}
	var pids []string
	for range 2 {
		for index, route := range routes {
			res := nativeHTTPFixtureRequest(t, route, t.Context(), "GET", "http://fixture.invalid/echo", "")
			_, _ = io.ReadAll(res.Body)
			_ = res.Body.Close()
			pid := res.Header.Get("X-Native-Pid")
			if len(pids) <= index {
				pids = append(pids, pid)
			} else if pids[index] != pid {
				t.Fatal("anonymous route construction retired another independent route")
			}
		}
	}
	if pids[0] != firstPID || pids[0] == pids[1] || pids[1] == pids[2] {
		t.Fatal("direct and anonymous proxy process isolation was lost")
	}
}

func TestGatewayNativeHTTPOlderSnapshotCannotRetireNewerProcess(t *testing.T) {
	p := nativeHTTPFixturePool(t, nil)
	newer := ProxyEntry{ID: "ordered-route", Version: 2, Type: "http", Host: "127.0.0.1", Port: 1234, Enabled: true}
	route := p.RoundTripper(&newer, time.Second)
	first := nativeHTTPFixtureRequest(t, route, t.Context(), "GET", "http://fixture.invalid/echo", "")
	_, _ = io.ReadAll(first.Body)
	_ = first.Body.Close()
	pid := first.Header.Get("X-Native-Pid")
	older := newer
	older.Version, older.Password = 1, "old-fixture"
	_ = p.RoundTripper(&older, time.Second)
	zeroVersion := newer
	zeroVersion.Version, zeroVersion.Password = 0, "unversioned-fixture"
	_ = p.RoundTripper(&zeroVersion, time.Second)
	for range 2 {
		res := nativeHTTPFixtureRequest(t, route, t.Context(), "GET", "http://fixture.invalid/echo", "")
		_, _ = io.ReadAll(res.Body)
		_ = res.Body.Close()
		if res.Header.Get("X-Native-Pid") != pid {
			t.Fatal("late older snapshot retired the already established new route")
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.entries) != 1 {
		t.Fatal("constructing a stale route changed live process inventory")
	}
	for _, child := range p.entries {
		if child.retired {
			t.Fatal("late older snapshot marked the new route retired")
		}
	}
}

func TestGatewayNativeHTTPLifecycleDefaultsAndConfigHandshake(t *testing.T) {
	for _, configured := range []string{"", "cli_request", "pooled"} {
		name, want := configured, configured
		if configured == "" {
			name, want = "default", "cli_request"
		}
		t.Run(name, func(t *testing.T) {
			p := nativeHTTPFixturePool(t, func(options *gatewayNativeOptions) { options.lifecycle = configured })
			if p.options.lifecycle != want {
				t.Fatalf("HTTP lifecycle=%q want=%q", p.options.lifecycle, want)
			}
			route := p.RoundTripper(nil, time.Second)
			var firstPID string
			for range 2 {
				response := nativeHTTPFixtureRequest(t, route, t.Context(), "GET", "http://fixture.invalid/echo", "")
				_, err := io.ReadAll(response.Body)
				_ = response.Body.Close()
				if err != nil || response.Header.Get("X-Native-Lifecycle") != want {
					t.Fatalf("helper did not receive configured HTTP lifecycle: %v", err)
				}
				pid := response.Header.Get("X-Native-Pid")
				if firstPID != "" && firstPID != pid {
					t.Fatal("request-scoped HTTP lifecycle unnecessarily restarted the helper process")
				}
				firstPID = pid
			}
		})
	}
}

func TestGatewayNativeHTTPLifecycleInvalidConfigurationRejected(t *testing.T) {
	for _, lifecycle := range []string{"unknown", "CLI_REQUEST", "pooled ", " cli_request"} {
		pool, err := newGatewayNativePoolWithOptions("not-a-helper", "", gatewayNativeOptions{lifecycle: lifecycle})
		if pool != nil || !errors.Is(err, errGatewayNativeLifecycle) {
			t.Fatalf("invalid lifecycle was not rejected before helper access: %q %v", lifecycle, err)
		}
	}
}

func TestGatewayNativeHTTPLifecycleRejectsMissingOrDifferentReadiness(t *testing.T) {
	for _, lifecycle := range []string{"cli_request", "pooled"} {
		for _, mode := range []string{"legacy-lifecycle", "wrong-lifecycle"} {
			t.Run(lifecycle+"/"+mode, func(t *testing.T) {
				var childCommand *exec.Cmd
				p := nativeHTTPFixturePool(t, func(options *gatewayNativeOptions) {
					options.lifecycle = lifecycle
					original := options.command
					options.command = func(path string) *exec.Cmd {
						cmd := original(path)
						cmd.Env = append(cmd.Env, "GATEWAY_NATIVE_FIXTURE_MODE="+mode)
						childCommand = cmd
						return cmd
					}
				})
				req, _ := http.NewRequestWithContext(t.Context(), "GET", "http://fixture.invalid/echo", nil)
				res, err := p.RoundTripper(nil, time.Second).RoundTrip(req)
				if res != nil || !errors.Is(err, errGatewayNativeLifecycleReady) {
					t.Fatalf("unproven helper lifecycle became usable: %v", err)
				}
				_ = p.Close()
				if childCommand == nil || childCommand.ProcessState == nil || len(p.entries) != 0 {
					t.Fatal("rejected helper lifecycle retained an uncollected child or pool slot")
				}
			})
		}
	}
}
