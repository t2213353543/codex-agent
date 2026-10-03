package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const managedIPGeoURL = "https://get.geojs.io/v1/ip/geo.json"
const managedIPGeoTTL = 24 * time.Hour
const managedIPGeoCacheLimit = 1024
const managedIPGeoBodyLimit = 256 * 1024

type managedIPGeoEntry struct {
	IP           string `json:"ip"`
	Status       string `json:"status"`
	Country      string `json:"country,omitempty"`
	CountryCode  string `json:"country_code,omitempty"`
	Region       string `json:"region,omitempty"`
	City         string `json:"city,omitempty"`
	Organization string `json:"organization,omitempty"`
	Timezone     string `json:"timezone,omitempty"`
	Source       string `json:"source,omitempty"`
	FetchedAt    int64  `json:"fetched_at,omitempty"`
	ExpiresAt    int64  `json:"expires_at,omitempty"`
	Cached       bool   `json:"cached"`
	ErrorCode    string `json:"error_code,omitempty"`
	Error        string `json:"error,omitempty"`
}

type managedIPGeoService struct {
	client *http.Client
	now    func() time.Time
	mu     sync.Mutex
	cache  map[string]managedIPGeoEntry
	active chan struct{}
}

func newManagedIPGeoService(transport http.RoundTripper) *managedIPGeoService {
	return &managedIPGeoService{
		client: &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		now:    time.Now, cache: make(map[string]managedIPGeoEntry), active: make(chan struct{}, 4),
	}
}

var managedIPGeo = newManagedIPGeoService(http.DefaultTransport)

// Conservative public-IP policy. In addition to RFC1918/ULA, exclude CGNAT,
// documentation, benchmarking, transition and protocol-assignment blocks.
// IPv6 permits native global unicast 2000::/3 only. This is a disclosure policy,
// not a claim that every allowed address currently has a globally routed prefix.
var managedIPGeoExcluded = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"), netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"), netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3fff::/20"),
}
var managedIPGeoNativeV6 = netip.MustParsePrefix("2000::/3")

func managedIPGeoAddress(raw string) (netip.Addr, error) {
	if raw == "" || len(raw) > 64 || raw != strings.TrimSpace(raw) {
		return netip.Addr{}, errInvalid
	}
	ip, err := netip.ParseAddr(raw)
	if err != nil || ip.Zone() != "" {
		return netip.Addr{}, errInvalid
	}
	return ip.Unmap(), nil
}

