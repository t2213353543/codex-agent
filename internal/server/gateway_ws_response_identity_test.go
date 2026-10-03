package server

import (
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
)

func TestGatewayWSResponseIdentityCompletedIDCannotConsumeNextTurn(t *testing.T) {
	for _, lanes := range []struct{ name, first, second, stale string }{
		{"without_stream", "", "", ""},
		{"reused_stream", "same", "same", "same"},
		{"different_stream_old_lane", "first", "second", "first"},
		{"different_stream_current_lane", "first", "second", "second"},
	} {
		for _, terminal := range []string{"response.completed", "response.failed"} {
			t.Run(lanes.name+"/"+terminal, func(t *testing.T) {
				p := cooldownNewTestProxy(t, 2)
				account, err := p.Store.Get("test")
				if err != nil {
					t.Fatal(err)
				}
				turns := gatewayWSTurns{proxy: p, keyID: "admin", pending: make(map[string][]*gatewayWSTurn)}
				var firstReleased, secondReleased atomic.Int32
				gatewayWSIdentityStart(t, &turns, account, lanes.first, "model-first", "", &firstReleased)
				turns.observe(gatewayWSIdentityEvent(t, "response.created", lanes.first, "response-first", 0))
				turns.observe(gatewayWSIdentityEvent(t, "response.completed", lanes.first, "response-first", 11))
				if turns.count != 0 || firstReleased.Load() != 1 || len(p.Metrics.RecentForAccount("test", 10)) != 1 {
					t.Fatal("fixture did not complete exactly the first turn")
				}
				gatewayWSIdentityStart(t, &turns, account, lanes.second, "model-second", "response-first", &secondReleased)
				stale := gatewayWSIdentityEvent(t, terminal, lanes.stale, "response-first", 91)
				// Match the actual upstream pump order: cooldown observation occurs
				// before terminal accounting, without changing forwarded bytes.
				turns.observeCooldown(t.Context(), stale, account)
				turns.observe(stale)
				if turns.count != 1 || len(turns.pending[lanes.second]) != 1 || secondReleased.Load() != 0 || p.inFlight(account.ID) != 1 {
					t.Errorf("old response ID consumed the next turn: pending=%d second_release=%d active=%d", turns.count, secondReleased.Load(), p.inFlight(account.ID))
				}
				if records := p.Metrics.RecentForAccount("test", 10); len(records) != 1 {
					t.Errorf("old terminal created a second record before the next response: records=%d", len(records))
				}
				if err := p.checkGatewayCooldown(withGatewayCooldownModel(t.Context(), "model-second"), account); err != nil {
					t.Errorf("old terminal applied cooldown to the next model: %v", err)
				}
				turns.observe(gatewayWSIdentityEvent(t, "response.created", lanes.second, "response-second", 0))
				turns.observe(gatewayWSIdentityEvent(t, "response.completed", lanes.second, "response-second", 22))
				if turns.count != 0 || len(turns.pending) != 0 || firstReleased.Load() != 1 || secondReleased.Load() != 1 || p.inFlight(account.ID) != 0 {
					t.Error("valid second response did not release only its own turn")
				}
				gatewayWSIdentityAssertRecords(t, p)
			})
		}
	}
}

func TestGatewayWSResponseIdentityActiveMismatchKeepsCurrentTurn(t *testing.T) {
	p := cooldownNewTestProxy(t, 1)
	account, err := p.Store.Get("test")
	if err != nil {
		t.Fatal(err)
	}
	turns := gatewayWSTurns{proxy: p, keyID: "admin", pending: make(map[string][]*gatewayWSTurn)}
	var released atomic.Int32
	gatewayWSIdentityStart(t, &turns, account, "same", "model-second", "", &released)
	turns.observe(gatewayWSIdentityEvent(t, "response.created", "same", "response-second", 0))
	turns.observe(gatewayWSIdentityEvent(t, "response.completed", "same", "unrelated-active-response", 91))
	if turns.count != 1 || released.Load() != 0 || len(p.Metrics.RecentForAccount("test", 10)) != 0 {
		t.Fatal("a mismatched active response ID consumed the current turn")
	}
	turns.observe(gatewayWSIdentityEvent(t, "response.completed", "same", "response-second", 22))
	if turns.count != 0 || released.Load() != 1 || p.inFlight(account.ID) != 0 {
		t.Fatal("the matching active response did not finish its turn")
	}
	if record, ok := p.Metrics.LastFor("test"); !ok || record.ResponseID != "response-second" || record.InputTokens != 22 || record.Status != "ok" {
		t.Fatal("mismatched response polluted the valid response metadata or usage")
	}
}

