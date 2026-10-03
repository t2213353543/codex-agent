package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type AccountListOptions struct {
	Limit   int
	Cursor  string
	Search  string
	GroupID string
	ProxyID string
	Status  string
	Enabled *bool
	// The file-store legacy list obtains request evidence in its Proxy caller.
	// Keep the credential generation beside it so a concurrent replacement
	// cannot reuse a status from the preceding metadata read.
	lastStatuses             map[string]string
	statusCredentialVersions map[string]int64
}

type AccountPage struct {
	Accounts   []Account `json:"accounts"`
	NextCursor string    `json:"next_cursor,omitempty"`
	HasMore    bool      `json:"has_more"`
}

type accountCursor struct {
	CreatedAt int64  `json:"created_at"`
	ID        string `json:"id"`
}

type accountRefreshEntry struct {
	wait chan struct{}
	refs int
}

var accountRefreshGates = struct {
	sync.Mutex
	entries map[string]*accountRefreshEntry
}{entries: make(map[string]*accountRefreshEntry)}

// Only the process's active refresh for an account occupies a pool connection.
// PostgreSQL's session lock then serializes that refresh with other processes.
func waitAccountRefresh(ctx context.Context, key string) (func(), error) {
	accountRefreshGates.Lock()
	entry := accountRefreshGates.entries[key]
	if entry == nil {
		entry = &accountRefreshEntry{wait: make(chan struct{}, 1)}
		accountRefreshGates.entries[key] = entry
	}
	entry.refs++
	accountRefreshGates.Unlock()
	drop := func() {
		accountRefreshGates.Lock()
		defer accountRefreshGates.Unlock()
		entry.refs--
		if entry.refs == 0 {
			delete(accountRefreshGates.entries, key)
		}
	}
	select {
	case entry.wait <- struct{}{}:
		return func() { <-entry.wait; drop() }, nil
	case <-ctx.Done():
		drop()
		return nil, ctx.Err()
	}
}

type accountSQL interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// Associations and credential versions are hydrated with one extra batched
// statement instead of three correlated subqueries per row, so listing pages
// and gateway snapshots cost one aggregate scan per relation rather than one
// index probe per relation per account.
const accountColumns = `a.id,a.display_name,a.enabled,a.notes,a.tags,a.concurrency,a.priority,a.weight,a.quota,a.window_days,a.window_start,a.expires_at,a.has_refresh_token,a.credential_status,a.last_checked_at,a.last_error,a.created_at,a.updated_at,a.version`

func accountContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 15*time.Second)
}

func accountDBError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var pgerr *pgconn.PgError
	if errors.As(err, &pgerr) {
		switch pgerr.Code {
		case "23503":
			return fmt.Errorf("%w: a referenced account, group or proxy does not exist", errInvalid)
		case "23505":
			return fmt.Errorf("%w: account already exists", errConflict)
		case "23514", "22001":
			return fmt.Errorf("%w: invalid account value", errInvalid)
		}
	}
	return safeDBError(err)
}

