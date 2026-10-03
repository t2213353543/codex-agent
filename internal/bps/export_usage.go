package bps

import (
	"bytes"
	"encoding/json"
	"strconv"
)

// ExportCacheCreationAsRead projects known native cache creation into the
// client's cache-read billing bucket. It must only run at the final client
// write boundary, after all observers, history and result accounting. It does
// not mutate raw, infer missing counters, or change total input/output tokens.
// An invalid, ambiguous, unknown or unchanged projection returns raw itself.
func ExportCacheCreationAsRead(raw []byte) []byte {
	return exportCacheCreationAsRead(raw, true)
}

// ExportResponseCacheCreationAsRead accepts a native Response object, whose
// unknown extensions may themselves contain a property named response.
func ExportResponseCacheCreationAsRead(raw []byte) []byte {
	return exportCacheCreationAsRead(raw, false)
}

func exportCacheCreationAsRead(raw []byte, event bool) []byte {
	// Ordinary deltas contain no cache-write counter. Avoid parsing or copying
	// them; escaped JSON member names remain supported on the slow path.
	if !bytes.Contains(raw, []byte("cache_write_tokens")) && !bytes.Contains(raw, []byte(`\u`)) {
		return raw
	}
	if !json.Valid(raw) {
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
	if !unique || input.end == 0 || details.end == 0 {
		return raw
	}
	read, write, unique := exportUsageFields(usageRaw[details.start:details.end], "cached_tokens", "cache_write_tokens")
	if !unique || read.end == 0 || write.end == 0 {
		return raw
	}
	inputCount, inputOK := exportUsageCount(usageRaw[input.start:input.end])
	detailRaw := usageRaw[details.start:details.end]
	readCount, readOK := exportUsageCount(detailRaw[read.start:read.end])
	writeCount, writeOK := exportUsageCount(detailRaw[write.start:write.end])
	// Subtract before adding to check both int64 overflow and the total-input
	// bound. Zero writes need no edit and make the transformation idempotent.
	if !inputOK || !readOK || !writeOK || writeCount == 0 || readCount > inputCount || writeCount > inputCount-readCount {
		return raw
	}
	base += details.start
	read.start, read.end = read.start+base, read.end+base
	write.start, write.end = write.start+base, write.end+base
	var number [20]byte
	readValue := strconv.AppendInt(number[:0], readCount+writeCount, 10)
	first, second := read, write
	firstValue, secondValue := readValue, []byte("0")
	if second.start < first.start {
		first, second = second, first
		firstValue, secondValue = secondValue, firstValue
	}
	out := make([]byte, 0, len(raw)-(first.end-first.start)-(second.end-second.start)+len(firstValue)+len(secondValue))
	out = append(out, raw[:first.start]...)
	out = append(out, firstValue...)
	out = append(out, raw[first.end:second.start]...)
	out = append(out, secondValue...)
	out = append(out, raw[second.end:]...)
	return out
}

func exportUsageCount(raw []byte) (int64, bool) {
	value, err := strconv.ParseInt(string(raw), 10, 64)
	return value, err == nil && value >= 0
}

type exportUsageSpan struct{ start, end int }

// raw must be validated JSON. Only the selected object's direct members are
// inspected; nested output/tool arguments never become accounting candidates.
// Reject duplicate selected members rather than depend on parser precedence.
func exportUsageFields(raw []byte, first, second string) (a, b exportUsageSpan, unique bool) {
	position := exportUsageSpace(raw, 0)
	if position >= len(raw) || raw[position] != '{' {
		return a, b, false
	}
	position = exportUsageSpace(raw, position+1)
	for raw[position] != '}' {
		nameStart := position
		nameEnd := exportUsageStringEnd(raw, position)
		position = exportUsageSpace(raw, nameEnd)
		position = exportUsageSpace(raw, position+1)
		valueEnd := exportUsageValueEnd(raw, position)
		name := raw[nameStart:nameEnd]
		if first != "" && exportUsageName(name, first) {
			if a.end != 0 {
				return a, b, false
			}
			a = exportUsageSpan{position, valueEnd}
		} else if second != "" && exportUsageName(name, second) {
			if b.end != 0 {
				return a, b, false
			}
			b = exportUsageSpan{position, valueEnd}
		}
		position = exportUsageSpace(raw, valueEnd)
		if raw[position] == ',' {
			position = exportUsageSpace(raw, position+1)
		}
	}
	return a, b, true
}

func exportUsageName(raw []byte, want string) bool {
	if bytes.Equal(raw[1:len(raw)-1], []byte(want)) {
		return true
	}
	if len(raw) > 6*len(want)+2 || bytes.IndexByte(raw, '\\') < 0 {
		return false
	}
	var name string
	return json.Unmarshal(raw, &name) == nil && name == want
}

func exportUsageSpace(raw []byte, position int) int {
	for position < len(raw) {
		switch raw[position] {
		case ' ', '\t', '\r', '\n':
			position++
		default:
			return position
		}
	}
	return position
}

func exportUsageStringEnd(raw []byte, position int) int {
	for position++; ; {
		quote := position + bytes.IndexByte(raw[position:], '"')
		backslashes := 0
		for before := quote - 1; raw[before] == '\\'; before-- {
			backslashes++
		}
		if backslashes%2 == 0 {
			return quote + 1
		}
		position = quote + 1
	}
}

func exportUsageValueEnd(raw []byte, position int) int {
	switch raw[position] {
	case '"':
		return exportUsageStringEnd(raw, position)
	case '{', '[':
		depth := 0
		for ; ; position++ {
			switch raw[position] {
			case '"':
				position = exportUsageStringEnd(raw, position) - 1
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return position + 1
				}
			}
		}
	default:
		for position < len(raw) {
			switch raw[position] {
			case ' ', '\t', '\r', '\n', ',', '}', ']':
				return position
			}
			position++
		}
		return position
	}
}
