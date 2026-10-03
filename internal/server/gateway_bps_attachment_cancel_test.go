package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Observe the real blocked singleflight call instead of relying on a sleep to
// assume the follower joined. This does not instrument production code.
func rootAttachmentWaitForFollower(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		stack := make([]byte, 1<<20)
		stack = stack[:runtime.Stack(stack, true)]
		for _, goroutine := range bytes.Split(stack, []byte("\n\n")) {
			lines := bytes.Split(goroutine, []byte{'\n'})
			if len(lines) > 1 && bytes.Contains(lines[0], []byte("[select]")) && bytes.Contains(lines[1], []byte("(*bpsAttachmentCache).file")) {
				return
			}
		}
		runtime.Gosched()
		time.Sleep(time.Millisecond)
	}
	t.Fatal("healthy gateway follower did not join the actual upload flight")
}

func TestBPSAttachmentCanceledLeaderDoesNotFailHealthyGatewayFollower(t *testing.T) {
	started, leaderTransportCanceled := make(chan struct{}), make(chan struct{})
	var uploads, generations, active atomic.Int32
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/attachments" {
			active.Add(1)
			defer active.Add(-1)
			_, _ = io.Copy(io.Discard, r.Body)
			if uploads.Add(1) == 1 {
				close(started)
				<-r.Context().Done()
				close(leaderTransportCanceled)
				return
			}
			_, _ = io.WriteString(w, `{"openai_file_id":"file_healthy_follower"}`)
			return
		}
		if r.URL.Path != "/responses" {
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			w.WriteHeader(400)
			return
		}
		generations.Add(1)
		body, _ := io.ReadAll(r.Body)
		if !bytes.Contains(body, []byte("file_healthy_follower")) {
			t.Error("follower generation did not use its successful upload")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, bpsGatewayFrame("response.completed", "healthy-follower", `[]`))
	})
	settings, _ := p.Settings.Get()
	settings.RateLimitCooldownEnabled = false
	if err := p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
	body := bpsAttachmentTestRequest(bpsAttachmentTestPNG).Body
	// fixture model maps locally; no live model or account data is involved.
	body = bytes.Replace(body, []byte(`"large":900719925474099312345,`), nil, 1)
	leaderCtx, cancelLeader := context.WithCancel(t.Context())
	defer cancelLeader()
	followerCtx, cancelFollower := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelFollower()
	leaderRequest := proxyRequest(http.MethodPost, "/v1/responses", string(body)).WithContext(leaderCtx)
	leaderRequest.Header.Set("Session-Id", "cancel-leader-session")
	followerRequest := proxyRequest(http.MethodPost, "/v1/responses", string(body)).WithContext(followerCtx)
	followerRequest.Header.Set("Session-Id", "healthy-follower-session")
	leaderResponse, followerResponse := httptest.NewRecorder(), httptest.NewRecorder()
	leaderDone, followerDone := make(chan struct{}), make(chan struct{})
	go func() { defer close(leaderDone); p.gatewayBPS(leaderResponse, leaderRequest) }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("leader upload did not start")
	}
	go func() { defer close(followerDone); p.gatewayBPS(followerResponse, followerRequest) }()
	rootAttachmentWaitForFollower(t)
	cancelLeader()
	select {
	case <-leaderDone:
	case <-time.After(3 * time.Second):
		t.Fatal("canceled gateway request did not exit")
	}
	select {
	case <-followerDone:
	case <-time.After(3 * time.Second):
		t.Fatal("healthy follower did not finish")
	}
	select {
	case <-leaderTransportCanceled:
	case <-time.After(time.Second):
		t.Fatal("canceled upload transport was left alive")
	}
	if followerCtx.Err() != nil {
		t.Fatal("fixture healthy caller canceled")
	}
	var reply struct{ Status string }
	_ = json.Unmarshal(followerResponse.Body.Bytes(), &reply)
	t.Logf("leader_http=%d follower_http=%d follower_context=%v uploads=%d generations=%d active_uploads=%d follower=%s", leaderResponse.Code, followerResponse.Code, followerCtx.Err(), uploads.Load(), generations.Load(), active.Load(), followerResponse.Body.String())
	if followerResponse.Code != 200 || reply.Status != "completed" || uploads.Load() != 2 || generations.Load() != 1 {
		t.Fatal("healthy caller inherited another request's cancellation instead of obtaining one replacement flight")
	}
	if leaderResponse.Code == 200 || bytes.Contains(leaderResponse.Body.Bytes(), []byte("response.completed")) {
		t.Fatal("canceled caller was revived or sent successful output")
	}
	p.BPS.attachments.mu.Lock()
	pending, entries := len(p.BPS.attachments.pending), len(p.BPS.attachments.entries)
	p.BPS.attachments.mu.Unlock()
	if pending != 0 || entries != 1 || active.Load() != 0 || p.inFlight("test") != 0 {
		t.Fatal("replacement upload leaked pending work, duplicate cache entries, or account slots")
	}
}

