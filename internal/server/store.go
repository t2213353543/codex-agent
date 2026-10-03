package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"time"
)

// Semantic errors shared by all stores. Admin handlers map them to 400
// (client errors) while any other store failure is a 500-class persistence
// problem.
var (
	errNotFound      = errors.New("not found")
	errInvalid       = errors.New("invalid value")
	errConflict      = errors.New("already exists")
	errStoreNoChange = errors.New("stored value is unchanged")
)

// newID returns a random identifier with a readable prefix.
func newID(prefix string) string {
	var random [6]byte
	if _, err := rand.Read(random[:]); err != nil {
		panic(err)
	}
	return prefix + hex.EncodeToString(random[:])
}

// newAPIKey returns a fresh gateway key in the conventional sk- format.
func newAPIKey() string {
	var random [24]byte
	if _, err := rand.Read(random[:]); err != nil {
		panic(err)
	}
	return "sk-" + hex.EncodeToString(random[:])
}

// fileStore persists one JSON document with atomic replace and a directory
// lock shared with the account store semantics.
type fileStore[T any] struct {
	path string
	db   *Database
	key  string
	mu   sync.Mutex
}

func newFileStore[T any](path string) *fileStore[T] {
	return &fileStore[T]{path: path}
}

func (f *fileStore[T]) Read() (T, error) {
	var empty T
	if f.db != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		err := f.db.readDocument(ctx, f.db.Pool, f.key, &empty)
		return empty, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, err := os.ReadFile(f.path)
	if errors.Is(err, os.ErrNotExist) {
		return empty, nil
	}
	if err != nil {
		return empty, fmt.Errorf("read %s: %w", filepath.Base(f.path), err)
	}
	if err := json.Unmarshal(raw, &empty); err != nil {
		return empty, fmt.Errorf("decode %s: %w", filepath.Base(f.path), err)
	}
	return empty, nil
}

func (f *fileStore[T]) Modify(change func(*T) error) error {
	if f.db != nil {
		return f.modifyPG(change)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(f.path), 0700); err != nil {
		return fmt.Errorf("create %s directory: %w", filepath.Base(f.path), err)
	}
	var value T
	if err := f.readLocked(&value); err != nil {
		return err
	}
	if err := change(&value); err != nil {
		if errors.Is(err, errStoreNoChange) {
			return nil
		}
		return err
	}
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(f.path), "."+filepath.Base(f.path)+"-*")
	if err != nil {
		return fmt.Errorf("create temporary %s: %w", filepath.Base(f.path), err)
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
	if err := os.Rename(temp, f.path); err != nil {
		return fmt.Errorf("replace %s: %w", filepath.Base(f.path), err)
	}
	return nil
}

func (f *fileStore[T]) readLocked(target *T) error {
	raw, err := os.ReadFile(f.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", filepath.Base(f.path), err)
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return fmt.Errorf("decode %s: %w", filepath.Base(f.path), err)
	}
	return nil
}

// Group is a routing bucket that accounts and API keys reference by ID.
type Group struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	CreatedAt   int64  `json:"created_at"`
	Version     int64  `json:"version"`
	UpdatedAt   int64  `json:"updated_at"`
}

type groupState struct {
	Groups []Group `json:"groups"`
}

type GroupStore struct {
	DB    *Database
	store *fileStore[groupState]
}

func NewGroupStore(path string) *GroupStore {
	return &GroupStore{store: newFileStore[groupState](path)}
}

func (g *GroupStore) List() ([]Group, error) {
	if g.DB != nil {
		return g.listPG()
	}
	state, err := g.store.Read()
	if state.Groups == nil {
		state.Groups = []Group{}
	}
	return state.Groups, err
}

func (g *GroupStore) Get(id string) (Group, error) {
	if g.DB != nil {
		return g.getPG(id)
	}
	groups, err := g.List()
	if err != nil {
		return Group{}, err
	}
	for _, group := range groups {
		if group.ID == id {
			return group, nil
		}
	}
	return Group{}, fmt.Errorf("%w: group %q", errNotFound, id)
}

func (g *GroupStore) Create(name, description string) (Group, error) {
	if g.DB != nil {
		return g.createPG(name, description)
	}
	if err := validGroupName(name); err != nil {
		return Group{}, err
	}
	group := Group{ID: newID("g_"), Name: name, Description: description, CreatedAt: time.Now().Unix()}
	err := g.store.Modify(func(state *groupState) error {
		for _, existing := range state.Groups {
			if existing.Name == name {
				return fmt.Errorf("%w: group %q", errConflict, name)
			}
		}
		state.Groups = append(state.Groups, group)
		return nil
	})
	if err != nil {
		return Group{}, err
	}
	return group, nil
}

func (g *GroupStore) Update(id, name, description string) (Group, error) {
	if g.DB != nil {
		return g.UpdateVersion(id, name, description, 0)
	}
	return g.updateFileVersion(id, name, description, 0)
}

