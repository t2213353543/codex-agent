package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
)

const maxAccountImportRows = 10000

type ImportOptions struct {
	Replace bool `json:"replace"`
	DryRun  bool `json:"dry_run"`
}

type ImportResult struct {
	Index    int      `json:"index"`
	Name     string   `json:"name"`
	ID       string   `json:"id,omitempty"`
	Status   string   `json:"status"`
	Error    string   `json:"error,omitempty"`
	Code     string   `json:"code,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

type ImportReport struct {
	Added     int            `json:"added"`
	Duplicate int            `json:"duplicate"`
	Replaced  int            `json:"replaced"`
	Failed    int            `json:"failed"`
	Results   []ImportResult `json:"results"`
}

func accountID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		panic(err)
	}
	return "a_" + hex.EncodeToString(value[:])
}

// A deterministic ID keeps legacy file reads stable until the one-time import.
func legacyAccountID(name string) string {
	sum := sha256.Sum256([]byte("legacy-account:" + name))
	return "a_" + hex.EncodeToString(sum[:16])
}

func tokenFingerprint(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// ParseImportInput first accepts a complete JSON document. JSON-looking input
// is never treated as a token after a decoding failure. Errors contain positions
// only, not source fragments which may contain credentials.
func ParseImportInput(raw []byte) ([]ImportItem, error) {
	if len(raw) > 32<<20 {
		return nil, fmt.Errorf("%w: import exceeds 32 MiB", errInvalid)
	}
	raw = bytes.TrimSpace(bytes.TrimPrefix(raw, []byte{0xef, 0xbb, 0xbf}))
	if len(raw) == 0 {
		return nil, fmt.Errorf("%w: empty import", errInvalid)
	}
	if json.Valid(raw) {
		return importJSONDocument(raw)
	}
	lines := bytes.Split(raw, []byte("\n"))
	items := make([]ImportItem, 0, len(lines))
	for index, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var rows []ImportItem
		var err error
		if bytes.ContainsAny(line[:1], "{[\"") || bytes.ContainsAny(line, "{}[]\"") {
			if !json.Valid(line) {
				return nil, fmt.Errorf("%w: malformed JSON at line %d", errInvalid, index+1)
			}
			rows, err = importJSONDocument(line)
		} else {
			token := string(line)
			name := ""
			if alias, value, found := strings.Cut(token, "----"); found {
				name, token = strings.TrimSpace(alias), strings.TrimSpace(value)
				if validAccountName(name) != nil {
					return nil, fmt.Errorf("%w: invalid account name at line %d", errInvalid, index+1)
				}
			}
			if token == "" || token == "null" || token == "true" || token == "false" || token == "undefined" || strings.IndexFunc(token, unicode.IsSpace) >= 0 || strings.ContainsAny(token, "|,:") || len(token) > 128<<10 || json.Valid([]byte(token)) {
				return nil, fmt.Errorf("%w: invalid token at line %d", errInvalid, index+1)
			}
			encoded, _ := json.Marshal(map[string]string{"name": name, "access_token": token})
			rows, err = importJSONDocument(encoded)
		}
		if err != nil {
			items = append(items, invalidImportItem(line, err))
		} else {
			items = append(items, rows...)
		}
		if len(items) > maxAccountImportRows {
			return nil, fmt.Errorf("%w: import exceeds %d rows", errInvalid, maxAccountImportRows)
		}
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("%w: empty import", errInvalid)
	}
	return items, nil
}

func importJSONDocument(raw []byte) ([]ImportItem, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, fmt.Errorf("%w: empty JSON document", errInvalid)
	}
	if raw[0] == '[' {
		var rows []json.RawMessage
		if json.Unmarshal(raw, &rows) != nil || len(rows) == 0 || len(rows) > maxAccountImportRows {
			return nil, fmt.Errorf("%w: expected 1 to %d accounts", errInvalid, maxAccountImportRows)
		}
		items := make([]ImportItem, 0, len(rows))
		for _, row := range rows {
			item, err := importJSONDocument(row)
			if err != nil || len(item) != 1 {
				if err == nil {
					err = errors.New("nested account lists are not supported")
				}
				items = append(items, invalidImportItem(row, err))
				continue
			}
			items = append(items, item[0])
		}
		return items, nil
	}
	object, err := accountObject(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: expected account JSON object or array", errInvalid)
	}
	if rows, ok := object["accounts"]; ok {
		if err := validateImportEnvelope(object); err != nil {
			return nil, err
		}
		items, err := importJSONDocument(rows)
		if err == nil && len(bytes.TrimSpace(object["proxies"])) > 0 && string(bytes.TrimSpace(object["proxies"])) != "[]" {
			for i := range items {
				items[i].warnings = append(items[i].warnings, "bundle_proxies_not_imported")
			}
		}
		return items, err
	}
	name, err := accountString(object, "name")
	if err != nil {
		return nil, fmt.Errorf("%w: name must be a string", errInvalid)
	}
	account, err := parseAccount("import", raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errInvalid, err)
	}
	if name == "" {
		name = "account-" + hex.EncodeToString(tokenFingerprint(account.AccessToken)[:8])
	}
	if validAccountName(name) != nil {
		return nil, fmt.Errorf("%w: invalid account name", errInvalid)
	}
	return []ImportItem{{Name: name, Credentials: append(json.RawMessage(nil), raw...)}}, nil
}

type accountImportError struct {
	code, message string
}

func (e *accountImportError) Error() string { return e.message }

func invalidImportItem(raw []byte, err error) ImportItem {
	name := ""
	if object, objectErr := accountObject(raw); objectErr == nil {
		if candidate, nameErr := accountString(object, "name"); nameErr == nil && validAccountName(candidate) == nil {
			name = candidate
		}
	}
	return ImportItem{Name: name, invalidError: err}
}

func validateImportEnvelope(object map[string]json.RawMessage) error {
	kind, err := accountString(object, "type")
	if err != nil || (kind != "" && kind != "sub2api-data" && kind != "sub2api-bundle") {
		return fmt.Errorf("%w: unsupported import data type", errInvalid)
	}
	if value, ok := object["version"]; ok {
		var version int
		if json.Unmarshal(value, &version) != nil || string(value) == "null" || (version != 0 && version != 1) {
			return fmt.Errorf("%w: unsupported import data version", errInvalid)
		}
	}
	accounts := bytes.TrimSpace(object["accounts"])
	if len(accounts) == 0 || accounts[0] != '[' {
		return fmt.Errorf("%w: accounts must be an array", errInvalid)
	}
	if value, ok := object["proxies"]; ok {
		value = bytes.TrimSpace(value)
		if len(value) == 0 || value[0] != '[' {
			return fmt.Errorf("%w: proxies must be an array", errInvalid)
		}
	}
	return nil
}

func validateImportedAccountKind(object map[string]json.RawMessage) error {
	platform, err := accountString(object, "platform")
	if err != nil || (platform != "" && platform != "openai") {
		return &accountImportError{"unsupported_platform", "only OpenAI accounts are supported"}
	}
	kind, err := accountString(object, "type")
	if err != nil || (kind != "" && kind != "oauth") {
		return &accountImportError{"unsupported_type", "only OAuth access-token accounts are supported"}
	}
	return nil
}

// API batches may wrap a complete auth.json in their credentials field.
// Support that historical shape as well as direct Codex/Sub2API credentials,
// with an explicit depth limit rather than recursively following arbitrary data.
func accountCredentialObject(object map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	var inheritedExpiry json.RawMessage
	for depth := 0; ; depth++ {
		if err := validateImportedAccountKind(object); err != nil {
			return nil, err
		}
		if object["platform"] == nil && object["expires_at"] != nil {
			inheritedExpiry = object["expires_at"]
		}
		found := false
		for _, key := range []string{"tokens", "credentials"} {
			if nested, ok := object[key]; ok {
				if depth == 8 {
					return nil, errors.New("credential wrappers exceed the supported nesting depth")
				}
				var err error
				object, err = accountObject(nested)
				if err != nil {
					return nil, fmt.Errorf("%s: %w", key, err)
				}
				found = true
				break
			}
		}
		if !found {
			if object["expires_at"] == nil && inheritedExpiry != nil {
				object["expires_at"] = inheritedExpiry
			}
			return object, nil
		}
	}
}

// Import only fields with the same meaning in this gateway. Foreign IDs,
// billing multipliers, fingerprint settings, and model maps are never copied
// into unrelated local fields.
func applyImportedAccountMetadata(account *Account, object map[string]json.RawMessage) error {
	account.importedFields = make(map[string]bool)
	for _, field := range []struct {
		key      string
		target   *int
		min, max int
	}{
		{"concurrency", &account.Concurrency, 1, 1000},
		{"priority", &account.Priority, -2147483648, 2147483647},
		{"weight", &account.Weight, 1, 1000},
		{"window_days", &account.WindowDays, 1, 365},
	} {
		if value, ok := object[field.key]; ok {
			if string(value) == "null" || json.Unmarshal(value, field.target) != nil || *field.target < field.min || *field.target > field.max {
				return &accountImportError{"invalid_metadata", fmt.Sprintf("%s must be an integer from %d to %d", field.key, field.min, field.max)}
			}
			account.importedFields[field.key] = true
		}
	}
	if value, ok := object["quota"]; ok {
		if string(value) == "null" || json.Unmarshal(value, &account.Quota) != nil || account.Quota < 0 || account.Quota > 9007199254740991 {
			return &accountImportError{"invalid_metadata", "quota must be a non-negative safe integer"}
		}
		account.importedFields["quota"] = true
	}
	if value, ok := object["enabled"]; ok {
		if string(value) == "null" || json.Unmarshal(value, &account.Enabled) != nil {
			return &accountImportError{"invalid_metadata", "enabled must be a boolean"}
		}
		account.importedFields["enabled"] = true
	}
	if value, ok := object["notes"]; ok && string(value) != "null" {
		if json.Unmarshal(value, &account.Notes) != nil || len(account.Notes) > 65536 {
			return &accountImportError{"invalid_metadata", "notes must be a string of at most 65536 bytes"}
		}
	}
	if value, ok := object["tags"]; ok {
		if string(value) == "null" || json.Unmarshal(value, &account.Tags) != nil || len(account.Tags) > 100 {
			return &accountImportError{"invalid_metadata", "tags must be an array of at most 100 strings"}
		}
		for _, tag := range account.Tags {
			if len(tag) > 256 {
				return &accountImportError{"invalid_metadata", "tags must contain strings of at most 256 bytes"}
			}
		}
		account.Tags = uniqueStrings(account.Tags)
	}
	return nil
}

func validateImportDefaults(defaults AccountDefaults) error {
	if defaults.Concurrency < 0 || defaults.Concurrency > 1000 || defaults.Weight < 0 || defaults.Weight > 1000 || defaults.WindowDays < 0 || defaults.WindowDays > 365 || defaults.Priority < -2147483648 || defaults.Priority > 2147483647 || defaults.Quota < 0 || defaults.Quota > 9007199254740991 {
		return fmt.Errorf("%w: import defaults are out of range", errInvalid)
	}
	return nil
}

func importedAccountWarnings(raw []byte, defaults AccountDefaults) ([]string, error) {
	object, err := accountObject(raw)
	if err != nil {
		return nil, err
	}
	warnings := []string{}
	for _, key := range []string{"proxy_key", "proxy_id"} {
		value := bytes.TrimSpace(object[key])
		if len(value) > 0 && string(value) != "null" && string(value) != `""` && string(value) != "0" {
			if len(defaults.Proxies) == 0 {
				return warnings, &accountImportError{"proxy_mapping_required", "choose a local proxy to replace the exported proxy reference"}
			}
			warnings = append(warnings, "external_proxy_replaced")
			break
		}
	}
	for _, key := range []string{"group_ids", "groups", "proxies"} {
		value := bytes.TrimSpace(object[key])
		if len(value) > 0 && string(value) != "null" && string(value) != "[]" {
			warnings = append(warnings, "external_bindings_not_imported")
			break
		}
	}
	for _, key := range []string{"rate_multiplier", "auto_pause_on_expired", "extra"} {
		if _, ok := object[key]; ok {
			warnings = append(warnings, "sub2api_settings_not_imported")
			break
		}
	}
	if object["platform"] != nil && object["expires_at"] != nil && string(object["expires_at"]) != "null" {
		warnings = append(warnings, "account_expiry_not_imported")
	}
	credentials, _ := accountCredentialObject(object)
	if _, ok := credentials["model_mapping"]; ok {
		warnings = append(warnings, "model_mapping_not_imported")
	}
	return warnings, nil
}

func prepareImport(items []ImportItem, defaults AccountDefaults) ([]Account, ImportReport, error) {
	return prepareImportContext(context.Background(), items, defaults)
}

func prepareImportContext(ctx context.Context, items []ImportItem, defaults AccountDefaults) ([]Account, ImportReport, error) {
	report := ImportReport{Results: make([]ImportResult, len(items))}
	if len(items) == 0 || len(items) > maxAccountImportRows {
		return nil, report, fmt.Errorf("%w: expected 1 to %d accounts", errInvalid, maxAccountImportRows)
	}
	if err := validateImportDefaults(defaults); err != nil {
		return nil, report, err
	}
	parsed := make([]Account, len(items))
	for i, item := range items {
		if err := ctx.Err(); err != nil {
			return nil, report, err
		}
		report.Results[i] = ImportResult{Index: i + 1, Name: item.Name, Warnings: append([]string(nil), item.warnings...)}
		account, err := parseAccount(item.Name, item.Credentials)
		if item.invalidError != nil {
			err = item.invalidError
		}
		if err == nil {
			warnings, warningErr := importedAccountWarnings(item.Credentials, defaults)
			report.Results[i].Warnings = append(report.Results[i].Warnings, warnings...)
			err = warningErr
		}
		if err != nil {
			report.Failed++
			report.Results[i].Status = "failed"
			report.Results[i].Code = "invalid_credentials"
			var importErr *accountImportError
			if errors.As(err, &importErr) {
				report.Results[i].Code = importErr.code
			}
			report.Results[i].Error = err.Error()
			continue
		}
		parsed[i] = applyAccountDefaults(account, defaults)
		normalizeAccount(&parsed[i])
		parsed[i].HasRefreshToken = parsed[i].RefreshToken != ""
	}
	return parsed, report, nil
}

// ImportDetailed is the management import entry point. Explicit replacement
// preserves management metadata, and a same-name different-token import never
// silently overwrites an account. The report never includes credential data.
// A newly rotated refresh token must survive an otherwise duplicate import.
// Empty fields never clear credentials and a conflicting identity is rejected.
func importedRefreshRotation(current, incoming Account) (Account, bool, error) {
	if incoming.RefreshToken == "" || incoming.RefreshToken == current.RefreshToken {
		return current, false, nil
	}
	if incoming.AccountID != "" && current.AccountID != "" && incoming.AccountID != current.AccountID || current.Identity != nil && incoming.Identity != nil && current.Identity.UserID != "" && incoming.Identity.UserID != "" && current.Identity.UserID != incoming.Identity.UserID {
		return current, false, errConflict
	}
	updated := current
	if incoming.Identity != nil {
		updated.Identity = incoming.Identity
	}
	updated.RefreshToken = incoming.RefreshToken
	if incoming.ClientID != "" {
		updated.ClientID = incoming.ClientID
	}
	if incoming.AccountID != "" {
		updated.AccountID = incoming.AccountID
	}
	if incoming.ExpiresAt > 0 {
		updated.ExpiresAt = incoming.ExpiresAt
	}
	return mergeImport(current, updated), true, nil
}

func (s Store) ImportDetailed(items []ImportItem, defaults AccountDefaults, options ImportOptions) (ImportReport, error) {
	return s.ImportDetailedContext(context.Background(), items, defaults, options)
}

func (s Store) ImportDetailedContext(ctx context.Context, items []ImportItem, defaults AccountDefaults, options ImportOptions) (ImportReport, error) {
	if err := ctx.Err(); err != nil {
		return ImportReport{}, err
	}
	parsed, report, err := prepareImportContext(ctx, items, defaults)
	if err != nil {
		return report, err
	}
	if s.DB != nil {
		return s.pgImportDetailedContext(ctx, parsed, report, options)
	}
	dryRun := errors.New("dry-run rollback")
	err = s.modify(func(state *accountState) error {
		for i, incoming := range parsed {
			if err := ctx.Err(); err != nil {
				return err
			}
			if report.Results[i].Status == "failed" {
				continue
			}
			currentIndex, duplicateIndex := -1, -1
			for j, current := range state.Accounts {
				if current.Name == incoming.Name {
					currentIndex = j
				}
				if current.AccessToken == incoming.AccessToken {
					duplicateIndex = j
				}
			}
			result := &report.Results[i]
			if duplicateIndex >= 0 {
				updated, rotated, err := importedRefreshRotation(state.Accounts[duplicateIndex], incoming)
				if err != nil {
					result.Status, result.Code, result.Error = "failed", "identity_conflict", "duplicate access token has conflicting account identity"
					report.Failed++
					continue
				}
				if rotated {
					state.Accounts[duplicateIndex] = updated
					result.Status, result.ID = "replaced", updated.ID
					report.Replaced++
				} else {
					result.Status, result.ID = "duplicate", updated.ID
					report.Duplicate++
				}
			} else if currentIndex >= 0 {
				if !options.Replace {
					result.Status, result.Code, result.Error = "failed", "name_conflict", "an account with this name already exists; replacement must be explicit"
					report.Failed++
					continue
				}
				state.Accounts[currentIndex] = mergeImport(state.Accounts[currentIndex], incoming)
				result.Status, result.ID = "replaced", state.Accounts[currentIndex].ID
				report.Replaced++
			} else {
				if len(state.Accounts) == 0 {
					state.Active = incoming.Name
				}
				state.Accounts = append(state.Accounts, incoming)
				result.Status, result.ID = "added", incoming.ID
				report.Added++
			}
		}
		if options.DryRun {
			return dryRun
		}
		return ctx.Err()
	})
	if errors.Is(err, dryRun) {
		err = nil
	}
	return report, err
}
