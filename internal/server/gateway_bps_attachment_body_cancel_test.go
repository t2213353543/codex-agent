package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAttachmentBoundaryCancelAfterSuccessfulHeadersKeepsHealthyFollower(t *testing.T) {
	for _, scenario := range []string{"partial_json/cancel", "complete_json_waiting_eof/cancel", "partial_json/deadline", "complete_json_waiting_eof/deadline"} {
		t.Run(scenario, func(t *testing.T) {
			stage, cause, _ := strings.Cut(scenario, "/")
			started, leaderTransportCanceled := make(chan struct{}), make(chan struct{})
			var uploads, generations atomic.Int32
			p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/attachments" {
					_, _ = io.Copy(io.Discard, r.Body)
					if uploads.Add(1) == 1 {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusOK)
						if stage == "partial_json" {
							_, _ = io.WriteString(w, `{"openai_file_id":"partial`)
						} else {
							_, _ = io.WriteString(w, `{"openai_file_id":"file_not_confirmed"}`)
						}
						w.(http.Flusher).Flush()
						close(started)
						<-r.Context().Done()
						close(leaderTransportCanceled)
						return
					}
					_, _ = io.WriteString(w, `{"openai_file_id":"file_healthy_follower"}`)
					return
				}
				if r.URL.Path != "/responses" {
					t.Error("unexpected fixture endpoint")
					w.WriteHeader(500)
					return
				}
				generations.Add(1)
				body, _ := io.ReadAll(r.Body)
				if !bytes.Contains(body, []byte("file_healthy_follower")) {
					t.Error("generation used unconfirmed attachment")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, bpsGatewayFrame("response.completed", "healthy-follower", `[]`))
			})
			settings, _ := p.Settings.Get()
			settings.RateLimitCooldownEnabled = false
			if err := p.Settings.Set(settings); err != nil {
				t.Fatal(err)
			}
			body := bytes.Replace(bpsAttachmentTestRequest(bpsAttachmentTestPNG).Body, []byte(`"large":900719925474099312345,`), nil, 1)
			leaderCtx, cancelLeader := context.WithCancel(t.Context())
			if cause == "deadline" {
				cancelLeader()
				leaderCtx, cancelLeader = context.WithTimeout(t.Context(), 500*time.Millisecond)
			}
			defer cancelLeader()
			followerCtx, cancelFollower := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancelFollower()
			leaderRequest := proxyRequest(http.MethodPost, "/v1/responses", string(body)).WithContext(leaderCtx)
			leaderRequest.Header.Set("Session-Id", "attachment-body-cancel-leader")
			followerRequest := proxyRequest(http.MethodPost, "/v1/responses", string(body)).WithContext(followerCtx)
			followerRequest.Header.Set("Session-Id", "attachment-body-healthy-follower")
			leaderResponse, followerResponse := httptest.NewRecorder(), httptest.NewRecorder()
			leaderDone, followerDone := make(chan struct{}), make(chan struct{})
			go func() { defer close(leaderDone); p.gatewayBPS(leaderResponse, leaderRequest) }()
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("upload headers not flushed")
			}
			go func() { defer close(followerDone); p.gatewayBPS(followerResponse, followerRequest) }()
			rootAttachmentWaitForFollower(t)
			if cause == "cancel" {
				cancelLeader()
			}
			select {
			case <-leaderDone:
			case <-time.After(3 * time.Second):
				t.Fatal("canceled leader stayed alive")
			}
			select {
			case <-followerDone:
			case <-time.After(3 * time.Second):
				t.Fatal("healthy follower did not finish")
			}
			select {
			case <-leaderTransportCanceled:
			case <-time.After(time.Second):
				t.Fatal("upstream canceled transport stayed alive")
			}
			if followerCtx.Err() != nil {
				t.Fatal("healthy caller context canceled")
			}
			wantCause := context.Canceled
			if cause == "deadline" {
				wantCause = context.DeadlineExceeded
			}
			if !errors.Is(leaderCtx.Err(), wantCause) {
				t.Fatal("leader did not reach intended cancellation boundary", leaderCtx.Err())
			}
			var reply struct{ Status string }
			_ = json.Unmarshal(followerResponse.Body.Bytes(), &reply)
			t.Logf("stage=%s leader_http=%d follower_http=%d uploads=%d generations=%d healthy_context=%v", stage, leaderResponse.Code, followerResponse.Code, uploads.Load(), generations.Load(), followerCtx.Err())
			if followerResponse.Code != 200 || reply.Status != "completed" || uploads.Load() != 2 || generations.Load() != 1 {
				t.Fatal("successful response headers hid leader cancellation and failed healthy follower")
			}
			if leaderResponse.Code == 200 || bytes.Contains(leaderResponse.Body.Bytes(), []byte("response.completed")) {
				t.Fatal("canceled leader revived")
			}
			p.BPS.attachments.mu.Lock()
			pending, entries := len(p.BPS.attachments.pending), len(p.BPS.attachments.entries)
			p.BPS.attachments.mu.Unlock()
			if pending != 0 || entries != 1 {
				t.Fatal("partial response cached or upload flight leaked")
			}
			p.BPS.attachments.mu.Lock()
			for _, entry := range p.BPS.attachments.entries {
				if entry.fileID != "file_healthy_follower" || entry.expires.Before(time.Now()) || entry.expires.After(time.Now().Add(bpsAttachmentTTL)) {
					t.Error("unconfirmed reference or invalid TTL cached")
				}
			}
			p.BPS.attachments.mu.Unlock()
		})
	}
}

