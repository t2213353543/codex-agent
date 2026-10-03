package server

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestGatewayRefreshPreflightRejectsCancelledExchange(t *testing.T) {
	for _, expired := range []bool{false, true} {
		name := "cancelled"
		if expired {
			name = "deadline"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			want := error(context.Canceled)
			if expired {
				cancel()
				ctx, cancel = context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
				want = context.DeadlineExceeded
			}
			defer cancel()
			cancel()
			guard := &gatewaySelectionPGRejectTransport{}
			account := Account{AccessToken: "synthetic-preflight-access", RefreshToken: "synthetic-preflight-refresh"}
			updated, err := refreshAccountWithSettings(ctx, &http.Client{Transport: guard}, "http://preflight.invalid/oauth/token", account, defaultSettings())
			if !errors.Is(err, want) || !errors.Is(err, errAccountRefreshNotStarted) || guard.calls.Load() != 0 || updated.AccessToken != account.AccessToken || updated.RefreshToken != account.RefreshToken {
				t.Fatal("expired caller reached OAuth or lost the pre-exchange cancellation marker")
			}
		})
	}
}

func TestGatewayRefreshPreflightDoesNotClassifyExchangeFailure(t *testing.T) {
	account := Account{AccessToken: "synthetic-preflight-access", RefreshToken: "synthetic-preflight-refresh"}
	client := &http.Client{Transport: sourceRefreshRoundTripper(func(request *http.Request) (*http.Response, error) {
		if request.Body != nil {
			_ = request.Body.Close()
		}
		return nil, context.DeadlineExceeded
	})}
	_, err := refreshAccountWithSettings(t.Context(), client, "http://preflight.invalid/oauth/token", account, defaultSettings())
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errAccountRefreshNotStarted) {
		t.Fatal("an attempted exchange timeout was incorrectly classified as not started")
	}
}
