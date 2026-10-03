package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestGatewayNativeActualCompressionPolicyAndRetry(t *testing.T) {
	var calls, refreshes atomic.Int32
	var mu sync.Mutex
	var wireHashes [][32]byte
	var preparedBodies [][]byte
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" {
			refreshes.Add(1)
			if r.Header.Get("Content-Encoding") != "" {
				t.Error("OAuth request received Responses compression")
			}
			_, _ = io.Copy(io.Discard, r.Body)
			_, _ = io.WriteString(w, gatewayKnownOwnerRefreshFixture(`{"access_token":"synthetic-compressed-refresh","refresh_token":"synthetic-compressed-rt"}`, "test-account", "fixture-user"))
			return
		}
		wire, err := io.ReadAll(r.Body)
		if err != nil || r.Header.Get("Content-Encoding") != "zstd" || r.ContentLength != int64(len(wire)) {
			t.Error("native compressed headers or body invalid")
			w.WriteHeader(400)
			return
		}
		decoder, err := zstd.NewReader(bytes.NewReader(wire), zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(2*maxBody))
		if err != nil {
			t.Error(err)
			return
		}
		decoded, err := io.ReadAll(decoder)
		decoder.Close()
		if err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		wireHashes = append(wireHashes, sha256.Sum256(wire))
		preparedBodies = append(preparedBodies, decoded)
		mu.Unlock()
		if calls.Add(1) == 1 {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"compressed\",\"output\":[],\"status\":\"completed\"}}\n\n")
	})
	realGatewayNativePool(t, p)
	settings, _ := p.Settings.Get()
	settings.BuiltinPromptEnabled, settings.BuiltinPrompt = true, "synthetic-prepared-policy"
	if err := p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
	input := []byte(`{"model":"test","stream":true,"instructions":"original","input":"hello","prompt_cache_key":"compression-thread"}`)
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
	if err != nil {
		t.Fatal(err)
	}
	compressed := encoder.EncodeAll(input, nil)
	encoder.Close()
	r := proxyRequest("POST", "/responses", "")
	r.Body = io.NopCloser(bytes.NewReader(compressed))
	r.ContentLength = int64(len(compressed))
	r.Header.Set("Content-Encoding", "zstd")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	if w.Code != 200 || calls.Load() != 2 || refreshes.Load() != 1 {
		t.Fatalf("compressed 401 lifecycle failed: status=%d calls=%d refresh=%d", w.Code, calls.Load(), refreshes.Load())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(wireHashes) != 2 || wireHashes[0] != wireHashes[1] || !bytes.Equal(preparedBodies[0], preparedBodies[1]) {
		t.Fatal("compressed retry changed wire or decoded bytes")
	}
	var payload map[string]json.RawMessage
	if json.Unmarshal(preparedBodies[0], &payload) != nil || !bytes.Contains(payload["instructions"], []byte("synthetic-prepared-policy")) || !bytes.HasPrefix(payload["input"], []byte("[")) {
		t.Fatal("compressed body bypassed preparation or gateway policy")
	}
	if rec, _ := p.Metrics.LastFor("test"); rec.SentModel != "test" || len(rec.Attempts) != 3 {
		t.Fatal("compression erased request observations")
	}
}

func TestGatewayNativeActualCompressionBoundaries(t *testing.T) {
	var hits atomic.Int32
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Content-Encoding") != "" {
			t.Error("unrequested outbound compression")
		}
		_, _ = io.WriteString(w, "ok")
	})
	realGatewayNativePool(t, p)
	client := p.clientFor(nil)
	for _, body := range []string{"not-json", " {\"valid\":true} "} {
		ctx := withGatewayNativeRequestCompression(context.Background())
		req, _ := http.NewRequestWithContext(ctx, "POST", p.BaseURL+"/responses", strings.NewReader(body))
		res, err := client.Do(req)
		if res != nil {
			_ = res.Body.Close()
		}
		if err == nil {
			t.Fatal("invalid/noncanonical JSON compression was accepted")
		}
	}
	ctx := withGatewayNativeRequestCompression(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", p.BaseURL+"/responses", strings.NewReader(`{}`))
	req.Header.Set("Content-Encoding", "zstd")
	if res, err := client.Do(req); err == nil {
		if res != nil {
			_ = res.Body.Close()
		}
		t.Fatal("double compression accepted")
	}
	if hits.Load() != 0 {
		t.Fatal("compression preflight failure reached upstream")
	}
}
