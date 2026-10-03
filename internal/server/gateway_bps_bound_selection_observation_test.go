package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestBPSBoundSelectionSnapshotOriginAndSameSelection(t *testing.T) {
	p := newTestProxy(t, 2)
	var fresh gatewayAccountSnapshotObservation
	accounts, err := p.accountsSnapshotObserved(&fresh)
	if err != nil || len(accounts) != 1 || fresh.origin != "fresh_read" || !fresh.known || fresh.ageMS != 0 {
		t.Fatalf("fresh snapshot: %+v %v", fresh, err)
	}
	loaded := time.Now().Add(-2 * time.Second)
	p.accountsMu.Lock()
	p.accountsAt = loaded
	p.accountsMu.Unlock()
	// The cache hit must not require a store read just for diagnostics.
	badStore := filepath.Join(t.TempDir(), "unreadable-store")
	if err := os.Mkdir(badStore, 0700); err != nil {
		t.Fatal(err)
	}
	p.Store.Path = badStore
	var cached gatewayAccountSnapshotObservation
	copyAccounts, err := p.accountsSnapshotObserved(&cached)
	if err != nil || !reflect.DeepEqual(accounts, copyAccounts) || cached.origin != "cache_hit" || !cached.known || cached.ageMS < 1900 || cached.ageMS > 3000 {
		t.Fatalf("cache snapshot: %+v %v", cached, err)
	}
	staleAt := time.Now().Add(-12 * time.Second)
	p.accountsMu.Lock()
	p.accountsAt = staleAt
	p.accountsMu.Unlock()
	var stale gatewayAccountSnapshotObservation
	copyAccounts, err = p.accountsSnapshotObserved(&stale)
	if err != nil || !reflect.DeepEqual(accounts, copyAccounts) || stale.origin != "stale_on_read_error" || stale.ageMS < 11900 || !p.accountsAt.Equal(staleAt) {
		t.Fatalf("stale snapshot: %+v %v", stale, err)
	}
	// A previously loaded empty catalog has the same cache/stale semantics.
	p.accountsMu.Lock()
	p.accounts = nil
	p.accountsAt = time.Now().Add(-time.Second)
	p.accountsMu.Unlock()
	var empty gatewayAccountSnapshotObservation
	if got, err := p.accountsSnapshotObserved(&empty); err != nil || len(got) != 0 || empty.origin != "cache_hit" || !empty.known {
		t.Fatalf("empty cache: %+v %v", empty, err)
	}
	p.accountsMu.Lock()
	p.accountsAt = time.Now().Add(-12 * time.Second)
	p.accountsMu.Unlock()
	if got, err := p.accountsSnapshotObserved(&empty); err != nil || len(got) != 0 || empty.origin != "stale_on_read_error" || !empty.known {
		t.Fatalf("empty stale: %+v %v", empty, err)
	}
	// Facts are a scalar snapshot and cannot change if the cache later reloads.
	observation := (&gatewayBoundCandidateSnapshot{accounts: accounts, snapshot: stale, admin: true}).missing("nonmatching-owner", 0)
	oldAge := *observation.SnapshotAgeMS
	p.accountsMu.Lock()
	p.accountsAt = time.Now()
	p.accountsMu.Unlock()
	if observation.SnapshotOrigin != "stale_on_read_error" || *observation.SnapshotAgeMS != oldAge || observation.EligibleCandidateCount != 0 {
		t.Fatal("captured facts followed a later cache update")
	}
}

