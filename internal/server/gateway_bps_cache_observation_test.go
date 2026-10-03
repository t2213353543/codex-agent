package server

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBPSFinalRequestCacheObservationTracksControlsWithoutTaskChurn(t *testing.T) {
	turn := 0
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		turn++
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, bpsGatewayFrame("response.completed", fmt.Sprintf("observed-%d", turn), "[]"))
	})
	var err error
	p.PromptCacheObserver, err = newGatewayPromptCacheObserver()
	if err != nil {
		t.Fatal(err)
	}
	settings, _ := p.Settings.Get()
	settings.PromptCacheKeyEnabled = true
	if err := p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
	for index, effort := range []string{"high", "high", "xhigh"} {
		body := fmt.Sprintf(`{"model":"gpt-6-astra","stream":false,"reasoning":{"effort":%q},"input":[{"role":"user","content":"same prefix"}]}`, effort)
		r := proxyRequest("POST", "/v1/responses", body)
		r.Header.Set("Session-Id", "cache-observation-session")
		w := httptest.NewRecorder()
		p.gatewayBPS(w, r)
		if w.Code != 200 {
			t.Fatalf("status=%d body=%s", w.Code, w.Body)
		}
		record, ok := p.Metrics.LastFor("test")
		if !ok || record.PromptCache == nil {
			t.Fatal("BPS request cache observation missing")
		}
		observed := record.PromptCache
		if observed.Coverage != "complete" || observed.KeyMode != "generated_prefix" || !observed.CacheKeyPresent {
			t.Fatalf("wrong observation: %+v", observed)
		}
		if index > 0 {
			if observed.Comparison != "compared" || observed.ControlsChanged != (index == 2) {
				t.Fatalf("BPS controls or volatile task identity misclassified: %+v", observed)
			}
		}
	}
}

func TestBPSRejectedParametersRetainRequestedModelWithoutInventingResponse(t *testing.T) {
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("rejected request reached upstream") })
	w := httptest.NewRecorder()
	p.gatewayBPS(w, proxyRequest("POST", "/v1/responses", `{"model":"gpt-6-astra","input":"hello","text":{"format":{"type":"json_schema","name":"demo","schema":{"type":"not-a-schema-type"}}}}`))
	record, ok := p.Metrics.LastFor("")
	if w.Code != 400 || !ok || record.Model != "gpt-6-astra" || record.ReturnedModel != "" || record.ErrorSource != "client" || record.ErrorPhase != "request_validation" {
		t.Fatalf("validation observation lost or invented upstream model: status=%d record=%+v", w.Code, record)
	}
}
