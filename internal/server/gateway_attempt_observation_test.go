package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func TestGatewayAttemptObservationContextAndUnknown(t *testing.T) {
	type fixtureKey struct{}
	parent := context.WithValue(t.Context(), fixtureKey{}, "preserved")
	ctx, observation := withGatewayAttemptObservation(parent)
	again, same := withGatewayAttemptObservation(ctx)
	if observation == nil || same != observation || again != ctx || ctx.Value(fixtureKey{}) != "preserved" {
		t.Fatal("observer changed context state or did not preserve one request collector")
	}
	if gatewayAttemptObservationFrom(parent) != nil || gatewayAttemptObservationFrom(nil) != nil || gatewayAttemptObservationFrom(ctx) != observation {
		t.Fatal("context lookup invented or lost an observation")
	}
	if !reflect.DeepEqual(observation.Snapshot(), GatewayAttemptSnapshot{}) {
		t.Fatal("unobserved data was materialized as measured zeros")
	}
	var absent *gatewayAttemptObservation
	absent.ObserveInbound("/responses", []byte(`{}`))
	absent.ObserveSent("/responses", []byte(`{}`))
	absent.ObserveReturned([]byte(`{}`))
	absent.ObserveStage("request_preparation", time.Time{}, time.Time{})
	absent.ObserveErrorTruncation(false)
	missing := absent.Begin("request", "/responses", time.Now())
	missing.Headers(200, time.Now())
	missing.Finish("", "", time.Now())
	if !reflect.DeepEqual(absent.Snapshot(), GatewayAttemptSnapshot{}) {
		t.Fatal("nil observation created a record")
	}
}

func TestGatewayAttemptObservationActual401RefreshRetrySequence(t *testing.T) {
	_, observation := withGatewayAttemptObservation(t.Context())
	start := time.Unix(1000, 0)
	observation.ObserveInbound("/v1/responses?private=query", []byte(`{"model":"inbound-alias","input":"PRIVATE_PROMPT"}`))
	observation.ObserveSent("https://upstream.invalid/backend-api/codex/responses", []byte(`{"model":"sent-model","input":"PRIVATE_PROMPT"}`))
	first := observation.Begin("request", "/backend-api/codex/responses", start)
	first.Headers(401, start.Add(12*time.Millisecond))
	first.Finish("upstream", "http_status", start.Add(12*time.Millisecond))
	refresh := observation.Begin("refresh", "https://login.invalid/oauth/token", start.Add(20*time.Millisecond))
	refresh.Headers(200, start.Add(25*time.Millisecond))
	refresh.Finish("", "", start.Add(31*time.Millisecond))
	retry := observation.Begin("request", "/responses", start.Add(40*time.Millisecond))
	retry.Headers(200, start.Add(48*time.Millisecond))
	retry.Finish("", "", start.Add(48*time.Millisecond))
	observation.ObserveReturned([]byte(`{"type":"response.completed","response":{"model":"returned-model","output":"PRIVATE_OUTPUT"}}`))
	snapshot := observation.Snapshot()
	if len(snapshot.Attempts) != 3 || snapshot.AttemptsTruncated == nil || *snapshot.AttemptsTruncated || snapshot.ErrorTruncated != nil {
		t.Fatal("real sequence metadata incomplete")
	}
	for index, want := range []struct {
		phase, endpoint  string
		status           int
		header, duration int64
	}{
		{"request", "/responses", 401, 12, 12}, {"refresh", "/oauth/token", 200, 5, 11}, {"request", "/responses", 200, 8, 8},
	} {
		got := snapshot.Attempts[index]
		if got.Attempt != index+1 || got.Phase != want.phase || got.Endpoint != want.endpoint || got.StatusCode == nil || *got.StatusCode != want.status ||
			got.HeaderMs == nil || *got.HeaderMs != want.header || got.DurationMs == nil || *got.DurationMs != want.duration {
			t.Fatalf("attempt %d does not describe its own measured interval", index+1)
		}
	}
	if snapshot.InboundModel != "inbound-alias" || snapshot.SentModel != "sent-model" || snapshot.ReturnedModel != "returned-model" || snapshot.InboundPath != "/responses" || snapshot.UpstreamPath != "/responses" {
		t.Fatal("model provenance mixed or endpoint route missing")
	}
	if snapshot.Attempts[0].ErrorSource != "upstream" || snapshot.Attempts[0].ErrorCode != "http_status" || snapshot.Attempts[1].ErrorCode != "" {
		t.Fatal("recovery failure attribution carried into another attempt")
	}
}

