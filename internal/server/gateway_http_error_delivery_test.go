package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The origin and downstream are both actual TCP. Incomplete upstream HTTP
// errors must not become a cleanly framed truncated error at the next hop.
func TestHTTPErrorDeliveryTruncationIsVisibleOnWire(t *testing.T) {
	for _, path := range []string{"/responses", "/responses/compact", "/models"} {
		for _, mode := range []string{"short_content_length", "over_limit", "exact_limit_control", "small_control"} {
			t.Run(path+"/"+mode, func(t *testing.T) {
				var requests atomic.Int32
				want := []byte(`{"error":{"message":"synthetic upstream error"}}`)
				if mode == "short_content_length" {
					want = append(want, bytes.Repeat([]byte{' '}, 64<<10)...)
				}
				if mode == "over_limit" {
					want = append(want, bytes.Repeat([]byte{' '}, (2<<20)+17-len(want))...)
				}
				if mode == "exact_limit_control" {
					want = append(want, bytes.Repeat([]byte{' '}, (2<<20)-len(want))...)
				}
				p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					_, _ = io.Copy(io.Discard, r.Body)
					advertised := len(want)
					if mode == "short_content_length" {
						advertised += 500
					}
					w.Header().Set("Content-Length", strconv.Itoa(advertised))
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("X-Codex-Turn-State", "error-state")
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write(want)
				})
				done := make(chan struct{})
				gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { defer close(done); p.ServeHTTP(w, r) }))
				defer gateway.Close()
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				method, requestBody := http.MethodPost, `{"model":"test","input":[],"stream":true}`
				if path == "/models" {
					method, requestBody = http.MethodGet, ""
				}
				request, _ := http.NewRequestWithContext(ctx, method, gateway.URL+path, strings.NewReader(requestBody))
				request.Header.Set("Authorization", "Bearer test-client-key")
				response, err := gateway.Client().Do(request)
				if err != nil {
					t.Fatalf("fixture did not deliver upstream error headers: %v", err)
				}
				body, readErr := io.ReadAll(response.Body)
				response.Body.Close()
				select {
				case <-done:
				case <-ctx.Done():
					t.Fatal("handler did not finish and release")
				}
				if response.StatusCode != 400 || response.Header.Get("X-Codex-Turn-State") != "error-state" || requests.Load() != 1 || p.inFlight("test") != 0 {
					t.Fatal("upstream status/state/admission/replay changed")
				}
				broken := mode == "short_content_length" || mode == "over_limit"
				if broken && readErr == nil {
					t.Fatalf("truncated error became clean EOF: mode=%s delivered=%d upstream=%d", mode, len(body), len(want))
				}
				if !broken && (readErr != nil || !bytes.Equal(body, want)) {
					t.Fatalf("complete error body changed: err=%v bytes=%d want=%d", readErr, len(body), len(want))
				}
				if len(body) > 2<<20 {
					t.Fatal("error forward cap exceeded")
				}
				rec, ok := p.Metrics.LastFor("test")
				if !ok || rec.Status != "error" || rec.HTTPCode != 400 || rec.ErrorSource != "upstream" || rec.ErrorPhase != "http_status" {
					t.Fatalf("original upstream error provenance changed: %+v", rec)
				}
			})
		}
	}
}

type httpBufferedWriteFault struct {
	http.ResponseWriter
	fail  bool
	wrote chan error
}

func (w httpBufferedWriteFault) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w httpBufferedWriteFault) Write(p []byte) (int, error) {
	if w.fail {
		_ = http.NewResponseController(w.ResponseWriter).SetWriteDeadline(time.Now().Add(-time.Second))
	}
	n, err := w.ResponseWriter.Write(p)
	w.wrote <- err
	return n, err
}

