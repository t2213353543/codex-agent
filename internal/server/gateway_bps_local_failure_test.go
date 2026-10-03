package server

import (
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

	"github.com/coder/websocket"
)

func bpsLocalFailureRecord(t *testing.T, p *Proxy) RequestRecord {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if records := p.Metrics.RecentErrors(1, time.Now().Unix()+1, 2); len(records) == 1 {
			return records[0]
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("expected one completed failed request record")
	return RequestRecord{}
}

func TestBPSLocalCompletionAuthorizationHTTPAndWebSocket(t *testing.T) {
	for _, transport := range []string{"json", "sse", "websocket", "structured_json", "structured_sse", "structured_websocket", "compact"} {
		for _, mutation := range []string{"disabled", "store_error"} {
			t.Run(transport+"/"+mutation, func(t *testing.T) {
				var p *Proxy
				var writer *KeyStore
				var key APIKey
				var calls atomic.Int32
				structured := strings.HasPrefix(transport, "structured_")
				p = bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					var payload []byte
					if structured {
						out := httptest.NewRecorder()
						bpsStructuredUpstreamReply(t, out, r, "completion_local", `{"number":9007199254740993}`)
						payload = bytes.Clone(out.Body.Bytes())
					} else if transport == "compact" {
						_, _ = io.Copy(io.Discard, r.Body)
						payload = []byte(`{"id":"completion_local","object":"response.compaction","output":[{"type":"compaction","encrypted_content":"private-state"}],"usage":{"input_tokens":100,"output_tokens":7}}`)
					} else {
						_, _ = io.Copy(io.Discard, r.Body)
						payload = []byte("data: " + `{"type":"response.output_text.delta","response_id":"completion_local","delta":"VISIBLE"}` + "\n\n" + bpsGatewayFrame("response.completed", "completion_local", "[]"))
					}
					gatewayKeyAuthorizationMutate(t, writer, key, mutation)
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = w.Write(payload)
				})
				key, account := gatewaySessionAuthorizationFixture(t, p)
				writer = gatewayKeyAuthorizationWriter(t, p, key)
				isWS := strings.HasSuffix(transport, "websocket")
				stream := isWS || strings.HasSuffix(transport, "sse")
				fields := map[string]any{"model": "gpt-6-astra", "input": "synthetic", "stream": stream}
				if structured {
					fields["text"] = json.RawMessage(bpsStructuredTestText)
				}
				const session = "completion-local-access"
				var terminal map[string]json.RawMessage
				if isWS {
					server := httptest.NewServer(p)
					defer server.Close()
					ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
					defer cancel()
					conn, _, err := websocket.Dial(ctx, server.URL+"/v1/responses", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + key.Key}, "Session-Id": {session}}})
					if err != nil {
						t.Fatal(err)
					}
					defer conn.CloseNow()
					fields["type"], fields["stream_id"] = "response.create", "local"
					raw, _ := json.Marshal(fields)
					bpsGatewayWSSend(t, conn, string(raw))
					terminal = bpsGatewayWSUntilTerminal(t, conn, "local")
				} else {
					path := "/v1/responses"
					if transport == "compact" {
						path += "/compact"
					}
					raw, _ := json.Marshal(fields)
					req := proxyRequest(http.MethodPost, path, string(raw))
					req.Header.Set("Authorization", "Bearer "+key.Key)
					req.Header.Set("Session-Id", session)
					out := httptest.NewRecorder()
					p.ServeHTTP(out, req)
					wantStatus := gatewayRecoveryAuthorizationStatus(mutation)
					if stream {
						wantStatus = http.StatusOK
						for _, event := range bpsGatewayEvents(t, out.Body.String()) {
							if bpsGatewayString(event, "type") == "response.failed" {
								terminal = event
							}
						}
					} else if !strings.Contains(out.Body.String(), "response_access_changed") {
						t.Fatal("JSON did not report the typed local completion failure")
					}
					if out.Code != wantStatus || strings.Contains(out.Body.String(), "authentication recovery") || strings.Contains(out.Body.String(), "response.completed") {
						t.Fatalf("local completion public boundary changed: HTTP %d body=%s", out.Code, out.Body.String())
					}
				}
				if stream && (terminal == nil || bpsGatewayString(terminal, "type") != "response.failed" || !bytes.Contains(terminal["response"], []byte(`"code":"response_access_changed"`))) {
					t.Fatalf("stream did not receive a local failed terminal: %s", terminal)
				}
				rec := bpsLocalFailureRecord(t, p)
				wantStatus := gatewayRecoveryAuthorizationStatus(mutation)
				if stream {
					wantStatus = http.StatusOK
				}
				if rec.ErrorSource != "gateway" || rec.ErrorPhase != "authorization" || rec.HTTPCode != wantStatus || rec.Status != "error" || calls.Load() != 1 {
					t.Fatalf("local completion recorded wrong provenance/status or replayed: source=%s phase=%s HTTP=%d status=%s calls=%d", rec.ErrorSource, rec.ErrorPhase, rec.HTTPCode, rec.Status, calls.Load())
				}
				if _, err := p.bpsLoadHistory(t.Context(), key.ID, "completion_local"); err == nil {
					t.Fatal("rejected completion published continuation history")
				}
				gatewaySessionAuthorizationAssertBinding(t, p, key.ID, session, account)
				if p.inFlight(account.ID) != 0 || p.inFlight("alternate") != 0 {
					t.Fatal("local failure leaked account admission")
				}
			})
		}
	}
}

