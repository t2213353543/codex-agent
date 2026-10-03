package server

import (
	"testing"
	"time"
)

func TestAppServerRealtimeSyntheticUsageMultipleResponsesAndTTFT(t *testing.T) {
	start := time.Unix(1700000000, 0)
	rec := RequestRecord{}
	watch := appServerRealtimeObservation{record: &rec, started: start}
	watch.eventAt([]byte(`{"type":"session.updated","session":{"id":"session"}}`), start.Add(time.Millisecond))
	if rec.TTFTMs != nil || rec.UsageKnown != nil {
		t.Fatal("handshake invented model output/usage")
	}
	watch.eventAt([]byte(`{"type":"response.output_audio_transcript.delta","delta":"actual synthetic output"}`), start.Add(73*time.Millisecond))
	first := []byte(`{"type":"response.done","response":{"id":"rt-1","model":"returned-realtime-model","usage":{"input_tokens":132,"output_tokens":121,"total_tokens":253,"input_token_details":{"cached_tokens":64,"audio_tokens":13}}}}`)
	watch.eventAt(first, start.Add(time.Second))
	watch.eventAt(first, start.Add(2*time.Second))
	watch.eventAt([]byte(`{"type":"response.completed","response":{"id":"rt-2","model":"returned-realtime-model","usage":{"input_tokens":10,"output_tokens":4,"total_tokens":14,"input_tokens_details":{"cached_tokens":0}}}}`), start.Add(3*time.Second))
	if rec.InputTokens != 142 || rec.OutputTokens != 125 || rec.CachedTokens != 64 || rec.UsageKnown == nil || !*rec.UsageKnown || rec.CachedTokensKnown == nil || !*rec.CachedTokensKnown {
		t.Fatalf("usage not observed/deduplicated %+v", rec)
	}
	if rec.TTFTMs == nil || *rec.TTFTMs != 73 || rec.FirstOutputKind != "response.output_audio_transcript.delta" {
		t.Fatal("TTFT used handshake/session elapsed")
	}
	if rec.ReturnedModel != "returned-realtime-model" || rec.UpstreamModelSource != "realtime_response" || rec.UsageTotalTokens == nil || *rec.UsageTotalTokens != 267 {
		t.Fatal("returned metadata missing")
	}
}

func TestAppServerRealtimeSyntheticUsageMissingIsUnknown(t *testing.T) {
	rec := RequestRecord{}
	watch := appServerRealtimeObservation{record: &rec}
	for _, raw := range []string{`{"type":"response.done","response":{"id":"none"}}`, `{"type":"response.done","response":{"id":"null","usage":null}}`, `{"type":"response.done","response":{"id":"bad","usage":{"input_tokens":-1,"output_tokens":3}}}`} {
		watch.eventAt([]byte(raw), time.Now())
	}
	if rec.UsageKnown != nil || rec.ReturnedModel != "" || rec.CachedTokens != 0 {
		t.Fatal("missing usage/model invented")
	}
	watch.eventAt([]byte(`{"type":"response.done","response":{"id":"partial","usage":{"input_tokens":9,"output_tokens":3}}}`), time.Now())
	if rec.InputTokens != 9 || rec.OutputTokens != 3 || rec.UsageSchemaSource != "realtime_response_usage_cache_unreported" || rec.CachedTokensKnown == nil || *rec.CachedTokensKnown {
		t.Fatal("actual counts lost or missing cache claimed known")
	}
	watch.eventAt([]byte(`{"type":"response.done","response":{"id":"later","usage":{"input_tokens":4,"output_tokens":1,"input_token_details":{"cached_tokens":2}}}}`), time.Now())
	if rec.CachedTokens != 2 || rec.UsageSchemaSource != "realtime_response_usage_cache_unreported" {
		t.Fatal("partial cache total claimed complete")
	}
}

func TestAppServerRealtimeV2OutputAliases(t *testing.T) {
	for _, event := range []string{`{"type":"response.audio.delta","delta":"base64synthetic"}`, `{"type":"response.output_text.done","text":"synthetic answer"}`, `{"type":"response.output_audio_transcript.done","transcript":"synthetic answer"}`} {
		start := time.Unix(1700000000, 0)
		rec := &RequestRecord{}
		watch := appServerRealtimeObservation{record: rec, started: start}
		watch.eventAt([]byte(event), start.Add(12*time.Millisecond))
		if rec.TTFTMs == nil || *rec.TTFTMs != 12 {
			t.Fatalf("official v2 output alias not observed: %s", event)
		}
	}
}
