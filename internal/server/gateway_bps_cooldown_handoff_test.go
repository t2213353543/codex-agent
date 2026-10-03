package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const teamCLimitedModel = "gpt-6-sol"

func TestTeamCRateLimitResolvedCooldownFailureBoundary(t *testing.T) {
	account := Account{ID: "fixture", Name: "fixture"}
	ctx := withGatewayAccountFailover(context.Background())
	for _, test := range []struct {
		name     string
		err      error
		eligible bool
	}{
		{"confirmed_model_cooldown", &gatewayCooldownError{until: time.Now().Add(time.Minute)}, true},
		{"redis_store_unavailable", &gatewayCooldownStoreError{}, false},
		{"cancelled", context.Canceled, false},
		{"deadline", context.DeadlineExceeded, false},
		{"other_local_failure", errors.New("fixture unavailable"), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := bpsResolvedCooldownFailure(ctx, account, test.err)
			if officialFailoverEligible(result) != test.eligible {
				t.Fatalf("local failure incorrectly permitted account retry: type=%T eligible=%v", test.err, officialFailoverEligible(result))
			}
			if !test.eligible && result != test.err {
				t.Fatal("fail-closed error identity changed")
			}
			if !errors.Is(result, test.err) {
				t.Fatal("typed selection cause lost")
			}
		})
	}
}

type teamCRateCalls struct {
	sync.Mutex
	values []string
}

func (c *teamCRateCalls) add(value string) {
	c.Lock()
	defer c.Unlock()
	c.values = append(c.values, value)
}

func (c *teamCRateCalls) count() int {
	c.Lock()
	defer c.Unlock()
	return len(c.values)
}

