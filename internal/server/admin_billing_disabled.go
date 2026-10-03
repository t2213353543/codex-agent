package server

import (
	"bytes"
	"encoding/json"
	"strings"
)

// Prevent retired names from falling through to account or record ID routes.
func retiredManagedBillingPath(path string) bool {
	for _, prefix := range []string{"/api/logs/pricing", "/api/logs/cost-rules", "/api/logs/sale-policy", "/api/logs/cost-policy", "/api/accounts/import-costs"} {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}

type managedAccountImportBundle managedAccountBundle

// Old backups remain importable without interpreting their retired cost data.
func (b *managedAccountImportBundle) UnmarshalJSON(data []byte) error {
	type accountBundle managedAccountImportBundle
	var envelope struct {
		*accountBundle
		Management json.RawMessage `json:"management"`
	}
	envelope.accountBundle = (*accountBundle)(b)
	strict := json.NewDecoder(bytes.NewReader(data))
	strict.DisallowUnknownFields()
	if err := strict.Decode(&envelope); err != nil {
		return err
	}
	if len(envelope.Management) == 0 || string(envelope.Management) == "null" {
		b.Management = nil
		return nil
	}
	var supported struct {
		Version int                  `json:"version"`
		Leases  []managedBundleLease `json:"leases"`
		Costs   json.RawMessage      `json:"costs"`
	}
	strict = json.NewDecoder(bytes.NewReader(envelope.Management))
	strict.DisallowUnknownFields()
	if err := strict.Decode(&supported); err != nil {
		return err
	}
	b.Management = &managedBundleManagement{Version: supported.Version, Leases: supported.Leases}
	return nil
}
