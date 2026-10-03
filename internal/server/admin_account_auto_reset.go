package server

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"sort"
	"time"
)

// This opt-in management policy reads upstream quota independently of routing.
// No gateway admission, cooldown, account enabled flag or local quota changes.
type managedAutoResetPolicy struct {
	Version           int64    `json:"version"`
	Enabled           bool     `json:"enabled"`
	Threshold         float64  `json:"threshold_percent"`
	Interval          int      `json:"interval_minutes"`
	CredentialVersion int64    `json:"credential_version"`
	Phase             string   `json:"phase"`
	NextRunAt         int64    `json:"next_run_at"`
	LastCheckedAt     int64    `json:"last_checked_at"`
	LastResetAt       int64    `json:"last_reset_at"`
	LastError         string   `json:"last_error"`
	RequestID         string   `json:"request_id"`
	CreditID          string   `json:"credit_id"`
	TriggerWindows    []string `json:"trigger_windows,omitempty"`
	LeaseUntil        int64    `json:"lease_until"`
}

func decodeManagedAutoReset(entry managementStateEntry) (managedAutoResetPolicy, error) {
	policy := managedAutoResetPolicy{Threshold: 95, Interval: 15, Phase: "off"}
	if entry.Version > 0 {
		if err := json.Unmarshal(entry.Payload, &policy); err != nil {
			return policy, err
		}
	}
	return policy, nil
}

func managedAccountCredentialVersion(p *Proxy, a Account) int64 {
	return a.CredentialVersion
}

func (p *Proxy) handleManagedAutoResetGet(w http.ResponseWriter, r *http.Request) {
	a, err := p.Store.GetMetadata(r.PathValue("name"))
	if err == nil && a.ID != r.PathValue("name") {
		err = errNotFound
	}
	if err != nil {
		p.storeError(w, err)
		return
	}
	entry, err := p.managementState().Get(r.Context(), "account-auto-reset", a.ID)
	if err != nil && !errors.Is(err, errNotFound) {
		p.storeError(w, err)
		return
	}
	policy, err := decodeManagedAutoReset(entry)
	if err != nil {
		p.storeError(w, err)
		return
	}
	writeJSON(w, map[string]any{"id": a.ID, "account_version": a.Version, "policy": policy})
}

func (p *Proxy) handleManagedAutoResetUpdate(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Version            int64   `json:"version"`
		AccountVersion     int64   `json:"account_version"`
		Enabled            bool    `json:"enabled"`
		Threshold          float64 `json:"threshold_percent"`
		Interval           int     `json:"interval_minutes"`
		Confirm            bool    `json:"confirm_auto_consume"`
		AcknowledgeUnknown bool    `json:"acknowledge_unknown"`
	}
	if err := readJSONLimit(w, r, &input, 8<<10); err != nil {
		adminError(w, err)
		return
	}
	if input.Version < 0 || input.AccountVersion < 1 || math.IsNaN(input.Threshold) || math.IsInf(input.Threshold, 0) || input.Threshold < 1 || input.Threshold > 100 || input.Interval < 5 || input.Interval > 1440 || input.Enabled && !input.Confirm {
		adminError(w, errInvalid)
		return
	}
	// The policy update only needs identity, version and credential epoch —
	// all metadata fields. Resolving without credential decryption avoids two
	// full account reads per PUT.
	a, err := p.Store.GetMetadata(r.PathValue("name"))
	if err == nil && a.ID != r.PathValue("name") {
		err = errNotFound
	}
	if err != nil {
		p.storeError(w, err)
		return
	}
	if a.Version != input.AccountVersion {
		p.storeError(w, errVersionConflict)
		return
	}
	err = p.managementState().UpdateAudited(r.Context(), "account-auto-reset", a.ID, "account.auto_reset.update", func(entry *managementStateEntry) error {
		policy, err := decodeManagedAutoReset(*entry)
		if err != nil {
			return err
		}
		if policy.Version != input.Version {
			return errVersionConflict
		}
		// Changing a running policy cannot authorize a second concurrent attempt.
		if input.Enabled && policy.Phase == "unknown" && !input.AcknowledgeUnknown {
			return errConflict
		}
		current, err := p.Store.GetMetadata(a.ID)
		if err != nil {
			return err
		}
		if current.Version != input.AccountVersion {
			return errVersionConflict
		}
		policy.Version++
		policy.Enabled, policy.Threshold, policy.Interval = input.Enabled, input.Threshold, input.Interval
		policy.CredentialVersion = managedAccountCredentialVersion(p, current)
		if policy.Phase != "checking" && policy.Phase != "resetting" && (policy.Phase != "unknown" || input.Enabled) {
			policy.Phase, policy.NextRunAt, policy.LastError = "off", 0, ""
			if input.Enabled {
				policy.Phase, policy.NextRunAt = "pending", time.Now().Unix()
			}
		}
		entry.Payload, err = json.Marshal(policy)
		return err
	})
	if err != nil {
		p.storeError(w, err)
		return
	}
	p.handleManagedAutoResetGet(w, r)
}