func TestGatewayWSResponseIdentityCompletedIDCannotPollutePendingMetadata(t *testing.T) {
	for _, kind := range []string{"response.created", "response.output_text.delta", "error"} {
		t.Run(kind, func(t *testing.T) {
			p := cooldownNewTestProxy(t, 2)
			account, err := p.Store.Get("test")
			if err != nil {
				t.Fatal(err)
			}
			turns := gatewayWSTurns{proxy: p, keyID: "admin", pending: make(map[string][]*gatewayWSTurn)}
			var firstReleased, secondReleased atomic.Int32
			gatewayWSIdentityStart(t, &turns, account, "same", "model-first", "", &firstReleased)
			turns.observe(gatewayWSIdentityEvent(t, "response.completed", "same", "response-first", 11))
			gatewayWSIdentityStart(t, &turns, account, "same", "model-second", "response-first", &secondReleased)
			event := map[string]any{"type": kind, "stream_id": "same", "response_id": "response-first"}
			switch kind {
			case "response.created":
				event["response"] = map[string]any{"id": "response-first", "status": "in_progress", "model": "stale-model"}
			case "response.output_text.delta":
				event["delta"] = "synthetic stale output"
			case "error":
				event["status"] = 429
				event["error"] = map[string]any{"code": "rate_limit_exceeded", "message": "synthetic stale error"}
			}
			raw, err := json.Marshal(event)
			if err != nil {
				t.Fatal(err)
			}
			turns.observeCooldown(t.Context(), raw, account)
			turns.observe(raw)
			if turns.count != 1 || len(turns.pending["same"]) != 1 || secondReleased.Load() != 0 {
				t.Fatal("stale event consumed the next pending turn")
			}
			pending := turns.pending["same"][0]
			if pending.responseID != "" || !pending.responseStarted.IsZero() || pending.rec.ResponseID != "" || pending.rec.TTFTMs != nil || pending.rec.ErrorSource != "" || pending.rec.ErrorPhase != "" || pending.rec.UpstreamModel != "" {
				t.Fatal("stale event contaminated the next turn's identity, timing or error metadata")
			}
			if len(p.Metrics.RecentForAccount("test", 10)) != 1 || p.checkGatewayCooldown(withGatewayCooldownModel(t.Context(), "model-second"), account) != nil {
				t.Fatal("stale event added a record or cooldown before the next response")
			}
			turns.observe(gatewayWSIdentityEvent(t, "response.created", "same", "response-second", 0))
			turns.observe(gatewayWSIdentityEvent(t, "response.completed", "same", "response-second", 22))
			gatewayWSIdentityAssertRecords(t, p)
			if firstReleased.Load() != 1 || secondReleased.Load() != 1 || p.inFlight(account.ID) != 0 {
				t.Fatal("valid continuation after a stale event leaked or duplicated admission")
			}
		})
	}
}

