package server

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// Opt-in, synthetic same-platform comparison. Only sanitized TLS metadata is
// retained; default trust probes intentionally stop after the first hello.
func TestSourceNativeHelperTLSComparison(t *testing.T) {
	cli, helper := os.Getenv("CODEX_NATIVE_CAPTURE_BIN"), os.Getenv("CODEX_NATIVE_HELPER_BIN")
	if cli == "" || helper == "" {
		t.Skip("set CODEX_NATIVE_CAPTURE_BIN and CODEX_NATIVE_HELPER_BIN")
	}
	identity := nativeCaptureIdentity(t, cli, "official_npm_release")
	helperBytes, err := os.ReadFile(helper)
	if err != nil {
		t.Fatal("cannot hash selected helper")
	}
	sum := sha256.Sum256(helperBytes)
	cases := []map[string]any{}
	for _, customCA := range []bool{false, true} {
		name := "default_trust_client_hello"
		if customCA {
			name = "custom_ca_direct_default_env"
		}
		t.Run(name, func(t *testing.T) {
			official := nativeCaptureRun(t, cli, identity, name, customCA, false, false)
			hello, protocol, tlsVersion, alpn := probeNativeHelperTLS(t, helper, customCA)
			if !official.Passed || len(official.Hellos) == 0 {
				t.Fatal("official CLI comparison sample did not complete")
			}
			left, right := official.Hellos[0], hello
			left.Extensions = slices.Clone(left.Extensions)
			right.Extensions = slices.Clone(right.Extensions)
			slices.Sort(left.Extensions)
			slices.Sort(right.Extensions)
			// Randomized extension order and key bytes are not device identity.
			matches := reflect.DeepEqual(left, right)
			negotiated := !customCA
			if customCA && len(official.Requests) > 0 {
				request := official.Requests[0]
				negotiated = request.HTTPProtocol == protocol && request.TLSVersion == tlsVersion && request.ALPN == alpn
			}
			cases = append(cases, map[string]any{
				"name": name, "official": official, "helper_client_hello": hello,
				"hello_fields_match_ignoring_extension_order": matches,
				"helper_http_protocol":                        protocol, "helper_tls_version": tlsVersion, "helper_alpn": alpn,
				"negotiated_protocols_match": negotiated,
			})
			if !matches || !negotiated {
				t.Error("observed native transport differs; inspect sanitized comparison report")
			}
		})
	}
	report := map[string]any{
		"schema": "native-helper-tls-comparison-v1", "passed": !t.Failed(),
		"os": runtime.GOOS, "arch": runtime.GOARCH, "helper_sha256": hex.EncodeToString(sum[:]),
		"controlled_loopback_only": true, "live_model_inference": false, "cases": cases,
		"limits": []string{
			"Two loopback TLS endpoints use the same certificate fixture generation and IP target kind; no production SNI or proxy.",
			"Default trust is a first-ClientHello observation only, not a successfully authenticated TLS connection.",
			"Custom CA selects rustls in both clients; it does not verify default native certificate trust.",
			"Compared parsed ClientHello metadata and negotiated protocols, not TLS random bytes, HTTP/2 SETTINGS, or full CLI behavior.",
			"One fresh handshake sample per mode; no TLS session resumption or cross-platform identity claim.",
		},
	}
	if path := os.Getenv("CODEX_NATIVE_HELPER_REPORT"); path != "" {
		encoded, err := json.MarshalIndent(report, "", "  ")
		if err != nil || os.WriteFile(path, append(encoded, '\n'), 0600) != nil {
			t.Fatal("cannot write sanitized helper comparison")
		}
	}
}

func probeNativeHelperTLS(t *testing.T, helper string, customCA bool) (nativeCaptureHello, string, string, string) {
	t.Helper()
	cert, _, caFile := nativeCaptureCertificate(t)
	var mu sync.Mutex
	var hello nativeCaptureHello
	var protocol, tlsVersion, alpn string
	seen := make(chan struct{})
	var once sync.Once
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		protocol, tlsVersion, alpn = r.Proto, tls.VersionName(r.TLS.Version), r.TLS.NegotiatedProtocol
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"synthetic\",\"status\":\"completed\"}}\n\n")
	}))
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.EnableHTTP2 = true
	server.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	server.Listener = &nativeCaptureListener{Listener: server.Listener, onHello: func(value nativeCaptureHello) {
		once.Do(func() {
			mu.Lock()
			hello = value
			mu.Unlock()
			close(seen)
		})
	}}
	server.StartTLS()
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 35*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, helper)
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		switch strings.ToUpper(key) {
		case "SSL_CERT_FILE", "SSL_CERT_DIR", "CODEX_CA_CERTIFICATE", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "RUST_LOG":
			continue
		}
		cmd.Env = append(cmd.Env, item)
	}
	if customCA {
		cmd.Env = append(cmd.Env, "SSL_CERT_FILE="+caFile)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal("helper stdin unavailable")
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal("helper stdout unavailable")
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal("helper failed to start")
	}
	defer func() { _ = stdin.Close(); cancel(); _ = cmd.Wait() }()
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		t.Fatal("test token unavailable")
	}
	secret := hex.EncodeToString(token)
	write := func(w io.Writer, object any) {
		encoded, err := json.Marshal(object)
		if err != nil || binary.Write(w, binary.BigEndian, uint32(len(encoded))) != nil {
			t.Fatal("helper IPC header failed")
		}
		if _, err := w.Write(encoded); err != nil {
			t.Fatal("helper IPC write failed")
		}
	}
	write(stdin, map[string]any{"protocol": 1, "listen": "127.0.0.1:0", "upstream": server.URL,
		"ipc_token": secret, "proxy": nil, "headers_timeout_ms": 15000, "idle_timeout_ms": 15000})
	line, err := bufio.NewReader(stdout).ReadBytes('\n')
	var ready struct {
		Listen string `json:"listen"`
	}
	if err != nil || len(line) > 4096 || json.Unmarshal(line, &ready) != nil {
		t.Fatal("helper readiness failed")
	}
	host, _, err := net.SplitHostPort(ready.Listen)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		t.Fatal("helper non-loopback address")
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", ready.Listen)
	if err != nil {
		t.Fatal("helper IPC dial failed")
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(25 * time.Second))
	write(conn, map[string]any{"protocol": 1, "ipc_token": secret, "method": "POST", "path": "/responses",
		"headers": [][2]string{{"content-type", "YXBwbGljYXRpb24vanNvbg=="}}, "body_length": 2})
	if _, err := io.WriteString(conn, "{}"); err != nil {
		t.Fatal("helper body write failed")
	}
	select {
	case <-seen:
	case <-ctx.Done():
		t.Fatal("helper did not send ClientHello")
	}
	if customCA {
		for total := 0; ; {
			var header [5]byte
			if _, err := io.ReadFull(conn, header[:]); err != nil {
				t.Fatal("helper response ended before END")
			}
			length := binary.BigEndian.Uint32(header[1:])
			if length > 65536 || total > 262144 {
				t.Fatal("helper response exceeds fixture bound")
			}
			if _, err := io.CopyN(io.Discard, conn, int64(length)); err != nil {
				t.Fatal("helper frame incomplete")
			}
			total += int(length)
			if header[0] == 4 {
				t.Fatal("helper TLS exchange failed")
			}
			if header[0] == 3 {
				break
			}
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if hello.ParseError != "" {
		t.Fatal("helper ClientHello parse failed")
	}
	return hello, protocol, tlsVersion, alpn
}
