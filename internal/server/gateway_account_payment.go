package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
)

const gatewayPaymentCooldown = 30 * time.Minute
const gatewayPaymentStatus = "payment_required"
const gatewayPaymentMessage = "upstream account returned payment required (402); scheduling paused for 30 minutes"

// Billing rejection is account-wide, independent of the optional model-level
// 429 policy. A bounded pause allows recovery after payment/workspace repair;
// credential replacement starts a new epoch and releases the prior rejection.
func (p *Proxy) checkGatewayPayment(account Account) error {
	if p.Store.DB == nil {
		account.CredentialVersion = 0
	}
	// The optional 429 policy has a conservative global overflow marker.
	// Billing checks must only consult this account's marker so disabling429
	// remains effective even when that unrelated map previously reached cap.
	key := gatewayCooldownKey(account, "payment")
	p.cooldownMu.Lock()
	until := p.cooldowns[key]
	if !time.Now().Before(until) {
		delete(p.cooldowns, key)
	}
	p.cooldownMu.Unlock()
	if account.CredentialStatus == gatewayPaymentStatus {
		if persisted := time.Unix(account.LastCheckedAt, 0).Add(gatewayPaymentCooldown); persisted.After(until) {
			until = persisted
		}
	}
	if time.Now().Before(until) {
		return &gatewayCooldownError{until: until, payment: true}
	}
	return nil
}

// Selection may precede a queued transport dispatch. Re-read the small payment
// projection at that boundary so another process's 402 cannot be hidden by an
// already-held lease or an old scheduling snapshot.
func (p *Proxy) checkGatewayPaymentDispatch(ctx context.Context, selected Account) error {
	if err := p.checkGatewayPayment(selected); err != nil {
		return gatewayBoundAccountUnavailable(ctx, selected, gatewayAccountRef(selected), err)
	}
	current := selected
	if db := p.Store.DB; db != nil && db.Pool != nil {
		// Billing belongs to the account, not the selected access-token epoch.
		// A concurrent OAuth rotation preserves payment_required; filtering
		// this read by the old epoch would turn that known block into NoRows.
		err := db.Pool.QueryRow(ctx, `SELECT credential_status,last_checked_at FROM accounts WHERE id=$1`, selected.ID).Scan(&current.CredentialStatus, &current.LastCheckedAt)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return &gatewayRecoveryAuthorizationError{status: http.StatusForbidden, beforeNativeDispatch: true}
			}
			return &gatewayCooldownStoreError{}
		}
	} else if p.Store.Path != "" {
		var err error
		current, err = p.Store.getAccountByID(selected.ID)
		if errors.Is(err, errNotFound) {
			return &gatewayRecoveryAuthorizationError{status: http.StatusForbidden, beforeNativeDispatch: true}
		}
		if err != nil {
			return err
		}
	}
	if err := p.checkGatewayPayment(current); err != nil {
		return gatewayBoundAccountUnavailable(ctx, current, gatewayAccountRef(current), err)
	}
	return nil
}

