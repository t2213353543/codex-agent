package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAuthRefreshAuditStrictResponseBoundary(t *testing.T) {
	const valid = `{"access_token":"rotated-access","refresh_token":"rotated-refresh"}`
	for _, test := range []struct {
		name   string
		status int
		body   string
		valid  bool
	}{
		{"created", 201, valid, true},
		{"other success", 299, valid, true},
		{"no content", 204, "", false},
		{"null", 200, "null", false},
		{"empty object", 200, "{}", false},
		{"second object", 200, valid + `{"ignored":"secret"}`, false},
		{"trailing garbage", 200, valid + "secret", false},
		{"truncated", 200, valid[:len(valid)-1], false},
		{"whitespace suffix", 200, valid + " \r\n\t", true},
		{"at limit", 200, valid + strings.Repeat(" ", (2<<20)-len(valid)), true},
		{"oversized suffix", 200, valid + strings.Repeat(" ", (2<<20)-len(valid)+1), false},
		{"oversized token", 200, `{"access_token":"` + strings.Repeat("a", 2<<20) + `"}`, false},
		{"wrong token type", 200, `{"access_token":123,"refresh_token":"rotated"}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			closed := &gatewayNativeTrackedBody{Reader: strings.NewReader(test.body)}
			client := &http.Client{Transport: sourceRefreshRoundTripper(func(r *http.Request) (*http.Response, error) {
				_ = r.Body.Close()
				return &http.Response{StatusCode: test.status, Body: closed}, nil
			})}
			before := Account{AccessToken: "old-access", RefreshToken: "old-refresh", ExpiresAt: 1}
			got, err := refreshAccountWithSettings(t.Context(), client, "http://refresh.invalid/token", before, defaultSettings())
			if (err == nil) != test.valid || closed.closed.Load() != 1 {
				t.Fatalf("valid=%v err=%v closes=%d", test.valid, err, closed.closed.Load())
			}
			if test.valid {
				if got.AccessToken != "rotated-access" || got.RefreshToken != "rotated-refresh" {
					t.Fatal("successful response did not rotate both credentials")
				}
			} else {
				var invalid *gatewayOAuthRefreshError
				if !errors.As(err, &invalid) || invalid.Kind != "invalid" || got.AccessToken != before.AccessToken || got.RefreshToken != before.RefreshToken {
					t.Fatal("invalid response leaked parser details or partially replaced credentials")
				}
			}
		})
	}
}

func TestAuthRefreshAuditExpiryMetadataCannotDiscardRotation(t *testing.T) {
	expiry := time.Now().Add(time.Hour).Unix()
	claims, _ := json.Marshal(map[string]any{"exp": expiry})
	jwt := "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".synthetic"
	for _, access := range []string{"opaque-access", jwt} {
		for _, metadata := range []string{"null", `"3600"`, "true", "{}", "[]", "-1", "0", "1.5", "9223372036854775807", "9223372036854775808", "1e1000"} {
			t.Run(fmt.Sprintf("jwt=%v/expiry=%s", access == jwt, metadata), func(t *testing.T) {
				encoded, _ := json.Marshal(access)
				client := &http.Client{Transport: sourceRefreshRoundTripper(func(r *http.Request) (*http.Response, error) {
					_ = r.Body.Close()
					body := `{"access_token":` + string(encoded) + `,"refresh_token":"rotated-refresh","expires_in":` + metadata + `}`
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
				})}
				got, err := refreshAccountWithSettings(t.Context(), client, "http://refresh.invalid/token", Account{AccessToken: "old", RefreshToken: "old-refresh", ExpiresAt: 12}, defaultSettings())
				wantExpiry := int64(0)
				if access == jwt {
					wantExpiry = expiry
				}
				if err != nil || got.AccessToken != access || got.RefreshToken != "rotated-refresh" || got.ExpiresAt != wantExpiry {
					t.Fatalf("rotation lost or invalid expiry persisted: err=%v expiry=%d want=%d", err, got.ExpiresAt, wantExpiry)
				}
			})
		}
	}
}

func TestAuthRefreshAuditErrorsDoNotExposeEndpointOrTransportSecrets(t *testing.T) {
	for _, endpoint := range []string{"http://secret:password@host.invalid/token", "http://host.invalid/%secret", "file:///secret", "relative-secret", "http://host.invalid/token#secret"} {
		t.Run(endpoint, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: sourceRefreshRoundTripper(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("transport-secret") })}
			_, err := refreshAccountWithSettings(t.Context(), client, endpoint, Account{RefreshToken: "refresh"}, defaultSettings())
			if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "password") || calls != 0 {
				t.Fatalf("invalid endpoint reached transport or leaked credentials: calls=%d err=%v", calls, err)
			}
		})
	}
	client := &http.Client{Transport: sourceRefreshRoundTripper(func(*http.Request) (*http.Response, error) { return nil, errors.New("transport-secret") })}
	for _, candidate := range []*http.Client{nil, client} {
		_, err := refreshAccountWithSettings(t.Context(), candidate, "http://host.invalid/token?secret=hidden", Account{RefreshToken: "refresh"}, defaultSettings())
		if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "hidden") {
			t.Fatalf("unsafe transport failure: %v", err)
		}
	}
}

func TestAuthRefreshAuditDetachedContextIsBounded(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	client := &http.Client{Transport: sourceRefreshRoundTripper(func(r *http.Request) (*http.Response, error) {
		cancel()
		deadline, exists := r.Context().Deadline()
		remaining := time.Until(deadline)
		if r.Context().Err() != nil || !exists || remaining < 40*time.Second || remaining > 45*time.Second {
			t.Error("exchange did not preserve an independent bounded commit budget")
		}
		_ = r.Body.Close()
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"refresh_token":"rotated-after-cancel"}`))}, nil
	})}
	got, err := refreshAccountWithSettings(ctx, client, "http://refresh.invalid/token", Account{AccessToken: "old", RefreshToken: "old-refresh"}, defaultSettings())
	if err != nil || got.RefreshToken != "rotated-after-cancel" || ctx.Err() == nil {
		t.Fatalf("rotation lost after caller cancellation: %v", err)
	}
}

