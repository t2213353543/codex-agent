package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type httpRecoveryLifecycleBody struct {
	io.ReadCloser
	closed atomic.Int32
}

func TestHTTPRecoveryLifecycleCancellationDuringGrantCloses401AndPreservesRotation(t *testing.T) {
	for _, outcome := range []string{"rejected", "rotated"} {
		for _, cause := range []string{"cancel", "deadline"} {
			t.Run(outcome+"/"+cause, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer func() { cancel() }()
				var grants, requests, clones atomic.Int32
				p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(io.Discard, r.Body)
					if r.URL.Path != "/oauth/token" {
						requests.Add(1)
						w.WriteHeader(401)
						_, _ = io.WriteString(w, "original diagnostic")
						return
					}
					grants.Add(1)
					if cause == "cancel" {
						cancel()
					} else {
						<-ctx.Done()
					}
					if r.Context().Err() != nil {
						t.Error("admitted rotating grant incorrectly inherited caller cancellation")
					}
					if outcome == "rejected" {
						w.WriteHeader(503)
						_, _ = io.WriteString(w, `{"error":"temporarily_unavailable"}`)
						return
					}
					_, _ = io.WriteString(w, gatewayKnownOwnerRefreshFixture(`{"access_token":"after-cancel-access","refresh_token":"after-cancel-refresh"}`, "test-account", "fixture-user"))
				})
				account, _ := p.Store.Get("test")
				if cause == "deadline" {
					cancel()
					deadlineCtx, cancelDeadline := context.WithTimeout(t.Context(), 250*time.Millisecond)
					defer cancelDeadline()
					ctx = deadlineCtx
				}
				request, _ := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/responses", strings.NewReader("exact"))
				request.GetBody = func() (io.ReadCloser, error) { clones.Add(1); return io.NopCloser(strings.NewReader("exact")), nil }
				var body *httpRecoveryLifecycleBody
				response, err := p.doAccountRequest(p.Client, request, account, func(c *http.Client, r *http.Request) (*http.Response, error) {
					res, e := c.Do(r)
					if res != nil {
						body = &httpRecoveryLifecycleBody{ReadCloser: res.Body}
						res.Body = body
					}
					return res, e
				}, func(Account) error { return nil })
				if response != nil {
					defer response.Body.Close()
				}
				if response != nil || !errors.Is(err, ctx.Err()) || ctx.Err() == nil || body == nil || body.closed.Load() != 1 || grants.Load() != 1 || requests.Load() != 1 || clones.Load() != 0 {
					t.Fatalf("cancelled recovery lifecycle incorrect: response=%v err=%v grants=%d requests=%d clones=%d", response != nil, err, grants.Load(), requests.Load(), clones.Load())
				}
				stored, _ := p.Store.Get("test")
				wantAccess, wantRefresh := account.AccessToken, account.RefreshToken
				if outcome == "rotated" {
					wantAccess, wantRefresh = "after-cancel-access", "after-cancel-refresh"
				}
				if stored.AccessToken != wantAccess || stored.RefreshToken != wantRefresh {
					t.Fatal("admitted successful rotation was lost or rejected grant changed credentials")
				}
			})
		}
	}
}

func (b *httpRecoveryLifecycleBody) Close() error {
	b.closed.Add(1)
	return b.ReadCloser.Close()
}

