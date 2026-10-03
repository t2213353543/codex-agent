package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
)

func TestBPSLiveCompatibleModelRoutes(t *testing.T) {
	file, binary, work := os.Getenv("BPS_LIVE_ACCOUNTS_FILE"), os.Getenv("BPS_LIVE_CODEX_BINARY"), os.Getenv("BPS_LIVE_WORK_ROOT")
	if file == "" || binary == "" || work == "" {
		t.Skip("explicit server-only live fixture required")
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal("live fixture unavailable")
	}
	var fixture struct {
		Accounts []bpsLiveAccount `json:"accounts"`
	}
	if json.Unmarshal(raw, &fixture) != nil || len(fixture.Accounts) == 0 {
		t.Fatal("invalid live fixture")
	}
	a := fixture.Accounts[0]
	var evidence []map[string]any
	h := newBPSLiveHarness(t, binary, work, a, []string{a.AccessToken, a.AccountID}, &evidence)
	var catalogProbes atomic.Int32
	base := h.recorder.p.Client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	h.recorder.p.Client.Transport = bpsCacheLiveTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet {
			catalogProbes.Add(1)
		}
		return base.RoundTrip(r)
	})
	for _, tc := range []struct{ model, effort, want string }{
		{"gpt-5.5", "max", "gpt-5.6-sol"},
		{"codex-auto-review", "ultra", "gpt-6-astra"},
		{"gpt-reserve", "max", "gpt-5.6-sol"},
		{"gpt-5.5-codex", "max", "gpt-6-astra"},
		{"unavailable-client-text-model", "ultra", "gpt-6-astra"},
	} {
		t.Run(tc.model, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.recorder.p.gatewayBPS(w, proxyRequest(http.MethodPost, "/v1/responses", fmt.Sprintf(`{"model":%q,"reasoning":{"effort":%q},"stream":false,"input":"Reply exactly OK without tools."}`, tc.model, tc.effort)))
			var result struct{ Status, Model string }
			_ = json.Unmarshal(w.Body.Bytes(), &result)
			record, ok := h.recorder.p.Metrics.LastFor("test")
			t.Logf("live_compat_route requested=%s effort=%s sent=%s sent_effort=%s returned=%s http=%d completed=%t", tc.model, tc.effort, record.SentModel, record.SentReasoningEffort, result.Model, w.Code, result.Status == "completed")
			if w.Code != 200 || result.Status != "completed" || result.Model != tc.want || !ok || record.Model != tc.model || record.SentModel != tc.want || record.SentReasoningEffort != "xhigh" || record.RequestedReasoningEffort != tc.effort {
				t.Fatal("compatibility route did not complete with observed requested and sent identities")
			}
		})
	}
	if catalogProbes.Load() != 0 {
		t.Fatal("fixed model rules triggered an upstream catalog probe")
	}
}