func (p *Proxy) processManagedAutoResets(ctx context.Context, now time.Time) {
	entries, err := p.managementState().List(ctx, "account-auto-reset")
	if err != nil {
		return
	}
	for id, entry := range entries {
		if ctx.Err() != nil {
			return
		}
		policy, err := decodeManagedAutoReset(entry)
		if err != nil {
			continue
		}
		if (policy.Phase == "checking" || policy.Phase == "resetting") && policy.LeaseUntil <= now.Unix() {
			// An interrupted consume is never retried automatically. A read-only
			// quota check may safely run again on the next configured interval.
			_ = p.managementState().Update(ctx, "account-auto-reset", id, func(current *managementStateEntry) error {
				row, e := decodeManagedAutoReset(*current)
				if e != nil {
					return e
				}
				if row.RequestID != policy.RequestID || row.LeaseUntil > now.Unix() {
					return errConflict
				}
				if row.Phase == "resetting" {
					row.Phase, row.Enabled, row.LastError = "unknown", false, "previous reset outcome is unknown; verify upstream quota before enabling again"
				} else if row.Phase == "checking" {
					row.Phase, row.LastError = "failed", "previous quota check was interrupted"
				} else {
					return errConflict
				}
				row.LeaseUntil = 0
				row.NextRunAt = now.Add(time.Duration(row.Interval) * time.Minute).Unix()
				current.Payload, e = json.Marshal(row)
				return e
			})
			continue
		}
		if !policy.Enabled || policy.NextRunAt > now.Unix() || policy.Phase == "checking" || policy.Phase == "resetting" {
			continue
		}
		p.processManagedAutoReset(ctx, id, policy.Version, now)
	}
}

