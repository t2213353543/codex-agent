package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func TestGatewayRecoveryAuthorizationPrecedesBodyCloneAndRetry(t *testing.T) {
	for _, denial := range []string{"revoked", "storage", "cancelled", "allowed"} {
		t.Run(denial, func(t *testing.T) {
			p := newTestProxy(t, 1)
			account, err := p.Store.Get("test")
			if err != nil {
				t.Fatal(err)
			}
			var refreshes, attempts, clones, authorizations atomic.Int32
			client := &http.Client{Transport: sourceRefreshRoundTripper(func(req *http.Request) (*http.Response, error) {
				refreshes.Add(1)
				_ = req.Body.Close()
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header),
					Body: io.NopCloser(strings.NewReader(gatewayKnownOwnerRefreshFixture(gatewayRecoveryAuthorizationTokens, account.AccountID, account.Identity.UserID))), Request: req}, nil
			})}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://fixture.invalid/responses", strings.NewReader("retained-original-body"))
			if err != nil {
				t.Fatal(err)
			}
			defer request.Body.Close()
			request.GetBody = func() (io.ReadCloser, error) {
				clones.Add(1)
				return io.NopCloser(strings.NewReader("retained-original-body")), nil
			}
			original := &gatewayNativeTrackedBody{Reader: strings.NewReader("synthetic rejected response")}
			var expected error
			switch denial {
			case "revoked":
				expected = &gatewayRecoveryAuthorizationError{status: http.StatusForbidden}
			case "storage":
				expected = &gatewayRecoveryAuthorizationError{status: http.StatusServiceUnavailable}
			case "cancelled":
				expected = context.Canceled
			}
			response, err := p.doAccountRequest(client, request, account,
				func(_ *http.Client, outgoing *http.Request) (*http.Response, error) {
					if attempts.Add(1) == 1 {
						_ = outgoing.Body.Close()
						return &http.Response{StatusCode: http.StatusUnauthorized, Header: make(http.Header), Body: original, Request: outgoing}, nil
					}
					body, readErr := io.ReadAll(outgoing.Body)
					_ = outgoing.Body.Close()
					if readErr != nil || string(body) != "retained-original-body" || outgoing.Header.Get("Authorization") != "Bearer synthetic-recovery-access" {
						t.Error("permitted retry lost exact body or recovered credentials")
					}
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: http.NoBody, Request: outgoing}, nil
				}, func(recovered Account) error {
					authorizations.Add(1)
					if recovered.AccessToken != "synthetic-recovery-access" {
						t.Error("authorization did not receive the recovered identity")
					}
					if denial == "cancelled" {
						cancel()
					}
					return expected
				})
			if response != nil {
				defer response.Body.Close()
			}
			if !errors.Is(err, expected) || refreshes.Load() != 1 || authorizations.Load() != 1 || original.closed.Load() != 1 {
				t.Fatal("retry authorization or original-body lifecycle changed")
			}
			if expected != nil {
				if response != nil || attempts.Load() != 1 || clones.Load() != 0 {
					t.Fatal("denied retry cloned request body or dispatched again")
				}
			} else if response == nil || response.StatusCode != http.StatusOK || attempts.Load() != 2 || clones.Load() != 1 {
				t.Fatal("permitted retry did not execute exactly once after authorization")
			}
			gatewayRecoveryAuthorizationAssertStored(t, p, account)
		})
	}
}
