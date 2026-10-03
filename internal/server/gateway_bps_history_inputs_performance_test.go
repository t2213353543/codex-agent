package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"codex-agent/internal/bps"
)

func bpsHistoryInputsBeforeReuse(original []byte, prepared *bps.Request, output []json.RawMessage) (json.RawMessage, []json.RawMessage) {
	var body map[string]json.RawMessage
	_ = json.Unmarshal(original, &body)
	delete(body, "previous_response_id")
	delete(body, "compaction_trigger")
	if prepared.Compact {
		body["input"], _ = json.Marshal(output)
		output = nil
	} else {
		if len(body["input"]) == 0 || string(body["input"]) == "null" {
			body["input"] = json.RawMessage("[]")
		}
		var s string
		if json.Unmarshal(body["input"], &s) == nil {
			body["input"], _ = json.Marshal([]any{map[string]any{"role": "user", "type": "message", "content": []any{map[string]string{"type": "input_text", "text": s}}}})
		}
	}
	raw, _ := json.Marshal(body)
	return raw, output
}

func TestBPSHistoryInputsReusePreservesStoredWire(t *testing.T) {
	for _, input := range []string{`[]`, ` [ {"role":"user","content":"<literal> 世界","n":9007199254740993} ] `, `"text"`, `""`, `null`} {
		for _, controls := range []string{"", `,"previous_response_id":""`, `,"compaction_trigger":null`, `,"input":[]`} {
			for _, compact := range []bool{false, true} {
				raw := []byte(`{"model":"fixture","input":` + input + controls + `}`)
				before := bytes.Clone(raw)
				prepared := &bps.Request{Compact: compact}
				output := []json.RawMessage{json.RawMessage(`{"type":"compaction","encrypted_content":"opaque"}`)}
				want, wantOutput := bpsHistoryInputsBeforeReuse(raw, prepared, output)
				got, gotOutput := bpsHistoryInputs(raw, prepared, output)
				a, err := json.Marshal(bpsHistoryRecord{Request: want, Output: wantOutput})
				if err != nil {
					t.Fatal(err)
				}
				b, err := json.Marshal(bpsHistoryRecord{Request: got, Output: gotOutput})
				if err != nil {
					t.Fatal(err)
				}
				var aa, bb any
				da, db := json.NewDecoder(bytes.NewReader(a)), json.NewDecoder(bytes.NewReader(b))
				da.UseNumber()
				db.UseNumber()
				if da.Decode(&aa) != nil || db.Decode(&bb) != nil || !reflect.DeepEqual(aa, bb) {
					t.Fatalf("history changed: %s / %s", a, b)
				}
				if !bytes.Equal(raw, before) {
					t.Fatal("history preparation mutated the request")
				}
			}
		}
	}
}

func TestBPSHistoryInputsDuplicateFieldsKeepCanonicalStorageSize(t *testing.T) {
	for _, raw := range []string{
		`{"model":"fixture","input":[{"role":"user","content":"discard me"}],"input":[]}`,
		`{"model":"fixture","instructions":"old","instructions":"new","input":[]}`,
		`{"model":"fixture","input":[],"inpu\u0074":[]}`,
		`{"mo\u0064el":"fixture","inpu\u0074":[]}`,
	} {
		prepared := &bps.Request{}
		want, _ := bpsHistoryInputsBeforeReuse([]byte(raw), prepared, nil)
		got, _ := bpsHistoryInputs([]byte(raw), prepared, nil)
		if !bytes.Equal(want, got) {
			t.Fatalf("duplicate members changed serialized history/cache size: %s / %s", want, got)
		}
	}
}

func BenchmarkBPSHistoryInputs(b *testing.B) {
	for _, size := range []int{32, 256 << 10, 1 << 20} {
		raw, _ := json.Marshal(map[string]any{"model": "fixture", "input": []any{map[string]string{"role": "user", "content": strings.Repeat("x", size)}}})
		for _, reuse := range []bool{false, true} {
			b.Run(fmt.Sprintf("bytes=%d/reuse=%t", size, reuse), func(b *testing.B) {
				prepared := &bps.Request{}
				b.ReportAllocs()
				for b.Loop() {
					var result []byte
					if reuse {
						result, _ = bpsHistoryInputs(raw, prepared, nil)
					} else {
						result, _ = bpsHistoryInputsBeforeReuse(raw, prepared, nil)
					}
					if len(result) == 0 {
						b.Fatal("empty history")
					}
				}
			})
		}
	}
}