func TestBPSBoundSelectionBPSValueMatrix(t *testing.T) {
	for _, tc := range []struct {
		name, change, payload, wantState, wantReplay, wantRecovery string
		replacement                                                bool
		status                                                     int
	}{
		{"disabled_recovered", "disabled", "plain", "exact_disabled", "full_plaintext", "recovered", true, 200},
		{"group_recovered", "group", "plain", "exact_group_denied", "full_plaintext", "recovered", true, 200},
		{"deleted_exhausted", "deleted", "plain", "missing_or_replaced", "full_plaintext", "candidates_exhausted", false, 503},
		{"disabled_exhausted", "disabled", "plain", "exact_disabled", "full_plaintext", "candidates_exhausted", false, 503},
		{"disabled_opaque", "disabled", "opaque", "exact_disabled", "opaque_or_file", "blocked_input", true, 503},
		{"disabled_previous", "disabled", "previous", "exact_disabled", "previous_response", "blocked_input", true, 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, bpsGatewayFrame("response.completed", "observed-replacement", "[]"))
			})
			if err := p.Store.UpdateAccount("test", func(a *Account) error { a.Groups = []string{"allowed-private-group"}; return nil }); err != nil {
				t.Fatal(err)
			}
			key, err := p.Keys.Create("private-bound-key", []string{"allowed-private-group"})
			if err != nil {
				t.Fatal(err)
			}
			original, err := p.Store.Get("test")
			if err != nil {
				t.Fatal(err)
			}
			const session = "private-bound-session"
			owner := gatewayAccountRef(original)
			if _, err := p.sessionAccount(t.Context(), key.ID, session, owner); err != nil {
				t.Fatal(err)
			}
			if tc.replacement {
				payload, _ := json.Marshal(map[string]any{"access_token": "private-synthetic-replacement", "expires_at": time.Now().Add(time.Hour).Unix()})
				if err := p.Store.ImportWith("replacement", payload, AccountDefaults{Groups: []string{"allowed-private-group"}, Concurrency: 1}); err != nil {
					t.Fatal(err)
				}
			}
			plain := json.RawMessage(`[{"role":"user","content":"private-body-do-not-record"}]`)
			if tc.payload == "previous" {
				request, _ := json.Marshal(map[string]any{"model": "gpt-6-astra", "input": plain})
				if err := p.bpsSaveHistory(withGatewayUnboundedPayload(t.Context(), true), key.ID, "private-prior-id", bpsHistoryRecord{Owner: owner, Session: session, Request: request}); err != nil {
					t.Fatal(err)
				}
			}
			switch tc.change {
			case "disabled":
				err = p.Store.UpdateAccount("test", func(a *Account) error { a.Enabled = false; return nil })
			case "group":
				err = p.Store.UpdateAccount("test", func(a *Account) error { a.Groups = []string{"denied-private-group"}; return nil })
			case "deleted":
				err = p.Store.Delete("test")
			}
			if err != nil {
				t.Fatal(err)
			}
			p.invalidateAccounts()
			body := map[string]any{"model": "gpt-6-astra", "stream": false, "input": plain}
			if tc.payload == "opaque" {
				body["input"] = json.RawMessage(`[{"type":"reasoning","encrypted_content":"private-cipher"}]`)
			}
			if tc.payload == "previous" {
				body["previous_response_id"] = "private-prior-id"
			}
			encoded, _ := json.Marshal(body)
			r := proxyRequest(http.MethodPost, "/v1/responses", string(encoded))
			r.Header.Set("Authorization", "Bearer "+key.Key)
			r.Header.Set("Session-Id", session)
			w := httptest.NewRecorder()
			p.gatewayBPS(w, r)
			if w.Code != tc.status {
				t.Fatalf("http=%d want=%d", w.Code, tc.status)
			}
			p.Metrics.mu.Lock()
			count := p.Metrics.recentAt
			record := p.Metrics.recent[0]
			p.Metrics.mu.Unlock()
			if count != 1 {
				t.Fatalf("records=%d want1", count)
			}
			obs := record.BoundSelection
			wantCount := 0
			if tc.replacement {
				wantCount = 1
			}
			if obs == nil || obs.BoundSnapshotState != tc.wantState || obs.EligibleCandidateCount != wantCount || obs.SnapshotOrigin != "fresh_read" || obs.SnapshotAgeMS == nil || *obs.SnapshotAgeMS != 0 || obs.ReplayClass != tc.wantReplay || obs.RecoveryOutcome != tc.wantRecovery {
				t.Fatalf("observation=%+v", obs)
			}
			if tc.status == 200 {
				if calls != 1 || record.Status != "ok" || record.Error != "" || record.Account != "replacement" || record.HTTPCode != 200 {
					t.Fatal("recovered record inherited prior failure")
				}
			} else {
				if calls != 0 || record.Status != "error" || record.Account != "" {
					t.Fatal("local failed selection changed execution")
				}
			}
			stored, err := p.Metrics.managedLogDetail(t.Context(), record.EventID)
			if err != nil || !reflect.DeepEqual(stored.BoundSelection, obs) {
				t.Fatal("persisted finite observation lost", err)
			}
			observationJSON, _ := json.Marshal(obs)
			for _, secret := range []string{key.ID, key.Key, owner, session, "private-", "test-account"} {
				if bytes.Contains(observationJSON, []byte(secret)) {
					t.Fatalf("private identity entered observation: %s", observationJSON)
				}
			}
			var fields map[string]any
			_ = json.Unmarshal(observationJSON, &fields)
			if len(fields) > 6 {
				t.Fatal("observation exceeds six fields")
			}
			t.Logf("%s http=%d calls=%d observation=%s", tc.name, w.Code, calls, observationJSON)
		})
	}
}

