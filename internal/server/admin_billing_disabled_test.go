package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestManagedBillingDisabled(t *testing.T)   { exerciseManagedBillingDisabled(t, webTestSetup) }
func TestPGManagedBillingDisabled(t *testing.T) { exerciseManagedBillingDisabled(t, adminPGTestProxy) }

func billingDisabledImportRow(name, multiplier string) map[string]any {
	return map[string]any{"name": name, "platform": "openai", "type": "oauth", "rate_multiplier": json.RawMessage(multiplier), "weight": 7, "credentials": map[string]any{"access_token": "synthetic-disabled-" + name, "refresh_token": "synthetic-refresh-" + name, "expires_at": 2100000000}}
}

func exerciseManagedBillingDisabled(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	p, h := setup(t)
	ctx := context.Background()
	_, beforeAccounts, err := p.Store.ListMetadata()
	if err != nil {
		t.Fatal(err)
	}
	retained := map[string]managementStateEntry{}
	for _, namespace := range []string{"usage-pricing", "usage-procurement", "usage-sale", "usage-cost-policy"} {
		if err := p.managementState().Update(ctx, namespace, "catalog", func(entry *managementStateEntry) error {
			entry.Payload = json.RawMessage(`{"retired_catalog":"preserve without reading or applying"}`)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		entry, err := p.managementState().Get(ctx, namespace, "catalog")
		if err != nil {
			t.Fatal(err)
		}
		retained[namespace] = entry
	}
	for _, path := range []string{"/api/logs/pricing", "/api/logs/cost-rules", "/api/logs/cost-rules/preview", "/api/logs/sale-policy", "/api/logs/sale-policy/preview", "/api/logs/cost-policy", "/api/logs/cost-policy/preview", "/api/accounts/import-costs", "/api/accounts/import-costs/receipts/old-receipt"} {
		for _, method := range []string{"GET", "POST", "PUT", "DELETE"} {
			t.Run(method+path, func(t *testing.T) {
				r := managedTestRequest(t, h, method, path, map[string]any{"confirm_restore": true}, 404)
				if !strings.Contains(r.Body.String(), `"code":"not_found"`) {
					t.Fatal("retired endpoint did not return stable not_found")
				}
			})
		}
	}
	for _, endpoint := range []string{"/api/accounts", "/api/accounts/import/preview"} {
		for _, form := range []string{"raw", "accounts", "credentials"} {
			row := billingDisabledImportRow("disabled-cost-"+form, "0.25")
			body := map[string]any{"include_external_costs": true}
			switch form {
			case "raw":
				raw, err := json.Marshal(row)
				if err != nil {
					t.Fatal(err)
				}
				body["raw"] = string(raw)
			case "accounts":
				body["accounts"] = []any{row}
			default:
				body["name"], body["credentials"] = row["name"], row
			}
			r := managedTestRequest(t, h, "POST", endpoint, body, 400)
			if !strings.Contains(r.Body.String(), `"code":"unsupported"`) {
				t.Fatal("cost import silently accepted")
			}
		}
	}
	if _, accounts, err := p.Store.ListMetadata(); err != nil || len(accounts) != len(beforeAccounts) {
		t.Fatal("rejected costs wrote accounts", err)
	}
	row := billingDisabledImportRow("nonbilling-import", "0.125")
	row["expires_at"], row["auto_pause_on_expired"] = time.Now().Add(time.Hour).Unix(), true
	r := managedTestRequest(t, h, "POST", "/api/accounts", map[string]any{"accounts": []any{row}, "include_external_costs": false, "include_external_leases": true}, 200)
	var response map[string]json.RawMessage
	if json.Unmarshal(r.Body.Bytes(), &response) != nil || response["cost_restore"] != nil || response["external_costs"] != nil || response["lease_restore"] == nil {
		t.Fatal("normal import lost lease or exposed cost receipt")
	}
	a, err := p.Store.Get("nonbilling-import")
	if err != nil || a.Weight != 7 || a.ExpiresAt != 2100000000 {
		t.Fatal("ordinary metadata or credential expiry changed", err)
	}
	selection := map[string]any{"items": []managedAccountItem{{ID: a.ID, Version: a.Version}}, "include_costs": true}
	for _, endpoint := range []string{"/api/accounts/export", "/api/accounts/export/credentials"} {
		r = managedTestRequest(t, h, "POST", endpoint, selection, 400)
		if !strings.Contains(r.Body.String(), `"code":"unsupported"`) {
			t.Fatal("cost export accepted")
		}
	}
	selection["include_costs"] = false
	managedTestRequest(t, h, "POST", "/api/accounts/export", selection, 200)
	r = managedTestRequest(t, h, "POST", "/api/accounts/export/credentials", selection, 200)
	var bundle map[string]any
	if err := json.Unmarshal(r.Body.Bytes(), &bundle); err != nil {
		t.Fatal(err)
	}
	accounts := bundle["accounts"].([]any)
	account := accounts[0].(map[string]any)
	account["name"] = "old-cost-backup-restored"
	account["credentials"] = map[string]any{"access_token": "synthetic-restored-no-billing", "refresh_token": "synthetic-restored-refresh", "expires_at": 2100000000}
	bundle["management"] = map[string]any{"version": 1, "costs": "obsolete unsupported shape is ignored", "leases": []any{map[string]any{"account_index": 1, "expires_at": time.Now().Add(2 * time.Hour).Unix(), "auto_disable": true, "restore_on_extend": true}}}
	for _, dry := range []bool{true, false} {
		r = managedTestRequest(t, h, "POST", "/api/accounts/import-bundle", map[string]any{"data": bundle, "dry_run": dry}, 200)
		response = nil
		if json.Unmarshal(r.Body.Bytes(), &response) != nil || response["cost_restore"] != nil || response["lease_restore"] == nil {
			t.Fatal("legacy backup cost applied or lease discarded")
		}
		var imported struct {
			Added        int                        `json:"added"`
			LeaseRestore managedLeaseRestoreReceipt `json:"lease_restore"`
		}
		if json.Unmarshal(r.Body.Bytes(), &imported) != nil || imported.Added != 1 || len(imported.LeaseRestore.Items) != 1 {
			t.Fatal("normal bundle failed")
		}
		if dry && imported.LeaseRestore.ID != "" || !dry && imported.LeaseRestore.ID == "" {
			t.Fatal("lease receipt stage changed")
		}
	}
	for _, namespace := range []string{"cost-restore"} {
		entries, err := p.managementState().List(ctx, namespace)
		if err != nil || len(entries) != 0 {
			t.Fatal("cost migration state created", err)
		}
	}
	for namespace, before := range retained {
		after, err := p.managementState().Get(ctx, namespace, "catalog")
		if err != nil || after.Version != before.Version || string(after.Payload) != string(before.Payload) {
			t.Fatal("retired persistent catalog mutated", namespace, err)
		}
	}
	// Real provider controls remain management functionality, not local billing.
	managedTestRequest(t, h, "GET", "/api/accounts/"+a.ID+"/upstream", nil, 200)
	managedTestRequest(t, h, "GET", "/api/accounts/"+a.ID+"/auto-reset", nil, 200)
}
