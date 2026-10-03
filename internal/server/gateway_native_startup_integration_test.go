package server

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// This fixture supplies only startup configuration. It never creates a gateway
// request, account, inference payload, target connection or proxy connection.
func TestGatewayNativeActualStartupWithoutUpstreamRequests(t *testing.T) {
	bin := os.Getenv("CODEX_NATIVE_HTTP_TEST_BIN")
	if bin == "" || runtime.GOOS != "linux" {
		t.Skip("set CODEX_NATIVE_HTTP_TEST_BIN on Linux for sealed-helper startup validation")
	}
	raw, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal("startup fixture helper unavailable")
	}
	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])
	for _, lifecycle := range []string{"cli_request", "pooled"} {
		for _, websocket := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/ws=%v", lifecycle, websocket), func(t *testing.T) {
				private := nativeStartupTempDirectory(t)
				t.Setenv("TMPDIR", private)
				for _, key := range []string{"CODEX_CA_CERTIFICATE", "SSL_CERT_FILE", "SSL_CERT_DIR"} {
					t.Setenv(key, "")
				}
				before := nativeStartupChildProcesses(t)
				p := &Proxy{}
				started := time.Now()
				if err := configureGatewayNativeHTTP(p, func(key string) string {
					switch key {
					case "CODEX_NATIVE_HTTP_HELPER":
						return bin
					case "CODEX_NATIVE_HTTP_SHA256":
						return digest
					case "CODEX_NATIVE_HTTP_LIFECYCLE":
						return lifecycle
					case "CODEX_NATIVE_WEBSOCKET":
						if websocket {
							return "1"
						}
					}
					return ""
				}); err != nil {
					t.Fatal("valid native helper did not pass local startup validation")
				}
				if p.NativeHTTP == nil || p.NativeWebSocket != websocket {
					t.Fatal("validated startup mode was not attached")
				}
				pool := p.NativeHTTP
				defer pool.Close()
				pool.mu.Lock()
				entries := len(pool.entries)
				pool.mu.Unlock()
				if entries != 0 {
					t.Fatal("startup validation left an admitted route or helper process in the pool")
				}
				if time.Since(started) > 15*time.Second {
					t.Fatal("local helper startup exceeded its bounded completion window")
				}
				nativeStartupAssertClean(t, private, before)
				if err := pool.Close(); err != nil {
					t.Fatal("validated empty pool did not close")
				}
				select {
				case <-pool.done:
				default:
					t.Fatal("startup pool reaper survived explicit close")
				}
			})
		}
	}
	for _, caVariable := range []string{"CODEX_CA_CERTIFICATE", "SSL_CERT_FILE"} {
		t.Run("invalid_ca/"+caVariable, func(t *testing.T) {
			private := nativeStartupTempDirectory(t)
			t.Setenv("TMPDIR", private)
			for _, key := range []string{"CODEX_CA_CERTIFICATE", "SSL_CERT_FILE", "SSL_CERT_DIR"} {
				t.Setenv(key, "")
			}
			t.Setenv(caVariable, filepath.Join(private, "missing-synthetic-ca.pem"))
			before := nativeStartupChildProcesses(t)
			p := &Proxy{}
			if err := configureGatewayNativeHTTP(p, func(key string) string {
				switch key {
				case "CODEX_NATIVE_HTTP_HELPER":
					return bin
				case "CODEX_NATIVE_HTTP_SHA256":
					return digest
				case "CODEX_NATIVE_WEBSOCKET":
					return "1"
				}
				return ""
			}); err == nil {
				if p.NativeHTTP != nil {
					_ = p.NativeHTTP.Close()
				}
				t.Fatal("invalid local CA configuration passed startup validation")
			}
			if p.NativeHTTP != nil || p.NativeWebSocket {
				t.Fatal("failed startup attached a partially usable native pool or WS mode")
			}
			nativeStartupAssertClean(t, private, before)
		})
	}
}

func nativeStartupChildProcesses(t *testing.T) map[string]bool {
	t.Helper()
	paths, err := filepath.Glob("/proc/self/task/*/children")
	if err != nil || len(paths) == 0 {
		t.Fatal("cannot observe local process child lifecycle")
	}
	children := map[string]bool{}
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue // A runtime worker can finish between glob and read.
		}
		if err != nil {
			t.Fatal("cannot observe local process children")
		}
		for _, child := range strings.Fields(string(raw)) {
			children[child] = true
		}
	}
	return children
}

func nativeStartupTempDirectory(t *testing.T) string {
	t.Helper()
	// Unix socket paths include the helper's cnx-* child; keep the parent
	// independent of long Go test names and inherited TMPDIR paths.
	directory, err := os.MkdirTemp("/tmp", "nstart-")
	if err != nil {
		t.Fatal("cannot create short private startup-test directory")
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	return directory
}

func nativeStartupAssertClean(t *testing.T, directory string, before map[string]bool) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 0 {
		t.Fatal("local startup probe left private socket directories or files")
	}
	for child := range nativeStartupChildProcesses(t) {
		if !before[child] {
			t.Fatal("local startup probe left a child process running or unreaped")
		}
	}
}
