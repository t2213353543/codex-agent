package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path"
	"reflect"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const compatRecordNamespace = "admin-compat"

func compatCopyRecord(r adminCompatRecord) adminCompatRecord {
	raw, _ := json.Marshal(r)
	var out adminCompatRecord
	_ = json.Unmarshal(raw, &out)
	if out.CPA == nil {
		out.CPA = map[string]json.RawMessage{}
	}
	if out.Sub2 == nil {
		out.Sub2 = map[string]json.RawMessage{}
	}
	return out
}

// marshalCompatRecord gives a stable byte form for change detection; json
// marshals map keys in sorted order and omitempty treats nil and empty maps
// alike, so identical payloads compare equal.
func marshalCompatRecord(r adminCompatRecord) []byte {
	raw, _ := json.Marshal(r)
	return raw
}

func compatRecordsPG(ctx context.Context, db *Database, q accountSQL, ids []string) (map[string]adminCompatRecord, error) {
	out := make(map[string]adminCompatRecord, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	names := make([]string, len(ids))
	byName := make(map[string]string, len(ids))
	for i, id := range ids {
		names[i], _ = managementStateKey(compatRecordNamespace, id)
		byName[names[i]] = id
	}
	rows, err := q.Query(ctx, "SELECT key,value FROM settings WHERE key=ANY($1::text[])", names)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var raw []byte
		if err := rows.Scan(&key, &raw); err != nil {
			return nil, err
		}
		var envelope struct {
			Ciphertext []byte `json:"ciphertext"`
		}
		if json.Unmarshal(raw, &envelope) != nil {
			return nil, errInvalid
		}
		plain, err := db.Decrypt(envelope.Ciphertext)
		if err != nil {
			return nil, err
		}
		var entry managementStateEntry
		if json.Unmarshal(plain, &entry) != nil || entry.Version < 1 {
			return nil, errInvalid
		}
		var record adminCompatRecord
		if json.Unmarshal(entry.Payload, &record) != nil {
			return nil, errInvalid
		}
		out[byName[key]] = record
	}
	return out, rows.Err()
}

func (p *Proxy) compatListAccounts(ctx context.Context, credentials bool) ([]adminCompatAccount, error) {
	return p.compatSelectAccounts(ctx, credentials, nil)
}

