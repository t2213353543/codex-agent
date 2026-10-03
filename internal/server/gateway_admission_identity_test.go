package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestGatewayAdmissionSurvivesAccountRename(t *testing.T) {
	for _, session := range []string{"", "sticky-admission-rename"} {
		t.Run("session_"+session, func(t *testing.T) {
			entered, finish, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var enterOnce, finishOnce sync.Once
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				enterOnce.Do(func() { close(entered) })
				<-finish
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[]}}\n\n")
			})
			defer finishOnce.Do(func() { close(finish) })
			if err := p.Store.UpdateAccount("test", func(a *Account) error { a.Concurrency = 1; return nil }); err != nil {
				t.Fatal(err)
			}
			account, err := p.Store.Get("test")
			if err != nil {
				t.Fatal(err)
			}
			key := gatewayAdmissionKey(account)
			request := func() *http.Request {
				r := proxyRequest(http.MethodPost, "/v1/responses", `{"model":"test","input":[],"stream":true}`)
				if session != "" {
					r.Header.Set("Session-Id", session)
				}
				return r
			}
			first := httptest.NewRecorder()
			go func() { defer close(done); p.ServeHTTP(first, request()) }()
			<-entered
			if err := p.Store.ApplyAll(func(state *accountState) error {
				for i := range state.Accounts {
					if state.Accounts[i].ID == account.ID {
						state.Accounts[i].Name = "renamed"
					}
				}
				if state.Active == account.Name {
					state.Active = "renamed"
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			p.invalidateAccounts()
			second := httptest.NewRecorder()
			p.ServeHTTP(second, request())
			if second.Code != http.StatusTooManyRequests || p.inFlight(key) != 1 || p.inFlight("renamed") != 1 {
				t.Errorf("renamed occupied account: HTTP=%d inflight=%d", second.Code, p.inFlight(key))
			}
			finishOnce.Do(func() { close(finish) })
			<-done
			third := httptest.NewRecorder()
			p.ServeHTTP(third, request())
			if first.Code != http.StatusOK || third.Code != http.StatusOK || p.inFlight(key) != 0 {
				t.Errorf("release after rename: first=%d third=%d inflight=%d", first.Code, third.Code, p.inFlight(key))
			}
		})
	}
}

func TestGatewayAdmissionReleaseIsIdempotent(t *testing.T) {
	p := newTestProxy(t, 1)
	a, err := p.Store.Get("test")
	if err != nil {
		t.Fatal(err)
	}
	first, ok := p.acquireGatewayAccount(a)
	if !ok {
		t.Fatal("first admission failed")
	}
	first()
	a.Name = "renamed"
	second, ok := p.acquireGatewayAccount(a)
	if !ok {
		t.Fatal("slot was not released")
	}
	first()
	if p.inFlight(gatewayAdmissionKey(a)) != 1 {
		t.Fatal("duplicate old release consumed a new turn's slot")
	}
	second()
	if len(p.admission) != 0 {
		t.Fatal("idle admission identity was not reclaimed")
	}
}
