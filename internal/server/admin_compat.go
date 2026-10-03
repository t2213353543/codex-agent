package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

type adminCompatRecord struct {
	FileName string                     `json:"filename,omitempty"`
	CPA      map[string]json.RawMessage `json:"cpa,omitempty"`
	Sub2     map[string]json.RawMessage `json:"sub2,omitempty"`
}

type adminCompatAccount struct {
	Account Account
	Record  adminCompatRecord
}

// Stable across restarts and credential rotations, without changing native IDs.
// 48 bits stay within JavaScript's exact integer range.
func compatNumericID(id string) int64 {
	sum := sha256.Sum256([]byte("admin-compat-id:" + id))
	return int64(binary.BigEndian.Uint64(sum[:8])>>16) + 1
}

func compatAuthIndex(id string) string {
	sum := sha256.Sum256([]byte("admin-compat-auth:" + id))
	return hex.EncodeToString(sum[:8])
}

func compatFileName(a Account, record adminCompatRecord) string {
	if record.FileName != "" {
		return record.FileName
	}
	if strings.HasSuffix(a.Name, ".json") {
		return a.Name
	}
	return a.Name + ".json"
}

func compatReadJSON(w http.ResponseWriter, r *http.Request, target any, limit int64) error {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' || !json.Valid(raw) {
		return fmt.Errorf("%w: expected one JSON object", errInvalid)
	}
	// Duplicate members are rejected even when the destination permits foreign
	// optional fields. Never allow two conflicting credentials or disabled flags.
	if err := compatValidateJSON(raw); err != nil {
		return err
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return fmt.Errorf("%w: invalid JSON fields", errInvalid)
	}
	return nil
}

func compatValidateJSON(raw []byte) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var consume func(int) error
	consume = func(depth int) error {
		if depth > 64 {
			return errInvalid
		}
		token, err := d.Token()
		if err != nil {
			return errInvalid
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return errInvalid
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return fmt.Errorf("%w: duplicate JSON member", errInvalid)
				}
				seen[name] = true
				if err := consume(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for d.More() {
				if err := consume(depth + 1); err != nil {
					return err
				}
			}
		default:
			return errInvalid
		}
		_, err = d.Token()
		return err
	}
	if err := consume(0); err != nil {
		return err
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return errInvalid
	}
	return nil
}

func compatErrorStatus(w http.ResponseWriter, r *http.Request, status int, message string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if strings.HasPrefix(r.URL.Path, "/api/v1/admin/") {
		_ = json.NewEncoder(w).Encode(map[string]any{"code": status, "message": message})
	} else {
		_ = json.NewEncoder(w).Encode(map[string]any{"error": message})
	}
}

func compatError(w http.ResponseWriter, r *http.Request, err error) {
	status, message := 500, "management storage operation failed"
	var tooLarge *http.MaxBytesError
	switch {
	case errors.Is(err, errAccountDrainConflict), errors.Is(err, errAccountDraining):
		status, message = 409, "account drain requires a matching completed operation"
	case errors.Is(err, errNotFound), errors.Is(err, errManagedLogNotFound):
		status, message = 404, "not found"
	case errors.Is(err, errVersionConflict), errors.Is(err, ErrTokenConflict):
		status, message = 409, "account changed; reload and retry"
	case errors.Is(err, errConflict):
		status, message = 409, "conflicting account or resource"
	case errors.As(err, &tooLarge), errors.Is(err, errAdminBodyTooLarge):
		status, message = 413, "request body too large"
	case errors.Is(err, errInvalid):
		status, message = 400, "invalid or unsupported management request"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		status, message = 503, "management operation canceled or timed out"
	}
	compatErrorStatus(w, r, status, message)
}

func compatHeader(r *http.Request, name string) (string, bool, bool) {
	var values []string
	present := false
	for key, raw := range r.Header {
		if strings.EqualFold(key, name) {
			present = true
			values = append(values, raw...)
		}
	}
	if !present {
		return "", false, true
	}
	if len(values) != 1 || strings.TrimSpace(values[0]) == "" {
		return "", true, false
	}
	value := strings.TrimSpace(values[0])
	if strings.ContainsAny(value, "\r\n\x00") {
		return "", true, false
	}
	return value, true, true
}

