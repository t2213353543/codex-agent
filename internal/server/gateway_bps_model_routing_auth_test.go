package server

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestBPSModelRoutingStaticRulesRejectRevokedAuthorizationBeforeGeneration(t *testing.T) {
	for _, mutation := range []string{"disabled", "groups", "store_error"} {
		t.Run(mutation, func(t *testing.T) {
			var catalogs, generations atomic.Int32
			p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					catalogs.Add(1)
					t.Error("static model routing must not query a remote catalog")
					w.WriteHeader(http.StatusBadGateway)
					return
				}
				if r.Method == http.MethodPost {
					generations.Add(1)
				}
				t.Errorf("revoked caller reached upstream: %s %s", r.Method, r.URL.Path)
				w.WriteHeader(http.StatusBadGateway)
			})
			key, account := gatewaySessionAuthorizationFixture(t, p)
			writer := gatewayKeyAuthorizationWriter(t, p, key)
			gatewayKeyAuthorizationMutate(t, writer, key, mutation)
			gatewayKeyAuthorizationAssertStaleIndex(t, p, key)
			request := proxyRequest(http.MethodPost, "/v1/responses", `{"model":"gpt-5.5","reasoning":{"effort":"max"},"stream":false,"input":"synthetic review"}`)
			request.Header.Set("Authorization", "Bearer "+key.Key)
			response := httptest.NewRecorder()
			p.gatewayBPS(response, request)
			want := http.StatusServiceUnavailable
			if mutation == "disabled" {
				want = http.StatusUnauthorized
			}
			if catalogs.Load() != 0 || generations.Load() != 0 || response.Code != want {
				t.Fatalf("catalog=%d post-revocation generation=%d response=%d want=%d", catalogs.Load(), generations.Load(), response.Code, want)
			}
			record, ok := p.Metrics.LastFor("test")
			if ok && (record.SentModel != "" || record.ReturnedModel != "") {
				t.Fatal("authorization rejection was recorded as a model generation")
			}
			if p.inFlight(account.ID) != 0 {
				t.Fatal("rejected static route retained an account slot")
			}
		})
	}
}
