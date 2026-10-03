package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func compatAssertError(t *testing.T, w *httptest.ResponseRecorder, path string, want int) {
	t.Helper()
	if w.Code != want {
		t.Fatalf("%s: status %d, want %d: %s", path, w.Code, want, w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("management error headers: %v", w.Header())
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("management error is not JSON: %v", err)
	}
	if strings.HasPrefix(path, "/api/v1/admin/") {
		var code int
		var message string
		if json.Unmarshal(body["code"], &code) != nil || code != want || json.Unmarshal(body["message"], &message) != nil || message == "" {
			t.Fatalf("Sub2API error envelope: %s", w.Body.String())
		}
	} else {
		var message string
		if json.Unmarshal(body["error"], &message) != nil || message == "" {
			t.Fatalf("CPA error envelope: %s", w.Body.String())
		}
	}
}

func TestAdminCompatAuthenticationHeaders(t *testing.T) {
	p, _ := webTestSetup(t)
	for _, api := range []struct{ name, path, header, other string }{
		{"sub2api", "/api/v1/admin/accounts", "X-API-Key", "X-Management-Key"},
		{"cpa", "/v0/management/auth-files", "X-Management-Key", "X-API-Key"},
	} {
		t.Run(api.name, func(t *testing.T) {
			for _, tc := range []struct {
				name    string
				headers http.Header
				ok      bool
			}{
				{"bearer", http.Header{"Authorization": {"Bearer " + webTestKey}}, true},
				{"lowercase_bearer", http.Header{"authorization": {"bEaReR " + webTestKey}}, true},
				{"alternate", http.Header{api.header: {webTestKey}}, true},
				{"alternate_lowercase", http.Header{strings.ToLower(api.header): {webTestKey}}, true},
				{"same_bearer_and_alternate", http.Header{"Authorization": {"Bearer " + webTestKey}, api.header: {webTestKey}}, true},
				{"missing", http.Header{}, false},
				{"wrong_scope_header", http.Header{api.other: {webTestKey}}, false},
				{"wrong_bearer", http.Header{"Authorization": {"Bearer incorrect"}}, false},
				{"wrong_alternate", http.Header{api.header: {"incorrect"}}, false},
				{"conflicting_credentials", http.Header{"Authorization": {"Bearer " + webTestKey}, api.header: {"incorrect"}}, false},
				{"invalid_bearer_cannot_fallback", http.Header{"Authorization": {"Basic bad"}, api.header: {webTestKey}}, false},
				{"empty_bearer_cannot_fallback", http.Header{"Authorization": {""}, api.header: {webTestKey}}, false},
				{"valueless_bearer_cannot_fallback", http.Header{"Authorization": nil, api.header: {webTestKey}}, false},
				{"duplicate_bearer", http.Header{"Authorization": {"Bearer " + webTestKey, "Bearer " + webTestKey}}, false},
				{"case_duplicate_bearer", http.Header{"Authorization": {"Bearer " + webTestKey}, "authorization": {"Bearer incorrect"}}, false},
				{"empty_alternate", http.Header{api.header: {""}}, false},
				{"valueless_alternate", http.Header{api.header: nil}, false},
				{"bearer_with_valueless_alternate", http.Header{"Authorization": {"Bearer " + webTestKey}, api.header: nil}, false},
				{"bearer_with_empty_alternate", http.Header{"Authorization": {"Bearer " + webTestKey}, api.header: {""}}, false},
				{"duplicate_alternate", http.Header{api.header: {webTestKey, webTestKey}}, false},
				{"case_duplicate_alternate", http.Header{api.header: {webTestKey}, strings.ToLower(api.header): {"incorrect"}}, false},
				{"joined_alternate", http.Header{api.header: {webTestKey + "," + webTestKey}}, false},
				{"newline_alternate", http.Header{api.header: {"bad\r\n" + webTestKey}}, false},
				{"nul_alternate", http.Header{api.header: {webTestKey + "\x00"}}, false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					r := httptest.NewRequest(http.MethodGet, api.path, nil)
					r.Header = tc.headers.Clone()
					r.Header.Set("Origin", "https://foreign-admin.invalid")
					r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "ambient-session"})
					w := httptest.NewRecorder()
					if got := p.authenticateCompat(w, r); got != tc.ok {
						t.Fatalf("authentication = %t, want %t", got, tc.ok)
					}
					if !tc.ok {
						compatAssertError(t, w, api.path, http.StatusUnauthorized)
						return
					}
					auth, ok := r.Context().Value(adminAuthenticationContextKey{}).(adminAuthentication)
					if !ok || auth.BearerHash != adminKeyHash(webTestKey) || auth.CookieToken != "" || auth.Endpoint != api.path {
						t.Fatal("compatibility authentication lost the explicit management identity")
					}
				})
			}
		})
	}
}

func TestAdminCompatNoGatewayOrCookieAuthorization(t *testing.T) {
	p, handler := webTestSetup(t)
	key, err := p.Keys.Create("gateway-only", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/v1/admin/accounts", "/v0/management/auth-files"} {
		for _, headers := range []http.Header{
			{"Authorization": {"Bearer " + key.Key}},
			{"X-API-Key": {key.Key}, "X-Management-Key": {key.Key}},
			{},
		} {
			r := httptest.NewRequest(http.MethodGet, path, nil)
			r.Header = headers.Clone()
			r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "ambient-session"})
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			compatAssertError(t, w, path, http.StatusUnauthorized)
			if strings.Contains(w.Body.String(), key.Key) || strings.Contains(w.Body.String(), webTestKey) {
				t.Fatal("authentication error exposed key material")
			}
		}
	}
	// Foreign authentication headers are scoped to their compatibility namespace.
	for _, path := range []string{"/api/accounts", "/api/settings", "/api/session", "/models", "/v1/models"} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("X-Management-Key", webTestKey)
		r.Header.Set("X-API-Key", webTestKey)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("foreign headers authorized %s: %d", path, w.Code)
		}
	}
}

