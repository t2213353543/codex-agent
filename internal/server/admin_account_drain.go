package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const accountDrainCapability = "codex_agent_account_drain/v1"
const accountDrainScope = "in_flight_requests"
const accountDrainFenceTTL = time.Minute

var errAccountDraining = errors.New("account is draining; no new requests are admitted")
var errAccountDrainConflict = errors.New("account drain identity, operation or token changed")
var errAccountDrainUnsupported = errors.New("shared account drain is unavailable")

type accountDrainRequest struct {
	OperationID  string `json:"operation_id"`
	WorkspaceSHA string `json:"expected_workspace_sha256"`
	UserSHA      string `json:"expected_user_sha256"`
}

type accountDrainRecord struct {
	OperationID, OwnerSHA, WorkspaceSHA, UserSHA, Token string
	CredentialVersion, CreatedAt                        int64
	ZeroObservedAtMS                                    int64
}

type accountDrainStatus struct {
	Capability        string `json:"capability"`
	Scope             string `json:"scope"`
	AccountID         int64  `json:"account_id"`
	Status            string `json:"status"`
	InFlight          int64  `json:"in_flight"`
	OwnerSHA          string `json:"owner_ref_sha"`
	CredentialVersion int64  `json:"credential_version"`
	Token             string `json:"drain_token"`
	RemainingSafetyMS int64  `json:"remaining_safety_ms"`
	SafetyReason      string `json:"safety_reason,omitempty"`
	// This protocol counts active request leases, not client-owned future calls.
	PendingToolSessions *int64 `json:"pending_tool_sessions"`
}

func accountDrainSHA(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func accountDrainHex(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32 && hex.EncodeToString(decoded) == value
}

func accountDrainOwner(account Account) string { return accountDrainSHA(gatewayAccountRef(account)) }

func (p *Proxy) accountDrainSupported() bool {
	return p.Store.DB != nil && p.Store.DB.Pool != nil && p.Store.DB.Redis != nil && p.Store.DB.Namespace != ""
}

func (p *Proxy) sub2Capabilities(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{}
	if p.accountDrainSupported() {
		data["account_drain"] = accountDrainCapability
		data["account_drain_scope"] = accountDrainScope
	}
	sub2Success(w, data)
}

func accountDrainHTTPError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, errAccountDrainConflict), errors.Is(err, errAccountDraining):
		compatErrorStatus(w, r, http.StatusConflict, err.Error())
	case errors.Is(err, errAccountDrainUnsupported), gatewaySharedAdmissionFailure(err):
		compatErrorStatus(w, r, http.StatusServiceUnavailable, "shared account drain unavailable")
	default:
		compatError(w, r, err)
	}
}

func (p *Proxy) sub2AccountDrain(w http.ResponseWriter, r *http.Request) {
	if !p.accountDrainSupported() {
		accountDrainHTTPError(w, r, errAccountDrainUnsupported)
		return
	}
	row, err := p.sub2LoadAccount(r)
	if err != nil {
		accountDrainHTTPError(w, r, err)
		return
	}
	var input *accountDrainRequest
	if r.Method == http.MethodPost {
		input = new(accountDrainRequest)
		if err = compatReadJSON(w, r, input, 4096); err != nil {
			accountDrainHTTPError(w, r, err)
			return
		}
		if !accountDrainHex(input.OperationID) || !accountDrainHex(input.WorkspaceSHA) || !accountDrainHex(input.UserSHA) {
			accountDrainHTTPError(w, r, errInvalid)
			return
		}
	}
	status, err := p.accountDrain(r.Context(), row.Account.ID, input)
	if err != nil {
		accountDrainHTTPError(w, r, err)
		return
	}
	sub2Success(w, status)
}

// Redis uses the admission hash slot. The fence and valid lease count are one
// operation; renew intentionally ignores the fence. PG remains authoritative,
// so Redis eviction/restart cannot re-enable a drained account. This cache is
// short lived and creates no permanent Redis history after account deletion.
const accountDrainFenceScript = `
redis.call('SET',KEYS[2],ARGV[1],'PX',ARGV[2])
local t=redis.call('TIME')
local now=tonumber(t[1])*1000+math.floor(tonumber(t[2])/1000)
redis.call('ZREMRANGEBYSCORE',KEYS[1],'-inf',now)
return {redis.call('ZCARD',KEYS[1]),now}`

func accountDrainKey(namespace string, account Account) string {
	return gatewaySharedAdmissionKey(namespace, account) + ":drain"
}

func readAccountDrain(ctx context.Context, q accountSQL, id string) (accountDrainRecord, error) {
	var record accountDrainRecord
	err := q.QueryRow(ctx, `SELECT operation_id,owner_ref_sha,workspace_sha256,user_sha256,credential_version,drain_token,created_at,zero_observed_at_ms FROM account_drains WHERE account_id=$1`, id).
		Scan(&record.OperationID, &record.OwnerSHA, &record.WorkspaceSHA, &record.UserSHA, &record.CredentialVersion, &record.Token, &record.CreatedAt, &record.ZeroObservedAtMS)
	return record, err
}

