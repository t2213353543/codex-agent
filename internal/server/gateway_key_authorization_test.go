package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestGatewayKeyAuthorizationJSONOtherInstanceRevocation(t *testing.T) {
	for _, mutation := range []string{"disabled", "deleted", "rotated"} {
		t.Run(mutation, func(t *testing.T) {
			p := newTestProxy(t, 1)
			key, _ := gatewaySessionAuthorizationFixture(t, p)
			writer := gatewayKeyAuthorizationWriter(t, p, key)
			gatewayKeyAuthorizationMutate(t, writer, key, mutation)
			gatewayKeyAuthorizationAssertStaleIndex(t, p, key)
			if _, _, valid := p.identifyKeyContext(t.Context(), key.Key); valid {
				t.Fatal("gateway accepted a key revoked by another store instance")
			}
		})
	}
}

func TestGatewayKeyAuthorizationHTTPRejectsOtherInstanceChanges(t *testing.T) {
	for _, mutation := range []string{"disabled", "deleted", "rotated", "groups"} {
		t.Run(mutation, func(t *testing.T) {
			var requests, original, alternate atomic.Int32
			p := gatewaySelectionAuthorizationProxy(t, &requests, &original, &alternate)
			key, account := gatewaySessionAuthorizationFixture(t, p)
			writer := gatewayKeyAuthorizationWriter(t, p, key)
			gatewaySelectionAuthorizationWarm(t, p, key, account, 2)
			gatewayKeyAuthorizationMutate(t, writer, key, mutation)
			gatewayKeyAuthorizationAssertStaleIndex(t, p, key)
			const session = "cross-instance-key-http"
			w := gatewaySelectionAuthorizationSend(p, key, session)
			if w.Code < 400 || requests.Load() != 0 {
				t.Errorf("other-instance key revocation reached upstream: status=%d requests=%d", w.Code, requests.Load())
			}
			bound, err := p.sessionAccount(t.Context(), key.ID, session, "")
			if err != nil || bound != "" {
				t.Error("revoked key established a new account binding")
			}
			if p.inFlight(account.ID) != 0 || p.inFlight("alternate") != 0 {
				t.Fatal("revoked key retained account admission")
			}
		})
	}
}

func TestGatewayKeyAuthorizationWebSocketRejectsOtherInstanceChanges(t *testing.T) {
	for _, mutation := range []string{"disabled", "deleted", "rotated", "groups", "store_error"} {
		t.Run(mutation, func(t *testing.T) {
			var handshakes, turns atomic.Int32
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				handshakes.Add(1)
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer conn.CloseNow()
				for {
					if _, _, err := conn.Read(r.Context()); err != nil {
						return
					}
					turns.Add(1)
					if err := conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"key-turn","status":"completed","output":[]}}`)); err != nil {
						return
					}
				}
			})
			key, account := gatewaySessionAuthorizationFixture(t, p)
			writer := gatewayKeyAuthorizationWriter(t, p, key)
			enableGatewayWS(t, p, "")
			server := httptest.NewServer(p)
			defer server.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			const session = "cross-instance-key-websocket"
			headers := http.Header{"Authorization": {"Bearer " + key.Key}, "Session-Id": {session}}
			conn, _, err := websocket.Dial(ctx, server.URL+"/responses", &websocket.DialOptions{HTTPHeader: headers})
			if err != nil {
				t.Fatal("ordinary-key WebSocket fixture could not connect")
			}
			defer conn.CloseNow()
			send := func() (string, int) {
				if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create","model":"fixture","input":[]}`)); err != nil {
					t.Fatal(err)
				}
				var frame struct {
					Type   string `json:"type"`
					Status int    `json:"status"`
				}
				if err := json.Unmarshal(gatewayWSRead(t, conn), &frame); err != nil {
					t.Fatal(err)
				}
				return frame.Type, frame.Status
			}
			if kind, _ := send(); kind != "response.completed" || turns.Load() != 1 {
				t.Fatal("ordinary-key WebSocket initial turn did not complete")
			}
			waitGatewayWSReleased(t, p)
			gatewayKeyAuthorizationMutate(t, writer, key, mutation)
			gatewayKeyAuthorizationAssertStaleIndex(t, p, key)
			kind, status := send()
			wantStatus := http.StatusForbidden
			if mutation == "store_error" {
				wantStatus = http.StatusServiceUnavailable
			}
			if kind != "error" || status != wantStatus || turns.Load() != 1 || handshakes.Load() != 1 {
				t.Errorf("other-instance key revocation reached established WebSocket: type=%s status=%d turns=%d handshakes=%d", kind, status, turns.Load(), handshakes.Load())
			}
			waitGatewayWSReleased(t, p)
			gatewaySessionAuthorizationAssertBinding(t, p, key.ID, session, account)
			if p.inFlight("alternate") != 0 {
				t.Fatal("revoked WebSocket key admitted another account")
			}
		})
	}
}

