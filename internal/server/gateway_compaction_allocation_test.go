package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

type compactAllocationFields struct {
	first, second string
	input         []json.RawMessage
	raw           json.RawMessage
}

func decodeAllocationFields(raw []byte, mode int, spans bool) (compactAllocationFields, error) {
	value := compactAllocationFields{first: "first-seed", second: "second-seed", input: []json.RawMessage{json.RawMessage(`"input-seed"`)}, raw: json.RawMessage(`"raw-seed"`)}
	var err error
	switch mode {
	case 0:
		if spans {
			err = gatewayCompactDecodeExact(raw, "type", &value.first, "", nil)
		} else {
			err = gatewayWSDecodeExact(raw, map[string]any{"type": &value.first})
		}
	case 1:
		if spans {
			err = gatewayCompactDecodeExact(raw, "input", &value.input, "", nil)
		} else {
			err = gatewayWSDecodeExact(raw, map[string]any{"input": &value.input})
		}
	case 2:
		if spans {
			err = gatewayCompactDecodeExact(raw, "type", &value.first, "encrypted_content", &value.raw)
		} else {
			err = gatewayWSDecodeExact(raw, map[string]any{"type": &value.first, "encrypted_content": &value.raw})
		}
	case 3:
		if spans {
			err = gatewayCompactDecodeExact(raw, "type", &value.first, "role", &value.second)
		} else {
			err = gatewayWSDecodeExact(raw, map[string]any{"type": &value.first, "role": &value.second})
		}
	}
	return value, err
}

func assertCompactAllocationParity(t testing.TB, raw []byte, mode int) {
	t.Helper()
	copyBefore := append([]byte(nil), raw...)
	old, oldErr := decodeAllocationFields(raw, mode, false)
	next, nextErr := decodeAllocationFields(raw, mode, true)
	if (oldErr == nil) != (nextErr == nil) {
		t.Fatalf("accepted shape differs mode=%d: map=%v span=%v", mode, oldErr, nextErr)
	}
	if oldErr == nil && !reflect.DeepEqual(old, next) {
		t.Fatalf("decoded field values differ mode=%d", mode)
	}
	if !bytes.Equal(raw, copyBefore) {
		t.Fatal("selector mutated caller bytes")
	}
	// Existing map iteration can stop at either bad selected field, so partial
	// target writes after errors are intentionally not a relied-on contract.
}

func TestCompactAllocationSelectedFieldsMatchExactMap(t *testing.T) {
	corpus := [][]byte{
		nil, {}, []byte(`null`), []byte(`[]`), []byte(`7`), []byte(`false`), []byte(`"object"`), []byte(`{}`), []byte(" \r\n{}\t"),
		[]byte(`{"type":"reasoning","role":"user","input":[],"encrypted_content":"OPAQUE"}`),
		[]byte(`{"type":null,"role":null,"input":null,"encrypted_content":null}`),
		[]byte(`{"type":false,"role":{},"input":"wrong","encrypted_content":[1e1000]}`),
		[]byte(`{"Type":{"opaque":1},"Role":"assistant","Input":false,"Encrypted_Content":null}`),
		[]byte(`{"t\u0079pe":"reasoning","r\u006fle":"user","i\u006eput":[{"n":9007199254740993}],"encrypted_\u0063ontent":"OPAQUE"}`),
		[]byte(`{"type":"first","t\u0079pe":"last","role":"assistant","role":"user","input":0,"input":[-0,1.2300e+009],"encrypted_content":{},"encrypted_content":"last"}`),
		[]byte(`{"type":"valid","type":{},"role":"user","role":false}`),
		[]byte(`{"type":"\ud800","role":"\udc00","encrypted_content":"\ud800","input":["\ud800"]}`),
		[]byte("{\"type\":\"\xff\",\"role\":\"\xfe\",\"encrypted_content\":\"\xfd\",\"input\":[\"\xfc\"]}"),
		[]byte(`{"unknown":{"type":"nested","role":"nested"},"future":[{},[],1e1000],"type":"reasoning"}`),
		[]byte(`{"type":"unterminated}`), []byte(`{"type":"reasoning",}`), []byte(`{"type":"reasoning"} {}`), []byte(`{"future":NaN}`),
	}
	for i, raw := range corpus {
		for mode := 0; mode < 4; mode++ {
			t.Run(fmt.Sprintf("shape%d/fields%d", i, mode), func(t *testing.T) { assertCompactAllocationParity(t, raw, mode) })
		}
	}
}

