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
)

func TestBPSUnboundedPlaintextSessionMigrationPreservesAccount(t *testing.T) {
	var seen string
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, bpsGatewayFrame("response.completed", "migrated", `[]`))
	})
	p.RuntimeModes = true
	account, err := p.Store.Get("test")
	if err != nil {
		t.Fatal(err)
	}
	ctx := withInferenceMode(t.Context(), inferenceAppServer)
	if err = p.bindInferenceSession(ctx, "admin", "migrating-session"); err != nil {
		t.Fatal(err)
	}
	if _, err = p.sessionAccount(ctx, "admin", "migrating-session", gatewayAccountRef(account)); err != nil {
		t.Fatal(err)
	}
	r := proxyRequest(http.MethodPost, "/v1/responses", `{"model":"gpt-6-astra","stream":false,"input":[{"role":"user","content":"first"},{"role":"assistant","content":"prior plaintext"},{"role":"user","content":"continue"}]}`)
	r.Header.Set("Session-Id", "migrating-session")
	r = r.WithContext(withInferenceMode(r.Context(), inferencePowerPoint))
	w := httptest.NewRecorder()
	p.gatewayBPS(w, r)
	if w.Code != 200 || seen != "Bearer test-upstream-token" {
		t.Fatalf("safe existing conversation migration failed: %d %s", w.Code, w.Body.String())
	}
	if owner, _ := p.sessionAccount(ctx, "admin", "migrating-session", ""); owner != gatewayAccountRef(account) {
		t.Fatal("migration changed account owner")
	}
	for _, input := range []string{`{"input":[{"type":"reasoning","encrypted_content":"opaque"}]}`, `{"input":[{"type":"compaction","encrypted_content":"opaque"}]}`, `{"input":[{"type":"function_call","encrypted_function_args":["opaque"]}]}`, `{"previous_response_id":"old","input":[]}`, `{"input":[{"type":"message","role":"user","content":[{"type":"input_image","file_id":"owned-file"}]}]}`} {
		if bpsAllowsModeMigration(withBPSModeMigration(t.Context(), []byte(input)), inferencePowerPoint) {
			t.Fatalf("opaque/delta history authorized migration: %s", input)
		}
	}
}

func TestBPSUnboundedLargeRequestAndOutputRetainContinuation(t *testing.T) {
	value := strings.Repeat("x", 33<<20)
	want := sha256.Sum256([]byte(value))
	calls := 0
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body struct {
			Input []struct {
				Role    string
				Content json.RawMessage
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		var matched bool
		for _, item := range body.Input {
			var parts []struct{ Text string }
			if json.Unmarshal(item.Content, &parts) != nil {
				continue
			}
			for _, part := range parts {
				if sha256.Sum256([]byte(part.Text)) == want {
					matched = true
				}
			}
		}
		if !matched {
			t.Fatal("large history text truncated before upstream")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if calls == 1 {
			_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"large-output","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"`)
			_, _ = io.WriteString(w, value)
			_, _ = io.WriteString(w, `"}]}],"usage":{"input_tokens":10,"output_tokens":2}}}`+"\n\n")
		} else {
			_, _ = io.WriteString(w, bpsGatewayFrame("response.completed", "continued", `[]`))
		}
	})
	body := `{"model":"gpt-6-astra","stream":false,"input":[{"role":"user","content":[{"type":"input_text","text":"` + value + `"}]}]}`
	r := proxyRequest(http.MethodPost, "/v1/responses", body)
	w := httptest.NewRecorder()
	p.gatewayBPS(w, r)
	if w.Code != 200 {
		t.Fatalf("large request/response rejected: %d %s", w.Code, w.Body.String())
	}
	var result struct {
		Output []struct{ Content []struct{ Text string } }
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Output) != 1 || len(result.Output[0].Content) != 1 || sha256.Sum256([]byte(result.Output[0].Content[0].Text)) != want {
		t.Fatal("large response lost exact output")
	}
	next := proxyRequest(http.MethodPost, "/v1/responses", `{"model":"gpt-6-astra","stream":false,"previous_response_id":"large-output","input":[{"role":"user","content":"continue"}]}`)
	w = httptest.NewRecorder()
	p.gatewayBPS(w, next)
	if w.Code != 200 || calls != 2 || !bytes.Contains(w.Body.Bytes(), []byte("continued")) {
		t.Fatalf("large cached continuation failed: %d %s", w.Code, w.Body.String())
	}
}

func TestBPSUnboundedHistoryRetainsMoreThanLegacyEntryLimit(t *testing.T) {
	p := &Proxy{BPS: &bpsRuntime{}}
	ctx := withGatewayUnboundedPayload(withInferenceMode(context.Background(), inferencePowerPoint), true)
	for i := 0; i < 513; i++ {
		if err := p.bpsSaveHistory(ctx, "key", fmt.Sprintf("response-%d", i), bpsHistoryRecord{Owner: "owner", Session: "thread", Request: json.RawMessage(`{"input":[]}`)}); err != nil {
			t.Fatal(err)
		}
	}
	first, err := p.bpsLoadHistory(ctx, "key", "response-0")
	if err != nil || first.Expires <= time.Now().Unix() || first.Mode != inferencePowerPoint {
		t.Fatalf("earlier active-backend response lost: %+v %v", first, err)
	}
	if _, err = p.bpsLoadHistory(withGatewayUnboundedPayload(withInferenceMode(ctx, inferenceAppServer), true), "key", "response-0"); err == nil {
		t.Fatal("unbounded cache removed backend identity isolation")
	}
}
