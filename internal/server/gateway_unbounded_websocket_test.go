package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/coder/websocket"
)

func newLightweightUnboundedWSSession(t *testing.T, concurrency int) *gatewayWSSessionAudit {
	t.Helper()
	accepted := make(chan *websocket.Conn, 1)
	requests := make(chan []byte, 1)
	closed := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer close(closed)
		defer conn.CloseNow()
		accepted <- conn
		for {
			_, payload, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			select {
			case requests <- payload:
			case <-t.Context().Done():
				return
			}
		}
	}))
	t.Cleanup(upstream.Close)
	p := newTestProxy(t, concurrency)
	p.RuntimeModes, p.AppServerCLI = true, &appServerCLI{}
	enableGatewayWS(t, p, "")
	settings, _ := p.Settings.Get()
	settings.InferenceMode = inferenceCodexCLI
	settings.AutomaticTokenRefreshEnabled = false
	if err := p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
	target, _ := url.Parse(upstream.URL)
	p.Client = &http.Client{Transport: appServerFailoverRoundTripper(func(req *http.Request) (*http.Response, error) {
		if !gatewayUnboundedPayload(req.Context()) {
			t.Error("WebSocket dispatch lost the lightweight payload policy")
		}
		clone := req.Clone(req.Context())
		clone.URL.Scheme, clone.URL.Host = target.Scheme, target.Host
		return http.DefaultTransport.RoundTrip(clone)
	})}
	server := httptest.NewServer(p)
	t.Cleanup(server.Close)
	client, _ := gatewayWSDial(t, server.URL, "")
	return &gatewayWSSessionAudit{p: p, client: client, accepted: accepted, requests: requests, closed: closed}
}

func TestAppServerUnboundedWSLongIdentifiersAndEarlierBranch(t *testing.T) {
	s := newLightweightUnboundedWSSession(t, 1)
	longSession := strings.Repeat("session", 600)
	longResponse := strings.Repeat("response", 150)
	longModel := strings.Repeat("model", 110)
	longStream := strings.Repeat("lane", 70)
	var firstCache string
	for i := 0; i < 258; i++ {
		// Each completed turn releases the single account slot. More than 128
		// distinct sessions on one socket is not inference concurrency.
		session := fmt.Sprintf("session-%d", i)
		id := fmt.Sprintf("response-%d", i)
		if i == 0 {
			session, id = longSession, longResponse
		}
		body, _ := json.Marshal(map[string]any{"type": "response.create", "model": longModel, "stream_id": longStream, "prompt_cache_key": session, "client_metadata": map[string]string{"session_id": session}, "input": []any{}})
		s.send(t, string(body))
		var forwarded struct {
			Model    string `json:"model"`
			StreamID string `json:"stream_id"`
			Cache    string `json:"prompt_cache_key"`
		}
		if err := json.Unmarshal(s.forwarded(t), &forwarded); err != nil {
			t.Fatal(err)
		}
		if forwarded.Model != longModel || forwarded.StreamID != longStream || forwarded.Cache == session {
			t.Fatal("long model or per-account cache isolation was changed")
		}
		if i == 0 {
			firstCache = forwarded.Cache
		}
		event, _ := json.Marshal(map[string]any{"type": "response.completed", "stream_id": longStream, "response": map[string]any{"id": id, "status": "completed", "output": []any{}, "usage": map[string]int{"input_tokens": 7, "output_tokens": 1}}})
		s.event(t, string(event))
	}
	// Returning to the first branch must retain its original cache policy even
	// after the former 256-entry FIFO window, and must accept the long raw ID.
	resume, _ := json.Marshal(map[string]any{"type": "response.create", "model": longModel, "stream_id": longStream, "prompt_cache_key": longSession, "client_metadata": map[string]string{"session_id": longSession}, "previous_response_id": longResponse, "input": []any{}})
	s.send(t, string(resume))
	var resumed struct {
		Previous string `json:"previous_response_id"`
		Cache    string `json:"prompt_cache_key"`
	}
	if err := json.Unmarshal(s.forwarded(t), &resumed); err != nil || resumed.Previous != longResponse || resumed.Cache != firstCache {
		t.Fatalf("earlier branch lost response or cache identity: %v", err)
	}
	event, _ := json.Marshal(map[string]any{"type": "response.completed", "stream_id": longStream, "response": map[string]any{"id": "resumed", "status": "completed", "output": []any{}}})
	s.event(t, string(event))
	waitGatewayWSReleased(t, s.p)
	for _, rec := range s.p.Metrics.RecentForAccount("test", 20) {
		if rec.Status != "ok" {
			t.Fatalf("long-lived connection recorded failure: %s", rec.Error)
		}
	}
}

func TestAppServerUnboundedWSPendingStillEnforcesAccountConcurrency(t *testing.T) {
	const concurrency = 130
	s := newLightweightUnboundedWSSession(t, concurrency)
	for i := 0; i < concurrency; i++ {
		s.send(t, fmt.Sprintf(`{"type":"response.create","model":"fixture","stream_id":"lane_%d","input":[]}`, i))
		s.forwarded(t)
	}
	if s.p.inFlight("test") != concurrency {
		t.Fatalf("unexpected account admission count: %d", s.p.inFlight("test"))
	}
	s.send(t, `{"type":"response.create","model":"fixture","stream_id":"excess","input":[]}`)
	if raw := string(gatewayWSRead(t, s.client)); !strings.Contains(raw, `"status":429`) {
		t.Fatalf("account concurrency was removed: %s", raw)
	}
	select {
	case <-s.requests:
		t.Fatal("request exceeding account concurrency reached upstream")
	default:
	}
	for i := 0; i < concurrency; i++ {
		s.event(t, fmt.Sprintf(`{"type":"response.completed","stream_id":"lane_%d","response":{"id":"resp_%d","status":"completed","output":[]}}`, i, i))
	}
	waitGatewayWSReleased(t, s.p)
	// The account becomes usable again without reconnecting or losing lanes.
	s.send(t, `{"type":"response.create","model":"fixture","stream_id":"after","input":[]}`)
	s.forwarded(t)
	s.event(t, `{"type":"response.completed","stream_id":"after","response":{"id":"resp_after","status":"completed","output":[]}}`)
	waitGatewayWSReleased(t, s.p)
}
