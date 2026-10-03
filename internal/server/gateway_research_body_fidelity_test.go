package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

// This token representation deliberately does not decode objects into maps:
// repeated nested members and exact numeric lexemes must remain observable.
// Stable sorting removes only irrelevant object-key order, not duplicate order.
type researchBodyMember struct {
	Name  string
	Value researchBodyNode
}

type researchBodyNode struct {
	Kind    string
	Scalar  string
	Members []researchBodyMember
	Items   []researchBodyNode
}

func researchBodyReadNode(decoder *json.Decoder) (researchBodyNode, error) {
	token, err := decoder.Token()
	if err != nil {
		return researchBodyNode{}, err
	}
	switch value := token.(type) {
	case nil:
		return researchBodyNode{Kind: "null"}, nil
	case bool:
		return researchBodyNode{Kind: "boolean", Scalar: fmt.Sprint(value)}, nil
	case string:
		return researchBodyNode{Kind: "string", Scalar: value}, nil
	case json.Number:
		return researchBodyNode{Kind: "number", Scalar: value.String()}, nil
	case json.Delim:
		node := researchBodyNode{}
		switch value {
		case '{':
			node.Kind = "object"
			for decoder.More() {
				name, err := decoder.Token()
				if err != nil {
					return node, err
				}
				key, ok := name.(string)
				if !ok {
					return node, fmt.Errorf("object key is not string")
				}
				child, err := researchBodyReadNode(decoder)
				if err != nil {
					return node, err
				}
				node.Members = append(node.Members, researchBodyMember{key, child})
			}
			sort.SliceStable(node.Members, func(i, j int) bool { return node.Members[i].Name < node.Members[j].Name })
		case '[':
			node.Kind = "array"
			for decoder.More() {
				child, err := researchBodyReadNode(decoder)
				if err != nil {
					return node, err
				}
				node.Items = append(node.Items, child)
			}
		default:
			return node, fmt.Errorf("unexpected closing delimiter")
		}
		_, err := decoder.Token()
		return node, err
	default:
		return researchBodyNode{}, fmt.Errorf("unknown JSON token")
	}
}

func researchBodyCanonical(raw []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	node, err := researchBodyReadNode(decoder)
	if err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, fmt.Errorf("unexpected trailing JSON")
	}
	return json.Marshal(node)
}