func TestAuthRefreshAuditReplayBodyFailuresPreserveOriginal(t *testing.T) {
	for _, mode := range []string{"missing getbody", "nil body", "empty body", "body and error", "missing authorization", "empty request", "cancel during clone"} {
		t.Run(mode, func(t *testing.T) {
			var refreshes, attempts atomic.Int32
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				refreshes.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				_, _ = io.WriteString(w, gatewayKnownOwnerRefreshFixture(`{"access_token":"rotated","refresh_token":"rotated-refresh"}`, "test-account", "fixture-user"))
			})
			account, err := p.Store.Get("test")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://request.invalid/responses", strings.NewReader("exact-body"))
			defer req.Body.Close()
			clone := &gatewayNativeTrackedBody{Reader: strings.NewReader("exact-body")}
			original := &gatewayNativeTrackedBody{Reader: strings.NewReader("original diagnostic")}
			authorize := func(Account) error { return nil }
			switch mode {
			case "missing getbody":
				req.GetBody = nil
			case "nil body":
				req.GetBody = func() (io.ReadCloser, error) { return nil, nil }
			case "empty body":
				req.GetBody = func() (io.ReadCloser, error) { return http.NoBody, nil }
			case "body and error":
				req.GetBody = func() (io.ReadCloser, error) { return clone, errors.New("clone failure") }
			case "missing authorization":
				authorize = nil
			case "empty request":
				req.Body, req.GetBody, req.ContentLength = http.NoBody, nil, 0
			case "cancel during clone":
				req.GetBody = func() (io.ReadCloser, error) { cancel(); return clone, nil }
			}
			res, err := p.doAccountRequest(p.Client, req, account, func(_ *http.Client, r *http.Request) (*http.Response, error) {
				if attempts.Add(1) == 1 {
					return &http.Response{StatusCode: 401, Body: original}, nil
				}
				if mode != "empty request" {
					t.Error("invalid request body was replayed")
				}
				if r.Body != http.NoBody {
					t.Error("empty replay acquired a body")
				}
				return &http.Response{StatusCode: 200}, nil
			}, authorize)
			if mode == "cancel during clone" {
				if !errors.Is(err, context.Canceled) || res != nil || clone.closed.Load() != 1 || original.closed.Load() != 1 {
					t.Fatal("canceled clone leaked bodies or dispatched replay")
				}
				return
			}
			if err != nil || res == nil {
				t.Fatalf("original response unavailable: %v", err)
			}
			defer res.Body.Close()
			if mode == "empty request" {
				if res.StatusCode != 200 || attempts.Load() != 2 || res.Body != http.NoBody || original.closed.Load() != 1 {
					t.Fatal("empty-body recovery or response normalization failed")
				}
			} else {
				body, _ := io.ReadAll(res.Body)
				if res.StatusCode != 401 || string(body) != "original diagnostic" || attempts.Load() != 1 || original.closed.Load() != 0 {
					t.Fatal("failed replay consumed the retained original response")
				}
			}
			if mode == "body and error" && clone.closed.Load() != 1 {
				t.Fatal("failed clone body was leaked")
			}
			wantRefreshes := int32(1)
			if mode == "missing getbody" || mode == "missing authorization" {
				wantRefreshes = 0
			}
			if refreshes.Load() != wantRefreshes {
				t.Fatalf("unexpected refresh count: %d", refreshes.Load())
			}
		})
	}
}

