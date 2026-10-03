package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBPSWebSocketReuseSettingsRoundTrip(t *testing.T) {
	p, handler := webTestSetup(t)
	get := httptest.NewRequest(http.MethodGet, "/api/settings", nil)
	get.Header.Set("Authorization", "Bearer "+webTestKey)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, get)
	if response.Code != http.StatusOK {
		t.Fatalf("management GET: %d", response.Code)
	}
	var payload map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if _, ok := payload["bps_websocket_reuse"]; !ok {
		t.Fatal("bps_websocket_reuse missing from management payload")
	}
	body, _ := json.Marshal(map[string]any{"bps_websocket_reuse": true})
	post := httptest.NewRequest(http.MethodPost, "/api/settings", bytes.NewReader(body))
	post.Header.Set("Authorization", "Bearer "+webTestKey)
	post.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, post)
	if response.Code != http.StatusOK {
		t.Fatalf("management POST: %d %s", response.Code, response.Body.String())
	}
	settings, err := p.Settings.Get()
	if err != nil {
		t.Fatal(err)
	}
	if !settings.BPSWebSocketReuse || !settings.PreferWebsocket {
		// PreferWebsocket may be false by default; the reuse flag itself must persist.
		if settings.BPSWebSocketReuse {
			t.Log("reuse enabled")
		}
	}
	if !settings.BPSWebSocketReuse {
		t.Fatal("bps_websocket_reuse did not persist")
	}
}
