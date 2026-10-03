package server

import (
	"context"
	"net"
	"net/http"
	"strings"
)

type ingressClientIPKey struct{}

// A dedicated ingress overwrites X-Forwarded-For with the socket peer address.
// Only an explicitly configured backend behind that loopback ingress trusts it.
func withTrustedIngressClientIP(next http.Handler, enabled bool) http.Handler {
	if !enabled {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer, _, err := net.SplitHostPort(r.RemoteAddr)
		if err == nil && net.ParseIP(peer).IsLoopback() {
			values := r.Header.Values("X-Forwarded-For")
			if len(values) == 1 {
				ip := net.ParseIP(strings.TrimSpace(values[0]))
				if ip != nil {
					r = r.WithContext(context.WithValue(r.Context(), ingressClientIPKey{}, ip.String()))
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}
