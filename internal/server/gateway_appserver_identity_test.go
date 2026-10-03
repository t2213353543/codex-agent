package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestAppServerIdentitySourceAndHeaderBoundary(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "official-fixture")
	content := []byte("offline-binary-fixture")
	if err := os.WriteFile(binary, content, 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	identity := gatewayRuntimeIdentity{Schema: "codex-runtime-identity-v1", CLIPath: binary, SourceCLISHA256: hex.EncodeToString(sum[:]), CLIVersion: clientVersion, Originator: appServerClientOriginator, UserAgent: appServerClientOriginator + "/" + clientVersion + " (Synthetic OS 1.0; x86_64) unknown (codex_agent_gateway; 1.0)", PlatformOS: runtime.GOOS, PlatformArch: runtime.GOARCH}
	raw, _ := json.Marshal(identity)
	file := filepath.Join(dir, "identity.json")
	_ = os.WriteFile(file, raw, 0600)
	a := &appServerCLI{}
	if err := a.configureIdentity(func(k string) string {
		if k == appServerIdentityEnv {
			return file
		}
		return ""
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := parseGatewayRuntimeIdentity(raw); err == nil {
		t.Fatal("App Server identity silently accepted as legacy CLI identity")
	}
	h := http.Header{"user-agent": []string{"old"}, "Originator": []string{"codex_exec"}, "version": []string{"old"}, "x-oai-attestation": []string{"old-proof"}, "Authorization": []string{"Bearer synthetic"}, "Session-Id": []string{"client-session"}, "X-Codex-Turn-State": []string{"opaque-turn"}}
	a.applyIdentity(h)
	if h.Get("User-Agent") != identity.UserAgent || h.Get("Originator") != appServerClientOriginator || h.Get("Version") != clientVersion || h.Get("X-Oai-Attestation") != "" {
		t.Fatal("identity header mismatch")
	}
	if h.Get("Authorization") != "Bearer synthetic" || h.Get("Session-Id") != "client-session" || h.Get("X-Codex-Turn-State") != "opaque-turn" {
		t.Fatal("identity override modified unrelated credentials/state")
	}
	for name := range h {
		if strings.EqualFold(name, "User-Agent") && name != "User-Agent" {
			t.Fatal("duplicate wire identity")
		}
	}
	_ = os.WriteFile(binary, []byte("changed-binary"), 0600)
	if err := (&appServerCLI{}).configureIdentity(func(string) string { return file }); err == nil {
		t.Fatal("changed official binary accepted")
	}
	if a.identity.UserAgent != identity.UserAgent {
		t.Fatal("immutable snapshot changed after disk mutation")
	}
}
