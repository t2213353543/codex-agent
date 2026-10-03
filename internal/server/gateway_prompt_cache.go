package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// Match Sub2API's ordinary OAuth account-identity scoping, not its opt-in
// fingerprint convergence: every original key keeps its own stable identity.
// Reference: Sub2API 49783993f3b8d3df50a4a3da67d589d106c4ccae,
// backend/internal/service/openai_codex_account_identity.go.
// Request content, cache options, history, and local account bindings remain
// unchanged. Only the selected outbound cache/session/thread values are scoped.
func prepareGatewayAccountPromptCache(prepared []byte, keyID string, account Account, settings Settings, unbounded ...bool) ([]byte, error) {
	return prepareGatewayAccountPromptCacheWithKind(prepared, keyID, account, settings, "", unbounded...)
}

// WebSocket partial continuations can omit client_metadata. Reuse the original
// response's key kind so the same cache key is not reclassified on a later turn.
func prepareGatewayAccountPromptCacheWithKind(prepared []byte, keyID string, account Account, settings Settings, overrideKind string, unbounded ...bool) ([]byte, error) {
	if !settings.PromptCacheKeyEnabled || keyID == "" ||
		(!gatewayCompleteOwner(account) && account.AccessToken == "" && account.RefreshToken == "") {
		return prepared, nil
	}
	// Resolve credentials only if the scanner finds a scoped value, and at
	// most once for a request. Missing-key/content-only requests should neither
	// allocate owner fingerprints nor repeat hashing for every metadata alias.
	var namespace string
	edits := gatewayPromptCacheIdentityEdits(prepared, func(kind, raw string) string {
		if namespace == "" {
			namespace = gatewayCacheOwnerNamespace(account)
		}
		return gatewayScopeCacheIdentityInNamespace(namespace, keyID, kind, raw)
	}, overrideKind)
	length := gatewayJSONEditedLength(prepared, edits)
	if length > maxBody && !gatewayPayloadUnboundedOption(unbounded) {
		return nil, errGatewayPreparedBodyLimit
	}
	return gatewayApplyJSONEdits(prepared, edits, length), nil
}

// Credential identity is independent of deployment-relative account rows and
// local authorization revisions. Known-owner token refreshes keep this value.
// Unlike Sub2API's workspace-only fallback, this gateway requires the user too:
// an imported workspace may contain multiple users. Its existing irreversible
// credential digest partitions incomplete owners without exposing tokens.
func gatewayCacheOwnerNamespace(account Account) string {
	workspace := strings.TrimSpace(account.AccountID)
	if gatewayCompleteOwner(account) {
		return "chatgpt:" + workspace + ":user:" + strings.TrimSpace(account.Identity.UserID)
	}
	if account.AccessToken == "" && account.RefreshToken == "" {
		return ""
	}
	return "chatgpt:" + workspace + ":credentials:" + gatewayUnknownOwnerCredentials(account)
}

func gatewayScopeCacheIdentityValue(account Account, keyID, kind, raw string) string {
	return gatewayScopeCacheIdentityInNamespace(gatewayCacheOwnerNamespace(account), keyID, kind, raw)
}

func gatewayScopeCacheIdentityInNamespace(namespace, keyID, kind, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || namespace == "" {
		return raw
	}
	// Use a length-framed JSON tuple because this gateway's API-key IDs are
	// strings, while Sub2API's equivalent input is a numeric database ID.
	seed, _ := json.Marshal([]string{"codex-agent:codex-account-identity:v1", keyID, namespace, kind, raw})
	sum := sha256.Sum256(seed)
	sum[6] = (sum[6] & 0x0f) | 0x40
	sum[8] = (sum[8] & 0x3f) | 0x80
	value := hex.EncodeToString(sum[:16])
	return value[:8] + "-" + value[8:12] + "-" + value[12:16] + "-" + value[16:20] + "-" + value[20:]
}

// Sub2API builds underscore session/conversation headers separately from the
// body identity UUIDs. Keep the same tenant + credential + original-ID scope.
func gatewayUpstreamCacheSession(keyID string, account Account, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	namespace := gatewayCacheOwnerNamespace(account)
	seed := "u" + keyID + ":" + raw
	if namespace != "" {
		seed = "u" + keyID + ":a" + namespace + ":" + raw
	}
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:8])
}

func gatewayPromptCacheKeyValue(prepared []byte) string {
	if !json.Valid(prepared) {
		return ""
	}
	fields := gatewayValidatedJSONFieldSpans(prepared, "prompt_cache_key", "")
	value := gatewayJSONSpanString(prepared, fields[0])
	if strings.TrimSpace(value) == "" {
		return ""
	}
	return value
}

type gatewayJSONEdit struct {
	gatewayJSONFieldSpan
	replacement []byte
}

func gatewayJSONSpanString(raw []byte, span gatewayJSONFieldSpan) string {
	if span.end == 0 {
		return ""
	}
	var value string
	_ = json.Unmarshal(raw[span.start:span.end], &value)
	return value
}

