package server

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

// This regression intentionally uses only the original WS adapter API so it
// also runs against the first deployed preference release plus the nil guard.
func TestBPSWebSocketRejectedHandshakeDoesNotPanic(t *testing.T) {
	for _, status := range []int{401, 403, 429, 500} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Error("rejected handshake sent a generation")
				}
				w.WriteHeader(status)
			}))
			defer server.Close()
			p := newTestProxy(t, 1)
			req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/responses", nil)
			req.Header.Set("Authorization", "Bearer synthetic-rejection-guard")
			response, err := p.bpsWebSocketRequest(req, nil, []byte(`{"model":"fixture","input":[]}`), defaultSettings())
			if response != nil && response.Body != nil {
				defer response.Body.Close()
			}
			if err != nil || response == nil || response.StatusCode != status {
				t.Fatalf("rejected handshake lost its HTTP status: response=%v err=%v", response, err)
			}
		})
	}
}
