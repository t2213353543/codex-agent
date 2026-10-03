package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAppServerProfileRequestAllocationSurvivesRetryAndRoute(t *testing.T) {
	ctx := withAppServerRequestAllocation(withGatewayRequestRoute(context.Background(), "key-A", "session-A"))
	req := httptest.NewRequest(http.MethodPost, "/responses", nil).WithContext(ctx)
	a := appServerRequestDefaults(req, "installation")
	b := appServerRequestDefaults(req.Clone(ctx), "installation")
	if a.SessionID != b.SessionID || a.TurnID != b.TurnID || *a.TurnStartedAtUnixMS != *b.TurnStartedAtUnixMS {
		t.Fatal("retry changed request identities")
	}
	next := req.WithContext(withAppServerRequestAllocation(withGatewayRequestRoute(context.Background(), "key-A", "session-A")))
	c := appServerRequestDefaults(next, "installation")
	if a.SessionID != c.SessionID || a.TurnID == c.TurnID {
		t.Fatal("route identity or turn lifetime incorrect")
	}
	other := req.WithContext(withAppServerRequestAllocation(withGatewayRequestRoute(context.Background(), "key-B", "session-A")))
	if appServerRequestDefaults(other, "installation").SessionID == a.SessionID {
		t.Fatal("different gateway key shared generated identity")
	}
	body := map[string]json.RawMessage{"client_metadata": json.RawMessage(`{"session_id":"client-session","thread_id":"client-thread","x-codex-turn-metadata":"{\"session_id\":\"client-session\",\"thread_id\":\"client-thread\"}"}`)}
	h := appServerRequestIdentityHeaders(http.Header{}, body, a)
	if h.Get("Session-Id") != "client-session" || h.Get("Thread-Id") != "client-thread" {
		t.Fatal("allocation overwrote client metadata identities")
	}
}