func (g *GroupStore) updateFileVersion(id, name, description string, expected int64) (Group, error) {
	if err := validGroupName(name); err != nil {
		return Group{}, err
	}
	var updated Group
	err := g.store.Modify(func(state *groupState) error {
		for i := range state.Groups {
			if state.Groups[i].ID == id {
				if expected > 0 && state.Groups[i].Version != expected {
					return errVersionConflict
				}
				for _, other := range state.Groups {
					if other.ID != id && other.Name == name {
						return fmt.Errorf("%w: group %q", errConflict, name)
					}
				}
				if state.Groups[i].Name == name && state.Groups[i].Description == description && state.Groups[i].Version > 0 {
					updated = state.Groups[i]
					return errStoreNoChange
				}
				state.Groups[i].Name = name
				state.Groups[i].Description = description
				state.Groups[i].Version = max(int64(1), state.Groups[i].Version+1)
				state.Groups[i].UpdatedAt = time.Now().Unix()
				updated = state.Groups[i]
				return nil
			}
		}
		return fmt.Errorf("%w: group %q", errNotFound, id)
	})
	if err != nil {
		return Group{}, err
	}
	return updated, nil
}

func (g *GroupStore) Delete(id string) error {
	if g.DB != nil {
		return g.deletePG(id)
	}
	return g.store.Modify(func(state *groupState) error {
		for i, group := range state.Groups {
			if group.ID == id {
				state.Groups = append(state.Groups[:i], state.Groups[i+1:]...)
				return nil
			}
		}
		return fmt.Errorf("%w: group %q", errNotFound, id)
	})
}

func validGroupName(name string) error {
	name = trimSpace(name)
	if name == "" || len(name) > 60 {
		return fmt.Errorf("%w: 分组名称需要 1 到 60 个字符", errInvalid)
	}
	return nil
}

func trimSpace(value string) string {
	for len(value) > 0 && (value[0] == ' ' || value[0] == '\t' || value[0] == '\n' || value[0] == '\r') {
		value = value[1:]
	}
	for len(value) > 0 && (value[len(value)-1] == ' ' || value[len(value)-1] == '\t' || value[len(value)-1] == '\n' || value[len(value)-1] == '\r') {
		value = value[:len(value)-1]
	}
	return value
}

// APIKey grants gateway access to the accounts of its groups.
type APIKey struct {
	Version   int64    `json:"version"`
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Key       string   `json:"key"`
	Groups    []string `json:"groups,omitempty"`
	Enabled   bool     `json:"enabled"`
	CreatedAt int64    `json:"created_at"`
	LastUsed  int64    `json:"last_used_at,omitempty"`
}

type keyState struct {
	Keys []APIKey `json:"keys"`
}

type KeyStore struct {
	store           *fileStore[keyState]
	mu              sync.RWMutex
	index           map[string]*APIKey  // key material -> entry
	currentSnapshot *currentKeySnapshot // immutable, validated against PG on every read
}

func NewKeyStore(path string) *KeyStore {
	result := &KeyStore{store: newFileStore[keyState](path), index: map[string]*APIKey{}}
	_, _ = result.List()
	return result
}

func (k *KeyStore) List() ([]APIKey, error) {
	state, err := k.readCurrentState(context.Background())
	if err != nil {
		return nil, err
	}
	if state.Keys == nil {
		state.Keys = []APIKey{}
	}
	k.mu.Lock()
	k.index = make(map[string]*APIKey, len(state.Keys))
	for i := range state.Keys {
		if state.Keys[i].Version < 1 {
			state.Keys[i].Version = 1
		}
		entry := &state.Keys[i]
		k.index[entry.Key] = entry
	}
	k.mu.Unlock()
	return state.Keys, nil
}

// Lookup returns the enabled entry for a gateway key.
func (k *KeyStore) Lookup(key string) (*APIKey, bool) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	entry, ok := k.index[key]
	if !ok || entry == nil || !entry.Enabled {
		return nil, false
	}
	return entry, true
}

// TouchLastUsed updates the in-memory last-used time; persistence happens on
// the periodic flush so hot request paths never rewrite the file.
func (k *KeyStore) TouchLastUsed(key string, at int64) {
	k.mu.Lock()
	if entry, ok := k.index[key]; ok {
		entry.LastUsed = at
	}
	k.mu.Unlock()
}

func (k *KeyStore) Create(name string, groups []string) (APIKey, error) {
	if trimSpace(name) == "" || len(name) > 60 {
		return APIKey{}, fmt.Errorf("%w: 密钥名称需要 1 到 60 个字符", errInvalid)
	}
	entry := APIKey{Version: 1, ID: newID("k_"), Name: name, Key: newAPIKey(), Groups: uniqueStrings(groups), Enabled: true, CreatedAt: time.Now().Unix()}
	err := k.store.Modify(func(state *keyState) error {
		state.Keys = append(state.Keys, entry)
		return nil
	})
	if err != nil {
		return APIKey{}, err
	}
	k.mu.Lock()
	k.index[entry.Key] = &entry
	k.mu.Unlock()
	return entry, nil
}

