package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBPSIngressClientIPRequiresExplicitTrustedHop(t *testing.T) {
	for _, tc := range []struct {
		enabled                 bool
		remote, forwarded, want string
	}{
		{true, "127.0.0.1:2345", "203.0.113.3", "203.0.113.3"},
		{true, "[::1]:2345", "2001:db8::3", "2001:db8::3"},
		{false, "127.0.0.1:2345", "203.0.113.3", "127.0.0.1"},
		{true, "203.0.113.4:2345", "203.0.113.3", "203.0.113.4"},
		{true, "127.0.0.1:2345", "203.0.113.3, 1.2.3.4", "127.0.0.1"},
		{true, "127.0.0.1:2345", "invalid", "127.0.0.1"},
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = tc.remote
		r.Header.Set("X-Forwarded-For", tc.forwarded)
		h := withTrustedIngressClientIP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if got := clientIP(r); got != tc.want {
				t.Errorf("ip=%s want=%s", got, tc.want)
			}
		}), tc.enabled)
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
}
