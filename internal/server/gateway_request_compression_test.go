package server

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/klauspost/compress/zstd"
)

func compressionTestEncoder(t *testing.T) *zstd.Encoder {
	t.Helper()
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1), zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(3)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = encoder.Close() })
	return encoder
}

func compressionTestDecode(t *testing.T, raw []byte) []byte {
	t.Helper()
	decoder, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(maxCompressedBody))
	if err != nil {
		t.Fatal(err)
	}
	defer decoder.Close()
	body, err := decoder.DecodeAll(raw, nil)
	if err != nil {
		t.Fatal("test observed invalid upstream zstd body")
	}
	return body
}

func TestGatewayRequestCompressionIntentGate(t *testing.T) {
	for _, scenario := range []struct {
		method, path, encoding string
		want                   bool
	}{
		{http.MethodPost, "/responses", "zstd", true},
		{http.MethodPost, "/responses", " ZSTD ", true},
		{http.MethodPost, "/responses", "", false},
		{http.MethodPost, "/responses", "identity", false},
		{http.MethodPost, "/responses", "gzip", false},
		{http.MethodPost, "/responses", "zstd, gzip", false},
		{http.MethodPost, "/responses/compact", "zstd", false},
		{http.MethodGet, "/models", "zstd", false},
		{http.MethodGet, "/responses", "zstd", false},
	} {
		t.Run(scenario.method+scenario.path+"/"+scenario.encoding, func(t *testing.T) {
			r := httptest.NewRequest(scenario.method, scenario.path, nil)
			r.Header.Set("Content-Encoding", scenario.encoding)
			if got := gatewayWantsRequestCompression(r, scenario.path); got != scenario.want {
				t.Fatalf("compression gate=%v want=%v", got, scenario.want)
			}
		})
	}
	r := httptest.NewRequest(http.MethodPost, "/responses", nil)
	r.Header.Add("Content-Encoding", "zstd")
	r.Header.Add("Content-Encoding", "identity")
	if gatewayWantsRequestCompression(r, "/responses") {
		t.Fatal("unsupported stacked encodings enabled compression")
	}
}

func TestGatewayRequestCompressionUsesPreparedPolicyBody(t *testing.T) {
	encoder := compressionTestEncoder(t)
	const input = `[{"type":"reasoning","encrypted_content":"opaque-history"},{"type":"function_call_output","call_id":"call-1","output":"result"}]`
	raw := []byte(`{"model":"test","stream":true,"instructions":"user instructions","input":` + input + `,"parallel_tool_calls":true}`)
	for _, encoded := range []bool{false, true} {
		for _, policy := range []bool{false, true} {
			t.Run(fmt.Sprintf("zstd=%v/policy=%v", encoded, policy), func(t *testing.T) {
				var attempts atomic.Int32
				p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
					attempts.Add(1)
					wire, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
						return
					}
					prepared := wire
					if encoded {
						if r.Header.Get("Content-Encoding") != "zstd" {
							t.Error("incoming zstd intent was dropped")
							return
						}
						prepared = compressionTestDecode(t, wire)
					} else if r.Header.Get("Content-Encoding") != "" {
						t.Error("identity client was silently compressed")
					}
					var actual map[string]json.RawMessage
					if json.Unmarshal(prepared, &actual) != nil {
						t.Error("prepared request is not JSON")
						return
					}
					instructions := "user instructions"
					if policy {
						instructions = "gateway prefix\n\n" + instructions
					}
					var observed string
					_ = json.Unmarshal(actual["instructions"], &observed)
					if observed != instructions || string(actual["input"]) != input || string(actual["stream"]) != "true" || string(actual["store"]) != "false" {
						t.Error("compressed wire replayed original body or altered prepared conversation")
					}
					_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[]}}\n\n")
				})
				settings, _ := p.Settings.Get()
				settings.BuiltinPromptEnabled, settings.BuiltinPrompt, settings.BuiltinPromptMode = policy, "gateway prefix", "prepend"
				settings.PromptCacheKeyEnabled, settings.CompatibilityNormalizationEnabled = false, false
				if err := p.Settings.Set(settings); err != nil {
					t.Fatal(err)
				}
				wire := raw
				if encoded {
					wire = encoder.EncodeAll(raw, nil)
				}
				r := proxyRequest(http.MethodPost, "/v1/responses", string(wire))
				if encoded {
					r.Header.Set("Content-Encoding", "zstd")
				}
				w := httptest.NewRecorder()
				p.ServeHTTP(w, r)
				if w.Code != http.StatusOK || attempts.Load() != 1 {
					t.Fatalf("compression policy route: status=%d attempts=%d", w.Code, attempts.Load())
				}
			})
		}
	}
}

