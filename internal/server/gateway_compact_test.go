package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSourceCompactAllowsSlowHeaders(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(80 * time.Millisecond)
		_, _ = io.WriteString(w, `{"output":[]}`)
	}))
	defer upstream.Close()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 10 * time.Millisecond
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 20 * time.Millisecond}
	req, _ := http.NewRequest(http.MethodPost, upstream.URL, nil)
	res, err := doCompactRequestWithTimeout(client, req, time.Second)
	if err != nil {
		t.Fatalf("compaction inherited the shorter normal request timeout: %v", err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil || string(body) != `{"output":[]}` {
		t.Fatalf("invalid compaction result: body=%q err=%v", body, err)
	}
	if client.Timeout != 20*time.Millisecond || transport.ResponseHeaderTimeout != 10*time.Millisecond {
		t.Fatal("compaction changed the shared client or transport")
	}
}

func TestSourceCompactTimeoutIncludesBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer upstream.Close()
	req, _ := http.NewRequest(http.MethodPost, upstream.URL, nil)
	res, err := doCompactRequestWithTimeout(upstream.Client(), req, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	_, err = io.ReadAll(res.Body)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("body stalled without the compaction deadline: %v", err)
	}
}

func TestSourceCompactClientCancellation(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(canceled)
	}))
	defer upstream.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, upstream.URL, nil)
	done := make(chan error, 1)
	go func() {
		res, err := doCompactRequestWithTimeout(upstream.Client(), req, time.Minute)
		if res != nil {
			_ = res.Body.Close()
		}
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("compaction did not reach upstream")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("compaction ignored client cancellation: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("compaction waited for its own deadline after client cancellation")
	}
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream request was not canceled")
	}
}
