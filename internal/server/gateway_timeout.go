package server

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"sync"
	"time"
)

const upstreamStreamIdleTimeout = 5 * time.Minute

var errUpstreamStreamIdleTimeout = errors.New("upstream stream idle timeout")

// doStreamingRequest uses the same connection pool and redirect policy as the
// normal client, but does not put a total deadline on an active Codex turn.
func doStreamingRequest(client *http.Client, req *http.Request) (*http.Response, error) {
	return doStreamingRequestWithIdleTimeout(client, req, upstreamStreamIdleTimeout)
}

func doStreamingRequestWithIdleTimeout(client *http.Client, req *http.Request, idle time.Duration) (*http.Response, error) {
	streamClient := *client
	streamClient.Timeout = 0
	ctx, cancel := context.WithCancel(req.Context())
	res, err := streamClient.Do(req.Clone(ctx))
	if err != nil {
		cancel()
		return res, err
	}
	mediaType, _, _ := mime.ParseMediaType(res.Header.Get("Content-Type"))
	// Responses are still parsed as SSE when a successful upstream omits or
	// mislabels its Content-Type. Do not let that bypass the event deadline.
	sse := mediaType == "text/event-stream" || (res.StatusCode >= 200 && res.StatusCode < 300 && req.Header.Get("Accept") == "text/event-stream")
	res.Body = &idleStreamBody{body: res.Body, cancel: cancel, idle: idle, sse: sse}
	return res, nil
}

type idleStreamBody struct {
	body      io.ReadCloser
	cancel    context.CancelFunc
	idle      time.Duration
	closeOnce sync.Once
	closeErr  error
	// Read has one consumer; Close may run concurrently. Only Read accesses
	// these fields, and every fired timer is joined before Read returns.
	timer     *time.Timer
	expired   chan struct{}
	timedOut  bool
	sse       bool
	progress  gatewaySSEIdleProgress
	remaining time.Duration
}

// Read starts the idle timer only while waiting for upstream bytes. Time spent
// sending the previous chunk to a slow downstream client does not count.
func (b *idleStreamBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if b.timedOut {
		return 0, errUpstreamStreamIdleTimeout
	}
	if b.timer == nil {
		b.remaining = b.idle
		b.expired = make(chan struct{}, 1)
		b.timer = time.AfterFunc(b.remaining, func() {
			_ = b.Close()
			b.expired <- struct{}{}
		})
	} else {
		b.timer.Reset(b.remaining)
	}
	start := time.Now()
	n, err := b.body.Read(p)
	if !b.timer.Stop() {
		// Wait for an already-started callback before returning. There are no
		// timer goroutines left behind after a read or its request completes.
		<-b.expired
		b.timedOut = true
		return n, errUpstreamStreamIdleTimeout
	}
	if b.sse {
		// Codex times eventsource().next(), so partial frames and comments
		// must consume the same event budget even when bytes keep arriving.
		b.remaining -= time.Since(start)
		if b.progress.consume(p[:n]) {
			b.remaining = b.idle
		}
	}
	return n, err
}

func (b *idleStreamBody) Close() error {
	b.closeOnce.Do(func() {
		b.cancel()
		b.closeErr = b.body.Close()
	})
	return b.closeErr
}
