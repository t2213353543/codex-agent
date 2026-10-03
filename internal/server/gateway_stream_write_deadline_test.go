package server

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const gatewayStreamDeadlineBudget = 40 * time.Millisecond
const gatewayStreamDeadlinePause = 150 * time.Millisecond

// Delegate to the actual net/http writer. Only map the production 60-second
// deadline to 40ms; no mocked timeout errors or alternate write behavior.
type gatewayStreamDeadlineWriter struct {
	http.ResponseWriter
	deadlines int
}

func (w *gatewayStreamDeadlineWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *gatewayStreamDeadlineWriter) SetWriteDeadline(deadline time.Time) error {
	if !deadline.IsZero() {
		w.deadlines++
		deadline = time.Now().Add(gatewayStreamDeadlineBudget)
	}
	return http.NewResponseController(w.ResponseWriter).SetWriteDeadline(deadline)
}

func gatewayStreamDeadlineServer(t *testing.T, h2 bool, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(handler)
	if h2 {
		server.EnableHTTP2 = true
		server.StartTLS()
	} else {
		server.Start()
	}
	t.Cleanup(server.Close)
	server.Client().Timeout = 3 * time.Second
	return server
}

// Both HTTP hops are actual TCP. Upstream pauses only after the downstream
// client has read the first flushed body bytes, isolating think-time from slow
// downstream consumption. The only acceleration is the controller deadline.
func gatewayStreamDeadlinePipe(t *testing.T, h2, sendSecond, clearDuringRead bool) (string, error, int, error) {
	t.Helper()
	receivedFirst := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "first\n")
		w.(http.Flusher).Flush()
		select {
		case <-receivedFirst:
		case <-r.Context().Done():
			return
		}
		time.Sleep(gatewayStreamDeadlinePause)
		if sendSecond {
			io.WriteString(w, "second\n")
			w.(http.Flusher).Flush()
		}
	}))
	defer upstream.Close()
	type result struct {
		count   int
		pipeErr error
	}
	done := make(chan result, 1)
	server := gatewayStreamDeadlineServer(t, h2, func(w http.ResponseWriter, r *http.Request) {
		res, err := upstream.Client().Get(upstream.URL)
		if err != nil {
			done <- result{pipeErr: err}
			return
		}
		defer res.Body.Close()
		writer := &gatewayStreamDeadlineWriter{ResponseWriter: w}
		controller := http.NewResponseController(writer)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		controller.Flush()
		if clearDuringRead {
			res.Body = &gatewayStreamDeadlineClearReader{ReadCloser: res.Body, controller: controller}
		}
		err = writeStreamPipe(writer, controller, res, nil, true)
		done <- result{writer.deadlines, err}
	})
	response, err := server.Client().Get(server.URL)
	if err != nil {
		close(receivedFirst)
		t.Fatal(err)
	}
	if h2 && response.ProtoMajor != 2 || !h2 && response.ProtoMajor != 1 {
		t.Fatalf("unexpected protocol %s", response.Proto)
	}
	first := make([]byte, len("first\n"))
	_, firstErr := io.ReadFull(response.Body, first)
	close(receivedFirst)
	if firstErr != nil {
		response.Body.Close()
		t.Fatal(firstErr)
	}
	rest, readErr := io.ReadAll(response.Body)
	response.Body.Close()
	state := <-done
	text := string(first) + string(rest)
	t.Logf("protocol=%s second=%v clearBeforeRead=%v actualDeadline=%s upstreamPause=%s body=%q client_error=%v setDeadline_calls=%d pipe_error=%v", response.Proto, sendSecond, clearDuringRead, gatewayStreamDeadlineBudget, gatewayStreamDeadlinePause, text, readErr, state.count, state.pipeErr)
	return text, readErr, state.count, state.pipeErr
}

type gatewayStreamDeadlineClearReader struct {
	io.ReadCloser
	controller *http.ResponseController
}

func (r *gatewayStreamDeadlineClearReader) Read(p []byte) (int, error) {
	r.controller.SetWriteDeadline(time.Time{})
	return r.ReadCloser.Read(p)
}

func TestGatewayStreamWriteDeadlineDoesNotExpireDuringUpstreamRead(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		for _, second := range []bool{true, false} {
			t.Run(fmt.Sprintf("HTTP2=%v/second=%v", h2, second), func(t *testing.T) {
				body, readErr, _, pipeErr := gatewayStreamDeadlinePipe(t, h2, second, false)
				want := "first\n"
				if second {
					want += "second\n"
				}
				if body != want || readErr != nil || pipeErr != nil {
					t.Fatal("upstream think-time inherited the completed downstream write deadline")
				}
			})
		}
	}
}

type gatewayStreamDeadlineSlowWriter struct{ *gatewayStreamDeadlineWriter }

func (w gatewayStreamDeadlineSlowWriter) Write(p []byte) (int, error) {
	time.Sleep(gatewayStreamDeadlinePause)
	return w.ResponseWriter.Write(p)
}
func TestGatewayStreamWriteDeadlineStillLimitsSlowDelivery(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("HTTP2=%v", h2), func(t *testing.T) {
			ended := make(chan error, 1)
			server := gatewayStreamDeadlineServer(t, h2, func(w http.ResponseWriter, r *http.Request) {
				writer := gatewayStreamDeadlineSlowWriter{&gatewayStreamDeadlineWriter{ResponseWriter: w}}
				control := http.NewResponseController(writer)
				w.WriteHeader(200)
				if err := control.Flush(); err != nil {
					ended <- err
					return
				}
				response := &http.Response{Body: io.NopCloser(bytes.NewReader(bytes.Repeat([]byte("x"), 8192)))}
				ended <- writeStreamPipe(writer, control, response, nil, true)
			})
			response, err := server.Client().Get(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			_, readErr := io.ReadAll(response.Body)
			response.Body.Close()
			pipeErr := <-ended
			if pipeErr == nil || readErr == nil {
				t.Fatalf("write budget bypassed: pipe=%v client=%v", pipeErr, readErr)
			}
		})
	}
}
func TestGatewayStreamWriteDeadlineClearedAtTerminalWithoutAnotherRead(t *testing.T) {
	writer := &gatewayStreamDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	body := `data: {"type":"response.completed","response":{"id":"terminal","status":"completed","output":[]}}` + "\n\n"
	scanner := &sseScanner{rec: &RequestRecord{}}
	response := &http.Response{Body: io.NopCloser(io.MultiReader(bytes.NewBufferString(body), errorReader{}))}
	err := writeStreamPipe(writer, http.NewResponseController(writer), response, scanner, true)
	if err != nil || !writer.last.IsZero() || writer.set == 0 {
		t.Fatalf("terminal deadline not cleared: %v", err)
	}
	if writer.Body.String() != body {
		t.Fatal("terminal bytes changed")
	}
}

type gatewayStreamDeadlineRecorder struct {
	*httptest.ResponseRecorder
	last time.Time
	set  int
}

func (w *gatewayStreamDeadlineRecorder) SetWriteDeadline(deadline time.Time) error {
	w.last = deadline
	if !deadline.IsZero() {
		w.set++
	}
	return nil
}
func TestGatewayStreamWriteDeadlineUnsupportedControllerKeepsReadError(t *testing.T) {
	writer := httptest.NewRecorder()
	response := &http.Response{Body: io.NopCloser(errorReader{})}
	err := writeStreamPipe(writer, http.NewResponseController(writer), response, nil, false)
	if err == nil || errors.Is(err, http.ErrNotSupported) {
		t.Fatalf("upstream error lost: %v", err)
	}
}
