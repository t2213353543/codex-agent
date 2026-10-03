package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestBPSOAuthActualTransportFailureKeepsProducerCode(t *testing.T) {
	var sends atomic.Int32
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		sends.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		connection, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_ = connection.Close() // a real accepted TCP request ends before headers
	})
	w := httptest.NewRecorder()
	r := proxyRequest(http.MethodPost, "/v1/responses", `{"model":"gpt-6-astra","stream":false,"input":"synthetic transport accounting"}`)
	p.gatewayBPS(w, r)
	record, ok := p.Metrics.LastFor("test")
	if !ok || sends.Load() != 1 || w.Code < 400 || len(record.Attempts) != 1 {
		t.Fatalf("fixture did not reach single real failed request: exists=%t sends=%d status=%d attempts=%d", ok, sends.Load(), w.Code, len(record.Attempts))
	}
	a := record.Attempts[0]
	if a.ObservedHTTPStatusCode != nil {
		t.Fatal("headers were invented for failed transport")
	}
	t.Logf("actual_network_sends=%d record_status=%s source=%s attempt_source=%s attempt_code=%q observed_headers=false schema=%d", sends.Load(), record.Status, record.ErrorSource, a.ErrorSource, a.ErrorCode, record.ObservationSchemaVersion)
	if a.ErrorSource != "transport" || a.ErrorCode != "transport" {
		t.Fatalf("v5 overwrote producer transport evidence: source=%q code=%q", a.ErrorSource, a.ErrorCode)
	}
}

func TestBPSOAuthCollectorTransportCodesAreNotLogicalSuccess(t *testing.T) {
	for _, tc := range []struct {
		name         string
		err          error
		source, code string
	}{{"deadline", context.DeadlineExceeded, "transport", "deadline"}, {"canceled", context.Canceled, "client", "canceled"}, {"connection", errors.New("synthetic connection failure"), "transport", "transport"}} {
		t.Run(tc.name, func(t *testing.T) {
			_, observer := withGatewayAttemptObservation(context.Background())
			h := observer.Begin("request", "/responses", time.Now())
			finishGatewayObservedAttempt(h, nil, tc.err)
			before := observer.Snapshot().Attempts[0]
			if before.ErrorCode != tc.code || before.ErrorSource != tc.source {
				t.Fatal("producer fixture failed")
			}
			s := officialFailoverState{}
			record := RequestRecord{Status: "error", HTTPCode: 502, ErrorSource: tc.source, SentModel: "gpt-6-astra", UpstreamPath: "/responses"}
			s.captureActualAttempts(observer, &record)
			s.recordAttempts(&record, nil)
			after := record.Attempts[0]
			if after.ErrorCode != before.ErrorCode || after.ErrorSource != before.ErrorSource {
				t.Fatalf("producer %s/%s became %s/%s", before.ErrorSource, before.ErrorCode, after.ErrorSource, after.ErrorCode)
			}
		})
	}
}

func TestBPSOAuthRefreshOnlyAndTruncatedPrefixRemainTruthful(t *testing.T) {
	_, observer := withGatewayAttemptObservation(context.Background())
	h := observer.Begin("refresh", "/oauth/token", time.Now())
	h.Headers(400, time.Now())
	h.Finish("upstream", "http_status", time.Now())
	s := officialFailoverState{}
	record := RequestRecord{Status: "error", HTTPCode: 503, ErrorSource: "gateway"}
	s.captureActualAttempts(observer, &record)
	s.recordAttempts(&record, nil)
	if len(record.Attempts) != 1 || record.Attempts[0].Phase != "refresh" || *record.Attempts[0].ObservedHTTPStatusCode != 400 || *record.Attempts[0].StatusCode != 400 || record.FinalUpstreamErrorCode != "" {
		t.Fatal("refresh-only event relabeled as generation")
	}
	_, observer = withGatewayAttemptObservation(context.Background())
	for i := 0; i < 6; i++ {
		h = observer.Begin("request", "/responses", time.Now())
		status := 401
		if i == 5 {
			status = 200
		}
		h.Headers(status, time.Now())
		code := "http_status"
		if status == 200 {
			code = ""
		}
		h.Finish("upstream", code, time.Now())
	}
	s = officialFailoverState{}
	record = RequestRecord{Status: "ok", HTTPCode: 200, SentModel: "gpt-6-astra", UpstreamPath: "/responses"}
	s.captureActualAttempts(observer, &record)
	s.recordAttempts(&record, nil)
	if len(record.Attempts) != 4 || record.AttemptsTruncated == nil || !*record.AttemptsTruncated {
		t.Fatal("truncation lost")
	}
	for _, a := range record.Attempts {
		if *a.StatusCode != 401 || *a.ObservedHTTPStatusCode != 401 || a.ErrorCode != "http_status" {
			t.Fatal("truncated prefix rewritten by hidden success")
		}
	}
}

func TestBPSOAuthAbortedAndSuccessfulFinalRequestsStayDistinct(t *testing.T) {
	for _, status := range []string{"aborted", "error", "ok"} {
		t.Run(status, func(t *testing.T) {
			_, observer := withGatewayAttemptObservation(context.Background())
			h := observer.Begin("request", "/responses", time.Now())
			wantCode := "canceled"
			source := "client"
			httpCode := 499
			if status == "ok" {
				h.Headers(200, time.Now())
				wantCode, source, httpCode = "", "", 200
			}
			h.Finish(source, wantCode, time.Now())
			s := officialFailoverState{}
			record := RequestRecord{Status: status, HTTPCode: httpCode, ErrorSource: source, SentModel: "gpt-6-astra", UpstreamPath: "/responses"}
			s.captureActualAttempts(observer, &record)
			s.recordAttempts(&record, nil)
			if record.Attempts[0].ErrorCode != wantCode || record.FinalUpstreamErrorCode != "" {
				t.Fatalf("final %s producer code changed: %+v", status, record.Attempts[0])
			}
		})
	}
}
