package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestPolicyCompressionAuditOpaqueJSONAndIdempotence(t *testing.T) {
	const raw = `{"model":"test","instructions":"client","input":[{"role":"system","content":"first","unknown":9007199254740993},{"type":"reasoning","encrypted_content":"opaque+/=","unknown":123456789012345678901234567890}],"tools":[{"type":"custom","name":"tool","format":{"definition":"start: WORD"},"unknown":9007199254740993}],"reasoning":{"effort":"high","future":1e1000},"future":{"n":123456789012345678901234567890,"nil":null},"stream":false,"store":true}`
	request := httptest.NewRequest(http.MethodPost, "/responses", nil)
	var original map[string]json.RawMessage
	_ = json.Unmarshal([]byte(raw), &original)
	for _, mode := range []string{"prepend", "append", "if_empty"} {
		t.Run(mode, func(t *testing.T) {
			settings := defaultSettings()
			settings.BuiltinPromptMode, settings.BuiltinPrompt = mode, "stable"
			first, _, err := applyGatewayPolicy(request, []byte(raw), "key", settings)
			if err != nil {
				t.Fatal(err)
			}
			second, _, err := applyGatewayPolicy(request, first, "key", settings)
			if err != nil || !bytes.Equal(first, second) {
				t.Fatalf("policy not idempotent: %v", err)
			}
			prepared, stream, err := prepareBody(first, false)
			if err != nil || stream {
				t.Fatalf("prepare: %v stream=%v", err, stream)
			}
			var final map[string]json.RawMessage
			_ = json.Unmarshal(prepared, &final)
			for _, field := range []string{"tools", "reasoning", "future"} {
				if !bytes.Equal(original[field], final[field]) {
					t.Fatalf("opaque field %s changed: %s", field, final[field])
				}
			}
			if !bytes.Contains(final["input"], []byte("123456789012345678901234567890")) || !bytes.Contains(final["input"], []byte(`"role":"developer"`)) {
				t.Fatal("history normalization corrupted opaque values")
			}
		})
	}
	settings := defaultSettings()
	settings.BuiltinPromptEnabled, settings.PromptCacheKeyEnabled = false, false
	for _, input := range []string{`null`, `"hello"`, `[]`} {
		body := []byte(`{"model":"test","instructions":null,"input":` + input + `}`)
		policy, _, err := applyGatewayPolicy(request, body, "key", settings)
		if err != nil || !bytes.Equal(policy, body) {
			t.Fatalf("transparent input %s: %v", input, err)
		}
		prepared, _, err := prepareBody(policy, false)
		if err != nil {
			t.Fatal(err)
		}
		var value map[string]json.RawMessage
		_ = json.Unmarshal(prepared, &value)
		if input == `"hello"` {
			if !bytes.Contains(value["input"], []byte(`"type":"input_text"`)) {
				t.Fatal("string input was not converted")
			}
		} else if string(value["input"]) != input {
			t.Fatalf("input %s changed", input)
		}
	}
}

func TestPolicyCompressionAuditCanonicalCacheAndSize(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/responses", nil)
	s := defaultSettings()
	s.BuiltinPromptEnabled = false
	first, firstSession, err := applyGatewayPolicy(r, []byte(`{"model":"test","instructions":"你好","tools":[{"a":9007199254740993,"b":null}],"input":"same anchor"}`), "key", s)
	if err != nil {
		t.Fatal(err)
	}
	second, secondSession, err := applyGatewayPolicy(r, []byte(`{"tools":[{"b":null,"a":9007199254740993}],"instructions":"\u4f60\u597d","model":"test","input":"same anchor"}`), "key", s)
	if err != nil || firstSession == "" || firstSession != secondSession {
		t.Fatalf("equivalent content session changed: %v", err)
	}
	third, thirdSession, err := applyGatewayPolicy(r, []byte(`{"model":"test","instructions":"你好","tools":[{"a":9007199254740992,"b":null}],"input":"same anchor"}`), "key", s)
	if err != nil || thirdSession == "" || firstSession == thirdSession {
		t.Fatal("content session rounded distinct integers")
	}
	for _, prepared := range [][]byte{first, second, third} {
		if policyString(t, prepared, "prompt_cache_key") != "" {
			t.Fatal("local content routing generated an upstream cache key")
		}
	}
	oversized := bytes.Repeat([]byte{' '}, maxBody+1)
	if _, _, err := applyGatewayPolicy(r, oversized, "key", s); !errors.Is(err, errGatewayPreparedBodyLimit) {
		t.Fatalf("policy entry size: %v", err)
	}
	if _, _, err := prepareBody(oversized, false); !errors.Is(err, errGatewayPreparedBodyLimit) {
		t.Fatalf("prepare entry size: %v", err)
	}
	if _, err := prepareResponsesCompaction(oversized); !errors.Is(err, errGatewayPreparedBodyLimit) {
		t.Fatalf("compact entry size: %v", err)
	}
	// API JSON does not HTML-escape legal source text into an oversized body.
	body := []byte(`{"model":"test","input":"` + strings.Repeat("<", maxBody/6) + `"}`)
	if prepared, _, err := prepareBody(body, false); err != nil || len(prepared) > len(body)+256 || bytes.Contains(prepared, []byte(`\u003c`)) {
		t.Fatalf("prepared source text expanded: bytes=%d err=%v", len(prepared), err)
	}
}

