package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestGatewayRequestCompressionDoesNotFlowIntoOAuth(t *testing.T) {
	ctx := withGatewayNativeRequestCompression(context.Background())
	_, observer := withGatewayAttemptObservation(ctx)
	ctx = context.WithValue(ctx, gatewayAttemptObservationContextKey{}, observer)
	client := &http.Client{Transport: sourceRefreshRoundTripper(func(r *http.Request) (*http.Response, error) {
		if gatewayNativeRequestCompression(r.Context()) {
			t.Error("Responses compression leaked into refresh context")
		}
		if gatewayAttemptObservationFrom(r.Context()) != observer {
			t.Error("clearing compression erased independent attempt observations")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"access_token":"new","refresh_token":"rotated"}`)), Request: r}, nil
	})}
	_, err := refreshAccountWithSettings(ctx, client, "http://fixture.invalid/oauth/token", Account{AccessToken: "old", RefreshToken: "refresh"}, defaultSettings())
	if err != nil {
		t.Fatal(err)
	}
	if !gatewayNativeRequestCompression(ctx) {
		t.Fatal("refresh altered original request compression choice")
	}
}

func TestGatewayRequestCompressionMissingCapabilityIsLocal(t *testing.T) {
	pool := nativeHTTPFixturePool(t, nil)
	ctx, observer := withGatewayAttemptObservation(withGatewayNativeRequestCompression(t.Context()))
	req, _ := http.NewRequestWithContext(ctx, "POST", "http://fixture.invalid/responses", strings.NewReader(`{}`))
	started := observer.Begin("request", req.URL.Path, time.Now())
	res, err := pool.RoundTripper(nil, time.Second).RoundTrip(req)
	if res != nil {
		_ = res.Body.Close()
	}
	if !errors.Is(err, errGatewayCompressionCapability) || gatewayTransportFailureStatus(err) != 503 {
		t.Fatal("missing native compression was not a local capability failure")
	}
	finishGatewayObservedAttempt(started, res, err)
	rec := &RequestRecord{}
	observeGatewayTransportError(rec, err, "response_headers")
	if rec.ErrorSource != "gateway" || rec.ErrorPhase != "native_compression_capability" || observer.Snapshot().Attempts[0].ErrorSource != "gateway" {
		t.Fatal("missing capability mislabeled as upstream failure")
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	for _, child := range pool.entries {
		if child.active != 0 {
			t.Fatal("missing compression capability leaked helper slot")
		}
	}
}
