// Package publicerrors keeps provider branding out of public error messages.
// Successful model output and protocol identities are left intact.
package publicerrors

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
)

var providerName = regexp.MustCompile(`(?i)powerpoint`)

func Message(value string) string {
	return strings.ReplaceAll(providerName.ReplaceAllString(value, "upstream"), "upstream upstream", "upstream")
}

// JSON preserves numeric precision and unknown fields. With errorBody=true the
// entire document is a failure payload; otherwise only error fields are edited.
func JSON(raw []byte, errorBody bool) []byte {
	if !providerName.Match(raw) && !bytes.Contains(raw, []byte(`\u`)) {
		return raw
	}
	var visit func(json.RawMessage, bool) (json.RawMessage, bool)
	visit = func(value json.RawMessage, failure bool) (json.RawMessage, bool) {
		// Valid JSON tells us its type from the first byte. Trying to decode
		// each value as string, object and array allocated conversion errors
		// for every numeric counter in large management responses.
		trimmed := bytes.TrimSpace(value)
		if len(trimmed) == 0 {
			return value, false
		}
		switch trimmed[0] {
		case '"':
			if failure {
				var text string
				if json.Unmarshal(value, &text) != nil {
					return value, false
				}
				clean := Message(text)
				if clean != text {
					encoded, _ := json.Marshal(clean)
					return encoded, true
				}
			}
			return value, false
		case '{':
			var object map[string]json.RawMessage
			if json.Unmarshal(value, &object) != nil {
				return value, false
			}
			changed := false
			for key, child := range object {
				errorField := key == "error" || key == "errors" || strings.HasSuffix(key, "_error")
				updated, edited := visit(child, failure || errorField)
				if edited {
					object[key], changed = updated, true
				}
			}
			if changed {
				encoded, _ := json.Marshal(object)
				return encoded, true
			}
			return value, false
		case '[':
			var list []json.RawMessage
			if json.Unmarshal(value, &list) != nil {
				return value, false
			}
			changed := false
			for i, child := range list {
				updated, edited := visit(child, failure)
				if edited {
					list[i], changed = updated, true
				}
			}
			if changed {
				encoded, _ := json.Marshal(list)
				return encoded, true
			}
		}
		return value, false
	}
	if !json.Valid(raw) {
		if errorBody {
			return []byte(Message(string(raw)))
		}
		return raw
	}
	result, _ := visit(raw, errorBody)
	return result
}
