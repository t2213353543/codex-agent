package server

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Accounts are not assigned to any group unless explicitly configured.
// Groups are created and deleted freely; deleting a group detaches it
// from every account and key.

// Account holds upstream OpenAI credentials plus gateway management fields.
// QuotaUsed is deliberately excluded from persistence: it is recomputed live
// from the request metrics so the account store only carries management data.
type Account struct {
	ID                string                  `json:"id"`
	Version           int64                   `json:"version"`
	UpdatedAt         int64                   `json:"updated_at"`
	Notes             string                  `json:"notes,omitempty"`
	Tags              []string                `json:"tags,omitempty"`
	LastCheckedAt     int64                   `json:"last_checked_at,omitempty"`
	CredentialStatus  string                  `json:"credential_status"`
	CredentialVersion int64                   `json:"-"`
	OwnerRevision     string                  `json:"-"`
	HasRefreshToken   bool                    `json:"has_refresh_token"`
	Name              string                  `json:"name"`
	AccessToken       string                  `json:"access_token"`
	RefreshToken      string                  `json:"refresh_token,omitempty"`
	AccountID         string                  `json:"account_id,omitempty"`
	ClientID          string                  `json:"client_id,omitempty"`
	Identity          *managedAccountIdentity `json:"identity,omitempty"`
	ExpiresAt         int64                   `json:"expires_at,omitempty"`
	Enabled           bool                    `json:"enabled,omitempty"`
	Groups            []string                `json:"groups,omitempty"`
	Concurrency       int                     `json:"concurrency,omitempty"`
	Priority          int                     `json:"priority,omitempty"`
	Weight            int                     `json:"weight,omitempty"`
	Proxies           []string                `json:"proxies,omitempty"`
	Quota             int64                   `json:"quota,omitempty"`
	WindowDays        int                     `json:"window_days,omitempty"`
	WindowStart       int64                   `json:"window_start,omitempty"`
	LastError         string                  `json:"last_error,omitempty"`
	CreatedAt         int64                   `json:"created_at,omitempty"`

	QuotaUsed int64 `json:"-"`

	// Presence is kept only while importing so zero/false source settings survive
	// default application. It is never part of the stored account or API output.
	importedFields map[string]bool
}

// AccountDefaults fills settings missing from newly imported accounts.
// OverrideImported explicitly replaces compatible source settings. Zero values
// for concurrency, weight, and window days fall back to the gateway defaults.
type AccountDefaults struct {
	Groups           []string `json:"groups"`
	Concurrency      int      `json:"concurrency"`
	Priority         int      `json:"priority"`
	Weight           int      `json:"weight"`
	Proxies          []string `json:"proxies"`
	Quota            int64    `json:"quota"`
	WindowDays       int      `json:"window_days"`
	OverrideImported bool     `json:"override_imported,omitempty"`
}

// ImportItem is one account in a batch import.
type ImportItem struct {
	Name         string          `json:"name"`
	Credentials  json.RawMessage `json:"credentials"`
	invalidError error
	warnings     []string
}

type Store struct {
	DB   *Database
	Path string
}

type accountState struct {
	Active             string                         `json:"active"`
	Accounts           []Account                      `json:"accounts"`
	ManagedLeases      map[string]managedAccountLease `json:"managed_account_leases,omitempty"`
	ManagedCRSLinks    map[string]managedCRSLink      `json:"managed_crs_links,omitempty"`
	AdminCompat        map[string]adminCompatRecord   `json:"admin_compat,omitempty"`
	OwnerRevisions     map[string]string              `json:"owner_revisions,omitempty"`
	CredentialVersions map[string]int64               `json:"credential_versions,omitempty"`
}

var ErrTokenConflict = errors.New("account changed while tokens were being refreshed")

const (
	accountLockWait  = 2 * time.Second
	accountLockStale = 2 * time.Minute
)

func (s Store) List() (string, []Account, error) {
	if s.DB != nil {
		return s.pgList(true)
	}
	state, err := s.readState()
	return state.Active, state.Accounts, err
}

// Import parses credentials and adds or replaces an account. Replacement only
// swaps credential fields; management fields are preserved.
func (s Store) Import(name string, raw []byte) error {
	return s.ImportWith(name, raw, AccountDefaults{})
}