func (p *Proxy) processManagedAutoReset(parent context.Context, id string, version int64, now time.Time) {
	var claimed managedAutoResetPolicy
	err := p.managementState().Update(parent, "account-auto-reset", id, func(entry *managementStateEntry) error {
		row, e := decodeManagedAutoReset(*entry)
		if e != nil {
			return e
		}
		if row.Version != version || !row.Enabled || row.NextRunAt > now.Unix() || row.Phase == "checking" || row.Phase == "resetting" {
			return errConflict
		}
		row.Phase, row.RequestID, row.LastError = "checking", newID("auto_"), ""
		row.LeaseUntil = now.Add(3 * time.Minute).Unix()
		row.LastCheckedAt = now.Unix()
		claimed = row
		entry.Payload, e = json.Marshal(row)
		return e
	})
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(parent, 90*time.Second)
	defer cancel()
	finish := func(phase, message string, reset bool) {
		persistCtx, persistCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer persistCancel()
		_ = p.managementState().Update(persistCtx, "account-auto-reset", id, func(entry *managementStateEntry) error {
			row, e := decodeManagedAutoReset(*entry)
			if e != nil {
				return e
			}
			if row.RequestID != claimed.RequestID {
				return errConflict
			}
			row.Phase, row.LastError, row.LeaseUntil = phase, redactMetricError(message), 0
			row.NextRunAt = time.Now().Add(time.Duration(row.Interval) * time.Minute).Unix()
			if reset {
				row.LastResetAt = time.Now().Unix()
			}
			if phase == "unknown" {
				row.Enabled = false
			}
			entry.Payload, e = json.Marshal(row)
			return e
		})
	}
	a, err := p.Store.Get(id)
	if err != nil {
		finish("paused", "account unavailable", false)
		return
	}
	if !a.Enabled || managedAccountCredentialVersion(p, a) != claimed.CredentialVersion {
		finish("paused", "account disabled or credentials changed; review the policy", false)
		return
	}
	usage := p.checkManagedUpstream(ctx, a, "usage", "")
	if err = p.cacheManagedUpstream(ctx, usage); err != nil {
		finish("failed", "quota snapshot could not be persisted", false)
		return
	}
	if !usage.OK {
		finish("failed", "upstream quota check failed", false)
		return
	}
	data, ok := usage.Data.(managedUsage)
	if !ok {
		finish("failed", "upstream quota payload unavailable", false)
		return
	}
	reached, known := managedAutoResetThreshold(data, claimed.Threshold, time.Now())
	if !known {
		finish("failed", "upstream quota windows are unknown or already expired", false)
		return
	}
	if !reached {
		if claimed.CreditID != "" && claimed.LastResetAt > 0 && !managedAutoResetRecovered(data, claimed, time.Now()) {
			finish("paused", "the quota window that triggered the previous reset has not been observed recovered", false)
			return
		}
		// Observe a recovered quota window before allowing another automatic
		// voucher. A stale high reading after success must not drain all credits.
		_ = p.managementState().Update(ctx, "account-auto-reset", id, func(entry *managementStateEntry) error {
			row, e := decodeManagedAutoReset(*entry)
			if e != nil {
				return e
			}
			if row.RequestID != claimed.RequestID {
				return errConflict
			}
			row.CreditID = ""
			row.TriggerWindows = nil
			entry.Payload, e = json.Marshal(row)
			return e
		})
		finish("below_threshold", "", false)
		return
	}
	if claimed.CreditID != "" && claimed.LastResetAt > 0 {
		finish("no_credit", "previous reset has not yet been followed by an observed recovered quota window", false)
		return
	}
	credits := p.checkManagedUpstream(ctx, a, "reset_credits", "")
	if err = p.cacheManagedUpstream(ctx, credits); err != nil {
		finish("failed", "credit snapshot could not be persisted", false)
		return
	}
	if !credits.OK {
		finish("failed", "upstream reset credits check failed", false)
		return
	}
	creditData, ok := credits.Data.(managedResetCredits)
	if !ok {
		finish("failed", "upstream reset credits payload unavailable", false)
		return
	}
	creditID := managedAutoResetCredit(creditData, time.Now())
	if creditID == "" {
		finish("no_credit", "", false)
		return
	}
	// A successful reset must not burn another voucher if upstream briefly
	// continues serving the old usage snapshot and the same available credit.
	if creditID == claimed.CreditID {
		finish("no_credit", "reset credit was already attempted", false)
		return
	}
	err = p.managementState().Update(ctx, "account-auto-reset", id, func(entry *managementStateEntry) error {
		row, e := decodeManagedAutoReset(*entry)
		if e != nil {
			return e
		}
		if row.RequestID != claimed.RequestID || row.Version != claimed.Version || !row.Enabled || row.Phase != "checking" {
			return errConflict
		}
		row.Phase, row.CreditID = "resetting", creditID
		row.TriggerWindows = nil
		for _, name := range []string{"primary", "secondary"} {
			if percent, known := managedAutoResetWindow(data, name, time.Now()); known && percent >= claimed.Threshold {
				row.TriggerWindows = append(row.TriggerWindows, name)
			}
		}
		entry.Payload, e = json.Marshal(row)
		return e
	})
	if err != nil {
		finish("failed", "policy changed before reset; no credit consumed", false)
		return
	}
	result, err := p.managedConsumeResetCredit(ctx, a, creditID, claimed.RequestID)
	if err != nil {
		finish("unknown", "reset outcome could not be durably confirmed; inspect upstream quota", false)
		return
	}
	if !result.OK {
		if result.Error != nil && result.Error.Code == "outcome_unknown" {
			finish("unknown", result.Error.Message, false)
		} else {
			finish("failed", "upstream reset was rejected", false)
		}
		return
	}
	raw, _ := json.Marshal(result.Data)
	var response struct {
		Code         string `json:"code"`
		WindowsReset int64  `json:"windows_reset"`
	}
	_ = json.Unmarshal(raw, &response)
	switch response.Code {
	case "reset":
		if response.WindowsReset > 0 {
			finish("succeeded", "", true)
		} else {
			finish("failed", "upstream acknowledged reset without any reset window", false)
		}
	case "no_credit", "already_redeemed":
		finish("no_credit", "", false)
	case "nothing_to_reset":
		finish("below_threshold", "", false)
	default:
		finish("unknown", "unrecognized upstream reset outcome", false)
	}
}

