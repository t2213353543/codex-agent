package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Two explicit live calls exercise the official client feature over HTTP and
// WebSocket. Shape observations retain no summary text or original item IDs.
func TestBPSLiveSequentialSummaryCLI(t *testing.T) {
	file, binary, work := os.Getenv("BPS_LIVE_ACCOUNTS_FILE"), os.Getenv("BPS_LIVE_CODEX_BINARY"), os.Getenv("BPS_LIVE_WORK_ROOT")
	if file == "" || binary == "" || work == "" {
		t.Skip("explicit server-only live fixture required")
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal("fixture unavailable")
	}
	var fixture struct{ Accounts []bpsLiveAccount }
	if json.Unmarshal(data, &fixture) != nil || len(fixture.Accounts) == 0 {
		t.Fatal("fixture invalid")
	}
	a := fixture.Accounts[0]
	var evidence []map[string]any
	defer func() {
		if path := os.Getenv("BPS_LIVE_EVIDENCE_FILE"); path != "" {
			if bpsLiveWriteEvidence(path, "sequential-summary-official-cli", !t.Failed(), evidence) != nil {
				t.Error("evidence persistence failed")
			}
		}
	}()
	for _, websocket := range []bool{false, true} {
		name := "sse"
		if websocket {
			name = "websocket"
		}
		if !t.Run(name, func(t *testing.T) {
			h := newBPSLiveHarness(t, binary, work, a, []string{a.AccessToken, a.AccountID}, &evidence)
			config := filepath.Join(h.home, "config.toml")
			raw, err := os.ReadFile(config)
			if err != nil {
				t.Fatal(err)
			}
			text := strings.Replace(string(raw), "[features]", "[features]\nconcurrent_reasoning_summaries=true", 1)
			text = "model_reasoning_summary=\"auto\"\n" + text
			if websocket {
				text = strings.Replace(text, "supports_websockets=false", "supports_websockets=true", 1)
				text = strings.Replace(text, "[features]", "[features]\nresponses_websockets_v2=true", 1)
				h.recorder.requireWS = true
			}
			if os.WriteFile(config, []byte(text), 0600) != nil {
				t.Fatal("config unavailable")
			}
			// For HTTP, observe the actual client writer; WS acceptance is asserted
			// separately by its real config and dedicated loopback transport tests.
			clientOptionRequests := 0
			front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost && r.URL.Path == "/v1/responses" {
					b, e := io.ReadAll(r.Body)
					if e != nil {
						http.Error(w, "fixture read failed", 500)
						return
					}
					r.Body.Close()
					r.Body = io.NopCloser(bytes.NewReader(b))
					var root struct {
						StreamOptions struct {
							Delivery string `json:"reasoning_summary_delivery"`
						} `json:"stream_options"`
					}
					if json.Unmarshal(b, &root) == nil && root.StreamOptions.Delivery == "sequential_cutoff" {
						clientOptionRequests++
					}
				}
				h.recorder.ServeHTTP(w, r)
			}))
			defer front.Close()
			// The original fixture URL is quoted in the known provider setting.
			lines := strings.Split(text, "\n")
			for i, line := range lines {
				if strings.HasPrefix(line, "base_url=") {
					lines[i] = fmt.Sprintf("base_url=%q", front.URL+"/v1")
				}
			}
			if os.WriteFile(config, []byte(strings.Join(lines, "\n")), 0600) != nil {
				t.Fatal("proxy config unavailable")
			}
			p := h.recorder.p
			next := p.Client.Transport
			if next == nil {
				next = http.DefaultTransport
			}
			posts, summaryDone, timelyDone := 0, 0, 0
			upstreamOption := false
			p.Client.Transport = bpsCacheLiveTransport(func(req *http.Request) (*http.Response, error) {
				if req.Method != http.MethodPost || !strings.HasSuffix(req.URL.Path, "/responses") || posts != 0 {
					return nil, fmt.Errorf("summary CLI permits one generation POST")
				}
				posts++
				body, e := io.ReadAll(req.Body)
				req.Body.Close()
				if e != nil {
					return nil, e
				}
				req.Body = io.NopCloser(bytes.NewReader(body))
				req.ContentLength = int64(len(body))
				req.GetBody = nil
				var root map[string]json.RawMessage
				_ = json.Unmarshal(body, &root)
				upstreamOption = root["stream_options"] != nil
				res, e := next.RoundTrip(req)
				if e != nil {
					return res, e
				}
				res.Body = &bpsCacheLiveResponseBody{ReadCloser: res.Body, complete: func(wire []byte) {
					active := ""
					for _, line := range bytes.Split(wire, []byte{'\n'}) {
						if !bytes.HasPrefix(line, []byte("data:")) {
							continue
						}
						var event struct {
							Type   string                    `json:"type"`
							ItemID string                    `json:"item_id"`
							Index  *int                      `json:"summary_index"`
							Text   *string                   `json:"text"`
							Item   struct{ ID, Type string } `json:"item"`
						}
						if json.Unmarshal(bytes.TrimSpace(line[5:]), &event) != nil {
							continue
						}
						switch event.Type {
						case "response.output_item.added":
							active = event.Item.ID
						case "response.output_item.done":
							if active == event.Item.ID {
								active = ""
							}
						case "response.reasoning_summary_text.done":
							summaryDone++
							if event.ItemID != "" && event.ItemID == active && event.Index != nil && *event.Index >= 0 && event.Text != nil && *event.Text != "" {
								timelyDone++
							}
						}
					}
				}}
				return res, nil
			})
			result := h.run(name, "Solve this synthetic scheduling puzzle without tools: A, B, C, D each take one minute. A precedes C; B precedes D; C cannot be second; D must be last. Give one valid order and a short explanation.", "exec", "--json", "--skip-git-repo-check", "--ignore-rules", "--color", "never", "--sandbox", "read-only", "-")
			record, ok := p.Metrics.LastFor("test")
			reasoningVisible := false
			for _, kind := range result.ItemTypes {
				if kind == "reasoning" {
					reasoningVisible = true
				}
			}
			upgrades := 0
			for _, r := range h.recorder.forCase(name) {
				if r.WebSocketUpgrade && r.Status == 101 {
					upgrades++
				}
			}
			passed := result.Completed && result.ExitCode == 0 && posts == 1 && !upstreamOption && summaryDone > 0 && timelyDone == summaryDone && reasoningVisible && bpsLiveNoTools(result) && ok && record.Status == "ok" && record.UsageKnown != nil && *record.UsageKnown
			if websocket {
				passed = passed && upgrades > 0 && record.RequestType == "websocket"
			} else {
				passed = passed && clientOptionRequests == 1 && record.RequestType == "http_sse"
			}
			row := map[string]any{"case": name, "passed": passed, "client_completed": result.Completed, "reasoning_item_visible": reasoningVisible, "generation_requests": posts, "client_option_http_requests": clientOptionRequests, "summary_done": summaryDone, "timely_summary_done": timelyDone, "upstream_stream_options": upstreamOption, "ws_upgrades": upgrades, "request_type": record.RequestType, "usage_known": record.UsageKnown, "cache_read_tokens": record.CachedTokens, "cache_write_tokens": record.CacheWriteTokens}
			evidence = append(evidence, row)
			b, _ := json.Marshal(row)
			t.Log(string(b))
			if !passed {
				t.Fatal("official summary feature did not complete with native timely summaries")
			}
		}) {
			return
		}
	}
}