func TestAdminCompatRouteErrorsAndManagementSwitch(t *testing.T) {
	p, handler := webTestSetup(t)
	settings, err := p.Settings.Latest()
	if err != nil {
		t.Fatal(err)
	}
	settings.AdminAPI = false
	if err := p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
	for _, api := range []struct{ base, list, header string }{
		{"/api/v1/admin", "/accounts", "X-API-Key"},
		{"/v0/management", "/auth-files", "X-Management-Key"},
	} {
		for _, tc := range []struct {
			method, suffix string
			want           int
		}{
			{http.MethodGet, api.list, http.StatusOK},
			{http.MethodGet, "/does-not-exist", http.StatusNotFound},
			{http.MethodOptions, api.list, http.StatusMethodNotAllowed},
		} {
			path := api.base + tc.suffix
			r := httptest.NewRequest(tc.method, path, nil)
			r.Header.Set(api.header, webTestKey)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if tc.want == http.StatusOK {
				if w.Code != http.StatusOK || !json.Valid(w.Body.Bytes()) || w.Header().Get("Cache-Control") != "no-store" {
					t.Fatalf("gateway admin switch disabled %s: %d %s", path, w.Code, w.Body.String())
				}
			} else {
				compatAssertError(t, w, path, tc.want)
				if tc.want == http.StatusMethodNotAllowed && !strings.Contains(w.Header().Get("Allow"), http.MethodGet) {
					t.Fatalf("missing Allow on %s: %v", path, w.Header())
				}
			}
			if tc.want != http.StatusOK {
				// Unknown routes and unsupported methods still require authentication.
				r = httptest.NewRequest(tc.method, path, nil)
				w = httptest.NewRecorder()
				handler.ServeHTTP(w, r)
				compatAssertError(t, w, path, http.StatusUnauthorized)
			}
		}
	}
}

func TestAdminCompatReadJSONRejectsAmbiguousBodies(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"duplicate_disabled", `{"disabled":true,"disabled":false}`},
		{"duplicate_credentials", `{"credentials":{"access_token":"one","access_token":"two"}}`},
		{"escaped_duplicate", `{"disabled":true,"disabl\u0065d":false}`},
		{"array_duplicate", `{"accounts":[{"name":"first","name":"second"}]}`},
		{"duplicate_unknown", `{"metadata":{"foreign":1,"foreign":2}}`},
		{"too_deep_objects", strings.Repeat(`{"nested":`, 66) + `0` + strings.Repeat(`}`, 66)},
		{"too_deep_arrays", `{"values":` + strings.Repeat(`[`, 66) + `0` + strings.Repeat(`]`, 66) + `}`},
		{"empty", ``},
		{"null", `null`},
		{"array", `[]`},
		{"multiple_values", `{} {}`},
		{"trailing_garbage", `{}x`},
		{"truncated", `{"credentials":`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body map[string]json.RawMessage
			r := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts", strings.NewReader(tc.body))
			w := httptest.NewRecorder()
			if err := compatReadJSON(w, r, &body, 4096); !errors.Is(err, errInvalid) {
				t.Fatalf("ambiguous body: %v", err)
			}
			if body != nil {
				t.Fatal("rejected JSON populated mutation input")
			}
		})
	}
	for _, body := range []string{
		`{"credentials":{"access_token":"one"},"metadata":{"access_token":"two"}}`,
		`{"disabled":false,"priority":0,"foreign":null,"accounts":[{"name":"a"},{"name":"b"}]}`,
	} {
		var decoded map[string]json.RawMessage
		r := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts", strings.NewReader(body))
		if err := compatReadJSON(httptest.NewRecorder(), r, &decoded, int64(len(body))); err != nil || decoded == nil {
			t.Fatalf("valid compatibility JSON rejected: %v", err)
		}
	}
	var decoded map[string]json.RawMessage
	r := httptest.NewRequest(http.MethodPost, "/v0/management/auth-files", strings.NewReader(`{"access_token":"`+strings.Repeat("a", 100)+`"}`))
	w := httptest.NewRecorder()
	err := compatReadJSON(w, r, &decoded, 32)
	var tooLarge *http.MaxBytesError
	if !errors.As(err, &tooLarge) || decoded != nil {
		t.Fatalf("oversize body was not stopped before mutation: %v", err)
	}
	compatError(w, r, err)
	compatAssertError(t, w, r.URL.Path, http.StatusRequestEntityTooLarge)
}

func TestAdminCompatRevokedKeyCannotAuthenticate(t *testing.T) {
	p, _ := webTestSetup(t)
	old := webTestKey
	newKey, err := p.rotateAdminKey(context.Background(), "compat-auth-rotation")
	if err != nil {
		t.Fatal(err)
	}
	for _, api := range []struct{ path, header string }{
		{"/api/v1/admin/accounts", "X-API-Key"},
		{"/v0/management/auth-files", "X-Management-Key"},
	} {
		for _, key := range []string{old, newKey} {
			r := httptest.NewRequest(http.MethodGet, api.path, nil)
			r.Header.Set(api.header, key)
			w := httptest.NewRecorder()
			if got := p.authenticateCompat(w, r); got != (key == newKey) {
				t.Fatalf("compatibility key rotation authorization mismatch: %t", got)
			}
			if key == old {
				compatAssertError(t, w, api.path, http.StatusUnauthorized)
			}
		}
	}
}
