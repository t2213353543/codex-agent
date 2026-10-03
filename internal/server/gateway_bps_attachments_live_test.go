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

func TestBPSLiveInlineImageAttachment(t *testing.T) {
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
	uploads := 0
	next := p.Client.Transport
	if next == nil {
		next = http.DefaultTransport
	}
	p.Client.Transport = bpsCacheLiveTransport(func(req *http.Request) (*http.Response, error) {
		res, e := next.RoundTrip(req)
		if e == nil && strings.HasSuffix(req.URL.Path, "/attachments") && req.Method == http.MethodPost {
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
		return res, e
	})
	t.Cleanup(func() {
		for _, id := range uploaded {
			req, _ := http.NewRequest(http.MethodDelete, p.BaseURL+"/attachments/delete/"+url.PathEscape(id), nil)
			applyBPSHeaders(req.Header, account)
			res, e := p.Client.Do(req)
			if e != nil {
				t.Error("synthetic attachment cleanup failed")
				continue
			}
			_, _ = io.Copy(io.Discard, res.Body)
			_ = res.Body.Close()
			if res.StatusCode < 200 || res.StatusCode >= 300 {
				t.Errorf("synthetic attachment cleanup status=%d", res.StatusCode)
			}
		}
	})
	picture := image.NewRGBA(image.Rect(0, 0, 128, 128))
	for y := 0; y < 128; y++ {
		for x := 0; x < 128; x++ {
			picture.Set(x, y, color.RGBA{0, 0, 255, 255})
		}
	}
	var content bytes.Buffer
	_ = png.Encode(&content, picture)
	input := []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "Look at the supplied image. Reply with its dominant color as a single uppercase English word. Do not use tools."}, map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(content.Bytes())}}}}
	body, _ := json.Marshal(map[string]any{"model": "gpt-6-astra", "reasoning": map[string]string{"effort": "xhigh"}, "stream": false, "input": input})
	for turn := 0; turn < 2; turn++ {
		r := proxyRequest(http.MethodPost, "/v1/responses", string(body))
		r.Header.Set("Session-Id", "live-image-attachment")
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
		if w.Code != 200 || result.Status != "completed" || strings.TrimSpace(answer) != "BLUE" {
			t.Fatalf("image verification failed: http=%d completed=%t color_match=%t", w.Code, result.Status == "completed", strings.TrimSpace(answer) == "BLUE")
		}
		t.Logf("live_image_attachment turn=%d http=%d completed=true image_color_verified=true", turn, w.Code)
	}
	mu.Lock()
	count := uploads
	mu.Unlock()
	if count != 1 {
		t.Fatalf("repeated image uploaded %d times", count)
	}
	t.Log("live_image_attachment uploads=1 generations=2 raw_image_preserved=true")
}
