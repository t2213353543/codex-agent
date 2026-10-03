package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
)

// This opt-in test sends only new synthetic history. It exercises the real
// gateway and attachment transport, not a replay of any captured customer turn.
func TestBPSLiveToolInlineImageAttachment(t *testing.T) {
	file, binary, work := os.Getenv("BPS_LIVE_ACCOUNTS_FILE"), os.Getenv("BPS_LIVE_CODEX_BINARY"), os.Getenv("BPS_LIVE_WORK_ROOT")
	if file == "" || binary == "" || work == "" {
		t.Skip("explicit server-only live fixture required")
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal("fixture unavailable")
	}
	var fixture struct {
		Accounts []bpsLiveAccount `json:"accounts"`
	}
	if json.Unmarshal(raw, &fixture) != nil || len(fixture.Accounts) == 0 {
		t.Fatal("invalid fixture")
	}
	a := fixture.Accounts[0]
	var evidence []map[string]any
	h := newBPSLiveHarness(t, binary, work, a, []string{a.AccessToken, a.AccountID}, &evidence)
	p := h.recorder.p
	account, err := p.Store.Get("test")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var uploaded []string
	uploads, generations, translatedImages := 0, 0, 0
	next := p.Client.Transport
	if next == nil {
		next = http.DefaultTransport
	}
	p.Client.Transport = bpsCacheLiveTransport(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/responses") {
			data, readErr := io.ReadAll(req.Body)
			_ = req.Body.Close()
			if readErr != nil {
				return nil, readErr
			}
			req.Body = io.NopCloser(bytes.NewReader(data))
			var body struct {
				Input []struct {
					Type   string          `json:"type"`
					Output json.RawMessage `json:"output"`
				} `json:"input"`
			}
			if json.Unmarshal(data, &body) != nil {
				t.Error("invalid outbound request")
			}
			count := 0
			for _, item := range body.Input {
				if item.Type != "custom_tool_call_output" {
					continue
				}
				var parts []struct {
					Type   string `json:"type"`
					FileID string `json:"file_id"`
					URL    string `json:"image_url"`
					Detail string `json:"detail"`
				}
				if json.Unmarshal(item.Output, &parts) != nil {
					t.Error("custom output array was not preserved")
					continue
				}
				for _, part := range parts {
					if part.Type != "input_image" {
						continue
					}
					if part.FileID == "" || part.URL != "" || part.Detail != "auto" {
						t.Error("custom tool image was not translated to an attachment")
					} else {
						count++
					}
				}
			}
			mu.Lock()
			generations++
			translatedImages += count
			mu.Unlock()
		}
		res, transportErr := next.RoundTrip(req)
		if transportErr == nil && req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/attachments") {
			data, readErr := io.ReadAll(res.Body)
			_ = res.Body.Close()
			res.Body = io.NopCloser(bytes.NewReader(data))
			var reply struct {
				ID string `json:"openai_file_id"`
			}
			_ = json.Unmarshal(data, &reply)
			mu.Lock()
			uploads++
			if reply.ID != "" {
				uploaded = append(uploaded, reply.ID)
			}
			mu.Unlock()
			if readErr != nil {
				return nil, readErr
			}
		}
		return res, transportErr
	})
	t.Cleanup(func() {
		mu.Lock()
		ids := append([]string(nil), uploaded...)
		mu.Unlock()
		for _, id := range ids {
			req, _ := http.NewRequest(http.MethodDelete, p.BaseURL+"/attachments/delete/"+url.PathEscape(id), nil)
			applyBPSHeaders(req.Header, account)
			res, cleanupErr := p.Client.Do(req)
			if cleanupErr != nil {
				t.Error("synthetic tool attachment cleanup failed")
				continue
			}
			_, _ = io.Copy(io.Discard, res.Body)
			_ = res.Body.Close()
			if res.StatusCode < 200 || res.StatusCode >= 300 {
				t.Errorf("synthetic tool attachment cleanup status=%d", res.StatusCode)
			}
		}
	})
	picture := image.NewRGBA(image.Rect(0, 0, 512, 512))
	for y := 0; y < 512; y++ {
		for x := 0; x < 512; x++ {
			picture.Set(x, y, color.RGBA{0, 0, 255, 255})
		}
	}
	var pngBytes bytes.Buffer
	if err := png.Encode(&pngBytes, picture); err != nil {
		t.Fatal(err)
	}
	imageURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(pngBytes.Bytes())
	tools := []any{map[string]any{"type": "namespace", "name": "functions", "description": "Synthetic tool image verification.", "tools": []any{map[string]any{"type": "custom", "name": "exec", "description": "Read a synthetic image fixture.", "format": map[string]string{"type": "grammar", "syntax": "lark", "definition": "start: /[\\s\\S]+/"}}}}}
	input := []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]string{"type": "input_text", "text": "Read the two synthetic image results."}}}}
	for _, suffix := range []string{"first", "second"} {
		input = append(input,
			map[string]any{"type": "custom_tool_call", "id": "ctc_live_tool_image_" + suffix, "call_id": "call_live_tool_image_" + suffix, "name": "exec", "namespace": "functions", "input": "read_synthetic_image()", "status": "completed"},
			map[string]any{"type": "custom_tool_call_output", "call_id": "call_live_tool_image_" + suffix, "output": []any{map[string]string{"type": "input_text", "text": "Synthetic image read successfully."}, map[string]string{"type": "input_image", "image_url": imageURL}}})
	}
	input = append(input, map[string]any{"type": "message", "role": "user", "content": []any{map[string]string{"type": "input_text", "text": "Look at the supplied images. Reply with their common dominant color as a single uppercase English word. Do not use tools."}}})
	body, _ := json.Marshal(map[string]any{"model": "gpt-6-astra", "reasoning": map[string]string{"effort": "xhigh"}, "stream": false, "tools": tools, "input": input})
	for turn := 0; turn < 2; turn++ {
		r := proxyRequest(http.MethodPost, "/v1/responses", string(body))
		r.Header.Set("Session-Id", "live-tool-image-attachment")
		w := httptest.NewRecorder()
		p.gatewayBPS(w, r)
		var result struct {
			Status string
			Output []struct {
				Type    string
				Content []struct{ Type, Text string }
			}
		}
		_ = json.Unmarshal(w.Body.Bytes(), &result)
		answer := ""
		for _, item := range result.Output {
			if item.Type == "message" {
				for _, part := range item.Content {
					if part.Type == "output_text" {
						answer += part.Text
					}
				}
			}
		}
		if w.Code != http.StatusOK || result.Status != "completed" || strings.TrimSpace(answer) != "BLUE" {
			t.Fatalf("custom tool image verification failed: http=%d completed=%t color_match=%t", w.Code, result.Status == "completed", strings.TrimSpace(answer) == "BLUE")
		}
		t.Logf("live_tool_image_attachment turn=%d http=%d completed=true image_color_verified=true", turn, w.Code)
	}
	mu.Lock()
	uploadCount, generationCount, imageCount := uploads, generations, translatedImages
	mu.Unlock()
	if uploadCount != 1 || generationCount != 2 || imageCount != 4 {
		t.Fatalf("custom tool image counts: uploads=%d generations=%d translated_images=%d", uploadCount, generationCount, imageCount)
	}
	t.Log("live_tool_image_attachment uploads=1 generations=2 translated_images=4 original_tool_type_preserved=true")
}
