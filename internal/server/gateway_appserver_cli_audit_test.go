package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type appServerAuditRoundTripper func(*http.Request) (*http.Response, error)

func (fn appServerAuditRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestAppServerCLIAuditExplicitEffortSchemaIdentityAndNull(t *testing.T) {
	for _, effort := range []string{"ultra", "persistent", "xhigh"} {
		t.Run(effort, func(t *testing.T) {
			raw := []byte(`{"model":"fixture","input":"hi","reasoning":{"effort":"` + effort + `","summary":"detailed","future_reasoning":{"large":9007199254740993}},"service_tier":"flex","parallel_tool_calls":true,"text":{"format":{"type":"json_schema","name":"caller-schema","strict":false,"schema":{"type":"object","properties":{"n":{"const":9007199254740993}}}}},"tools":[{"type":"namespace","name":"client_namespace","tools":[{"type":"function","name":"inspect","parameters":{"type":"object"}}]}],"tool_choice":{"type":"function","namespace":"client_namespace","name":"inspect"}}`)
			headers := http.Header{}
			headers.Set("x-codex-installation-id", "caller-installation")
			in, _, err := prepareAppServerCLI(raw, headers, json.RawMessage(appServerCLITestModel), "generated-installation")
			if err != nil {
				t.Fatal(err)
			}
			if in.ClientMetadata["x-codex-installation-id"] != "caller-installation" {
				t.Fatal("caller installation replaced")
			}
			wire, err := buildAppServerRequest(in)
			if err != nil {
				t.Fatal(err)
			}
			var body map[string]json.RawMessage
			_ = json.Unmarshal(wire.Body, &body)
			for _, part := range []string{`"effort":"` + effort + `"`, `"summary":"detailed"`, "9007199254740993"} {
				if !strings.Contains(string(body["reasoning"]), part) {
					t.Fatalf("explicit reasoning lost: %s", body["reasoning"])
				}
			}
			if string(body["service_tier"]) != `"flex"` || string(body["parallel_tool_calls"]) != "true" {
				t.Fatal("explicit request control changed")
			}
			if !strings.Contains(string(body["text"]), "caller-schema") || !strings.Contains(string(body["text"]), "9007199254740993") {
				t.Fatal("schema/name precision lost")
			}
			if !strings.Contains(string(body["tool_choice"]), `"namespace":"client_namespace"`) {
				t.Fatal("explicit namespace lost")
			}
		})
	}
	in, _, err := prepareAppServerCLI([]byte(`{"model":"fixture","input":"hi","text":null,"reasoning":null}`), http.Header{}, json.RawMessage(appServerCLITestModel), "install")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = buildAppServerRequest(in); err != nil {
		t.Fatal(err)
	}
}

func TestAppServerCLIAuditInputMappingFailureNotOverwritten(t *testing.T) {
	_, _, err := prepareAppServerCLI([]byte(`{"model":"fixture","input":[{"type":"custom_tool_call","name":"inspect","input":"bad type"}],"tools":[{"type":"function","name":"inspect","parameters":{"type":"object"}}],"tool_choice":"auto"}`), http.Header{}, json.RawMessage(appServerCLITestModel), "install")
	if err == nil {
		t.Fatal("tool-choice success swallowed input type mismatch")
	}
	if _, _, err = prepareAppServerCLI([]byte(`{"model":"fixture","input":"hi","service_tier":{}}`), http.Header{}, json.RawMessage(appServerCLITestModel), "install"); err == nil {
		t.Fatal("malformed service tier ignored")
	}
}

func TestAppServerCLIAuditCatalogWaiterCancelAndCacheCopies(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		close(started)
		<-release
		_, _ = io.WriteString(w, `{"models":[`+appServerCLITestModel+`]}`)
	}))
	defer up.Close()
	a := &appServerCLI{catalog: map[[32]byte]appServerCatalogSnapshot{}}
	t.Cleanup(a.Close)
	request := func(ctx context.Context) *http.Request {
		r, _ := http.NewRequestWithContext(ctx, "POST", up.URL+"/backend-api/codex/responses", nil)
		r.Header.Set("Authorization", "Bearer synthetic")
		return r
	}
	leaderDone := make(chan error, 1)
	go func() { _, _, err := a.model(request(t.Context()), up.Client(), nil, "fixture"); leaderDone <- err }()
	<-started
	ctx, cancel := context.WithCancel(t.Context())
	waiterDone := make(chan error, 1)
	go func() { _, _, err := a.model(request(ctx), up.Client(), nil, "fixture"); waiterDone <- err }()
	cancel()
	select {
	case err := <-waiterDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waiter cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("waiter blocked on leader")
	}
	close(release)
	if err := <-leaderDone; err != nil {
		t.Fatal(err)
	}
	first, _, err := a.model(request(t.Context()), up.Client(), nil, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	first[0] = '!'
	second, _, err := a.model(request(t.Context()), up.Client(), nil, "fixture")
	if err != nil || !json.Valid(second) {
		t.Fatal("caller mutated shared catalog")
	}
	_, _, err = a.model(request(t.Context()), up.Client(), nil, "missing")
	if err == nil || calls.Load() != 1 {
		t.Fatal("missing model bypassed fresh catalog")
	}
}

