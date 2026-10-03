package bps

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
)

func TestIndependentSequentialCutoffPrepareCompatibility(t *testing.T) {
	opt := Options{TaskID: "fixed-task", TurnID: "fixed-turn", StructuredOutput: true}
	for _, control := range []string{`{"reasoning_summary_delivery":"sequential_cutoff"}`, ` {"include_usage":true,"reasoning_summary_delivery":"sequential_cutoff"} `, `{"reasoning_summary_delivery":"sequential_cutoff","include_usage":false}`, `null`, `{}`} {
		base := []byte(`{"model":"gpt-6-astra","stream":true,"input":[{"type":"reasoning","encrypted_content":"ORIGINAL_CIPHER","summary":[]},{"role":"user","content":"ORIGINAL"}]}`)
		candidate := append(bytes.Clone(base[:len(base)-1]), []byte(`,"stream_options":`+control+`}`)...)
		plain, e := Prepare(base, opt)
		if e != nil {
			t.Fatal(e)
		}
		prepared, e := Prepare(candidate, opt)
		if e != nil {
			t.Fatal(e)
		}
		if !bytes.Equal(plain.Body, prepared.Body) {
			t.Fatal("accepted delivery control changed actual upstream protocol")
		}
		if !bytes.Equal(bytes.TrimSpace(prepared.Policy.ClientControls["stream_options"]), bytes.TrimSpace([]byte(control))) {
			t.Fatal("delivery control not retained locally")
		}
	}
	for _, control := range []string{`{"reasoning_summary_delivery":"unknown"}`, `{"reasoning_summary_delivery":"SEQUENTIAL_CUTOFF"}`, `{"reasoning_summary_delivery":true}`, `{"reasoning_summary_delivery":null}`, `{"reasoning_summary_delivery":3}`, `{"reasoning_summary_delivery":{}}`, `{"reasoning_summary_delivery":[]}`, `{"reasoning_summary_delivery":"sequential_cutoff","include_usage":null}`, `{"reasoning_summary_delivery":"sequential_cutoff","unknown":true}`, `[]`, `true`} {
		_, err := Prepare([]byte(`{"model":"gpt-6-astra","input":[],"stream_options":`+control+`}`), opt)
		var param *ParameterError
		if !errors.As(err, &param) {
			t.Fatalf("invalid control not typed-rejected %s", control)
		}
	}
	r, e := Prepare([]byte(`{"model":"gpt-6-astra","input":[],"stream_options":{"reasoning_summary_delivery":"sequential_cutoff"}}`), Options{TaskID: "fixed", TurnID: "turn", Compact: true})
	if e != nil {
		t.Fatal(e)
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(r.Body, &fields)
	if len(fields) != 3 || fields["stream_options"] != nil {
		t.Fatal("compact schema polluted")
	}
}
