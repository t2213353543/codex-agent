package server

import (
	"bytes"
	"codex-agent/internal/publicerrors"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/redis/go-redis/v9"
)

const sessionCookie = "codex_admin_session"

var fallbackRotateMu sync.Mutex

type adminAuthenticationContextKey struct{}
type adminAuthentication struct {
	BearerHash  string
	CookieToken string
	ExpiresAt   int64
	Endpoint    string
}

func adminKeyHash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// Explicit credentials never fall back to an ambient browser session.
func adminBearer(r *http.Request) (token string, present, valid bool) {
	var values []string
	for name, value := range r.Header {
		if strings.EqualFold(name, "Authorization") {
			present = true
			values = append(values, value...)
		}
	}
	if !present {
		return "", false, false
	}
	if len(values) != 1 {
		return "", true, false
	}
	for _, c := range values[0] {
		if c > 126 || c < 32 && c != '\t' {
			return "", true, false
		}
	}
	parts := strings.Fields(values[0])
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", true, false
	}
	for _, c := range parts[1] {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-._~+/=", c)) {
			return "", true, false
		}
	}
	return parts[1], true, true
}

func rememberAdminAuthentication(r *http.Request, auth adminAuthentication) {
	auth.Endpoint = r.URL.Path
	*r = *r.WithContext(context.WithValue(r.Context(), adminAuthenticationContextKey{}, auth))
}

func rotationEndpoint(ctx context.Context) string {
	if auth, ok := ctx.Value(adminAuthenticationContextKey{}).(adminAuthentication); ok {
		return auth.Endpoint
	}
	return "/api/settings/rotate-key"
}

func validRotationRequestID(requestID string) bool {
	if requestID == "" || len(requestID) > 128 {
		return false
	}
	for _, c := range requestID {
		if c <= 32 || c == 127 || unicode.IsSpace(c) {
			return false
		}
	}
	return true
}

func validRotationReplay(prior rotationResult, active, endpoint string) bool {
	storedEndpoint := prior.Endpoint
	if storedEndpoint == "" {
		storedEndpoint = "/api/settings/rotate-key"
	}
	age := time.Now().Unix() - prior.CreatedAt
	return prior.Key != "" && active != "" && subtle.ConstantTimeCompare([]byte(active), []byte(prior.Key)) == 1 && age >= 0 && age <= 86400 && storedEndpoint == endpoint
}

func rotationCallerMatches(ctx context.Context, current string) bool {
	auth, ok := ctx.Value(adminAuthenticationContextKey{}).(adminAuthentication)
	return !ok || auth.BearerHash == "" || subtle.ConstantTimeCompare([]byte(auth.BearerHash), []byte(adminKeyHash(current))) == 1
}

func rotationReplayCallerMatches(ctx context.Context, prior rotationResult) bool {
	if rotationCallerMatches(ctx, prior.Key) {
		return true
	}
	auth, ok := ctx.Value(adminAuthenticationContextKey{}).(adminAuthentication)
	return ok && auth.BearerHash != "" && subtle.ConstantTimeCompare([]byte(auth.BearerHash), []byte(prior.PreviousHash)) == 1
}

