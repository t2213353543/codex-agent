package server

import (
	"bytes"
	"context"
	"encoding/json"
	"time"
)

const managedExternalLeaseSource = "sub2api_outer_account"

type managedExternalLease struct {
	managedBundleLease
	Source            string `json:"source"`
	ExpiryFormat      string `json:"expiry_format"`
	AutoDisableSource string `json:"auto_disable_source"`
	otherSettings     bool
}

func invalidManagedExternalLease() error {
	return &accountImportError{"invalid_external_lease", "Sub2API account lease requires an outer Unix-second integer or RFC3339 expires_at and a boolean auto_pause_on_expired"}
}

// Only a complete typed account wrapper carries an external account lease.
// Nested OAuth credentials and untyped Codex token expiry are never promoted.
func parseManagedExternalLease(raw json.RawMessage) (*managedExternalLease, error) {
	object, err := accountObject(raw)
	if err != nil {
		return nil, nil
	}
	for depth := 0; depth <= 8; depth++ {
		platform, platformErr := accountString(object, "platform")
		kind, kindErr := accountString(object, "type")
		if platform == "openai" && kind == "oauth" && platformErr == nil && kindErr == nil {
			expiry, hasExpiry := object["expires_at"]
			pause, hasPause := object["auto_pause_on_expired"]
			if !hasExpiry && !hasPause {
				return nil, nil
			}
			if _, err := accountObject(object["credentials"]); err != nil {
				return nil, invalidManagedExternalLease()
			}
			lease := &managedExternalLease{Source: managedExternalLeaseSource, ExpiryFormat: "unlimited", AutoDisableSource: "sub2api_default"}
			lease.AutoDisable = true
			if hasPause && !bytes.Equal(bytes.TrimSpace(pause), []byte("null")) {
				if json.Unmarshal(pause, &lease.AutoDisable) != nil {
					return nil, invalidManagedExternalLease()
				}
				lease.AutoDisableSource = "explicit"
			}
			if hasExpiry && !bytes.Equal(bytes.TrimSpace(expiry), []byte("null")) {
				if len(expiry) > 0 && bytes.TrimSpace(expiry)[0] == '"' {
					var value string
					if json.Unmarshal(expiry, &value) != nil {
						return nil, invalidManagedExternalLease()
					}
					parsed, err := time.Parse(time.RFC3339Nano, value)
					if err != nil {
						return nil, invalidManagedExternalLease()
					}
					lease.ExpiresAt, lease.ExpiryFormat = parsed.Unix(), "rfc3339"
				} else {
					if json.Unmarshal(expiry, &lease.ExpiresAt) != nil {
						return nil, invalidManagedExternalLease()
					}
					lease.ExpiryFormat = "unix_seconds"
				}
				if lease.ExpiresAt < 0 || lease.ExpiresAt > 253402300799 {
					return nil, invalidManagedExternalLease()
				}
				if lease.ExpiresAt == 0 {
					lease.ExpiryFormat = "unlimited"
				}
			}
			_, hasRate := object["rate_multiplier"]
			_, hasExtra := object["extra"]
			lease.otherSettings = hasRate || hasExtra
			return lease, nil
		}
		// Once a foreign/partial typed object is encountered, do not reinterpret
		// one of its nested credentials as an account belonging to another type.
		if object["platform"] != nil || object["type"] != nil {
			return nil, nil
		}
		nested, ok := object["credentials"]
		if !ok {
			return nil, nil
		}
		object, err = accountObject(nested)
		if err != nil {
			return nil, nil
		}
	}
	return nil, nil
}

func prepareManagedExternalLeaseItems(items []ImportItem) []managedExternalLease {
	leases := []managedExternalLease{}
	for i := range items {
		if items[i].invalidError != nil {
			continue
		}
		lease, err := parseManagedExternalLease(items[i].Credentials)
		if err != nil {
			items[i].invalidError = err
			continue
		}
		if lease != nil {
			lease.AccountIndex = i + 1
			leases = append(leases, *lease)
		}
	}
	return leases
}

func (p *Proxy) attachManagedExternalLeaseReceipt(ctx context.Context, response map[string]any, report *ImportReport, items []ImportItem, defaults AccountDefaults, leases []managedExternalLease, dryRun bool) {
	management := &managedBundleManagement{Version: 1, Leases: []managedBundleLease{}}
	parsed := make([]Account, len(items))
	for _, lease := range leases {
		index := lease.AccountIndex - 1
		management.Leases = append(management.Leases, lease.managedBundleLease)
		account, err := parseAccount(items[index].Name, items[index].Credentials)
		if err == nil {
			parsed[index] = applyAccountDefaults(account, defaults)
		}
		result := &report.Results[index]
		if result.Status == "failed" {
			continue
		}
		warnings := []string{}
		for _, warning := range result.Warnings {
			if warning == "account_expiry_not_imported" || warning == "sub2api_settings_not_imported" && !lease.otherSettings {
				continue
			}
			warnings = append(warnings, warning)
		}
		result.Warnings = append(warnings, "external_lease_pending_confirmation")
	}
	receipt := p.prepareManagedBundleLeases(ctx, management, *report, parsed, dryRun)
	// Existing same-token imports may retain management fields; every actual
	// target and expiration effect is determined by the stable-ID receipt helper.
	response["external_leases"] = leases
	response["lease_restore_source"] = managedExternalLeaseSource
	response["lease_restore"] = receipt
}