func (p *Proxy) recordGatewayPayment(ctx context.Context, selected Account) {
	if selected.ID == "" || selected.AccessToken == "" {
		return
	}
	local := selected
	if p.Store.DB == nil {
		local.CredentialVersion = 0
	}
	now := time.Now()
	p.localGatewayCooldown([]string{gatewayCooldownKey(local, "payment")}, now.Add(gatewayPaymentCooldown))
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if db := p.Store.DB; db != nil {
		// Match both the persisted epoch and actual token: a late response from
		// a replaced/rotated credential must never quarantine its replacement.
		// CAS the account revision rather than taking another credential lock.
		// Credential writers already update accounts atomically; this avoids
		// introducing an opposite account/credential row-lock ordering.
		current := selected
		for range 2 {
			tag, err := db.Pool.Exec(ctx, `UPDATE accounts a SET credential_status=$4,last_checked_at=$5,last_error=$6,updated_at=$5,version=a.version+1
			 WHERE a.id=$1 AND a.version=$7 AND EXISTS(SELECT 1 FROM account_credentials c WHERE c.account_id=a.id AND c.credential_version=$2 AND c.token_fingerprint=$3)
			 AND (a.credential_status<>$4 OR a.last_checked_at<$5-60)`, selected.ID, selected.CredentialVersion, tokenFingerprint(selected.AccessToken), gatewayPaymentStatus, now.Unix(), gatewayPaymentMessage, current.Version)
			if err != nil {
				return
			}
			if tag.RowsAffected() > 0 {
				db.invalidate()
				p.invalidateAccounts()
				return
			}
			current, err = p.Store.pgGetContext(ctx, selected.ID, true)
			if err != nil || current.CredentialVersion != selected.CredentialVersion || current.AccessToken != selected.AccessToken || current.CredentialStatus == gatewayPaymentStatus && current.LastCheckedAt >= now.Unix()-60 {
				return
			}
		}
		return
	}
	err := p.Store.modify(func(state *accountState) error {
		for i := range state.Accounts {
			a := &state.Accounts[i]
			if a.ID != selected.ID {
				continue
			}
			if a.AccessToken != selected.AccessToken || a.CredentialStatus == gatewayPaymentStatus && a.LastCheckedAt >= now.Unix()-60 {
				return errGatewayAccountErrorUnchanged
			}
			a.CredentialStatus, a.LastCheckedAt, a.LastError = gatewayPaymentStatus, now.Unix(), gatewayPaymentMessage
			a.UpdatedAt, a.Version = now.Unix(), a.Version+1
			return nil
		}
		return errNotFound
	})
	if err == nil {
		p.invalidateAccounts()
	}
}

// Only structured upstream fields are scheduling evidence. Free-form error
// prose, prompt text and unrelated successful payloads never quarantine an
// account, even if they happen to contain the number 402.
func gatewayPaymentFailureFields(fields map[string]json.RawMessage, terminal bool) bool {
	var kind, code string
	var status, statusCode int
	var nested, detail, response json.RawMessage
	if gatewayWSDecodeFields(fields, map[string]any{"type": &kind, "code": &code, "status": &status, "status_code": &statusCode, "error": &nested, "detail": &detail, "response": &response}) != nil {
		return false
	}
	if terminal && kind != "error" && kind != "response.failed" {
		return false
	}
	match := func(code, kind string, status, statusCode int) bool {
		return status == http.StatusPaymentRequired || statusCode == http.StatusPaymentRequired || code == "payment_required" || code == "deactivated_workspace" || code == "billing_not_active" || kind == "payment_required"
	}
	if match(code, kind, status, statusCode) {
		return true
	}
	if len(response) > 0 && string(response) != "null" {
		var responseError json.RawMessage
		if gatewayWSDecodeExact(response, map[string]any{"error": &responseError}) == nil && len(responseError) > 0 {
			detail = responseError
		}
	}
	for _, raw := range []json.RawMessage{nested, detail} {
		var innerCode, innerType string
		var innerStatus, innerStatusCode int
		if len(raw) > 0 && gatewayWSDecodeExact(raw, map[string]any{"code": &innerCode, "type": &innerType, "status": &innerStatus, "status_code": &innerStatusCode}) == nil && match(innerCode, innerType, innerStatus, innerStatusCode) {
			return true
		}
	}
	return false
}

func gatewayPaymentFailureBody(raw []byte) bool {
	fields, err := gatewayWSParseFields(raw)
	return err == nil && gatewayPaymentFailureFields(fields, false)
}

func (p *Proxy) observeGatewayPaymentBody(response *http.Response, raw []byte) {
	if response == nil || response.Request == nil || response.StatusCode < 400 || !gatewayPaymentFailureBody(raw) {
		return
	}
	if account, ok := response.Request.Context().Value(gatewayCooldownAccountContextKey{}).(Account); ok {
		p.recordGatewayPayment(response.Request.Context(), account)
	}
}
