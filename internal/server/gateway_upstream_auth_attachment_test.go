package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestBPSUpstreamAuthenticationAttachmentStatus(t *testing.T) {
	var uploads, generations int
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if r.URL.Path == "/attachments" {
			uploads++
		} else {
			generations++
		}
		w.WriteHeader(401)
		_, _ = io.WriteString(w, `{"error":{"code":"unauthorized","message":"SYNTHETIC_UPLOAD_TOKEN_REVOKED"}}`)
	})
	settings, _ := p.Settings.Get()
	settings.Upstream401RecoveryEnabled = false
	settings.AutomaticTokenRefreshEnabled = false
	if err := p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
	if err := p.Store.UpdateAccount("test", func(a *Account) error { a.Priority = 100; return nil }); err != nil {
		t.Fatal(err)
	}
	credential, _ := json.Marshal(map[string]any{"access_token": "synthetic-second-token", "expires_at": time.Now().Add(time.Hour).Unix()})
	if err := p.Store.ImportWith("second", credential, AccountDefaults{Concurrency: 2, Priority: 1, Weight: 1}); err != nil {
		t.Fatal(err)
	}
	p.invalidateAccounts()
	image, _ := json.Marshal(bpsAttachmentTestURI(bpsAttachmentTestPNG))
	body := fmt.Sprintf(`{"model":"gpt-6-astra","stream":false,"input":[{"role":"user","content":[{"type":"input_text","text":"synthetic"},{"type":"input_image","image_url":%s}]}]}`, image)
	out := httptest.NewRecorder()
	p.ServeHTTP(out, proxyRequest(http.MethodPost, "/v1/responses", body))
	rec, ok := p.Metrics.LastFor("second")
	t.Logf("UPLOAD_STATUS http=%d uploads=%d generations=%d record=%t identity_present_after_finish=%t source=%s phase=%s", out.Code, uploads, generations, ok, rec.AccountID != "", rec.ErrorSource, rec.ErrorPhase)
	if !ok || uploads != 2 || generations != 0 || rec.ErrorSource != "upstream" || rec.ErrorPhase != "attachment" {
		t.Fatal("fixture did not reach exhausted upload authentication boundary")
	}
	if rec.AccountID == "" || rec.HTTPCode != 503 || len(rec.Attempts) != 2 || rec.UsageKnown != nil && *rec.UsageKnown {
		t.Fatal("attachment public status or actual accounting missing")
	}
	for _, attempt := range rec.Attempts {
		if attempt.Endpoint != "/attachments" || attempt.StatusCode == nil || *attempt.StatusCode != 401 || attempt.ObservedHTTPStatusCode == nil || *attempt.ObservedHTTPStatusCode != 401 {
			t.Fatal("attachment actual401 was overwritten")
		}
	}
	if out.Code != 503 {
		t.Errorf("RED: pre-generation upstream upload authentication escaped as public%d", out.Code)
	}
}
