package bps

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"strings"
	"testing"
)

func TestExportCacheReadRatioNativeAndMissingFields(t *testing.T) {
	cases := []struct {
		name, raw, want string
		percent         int
	}{
		{"zero", `{"usage":{"input_tokens":101,"output_tokens":7,"total_tokens":108,"input_tokens_details":{"cached_tokens":99,"cache_write_tokens":2}}}`, `{"usage":{"input_tokens":101,"output_tokens":7,"total_tokens":108,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":2}}}`, 0},
		{"hundred", `{"usage":{"input_tokens":101,"input_tokens_details":{"cached_tokens":9,"cache_write_tokens":2}}}`, `{"usage":{"input_tokens":101,"input_tokens_details":{"cached_tokens":101,"cache_write_tokens":2}}}`, 100},
		{"round_down", `{"usage":{"input_tokens":101,"input_tokens_details":{"cached_tokens":99}}}`, `{"usage":{"input_tokens":101,"input_tokens_details":{"cached_tokens":33}}}`, 33},
		{"event", `{"type":"response.completed","response":{"usage":{"input_tokens":101,"input_tokens_details":{"cached_tokens":1}}}}`, `{"type":"response.completed","response":{"usage":{"input_tokens":101,"input_tokens_details":{"cached_tokens":33}}}}`, 33},
		{"add_details", `{"usage":{"input_tokens":101,"output_tokens":7 }}`, `{"usage":{"input_tokens":101,"output_tokens":7 ,"input_tokens_details":{"cached_tokens":33}}}`, 33},
		{"add_read", `{"usage":{"input_tokens":101,"input_tokens_details":{"cache_write_tokens":5 }}}`, `{"usage":{"input_tokens":101,"input_tokens_details":{"cache_write_tokens":5 ,"cached_tokens":33}}}`, 33},
		{"empty_details", `{"usage":{"input_tokens":101,"input_tokens_details":{ }}}`, `{"usage":{"input_tokens":101,"input_tokens_details":{ "cached_tokens":33}}}`, 33},
		{"known_zero", `{"usage":{"input_tokens":0}}`, `{"usage":{"input_tokens":0,"input_tokens_details":{"cached_tokens":0}}}`, 100},
		{"explicit_overrides_null_read", `{"usage":{"input_tokens":101,"input_tokens_details":{"cached_tokens":null}}}`, `{"usage":{"input_tokens":101,"input_tokens_details":{"cached_tokens":33}}}`, 33},
		{"explicit_overrides_over_input", `{"usage":{"input_tokens":101,"input_tokens_details":{"cached_tokens":99999}}}`, `{"usage":{"input_tokens":101,"input_tokens_details":{"cached_tokens":33}}}`, 33},
		{"escaped_names", `{"res\u0070onse":{"us\u0061ge":{"input_\u0074okens":101,"input_tokens_\u0064etails":{"cach\u0065d_tokens":0,"cache_write_tokens":5}}}}`, `{"res\u0070onse":{"us\u0061ge":{"input_\u0074okens":101,"input_tokens_\u0064etails":{"cach\u0065d_tokens":33,"cache_write_tokens":5}}}}`, 33},
		{"aliases_untouched", `{"usage":{"input_tokens":101,"prompt_tokens":7000,"cached_tokens":19,"cache_creation_input_tokens":888,"prompt_tokens_details":{"cached_tokens":31},"input_tokens_details":{"cached_tokens":1,"cache_write_tokens":5}}}`, `{"usage":{"input_tokens":101,"prompt_tokens":7000,"cached_tokens":19,"cache_creation_input_tokens":888,"prompt_tokens_details":{"cached_tokens":31},"input_tokens_details":{"cached_tokens":33,"cache_write_tokens":5}}}`, 33},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte(tc.raw)
			got := ExportCacheReadRatio(raw, tc.percent)
			if string(got) != tc.want || !json.Valid(got) {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
			if string(raw) != tc.raw {
				t.Fatal("caller-owned bytes mutated")
			}
			again := ExportCacheReadRatio(got, tc.percent)
			if !bytes.Equal(got, again) || &again[0] != &got[0] {
				t.Fatal("second projection changed bytes or allocated")
			}
		})
	}
}

func TestExportCacheReadRatioExactArithmetic(t *testing.T) {
	for _, input := range []int64{0, 1, 99, 100, 101, 9007199254740993, math.MaxInt64 - 1, math.MaxInt64} {
		for _, percent := range []int{0, 1, 33, 50, 99, 100} {
			raw := []byte(fmt.Sprintf(`{"usage":{"input_tokens":%d,"input_tokens_details":{"cached_tokens":7,"cache_write_tokens":8},"output_tokens":9,"total_tokens":999}}`, input))
			wantCount := new(big.Int).Mul(big.NewInt(input), big.NewInt(int64(percent)))
			wantCount.Quo(wantCount, big.NewInt(100))
			want := bytes.Replace(raw, []byte(`"cached_tokens":7`), []byte(`"cached_tokens":`+wantCount.String()), 1)
			if got := ExportResponseCacheReadRatio(raw, percent); !bytes.Equal(got, want) {
				t.Fatalf("input=%d percent=%d: got %s, want %s", input, percent, got, want)
			}
		}
	}
}

