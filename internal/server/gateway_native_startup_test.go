package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func nativeStartupFixture(t *testing.T, lifecycle, mode, capabilities string) (*gatewayNativePool, **exec.Cmd) {
	t.Helper()
	var command *exec.Cmd
	pool := nativeHTTPFixturePool(t, func(options *gatewayNativeOptions) {
		original := options.command
		options.lifecycle = lifecycle
		options.stop = 3 * time.Second
		options.command = func(path string) *exec.Cmd {
			command = original(path)
			command.Env = append(command.Env, "GATEWAY_NATIVE_FIXTURE_MODE="+mode,
				"GATEWAY_NATIVE_FIXTURE_CAPABILITIES="+capabilities)
			return command
		}
		if mode == "no-ready" {
			options.startup = 150 * time.Millisecond
		}
		if mode == "ignore-eof" {
			options.stop = 150 * time.Millisecond
		}
	})
	return pool, &command
}

func TestGatewayNativeStartupCapabilitiesAndLifecycle(t *testing.T) {
	for _, lifecycle := range []string{"cli_request", "pooled"} {
		for _, scenario := range []struct {
			name, mode, capabilities string
			websocket                bool
			want                     error
		}{
			{name: "http", capabilities: "request_zstd_v1"},
			{name: "http-and-ws", capabilities: "request_zstd_v1,websocket_message_v1", websocket: true},
			{name: "no-compression", capabilities: "websocket_message_v1", want: errGatewayCompressionCapability},
			{name: "no-ws", capabilities: "request_zstd_v1", websocket: true, want: errNativeWSCapability},
			{name: "no-capabilities", want: errGatewayCompressionCapability},
			{name: "unknown-capability", capabilities: "future_v1,request_zstd_v1"},
			{name: "bad-capability", capabilities: "request_zstd_v1,invalid value", want: errGatewayNativeUnavailable},
			{name: "old-lifecycle", mode: "legacy-lifecycle", want: errGatewayNativeLifecycleReady},
			{name: "wrong-lifecycle", mode: "wrong-lifecycle", want: errGatewayNativeLifecycleReady},
			{name: "bad-protocol", mode: "bad-ready", want: errGatewayNativeUnavailable},
			{name: "legacy-concurrency", mode: "legacy-concurrency", want: errGatewayNativeConcurrencyReady},
			{name: "missing-concurrency", mode: "missing-concurrency", want: errGatewayNativeConcurrencyReady},
			{name: "no-ready", mode: "no-ready", want: errGatewayNativeUnavailable},
		} {
			t.Run(lifecycle+"/"+scenario.name, func(t *testing.T) {
				pool, command := nativeStartupFixture(t, lifecycle, scenario.mode, scenario.capabilities)
				if err := pool.validateStartup(t.Context(), scenario.websocket); !errors.Is(err, scenario.want) {
					t.Fatalf("startup error = %v; want %v", err, scenario.want)
				}
				if *command == nil || (*command).ProcessState == nil {
					t.Fatal("startup returned without collecting its disposable helper")
				}
				pool.mu.Lock()
				entries := len(pool.entries)
				pool.mu.Unlock()
				if entries != 0 {
					t.Fatal("startup left a process reservation in the routing pool")
				}
			})
		}
	}
}

func TestGatewayNativeStartupRequiresCleanExit(t *testing.T) {
	for _, mode := range []string{"bad-exit", "ignore-eof"} {
		t.Run(mode, func(t *testing.T) {
			pool, command := nativeStartupFixture(t, "cli_request", mode, "request_zstd_v1")
			if err := pool.validateStartup(t.Context(), false); !errors.Is(err, errGatewayNativeUnavailable) {
				t.Fatalf("unhealthy helper shutdown accepted: %v", err)
			}
			if *command == nil || (*command).ProcessState == nil || (*command).ProcessState.Success() {
				t.Fatal("helper was not reaped after unsuccessful shutdown")
			}
		})
	}
}

func TestGatewayNativeStartupCanceledBeforeSpawn(t *testing.T) {
	pool, command := nativeStartupFixture(t, "cli_request", "", "request_zstd_v1")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := pool.validateStartup(ctx, false); !errors.Is(err, context.Canceled) || *command != nil {
		t.Fatal("canceled startup spawned a helper or lost cancellation")
	}
}

func TestGatewayNativeStartupDoesNotConsumeServingPool(t *testing.T) {
	pool, command := nativeStartupFixture(t, "cli_request", "", "request_zstd_v1")
	if err := pool.validateStartup(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	probe := *command
	response := nativeHTTPFixtureRequest(t, pool.RoundTripper(nil, time.Second), t.Context(), http.MethodGet, "http://synthetic.invalid/echo", "")
	_ = response.Body.Close()
	if probe == *command || !probe.ProcessState.Success() {
		t.Fatal("serving request reused the disposable startup process")
	}
}

func TestGatewayNativeStartupReapsUnreadyUnixSocket(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("production native IPC requires Linux")
	}
	for _, mode := range []string{"bad-ready", "bound-no-ready"} {
		t.Run(mode, func(t *testing.T) {
			dir := nativeStartupTempDirectory(t)
			t.Setenv("TMPDIR", dir)
			pool, command := nativeStartupFixture(t, "cli_request", mode, "request_zstd_v1")
			pool.options.network = "unix"
			pool.options.startup = 250 * time.Millisecond
			if err := pool.validateStartup(t.Context(), false); !errors.Is(err, errGatewayNativeUnavailable) {
				t.Fatalf("unready helper accepted: %v", err)
			}
			if *command == nil || (*command).ProcessState == nil {
				t.Fatal("startup returned before collecting the unready helper")
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 0 {
				t.Fatal("unready helper retained its private socket directory")
			}
		})
	}
}

func TestGatewayNativeStartupPreservesReplacedUnixSocket(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("production native IPC requires Linux")
	}
	t.Setenv("TMPDIR", nativeStartupTempDirectory(t))
	pool, _ := nativeStartupFixture(t, "cli_request", "", "request_zstd_v1")
	pool.options.network = "unix"
	child, err := pool.start(t.Context(), gatewayNativeKey{origin: "http://127.0.0.1:1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer child.shutdown(pool.options.stop)
	path := filepath.Join(child.dir, "s")
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	replacement, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	child.shutdown(pool.options.stop)
	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(current, info) {
		t.Fatal("shutdown deleted a replacement socket instead of preserving its recorded identity")
	}
}