func researchBodyHash(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

const researchRichCLIBody = `{
 "model": "fidelity-fixture-model",
 "instructions": "Keep <xml>& data / and \/ slash; Unicode: 你好 \\u literal, \"quote\", \n line; escaped \u4f60 is text.",
 "input": [
   {"type":"message","role":"developer","content":[{"type":"input_text","text":"Literal 你好 and escaped \u4f60; <>& /"}],"phase":"commentary"},
   {"type":"reasoning","id":"rs_fixture","encrypted_content":"opaque+/=<>&\\\"","summary":[],"future":{"duplicate":"first","duplicate":"second","n":9007199254740993}},
   {"type":"compaction","encrypted_content":"opaque-compaction+/=","unknown":{"x":1.0000000000000000000000000001,"x":-0.000}},
   {"type":"function_call","id":"fc_fixture","call_id":"call_fixture","name":"inspect","arguments":"{\"exact\":9007199254740995,\"text\":\"<>&\"}","encrypted_function_args":["opaque+/="],"namespace":"functions"},
   {"type":"function_call_output","call_id":"call_fixture","output":"C:\\folder\\file and /a/b <>&"},
   {"type":"custom_tool_call","id":"ct_fixture","call_id":"custom_fixture","name":"apply_patch","input":"*** Begin Patch\n*** End Patch","format":{"grammar":"start: /[<>]&/"}},
   {"type":"custom_tool_call_output","call_id":"custom_fixture","output":[{"type":"input_text","text":"unchanged"}]},
   {"type":"message","role":"assistant","phase":"analysis","content":[{"type":"output_text","text":"continuation <>&"}]},
   {"type":"message","role":"user","content":[{"type":"input_text","text":"\u4f60\u597d \ud83e\uddea <>& \/"}]}
 ],
 "tools":[{"type":"function","name":"inspect","parameters":{"type":"object","properties":{"number":{"const":9007199254740995,"minimum":-0,"maximum":1e1000},"text":{"pattern":"[<>&/]","enum":["你好","\u4f60","<>&"]}},"unknown":{"member":1,"member":2}}}],
 "reasoning":{"effort":"xhigh","summary":"detailed","context":"all_turns","opaque_number":1.2300e+009},
 "service_tier":"priority",
 "text":{"verbosity":"high","format":{"type":"json_schema","name":"fidelity","strict":true,"schema":{"type":"object","properties":{"big":{"const":123456789012345678901234567890}}}}},
 "include":["reasoning.encrypted_content"],
 "previous_response_id":"resp_existing",
 "prompt_cache_key":"fidelity-explicit-session",
 "prompt_cache_options":{"mode":"implicit","breakpoints":[2,0],"opaque":{"same":true,"same":false}},
 "client_metadata":{"turn_id":"turn-fixture","opaque":"<>&你好"},
 "parallel_tool_calls":false,
 "tool_choice":"auto",
 "stream_options":{"reasoning_summary_delivery":"sequential_cutoff"},
 "access_programs":["synthetic"],
 "store":false,
 "stream":true,
 "future":{"array":[null,false,0,-0,9007199254740993,1e1000,"<>&","\u0000"],"same":"first","same":"second"}
}`

func TestResearchBodyComparatorDetectsOpaqueLossWithoutFloatRounding(t *testing.T) {
	for _, equivalent := range [][2]string{
		{`{"b":"\u4f60\u597d","a":1}`, `{"a":1,"b":"\u4f60\u597d"}`},
		{`{"b":"\/<>&","a":"你好"}`, `{"a":"\u4f60\u597d","b":"/\u003c\u003e\u0026"}`},
		{`{"z":true,"same":1,"same":2}`, `{"same":1,"same":2,"z":true}`},
	} {
		left, errLeft := researchBodyCanonical([]byte(equivalent[0]))
		right, errRight := researchBodyCanonical([]byte(equivalent[1]))
		if errLeft != nil || errRight != nil || !bytes.Equal(left, right) {
			t.Fatal("comparison treated JSON formatting/valid string escapes as semantic changes")
		}
	}
	for _, distinct := range [][2]string{
		{`{"n":9007199254740993}`, `{"n":9007199254740992}`},
		{`{"n":1e1000}`, `{"n":1e999}`},
		{`{"n":-0}`, `{"n":0}`},
		{`{"n":1.2300e+009}`, `{"n":1230000000}`},
		{`{"same":1,"same":2}`, `{"same":2}`},
		{`{"same":1,"same":2}`, `{"same":2,"same":1}`},
		{`{"input":[1,2]}`, `{"input":[2,1]}`},
	} {
		left, _ := researchBodyCanonical([]byte(distinct[0]))
		right, _ := researchBodyCanonical([]byte(distinct[1]))
		if bytes.Equal(left, right) {
			t.Fatal("comparison hid a number lexeme, duplicate member or array ordering change")
		}
	}
}

func TestResearchBodyRichCLICompleteWireTreePreserved(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(fmt.Sprintf("native=%v", native), func(t *testing.T) {
			if native && (runtime.GOOS != "linux" || os.Getenv("CODEX_NATIVE_HTTP_TEST_BIN") == "") {
				t.Skip("Linux and explicit real Rust helper required")
			}
			for _, compressed := range []bool{false, true} {
				for _, normalization := range []bool{false, true} {
					t.Run(fmt.Sprintf("zstd=%v/compatibility=%v", compressed, normalization), func(t *testing.T) {
						captured := make(chan []byte, 1)
						p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
							raw, err := io.ReadAll(r.Body)
							if err == nil && r.Header.Get("Content-Encoding") == "zstd" {
								decoder, decodeErr := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
								if decodeErr != nil {
									err = decodeErr
								} else {
									raw, err = decoder.DecodeAll(raw, nil)
									decoder.Close()
								}
							}
							if err != nil {
								t.Error("synthetic upstream body read failed")
								http.Error(w, "fixture read failure", 400)
								return
							}
							captured <- raw
							w.Header().Set("Content-Type", "text/event-stream")
							_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"fidelity-result\",\"status\":\"completed\",\"output\":[]}}\n\n")
						})
						settings, err := p.Settings.Get()
						if err != nil {
							t.Fatal(err)
						}
						settings.BuiltinPromptEnabled = false
						settings.PromptCacheKeyEnabled = true // Only the explicit identity scalar is scoped.
						settings.CompatibilityNormalizationEnabled = normalization
						if err := p.Settings.Set(settings); err != nil {
							t.Fatal(err)
						}
						if native {
							realGatewayNativePool(t, p)
						}
						gateway := httptest.NewServer(p)
						defer gateway.Close()
						original := []byte(strings.ReplaceAll(researchRichCLIBody, "continuation <>&", "continuation <>&\u2028\u2029"))
						incoming := original
						if compressed {
							encoder, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
							if err != nil {
								t.Fatal(err)
							}
							incoming = encoder.EncodeAll(original, nil)
							encoder.Close()
						}
						ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
						defer cancel()
						r, err := http.NewRequestWithContext(ctx, http.MethodPost, gateway.URL+"/responses", bytes.NewReader(incoming))
						if err != nil {
							t.Fatal(err)
						}
						r.Header.Set("Authorization", "Bearer test-client-key")
						r.Header.Set("Content-Type", "application/json")
						r.Header.Set("Session-Id", "fidelity-explicit-session")
						r.Header.Set("User-Agent", "codex_cli_rs/0.155.0 (Linux 6.8; x86_64) unknown")
						r.Header.Set("Originator", "codex_cli_rs")
						if compressed {
							r.Header.Set("Content-Encoding", "zstd")
						}
						res, err := gateway.Client().Do(r)
						if err != nil {
							t.Fatal(err)
						}
						_, readErr := io.Copy(io.Discard, res.Body)
						_ = res.Body.Close()
						if readErr != nil || res.StatusCode != 200 {
							t.Fatalf("rich synthetic CLI request failed: status=%d read=%v", res.StatusCode, readErr)
						}
						var actual []byte
						select {
						case actual = <-captured:
						case <-ctx.Done():
							t.Fatal("upstream did not capture body")
						}
						expected := []byte(strings.Replace(string(original), `"prompt_cache_key":"fidelity-explicit-session"`, `"prompt_cache_key":"`+gatewayFixtureScopedValue("prompt-cache", "fidelity-explicit-session")+`"`, 1))
						wantTree, errWant := researchBodyCanonical(expected)
						actualTree, errActual := researchBodyCanonical(actual)
						if errWant != nil || errActual != nil || !bytes.Equal(wantTree, actualTree) {
							t.Fatal("full request tree changed, including an opaque duplicate or exact number")
						}
						var compact bytes.Buffer
						if err := json.Compact(&compact, expected); err != nil {
							t.Fatal(err)
						}
						var escaped bytes.Buffer
						json.HTMLEscape(&escaped, compact.Bytes())
						if len(actual) != compact.Len() {
							t.Errorf("API JSON unexpectedly expanded source characters: actual=%d compact=%d", len(actual), compact.Len())
						}
						t.Logf("ingress_decompressed_bytes=%d compact_bytes=%d escaped_compact_bytes=%d upstream_decompressed_bytes=%d ingress_raw_sha256=%s upstream_raw_sha256=%s full_tree_sha256=%s exact_tree_equal=true", len(original), compact.Len(), escaped.Len(), len(actual), researchBodyHash(original), researchBodyHash(actual), researchBodyHash(actualTree))
					})
				}
			}
		})
	}
}

