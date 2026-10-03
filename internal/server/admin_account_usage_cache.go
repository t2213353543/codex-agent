package server

import (
	"context"
	"encoding/json"
	"time"
)

// List refreshes reuse only recent official usage. Local quota accounting never
// substitutes for a missing official response, and credential changes invalidate
// snapshots even if the account ID is unchanged.
func (p *Proxy) cachedManagedUsage(ctx context.Context, account Account, allowCache bool) *managedUpstreamSnapshot {
	if !allowCache {
		return nil
	}
	entry, err := p.managementState().Get(ctx, "account-upstream", account.ID)
	if err != nil {
		return nil
	}
	var snapshots map[string]managedUpstreamSnapshot
	if json.Unmarshal(entry.Payload, &snapshots) != nil {
		return nil
	}
	snapshot, found := snapshots["usage"]
	credentialVersion := account.CredentialVersion
	now := time.Now().Unix()
	if !found || !snapshot.OK || snapshot.Stale || snapshot.ID != account.ID || snapshot.Kind != "usage" || snapshot.Source != "codex_usage" || snapshot.CredentialVersion != credentialVersion || snapshot.CheckedAt <= 0 || snapshot.CheckedAt > now || now-snapshot.CheckedAt >= 300 {
		return nil
	}
	// A reset boundary makes even a young cache entry obsolete. Preserve unknown
	// fields rather than interpreting a missing reset time as Unix epoch zero.
	raw, err := json.Marshal(snapshot.Data)
	if err != nil {
		return nil
	}
	parsed, err := parseManagedUsage(raw)
	if err != nil {
		return nil
	}
	usage := parsed.(managedUsage)
	rates := []*managedRateLimit{usage.RateLimit}
	for _, extra := range usage.Additional {
		rates = append(rates, extra.RateLimit)
	}
	for _, rate := range rates {
		if rate == nil {
			continue
		}
		for _, window := range []*managedRateWindow{rate.PrimaryWindow, rate.SecondaryWindow} {
			if window == nil {
				continue
			}
			if window.ResetAt != nil && *window.ResetAt > 0 && *window.ResetAt <= now || window.ResetAt == nil && window.ResetAfterSeconds != nil && *window.ResetAfterSeconds >= 0 && snapshot.CheckedAt+*window.ResetAfterSeconds <= now {
				return nil
			}
		}
	}
	if usage.SpendControl != nil && usage.SpendControl.IndividualLimit != nil {
		limit := usage.SpendControl.IndividualLimit
		if limit.ResetAt != nil && *limit.ResetAt > 0 && *limit.ResetAt <= now ||
			limit.ResetAt == nil && limit.ResetAfterSeconds != nil && *limit.ResetAfterSeconds >= 0 && snapshot.CheckedAt+*limit.ResetAfterSeconds <= now {
			return nil
		}
	}
	for _, model := range usage.ModelUsage {
		if model.AvailableAt != nil {
			if availableAt, err := time.Parse(time.RFC3339Nano, *model.AvailableAt); err == nil && !availableAt.After(time.Now()) {
				return nil
			}
		}
	}
	return &snapshot
}
