package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestGatewayWSKnownResponseWithoutLaneCompletesItsOwnHead(t *testing.T) {
	for _, kind := range []string{"response.completed", "response.failed"} {
		t.Run(kind, func(t *testing.T) {
			s := newGatewayWSSessionAudit(t, "known-lane", nil)
			enableRateLimitCooldownForTest(t, s.p)
			for _, lane := range []string{"a", "b"} {
				s.send(t, fmt.Sprintf(`{"type":"response.create","model":"model-%s","stream_id":"%s","input":[]}`, lane, lane))
				s.forwarded(t)
				s.event(t, fmt.Sprintf(`{"type":"response.created","stream_id":"%s","response":{"id":"response-%s","status":"in_progress"}}`, lane, lane))
			}
			raw := fmt.Sprintf(`{"type":%q,"response":{"id":"response-a","status":"completed","usage":{"input_tokens":13,"output_tokens":2}}}`, kind)
			if kind == "response.failed" {
				raw = `{"type":"response.failed","response":{"id":"response-a","status":"failed","error":{"code":"rate_limit_exceeded","message":"synthetic"}}}`
			}
			s.event(t, raw)
			if s.p.inFlight("test") != 1 {
				t.Fatalf("known response without redundant stream_id retained or consumed wrong slots: %d", s.p.inFlight("test"))
			}
			account, err := s.p.Store.Get("test")
			if err != nil {
				t.Fatal(err)
			}
			if kind == "response.failed" {
				var cooldown *gatewayCooldownError
				if !errors.As(s.p.checkGatewayCooldown(withGatewayCooldownModel(t.Context(), "model-a"), account), &cooldown) {
					t.Fatal("known lane failure did not set its model cooldown")
				}
				if s.p.checkGatewayCooldown(withGatewayCooldownModel(t.Context(), "model-b"), account) != nil {
					t.Fatal("known lane failure polluted the other model")
				}
			}
			// The first failure is terminal for WS. Late success/failure with the
			// same ID must not consume the remaining lane or rewrite its record.
			s.event(t, raw)
			if s.p.inFlight("test") != 1 {
				t.Fatal("duplicate known terminal released the other lane")
			}
			s.event(t, `{"type":"response.completed","stream_id":"b","response":{"id":"response-b","status":"completed","usage":{"input_tokens":17,"output_tokens":3}}}`)
			waitGatewayWSReleased(t, s.p)
			if got := s.p.Metrics.RecentForAccount("test", 10); len(got) != 2 {
				t.Fatalf("records = %d", len(got))
			} else {
				for _, record := range got {
					if record.AccountID != account.ID || record.AccountCredentialVersion != account.CredentialVersion {
						t.Fatal("response attribution changed account or credential epoch")
					}
					if record.Model == "model-b" && (record.InputTokens != 17 || record.Status != "ok") {
						t.Fatal("foreign lane corrupted normal completion")
					}
				}
			}
		})
	}
}

func TestGatewayWSKnownResponseCannotMoveToExplicitForeignLane(t *testing.T) {
	for _, kind := range []string{"response.completed", "response.failed"} {
		t.Run(kind, func(t *testing.T) {
			s := newGatewayWSSessionAudit(t, "foreign-lane", nil)
			enableRateLimitCooldownForTest(t, s.p)
			s.send(t, `{"type":"response.create","model":"model-a","stream_id":"a","input":[]}`)
			s.forwarded(t)
			s.event(t, `{"type":"response.created","stream_id":"a","response":{"id":"known-a","status":"in_progress"}}`)
			s.send(t, `{"type":"response.create","model":"model-b","stream_id":"b","input":[]}`)
			s.forwarded(t)
			raw := `{"type":"response.completed","stream_id":"b","response":{"id":"known-a","status":"completed","usage":{"input_tokens":999,"output_tokens":1}}}`
			if kind == "response.failed" {
				raw = `{"type":"response.failed","stream_id":"b","response":{"id":"known-a","status":"failed","error":{"code":"rate_limit_exceeded","message":"wrong lane"}}}`
			}
			s.event(t, raw)
			if s.p.inFlight("test") != 2 {
				t.Fatalf("known response moved to explicit foreign lane: %d", s.p.inFlight("test"))
			}
			account, _ := s.p.Store.Get("test")
			if s.p.checkGatewayCooldown(withGatewayCooldownModel(t.Context(), "model-b"), account) != nil {
				t.Fatal("foreign identity applied model-b cooldown")
			}
			s.event(t, `{"type":"response.completed","response":{"id":"known-a","status":"completed"}}`)
			s.event(t, `{"type":"response.completed","stream_id":"b","response":{"id":"known-b","status":"completed"}}`)
			waitGatewayWSReleased(t, s.p)
		})
	}
}

