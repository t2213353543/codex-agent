package server

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// Same first-turn layout as the actual 0.155.0 raw CLI capture: Lite tools and
// instructions are input items; top-level instructions/tools are omitted.
const researchLiteHTTPBody = `{"model":"gpt-5.6-sol","input":[{"type":"additional_tools","id":"at-fixture","role":"developer","tools":[{"type":"function","name":"inspect","parameters":{"const":9007199254740993}}]},{"type":"message","id":"msg-instructions","role":"developer","content":[{"type":"input_text","text":"Preserve original instructions <>& and 你好"}]},{"type":"message","id":"msg-question","role":"user","content":[{"type":"input_text","text":"Unmodified synthetic question"}],"internal_chat_message_metadata_passthrough":{"turn_id":"fixture-turn","create_time":1.2300e+009}}],"tool_choice":"auto","parallel_tool_calls":false,"reasoning":{"effort":"medium","context":"all_turns"},"store":false,"stream":true,"include":["reasoning.encrypted_content"],"prompt_cache_key":"fixture-session","text":{"verbosity":"low"},"client_metadata":{"session_id":"fixture-session","thread_id":"fixture-session","turn_id":"fixture-turn"}}`

const researchLiteHeader = "X-Openai-Internal-Codex-Responses-Lite"

func TestResearchLiteBodyHTTPScalarMatchesForwardedValue(t *testing.T) {
	for _, tc := range []struct {
		name   string
		head   http.Header
		omit   bool
		actual string
	}{
		{"exact_true", http.Header{researchLiteHeader: {"true"}}, true, "true"},
		{"absent", http.Header{}, false, ""},
		{"false", http.Header{researchLiteHeader: {"false"}}, false, "false"},
		{"uppercase_value", http.Header{researchLiteHeader: {"TRUE"}}, false, "TRUE"},
		{"titlecase_value", http.Header{researchLiteHeader: {"True"}}, false, "True"},
		{"comma_list", http.Header{researchLiteHeader: {"true,false"}}, false, "true,false"},
		{"empty", http.Header{researchLiteHeader: {""}}, false, ""},
		{"lowercase_name", http.Header{"x-openai-internal-codex-responses-lite": {"true"}}, true, "true"},
		{"duplicate_first_true", http.Header{researchLiteHeader: {"true", "false"}}, true, "true"},
		{"duplicate_first_false", http.Header{researchLiteHeader: {"false", "true"}}, false, "false"},
		{"canonical_true_wins", http.Header{researchLiteHeader: {"true"}, "x-openai-internal-codex-responses-lite": {"false"}}, true, "true"},
		{"canonical_false_wins", http.Header{researchLiteHeader: {"false"}, "x-openai-internal-codex-responses-lite": {"true"}}, false, "false"},
		{"connection_nominated", http.Header{researchLiteHeader: {"true"}, "Connection": {"keep-alive, X-OpenAI-Internal-Codex-Responses-Lite"}}, false, ""},
		{"mixed_case_connection", http.Header{researchLiteHeader: {"true"}, "connection": {"x-openai-internal-codex-responses-lite"}}, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runResearchLiteHTTP(t, false, false, tc.head, []byte(researchLiteHTTPBody), tc.omit, tc.actual)
		})
	}
}

func TestResearchLiteBodyHTTPExplicitInstructionsAndCompression(t *testing.T) {
	for _, compressed := range []bool{false, true} {
		for _, instruction := range []string{"absent", `""`, `null`, `"Explicit instructions stay <>&"`} {
			t.Run(fmt.Sprintf("zstd=%v/instructions=%s", compressed, instruction), func(t *testing.T) {
				body := []byte(researchLiteHTTPBody)
				if instruction != "absent" {
					body = append([]byte(`{"instructions":`+instruction+`,`), body[1:]...)
				}
				runResearchLiteHTTP(t, false, compressed, http.Header{researchLiteHeader: {"true"}}, body, instruction == "absent", "true")
			})
		}
	}
}

func TestResearchLiteBodyActualNativeHTTP(t *testing.T) {
	for _, compressed := range []bool{false, true} {
		t.Run(fmt.Sprintf("zstd=%v", compressed), func(t *testing.T) {
			runResearchLiteHTTP(t, true, compressed, http.Header{researchLiteHeader: {"true"}}, []byte(researchLiteHTTPBody), true, "true")
		})
	}
}

