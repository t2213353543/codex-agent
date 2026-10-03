package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestBPSWSAdapterFragmentedMultilineAndOwnership(t *testing.T) {
	var got [][]byte
	w := &bpsWSResponseWriter{streamID: "lane_1", send: func(raw []byte) error {
		got = append(got, bytes.Clone(raw))
		return nil
	}}
	wire := []byte(": keepalive\n\ndata: {\"type\":\"response.output_text.delta\",\n" +
		"data: \"delta\":\"first\",\"integer\":9007199254740993}\n\ndata: [DONE]\n\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"second\"}\n\n")
	for i := 0; i < len(wire); i += 7 {
		chunk := wire[i:min(i+7, len(wire))]
		if n, err := w.Write(chunk); err != nil || n != len(chunk) {
			t.Fatalf("write: %d %v", n, err)
		}
	}
	if len(got) != 2 {
		t.Fatalf("events: %q", got)
	}
	for i, raw := range got {
		var obj map[string]json.RawMessage
		if json.Unmarshal(raw, &obj) != nil || bpsGatewayString(obj, "stream_id") != "lane_1" {
			t.Fatalf("frame: %s", raw)
		}
		want := []string{"first", "second"}[i]
		if bpsGatewayString(obj, "delta") != want {
			t.Fatalf("delta: %s", raw)
		}
		if i == 0 && string(obj["integer"]) != "9007199254740993" {
			t.Fatalf("number changed: %s", raw)
		}
	}
}

func TestBPSWSAdapterEventMetadataAndWriteFailure(t *testing.T) {
	var calls []string
	wantErr := errors.New("blocked writer")
	w := &bpsWSResponseWriter{streamID: "lane_1", onResponseID: func(id string) { calls = append(calls, id) },
		onTerminal: func() { calls = append(calls, "terminal") }, send: func(raw []byte) error {
			var obj map[string]json.RawMessage
			if json.Unmarshal(raw, &obj) != nil || bpsGatewayString(obj, "stream_id") != "lane_1" {
				t.Fatalf("frame: %s", raw)
			}
			calls = append(calls, "send")
			return wantErr
		}}
	// Escaped names and duplicate fields follow encoding/json's final-value rule.
	raw := []byte(`{"type":"response.created","t\u0079pe":"response.completed","response":{"id":"wrong","i\u0064":"resp_final"},"stream_id":"wrong","stream_id":"also-wrong"}`)
	if err := w.event(raw); !errors.Is(err, wantErr) {
		t.Fatalf("send error: %v", err)
	}
	if !w.terminal || w.responseID != "resp_final" || !reflect.DeepEqual(calls, []string{"resp_final", "terminal", "send"}) {
		t.Fatalf("callbacks: %v", calls)
	}
	if err := w.event([]byte(`{"type":`)); err == nil {
		t.Fatal("accepted invalid JSON")
	}
}

func TestBPSHistoryArrayMergePreservesNullAndRawItems(t *testing.T) {
	ctx := context.Background()
	for _, saved := range []string{"null", "[]", `[null,{"type":"function_call","arguments":"{}","large":9007199254740993}]`} {
		for _, increment := range []string{"null", "[]", ` [null,{"type":"function_call_output","output":"a,b[]{}\\\"","large":1.2500e+02}] `} {
			t.Run(fmt.Sprintf("%s/%s", saved, increment), func(t *testing.T) {
				p := &Proxy{BPS: &bpsRuntime{}}
				output := []json.RawMessage{json.RawMessage(`{"type":"reasoning","encrypted_content":"opaque"}`), nil}
				record := bpsHistoryRecord{Owner: "owner", Session: "session", Request: json.RawMessage(`{"input":` + saved + `,"instructions":"keep"}`), Output: output}
				if err := p.bpsSaveHistory(ctx, "key", "resp", record); err != nil {
					t.Fatal(err)
				}
				raw, session, owner, err := p.bpsExpandHistory(ctx, "key", []byte(`{"previous_response_id":"resp","input":`+increment+`}`), "")
				if err != nil || session != "session" || owner != "owner" {
					t.Fatalf("expand: %s %s %v", session, owner, err)
				}
				var got struct {
					Input        []json.RawMessage
					Instructions string
				}
				if err := json.Unmarshal(raw, &got); err != nil || got.Instructions != "keep" {
					t.Fatalf("invalid merge: %s %v", raw, err)
				}
				var past, extra []json.RawMessage
				_ = json.Unmarshal([]byte(saved), &past)
				_ = json.Unmarshal([]byte(increment), &extra)
				want := append(append(past, json.RawMessage(`{"type":"reasoning","encrypted_content":"opaque"}`), json.RawMessage(`null`)), extra...)
				if !reflect.DeepEqual(got.Input, want) {
					t.Fatalf("merge mismatch: %s", raw)
				}
			})
		}
	}
}

