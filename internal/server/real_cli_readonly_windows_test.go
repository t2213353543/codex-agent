package server

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestRealCodexCLIReadOnlyWindowsSandbox checks the official unelevated backend
// with a temporary read-only/never profile. It invokes no elevated setup,
// service installation, account creation, or manual ACL changes. The official
// backend may initialize its normal NUL/private-desktop runtime object ACLs.
func TestRealCodexCLIReadOnlyWindowsSandbox(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows-specific official restricted-token sandbox test")
	}
	bin := os.Getenv("CODEX_REAL_CLI_BIN")
	if bin == "" {
		t.Skip("set CODEX_REAL_CLI_BIN to opt into the official Windows CLI sandbox integration")
	}
	bin, err := filepath.Abs(bin)
	if err != nil {
		t.Fatal(err)
	}
	version, err := exec.Command(bin, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("official CLI version: %v: %s", err, version)
	}
	t.Logf("real client: %s; sandbox: read-only/never, windows.unelevated; upstream: local deterministic fixture", strings.TrimSpace(string(version)))
	runRealCodexConversationWithSandbox(t, bin, false, false, true)
}