func (s Store) queryAccounts(ctx context.Context, q accountSQL, suffix string, args []any, credentials bool) ([]Account, error) {
	rows, err := q.Query(ctx, "SELECT "+accountColumns+" FROM accounts a "+suffix, args...)
	if err != nil {
		return nil, accountDBError(err)
	}
	accounts := make([]Account, 0)
	for rows.Next() {
		var a Account
		if err := rows.Scan(&a.ID, &a.Name, &a.Enabled, &a.Notes, &a.Tags, &a.Concurrency, &a.Priority, &a.Weight, &a.Quota, &a.WindowDays, &a.WindowStart, &a.ExpiresAt, &a.HasRefreshToken, &a.CredentialStatus, &a.LastCheckedAt, &a.LastError, &a.CreatedAt, &a.UpdatedAt, &a.Version); err != nil {
			rows.Close()
			return nil, err
		}
		accounts = append(accounts, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := s.hydrateAccountAssociations(ctx, q, accounts); err != nil {
		return nil, err
	}
	if !credentials || len(accounts) == 0 {
		return accounts, nil
	}
	return s.queryAccountCredentials(ctx, q, accounts)
}

// hydrateAccountAssociations fills groups, proxies and credential versions for
// the scanned page with one aggregate statement. Missing relations stay empty
// arrays and a missing credential row keeps version zero, matching the former
// correlated-subquery output.
func (s Store) hydrateAccountAssociations(ctx context.Context, q accountSQL, accounts []Account) error {
	if len(accounts) == 0 {
		return nil
	}
	ids := make([]string, len(accounts))
	positions := make(map[string]int, len(accounts))
	for i, a := range accounts {
		ids[i] = a.ID
		positions[a.ID] = i
	}
	var groupsRaw, proxiesRaw, versionsRaw []byte
	statement := `SELECT
 COALESCE((SELECT jsonb_object_agg(account_id,groups) FROM (SELECT account_id,array_agg(group_id ORDER BY group_id) AS groups FROM account_groups WHERE account_id=ANY($1::text[]) GROUP BY account_id) t),'{}'::jsonb),
 COALESCE((SELECT jsonb_object_agg(account_id,proxies) FROM (SELECT account_id,array_agg(proxy_id ORDER BY position,proxy_id) AS proxies FROM account_proxies WHERE account_id=ANY($1::text[]) GROUP BY account_id) t),'{}'::jsonb),
 COALESCE((SELECT jsonb_object_agg(account_id,credential_version) FROM account_credentials WHERE account_id=ANY($1::text[])),'{}'::jsonb)`
	if err := q.QueryRow(ctx, statement, ids).Scan(&groupsRaw, &proxiesRaw, &versionsRaw); err != nil {
		return err
	}
	var groups, proxies map[string][]string
	var versions map[string]int64
	if err := json.Unmarshal(groupsRaw, &groups); err != nil {
		return err
	}
	if err := json.Unmarshal(proxiesRaw, &proxies); err != nil {
		return err
	}
	if err := json.Unmarshal(versionsRaw, &versions); err != nil {
		return err
	}
	for id, values := range groups {
		accounts[positions[id]].Groups = values
	}
	for id, values := range proxies {
		accounts[positions[id]].Proxies = values
	}
	for id, version := range versions {
		accounts[positions[id]].CredentialVersion = version
	}
	// The correlated ARRAY(...) subqueries always produced '{}', never NULL.
	for i := range accounts {
		if accounts[i].Groups == nil {
			accounts[i].Groups = []string{}
		}
		if accounts[i].Proxies == nil {
			accounts[i].Proxies = []string{}
		}
	}
	return nil
}

// Hydrate only accounts selected for the current page, retaining the caller's
// transaction snapshot for metadata and credential generations.
func (s Store) queryAccountCredentials(ctx context.Context, q accountSQL, accounts []Account) ([]Account, error) {
	if len(accounts) == 0 {
		return accounts, nil
	}
	ids := make([]string, len(accounts))
	positions := make(map[string]int, len(accounts))
	for i, a := range accounts {
		ids[i] = a.ID
		positions[a.ID] = i
	}
	rows, err := q.Query(ctx, `SELECT account_id,credentials_cipher,credential_version FROM account_credentials WHERE account_id=ANY($1::text[])`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var id string
		var cipher []byte
		var version int64
		if err := rows.Scan(&id, &cipher, &version); err != nil {
			return nil, err
		}
		plain, err := s.DB.Decrypt(cipher)
		if err != nil {
			return nil, errors.New("decrypt account credentials failed")
		}
		var secret accountSecret
		if err := json.Unmarshal(plain, &secret); err != nil {
			return nil, errors.New("invalid encrypted account credentials")
		}
		a := &accounts[positions[id]]
		a.AccessToken, a.RefreshToken, a.AccountID, a.ClientID = secret.AccessToken, secret.RefreshToken, secret.AccountID, secret.ClientID
		a.CredentialVersion = version
		a.Identity = secret.Identity
		a.OwnerRevision = secret.OwnerRevision
		if !validGatewayOwnerRevision(a.OwnerRevision) {
			return nil, errors.New("invalid encrypted account ownership revision")
		}
		seen++
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if seen != len(accounts) {
		return nil, errors.New("account credentials are missing")
	}
	return accounts, nil
}

type accountSecret struct {
	AccessToken   string                  `json:"access_token"`
	RefreshToken  string                  `json:"refresh_token,omitempty"`
	AccountID     string                  `json:"account_id,omitempty"`
	ClientID      string                  `json:"client_id,omitempty"`
	Identity      *managedAccountIdentity `json:"identity,omitempty"`
	OwnerRevision string                  `json:"owner_revision,omitempty"`
}

func (s Store) credentialCipher(a Account) ([]byte, error) {
	raw, err := json.Marshal(accountSecret{AccessToken: a.AccessToken, RefreshToken: a.RefreshToken, AccountID: a.AccountID, ClientID: a.ClientID, Identity: a.Identity, OwnerRevision: a.OwnerRevision})
	if err != nil {
		return nil, err
	}
	return s.DB.Encrypt(raw)
}

func (s Store) pgGetFrom(ctx context.Context, q accountSQL, ref string, credentials bool) (Account, error) {
	accounts, err := s.queryAccounts(ctx, q, `WHERE a.id=$1 OR (a.display_name=$1 AND NOT EXISTS(SELECT 1 FROM accounts exact WHERE exact.id=$1)) ORDER BY a.id LIMIT 2`, []any{ref}, credentials)
	if err != nil {
		return Account{}, err
	}
	if len(accounts) == 0 {
		return Account{}, fmt.Errorf("%w: account", errNotFound)
	}
	if len(accounts) > 1 {
		return Account{}, fmt.Errorf("%w: ambiguous account name; use account ID", errConflict)
	}
	return accounts[0], nil
}

func (s Store) pgGet(ref string, credentials bool) (Account, error) {
	return s.pgGetContext(context.Background(), ref, credentials)
}

func (s Store) pgGetContext(parent context.Context, ref string, credentials bool) (Account, error) {
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	if !credentials {
		return s.pgGetFrom(ctx, s.DB.Pool, ref, false)
	}
	tx, err := s.DB.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return Account{}, safeDBError(err)
	}
	defer tx.Rollback(context.Background())
	a, err := s.pgGetFrom(ctx, tx, ref, true)
	if err != nil {
		return Account{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Account{}, safeDBError(err)
	}
	return a, nil
}

func (s Store) pgList(credentials bool) (string, []Account, error) {
	ctx, cancel := accountContext()
	defer cancel()
	tx, err := s.DB.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return "", nil, safeDBError(err)
	}
	defer tx.Rollback(context.Background())
	// Gateway List preserves insertion order, including accounts imported with
	// identical timestamps. Management pages use their own stable cursor order.
	accounts, err := s.queryAccounts(ctx, tx, "ORDER BY a.ordinal", nil, credentials)
	if err != nil {
		return "", nil, err
	}
	var id string
	err = tx.QueryRow(ctx, `SELECT value #>> '{}' FROM settings WHERE key='active_account_id'`).Scan(&id)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", nil, safeDBError(err)
	}
	for _, a := range accounts {
		if a.ID == id {
			return a.Name, accounts, nil
		}
	}
	return "", accounts, nil
}

func (s Store) ListMetadata() (string, []Account, error) {
	if s.DB != nil {
		return s.pgList(false)
	}
	active, accounts, err := s.List()
	for i := range accounts {
		accounts[i] = accountMetadata(accounts[i])
	}
	return active, accounts, err
}

func accountMetadata(a Account) Account {
	a.HasRefreshToken = a.HasRefreshToken || a.RefreshToken != ""
	a.AccessToken = ""
	a.RefreshToken = ""
	a.AccountID = ""
	a.ClientID = ""
	return a
}

func (s Store) GetMetadata(ref string) (Account, error) {
	return s.GetMetadataContext(context.Background(), ref)
}

func (s Store) GetMetadataContext(ctx context.Context, ref string) (Account, error) {
	if err := ctx.Err(); err != nil {
		return Account{}, err
	}
	if s.DB != nil {
		return s.pgGetContext(ctx, ref, false)
	}
	_, accounts, err := s.ListMetadata()
	if err != nil {
		return Account{}, err
	}
	for _, a := range accounts {
		if a.ID == ref {
			return a, nil
		}
	}
	var matches []Account
	for _, a := range accounts {
		if a.Name == ref {
			matches = append(matches, a)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return Account{}, fmt.Errorf("%w: ambiguous account name", errConflict)
	}
	return Account{}, errNotFound
}

// metadataByIDsContext performs the stable-ID preflight used by management
// batch actions in one bounded read.  The action handlers still run their
// existing per-item CAS transactions afterwards; this read only replaces the
// redundant one-account-at-a-time alias/version checks.  In particular, names
// are deliberately not accepted here: management actions are defined in terms
// of stable account IDs.
func (s Store) metadataByIDsContext(parent context.Context, ids []string) (map[string]Account, error) {
	result := make(map[string]Account, len(ids))
	if len(ids) == 0 {
		return result, nil
	}
	if err := parent.Err(); err != nil {
		return nil, err
	}
	if s.DB == nil {
		_, accounts, err := s.ListMetadata()
		if err != nil {
			return nil, err
		}
		wanted := make(map[string]struct{}, len(ids))
		for _, id := range ids {
			wanted[id] = struct{}{}
		}
		for _, account := range accounts {
			if _, ok := wanted[account.ID]; ok {
				result[account.ID] = account
			}
		}
		if err := parent.Err(); err != nil {
			return nil, err
		}
		return result, nil
	}
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	rows, err := s.DB.Pool.Query(ctx, `SELECT `+accountColumns+` FROM accounts a WHERE a.id=ANY($1::text[])`, ids)
	if err != nil {
		return nil, accountDBError(err)
	}
	defer rows.Close()
	for rows.Next() {
		var account Account
		if err := rows.Scan(&account.ID, &account.Name, &account.Enabled, &account.Notes, &account.Tags, &account.Concurrency, &account.Priority, &account.Weight, &account.Quota, &account.WindowDays, &account.WindowStart, &account.ExpiresAt, &account.HasRefreshToken, &account.CredentialStatus, &account.LastCheckedAt, &account.LastError, &account.CreatedAt, &account.UpdatedAt, &account.Version); err != nil {
			return nil, err
		}
		result[account.ID] = accountMetadata(account)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := parent.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func (s Store) ListPage(options AccountListOptions) (AccountPage, error) {
	page := AccountPage{Accounts: []Account{}}
	if options.Limit == 0 {
		options.Limit = 100
	}
	if options.Limit < 1 || options.Limit > 500 {
		return page, fmt.Errorf("%w: limit must be 1 to 500", errInvalid)
	}
	if options.Status != "" && !accountHas([]string{"disabled", "expired", "error", "ok", "unverified"}, options.Status) {
		return page, fmt.Errorf("%w: invalid account status", errInvalid)
	}
	var cursor accountCursor
	if options.Cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(options.Cursor)
		if err != nil || json.Unmarshal(raw, &cursor) != nil || cursor.ID == "" {
			return page, fmt.Errorf("%w: invalid account cursor", errInvalid)
		}
	}
	var accounts []Account
	var err error
	if s.DB != nil {
		ctx, cancel := accountContext()
		defer cancel()
		conditions := []string{"true"}
		args := []any{}
		add := func(fragment string, value any) {
			args = append(args, value)
			conditions = append(conditions, fmt.Sprintf(fragment, len(args)))
		}
		if options.Search != "" {
			add("strpos(lower(a.display_name),lower($%d))>0", options.Search)
		}
		if options.GroupID != "" {
			add("EXISTS(SELECT 1 FROM account_groups g WHERE g.account_id=a.id AND g.group_id=$%d)", options.GroupID)
		}
		if options.ProxyID != "" {
			add("EXISTS(SELECT 1 FROM account_proxies p WHERE p.account_id=a.id AND p.proxy_id=$%d)", options.ProxyID)
		}
		if options.Status != "" {
			// Request state is one durable row per stable account ID. This
			// indexed lookup does not scan the log and survives process restart.
			last := `(SELECT ` + managedCurrentRequestStatusSQL("ars") + ` FROM account_credential_request_state ars WHERE ars.account_ref=a.id)`
			add(managedAccountStatusSQL("extract(epoch FROM now())::bigint", last)+"=$%d", options.Status)
		}
		if options.Enabled != nil {
			add("a.enabled=$%d", *options.Enabled)
		}
		if cursor.ID != "" {
			args = append(args, cursor.CreatedAt, cursor.ID)
			conditions = append(conditions, fmt.Sprintf("(a.created_at,a.id)<($%d,$%d)", len(args)-1, len(args)))
		}
		args = append(args, options.Limit+1)
		accounts, err = s.queryAccounts(ctx, s.DB.Pool, "WHERE "+strings.Join(conditions, " AND ")+fmt.Sprintf(" ORDER BY a.created_at DESC,a.id DESC LIMIT $%d", len(args)), args, false)
	} else {
		_, all, e := s.ListMetadata()
		err = e
		for _, a := range all {
			if options.Search != "" && !strings.Contains(strings.ToLower(a.Name), strings.ToLower(options.Search)) {
				continue
			}
			lastStatus := ""
			if a.CredentialVersion > 0 && options.statusCredentialVersions[a.ID] == a.CredentialVersion {
				lastStatus = options.lastStatuses[a.ID]
			}
			if options.Status != "" && accountManagementStatus(a, lastStatus, time.Now().Unix()) != options.Status {
				continue
			}
			if options.Enabled != nil && a.Enabled != *options.Enabled {
				continue
			}
			if options.GroupID != "" && !accountHas(a.Groups, options.GroupID) {
				continue
			}
			if options.ProxyID != "" && !accountHas(a.Proxies, options.ProxyID) {
				continue
			}
			if cursor.ID != "" && (a.CreatedAt > cursor.CreatedAt || (a.CreatedAt == cursor.CreatedAt && a.ID >= cursor.ID)) {
				continue
			}
			accounts = append(accounts, a)
		}
		sort.Slice(accounts, func(i, j int) bool {
			if accounts[i].CreatedAt == accounts[j].CreatedAt {
				return accounts[i].ID > accounts[j].ID
			}
			return accounts[i].CreatedAt > accounts[j].CreatedAt
		})
	}
	if err != nil {
		return page, err
	}
	page.HasMore = len(accounts) > options.Limit
	if page.HasMore {
		accounts = accounts[:options.Limit]
	}
	if accounts != nil {
		page.Accounts = accounts
	}
	if page.HasMore {
		last := accounts[len(accounts)-1]
		raw, _ := json.Marshal(accountCursor{last.CreatedAt, last.ID})
		page.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	return page, nil
}

func accountManagementStatus(a Account, lastStatus string, now int64) string {
	if !a.Enabled {
		return "disabled"
	}
	if a.ExpiresAt > 0 && a.ExpiresAt < now {
		return "expired"
	}
	if accountHas([]string{"error", "invalid", "refresh_failed", "failed"}, a.CredentialStatus) || a.CredentialStatus == gatewayPaymentStatus && a.LastCheckedAt+int64(gatewayPaymentCooldown/time.Second) > now || (lastStatus != "" && lastStatus != "ok") {
		return "error"
	}
	if lastStatus == "ok" || (a.LastCheckedAt > 0 && a.CredentialStatus == "valid") {
		return "ok"
	}
	return "unverified"
}

func accountHas(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}

func (s Store) pgAssociations(ctx context.Context, tx pgx.Tx, a Account) error {
	return s.pgChangedAssociations(ctx, tx, a, true, true)
}

func (s Store) pgChangedAssociations(ctx context.Context, tx pgx.Tx, a Account, groups, proxies bool) error {
	if groups {
		if _, err := tx.Exec(ctx, `DELETE FROM account_groups WHERE account_id=$1`, a.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO account_groups(account_id,group_id) SELECT $1,unnest($2::text[])`, a.ID, uniqueStrings(a.Groups)); err != nil {
			return accountDBError(err)
		}
	}
	if proxies {
		if _, err := tx.Exec(ctx, `DELETE FROM account_proxies WHERE account_id=$1`, a.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO account_proxies(account_id,proxy_id,position) SELECT $1,item,ordinality::int FROM unnest($2::text[]) WITH ORDINALITY AS p(item,ordinality)`, a.ID, uniqueStrings(a.Proxies)); err != nil {
			return accountDBError(err)
		}
	}
	return nil
}

func (s Store) pgInsertAccount(ctx context.Context, tx pgx.Tx, a Account) error {
	normalizeAccount(&a)
	if a.ID == "" {
		a.ID = accountID()
	}
	if err := advanceGatewayOwnerRevision(nil, &a); err != nil {
		return err
	}
	if a.CreatedAt == 0 {
		a.CreatedAt = time.Now().Unix()
	}
	a.UpdatedAt = time.Now().Unix()
	if a.Tags == nil {
		a.Tags = []string{}
	}
	_, err := tx.Exec(ctx, `INSERT INTO accounts(id,display_name,enabled,notes,tags,concurrency,priority,weight,quota,window_days,window_start,expires_at,has_refresh_token,credential_status,last_checked_at,last_error,created_at,updated_at,version) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)`, a.ID, a.Name, a.Enabled, a.Notes, a.Tags, a.Concurrency, a.Priority, a.Weight, a.Quota, a.WindowDays, a.WindowStart, a.ExpiresAt, a.RefreshToken != "", a.CredentialStatus, a.LastCheckedAt, a.LastError, a.CreatedAt, a.UpdatedAt, a.Version)
	if err != nil {
		return accountDBError(err)
	}
	cipher, err := s.credentialCipher(a)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO account_credentials(account_id,credentials_cipher,token_fingerprint,expires_at,credential_version,encryption_key_id,updated_at) VALUES($1,$2,$3,$4,1,$5,$6)`, a.ID, cipher, tokenFingerprint(a.AccessToken), a.ExpiresAt, s.DB.KeyID, a.UpdatedAt)
	if err != nil {
		return accountDBError(err)
	}
	if err := s.pgAssociations(ctx, tx, a); err != nil {
		return err
	}
	return s.DB.audit(ctx, tx, "account.import", a.ID)
}

func (s Store) pgSaveMetadata(ctx context.Context, tx pgx.Tx, a Account, expected int64) error {
	return s.pgSaveMetadataChanges(ctx, tx, a, expected, nil)
}

func (s Store) pgSaveMetadataChanges(ctx context.Context, tx pgx.Tx, a Account, expected int64, prior *Account) error {
	if err := validAccountName(a.Name); err != nil {
		return fmt.Errorf("%w: %s", errInvalid, err)
	}
	if len(a.Notes) > 16384 || len(a.Tags) > 100 {
		return fmt.Errorf("%w: notes or tags exceed limits", errInvalid)
	}
	if a.Tags == nil {
		a.Tags = []string{}
	}
	normalizeAccount(&a)
	tag, err := tx.Exec(ctx, `UPDATE accounts SET display_name=$2,enabled=$3,notes=$4,tags=$5,concurrency=$6,priority=$7,weight=$8,quota=$9,window_days=$10,window_start=$11,credential_status=$12,last_checked_at=$13,last_error=$14,updated_at=$15,version=version+1 WHERE id=$1 AND version=$16`, a.ID, a.Name, a.Enabled, a.Notes, uniqueStrings(a.Tags), a.Concurrency, a.Priority, a.Weight, a.Quota, a.WindowDays, a.WindowStart, a.CredentialStatus, a.LastCheckedAt, a.LastError, time.Now().Unix(), expected)
	if err != nil {
		return accountDBError(err)
	}
	if tag.RowsAffected() == 0 {
		return errVersionConflict
	}
	groupsChanged, proxiesChanged := true, true
	if prior != nil {
		// Groups are sets; proxy order selects routing priority. The metadata
		// CAS above now holds the account row lock. All supported relation
		// writers also advance its version, so a stale read cannot skip a change.
		before, after := uniqueStrings(slices.Clone(prior.Groups)), uniqueStrings(slices.Clone(a.Groups))
		slices.Sort(before)
		slices.Sort(after)
		groupsChanged = !slices.Equal(before, after)
		proxiesChanged = !slices.Equal(uniqueStrings(prior.Proxies), uniqueStrings(a.Proxies))
	}
	if err := s.pgChangedAssociations(ctx, tx, a, groupsChanged, proxiesChanged); err != nil {
		return err
	}
	return s.DB.audit(ctx, tx, "account.update", a.ID)
}

func (s Store) UpdateAccountVersion(ref string, expected int64, apply func(*Account) error) (Account, error) {
	return s.UpdateAccountVersionContext(context.Background(), ref, expected, apply)
}

func (s Store) UpdateAccountVersionContext(parent context.Context, ref string, expected int64, apply func(*Account) error) (Account, error) {
	if err := parent.Err(); err != nil {
		return Account{}, err
	}
	if s.DB == nil {
		var result Account
		err := s.modify(func(state *accountState) error {
			if err := parent.Err(); err != nil {
				return err
			}
			for i, current := range state.Accounts {
				if current.ID != ref && current.Name != ref {
					continue
				}
				if expected > 0 && current.Version != expected {
					return errVersionConflict
				}
				updated := current
				if err := apply(&updated); err != nil {
					return err
				}
				if err := parent.Err(); err != nil {
					return err
				}
				if validAccountName(updated.Name) != nil {
					return errInvalid
				}
				for _, other := range state.Accounts {
					if other.ID != current.ID && other.Name == updated.Name {
						return errConflict
					}
				}
				updated.ID = current.ID
				updated.Version = current.Version + 1
				updated.UpdatedAt = time.Now().Unix()
				state.Accounts[i] = updated
				if state.Active == current.Name {
					state.Active = updated.Name
				}
				result = accountMetadata(updated)
				return nil
			}
			return errNotFound
		})
		return result, err
	}
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	tx, err := s.DB.Pool.Begin(ctx)
	if err != nil {
		return Account{}, err
	}
	defer tx.Rollback(context.Background())
	current, err := s.pgGetFrom(ctx, tx, ref, false)
	if err != nil {
		return Account{}, err
	}
	if expected > 0 && current.Version != expected {
		return Account{}, errVersionConflict
	}
	updated := current
	current.Groups = slices.Clone(current.Groups)
	current.Proxies = slices.Clone(current.Proxies)
	if err := apply(&updated); err != nil {
		return Account{}, err
	}
	updated.ID = current.ID
	if updated.Name != current.Name {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('codex-agent:import:' || current_schema(), 0))`); err != nil {
			return Account{}, err
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM accounts WHERE display_name=$1 AND id<>$2)`, updated.Name, current.ID).Scan(&exists); err != nil {
			return Account{}, err
		}
		if exists {
			return Account{}, errConflict
		}
	}
	if err := s.pgSaveMetadataChanges(ctx, tx, updated, current.Version, &current); err != nil {
		return Account{}, err
	}
	result, err := s.pgGetFrom(ctx, tx, current.ID, false)
	if err != nil {
		return Account{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Account{}, err
	}
	s.DB.invalidate()
	return result, nil
}

func (s Store) DeleteVersion(ref string, expected int64) error {
	if s.DB != nil {
		return s.pgDelete(ref, expected)
	}
	return s.modify(func(state *accountState) error {
		for i, a := range state.Accounts {
			if a.ID != ref && a.Name != ref {
				continue
			}
			if expected > 0 && a.Version != expected {
				return errVersionConflict
			}
			state.Accounts = append(state.Accounts[:i], state.Accounts[i+1:]...)
			delete(state.AdminCompat, a.ID)
			if state.Active == a.Name {
				state.Active = ""
			}
			return nil
		}
		return errNotFound
	})
}

func (s Store) pgDelete(ref string, expected int64) error {
	ctx, cancel := accountContext()
	defer cancel()
	tx, err := s.DB.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	a, err := s.pgGetFrom(ctx, tx, ref, false)
	if err != nil {
		return err
	}
	if expected > 0 && expected != a.Version {
		return errVersionConflict
	}
	tag, err := tx.Exec(ctx, `DELETE FROM accounts WHERE id=$1 AND version=$2`, a.ID, a.Version)
	if err != nil {
		return accountDBError(err)
	}
	if tag.RowsAffected() != 1 {
		return errVersionConflict
	}
	compatKey, _ := managementStateKey(compatRecordNamespace, a.ID)
	if _, err := tx.Exec(ctx, `DELETE FROM settings WHERE key=$1`, compatKey); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM settings WHERE key='active_account_id' AND value=to_jsonb($1::text)`, a.ID); err != nil {
		return err
	}
	if err := s.DB.audit(ctx, tx, "account.delete", a.ID); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	s.DB.invalidate()
	return nil
}

func (s Store) pgUse(ref string) error {
	ctx, cancel := accountContext()
	defer cancel()
	tx, err := s.DB.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	a, err := s.pgGetFrom(ctx, tx, ref, false)
	if err != nil {
		return err
	}
	// Keep the selection valid until commit. A concurrent deletion then removes
	// this setting in its own transaction after this key-share lock is released.
	var selectedID string
	if err := tx.QueryRow(ctx, `SELECT id FROM accounts WHERE id=$1 FOR KEY SHARE`, a.ID).Scan(&selectedID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errNotFound
		}
		return safeDBError(err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO settings(key,value,version) VALUES('active_account_id',to_jsonb($1::text),1) ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value,version=settings.version+1`, a.ID)
	if err != nil {
		return err
	}
	if err := s.DB.audit(ctx, tx, "account.select", a.ID); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	s.DB.invalidate()
	return nil
}

func (s Store) pgActive() (Account, error) {
	ctx, cancel := accountContext()
	defer cancel()
	tx, err := s.DB.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return Account{}, safeDBError(err)
	}
	defer tx.Rollback(context.Background())
	var ref string
	if err := tx.QueryRow(ctx, `SELECT value #>> '{}' FROM settings WHERE key='active_account_id'`).Scan(&ref); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Account{}, errors.New("no active account; import or select an account first")
		}
		return Account{}, err
	}
	a, err := s.pgGetFrom(ctx, tx, ref, true)
	if err != nil {
		return Account{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Account{}, safeDBError(err)
	}
	return a, nil
}

func (s Store) pgApplyAll(change func(*accountState) error) error {
	ctx, cancel := accountContext()
	defer cancel()
	tx, err := s.DB.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	accounts, err := s.queryAccounts(ctx, tx, "ORDER BY a.id", nil, false)
	if err != nil {
		return err
	}
	state := accountState{Accounts: accounts}
	versions := make(map[string]int64, len(accounts))
	for _, a := range accounts {
		versions[a.ID] = a.Version
	}
	if err := change(&state); err != nil {
		return err
	}
	for _, a := range state.Accounts {
		version, ok := versions[a.ID]
		if !ok {
			return fmt.Errorf("%w: ApplyAll only supports account metadata changes", errInvalid)
		}
		if err := s.pgSaveMetadata(ctx, tx, a, version); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	s.DB.invalidate()
	return nil
}

func (s Store) pgImportDetailed(parsed []Account, report ImportReport, options ImportOptions) (ImportReport, error) {
	return s.pgImportDetailedContext(context.Background(), parsed, report, options)
}

func (s Store) pgImportDetailedContext(parent context.Context, parsed []Account, report ImportReport, options ImportOptions) (ImportReport, error) {
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()
	tx, err := s.DB.Pool.Begin(ctx)
	if err != nil {
		return report, err
	}
	defer tx.Rollback(context.Background())
	report, err = s.pgImportDetailedTx(ctx, tx, parsed, report, options)
	if err != nil || options.DryRun {
		return report, err
	}
	if err := tx.Commit(ctx); err != nil {
		return report, err
	}
	s.DB.invalidate()
	return report, nil
}

// pgImportDetailedTx preserves native import decisions inside an existing
// transaction. Its caller owns commit/rollback, dry-run and cache invalidation.
// It must not reacquire a pool connection while that transaction is held.
func (s Store) pgImportDetailedTx(ctx context.Context, tx pgx.Tx, parsed []Account, report ImportReport, options ImportOptions) (ImportReport, error) {
	// Imports and display-name renames serialize only their short database work.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('codex-agent:import:' || current_schema(), 0))`); err != nil {
		return report, err
	}
	if inserted, err := s.pgImportNewAccounts(ctx, tx, parsed, &report); err != nil {
		return report, err
	} else if inserted {
		return report, nil
	}
	if unchanged, err := s.pgImportUnchangedAccounts(ctx, tx, parsed, &report); err != nil {
		return report, err
	} else if unchanged {
		return report, nil
	}
	for i, incoming := range parsed {
		if report.Results[i].Status == "failed" {
			continue
		}
		result := &report.Results[i]
		var duplicate string
		err := tx.QueryRow(ctx, `SELECT account_id FROM account_credentials WHERE token_fingerprint=$1 ORDER BY account_id LIMIT 1`, tokenFingerprint(incoming.AccessToken)).Scan(&duplicate)
		if err == nil {
			matches, readErr := s.queryAccounts(ctx, tx, "WHERE a.id=$1", []any{duplicate}, true)
			if readErr != nil {
				return report, readErr
			}
			if len(matches) != 1 {
				return report, errNotFound
			}
			updated, rotated, rotationErr := importedRefreshRotation(matches[0], incoming)
			if rotationErr != nil {
				result.Status, result.Code, result.Error = "failed", "identity_conflict", "duplicate access token has conflicting account identity"
				report.Failed++
				continue
			}
			if rotated {
				if err := s.pgWriteTokens(ctx, tx, matches[0], updated); err != nil {
					return report, err
				}
				result.Status, result.ID = "replaced", duplicate
				report.Replaced++
			} else {
				result.Status, result.ID = "duplicate", duplicate
				report.Duplicate++
			}
			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return report, err
		}
		// Import names are display names, even when one happens to resemble a
		// different account's stable ID. Path lookup's ID precedence must not
		// redirect an explicit credential replacement to that other account.
		matches, err := s.queryAccounts(ctx, tx, `WHERE a.display_name=$1 ORDER BY a.id LIMIT 2`, []any{incoming.Name}, true)
		var current Account
		if err == nil {
			switch len(matches) {
			case 0:
				err = errNotFound
			case 1:
				current = matches[0]
			default:
				err = errConflict
			}
		}
		if err == nil {
			if !options.Replace {
				result.Status, result.Code, result.Error = "failed", "name_conflict", "an account with this name already exists; replacement must be explicit"
				report.Failed++
				continue
			}
			updated := mergeImport(current, incoming)
			if err := s.pgWriteTokens(ctx, tx, current, updated); err != nil {
				return report, err
			}
			result.Status, result.ID = "replaced", current.ID
			report.Replaced++
		} else if errors.Is(err, errNotFound) {
			if err := s.pgInsertAccount(ctx, tx, incoming); err != nil {
				return report, err
			}
			// Select only the first-ever account. Existing settings are not changed.
			_, err := tx.Exec(ctx, `INSERT INTO settings(key,value,version) SELECT 'active_account_id',to_jsonb($1::text),1 WHERE NOT EXISTS(SELECT 1 FROM accounts WHERE id<>$1) ON CONFLICT(key) DO NOTHING`, incoming.ID)
			if err != nil {
				return report, err
			}
			result.Status, result.ID = "added", incoming.ID
			report.Added++
		} else if errors.Is(err, errConflict) {
			result.Status, result.Code, result.Error = "failed", "ambiguous_name", "multiple accounts share this name; use a stable ID"
			report.Failed++
		} else {
			return report, err
		}
	}
	return report, nil
}

func (s Store) pgWriteTokens(ctx context.Context, tx pgx.Tx, current, updated Account) error {
	return s.pgWriteTokensWithPaymentPolicy(ctx, tx, current, updated, false)
}

func (s Store) pgWriteTokensWithPaymentPolicy(ctx context.Context, tx pgx.Tx, current, updated Account, preservePayment bool) error {
	if strings.TrimSpace(updated.AccessToken) == "" {
		return fmt.Errorf("%w: access_token must not be empty", errInvalid)
	}
	if err := advanceGatewayOwnerRevision(&current, &updated); err != nil {
		return err
	}
	cipher, err := s.credentialCipher(updated)
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE account_credentials SET credentials_cipher=$2,token_fingerprint=$3,expires_at=$4,credential_version=credential_version+1,encryption_key_id=$5,updated_at=$6 WHERE account_id=$1 AND credential_version=$7`, current.ID, cipher, tokenFingerprint(updated.AccessToken), updated.ExpiresAt, s.DB.KeyID, time.Now().Unix(), current.CredentialVersion)
	if err != nil {
		return accountDBError(err)
	}
	if tag.RowsAffected() != 1 {
		return ErrTokenConflict
	}
	tag, err = tx.Exec(ctx, `UPDATE accounts SET expires_at=$2,has_refresh_token=$3,
	 credential_status=CASE WHEN $7 AND credential_status='payment_required' THEN credential_status ELSE $4 END,
	 last_error=CASE WHEN $7 AND credential_status='payment_required' THEN last_error ELSE '' END,
	 last_checked_at=CASE WHEN $7 AND credential_status='payment_required' THEN last_checked_at ELSE $6 END,
	 updated_at=$5,version=version+1 WHERE id=$1`, current.ID, updated.ExpiresAt, updated.RefreshToken != "", "unverified", time.Now().Unix(), updated.LastCheckedAt, preservePayment)
	if err != nil {
		return accountDBError(err)
	}
	if tag.RowsAffected() != 1 {
		return ErrTokenConflict
	}
	return s.DB.audit(ctx, tx, "account.credentials.update", current.ID)
}

func (s Store) pgRefreshFailure(ctx context.Context, conn accountSQL, current Account) error {
	// Lock the credential row in the short write transaction before checking
	// its version, so a concurrent successful re-import cannot be marked failed.
	var credentialVersion int64
	err := conn.QueryRow(ctx, `SELECT credential_version FROM account_credentials WHERE account_id=$1 FOR UPDATE`, current.ID).Scan(&credentialVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return safeDBError(err)
	}
	if credentialVersion != current.CredentialVersion {
		return nil
	}
	_, err = conn.Exec(ctx, `UPDATE accounts SET
	 credential_status=CASE WHEN credential_status='payment_required' THEN credential_status ELSE 'refresh_failed' END,
	 last_checked_at=CASE WHEN credential_status='payment_required' THEN last_checked_at ELSE $2 END,
	 last_error=CASE WHEN credential_status='payment_required' THEN last_error ELSE 'credential refresh failed' END,
	 updated_at=$2,version=version+1 WHERE id=$1`, current.ID, time.Now().Unix())
	if err != nil {
		return safeDBError(err)
	}
	_, err = conn.Exec(ctx, `INSERT INTO audit_logs(action,entity_id,result) VALUES('account.credentials.refresh',$1,'failed')`, current.ID)
	return safeDBError(err)
}

// ImportLegacyAccounts participates in the caller's migration transaction, so
// relations and the active selection are committed together with other stores.
// The importer validates its source manifest before calling this helper. A
// second identical invocation is safe; any account difference is a conflict.
func (s Store) ImportLegacyAccounts(ctx context.Context, tx pgx.Tx, state accountState) error {
	if s.DB == nil {
		return errors.New("legacy migration requires PostgreSQL")
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('codex-agent:import:' || current_schema(), 0))`); err != nil {
		return err
	}
	seen := make(map[string]bool, len(state.Accounts))
	var activeID string
	for _, account := range state.Accounts {
		if validAccountName(account.Name) != nil || strings.TrimSpace(account.AccessToken) == "" {
			return fmt.Errorf("%w: invalid legacy account", errInvalid)
		}
		normalizeAccount(&account)
		if seen[account.ID] {
			return fmt.Errorf("%w: duplicate legacy account ID", errConflict)
		}
		seen[account.ID] = true
		if account.Name == state.Active {
			activeID = account.ID
		}
		current, err := s.pgGetFrom(ctx, tx, account.ID, true)
		if err == nil {
			// Compare every persisted management and credential field. Version
			// and update timestamps are generated by the destination.
			current.CredentialVersion, account.CredentialVersion = 0, 0
			current.UpdatedAt, account.UpdatedAt = 0, 0
			current.Version, account.Version = 0, 0
			current.HasRefreshToken, account.HasRefreshToken = false, false
			if account.CreatedAt == 0 {
				current.CreatedAt = 0
			}
			// Group membership is a set; PostgreSQL reads it in deterministic
			// ID order even if the source document used a different order.
			current.Groups = append([]string(nil), current.Groups...)
			account.Groups = append([]string(nil), account.Groups...)
			sort.Strings(current.Groups)
			sort.Strings(account.Groups)
			left, _ := json.Marshal(current)
			right, _ := json.Marshal(account)
			if !jsonEqualAccount(left, right) {
				return fmt.Errorf("%w: legacy account differs from destination", errConflict)
			}
			continue
		}
		if !errors.Is(err, errNotFound) {
			return err
		}
		if err := s.pgInsertAccount(ctx, tx, account); err != nil {
			return err
		}
	}
	if state.Active != "" && activeID == "" {
		return fmt.Errorf("%w: active legacy account is missing", errInvalid)
	}
	if activeID != "" {
		_, err := tx.Exec(ctx, `INSERT INTO settings(key,value,version) VALUES('active_account_id',to_jsonb($1::text),1) ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value,version=settings.version+1`, activeID)
		return err
	}
	return nil
}

func jsonEqualAccount(left, right []byte) bool { return string(left) == string(right) }

func (s Store) pgUpdateTokens(ref, previous string, updated Account) error {
	ctx, cancel := accountContext()
	defer cancel()
	tx, err := s.DB.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	current, err := s.pgGetFrom(ctx, tx, ref, true)
	if err != nil {
		if errors.Is(err, errNotFound) {
			return fmt.Errorf("%w: %w", errNotFound, ErrTokenConflict)
		}
		return err
	}
	if current.AccessToken != previous {
		return ErrTokenConflict
	}
	updated = mergeAccountTokens(current, updated)
	if err := s.pgWriteTokensWithPaymentPolicy(ctx, tx, current, updated, true); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	s.DB.invalidate()
	return nil
}

func (s Store) pgRefresh(ref, previous string, refresh func(Account) (Account, error)) (Account, error) {
	return s.pgRefreshContext(context.Background(), ref, previous, refresh)
}

func (s Store) pgRefreshContext(parent context.Context, ref, previous string, refresh func(Account) (Account, error)) (result Account, resultErr error) {
	current, err := s.pgGetContext(parent, ref, false)
	if err := parent.Err(); err != nil {
		return Account{}, err
	}
	if err != nil {
		return Account{}, err
	}
	operationDeadline := time.Now().Add(2 * time.Minute)
	ctx, cancel := context.WithDeadline(parent, operationDeadline)
	defer cancel()
	waitCtx, callbackStarted := ctx, false
	defer func() {
		if !callbackStarted && waitCtx.Err() != nil {
			result, resultErr = Account{}, waitCtx.Err()
		}
	}()
	cfg := s.DB.Pool.Config().ConnConfig
	localKey := fmt.Sprintf("%s:%d/%s/%s/%s", cfg.Host, cfg.Port, cfg.Database, cfg.RuntimeParams["search_path"], current.ID)
	releaseLocal, err := waitAccountRefresh(ctx, localKey)
	if err != nil {
		return Account{}, err
	}
	defer releaseLocal()
	conn, err := s.DB.Pool.Acquire(ctx)
	if err != nil {
		return Account{}, err
	}
	hash := fnv.New64a()
	var scope string
	if err := conn.QueryRow(ctx, `SELECT current_database() || ':' || current_schema()`).Scan(&scope); err != nil {
		conn.Release()
		return Account{}, safeDBError(err)
	}
	_, _ = hash.Write([]byte("codex-agent:refresh:" + scope + ":" + current.ID))
	lockID := int64(hash.Sum64())
	locked := false
	defer func() {
		if conn == nil {
			return
		}
		if locked {
			unlockCtx, unlockCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer unlockCancel()
			var released bool
			err := conn.QueryRow(unlockCtx, `SELECT pg_advisory_unlock($1)`, lockID).Scan(&released)
			if err != nil || !released {
				raw := conn.Hijack()
				_ = raw.Close(unlockCtx)
				return
			}
		}
		conn.Release()
	}()
	for !locked {
		if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, lockID).Scan(&locked); err != nil {
			// An interrupted response may have acquired the session lock. Close
			// this physical connection instead of returning it to the pool.
			raw := conn.Hijack()
			_ = raw.Close(context.Background())
			conn = nil
			return Account{}, safeDBError(err)
		}
		if !locked {
			timer := time.NewTimer(25 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return Account{}, ctx.Err()
			case <-timer.C:
			}
		}
	}
	readTx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return Account{}, safeDBError(err)
	}
	current, err = s.pgGetFrom(ctx, readTx, current.ID, true)
	if err != nil {
		_ = readTx.Rollback(context.Background())
		return Account{}, err
	}
	if err := readTx.Commit(ctx); err != nil {
		return Account{}, safeDBError(err)
	}
	if err := ctx.Err(); err != nil {
		return Account{}, err
	}
	if current.AccessToken != previous {
		return current, nil
	}
	// This connection holds only a session advisory lock. No transaction spans
	// the upstream exchange. Keep the original operation deadline, not a shorter
	// caller deadline or a newly reset budget, when protecting rotated tokens.
	ctx, persistCancel := context.WithDeadline(context.WithoutCancel(parent), operationDeadline)
	defer persistCancel()
	callbackStarted = true
	updated, err := refresh(current)
	if err != nil {
		// Rejection before exchange is not evidence of an invalid credential.
		// Do not suppress real exchange failures just because the caller left.
		if errors.Is(err, errManagedRefreshPrecondition) || errors.Is(err, errAccountRefreshNotStarted) {
			return Account{}, err
		}
		failureCtx, failureCancel := accountContext()
		defer failureCancel()
		tx, beginErr := conn.Begin(failureCtx)
		if beginErr != nil {
			return Account{}, errors.Join(err, safeDBError(beginErr))
		}
		defer tx.Rollback(context.Background())
		if saveErr := s.pgRefreshFailure(failureCtx, tx, current); saveErr != nil {
			return Account{}, errors.Join(err, saveErr)
		}
		if commitErr := tx.Commit(failureCtx); commitErr != nil {
			return Account{}, errors.Join(err, safeDBError(commitErr))
		}
		s.DB.invalidate()
		return Account{}, err
	}
	updated = mergeAccountTokens(current, updated)
	updated.LastCheckedAt = time.Now().Unix()
	tx, err := conn.Begin(ctx)
	if err != nil {
		return Account{}, err
	}
	defer tx.Rollback(context.Background())
	if err := s.pgWriteTokensWithPaymentPolicy(ctx, tx, current, updated, true); err != nil {
		return Account{}, err
	}
	result, err = s.pgGetFrom(ctx, tx, current.ID, true)
	if err != nil {
		return Account{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Account{}, err
	}
	s.DB.invalidate()
	return result, nil
}
