package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBPSTopPUnsupportedIdentifiesFieldWithoutDispatch(t *testing.T) {
	for _, value := range []string{"0.7", "1", "null", `"invalid"`} {
		t.Run(value, func(t *testing.T) {
			sends := 0
			p := bpsGatewayFixture(t, func(http.ResponseWriter, *http.Request) { sends++ })
			w := httptest.NewRecorder()
			p.gatewayBPS(w, proxyRequest(http.MethodPost, "/v1/responses", `{"model":"gpt-6-astra","input":"synthetic","stream":true,"top_p":`+value+`}`))
			var response struct{ Error struct{ Code, Param string } }
			if json.Unmarshal(w.Body.Bytes(), &response) != nil || w.Code != 400 || response.Error.Code != "unsupported_parameter" || response.Error.Param != "top_p" || strings.Contains(w.Body.String(), "PowerPoint") {
				t.Fatal("incompatible field lost typed parameter error")
			}
			p.Metrics.mu.Lock()
			count := p.Metrics.recentAt
			var record RequestRecord
			if count > 0 {
				record = p.Metrics.recent[count-1]
			}
			p.Metrics.mu.Unlock()
			if sends != 0 || count != 1 || record.ErrorParam != "top_p" || record.RequestType != "http_sse" || !record.Stream || len(record.Attempts) != 0 || record.UsageKnown != nil {
				t.Fatal("unsupported control was sent or its observation was fabricated")
			}
		})
	}
}