func (p *Proxy) authenticateCompat(w http.ResponseWriter, r *http.Request) bool {
	value, present, valid := adminBearer(r)
	if present && !valid {
		compatErrorStatus(w, r, 401, "invalid admin key")
		return false
	}
	header := "X-Management-Key"
	if strings.HasPrefix(r.URL.Path, "/api/v1/admin/") {
		header = "X-API-Key"
	}
	alternate, hasAlternate, ok := compatHeader(r, header)
	if !ok || (present && hasAlternate && subtle.ConstantTimeCompare([]byte(value), []byte(alternate)) != 1) {
		compatErrorStatus(w, r, 401, "invalid admin key")
		return false
	}
	if !present && hasAlternate {
		value = alternate
		present = true
	}
	if !present {
		compatErrorStatus(w, r, 401, "management key required")
		return false
	}
	key, err := p.currentAdminKey(r.Context())
	if err != nil {
		compatErrorStatus(w, r, 503, "authentication storage unavailable")
		return false
	}
	if key == "" || subtle.ConstantTimeCompare([]byte(key), []byte(value)) != 1 {
		compatErrorStatus(w, r, 401, "invalid admin key")
		return false
	}
	rememberAdminAuthentication(r, adminAuthentication{BearerHash: adminKeyHash(value)})
	return true
}

func (p *Proxy) compatRoutes() http.Handler {
	mux := http.NewServeMux()
	p.registerSub2CompatRoutes(mux)
	p.registerCPACompatRoutes(mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if !p.authenticateCompat(w, r) {
			return
		}
		if _, pattern := mux.Handler(r); pattern == "" {
			// Let ServeMux compute Allow without emitting its plain-text body.
			probe := &compatRouteProbe{header: make(http.Header)}
			mux.ServeHTTP(probe, r)
			if allow := probe.header.Get("Allow"); allow != "" {
				w.Header().Set("Allow", allow)
			}
			status := probe.status
			if status == 0 {
				status = 404
			}
			compatErrorStatus(w, r, status, http.StatusText(status))
			return
		}
		mux.ServeHTTP(w, r)
	})
}

type compatRouteProbe struct {
	header http.Header
	status int
}

func (w *compatRouteProbe) Header() http.Header    { return w.header }
func (w *compatRouteProbe) WriteHeader(status int) { w.status = status }
func (w *compatRouteProbe) Write(raw []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	return len(raw), nil
}

func (p *Proxy) compatResolveAccount(ctx context.Context, ref string) (string, error) {
	if ref == "" {
		return "", errInvalid
	}
	// Native IDs are stable and indexed.  Resolve this overwhelmingly common
	// form directly instead of materializing every compatibility record (and,
	// in PostgreSQL, decrypting one settings row per account) just to find an
	// exact match.
	if candidates, err := p.Store.metadataByIDsContext(ctx, []string{ref}); err == nil {
		if _, ok := candidates[ref]; ok {
			return ref, nil
		}
	} else {
		return "", err
	}
	if p.Store.DB != nil {
		// Display names, preserved file names and numeric compatibility IDs
		// resolve through bounded queries; the full projection stays reserved
		// for the in-memory file store.
		return p.compatResolveAliasPG(ctx, ref)
	}
	accounts, err := p.compatAccountReferences(ctx)
	if err != nil {
		return "", err
	}
	// Native stable identifiers retain precedence over display names.
	for _, item := range accounts {
		if item.Account.ID == ref {
			return ref, nil
		}
	}
	id := ""
	for _, item := range accounts {
		a := item.Account
		if a.Name == ref || compatFileName(a, item.Record) == ref || strconv.FormatInt(compatNumericID(a.ID), 10) == ref {
			if id != "" && id != a.ID {
				return "", errConflict
			}
			id = a.ID
		}
	}
	if id == "" {
		return "", errNotFound
	}
	return id, nil
}

func (p *Proxy) compatRefreshAccount(ctx context.Context, id string) (Account, error) {
	a, err := p.Store.Get(id)
	if err != nil {
		return Account{}, err
	}
	if a.RefreshToken == "" {
		return Account{}, fmt.Errorf("%w: account has no refresh token", errInvalid)
	}
	updated, err := p.Store.Refresh(id, a.AccessToken, func(current Account) (Account, error) { return p.refreshAccountWithFingerprint(ctx, current) })
	if err == nil {
		p.invalidateAccounts()
	}
	return updated, err
}
