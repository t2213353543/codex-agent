package publicerrors

import (
	"bytes"
	"testing"
)

func TestErrorMessagesLeaveSuccessfulContentAndExactNumbers(t *testing.T) {
	raw := []byte(`{"output":[{"content":[{"text":"PowerPoint tutorial"}]}],"error":{"message":"PoWeRpOiNt rejected","code":"upstream_error"},"counter":9007199254740993}`)
	clean := JSON(raw, false)
	if !bytes.Contains(clean, []byte(`"PowerPoint tutorial"`)) || !bytes.Contains(clean, []byte(`"upstream rejected"`)) || !bytes.Contains(clean, []byte(`9007199254740993`)) {
		t.Fatalf("error sanitization altered other content: %s", clean)
	}
	for _, raw := range []string{`{"error":{"message":"\u0050owerPoint failed"}}`, `{"detail":"PowerPoint failed"}`, `PowerPoint failed`} {
		if clean := JSON([]byte(raw), true); bytes.Contains(bytes.ToLower(clean), []byte("powerpoint")) || bytes.Contains(clean, []byte(`\u0050owerPoint`)) {
			t.Fatalf("provider leaked: %s", clean)
		}
	}
}

func TestJSONSanitizationMixedValueTypes(t *testing.T) {
	raw := []byte(` {"mode":"PowerPoint","data":[null,true,false,9007199254740993,-2.5,"PowerPoint"],"nested":[{"last_error":["\\u0050owerPoint",null,{"message":"PowerPoint failed","retry":false,"code":413}]}],"error":null} `)
	clean := JSON(raw, false)
	for _, unchanged := range []string{`"mode":"PowerPoint"`, `[null,true,false,9007199254740993,-2.5,"PowerPoint"]`, `"retry":false`, `"code":413`, `"error":null`} {
		if !bytes.Contains(clean, []byte(unchanged)) {
			t.Fatalf("non-error value changed: %s", clean)
		}
	}
	if !bytes.Contains(clean, []byte(`"message":"upstream failed"`)) {
		t.Fatalf("nested error was not sanitized: %s", clean)
	}
	for _, raw := range []string{`null`, `true`, `9007199254740993`, `"PowerPoint success"`, `{"error":"PowerPoint",broken}`} {
		if got := JSON([]byte(raw), false); !bytes.Equal(got, []byte(raw)) {
			t.Fatalf("unchanged payload differed: %s", got)
		}
	}
}
