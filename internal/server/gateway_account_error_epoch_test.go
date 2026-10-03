package server

import (
	"context"
	"errors"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"
)

// Pause at the actual post-refresh cache invalidation. The file refresh lock
// has been released here, so reauthorization can commit before error recording.
func TestGatewayAccountErrorAfterRefreshAndConcurrentEdit(t *testing.T) {
	for _, bound := range []bool{false, true} {
		for _, edit := range []string{"credentials", "rename", "reuse_name"} {
			name := edit
			if bound {
				name += "_bound"
			}
			t.Run(name, func(t *testing.T) {
				entered, reply := make(chan struct{}), make(chan struct{})
				var once sync.Once
				p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
					close(entered)
					<-reply
					w.WriteHeader(http.StatusUnauthorized)
					_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
				})
				defer once.Do(func() { close(reply) })
				if err := p.Store.UpdateAccount("test", func(a *Account) error {
					a.ExpiresAt = time.Now().Add(-time.Hour).Unix()
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				selected, err := p.Store.Get("test")
				if err != nil {
					t.Fatal(err)
				}
				session := ""
				if bound {
					session = "synthetic-error-epoch"
					if _, err := p.sessionAccount(t.Context(), "admin", session, gatewayAccountRef(selected)); err != nil {
						t.Fatal(err)
					}
				}
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				done := make(chan error, 1)
				go func() {
					_, release, err := p.pickSessionAccount(ctx, p.AdminKey, "admin", session)
					if release != nil {
						release()
					}
					done <- err
				}()
				select {
				case <-entered:
				case <-ctx.Done():
					t.Fatal("refresh did not start")
				}
				p.accountsMu.Lock()
				locked := true
				defer func() {
					if locked {
						p.accountsMu.Unlock()
					}
				}()
				once.Do(func() { close(reply) })
				if edit == "credentials" {
					incoming := selected
					incoming.AccessToken, incoming.RefreshToken = "synthetic-new-at", "synthetic-new-rt"
					incoming.ExpiresAt = time.Now().Add(time.Hour).Unix()
					if _, err := p.Store.reauthorizeManagedAccount(ctx, selected.ID, selected.Version, incoming, false); err != nil {
						t.Fatal(err)
					}
				} else {
					if _, err := p.Store.UpdateAccountVersion(selected.ID, selected.Version, func(a *Account) error { a.Name = "renamed-original"; return nil }); err != nil {
						t.Fatal(err)
					}
					if edit == "reuse_name" {
						if err := p.Store.Import("test", []byte(`{"access_token":"synthetic-other-at","account_id":"synthetic-other-owner"}`)); err != nil {
							t.Fatal(err)
						}
					}
				}
				p.accountsMu.Unlock()
				locked = false
				select {
				case err := <-done:
					if err == nil || errors.Is(err, context.DeadlineExceeded) {
						t.Fatal("expected completed original refresh failure")
					}
				case <-ctx.Done():
					t.Fatal("failed selection did not finish")
				}
				current, err := p.Store.getAccountByID(selected.ID)
				if err != nil {
					t.Fatal(err)
				}
				if edit == "credentials" {
					if current.LastError != "" || current.CredentialStatus != "unverified" || current.CredentialVersion <= selected.CredentialVersion {
						t.Fatal("old refresh failure contaminated replacement credentials")
					}
				} else if current.LastError == "" || current.CredentialVersion != selected.CredentialVersion {
					t.Fatal("same-credential failure lost after rename")
				}
				if edit == "reuse_name" {
					other, err := p.Store.Get("test")
					if err != nil || other.ID == selected.ID || other.LastError != "" {
						t.Fatal("old failure was written to account reusing the name")
					}
				}
			})
		}
	}
}

func TestGatewayAccountRepeatedErrorDoesNotRewriteFile(t *testing.T) {
	p := newTestProxy(t, 1)
	selected, err := p.Store.Get("test")
	if err != nil {
		t.Fatal(err)
	}
	p.noteAccountError(selected, errors.New("synthetic refresh failure"))
	after, err := p.Store.Get("test")
	if err != nil || after.LastError != "synthetic refresh failure" {
		t.Fatal("first error missing")
	}
	old := time.Unix(1_600_000_000, 0)
	if err := os.Chtimes(p.Store.Path, old, old); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		p.noteAccountError(selected, errors.New("synthetic refresh failure"))
	}
	info, err := os.Stat(p.Store.Path)
	if err != nil || !info.ModTime().Equal(old) {
		t.Fatal("unchanged error rewrote and synced the account store")
	}
}
