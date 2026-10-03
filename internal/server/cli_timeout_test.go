package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestCLIStreamingOutlivesClientTotalTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < 5; i++ {
			_, _ = io.WriteString(w, "data: chunk\n\n")
			w.(http.Flusher).Flush()
			select {
			case <-time.After(30 * time.Millisecond):
			case <-r.Context().Done():
				return
			}
		}
	}))
	defer server.Close()
	client := server.Client()
	client.Timeout = 40 * time.Millisecond
	req, err := http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := doStreamingRequestWithIdleTimeout(client, req, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil || len(body) != 5*len("data: chunk\n\n") {
		t.Fatalf("active stream interrupted: bytes=%d err=%v", len(body), err)
	}
	if client.Timeout != 40*time.Millisecond {
		t.Fatalf("stream modified shared client timeout: %s", client.Timeout)
	}
}

func TestCLIStreamingIdleClosesAndCancelsUpstream(t *testing.T) {
	upstreamDone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(upstreamDone)
	}))
	defer server.Close()
	req, err := http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := doStreamingRequestWithIdleTimeout(server.Client(), req, 30*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if _, err := io.ReadAll(res.Body); !errors.Is(err, errUpstreamStreamIdleTimeout) {
		t.Fatalf("idle stream error = %v", err)
	}
	select {
	case <-upstreamDone:
	case <-time.After(time.Second):
		t.Fatal("idle timeout left upstream request running")
	}
}

func TestCLIStreamingCancellationInterruptsRead(t *testing.T) {
	upstreamDone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(upstreamDone)
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := doStreamingRequestWithIdleTimeout(server.Client(), req, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	readDone := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(res.Body)
		readDone <- err
	}()
	cancel()
	select {
	case err := <-readDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled read error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation left stream reader running")
	}
	select {
	case <-upstreamDone:
	case <-time.After(time.Second):
		t.Fatal("cancellation left upstream request running")
	}
}

func TestCLIStreamingIdleExcludesDownstreamBackpressure(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	body := &idleStreamBody{body: reader, cancel: cancel, idle: 30 * time.Millisecond}
	defer body.Close()
	writeDone := make(chan error, 1)
	go func() {
		_, err := writer.Write([]byte("chunk"))
		writeDone <- err
	}()
	buf := make([]byte, 5)
	if _, err := io.ReadFull(body, buf); err != nil {
		t.Fatal(err)
	}
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
	// Simulate waiting on the downstream socket, with no upstream Read pending.
	select {
	case <-ctx.Done():
		t.Fatal("completed upstream read left its idle timer active")
	case <-time.After(3 * body.idle):
	}
	go func() {
		_, err := writer.Write([]byte("again"))
		writeDone <- err
	}()
	if _, err := io.ReadFull(body, buf); err != nil || string(buf) != "again" {
		t.Fatalf("stream closed during downstream backpressure: bytes=%q err=%v", buf, err)
	}
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
}

func TestCLIStreamingCloseUnblocksReadAndStopsTimer(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	_, cancel := context.WithCancel(context.Background())
	body := &idleStreamBody{body: reader, cancel: cancel, idle: time.Hour}
	readDone := make(chan error, 1)
	go func() {
		_, err := body.Read(make([]byte, 1))
		readDone <- err
	}()
	var closers sync.WaitGroup
	for i := 0; i < 5; i++ {
		closers.Go(func() { _ = body.Close() })
	}
	closers.Wait()
	select {
	case err := <-readDone:
		if !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("closed read error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close left stream reader running")
	}
}
