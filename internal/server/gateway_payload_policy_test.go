package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

func TestAppServerUnboundedIngressAndPreparedRequest(t *testing.T) {
	payload := strings.Repeat("a", 33<<20)
	want := sha256.Sum256([]byte(payload))
	raw := []byte(`{"model":"fixture","stream":false,"prompt_cache_key":"client-cache","input":[{"type":"additional_tools","role":"developer","tools":[]},{"type":"message","role":"user","content":[{"type":"input_text","text":"` + payload + `"}]}]}`)
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
	if err != nil {
		t.Fatal(err)
	}
	defer encoder.Close()
	for _, encoding := range []string{"identity", "zstd"} {
		t.Run(encoding, func(t *testing.T) {
			p := newTestProxy(t, 1)
			p.RuntimeModes = true
			p.AppServerCLI = &appServerCLI{}
			settings, _ := p.Settings.Get()
			settings.InferenceMode = inferenceCodexCLI
			settings.BuiltinPromptEnabled = false
			settings.PromptCacheKeyEnabled = true
			settings.AutomaticTokenRefreshEnabled = false
			if err := p.Settings.Set(settings); err != nil {
				t.Fatal(err)
			}
			called := false
			p.Client = &http.Client{Transport: appServerFailoverRoundTripper(func(req *http.Request) (*http.Response, error) {
				called = true
				if !gatewayUnboundedPayload(req.Context()) {
					t.Error("payload policy lost before dispatch")
				}
				var source io.Reader = req.Body
				if req.Header.Get("Content-Encoding") == "zstd" {
					decoder, err := zstd.NewReader(req.Body, zstd.WithDecoderMaxMemory(1<<34), zstd.WithDecoderConcurrency(1))
					if err != nil {
						t.Fatal(err)
					}
					defer decoder.Close()
					source = decoder
				}
				body, err := io.ReadAll(source)
				if err != nil {
					t.Fatal(err)
				}
				var parsed struct {
					Input []struct{ Content []struct{ Text string } }
					Cache string `json:"prompt_cache_key"`
				}
				if json.Unmarshal(body, &parsed) != nil || len(parsed.Input) != 2 || len(parsed.Input[1].Content) != 1 {
					t.Fatal("large request shape lost")
				}
				if sha256.Sum256([]byte(parsed.Input[1].Content[0].Text)) != want {
					t.Error("large input truncated")
				}
				if parsed.Cache == "client-cache" {
					t.Error("unbounded bypassed per-account cache rewrite")
				}
				response := `data: {"type":"response.completed","response":{"id":"resp_large","status":"completed","output":[],"usage":{"input_tokens":10,"output_tokens":1,"total_tokens":11}}}` + "\n\n"
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(response)), Request: req}, nil
			})}
			server := httptest.NewServer(p)
			defer server.Close()
			wire := raw
			if encoding == "zstd" {
				wire = encoder.EncodeAll(raw, nil)
			}
			req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/v1/responses", bytes.NewReader(wire))
			req.Header.Set("Authorization", "Bearer test-client-key")
			req.Header.Set("Content-Encoding", encoding)
			req.Header.Set("Session-Id", "large-session")
			res, err := server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			data, _ := io.ReadAll(res.Body)
			_ = res.Body.Close()
			if !called || res.StatusCode != 200 || !bytes.Contains(data, []byte("resp_large")) {
				t.Fatalf("large request rejected: %d %s", res.StatusCode, data)
			}
		})
	}
	if _, _, err := prepareHTTPBody(raw, false, nil); err == nil {
		t.Fatal("legacy size contract unexpectedly removed")
	}
	legacy := httptest.NewRequest(http.MethodPost, "/responses", bytes.NewReader(raw))
	if _, err := readBody(httptest.NewRecorder(), legacy); err == nil {
		t.Fatal("legacy body limit unexpectedly removed")
	}
}

func TestAppServerUnboundedCompactionRetainsCallerMessages(t *testing.T) {
	large := strings.Repeat("C", 300<<10)
	request, _ := json.Marshal(map[string]any{"model": "fixture", "input": []any{map[string]string{"type": "message", "role": "developer", "content": "KEEP INSTRUCTIONS"}, map[string]string{"type": "message", "role": "user", "content": large}}})
	response := []byte(`{"id":"resp_compaction","status":"completed","output":[{"type":"compaction","encrypted_content":"opaque"}]}`)
	actual, err := responsesCompactionResult(request, response, true)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(actual, []byte("KEEP INSTRUCTIONS")) || !bytes.Contains(actual, []byte(large)) {
		t.Fatal("local 64K token approximation discarded caller history")
	}
	if bytes.Count(actual, []byte(`"type":"compaction"`)) != 1 {
		t.Fatal("missing/duplicate compaction")
	}
}

func TestAppServerUnboundedSessionAndAffinity(t *testing.T) {
	ctx := withGatewayUnboundedPayload(withInferenceMode(context.Background(), inferenceCodexCLI), true)
	p := &Proxy{RuntimeModes: true, inferenceSessions: map[string]inferenceSessionBinding{}, sessionRoutes: map[string]gatewaySessionRoute{}}
	for i := 0; i < maxGatewaySessions; i++ {
		key := fmt.Sprint(i)
		p.sessionRoutes[key] = gatewaySessionRoute{Account: "existing", Expires: time.Now().Add(time.Hour)}
		if i < 4096 {
			p.inferenceSessions[key] = inferenceSessionBinding{mode: inferenceCodexCLI, expires: time.Now().Add(time.Hour)}
		}
	}
	if err := p.bindInferenceSession(ctx, "key", "new-session"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.sessionAccount(ctx, "key", "new-session", "new-account"); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("x", (1<<20)+1)
	raw, _ := json.Marshal(map[string]string{"model": "fixture", "input": long})
	if gatewayContentSessionKey(raw, true) == "" {
		t.Fatal("large first input lost stable account affinity")
	}
	if _, ok := gatewayCacheAffinitySeed("key", raw, true); !ok {
		t.Fatal("large first input lost cache affinity")
	}
}

func TestAppServerUnboundedMissingNativeCapabilityIsLocal(t *testing.T) {
	if gatewayTransportFailureStatus(errGatewayNativeUnboundedReady) != http.StatusServiceUnavailable {
		t.Fatal("old helper misclassified as upstream 502")
	}
	rec := &RequestRecord{}
	observeGatewayTransportError(rec, errGatewayNativeUnboundedReady, "response_headers")
	if rec.ErrorSource != "gateway" || rec.ErrorPhase != "native_payload_capability" {
		t.Fatalf("wrong capability diagnosis: %s/%s", rec.ErrorSource, rec.ErrorPhase)
	}
}