func gatewayPromptCacheIdentityEdits(prepared []byte, scope func(kind, raw string) string, overrideKind string) []gatewayJSONEdit {
	if !json.Valid(prepared) {
		return nil
	}
	fields := gatewayValidatedJSONFieldSpans(prepared, "prompt_cache_key", "client_metadata")
	var edits []gatewayJSONEdit
	session := ""
	if metadata := fields[1]; metadata.end != 0 {
		raw := prepared[metadata.start:metadata.end]
		identity := gatewayValidatedJSONFieldSpans(raw, "session_id", "x-codex-turn-metadata")
		session = gatewayJSONSpanString(raw, identity[0])
		edits = gatewayCacheMetadataEdits(raw, metadata.start, scope, edits)
		// Embedded metadata is a JSON-encoded string. Locate edits in its
		// decoded small object, then map the spans back to the ORIGINAL string
		// bytes. Unknown escape spellings and large integers stay byte-exact.
		if embedded := identity[1]; embedded.end != 0 {
			encoded := raw[embedded.start:embedded.end]
			var decoded string
			if json.Unmarshal(encoded, &decoded) == nil && json.Valid([]byte(decoded)) {
				for _, edit := range gatewayCacheMetadataEdits([]byte(decoded), 0, scope, nil) {
					quoted, _ := json.Marshal(string(edit.replacement))
					edits = append(edits, gatewayJSONEdit{
						gatewayJSONFieldSpan{
							metadata.start + embedded.start + gatewayJSONEncodedStringOffset(encoded, edit.start),
							metadata.start + embedded.start + gatewayJSONEncodedStringOffset(encoded, edit.end),
						}, quoted[1 : len(quoted)-1],
					})
				}
			}
		}
	}
	if key := gatewayJSONSpanString(prepared, fields[0]); strings.TrimSpace(key) != "" {
		kind := "prompt-cache"
		if strings.TrimSpace(session) != "" && key == session {
			kind = "session"
		}
		if overrideKind == "session" || overrideKind == "prompt-cache" {
			kind = overrideKind
		}
		replacement, _ := json.Marshal(scope(kind, key))
		edits = append(edits, gatewayJSONEdit{fields[0], replacement})
	}
	return edits
}

func gatewayCacheMetadataEdits(raw []byte, offset int, scope func(kind, raw string) string, edits []gatewayJSONEdit) []gatewayJSONEdit {
	for _, fields := range [][3]string{{"session_id", "session-id", "session"}, {"thread_id", "thread-id", "thread"}} {
		for _, span := range gatewayValidatedJSONFieldSpans(raw, fields[0], fields[1]) {
			value := gatewayJSONSpanString(raw, span)
			if strings.TrimSpace(value) == "" {
				continue
			}
			replacement, _ := json.Marshal(scope(fields[2], value))
			edits = append(edits, gatewayJSONEdit{gatewayJSONFieldSpan{offset + span.start, offset + span.end}, replacement})
		}
	}
	return edits
}

// encoded is a validated JSON string and decodedOffset is a token boundary in
// the string produced by encoding/json. Unicode surrogate escapes and invalid
// UTF-8 follow that decoder, so source offsets do not depend on escape spelling.
func gatewayJSONEncodedStringOffset(encoded []byte, decodedOffset int) int {
	position, decoded := 1, 0
	for decoded < decodedOffset {
		switch {
		case encoded[position] == '\\':
			if encoded[position+1] != 'u' {
				position += 2
				decoded++
				continue
			}
			code, _ := strconv.ParseUint(string(encoded[position+2:position+6]), 16, 16)
			r := rune(code)
			position += 6
			if utf16.IsSurrogate(r) {
				next := utf8.RuneError
				if position+6 <= len(encoded)-1 && encoded[position] == '\\' && encoded[position+1] == 'u' {
					code, _ := strconv.ParseUint(string(encoded[position+2:position+6]), 16, 16)
					next = utf16.DecodeRune(r, rune(code))
					if next != utf8.RuneError {
						position += 6
					}
				}
				r = next
			}
			decoded += utf8.RuneLen(r)
		case encoded[position] < utf8.RuneSelf:
			position++
			decoded++
		default:
			r, size := utf8.DecodeRune(encoded[position:])
			position += size
			decoded += utf8.RuneLen(r)
		}
	}
	return position
}

func gatewayJSONEditedLength(raw []byte, edits []gatewayJSONEdit) int {
	length := len(raw)
	for _, edit := range edits {
		length += len(edit.replacement) - (edit.end - edit.start)
	}
	return length
}

func gatewayApplyJSONEdits(raw []byte, edits []gatewayJSONEdit, length int) []byte {
	if len(edits) == 0 {
		return raw
	}
	sort.Slice(edits, func(i, j int) bool { return edits[i].start < edits[j].start })
	next := make([]byte, 0, length)
	position := 0
	for _, edit := range edits {
		next = append(next, raw[position:edit.start]...)
		next = append(next, edit.replacement...)
		position = edit.end
	}
	return append(next, raw[position:]...)
}

