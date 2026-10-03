package bps

import (
	"bytes"
	"encoding/json"
	"strconv"
)

// ExportCacheReadRatio projects the requested percentage of known input tokens
// into native cached_tokens at the final client event write boundary. It must
// run after observers/history/accounting and after any cache-creation export.
// It never modifies raw or any other token counter, including cache writes.
func ExportCacheReadRatio(raw []byte, percent int) []byte {
	return exportCacheReadRatio(raw, percent, true)
}

// ExportResponseCacheReadRatio accepts a native Response object. Its unknown
// extension named response is not an event envelope and remains untouched.
func ExportResponseCacheReadRatio(raw []byte, percent int) []byte {
	return exportCacheReadRatio(raw, percent, false)
}

func exportCacheReadRatio(raw []byte, percent int, event bool) []byte {
	if percent < 0 || percent > 100 ||
		(!bytes.Contains(raw, []byte("input_tokens")) && !bytes.Contains(raw, []byte(`\u`))) || !json.Valid(raw) {
		return raw
	}
	responseField := ""
	if event {
		responseField = "response"
	}
	response, usage, unique := exportUsageFields(raw, responseField, "usage")
	if !unique {
		return raw
	}
	base := 0
	if response.end != 0 {
		_, nestedUsage, unique := exportUsageFields(raw[response.start:response.end], "", "usage")
		if !unique {
			return raw
		}
		usage = nestedUsage
		base = response.start
	}
	if usage.end == 0 {
		return raw
	}
	base += usage.start
	usageRaw := raw[base : base+usage.end-usage.start]
	input, details, unique := exportUsageFields(usageRaw, "input_tokens", "input_tokens_details")
	if !unique || input.end == 0 {
		return raw
	}
	inputCount, known := exportUsageCount(usageRaw[input.start:input.end])
	if !known {
		return raw
	}
	// Split before multiplying: even MaxInt64 input at 100% cannot overflow.
	p := int64(percent)
	cached := inputCount/100*p + inputCount%100*p/100
	var number [20]byte
	value := strconv.AppendInt(number[:0], cached, 10)
	if details.end == 0 {
		member := append([]byte(`"input_tokens_details":{"cached_tokens":`), value...)
		member = append(member, '}')
		return exportUsageAppendMember(raw, base, usageRaw, member)
	}
	detailRaw := usageRaw[details.start:details.end]
	read, _, unique := exportUsageFields(detailRaw, "cached_tokens", "")
	if !unique {
		return raw
	}
	base += details.start
	if read.end == 0 {
		member := append([]byte(`"cached_tokens":`), value...)
		return exportUsageAppendMember(raw, base, detailRaw, member)
	}
	if previous, ok := exportUsageCount(detailRaw[read.start:read.end]); ok && previous == cached {
		return raw
	}
	start, end := base+read.start, base+read.end
	out := make([]byte, 0, len(raw)-(end-start)+len(value))
	out = append(out, raw[:start]...)
	out = append(out, value...)
	out = append(out, raw[end:]...)
	return out
}

// object is a validated object span ending in }. Insert immediately before its
// closing brace so existing whitespace, escaped names and extensions survive.
func exportUsageAppendMember(raw []byte, base int, object, member []byte) []byte {
	comma := object[exportUsageSpace(object, exportUsageSpace(object, 0)+1)] != '}'
	position := base + len(object) - 1
	extra := len(member)
	if comma {
		extra++
	}
	out := make([]byte, 0, len(raw)+extra)
	out = append(out, raw[:position]...)
	if comma {
		out = append(out, ',')
	}
	out = append(out, member...)
	out = append(out, raw[position:]...)
	return out
}