func TestAuthRefreshAuditMalformedTransportResponse(t *testing.T) {
	for _, response := range []*http.Response{nil, {StatusCode: 200, ContentLength: 1}} {
		p := &Proxy{}
		req, _ := http.NewRequest(http.MethodGet, "http://request.invalid", nil)
		_, err := p.doAccountRequest(&http.Client{}, req, Account{}, func(*http.Client, *http.Request) (*http.Response, error) { return response, nil }, func(Account) error { return nil })
		if err == nil {
			t.Fatal("malformed transport result was accepted")
		}
	}
}

func TestAuthRefreshAuditConcurrentRefreshTokenOnlyRotation(t *testing.T) {
	var refreshes atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if refreshes.Add(1) == 1 {
			close(started)
			<-release
		}
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, `{"refresh_token":"rotated-without-access"}`)
	})
	rejected, err := p.Store.Get("test")
	if err != nil {
		t.Fatal(err)
	}
	var ready, workers sync.WaitGroup
	start := make(chan struct{})
	ready.Add(16)
	for range 16 {
		workers.Go(func() {
			ready.Done()
			<-start
			updated, err := p.recoverUnauthorized(t.Context(), rejected, p.Client)
			if err != nil || updated.AccessToken != rejected.AccessToken || updated.RefreshToken != "rotated-without-access" {
				t.Errorf("concurrent RT-only recovery failed: %v", err)
			}
		})
	}
	ready.Wait()
	close(start)
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("no refresh attempt reached the server")
	}
	close(release)
	workers.Wait()
	stored, err := p.Store.Get(rejected.ID)
	if err != nil || refreshes.Load() != 1 || stored.RefreshToken != "rotated-without-access" || stored.AccessToken != rejected.AccessToken {
		t.Fatalf("stale 401 waiters reused rotated RT: exchanges=%d err=%v", refreshes.Load(), err)
	}
}

type gatewayRefreshAuditErrorReader struct{ err error }

func (r gatewayRefreshAuditErrorReader) Read([]byte) (int, error) { return 0, r.err }

func TestAuthRefreshAuditTruncatedAndCanceledBody(t *testing.T) {
	for _, cause := range []error{io.ErrUnexpectedEOF, context.Canceled, context.DeadlineExceeded} {
		t.Run(cause.Error(), func(t *testing.T) {
			reader := io.MultiReader(strings.NewReader(`{"access_token":"unverified","refresh_token":"unverified-refresh"}`), gatewayRefreshAuditErrorReader{cause})
			body := &gatewayNativeTrackedBody{Reader: reader}
			client := &http.Client{Transport: sourceRefreshRoundTripper(func(r *http.Request) (*http.Response, error) {
				_ = r.Body.Close()
				return &http.Response{StatusCode: 200, Body: body}, nil
			})}
			got, err := refreshAccountWithSettings(t.Context(), client, "http://refresh.invalid/token", Account{AccessToken: "old", RefreshToken: "old-refresh"}, defaultSettings())
			if err == nil || got.AccessToken != "old" || got.RefreshToken != "old-refresh" || body.closed.Load() != 1 {
				t.Fatal("incomplete body was accepted or leaked")
			}
			if cause != io.ErrUnexpectedEOF && (!errors.Is(err, cause) || errors.Is(err, errAccountRefreshNotStarted)) {
				t.Fatal("attempted refresh lost the body cancellation cause")
			}
		})
	}
}