func TestBPSOwnerConflictKeepsSpecificRecordHTTPAndWebSocket(t *testing.T) {
	for _, transport := range []string{"http", "websocket"} {
		t.Run(transport, func(t *testing.T) {
			var calls atomic.Int32
			p := bpsGatewayFixture(t, func(http.ResponseWriter, *http.Request) { calls.Add(1) })
			account, err := p.Store.Get("test")
			if err != nil {
				t.Fatal(err)
			}
			const session = "owner-conflict-local"
			if err := p.bpsSaveHistory(t.Context(), "admin", "resp_owner_conflict", bpsHistoryRecord{Owner: gatewayAccountRef(account), Session: session, Request: []byte(`{"model":"gpt-6-astra","input":[]}`)}); err != nil {
				t.Fatal(err)
			}
			if _, err := p.sessionAccount(t.Context(), "admin", session, "different-owner"); err != nil {
				t.Fatal(err)
			}
			if transport == "http" {
				out := httptest.NewRecorder()
				req := proxyRequest(http.MethodPost, "/v1/responses", `{"model":"gpt-6-astra","previous_response_id":"resp_owner_conflict","input":[]}`)
				req.Header.Set("Session-Id", session)
				p.ServeHTTP(out, req)
				if out.Code != http.StatusConflict {
					t.Fatalf("owner conflict HTTP %d", out.Code)
				}
			} else {
				server := httptest.NewServer(p)
				defer server.Close()
				conn, _ := gatewayWSDial(t, server.URL, session)
				defer conn.CloseNow()
				bpsGatewayWSSend(t, conn, `{"type":"response.create","stream_id":"owner","model":"gpt-6-astra","previous_response_id":"resp_owner_conflict","input":[]}`)
				event := bpsGatewayWSUntilTerminal(t, conn, "owner")
				if bpsGatewayString(event, "type") != "error" || string(event["status"]) != "409" {
					t.Fatalf("owner conflict event %s", event)
				}
			}
			rec := bpsLocalFailureRecord(t, p)
			if rec.Error != "previous response account binding is unavailable or changed" || rec.ErrorSource != "gateway" || rec.ErrorPhase != "session_binding" || rec.HTTPCode != http.StatusConflict || calls.Load() != 0 {
				t.Fatalf("owner conflict lost cause or dispatched history: %+v calls=%d", rec, calls.Load())
			}
			if owner, err := p.sessionAccount(t.Context(), "admin", session, ""); err != nil || owner != "different-owner" {
				t.Fatal("owner conflict changed existing binding")
			}
		})
	}
}

func TestBPSSelectedOwnerConflictKeepsSpecificRecord(t *testing.T) {
	var calls atomic.Int32
	p := bpsGatewayFixture(t, func(http.ResponseWriter, *http.Request) { calls.Add(1) })
	account, err := p.Store.Get("test")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Store.ImportWith("alternate", []byte(`{"access_token":"synthetic-alternate","expires_at":4102444800}`), AccountDefaults{Concurrency: 1}); err != nil {
		t.Fatal(err)
	}
	const session = "owner-selected-conflict"
	if err := p.bpsSaveHistory(t.Context(), "admin", "resp_prior_owner", bpsHistoryRecord{Owner: gatewayAccountRef(account), Session: session, Request: []byte(`{"model":"gpt-6-astra","input":[]}`)}); err != nil {
		t.Fatal(err)
	}
	ctx := withGatewayAccountFailover(t.Context())
	gatewayExcludeFailedAccount(ctx, account)
	state := &officialFailoverState{retrying: true, started: time.Now()}
	ctx = context.WithValue(ctx, officialFailoverKey{}, state)
	req := proxyRequest(http.MethodPost, "/v1/responses", fmt.Sprintf(`{"model":"gpt-6-astra","previous_response_id":"resp_prior_owner","input":[],"prompt_cache_key":%q}`, session)).WithContext(ctx)
	out := httptest.NewRecorder()
	p.gatewayBPS(out, req)
	rec := state.record
	if rec == nil || rec.Error != "previous response belongs to a different account identity" || rec.Status != "error" || rec.ErrorSource != "gateway" || rec.ErrorPhase != "session_binding" || rec.HTTPCode != 409 || calls.Load() != 0 {
		t.Fatalf("selected owner guard lost its cause: record=%+v calls=%d", rec, calls.Load())
	}
}
