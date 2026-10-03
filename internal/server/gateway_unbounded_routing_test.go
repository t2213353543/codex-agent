package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAppServerUnboundedFailoverReplaysCompleteRequest(t *testing.T) {
	p := appServerAuxiliaryTestProxy(t)
	if err := p.Store.UpdateAccount("test", func(a *Account) error {
		a.Priority, a.AccessToken, a.RefreshToken = 100, "bad", ""
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	credentials, _ := json.Marshal(map[string]any{"access_token": "good", "expires_at": time.Now().Add(time.Hour).Unix()})
	if err := p.Store.ImportWith("good", credentials, AccountDefaults{Concurrency: 1, Priority: 0}); err != nil {
		t.Fatal(err)
	}
	p.invalidateAccounts()
	payload := strings.Repeat("r", 33<<20)
	want := sha256.Sum256([]byte(payload))
	calls := 0
	cacheKeys := map[string]bool{}
	p.Client = &http.Client{Transport: appServerFailoverRoundTripper(func(req *http.Request) (*http.Response, error) {
		calls++
		if !gatewayUnboundedPayload(req.Context()) {
			t.Error("account failover lost unrestricted payload policy")
		}
		var body struct {
			Input []struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"input"`
			Cache string `json:"prompt_cache_key"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil || len(body.Input) != 2 || len(body.Input[1].Content) != 1 || sha256.Sum256([]byte(body.Input[1].Content[0].Text)) != want {
			t.Fatalf("large replay was changed: %v", err)
		}
		cacheKeys[body.Cache] = true
		status, response := http.StatusUnauthorized, `{"error":{"code":"unauthorized","message":"synthetic invalid account"}}`
		if req.Header.Get("Authorization") == "Bearer good" {
			status = http.StatusOK
			response = `data: {"type":"response.completed","response":{"id":"large_replayed","status":"completed","output":[],"usage":{"input_tokens":10,"output_tokens":1}}}` + "\n\n"
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(response)), Request: req}, nil
	})}
	raw := `{"model":"fixture","stream":false,"prompt_cache_key":"same-large-cache","input":[{"type":"additional_tools","role":"developer","tools":[]},{"type":"message","role":"user","content":[{"type":"input_text","text":"` + payload + `"}]}]}`
	req := appServerAuxiliaryTestRequest("/v1/responses", raw, "test-client-key")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, req)
	if w.Code != http.StatusOK || calls != 2 || !bytes.Contains(w.Body.Bytes(), []byte("large_replayed")) || len(cacheKeys) != 2 || cacheKeys["same-large-cache"] {
		t.Fatalf("large request did not survive account-scoped replay: status=%d calls=%d cache namespaces=%d", w.Code, calls, len(cacheKeys))
	}
	if p.inFlight("test") != 0 || p.inFlight("good") != 0 {
		t.Fatal("failover retained an account concurrency lease")
	}
}

func TestAppServerUnboundedAuxiliaryAndModelsBodies(t *testing.T) {
	for _, scenario := range []struct {
		path, method string
		size         int
	}{
		{path: "/v1/images/edits", method: http.MethodPost, size: 33 << 20},
		{path: "/v1/models", method: http.MethodGet, size: 9 << 20},
	} {
		t.Run(scenario.path, func(t *testing.T) {
			p := appServerAuxiliaryTestProxy(t)
			large := strings.Repeat("a", scenario.size)
			request := `{"model":"fixture","image":"` + large + `"}`
			response := `{"data":[],"models":[],"opaque":"` + large + `"}`
			p.Client = &http.Client{Transport: appServerFailoverRoundTripper(func(req *http.Request) (*http.Response, error) {
				if !gatewayUnboundedPayload(req.Context()) {
					t.Error("auxiliary/models request lost unrestricted payload policy")
				}
				if scenario.method == http.MethodPost {
					raw, err := io.ReadAll(req.Body)
					if err != nil || sha256.Sum256(raw) != sha256.Sum256([]byte(request)) {
						t.Error("large auxiliary request was changed")
					}
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(response)), Request: req}, nil
			})}
			req := appServerAuxiliaryTestRequest(scenario.path, request, "test-client-key")
			req.Method = scenario.method
			w := httptest.NewRecorder()
			p.ServeHTTP(w, req)
			if w.Code != http.StatusOK || sha256.Sum256(w.Body.Bytes()) != sha256.Sum256([]byte(response)) {
				t.Fatalf("large auxiliary/models response was changed: status=%d bytes=%d", w.Code, w.Body.Len())
			}
		})
	}
}