func TestExportCacheReadRatioUnknownInvalidAndAmbiguous(t *testing.T) {
	cases := map[string]string{
		"empty": "", "invalid": `{"usage":`, "scalar": `null`, "array": `[{"usage":{"input_tokens":10}}]`,
		"delta":        `{"type":"response.output_text.delta","delta":"input_tokens usage"}`,
		"nested_tool":  `{"item":{"type":"function_call","usage":{"input_tokens":10,"input_tokens_details":{"cached_tokens":1}}}}`,
		"string_decoy": `{"output":"{\"usage\":{\"input_tokens\":10}}"}`,
		"no_input":     `{"usage":{"input_tokens_details":{"cached_tokens":1}}}`,
		"alias_input":  `{"usage":{"prompt_tokens":10,"input_tokens_details":{"cached_tokens":1}}}`,
		"usage_null":   `{"usage":null}`, "usage_array": `{"usage":[{"input_tokens":10}]}`,
		"details_null":       `{"usage":{"input_tokens":10,"input_tokens_details":null}}`,
		"details_array":      `{"usage":{"input_tokens":10,"input_tokens_details":[]}}`,
		"duplicate_usage":    `{"usage":{"input_tokens":10},"us\u0061ge":{"input_tokens":10}}`,
		"duplicate_response": `{"response":{"usage":{"input_tokens":10}},"response":{"usage":{"input_tokens":20}}}`,
		"duplicate_input":    `{"usage":{"input_tokens":10,"input_\u0074okens":20}}`,
		"duplicate_details":  `{"usage":{"input_tokens":10,"input_tokens_details":{},"input_tokens_details":{}}}`,
		"duplicate_read":     `{"usage":{"input_tokens":10,"input_tokens_details":{"cached_tokens":1,"cach\u0065d_tokens":2}}}`,
		"no_root_fallback":   `{"response":null,"usage":{"input_tokens":10}}`,
		"no_nested_fallback": `{"response":{"output":[{"usage":{"input_tokens":10}}]},"usage":{"input_tokens":10}}`,
	}
	for _, input := range []string{"null", `"10"`, "10.0", "1e1", "-1", "true", "{}", "[]", "9223372036854775808"} {
		cases["input_"+input] = `{"usage":{"input_tokens":` + input + `,"input_tokens_details":{"cached_tokens":1}}}`
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			data := []byte(raw)
			got := ExportCacheReadRatio(data, 33)
			if !bytes.Equal(got, data) || len(data) > 0 && &got[0] != &data[0] {
				t.Fatalf("unknown or ambiguous accounting changed: %s", got)
			}
		})
	}
	for _, percent := range []int{-1, -100, 101, math.MaxInt} {
		raw := []byte(`{"usage":{"input_tokens":10,"input_tokens_details":{"cached_tokens":1}}}`)
		if got := ExportCacheReadRatio(raw, percent); &got[0] != &raw[0] {
			t.Fatalf("invalid percentage %d changed raw", percent)
		}
	}
}

func TestExportCacheReadRatioPreservesOpaqueBytesAndResponseExtension(t *testing.T) {
	opaque := strings.Repeat(`\"escaped\"\\opaque \u0061 } { [ ]`, 1000)
	raw := []byte(` { "response":{"usage":{"input_tokens":900,"input_tokens_details":{"cached_tokens":900}}}, "output":[{"type":"reasoning","encrypted_content":"` + opaque + `"},{"type":"function_call","arguments":"{\"input_tokens\":12}","usage":{"input_tokens":200}}], "usage" : { "input_tokens" : 101, "input_tokens_details" : { "cached_tokens" : 7, "cache_write_tokens" : 21 }, "private":{"cached_tokens":88} }, "extension":9007199254740993 } `)
	want := bytes.Replace(raw, []byte(`"cached_tokens" : 7`), []byte(`"cached_tokens" : 33`), 1)
	got := ExportResponseCacheReadRatio(raw, 33)
	if !json.Valid(raw) || !bytes.Equal(got, want) {
		t.Fatal("response projection modified opaque bytes or extension response instead of native usage")
	}
	// Event projection recognizes the envelope, leaving root usage untouched.
	wantEvent := bytes.Replace(raw, []byte(`"cached_tokens":900`), []byte(`"cached_tokens":297`), 1)
	if got := ExportCacheReadRatio(raw, 33); !bytes.Equal(got, wantEvent) {
		t.Fatal("event projection did not isolate nested Response usage")
	}
}

func TestExportCacheReadRatioAfterCreationProjection(t *testing.T) {
	raw := []byte(`{"usage":{"input_tokens":1000,"output_tokens":11,"total_tokens":1011,"input_tokens_details":{"cached_tokens":750,"cache_write_tokens":250}}}`)
	created := ExportResponseCacheCreationAsRead(raw)
	got := ExportResponseCacheReadRatio(created, 33)
	want := `{"usage":{"input_tokens":1000,"output_tokens":11,"total_tokens":1011,"input_tokens_details":{"cached_tokens":330,"cache_write_tokens":0}}}`
	if string(got) != want {
		t.Fatalf("explicit ratio must override creation projection: %s", got)
	}
	if !bytes.Contains(raw, []byte(`"cache_write_tokens":250`)) || !bytes.Contains(created, []byte(`"cached_tokens":1000`)) {
		t.Fatal("an earlier boundary's bytes were changed")
	}
}