func TestGatewayAttemptObservationUnknownZeroAndIdempotence(t *testing.T) {
	_, observation := withGatewayAttemptObservation(t.Context())
	start := time.Unix(1000, 0)
	timeout := observation.Begin("request", "/responses", start)
	timeout.Headers(0, start.Add(time.Millisecond))
	timeout.Finish("transport", "timeout", start.Add(7*time.Millisecond))
	timeout.Headers(200, start.Add(8*time.Millisecond))
	timeout.Finish("upstream", "http_status", start.Add(9*time.Millisecond))
	zero := observation.Begin("websocket", "/responses", start)
	zero.Headers(101, start)
	zero.Headers(429, start.Add(time.Second))
	zero.Finish("", "", start)
	unknown := observation.Begin("refresh", "/oauth/token", time.Time{})
	unknown.Headers(500, start)
	unknown.Finish("upstream", "http_status", start.Add(time.Second))
	backward := observation.Begin("request", "/models", start)
	backward.Headers(200, start.Add(time.Second))
	backward.Finish("", "", start.Add(-time.Second))
	snapshot := observation.Snapshot()
	a := snapshot.Attempts[0]
	if a.HeaderMs != nil || a.StatusCode != nil || a.DurationMs == nil || *a.DurationMs != 7 || a.ErrorCode != "timeout" || a.ErrorSource != "transport" {
		t.Fatal("timeout gained final headers or duplicate finish changed outcome")
	}
	a = snapshot.Attempts[1]
	if a.HeaderMs == nil || *a.HeaderMs != 0 || a.DurationMs == nil || *a.DurationMs != 0 || a.StatusCode == nil || *a.StatusCode != 101 {
		t.Fatal("measured zero or first actual handshake changed")
	}
	a = snapshot.Attempts[2]
	if a.HeaderMs != nil || a.DurationMs != nil || a.StatusCode == nil || *a.StatusCode != 500 {
		t.Fatal("unknown start fabricated timing or lost actual status")
	}
	if snapshot.Attempts[3].DurationMs != nil {
		t.Fatal("backwards clock fabricated duration")
	}
}

func TestGatewayAttemptObservationCapAndConcurrentSnapshot(t *testing.T) {
	_, observation := withGatewayAttemptObservation(t.Context())
	if observation.Begin("PRIVATE_ARBITRARY_PHASE", "/responses", time.Now()) != nil || observation.Snapshot().AttemptsTruncated != nil {
		t.Fatal("unsupported phase created an attempt")
	}
	var tasks sync.WaitGroup
	for range 64 {
		tasks.Add(1)
		go func() {
			defer tasks.Done()
			start := time.Now()
			attempt := observation.Begin("request", "/responses", start)
			attempt.Headers(200, start)
			attempt.Finish("", "", start)
			_ = observation.Snapshot()
		}()
	}
	tasks.Wait()
	snapshot := observation.Snapshot()
	if len(snapshot.Attempts) != gatewayAttemptObservationLimit || snapshot.AttemptsTruncated == nil || !*snapshot.AttemptsTruncated {
		t.Fatal("concurrent attempt state was not bounded and explicitly truncated")
	}
	for index, attempt := range snapshot.Attempts {
		if attempt.Attempt != index+1 || attempt.DurationMs == nil || attempt.HeaderMs == nil || attempt.StatusCode == nil {
			t.Fatal("bounded starts were not ordered or lost concurrent completion")
		}
	}
}