func TestCompactAllocationValidatesOpaqueTailBeforeSelectedDecode(t *testing.T) {
	for _, padding := range []string{"\v", "\f", "\u00a0", "\u2003", "\ufeff", "\x00"} {
		for _, raw := range [][]byte{[]byte(padding + `{"type":"reasoning"}`), []byte(`{"type":"reasoning"}` + padding)} {
			for mode := 0; mode < 4; mode++ {
				assertCompactAllocationParity(t, raw, mode)
			}
			selected := "unchanged"
			if err := gatewayCompactDecodeExact(raw, "type", &selected, "", nil); err == nil || selected != "unchanged" {
				t.Fatal("non-JSON surrounding whitespace bypassed raw validation")
			}
		}
	}
	for _, tail := range []string{`,"future":`, `,"future":[1,]`, `,"future":"bad\xescape"`, `,"future":` + strings.Repeat("[", 10001) + "0" + strings.Repeat("]", 10001)} {
		raw := []byte(`{"type":"reasoning"` + tail + `}`)
		var selected string
		if err := gatewayCompactDecodeExact(raw, "type", &selected, "", nil); err == nil {
			t.Fatal("invalid opaque tail bypassed full JSON validation")
		}
		if selected != "" {
			t.Fatal("selected value written before complete validation")
		}
	}
	for _, depth := range []int{100, 9998, 9999, 10000} {
		raw := []byte(`{"future":` + strings.Repeat("[", depth) + "0" + strings.Repeat("]", depth) + `,"type":"reasoning"}`)
		assertCompactAllocationParity(t, raw, 0)
	}
}

func TestCompactAllocationNamesAndMissingFields(t *testing.T) {
	for _, pair := range [][2]string{{"", ""}, {"type", "type"}} {
		first, second := "first", "second"
		if err := gatewayCompactDecodeExact([]byte(`{"type":"changed"}`), pair[0], &first, pair[1], &second); err == nil || first != "first" || second != "second" {
			t.Fatal("invalid internal selector accepted or mutated output")
		}
	}
	first, second := "first", "second"
	if err := gatewayCompactDecodeExact([]byte(`{"unknown":[],"Type":"other"}`), "type", &first, "role", &second); err != nil || first != "first" || second != "second" {
		t.Fatal("missing exact members overwrote supplied defaults")
	}
}

func TestCompactAllocationOpaqueCopiesAvoidedAndPublicResultsOwned(t *testing.T) {
	raw := []byte(`{"type":"compaction","encrypted_content":"` + strings.Repeat("A", 32<<10) + `","future":"` + strings.Repeat("B", 32<<10) + `"}`)
	var first string
	var borrowed json.RawMessage
	if err := gatewayCompactDecodeExact(raw, "type", &first, "encrypted_content", &borrowed); err != nil {
		t.Fatal(err)
	}
	span := gatewayValidatedJSONFieldSpans(raw, "encrypted_content", "")[0]
	if len(borrowed) == 0 || &borrowed[0] != &raw[span.start] {
		t.Fatal("local opaque field was copied instead of borrowed")
	}
	mapAllocs := testing.AllocsPerRun(20, func() {
		var kind string
		var value json.RawMessage
		if gatewayWSDecodeExact(raw, map[string]any{"type": &kind, "encrypted_content": &value}) != nil {
			panic("fixture")
		}
	})
	spanAllocs := testing.AllocsPerRun(20, func() {
		var kind string
		var value json.RawMessage
		if gatewayCompactDecodeExact(raw, "type", &kind, "encrypted_content", &value) != nil {
			panic("fixture")
		}
	})
	if spanAllocs >= mapAllocs/2 {
		t.Fatalf("selector did not meaningfully reduce allocation count: span=%v map=%v", spanAllocs, mapAllocs)
	}
	request := []byte(`{"model":"test","input":[{"type":"message","role":"user","content":"retained"}]}`)
	response := append(append([]byte(`{"status":"completed","output":[`), raw...), []byte(`]}`)...)
	result, err := responsesCompactionResult(request, response)
	if err != nil {
		t.Fatal(err)
	}
	owned := append([]byte(nil), result...)
	for i := range request {
		request[i] = 'x'
	}
	for i := range response {
		response[i] = 'x'
	}
	if !bytes.Equal(result, owned) || !json.Valid(result) {
		t.Fatal("borrowed request/response bytes escaped into final public result")
	}
}

func FuzzCompactAllocationSelectedMapParity(f *testing.F) {
	for _, raw := range []string{`{}`, `{"type":"a","role":"user"}`, `{"t\u0079pe":"a","type":"b"}`, `{"type":null,"input":[],"encrypted_content":{"opaque":-0}}`} {
		for mode := uint8(0); mode < 4; mode++ {
			f.Add([]byte(raw), mode)
		}
	}
	f.Fuzz(func(t *testing.T, raw []byte, mode uint8) {
		if len(raw) > 64<<10 {
			t.Skip()
		}
		assertCompactAllocationParity(t, raw, int(mode%4))
	})
}
