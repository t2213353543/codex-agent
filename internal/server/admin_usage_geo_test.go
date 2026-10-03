package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type managedGeoRoundTripper func(*http.Request) (*http.Response, error)

func (f managedGeoRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func managedGeoResponse(body string) *http.Response {
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}
func managedGeoFixture(ip string) string {
	body, _ := json.Marshal(map[string]any{"ip": ip, "country": "United States", "country_code": "US", "region": "Synthetic region", "city": "Synthetic city", "organization_name": "Synthetic organization", "organization": "AS000 Legacy organization", "timezone": "Etc/UTC"})
	return string(body)
}

func TestManagedIPGeoAddressPolicy(t *testing.T) {
	for _, raw := range []string{"", "localhost", "example.com", "8.8.8.8:443", "[2001:4860:4860::8888]", "[2001:4860:4860::8888]:443", "fe80::1%eth0", "2001:4860::1%zone", " 8.8.8.8", "8.8.8.8\n", "8.8.8.999", "010.0.0.1", "2130706433", "http://8.8.8.8", "8.8.8.8,1.1.1.1"} {
		if _, err := managedIPGeoAddress(raw); err == nil {
			t.Errorf("accepted invalid literal %q", raw)
		}
	}
	private := []string{"0.0.0.0", "0.8.8.8", "10.1.2.3", "100.64.0.1", "100.127.255.255", "127.0.0.1", "169.254.169.254", "172.16.0.1", "172.31.255.255", "192.168.1.1", "192.0.0.9", "192.0.2.2", "192.88.99.1", "198.18.0.1", "198.19.255.255", "198.51.100.2", "203.0.113.2", "224.1.1.1", "240.1.1.1", "255.255.255.255", "::", "::1", "::ffff:192.168.1.1", "64:ff9b::808:808", "100::1", "fc00::1", "fd00::1", "fe80::1", "fec0::1", "ff02::1", "2001::1", "2001:2::1", "2001:db8::1", "2002:0808:0808::1", "3fff::1", "5f00::1"}
	for _, raw := range private {
		ip, err := managedIPGeoAddress(raw)
		if err != nil || managedIPGeoPublic(ip) {
			t.Errorf("would disclose private/reserved literal %q", raw)
		}
	}
	for _, raw := range []string{"8.8.8.8", "1.1.1.1", "100.63.255.255", "100.128.0.1", "172.15.255.255", "172.32.0.1", "::ffff:8.8.8.8", "2001:4860:4860::8888", "2606:4700:4700::1111"} {
		ip, err := managedIPGeoAddress(raw)
		if err != nil || !managedIPGeoPublic(ip) {
			t.Errorf("rejected ordinary public literal %q", raw)
		}
	}
}

func TestManagedIPGeoExplicitRequestCacheAndBounds(t *testing.T) {
	var calls int
	now := time.Unix(1700000000, 0)
	service := newManagedIPGeoService(managedGeoRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != "GET" || r.URL.Scheme != "https" || r.URL.Host != "get.geojs.io" || r.URL.Path != "/v1/ip/geo.json" || r.URL.Query().Get("ip") != "8.8.8.8" || len(r.URL.Query()) != 1 || r.Body != nil {
			t.Fatal("unexpected outbound request", r.Method, r.URL.String())
		}
		for _, name := range []string{"Authorization", "Cookie", "X-Forwarded-For", "X-API-Key", "Referer"} {
			if r.Header.Get(name) != "" {
				t.Fatalf("forwarded private header %s", name)
			}
		}
		if _, ok := r.Context().Deadline(); !ok {
			t.Fatal("request has no deadline")
		}
		return managedGeoResponse(managedGeoFixture("8.8.8.8")), nil
	}))
	service.now = func() time.Time { return now }
	ctx := context.Background()
	items, err := service.lookup(ctx, []string{"8.8.8.8", "::ffff:8.8.8.8", "10.0.0.1"}, false)
	if err != nil || len(items) != 2 || calls != 1 || items[0].Status != "success" || items[0].Cached || items[0].Organization != "Synthetic organization" || items[0].Source != "GeoJS" || items[0].FetchedAt != now.Unix() || items[0].ExpiresAt != now.Add(24*time.Hour).Unix() || items[1].Status != "private" || items[1].FetchedAt != 0 || items[1].Source != "" {
		t.Fatalf("unexpected lookup: %+v %v", items, err)
	}
	now = now.Add(time.Hour)
	items, _ = service.lookup(ctx, []string{"8.8.8.8"}, false)
	if calls != 1 || !items[0].Cached || items[0].FetchedAt == now.Unix() {
		t.Fatal("cache falsely refreshed its source time")
	}
	items, _ = service.lookup(ctx, []string{"8.8.8.8"}, true)
	if calls != 2 || items[0].Cached || items[0].FetchedAt != now.Unix() {
		t.Fatal("explicit force did not query")
	}
	now = now.Add(24 * time.Hour)
	items, _ = service.lookup(ctx, []string{"8.8.8.8"}, false)
	if calls != 3 || items[0].Cached {
		t.Fatal("expired entry reused")
	}
	for _, raw := range [][]string{nil, {"8.8.8.8", "hostname.example"}, make([]string, 51)} {
		if _, err = service.lookup(ctx, raw, true); err == nil {
			t.Fatal("invalid batch accepted")
		}
	}
	items, err = service.lookup(ctx, []string{"192.168.1.1", "::ffff:127.0.0.1", "2001:db8::1"}, true)
	if err != nil || len(items) != 3 || calls != 3 {
		t.Fatal("invalid/private batch caused outbound request")
	}
	for i := 0; i < managedIPGeoCacheLimit+30; i++ {
		service.remember(managedIPGeoEntry{IP: fmt.Sprintf("cache-fixture-%d", i), Status: "success", FetchedAt: now.Unix() + int64(i), ExpiresAt: now.Unix() + int64(i) + 86400})
	}
	if len(service.cache) != managedIPGeoCacheLimit {
		t.Fatalf("cache unbounded: %d", len(service.cache))
	}
	if _, found := service.cache["cache-fixture-0"]; found {
		t.Fatal("oldest cache entry not evicted")
	}
}

