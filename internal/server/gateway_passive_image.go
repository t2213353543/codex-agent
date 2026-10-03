package server

import "encoding/json"

func observedGatewayImageObject(raw json.RawMessage) map[string]json.RawMessage {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil {
		return nil
	}
	return object
}

// Read only explicit usage counters. No image body, URL, dimensions, inferred
// token count, or fallback price is consulted or retained.
func observeGatewayImageUsage(record *RequestRecord, object map[string]json.RawMessage) {
	usage := observedGatewayImageObject(object["usage"])
	tools := observedGatewayImageObject(object["tool_usage"])
	imageGen := observedGatewayImageObject(tools["image_gen"])
	for _, direction := range []struct {
		primary, compatible string
		value               *(*int64)
		source              *string
	}{
		{"input_tokens_details", "prompt_tokens_details", &record.ImageInputTokens, &record.ImageInputTokensSource},
		{"output_tokens_details", "completion_tokens_details", &record.ImageOutputTokens, &record.ImageOutputTokensSource},
	} {
		candidates := []struct {
			object        map[string]json.RawMessage
			field, source string
		}{
			{usage, direction.primary, "usage." + direction.primary + ".image_tokens"},
			{usage, direction.compatible, "usage." + direction.compatible + ".image_tokens"},
			{imageGen, direction.primary, "tool_usage.image_gen." + direction.primary + ".image_tokens"},
		}
		for rank, candidate := range candidates {
			details := observedGatewayImageObject(candidate.object[candidate.field])
			value := observedGatewayCount(details["image_tokens"])
			if value == nil {
				continue
			}
			// A later lower-priority summary must not overwrite a primary explicit
			// zero. Repeated terminal events replace a counter, never add it twice.
			oldRank := len(candidates)
			for i, old := range candidates {
				if old.source == *direction.source {
					oldRank = i
					break
				}
			}
			if *direction.value == nil || rank <= oldRank {
				*direction.value, *direction.source = value, candidate.source
			}
			break
		}
	}
}