func (k *KeyStore) Update(id string, apply func(*APIKey) error) (APIKey, error) {
	var updated APIKey
	unchanged := false
	err := k.store.Modify(func(state *keyState) error {
		for i := range state.Keys {
			if state.Keys[i].ID == id {
				original := state.Keys[i]
				original.Groups = slices.Clone(original.Groups)
				if state.Keys[i].Version < 1 {
					state.Keys[i].Version = 1
				}
				if err := apply(&state.Keys[i]); err != nil {
					return err
				}
				if reflect.DeepEqual(original, state.Keys[i]) {
					updated, unchanged = state.Keys[i], true
					return errStoreNoChange
				}
				state.Keys[i].Version++
				updated = state.Keys[i]
				return nil
			}
		}
		return fmt.Errorf("%w: key %q", errNotFound, id)
	})
	if err != nil {
		return APIKey{}, err
	}
	if !unchanged {
		_, _ = k.List()
	} else {
		k.publishCurrentEntry(updated)
	}
	return updated, nil
}

func (k *KeyStore) publishCurrentEntry(value APIKey) {
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, current := range k.index {
		if current != nil && current.ID == value.ID && current.Version > value.Version {
			return
		}
	}
	for token, current := range k.index {
		if current != nil && current.ID == value.ID {
			if token == value.Key {
				value.LastUsed = max(value.LastUsed, current.LastUsed)
			}
			delete(k.index, token)
		}
	}
	if k.index == nil {
		k.index = make(map[string]*APIKey)
	}
	value.Groups = slices.Clone(value.Groups)
	k.index[value.Key] = &value
}

func (k *KeyStore) Delete(id string) error {
	err := k.store.Modify(func(state *keyState) error {
		for i, entry := range state.Keys {
			if entry.ID == id {
				state.Keys = append(state.Keys[:i], state.Keys[i+1:]...)
				return nil
			}
		}
		return fmt.Errorf("%w: key %q", errNotFound, id)
	})
	if err != nil {
		return err
	}
	_, _ = k.List()
	return nil
}

// RemoveGroup detaches a deleted group from every key.
func (k *KeyStore) RemoveGroup(id string) error {
	err := k.store.Modify(func(state *keyState) error {
		for i := range state.Keys {
			state.Keys[i].Groups = removeString(state.Keys[i].Groups, id)
		}
		return nil
	})
	if err != nil {
		return err
	}
	_, _ = k.List()
	return nil
}