func TestGatewayAttemptObservationSnapshotsDoNotAlias(t *testing.T) {
	_, observation := withGatewayAttemptObservation(t.Context())
	start := time.Now()
	attempt := observation.Begin("request", "/responses", start)
	before := observation.Snapshot()
	attempt.Headers(200, start)
	attempt.Finish("", "", start)
	observation.ObserveStage("account_selection", start, start.Add(3*time.Millisecond))
	observation.ObserveErrorTruncation(false)
	after := observation.Snapshot()
	if before.Attempts[0].StatusCode != nil || before.Attempts[0].HeaderMs != nil || before.Attempts[0].DurationMs != nil {
		t.Fatal("old snapshot changed after completion")
	}
	*after.Attempts[0].StatusCode, *after.Attempts[0].DurationMs, *after.Attempts[0].HeaderMs = 599, 123, 456
	after.Attempts[0].Endpoint = "/PRIVATE_MUTATION"
	*after.AccountSelectionMs, *after.AttemptsTruncated, *after.ErrorTruncated = 999, true, true
	unchanged := observation.Snapshot()
	if *unchanged.Attempts[0].StatusCode != 200 || *unchanged.Attempts[0].DurationMs != 0 || *unchanged.Attempts[0].HeaderMs != 0 || unchanged.Attempts[0].Endpoint != "/responses" || *unchanged.AccountSelectionMs != 3 || *unchanged.AttemptsTruncated || *unchanged.ErrorTruncated {
		t.Fatal("exported snapshot aliases mutable collector state")
	}
}

func TestGatewayAttemptObservationPathAllowlistAndNoCredentials(t *testing.T) {
	for _, test := range []struct{ uri, want string }{
		{"/responses", "/responses"}, {"/v1/responses?key=PRIVATE_QUERY", "/responses"},
		{"https://PRIVATE_USER:PRIVATE_PASSWORD@PRIVATE_HOST/backend-api/codex/responses/compact?secret=PRIVATE_TOKEN#PRIVATE_FRAGMENT", "/responses/compact"},
		{"/v1/models?client_version=0.155.0", "/models"}, {"https://PRIVATE_HOST/oauth/token", "/oauth/token"},
		{"/guardian", "/guardian"}, {"/backend-api/codex/guardian-classifier", "/guardian-classifier"},
		{"/private/responses", ""}, {"/responses/PRIVATE_ACCOUNT", ""}, {"/models/../responses", ""},
		{"/%72esponses", ""}, {"//PRIVATE_HOST/unrecognized", ""}, {"mailto:PRIVATE_TOKEN", ""}, {"PRIVATE_CREDENTIAL", ""},
	} {
		if got := gatewayObservedRoute(test.uri); got != test.want {
			t.Errorf("allowlisted endpoint mismatch for test with expected route %q", test.want)
		}
	}
	_, observation := withGatewayAttemptObservation(t.Context())
	attempt := observation.Begin("request", "https://PRIVATE_USER:PRIVATE_PASSWORD@PRIVATE_HOST/responses?token=PRIVATE_TOKEN", time.Now())
	attempt.Finish("PRIVATE_ERROR_SOURCE", "PRIVATE_ERROR_WITH_TOKEN", time.Now())
	encoded, _ := json.Marshal(observation.Snapshot())
	if bytes.Contains(encoded, []byte("PRIVATE_")) {
		t.Fatal("collector retained authority/query/raw error content")
	}
	got := observation.Snapshot().Attempts[0]
	if got.ErrorSource != "" || got.ErrorCode != "unknown" {
		t.Fatal("untrusted error text escaped fixed classification")
	}
}

func TestGatewayAttemptObservationModelsRemainDistinctAndBounded(t *testing.T) {
	_, observation := withGatewayAttemptObservation(t.Context())
	request := []byte(`{"model":"requested","input":"PRIVATE_PROMPT","tools":[{"arguments":"PRIVATE_ARGUMENTS"}],"headers":{"Authorization":"PRIVATE_BEARER"}}`)
	unchanged := bytes.Clone(request)
	observation.ObserveInbound("/responses", request)
	snapshot := observation.Snapshot()
	if snapshot.InboundModel != "requested" || snapshot.SentModel != "" || snapshot.ReturnedModel != "" || !bytes.Equal(request, unchanged) {
		t.Fatal("inbound metadata invented upstream observations or changed bytes")
	}
	observation.ObserveSent("/responses", []byte(`{"model":"sent-alias"}`))
	observation.ObserveReturned([]byte(`{"model":"event-wrong-scope","response":{"model":"actual","output":"PRIVATE_OUTPUT"},"error":"PRIVATE_RAW_ERROR"}`))
	snapshot = observation.Snapshot()
	if snapshot.InboundModel != "requested" || snapshot.SentModel != "sent-alias" || snapshot.ReturnedModel != "actual" {
		t.Fatal("model sources overwrite one another")
	}
	observation.ObserveReturned([]byte(`{"model":"event-wrong-scope","response":{}}`))
	if observation.Snapshot().ReturnedModel != "actual" {
		t.Fatal("missing nested model became top-level event model")
	}
	observation.ObserveReturned([]byte(`{"model":"event-wrong-scope","response":null}`))
	if observation.Snapshot().ReturnedModel != "actual" {
		t.Fatal("null nested response became an actual returned model")
	}
	encoded, _ := json.Marshal(snapshot)
	if bytes.Contains(encoded, []byte("PRIVATE_")) {
		t.Fatal("model observer retained non-model request/response content")
	}
	long := strings.Repeat(string(rune(0x754c)), 300)
	payload, _ := json.Marshal(map[string]string{"model": long})
	observation.ObserveSent("/responses", payload)
	bounded := observation.Snapshot().SentModel
	if len(bounded) > 512 || !utf8.ValidString(bounded) || bounded == "" {
		t.Fatal("model scalar is unbounded or invalid UTF-8")
	}
	for _, invalid := range []string{`{"model":null}`, `{"model":[]}`, `{"model":{}}`, `not-json`} {
		observation.ObserveReturned([]byte(invalid))
	}
	if observation.Snapshot().ReturnedModel != "actual" {
		t.Fatal("invalid model value changed an actual observation")
	}
}