func runResearchLiteHTTP(t *testing.T, native, compressed bool, headers http.Header, original []byte, omit bool, expectedHeader string) {
	t.Helper()
	var calls atomic.Int32
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		if compressed {
			if r.Header.Get("Content-Encoding") != "zstd" {
				t.Error("explicit CLI zstd request choice was lost")
			}
			decoder, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
			if err != nil {
				t.Error(err)
				return
			}
			raw, err = decoder.DecodeAll(raw, nil)
			decoder.Close()
			if err != nil {
				t.Error(err)
				return
			}
		}
		if r.Header.Get(researchLiteHeader) != expectedHeader || len(r.Header.Values(researchLiteHeader)) > 1 {
			t.Error("body decision disagrees with forwarded scalar Lite header")
		}
		var before, after map[string]json.RawMessage
		if json.Unmarshal(original, &before) != nil || json.Unmarshal(raw, &after) != nil {
			t.Error("request JSON unavailable")
			return
		}
		if _, exists := after["instructions"]; exists == omit {
			t.Error("native Lite missing instructions was not preserved, or legacy default was removed")
		}
		if !omit && before["instructions"] == nil {
			before["instructions"] = json.RawMessage(`""`)
		}
		left, _ := marshalGatewayRequestJSON(before)
		want, err := canonicalGatewayCacheJSON(left)
		got, actualErr := canonicalGatewayCacheJSON(raw)
		if err != nil || actualErr != nil || !bytes.Equal(want, got) {
			t.Error("Lite fix changed another complete request field, input item, numeric lexeme or metadata")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"lite-fixture\",\"status\":\"completed\",\"output\":[]}}\n\n")
	})
	settings := defaultSettings()
	settings.BuiltinPromptEnabled = false
	settings.PromptCacheKeyEnabled = false
	settings.CompatibilityNormalizationEnabled = false
	if err := p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
	if native {
		realGatewayNativePool(t, p)
	}
	body := original
	if compressed {
		encoder, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
		if err != nil {
			t.Fatal(err)
		}
		body = encoder.EncodeAll(original, nil)
		encoder.Close()
	}
	r := proxyRequest(http.MethodPost, "/responses", string(body))
	for key, values := range headers {
		r.Header[key] = append([]string(nil), values...)
	}
	if compressed {
		r.Header.Set("Content-Encoding", "zstd")
	}
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	if w.Code != http.StatusOK || calls.Load() != 1 {
		t.Fatalf("Lite request failed or retried: status=%d calls=%d", w.Code, calls.Load())
	}
}

func TestResearchLiteBodyGzipRemainsRejectedBeforeUpstream(t *testing.T) {
	var compressed bytes.Buffer
	encoder := gzip.NewWriter(&compressed)
	_, _ = encoder.Write([]byte(researchLiteHTTPBody))
	if err := encoder.Close(); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) })
	r := proxyRequest(http.MethodPost, "/responses", compressed.String())
	r.Header.Set("Content-Encoding", "gzip")
	r.Header.Set(researchLiteHeader, "true")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	if w.Code != http.StatusUnsupportedMediaType || calls.Load() != 0 {
		t.Fatal("Lite header changed the gzip rejection boundary")
	}
}

func TestResearchLiteBodyCompactAndExplicitPromptPolicyUnchanged(t *testing.T) {
	headers := http.Header{researchLiteHeader: {"true"}}
	for _, instructions := range []string{"absent", `""`, `null`, `"explicit compact instructions"`} {
		t.Run("compact="+instructions, func(t *testing.T) {
			raw := []byte(researchLiteHTTPBody)
			if instructions != "absent" {
				raw = append([]byte(`{"instructions":`+instructions+`,`), raw[1:]...)
			}
			legacy, legacyStream, legacyErr := prepareBody(raw, true)
			lite, liteStream, liteErr := prepareHTTPBody(raw, true, headers)
			if legacyErr != nil || liteErr != nil || legacyStream || liteStream || !bytes.Equal(legacy, lite) {
				t.Fatal("Lite changed compact body or stream behavior")
			}
		})
	}
	t.Run("legacy_wrapper", func(t *testing.T) {
		legacy, _, err := prepareBody([]byte(researchLiteHTTPBody), false)
		var body map[string]json.RawMessage
		if err != nil || json.Unmarshal(legacy, &body) != nil || string(body["instructions"]) != `""` {
			t.Fatal("headerless prepareBody lost its legacy instruction default")
		}
	})
	t.Run("configured_prompt", func(t *testing.T) {
		settings := defaultSettings()
		settings.BuiltinPromptEnabled = true
		settings.BuiltinPrompt = "Explicit administrator prompt remains configured"
		settings.PromptCacheKeyEnabled = false
		r := proxyRequest(http.MethodPost, "/responses", researchLiteHTTPBody)
		r.Header = headers.Clone()
		policy, _, err := applyGatewayPolicy(r, []byte(researchLiteHTTPBody), "fixture", settings)
		if err != nil {
			t.Fatal(err)
		}
		body, _, err := prepareHTTPBody(policy, false, r.Header)
		var parsed map[string]json.RawMessage
		var instructions string
		if err != nil || json.Unmarshal(body, &parsed) != nil || json.Unmarshal(parsed["instructions"], &instructions) != nil || instructions != settings.BuiltinPrompt {
			t.Fatal("Lite omission removed an explicitly configured prompt policy")
		}
	})
}
