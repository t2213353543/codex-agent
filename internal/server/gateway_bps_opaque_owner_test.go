package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func opaqueReviewOutput(kind, cipher string) []json.RawMessage {
	raw, _ := json.Marshal(map[string]any{"type": kind, "encrypted_content": cipher, "summary": []any{}})
	return []json.RawMessage{raw}
}

func opaqueReviewBody(output []json.RawMessage) []byte {
	raw, _ := json.Marshal(map[string]any{"model": "gpt-6-astra", "input": output})
	return raw
}

func TestBPSOpaqueOwnerRegistryAtomicConflictAndIsolation(t *testing.T) {
	p := bpsGatewayFixture(t, func(http.ResponseWriter, *http.Request) { t.Error("registry unit fixture must not dispatch") })
	ctx := withGatewayUnboundedPayload(t.Context(), true)
	a := bpsHistoryRecord{Owner: "owner-A", Session: "session-A", Request: json.RawMessage(`{"input":[]}`), Output: opaqueReviewOutput("reasoning", "decoded-value")}
	if err := p.bpsSaveHistoryWithOpaqueOwners(ctx, "key-A", "resp-A", a, a.Output); err != nil {
		t.Fatal(err)
	}
	before, err := p.bpsLoadHistory(ctx, "key-A", "resp-A")
	if err != nil {
		t.Fatal(err)
	}
	// Identical decoded string with different JSON spelling resolves once.
	escaped := []byte(`{"input":[{"type":"reasoning","encrypted_content":"decoded-\u0076alue"}]}`)
	if got, err := p.bpsResolveOpaqueOwner(ctx, "key-A", escaped, ""); err != nil || got != "owner-A" {
		t.Fatal("decoded JSON string proof not found")
	}
	for _, tc := range []struct{ name, key, mode string }{{"other_key", "key-B", ""}, {"other_mode", "key-A", inferenceCodexCLI}} {
		t.Run(tc.name, func(t *testing.T) {
			c := ctx
			if tc.mode != "" {
				c = withInferenceMode(c, tc.mode)
			}
			if got, err := p.bpsResolveOpaqueOwner(c, tc.key, escaped, ""); err != nil || got != "" {
				t.Fatal("ownership proof leaked across key/mode")
			}
		})
	}
	b := bpsHistoryRecord{Owner: "owner-B", Session: "session-B", Request: json.RawMessage(`{"input":[]}`), Output: opaqueReviewOutput("reasoning", "fresh-new-value")}
	b.Output = append(b.Output, a.Output...)
	if err := p.bpsSaveHistoryWithOpaqueOwners(ctx, "key-A", "resp-conflict", b, b.Output); !errors.Is(err, errBPSOpaqueOwnerConflict) {
		t.Fatal("conflicting final batch accepted")
	}
	if _, err := p.bpsLoadHistory(ctx, "key-A", "resp-conflict"); !errors.Is(err, errBPSHistoryMissing) {
		t.Fatal("conflict published response history")
	}
	if got, err := p.bpsResolveOpaqueOwner(ctx, "key-A", opaqueReviewBody(b.Output[:1]), ""); err != nil || got != "" {
		t.Fatal("first fresh batch proof partially written")
	}
	after, err := p.bpsLoadHistory(ctx, "key-A", "resp-A")
	if err != nil || after.Owner != before.Owner || !bytes.Equal(after.Output[0], before.Output[0]) {
		t.Fatal("prior successful history changed")
	}
	if _, err := p.bpsResolveOpaqueOwner(ctx, "key-A", escaped, "owner-B"); !errors.Is(err, errBPSOpaqueOwnerConflict) {
		t.Fatal("previous_response owner conflict accepted")
	}
	b.Output = b.Output[:1]
	if err := p.bpsSaveHistoryWithOpaqueOwners(ctx, "key-A", "resp-B", b, b.Output); err != nil {
		t.Fatal(err)
	}
	mixed := append(append([]json.RawMessage(nil), a.Output...), b.Output...)
	if _, err := p.bpsResolveOpaqueOwner(ctx, "key-A", opaqueReviewBody(mixed), ""); !errors.Is(err, errBPSOpaqueOwnerConflict) {
		t.Fatal("input with known owners A and B accepted")
	}
}