func managedAutoResetThreshold(usage managedUsage, threshold float64, now time.Time) (bool, bool) {
	known := false
	for _, name := range []string{"primary", "secondary"} {
		percent, valid := managedAutoResetWindow(usage, name, now)
		if !valid {
			continue
		}
		known = true
		if percent >= threshold {
			return true, true
		}
	}
	return false, known
}

func managedAutoResetWindow(usage managedUsage, name string, now time.Time) (float64, bool) {
	if usage.RateLimit == nil {
		return 0, false
	}
	window := usage.RateLimit.PrimaryWindow
	if name == "secondary" {
		window = usage.RateLimit.SecondaryWindow
	} else if name != "primary" {
		return 0, false
	}
	if window == nil || window.UsedPercent == nil || *window.UsedPercent < 0 || *window.UsedPercent > 100 || math.IsNaN(*window.UsedPercent) || math.IsInf(*window.UsedPercent, 0) {
		return 0, false
	}
	if window.ResetAt != nil && *window.ResetAt <= now.Unix() || window.ResetAfterSeconds != nil && *window.ResetAfterSeconds <= 0 {
		return 0, false
	}
	return *window.UsedPercent, true
}
func managedAutoResetRecovered(usage managedUsage, policy managedAutoResetPolicy, now time.Time) bool {
	names := policy.TriggerWindows
	if len(names) == 0 {
		names = []string{"primary", "secondary"}
	}
	for _, name := range names {
		percent, known := managedAutoResetWindow(usage, name, now)
		if !known || percent >= policy.Threshold {
			return false
		}
	}
	return true
}

func managedAutoResetCredit(data managedResetCredits, now time.Time) string {
	if data.AvailableCount == nil || *data.AvailableCount < 1 {
		return ""
	}
	type candidate struct {
		id     string
		expiry int64
	}
	options := []candidate{}
	for _, credit := range data.Credits {
		if credit.Status != "available" || !validRotationRequestID(credit.ID) || credit.ResetType != "codex_rate_limits" || credit.ExpiresAt == nil || *credit.ExpiresAt == "" {
			continue
		}
		value, err := time.Parse(time.RFC3339, *credit.ExpiresAt)
		if err != nil || !value.After(now) {
			continue
		}
		expiry := value.Unix()
		options = append(options, candidate{credit.ID, expiry})
	}
	sort.Slice(options, func(i, j int) bool {
		if options[i].expiry == options[j].expiry {
			return options[i].id < options[j].id
		}
		return options[i].expiry < options[j].expiry
	})
	if len(options) == 0 {
		return ""
	}
	return options[0].id
}
