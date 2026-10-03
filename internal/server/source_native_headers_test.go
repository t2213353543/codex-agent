package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSourceNativeExecHeaderAbsence(t *testing.T) {
	for _, explicitVersion := range []string{"", "0.154.0-alpha.6.2"} {
		t.Run("version_"+explicitVersion, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("Version"); got != explicitVersion {
					t.Errorf("version header = %q, want %q", got, explicitVersion)
				}
				if got := r.Header.Get("Accept-Encoding"); got != "" {
					t.Errorf("unexpected gateway response-compression negotiation: %q", got)
				}
				if !strings.HasPrefix(r.UserAgent(), "codex_exec/0.154.0") || r.Header.Get("Originator") != "codex_exec" {
					t.Error("native exec identity changed")
				}
				_, _ = io.WriteString(w, "ok")
			}))
			defer upstream.Close()
			source := make(http.Header)
			source.Set("User-Agent", "codex_exec/0.154.0 (Windows 10.0.26200; x86_64) unknown (codex_exec; 0.154.0)")
			source.Set("Originator", "codex_exec")
			if explicitVersion != "" {
				source.Set("Version", explicitVersion)
			}
			r, _ := http.NewRequest(http.MethodGet, upstream.URL, nil)
			copyRequestHeaders(r.Header, source)
			client := newHTTPClient()
			defer client.CloseIdleConnections()
			response, err := client.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
		})
	}
}