func (p *Proxy) accountDrainLeaseCount(ctx context.Context, account Account, record accountDrainRecord) (int64, int64, error) {
	db := p.Store.DB
	op, cancel := context.WithTimeout(ctx, gatewaySharedAdmissionDefaults.operation)
	defer cancel()
	observation, err := db.Redis.WithTimeout(gatewaySharedAdmissionDefaults.operation).Eval(op, accountDrainFenceScript,
		[]string{gatewaySharedAdmissionKey(db.Namespace, account), accountDrainKey(db.Namespace, account)}, record.Token, accountDrainFenceTTL.Milliseconds()).Int64Slice()
	if err != nil {
		return 0, 0, &gatewaySharedAdmissionError{phase: "unavailable"}
	}
	if len(observation) != 2 || observation[0] < 0 || observation[1] <= 0 {
		return 0, 0, &gatewaySharedAdmissionError{phase: "unavailable"}
	}
	return observation[0], observation[1], nil
}

// Missing Redis leases are not proof that every transport has stopped: a lost
// key may precede its request watchdog cancellation. Retirement alone observes
// zero for one full lease TTL; this never delays or queues inference requests.
// Redis TIME is shared with the lease scores; caller wall clocks are not used.
func accountDrainRemainingSafety(record accountDrainRecord, count, nowMS int64) int64 {
	window := gatewaySharedAdmissionDefaults.ttl.Milliseconds()
	if count > 0 || record.ZeroObservedAtMS <= 0 || nowMS < record.ZeroObservedAtMS {
		return window
	}
	return max(int64(0), window-(nowMS-record.ZeroObservedAtMS))
}

func updateAccountDrainObservation(ctx context.Context, tx pgx.Tx, id string, record *accountDrainRecord, count, nowMS int64) error {
	zero := record.ZeroObservedAtMS
	if count > 0 {
		zero = 0
	} else if zero == 0 || zero > nowMS {
		zero = nowMS
	}
	if zero == record.ZeroObservedAtMS {
		return nil
	}
	if _, err := tx.Exec(ctx, "UPDATE account_drains SET zero_observed_at_ms=$2 WHERE account_id=$1", id, zero); err != nil {
		return err
	}
	record.ZeroObservedAtMS = zero
	return nil
}

// Credential -> account is the existing refresh/import/delete lock order.
// NOWAIT avoids adding scheduling waits while still serializing the atomic
// drain/admission boundary. No network to the provider occurs under this lock.
func lockAccountDrain(ctx context.Context, tx pgx.Tx, id string) error {
	var found string
	if err := tx.QueryRow(ctx, `SELECT account_id FROM account_credentials WHERE account_id=$1 FOR SHARE NOWAIT`, id).Scan(&found); err != nil {
		return accountDrainLockError(err)
	}
	if err := tx.QueryRow(ctx, `SELECT id FROM accounts WHERE id=$1 FOR UPDATE NOWAIT`, id).Scan(&found); err != nil {
		return accountDrainLockError(err)
	}
	return nil
}

func accountDrainLockError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotFound
	}
	var pgerr *pgconn.PgError
	if errors.As(err, &pgerr) && pgerr.Code == "55P03" {
		return errAccountDrainConflict
	}
	return err
}

func (p *Proxy) accountDrain(ctx context.Context, id string, input *accountDrainRequest) (accountDrainStatus, error) {
	var status accountDrainStatus
	if !p.accountDrainSupported() {
		return status, errAccountDrainUnsupported
	}
	tx, err := p.Store.DB.Pool.Begin(ctx)
	if err != nil {
		return status, err
	}
	defer tx.Rollback(context.Background())
	if err = lockAccountDrain(ctx, tx, id); err != nil {
		return status, err
	}
	account, err := p.Store.pgGetFrom(ctx, tx, id, true)
	if err != nil {
		return status, err
	}
	if !gatewayCompleteOwner(account) {
		return status, errAccountDrainConflict
	}
	owner := accountDrainOwner(account)
	workspace, user := accountDrainSHA(account.AccountID), accountDrainSHA(account.Identity.UserID)
	if input != nil && (input.WorkspaceSHA != workspace || input.UserSHA != user) {
		return status, errAccountDrainConflict
	}
	record, err := readAccountDrain(ctx, tx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		if input == nil {
			return status, errNotFound
		}
		var nonce [32]byte
		if _, err = rand.Read(nonce[:]); err != nil {
			return status, err
		}
		record = accountDrainRecord{OperationID: input.OperationID, OwnerSHA: owner, WorkspaceSHA: workspace, UserSHA: user,
			CredentialVersion: account.CredentialVersion, Token: hex.EncodeToString(nonce[:]), CreatedAt: time.Now().Unix()}
		_, err = tx.Exec(ctx, `INSERT INTO account_drains(account_id,operation_id,owner_ref_sha,workspace_sha256,user_sha256,credential_version,drain_token,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`,
			id, record.OperationID, record.OwnerSHA, record.WorkspaceSHA, record.UserSHA, record.CredentialVersion, record.Token, record.CreatedAt)
	}
	if err != nil {
		return status, err
	}
	if record.OwnerSHA != owner || record.WorkspaceSHA != workspace || record.UserSHA != user || input != nil && record.OperationID != input.OperationID {
		return status, errAccountDrainConflict
	}
	count, nowMS, err := p.accountDrainLeaseCount(ctx, account, record)
	if err != nil {
		return status, err
	}
	if err = updateAccountDrainObservation(ctx, tx, id, &record, count, nowMS); err != nil {
		return status, err
	}
	remaining := accountDrainRemainingSafety(record, count, nowMS)
	if err = tx.Commit(ctx); err != nil {
		return status, err
	}
	status = accountDrainStatus{Capability: accountDrainCapability, Scope: accountDrainScope, AccountID: compatNumericID(id), Status: "draining",
		InFlight: count, OwnerSHA: owner, CredentialVersion: account.CredentialVersion, Token: record.Token, RemainingSafetyMS: remaining}
	if count == 0 && remaining == 0 {
		status.Status = "drained"
	} else if count == 0 {
		status.SafetyReason = "lease_expiry_observation"
	}
	return status, nil
}

