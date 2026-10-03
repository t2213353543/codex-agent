package server

import (
	"crypto/sha256"
	"encoding/json"
	"math"
	"strings"
	"time"
)

// A sideband can carry multiple responses. Only upstream JSON is inspected;
// message bytes, media frames and client-owned protocol state stay untouched.
type appServerRealtimeObservation struct {
	record          *RequestRecord
	started         time.Time
	seen            map[[32]byte]bool
	cacheIncomplete bool
	totalIncomplete bool
	usageOverflow   bool
}

func (o *appServerRealtimeObservation) eventAt(raw []byte, now time.Time) {
	var event struct {
		Type       string `json:"type"`
		Delta      string `json:"delta"`
		Audio      string `json:"audio"`
		Data       string `json:"data"`
		Text       string `json:"text"`
		Transcript string `json:"transcript"`
		Item       struct {
			Text string `json:"text"`
		} `json:"item"`
		Response struct {
			ID    string                     `json:"id"`
			Model string                     `json:"model"`
			Usage map[string]json.RawMessage `json:"usage"`
		} `json:"response"`
	}
	if o.record == nil || json.Unmarshal(raw, &event) != nil {
		return
	}
	output := false
	switch event.Type {
	case "response.output_text.delta", "response.output_audio_transcript.delta", "conversation.output_transcript.delta", "response.output_audio.delta", "response.audio.delta", "conversation.output_audio.delta":
		output = event.Delta != "" || event.Data != ""
	case "response.output_text.done":
		output = event.Text != ""
	case "response.output_audio_transcript.done":
		output = event.Transcript != ""
	case "output_transcript.added":
		output = event.Item.Text != ""
	case "output_audio.delta":
		output = event.Audio != ""
	}
	if output && o.record.TTFTMs == nil && !o.started.IsZero() {
		value := max(int64(0), now.Sub(o.started).Milliseconds())
		o.record.TTFTMs, o.record.FirstOutputKind = &value, event.Type
	}
	if event.Type != "response.done" && event.Type != "response.completed" {
		return
	}
	if event.Response.Model != "" {
		o.record.ReturnedModel = boundedGatewayMetadata(event.Response.Model, 512)
		o.record.UpstreamModel, o.record.UpstreamModelSource = o.record.ReturnedModel, "realtime_response"
	}
	if event.Response.ID == "" || event.Response.Usage == nil {
		return
	}
	digest := sha256.Sum256([]byte(event.Response.ID))
	if o.seen[digest] {
		return
	}
	usage := event.Response.Usage
	input, outputCount := observedGatewayCount(usage["input_tokens"]), observedGatewayCount(usage["output_tokens"])
	if input == nil || outputCount == nil {
		return
	}
	var details map[string]json.RawMessage
	detailRaw, hasDetails := usage["input_token_details"]
	if !hasDetails {
		detailRaw = usage["input_tokens_details"]
	}
	if len(detailRaw) > 0 && json.Unmarshal(detailRaw, &details) != nil {
		return
	}
	var cached *int64
	if raw, ok := details["cached_tokens"]; ok {
		cached = observedGatewayCount(raw)
		if cached == nil {
			return
		}
	}
	if *input > math.MaxInt64-*outputCount || *input > math.MaxInt64-o.record.InputTokens || *outputCount > math.MaxInt64-o.record.OutputTokens || cached != nil && *cached > math.MaxInt64-o.record.CachedTokens {
		o.usageOverflow, o.totalIncomplete = true, true
		known := false
		o.record.UsageKnown, o.record.CachedTokensKnown = &known, &known
		o.record.UsageTotalTokens = nil
		o.record.UsageSchemaSource = "realtime_response_usage_overflow"
		return
	}
	if o.seen == nil {
		o.seen = make(map[[32]byte]bool)
	}
	o.seen[digest] = true
	o.record.InputTokens += *input
	o.record.OutputTokens += *outputCount
	if cached != nil {
		o.record.CachedTokens += *cached
	} else {
		o.cacheIncomplete = true
	}
	known := !o.usageOverflow
	o.record.UsageKnown = &known
	o.record.UsageSchemaSource = "realtime_response_usage"
	if o.cacheIncomplete {
		o.record.UsageSchemaSource += "_cache_unreported"
	}
	if o.usageOverflow {
		o.record.UsageSchemaSource = "realtime_response_usage_overflow"
	}
	cacheKnown := !o.cacheIncomplete && !o.usageOverflow
	o.record.CachedTokensKnown = &cacheKnown
	o.record.ResponseID = boundedGatewayMetadata(event.Response.ID, 1024)
	// No estimated total, cost, cache or audio-token count is manufactured.
	if total := observedGatewayCount(usage["total_tokens"]); total != nil && !o.totalIncomplete {
		if o.record.UsageTotalTokens == nil {
			value := int64(0)
			o.record.UsageTotalTokens = &value
		}
		if *total <= math.MaxInt64-*o.record.UsageTotalTokens {
			*o.record.UsageTotalTokens += *total
		} else {
			o.totalIncomplete = true
			o.record.UsageTotalTokens = nil
		}
	} else {
		o.totalIncomplete = true
		o.record.UsageTotalTokens = nil
	}
	if strings.TrimSpace(o.record.Model) == "" && o.record.ReturnedModel != "" {
		o.record.Model = o.record.ReturnedModel
	}
}