// sub2SelectedAccounts resolves numeric compatibility IDs without loading the
// full account catalog or decrypting unrelated credentials.  Numeric IDs are a
// stable hash of the native ID, so PostgreSQL cannot seek them directly; the
// initial projection therefore scans only account IDs and keeps all expensive
// metadata/credential hydration bounded to the requested rows.
func (p *Proxy) sub2SelectedAccounts(rctx context.Context, r *http.Request, selected map[int64]bool) ([]adminCompatAccount, error) {
	if err := rctx.Err(); err != nil {
		return nil, err
	}
	if len(selected) == 0 {
		return []adminCompatAccount{}, nil
	}
	if p.Store.DB == nil {
		state, err := p.Store.readState()
		if err != nil {
			return nil, err
		}
		rows := make([]adminCompatAccount, 0, len(selected))
		seen := make(map[int64]string, len(state.Accounts))
		for _, account := range state.Accounts {
			numeric := compatNumericID(account.ID)
			if prior := seen[numeric]; prior != "" && prior != account.ID {
				return nil, errConflict
			}
			seen[numeric] = account.ID
			if selected[numeric] {
				rows = append(rows, adminCompatAccount{Account: account, Record: compatCopyRecord(state.AdminCompat[account.ID])})
			}
		}
		if len(rows) != len(selected) {
			return nil, errNotFound
		}
		return sub2FilterAccountRows(r, rows), nil
	}

	db := p.Store.DB
	tx, err := db.Pool.BeginTx(rctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.Background())
	ids := make([]string, 0, len(selected))
	seen := make(map[int64]string)
	rows, err := tx.Query(rctx, "SELECT id FROM accounts ORDER BY created_at DESC,id DESC")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		numeric := compatNumericID(id)
		if prior := seen[numeric]; prior != "" && prior != id {
			rows.Close()
			return nil, errConflict
		}
		seen[numeric] = id
		if selected[numeric] {
			ids = append(ids, id)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if len(ids) != len(selected) {
		return nil, errNotFound
	}
	accounts, err := p.Store.queryAccounts(rctx, tx, "WHERE a.id=ANY($1::text[]) ORDER BY a.created_at DESC,a.id DESC", []any{ids}, true)
	if err != nil {
		return nil, err
	}
	records, err := compatRecordsPG(rctx, db, tx, ids)
	if err != nil {
		return nil, err
	}
	joined, err := compatJoinAccountRecords(rctx, accounts, records)
	if err != nil {
		return nil, err
	}
	joined = sub2FilterAccountRows(r, joined)
	if len(joined) != len(selected) {
		return nil, errNotFound
	}
	if err := tx.Commit(rctx); err != nil {
		return nil, err
	}
	return joined, nil
}

// Apply metadata-only selections before hydrating credentials. The transaction
// keeps account metadata, preserved aliases and selected secrets coherent.
func (p *Proxy) compatSelectAccounts(ctx context.Context, credentials bool, selectRows func([]adminCompatAccount) []adminCompatAccount) ([]adminCompatAccount, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var accounts []Account
	var records map[string]adminCompatRecord
	if db := p.Store.DB; db != nil {
		tx, err := db.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
		if err != nil {
			return nil, err
		}
		defer tx.Rollback(context.Background())
		accounts, err = p.Store.queryAccounts(ctx, tx, "ORDER BY a.created_at DESC,a.id DESC", nil, false)
		if err != nil {
			return nil, err
		}
		ids := make([]string, len(accounts))
		for i, a := range accounts {
			ids[i] = a.ID
		}
		records, err = compatRecordsPG(ctx, db, tx, ids)
		if err != nil {
			return nil, err
		}
		rows, err := compatJoinAccountRecords(ctx, accounts, records)
		if err != nil {
			return nil, err
		}
		if selectRows != nil {
			rows = selectRows(rows)
		}
		if credentials {
			selected := make([]Account, len(rows))
			for i, row := range rows {
				selected[i] = row.Account
			}
			selected, err = p.Store.queryAccountCredentials(ctx, tx, selected)
			if err != nil {
				return nil, err
			}
			for i := range rows {
				rows[i].Account = selected[i]
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return rows, nil
	} else {
		state, err := p.Store.readState()
		if err != nil {
			return nil, err
		}
		accounts, records = state.Accounts, state.AdminCompat
		if !credentials {
			for i, a := range accounts {
				accounts[i] = accountMetadata(a)
			}
		}
	}
	rows, err := compatJoinAccountRecords(ctx, accounts, records)
	if err != nil {
		return nil, err
	}
	if selectRows != nil {
		rows = selectRows(rows)
	}
	return rows, nil
}

func compatJoinAccountRecords(ctx context.Context, accounts []Account, records map[string]adminCompatRecord) ([]adminCompatAccount, error) {
	out := make([]adminCompatAccount, 0, len(accounts))
	seen := make(map[int64]string, len(accounts))
	for _, a := range accounts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n := compatNumericID(a.ID)
		if prior := seen[n]; prior != "" && prior != a.ID {
			return nil, errConflict
		}
		seen[n] = a.ID
		out = append(out, adminCompatAccount{a, compatCopyRecord(records[a.ID])})
	}
	return out, nil
}

func (p *Proxy) compatGetAccount(ctx context.Context, id string) (adminCompatAccount, error) {
	if err := ctx.Err(); err != nil {
		return adminCompatAccount{}, err
	}
	if db := p.Store.DB; db != nil {
		tx, err := db.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
		if err != nil {
			return adminCompatAccount{}, err
		}
		defer tx.Rollback(context.Background())
		a, err := p.Store.pgGetFrom(ctx, tx, id, true)
		if err != nil {
			return adminCompatAccount{}, err
		}
		if a.ID != id {
			return adminCompatAccount{}, errNotFound
		}
		records, err := compatRecordsPG(ctx, db, tx, []string{id})
		if err != nil {
			return adminCompatAccount{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return adminCompatAccount{}, err
		}
		return adminCompatAccount{a, compatCopyRecord(records[id])}, nil
	}
	state, err := p.Store.readState()
	if err != nil {
		return adminCompatAccount{}, err
	}
	for _, a := range state.Accounts {
		if a.ID == id {
			return adminCompatAccount{a, compatCopyRecord(state.AdminCompat[id])}, nil
		}
	}
	return adminCompatAccount{}, errNotFound
}

func compatValidateAccount(a Account, r adminCompatRecord) error {
	if validAccountName(a.Name) != nil || len(a.Name) > 100 || a.AccessToken == "" || len(a.AccessToken) > 128<<10 || strings.ContainsAny(a.AccessToken, " \r\n\t") || a.Concurrency < 1 || a.Concurrency > 1000 || a.Weight < 1 || a.Weight > 1000 || len(a.Notes) > 16384 || len(a.Tags) > 100 {
		return errInvalid
	}
	if a.ExpiresAt < 0 || a.ExpiresAt > 253402300799 || a.WindowDays < 1 || a.WindowDays > 365 || a.Priority < -2147483648 || a.Priority > 2147483647 {
		return errInvalid
	}
	if r.FileName != "" && (len(r.FileName) > 240 || path.Base(r.FileName) != r.FileName || strings.ContainsAny(r.FileName, "\\\x00\r\n") || !strings.HasSuffix(r.FileName, ".json")) {
		return errInvalid
	}
	raw, err := json.Marshal(r)
	if err != nil || len(raw) > 2<<20 {
		return errInvalid
	}
	return nil
}

func compatCredentialsChanged(a, b Account) bool {
	return a.AccessToken != b.AccessToken || a.RefreshToken != b.RefreshToken || a.AccountID != b.AccountID || a.ClientID != b.ClientID || a.ExpiresAt != b.ExpiresAt || !reflect.DeepEqual(a.Identity, b.Identity)
}

func compatSyncCredentialMetadata(previous adminCompatRecord, next *adminCompatRecord, a Account) error {
	var oldSub2, newSub2 map[string]json.RawMessage
	_ = json.Unmarshal(previous.Sub2["credentials"], &oldSub2)
	_ = json.Unmarshal(next.Sub2["credentials"], &newSub2)
	oldCPA, newCPA := previous.CPA["id_token"], next.CPA["id_token"]
	oldID, newID := oldSub2["id_token"], newSub2["id_token"]
	cpaChanged, sub2Changed := !bytes.Equal(oldCPA, newCPA), !bytes.Equal(oldID, newID)
	if cpaChanged && sub2Changed && !bytes.Equal(newCPA, newID) {
		return errConflict
	}
	idToken := newCPA
	if sub2Changed {
		idToken = newID
	} else if !cpaChanged && len(idToken) == 0 {
		idToken = newID
	}
	if next.CPA == nil {
		next.CPA = map[string]json.RawMessage{}
	}
	if len(idToken) > 0 {
		next.CPA["id_token"] = bytes.Clone(idToken)
	} else {
		delete(next.CPA, "id_token")
	}
	if next.Sub2 == nil {
		next.Sub2 = map[string]json.RawMessage{}
	}
	if newSub2 == nil {
		newSub2 = map[string]json.RawMessage{}
	}
	if len(idToken) > 0 {
		newSub2["id_token"] = bytes.Clone(idToken)
	} else {
		delete(newSub2, "id_token")
	}
	for key, value := range map[string]any{"access_token": a.AccessToken, "refresh_token": a.RefreshToken, "account_id": a.AccountID, "chatgpt_account_id": a.AccountID, "client_id": a.ClientID, "expires_at": a.ExpiresAt} {
		newSub2[key], _ = json.Marshal(value)
	}
	next.Sub2["credentials"], _ = json.Marshal(newSub2)
	return nil
}

func compatLeaseChange(previous, next adminCompatRecord, lease *managedAccountLease, a *Account) (bool, error) {
	if bytes.Equal(previous.Sub2["expires_at"], next.Sub2["expires_at"]) && bytes.Equal(previous.Sub2["auto_pause_on_expired"], next.Sub2["auto_pause_on_expired"]) {
		if lease.AutoDisable && lease.ExpiresAt > 0 && lease.ExpiresAt <= time.Now().Unix() && a.Enabled {
			a.Enabled = false
			lease.DisabledByLease = true
			lease.DisabledAt = time.Now().Unix()
			lease.DisabledVersion = a.Version
			lease.Version++
			return true, nil
		}
		return false, nil
	}
	if next.Sub2["expires_at"] == nil && next.Sub2["auto_pause_on_expired"] == nil {
		return false, nil
	}
	object := map[string]any{"platform": "openai", "type": "oauth", "credentials": map[string]any{}}
	for _, key := range []string{"expires_at", "auto_pause_on_expired"} {
		if raw := next.Sub2[key]; raw != nil {
			object[key] = raw
		}
	}
	raw, _ := json.Marshal(object)
	parsed, err := parseManagedExternalLease(raw)
	if err != nil || parsed == nil {
		return false, errInvalid
	}
	lease.ExpiresAt, lease.AutoDisable = parsed.ExpiresAt, parsed.AutoDisable
	lease.Version++
	lease.RestoreOnExtend = false
	if lease.AutoDisable && lease.ExpiresAt > 0 && lease.ExpiresAt <= time.Now().Unix() && a.Enabled {
		a.Enabled = false
		lease.DisabledByLease = true
		lease.DisabledAt = time.Now().Unix()
		lease.DisabledVersion = a.Version
	} else if a.Enabled {
		lease.DisabledByLease = false
		lease.DisabledVersion = 0
		lease.DisabledAt = 0
	}
	return true, nil
}

// A compatibility write is one account/credential/alias/metadata/lease commit.
// Existing credential and owner-generation helpers still invalidate old sessions.
func (p *Proxy) compatMutateAccount(ctx context.Context, id string, expected int64, change func(*Account, *adminCompatRecord) error) (adminCompatAccount, error) {
	if err := ctx.Err(); err != nil {
		return adminCompatAccount{}, err
	}
	creating := id == ""
	if creating {
		id = accountID()
	}
	initial := func() Account {
		now := time.Now().Unix()
		return Account{ID: id, Version: 1, CredentialVersion: 1, Enabled: true, Concurrency: 1, Weight: 1, WindowDays: 30, CredentialStatus: "unverified", CreatedAt: now, UpdatedAt: now}
	}
	if p.Store.DB == nil {
		var result adminCompatAccount
		err := p.Store.modify(func(state *accountState) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			index := -1
			a := initial()
			var previous adminCompatRecord
			for i, row := range state.Accounts {
				if row.ID == id {
					index = i
					a = row
					previous = state.AdminCompat[id]
					break
				}
			}
			if !creating && index < 0 {
				return errNotFound
			}
			if expected > 0 && a.Version != expected {
				return errVersionConflict
			}
			record := compatCopyRecord(previous)
			prior := a
			if err := change(&a, &record); err != nil {
				return err
			}
			if err := compatSyncCredentialMetadata(previous, &record, a); err != nil {
				return err
			}
			a.ID = id
			if !creating {
				a.Version = prior.Version + 1
			}
			a.UpdatedAt = time.Now().Unix()
			a.HasRefreshToken = a.RefreshToken != ""
			lease := state.ManagedLeases[id]
			leaseChanged, err := compatLeaseChange(previous, record, &lease, &a)
			if err != nil {
				return err
			}
			if err := compatValidateAccount(a, record); err != nil {
				return err
			}
			if len(a.Groups) > 0 {
				groups, err := p.Groups.List()
				if err != nil {
					return err
				}
				known := map[string]bool{}
				for _, g := range groups {
					known[g.ID] = true
				}
				for _, id := range a.Groups {
					if !known[id] {
						return errInvalid
					}
				}
			}
			if len(a.Proxies) > 0 {
				proxies, err := p.Proxies.managedMetadata()
				if err != nil {
					return err
				}
				known := map[string]bool{}
				for _, px := range proxies {
					known[px.ID] = true
				}
				for _, id := range a.Proxies {
					if !known[id] {
						return errInvalid
					}
				}
			}
			for _, other := range state.Accounts {
				if other.ID != id && (other.Name == a.Name || other.AccessToken == a.AccessToken || compatFileName(other, state.AdminCompat[other.ID]) == compatFileName(a, record)) {
					return errConflict
				}
			}
			if state.AdminCompat == nil {
				state.AdminCompat = map[string]adminCompatRecord{}
			}
			state.AdminCompat[id] = record
			if leaseChanged {
				if state.ManagedLeases == nil {
					state.ManagedLeases = map[string]managedAccountLease{}
				}
				state.ManagedLeases[id] = lease
			}
			if creating {
				state.Accounts = append(state.Accounts, a)
				if state.Active == "" {
					state.Active = a.Name
				}
			} else {
				state.Accounts[index] = a
				if state.Active == prior.Name {
					state.Active = a.Name
				}
			}
			result = adminCompatAccount{a, record}
			return ctx.Err()
		})
		if err != nil {
			return adminCompatAccount{}, err
		}
		p.invalidateAccounts()
		// Re-read the committed credential/ownership generations assigned by modify.
		stored, err := p.compatGetAccount(ctx, id)
		if err == nil {
			result = stored
		}
		return result, err
	}
	db := p.Store.DB
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		return adminCompatAccount{}, err
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended('codex-agent:import:' || current_schema(),0))"); err != nil {
		return adminCompatAccount{}, err
	}
	leaseName, _ := managementStateKey("account-leases", id)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended(current_schema() || $1,91823017))", leaseName); err != nil {
		return adminCompatAccount{}, err
	}
	var leaseEntry managementStateEntry
	if err = db.readDocument(ctx, tx, leaseName, &leaseEntry); err != nil {
		return adminCompatAccount{}, err
	}
	var lease managedAccountLease
	if leaseEntry.Version > 0 && json.Unmarshal(leaseEntry.Payload, &lease) != nil {
		return adminCompatAccount{}, errInvalid
	}
	a := initial()
	var previous adminCompatRecord
	if !creating {
		if _, err = tx.Exec(ctx, "SELECT 1 FROM account_credentials WHERE account_id=$1 FOR UPDATE", id); err != nil {
			return adminCompatAccount{}, err
		}
		// The account ID is immutable here. Preserve writer serialization
		// without conflicting with shared admission's key-share lock.
		if _, err = tx.Exec(ctx, "SELECT 1 FROM accounts WHERE id=$1 FOR NO KEY UPDATE", id); err != nil {
			return adminCompatAccount{}, err
		}
		a, err = p.Store.pgGetFrom(ctx, tx, id, true)
		if err != nil {
			return adminCompatAccount{}, err
		}
		if a.ID != id {
			return adminCompatAccount{}, errNotFound
		}
		records, err := compatRecordsPG(ctx, db, tx, []string{id})
		if err != nil {
			return adminCompatAccount{}, err
		}
		previous = records[id]
	}
	if expected > 0 && a.Version != expected {
		return adminCompatAccount{}, errVersionConflict
	}
	prior := a
	record := compatCopyRecord(previous)
	if err = change(&a, &record); err != nil {
		return adminCompatAccount{}, err
	}
	if err = compatSyncCredentialMetadata(previous, &record, a); err != nil {
		return adminCompatAccount{}, err
	}
	a.ID = id
	tokenChanged := !creating && compatCredentialsChanged(prior, a)
	if !creating {
		a.Version = prior.Version + 1
		if tokenChanged {
			a.Version++
		}
	}
	a.UpdatedAt = time.Now().Unix()
	a.HasRefreshToken = a.RefreshToken != ""
	leaseChanged, err := compatLeaseChange(previous, record, &lease, &a)
	if err != nil {
		return adminCompatAccount{}, err
	}
	if err = compatValidateAccount(a, record); err != nil {
		return adminCompatAccount{}, err
	}
	var conflict bool
	if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM accounts WHERE id<>$1 AND display_name=$2) OR EXISTS(SELECT 1 FROM account_credentials WHERE account_id<>$1 AND token_fingerprint=$3)", id, a.Name, tokenFingerprint(a.AccessToken)).Scan(&conflict); err != nil {
		return adminCompatAccount{}, err
	}
	if conflict {
		return adminCompatAccount{}, errConflict
	}
	filename := compatFileName(a, record)
	linkName, _ := managementStateKey("admin-compat-files", filename)
	var link managementStateEntry
	if err = db.readDocument(ctx, tx, linkName, &link); err != nil {
		return adminCompatAccount{}, err
	}
	if link.Version > 0 {
		var other string
		if json.Unmarshal(link.Payload, &other) != nil {
			return adminCompatAccount{}, errInvalid
		}
		if other != id {
			otherAccount, lookupErr := p.Store.pgGetFrom(ctx, tx, other, false)
			if lookupErr != nil && !errors.Is(lookupErr, errNotFound) {
				return adminCompatAccount{}, lookupErr
			}
			if lookupErr == nil && otherAccount.ID == other {
				otherRecords, lookupErr := compatRecordsPG(ctx, db, tx, []string{other})
				if lookupErr != nil {
					return adminCompatAccount{}, lookupErr
				}
				if compatFileName(otherAccount, otherRecords[other]) == filename {
					return adminCompatAccount{}, errConflict
				}
			}
		}
	}
	if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM accounts WHERE id<>$1 AND (display_name=$2 OR display_name=$3))", id, filename, strings.TrimSuffix(filename, ".json")).Scan(&conflict); err != nil {
		return adminCompatAccount{}, err
	}
	if conflict {
		return adminCompatAccount{}, errConflict
	}
	if creating {
		if err = p.Store.pgInsertAccount(ctx, tx, a); err != nil {
			return adminCompatAccount{}, err
		}
		if _, err = tx.Exec(ctx, "INSERT INTO settings(key,value,version) SELECT 'active_account_id',to_jsonb($1::text),1 WHERE NOT EXISTS(SELECT 1 FROM accounts WHERE id<>$1) ON CONFLICT(key) DO NOTHING", id); err != nil {
			return adminCompatAccount{}, err
		}
	} else {
		version := prior.Version
		if tokenChanged {
			if err = p.Store.pgWriteTokens(ctx, tx, prior, a); err != nil {
				return adminCompatAccount{}, err
			}
			version++
		}
		if err = p.Store.pgSaveMetadata(ctx, tx, a, version); err != nil {
			return adminCompatAccount{}, err
		}
	}
	write := func(name string, value any) error {
		var e managementStateEntry
		if err := db.readDocument(ctx, tx, name, &e); err != nil {
			return err
		}
		e.Version++
		e.UpdatedAt = time.Now().Unix()
		e.Payload, _ = json.Marshal(value)
		return db.writeDocument(ctx, tx, name, e)
	}
	// Management edits (status toggles, priority, notes) do not touch the
	// compatibility record or the file-name link. Skip rewrites whose payload
	// is byte-identical so those saves stop bumping document versions.
	nextRaw, beforeRaw := marshalCompatRecord(record), marshalCompatRecord(previous)
	linkCurrent, idRaw := false, sub2Raw(id)
	if !creating && link.Version > 0 {
		linkCurrent = bytes.Equal(link.Payload, idRaw)
	}
	name, _ := managementStateKey(compatRecordNamespace, id)
	if creating || !bytes.Equal(nextRaw, beforeRaw) {
		if err = write(name, record); err != nil {
			return adminCompatAccount{}, err
		}
	}
	if creating || !linkCurrent {
		if err = write(linkName, id); err != nil {
			return adminCompatAccount{}, err
		}
	}
	if oldFile := compatFileName(prior, previous); !creating && oldFile != filename {
		oldName, _ := managementStateKey("admin-compat-files", oldFile)
		if _, err = tx.Exec(ctx, "DELETE FROM settings WHERE key=$1", oldName); err != nil {
			return adminCompatAccount{}, err
		}
	}
	if leaseChanged {
		if err = write(leaseName, lease); err != nil {
			return adminCompatAccount{}, err
		}
	}
	if err = db.audit(ctx, tx, "account.compat.update", id); err != nil {
		return adminCompatAccount{}, err
	}
	a, err = p.Store.pgGetFrom(ctx, tx, id, true)
	if err != nil {
		return adminCompatAccount{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return adminCompatAccount{}, err
	}
	db.invalidate()
	p.invalidateAccounts()
	return adminCompatAccount{a, record}, nil
}