// defaultGatewayPromptCacheSpan recognizes only the default CLI cache key.
func defaultGatewayPromptCacheSpan(prepared []byte) (gatewayJSONFieldSpan, bool) {
	// The span scanner below only locates fields; the standard parser remains
	// the validator. Unlike Decoder+RawMessage, this does not allocate copies of
	// multi-megabyte tool results or grow a second full-body decoder buffer.
	if !json.Valid(prepared) {
		return gatewayJSONFieldSpan{}, false
	}
	fields := gatewayValidatedJSONFieldSpans(prepared, "prompt_cache_key", "client_metadata")
	if fields[0].end == 0 || fields[1].end == 0 {
		return gatewayJSONFieldSpan{}, false
	}
	metadata := prepared[fields[1].start:fields[1].end]
	identity := gatewayValidatedJSONFieldSpans(metadata, "session_id", "")
	if identity[0].end == 0 {
		return gatewayJSONFieldSpan{}, false
	}
	var cache, session string
	if json.Unmarshal(prepared[fields[0].start:fields[0].end], &cache) != nil ||
		json.Unmarshal(metadata[identity[0].start:identity[0].end], &session) != nil ||
		cache == "" || session == "" || cache != session {
		return gatewayJSONFieldSpan{}, false
	}
	return fields[0], true
}

// All scoped identity values are 36-character UUIDs. Preview the exact wire size
// without an account, before admission creates a sticky binding. This includes
// metadata-only requests and embedded JSON string escaping, not just the key.
func validateGatewayAccountPromptCacheSize(prepared []byte, keyID string, settings Settings, unbounded ...bool) error {
	if gatewayPayloadUnboundedOption(unbounded) {
		return nil
	}
	if !settings.PromptCacheKeyEnabled || keyID == "" {
		return nil
	}
	// One key, four direct identity aliases, and four embedded aliases can
	// each grow by at most 35 bytes (the shortest nonempty value is one byte).
	if len(prepared) <= maxBody-9*35 {
		return nil
	}
	edits := gatewayPromptCacheIdentityEdits(prepared, func(string, string) string {
		return "00000000-0000-4000-8000-000000000000"
	}, "")
	if gatewayJSONEditedLength(prepared, edits) > maxBody {
		return errGatewayPreparedBodyLimit
	}
	return nil
}

type gatewayJSONFieldSpan struct{ start, end int }

// All callers must first validate the complete document with json.Valid. This
// scanner is deliberately not a second JSON validator. Returned spans refer to
// the original bytes, and duplicate members use the final value as encoding/json
// does. Values nested inside arrays/objects never become top-level candidates.
func gatewayValidatedJSONFieldSpans(raw []byte, first, second string) [2]gatewayJSONFieldSpan {
	var fields [2]gatewayJSONFieldSpan
	position := gatewayJSONSkipSpace(raw, 0)
	if position >= len(raw) || raw[position] != '{' {
		return fields
	}
	position = gatewayJSONSkipSpace(raw, position+1)
	for raw[position] != '}' {
		nameStart := position
		nameEnd := gatewayJSONStringEnd(raw, position)
		position = gatewayJSONSkipSpace(raw, nameEnd)
		position = gatewayJSONSkipSpace(raw, position+1) // The validated colon.
		valueEnd := gatewayJSONValueEnd(raw, position)
		name := raw[nameStart:nameEnd]
		if gatewayJSONFieldName(name, first) {
			fields[0] = gatewayJSONFieldSpan{position, valueEnd}
		} else if second != "" && gatewayJSONFieldName(name, second) {
			fields[1] = gatewayJSONFieldSpan{position, valueEnd}
		}
		position = gatewayJSONSkipSpace(raw, valueEnd)
		if raw[position] == ',' {
			position = gatewayJSONSkipSpace(raw, position+1)
		}
	}
	return fields
}

func gatewayJSONFieldName(raw []byte, want string) bool {
	if bytes.Equal(raw[1:len(raw)-1], []byte(want)) {
		return true
	}
	// Target names are ASCII. An escaped spelling is at most six source bytes
	// per character; unrelated long names need no decoding or allocation.
	if len(raw) > 6*len(want)+2 || bytes.IndexByte(raw, '\\') < 0 {
		return false
	}
	var decoded string
	return json.Unmarshal(raw, &decoded) == nil && decoded == want
}

func gatewayJSONSkipSpace(raw []byte, position int) int {
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

func gatewayJSONStringEnd(raw []byte, position int) int {
	// IndexByte skips large ordinary tool-output strings using the standard
	// library's optimized search. Only quotes preceded by an odd number of
	// backslashes are escaped; braces and Unicode escapes are ordinary data.
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

func gatewayJSONValueEnd(raw []byte, position int) int {
	switch raw[position] {
	case '"':
		return gatewayJSONStringEnd(raw, position)
	case '{', '[':
		depth := 0
		for ; ; position++ {
			switch raw[position] {
			case '"':
				position = gatewayJSONStringEnd(raw, position) - 1
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