func TestResearchBodyKnownAdaptersChangeOnlyDeclaredFields(t *testing.T) {
	const opaque = `,"opaque":{"same":1,"same":2,"precise":9007199254740993,"scientific":1.2300e+009,"text":"\u4f60\u597d <>& \/"}`
	for _, scenario := range []struct {
		name, incoming, expected string
		promptMode, prompt       string
	}{
		{"stream_store", `{"model":"fixture","instructions":"unchanged","input":[],"stream":false,"store":true` + opaque + `}`, `{"model":"fixture","instructions":"unchanged","input":[],"stream":true,"store":false` + opaque + `}`, "", ""},
		{"missing_instructions", `{"model":"fixture","input":[],"stream":true,"store":false` + opaque + `}`, `{"model":"fixture","instructions":"","input":[],"stream":true,"store":false` + opaque + `}`, "", ""},
		{"explicit_null_instructions", `{"model":"fixture","instructions":null,"input":[],"stream":true,"store":false` + opaque + `}`, `{"model":"fixture","instructions":null,"input":[],"stream":true,"store":false` + opaque + `}`, "", ""},
		{"string_input", `{"model":"fixture","instructions":"unchanged","input":"你好 <>& \/ \"quoted\"","stream":true,"store":false` + opaque + `}`, `{"model":"fixture","instructions":"unchanged","input":[{"role":"user","content":[{"type":"input_text","text":"你好 <>& \/ \"quoted\""}]}],"stream":true,"store":false` + opaque + `}`, "", ""},
		{"declared_prompt_prepend", `{"model":"fixture","instructions":"caller <>&","input":[],"stream":true,"store":false` + opaque + `}`, `{"model":"fixture","instructions":"configured <>&\n\ncaller <>&","input":[],"stream":true,"store":false` + opaque + `}`, "prepend", "configured <>&"},
		{"declared_prompt_append", `{"model":"fixture","instructions":"caller <>&","input":[],"stream":true,"store":false` + opaque + `}`, `{"model":"fixture","instructions":"caller <>&\n\nconfigured <>&","input":[],"stream":true,"store":false` + opaque + `}`, "append", "configured <>&"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			captured := make(chan []byte, 1)
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				captured <- raw
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"adapter-control\",\"status\":\"completed\",\"output\":[]}}\n\n")
			})
			settings, err := p.Settings.Get()
			if err != nil {
				t.Fatal(err)
			}
			settings.PromptCacheKeyEnabled = false
			settings.BuiltinPromptEnabled = scenario.prompt != ""
			if scenario.prompt != "" {
				settings.BuiltinPromptMode, settings.BuiltinPrompt = scenario.promptMode, scenario.prompt
			}
			if err := p.Settings.Set(settings); err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			p.ServeHTTP(w, proxyRequest(http.MethodPost, "/responses", scenario.incoming))
			if w.Code != http.StatusOK {
				t.Fatalf("known adapter fixture failed: %d", w.Code)
			}
			actual := <-captured
			wantTree, wantErr := researchBodyCanonical([]byte(scenario.expected))
			actualTree, actualErr := researchBodyCanonical(actual)
			if wantErr != nil || actualErr != nil || !bytes.Equal(wantTree, actualTree) {
				t.Fatal("known adapter changed additional fields beyond its declared contract")
			}
		})
	}
}
