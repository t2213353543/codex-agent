package server

import (
	"codex-agent/internal/publicerrors"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// webHandler wires the gateway, the admin API and the static frontend
// behind shared security headers.
func webHandler(p *Proxy, dir string) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/health", http.HandlerFunc(p.health))
	mux.Handle("/v1/", p)
	mux.Handle("/backend-api/", p)
	mux.Handle("/responses", p)
	mux.Handle("/responses/compact", p)
	mux.Handle("/guardian", p)
	mux.Handle("/guardian-classifier", p)
	mux.Handle("/models", p)
	mux.Handle("/api/", p.adminRoutes())
	compat := p.compatRoutes()
	mux.Handle("/api/v1/admin/", compat)
	mux.Handle("/v0/management/", compat)
	mux.Handle("/", staticHandler(dir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		mux.ServeHTTP(w, r)
	})
}

func (p *Proxy) health(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		apiError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// staticHandler serves the built frontend: only the root document, assets
// and the favicon; unknown paths stay 404 instead of falling back to SPA.
func staticHandler(dir string) http.Handler {
	static := http.FileServer(http.Dir(dir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			apiError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if r.URL.Path == "/" {
			if _, err := os.Stat(filepath.Join(dir, "index.html")); err != nil {
				apiError(w, http.StatusServiceUnavailable, "frontend not built")
				return
			}
			w.Header().Set("Cache-Control", "no-cache")
		} else if !strings.HasPrefix(r.URL.Path, "/assets/") && r.URL.Path != "/favicon.svg" {
			apiError(w, http.StatusNotFound, "not found")
			return
		}
		static.ServeHTTP(w, r)
	})
}

func readJSON(w http.ResponseWriter, r *http.Request, target any) error {
	return readJSONLimit(w, r, target, 2<<20)
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	raw, err := json.Marshal(value)
	if err != nil {
		return
	}
	_, _ = w.Write(append(publicerrors.JSON(raw, false), '\n'))
}