func TestAppServerCLIAuditCatalogCancelRetryGuardAndRouteScopes(t *testing.T) {
	var calls atomic.Int32
	client := &http.Client{Transport: appServerAuditRoundTripper(func(req *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			return nil, context.Canceled
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"models":[` + appServerCLITestModel + `]}`))}, nil
	})}
	a := &appServerCLI{catalog: map[[32]byte]appServerCatalogSnapshot{}}
	request := func(ctx context.Context) *http.Request {
		r, _ := http.NewRequestWithContext(ctx, "POST", "https://example.invalid/backend-api/codex/responses", nil)
		r.Header.Set("Authorization", "Bearer synthetic")
		return r
	}
	for _, version := range []int64{1, 1, 2} {
		proxy := &ProxyEntry{ID: "route", Version: version, Type: "http", Host: "127.0.0.1", Port: 8, Enabled: true}
		if _, _, err := a.model(request(t.Context()), client, proxy, "fixture"); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 3 {
		t.Fatalf("cancel retry/proxy version scope: %d", calls.Load())
	}
	rejected := errors.New("revoked synthetic authority")
	ctx := withGatewayNativeDispatchGuard(t.Context(), func(context.Context) error { return rejected })
	_, _, err := a.model(request(ctx), client, nil, "fixture")
	if !errors.Is(err, rejected) || calls.Load() != 3 {
		t.Fatal("live authority guard bypass")
	}
	a.Close()
	if _, _, err = a.model(request(t.Context()), client, nil, "fixture"); !errors.Is(err, errAppServerTransportClosed) {
		t.Fatal("catalog used after close")
	}
}

func TestAppServerCLIAuditSSEPreservesControlUsageAndPrecision(t *testing.T) {
	adapter, err := buildAppServerTools(json.RawMessage(`[{"type":"function","name":"inspect","parameters":{"type":"object"}}]`), true)
	if err != nil {
		t.Fatal(err)
	}
	event := `{"type":"response.completed","response":{"id":"resp_exact","model":"fixture","output":[{"type":"function_call","id":"fc_exact","call_id":"call_exact","namespace":"functions","name":"inspect","arguments":"{}"}],"usage":{"input_tokens":9007199254740993,"output_tokens":8,"input_tokens_details":{"cached_tokens":7},"output_tokens_details":{"reasoning_tokens":6}}}}`
	source := ": heartbeat\n\nid: event_exact\nevent: response.completed\nretry: 1234\ndata: " + event + "\n\n" + "data: [DONE]\n\n"
	stream := newAppServerMappedStream(io.NopCloser(strings.NewReader(source)), adapter)
	raw, err := io.ReadAll(stream)
	_ = stream.Close()
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{": heartbeat\n\n", "id: event_exact\n", "event: response.completed\n", "retry: 1234\n", "9007199254740993", `"cached_tokens":7`, `"reasoning_tokens":6`, `"model":"fixture"`, "data: [DONE]\n\n"} {
		if !bytes.Contains(raw, []byte(field)) {
			t.Fatalf("lost SSE/control/usage field %q", field)
		}
	}
	if bytes.Contains(raw, []byte(`"namespace":"functions"`)) {
		t.Fatal("tool identity was not restored")
	}
}

func TestAppServerCLIAuditNoFallbackAfterGeneration(t *testing.T) {
	var httpGenerations atomic.Int32
	client := &http.Client{Transport: appServerAuditRoundTripper(func(req *http.Request) (*http.Response, error) {
		body := `{"models":[` + appServerCLITestModel + `]}`
		if req.Method == "POST" {
			httpGenerations.Add(1)
			body = "data: " + `{"type":"response.completed","response":{"id":"http","output":[]}}` + "\n\n"
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	for _, failure := range []string{"forbidden", "after-generation"} {
		t.Run(failure, func(t *testing.T) {
			a := newAppServerCLI(&Proxy{})
			defer a.Close()
			conn := newAppServerTransportFakeWS()
			if failure == "forbidden" {
				a.transport.dial = func(context.Context, http.Header, *ProxyEntry) (gatewayWSUpstream, *http.Response, error) {
					return nil, &http.Response{StatusCode: 403, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, errNativeWSHandshake
				}
			} else {
				a.transport.dial = func(context.Context, http.Header, *ProxyEntry) (gatewayWSUpstream, *http.Response, error) {
					return conn, &http.Response{StatusCode: 101, Header: make(http.Header)}, nil
				}
				conn.onWrite = func(n int, raw []byte) error {
					if n == 1 {
						conn.reads <- []byte(`{"type":"response.completed","response":{"id":"warm","output":[]}}`)
						return nil
					}
					return io.ErrClosedPipe
				}
			}
			prepared := []byte(`{"model":"fixture","input":"hi","reasoning":{"effort":"xhigh"}}`)
			req := appServerTransportTestRequest(t.Context())
			before := httpGenerations.Load()
			res, err := a.RoundTrip(req, client, nil, prepared)
			if res != nil {
				_, _ = io.ReadAll(res.Body)
				_ = res.Body.Close()
			}
			if failure == "forbidden" && (err != nil || httpGenerations.Load() != before+1) {
				t.Fatal("safe handshake fallback missing")
			}
			if failure == "after-generation" && (err == nil || httpGenerations.Load() != before) {
				t.Fatal("ambiguous generation replayed over HTTP")
			}
		})
	}
}
