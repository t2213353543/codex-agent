package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestManagedAccountTestStream(t *testing.T)   { runManagedAccountTestStream(t, webTestSetup) }
func TestPGManagedAccountTestStream(t *testing.T) { runManagedAccountTestStream(t, adminPGTestProxy) }
func runManagedAccountTestStream(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	p, h := setup(t)
	a := managedTestAccount(t, p, "sse-account", nil)
	var mode atomic.Int32
	var requests atomic.Int32
	finish := make(chan struct{})
	canceled := make(chan struct{}, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		body, _ := io.ReadAll(r.Body)
		if !bytes.Contains(body, []byte("synthetic custom prompt")) || bytes.Contains(body, []byte("max_output_tokens")) {
			t.Error("custom prompt or Codex contract lost")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"incremental\"}\n\n")
		w.(http.Flusher).Flush()
		if mode.Load() == 0 {
			select {
			case <-finish:
				fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":2,\"output_tokens\":1}}}\n\n")
			case <-r.Context().Done():
				canceled <- struct{}{}
			}
		} else {
			<-r.Context().Done()
			canceled <- struct{}{}
		}
	}))
	defer upstream.Close()
	p.BaseURL = upstream.URL + "/backend-api/codex"
	body := map[string]any{"version": a.Version, "model": "fixture-model", "prompt": "synthetic custom prompt", "confirm_inference": true}
	application := httptest.NewServer(h)
	defer application.Close()
	connect := func(ctx context.Context) *http.Response {
		t.Helper()
		raw, _ := json.Marshal(body)
		request, _ := http.NewRequestWithContext(ctx, "POST", application.URL+"/api/accounts/"+a.ID+"/test-stream", bytes.NewReader(raw))
		request.Header.Set("Authorization", "Bearer "+webTestKey)
		request.Header.Set("Content-Type", "application/json")
		response, err := application.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != 200 || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
			raw, _ := io.ReadAll(response.Body)
			response.Body.Close()
			t.Fatalf("SSE unavailable %d %s", response.StatusCode, raw)
		}
		return response
	}
	response := connect(context.Background())
	reader := bufio.NewReader(response.Body)
	observedDelta := false
	for i := 0; i < 20; i++ {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(line, "incremental") {
			observedDelta = true
			break
		}
	}
	if !observedDelta {
		t.Fatal("delta not delivered before upstream terminal event")
	}
	close(finish)
	rest, err := io.ReadAll(reader)
	response.Body.Close()
	if err != nil || !strings.Contains(string(rest), "event: result") || !strings.Contains(string(rest), `"ok":true`) {
		t.Fatalf("missing terminal success %v %s", err, rest)
	}
	before, err := p.managementState().Get(context.Background(), "account-upstream", a.ID)
	if err != nil {
		t.Fatal(err)
	}
	mode.Store(1)
	ctx, cancel := context.WithCancel(context.Background())
	response = connect(ctx)
	reader = bufio.NewReader(response.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(line, "incremental") {
			break
		}
	}
	cancel()
	response.Body.Close()
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("client abort did not cancel upstream")
	}
	after, err := p.managementState().Get(context.Background(), "account-upstream", a.ID)
	if err != nil || after.Version != before.Version {
		t.Fatal("aborted test cached successful result")
	}
	body["confirm_inference"] = false
	managedTestRequest(t, h, "POST", "/api/accounts/"+a.ID+"/test-stream", body, 400)
	if requests.Load() != 2 {
		t.Fatal("unconfirmed management inference ran")
	}
}