func teamCRateFixture(t *testing.T, mode string, second bool) (*Proxy, *teamCRateCalls) {
	t.Helper()
	calls := &teamCRateCalls{}
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if r.URL.Path != "/responses" {
			t.Errorf("unexpected fixture route: %s", r.URL.Path)
			w.WriteHeader(500)
			return
		}
		identity := r.Header.Get("Authorization")
		calls.add(identity)
		if second && mode != "all_limited" && identity == "Bearer second-fixture-token" {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, bpsGatewayFrame("response.completed", "fixture-success", "[]"))
			return
		}
		if mode == "http429" {
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(429)
			_, _ = io.WriteString(w, `{"error":{"code":"rate_limit_exceeded","message":"fixture quota"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: "+`{"type":"response.created","response":{"id":"fixture-limited","status":"in_progress"}}`+"\n\n")
		if mode == "after_output" {
			_, _ = io.WriteString(w, "data: "+`{"type":"response.output_text.delta","delta":"VISIBLE"}`+"\n\n")
		}
		_, _ = io.WriteString(w, "data: "+`{"type":"response.failed","headers":{"Retry-After":"30"},"response":{"id":"fixture-limited","status":"failed","output":[],"error":{"code":"rate_limit_exceeded","message":"fixture quota"}}}`+"\n\n")
	})
	settings, _ := p.Settings.Get()
	settings.RateLimitCooldownEnabled = true
	settings.Upstream401RecoveryEnabled = false
	settings.AutomaticTokenRefreshEnabled = false
	if err := p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
	if err := p.Store.UpdateAccount("test", func(a *Account) error { a.Priority = 100; return nil }); err != nil {
		t.Fatal(err)
	}
	if second {
		raw, _ := json.Marshal(map[string]any{"access_token": "second-fixture-token", "expires_at": time.Now().Add(time.Hour).Unix()})
		if err := p.Store.ImportWith("second", raw, AccountDefaults{Concurrency: 100, Priority: 0}); err != nil {
			t.Fatal(err)
		}
	}
	p.invalidateAccounts()
	return p, calls
}

func teamCRateRequest(t *testing.T, p *Proxy, stream bool, session string) *httptest.ResponseRecorder {
	t.Helper()
	req := proxyRequest(http.MethodPost, "/v1/responses", fmt.Sprintf(`{"model":%q,"stream":%t,"input":[{"role":"user","content":"synthetic rate fixture"}]}`, teamCLimitedModel, stream))
	req.Header.Set("Session-Id", session)
	out := httptest.NewRecorder()
	p.gatewayBPS(out, req)
	return out
}

func teamCAssertLimitedAttempt(t *testing.T, p *Proxy, account string, attempts int) RequestRecord {
	t.Helper()
	record, ok := p.Metrics.LastFor(account)
	if !ok || len(record.Attempts) != attempts {
		t.Fatalf("attempt accounting mismatch: exists=%v attempts=%+v", ok, record.Attempts)
	}
	first := record.Attempts[0]
	if first.StatusCode == nil || *first.StatusCode != 429 || first.ErrorCode != "rateLimitExceeded" || first.ErrorSource != "upstream" {
		t.Fatalf("structured quota mapping missing: %+v", first)
	}
	return record
}

func TestTeamCRateLimitCreatedDoesNotBlockFailover(t *testing.T) {
	for _, mode := range []string{"sse", "json", "after_output", "all_limited"} {
		t.Run(mode, func(t *testing.T) {
			p, calls := teamCRateFixture(t, mode, true)
			out := teamCRateRequest(t, p, mode != "json", "created-boundary")
			wantCalls := 2
			if mode == "after_output" {
				wantCalls = 1
			}
			if calls.count() != wantCalls {
				t.Fatalf("calls=%d want=%d", calls.count(), wantCalls)
			}
			account := "second"
			if mode == "after_output" {
				account = "test"
			}
			record := teamCAssertLimitedAttempt(t, p, account, wantCalls)
			if mode == "sse" || mode == "json" {
				if record.Status != "ok" || !strings.Contains(out.Body.String(), "fixture-success") || strings.Contains(out.Body.String(), "fixture-limited") {
					t.Fatalf("created-only attempt was exposed or not replaced: record=%s body=%s", record.Status, out.Body.String())
				}
			} else if mode == "after_output" {
				if !strings.Contains(out.Body.String(), "VISIBLE") || record.Status != "error" || record.TTFTMs == nil {
					t.Fatal("already visible output guard changed")
				}
			} else {
				last := record.Attempts[len(record.Attempts)-1]
				if record.Status != "error" || record.HTTPCode != 200 || last.StatusCode == nil || *last.StatusCode != 429 || last.ErrorCode != "rateLimitExceeded" || !strings.Contains(out.Body.String(), "rate_limit_exceeded") {
					t.Fatalf("all-account exhaustion erased original SSE or structured failures: %+v body=%s", record.Attempts, out.Body.String())
				}
			}
			for _, name := range []string{"test", "second"} {
				if p.inFlight(name) != 0 {
					t.Fatalf("lease leaked for %s", name)
				}
			}
		})
	}
}

func TestTeamCRateLimitCooldownUsesAcceptedSSE(t *testing.T) {
	for _, mode := range []string{"http429", "sse", "json", "after_output"} {
		t.Run(mode, func(t *testing.T) {
			p, calls := teamCRateFixture(t, mode, false)
			_ = teamCRateRequest(t, p, mode != "json", "rate-first")
			teamCAssertLimitedAttempt(t, p, "test", 1)
			account, err := p.Store.Get("test")
			if err != nil {
				t.Fatal(err)
			}
			var cooldown *gatewayCooldownError
			err = p.checkGatewayCooldown(withGatewayCooldownModel(context.Background(), teamCLimitedModel), account)
			if !errors.As(err, &cooldown) {
				t.Errorf("accepted account/model quota produced no cooldown: mode=%s err=%v", mode, err)
			} else if remaining := time.Until(cooldown.until); remaining < 28*time.Second || remaining > 31*time.Second {
				t.Errorf("explicit Retry-After not retained: %v", remaining)
			}
			if err := p.checkGatewayCooldown(withGatewayCooldownModel(context.Background(), "different-fixture-model"), account); err != nil {
				t.Errorf("quota incorrectly cooled an unrelated model: %v", err)
			}
			next := teamCRateRequest(t, p, true, "rate-second-new-session")
			if calls.count() != 1 || next.Code != 429 {
				t.Errorf("new request redispatched limited model: calls=%d next_http=%d retry_after=%q", calls.count(), next.Code, next.Header().Get("Retry-After"))
			}
		})
	}
}

func TestTeamCRateLimitAllAccountsCooldownNextRequest(t *testing.T) {
	p, calls := teamCRateFixture(t, "all_limited", true)
	first := teamCRateRequest(t, p, true, "all-limited-first")
	if calls.count() != 2 || first.Code != 200 {
		t.Fatalf("first request should retain two real SSE failures: calls=%d code=%d", calls.count(), first.Code)
	}
	teamCAssertLimitedAttempt(t, p, "second", 2)
	second := teamCRateRequest(t, p, true, "all-limited-second-new-session")
	if calls.count() != 2 || second.Code != 429 {
		t.Fatalf("all limited accounts immediately redispatched: total_calls=%d second_http=%d retry_after=%q", calls.count(), second.Code, second.Header().Get("Retry-After"))
	}
}

func TestTeamCRateLimitAliasChecksResolvedCooldownBeforeDispatch(t *testing.T) {
	p, calls := teamCRateFixture(t, "http429", false)
	account, err := p.Store.Get("test")
	if err != nil {
		t.Fatal(err)
	}
	p.rememberBPSModelCatalog(account, nil, "admin", bpsRouteCatalogFixture(t))
	request := func(session string) *httptest.ResponseRecorder {
		req := proxyRequest(http.MethodPost, "/v1/responses", `{"model":"gpt-5.5","stream":true,"input":[{"role":"user","content":"synthetic alias cooldown"}]}`)
		req.Header.Set("Session-Id", session)
		out := httptest.NewRecorder()
		p.gatewayBPS(out, req)
		return out
	}
	first := request("alias-first")
	if calls.count() != 1 || first.Code != 429 || first.Header().Get("X-Resolved-Model") != "gpt-5.6-sol" {
		t.Fatalf("alias fixture did not dispatch exact resolved model: calls=%d code=%d resolved=%q", calls.count(), first.Code, first.Header().Get("X-Resolved-Model"))
	}
	var limited *gatewayCooldownError
	if err := p.checkGatewayCooldown(withGatewayCooldownModel(context.Background(), "gpt-5.6-sol"), account); !errors.As(err, &limited) {
		t.Fatalf("HTTP429 did not record actual resolved cooldown: %v", err)
	}
	if err := p.checkGatewayCooldown(withGatewayCooldownModel(context.Background(), "gpt-5.5"), account); err != nil {
		t.Fatalf("fixture unexpectedly stored requested alias cooldown: %v", err)
	}
	second := request("alias-second-new-session")
	if calls.count() != 1 || second.Code != 429 {
		t.Fatalf("requested alias bypassed known resolved cooldown: calls=%d code=%d", calls.count(), second.Code)
	}
	record, ok := p.Metrics.LastFor("test")
	if !ok || len(record.Attempts) != 0 {
		t.Fatalf("local resolved cooldown was counted as an upstream attempt: %+v", record.Attempts)
	}
}

func TestTeamCRateLimitAliasLocalSelectionAttemptAccounting(t *testing.T) {
	for _, mode := range []string{"first_cooled", "all_cooled", "real_then_cooled", "different_resolved_model"} {
		t.Run(mode, func(t *testing.T) {
			p, calls := teamCRateFixture(t, "http429", true)
			first, err := p.Store.Get("test")
			if err != nil {
				t.Fatal(err)
			}
			second, err := p.Store.Get("second")
			if err != nil {
				t.Fatal(err)
			}
			p.rememberBPSModelCatalog(first, nil, "admin", bpsRouteCatalogFixture(t))
			secondCatalog := bpsRouteCatalogFixture(t)
			if mode == "different_resolved_model" {
				secondCatalog.ModelCatalog.Models = secondCatalog.ModelCatalog.Models[2:3] // advertised gpt-6-sol
				settings, err := p.Settings.Get()
				if err != nil {
					t.Fatal(err)
				}
				settings.BPSModelRoutes = "gpt-5.5=gpt-6-sol\n*=gpt-6-astra"
				if err = p.Settings.Set(settings); err != nil {
					t.Fatal(err)
				}
			}
			p.rememberBPSModelCatalog(second, nil, "admin", secondCatalog)
			limitedScope := withGatewayCooldownModel(context.Background(), "gpt-5.6-sol")
			if mode != "real_then_cooled" {
				firstScope := limitedScope
				if mode == "different_resolved_model" {
					firstScope = withGatewayCooldownModel(context.Background(), "gpt-6-sol")
				}
				p.recordGatewayCooldown(firstScope, first, "30")
			}
			if mode == "all_cooled" || mode == "real_then_cooled" || mode == "different_resolved_model" {
				p.recordGatewayCooldown(limitedScope, second, "30")
			}
			req := proxyRequest(http.MethodPost, "/v1/responses", `{"model":"gpt-5.5","stream":true,"input":[{"role":"user","content":"synthetic local selection"}]}`)
			req.Header.Set("Session-Id", "alias-selection-"+mode)
			out := httptest.NewRecorder()
			p.gatewayBPS(out, req)
			p.Metrics.mu.Lock()
			records := append([]RequestRecord(nil), p.Metrics.recent[:p.Metrics.recentAt]...)
			p.Metrics.mu.Unlock()
			if len(records) != 1 {
				t.Fatalf("one incoming request produced %d records", len(records))
			}
			record := records[0]
			wantCalls, wantStatus := 1, 200
			if mode == "all_cooled" {
				wantCalls, wantStatus = 0, 429
			}
			if mode == "real_then_cooled" {
				wantStatus = 429
			}
			if calls.count() != wantCalls || out.Code != wantStatus || len(record.Attempts) != wantCalls {
				t.Fatalf("local cooldown miscounted/replayed: mode=%s calls=%d code=%d attempts=%+v", mode, calls.count(), out.Code, record.Attempts)
			}
			if mode == "real_then_cooled" {
				if record.Account != "test" || record.Attempts[0].ErrorCode != "rateLimitExceeded" || !strings.Contains(out.Body.String(), "fixture quota") {
					t.Fatalf("local skipped account replaced real upstream failure: account=%s attempts=%+v body=%s", record.Account, record.Attempts, out.Body.String())
				}
			} else if wantStatus == 200 {
				if record.Account != "second" || record.Attempts[0].ErrorCode != "" || !strings.Contains(out.Body.String(), "fixture-success") {
					t.Fatalf("eligible replacement did not succeed alone: %+v", record.Attempts)
				}
			}
			if mode == "different_resolved_model" && out.Header().Get("X-Resolved-Model") != "gpt-6-sol" {
				t.Fatal("unrelated actual model was frozen or routing changed")
			}
		})
	}
}