func TestManagedIPGeoBatchMappingAndFailures(t *testing.T) {
	cases := []struct {
		name, body, code string
		status           int
	}{
		{"wrong-ip", managedGeoFixture("1.1.1.1"), "invalid_response", 200},
		{"malformed-json", `{"ip":`, "invalid_response", 200},
		{"missing-country", `{"ip":"8.8.8.8"}`, "not_found", 200},
		{"invalid-country", `{"ip":"8.8.8.8","country_code":"<script>"}`, "invalid_response", 200},
		{"too-long-country", `{"ip":"8.8.8.8","country_code":"US","country":"` + strings.Repeat("x", 101) + `"}`, "invalid_response", 200},
		{"control-char", `{"ip":"8.8.8.8","country_code":"US","city":"hidden\nvalue"}`, "invalid_response", 200},
		{"oversize", strings.Repeat("x", managedIPGeoBodyLimit+1), "response_too_large", 200},
		{"upstream-error", `Bearer private-provider-text`, "upstream_unavailable", 503},
		{"redirect", "", "upstream_unavailable", 302},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			calls := 0
			service := newManagedIPGeoService(managedGeoRoundTripper(func(r *http.Request) (*http.Response, error) {
				calls++
				response := managedGeoResponse(item.body)
				response.StatusCode = item.status
				response.Header.Set("Location", "http://127.0.0.1/private")
				return response, nil
			}))
			items, err := service.lookup(context.Background(), []string{"8.8.8.8"}, false)
			if err != nil || len(items) != 1 || items[0].Status != "error" || items[0].ErrorCode != item.code || items[0].FetchedAt != 0 || len(service.cache) != 0 || calls != 1 || strings.Contains(items[0].Error, "private-provider") {
				t.Fatalf("failure misreported/cached/redirected: %+v %v calls=%d", items, err, calls)
			}
		})
	}
	t.Run("out-of-order-and-missing", func(t *testing.T) {
		service := newManagedIPGeoService(managedGeoRoundTripper(func(r *http.Request) (*http.Response, error) {
			if r.URL.Query().Get("ip") != "8.8.8.8,1.1.1.1,9.9.9.9" {
				t.Fatal("batch query did not contain requested IPs")
			}
			return managedGeoResponse("[" + managedGeoFixture("1.1.1.1") + "," + managedGeoFixture("8.8.8.8") + "]"), nil
		}))
		items, err := service.lookup(context.Background(), []string{"8.8.8.8", "1.1.1.1", "9.9.9.9"}, false)
		if err != nil || items[0].IP != "8.8.8.8" || items[1].IP != "1.1.1.1" || items[0].Status != "success" || items[1].Status != "success" || items[2].Status != "error" || items[2].ErrorCode != "not_found" {
			t.Fatal(items, err)
		}
	})
	for _, body := range []string{"[" + managedGeoFixture("8.8.8.8") + "," + managedGeoFixture("8.8.8.8") + "]", "[" + managedGeoFixture("8.8.8.8") + "," + managedGeoFixture("9.9.9.9") + "]"} {
		service := newManagedIPGeoService(managedGeoRoundTripper(func(*http.Request) (*http.Response, error) { return managedGeoResponse(body), nil }))
		items, err := service.lookup(context.Background(), []string{"8.8.8.8", "1.1.1.1"}, false)
		if err != nil || items[0].Status != "error" || items[1].Status != "error" || len(service.cache) != 0 {
			t.Fatal("mismatched batch was cached", items, err)
		}
	}
	t.Run("network-error-and-cancellation", func(t *testing.T) {
		service := newManagedIPGeoService(managedGeoRoundTripper(func(*http.Request) (*http.Response, error) { return nil, errors.New("do not expose private endpoint") }))
		items, err := service.lookup(context.Background(), []string{"8.8.8.8"}, false)
		if err != nil || items[0].Status != "error" || strings.Contains(items[0].Error, "private endpoint") {
			t.Fatal(items, err)
		}
		service = newManagedIPGeoService(managedGeoRoundTripper(func(r *http.Request) (*http.Response, error) { <-r.Context().Done(); return nil, r.Context().Err() }))
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
		defer cancel()
		items, err = service.lookup(ctx, []string{"8.8.8.8"}, false)
		if err != nil || items[0].Status != "error" || len(service.cache) != 0 {
			t.Fatal("canceled request succeeded")
		}
	})
}

