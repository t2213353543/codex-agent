package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/coder/websocket"
)

func TestGatewayAttemptHooksActual401RefreshAndBytes(t *testing.T) {
	const terminal = "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"done\",\"model\":\"returned-model\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":8,\"output_tokens\":1}}}\n\n"
	var requests, refreshes atomic.Int32
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" {
			refreshes.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			_, _ = io.WriteString(w, gatewayKnownOwnerRefreshFixture(`{"access_token":"synthetic-rotated","refresh_token":"synthetic-rt"}`, "test-account", "fixture-user"))
			return
		}
		if requests.Add(1) == 1 {
			w.WriteHeader(401)
			return
		}
		_, _ = io.WriteString(w, terminal)
	})
	w := httptest.NewRecorder()
	p.ServeHTTP(w, proxyRequest("POST", "/v1/responses?private=PRIVATE_QUERY", `{"model":"sent-model","stream":true,"input":"PRIVATE_PROMPT"}`))
	if w.Code != 200 || w.Body.String() != terminal || requests.Load() != 2 || refreshes.Load() != 1 {
		t.Fatal("attempt observation changed dispatch or response bytes")
	}
	rec, ok := p.Metrics.LastFor("test")
	if !ok || rec.SentModel != "sent-model" || rec.ReturnedModel != "returned-model" || rec.Model != "sent-model" || rec.InboundPath != "/responses" || rec.UpstreamPath != "/responses" {
		t.Fatalf("incorrect observation sources: sent=%q returned=%q inbound=%q", rec.SentModel, rec.ReturnedModel, rec.InboundPath)
	}
	if len(rec.Attempts) != 3 || rec.AttemptsTruncated == nil || *rec.AttemptsTruncated {
		t.Fatal("401 actual request/refresh/request observations missing")
	}
	for i, want := range []struct {
		phase, endpoint string
		status          int
	}{{"request", "/responses", 401}, {"refresh", "/oauth/token", 200}, {"request", "/responses", 200}} {
		a := rec.Attempts[i]
		if a.Attempt != i+1 || a.Phase != want.phase || a.Endpoint != want.endpoint || a.StatusCode == nil || *a.StatusCode != want.status || a.DurationMs == nil || a.HeaderMs == nil {
			t.Fatalf("attempt %d not sourced from actual send", i)
		}
	}
	if rec.RequestPreparationMs == nil || rec.AccountSelectionMs == nil || rec.ProxySelectionMs == nil || rec.ObservationSchemaVersion != 1 || rec.UsageSchemaSource != "responses_usage" {
		t.Fatal("observed stages/schema missing")
	}
	raw, _ := json.Marshal(rec)
	if strings.Contains(string(raw), "PRIVATE_") || strings.Contains(string(raw), "synthetic-rt") {
		t.Fatal("attempt metadata retained request content or credentials")
	}
}

func TestGatewayAttemptHooksNoDispatchAndTruncation(t *testing.T) {
	for _, oversized := range []bool{false, true} {
		t.Run(fmt.Sprint(oversized), func(t *testing.T) {
			prefix := `{"detail":{},"padding":"`
			body := prefix + strings.Repeat("x", (64<<10)-len(prefix)-2) + `"}`
			if oversized {
				body += "extra"
			}
			p := proxyFixture(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(429); _, _ = io.WriteString(w, body) })
			w := httptest.NewRecorder()
			p.ServeHTTP(w, proxyRequest("POST", "/responses", `{"model":"model","input":[]}`))
			if w.Body.String() != body || w.Code != 429 {
				t.Fatal("diagnostic observer changed error passthrough")
			}
			rec, _ := p.Metrics.LastFor("test")
			if rec.ErrorTruncated == nil || *rec.ErrorTruncated != oversized || len(rec.Attempts) != 1 {
				t.Fatal("diagnostic bound not based on observed bytes")
			}
		})
	}
	p := newTestProxy(t, 1)
	w := httptest.NewRecorder()
	p.ServeHTTP(w, proxyRequest("POST", "/responses", `invalid`))
	rec, _ := p.Metrics.LastFor("test")
	if len(rec.Attempts) != 0 || rec.AttemptsTruncated != nil || rec.SentModel != "" || rec.RequestPreparationMs != nil {
		t.Fatal("rejected input invented dispatch/stage metadata")
	}
}

func TestGatewayAttemptHooksWebSocketFirstHandshakeOnly(t *testing.T) {
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		for i := 0; i < 2; i++ {
			if _, _, err = c.Read(r.Context()); err != nil {
				return
			}
			_ = c.Write(r.Context(), websocket.MessageText, []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":"attempt-%d","model":"returned","status":"completed","output":[]}}`, i)))
		}
		_, _, _ = c.Read(r.Context())
	})
	enableGatewayWS(t, p, "")
	server := httptest.NewServer(p)
	defer server.Close()
	c, _ := gatewayWSDial(t, server.URL, "attempt-session")
	for range 2 {
		if err := c.Write(t.Context(), websocket.MessageText, []byte(`{"type":"response.create","model":"sent","input":[]}`)); err != nil {
			t.Fatal(err)
		}
		gatewayWSRead(t, c)
	}
	_ = c.CloseNow()
	waitGatewayWSReleased(t, p)
	records := p.Metrics.RecentForAccount("test", 10)
	if len(records) != 2 {
		t.Fatal("WS record cardinality changed")
	}
	count := 0
	for _, r := range records {
		if r.SentModel != "sent" || r.ReturnedModel != "returned" || r.UpstreamPath != "/responses" {
			t.Fatal("WS sources missing")
		}
		for _, a := range r.Attempts {
			if a.Phase != "websocket" || a.StatusCode == nil || *a.StatusCode != 101 {
				t.Fatal("WS handshake evidence incorrect")
			}
			count++
		}
	}
	if count != 1 {
		t.Fatal("WS handshake attempts repeated across turns")
	}
}