func (s Store) ImportWith(name string, raw []byte, defaults AccountDefaults) error {
	if s.DB != nil {
		report, err := s.ImportDetailed([]ImportItem{{Name: name, Credentials: raw}}, defaults, ImportOptions{})
		if err != nil {
			return err
		}
		if report.Failed > 0 {
			return fmt.Errorf("%w: import conflict", errConflict)
		}
		return nil
	}
	account, err := parseAccount(name, raw)
	if err != nil {
		return err
	}
	account = applyAccountDefaults(account, defaults)
	return s.modify(func(state *accountState) error {
		for i := range state.Accounts {
			if state.Accounts[i].Name == name {
				state.Accounts[i] = mergeImport(state.Accounts[i], account)
				return nil
			}
		}
		if len(state.Accounts) == 0 {
			state.Active = name
		}
		state.Accounts = append(state.Accounts, account)
		return nil
	})
}

// ImportBatch validates and stores many accounts in one atomic write. It
// returns the number of accounts imported.
func (s Store) ImportBatch(items []ImportItem, defaults AccountDefaults) (int, error) {
	report, err := s.ImportDetailed(items, defaults, ImportOptions{})
	if err != nil {
		return 0, err
	}
	if report.Failed > 0 {
		return report.Added + report.Replaced, fmt.Errorf("%w: %d import rows failed", errConflict, report.Failed)
	}
	return report.Added + report.Replaced, nil
}

func applyAccountDefaults(account Account, d AccountDefaults) Account {
	if !account.importedFields["enabled"] {
		account.Enabled = true
	}
	account.CreatedAt = time.Now().Unix()
	account.UpdatedAt = account.CreatedAt
	account.ID = accountID()
	account.Version = 1
	account.CredentialVersion = 1
	account.CredentialStatus = "unverified"
	if len(d.Groups) > 0 {
		account.Groups = uniqueStrings(d.Groups)
	}
	if d.Concurrency > 0 && (d.OverrideImported || !account.importedFields["concurrency"]) {
		account.Concurrency = d.Concurrency
	}
	if d.OverrideImported || !account.importedFields["priority"] {
		account.Priority = d.Priority
	}
	if d.Weight > 0 && (d.OverrideImported || !account.importedFields["weight"]) {
		account.Weight = d.Weight
	}
	if len(d.Proxies) > 0 {
		account.Proxies = uniqueStrings(d.Proxies)
	}
	if d.OverrideImported || !account.importedFields["quota"] {
		account.Quota = d.Quota
	}
	if d.OverrideImported || !account.importedFields["window_days"] {
		account.WindowDays = d.WindowDays
	}
	account.importedFields = nil
	if account.Quota > 0 {
		account.WindowStart = time.Now().Unix()
	}
	return account
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}

// mergeImport keeps the management state of the existing account and swaps in
// the credential state of the imported one.
func mergeImport(current, imported Account) Account {
	imported.ID = current.ID
	imported.Version = current.Version + 1
	imported.CredentialVersion = current.CredentialVersion + 1
	imported.UpdatedAt = time.Now().Unix()
	imported.Notes = current.Notes
	imported.Tags = current.Tags
	imported.LastCheckedAt = 0
	imported.CredentialStatus = "unverified"
	imported.Enabled = current.Enabled
	imported.Groups = current.Groups
	imported.Concurrency = current.Concurrency
	imported.Priority = current.Priority
	imported.Weight = current.Weight
	imported.Proxies = current.Proxies
	imported.Quota = current.Quota
	imported.WindowDays = current.WindowDays
	imported.WindowStart = current.WindowStart
	imported.LastError = ""
	imported.CreatedAt = current.CreatedAt
	return imported
}

// UpdateAccount applies a mutation to one account's management fields.
func (s Store) UpdateAccount(name string, apply func(*Account) error) error {
	if s.DB != nil {
		_, err := s.UpdateAccountVersion(name, 0, apply)
		return err
	}
	return s.modify(func(state *accountState) error {
		for i := range state.Accounts {
			if state.Accounts[i].Name == name || state.Accounts[i].ID == name {
				if err := apply(&state.Accounts[i]); err != nil {
					return err
				}
				return nil
			}
		}
		return fmt.Errorf("%w: account %q", errNotFound, name)
	})
}

