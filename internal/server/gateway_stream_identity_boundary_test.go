package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Display metadata is deliberately bounded/redacted; response ownership is
// not. Two distinct raw IDs must not become the same accounting identity.
func TestStreamLifecycleExactResponseIdentityAccounting(t *testing.T) {
	for _, transport := range []string{"go", "native"} {
		for _, test := range []struct {
			name, first, last, top string
			want                   int64
		}{
			{"different_long_suffix", strings.Repeat("x", 1024) + "A", strings.Repeat("x", 1024) + "B", "", 0},
			{"different_whitespace", "response_a", " response_a ", "", 0},
			{"different_redacted", "Bearer PRIVATE_FIRST", "Bearer PRIVATE_SECOND", "", 0},
			{"identical_redacted_control", "Bearer PRIVATE_FIRST", "Bearer PRIVATE_FIRST", "", 13},
			{"identical_long_control", strings.Repeat("x", 1024) + "A", strings.Repeat("x", 1024) + "A", "", 13},
			{"top_nested_conflict", "response_a", "response_a", "response_b", 0},
			{"top_only_foreign_terminal", "response_a", "", "response_b", 0},
			{"top_only_matching_control", "response_a", "", "response_a", 13},
			{"top_nested_long_conflict", strings.Repeat("x", 1024) + "A", strings.Repeat("x", 1024) + "A", strings.Repeat("x", 1024) + "B", 0},
			{"identical_short_control", "response_a", "response_a", "", 13},
		} {
			for _, stream := range []bool{true, false} {
				t.Run(fmt.Sprintf("%s/%s/stream=%v", transport, test.name, stream), func(t *testing.T) {
					created, _ := json.Marshal(map[string]any{"type": "response.created", "response": map[string]any{"id": test.first, "status": "in_progress"}})
					completed, _ := json.Marshal(map[string]any{"type": "response.completed", "response_id": test.top, "response": map[string]any{"id": test.last, "status": "completed", "output": []any{}, "usage": map[string]int{"input_tokens": 13, "output_tokens": 7, "total_tokens": 20}}})
					wire := "data: " + string(created) + "\n\ndata: " + string(completed) + "\n\n"
					p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
						_, _ = io.Copy(io.Discard, r.Body)
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, wire)
					})
					if transport == "native" {
						realGatewayNativePool(t, p)
					}
					done := make(chan struct{})
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { defer close(done); p.ServeHTTP(w, r) }))
					defer server.Close()
					ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
					defer cancel()
					req, _ := http.NewRequestWithContext(ctx, "POST", server.URL+"/responses", strings.NewReader(fmt.Sprintf(`{"model":"test","stream":%v,"input":[]}`, stream)))
					req.Header.Set("Authorization", "Bearer test-client-key")
					res, err := server.Client().Do(req)
					if err != nil {
						t.Fatal(err)
					}
					raw, err := io.ReadAll(res.Body)
					_ = res.Body.Close()
					if err != nil || stream && string(raw) != wire {
						t.Fatal("response wire was altered")
					}
					if !stream && ((test.want == 0 && res.StatusCode != http.StatusBadGateway) || (test.want > 0 && res.StatusCode != http.StatusOK)) {
						t.Fatalf("JSON identity validation status=%d", res.StatusCode)
					}
					<-done
					record, ok := p.Metrics.LastFor("test")
					if !ok {
						t.Fatal("missing request record")
					}
					if record.InputTokens != test.want || (test.want == 0 && record.Status != "error") || (test.want != 0 && record.Status != "ok") {
						t.Fatalf("distinct raw response IDs conflated: input=%d status=%s want input=%d", record.InputTokens, record.Status, test.want)
					}
					if len(record.ResponseID) > 1024 {
						t.Fatal("display identity lost its bounded contract")
					}
				})
			}
		}
	}
}

func TestStreamLifecycleResponseIdentityDigestStaysPrivate(t *testing.T) {
	const first = "Bearer PRIVATE_FIRST"
	const foreign = "Bearer PRIVATE_SECOND"
	var record RequestRecord
	observer := gatewayObservation{record: &record, started: time.Now()}
	event, _ := json.Marshal(map[string]any{"type": "response.created", "response": map[string]string{"id": first}})
	observer.event(event)
	copy := record // The live record can move between observers by value.
	if gatewayResponseIDMatchesRecord(&copy, foreign) || !gatewayResponseIDMatchesRecord(&copy, first) {
		t.Fatal("copied record lost exact response ownership")
	}
	conflict, _ := json.Marshal(map[string]any{"type": "response.completed", "response_id": foreign, "response": map[string]string{"id": first, "status": "completed"}})
	(gatewayObservation{record: &copy, started: time.Now()}).event(conflict)
	if copy.TerminalStatus != "" {
		t.Fatal("conflicting envelope changed terminal status")
	}
	raw, err := json.Marshal(copy)
	if err != nil || bytes.Contains(raw, []byte("PRIVATE_FIRST")) || bytes.Contains(raw, []byte("PRIVATE_SECOND")) || bytes.Contains(raw, []byte("responseIdentity")) || !bytes.Contains(raw, []byte("[REDACTED]")) {
		t.Fatal("private identity state leaked into persisted/displayed record")
	}
}