// A normal real net/http connection write timeout must be captured by the
// existing gatewayResponseWriter. Ignored return syntax is not itself a bug.
func TestHTTPBufferedDeliveryActualWriteErrorAlreadyRecorded(t *testing.T) {
	for _, path := range []string{"/responses", "/responses/compact", "/models"} {
		for _, fail := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/fail=%v", path, fail), func(t *testing.T) {
				large := strings.Repeat("x", 128<<10)
				result := `{"id":"buffered-result","status":"completed","usage":{"input_tokens":17,"output_tokens":5},"output":[{"type":"reasoning","encrypted_content":"` + large + `"}]}`
				wire := result
				if path == "/responses" {
					wire = "data: {\"type\":\"response.completed\",\"response\":" + result + "}\n\n"
				}
				if path == "/models" {
					wire = `{"models":[],"opaque":"` + large + `"}`
				}
				p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(io.Discard, r.Body)
					_, _ = io.WriteString(w, wire)
				})
				written := make(chan error, 4)
				done := make(chan struct{})
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					defer close(done)
					p.ServeHTTP(httpBufferedWriteFault{w, fail, written}, r)
				}))
				defer server.Close()
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				method := http.MethodPost
				if path == "/models" {
					method = http.MethodGet
				}
				request, _ := http.NewRequestWithContext(ctx, method, server.URL+path, strings.NewReader(`{"model":"test","input":[],"stream":false}`))
				request.Header.Set("Authorization", "Bearer test-client-key")
				response, err := server.Client().Do(request)
				if response != nil {
					_, _ = io.Copy(io.Discard, response.Body)
					response.Body.Close()
				}
				if !fail && err != nil {
					t.Fatal(err)
				}
				select {
				case <-done:
				case <-ctx.Done():
					t.Fatal("buffered handler did not finish")
				}
				select {
				case writeErr := <-written:
					if (writeErr != nil) != fail {
						t.Fatalf("real write fault did not occur: %v", writeErr)
					}
				case <-ctx.Done():
					t.Fatal("no downstream Write observed")
				}
				rec, ok := p.Metrics.LastFor("test")
				if !ok {
					t.Fatal("record missing after handler barrier")
				}
				wantStatus := "ok"
				if fail {
					wantStatus = "aborted"
				}
				if rec.Status != wantStatus || rec.HTTPCode != 200 || (fail && (rec.ErrorSource != "client" || rec.ErrorPhase != "response_write")) {
					t.Fatalf("existing response writer lost delivery result: %+v", rec)
				}
				if path != "/models" && (rec.TerminalStatus != "completed" || rec.InputTokens != 17 || rec.OutputTokens != 5) {
					t.Fatal("write fault erased actual upstream result")
				}
			})
		}
	}
}

type httpErrorDeliveryReader struct {
	read func([]byte) (int, error)
}

func (r httpErrorDeliveryReader) Read(p []byte) (int, error) { return r.read(p) }

type httpErrorDeliveryWriter struct {
	write func([]byte) (int, error)
}

func (w httpErrorDeliveryWriter) Write(p []byte) (int, error) { return w.write(p) }