// FlushLastUsed persists any in-memory last-used updates to disk.
func (k *KeyStore) FlushLastUsed() error {
	k.mu.RLock()
	dirty := make(map[string]int64)
	for key, entry := range k.index {
		if entry != nil && entry.LastUsed > 0 {
			dirty[key] = entry.LastUsed
		}
	}
	k.mu.RUnlock()
	if len(dirty) == 0 {
		return nil
	}
	err := k.store.Modify(func(state *keyState) error {
		for i := range state.Keys {
			if at, ok := dirty[state.Keys[i].Key]; ok {
				state.Keys[i].LastUsed = at
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	_, _ = k.List()
	return nil
}

func removeString(values []string, target string) []string {
	result := values[:0]
	for _, value := range values {
		if value != target {
			result = append(result, value)
		}
	}
	return result
}

// ProxyEntry is one outbound proxy that accounts can bind for upstream calls.
type ProxyEntry struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Type        string `json:"type"` // http | socks5
	Host        string `json:"host"`
	Port        int    `json:"port"`
	Username    string `json:"username,omitempty"`
	Password    string `json:"password,omitempty"`
	HasPassword bool   `json:"has_password"`
	Version     int64  `json:"version"`
	UpdatedAt   int64  `json:"updated_at"`
	Enabled     bool   `json:"enabled"`
	CreatedAt   int64  `json:"created_at"`
}

type proxyState struct {
	Proxies []ProxyEntry `json:"proxies"`
}

type ProxyStore struct {
	DB    *Database
	store *fileStore[proxyState]
	mu    sync.RWMutex
	index map[string]*ProxyEntry
}

func NewProxyStore(path string) *ProxyStore {
	result := &ProxyStore{store: newFileStore[proxyState](path), index: map[string]*ProxyEntry{}}
	_, _ = result.List()
	return result
}

func (p *ProxyStore) List() ([]ProxyEntry, error) {
	if p.DB != nil {
		return p.listPG(true)
	}
	state, err := p.store.Read()
	if err != nil {
		return nil, err
	}
	if state.Proxies == nil {
		state.Proxies = []ProxyEntry{}
	}
	p.mu.Lock()
	p.index = make(map[string]*ProxyEntry, len(state.Proxies))
	for i := range state.Proxies {
		entry := &state.Proxies[i]
		p.index[entry.ID] = entry
	}
	p.mu.Unlock()
	return state.Proxies, nil
}

// Get resolves one proxy from the in-memory index; gateway hot paths never
// touch the disk file.
func (p *ProxyStore) Get(id string) (*ProxyEntry, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	entry, ok := p.index[id]
	if !ok || entry == nil || !entry.Enabled {
		return nil, false
	}
	return entry, true
}

func validProxy(entry ProxyEntry) error {
	if trimSpace(entry.Name) == "" || len(entry.Name) > 60 {
		return fmt.Errorf("%w: 代理名称需要 1 到 60 个字符", errInvalid)
	}
	if entry.Type != "http" && entry.Type != "socks5" {
		return fmt.Errorf("%w: 代理类型必须是 http 或 socks5", errInvalid)
	}
	if trimSpace(entry.Host) == "" || len(entry.Host) > 253 {
		return fmt.Errorf("%w: 代理主机不能为空且不能超过 253 个字符", errInvalid)
	}
	if entry.Port < 1 || entry.Port > 65535 {
		return fmt.Errorf("%w: 代理端口必须在 1 到 65535 之间", errInvalid)
	}
	return nil
}

func (p *ProxyStore) Create(entry ProxyEntry) (ProxyEntry, error) {
	if p.DB != nil {
		return p.createPG(entry)
	}
	if err := validProxy(entry); err != nil {
		return ProxyEntry{}, err
	}
	entry.ID = newID("p_")
	entry.CreatedAt = time.Now().Unix()
	entry.Enabled = true
	err := p.store.Modify(func(state *proxyState) error {
		state.Proxies = append(state.Proxies, entry)
		return nil
	})
	if err != nil {
		return ProxyEntry{}, err
	}
	_, _ = p.List()
	return entry, nil
}

func (p *ProxyStore) Update(id string, apply func(*ProxyEntry) error) (ProxyEntry, error) {
	if p.DB != nil {
		return p.UpdateVersion(id, 0, apply)
	}
	var updated ProxyEntry
	unchanged := false
	err := p.store.Modify(func(state *proxyState) error {
		for i := range state.Proxies {
			if state.Proxies[i].ID == id {
				original := state.Proxies[i]
				if err := apply(&state.Proxies[i]); err != nil {
					return err
				}
				if err := validProxy(state.Proxies[i]); err != nil {
					return err
				}
				updated = state.Proxies[i]
				if updated == original {
					unchanged = true
					return errStoreNoChange
				}
				return nil
			}
		}
		return fmt.Errorf("%w: proxy %q", errNotFound, id)
	})
	if err != nil {
		return ProxyEntry{}, err
	}
	if !unchanged {
		_, _ = p.List()
	} else {
		p.publishCurrentEntry(updated)
	}
	return updated, nil
}

func (p *ProxyStore) publishCurrentEntry(value ProxyEntry) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.index == nil {
		p.index = make(map[string]*ProxyEntry)
	}
	if current := p.index[value.ID]; current == nil || current.Version <= value.Version {
		p.index[value.ID] = &value
	}
}

func (p *ProxyStore) Delete(id string) error {
	if p.DB != nil {
		return p.deletePG(id)
	}
	err := p.store.Modify(func(state *proxyState) error {
		for i, entry := range state.Proxies {
			if entry.ID == id {
				state.Proxies = append(state.Proxies[:i], state.Proxies[i+1:]...)
				return nil
			}
		}
		return fmt.Errorf("%w: proxy %q", errNotFound, id)
	})
	if err != nil {
		return err
	}
	_, _ = p.List()
	return nil
}

// Settings holds admin-level toggles. The admin key itself lives in the
// client-key file so old data directories keep working unchanged.
type Settings struct {
	Version                              int64  `json:"settings_version"`
	InferenceMode                        string `json:"inference_mode"`
	AdminAPI                             bool   `json:"admin_api"`
	WebsocketEnabled                     bool   `json:"websocket_enabled"`
	PreferWebsocket                      bool   `json:"prefer_websocket"`
	BPSWebSocketReuse                    bool   `json:"bps_websocket_reuse"`
	CodexFingerprintEnabled              bool   `json:"codex_fingerprint_enabled"`
	CodexFingerprintProfile              string `json:"codex_fingerprint_profile"`
	BuiltinPromptEnabled                 bool   `json:"builtin_prompt_enabled"`
	BuiltinPrompt                        string `json:"builtin_prompt"`
	StickySessionsEnabled                bool   `json:"sticky_sessions_enabled"`
	StickySessionTTLSeconds              int    `json:"sticky_session_ttl_seconds"`
	PromptCacheKeyEnabled                bool   `json:"prompt_cache_key_enabled"`
	CacheCreationAsReadEnabled           bool   `json:"cache_creation_as_read_enabled"`
	CacheReadRatioEnabled                bool   `json:"cache_read_ratio_enabled"`
	CacheReadRatioPercent                int    `json:"cache_read_ratio_percent"`
	BPSPromptCacheScope                  string `json:"bps_prompt_cache_scope"`
	BPSModelRoutes                       string `json:"bps_model_routes"`
	RateLimitCooldownEnabled             bool   `json:"rate_limit_cooldown_enabled"`
	HTTPStreamEnabled                    bool   `json:"http_stream_enabled"`
	HTTPJSONEnabled                      bool   `json:"http_json_enabled"`
	CompactEnabled                       bool   `json:"compact_enabled"`
	ModelsEnabled                        bool   `json:"models_enabled"`
	CompatibilityNormalizationEnabled    bool   `json:"compatibility_normalization_enabled"`
	AutomaticTokenRefreshEnabled         bool   `json:"automatic_token_refresh_enabled"`
	Upstream401RecoveryEnabled           bool   `json:"upstream_401_recovery_enabled"`
	BuiltinPromptMode                    string `json:"builtin_prompt_mode"`
	UpstreamStreamIdleTimeoutSeconds     int    `json:"upstream_stream_idle_timeout_seconds"`
	UpstreamResponseHeaderTimeoutSeconds int    `json:"upstream_response_header_timeout_seconds"`
	UpstreamCompactTimeoutSeconds        int    `json:"upstream_compact_timeout_seconds"`
}

const defaultBuiltinPrompt = "You are a helpful, precise assistant. Follow the user's instructions, preserve relevant context, and be clear about uncertainty. Use tools when appropriate, and only claim actions or results that were actually performed or observed."
const maxBuiltinPromptBytes = 32 << 10
const settingsCacheTTL = 2 * time.Second

func defaultSettings() Settings {
	return Settings{Version: 1, InferenceMode: inferencePowerPoint, AdminAPI: true, BuiltinPromptEnabled: true,
		CodexFingerprintProfile: defaultCodexFingerprintProfile,
		BuiltinPrompt:           defaultBuiltinPrompt, StickySessionsEnabled: true,
		StickySessionTTLSeconds: 604800, PromptCacheKeyEnabled: true, BPSPromptCacheScope: bpsPromptCacheScopeClient,
		BPSModelRoutes: defaultBPSModelRoutes(), CacheCreationAsReadEnabled: true,
		HTTPStreamEnabled: true, HTTPJSONEnabled: true, CompactEnabled: true,
		ModelsEnabled: true, CompatibilityNormalizationEnabled: true,
		AutomaticTokenRefreshEnabled: true, Upstream401RecoveryEnabled: true,
		BuiltinPromptMode: "prepend", UpstreamStreamIdleTimeoutSeconds: 300,
		UpstreamResponseHeaderTimeoutSeconds: 90, UpstreamCompactTimeoutSeconds: 1200}
}

func validateSettings(settings Settings) error {
	if settings.CacheReadRatioPercent < 0 || settings.CacheReadRatioPercent > 100 {
		return fmt.Errorf("%w: cache_read_ratio_percent must be between 0 and 100", errInvalid)
	}
	if !validInferenceMode(settings.InferenceMode) {
		return fmt.Errorf("%w: inference_mode must be powerpoint_bps, official_codex_cli or official_app_server", errInvalid)
	}
	if settings.Version < 0 {
		return fmt.Errorf("%w: settings_version must not be negative", errInvalid)
	}
	switch settings.BPSPromptCacheScope {
	case bpsPromptCacheScopeClient, bpsPromptCacheScopeAPIKey:
	default:
		return fmt.Errorf("%w: bps_prompt_cache_scope must be client or api_key", errInvalid)
	}
	if err := validateBPSModelRoutes(settings.BPSModelRoutes); err != nil {
		return fmt.Errorf("%w: bps_model_routes: %v", errInvalid, err)
	}
	switch settings.BuiltinPromptMode {
	case "prepend", "append", "if_empty":
	default:
		return fmt.Errorf("%w: builtin_prompt_mode must be prepend, append or if_empty", errInvalid)
	}
	if settings.UpstreamStreamIdleTimeoutSeconds < 10 || settings.UpstreamStreamIdleTimeoutSeconds > 3600 {
		return fmt.Errorf("%w: upstream_stream_idle_timeout_seconds must be between 10 and 3600", errInvalid)
	}
	if settings.UpstreamResponseHeaderTimeoutSeconds < 5 || settings.UpstreamResponseHeaderTimeoutSeconds > 600 {
		return fmt.Errorf("%w: upstream_response_header_timeout_seconds must be between 5 and 600", errInvalid)
	}
	if settings.UpstreamCompactTimeoutSeconds < 30 || settings.UpstreamCompactTimeoutSeconds > 3600 {
		return fmt.Errorf("%w: upstream_compact_timeout_seconds must be between 30 and 3600", errInvalid)
	}
	if _, ok := codexFingerprintProfile(settings.CodexFingerprintProfile); !ok {
		return fmt.Errorf("%w: codex_fingerprint_profile must be linux or macos", errInvalid)
	}
	if settings.StickySessionTTLSeconds < 60 || settings.StickySessionTTLSeconds > 2592000 {
		return fmt.Errorf("%w: sticky_session_ttl_seconds must be between 60 and 2592000", errInvalid)
	}
	if len(settings.BuiltinPrompt) > maxBuiltinPromptBytes {
		return fmt.Errorf("%w: builtin_prompt must not exceed 32768 UTF-8 bytes", errInvalid)
	}
	return nil
}

type SettingsStore struct {
	store    *fileStore[settingsState]
	mu       sync.RWMutex
	cached   Settings
	loaded   bool
	loadedAt time.Time
}

// settingsState is the on-disk document; absence means defaults apply.
type settingsState struct {
	Version                              int64   `json:"version"`
	InferenceMode                        *string `json:"inference_mode,omitempty"`
	AdminAPI                             bool    `json:"admin_api"`
	WebsocketEnabled                     *bool   `json:"websocket_enabled,omitempty"`
	PreferWebsocket                      *bool   `json:"prefer_websocket,omitempty"`
	BPSWebSocketReuse                    *bool   `json:"bps_websocket_reuse,omitempty"`
	CodexFingerprintEnabled              *bool   `json:"codex_fingerprint_enabled,omitempty"`
	CodexFingerprintProfile              *string `json:"codex_fingerprint_profile,omitempty"`
	BuiltinPromptEnabled                 *bool   `json:"builtin_prompt_enabled,omitempty"`
	BuiltinPrompt                        *string `json:"builtin_prompt,omitempty"`
	StickySessionsEnabled                *bool   `json:"sticky_sessions_enabled,omitempty"`
	StickySessionTTLSeconds              *int    `json:"sticky_session_ttl_seconds,omitempty"`
	PromptCacheKeyEnabled                *bool   `json:"prompt_cache_key_enabled,omitempty"`
	CacheCreationAsReadEnabled           *bool   `json:"cache_creation_as_read_enabled,omitempty"`
	CacheReadRatioEnabled                *bool   `json:"cache_read_ratio_enabled,omitempty"`
	CacheReadRatioPercent                *int    `json:"cache_read_ratio_percent,omitempty"`
	BPSPromptCacheScope                  *string `json:"bps_prompt_cache_scope,omitempty"`
	BPSModelRoutes                       *string `json:"bps_model_routes,omitempty"`
	RateLimitCooldownEnabled             *bool   `json:"rate_limit_cooldown_enabled,omitempty"`
	HTTPStreamEnabled                    *bool   `json:"http_stream_enabled,omitempty"`
	HTTPJSONEnabled                      *bool   `json:"http_json_enabled,omitempty"`
	CompactEnabled                       *bool   `json:"compact_enabled,omitempty"`
	ModelsEnabled                        *bool   `json:"models_enabled,omitempty"`
	CompatibilityNormalizationEnabled    *bool   `json:"compatibility_normalization_enabled,omitempty"`
	AutomaticTokenRefreshEnabled         *bool   `json:"automatic_token_refresh_enabled,omitempty"`
	Upstream401RecoveryEnabled           *bool   `json:"upstream_401_recovery_enabled,omitempty"`
	BuiltinPromptMode                    *string `json:"builtin_prompt_mode,omitempty"`
	UpstreamStreamIdleTimeoutSeconds     *int    `json:"upstream_stream_idle_timeout_seconds,omitempty"`
	UpstreamResponseHeaderTimeoutSeconds *int    `json:"upstream_response_header_timeout_seconds,omitempty"`
	UpstreamCompactTimeoutSeconds        *int    `json:"upstream_compact_timeout_seconds,omitempty"`
}

func (state settingsState) settings() Settings {
	value := defaultSettings()
	if state.Version > 0 {
		value.Version = state.Version
	}
	value.AdminAPI = state.AdminAPI
	if state.InferenceMode != nil {
		value.InferenceMode = *state.InferenceMode
	}
	if state.WebsocketEnabled != nil {
		value.WebsocketEnabled = *state.WebsocketEnabled
	}
	if state.PreferWebsocket != nil {
		value.PreferWebsocket = *state.PreferWebsocket
	}
	if state.BPSWebSocketReuse != nil {
		value.BPSWebSocketReuse = *state.BPSWebSocketReuse
	}
	if state.CodexFingerprintEnabled != nil {
		value.CodexFingerprintEnabled = *state.CodexFingerprintEnabled
	}
	if state.CodexFingerprintProfile != nil {
		value.CodexFingerprintProfile = *state.CodexFingerprintProfile
	}
	if state.BuiltinPromptEnabled != nil {
		value.BuiltinPromptEnabled = *state.BuiltinPromptEnabled
	}
	if state.BuiltinPrompt != nil {
		value.BuiltinPrompt = *state.BuiltinPrompt
	}
	if state.StickySessionsEnabled != nil {
		value.StickySessionsEnabled = *state.StickySessionsEnabled
	}
	if state.StickySessionTTLSeconds != nil {
		value.StickySessionTTLSeconds = *state.StickySessionTTLSeconds
	}
	if state.PromptCacheKeyEnabled != nil {
		value.PromptCacheKeyEnabled = *state.PromptCacheKeyEnabled
	}
	if state.CacheCreationAsReadEnabled != nil {
		value.CacheCreationAsReadEnabled = *state.CacheCreationAsReadEnabled
	}
	if state.CacheReadRatioEnabled != nil {
		value.CacheReadRatioEnabled = *state.CacheReadRatioEnabled
	}
	if state.CacheReadRatioPercent != nil {
		value.CacheReadRatioPercent = *state.CacheReadRatioPercent
	}
	if state.BPSPromptCacheScope != nil {
		value.BPSPromptCacheScope = *state.BPSPromptCacheScope
	}
	if state.BPSModelRoutes != nil {
		value.BPSModelRoutes = *state.BPSModelRoutes
	}
	if state.RateLimitCooldownEnabled != nil {
		value.RateLimitCooldownEnabled = *state.RateLimitCooldownEnabled
	}
	if state.HTTPStreamEnabled != nil {
		value.HTTPStreamEnabled = *state.HTTPStreamEnabled
	}
	if state.HTTPJSONEnabled != nil {
		value.HTTPJSONEnabled = *state.HTTPJSONEnabled
	}
	if state.CompactEnabled != nil {
		value.CompactEnabled = *state.CompactEnabled
	}
	if state.ModelsEnabled != nil {
		value.ModelsEnabled = *state.ModelsEnabled
	}
	if state.CompatibilityNormalizationEnabled != nil {
		value.CompatibilityNormalizationEnabled = *state.CompatibilityNormalizationEnabled
	}
	if state.AutomaticTokenRefreshEnabled != nil {
		value.AutomaticTokenRefreshEnabled = *state.AutomaticTokenRefreshEnabled
	}
	if state.Upstream401RecoveryEnabled != nil {
		value.Upstream401RecoveryEnabled = *state.Upstream401RecoveryEnabled
	}
	if state.BuiltinPromptMode != nil {
		value.BuiltinPromptMode = *state.BuiltinPromptMode
	}
	if state.UpstreamStreamIdleTimeoutSeconds != nil {
		value.UpstreamStreamIdleTimeoutSeconds = *state.UpstreamStreamIdleTimeoutSeconds
	}
	if state.UpstreamResponseHeaderTimeoutSeconds != nil {
		value.UpstreamResponseHeaderTimeoutSeconds = *state.UpstreamResponseHeaderTimeoutSeconds
	}
	if state.UpstreamCompactTimeoutSeconds != nil {
		value.UpstreamCompactTimeoutSeconds = *state.UpstreamCompactTimeoutSeconds
	}
	return value
}

// Missing legacy fields receive defaults; explicitly persisted invalid fields
// must never become runtime deadlines or policy. In particular, converting an
// unchecked number of seconds to time.Duration can overflow into no timeout.
func (state settingsState) validatedSettings() (Settings, error) {
	if state.Version < 0 {
		return Settings{}, fmt.Errorf("%w: stored settings version must not be negative", errInvalid)
	}
	value := state.settings()
	if err := validateSettings(value); err != nil {
		return Settings{}, err
	}
	return value, nil
}

func (settings Settings) state() settingsState {
	return settingsState{
		Version: settings.Version, AdminAPI: settings.AdminAPI,
		InferenceMode:           &settings.InferenceMode,
		WebsocketEnabled:        &settings.WebsocketEnabled,
		PreferWebsocket:         &settings.PreferWebsocket,
		BPSWebSocketReuse:       &settings.BPSWebSocketReuse,
		CodexFingerprintEnabled: &settings.CodexFingerprintEnabled,
		CodexFingerprintProfile: &settings.CodexFingerprintProfile,
		BuiltinPromptEnabled:    &settings.BuiltinPromptEnabled, BuiltinPrompt: &settings.BuiltinPrompt,
		StickySessionsEnabled:                &settings.StickySessionsEnabled,
		StickySessionTTLSeconds:              &settings.StickySessionTTLSeconds,
		PromptCacheKeyEnabled:                &settings.PromptCacheKeyEnabled,
		CacheCreationAsReadEnabled:           &settings.CacheCreationAsReadEnabled,
		CacheReadRatioEnabled:                &settings.CacheReadRatioEnabled,
		CacheReadRatioPercent:                &settings.CacheReadRatioPercent,
		BPSPromptCacheScope:                  &settings.BPSPromptCacheScope,
		BPSModelRoutes:                       &settings.BPSModelRoutes,
		RateLimitCooldownEnabled:             &settings.RateLimitCooldownEnabled,
		HTTPStreamEnabled:                    &settings.HTTPStreamEnabled,
		HTTPJSONEnabled:                      &settings.HTTPJSONEnabled,
		CompactEnabled:                       &settings.CompactEnabled,
		ModelsEnabled:                        &settings.ModelsEnabled,
		CompatibilityNormalizationEnabled:    &settings.CompatibilityNormalizationEnabled,
		AutomaticTokenRefreshEnabled:         &settings.AutomaticTokenRefreshEnabled,
		Upstream401RecoveryEnabled:           &settings.Upstream401RecoveryEnabled,
		BuiltinPromptMode:                    &settings.BuiltinPromptMode,
		UpstreamStreamIdleTimeoutSeconds:     &settings.UpstreamStreamIdleTimeoutSeconds,
		UpstreamResponseHeaderTimeoutSeconds: &settings.UpstreamResponseHeaderTimeoutSeconds,
		UpstreamCompactTimeoutSeconds:        &settings.UpstreamCompactTimeoutSeconds,
	}
}

func NewSettingsStore(path string) *SettingsStore {
	return &SettingsStore{store: newFileStore[settingsState](path)}
}

// Get returns the cached settings, loading the file on first use. Admin API
// calls update the cache through Set, and the gateway reads it per request,
// so file stores are never read on the hot path. PG settings expire after two
// seconds so another gateway process cannot retain stale toggles indefinitely.
func (s *SettingsStore) Get() (Settings, error) {
	s.mu.RLock()
	if s.loaded && (s.store.db == nil || time.Since(s.loadedAt) < settingsCacheTTL) {
		value := s.cached
		s.mu.RUnlock()
		return value, nil
	}
	s.mu.RUnlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loaded && (s.store.db == nil || time.Since(s.loadedAt) < settingsCacheTTL) {
		return s.cached, nil
	}
	state, err := s.store.Read()
	if err != nil {
		return Settings{}, err
	}
	if s.store.db == nil {
		if _, statErr := os.Stat(s.store.path); errors.Is(statErr, os.ErrNotExist) {
			state.AdminAPI = true
		}
	}
	value, err := state.validatedSettings()
	if err != nil {
		return Settings{}, err
	}
	s.cached = value
	s.loaded = true
	s.loadedAt = time.Now()
	return s.cached, nil
}

func (s *SettingsStore) Set(settings Settings) error {
	defaults := defaultSettings()
	if settings.InferenceMode == "" {
		settings.InferenceMode = defaults.InferenceMode
	}
	if settings.BPSPromptCacheScope == "" {
		settings.BPSPromptCacheScope = defaults.BPSPromptCacheScope
	}
	if settings.BPSModelRoutes == "" {
		settings.BPSModelRoutes = defaults.BPSModelRoutes
	}
	if settings.BuiltinPromptMode == "" {
		settings.BuiltinPromptMode = defaults.BuiltinPromptMode
	}
	if settings.UpstreamStreamIdleTimeoutSeconds == 0 {
		settings.UpstreamStreamIdleTimeoutSeconds = defaults.UpstreamStreamIdleTimeoutSeconds
	}
	if settings.UpstreamResponseHeaderTimeoutSeconds == 0 {
		settings.UpstreamResponseHeaderTimeoutSeconds = defaults.UpstreamResponseHeaderTimeoutSeconds
	}
	if settings.UpstreamCompactTimeoutSeconds == 0 {
		settings.UpstreamCompactTimeoutSeconds = defaults.UpstreamCompactTimeoutSeconds
	}
	if settings.CodexFingerprintProfile == "" {
		settings.CodexFingerprintProfile = defaultCodexFingerprintProfile
	}
	// Older in-process callers only supplied AdminAPI. Zero TTL is not a valid
	// admin API update, but remains a compatibility default for those callers.
	if settings.StickySessionTTLSeconds == 0 {
		settings.StickySessionTTLSeconds = 604800
	}
	if err := validateSettings(settings); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.store.Modify(func(state *settingsState) error {
		if state.Version < 0 || state.Version == math.MaxInt64 {
			return fmt.Errorf("%w: stored settings version cannot be incremented", errInvalid)
		}
		if state.Version < 1 {
			state.Version = 1
		}
		if settings.Version > 0 && settings.Version != state.Version {
			return errVersionConflict
		}
		next := settings.state()
		next.Version = state.Version
		if reflect.DeepEqual(*state, next) {
			// A POST whose merged fields equal the stored settings rewrites
			// nothing: no document write, no settings_version bump.
			settings.Version = state.Version
			return errStoreNoChange
		}
		state.Version++
		next.Version = state.Version
		settings.Version = state.Version
		*state = next
		return nil
	})
	if err != nil {
		return err
	}
	s.cached = settings
	s.loaded = true
	s.loadedAt = time.Now()
	return nil
}

// Read only for an authenticated management update. Invalid persisted policy
// must be repairable, but it must never be published by Get/Latest or enter the
// runtime cache. Read the current document even in file mode, then let the
// handler validate the entire merged policy and Set enforce its version CAS.
func (s *SettingsStore) readForUpdate() (Settings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.store.Read()
	if err != nil {
		return Settings{}, err
	}
	if state.Version < 0 {
		return Settings{}, fmt.Errorf("%w: stored settings version must not be negative", errInvalid)
	}
	if s.store.db == nil {
		if _, statErr := os.Stat(s.store.path); errors.Is(statErr, os.ErrNotExist) {
			state.AdminAPI = true
		}
	}
	return state.settings(), nil
}

// Latest is used by management requests; gateway Get keeps its existing cached behavior.
func (s *SettingsStore) Latest() (Settings, error) {
	if s.store.db == nil {
		return s.Get()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.store.Read()
	if err != nil {
		return Settings{}, err
	}
	value, err := state.validatedSettings()
	if err != nil {
		return Settings{}, err
	}
	s.cached = value
	s.loaded = true
	s.loadedAt = time.Now()
	return value, nil
}
