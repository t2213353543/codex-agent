package bps

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestExportCacheCreationAsReadKnownNative(t *testing.T) {
	cases := []struct{ name, raw, want string }{
		{"response", `{"id":"r","usage":{"input_tokens":1000,"output_tokens":11,"total_tokens":1011,"input_tokens_details":{"cached_tokens":750,"cache_write_tokens":250,"image_tokens":4}}}`, `{"id":"r","usage":{"input_tokens":1000,"output_tokens":11,"total_tokens":1011,"input_tokens_details":{"cached_tokens":1000,"cache_write_tokens":0,"image_tokens":4}}}`},
		{"event", `{"type":"response.completed","response":{"id":"r","usage":{"input_tokens":1000,"input_tokens_details":{"cache_write_tokens":250,"cached_tokens":750}}}}`, `{"type":"response.completed","response":{"id":"r","usage":{"input_tokens":1000,"input_tokens_details":{"cache_write_tokens":0,"cached_tokens":1000}}}}`},
		{"read_zero", `{"usage":{"input_tokens":1000,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":250}}}`, `{"usage":{"input_tokens":1000,"input_tokens_details":{"cached_tokens":250,"cache_write_tokens":0}}}`},
		{"not_all_input", `{"usage":{"input_tokens":1000,"input_tokens_details":{"cached_tokens":700,"cache_write_tokens":250}}}`, `{"usage":{"input_tokens":1000,"input_tokens_details":{"cached_tokens":950,"cache_write_tokens":0}}}`},
		{"over_2pow53", `{"usage":{"input_tokens":9007199254740993,"input_tokens_details":{"cached_tokens":9007199254740992,"cache_write_tokens":1}}}`, `{"usage":{"input_tokens":9007199254740993,"input_tokens_details":{"cached_tokens":9007199254740993,"cache_write_tokens":0}}}`},
		{"int64_max", `{"usage":{"input_tokens":9223372036854775807,"input_tokens_details":{"cached_tokens":9223372036854775806,"cache_write_tokens":1}}}`, `{"usage":{"input_tokens":9223372036854775807,"input_tokens_details":{"cached_tokens":9223372036854775807,"cache_write_tokens":0}}}`},
		{"escaped_members", `{ "us\u0061ge" : {"input_tokens":1000,"input_tokens_details":{"cach\u0065d_tokens" : 750 , "cache_\u0077rite_tokens" : 250 } } }`, `{ "us\u0061ge" : {"input_tokens":1000,"input_tokens_details":{"cach\u0065d_tokens" : 1000 , "cache_\u0077rite_tokens" : 0 } } }`},
		{"aliases_untouched_but_shadowed", `{"usage":{"input_tokens":1000,"prompt_tokens":7000,"cached_tokens":19,"cache_creation_input_tokens":888,"prompt_tokens_details":{"cached_tokens":31,"cache_write_tokens":22},"input_tokens_details":{"cached_tokens":750,"cache_write_tokens":250}}}`, `{"usage":{"input_tokens":1000,"prompt_tokens":7000,"cached_tokens":19,"cache_creation_input_tokens":888,"prompt_tokens_details":{"cached_tokens":31,"cache_write_tokens":22},"input_tokens_details":{"cached_tokens":1000,"cache_write_tokens":0}}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte(tc.raw)
			got := ExportCacheCreationAsRead(raw)
			if string(got) != tc.want || !json.Valid(got) {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
			if string(raw) != tc.raw {
				t.Fatal("caller-owned upstream bytes mutated")
			}
			again := ExportCacheCreationAsRead(got)
			if !bytes.Equal(again, got) || &again[0] != &got[0] {
				t.Fatal("projection is not idempotent")
			}
		})
	}
}

func TestExportCacheCreationAsReadUnknownOrInvalidNoChange(t *testing.T) {
	usage := func(input, details string) string {
		return `{"usage":{"input_tokens":` + input + `,"input_tokens_details":` + details + `}}`
	}
	cases := map[string]string{
		"empty": "", "invalid_json": `{"cache_write_tokens":`,
		"delta":                               `{"type":"response.output_text.delta","delta":"opaque usage cache_write_tokens"}`,
		"array":                               `[{"usage":{"input_tokens":10,"input_tokens_details":{"cached_tokens":5,"cache_write_tokens":5}}}]`,
		"no_usage":                            `{"output":[{"usage":{"input_tokens":10,"input_tokens_details":{"cached_tokens":5,"cache_write_tokens":5}}}]}`,
		"string_decoy":                        `{"output":"{\"usage\":{\"input_tokens\":10,\"input_tokens_details\":{\"cached_tokens\":5,\"cache_write_tokens\":5}}}"}`,
		"missing_read":                        usage("1000", `{"cache_write_tokens":250}`),
		"missing_write":                       usage("1000", `{"cached_tokens":750}`),
		"native_read_null_alias_present":      `{"usage":{"input_tokens":1000,"cached_tokens":750,"input_tokens_details":{"cached_tokens":null,"cache_write_tokens":250}}}`,
		"native_write_null_alias_present":     `{"usage":{"input_tokens":1000,"cache_creation_input_tokens":250,"input_tokens_details":{"cached_tokens":750,"cache_write_tokens":null}}}`,
		"only_alias_read":                     `{"usage":{"input_tokens":1000,"cached_tokens":750,"input_tokens_details":{"cache_write_tokens":250}}}`,
		"only_alias_write":                    `{"usage":{"input_tokens":1000,"cache_creation_input_tokens":250,"input_tokens_details":{"cached_tokens":750}}}`,
		"only_prompt_input":                   `{"usage":{"prompt_tokens":1000,"input_tokens_details":{"cached_tokens":750,"cache_write_tokens":250}}}`,
		"input_null":                          usage("null", `{"cached_tokens":750,"cache_write_tokens":250}`),
		"input_string":                        usage(`"1000"`, `{"cached_tokens":750,"cache_write_tokens":250}`),
		"input_float":                         usage("1000.0", `{"cached_tokens":750,"cache_write_tokens":250}`),
		"input_exponent":                      usage("1e3", `{"cached_tokens":750,"cache_write_tokens":250}`),
		"input_negative":                      usage("-1000", `{"cached_tokens":750,"cache_write_tokens":250}`),
		"input_overflow":                      usage("9223372036854775808", `{"cached_tokens":750,"cache_write_tokens":250}`),
		"read_null":                           usage("1000", `{"cached_tokens":null,"cache_write_tokens":250}`),
		"read_negative":                       usage("1000", `{"cached_tokens":-1,"cache_write_tokens":250}`),
		"read_fraction":                       usage("1000", `{"cached_tokens":749.5,"cache_write_tokens":250}`),
		"read_bool":                           usage("1000", `{"cached_tokens":true,"cache_write_tokens":250}`),
		"read_overflow":                       usage("1000", `{"cached_tokens":9223372036854775808,"cache_write_tokens":250}`),
		"write_negative":                      usage("1000", `{"cached_tokens":750,"cache_write_tokens":-1}`),
		"write_string":                        usage("1000", `{"cached_tokens":750,"cache_write_tokens":"250"}`),
		"write_overflow":                      usage("1000", `{"cached_tokens":750,"cache_write_tokens":9223372036854775808}`),
		"write_zero":                          usage("1000", `{"cached_tokens":750,"cache_write_tokens":0}`),
		"both_zero":                           usage("0", `{"cached_tokens":0,"cache_write_tokens":0}`),
		"read_exceeds_input":                  usage("1000", `{"cached_tokens":1001,"cache_write_tokens":1}`),
		"sum_exceeds_input":                   usage("1000", `{"cached_tokens":750,"cache_write_tokens":251}`),
		"sum_overflow":                        usage("9223372036854775807", `{"cached_tokens":9223372036854775807,"cache_write_tokens":1}`),
		"details_array":                       usage("1000", `[{"cached_tokens":750,"cache_write_tokens":250}]`),
		"details_null":                        usage("1000", "null"),
		"duplicate_input":                     `{"usage":{"input_tokens":999,"input_tokens":1000,"input_tokens_details":{"cached_tokens":750,"cache_write_tokens":250}}}`,
		"duplicate_read":                      usage("1000", `{"cached_tokens":1,"cached_tokens":750,"cache_write_tokens":250}`),
		"duplicate_escaped_write":             usage("1000", `{"cached_tokens":750,"cache_write_tokens":1,"cache_\u0077rite_tokens":250}`),
		"duplicate_usage":                     `{"usage":{},"usage":{"input_tokens":1000,"input_tokens_details":{"cached_tokens":750,"cache_write_tokens":250}}}`,
		"malformed_response_no_root_fallback": `{"response":null,"usage":{"input_tokens":1000,"input_tokens_details":{"cached_tokens":750,"cache_write_tokens":250}}}`,
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			raw := []byte(tc)
			got := ExportCacheCreationAsRead(raw)
			if !bytes.Equal(got, raw) || len(got) > 0 && &got[0] != &raw[0] {
				t.Fatalf("unknown/invalid response changed: %s", got)
			}
		})
	}
}

func TestExportCacheCreationAsReadPreservesOpaqueBytes(t *testing.T) {
	content := strings.Repeat(`\"escaped\"\\opaque \u0061 } { [ ]`, 4000)
	raw := []byte(` { "type":"response.failed", "response" : { "id" : "r", "output":[{"type":"reasoning","encrypted_content":"` + content + `"}], "large":9007199254740993, "error":{"code":"rate_limit_exceeded"}, "usage" : { "input_tokens" : 1000, "input_tokens_details" : { "cached_tokens" : 750, "cache_write_tokens" : 250 }, "private" : {"cache_write_tokens":999} }, "after" : "unchanged" } } `)
	want := bytes.Replace(raw, []byte(`"cached_tokens" : 750, "cache_write_tokens" : 250`), []byte(`"cached_tokens" : 1000, "cache_write_tokens" : 0`), 1)
	got := ExportCacheCreationAsRead(raw)
	if !json.Valid(raw) || !json.Valid(got) || !bytes.Equal(got, want) {
		t.Fatal("a byte outside the two native usage leaves changed")
	}
}

func TestExportCacheCreationAsReadByteProjectionMatrix(t *testing.T) {
	for read := int64(0); read < 100; read += 7 {
		for write := int64(1); write < 100; write += 11 {
			raw := []byte(fmt.Sprintf(`{"response":{"usage":{"input_tokens":200,"input_tokens_details":{"cached_tokens":%d,"cache_write_tokens":%d}}},"unrelated":{"cached_tokens":%d,"cache_write_tokens":%d}}`, read, write, read, write))
			want := []byte(fmt.Sprintf(`{"response":{"usage":{"input_tokens":200,"input_tokens_details":{"cached_tokens":%d,"cache_write_tokens":0}}},"unrelated":{"cached_tokens":%d,"cache_write_tokens":%d}}`, read+write, read, write))
			if got := ExportCacheCreationAsRead(raw); !bytes.Equal(got, want) {
				t.Fatalf("read=%d write=%d changed non-accounting data", read, write)
			}
		}
	}
}

func BenchmarkExportCacheCreationAsRead(b *testing.B) {
	delta := []byte(`{"type":"response.output_text.delta","delta":"` + strings.Repeat("a", 4096) + `"}`)
	terminal := []byte(`{"response":{"output":[{"encrypted_content":"` + strings.Repeat("a", 1<<20) + `"}],"usage":{"input_tokens":1000,"input_tokens_details":{"cached_tokens":750,"cache_write_tokens":250}}}}`)
	for _, tc := range []struct {
		name string
		raw  []byte
	}{{"delta_4KiB", delta}, {"terminal_1MiB", terminal}, {"already_projected_1MiB", ExportCacheCreationAsRead(terminal)}} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(tc.raw)))
			for b.Loop() {
				_ = ExportCacheCreationAsRead(tc.raw)
			}
		})
	}
}