func (p *Proxy) compatDeleteAccount(ctx context.Context, id string, expected int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.Store.DB == nil {
		if _, provided := ctx.Value(accountDrainDeleteContextKey{}).(accountDrainDeleteFence); provided {
			return errAccountDrainUnsupported
		}
		err := p.Store.modify(func(state *accountState) error {
			for i, a := range state.Accounts {
				if a.ID == id {
					if expected > 0 && a.Version != expected {
						return errVersionConflict
					}
					state.Accounts = append(state.Accounts[:i], state.Accounts[i+1:]...)
					delete(state.AdminCompat, id)
					delete(state.ManagedLeases, id)
					if state.Active == a.Name {
						state.Active = ""
					}
					return ctx.Err()
				}
			}
			return errNotFound
		})
		if err != nil {
			return err
		}
	} else {
		db := p.Store.DB
		tx, err := db.Pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(context.Background())
		if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended('codex-agent:import:' || current_schema(),0))"); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, "SELECT 1 FROM account_credentials WHERE account_id=$1 FOR UPDATE", id); err != nil {
			return err
		}
		var version int64
		if err = tx.QueryRow(ctx, "SELECT version FROM accounts WHERE id=$1 FOR UPDATE", id).Scan(&version); errors.Is(err, pgx.ErrNoRows) {
			return errNotFound
		} else if err != nil {
			return err
		}
		if expected > 0 && version != expected {
			return errVersionConflict
		}
		a, err := p.Store.pgGetFrom(ctx, tx, id, false)
		if err != nil {
			return err
		}
		if err = p.checkAccountDrainDelete(ctx, tx, a); err != nil {
			if errors.Is(err, errAccountDraining) {
				// A rejected fenced delete still observed current lease state.
				// Commit that observation only (no account mutation has run), so
				// a nonzero lease resets an older zero safety window durably.
				if commitErr := tx.Commit(ctx); commitErr != nil {
					return commitErr
				}
			}
			return err
		}
		records, err := compatRecordsPG(ctx, db, tx, []string{id})
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, "DELETE FROM accounts WHERE id=$1", id); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, "DELETE FROM settings WHERE key='active_account_id' AND value=to_jsonb($1::text)", id); err != nil {
			return err
		}
		keys := []string{}
		for _, pair := range [][2]string{{compatRecordNamespace, id}, {"admin-compat-files", compatFileName(a, records[id])}, {"account-leases", id}} {
			key, _ := managementStateKey(pair[0], pair[1])
			keys = append(keys, key)
		}
		if _, err = tx.Exec(ctx, "DELETE FROM settings WHERE key=ANY($1::text[])", keys); err != nil {
			return err
		}
		if err = db.audit(ctx, tx, "account.compat.delete", id); err != nil {
			return err
		}
		if err = tx.Commit(ctx); err != nil {
			return err
		}
		db.invalidate()
	}
	p.cleanupManagedDeletedAccountSchedules(ctx, id)
	p.invalidateAccounts()
	return nil
}