// ApplyAll mutates the whole account store in one atomic write.
func (s Store) ApplyAll(change func(*accountState) error) error {
	if s.DB != nil {
		return s.pgApplyAll(change)
	}
	return s.modify(change)
}

func (s Store) Use(name string) error {
	if s.DB != nil {
		return s.pgUse(name)
	}
	return s.modify(func(state *accountState) error {
		for _, account := range state.Accounts {
			if account.Name == name || account.ID == name {
				state.Active = account.Name
				return nil
			}
		}
		return fmt.Errorf("%w: account %q", errNotFound, name)
	})
}

func (s Store) Delete(name string) error {
	if s.DB != nil {
		return s.pgDelete(name, 0)
	}
	return s.modify(func(state *accountState) error {
		for i, account := range state.Accounts {
			if account.Name == name || account.ID == name {
				state.Accounts = append(state.Accounts[:i], state.Accounts[i+1:]...)
				delete(state.AdminCompat, account.ID)
				if state.Active == account.Name {
					state.Active = ""
				}
				return nil
			}
		}
		return fmt.Errorf("%w: account %q", errNotFound, name)
	})
}

func (s Store) Active() (Account, error) {
	if s.DB != nil {
		return s.pgActive()
	}
	state, err := s.readState()
	if err != nil {
		return Account{}, err
	}
	for _, account := range state.Accounts {
		if account.Name == state.Active {
			return account, nil
		}
	}
	return Account{}, errors.New("no active account; import or select an account first")
}

// Get returns one account by name.
func (s Store) Get(name string) (Account, error) {
	if s.DB != nil {
		return s.pgGet(name, true)
	}
	state, err := s.readState()
	if err != nil {
		return Account{}, err
	}
	for _, account := range state.Accounts {
		if account.Name == name || account.ID == name {
			return account, nil
		}
	}
	return Account{}, fmt.Errorf("%w: account %q", errNotFound, name)
}

func (s Store) UpdateTokens(name, previousAccessToken string, updated Account) error {
	if s.DB != nil {
		return s.pgUpdateTokens(name, previousAccessToken, updated)
	}
	if strings.TrimSpace(updated.AccessToken) == "" {
		return errors.New("access_token must not be empty")
	}
	return s.modify(func(state *accountState) error {
		for i, current := range state.Accounts {
			if current.Name != name && current.ID != name {
				continue
			}
			if current.AccessToken != previousAccessToken {
				return ErrTokenConflict
			}
			state.Accounts[i] = mergeAccountTokens(current, updated)
			return nil
		}
		return fmt.Errorf("%w: account %q: %w", errNotFound, name, ErrTokenConflict)
	})
}

func (s Store) Refresh(name, previousAccessToken string, refresh func(Account) (Account, error)) (Account, error) {
	if s.DB != nil {
		return s.pgRefresh(name, previousAccessToken, refresh)
	}
	var result Account
	err := s.modify(func(state *accountState) error {
		for i, current := range state.Accounts {
			if current.Name != name && current.ID != name {
				continue
			}
			if current.AccessToken != previousAccessToken {
				result = current
				return nil
			}
			// Keep the store lock across the exchange: refresh tokens can rotate.
			updated, err := refresh(current)
			if err != nil {
				return err
			}
			if strings.TrimSpace(updated.AccessToken) == "" {
				return errors.New("access_token must not be empty")
			}
			result = mergeAccountTokens(current, updated)
			state.Accounts[i] = result
			return nil
		}
		return fmt.Errorf("%w: account %q: %w", errNotFound, name, ErrTokenConflict)
	})
	if err != nil {
		return Account{}, err
	}
	// The file write assigns the authoritative revision after the callback.
	// Reload it rather than returning a pre-commit ownership snapshot.
	return s.getAccountByID(result.ID)
}

// Internal ownership checks must not let a display name shadow a stable ID.
func (s Store) getAccountByID(id string) (Account, error) {
	if s.DB != nil {
		account, err := s.pgGet(id, true)
		if err != nil {
			return Account{}, err
		}
		if account.ID == id {
			return account, nil
		}
		return Account{}, errNotFound
	}
	state, err := s.readState()
	if err != nil {
		return Account{}, err
	}
	for _, account := range state.Accounts {
		if account.ID == id {
			return account, nil
		}
	}
	return Account{}, errNotFound
}