func TestGatewayRequestCompressionNativeIntentKeepsRawPreparedBody(t *testing.T) {
	raw := []byte(`{"model":"test","input":[],"stream":true,"store":false}`)
	r := httptest.NewRequest(http.MethodPost, "/responses", nil)
	r.Header.Set("Content-Encoding", "zstd")
	native := &Proxy{NativeHTTP: &gatewayNativePool{}}
	wire, compress, err := prepareGatewayRequestBody(native, r, "/responses", raw)
	if err != nil || !compress || !bytes.Equal(wire, raw) {
		t.Fatal("native request must send prepared JSON plus explicit IPC compression instruction")
	}
	legacy := &Proxy{}
	compressed, compress, err := prepareGatewayRequestBody(legacy, r, "/responses", raw)
	if err != nil || compress || !bytes.Equal(compressionTestDecode(t, compressed), raw) {
		t.Fatal("Go request encoding lost prepared JSON or used native instruction")
	}
	for _, path := range []string{"/responses/compact", "/models"} {
		wire, compress, err := prepareGatewayRequestBody(native, r, path, raw)
		if err != nil || compress || !bytes.Equal(wire, raw) {
			t.Fatal("non-Responses endpoint inherited compression intent")
		}
	}
	if gatewayNativeRequestCompression(context.Background()) || !gatewayNativeRequestCompression(withGatewayNativeRequestCompression(context.Background())) {
		t.Fatal("native compression context did not preserve explicit intent")
	}
}

func TestGatewayRequestCompression401ReusesExactWireBytes(t *testing.T) {
	var attempts, refreshes atomic.Int32
	var mu sync.Mutex
	var first []byte
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		wire, _ := io.ReadAll(r.Body)
		if r.URL.Path == "/oauth/token" {
			refreshes.Add(1)
			if r.Header.Get("Content-Encoding") != "" {
				t.Error("OAuth request inherited response compression")
			}
			_, _ = io.WriteString(w, gatewayKnownOwnerRefreshFixture(`{"access_token":"compression-rotated","refresh_token":"compression-rotated-rt"}`, "test-account", "fixture-user"))
			return
		}
		count := attempts.Add(1)
		if r.Header.Get("Content-Encoding") != "zstd" || r.ContentLength != int64(len(wire)) {
			t.Error("compressed request framing changed across 401")
		}
		_ = compressionTestDecode(t, wire)
		mu.Lock()
		defer mu.Unlock()
		if count == 1 {
			first = bytes.Clone(wire)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if count != 2 || !bytes.Equal(first, wire) || r.Header.Get("Authorization") != "Bearer compression-rotated" {
			t.Error("401 changed compressed request bytes or retried more than once")
		}
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[]}}\n\n")
	})
	raw := []byte(`{"model":"test","stream":true,"input":[{"type":"function_call_output","call_id":"call-compressed","output":"once"}]}`)
	r := proxyRequest(http.MethodPost, "/responses", string(compressionTestEncoder(t).EncodeAll(raw, nil)))
	r.Header.Set("Content-Encoding", "zstd")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	if w.Code != http.StatusOK || attempts.Load() != 2 || refreshes.Load() != 1 {
		t.Fatalf("compressed 401 lifecycle: status=%d attempts=%d refreshes=%d", w.Code, attempts.Load(), refreshes.Load())
	}
}

type compressionZeroReader struct{}

func (compressionZeroReader) Read(data []byte) (int, error) {
	clear(data)
	return len(data), nil
}

func TestGatewayRequestCompressionLimitsRejectBeforeUpstream(t *testing.T) {
	encoder := compressionTestEncoder(t)
	for _, scenario := range []string{"decoded", "wire", "prepared-growth", "damaged-frame", "stacked-encoding"} {
		t.Run(scenario, func(t *testing.T) {
			var attempts atomic.Int32
			p := proxyFixture(t, func(w http.ResponseWriter, _ *http.Request) { attempts.Add(1); w.WriteHeader(http.StatusOK) })
			settings, _ := p.Settings.Get()
			settings.BuiltinPromptEnabled, settings.PromptCacheKeyEnabled, settings.CompatibilityNormalizationEnabled = false, false, false
			if err := p.Settings.Set(settings); err != nil {
				t.Fatal(err)
			}
			encoding, status := "zstd", http.StatusRequestEntityTooLarge
			var body io.Reader
			switch scenario {
			case "decoded":
				body = bytes.NewReader(encoder.EncodeAll(bytes.Repeat([]byte{'x'}, maxBody+1), nil))
			case "wire":
				var prefix [8]byte
				binary.LittleEndian.PutUint32(prefix[:4], 0x184d2a50)
				binary.LittleEndian.PutUint32(prefix[4:], maxBody+1)
				body = io.MultiReader(bytes.NewReader(prefix[:]), io.LimitReader(compressionZeroReader{}, maxBody+1))
			case "prepared-growth":
				encoding = "identity"
				const prefix, suffix = `{"model":"test","input":"`, `"}`
				body = io.MultiReader(strings.NewReader(prefix), strings.NewReader(strings.Repeat("x", maxBody-len(prefix)-len(suffix))), strings.NewReader(suffix))
			case "damaged-frame":
				body, status = strings.NewReader("invalid-zstd"), http.StatusBadRequest
			case "stacked-encoding":
				encoding, body, status = "zstd, gzip", strings.NewReader(`{}`), http.StatusUnsupportedMediaType
			}
			r := httptest.NewRequest(http.MethodPost, "/responses", body)
			r.Header.Set("Authorization", "Bearer test-client-key")
			r.Header.Set("Content-Encoding", encoding)
			w := httptest.NewRecorder()
			p.ServeHTTP(w, r)
			if w.Code != status || attempts.Load() != 0 || p.inFlight("test") != 0 {
				t.Fatalf("body limit crossed upstream boundary: status=%d want=%d attempts=%d inflight=%d", w.Code, status, attempts.Load(), p.inFlight("test"))
			}
		})
	}
}

