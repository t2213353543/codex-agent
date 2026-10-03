package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Credential refresh holds a session advisory lock, not a transaction across
// OAuth. A separate writer pool can commit management changes in the callback.
func TestPGGatewayRecoveryAuthorizationRechecksAfterConcurrentOAuthMutation(t *testing.T) {
	for _, mutation := range []string{"account_groups", "account_disabled", "key_groups", "key_disabled"} {
		t.Run(mutation, func(t *testing.T) {
			f := newGatewaySelectionPGFixture(t, false, false)
			config := f.writer.Store.DB.Pool.Config()
			config.MaxConns, config.MinConns = 2, 0
			writerPool, err := pgxpool.NewWithConfig(t.Context(), config)
			if err != nil {
				t.Fatal("cannot initialize independent management writer pool")
			}
			t.Cleanup(writerPool.Close)
			writerDB := *f.writer.Store.DB
			writerDB.Pool = writerPool
			f.writer.Store.DB = &writerDB
			f.writer.Keys, err = NewPGKeyStore(&writerDB)
			if err != nil {
				t.Fatal("cannot initialize independent management key writer")
			}
			metrics, err := NewMetrics(t.TempDir())
			if err != nil {
				t.Fatal("cannot initialize isolated request observations")
			}
			t.Cleanup(metrics.Close)
			f.reader.Metrics = metrics
			const session = "pg-recovery-authorization-session"
			f.selectNewSession(t, session, &f.primary)
			gatewayKeyPGAssertCachedSnapshot(t, f.reader.Keys, f.key)
			var requests, refreshes, mutations atomic.Int32
			const initialFailure = `{"error":{"message":"synthetic initial unauthorized"}}`
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				switch r.URL.Path {
				case "/oauth/token":
					if r.Method != http.MethodPost || refreshes.Add(1) != 1 {
						t.Error("unexpected duplicate or non-POST OAuth request")
						http.Error(w, "synthetic unexpected OAuth request", http.StatusInternalServerError)
						return
					}
					var mutateErr error
					if strings.HasPrefix(mutation, "account_") {
						mutateErr = f.writer.Store.UpdateAccount(f.primary.ID, func(account *Account) error {
							if mutation == "account_groups" {
								account.Groups = []string{f.otherGroup}
							} else {
								account.Enabled = false
							}
							return nil
						})
					} else {
						_, mutateErr = f.writer.Keys.Update(f.key.ID, func(key *APIKey) error {
							if mutation == "key_groups" {
								key.Groups = []string{f.otherGroup}
							} else {
								key.Enabled = false
							}
							return nil
						})
					}
					if mutateErr != nil {
						t.Error("independent management mutation failed during OAuth")
						http.Error(w, "synthetic management mutation failed", http.StatusInternalServerError)
						return
					}
					mutations.Add(1)
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, gatewayKnownOwnerRefreshFixture(gatewayRecoveryAuthorizationTokens, f.primary.AccountID, f.primary.Identity.UserID))
				case "/responses":
					if r.Method != http.MethodPost || requests.Add(1) != 1 {
						t.Error("inference was retried after authorization changed during OAuth")
						http.Error(w, "synthetic forbidden retry", http.StatusInternalServerError)
						return
					}
					if r.Header.Get("Authorization") != "Bearer "+f.primary.AccessToken {
						t.Error("first model request used unexpected credentials")
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusUnauthorized)
					_, _ = io.WriteString(w, initialFailure)
				default:
					t.Error("unexpected request outside the two synthetic upstream endpoints")
					http.Error(w, "synthetic unexpected path", http.StatusNotFound)
				}
			}))
			t.Cleanup(server.Close)
			f.reader.Client = server.Client()
			f.reader.Client.Timeout = 5 * time.Second
			f.reader.BaseURL, f.reader.TokenURL = server.URL, server.URL+"/oauth/token"
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			r := proxyRequest(http.MethodPost, "/responses", `{"model":"fixture","stream":false,"input":[]}`).WithContext(ctx)
			r.Header.Set("Authorization", "Bearer "+f.key.Key)
			r.Header.Set("Session-Id", session)
			w := httptest.NewRecorder()
			f.reader.ServeHTTP(w, r)
			wantStatus := http.StatusForbidden
			if mutation == "account_disabled" {
				// recoverUnauthorized itself rejects a disabled refreshed row;
				// that existing failure path preserves the first upstream 401.
				wantStatus = http.StatusUnauthorized
			}
			if w.Code != wantStatus || requests.Load() != 1 || refreshes.Load() != 1 || mutations.Load() != 1 {
				t.Fatalf("recovery dispatch mismatch: status=%d want=%d requests=%d refreshes=%d mutations=%d",
					w.Code, wantStatus, requests.Load(), refreshes.Load(), mutations.Load())
			}
			if mutation == "account_disabled" {
				if w.Body.String() != initialFailure {
					t.Fatal("existing disabled-account recovery contract lost the original upstream 401 body")
				}
			} else if !strings.Contains(w.Body.String(), (&gatewayRecoveryAuthorizationError{status: wantStatus}).Error()) {
				t.Fatal("local post-recovery authorization failure was mislabeled as upstream failure")
			}
			current, err := f.writer.Store.Get(f.primary.ID)
			if err != nil || current.AccessToken != "synthetic-recovery-access" || current.RefreshToken != "synthetic-recovery-refresh" ||
				current.CredentialVersion != f.primary.CredentialVersion+1 || current.ExpiresAt <= time.Now().Unix() ||
				current.OwnerRevision != f.primary.OwnerRevision || gatewayAccountRef(current) != gatewayAccountRef(f.primary) ||
				current.LastError != f.primary.LastError || current.CredentialStatus == "refresh_failed" {
				t.Fatal("authorization denial lost successful OAuth rotation or changed ownership/error state")
			}
			wantGroups := f.primary.Groups
			if mutation == "account_groups" {
				wantGroups = []string{f.otherGroup}
			}
			if !reflect.DeepEqual(current.Groups, wantGroups) || current.Enabled != (mutation != "account_disabled") {
				t.Fatal("credential persistence overwrote concurrently committed account eligibility")
			}
			key, found, err := f.writer.Keys.LookupCurrent(t.Context(), f.key.Key)
			if mutation == "key_disabled" {
				if err != nil || found {
					t.Fatal("concurrent key disable was not persisted")
				}
			} else {
				wantKeyGroups := f.key.Groups
				if mutation == "key_groups" {
					wantKeyGroups = []string{f.otherGroup}
				}
				if err != nil || !found || key.ID != f.key.ID || !reflect.DeepEqual(key.Groups, wantKeyGroups) {
					t.Fatal("concurrent key groups were not persisted")
				}
			}
			gatewayKeyPGAssertCachedSnapshot(t, f.reader.Keys, f.key)
			bound, err := f.reader.sessionAccount(t.Context(), f.key.ID, session, "")
			if err != nil || bound != gatewayAccountRef(f.primary) || f.reader.inFlight(gatewayAdmissionKey(f.primary)) != 0 {
				t.Fatal("recovery denial migrated the original Redis binding or leaked admission")
			}
			record, found := metrics.LastFor(f.primary.Name)
			if !found || record.Status != "error" || len(record.Attempts) != 2 || record.AttemptsTruncated == nil || *record.AttemptsTruncated {
				t.Fatal("recovery observations omitted a real attempt or invented a retry")
			}
			for i, want := range []struct {
				phase, endpoint string
				status          int
			}{{"request", "/responses", 401}, {"refresh", "/oauth/token", 200}} {
				attempt := record.Attempts[i]
				if attempt.Attempt != i+1 || attempt.Phase != want.phase || attempt.Endpoint != want.endpoint ||
					attempt.StatusCode == nil || *attempt.StatusCode != want.status || attempt.DurationMs == nil {
					t.Fatal("attempt metadata does not match the actual request and OAuth exchange")
				}
			}
			wantSource, wantPhase := "gateway", "authorization"
			if mutation == "account_disabled" {
				wantSource, wantPhase = "upstream", "http_status"
			}
			if record.ErrorSource != wantSource || record.ErrorPhase != wantPhase {
				t.Fatal("post-recovery outcome has the wrong passive error classification")
			}
			raw, err := json.Marshal(record)
			if err != nil {
				t.Fatal("cannot inspect synthetic observation privacy")
			}
			for _, secret := range []string{f.key.Key, f.primary.AccessToken, f.primary.RefreshToken, current.AccessToken, current.RefreshToken} {
				if strings.Contains(string(raw), secret) {
					t.Fatal("request observations exposed fixture credentials")
				}
			}
		})
	}
}