func mergeAccountTokens(current, updated Account) Account {
	updated.ID = current.ID
	updated.Version = current.Version + 1
	updated.CredentialVersion = current.CredentialVersion + 1
	updated.UpdatedAt = time.Now().Unix()
	updated.Notes = current.Notes
	updated.Tags = current.Tags
	updated.LastCheckedAt = current.LastCheckedAt
	updated.CredentialStatus = "unverified"
	updated.Name = current.Name
	updated.Enabled = current.Enabled
	updated.Groups = current.Groups
	updated.Concurrency = current.Concurrency
	updated.Priority = current.Priority
	updated.Weight = current.Weight
	updated.Proxies = current.Proxies
	updated.Quota = current.Quota
	updated.WindowDays = current.WindowDays
	updated.WindowStart = current.WindowStart
	updated.LastError = ""
	// OAuth rotation changes authentication material, not the workspace's
	// billing eligibility. Explicit import/reauthorization uses mergeImport
	// and intentionally clears this reversible scheduling state.
	if current.CredentialStatus == gatewayPaymentStatus {
		updated.CredentialStatus, updated.LastCheckedAt, updated.LastError = current.CredentialStatus, current.LastCheckedAt, current.LastError
	}
	updated.CreatedAt = current.CreatedAt
	if updated.AccountID == "" {
		updated.AccountID = current.AccountID
	}
	if updated.RefreshToken == "" {
		updated.RefreshToken = current.RefreshToken
	}
	if updated.ClientID == "" {
		updated.ClientID = current.ClientID
	}
	if updated.ExpiresAt == 0 {
		updated.ExpiresAt, _ = jwtAccountClaims(updated.AccessToken)
	}
	return updated
}

// effectiveConcurrency maps the persisted value onto the gateway default.
func (a Account) effectiveConcurrency() int {
	if a.Concurrency < 1 {
		return 1
	}
	return a.Concurrency
}

func (a Account) effectiveWeight() int {
	if a.Weight < 1 {
		return 1
	}
	return a.Weight
}

func (a Account) effectiveWindowDays() int {
	if a.WindowDays < 1 {
		return 30
	}
	return a.WindowDays
}

func validAccountName(name string) error {
	if strings.TrimSpace(name) == "" || len(name) > 100 {
		return errors.New("account name must contain 1 to 100 bytes")
	}
	for _, c := range name {
		if unicode.IsControl(c) || c == ' ' || c == ' ' {
			return errors.New("account name must not contain control characters or newlines")
		}
	}
	return nil
}

func accountObject(raw []byte) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, errors.New("account data must be a JSON object")
	}
	if object == nil {
		return nil, errors.New("account data must not be null")
	}
	return object, nil
}

func accountString(object map[string]json.RawMessage, key string) (string, error) {
	raw, ok := object[key]
	if !ok {
		return "", nil
	}
	var value string
	if string(raw) == "null" || json.Unmarshal(raw, &value) != nil {
		return "", fmt.Errorf("%s must be a string", key)
	}
	return strings.TrimSpace(value), nil
}

