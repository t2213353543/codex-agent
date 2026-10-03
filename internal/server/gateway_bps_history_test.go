package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestBPSIngressFailuresPreserveManagementObservation(t *testing.T) {
	p, h := webTestSetup(t)
	p.BPS = &bpsRuntime{}
	if err := p.startGatewayIngressEvents(); err != nil {
		t.Fatal(err)
	}
	res := webTestRequest(h, http.MethodPost, "/v1/responses", `{"model":"gpt-test","input":"private"}`, "invalid-key")
	if res.Code != 401 {
		t.Fatalf("unauthorized request returned %d", res.Code)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := p.IngressEvents.Close(ctx); err != nil {
		t.Fatal(err)
	}
	page := managedTestRequest(t, h, "GET", "/api/ingress-events", nil, 200)
	var got ingressPageFixture
	if json.Unmarshal(page.Body.Bytes(), &got) != nil || len(got.Items) != 1 || got.Items[0].Reason != "invalid_api_key" {
		t.Fatal("BPS authorization failure missing management ingress observation")
	}
	if strings.Contains(page.Body.String(), "invalid-key") || strings.Contains(page.Body.String(), "private") {
		t.Fatal("ingress leaked request content")
	}
}

func TestBPSHistoryKeyAndOwnerIsolation(t *testing.T) {
	p := &Proxy{BPS: &bpsRuntime{}}
	ctx := context.Background()
	record := bpsHistoryRecord{Owner: "account-a:revision-1", Session: "thread-a", Request: json.RawMessage(`{"model":"gpt-test","input":[{"role":"user","content":"remember A"}],"instructions":"original"}`), Output: []json.RawMessage{json.RawMessage(`{"role":"assistant","type":"message","content":[{"type":"output_text","text":"A"}]}`)}}
	if err := p.bpsSaveHistory(ctx, "key-a", "resp-a", record); err != nil {
		t.Fatal(err)
	}
	if _, err := p.bpsLoadHistory(ctx, "key-b", "resp-a"); !errors.Is(err, errBPSHistoryMissing) {
		t.Fatalf("other key loaded history: %v", err)
	}
	raw, session, owner, err := p.bpsExpandHistory(ctx, "key-a", []byte(`{"model":"gpt-next","previous_response_id":"resp-a","input":[{"role":"user","content":"next"}]}`), "")
	if err != nil {
		t.Fatal(err)
	}
	if session != "thread-a" || owner != "account-a:revision-1" {
		t.Fatalf("lost owner/session: %s %s", session, owner)
	}
	var got map[string]json.RawMessage
	_ = json.Unmarshal(raw, &got)
	var input []json.RawMessage
	_ = json.Unmarshal(got["input"], &input)
	if len(input) != 3 || string(got["model"]) != `"gpt-next"` || string(got["instructions"]) != `"original"` {
		t.Fatalf("incorrect continuation: %s", raw)
	}
	if _, ok := got["previous_response_id"]; ok {
		t.Fatal("upstream incremental field retained")
	}
	if _, _, _, err := p.bpsExpandHistory(ctx, "key-a", []byte(`{"previous_response_id":"resp-a","input":[]}`), "thread-b"); err == nil {
		t.Fatal("cross-thread continuation accepted")
	}
}

func TestBPSRuntimeHasNoCodexInferenceFallback(t *testing.T) {
	values := map[string]string{}
	get := func(k string) string { return values[k] }
	runtime, base, err := newBPSRuntime(get)
	if err != nil || runtime == nil || base != "https://bps.openai.com/basispoints/api" {
		t.Fatalf("invalid production defaults: %s %v", base, err)
	}
	for _, bad := range []string{"http://example.com/api", "https://user:secret@example.com/api", "https://example.com/api?token=x"} {
		values["BPS_BASE_URL"] = bad
		if _, _, err := newBPSRuntime(get); err == nil {
			t.Errorf("invalid base accepted: %s", bad)
		}
	}
	values["BPS_BASE_URL"] = "http://127.0.0.1:10000/api"
	values["BPS_TOOLS_VERSION"] = "tools-excel-core-any"
	if _, _, err := newBPSRuntime(get); err == nil {
		t.Fatal("wrong profile accepted")
	}
	delete(values, "BPS_TOOLS_VERSION")
	values["BPS_WEB_SEARCH"] = "disabled"
	runtime, _, err = newBPSRuntime(get)
	if err != nil || !runtime.SearchDisabled {
		t.Fatalf("search policy ignored: %v", err)
	}
}

func TestBPSHistoryMissingAndMalformedDoNotSelectAccount(t *testing.T) {
	p := &Proxy{BPS: &bpsRuntime{}}
	for _, raw := range []string{`null`, `[]`, `{"previous_response_id":12}`, `{"previous_response_id":"missing"}`} {
		if _, _, _, err := p.bpsExpandHistory(context.Background(), "key", []byte(raw), ""); err == nil {
			t.Errorf("bad history accepted %s", raw)
		}
	}
	if bpsHistoryKey("a", "b") == bpsHistoryKey("b", "a") || strings.Contains(bpsHistoryKey("secret-key", "private-response"), "private") {
		t.Fatal("history key scope not hashed")
	}
}