func TestGatewayWSKnownResponseUnknownIDsAndConnectionClose(t *testing.T) {
	s := newGatewayWSSessionAudit(t, "unknown-lane", nil)
	enableRateLimitCooldownForTest(t, s.p)
	s.send(t, `{"type":"response.create","model":"first","stream_id":"a","input":[]}`)
	s.forwarded(t)
	s.event(t, `{"type":"response.created","stream_id":"a","response":{"id":"known-first","status":"in_progress"}}`)
	s.send(t, `{"type":"response.create","model":"queued","stream_id":"a","input":[]}`)
	s.forwarded(t)
	for _, raw := range []string{
		`{"type":"response.completed","response":{"id":"never-observed","status":"completed","usage":{"input_tokens":999,"output_tokens":1}}}`,
		`{"type":"response.failed","stream_id":"unknown","response":{"id":"known-first","status":"failed","error":{"code":"rate_limit_exceeded"}}}`,
	} {
		s.event(t, raw)
	}
	if s.p.inFlight("test") != 2 || len(s.p.Metrics.RecentForAccount("test", 10)) != 0 {
		t.Fatal("unknown/explicit wrong lane consumed current or queued turn")
	}
	s.event(t, `{"type":"response.completed","response":{"id":"known-first","status":"completed","usage":{"input_tokens":11,"output_tokens":1}}}`)
	s.event(t, `{"type":"response.failed","response":{"id":"known-first","error":{"code":"rate_limit_exceeded"}}}`)
	if s.p.inFlight("test") != 1 {
		t.Fatal("late terminal consumed promoted queued request")
	}
	_ = s.client.CloseNow()
	waitGatewayWSReleased(t, s.p)
	records := s.p.Metrics.RecentForAccount("test", 10)
	if len(records) != 2 {
		t.Fatalf("connection close records = %d", len(records))
	}
	for _, record := range records {
		if record.Model == "first" && (record.Status != "ok" || record.InputTokens != 11) {
			t.Fatal("completed head changed on close")
		}
		if record.Model == "queued" && (record.Status != "aborted" || record.InputTokens != 0) {
			t.Fatal("queued request inherited old response on close")
		}
	}
}

func TestGatewayWSKnownDefaultLaneErrorDoesNotAbortOtherLanes(t *testing.T) {
	for _, laneMember := range []string{"", `,"stream_id":""`} {
		t.Run(fmt.Sprintf("explicit_empty=%t", laneMember != ""), func(t *testing.T) {
			s := newGatewayWSSessionAudit(t, "known-default-error", nil)
			enableRateLimitCooldownForTest(t, s.p)
			s.send(t, `{"type":"response.create","model":"default-first","input":[]}`)
			s.forwarded(t)
			s.event(t, `{"type":"response.created","response":{"id":"known-default","status":"in_progress"}}`)
			s.send(t, `{"type":"response.create","model":"default-queued","input":[]}`)
			s.forwarded(t)
			s.send(t, `{"type":"response.create","model":"named-other","stream_id":"other","input":[]}`)
			s.forwarded(t)
			s.event(t, `{"type":"response.created","stream_id":"other","response":{"id":"known-other","status":"in_progress"}}`)
			raw := `{"type":"error","response_id":"known-default","status":429,"error":{"code":"rate_limit_exceeded","message":"default-only"}}`
			if laneMember != "" {
				raw = strings.TrimSuffix(raw, "}") + laneMember + `}`
			}
			s.event(t, raw)
			// A real roundtrip on the other lane proves that a forwarded scoped
			// error did not cause the asynchronous gateway pump to close peers.
			s.event(t, `{"type":"response.completed","stream_id":"other","response":{"id":"known-other","status":"completed","usage":{"input_tokens":17,"output_tokens":2}}}`)
			if s.p.inFlight("test") != 1 {
				t.Fatalf("default error did not release exactly its head: %d", s.p.inFlight("test"))
			}
			account, err := s.p.Store.Get("test")
			if err != nil {
				t.Fatal(err)
			}
			var cooldown *gatewayCooldownError
			if !errors.As(s.p.checkGatewayCooldown(withGatewayCooldownModel(t.Context(), "default-first"), account), &cooldown) {
				t.Fatal("default head missed its cooldown")
			}
			for _, model := range []string{"named-other", "default-queued"} {
				if s.p.checkGatewayCooldown(withGatewayCooldownModel(t.Context(), model), account) != nil {
					t.Fatal("default error broadened cooldown")
				}
			}
			s.event(t, raw) // Already-retired ID must not consume promoted default head.
			s.event(t, `{"type":"response.completed","response":{"id":"new-default","status":"completed","usage":{"input_tokens":23,"output_tokens":3}}}`)
			waitGatewayWSReleased(t, s.p)
			records := s.p.Metrics.RecentForAccount("test", 10)
			if len(records) != 3 {
				t.Fatalf("records = %d", len(records))
			}
			for _, record := range records {
				if record.Model == "default-first" && (record.Status != "error" || record.ResponseID != "known-default") {
					t.Fatal("default error attribution lost")
				}
				if record.Model != "default-first" && record.Status != "ok" {
					t.Fatal("known default error aborted another turn")
				}
			}
		})
	}
}

