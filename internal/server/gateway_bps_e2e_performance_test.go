package server

// Opt-in loopback experiment, not a live-provider benchmark. Both revisions must
// run this exact file with identical environment and without competing load.
import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

var bpsPerfEpoch = time.Now()
var bpsPerfNow = func() int64 { return time.Since(bpsPerfEpoch).Nanoseconds() }
var bpsPerfClock = "Go monotonic clock"

func bpsPerfMillisecondsSince(start int64) float64 {
	return float64(bpsPerfNow()-start) / float64(time.Millisecond)
}

func TestBPSPerformanceClockResolution(t *testing.T) {
	if os.Getenv("GATEWAY_PERF_CLOCK_CHECK") != "1" {
		t.Skip("opt-in performance clock check")
	}
	previous, minimum := bpsPerfNow(), int64(time.Hour)
	positive := 0
	for i := 0; i < 1000; i++ {
		now := bpsPerfNow()
		if now < previous {
			t.Fatal("performance clock moved backwards")
		}
		if elapsed := now - previous; elapsed > 0 {
			positive++
			minimum = min(minimum, elapsed)
		}
		previous = now
	}
	if positive == 0 || minimum > int64(time.Microsecond) {
		t.Fatalf("insufficient clock resolution: positive=%d min=%d ns", positive, minimum)
	}
	t.Logf("%s: %d/1000 positive intervals; minimum=%d ns", bpsPerfClock, positive, minimum)
}

type bpsPerfScenario struct {
	Name            string `json:"name"`
	Transport       string `json:"transport"`
	InputBytes      int    `json:"input_bytes"`
	DeltaFrames     int    `json:"delta_frames"`
	DeltaBytes      int    `json:"delta_bytes"`
	Concurrency     int    `json:"concurrency"`
	History         bool   `json:"history"`
	FirstDelayMS    int    `json:"upstream_first_delay_ms"`
	TerminalDelayMS int    `json:"upstream_terminal_delay_ms"`
}

type bpsPerfSample struct {
	Worker        int     `json:"worker"`
	Iteration     int     `json:"iteration"`
	TTFTMS        float64 `json:"ttft_ms,omitempty"`
	TotalMS       float64 `json:"total_ms"`
	ResponseBytes int     `json:"response_bytes"`
	DeltaFrames   int     `json:"delta_frames"`
}

type bpsPerfRun struct {
	Scenario       bpsPerfScenario `json:"scenario"`
	Samples        []bpsPerfSample `json:"samples"`
	MeasuredWallMS float64         `json:"measured_wall_ms"`
	AllocBytes     uint64          `json:"alloc_bytes"`
	Mallocs        uint64          `json:"mallocs"`
	GCCycles       uint32          `json:"gc_cycles"`
}

func bpsPerfPositiveEnv(t *testing.T, name string, fallback int) int {
	t.Helper()
	if value := os.Getenv(name); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 {
			t.Fatalf("%s must be a positive integer", name)
		}
		return n
	}
	return fallback
}