func TestAttachmentBoundaryMalformedResponseWithoutCancellationStaysFailure(t *testing.T) {
	for _, reply := range []string{`{"openai_file_id":"truncated`, `{"openai_file_id":"file_untrusted"}{}`, `not-json`} {
		t.Run(reply, func(t *testing.T) {
			var calls atomic.Int32
			p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				_, _ = io.WriteString(w, reply)
			})
			account, settings := bpsAttachmentFixtureAccount(t, p)
			request := bpsAttachmentTestRequest(bpsAttachmentTestPNG)
			original := bytes.Clone(request.Body)
			err := p.prepareBPSAttachments(t.Context(), account, nil, "synthetic-tenant", request, settings)
			var invalid *bpsAttachmentError
			if !errors.As(err, &invalid) || invalid.StatusCode != 502 || invalid.Stage != "response" || t.Context().Err() != nil {
				t.Fatal("real malformed response relabeled as cancellation", err)
			}
			if calls.Load() != 1 || !bytes.Equal(request.Body, original) || len(p.BPS.attachments.entries) != 0 || len(p.BPS.attachments.pending) != 0 {
				t.Fatal("malformed response retried, cached, leaked or partially rewrote request")
			}
		})
	}
}

type attachmentBodyReadBarrier struct {
	io.ReadCloser
	started chan<- struct{}
}

func (b *attachmentBodyReadBarrier) Read(dst []byte) (int, error) {
	select {
	case b.started <- struct{}{}:
	default:
	}
	return b.ReadCloser.Read(dst)
}

type attachmentBodyReadTransport struct {
	base    http.RoundTripper
	started chan<- struct{}
}

func (tr attachmentBodyReadTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	response, err := tr.base.RoundTrip(req)
	if err == nil && response.StatusCode == http.StatusOK {
		response.Body = &attachmentBodyReadBarrier{ReadCloser: response.Body, started: tr.started}
	}
	return response, err
}

func TestAttachmentBoundaryBodyReadKeepsAdmissionCancellationCause(t *testing.T) {
	started := make(chan struct{}, 1)
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, `{"openai_file_id":"unfinished`)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	// Server Flush alone does not prove the client accepted response headers.
	// Signal only when upload's JSON decoder reads the real HTTP response body;
	// cancellation can no longer race the header-await transport boundary.
	p.Client.Transport = attachmentBodyReadTransport{base: p.Client.Transport, started: started}
	account, settings := bpsAttachmentFixtureAccount(t, p)
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(context.Canceled)
	result := make(chan error, 1)
	go func() {
		result <- p.prepareBPSAttachments(ctx, account, nil, "synthetic", bpsAttachmentTestRequest(bpsAttachmentTestPNG), settings)
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("body read not reached")
	}
	cause := &gatewaySharedAdmissionError{phase: "lease lost"}
	cancel(cause)
	select {
	case err := <-result:
		var invalid *bpsAttachmentError
		if !errors.Is(err, context.Canceled) || errors.As(err, &invalid) || context.Cause(ctx) != cause {
			t.Fatal("admission cancellation became malformed upload response", err)
		}
	case <-time.After(time.Second):
		t.Fatal("body cancellation did not stop upload")
	}
	if len(p.BPS.attachments.pending) != 0 || len(p.BPS.attachments.entries) != 0 {
		t.Fatal("canceled admission cached partial file or leaked flight")
	}
}