func TestBPSHistoryRejectedArrayTypesAndCallerMutation(t *testing.T) {
	p := &Proxy{BPS: &bpsRuntime{}}
	ctx := context.Background()
	record := bpsHistoryRecord{Request: json.RawMessage(`{"input":[{"role":"user","content":"original"}]}`)}
	if err := p.bpsSaveHistory(ctx, "key", "resp", record); err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{`"text"`, `{}`, `true`, `1`} {
		if _, _, _, err := p.bpsExpandHistory(ctx, "key", []byte(`{"previous_response_id":"resp","input":`+input+`}`), ""); err == nil {
			t.Errorf("accepted non-array %s", input)
		}
	}
	loaded, err := p.bpsLoadHistory(ctx, "key", "resp")
	if err != nil {
		t.Fatal(err)
	}
	for i := range loaded.Request {
		loaded.Request[i] = 'x'
	}
	loaded, err = p.bpsLoadHistory(ctx, "key", "resp")
	if err != nil || !bytes.Contains(loaded.Request, []byte("original")) {
		t.Fatal("loaded record aliased stored bytes")
	}
	for _, raw := range []string{`{"previous_response_id":false}`, `{"previous_response_id":[]}`, `{"previous_response_id":{}}`} {
		if _, _, _, err := p.bpsExpandHistory(ctx, "key", []byte(raw), ""); err == nil {
			t.Errorf("accepted invalid previous ID: %s", raw)
		}
	}
}

func TestBPSHistoryLargeWhitespaceCompactsBeforeLimit(t *testing.T) {
	p := &Proxy{BPS: &bpsRuntime{}}
	ctx := context.Background()
	// The incoming body is within maxBody but its existing saved history makes
	// the combined raw array exceed it. Only compacted wire bytes count.
	past := `{"role":"user","content":"` + strings.Repeat("x", 1024) + `"}`
	if err := p.bpsSaveHistory(ctx, "key", "resp", bpsHistoryRecord{Request: json.RawMessage(`{"input":[` + past + `]}`)}); err != nil {
		t.Fatal(err)
	}
	prefix, suffix := `{"previous_response_id":"resp","input":[null,`, `null]}`
	raw := []byte(prefix + strings.Repeat(" ", maxBody-len(prefix)-len(suffix)) + suffix)
	expanded, _, _, err := p.bpsExpandHistory(ctx, "key", raw, "")
	if err != nil || len(expanded) > 1200 || !json.Valid(expanded) {
		t.Fatalf("bounded compacted conversation rejected: len=%d err=%v", len(expanded), err)
	}
}

func FuzzBPSHistoryObjectFields(f *testing.F) {
	for _, raw := range []string{
		`{}`, ` { "input" : [ {"role":"user","content":"hi"} ], "previous_response_id":null } `,
		`{"input":[],"inpu\u0074":[null],"previous_response_id":"wrong","previous_response_id":"right"}`,
		`{"\\\"[]{}":"a,b[]{}\\\"","unicode":"\ud800","nested":{"input":[{}]},"n":9007199254740993}`,
		`null`, `[]`, `{"input":`,
	} {
		f.Add([]byte(raw))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		if !json.Valid(raw) {
			return
		}
		var want map[string]json.RawMessage
		if err := json.Unmarshal(raw, &want); err != nil || want == nil {
			return
		}
		got := bpsHistoryObjectFields(raw)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("field mismatch for %q\ngot=%q\nwant=%q", raw, got, want)
		}
	})
}

func FuzzBPSWSAdapterFragmentBoundaries(f *testing.F) {
	f.Add("first", 1)
	f.Add("a,b[]{}\\\"\n世界", 7)
	f.Add("", 4096)
	f.Fuzz(func(t *testing.T, delta string, chunkSize int) {
		if len(delta) > 8192 {
			return
		}
		chunkSize = int(uint(chunkSize)%1024) + 1
		frame, _ := json.Marshal(map[string]any{"type": "response.output_text.delta", "delta": delta, "n": json.Number("9007199254740993")})
		wire := append([]byte("data: "), frame...)
		wire = append(wire, '\n', '\n')
		wire = append(wire, wire...)
		var frames [][]byte
		w := &bpsWSResponseWriter{streamID: "lane_1", send: func(raw []byte) error { frames = append(frames, bytes.Clone(raw)); return nil }}
		for i := 0; i < len(wire); i += chunkSize {
			if _, err := w.Write(wire[i:min(i+chunkSize, len(wire))]); err != nil {
				t.Fatal(err)
			}
		}
		if len(frames) != 2 || !bytes.Equal(frames[0], frames[1]) {
			t.Fatalf("fragment mismatch %q", frames)
		}
		var got, want map[string]json.RawMessage
		_ = json.Unmarshal(frame, &want)
		want["stream_id"] = json.RawMessage(`"lane_1"`)
		if json.Unmarshal(frames[0], &got) != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("frame changed: %q", frames[0])
		}
	})
}
