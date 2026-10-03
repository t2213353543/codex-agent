package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coder/websocket"
)

func TestGatewayRecordCredentialsFollowRetryAndKeepOldInflightEpoch(t *testing.T) {
	for _, retry := range []bool{false, true} {
		t.Run(fmt.Sprint(retry), func(t *testing.T) {
			p := proxyFixture(t, func(http.ResponseWriter, *http.Request) { t.Error("unexpected network grant") })
			if err := p.Store.UpdateAccount("test", func(a *Account) error {
				a.Identity = &managedAccountIdentity{WorkspaceID: "synthetic-workspace", UserID: "synthetic-user"}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			original, err := p.Store.Get("test")
			if err != nil || original.CredentialVersion <= 0 {
				t.Fatal("missing original epoch", err)
			}
			req, _ := http.NewRequestWithContext(context.Background(), "POST", "http://127.0.0.1/responses", strings.NewReader(`{}`))
			req.Header.Set("Authorization", "Bearer "+original.AccessToken)
			calls := 0
			var current Account
			record := RequestRecord{}
			var authorize func(Account) error
			if retry {
				authorize = func(Account) error { return nil }
			}
			response, err := p.doObservedAccountRequest(&record, p.Client, req, original, func(_ *http.Client, request *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					if err := p.Store.UpdateAccount(original.ID, func(a *Account) error { a.AccessToken = "synthetic-new-access"; return nil }); err != nil {
						t.Fatal(err)
					}
					current, err = p.Store.Get(original.ID)
					if err != nil {
						t.Fatal(err)
					}
				} else if request.Header.Get("Authorization") != "Bearer "+current.AccessToken {
					t.Fatal("retry used old token")
				}
				status := 200
				if retry && calls == 1 {
					status = 401
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
			}, authorize)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			want := original.CredentialVersion
			if retry {
				want = current.CredentialVersion
				if calls != 2 {
					t.Fatalf("retry calls=%d", calls)
				}
			}
			if want <= 0 || current.CredentialVersion <= original.CredentialVersion || record.AccountID != original.ID || record.AccountCredentialVersion != want {
				t.Fatalf("record mismatched actual credentials: got id=%q epoch=%d want=%d old=%d new=%d", record.AccountID, record.AccountCredentialVersion, want, original.CredentialVersion, current.CredentialVersion)
			}
		})
	}
}

func TestGatewayRecordWebSocketKeepsHandshakeEpochAfterReauthorization(t *testing.T) {
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		for i := range 2 {
			if _, _, err := conn.Read(r.Context()); err != nil {
				return
			}
			if err := conn.Write(r.Context(), websocket.MessageText, []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_epoch_%d","status":"completed"}}`, i))); err != nil {
				return
			}
		}
	})
	if err := p.Store.UpdateAccount("test", func(a *Account) error {
		a.Identity = &managedAccountIdentity{WorkspaceID: "synthetic-workspace", UserID: "synthetic-user"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	enableGatewayWS(t, p, "")
	original, err := p.Store.Get("test")
	if err != nil || original.CredentialVersion <= 0 {
		t.Fatal("missing handshake epoch", err)
	}
	server := httptest.NewServer(p)
	defer server.Close()
	conn, _ := gatewayWSDial(t, server.URL, "record-epoch-session")
	defer conn.CloseNow()
	request := []byte(`{"type":"response.create","model":"test","input":[]}`)
	for i := range 2 {
		if i == 1 {
			if err := p.Store.UpdateAccount(original.ID, func(a *Account) error { a.AccessToken = "synthetic-new-ws-at"; return nil }); err != nil {
				t.Fatal(err)
			}
			p.invalidateAccounts()
		}
		if err := gatewayWSWrite(context.Background(), conn, websocket.MessageText, request); err != nil {
			t.Fatal(err)
		}
		if body := gatewayWSRead(t, conn); !strings.Contains(string(body), "response.completed") {
			t.Fatalf("turn failed: %s", body)
		}
		waitGatewayWSReleased(t, p)
	}
	current, _ := p.Store.Get(original.ID)
	if current.CredentialVersion <= original.CredentialVersion {
		t.Fatal("fixture did not replace token")
	}
	records := p.Metrics.RecentForAccount(original.Name, 10)
	if len(records) != 2 {
		t.Fatalf("records=%d", len(records))
	}
	for _, record := range records {
		if record.AccountID != original.ID || record.AccountCredentialVersion != original.CredentialVersion {
			t.Fatalf("old socket misattributed to new credentials: %d want %d", record.AccountCredentialVersion, original.CredentialVersion)
		}
	}
}