func TestHTTPErrorDeliveryBoundedCopyStopsAtActualBoundary(t *testing.T) {
	t.Run("empty_reads_bounded", func(t *testing.T) {
		reads := 0
		reader := httpErrorDeliveryReader{func([]byte) (int, error) {
			reads++
			if reads > 100 {
				return 0, errors.New("fixture guard exceeded")
			}
			return 0, nil
		}}
		prefix, err := copyGatewayErrorBody(io.Discard, reader, nil)
		if !errors.Is(err, io.ErrNoProgress) || reads != 100 || len(prefix) != 0 {
			t.Fatalf("empty reader was not bounded: reads=%d err=%v", reads, err)
		}
	})
	t.Run("progress_resets_empty_read_budget", func(t *testing.T) {
		calls := 0
		reader := httpErrorDeliveryReader{func(p []byte) (int, error) {
			calls++
			switch calls {
			case 100:
				p[0] = 'a'
				return 1, nil
			case 200:
				p[0] = 'b'
				return 1, io.EOF
			default:
				return 0, nil
			}
		}}
		var output bytes.Buffer
		prefix, err := copyGatewayErrorBody(&output, reader, nil)
		if err != nil || string(prefix) != "ab" || output.String() != "ab" || calls != 200 {
			t.Fatal("ordinary reader progress was treated as a stall")
		}
	})
	t.Run("bounded_read_and_no_extra_forward", func(t *testing.T) {
		source := bytes.NewReader(bytes.Repeat([]byte{'x'}, gatewayErrorBodyLimit+73))
		var output bytes.Buffer
		_, observer := withGatewayAttemptObservation(t.Context())
		prefix, err := copyGatewayErrorBody(&output, source, observer)
		if !errors.Is(err, errGatewayErrorBodyLimit) || output.Len() != gatewayErrorBodyLimit || len(prefix) != 64<<10 || source.Len() != 72 {
			t.Fatal("forward or read budget exceeded")
		}
		if snapshot := observer.Snapshot(); snapshot.ErrorTruncated == nil || !*snapshot.ErrorTruncated {
			t.Fatal("observed diagnostic truncation missing")
		}
	})
	t.Run("bytes_plus_read_error_preserved", func(t *testing.T) {
		reads := 0
		cause := errors.New("synthetic read failed")
		reader := httpErrorDeliveryReader{func(p []byte) (int, error) { reads++; return copy(p, "partial original error"), cause }}
		var output bytes.Buffer
		prefix, err := copyGatewayErrorBody(&output, reader, nil)
		if !errors.Is(err, cause) || string(prefix) != "partial original error" || output.String() != string(prefix) || reads != 1 {
			t.Fatal("final bytes/read error were lost")
		}
	})
	for _, short := range []bool{false, true} {
		t.Run(fmt.Sprint("write_failure_short=", short), func(t *testing.T) {
			reads, writes := 0, 0
			reader := httpErrorDeliveryReader{func(p []byte) (int, error) {
				reads++
				if reads > 1 {
					return 0, errors.New("unexpected second read")
				}
				return copy(p, "original diagnostic"), nil
			}}
			writer := httpErrorDeliveryWriter{func(p []byte) (int, error) {
				writes++
				if short {
					return len(p) - 1, nil
				}
				return 1, io.ErrClosedPipe
			}}
			prefix, err := copyGatewayErrorBody(writer, reader, nil)
			wantErr := io.ErrClosedPipe
			if short {
				wantErr = io.ErrShortWrite
			}
			if !errors.Is(err, wantErr) || reads != 1 || writes != 1 || string(prefix) != "original diagnostic" {
				t.Fatal("failed writer caused another upstream read or lost diagnostic")
			}
		})
	}
}

// Cancellation of a client reading an error must close the actual origin body,
// stop the copy loop, and release admission even while the origin waits for EOF.
func TestHTTPErrorDeliveryClientCancelClosesWaitingOrigin(t *testing.T) {
	for _, path := range []string{"/responses", "/models"} {
		t.Run(path, func(t *testing.T) {
			originDone := make(chan struct{})
			sent := make(chan struct{})
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				defer close(originDone)
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(400)
				_, _ = w.Write(bytes.Repeat([]byte{'x'}, 64<<10))
				w.(http.Flusher).Flush()
				close(sent)
				<-r.Context().Done()
			})
			done := make(chan struct{})
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { defer close(done); p.ServeHTTP(w, r) }))
			defer gateway.Close()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			method := http.MethodPost
			if path == "/models" {
				method = http.MethodGet
			}
			request, _ := http.NewRequestWithContext(ctx, method, gateway.URL+path, strings.NewReader(`{"model":"test","input":[],"stream":true}`))
			request.Header.Set("Authorization", "Bearer test-client-key")
			response, err := gateway.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			<-sent
			if _, err := io.ReadFull(response.Body, make([]byte, 32<<10)); err != nil {
				t.Fatal(err)
			}
			cancel()
			response.Body.Close()
			for _, ch := range []chan struct{}{done, originDone} {
				select {
				case <-ch:
				case <-time.After(3 * time.Second):
					t.Fatal("canceled error copy retained origin or gateway handler")
				}
			}
			if p.inFlight("test") != 0 {
				t.Fatal("canceled copy leaked admission")
			}
			rec, ok := p.Metrics.LastFor("test")
			if !ok || rec.Status != "error" || rec.HTTPCode != 400 || rec.ErrorSource != "upstream" {
				t.Fatal("client cancel erased original HTTP error")
			}
		})
	}
}