func managedIPGeoPublic(ip netip.Addr) bool {
	if !ip.IsValid() || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.Is6() && !managedIPGeoNativeV6.Contains(ip) {
		return false
	}
	for _, prefix := range managedIPGeoExcluded {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}

func managedIPGeoFailure(ip, code string) managedIPGeoEntry {
	message := "GeoJS 暂时不可用，请稍后手动重试"
	switch code {
	case "invalid_response":
		message = "GeoJS 返回的数据不完整或与查询 IP 不匹配"
	case "not_found":
		message = "GeoJS 未返回此 IP 的归属地"
	case "response_too_large":
		message = "GeoJS 返回的数据超过大小限制"
	}
	return managedIPGeoEntry{IP: ip, Status: "error", Source: "GeoJS", ErrorCode: code, Error: message}
}

func (s *managedIPGeoService) cached(ip string, now int64) (managedIPGeoEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.cache[ip]
	if !ok || entry.ExpiresAt <= now || entry.FetchedAt > now {
		delete(s.cache, ip)
		return managedIPGeoEntry{}, false
	}
	entry.Cached = true
	return entry, true
}

func (s *managedIPGeoService) remember(entry managedIPGeoEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ip, old := range s.cache {
		if old.ExpiresAt <= entry.FetchedAt || old.FetchedAt > entry.FetchedAt {
			delete(s.cache, ip)
		}
	}
	if _, present := s.cache[entry.IP]; !present && len(s.cache) >= managedIPGeoCacheLimit {
		oldestIP, oldest := "", int64(1<<63-1)
		for ip, old := range s.cache {
			if old.FetchedAt < oldest || old.FetchedAt == oldest && ip < oldestIP {
				oldestIP, oldest = ip, old.FetchedAt
			}
		}
		delete(s.cache, oldestIP)
	}
	s.cache[entry.IP] = entry
}

func (s *managedIPGeoService) lookup(ctx context.Context, raw []string, force bool) ([]managedIPGeoEntry, error) {
	if len(raw) < 1 || len(raw) > 50 {
		return nil, fmt.Errorf("%w: provide 1 to 50 IP addresses", errInvalid)
	}
	ips := make([]netip.Addr, 0, len(raw))
	seen := make(map[netip.Addr]bool)
	// Validate the whole batch before any outbound request; a hostname, port,
	// zone or malformed literal must never cause a partial disclosure.
	for _, value := range raw {
		ip, err := managedIPGeoAddress(value)
		if err != nil {
			return nil, fmt.Errorf("%w: only bare IPv4 or IPv6 addresses are accepted", errInvalid)
		}
		if !seen[ip] {
			seen[ip] = true
			ips = append(ips, ip)
		}
	}
	out, pending := make([]managedIPGeoEntry, len(ips)), []string{}
	now := s.now().Unix()
	for i, ip := range ips {
		text := ip.String()
		if !managedIPGeoPublic(ip) {
			out[i] = managedIPGeoEntry{IP: text, Status: "private", ErrorCode: "non_public", Error: "私有、保留或非公网地址，不发送至 GeoJS"}
		} else if entry, ok := s.cached(text, now); ok && !force {
			out[i] = entry
		} else {
			out[i] = managedIPGeoFailure(text, "upstream_unavailable")
			pending = append(pending, text)
		}
	}
	if len(pending) == 0 {
		return out, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	select {
	case s.active <- struct{}{}:
		defer func() { <-s.active }()
	case <-ctx.Done():
		return out, nil
	}
	entries, code := s.fetch(ctx, pending)
	for i := range out {
		if out[i].Status != "error" {
			continue
		}
		if code != "" {
			out[i] = managedIPGeoFailure(out[i].IP, code)
		} else if entry, found := entries[out[i].IP]; found {
			out[i] = entry
			if entry.Status == "success" {
				s.remember(entry)
			}
		} else {
			out[i] = managedIPGeoFailure(out[i].IP, "not_found")
		}
	}
	return out, nil
}

func (s *managedIPGeoService) fetch(ctx context.Context, ips []string) (map[string]managedIPGeoEntry, string) {
	values := url.Values{"ip": []string{strings.Join(ips, ",")}}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, managedIPGeoURL+"?"+values.Encode(), nil)
	if err != nil {
		return nil, "upstream_unavailable"
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "codex-agent-management-ip-geo")
	// No administrator cookie, account credential, log content or IP headers
	// are copied. The only caller data is the validated IP query list.
	response, err := s.client.Do(request)
	if err != nil {
		return nil, "upstream_unavailable"
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, "upstream_unavailable"
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, managedIPGeoBodyLimit+1))
	if err != nil {
		return nil, "upstream_unavailable"
	}
	if len(body) > managedIPGeoBodyLimit {
		return nil, "response_too_large"
	}
	type geoResult struct {
		IP                 string `json:"ip"`
		Country            string `json:"country"`
		CountryCode        string `json:"country_code"`
		Region             string `json:"region"`
		City               string `json:"city"`
		Organization       string `json:"organization_name"`
		LegacyOrganization string `json:"organization"`
		Timezone           string `json:"timezone"`
	}
	body = bytes.TrimSpace(body)
	var rows []geoResult
	if len(body) > 0 && body[0] == '{' && len(ips) == 1 {
		var row geoResult
		err = json.Unmarshal(body, &row)
		rows = []geoResult{row}
	} else {
		err = json.Unmarshal(body, &rows)
	}
	if err != nil || len(rows) > len(ips) {
		return nil, "invalid_response"
	}
	wanted, results := map[string]bool{}, map[string]managedIPGeoEntry{}
	for _, ip := range ips {
		wanted[ip] = true
	}
	checked := s.now().Unix()
	for _, row := range rows {
		ip, err := managedIPGeoAddress(row.IP)
		if err != nil {
			return nil, "invalid_response"
		}
		key := ip.String()
		if _, duplicate := results[key]; duplicate || !wanted[key] {
			return nil, "invalid_response"
		}
		if row.CountryCode == "" {
			results[key] = managedIPGeoFailure(key, "not_found")
			continue
		}
		if len(row.CountryCode) != 2 || row.CountryCode[0] < 'A' || row.CountryCode[0] > 'Z' || row.CountryCode[1] < 'A' || row.CountryCode[1] > 'Z' {
			results[key] = managedIPGeoFailure(key, "invalid_response")
			continue
		}
		if row.Organization == "" {
			row.Organization = row.LegacyOrganization
		}
		if strings.EqualFold(row.Organization, "Unknown") {
			row.Organization = ""
		}
		valid := true
		for _, field := range []struct {
			value string
			limit int
		}{{row.Country, 100}, {row.Region, 200}, {row.City, 200}, {row.Organization, 300}, {row.Timezone, 100}} {
			if !utf8.ValidString(field.value) || len(field.value) > field.limit || strings.IndexFunc(field.value, unicode.IsControl) >= 0 {
				valid = false
			}
		}
		if !valid {
			results[key] = managedIPGeoFailure(key, "invalid_response")
			continue
		}
		results[key] = managedIPGeoEntry{IP: key, Status: "success", Country: strings.TrimSpace(row.Country), CountryCode: row.CountryCode, Region: strings.TrimSpace(row.Region), City: strings.TrimSpace(row.City), Organization: strings.TrimSpace(row.Organization), Timezone: strings.TrimSpace(row.Timezone), Source: "GeoJS", FetchedAt: checked, ExpiresAt: checked + int64(managedIPGeoTTL/time.Second)}
	}
	return results, ""
}

func (p *Proxy) handleManagedIPGeo(w http.ResponseWriter, r *http.Request) {
	var request struct {
		IPs   []string `json:"ips"`
		Force bool     `json:"force"`
	}
	if err := readJSON(w, r, &request); err != nil {
		adminError(w, err)
		return
	}
	items, err := managedIPGeo.lookup(r.Context(), request.IPs, request.Force)
	if err != nil {
		adminError(w, err)
		return
	}
	writeJSON(w, map[string]any{"items": items, "source": "GeoJS", "cache_ttl_seconds": int64(managedIPGeoTTL / time.Second)})
}

func (p *Proxy) registerManagedIPGeoRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/logs/ip-geo", p.handleManagedIPGeo)
}
