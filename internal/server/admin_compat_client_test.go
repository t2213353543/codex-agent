package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAdminCompatRealLoophubClient(t *testing.T) {
	python, err := exec.LookPath("python")
	if err != nil {
		python, err = exec.LookPath("python3")
	}
	if err != nil {
		t.Skip("Python and curl_cffi are required for the actual client contract")
	}
	source := os.Getenv("CODEX_TEST_LOOPHUB_EXPORTER")
	if source == "" {
		source = filepath.Join(filepath.Dir(projectRoot(t)), "loophub-production-maintenance", "webui", "exporter.py")
	}
	if _, err := os.Stat(source); err != nil {
		t.Skip("set CODEX_TEST_LOOPHUB_EXPORTER to the actual Loophub webui/exporter.py")
	}
	if err := exec.Command(python, "-c", "import curl_cffi").Run(); err != nil {
		t.Skip("curl_cffi is required to exercise the actual exporter HTTP and multipart implementation")
	}
	p, handler := webTestSetup(t)
	group, err := p.Groups.Create("real-client-contract", "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	settings, err := p.Settings.Latest()
	if err != nil {
		t.Fatal(err)
	}
	settings.WebsocketEnabled = true
	if err := p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
	var inference, privacy atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer eyJ") {
			t.Error("actual client request lost the selected synthetic credentials")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodPatch && (r.URL.Path == "/exporter-privacy" || r.URL.Path == "/backend-api/settings/user"):
			privacy.Add(1)
			if r.URL.Query().Get("feature") != "training_allowed" || r.URL.Query().Get("value") != "false" {
				t.Error("privacy control did not disable upstream training")
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"training_allowed":false}`)
		case r.Method == http.MethodPost && r.URL.Path == "/backend-api/codex/responses":
			inference.Add(1)
			var body map[string]any
			if json.NewDecoder(r.Body).Decode(&body) != nil || body["model"] != "fixture-model" || r.Header.Get("ChatGPT-Account-Id") != "synthetic-client-workspace" {
				t.Error("actual client model test lost its model or account identity")
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":2,\"output_tokens\":1}}}\n\n")
		default:
			t.Errorf("unexpected synthetic upstream request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()
	p.BaseURL = upstream.URL + "/backend-api/codex"
	var mu sync.Mutex
	methods := make(map[string]int)
	application := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		methods[r.Method]++
		mu.Unlock()
		handler.ServeHTTP(w, r)
	}))
	defer application.Close()
	params, _ := json.Marshal(map[string]any{"base": application.URL, "upstream": upstream.URL, "key": webTestKey, "group_id": compatNumericID(group.ID), "source": source})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, python, filepath.Join(projectRoot(t), "tools", "admin_compat_client_contract.py"))
	command.Stdin = bytes.NewReader(params)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("real exporter contract failed: %v\n%s", err, output)
	}
	var result struct {
		OK           bool     `json:"ok"`
		SourceSHA256 string   `json:"source_sha256"`
		Flows        []string `json:"flows"`
	}
	if json.Unmarshal(output, &result) != nil || !result.OK || len(result.SourceSHA256) != 64 || len(result.Flows) != 11 {
		t.Fatalf("invalid actual client evidence: %s", output)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete} {
		if methods[method] == 0 {
			t.Fatalf("actual client never exercised %s", method)
		}
	}
	if inference.Load() != 1 || privacy.Load() < 4 {
		t.Fatalf("upstream effects were not real: inference=%d privacy=%d", inference.Load(), privacy.Load())
	}
	_, accounts, err := p.Store.List()
	if err != nil || len(accounts) != 1 || accounts[0].Name != "test" {
		t.Fatal("cross-protocol deletion changed the unrelated fixture account")
	}
	t.Logf("actual exporter SHA256=%s; flows=%d; HTTP methods=%v; upstream inference=%d privacy=%d", result.SourceSHA256, len(result.Flows), methods, inference.Load(), privacy.Load())
}
