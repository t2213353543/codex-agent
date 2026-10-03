package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBPSSessionCommitFailureIsNotClientWriteOrDeliveredTTFT(t *testing.T) {
	for _, cause := range []error{errors.New("session account changed during authentication failover"), errors.New("session routing unavailable"), context.Canceled} {
		t.Run(cause.Error(), func(t *testing.T) {
			p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: "+`{"type":"response.created","response":{"id":"commit-test","model":"gpt-6-astra","status":"in_progress","output":[]}}`+"\n\n")
				_, _ = io.WriteString(w, "data: "+`{"type":"response.output_text.delta","response_id":"commit-test","delta":"PRIVATE_NOT_DELIVERED"}`+"\n\n")
			})
			out := httptest.NewRecorder()
			state := &officialFailoverState{started: time.Now(), commit: func() error { return cause }}
			writer := &officialFailoverWriter{target: out, header: make(http.Header), state: state}
			req := proxyRequest(http.MethodPost, "/v1/responses", `{"model":"gpt-6-astra","stream":true,"input":"synthetic"}`)
			req = req.WithContext(context.WithValue(req.Context(), officialFailoverKey{}, state))
			var recovered any
			func() { defer func() { recovered = recover() }(); p.gatewayBPS(writer, req) }()
			if recovered != http.ErrAbortHandler || out.Body.Len() != 0 || writer.delivered || state.committed || !errors.Is(writer.commitErr, cause) {
				t.Fatalf("commit barrier changed: panic=%v delivered=%t err=%v", recovered, writer.delivered, writer.commitErr)
			}
			rec := state.record
			if rec == nil || rec.Status != "error" || rec.ErrorSource != "gateway" || rec.ErrorPhase != "session_binding" || rec.TTFTMs != nil || rec.FirstOutputKind != "" || rec.UsageKnown != nil {
				t.Fatalf("binding rejection misreported: %+v", rec)
			}
			if rec.Error != cause.Error() || strings.Contains(out.Body.String(), "PRIVATE_NOT_DELIVERED") {
				t.Fatal("cause changed or output leaked")
			}
		})
	}
}

func TestBPSSessionCommitErrorKeepsSuccessfulAndClientWriteBoundaries(t *testing.T) {
	state := &officialFailoverState{commit: func() error { return nil }}
	if state.commitOwner() != nil || state.commitOwner() != nil || !state.committed {
		t.Fatal("successful commit changed")
	}
	known := int64(7)
	rec := RequestRecord{TTFTMs: &known, FirstOutputKind: "response.output_text.delta"}
	w := gatewayResponseWriter{record: &rec}
	w.observeWriteError(io.ErrClosedPipe)
	if rec.ErrorSource != "client" || rec.ErrorPhase != "response_write" || rec.TTFTMs == nil {
		t.Fatal("actual client write misclassified")
	}
	if observeGatewaySessionCommitError(&rec, errors.New("session account changed during authentication failover")) {
		t.Fatal("free-form text classified as binding error")
	}
}
