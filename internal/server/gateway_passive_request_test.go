package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestGatewayPassiveRequestMetadata(t *testing.T) {
	for _, value := range []string{"", "two ids", "id\nsecret", "\u5173\u8054", strings.Repeat("a", 129), "url?secret=1"} {
		r := httptest.NewRequest("GET", "http://local/", nil)
		r.Header.Set("X-Client-Request-ID", value)
		if gatewayObservedClientRequestID(r) != "" {
			t.Fatalf("invalid correlation accepted: %q", value)
		}
	}
	r := httptest.NewRequest("GET", "http://local/", nil)
	r.Header.Set("Authorization", "Bearer PRIVATE_TOKEN")
	r.Header.Set("X-Request-ID", "not-client-id")
	if gatewayObservedClientRequestID(r) != "" {
		t.Fatal("other header became client correlation")
	}
	r.Header.Set("X-Client-Request-ID", "request_A.9:part-1")
	if gatewayObservedClientRequestID(r) != "request_A.9:part-1" {
		t.Fatal("valid correlation lost")
	}
	r.Header.Add("X-Client-Request-ID", "duplicate")
	if gatewayObservedClientRequestID(r) != "" {
		t.Fatal("ambiguous header accepted")
	}
	var rec RequestRecord
	zero := time.Unix(1700000000, 0)
	observeGatewayAuthentication(&rec, zero, zero)
	observeGatewayResponseTransfer(&rec, time.Time{}, zero)
	if rec.AuthenticationMs == nil || *rec.AuthenticationMs != 0 || rec.ResponseTransferMs != nil {
		t.Fatal("timing zero/unknown conflated")
	}
	observeGatewayResponseTransfer(&rec, zero, zero.Add(7*time.Millisecond))
	if rec.ResponseTransferMs == nil || *rec.ResponseTransferMs != 7 || rec.TimingSource != "gateway_handler_v1" {
		t.Fatal("timing provenance missing")
	}
}

