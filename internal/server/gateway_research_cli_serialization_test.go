package server

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestResearchCLISerializationReportSeparatesLexicalAndSemanticChanges(t *testing.T) {
	before := nativeCompressionShape([]byte(`{"input":[{"text":"<private> & 中文"}],"n":9007199254740993,"model":"fixture"}`))
	after := nativeCompressionShape([]byte(`{"model":"fixture","input":[{"text":"\u003cprivate\u003e \u0026 \u4e2d\u6587"}],"n":9007199254740993}`))
	delta := nativeCompressionCompareShape(before, after)
	if !delta.ContentTreePreserved || len(delta.SemanticFieldsChanged) != 0 || len(delta.SerializationFieldsChanged) != 1 || delta.SerializationFieldsChanged[0] != "input" || len(delta.ExtraFields) != 0 || len(delta.MissingFields) != 0 || before.RawHTMLCharacters != 3 || after.EscapedHTMLCharacters != 3 {
		t.Fatalf("serialization mistaken for semantic change: %+v", delta)
	}
	changed := nativeCompressionCompareShape(before, nativeCompressionShape([]byte(`{"input":[],"n":9007199254740992,"new":true}`)))
	if len(changed.SemanticFieldsChanged) != 2 || len(changed.MissingFields) != 1 || changed.MissingFields[0] != "model" || len(changed.ExtraFields) != 1 {
		t.Fatalf("actual mutation hidden by digest: %+v", changed)
	}
	report, _ := json.Marshal([]any{before, after, delta})
	if strings.Contains(string(report), "private") || strings.Contains(string(report), "中文") {
		t.Fatal("value content persisted")
	}
	for _, original := range []string{`{"extension":{"x":1,"x":2}}`, `{"extension":1,"extension":2}`} {
		mutated := strings.ReplaceAll(original, `"x":1,`, "")
		mutated = strings.ReplaceAll(mutated, `"extension":1,`, "")
		if nativeCompressionCompareShape(nativeCompressionShape([]byte(original)), nativeCompressionShape([]byte(mutated))).ContentTreePreserved {
			t.Fatal("duplicate member loss hidden by content tree digest")
		}
	}
}