func sessionError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"type": "proxy_error", "code": publicerrors.Message(code), "message": publicerrors.Message(message)}})
}
func (p *Proxy) currentAdminKey(ctx context.Context) (string, error) {
	if p.Store.DB == nil {
		return p.adminKey(), nil
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var key string
	if err := p.Store.DB.readDocument(ctx, p.Store.DB.Pool, "admin_key", &key); err != nil {
		return "", err
	}
	return key, nil
}
func (d *Database) initializeAdminKey(ctx context.Context, key string) (string, error) {
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return "", safeDBError(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended(current_schema() || ':document:admin_key',0))"); err != nil {
		return "", safeDBError(err)
	}
	var current string
	if err = d.readDocument(ctx, tx, "admin_key", &current); err != nil {
		return "", err
	}
	if current == "" {
		if len(key) < 32 {
			return "", errors.New("ADMIN_KEY must contain at least 32 characters for first startup")
		}
		current = key
		if err = d.writeDocument(ctx, tx, "admin_key", current); err != nil {
			return "", err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return "", safeDBError(err)
	}
	return current, nil
}
func (p *Proxy) rotateAdminKey(ctx context.Context, requestID string) (string, error) {
	if !validRotationRequestID(requestID) {
		return "", errInvalid
	}
	if p.Store.DB == nil {
		fallbackRotateMu.Lock()
		defer fallbackRotateMu.Unlock()
		return p.rotateFileAdminKey(ctx, requestID)
	}
	d := p.Store.DB
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return "", safeDBError(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended(current_schema() || ':document:admin_key',0))"); err != nil {
		return "", safeDBError(err)
	}
	var prior rotationResult
	if err = d.readDocument(ctx, tx, "rotation:"+requestID, &prior); err != nil {
		return "", err
	}
	if prior.Key != "" {
		var active string
		if err = d.readDocument(ctx, tx, "admin_key", &active); err != nil {
			return "", err
		}
		if !validRotationReplay(prior, active, rotationEndpoint(ctx)) || !rotationReplayCallerMatches(ctx, prior) {
			return "", errConflict
		}
		return prior.Key, nil
	}
	var previous string
	if err = d.readDocument(ctx, tx, "admin_key", &previous); err != nil {
		return "", err
	}
	previousHash := sha256.Sum256([]byte(previous))
	if !rotationCallerMatches(ctx, previous) {
		return "", errConflict
	}
	key := "ca_" + strings.TrimPrefix(newAPIKey(), "sk-")
	if err = d.writeDocument(ctx, tx, "admin_key", key); err != nil {
		return "", err
	}
	if err = d.writeDocument(ctx, tx, "rotation:"+requestID, rotationResult{Key: key, PreviousHash: hex.EncodeToString(previousHash[:]), CreatedAt: time.Now().Unix(), Endpoint: rotationEndpoint(ctx), RequestID: requestID}); err != nil {
		return "", err
	}
	if err = d.audit(ctx, tx, "admin_key.rotate", requestID); err != nil {
		return "", safeDBError(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return "", safeDBError(err)
	}
	p.setAdminKey(key)
	d.invalidate()
	return key, nil
}

type rotationResult struct {
	Key          string `json:"key"`
	PreviousHash string `json:"previous_hash"`
	CreatedAt    int64  `json:"created_at"`
	Endpoint     string `json:"endpoint,omitempty"`
	RequestID    string `json:"request_id,omitempty"`
}

func (p *Proxy) rotateFileAdminKey(ctx context.Context, requestID string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if strings.TrimSpace(p.KeyFile) == "" {
		return "", errors.New("admin key file is not configured")
	}
	store := newFileStore[rotationResult](p.KeyFile + ".rotation.json")
	prior, err := store.Read()
	if err != nil {
		return "", err
	}
	active := p.adminKey()
	if prior.RequestID == requestID && prior.Key != "" {
		if !validRotationReplay(prior, active, rotationEndpoint(ctx)) || !rotationReplayCallerMatches(ctx, prior) {
			return "", errConflict
		}
		return prior.Key, nil
	}
	if !rotationCallerMatches(ctx, active) {
		return "", errConflict
	}
	key := "ca_" + strings.TrimPrefix(newAPIKey(), "sk-")
	result := rotationResult{Key: key, PreviousHash: adminKeyHash(active), CreatedAt: time.Now().Unix(), Endpoint: rotationEndpoint(ctx), RequestID: requestID}
	// Recovery evidence is durable before activation. A failed key-file commit
	// leaves the old active key valid and cannot authorize a pending replay.
	if err := store.Modify(func(value *rotationResult) error { *value = result; return nil }); err != nil {
		return "", err
	}
	file, err := os.CreateTemp(filepath.Dir(p.KeyFile), ".client-key-*")
	if err != nil {
		return "", err
	}
	temp := file.Name()
	defer os.Remove(temp)
	if err = file.Chmod(0600); err == nil {
		_, err = file.WriteString(key + "\n")
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return "", err
	}
	if err = os.Rename(temp, p.KeyFile); err != nil {
		return "", err
	}
	p.setAdminKey(key)
	return key, nil
}

type adminSession struct {
	ExpiresAt int64 `json:"expires_at"`
}

func (d *Database) sessionKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return d.Namespace + "session:" + hex.EncodeToString(sum[:])
}
func sameOrigin(r *http.Request) bool {
	var origins []string
	present := false
	for name, values := range r.Header {
		if strings.EqualFold(name, "Origin") {
			present = true
			origins = append(origins, values...)
		}
	}
	if !present {
		return true
	}
	if len(origins) != 1 || origins[0] == "" || strings.ContainsAny(origins[0], "?#") {
		return false
	}
	u, err := url.Parse(origins[0])
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.User == nil && u.Path == "" && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && strings.EqualFold(u.Host, r.Host)
}
func (p *Proxy) authenticateAdmin(w http.ResponseWriter, r *http.Request) bool {
	token, present, valid := adminBearer(r)
	if present {
		if !valid {
			sessionError(w, 401, "unauthorized", "invalid admin key")
			return false
		}
		key, err := p.currentAdminKey(r.Context())
		if err != nil {
			sessionError(w, 503, "storage_unavailable", "authentication storage unavailable")
			return false
		}
		if key == "" || subtle.ConstantTimeCompare([]byte(key), []byte(token)) != 1 {
			if !p.rotationReplayAuthorized(r) {
				sessionError(w, 401, "unauthorized", "invalid admin key")
				return false
			}
		}
		rememberAdminAuthentication(r, adminAuthentication{BearerHash: adminKeyHash(token)})
		return true
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead && !sameOrigin(r) {
		sessionError(w, 403, "origin_mismatch", "request origin does not match")
		return false
	}
	cookies := r.CookiesNamed(sessionCookie)
	if len(cookies) > 1 || len(cookies) == 1 && cookies[0].Value == "" {
		sessionError(w, 401, "session_expired", "invalid admin session")
		return false
	}
	if len(cookies) == 1 {
		cookie := cookies[0]
		d := p.Store.DB
		if d == nil || d.Redis == nil {
			sessionError(w, 503, "session_unavailable", "session service unavailable")
			return false
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		raw, err := d.Redis.Get(ctx, d.sessionKey(cookie.Value)).Bytes()
		if errors.Is(err, redis.Nil) {
			sessionError(w, 401, "session_expired", "session expired")
			return false
		}
		if err != nil {
			sessionError(w, 503, "session_unavailable", "session service unavailable")
			return false
		}
		var session adminSession
		if json.Unmarshal(raw, &session) != nil || session.ExpiresAt <= time.Now().Unix() {
			sessionError(w, 401, "session_expired", "session expired")
			return false
		}
		rememberAdminAuthentication(r, adminAuthentication{CookieToken: cookie.Value, ExpiresAt: session.ExpiresAt})
		return true
	}
	sessionError(w, 401, "unauthorized", "invalid admin key")
	return false
}
func (p *Proxy) handleSessionLogin(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		sessionError(w, 403, "origin_mismatch", "request origin does not match")
		return
	}
	var input struct {
		AdminKey string `json:"admin_key"`
	}
	if err := readJSON(w, r, &input); err != nil {
		adminError(w, err)
		return
	}
	key, err := p.currentAdminKey(r.Context())
	if err != nil {
		sessionError(w, 503, "storage_unavailable", "authentication storage unavailable")
		return
	}
	if key == "" || subtle.ConstantTimeCompare([]byte(key), []byte(input.AdminKey)) != 1 {
		sessionError(w, 401, "unauthorized", "invalid admin key")
		return
	}
	d := p.Store.DB
	if d == nil || d.Redis == nil {
		sessionError(w, 503, "session_unavailable", "session service unavailable")
		return
	}
	token := newAPIKey() + newAPIKey()
	session := adminSession{ExpiresAt: time.Now().Add(d.SessionTTL).Unix()}
	raw, _ := json.Marshal(session)
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err = d.Redis.Set(ctx, d.sessionKey(token), raw, d.SessionTTL).Err(); err != nil {
		sessionError(w, 503, "session_unavailable", "session service unavailable")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: token, Path: "/api", HttpOnly: true, Secure: d.CookieSecure, SameSite: http.SameSiteStrictMode, MaxAge: int(d.SessionTTL.Seconds()), Expires: time.Unix(session.ExpiresAt, 0)})
	writeJSON(w, map[string]any{"authenticated": true, "expires_at": session.ExpiresAt})
}
func (p *Proxy) handleSessionGet(w http.ResponseWriter, r *http.Request) {
	expires := int64(0)
	if auth, ok := r.Context().Value(adminAuthenticationContextKey{}).(adminAuthentication); ok {
		expires = auth.ExpiresAt
	}
	writeJSON(w, map[string]any{"authenticated": true, "expires_at": expires})
}
func (p *Proxy) handleSessionLogout(w http.ResponseWriter, r *http.Request) {
	if auth, ok := r.Context().Value(adminAuthenticationContextKey{}).(adminAuthentication); ok && auth.CookieToken != "" {
		d := p.Store.DB
		if d == nil || d.Redis == nil {
			sessionError(w, 503, "session_unavailable", "session service unavailable")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := d.Redis.Del(ctx, d.sessionKey(auth.CookieToken)).Err(); err != nil {
			sessionError(w, 503, "session_unavailable", "session service unavailable")
			return
		}
	}
	secure := true
	if p.Store.DB != nil {
		secure = p.Store.DB.CookieSecure
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/api", HttpOnly: true, Secure: secure, SameSite: http.SameSiteStrictMode, MaxAge: -1, Expires: time.Unix(1, 0)})
	writeJSON(w, map[string]any{"ok": true, "authenticated": false})
}

// A lost rotation response can be replayed with the old bearer key and exact request ID.
// This grants no other endpoint access and never performs a second rotation.
func (p *Proxy) rotationReplayAuthorized(r *http.Request) bool {
	token, present, valid := adminBearer(r)
	if !present || !valid || r.Method != http.MethodPost || (r.URL.Path != "/api/settings/rotate-key" && r.URL.Path != "/api/settings") || r.Body == nil {
		return false
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, (2<<20)+1))
	r.Body = io.NopCloser(bytes.NewReader(raw))
	if err != nil || len(raw) > 2<<20 {
		return false
	}
	var input struct {
		RequestID     string `json:"request_id"`
		Regenerate    bool   `json:"regenerate"`
		RegenerateKey bool   `json:"regenerate_key"`
	}
	// Replay grants a narrow exception to revoked credentials. Reject duplicate,
	// unknown, or case-variant JSON fields rather than accepting last-value wins.
	decoder := json.NewDecoder(bytes.NewReader(raw))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return false
	}
	seen := make(map[string]bool)
	for decoder.More() {
		field, err := decoder.Token()
		name, ok := field.(string)
		if err != nil || !ok || seen[name] {
			return false
		}
		seen[name] = true
		switch name {
		case "request_id":
			err = decoder.Decode(&input.RequestID)
		case "regenerate":
			if r.URL.Path != "/api/settings" {
				return false
			}
			err = decoder.Decode(&input.Regenerate)
		case "regenerate_key":
			if r.URL.Path != "/api/settings" {
				return false
			}
			err = decoder.Decode(&input.RegenerateKey)
		default:
			return false
		}
		if err != nil {
			return false
		}
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') || decoder.Decode(new(any)) != io.EOF || !validRotationRequestID(input.RequestID) {
		return false
	}
	if r.URL.Path == "/api/settings" && !input.Regenerate && !input.RegenerateKey {
		return false
	}
	var prior rotationResult
	if p.Store.DB == nil {
		if strings.TrimSpace(p.KeyFile) == "" {
			return false
		}
		prior, err = newFileStore[rotationResult](p.KeyFile + ".rotation.json").Read()
		if prior.RequestID != input.RequestID {
			return false
		}
	} else {
		err = p.Store.DB.readDocument(r.Context(), p.Store.DB.Pool, "rotation:"+input.RequestID, &prior)
	}
	if err != nil {
		return false
	}
	active, err := p.currentAdminKey(r.Context())
	if err != nil || !validRotationReplay(prior, active, r.URL.Path) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(prior.PreviousHash), []byte(adminKeyHash(token))) == 1
}