func TestBPSBoundSelectionReplayClassificationSharesOriginalDecision(t *testing.T) {
	for _, tc := range []struct {
		raw, classification string
		allowed             bool
	}{
		{`{"input":[]}`, "full_plaintext", true},
		{`{"input":"text"}`, "full_plaintext", true},
		{`{"input":[{"encrypted_content":"opaque"}]}`, "opaque_or_file", false},
		{`{"input":[{"file_id":"private-file"}]}`, "opaque_or_file", false},
		{`{"input":[],"previous_response_id":"private-response"}`, "previous_response", false},
		{`{"input":[],"previous_response_id":17}`, "unobserved", false},
		{`{"input":null}`, "unobserved", true},
		{`{"input":{}}`, "unobserved", true},
		{`{}`, "unobserved", false},
		{`{`, "unobserved", false},
	} {
		t.Run(tc.classification+fmt.Sprint(len(tc.raw)), func(t *testing.T) {
			class := ""
			ctx := withBPSModeMigrationObserved(t.Context(), []byte(tc.raw), &class)
			if class != tc.classification || bpsAllowsModeMigration(ctx, inferencePowerPoint) != tc.allowed {
				t.Fatalf("class=%s allow=%v", class, bpsAllowsModeMigration(ctx, inferencePowerPoint))
			}
			plain := withBPSModeMigration(t.Context(), []byte(tc.raw))
			if bpsAllowsModeMigration(plain, inferencePowerPoint) != tc.allowed {
				t.Fatal("existing wrapper decision changed")
			}
		})
	}
}

func TestBPSBoundSelectionImmutableFirstTailAndFinalDisposition(t *testing.T) {
	zero := int64(0)
	first := &GatewayBoundSelectionObservation{BoundSnapshotState: "exact_disabled", SnapshotOrigin: "cache_hit", SnapshotAgeMS: &zero}
	state := &officialFailoverState{boundReplayClass: "previous_response"}
	state.rememberBoundSelection(first)
	first.BoundSnapshotState = "mutated"
	*first.SnapshotAgeMS = 999
	state.rememberBoundSelection(&GatewayBoundSelectionObservation{BoundSnapshotState: "second-tail"})
	for _, tc := range []struct {
		name, status, want                 string
		retry, commit, exhausted, canceled bool
		failure                            error
	}{
		{"blocked", "error", "blocked_input", false, false, false, false, nil},
		{"exhausted", "error", "candidates_exhausted", true, false, true, false, nil},
		{"selected_not_committed", "ok", "replacement_failed", true, false, false, false, nil},
		{"committed_success", "ok", "recovered", true, true, false, false, nil},
		{"cas_failed", "error", "replacement_failed", true, false, false, false, errors.New("fixture conflict")},
		{"deliver_failed", "aborted", "replacement_failed", true, true, false, false, nil},
		{"canceled_after_commit", "aborted", "blocked_context", true, true, false, true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state.retrying, state.committed, state.failure, state.replayBlocked = tc.retry, tc.commit, tc.failure, true
			record := RequestRecord{Status: tc.status, Account: "current", HTTPCode: 200}
			ctx := t.Context()
			if tc.canceled {
				c, cancel := context.WithCancel(ctx)
				cancel()
				ctx = c
			}
			state.finishBoundSelection(ctx, &record, tc.exhausted)
			if record.BoundSelection == nil || record.BoundSelection.BoundSnapshotState != "exact_disabled" || *record.BoundSelection.SnapshotAgeMS != 0 || record.BoundSelection.ReplayClass != "previous_response" || record.BoundSelection.RecoveryOutcome != tc.want || record.Account != "current" || record.Status != tc.status || record.HTTPCode != 200 {
				t.Fatalf("first/final boundary changed: %+v", record.BoundSelection)
			}
			record.BoundSelection.BoundSnapshotState = "changed-record"
			*record.BoundSelection.SnapshotAgeMS = 500
		})
	}
	var absent RequestRecord
	(&officialFailoverState{}).finishBoundSelection(t.Context(), &absent, false)
	encoded, _ := json.Marshal(absent)
	if bytes.Contains(encoded, []byte("bound_selection")) {
		t.Fatal("unobserved bound metadata fabricated")
	}
}

func TestBPSBoundSelectionOrdinarySuccessRemainsUnobserved(t *testing.T) {
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, bpsGatewayFrame("response.completed", "ordinary-no-tail", "[]"))
	})
	r := proxyRequest(http.MethodPost, "/v1/responses", `{"model":"gpt-6-astra","input":"synthetic","stream":false}`)
	r.Header.Set("Session-Id", "ordinary-session")
	w := httptest.NewRecorder()
	p.gatewayBPS(w, r)
	record, ok := p.Metrics.LastFor("test")
	if w.Code != 200 || !ok || record.BoundSelection != nil || record.Status != "ok" {
		t.Fatal("ordinary success acquired bound failure observation")
	}
	encoded, _ := json.Marshal(record)
	if bytes.Contains(encoded, []byte("bound_selection")) {
		t.Fatal("absent bound selection serialized as an observed value")
	}
}
