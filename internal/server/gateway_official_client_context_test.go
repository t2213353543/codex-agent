package server

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestOfficialClientContextRPCFixture(t *testing.T) {
	capture := ""
	for _, arg := range os.Args {
		if strings.HasPrefix(arg, "--client-context-capture=") {
			capture = strings.TrimPrefix(arg, "--client-context-capture=")
		}
	}
	if capture == "" {
		return
	}
	encoder := json.NewEncoder(os.Stdout)
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var request struct {
			ID     json.RawMessage
			Method string
			Params map[string]json.RawMessage
		}
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			os.Exit(2)
		}
		if len(request.ID) == 0 {
			continue
		}
		result := any(map[string]any{})
		switch request.Method {
		case "initialize", "account/login/start":
		case "model/list":
			_ = os.WriteFile(filepath.Join(os.Getenv("CODEX_HOME"), "models_cache.json"), []byte(`{"models":[{"slug":"model-a","tool_mode":"direct"}]}`), 0600)
			result = map[string]any{"data": []any{map[string]string{"model": "model-a"}}}
		case "thread/start":
			encoded, _ := json.Marshal(request.Params)
			_ = os.WriteFile(capture, encoded, 0600)
			result = map[string]any{"thread": map[string]string{"id": "context-thread"}, "model": "model-a"}
		default:
			os.Exit(3)
		}
		_ = encoder.Encode(map[string]any{"id": request.ID, "result": result})
	}
	os.Exit(0)
}

func TestOfficialClientThreadSuppressesOnlyRuntimeEnvironment(t *testing.T) {
	root := t.TempDir()
	rt, err := newOfficialRuntime(func(name string) string {
		if name == "CODEX_RUNTIME_DIR" {
			return root
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	capture := filepath.Join(root, "thread-start.json")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// The runtime intentionally does not inherit arbitrary variables. Wrap its
	// command with a test process whose flags identify this local fixture.
	rt.command = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, executable, "-test.run=^TestOfficialClientContextRPCFixture$", "--", "--client-context-capture="+capture)
	}
	account := Account{ID: "fixture", AccountID: "fixture-account", AccessToken: "fixture-token"}
	caller := json.RawMessage(`{"role":"user","content":"<environment_context><cwd>/client/workspace</cwd></environment_context> caller task"}`)
	thread, delta, err := rt.responseThread(t.Context(), "key", "session", account, nil, officialResponseInput{Model: "model-a"}, []json.RawMessage{caller})
	if err != nil {
		t.Fatal(err)
	}
	thread.mu.Unlock()
	if len(delta) != 1 || string(delta[0]) != string(caller) {
		t.Fatal("caller-provided environment or input changed")
	}
	raw, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	var params struct {
		CWD    string                     `json:"cwd"`
		Config map[string]json.RawMessage `json:"config"`
	}
	if json.Unmarshal(raw, &params) != nil || string(params.Config["include_environment_context"]) != "false" {
		t.Fatal("bridge still injects server runtime environment")
	}
	if params.CWD != thread.rpc.work {
		t.Fatal("runtime workspace isolation was changed")
	}
	config, err := os.ReadFile(filepath.Join(thread.rpc.home, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(config), "include_environment_context") {
		t.Fatal("thread-only policy leaked into native/hosted process configuration")
	}
}