func TestGatewayKeyAuthorizationCurrentLookupAndNewCredentials(t *testing.T) {
	t.Run("new_key_without_reader_index", func(t *testing.T) {
		var requests, original, alternate atomic.Int32
		p := gatewaySelectionAuthorizationProxy(t, &requests, &original, &alternate)
		key, _ := gatewaySessionAuthorizationFixture(t, p)
		writer := gatewayKeyAuthorizationWriter(t, p, key)
		created, err := writer.Create("new-other-instance-key", []string{"group-one"})
		if err != nil {
			t.Fatal(err)
		}
		p.Keys.mu.RLock()
		_, indexed := p.Keys.index[created.Key]
		p.Keys.mu.RUnlock()
		if indexed {
			t.Fatal("new key was unexpectedly present in the reader index")
		}
		if w := gatewaySelectionAuthorizationSend(p, created, "new-key-session"); w.Code != http.StatusOK || requests.Load() != 1 {
			t.Fatal("new current key required management-index synchronization")
		}
		p.Keys.mu.RLock()
		_, indexed = p.Keys.index[created.Key]
		p.Keys.mu.RUnlock()
		if indexed {
			t.Fatal("gateway authorization rewrote the management key index")
		}
	})
	t.Run("rotation_old_denied_new_allowed", func(t *testing.T) {
		var requests, original, alternate atomic.Int32
		p := gatewaySelectionAuthorizationProxy(t, &requests, &original, &alternate)
		key, _ := gatewaySessionAuthorizationFixture(t, p)
		writer := gatewayKeyAuthorizationWriter(t, p, key)
		gatewayKeyAuthorizationMutate(t, writer, key, "rotated")
		gatewayKeyAuthorizationAssertStaleIndex(t, p, key)
		if _, _, valid := p.identifyKeyContext(t.Context(), key.Key); valid {
			t.Fatal("rotated token retained gateway authorization")
		}
		current, ok := writer.Lookup("sk-synthetic-replacement-key")
		if !ok {
			t.Fatal("fixture replacement key missing")
		}
		if w := gatewaySelectionAuthorizationSend(p, *current, "rotated-key-session"); w.Code != http.StatusOK || requests.Load() != 1 {
			t.Fatal("replacement token could not authorize independently of the stale index")
		}
		gatewayKeyAuthorizationAssertStaleIndex(t, p, key)
	})
	t.Run("detached_groups", func(t *testing.T) {
		p := newTestProxy(t, 1)
		key, _ := gatewaySessionAuthorizationFixture(t, p)
		gatewayKeyAuthorizationWriter(t, p, key)
		current, found, err := p.Keys.LookupCurrent(t.Context(), key.Key)
		if err != nil || !found || len(current.Groups) != 1 {
			t.Fatal("current key could not be read")
		}
		current.Groups[0] = "mutated-return-value"
		current.Groups = append(current.Groups, "extra-return-value")
		again, found, err := p.Keys.LookupCurrent(t.Context(), key.Key)
		if err != nil || !found || !slices.Equal(again.Groups, []string{"group-one"}) {
			t.Fatal("mutating a returned key changed a later current lookup")
		}
		gatewayKeyAuthorizationAssertStaleIndex(t, p, key)
	})
}

