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
	"sync"
	"testing"
	"time"
)

func TestBPSSessionCASConcurrentSameReplacementIsIdempotent(t *testing.T) {
	p := newTestProxy(t, 2)
	if _, err := p.sessionAccount(t.Context(), "admin", "same-target", "old-owner"); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan bool, 2)
	var group sync.WaitGroup
	for i := 0; i < 2; i++ {
		group.Go(func() {
			<-start
			actual, accepted, err := p.switchSessionAccount(t.Context(), "admin", "same-target", "old-owner", "same-replacement")
			results <- err == nil && accepted && actual == "same-replacement"
		})
	}
	close(start)
	group.Wait()
	close(results)
	for accepted := range results {
		if !accepted {
			t.Fatal("second same-target CAS was not idempotently accepted")
		}
	}
}

type rootCASLane struct {
	entered  chan string
	release  chan struct{}
	canceled chan struct{}
	response *httptest.ResponseRecorder
	done     chan struct{}
}

func rootCASConcurrentGateway(t *testing.T, slow, sameOwner, stream bool) {
	t.Helper()
	lanes := map[string]*rootCASLane{}
	for _, name := range []string{"first", "second"} {
		lanes[name] = &rootCASLane{entered: make(chan string, 1), release: make(chan struct{}), canceled: make(chan struct{}), response: httptest.NewRecorder(), done: make(chan struct{})}
	}
	var networkMu sync.Mutex
	var network []string
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		lane := "first"
		if bytes.Contains(body, []byte("lane-second")) {
			lane = "second"
		}
		owner := map[string]string{"Bearer same-token": "same", "Bearer other-token": "other", "Bearer test-upstream-token": "old"}[r.Header.Get("Authorization")]
		networkMu.Lock()
		network = append(network, lane+":"+owner)
		networkMu.Unlock()
		lanes[lane].entered <- owner
		select {
		case <-lanes[lane].release:
		case <-r.Context().Done():
			close(lanes[lane].canceled)
			return
		}
		output := []any{map[string]any{"type": "reasoning", "id": "rs_" + lane, "summary": []any{}, "encrypted_content": "synthetic_cipher_" + owner + "_" + lane}, map[string]any{"type": "message", "id": "msg_" + lane, "role": "assistant", "status": "completed", "content": []any{map[string]string{"type": "output_text", "text": "RESULT_" + owner + "_" + lane}}}}
		terminal, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_cas_" + lane, "model": "gpt-6-astra", "status": "completed", "output": output, "usage": map[string]int{"input_tokens": 10, "output_tokens": 2}}})
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: %s\n\n", terminal)
	})
	settings, _ := p.Settings.Get()
	settings.RateLimitCooldownEnabled = false
	if err := p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
	capacity := 2
	if !sameOwner {
		capacity = 1
	}
	for _, entry := range []struct {
		name, token, workspace, user string
		priority, concurrency        int
	}{{"same", "same-token", "same-workspace", "same-user", 100, capacity}, {"other", "other-token", "other-workspace", "other-user", 0, 2}} {
		credentials, _ := json.Marshal(map[string]any{"access_token": entry.token, "chatgpt_account_id": entry.workspace, "expires_at": time.Now().Add(time.Hour).Unix()})
		if err := p.Store.ImportWith(entry.name, credentials, AccountDefaults{Concurrency: entry.concurrency, Priority: entry.priority, Weight: 1}); err != nil {
			t.Fatal(err)
		}
		if err := p.Store.UpdateAccount(entry.name, func(a *Account) error {
			a.Identity = &managedAccountIdentity{UserID: entry.user, WorkspaceID: entry.workspace}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	old, _ := p.Store.Get("test")
	const session = "concurrent-cas-fixture"
	if _, err := p.sessionAccount(t.Context(), "admin", session, gatewayAccountRef(old)); err != nil {
		t.Fatal(err)
	}
	if err := p.Store.UpdateAccount("test", func(a *Account) error { a.Enabled = false; return nil }); err != nil {
		t.Fatal(err)
	}
	p.invalidateAccounts()
	ctx, cancel := context.WithTimeout(t.Context(), 28*time.Second)
	defer cancel()
	for _, name := range []string{"first", "second"} {
		lane := lanes[name]
		request := proxyRequest(http.MethodPost, "/v1/responses", fmt.Sprintf(`{"model":"gpt-6-astra","stream":%t,"input":"full plaintext lane-%s"}`, stream, name)).WithContext(ctx)
		request.Header.Set("Session-Id", session)
		go func() {
			defer close(lane.done)
			defer func() {
				if value := recover(); value != nil && value != http.ErrAbortHandler {
					panic(value)
				}
			}()
			p.gatewayBPS(lane.response, request)
		}()
		select {
		case got := <-lane.entered:
			want := "same"
			if name == "second" && !sameOwner {
				want = "other"
			}
			if got != want {
				t.Fatalf("fixture selected %s for %s want %s", got, name, want)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("%s did not reach upstream", name)
		}
	}
	close(lanes["first"].release)
	select {
	case <-lanes["first"].done:
	case <-time.After(3 * time.Second):
		t.Fatal("first did not complete")
	}
	if lanes["first"].response.Code != 200 || !strings.Contains(lanes["first"].response.Body.String(), "RESULT_same_first") {
		t.Fatal("first winner did not commit result")
	}
	if slow {
		// Use the real production 20-second lease period, not a shortened
		// overlay or synthetic state mutation, to prove a live request is harmed.
		timer := time.NewTimer(21 * time.Second)
		select {
		case <-lanes["second"].canceled:
			if !timer.Stop() {
				<-timer.C
			}
		case <-timer.C:
		}
	}
	close(lanes["second"].release)
	select {
	case <-lanes["second"].done:
	case <-time.After(3 * time.Second):
		t.Fatal("second did not finish")
	}
	networkMu.Lock()
	calls := append([]string(nil), network...)
	networkMu.Unlock()
	second := lanes["second"].response
	secondCanceled := false
	select {
	case <-lanes["second"].canceled:
		secondCanceled = true
	default:
	}
	bound, err := p.sessionAccount(t.Context(), "admin", session, "")
	winner, _ := p.Store.Get("same")
	if err != nil || bound != gatewayAccountRef(winner) || len(calls) != 2 || p.inFlight("same") != 0 || p.inFlight("other") != 0 {
		t.Fatalf("winner overwritten, repeated generation or slot leak: calls=%v", calls)
	}
	t.Logf("slow=%t same_owner=%t stream=%t first_http=%d second_http=%d second_transport_canceled=%t caller_context=%v calls=%v", slow, sameOwner, stream, lanes["first"].response.Code, second.Code, secondCanceled, ctx.Err(), calls)
	if sameOwner {
		if ctx.Err() != nil || secondCanceled || second.Code != 200 || !strings.Contains(second.Body.String(), "RESULT_same_second") {
			t.Fatalf("healthy same-replacement request failed after concurrent winner: status=%d canceled=%t caller=%v body=%s", second.Code, secondCanceled, ctx.Err(), second.Body.String())
		}
		for _, name := range []string{"first", "second"} {
			history, err := p.bpsLoadHistory(withGatewayUnboundedPayload(t.Context(), true), "admin", "resp_cas_"+name)
			if err != nil || history.Owner != gatewayAccountRef(winner) {
				t.Fatal("same-owner response history not correctly bound")
			}
		}
	} else {
		if second.Code == 200 && !stream {
			t.Fatal("different-owner losing result reported as successful JSON")
		}
		if bytes.Contains(second.Body.Bytes(), []byte("RESULT_other")) || bytes.Contains(second.Body.Bytes(), []byte("synthetic_cipher_other")) {
			t.Fatal("different winner allowed foreign-owned result/cipher to escape")
		}
		if _, err := p.bpsLoadHistory(withGatewayUnboundedPayload(t.Context(), true), "admin", "resp_cas_second"); err == nil {
			t.Fatal("losing owner's history was persisted under winner session")
		}
	}
}

func TestBPSSessionCASSameReplacementGatewayFastCompletes(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) { rootCASConcurrentGateway(t, false, true, stream) })
	}
}
func TestBPSSessionCASDifferentWinnerDoesNotLeakOrReplay(t *testing.T) {
	rootCASConcurrentGateway(t, false, false, false)
}
func TestBPSSessionCASSameReplacementSurvivesOriginalLeaseTick(t *testing.T) {
	rootCASConcurrentGateway(t, true, true, false)
}