type auditCancelReader struct {
	reader io.Reader
	cancel context.CancelFunc
	reads  int
}

func (r *auditCancelReader) Read(p []byte) (int, error) {
	r.reads++
	n, err := r.reader.Read(p)
	if r.cancel != nil {
		r.cancel()
	}
	return n, err
}

func TestPolicyCompressionAuditCancellation(t *testing.T) {
	encoder := compressionTestEncoder(t)
	for _, encoding := range []string{"identity", "zstd"} {
		for _, before := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/before=%v", encoding, before), func(t *testing.T) {
				body := []byte(`{"model":"test","input":[]}`)
				if encoding == "zstd" {
					body = encoder.EncodeAll(body, nil)
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				reader := &auditCancelReader{reader: bytes.NewReader(body), cancel: cancel}
				r := httptest.NewRequest(http.MethodPost, "/responses", reader).WithContext(ctx)
				r.Header.Set("Content-Encoding", encoding)
				if before {
					cancel()
				}
				if _, err := readBody(httptest.NewRecorder(), r); !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation lost: %v", err)
				}
				if before && reader.reads != 0 {
					t.Fatal("canceled body was read")
				}
				for _, proxy := range []*Proxy{{}, {NativeHTTP: &gatewayNativePool{}}} {
					if _, _, err := prepareGatewayRequestBody(proxy, r, "/responses", body); !errors.Is(err, context.Canceled) {
						t.Fatalf("compression accepted canceled context: %v", err)
					}
				}
			})
		}
	}
}

func TestPolicyCompressionAuditEncodingAliasesAndFrames(t *testing.T) {
	encoder := compressionTestEncoder(t)
	const body = `{"model":"test","input":[]}`
	frame := encoder.EncodeAll([]byte(body), nil)
	for _, header := range []http.Header{
		{"Content-Encoding": {"zstd", "zstd"}},
		{"Content-Encoding": {"zstd"}, "content-encoding": {"identity"}},
		{"Content-Encoding": {""}, "content-encoding": {"zstd"}},
	} {
		r := httptest.NewRequest(http.MethodPost, "/responses", bytes.NewReader(frame))
		r.Header = header
		if gatewayWantsRequestCompression(r, "/responses") {
			t.Fatal("stacked encodings enabled compression")
		}
		if _, err := readBody(httptest.NewRecorder(), r); !errors.Is(err, errUnsupportedContentEncoding) {
			t.Fatalf("stacked encoding accepted: %v", err)
		}
	}
	r := httptest.NewRequest(http.MethodPost, "/responses", bytes.NewReader(frame))
	r.Header = http.Header{"content-encoding": {" ZSTD "}}
	if got, err := readBody(httptest.NewRecorder(), r); err != nil || string(got) != body || !gatewayWantsRequestCompression(r, "/responses") {
		t.Fatalf("lowercase header: %v", err)
	}
	for name, wire := range map[string][]byte{"truncated": frame[:len(frame)-1], "trailing_junk": append(bytes.Clone(frame), 'x'), "bad_checksum": append(bytes.Clone(frame[:len(frame)-1]), frame[len(frame)-1]^0xff)} {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/responses", bytes.NewReader(wire))
			r.Header.Set("Content-Encoding", "zstd")
			if _, err := readBody(httptest.NewRecorder(), r); err == nil || !strings.Contains(err.Error(), "invalid zstd") {
				t.Fatalf("damaged frame: %v", err)
			}
		})
	}
	joined := append(encoder.EncodeAll([]byte(`{"model":`), nil), encoder.EncodeAll([]byte(`"test","input":[]}`), nil)...)
	r = httptest.NewRequest(http.MethodPost, "/responses", bytes.NewReader(joined))
	r.Header.Set("Content-Encoding", "zstd")
	if got, err := readBody(httptest.NewRecorder(), r); err != nil || string(got) != body {
		t.Fatalf("concatenated frames: %v %s", err, got)
	}
}

func TestPolicyCompressionAuditGuardianRoutes(t *testing.T) {
	testPolicyCompressionAuditGuardianRoutes(t, false)
}

func TestGatewayNativeActualCompressionGuardianRoutes(t *testing.T) {
	testPolicyCompressionAuditGuardianRoutes(t, true)
}

