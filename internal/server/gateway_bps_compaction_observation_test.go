package server

import (
	"testing"
)

func TestBPSDispatchedWebSocketCompactionObservation(t *testing.T) {
	for _, tc := range []struct {
		body        string
		known, want bool
	}{
		{`{"stream":true,"input":[{"type":"compaction_trigger"}]}`, true, true},
		{`{"stream":true,"input":[{"type":"compaction","encrypted_content":"opaque"}]}`, true, false},
		{`{"type":"response.create","input":[{"type":"compaction_trigger"}]}`, true, true},
		{`{"stream":false,"input":[{"type":"compaction_trigger"}]}`, false, false},
		{`{"stream":"true","input":[{"type":"compaction_trigger"}]}`, false, false},
		{`{"type":"response.cancel","stream":true,"input":[{"type":"compaction_trigger"}]}`, false, false},
	} {
		rec := RequestRecord{Endpoint: "responses"}
		observeGatewayRequest(&rec, "synthetic", "websocket", []byte(tc.body))
		if (rec.NativeCompaction != nil) != tc.known || tc.known && *rec.NativeCompaction != tc.want {
			t.Fatalf("compaction observation wrong: body=%s observed=%v", tc.body, rec.NativeCompaction)
		}
	}
}