func TestBPSOpaqueOwnerRegistryBoundedAndInputDoNotRegister(t *testing.T) {
	p := bpsGatewayFixture(t, func(http.ResponseWriter, *http.Request) { t.Error("registry unit fixture must not dispatch") })
	ctx := withGatewayUnboundedPayload(t.Context(), true)
	output := opaqueReviewOutput("reasoning", "bounded-state")
	record := bpsHistoryRecord{Owner: "bounded-owner", Session: "bounded-session", Request: json.RawMessage(`{"input":[]}`), Output: output}
	if err := p.bpsSaveHistoryWithOpaqueOwners(t.Context(), "key", "bounded-response", record, output); err != nil {
		t.Fatal(err)
	}
	if got, err := p.bpsResolveOpaqueOwner(ctx, "key", opaqueReviewBody(output), ""); err != nil || got != "" {
		t.Fatal("bounded mode acquired a new permanent registry")
	}
	if history, err := p.bpsLoadHistory(t.Context(), "key", "bounded-response"); err != nil || history.Expires == 0 {
		t.Fatal("bounded history lifetime changed")
	}
	for _, kind := range []string{"message", "function_call_output", "compaction_summary"} {
		items := opaqueReviewOutput(kind, "input-is-not-proof")
		record.Output = items
		if err := p.bpsSaveHistoryWithOpaqueOwners(ctx, "key", "not-indexed-"+kind, record, items); err != nil {
			t.Fatal(err)
		}
		if got, err := p.bpsResolveOpaqueOwner(ctx, "key", opaqueReviewBody(opaqueReviewOutput("reasoning", "input-is-not-proof")), ""); err != nil || got != "" {
			t.Fatal("noneligible item became an ownership proof")
		}
	}
	if got, err := p.bpsResolveOpaqueOwner(ctx, "key", opaqueReviewBody(opaqueReviewOutput("reasoning", "unknown-imported-state")), ""); err != nil || got != "" {
		t.Fatal("unknown client input invented an owner")
	}
}

func TestBPSOpaqueOwnerRegistersOnlySuccessfulTerminalAndNativeCompact(t *testing.T) {
	for _, mode := range []string{"failed", "native_compact", "explicit_conflict"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			const cipher = "actual-fixture-terminal-state"
			p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				_, _ = io.Copy(io.Discard, r.Body)
				if mode == "native_compact" {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"id":"compact-indexed","model":"gpt-6-astra","status":"completed","output":[{"type":"compaction","encrypted_content":"`+cipher+`"}]}`)
				} else if mode == "failed" {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: "+`{"type":"response.failed","response":{"id":"failed-not-indexed","status":"failed","output":[{"type":"reasoning","encrypted_content":"`+cipher+`","summary":[]}],"error":{"code":"server_error"}}}`+"\n\n")
				} else {
					w.Header().Set("Content-Type", "text/event-stream")
					output, _ := json.Marshal(opaqueReviewOutput("reasoning", cipher))
					_, _ = io.WriteString(w, bpsGatewayFrame("response.completed", "successful-indexed", string(output)))
				}
			})
			path := "/v1/responses"
			if mode == "native_compact" {
				path += "/compact"
			}
			r := proxyRequest("POST", path, `{"model":"gpt-6-astra","stream":false,"input":[{"role":"user","content":"synthetic"}]}`)
			r.Header.Set("Session-Id", "first-session")
			out := httptest.NewRecorder()
			p.gatewayBPS(out, r)
			ctx := withGatewayUnboundedPayload(t.Context(), true)
			kind := "reasoning"
			if mode == "native_compact" {
				kind = "compaction"
			}
			owner, err := p.bpsResolveOpaqueOwner(ctx, "admin", opaqueReviewBody(opaqueReviewOutput(kind, cipher)), "")
			account, _ := p.Store.Get("test")
			if mode == "failed" {
				if out.Code != 502 || err != nil || owner != "" {
					t.Fatal("failed terminal indexed incomplete output")
				}
				return
			}
			if out.Code != 200 || err != nil || owner != gatewayAccountRef(account) {
				t.Fatal("valid successful terminal did not publish exact owner")
			}
			if mode == "native_compact" {
				history, err := p.bpsLoadHistory(ctx, "admin", "compact-indexed")
				if err != nil || len(history.Output) != 0 || !bytes.Contains(history.Request, []byte(cipher)) {
					t.Fatal("compact storage shape changed or independent actualOutput was ignored")
				}
				return
			}
			if _, err := p.sessionAccount(ctx, "admin", "explicit-other-owner", "different-existing-owner"); err != nil {
				t.Fatal(err)
			}
			body := opaqueReviewBody(opaqueReviewOutput("reasoning", cipher))
			r = proxyRequest("POST", "/v1/responses", string(body))
			r.Header.Set("Session-Id", "explicit-other-owner")
			out = httptest.NewRecorder()
			p.gatewayBPS(out, r)
			if out.Code != 409 || calls != 1 {
				t.Fatal("known proof overwrote a conflicting explicit binding or dispatched")
			}
			bound, err := p.sessionAccount(ctx, "admin", "explicit-other-owner", "")
			if err != nil || bound != "different-existing-owner" {
				t.Fatal("conflicting owner was migrated")
			}
		})
	}
}