func TestGatewayKeyAuthorizationStoreFailureIsUnavailableIngress(t *testing.T) {
	var requests, original, alternate atomic.Int32
	p := gatewaySelectionAuthorizationProxy(t, &requests, &original, &alternate)
	key, _ := gatewaySessionAuthorizationFixture(t, p)
	writer := gatewayKeyAuthorizationWriter(t, p, key)
	if err := p.startGatewayIngressEvents(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = p.IngressEvents.Close(ctx)
	})
	gatewayKeyAuthorizationMutate(t, writer, key, "store_error")
	gatewayKeyAuthorizationAssertStaleIndex(t, p, key)
	if w := gatewaySelectionAuthorizationSend(p, key, "store-unavailable-session"); w.Code != http.StatusServiceUnavailable || requests.Load() != 0 {
		t.Errorf("key storage failure did not fail closed before business handling: status=%d upstream=%d", w.Code, requests.Load())
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := p.IngressEvents.Close(ctx); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/api/ingress-events?reason=key_store_unavailable&status=503", nil)
	w := httptest.NewRecorder()
	p.handleManagedIngressEvents(w, r)
	var page ingressPageFixture
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &page) != nil || page.Total != 1 || len(page.Items) != 1 || page.Items[0].Reason != "key_store_unavailable" || page.Items[0].HTTPCode != http.StatusServiceUnavailable {
		t.Fatal("key storage failure was not retained and queryable as a bounded ingress reason")
	}
	p.Metrics.mu.Lock()
	businessRecords := len(p.Metrics.accounts)
	p.Metrics.mu.Unlock()
	if businessRecords != 0 {
		t.Fatal("key storage failure entered business usage metrics")
	}
	gatewayKeyAuthorizationAssertStaleIndex(t, p, key)
}

func gatewayKeyAuthorizationWriter(t *testing.T, p *Proxy, key APIKey) *KeyStore {
	t.Helper()
	if _, err := p.Keys.List(); err != nil {
		t.Fatal(err)
	}
	writer := NewKeyStore(p.Keys.store.path)
	if current, ok := writer.Lookup(key.Key); !ok || current.ID != key.ID {
		t.Fatal("independent writer did not load the same initial key")
	}
	if writer == p.Keys || writer.store == p.Keys.store {
		t.Fatal("fixture did not create independent key indexes")
	}
	return writer
}

func gatewayKeyAuthorizationMutate(t *testing.T, writer *KeyStore, key APIKey, mutation string) {
	t.Helper()
	if mutation == "store_error" {
		raw, err := os.ReadFile(writer.store.path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := os.WriteFile(writer.store.path, raw, 0600); err != nil {
				t.Error("could not restore private synthetic key storage")
			}
		})
		if err := os.WriteFile(writer.store.path, []byte(`{"keys":`), 0600); err != nil {
			t.Fatal(err)
		}
		return
	}
	var err error
	if mutation == "deleted" {
		err = writer.Delete(key.ID)
	} else {
		_, err = writer.Update(key.ID, func(current *APIKey) error {
			switch mutation {
			case "disabled":
				current.Enabled = false
			case "rotated":
				current.Key = "sk-synthetic-replacement-key"
			case "groups":
				current.Groups = []string{"group-two"}
			}
			return nil
		})
	}
	if err != nil {
		t.Fatal(err)
	}
}

func gatewayKeyAuthorizationAssertStaleIndex(t *testing.T, p *Proxy, key APIKey) {
	t.Helper()
	p.Keys.mu.RLock()
	defer p.Keys.mu.RUnlock()
	old := p.Keys.index[key.Key]
	if old == nil || old.ID != key.ID || !old.Enabled || old.Version != key.Version || len(old.Groups) != 1 || old.Groups[0] != "group-one" {
		t.Fatal("reader key index was accidentally refreshed before authorization")
	}
}
