package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Each test uses the existing isolated PostgreSQL schema fixture. No upstream
// credentials, production tables, or Redis namespaces participate in this suite.
func adminKeyGatewayPGProxies(t *testing.T) (*Proxy, *Proxy, string) {
	t.Helper()
	store := accountPGTestStore(t)
	key := "ca_" + strings.Repeat("synthetic-gateway-admin-", 3)
	if _, err := store.DB.initializeAdminKey(context.Background(), key); err != nil {
		t.Fatal("cannot initialize synthetic admin key")
	}
	first := &Proxy{Store: store, Settings: NewPGSettingsStore(store.DB), AdminKey: key, startedAt: time.Now()}
	second := &Proxy{Store: store, Settings: NewPGSettingsStore(store.DB), AdminKey: key, startedAt: time.Now()}
	settings := defaultSettings()
	settings.AdminAPI = true
	if err := first.Settings.Set(settings); err != nil {
		t.Fatal("cannot initialize synthetic gateway settings")
	}
	return first, second, key
}

func TestPGAdminGatewayRotationImmediatelyRevokesOtherInstance(t *testing.T) {
	first, second, old := adminKeyGatewayPGProxies(t)
	ctx := context.Background()
	if id, _, ok := second.identifyKeyContext(ctx, old); !ok || id != "admin" {
		t.Fatal("second instance did not accept the initial admin key")
	}
	fresh, err := first.rotateAdminKey(ctx, "gateway-cross-instance-rotation")
	if err != nil || fresh == old || fresh == "" {
		t.Fatal("synthetic admin key rotation failed")
	}
	// Prove this is not passing because both proxies share an in-memory key.
	if second.adminKey() != old {
		t.Fatal("test requires a stale in-memory key on the second instance")
	}
	if _, _, ok := second.identifyKeyContext(ctx, old); ok {
		t.Fatal("other instance still authorizes the revoked admin key")
	}
	if id, _, ok := second.identifyKeyContext(ctx, fresh); !ok || id != "admin" {
		t.Fatal("other instance did not immediately accept the current admin key")
	}
	// WebSocket subsequent-turn authorization goes through candidatesFor and
	// must reject the same revoked key before selecting any upstream account.
	if _, _, _, err := second.candidatesFor(old); err == nil {
		t.Fatal("account selection accepted the revoked key")
	}
	response := webTestRequest(second.adminRoutes(), http.MethodGet, "/api/settings", "", "Bearer "+fresh)
	var settings struct {
		AdminKey       string `json:"admin_key"`
		AdminKeyMasked string `json:"admin_key_masked"`
	}
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &settings) != nil {
		t.Fatalf("current admin key could not read settings: HTTP %d", response.Code)
	}
	if settings.AdminKey != "" || settings.AdminKeyMasked != maskKey(fresh) {
		t.Fatal("settings did not display the authoritative current key mask")
	}
	if strings.Contains(response.Body.String(), fresh) || strings.Contains(response.Body.String(), old) {
		t.Fatal("settings response exposed an unmasked admin key")
	}
	response = webTestRequest(second.adminRoutes(), http.MethodGet, "/api/settings", "", "Bearer "+old)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("revoked key retained management access: HTTP %d", response.Code)
	}
}

func TestPGAdminGatewayDisabledKeepsManagementAccess(t *testing.T) {
	first, second, key := adminKeyGatewayPGProxies(t)
	settings, err := first.Settings.Get()
	if err != nil {
		t.Fatal("cannot read synthetic settings")
	}
	settings.AdminAPI = false
	if err := first.Settings.Set(settings); err != nil {
		t.Fatal("cannot disable synthetic admin gateway access")
	}
	if _, _, ok := second.identifyKeyContext(context.Background(), key); ok {
		t.Fatal("disabled admin gateway access still permits inference")
	}
	for _, endpoint := range []string{"/api/settings", "/api/session"} {
		response := webTestRequest(second.adminRoutes(), http.MethodGet, endpoint, "", "Bearer "+key)
		if response.Code != http.StatusOK {
			t.Fatalf("disabling gateway access blocked management %s: HTTP %d", endpoint, response.Code)
		}
	}
}

func TestPGAdminGatewayDatabaseFailureDoesNotTrustCachedKey(t *testing.T) {
	_, second, key := adminKeyGatewayPGProxies(t)
	if _, _, ok := second.identifyKeyContext(context.Background(), key); !ok {
		t.Fatal("could not warm the valid gateway key and settings")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, ok := second.identifyKeyContext(canceled, key); ok {
		t.Fatal("canceled authoritative lookup fell back to the cached admin key")
	}
	second.Store.DB.Pool.Close()
	if second.adminKey() != key {
		t.Fatal("test requires the previously valid key to remain cached")
	}
	if _, _, ok := second.identifyKeyContext(context.Background(), key); ok {
		t.Fatal("database outage fell back to the cached admin key")
	}
	response := webTestRequest(second.adminRoutes(), http.MethodGet, "/api/settings", "", "Bearer "+key)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("database outage did not fail management authentication closed: HTTP %d", response.Code)
	}
	var failure struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(response.Body.Bytes(), &failure) != nil || failure.Error.Code != "storage_unavailable" {
		t.Fatal("authentication storage failure omitted its stable error code")
	}
}