func parseAccount(name string, raw []byte) (Account, error) {
	if err := validAccountName(name); err != nil {
		return Account{}, err
	}
	object, err := accountObject(raw)
	if err != nil {
		return Account{}, err
	}
	root := object
	if err := validateImportedAccountKind(root); err != nil {
		return Account{}, err
	}
	object, err = accountCredentialObject(root)
	if err != nil {
		return Account{}, err
	}
	account := Account{Name: name}
	fields := []struct {
		key    string
		target *string
	}{
		{"access_token", &account.AccessToken},
		{"refresh_token", &account.RefreshToken},
		{"account_id", &account.AccountID},
		{"client_id", &account.ClientID},
	}
	for _, field := range fields {
		*field.target, err = accountString(object, field.key)
		if err != nil {
			return Account{}, err
		}
	}
	if account.AccessToken == "" {
		if len(root["agent_identity"]) > 0 {
			return Account{}, errors.New("agent_identity-only authentication requires unsupported task signing; provide OAuth tokens alongside it")
		}
		return Account{}, errors.New("access_token must not be empty")
	}
	if strings.IndexFunc(account.AccessToken, unicode.IsSpace) >= 0 || len(account.AccessToken) > 128<<10 {
		return Account{}, errors.New("access_token must be a token without whitespace, at most 128 KiB")
	}
	if account.AccountID == "" {
		account.AccountID, err = accountString(object, "chatgpt_account_id")
		if err != nil {
			return Account{}, err
		}
	}
	expiresRaw, ok := object["expires_at"]
	// Sub2API's outer expires_at is the management expiry of the account,
	// whereas credentials.expires_at describes the actual OAuth token.
	if !ok && root["platform"] == nil {
		expiresRaw = root["expires_at"]
	}
	if len(expiresRaw) > 0 {
		account.ExpiresAt, err = accountExpiry(expiresRaw)
		if err != nil {
			return Account{}, fmt.Errorf("expires_at: %w", err)
		}
	}
	jwtExpiry, jwtID := jwtAccountClaims(account.AccessToken)
	if account.ExpiresAt == 0 {
		account.ExpiresAt = jwtExpiry
	}
	if account.AccountID == "" {
		account.AccountID = jwtID
	}
	if account.AccountID == "" {
		idToken, tokenErr := accountString(object, "id_token")
		if tokenErr != nil {
			return Account{}, tokenErr
		}
		_, account.AccountID = jwtAccountClaims(idToken)
	}
	if err := applyImportedAccountMetadata(&account, root); err != nil {
		return Account{}, err
	}
	account.Identity, err = parseManagedIdentity(root, object, account)
	if err != nil {
		return Account{}, err
	}
	if account.AccountID == "" && account.Identity != nil {
		account.AccountID = account.Identity.WorkspaceID
	}
	return account, nil
}

func accountExpiry(raw json.RawMessage) (int64, error) {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, errors.New("expected a timestamp or RFC3339 string")
	}
	switch value := value.(type) {
	case float64:
		if math.IsNaN(value) || math.IsInf(value, 0) || value > 9007199254740991 || value < 0 || value != math.Trunc(value) {
			return 0, errors.New("timestamp is out of range")
		}
		return int64(value), nil
	case string:
		if number, err := strconv.ParseInt(value, 10, 64); err == nil && number >= 0 && number <= 9007199254740991 {
			return number, nil
		}
		if date, err := time.Parse(time.RFC3339, value); err == nil {
			return date.Unix(), nil
		}
	}
	return 0, errors.New("expected a timestamp or RFC3339 string")
}

func jwtAccountClaims(token string) (int64, string) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return 0, ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0, ""
	}
	object, err := accountObject(payload)
	if err != nil {
		return 0, ""
	}
	expiry, _ := accountExpiry(object["exp"])
	var auth struct {
		AccountID string `json:"chatgpt_account_id"`
	}
	_ = json.Unmarshal(object["https://api.openai.com/auth"], &auth)
	return expiry, auth.AccountID
}

func (s Store) readState() (accountState, error) {
	empty := accountState{Accounts: []Account{}}
	if s.Path == "" {
		return empty, errors.New("account store path is empty")
	}
	raw, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return empty, nil
	}
	if err != nil {
		return empty, fmt.Errorf("read account store: %w", err)
	}
	var state *accountState
	if err := json.Unmarshal(raw, &state); err != nil {
		return empty, fmt.Errorf("decode account store: %w", err)
	}
	if state == nil {
		return empty, errors.New("account store must not be null")
	}
	seen := make(map[string]bool)
	for i := range state.Accounts {
		account := &state.Accounts[i]
		if err := validAccountName(account.Name); err != nil {
			return empty, fmt.Errorf("invalid account store: %w", err)
		}
		if seen[account.Name] || strings.TrimSpace(account.AccessToken) == "" {
			return empty, errors.New("account store contains duplicate names or empty tokens")
		}
		seen[account.Name] = true
		normalizeAccount(account)
		account.OwnerRevision = state.OwnerRevisions[account.ID]
		account.CredentialVersion = state.CredentialVersions[account.ID]
		if account.CredentialVersion < 0 {
			return empty, errors.New("invalid persisted account credential version")
		}
		if account.CredentialVersion == 0 {
			// Old file stores did not persist credential generations. Establish
			// generation 1 without attributing legacy request logs to it.
			account.CredentialVersion = 1
		}
		if !validGatewayOwnerRevision(account.OwnerRevision) {
			return empty, errors.New("invalid persisted account ownership revision")
		}
	}
	if state.Active != "" && !seen[state.Active] {
		return empty, errors.New("active account is missing from the account store")
	}
	if state.Accounts == nil {
		state.Accounts = []Account{}
	}
	return *state, nil
}