func testPolicyCompressionAuditGuardianRoutes(t *testing.T, native bool) {
	t.Helper()
	encoder := compressionTestEncoder(t)
	const original = `{"model":"guardian-model","input":[],"instructions":"specialized guardian instructions","stream":true,"store":false}`
	for _, path := range []string{"/guardian", "/guardian-classifier"} {
		t.Run(path, func(t *testing.T) {
			var calls atomic.Int32
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path != path || r.Header.Get("Content-Encoding") != "zstd" {
					t.Errorf("guardian compression lost: %s %s", r.URL.Path, r.Header.Get("Content-Encoding"))
				}
				wire, _ := io.ReadAll(r.Body)
				decoded := compressionTestDecode(t, wire)
				if policyString(t, decoded, "instructions") != "specialized guardian instructions" || policyString(t, decoded, "prompt_cache_key") != "" {
					t.Error("guardian received ordinary prompt/cache policy")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"guardian-response\",\"status\":\"completed\",\"output\":[]}}\n\n")
			})
			if native {
				realGatewayNativePool(t, p)
			}
			r := proxyRequest(http.MethodPost, "/v1"+path, string(encoder.EncodeAll([]byte(original), nil)))
			r.Header.Set("Content-Encoding", "zstd")
			w := httptest.NewRecorder()
			p.ServeHTTP(w, r)
			if w.Code != http.StatusOK || calls.Load() != 1 {
				t.Fatalf("guardian request: %d calls=%d body=%s", w.Code, calls.Load(), w.Body.String())
			}
		})
	}
}

func TestPolicyCompressionAuditConcurrentEncoderIsolation(t *testing.T) {
	var group sync.WaitGroup
	errorsFound := make(chan error, 12)
	for index := range 12 {
		group.Go(func() {
			r := httptest.NewRequest(http.MethodPost, "/responses", nil)
			r.Header.Set("Content-Encoding", "zstd")
			body := []byte(fmt.Sprintf(`{"request":%d,"input":"%s"}`, index, strings.Repeat(string(rune('a'+index)), 128<<10)))
			wire, _, err := prepareGatewayRequestBody(&Proxy{}, r, "/responses", body)
			if err != nil {
				errorsFound <- err
				return
			}
			decoder, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
			if err != nil {
				errorsFound <- err
				return
			}
			defer decoder.Close()
			decoded, err := decoder.DecodeAll(wire, nil)
			if err != nil || !bytes.Equal(decoded, body) {
				errorsFound <- fmt.Errorf("request %d corrupted: %v", index, err)
			}
		})
	}
	group.Wait()
	close(errorsFound)
	for err := range errorsFound {
		t.Error(err)
	}
	if len(gatewayRequestEncoders) > 2 {
		t.Fatal("encoder cache exceeded bound")
	}
}

func TestPolicyCompressionAuditCompactValidation(t *testing.T) {
	request, err := prepareResponsesCompaction([]byte(`{"model":"test","input":"remember"}`))
	if err != nil || !bytes.Contains(request, []byte(`"type":"input_text"`)) {
		t.Fatalf("compact string input: %v %s", err, request)
	}
	const summary = `{"type":"compaction","encrypted_content":"opaque+/=","future":9007199254740993}`
	for _, invalid := range []string{`null`, `[]`, `"item"`, `42`, `{}`, `{"type":null}`, `{"type":"compaction","encrypted_content":" "}`} {
		response := []byte(`{"status":"completed","output":[` + invalid + `,` + summary + `]}`)
		if _, err := responsesCompactionResult(request, response); err == nil {
			t.Fatalf("malformed compaction output accepted: %s", invalid)
		}
	}
	response := []byte(`{"status":"completed","output":[{"type":"future_item","encrypted_content":{"opaque":"unrelated"}},` + summary + `]}`)
	result, err := responsesCompactionResult(request, response)
	if err != nil || !bytes.Contains(result, []byte(summary)) {
		t.Fatalf("opaque compact summary was modified: %v %s", err, result)
	}
}

func BenchmarkGatewayRequestCompressionAudit(b *testing.B) {
	for _, size := range []int{4096, 128 << 10, 1 << 20} {
		body := bytes.Repeat([]byte("typical request tool call history "), (size+32)/33)[:size]
		for _, reuse := range []bool{false, true} {
			b.Run(fmt.Sprintf("bytes=%d/reuse=%v", size, reuse), func(b *testing.B) {
				r := httptest.NewRequest(http.MethodPost, "/responses", nil)
				r.Header.Set("Content-Encoding", "zstd")
				b.SetBytes(int64(len(body)))
				b.ReportAllocs()
				for b.Loop() {
					if reuse {
						if _, _, err := prepareGatewayRequestBody(&Proxy{}, r, "/responses", body); err != nil {
							b.Fatal(err)
						}
					} else {
						encoder, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1), zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(3)))
						if err != nil {
							b.Fatal(err)
						}
						_ = encoder.EncodeAll(body, nil)
						_ = encoder.Close()
					}
				}
			})
		}
	}
}