func TestGatewayPassiveIngressDoesNotChangeWireOrUsage(t *testing.T) {
	var calls atomic.Int32
	p := proxyFixture(t, func(http.ResponseWriter, *http.Request) { calls.Add(1) })
	requests := []struct {
		method, path            string
		unauthorized, websocket bool
		code                    int
	}{
		{"POST", "/v1/responses?PRIVATE_QUERY=1", true, false, 401},
		{"GET", "/v1/responses", false, false, 405},
		{"POST", "/v1/models", false, false, 405},
		{"GET", "/v1/PRIVATE_PATH", false, false, 404},
		{"GET", "/v1/responses", false, true, 426},
	}
	request := func(item struct {
		method, path            string
		unauthorized, websocket bool
		code                    int
	}) *http.Request {
		r := proxyRequest(item.method, item.path, "PRIVATE_BODY")
		if item.unauthorized {
			r.Header.Set("Authorization", "Bearer PRIVATE_INVALID_TOKEN")
		}
		if item.websocket {
			r.Header.Set("Upgrade", "websocket")
		}
		r.Header.Set("X-Client-Request-ID", "passive-correlation")
		r.Header.Set("Cookie", "PRIVATE_COOKIE")
		return r
	}
	var previous []*httptest.ResponseRecorder
	for _, item := range requests {
		w := httptest.NewRecorder()
		p.ServeHTTP(w, request(item))
		previous = append(previous, w)
	}
	if err := p.startGatewayIngressEvents(); err != nil {
		t.Fatal(err)
	}
	defer p.IngressEvents.Close(context.Background())
	for i, item := range requests {
		w := httptest.NewRecorder()
		p.ServeHTTP(w, request(item))
		if w.Code != item.code || w.Code != previous[i].Code || !bytes.Equal(w.Body.Bytes(), previous[i].Body.Bytes()) || !reflect.DeepEqual(w.Header(), previous[i].Header()) {
			t.Fatalf("ingress observation changed status/body/headers for %s", item.path)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.IngressEvents.Close(ctx); err != nil {
		t.Fatal(err)
	}
	entry, err := p.managementState().Get(ctx, "ingress-events", "recent")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := decodeGatewayIngressDocument(entry)
	if err != nil || len(doc.Events) != len(requests) {
		t.Fatal("ingress not persisted", len(doc.Events), err)
	}
	for _, event := range doc.Events {
		if event.AuthenticationMs == nil || event.ClientRequestID != "passive-correlation" {
			t.Fatal("ingress correlation or auth observation absent")
		}
	}
	if strings.Contains(string(entry.Payload), "PRIVATE_") {
		t.Fatal("ingress stored body/path/query/cookie/credential")
	}
	page, err := p.Metrics.managedLogs(ctx, managedQuery(t, "exact_total=true"))
	if err != nil || page.Total != 0 || calls.Load() != 0 {
		t.Fatal("pre-admission rejects entered business accounting or upstream", page.Total, calls.Load(), err)
	}
}

func TestGatewayPassiveHTTPWireAndTiming(t *testing.T) {
	const wire = "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"passive-response\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":8,\"output_tokens\":2}}}\n\n"
	for _, target := range []string{"/v1/responses", "/v1/responses/compact", "/v1/models"} {
		t.Run(target, func(t *testing.T) {
			var calls atomic.Int32
			var upstreamBody []byte
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				upstreamBody, _ = io.ReadAll(r.Body)
				w.Header().Set("X-Request-ID", "upstream-only")
				if target == "/v1/models" {
					_, _ = io.WriteString(w, `{"models":[]}`)
					return
				}
				if target == "/v1/responses/compact" {
					_, _ = io.WriteString(w, `{"id":"compact-id","usage":{"input_tokens":8,"output_tokens":2},"output":[]}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(200)
				w.(http.Flusher).Flush()
				time.Sleep(15 * time.Millisecond)
				_, _ = io.WriteString(w, wire)
			})
			method, body := "POST", `{"model":"passive-model","stream":true,"input":[]}`
			if target == "/v1/models" {
				method, body = "GET", ""
			}
			request := proxyRequest(method, target, body)
			request.Header.Set("X-Client-Request-ID", "client-observed-1")
			before := request.Header.Clone()
			w := httptest.NewRecorder()
			p.ServeHTTP(w, request)
			if w.Code != 200 || calls.Load() != 1 {
				t.Fatal("passive metadata changed dispatch/status", w.Code, calls.Load())
			}
			if target == "/v1/responses" && w.Body.String() != wire {
				t.Fatal("passive metadata changed SSE bytes")
			}
			if target == "/v1/models" && w.Body.String() != `{"models":[]}` {
				t.Fatal("models bytes changed")
			}
			if w.Header().Get("X-Client-Request-ID") != "" || request.Header.Get("X-Client-Request-ID") != before.Get("X-Client-Request-ID") {
				t.Fatal("generated or altered correlation header")
			}
			rec, ok := p.Metrics.LastFor("test")
			if !ok || rec.ClientRequestID != "client-observed-1" || rec.ClientRequestIDScope != "request" || rec.ClientRequestIDSource != "request_header" || rec.RequestID != "upstream-only" {
				t.Fatalf("independent request identity missing: %+v", rec)
			}
			if rec.AuthenticationMs == nil || rec.ResponseTransferMs == nil || *rec.AuthenticationMs < 0 || *rec.ResponseTransferMs < 0 {
				t.Fatal("true HTTP boundaries unobserved")
			}
			if target == "/v1/responses" && (*rec.ResponseTransferMs < 10 || rec.tokens() != 10) {
				t.Fatal("body interval or original token count changed")
			}
			if bytes.Contains(upstreamBody, []byte("client-observed-1")) {
				t.Fatal("passive metadata inserted into request body")
			}
		})
	}
}

func TestGatewayPassiveWSTurnIsolation(t *testing.T) {
	var count atomic.Int32
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		for i := 0; i < 2; i++ {
			_, payload, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			if strings.Contains(string(payload), "client-passive-ws") {
				t.Error("connection identifier inserted in WS body")
			}
			count.Add(1)
			_ = conn.Write(r.Context(), websocket.MessageText, []byte(fmt.Sprintf(`{"type":"response.created","response":{"id":"turn-%d"}}`, i)))
			if i == 0 {
				time.Sleep(20 * time.Millisecond)
			}
			_ = conn.Write(r.Context(), websocket.MessageText, []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":"turn-%d","status":"completed","output":[],"usage":{"input_tokens":3,"output_tokens":1}}}`, i)))
		}
		_, _, _ = conn.Read(r.Context())
	})
	enableGatewayWS(t, p, "")
	server := httptest.NewServer(p)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, response, err := websocket.Dial(ctx, server.URL+"/v1/responses", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer test-client-key"}, "X-Client-Request-ID": {"client-passive-ws"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	if response.Header.Get("X-Client-Request-ID") != "" {
		t.Fatal("new response ID header was introduced")
	}
	for i := 0; i < 2; i++ {
		if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create","model":"fixture","input":[]}`)); err != nil {
			t.Fatal(err)
		}
		_ = gatewayWSRead(t, conn)
		_ = gatewayWSRead(t, conn)
		waitGatewayWSReleased(t, p)
		rec, ok := p.Metrics.LastFor("test")
		if !ok || rec.ClientRequestID != "client-passive-ws" || rec.ClientRequestIDScope != "connection" || rec.AuthenticationMs == nil || rec.ResponseTransferMs == nil || rec.tokens() != 4 {
			t.Fatal("per-turn identity/timing lost")
		}
		if i == 0 && *rec.ResponseTransferMs < 15 {
			t.Fatal("first event interval not observed")
		}
	}
	rows := p.Metrics.RecentForAccount("test", 10)
	if len(rows) != 2 || count.Load() != 2 {
		t.Fatal("passive hooks altered turn cardinality")
	}
}

func TestManagedPassiveClientIDQuery(t *testing.T)   { exercisePassiveClientIDQuery(t, false) }
func TestPGManagedPassiveClientIDQuery(t *testing.T) { exercisePassiveClientIDQuery(t, true) }
func exercisePassiveClientIDQuery(t *testing.T, pg bool) {
	m := managedUsageFixture(t, pg)
	m.Record(RequestRecord{Time: 1700000020, EventID: "client-source-record", Status: "ok", ClientRequestID: "client-id-only", ClientRequestIDSource: "request_header", ClientRequestIDScope: "request", RequestID: "upstream-id-only"})
	q := managedQuery(t, "client_request_id=client-id-only&exact_total=true")
	page, err := m.managedLogs(context.Background(), q)
	if err != nil || page.Total != 1 {
		t.Fatal("client ID not independently queryable", err)
	}
	q.RequestID = "client-id-only"
	page, err = m.managedLogs(context.Background(), q)
	if err != nil || page.Total != 0 {
		t.Fatal("upstream request ID confused with client header", err)
	}
	q.RequestID = ""
	var out bytes.Buffer
	if _, err = m.exportManagedLogsWithPrivacy(context.Background(), q, "jsonl", &out, true); err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &record); err != nil {
		t.Fatal(err)
	}
	if record["client_request_id"] == "client-id-only" || record["client_request_id_source"] != "request_header" {
		t.Fatal("correlation export privacy/source lost")
	}
}