// normalizeAccount upgrades legacy entries and clamps management fields.
// Accounts written before management fields existed have no created_at and
// are treated as enabled. Ungrouped accounts stay ungrouped.
func normalizeAccount(account *Account) {
	if account.ID == "" {
		account.ID = legacyAccountID(account.Name)
	}
	if account.Version == 0 {
		account.Version = 1
	}
	if account.CredentialStatus == "" {
		account.CredentialStatus = "unverified"
	}
	if account.CreatedAt == 0 {
		account.Enabled = true
	}
	account.Groups = uniqueStrings(account.Groups)
	if account.Concurrency < 1 {
		account.Concurrency = 1
	}
	if account.Weight < 1 {
		account.Weight = 1
	}
	if account.WindowDays < 1 {
		account.WindowDays = 30
	}
	if account.Quota < 0 {
		account.Quota = 0
	}
	if account.Quota > 0 && account.WindowStart == 0 {
		account.WindowStart = time.Now().Unix()
	}
}

func (s Store) modify(change func(*accountState) error) error {
	if s.Path == "" {
		return errors.New("account store path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0700); err != nil {
		return fmt.Errorf("create account store directory: %w", err)
	}
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	state, err := s.readState()
	if err != nil {
		return err
	}
	previous := make(map[string]Account, len(state.Accounts))
	for _, account := range state.Accounts {
		if account.Identity != nil {
			identity := *account.Identity
			account.Identity = &identity
		}
		previous[account.ID] = account
	}
	if err := change(&state); err != nil {
		return err
	}
	revisions := make(map[string]string, len(state.Accounts))
	credentialVersions := make(map[string]int64, len(state.Accounts))
	for i := range state.Accounts {
		normalizeAccount(&state.Accounts[i])
		account := &state.Accounts[i]
		var old *Account
		if value, ok := previous[account.ID]; ok {
			old = &value
		}
		if err := advanceGatewayOwnerRevision(old, account); err != nil {
			return err
		}
		if old == nil {
			account.CredentialVersion = 1
		} else {
			changed := old.AccessToken != account.AccessToken || old.RefreshToken != account.RefreshToken ||
				old.ClientID != account.ClientID || old.AccountID != account.AccountID || !reflect.DeepEqual(old.Identity, account.Identity)
			if changed || account.CredentialVersion > old.CredentialVersion {
				if old.CredentialVersion == math.MaxInt64 {
					return errors.New("account credential version exhausted")
				}
				account.CredentialVersion = old.CredentialVersion + 1
			} else {
				account.CredentialVersion = old.CredentialVersion
			}
		}
		credentialVersions[account.ID] = account.CredentialVersion
		if account.OwnerRevision != "" {
			revisions[account.ID] = account.OwnerRevision
		}
	}
	state.OwnerRevisions = revisions
	state.CredentialVersions = credentialVersions
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(s.Path), ".accounts-*")
	if err != nil {
		return fmt.Errorf("create temporary account store: %w", err)
	}
	temp := file.Name()
	defer os.Remove(temp)
	if err := file.Chmod(0600); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(append(raw, '\n')); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(temp, s.Path); err != nil {
		return fmt.Errorf("replace account store: %w", err)
	}
	return nil
}

func (s Store) lock() (func(), error) {
	path := s.Path + ".lock"
	deadline := time.Now().Add(accountLockWait)
	for {
		if err := os.Mkdir(path, 0700); err == nil {
			return func() { _ = os.Remove(path) }, nil
		} else if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("lock account store: %w", err)
		}
		info, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("inspect account store lock: %w", err)
		}
		if time.Since(info.ModTime()) > accountLockStale {
			return nil, fmt.Errorf("account store lock is stale: %s; verify no process is writing and remove it manually", path)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("account store is locked: %s", path)
		}
		time.Sleep(25 * time.Millisecond)
	}
}
