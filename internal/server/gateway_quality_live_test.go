package server

// Explicit opt-in real-account experiment. Credentials stay in private files
// on the selected host; reports contain aliases and synthetic puzzle output.
import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

type qualityLiveAccount struct {
	Alias          string `json:"alias"`
	CredentialFile string `json:"credential_file"`
	Production     bool   `json:"production"`
}
type qualityLiveConfig struct {
	AllowLive         bool                 `json:"allow_live"`
	Accounts          []qualityLiveAccount `json:"accounts"`
	Models            []string             `json:"models"`
	Effort            string               `json:"effort"`
	Arms              []string             `json:"arms"`
	Rounds            int                  `json:"rounds"`
	Prompt            string               `json:"prompt"`
	Instructions      string               `json:"instructions"`
	Settings          Settings             `json:"settings"`
	ProductionURL     string               `json:"production_url"`
	ProductionKeyFile string               `json:"production_key_file"`
	Output            string               `json:"output"`
	OutboundProxy     *ProxyEntry          `json:"outbound_proxy"`
}

func qualityDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func TestGatewayAccountQualityLive(t *testing.T) {
	path := os.Getenv("CODEX_QUALITY_PROBE_CONFIG")
	if path == "" {
		t.Skip("explicit private quality probe configuration is required")
	}
	if runtime.GOOS != "linux" || os.Getenv("CODEX_NATIVE_HTTP_TEST_BIN") == "" {
		t.Fatal("Linux and explicit native helper required")
	}
	raw, err := os.ReadFile(path)
	var config qualityLiveConfig
	if err != nil || json.Unmarshal(raw, &config) != nil || !config.AllowLive || len(config.Accounts) == 0 || config.Rounds < 1 || config.Rounds > 4 || len(config.Models) == 0 || config.Prompt == "" || config.Output == "" {
		t.Fatal("invalid private probe configuration")
	}
	if err := validateSettings(config.Settings); err != nil {
		t.Fatal("invalid experiment settings")
	}
	if err := os.MkdirAll(config.Output, 0700); err != nil {
		t.Fatal("cannot create report directory")
	}
	out, err := os.OpenFile(filepath.Join(config.Output, "results.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal("report already exists or cannot be created")
	}
	defer out.Close()
	sequence := 0
	for _, entry := range config.Accounts {
		credentials, err := os.ReadFile(entry.CredentialFile)
		var secret struct {
			AccessToken string `json:"access_token"`
			AccountID   string `json:"account_id"`
			ExpiresAt   int64  `json:"expires_at"`
		}
		if err != nil || json.Unmarshal(credentials, &secret) != nil || secret.AccessToken == "" || secret.AccountID == "" || entry.Alias == "" {
			t.Fatal("private account snapshot invalid")
		}
		if secret.ExpiresAt > 0 && secret.ExpiresAt < time.Now().Add(15*time.Minute).Unix() {
			t.Fatal("account snapshot expires too soon")
		}
		p := newTestProxy(t, 1)
		if err := p.Store.UpdateAccount("test", func(a *Account) error {
			a.AccessToken = secret.AccessToken
			a.RefreshToken = ""
			a.AccountID = secret.AccountID
			a.ExpiresAt = secret.ExpiresAt
			a.Identity = nil
			a.Enabled = true
			return nil
		}); err != nil {
			t.Fatal("cannot initialize isolated account")
		}
		p.BaseURL = codexURL
		p.TokenURL = "http://127.0.0.1:1/disabled-refresh"
		var selectedProxy *ProxyEntry
		if config.OutboundProxy != nil {
			created, err := p.Proxies.Create(*config.OutboundProxy)
			if err != nil {
				t.Fatal("invalid explicit experiment proxy")
			}
			selectedProxy = &created
			if err := p.Store.UpdateAccount("test", func(a *Account) error { a.Proxies = []string{created.ID}; return nil }); err != nil {
				t.Fatal("cannot bind experiment proxy")
			}
		}
		realGatewayNativePool(t, p)
		gateway := httptest.NewServer(p)
		for _, model := range config.Models {
			for round := 0; round < config.Rounds; round++ {
				arms := append([]string(nil), config.Arms...)
				if round%2 == 1 {
					for left, right := 0, len(arms)-1; left < right; left, right = left+1, right-1 {
						arms[left], arms[right] = arms[right], arms[left]
					}
				}
				for _, arm := range arms {
					if arm == "production" && !entry.Production {
						continue
					}
					sequence++
					settings := config.Settings
					settings.Version = 0 // Isolated experiment store, not production CAS.
					settings.AutomaticTokenRefreshEnabled = false
					settings.Upstream401RecoveryEnabled = false
					if arm == "transparent" || arm == "direct" || arm == "routing_hint" {
						settings.BuiltinPromptEnabled = false
						settings.CodexFingerprintEnabled = false
						settings.PromptCacheKeyEnabled = false
						settings.CompatibilityNormalizationEnabled = false
					}
					if arm == "no_prompt" {
						settings.BuiltinPromptEnabled = false
					}
					if arm != "candidate" && arm != "production" && arm != "transparent" && arm != "direct" && arm != "routing_hint" && arm != "no_prompt" {
						t.Fatal("unknown experiment arm")
					}
					if err := p.Settings.Set(settings); err != nil {
						t.Fatal("cannot apply isolated settings")
					}
					body, _ := json.Marshal(map[string]any{"model": model, "input": []any{map[string]any{"role": "user", "content": []any{map[string]string{"type": "input_text", "text": config.Prompt}}}}, "instructions": config.Instructions, "stream": true, "store": false, "reasoning": map[string]string{"effort": config.Effort}})
					ctx, cancel := context.WithTimeout(t.Context(), 240*time.Second)
					url := gateway.URL + "/v1/responses"
					client := gateway.Client()
					key := "test-client-key"
					if arm == "production" {
						url = config.ProductionURL + "/v1/responses"
						keyBytes, e := os.ReadFile(config.ProductionKeyFile)
						if e != nil {
							cancel()
							t.Fatal("production test key unavailable")
						}
						key = strings.TrimSpace(string(keyBytes))
						client = newHTTPClient()
					}
					if arm == "direct" {
						url = codexURL + "/responses"
						key = secret.AccessToken
						client = p.gatewayClientFor(selectedProxy, gatewayClientHTTP)
					}
					req, e := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
					if e != nil {
						cancel()
						t.Fatal("cannot build probe")
					}
					req.Header.Set("Authorization", "Bearer "+key)
					req.Header.Set("Content-Type", "application/json")
					req.Header.Set("Accept", "text/event-stream")
					req.Header.Set("User-Agent", "codex_cli_rs/0.155.0 (Linux 6.8.0; x86_64) unknown")
					req.Header.Set("Originator", "codex_cli_rs")
					req.Header.Set("Version", "0.155.0")
					req.Header.Set("Session-Id", fmt.Sprintf("quality-%s-%d-%d", entry.Alias, time.Now().UnixNano(), sequence))
					requestID := fmt.Sprintf("quality-%s-%d-%d", entry.Alias, time.Now().UnixNano(), sequence)
					req.Header.Set("X-Client-Request-Id", requestID)
					if arm == "direct" {
						req.Header.Set("ChatGPT-Account-Id", secret.AccountID)
					}
					if arm == "routing_hint" {
						req.Header.Set("X-Codex-Routing-Hint", "model="+model)
					}
					prepared := body
					if arm != "direct" {
						var prepErr error
						prepared, _, prepErr = applyGatewayPolicy(req, body, "admin", settings)
						if prepErr != nil {
							cancel()
							t.Fatal("prepare prediction failed")
						}
						prepared, _, prepErr = prepareBody(prepared, false)
						if prepErr != nil {
							cancel()
							t.Fatal("prepare prediction failed")
						}
					}
					row := map[string]any{"account_alias": entry.Alias, "credential_sha256": qualityDigest([]byte(secret.AccessToken)), "arm": arm, "round": round + 1, "sequence": sequence, "model": model, "effort": config.Effort, "prompt_sha256": qualityDigest([]byte(config.Prompt)), "request_sha256": qualityDigest(body), "predicted_upstream_body_sha256": qualityDigest(prepared), "started_at": time.Now().UTC().Format(time.RFC3339Nano), "live_model": true, "attempts": 1}
					row["request_id"] = requestID
					var upstreamMetadata qualityMetadataCapture
					started := time.Now()
					res, callErr := client.Do(req)
					if callErr != nil {
						row["failure_category"] = "transport_error"
						row["timeout"] = ctx.Err() != nil
					} else {
						row["http_status"] = res.StatusCode
						row["header_ms"] = time.Since(started).Milliseconds()
						row["response_headers"] = qualityResponseHeaders(res.Header)
						upstreamMetadata.headers(res.Header)
						if res.StatusCode != 200 {
							failure, _ := io.ReadAll(io.LimitReader(res.Body, (64<<10)+1))
							row["response_error"] = qualityErrorFields(failure)
							row["failure_category"] = "http_error"
						} else {
							counts := map[string]int{}
							observed := func(data []byte) {
								upstreamMetadata.event(data)
								var event struct {
									Type string `json:"type"`
								}
								if json.Unmarshal(data, &event) == nil {
									counts[event.Type]++
								}
							}
							result, readErr := collectResponseObserved(res.Body, observed)
							row["event_counts"] = counts
							if readErr != nil {
								row["failure_category"] = "response_error"
								row["timeout"] = ctx.Err() != nil
								qualityCaptureResponseFailure(row, readErr)
							} else {
								var decoded map[string]json.RawMessage
								_ = json.Unmarshal(result, &decoded)
								for _, name := range []string{"model", "status", "reasoning", "service_tier", "usage"} {
									if value := decoded[name]; len(value) > 0 {
										row["response_"+name] = value
									}
								}
								var items []struct {
									Type    string `json:"type"`
									Content []struct {
										Type string `json:"type"`
										Text string `json:"text"`
									} `json:"content"`
								}
								_ = json.Unmarshal(decoded["output"], &items)
								var texts []string
								for _, item := range items {
									if item.Type == "message" {
										for _, content := range item.Content {
											if content.Type == "output_text" {
												texts = append(texts, content.Text)
											}
										}
									}
								}
								row["output_text"] = strings.Join(texts, "\n")
								row["protocol_success"] = string(decoded["status"]) == `"completed"`
							}
						}
						_ = res.Body.Close()
					}
					row["duration_ms"] = time.Since(started).Milliseconds()
					row["upstream_metadata"] = upstreamMetadata.summary()
					cancel()
					if arm != "direct" && arm != "production" {
						for _, rec := range p.Metrics.RecentForAccount("test", 16) {
							if rec.ClientRequestID != requestID {
								continue
							}
							row["gateway_status"] = rec.Status
							row["gateway_returned_model"] = rec.ReturnedModel
							row["gateway_attempt_count"] = len(rec.Attempts)
							break
						}
					}
					encoded, _ := json.Marshal(row)
					_, _ = out.Write(append(encoded, '\n'))
					_ = out.Sync()
					t.Logf("quality probe account=%s arm=%s model=%s round=%d complete=%v status=%v ms=%v", entry.Alias, arm, model, round+1, row["protocol_success"], row["http_status"], row["duration_ms"])
				}
			}
		}
		gateway.Close()
		p.closeGatewayClients()
	}
}