func TestManagedIPGeoConcurrentBoundedQueries(t *testing.T) {
	var active, maximum atomic.Int32
	service := newManagedIPGeoService(managedGeoRoundTripper(func(r *http.Request) (*http.Response, error) {
		current := active.Add(1)
		defer active.Add(-1)
		for old := maximum.Load(); current > old && !maximum.CompareAndSwap(old, current); old = maximum.Load() {
		}
		time.Sleep(5 * time.Millisecond)
		return managedGeoResponse(managedGeoFixture(r.URL.Query().Get("ip"))), nil
	}))
	var done sync.WaitGroup
	for i := 1; i <= 20; i++ {
		done.Add(1)
		go func(i int) {
			defer done.Done()
			entries, err := service.lookup(context.Background(), []string{fmt.Sprintf("8.8.4.%d", i)}, false)
			if err != nil || len(entries) != 1 || entries[0].Status != "success" {
				t.Error("concurrent query failed", err)
			}
		}(i)
	}
	done.Wait()
	if maximum.Load() > 4 || len(service.cache) != 20 {
		t.Fatalf("concurrency/cache mismatch: %d %d", maximum.Load(), len(service.cache))
	}
}

func TestManagedIPGeoManagementAuthentication(t *testing.T) {
	_, handler := webTestSetup(t)
	request := httptest.NewRequest(http.MethodPost, "/api/logs/ip-geo", strings.NewReader(`{"ips":["127.0.0.1"]}`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 401 {
		t.Fatalf("anonymous geo query returned %d", response.Code)
	}
	response = managedTestRequest(t, handler, "POST", "/api/logs/ip-geo", map[string]any{"ips": []string{"127.0.0.1"}}, 200)
	if !strings.Contains(response.Body.String(), `"status":"private"`) || strings.Contains(response.Body.String(), "fetched_at") {
		t.Fatal("private literal reported as GeoJS success")
	}
	managedTestRequest(t, handler, "POST", "/api/logs/ip-geo", map[string]any{"ips": []string{"hostname.example"}}, 400)
	managedTestRequest(t, handler, "POST", "/api/logs/ip-geo", map[string]any{"ips": []string{"127.0.0.1"}, "url": "https://different.example"}, 400)
}
