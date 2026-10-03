package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func externalLeaseFixture(name string, expiry any, pause any) map[string]any {
	return map[string]any{"name": name, "platform": "openai", "type": "oauth", "expires_at": expiry, "auto_pause_on_expired": pause, "credentials": map[string]any{"access_token": "synthetic-external-" + name, "refresh_token": "synthetic-refresh-" + name, "expires_at": 2100000000}}
}
func marshalExternalFixture(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestManagedExternalLeaseParsing(t *testing.T) {
	for _, item := range []struct {
		name           string
		expiry         any
		pause          any
		want           int64
		format, source string
		auto           bool
	}{
		{"unix", int64(2000000000), false, 2000000000, "unix_seconds", "explicit", false},
		{"date", "2033-05-18T11:33:20+08:00", true, 2000000000, "rfc3339", "explicit", true},
		{"fractional-date", "2033-05-18T03:33:20.999Z", nil, 2000000000, "rfc3339", "sub2api_default", true},
		{"null", nil, nil, 0, "unlimited", "sub2api_default", true},
		{"zero", 0, false, 0, "unlimited", "explicit", false},
	} {
		t.Run(item.name, func(t *testing.T) {
			lease, err := parseManagedExternalLease(marshalExternalFixture(t, externalLeaseFixture(item.name, item.expiry, item.pause)))
			if err != nil || lease == nil || lease.ExpiresAt != item.want || lease.ExpiryFormat != item.format || lease.AutoDisableSource != item.source || lease.AutoDisable != item.auto || lease.RestoreOnExtend || lease.Source != managedExternalLeaseSource {
				t.Fatalf("lease contract: %+v %v", lease, err)
			}
		})
	}
	for _, item := range []struct {
		name string
		edit func(map[string]any)
	}{
		{"negative", func(v map[string]any) { v["expires_at"] = -1 }},
		{"too-large", func(v map[string]any) { v["expires_at"] = int64(253402300800) }},
		{"milliseconds", func(v map[string]any) { v["expires_at"] = int64(2000000000000) }},
		{"float", func(v map[string]any) { v["expires_at"] = 1.5 }},
		{"string-number", func(v map[string]any) { v["expires_at"] = "2000000000" }},
		{"bad-date", func(v map[string]any) { v["expires_at"] = "tomorrow" }},
		{"bare-date", func(v map[string]any) { v["expires_at"] = "2033-05-18" }},
		{"boolean-expiry", func(v map[string]any) { v["expires_at"] = true }},
		{"bad-pause", func(v map[string]any) { v["auto_pause_on_expired"] = "true" }},
		{"numeric-pause", func(v map[string]any) { v["auto_pause_on_expired"] = 1 }},
		{"no-credentials", func(v map[string]any) { delete(v, "credentials"); v["access_token"] = "synthetic-token" }},
	} {
		t.Run(item.name, func(t *testing.T) {
			value := externalLeaseFixture(item.name, 2000000000, true)
			item.edit(value)
			_, err := parseManagedExternalLease(marshalExternalFixture(t, value))
			var typed *accountImportError
			if !errors.As(err, &typed) || typed.code != "invalid_external_lease" {
				t.Fatal("invalid outer lease not rejected", err)
			}
		})
	}
	for _, value := range []any{
		map[string]any{"access_token": "synthetic-only-token", "expires_at": 2000000000},
		map[string]any{"tokens": map[string]any{"access_token": "synthetic-only-token", "expires_at": 2000000000}},
		map[string]any{"name": "untyped", "credentials": map[string]any{"access_token": "synthetic-only-token", "expires_at": 2000000000}},
		map[string]any{"platform": "openai", "expires_at": 2000000000, "credentials": map[string]any{"access_token": "synthetic-only-token"}},
		map[string]any{"type": "oauth", "expires_at": 2000000000, "credentials": map[string]any{"access_token": "synthetic-only-token"}},
	} {
		if lease, err := parseManagedExternalLease(marshalExternalFixture(t, value)); err != nil || lease != nil {
			t.Fatal("untyped credential promoted into lease", lease, err)
		}
	}
	inner := externalLeaseFixture("nested", 2000000000, false)
	wrapped := map[string]any{"name": "API-name", "credentials": inner}
	if lease, err := parseManagedExternalLease(marshalExternalFixture(t, wrapped)); err != nil || lease == nil || lease.ExpiresAt != 2000000000 {
		t.Fatal("typed inner API wrapper lost", err)
	}
	delete(inner, "auto_pause_on_expired")
	delete(inner, "expires_at")
	if lease, err := parseManagedExternalLease(marshalExternalFixture(t, inner)); err != nil || lease != nil {
		t.Fatal("token expiry mistaken for outer lease", lease, err)
	}
}

type externalLeaseImportResponse struct {
	ImportReport
	Leases  []managedExternalLease     `json:"external_leases"`
	Receipt managedLeaseRestoreReceipt `json:"lease_restore"`
	Source  string                     `json:"lease_restore_source"`
}

func TestManagedExternalLeaseImport(t *testing.T) {
	exerciseManagedExternalLeaseImport(t, webTestSetup)
}
func TestPGManagedExternalLeaseImport(t *testing.T) {
	exerciseManagedExternalLeaseImport(t, adminPGTestProxy)
}
func exerciseManagedExternalLeaseImport(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	p, h := setup(t)
	ctx := context.Background()
	for _, form := range []string{"raw", "accounts", "credentials"} {
		t.Run(form, func(t *testing.T) {
			future := time.Now().Add(24 * time.Hour).Unix()
			row := externalLeaseFixture("external-"+form, future, false)
			body := map[string]any{"include_external_leases": true}
			switch form {
			case "raw":
				body["raw"] = string(marshalExternalFixture(t, map[string]any{"type": "sub2api-data", "version": 1, "accounts": []any{row}, "proxies": []any{}}))
			case "accounts":
				body["accounts"] = []any{row}
			case "credentials":
				body["name"] = row["name"]
				body["credentials"] = row
			}
			r := managedTestRequest(t, h, "POST", "/api/accounts/import/preview", body, 200)
			var response externalLeaseImportResponse
			if json.Unmarshal(r.Body.Bytes(), &response) != nil || len(response.Leases) != 1 || response.Source != managedExternalLeaseSource || response.Receipt.ID != "" || response.Receipt.Items[0].Status != "pending_import" {
				t.Fatal("preview contract", r.Body.String())
			}
			if _, err := p.Store.GetMetadata(row["name"].(string)); !errors.Is(err, errNotFound) {
				t.Fatal("preview wrote account")
			}
			if !stringIn("external_lease_pending_confirmation", response.Results[0].Warnings...) || stringIn("account_expiry_not_imported", response.Results[0].Warnings...) || stringIn("sub2api_settings_not_imported", response.Results[0].Warnings...) {
				t.Fatal("processed warnings inaccurate", response.Results)
			}
			r = managedTestRequest(t, h, "POST", "/api/accounts", body, 200)
			if json.Unmarshal(r.Body.Bytes(), &response) != nil || response.Receipt.ID == "" || response.Receipt.Items[0].Status != "pending" {
				t.Fatal("real receipt missing", r.Body.String())
			}
			item := response.Receipt.Items[0]
			current, err := p.Store.Get(item.ID)
			if err != nil || current.ID != response.Results[0].ID || current.ExpiresAt != 2100000000 {
				t.Fatal("token expiry/target changed", err)
			}
			if _, err = p.getManagedLease(ctx, item.ID); !errors.Is(err, errNotFound) {
				t.Fatal("lease auto-applied during account import")
			}
			request := map[string]any{"receipt_id": response.Receipt.ID, "items": []managedLeaseRestoreTarget{{item.AccountIndex, item.ID, item.AccountVersion, item.LeaseVersion}}, "confirm_restore": true}
			managedTestRequest(t, h, "POST", "/api/accounts/import-leases", request, 200)
			lease, err := p.getManagedLease(ctx, item.ID)
			if err != nil || lease.ExpiresAt != future || lease.AutoDisable || lease.RestoreOnExtend || lease.DisabledByLease {
				t.Fatal("wrong lease semantics", lease, err)
			}
			if strings.Contains(r.Body.String(), "synthetic-external-") || strings.Contains(r.Body.String(), "synthetic-refresh-") {
				t.Fatal("lease receipt leaked credentials")
			}
		})
	}
	t.Run("opt-in-warnings-and-invalid-row", func(t *testing.T) {
		row := externalLeaseFixture("warnings", 2000000000, true)
		row["rate_multiplier"] = 2
		row["extra"] = map[string]any{"unmapped": "metadata"}
		row["credentials"].(map[string]any)["model_mapping"] = map[string]string{"a": "b"}
		body := map[string]any{"accounts": []any{row}, "dry_run": true}
		r := managedTestRequest(t, h, "POST", "/api/accounts", body, 200)
		var raw map[string]json.RawMessage
		_ = json.Unmarshal(r.Body.Bytes(), &raw)
		if raw["lease_restore"] != nil {
			t.Fatal("external adapter enabled by default")
		}
		var legacy ImportReport
		_ = json.Unmarshal(r.Body.Bytes(), &legacy)
		if !stringIn("account_expiry_not_imported", legacy.Results[0].Warnings...) || !stringIn("sub2api_settings_not_imported", legacy.Results[0].Warnings...) {
			t.Fatal("legacy warnings changed")
		}
		body["include_external_leases"] = true
		r = managedTestRequest(t, h, "POST", "/api/accounts", body, 200)
		_ = json.Unmarshal(r.Body.Bytes(), &legacy)
		if stringIn("account_expiry_not_imported", legacy.Results[0].Warnings...) || !stringIn("sub2api_settings_not_imported", legacy.Results[0].Warnings...) || !stringIn("model_mapping_not_imported", legacy.Results[0].Warnings...) {
			t.Fatal("unrelated warnings suppressed", legacy.Results)
		}
		good := externalLeaseFixture("valid-mixed", 2000000000, true)
		bad := externalLeaseFixture("invalid-mixed", "bad-date", true)
		body = map[string]any{"accounts": []any{bad, good}, "include_external_leases": true}
		r = managedTestRequest(t, h, "POST", "/api/accounts", body, 200)
		_ = json.Unmarshal(r.Body.Bytes(), &legacy)
		if legacy.Added != 1 || legacy.Failed != 1 || legacy.Results[0].Code != "invalid_external_lease" || legacy.Results[1].Status != "added" {
			t.Fatal("invalid external lease not row-isolated", r.Body.String())
		}
		if _, err := p.Store.GetMetadata("invalid-mixed"); !errors.Is(err, errNotFound) {
			t.Fatal("invalid lease row imported account")
		}
		body["include_external_leases"] = false
		body["accounts"] = []any{bad}
		r = managedTestRequest(t, h, "POST", "/api/accounts", body, 200)
		_ = json.Unmarshal(r.Body.Bytes(), &legacy)
		if legacy.Added != 1 {
			t.Fatal("opt-out did not preserve generic import semantics")
		}
	})
	t.Run("duplicate-and-conflict-guards", func(t *testing.T) {
		row := externalLeaseFixture("external-duplicate", time.Now().Unix()-60, true)
		body := map[string]any{"accounts": []any{row}, "include_external_leases": true}
		managedTestRequest(t, h, "POST", "/api/accounts", body, 200)
		r := managedTestRequest(t, h, "POST", "/api/accounts", body, 200)
		var output externalLeaseImportResponse
		_ = json.Unmarshal(r.Body.Bytes(), &output)
		if output.Results[0].Status != "duplicate" || output.Receipt.Items[0].Status != "pending" {
			t.Fatal("duplicate target not available for explicit lease mapping")
		}
		item := output.Receipt.Items[0]
		request := map[string]any{"receipt_id": output.Receipt.ID, "items": []managedLeaseRestoreTarget{{item.AccountIndex, item.ID, item.AccountVersion, item.LeaseVersion}}, "confirm_restore": true}
		r = managedTestRequest(t, h, "POST", "/api/accounts/import-leases", request, 200)
		if !strings.Contains(r.Body.String(), "confirm_expired_required") {
			t.Fatal("foreign expired lease bypassed confirmation")
		}
		request["confirm_expired"] = true
		managedTestRequest(t, h, "POST", "/api/accounts/import-leases", request, 200)
		account, _ := p.Store.Get(item.ID)
		if account.Enabled {
			t.Fatal("confirmed expired lease did not disable")
		}
		body["accounts"] = []any{row, row}
		r = managedTestRequest(t, h, "POST", "/api/accounts", body, 200)
		_ = json.Unmarshal(r.Body.Bytes(), &output)
		if len(output.Receipt.Items) != 2 || output.Receipt.Items[0].Code != "ambiguous_import_target" || output.Receipt.Items[1].Code != "ambiguous_import_target" {
			t.Fatal("multiple source rows assigned one lease target")
		}
		conflict := externalLeaseFixture("external-duplicate", 2000000000, true)
		conflict["credentials"].(map[string]any)["access_token"] = "synthetic-conflicting-new-token"
		body["accounts"] = []any{conflict}
		r = managedTestRequest(t, h, "POST", "/api/accounts", body, 200)
		_ = json.Unmarshal(r.Body.Bytes(), &output)
		if output.Results[0].Status != "failed" || output.Receipt.Items[0].Status != "skipped" {
			t.Fatal("same-name conflict granted lease target")
		}
	})
}