func TestHTTPRecoveryLifecycleCancellationDoesNotReturnLive401(t *testing.T) {
	for _, phase := range []string{"after_first_headers", "getbody_error", "getbody_nil", "getbody_nobody", "getbody_success"} {
		t.Run(phase, func(t *testing.T) {
			var requests, grants atomic.Int32
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if r.URL.Path == "/oauth/token" {
					grants.Add(1)
					_, _ = io.WriteString(w, gatewayKnownOwnerRefreshFixture(`{"access_token":"new-access","refresh_token":"new-refresh"}`, "test-account", "fixture-user"))
					return
				}
				requests.Add(1)
				w.WriteHeader(401)
				_, _ = io.WriteString(w, `{"error":"original private diagnostic"}`)
			})
			account, _ := p.Store.Get("test")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			request, _ := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/responses", strings.NewReader("unchanged-body"))
			clone := &httpRecoveryLifecycleBody{ReadCloser: io.NopCloser(strings.NewReader("unchanged-body"))}
			var cloned atomic.Int32
			if phase != "after_first_headers" {
				request.GetBody = func() (io.ReadCloser, error) {
					cloned.Add(1)
					cancel()
					switch phase {
					case "getbody_error":
						return clone, errors.New("synthetic clone failed")
					case "getbody_nil":
						return nil, nil
					case "getbody_nobody":
						return http.NoBody, nil
					default:
						return clone, nil
					}
				}
			}
			var received *httpRecoveryLifecycleBody
			var dispatches atomic.Int32
			response, err := p.doAccountRequest(p.Client, request, account, func(client *http.Client, request *http.Request) (*http.Response, error) {
				dispatches.Add(1)
				response, err := client.Do(request)
				if response != nil {
					received = &httpRecoveryLifecycleBody{ReadCloser: response.Body}
					response.Body = received
				}
				if phase == "after_first_headers" {
					cancel()
				}
				return response, err
			}, func(Account) error { return nil })
			if response != nil {
				defer response.Body.Close()
			}
			if !errors.Is(err, context.Canceled) || response != nil || received == nil || received.closed.Load() != 1 || dispatches.Load() != 1 || requests.Load() != 1 {
				t.Fatalf("cancellation leaked or exposed original 401: err=%v response=%v closes=%d dispatches=%d requests=%d", err, response != nil, func() int32 {
					if received == nil {
						return -1
					}
					return received.closed.Load()
				}(), dispatches.Load(), requests.Load())
			}
			if phase == "after_first_headers" {
				if grants.Load() != 0 || cloned.Load() != 0 {
					t.Fatal("cancelled initial response began recovery")
				}
			} else {
				stored, _ := p.Store.Get("test")
				if grants.Load() != 1 || cloned.Load() != 1 || stored.AccessToken != "new-access" || stored.RefreshToken != "new-refresh" {
					t.Fatal("completed synthetic rotation was lost or repeated")
				}
			}
			wantCloses := int32(0)
			if phase == "getbody_error" || phase == "getbody_success" {
				wantCloses = 1
			}
			if clone.closed.Load() != wantCloses {
				t.Fatalf("undispatched clone closes=%d want=%d", clone.closed.Load(), wantCloses)
			}
		})
	}
}

