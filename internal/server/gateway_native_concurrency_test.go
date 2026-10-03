package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"
)

func TestGatewayNativeHTTPAccountControlledConcurrency(t *testing.T) {
	const concurrent = 96
	pool := nativeHTTPFixturePool(t, nil)
	rt := pool.RoundTripper(nil, 10*time.Second)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	type result struct {
		response *http.Response
		err      error
	}
	results := make(chan result, concurrent)
	start := make(chan struct{})
	for range concurrent {
		go func() {
			<-start
			req, _ := http.NewRequestWithContext(ctx, "GET", "http://concurrent.invalid/hold", nil)
			response, err := rt.RoundTrip(req)
			results <- result{response, err}
		}()
	}
	close(start)
	var responses []*http.Response
	defer func() {
		for _, response := range responses {
			_ = response.Body.Close()
		}
	}()
	for range concurrent {
		result := <-results
		if result.err != nil {
			t.Fatalf("admitted request rejected by native transport: %v", result.err)
		}
		responses = append(responses, result.response)
		if _, err := io.ReadFull(result.response.Body, make([]byte, 5)); err != nil {
			t.Fatal(err)
		}
	}
	child := nativeResourceOnlyChild(t, pool)
	pool.mu.Lock()
	active := child.active
	pool.mu.Unlock()
	if active != concurrent {
		t.Fatalf("only %d/%d requests remained simultaneously active", active, concurrent)
	}
	// A full burst must also release all reservations on cancellation.
	cancel()
	for _, response := range responses {
		_ = response.Body.Close()
	}
	pool.mu.Lock()
	active = child.active
	pool.mu.Unlock()
	if active != 0 {
		t.Fatalf("cancelled burst retained %d active requests", active)
	}
	response := nativeHTTPFixtureRequest(t, rt, t.Context(), "GET", "http://concurrent.invalid/echo", "")
	_ = response.Body.Close()
}

func TestGatewayNativeHTTPActiveRoutesExceedIdleBudget(t *testing.T) {
	const routes = 20
	const idleBudget = 3
	pool := nativeHTTPFixturePool(t, func(options *gatewayNativeOptions) { options.idleProcessLimit = idleBudget })
	rt := pool.RoundTripper(nil, time.Second)
	var responses []*http.Response
	for i := range routes {
		response := nativeHTTPFixtureRequest(t, rt, t.Context(), "GET", fmt.Sprintf("http://route-%d.invalid/hold", i), "")
		responses = append(responses, response)
		defer response.Body.Close()
		if _, err := io.ReadFull(response.Body, make([]byte, 5)); err != nil {
			t.Fatal(err)
		}
	}
	pool.mu.Lock()
	count := len(pool.entries)
	pool.mu.Unlock()
	if count != routes {
		t.Fatalf("active route count=%d; want %d", count, routes)
	}
	var workers sync.WaitGroup
	for _, response := range responses {
		workers.Go(func() { _ = response.Body.Close() })
	}
	workers.Wait()
	deadline := time.Now().Add(5 * time.Second)
	for {
		pool.mu.Lock()
		count = len(pool.entries)
		pool.mu.Unlock()
		if count == idleBudget {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("released routes retained %d helpers; want idle budget %d", count, idleBudget)
		}
		time.Sleep(time.Millisecond)
	}
}