func TestGatewayWSResponseIdentityFirstFailureAndIDlessCompatibility(t *testing.T) {
	t.Run("first_failure_429", func(t *testing.T) {
		p := cooldownNewTestProxy(t, 1)
		account, err := p.Store.Get("test")
		if err != nil {
			t.Fatal(err)
		}
		turns := gatewayWSTurns{proxy: p, keyID: "admin", pending: make(map[string][]*gatewayWSTurn)}
		var released atomic.Int32
		gatewayWSIdentityStart(t, &turns, account, "same", "model-first", "", &released)
		turns.observe(gatewayWSIdentityEvent(t, "response.created", "same", "first-failed-response", 0))
		raw := gatewayWSIdentityEvent(t, "response.failed", "same", "first-failed-response", 33)
		turns.observeCooldown(t.Context(), raw, account)
		turns.observe(raw)
		var cooldown *gatewayCooldownError
		if !errors.As(p.checkGatewayCooldown(withGatewayCooldownModel(t.Context(), "model-first"), account), &cooldown) || p.checkGatewayCooldown(withGatewayCooldownModel(t.Context(), "unrelated-model"), account) != nil {
			t.Fatal("first genuine rate limit lost its exact model cooldown scope")
		}
		turns.observeCooldown(t.Context(), raw, account)
		turns.observe(raw)
		records := p.Metrics.RecentForAccount("test", 10)
		if len(records) != 1 || records[0].ResponseID != "first-failed-response" || records[0].Status != "error" || records[0].InputTokens != 33 || released.Load() != 1 || turns.count != 0 || p.inFlight(account.ID) != 0 {
			t.Fatal("first genuine failure was lost or its duplicate repeated accounting/release")
		}
		if p.checkGatewayCooldown(withGatewayCooldownModel(t.Context(), "unrelated-model"), account) != nil {
			t.Fatal("duplicate failure broadened its cooldown to the connection")
		}
	})
	t.Run("idless_terminal", func(t *testing.T) {
		p := cooldownNewTestProxy(t, 1)
		account, err := p.Store.Get("test")
		if err != nil {
			t.Fatal(err)
		}
		turns := gatewayWSTurns{proxy: p, keyID: "admin", pending: make(map[string][]*gatewayWSTurn)}
		var released atomic.Int32
		for _, tokens := range []int{11, 22} {
			gatewayWSIdentityStart(t, &turns, account, "", "model-first", "", &released)
			turns.observe(gatewayWSIdentityEvent(t, "response.completed", "", "", tokens))
		}
		records := p.Metrics.RecentForAccount("test", 10)
		if len(records) != 2 || released.Load() != 2 || turns.count != 0 || p.inFlight(account.ID) != 0 {
			t.Fatal("ID-less terminal compatibility was incorrectly deduplicated")
		}
		var total int64
		for _, record := range records {
			if record.ResponseID != "" || record.Status != "ok" {
				t.Fatal("ID-less terminal gained an invented response identity or failed status")
			}
			total += record.InputTokens
		}
		if total != 33 {
			t.Fatal("ID-less terminal compatibility lost reported usage")
		}
	})
}

func gatewayWSIdentityStart(t *testing.T, turns *gatewayWSTurns, account Account, stream, model, previous string, released *atomic.Int32) {
	t.Helper()
	release, acquired := turns.proxy.acquireGatewayAccount(account)
	if !acquired {
		t.Fatal("fixture could not acquire its account slot")
	}
	t.Cleanup(release)
	request := map[string]any{"type": "response.create", "model": model, "input": []any{}}
	if stream != "" {
		request["stream_id"] = stream
	}
	if previous != "" {
		request["previous_response_id"] = previous
	}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := turns.start(raw, account, nil, func() { released.Add(1); release() }); err != nil {
		t.Fatal(err)
	}
}

func gatewayWSIdentityEvent(t *testing.T, kind, stream, responseID string, tokens int) []byte {
	t.Helper()
	response := map[string]any{"id": responseID, "status": "in_progress"}
	event := map[string]any{"type": kind, "response": response}
	if stream != "" {
		event["stream_id"] = stream
	}
	if kind == "response.completed" || kind == "response.failed" {
		response["status"] = "completed"
		response["usage"] = map[string]any{"input_tokens": tokens, "output_tokens": 1, "input_tokens_details": map[string]any{"cached_tokens": 0}}
	}
	if kind == "response.failed" {
		response["status"] = "failed"
		response["error"] = map[string]any{"code": "rate_limit_exceeded", "message": "synthetic late failure"}
		event["headers"] = map[string]string{"Retry-After": "30"}
	}
	raw, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func gatewayWSIdentityAssertRecords(t *testing.T, p *Proxy) {
	t.Helper()
	records := p.Metrics.RecentForAccount("test", 10)
	if len(records) != 2 {
		t.Errorf("two valid responses produced %d records", len(records))
		return
	}
	seen := make(map[string]bool)
	for _, record := range records {
		wantTokens := int64(11)
		wantID := "response-first"
		if record.Model == "model-second" {
			wantTokens, wantID = 22, "response-second"
		}
		if seen[record.Model] || record.ResponseID != wantID || record.InputTokens != wantTokens || record.Status != "ok" {
			t.Errorf("late terminal polluted final records: model=%s response=%s input=%d status=%s", record.Model, record.ResponseID, record.InputTokens, record.Status)
		}
		seen[record.Model] = true
	}
	if !seen["model-first"] || !seen["model-second"] {
		t.Error("valid response records lost a model identity")
	}
}