type rootAttachmentJoinContext struct {
	context.Context
	once   sync.Once
	joined chan struct{}
}

func (c *rootAttachmentJoinContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.joined) })
	return c.Context.Done()
}

func TestBPSAttachmentHTTPFailuresDoNotRetryHealthyWaiter(t *testing.T) {
	for _, status := range []int{401, 429, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			cache := &bpsAttachmentCache{}
			entered, release := make(chan struct{}), make(chan struct{})
			var uploads atomic.Int32
			errUpload := &bpsAttachmentError{Stage: "upload", StatusCode: status}
			upload := func() (string, error) { uploads.Add(1); close(entered); <-release; return "", errUpload }
			leader, follower := make(chan error, 1), make(chan error, 1)
			go func() { _, err := cache.file(t.Context(), "same-image", upload); leader <- err }()
			<-entered
			joined := &rootAttachmentJoinContext{Context: t.Context(), joined: make(chan struct{})}
			go func() {
				_, err := cache.file(joined, "same-image", func() (string, error) { uploads.Add(1); return "unexpected", nil })
				follower <- err
			}()
			<-joined.joined
			close(release)
			if !errors.Is(<-leader, errUpload) || !errors.Is(<-follower, errUpload) || uploads.Load() != 1 {
				t.Fatal("actual HTTP failure was retried or lost")
			}
		})
	}
}

func TestBPSAttachmentHealthyWaiterRecoveryIsBounded(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(cause.Error(), func(t *testing.T) {
			cache := &bpsAttachmentCache{}
			flight := &bpsAttachmentFlight{done: make(chan struct{}), err: cause}
			cache.pending = map[string]*bpsAttachmentFlight{"fixture": flight}
			joined := &rootAttachmentJoinContext{Context: t.Context(), joined: make(chan struct{})}
			var uploads atomic.Int32
			result := make(chan error, 1)
			go func() {
				_, err := cache.file(joined, "fixture", func() (string, error) { uploads.Add(1); return "", context.Canceled })
				result <- err
			}()
			<-joined.joined
			cache.mu.Lock()
			delete(cache.pending, "fixture")
			close(flight.done)
			cache.mu.Unlock()
			if err := <-result; !errors.Is(err, context.Canceled) || uploads.Load() != 1 {
				t.Fatalf("recovery did not stop after its one own failed POST: uploads=%d err=%v", uploads.Load(), err)
			}
			cache.mu.Lock()
			pending := len(cache.pending)
			cache.mu.Unlock()
			if pending != 0 {
				t.Fatal("failed replacement left pending flight")
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var uploads atomic.Int32
	_, err := (&bpsAttachmentCache{}).file(ctx, "fixture", func() (string, error) { uploads.Add(1); return "unexpected", nil })
	if !errors.Is(err, context.Canceled) || uploads.Load() != 0 {
		t.Fatal("already-canceled caller dispatched upload")
	}
}

func TestBPSAttachmentPartialCacheSurvivesDifferentFlightCancellation(t *testing.T) {
	cache := &bpsAttachmentCache{}
	if id, err := cache.file(t.Context(), "owner:image-one", func() (string, error) { return "file_one", nil }); err != nil || id != "file_one" {
		t.Fatal(err)
	}
	flight := &bpsAttachmentFlight{done: make(chan struct{}), err: context.Canceled}
	cache.pending["owner:image-two"] = flight
	joined := &rootAttachmentJoinContext{Context: t.Context(), joined: make(chan struct{})}
	result := make(chan string, 1)
	var uploads atomic.Int32
	go func() {
		id, err := cache.file(joined, "owner:image-two", func() (string, error) { uploads.Add(1); return "file_two", nil })
		if err != nil {
			id = "error:" + err.Error()
		}
		result <- id
	}()
	<-joined.joined
	cache.mu.Lock()
	delete(cache.pending, "owner:image-two")
	close(flight.done)
	cache.mu.Unlock()
	if got := <-result; got != "file_two" {
		t.Fatalf("healthy second image failed: %s", got)
	}
	if got, err := cache.file(t.Context(), "owner:image-one", func() (string, error) { uploads.Add(1); return "unexpected", nil }); err != nil || got != "file_one" || uploads.Load() != 1 {
		t.Fatal("successful first image cache lost or reuploaded")
	}
}