func TestBPSGatewayE2EPerformance(t *testing.T) {
	if os.Getenv("GATEWAY_PERF_OUTPUT") == "" {
		t.Skip("set GATEWAY_PERF_OUTPUT to export measured, validated loopback samples")
	}
	n := bpsPerfPositiveEnv(t, "GATEWAY_PERF_SAMPLES", 100)
	warmup := bpsPerfPositiveEnv(t, "GATEWAY_PERF_WARMUP", 5)
	scenarios := []bpsPerfScenario{
		{"http_sse_short", "sse", 64, 32, 64, 1, false, 0, 0},
		{"http_sse_input256k", "sse", 256 << 10, 32, 64, 1, false, 0, 0},
		{"http_sse_256frames", "sse", 64, 256, 256, 1, false, 0, 0},
		{"http_json_input256k", "json", 256 << 10, 64, 1024, 1, false, 0, 0},
		{"http_compact_short", "compact", 64, 0, 0, 1, false, 0, 0},
		{"http_compact_input256k", "compact", 256 << 10, 0, 0, 1, false, 0, 0},
		{"http_models", "models", 0, 0, 0, 1, false, 0, 0},
		{"ws_short", "ws", 64, 32, 64, 1, false, 0, 0},
		{"ws_input256k", "ws", 256 << 10, 32, 64, 1, false, 0, 0},
		{"ws_256frames", "ws", 64, 256, 256, 1, false, 0, 0},
		{"ws_history256k", "ws", 256 << 10, 32, 64, 1, true, 0, 0},
		{"http_sse_concurrent4", "sse", 64, 32, 64, 4, false, 0, 0},
		{"ws_concurrent4", "ws", 64, 32, 64, 4, false, 0, 0},
		{"http_compact_trigger_sse_input256k", "compact_sse", 256 << 10, 0, 0, 1, false, 0, 0},
		{"ws_compact_input256k", "ws_compact", 256 << 10, 0, 0, 1, false, 0, 0},
		{"ws_warmup_input256k", "ws_warmup", 256 << 10, 0, 0, 1, false, 0, 0},
	}
	scenarios = append(scenarios, bpsPerfScenario{"http_sse_input256k_delay20ms", "sse", 256 << 10, 32, 64, 1, false, 20, 20})
	var runs []bpsPerfRun
	for _, scenario := range scenarios {
		if filter := os.Getenv("GATEWAY_PERF_FILTER"); filter != "" && !strings.Contains(scenario.Name, filter) {
			continue
		}
		t.Run(scenario.Name, func(t *testing.T) {
			runs = append(runs, bpsPerfMeasure(t, scenario, n, warmup))
		})
	}
	if t.Failed() {
		t.Fatal("performance run failed correctness checks; refusing to publish samples")
	}
	report := struct {
		Schema     int          `json:"schema"`
		Label      string       `json:"label"`
		Timestamp  string       `json:"timestamp"`
		GoVersion  string       `json:"go_version"`
		OS         string       `json:"os"`
		Arch       string       `json:"arch"`
		GOMAXPROCS int          `json:"gomaxprocs"`
		Clock      string       `json:"clock"`
		Samples    int          `json:"samples_per_worker"`
		Warmup     int          `json:"warmup_per_worker"`
		Method     string       `json:"method"`
		Runs       []bpsPerfRun `json:"runs"`
	}{1, os.Getenv("GATEWAY_PERF_LABEL"), time.Now().UTC().Format(time.RFC3339), runtime.Version(), runtime.GOOS, runtime.GOARCH, runtime.GOMAXPROCS(0), bpsPerfClock, n, warmup,
		"Synthetic upstream (zero delay except named delay20ms scenario: 20ms before first delta plus 20ms before terminal) and client over real loopback TCP; temporary local JSON stores/metrics, in-memory history, no Redis/Postgres/provider network. HTTP keepalive and persistent WS connections. TTFT is first nonempty output_text delta (JSON: complete answer availability); compact/models omit TTFT. Total ends at validated completed event / full JSON body. Payload generation, dialing, warmup and history seeding excluded. Samples include client response JSON validation cost. Allocation deltas cover entire process during concurrent measurement.", runs}
	raw, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	output := os.Getenv("GATEWAY_PERF_OUTPUT")
	if err := os.MkdirAll(filepath.Dir(output), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(output, append(raw, '\n'), 0644); err != nil {
		t.Fatal(err)
	}
	t.Logf("exported %d scenarios to %s", len(runs), output)
}

func bpsPerfMeasure(t *testing.T, scenario bpsPerfScenario, n, warmup int) bpsPerfRun {
	t.Helper()
	input := strings.Repeat("x", scenario.InputBytes)
	var outputText strings.Builder
	frames := make([]string, scenario.DeltaFrames)
	for i := range frames {
		delta := fmt.Sprintf("%06d:", i) + strings.Repeat("d", scenario.DeltaBytes-7)
		outputText.WriteString(delta)
		frames[i] = `data: {"type":"response.output_text.delta","item_id":"msg_perf","delta":"` + delta + `"}` + "\n\n"
	}
	expected := outputText.String()
	encodedText, _ := json.Marshal(expected)
	output := `[{"id":"msg_perf","type":"message","role":"assistant","content":[{"type":"output_text","text":` + string(encodedText) + `}]}]`
	var sequence atomic.Uint64
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil || (len(input) > 0 && !bytes.Contains(body, []byte(input))) {
			t.Errorf("upstream input integrity failure: err=%v bytes=%d", err, len(body))
			http.Error(w, "input integrity failure", 400)
			return
		}
		id := fmt.Sprintf("resp_perf_%d", sequence.Add(1))
		switch r.URL.Path {
		case "/responses/access":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"allowed":true,"model_catalog":{"default_model":"gpt-6-astra","models":[{"id":"gpt-6-astra","label":"Performance fixture","efforts":[{"value":"medium","label":"Medium"}],"default_effort":"medium"}]}}`)
		case "/responses/compact":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"`+id+`","object":"response.compaction","output":[{"type":"compaction","encrypted_content":"perf-opaque-state"}],"usage":{"input_tokens":12,"output_tokens":3}}`)
		case "/responses":
			if scenario.History && bytes.Contains(body, []byte("perf-next-turn")) && !bytes.Contains(body, encodedText) {
				t.Error("incremental request did not preserve prior assistant output")
				http.Error(w, "history integrity failure", 400)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			time.Sleep(time.Duration(scenario.FirstDelayMS) * time.Millisecond)
			for _, frame := range frames {
				_, _ = io.WriteString(w, frame)
				if err := http.NewResponseController(w).Flush(); err != nil {
					return
				}
			}
			time.Sleep(time.Duration(scenario.TerminalDelayMS) * time.Millisecond)
			_, _ = io.WriteString(w, bpsGatewayFrame("response.completed", id, output))
		default:
			t.Errorf("unexpected upstream route %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	server := httptest.NewServer(p)
	defer server.Close()
	type worker struct {
		client  *http.Client
		conn    *websocket.Conn
		body    []byte
		session string
	}
	workers := make([]worker, scenario.Concurrency)
	for i := range workers {
		worker := &workers[i]
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.MaxIdleConnsPerHost = scenario.Concurrency + 1
		worker.client = &http.Client{Transport: transport, Timeout: 30 * time.Second}
		defer transport.CloseIdleConnections()
		worker.session = fmt.Sprintf("perf-%s-worker-%d", scenario.Name, i)
		fields := map[string]any{"model": "gpt-6-astra", "stream": scenario.Transport != "json", "input": []any{map[string]any{"role": "user", "content": input}}}
		if scenario.Transport == "compact_sse" || scenario.Transport == "ws_compact" {
			fields["input"] = append(fields["input"].([]any), map[string]any{"type": "compaction_trigger"})
		}
		if scenario.Transport == "ws_warmup" {
			fields["generate"] = false
		}
		if strings.HasPrefix(scenario.Transport, "ws") {
			var err error
			worker.conn, _, err = websocket.Dial(t.Context(), server.URL+"/v1/responses", &websocket.DialOptions{HTTPClient: worker.client, HTTPHeader: http.Header{"Authorization": {"Bearer test-client-key"}, "Session-Id": {worker.session}}, CompressionMode: websocket.CompressionDisabled})
			if err != nil {
				t.Fatal(err)
			}
			defer worker.conn.CloseNow()
			worker.conn.SetReadLimit(4 << 20)
			fields["type"], fields["stream_id"] = "response.create", "perf_lane"
		}
		worker.body, _ = json.Marshal(fields)
		if scenario.History {
			_, id, err := bpsPerfWS(t.Context(), worker.conn, worker.body, expected, scenario.DeltaFrames, scenario.Transport)
			if err != nil {
				t.Fatal(err)
			}
			fields["previous_response_id"] = id
			fields["input"] = []any{map[string]any{"role": "user", "content": "perf-next-turn"}}
			worker.body, _ = json.Marshal(fields)
		}
	}
	do := func(worker int) (bpsPerfSample, error) {
		w := workers[worker]
		if w.conn != nil {
			sample, _, err := bpsPerfWS(t.Context(), w.conn, w.body, expected, scenario.DeltaFrames, scenario.Transport)
			return sample, err
		}
		return bpsPerfHTTP(t.Context(), w.client, server.URL, w.session, w.body, scenario, expected)
	}
	for worker := range workers {
		for i := 0; i < warmup; i++ {
			if _, err := do(worker); err != nil {
				t.Fatal(err)
			}
		}
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	run := bpsPerfRun{Scenario: scenario, Samples: make([]bpsPerfSample, n*len(workers))}
	var wg sync.WaitGroup
	errors := make(chan error, len(workers))
	start := bpsPerfNow()
	for worker := range workers {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < n; i++ {
				sample, err := do(worker)
				if err != nil {
					errors <- fmt.Errorf("worker=%d iteration=%d: %w", worker, i, err)
					return
				}
				sample.Worker, sample.Iteration = worker, i
				run.Samples[worker*n+i] = sample
			}
		}(worker)
	}
	wg.Wait()
	run.MeasuredWallMS = bpsPerfMillisecondsSince(start)
	runtime.ReadMemStats(&after)
	close(errors)
	for err := range errors {
		t.Error(err)
	}
	wantCalls := uint64((n + warmup) * len(workers))
	if scenario.Transport == "ws_warmup" {
		wantCalls = 0
	}
	if scenario.History {
		wantCalls += uint64(len(workers))
	}
	if calls := sequence.Load(); calls != wantCalls {
		t.Errorf("unexpected upstream call count: got %d, want %d", calls, wantCalls)
	}
	run.AllocBytes, run.Mallocs, run.GCCycles = after.TotalAlloc-before.TotalAlloc, after.Mallocs-before.Mallocs, after.NumGC-before.NumGC
	t.Logf("%d validated samples in %.2f ms, %.0f bytes/request", len(run.Samples), run.MeasuredWallMS, float64(run.AllocBytes)/float64(len(run.Samples)))
	return run
}

type bpsPerfResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Output []struct {
		Type    string `json:"type"`
		Data    string `json:"encrypted_content"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"output"`
}

func bpsPerfCheckResponse(response bpsPerfResponse, expected string) error {
	var text strings.Builder
	for _, output := range response.Output {
		for _, content := range output.Content {
			if content.Type == "output_text" {
				text.WriteString(content.Text)
			}
		}
	}
	if response.ID == "" || response.Status != "completed" || text.String() != expected {
		return fmt.Errorf("invalid terminal: id=%q status=%q text bytes=%d expected=%d", response.ID, response.Status, text.Len(), len(expected))
	}
	return nil
}

type bpsPerfEvents struct {
	start    int64
	first    bool
	sample   bpsPerfSample
	text     strings.Builder
	response string
	expected string
	want     int
	ws       bool
	mode     string
}

func (events *bpsPerfEvents) consume(raw []byte) (bool, error) {
	events.sample.ResponseBytes += len(raw)
	var event struct {
		Type     string          `json:"type"`
		Delta    string          `json:"delta"`
		StreamID string          `json:"stream_id"`
		Response bpsPerfResponse `json:"response"`
	}
	if err := json.Unmarshal(raw, &event); err != nil {
		return false, err
	}
	if events.ws && event.StreamID != "perf_lane" {
		return false, fmt.Errorf("wrong WS stream id: %q", event.StreamID)
	}
	switch event.Type {
	case "response.output_text.delta":
		if event.Delta != "" && !events.first {
			events.sample.TTFTMS = bpsPerfMillisecondsSince(events.start)
			events.first = true
		}
		events.text.WriteString(event.Delta)
		events.sample.DeltaFrames++
	case "response.completed":
		if events.mode == "compact_sse" || events.mode == "ws_compact" {
			response := event.Response
			if response.ID == "" || response.Status != "completed" || len(response.Output) != 1 || response.Output[0].Type != "compaction" || response.Output[0].Data != "perf-opaque-state" {
				return false, fmt.Errorf("invalid compact terminal: %s", raw)
			}
		} else {
			if err := bpsPerfCheckResponse(event.Response, events.expected); err != nil {
				return false, err
			}
			if events.mode == "ws_warmup" && (!strings.HasPrefix(event.Response.ID, "resp_warmup_") || len(event.Response.Output) != 0) {
				return false, fmt.Errorf("invalid warmup terminal: %s", raw)
			}
		}
		if events.sample.DeltaFrames != events.want || events.text.String() != events.expected {
			return false, fmt.Errorf("lost/reordered deltas: frames=%d/%d bytes=%d/%d", events.sample.DeltaFrames, events.want, events.text.Len(), len(events.expected))
		}
		if events.want > 0 && (!events.first || events.sample.TTFTMS <= 0) {
			return false, fmt.Errorf("missing first delta or insufficient performance clock resolution")
		}
		events.response = event.Response.ID
		events.sample.TotalMS = bpsPerfMillisecondsSince(events.start)
		return true, nil
	case "error", "response.failed", "response.cancelled":
		return false, fmt.Errorf("request failed: %s", raw)
	}
	return false, nil
}

func bpsPerfWS(ctx context.Context, conn *websocket.Conn, body []byte, expected string, frames int, mode string) (bpsPerfSample, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	events := bpsPerfEvents{start: bpsPerfNow(), expected: expected, want: frames, ws: true, mode: mode}
	if err := conn.Write(ctx, websocket.MessageText, body); err != nil {
		return events.sample, "", err
	}
	for {
		kind, raw, err := conn.Read(ctx)
		if err != nil {
			return events.sample, "", err
		}
		if kind != websocket.MessageText {
			return events.sample, "", fmt.Errorf("unexpected binary frame")
		}
		done, err := events.consume(raw)
		if err != nil || done {
			return events.sample, events.response, err
		}
	}
}

func bpsPerfHTTP(ctx context.Context, client *http.Client, target, session string, body []byte, scenario bpsPerfScenario, expected string) (bpsPerfSample, error) {
	method, path := http.MethodPost, "/v1/responses"
	if scenario.Transport == "compact" {
		path += "/compact"
	} else if scenario.Transport == "models" {
		method, path, body = http.MethodGet, "/v1/models?client_version=0.155.0", nil
	}
	request, err := http.NewRequestWithContext(ctx, method, target+path, bytes.NewReader(body))
	if err != nil {
		return bpsPerfSample{}, err
	}
	request.Header.Set("Authorization", "Bearer test-client-key")
	request.Header.Set("Session-Id", session)
	request.Header.Set("Content-Type", "application/json")
	events := bpsPerfEvents{start: bpsPerfNow(), expected: expected, want: scenario.DeltaFrames, mode: scenario.Transport}
	response, err := client.Do(request)
	if err != nil {
		return events.sample, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(response.Body)
		return events.sample, fmt.Errorf("HTTP %d: %s", response.StatusCode, raw)
	}
	if scenario.Transport == "sse" || scenario.Transport == "compact_sse" {
		scanner := bufio.NewScanner(response.Body)
		scanner.Buffer(make([]byte, 8192), 4<<20)
		complete := false
		for scanner.Scan() {
			line := scanner.Bytes()
			if !bytes.HasPrefix(line, []byte("data: ")) {
				continue
			}
			done, err := events.consume(line[len("data: "):])
			if err != nil {
				return events.sample, err
			}
			complete = complete || done
		}
		if err := scanner.Err(); err != nil {
			return events.sample, err
		}
		if !complete {
			return events.sample, fmt.Errorf("SSE EOF without completed response")
		}
		return events.sample, nil
	}
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		return events.sample, err
	}
	events.sample.ResponseBytes = len(raw)
	switch scenario.Transport {
	case "json":
		var terminal bpsPerfResponse
		if err := json.Unmarshal(raw, &terminal); err != nil {
			return events.sample, err
		}
		if err := bpsPerfCheckResponse(terminal, expected); err != nil {
			return events.sample, err
		}
		events.sample.TTFTMS = bpsPerfMillisecondsSince(events.start)
	case "compact":
		var terminal struct {
			ID     string `json:"id"`
			Output []struct {
				Type string `json:"type"`
				Data string `json:"encrypted_content"`
			} `json:"output"`
		}
		if json.Unmarshal(raw, &terminal) != nil || terminal.ID == "" || len(terminal.Output) != 1 || terminal.Output[0].Type != "compaction" || terminal.Output[0].Data != "perf-opaque-state" {
			return events.sample, fmt.Errorf("invalid compact response: %s", raw)
		}
	case "models":
		var catalog struct {
			Models []struct {
				Slug string `json:"slug"`
			} `json:"models"`
		}
		if json.Unmarshal(raw, &catalog) != nil || len(catalog.Models) != 1 || catalog.Models[0].Slug != "gpt-6-astra" {
			return events.sample, fmt.Errorf("invalid model catalog: %s", raw)
		}
	}
	events.sample.TotalMS = bpsPerfMillisecondsSince(events.start)
	return events.sample, nil
}