type accountDrainDeleteFence struct{ Token, OwnerSHA string }
type accountDrainDeleteContextKey struct{}

func accountDrainDeleteContext(r *http.Request) (context.Context, error) {
	token, hasToken, validToken := compatHeader(r, "X-Codex-Drain-Token")
	owner, hasOwner, validOwner := compatHeader(r, "X-Codex-Owner-Ref-Sha")
	if !hasToken && !hasOwner {
		return r.Context(), nil
	}
	if !validToken || !validOwner || !hasToken || !hasOwner || !accountDrainHex(token) || !accountDrainHex(owner) {
		return nil, errAccountDrainConflict
	}
	return context.WithValue(r.Context(), accountDrainDeleteContextKey{}, accountDrainDeleteFence{token, owner}), nil
}

// Invoked inside compatDeleteAccount's credential/account write transaction.
// No token can authorize a replacement owner, active lease or absent operation.
// Existing non-drained accounts retain their compatibility deletion behavior.
func (p *Proxy) checkAccountDrainDelete(ctx context.Context, tx pgx.Tx, account Account) error {
	fence, provided := ctx.Value(accountDrainDeleteContextKey{}).(accountDrainDeleteFence)
	record, err := readAccountDrain(ctx, tx, account.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		if provided {
			return errAccountDrainConflict
		}
		return nil
	}
	if err != nil {
		return err
	}
	if !provided || subtle.ConstantTimeCompare([]byte(record.Token), []byte(fence.Token)) != 1 || record.OwnerSHA != fence.OwnerSHA {
		return errAccountDrainConflict
	}
	// Non-draining compatibility deletion must continue to work even when a
	// legacy credential document is unreadable. Only a fenced deletion needs
	// the complete current owner; both rows are already locked by its caller.
	current, err := p.Store.pgGetFrom(ctx, tx, account.ID, true)
	if err != nil {
		return err
	}
	if record.OwnerSHA != accountDrainOwner(current) {
		return errAccountDrainConflict
	}
	count, nowMS, err := p.accountDrainLeaseCount(ctx, account, record)
	if err != nil {
		return err
	}
	if err = updateAccountDrainObservation(ctx, tx, account.ID, &record, count, nowMS); err != nil {
		return err
	}
	if count != 0 || accountDrainRemainingSafety(record, count, nowMS) != 0 {
		return errAccountDraining
	}
	return nil
}

// Hold a shared row lock only through the admission Lua call. Drain takes an
// incompatible row lock, so a missing/evicted Redis fence never permits a new
// lease after the durable drain starts. No blocking retry or inference queue.
func (p *Proxy) lockGatewayAdmissionDrain(ctx context.Context, account Account) (func(), error) {
	db := p.Store.DB
	if db.Pool == nil {
		return func() {}, nil
	} // isolated Redis protocol fixtures
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		return nil, &gatewaySharedAdmissionError{phase: "unavailable"}
	}
	closeTx := func() { _ = tx.Rollback(context.Background()) }
	var id string
	err = tx.QueryRow(ctx, `SELECT id FROM accounts WHERE id=$1 FOR KEY SHARE NOWAIT`, account.ID).Scan(&id)
	if err != nil {
		closeTx()
		var pgerr *pgconn.PgError
		if errors.As(err, &pgerr) && pgerr.Code == "55P03" {
			return nil, errAccountDraining
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errAccountDraining
		}
		return nil, &gatewaySharedAdmissionError{phase: "unavailable"}
	}
	var draining bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM account_drains WHERE account_id=$1)`, account.ID).Scan(&draining)
	if err != nil || draining {
		closeTx()
		if err != nil {
			return nil, &gatewaySharedAdmissionError{phase: "unavailable"}
		}
		return nil, errAccountDraining
	}
	return closeTx, nil
}