func TestGatewayAttemptObservationStagesAndErrorTruncationAreExplicit(t *testing.T) {
	_, observation := withGatewayAttemptObservation(t.Context())
	start := time.Unix(1000, 0)
	observation.ObserveStage("request_preparation", start, start)
	observation.ObserveStage("account_selection", start, start.Add(4*time.Millisecond))
	observation.ObserveStage("proxy_selection", start, start.Add(2*time.Millisecond))
	observation.ObserveStage("account_selection", start, start.Add(time.Hour))
	observation.ObserveStage("dns_tls_guess", start, start.Add(time.Hour))
	snapshot := observation.Snapshot()
	if snapshot.RequestPreparationMs == nil || *snapshot.RequestPreparationMs != 0 || snapshot.AccountSelectionMs == nil || *snapshot.AccountSelectionMs != 4 || snapshot.ProxySelectionMs == nil || *snapshot.ProxySelectionMs != 2 {
		t.Fatal("explicit local stage observations changed")
	}
	if snapshot.ErrorTruncated != nil {
		t.Fatal("unobserved diagnostic truncation invented false")
	}
	observation.ObserveErrorTruncation(false)
	if got := observation.Snapshot().ErrorTruncated; got == nil || *got {
		t.Fatal("known untruncated diagnostic not explicit false")
	}
	observation.ObserveErrorTruncation(true)
	observation.ObserveErrorTruncation(false)
	if got := observation.Snapshot().ErrorTruncated; got == nil || !*got {
		t.Fatal("later diagnostic erased observed truncation")
	}
	_, missing := withGatewayAttemptObservation(t.Context())
	missing.ObserveStage("account_selection", time.Time{}, start)
	missing.ObserveStage("proxy_selection", start, start.Add(-time.Second))
	if got := missing.Snapshot(); got.AccountSelectionMs != nil || got.ProxySelectionMs != nil {
		t.Fatal("missing/invalid timestamps became measurements")
	}
}

func TestGatewayAttemptObservationPendingAndCanceledAttempt(t *testing.T) {
	ctx, observation := withGatewayAttemptObservation(t.Context())
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if gatewayAttemptObservationFrom(canceled) != observation {
		t.Fatal("cancellation removed metadata context")
	}
	attempt := observation.Begin("websocket", "/responses", time.Now())
	pending := observation.Snapshot()
	if len(pending.Attempts) != 1 || pending.Attempts[0].StatusCode != nil || pending.Attempts[0].DurationMs != nil || pending.Attempts[0].HeaderMs != nil {
		t.Fatal("unfinished handshake claimed completion")
	}
	attempt.Finish("client", "canceled", time.Now())
	complete := observation.Snapshot().Attempts[0]
	if complete.ErrorCode != "canceled" || complete.HeaderMs != nil || complete.StatusCode != nil || complete.DurationMs == nil {
		t.Fatal("canceled handshake acquired response headers")
	}
	if got := fmt.Sprint(complete.Attempt); got != "1" {
		t.Fatal("attempt sequence is not local and bounded")
	}
}
