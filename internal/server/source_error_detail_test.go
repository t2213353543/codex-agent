package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSourceHTTPErrorDetailPreservesResponseAndDiagnostic(t *testing.T) {
	const message = "The 'gpt-5.4' model is not supported when using Codex with a ChatGPT account."
	payload, err := json.Marshal(map[string]string{"detail": message})
	if err != nil {
		t.Fatal(err)
	}
	for name, stream := range map[string]string{"json": "false", "stream": "true"} {
		t.Run(name, func(t *testing.T) {
			p := proxyFixture(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Request-Id", "req_unsupported_model")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write(payload)
			})
			w := httptest.NewRecorder()
			p.ServeHTTP(w, proxyRequest(http.MethodPost, "/responses", `{"model":"gpt-5.4","stream":`+stream+`}`))
			if w.Code != http.StatusBadRequest || w.Body.String() != string(payload) || w.Header().Get("X-Request-Id") != "req_unsupported_model" {
				t.Fatalf("upstream error response changed: status=%d headers=%v body=%q", w.Code, w.Header(), w.Body.String())
			}
			rec, ok := p.Metrics.LastFor("test")
			if !ok || rec.HTTPCode != http.StatusBadRequest || rec.Status != "error" || rec.Error != message {
				t.Fatalf("detail diagnostic missing from metrics: %+v", rec)
			}
		})
	}
}

func TestSourceErrorDetailIgnoresNonStrings(t *testing.T) {
	for _, detail := range []string{`null`, `false`, `123`, `[]`, `{"message":"nested detail"}`, `[{"msg":"validation"}]`} {
		t.Run(detail, func(t *testing.T) {
			if got := errorFromPayload([]byte(`{"detail":` + detail + `}`)); got != "" {
				t.Fatalf("non-string detail became a diagnostic: %q", got)
			}
			if got := errorFromPayload([]byte(`{"message":"canonical message","detail":` + detail + `}`)); got != "canonical message" {
				t.Fatalf("non-string detail obscured canonical message: %q", got)
			}
		})
	}
	for _, payload := range []string{
		`{"error":{"message":"canonical message"},"detail":"fallback detail"}`,
		`{"message":"canonical message","detail":"fallback detail"}`,
		`{"code":"canonical message","detail":"fallback detail"}`,
	} {
		if got := errorFromPayload([]byte(payload)); got != "canonical message" {
			t.Fatalf("detail changed existing diagnostic precedence: %q", got)
		}
	}
}

func TestSourceHTTPErrorDetailUsesExistingMetricRedaction(t *testing.T) {
	message := "Bearer should-not-appear " + strings.Repeat("x", 2500)
	payload, err := json.Marshal(map[string]string{"detail": message})
	if err != nil {
		t.Fatal(err)
	}
	p := proxyFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, string(payload))
	})
	w := httptest.NewRecorder()
	p.ServeHTTP(w, proxyRequest(http.MethodPost, "/responses", `{"model":"test","stream":true}`))
	rec, ok := p.Metrics.LastFor("test")
	if !ok || len(rec.Error) > 2000 || strings.Contains(rec.Error, "should-not-appear") || !strings.Contains(rec.Error, "[REDACTED]") {
		t.Fatalf("detail bypassed metric redaction/length bound: length=%d diagnostic=%q", len(rec.Error), rec.Error)
	}
	if w.Body.String() != string(payload) {
		t.Fatal("metric redaction changed the upstream response body")
	}
}
