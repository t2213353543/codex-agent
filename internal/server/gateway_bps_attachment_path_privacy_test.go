package server

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Complements the author's seven positive/unknown routes and the original
// /responses privacy test with the new prefix's ambiguous path spellings.
func TestBPSAttachmentRoutePrivacyBoundary(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{"authority", "https://PRIVATE_USER:PRIVATE_PASSWORD@PRIVATE_HOST/basispoints/api/attachments?PRIVATE_QUERY#PRIVATE_FRAGMENT", "/attachments"},
		{"network_relative", "//PRIVATE_HOST/basispoints/api/attachments?PRIVATE_QUERY#PRIVATE_FRAGMENT", "/attachments"},
		{"encoded_prefix_slash", "/basispoints%2Fapi/attachments", ""},
		{"encoded_endpoint_slash", "/basispoints/api%2Fattachments", ""},
		{"encoded_letter", "/basispoints/api/%61ttachments", ""},
		{"encoded_dot", "/basispoints/api/%2e/attachments", ""},
		{"literal_dot", "/basispoints/api/./attachments", ""},
		{"literal_parent", "/basispoints/api/PRIVATE_ID/../attachments", ""},
		{"encoded_parent", "/basispoints/api/PRIVATE_ID/%2e%2e/attachments", ""},
		{"escaped_id_slash", "/basispoints/api/attachments%2FPRIVATE_ID", ""},
		{"double_escape", "/basispoints/api%252Fattachments", ""},
		{"double_slash", "/basispoints/api//attachments", ""},
		{"nested_prefix", "/v1/basispoints/api/attachments", ""},
		{"prefix_boundary", "/basispoints/apiPRIVATE_ID/attachments", ""},
		{"suffix", "/basispoints/api/attachments;PRIVATE_ID", ""},
		{"opaque", "private:basispoints/api/attachments", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := gatewayObservedRoute(tc.input); got != tc.want {
				t.Fatalf("route = %q; want %q", got, tc.want)
			}
			_, observer := withGatewayAttemptObservation(t.Context())
			observer.ObserveInbound(tc.input, nil)
			observer.ObserveSent(tc.input, nil)
			handle := observer.Begin("request", tc.input, time.Now())
			handle.Headers(429, time.Now())
			handle.Finish("upstream", "http_status", time.Now())
			rec := RequestRecord{Endpoint: "responses", Status: "error", HTTPCode: 429}
			applyGatewayAttemptSnapshot(&rec, observer)
			encoded, _ := json.Marshal(rec)
			if strings.Contains(string(encoded), "PRIVATE_") {
				t.Fatal("private URI data entered record")
			}
			if rec.InboundPath != tc.want || rec.UpstreamPath != tc.want || len(rec.Attempts) != 1 || rec.Attempts[0].Endpoint != tc.want {
				t.Fatal("projection changed route")
			}
		})
	}
}