func TestHTTPRecoveryLifecycleOrdinaryFailurePreservesOriginal401(t *testing.T) {
	for _, failure := range []string{"grant_rejected", "getbody_error", "getbody_nil", "getbody_nobody", "getbody_missing", "authorizer_missing", "owner_drift"} {
		t.Run(failure, func(t *testing.T) {
			var grants, requests atomic.Int32
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				grants.Add(1)
				if failure == "grant_rejected" {
					w.WriteHeader(503)
					_, _ = io.WriteString(w, `{"error":"temporarily_unavailable"}`)
					return
				}
				_, _ = io.WriteString(w, gatewayKnownOwnerRefreshFixture(`{"access_token":"new-access","refresh_token":"new-refresh"}`, "test-account", "fixture-user"))
			})
			account, _ := p.Store.Get("test")
			request, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://synthetic.invalid/responses", strings.NewReader("exact"))
			clone := &httpRecoveryLifecycleBody{ReadCloser: io.NopCloser(strings.NewReader("exact"))}
			authorize := func(Account) error { return nil }
			switch failure {
			case "getbody_error":
				request.GetBody = func() (io.ReadCloser, error) { return clone, errors.New("clone error") }
			case "getbody_nil":
				request.GetBody = func() (io.ReadCloser, error) { return nil, nil }
			case "getbody_nobody":
				request.GetBody = func() (io.ReadCloser, error) { return http.NoBody, nil }
			case "getbody_missing":
				request.GetBody = nil
			case "authorizer_missing":
				authorize = nil
			case "owner_drift":
				if err := p.Store.UpdateAccount("test", func(a *Account) error {
					a.AccessToken = "other-owner-token"
					a.AccountID = "other-workspace"
					a.Identity = &managedAccountIdentity{WorkspaceID: "other-workspace", UserID: "other-user"}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			body := &httpRecoveryLifecycleBody{ReadCloser: io.NopCloser(strings.NewReader("exact-original-diagnostic"))}
			response, err := p.doAccountRequest(p.Client, request, account, func(_ *http.Client, r *http.Request) (*http.Response, error) {
				requests.Add(1)
				r.Body.Close()
				return &http.Response{StatusCode: 401, Body: body, Header: make(http.Header)}, nil
			}, authorize)
			if err != nil || response == nil || response.StatusCode != 401 || body.closed.Load() != 0 || requests.Load() != 1 {
				t.Fatalf("original diagnostic lost: %v", err)
			}
			raw, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil || string(raw) != "exact-original-diagnostic" || body.closed.Load() != 1 {
				t.Fatal("original diagnostic consumed or leaked")
			}
			if failure == "getbody_error" && clone.closed.Load() != 1 {
				t.Fatal("failed unsubmitted clone leaked")
			}
			want := int32(1)
			if failure == "getbody_missing" || failure == "authorizer_missing" || failure == "owner_drift" {
				want = 0
			}
			if grants.Load() != want {
				t.Fatalf("grants=%d want=%d", grants.Load(), want)
			}
		})
	}
}

func TestHTTPRecoveryLifecycleExplicit401OnlyAndOneRetry(t *testing.T) {
	for _, status := range []int{200, 400, 401, 403, 429, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var grants, attempts atomic.Int32
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				grants.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				_, _ = io.WriteString(w, gatewayKnownOwnerRefreshFixture(`{"access_token":"new-access","refresh_token":"new-refresh"}`, "test-account", "fixture-user"))
			})
			account, _ := p.Store.Get("test")
			request, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://synthetic.invalid/responses", strings.NewReader("exact"))
			var bodies []*httpRecoveryLifecycleBody
			response, err := p.doAccountRequest(p.Client, request, account, func(_ *http.Client, r *http.Request) (*http.Response, error) {
				attempts.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				r.Body.Close()
				b := &httpRecoveryLifecycleBody{ReadCloser: io.NopCloser(strings.NewReader("payload"))}
				bodies = append(bodies, b)
				return &http.Response{StatusCode: status, Body: b}, nil
			}, func(Account) error { return nil })
			if err != nil || response == nil || response.StatusCode != status {
				t.Fatal("unexpected response")
			}
			wantAttempts, wantGrants := int32(1), int32(0)
			if status == 401 {
				wantAttempts, wantGrants = 2, 1
			}
			if attempts.Load() != wantAttempts || grants.Load() != wantGrants {
				t.Fatal("status triggered unexpected recovery/replay")
			}
			for i, b := range bodies {
				want := int32(1)
				if i == len(bodies)-1 {
					want = 0
				}
				if b.closed.Load() != want {
					t.Fatal("response body ownership changed")
				}
			}
			response.Body.Close()
		})
	}
}

// A real redirect rejection is the documented http.Client response+error
// case. Its response body has already been closed by net/http. Keep that
// ownership contract distinct from malformed synthetic RoundTripper returns.
func TestHTTPRecoveryLifecycleRedirectErrorNeverRecovers(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Location", "/other")
		w.WriteHeader(302)
		_, _ = io.WriteString(w, "redirect")
	}))
	defer server.Close()
	client := server.Client()
	rejected := errors.New("synthetic redirect rejected")
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return rejected }
	p := newTestProxy(t, 1)
	account, _ := p.Store.Get("test")
	request, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, nil)
	var authorizations atomic.Int32
	response, err := p.doAccountRequest(client, request, account, doHTTPAccountRequest, func(Account) error { authorizations.Add(1); return nil })
	if !errors.Is(err, rejected) || response == nil || response.StatusCode != 302 || calls.Load() != 1 || authorizations.Load() != 0 {
		t.Fatal("redirect error replayed or lost original error")
	}
	_, readErr := response.Body.Read(make([]byte, 1))
	if readErr == nil {
		t.Fatal("http.Client error response body unexpectedly live")
	}
}