func TestGatewayWSResponseLaneOnlyUniqueActiveHeads(t *testing.T) {
	turns := gatewayWSTurns{pending: map[string][]*gatewayWSTurn{
		"a": {{responseID: "active-a"}, {responseID: "queued-id"}},
		"b": {{responseID: "active-b"}},
	}}
	for _, tc := range []struct {
		lane, id string
		explicit bool
		want     string
		okay     bool
	}{
		{"", "active-a", false, "a", true},
		{"b", "active-a", true, "", false},
		{"", "queued-id", false, "", true},
		{"", "unknown", false, "", true},
		{"a", "active-a", true, "a", true},
		{"", "active-a", true, "", false},
	} {
		lane, _, okay := turns.responseLaneLocked(tc.lane, tc.id, tc.explicit)
		if lane != tc.want || okay != tc.okay {
			t.Fatalf("association mismatch for %+v: %q %t", tc, lane, okay)
		}
	}
	turns.pending["b"][0].responseID = "active-a"
	if _, _, okay := turns.responseLaneLocked("", "active-a", false); okay {
		t.Fatal("ambiguous ID selected a random model")
	}
}

func TestGatewayWSContinuationActualNativeMissingLaneAndPolicyGate(t *testing.T) {
	requests := make(chan []byte, 4)
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		for number := 0; ; number++ {
			_, raw, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			requests <- raw
			var request struct {
				StreamID string `json:"stream_id"`
			}
			if json.Unmarshal(raw, &request) != nil {
				return
			}
			created := fmt.Sprintf(`{"type":"response.created","stream_id":%q,"response":{"id":"native-%d","status":"in_progress"}}`, request.StreamID, number)
			completed := fmt.Sprintf(`{"type":"response.completed","response":{"id":"native-%d","status":"completed","usage":{"input_tokens":11,"output_tokens":2}}}`, number)
			if conn.Write(r.Context(), websocket.MessageText, []byte(created)) != nil {
				return
			}
			if conn.Write(r.Context(), websocket.MessageText, []byte(completed)) != nil {
				return
			}
		}
	})
	enableGatewayWS(t, p, "old native prefix")
	realGatewayNativePool(t, p)
	p.NativeWebSocket = true
	server := httptest.NewServer(p)
	t.Cleanup(server.Close)
	client, _ := gatewayWSDial(t, server.URL, "native-policy")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	request := `{"type":"response.create","stream_id":"main","model":"fixture","instructions":"native client","input":[]}`
	for _, previous := range []string{"", "native-0"} {
		payload := request
		if previous != "" {
			payload = strings.TrimSuffix(payload, "}") + `,"previous_response_id":"` + previous + `"}`
		}
		if err := client.Write(ctx, websocket.MessageText, []byte(payload)); err != nil {
			t.Fatal(err)
		}
		gatewayWSRead(t, client)
		if raw := gatewayWSRead(t, client); !strings.Contains(string(raw), `"response.completed"`) {
			t.Fatal("native terminal lost")
		}
		<-requests
		waitGatewayWSReleased(t, p)
	}
	settings, _ := p.Settings.Get()
	settings.BuiltinPrompt = "new native prefix"
	if err := p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
	delta := strings.TrimSuffix(request, "}") + `,"previous_response_id":"native-1"}`
	if err := client.Write(ctx, websocket.MessageText, []byte(delta)); err != nil {
		t.Fatal(err)
	}
	if raw := gatewayWSRead(t, client); !strings.Contains(string(raw), "resend full input") {
		t.Fatalf("native delta bypassed policy gate: %s", raw)
	}
	select {
	case <-requests:
		t.Fatal("rejected native delta was sent")
	default:
	}
	if err := client.Write(ctx, websocket.MessageText, []byte(request)); err != nil {
		t.Fatal(err)
	}
	gatewayWSRead(t, client)
	gatewayWSRead(t, client)
	if raw := <-requests; !strings.Contains(string(raw), "new native prefix") {
		t.Fatal("native full request did not apply current policy")
	}
	waitGatewayWSReleased(t, p)
	if records := p.Metrics.RecentForAccount("test", 10); len(records) != 3 {
		t.Fatalf("native record count = %d", len(records))
	}
}