// Invoke the actual transport observation wrapper, with a local callback that
// supplies headers directly. No HTTP client transport or remote service runs.
func independentAttachmentObservedDispatch(t *testing.T, base string, route string) RequestRecord {
	t.Helper()
	p := newTestProxy(t, 1)
	account, err := p.Store.Get("test")
	if err != nil {
		t.Fatal(err)
	}
	target, err := gatewayUpstreamURL(base, route, url.Values{"private_token": {"PRIVATE_QUERY"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, observer := withGatewayAttemptObservation(t.Context())
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target+"#PRIVATE_FRAGMENT", nil)
	if err != nil {
		t.Fatal(err)
	}
	rec := RequestRecord{Endpoint: "responses", Status: "error", HTTPCode: 429}
	calls := 0
	response, err := p.doObservedAccountRequest(&rec, p.Client, request, account, func(_ *http.Client, sent *http.Request) (*http.Response, error) {
		calls++
		if sent.URL.RequestURI() != request.URL.RequestURI() {
			t.Fatal("observation changed outgoing URI")
		}
		return &http.Response{StatusCode: 204, Body: http.NoBody, Header: make(http.Header), Request: sent}, nil
	}, nil)
	if err != nil || response == nil || calls != 1 {
		t.Fatalf("dispatch boundary failed: %v, calls=%d", err, calls)
	}
	applyGatewayAttemptSnapshot(&rec, observer)
	if len(rec.Attempts) != 1 {
		t.Fatal("one dispatch must produce one attempt")
	}
	encoded, _ := json.Marshal(rec)
	if strings.Contains(string(encoded), "PRIVATE_") {
		t.Fatal("actual producer retained private host/query/fragment")
	}
	t.Logf("wire_path=%s raw_path=%s endpoint=%q", request.URL.EscapedPath(), request.URL.RawPath, rec.Attempts[0].Endpoint)
	return rec
}

func TestBPSAttachmentRouteConsumerFields(t *testing.T) {
	for _, tc := range []struct{ name, base, route string }{
		{"canonical_attachment", "https://PRIVATE_HOST/basispoints/api", "/attachments"},
		{"legacy_v1_attachment", "https://PRIVATE_HOST/v1", "/attachments"},
		{"legacy_backend_attachment", "https://PRIVATE_HOST/backend-api/codex", "/attachments"},
		{"canonical_generation", "https://PRIVATE_HOST/basispoints/api", "/responses"},
		{"canonical_compact", "https://PRIVATE_HOST/basispoints/api", "/responses/compact"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := independentAttachmentObservedDispatch(t, tc.base, tc.route)
			if rec.Attempts[0].Endpoint != tc.route {
				t.Fatalf("attempt route=%q want=%q", rec.Attempts[0].Endpoint, tc.route)
			}
			if rec.Endpoint != "responses" || rec.UpstreamPath != "" || rec.SentModel != "" {
				t.Fatal("Begin invented request-level generation metadata")
			}
			if !(managedLogQuery{Endpoint: "responses"}).matches(rec) || (managedLogQuery{Endpoint: "attachments"}).matches(rec) || (managedLogQuery{UpstreamPath: "/attachments"}).matches(rec) {
				t.Fatal("attempt endpoint incorrectly changed request-level filters")
			}
			exported := managedExportRecord(rec, true)
			raw, _ := json.Marshal(exported)
			if strings.Contains(string(raw), "PRIVATE_") || len(exported.Attempts) != 1 || exported.Attempts[0].Endpoint != tc.route {
				t.Fatal("managed projection leaked or erased route")
			}
		})
	}
	// The shared helper intentionally affects callers that explicitly ObserveSent
	// with a configured prefix. This changes only upstream_path, not Endpoint.
	_, observer := withGatewayAttemptObservation(t.Context())
	observer.ObserveSent("/basispoints/api/responses", []byte(`{"model":"synthetic"}`))
	rec := RequestRecord{Endpoint: "responses"}
	applyGatewayAttemptSnapshot(&rec, observer)
	if !(managedLogQuery{UpstreamPath: "/responses"}).matches(rec) {
		t.Fatal("explicit ObserveSent canonical path missing")
	}
	if (managedLogQuery{UpstreamPath: "/basispoints/api/responses"}).matches(rec) {
		t.Fatal("filter did not use stored canonical value")
	}
}

func TestBPSAttachmentRouteEscapedProducer(t *testing.T) {
	for _, tc := range []struct{ name, base, route string }{
		{"new_attachment_prefix", "https://PRIVATE_HOST/basispoints%2Fapi", "/attachments"},
		{"new_generation_prefix", "https://PRIVATE_HOST/basispoints%2Fapi", "/responses"},
		{"existing_generation_control", "https://PRIVATE_HOST/backend-api%2Fcodex", "/responses"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := independentAttachmentObservedDispatch(t, tc.base, tc.route)
			if rec.Attempts[0].Endpoint != "" {
				t.Fatalf("escaped configured prefix was mislabeled canonical %q", rec.Attempts[0].Endpoint)
			}
		})
	}
}