func TestGatewayRequestCompressionEncodedEnvelopeLimit(t *testing.T) {
	// Encoder overhead may expand incompressible input. The decoded request
	// limit stays 32 MiB; only the outbound compressed envelope gets headroom.
	if maxCompressedBody != maxBody+(1<<20) {
		t.Fatal("compressed envelope limit is not the explicit 33 MiB contract")
	}
	r := httptest.NewRequest(http.MethodPost, "/responses", nil)
	r.Header.Set("Content-Encoding", "zstd")
	raw := make([]byte, maxBody+1)
	for _, p := range []*Proxy{{}, {NativeHTTP: &gatewayNativePool{}}} {
		if _, _, err := prepareGatewayRequestBody(p, r, "/responses", raw); !errors.Is(err, errGatewayPreparedBodyLimit) {
			t.Fatalf("oversized decoded body was allowed into compression: %v", err)
		}
	}
	raw = raw[:maxBody]
	if _, err := rand.New(rand.NewSource(42)).Read(raw); err != nil {
		t.Fatal(err)
	}
	wire, native, err := prepareGatewayRequestBody(&Proxy{}, r, "/responses", raw)
	if err != nil || native || len(wire) <= maxBody || len(wire) > maxCompressedBody {
		t.Fatalf("incompressible boundary mishandled: wire=%d native=%v err=%v", len(wire), native, err)
	}
	if decoded := compressionTestDecode(t, wire); !bytes.Equal(decoded, raw) {
		t.Fatal("compressed envelope boundary corrupted decoded bytes")
	}
	prepared, native, err := prepareGatewayRequestBody(&Proxy{NativeHTTP: &gatewayNativePool{}}, r, "/responses", raw)
	if err != nil || !native || len(prepared) != maxBody || !bytes.Equal(prepared, raw) {
		t.Fatal("native IPC confused its decoded bound with compressed wire headroom")
	}
}

func TestGatewayRequestCompressionExcludedRoutesStayUnencoded(t *testing.T) {
	encoder := compressionTestEncoder(t)
	for _, path := range []string{"/responses/compact", "/models"} {
		t.Run(path, func(t *testing.T) {
			var calls atomic.Int32
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Header.Get("Content-Encoding") != "" {
					t.Error("excluded route inherited HTTP zstd")
				}
				body, _ := io.ReadAll(r.Body)
				if path == "/responses/compact" {
					var object map[string]json.RawMessage
					if json.Unmarshal(body, &object) != nil || object["stream"] != nil || object["store"] != nil {
						t.Error("compact preparation changed while disabling compression")
					}
				} else if len(body) != 0 {
					t.Error("models GET gained a request body")
				}
				_, _ = io.WriteString(w, `{"output":[],"models":[]}`)
			})
			method, body := http.MethodGet, ""
			if path == "/responses/compact" {
				method = http.MethodPost
				body = string(encoder.EncodeAll([]byte(`{"model":"test","input":[],"stream":true,"store":true}`), nil))
			}
			r := proxyRequest(method, "/v1"+path, body)
			r.Header.Set("Content-Encoding", "zstd")
			w := httptest.NewRecorder()
			p.ServeHTTP(w, r)
			if w.Code != http.StatusOK || calls.Load() != 1 {
				t.Fatalf("excluded route result: HTTP=%d calls=%d", w.Code, calls.Load())
			}
		})
	}
	t.Run("websocket", func(t *testing.T) {
		var calls atomic.Int32
		p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			if r.Header.Get("Content-Encoding") != "" {
				t.Error("WS handshake inherited HTTP compression")
			}
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer conn.CloseNow()
			_, body, err := conn.Read(r.Context())
			if err != nil || !json.Valid(body) {
				t.Error("WS message was encoded as an HTTP compression body")
				return
			}
			_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.completed","response":{"output":[]}}`))
			_, _, _ = conn.Read(r.Context())
		})
		enableGatewayWS(t, p, "")
		server := httptest.NewServer(p)
		defer server.Close()
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		defer cancel()
		conn, _, err := websocket.Dial(ctx, server.URL+"/responses", &websocket.DialOptions{HTTPHeader: http.Header{
			"Authorization": {"Bearer test-client-key"}, "Content-Encoding": {"zstd"}, "Session-Id": {"compression-excluded-ws"},
		}})
		if err != nil {
			t.Fatal(err)
		}
		defer conn.CloseNow()
		if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create","model":"test","input":[]}`)); err != nil {
			t.Fatal(err)
		}
		if _, body, err := conn.Read(ctx); err != nil || !strings.Contains(string(body), "response.completed") || calls.Load() != 1 {
			t.Fatal("excluded WS route failed")
		}
	})
}
