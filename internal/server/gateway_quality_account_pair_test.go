package server

// Opt-in experimental server plus offline regressions. The live entry runs
// current production policy with one imported account and one ordinary local
// customer key. It never mutates production stores or resolves other routes.
import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

const qualityPairClientKey = "quality-pair-local-client-v1"
const qualityPairKeyID = "quality-account-pair-key"
const qualityPairGroup = "quality-account-pair-group"
const qualityPairAccount = "quality-account-pair"
const qualityPairHelperSHA = "9110b869e65db6bf7664e39fcfe3b2ec17fa7a0207ddbeccf28d73dbc12ccce8"

type qualityPairConfig struct {
	CredentialFile string   `json:"credential_file"`
	Output         string   `json:"output"`
	StateDir       string   `json:"state_dir,omitempty"`
	Settings       Settings `json:"settings"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty"`
}

type qualityPairSecret struct {
	AccessToken string `json:"access_token"`
	AccountID   string `json:"account_id"`
	Exp         int64  `json:"exp"`
}

func qualityPairInvalid() error { return errors.New("invalid private account pair fixture input") }

func qualityPairPrivateRead(path string, limit int64) ([]byte, error) {
	if !filepath.IsAbs(path) {
		return nil, qualityPairInvalid()
	}
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Size() <= 0 || before.Size() > limit {
		return nil, qualityPairInvalid()
	}
	if runtime.GOOS != "windows" {
		if before.Mode().Perm()&0077 != 0 {
			return nil, qualityPairInvalid()
		}
		// The stat representation is platform-specific; reflect keeps this test
		// file buildable on Windows while requiring the owner on Linux.
		info := reflect.ValueOf(before.Sys())
		if info.Kind() != reflect.Pointer || info.IsNil() {
			return nil, qualityPairInvalid()
		}
		uid := info.Elem().FieldByName("Uid")
		if !uid.IsValid() || !uid.CanUint() || uid.Uint() != uint64(os.Geteuid()) {
			return nil, qualityPairInvalid()
		}
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, qualityPairInvalid()
	}
	defer file.Close()
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) {
		return nil, qualityPairInvalid()
	}
	raw, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(raw)) > limit || len(raw) == 0 {
		return nil, qualityPairInvalid()
	}
	return raw, nil
}

// Reject duplicate members at every depth before struct parsing. This prevents
// credentials/settings with two different interpretations in adjacent tools.
func qualityPairUniqueJSON(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	nodes := 0
	var value func(int) error
	value = func(depth int) error {
		nodes++
		if depth > 64 || nodes > 65536 {
			return qualityPairInvalid()
		}
		token, err := decoder.Token()
		if err != nil {
			return qualityPairInvalid()
		}
		delim, container := token.(json.Delim)
		if !container {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for decoder.More() {
				token, err := decoder.Token()
				name, ok := token.(string)
				if err != nil || !ok || seen[name] {
					return qualityPairInvalid()
				}
				seen[name] = true
				if err := value(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for decoder.More() {
				if err := value(depth + 1); err != nil {
					return err
				}
			}
		default:
			return qualityPairInvalid()
		}
		end, err := decoder.Token()
		if err != nil || (delim == '{' && end != json.Delim('}')) || (delim == '[' && end != json.Delim(']')) {
			return qualityPairInvalid()
		}
		return nil
	}
	if value(0) != nil {
		return qualityPairInvalid()
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return qualityPairInvalid()
	}
	return nil
}

func qualityPairDecode(raw []byte, target any) error {
	if qualityPairUniqueJSON(raw) != nil {
		return qualityPairInvalid()
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil {
		return qualityPairInvalid()
	}
	return nil
}

func qualityPairLoadConfig(path string) (qualityPairConfig, error) {
	var config qualityPairConfig
	raw, err := qualityPairPrivateRead(path, 128<<10)
	if err != nil || qualityPairDecode(raw, &config) != nil || !filepath.IsAbs(config.CredentialFile) || !filepath.IsAbs(config.Output) {
		return config, qualityPairInvalid()
	}
	if config.TimeoutSeconds == 0 {
		config.TimeoutSeconds = 270
	}
	if config.TimeoutSeconds < 5 || config.TimeoutSeconds > 300 || validateSettings(config.Settings) != nil {
		return config, qualityPairInvalid()
	}
	if config.StateDir != "" && (!filepath.IsAbs(config.StateDir) || filepath.Clean(config.StateDir) == filepath.Clean(config.Output)) {
		return config, qualityPairInvalid()
	}
	var input map[string]json.RawMessage
	_ = json.Unmarshal(raw, &input)
	for name := range input {
		switch name {
		case "credential_file", "output", "state_dir", "settings", "timeout_seconds":
		default:
			return config, qualityPairInvalid()
		}
	}
	var supplied map[string]json.RawMessage
	if json.Unmarshal(input["settings"], &supplied) != nil {
		return config, qualityPairInvalid()
	}
	// No zero-value fallback may silently disable a production policy toggle.
	encoded, _ := json.Marshal(Settings{})
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(encoded, &fields)
	if len(supplied) != len(fields) {
		return config, qualityPairInvalid()
	}
	for name := range fields {
		if raw, present := supplied[name]; !present || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return config, qualityPairInvalid()
		}
	}
	return config, nil
}

func qualityPairLoadSecret(path string, minimumExpiry time.Time) (qualityPairSecret, string, error) {
	var secret qualityPairSecret
	raw, err := qualityPairPrivateRead(path, 64<<10)
	if err != nil || qualityPairDecode(raw, &secret) != nil || secret.Exp <= minimumExpiry.Unix() || len(secret.AccessToken) > 48<<10 || len(secret.AccountID) > 160 || secret.AccountID == "" {
		return secret, "", qualityPairInvalid()
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || len(fields) != 3 {
		return secret, "", qualityPairInvalid()
	}
	for _, name := range []string{"access_token", "account_id", "exp"} {
		if _, exists := fields[name]; !exists {
			return secret, "", qualityPairInvalid()
		}
	}
	for _, char := range secret.AccessToken {
		if char <= 32 || char > 126 {
			return secret, "", qualityPairInvalid()
		}
	}
	parts := strings.Split(secret.AccessToken, ".")
	if len(parts) != 3 || parts[0] == "" || parts[2] == "" {
		return secret, "", qualityPairInvalid()
	}
	claimsRaw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(claimsRaw) > 64<<10 || qualityPairUniqueJSON(claimsRaw) != nil {
		return secret, "", qualityPairInvalid()
	}
	var claims map[string]json.RawMessage
	if json.Unmarshal(claimsRaw, &claims) != nil {
		return secret, "", qualityPairInvalid()
	}
	var expiry int64
	if json.Unmarshal(claims["exp"], &expiry) != nil || expiry != secret.Exp {
		return secret, "", qualityPairInvalid()
	}
	var auth map[string]json.RawMessage
	if json.Unmarshal(claims["https://api.openai.com/auth"], &auth) != nil || auth == nil {
		return secret, "", qualityPairInvalid()
	}
	var workspace, user string
	if json.Unmarshal(auth["chatgpt_account_id"], &workspace) != nil || workspace != secret.AccountID {
		return secret, "", qualityPairInvalid()
	}
	userRaw, exists := auth["chatgpt_user_id"]
	if !exists || bytes.Equal(bytes.TrimSpace(userRaw), []byte("null")) {
		userRaw = auth["user_id"]
	}
	if json.Unmarshal(userRaw, &user) != nil || strings.TrimSpace(user) == "" || len(user) > 512 || strings.ContainsAny(user+workspace, "\r\n\x00") {
		return secret, "", qualityPairInvalid()
	}
	return secret, user, nil
}

func qualityPairAccountStore(dir string, secret qualityPairSecret, user string) (Store, Account, bool, error) {
	store := Store{Path: filepath.Join(dir, "accounts.json")}
	if !filepath.IsAbs(dir) {
		return store, Account{}, false, qualityPairInvalid()
	}
	if err := os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
		return store, Account{}, false, qualityPairInvalid()
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return store, Account{}, false, qualityPairInvalid()
	}
	for current := filepath.Clean(dir); ; current = filepath.Dir(current) {
		entry, err := os.Lstat(current)
		if err != nil || entry.Mode()&os.ModeSymlink != 0 {
			return store, Account{}, false, qualityPairInvalid()
		}
		if filepath.Dir(current) == current {
			break
		}
	}
	if runtime.GOOS != "windows" {
		stat := reflect.ValueOf(info.Sys())
		if info.Mode().Perm()&0077 != 0 || stat.Kind() != reflect.Pointer || stat.IsNil() {
			return store, Account{}, false, qualityPairInvalid()
		}
		uid := stat.Elem().FieldByName("Uid")
		if !uid.IsValid() || !uid.CanUint() || uid.Uint() != uint64(os.Geteuid()) {
			return store, Account{}, false, qualityPairInvalid()
		}
	}
	reused := false
	if _, err := os.Lstat(store.Path); err == nil {
		reused = true
		if _, err := qualityPairPrivateRead(store.Path, 128<<10); err != nil {
			return store, Account{}, false, qualityPairInvalid()
		}
	} else if !os.IsNotExist(err) {
		return store, Account{}, false, qualityPairInvalid()
	} else {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 0 {
			return store, Account{}, false, qualityPairInvalid()
		}
		credentials, _ := json.Marshal(map[string]any{"access_token": secret.AccessToken, "chatgpt_account_id": secret.AccountID, "expires_at": secret.Exp})
		if store.ImportWith(qualityPairAccount, credentials, AccountDefaults{Concurrency: 1, Groups: []string{qualityPairGroup}}) != nil {
			return store, Account{}, false, qualityPairInvalid()
		}
	}
	_, accounts, err := store.List()
	if err != nil || len(accounts) != 1 {
		return store, Account{}, reused, qualityPairInvalid()
	}
	account := accounts[0]
	if account.Name != qualityPairAccount || account.AccessToken != secret.AccessToken || account.AccountID != secret.AccountID || account.ExpiresAt != secret.Exp || account.RefreshToken != "" ||
		account.Identity == nil || account.Identity.UserID != user || account.Identity.WorkspaceID != secret.AccountID || !gatewayCompleteOwner(account) ||
		!account.Enabled || account.Concurrency != 1 || !reflect.DeepEqual(account.Groups, []string{qualityPairGroup}) || len(account.Proxies) != 0 {
		return store, Account{}, reused, qualityPairInvalid()
	}
	return store, account, reused, nil
}

func qualityPairNewProxy(t *testing.T, secret qualityPairSecret, user string, source Settings, persistent ...string) (*Proxy, Account, Settings) {
	t.Helper()
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "account-state")
	if len(persistent) > 0 && persistent[0] != "" {
		stateDir = persistent[0]
	}
	store, account, _, err := qualityPairAccountStore(stateDir, secret, user)
	if err != nil {
		t.Fatal("isolated account state does not match credential snapshot")
	}
	metrics, err := NewMetrics(filepath.Join(dir, "metrics"))
	if err != nil {
		t.Fatal("cannot create isolated metrics")
	}
	t.Cleanup(metrics.Close)
	p := &Proxy{Store: store, Groups: NewGroupStore(filepath.Join(dir, "groups.json")), Keys: NewKeyStore(filepath.Join(dir, "keys.json")),
		Proxies: NewProxyStore(filepath.Join(dir, "proxies.json")), Settings: NewSettingsStore(filepath.Join(dir, "settings.json")), Metrics: metrics,
		AdminKey: "quality-pair-unused-admin-v1", KeyFile: filepath.Join(dir, "admin-key"), BaseURL: codexURL,
		TokenURL: "http://127.0.0.1:9/refresh-disabled", Client: newHTTPClient(), startedAt: time.Now()}
	if err := p.Groups.store.Modify(func(state *groupState) error {
		state.Groups = []Group{{ID: qualityPairGroup, Name: qualityPairGroup, Version: 1}}
		return nil
	}); err != nil {
		t.Fatal("cannot create isolated group")
	}
	if err := p.Keys.store.Modify(func(state *keyState) error {
		state.Keys = []APIKey{{ID: qualityPairKeyID, Name: "isolated ordinary customer", Key: qualityPairClientKey, Groups: []string{qualityPairGroup}, Version: 1, Enabled: true}}
		return nil
	}); err != nil {
		t.Fatal("cannot create isolated customer key")
	}
	if _, err := p.Keys.List(); err != nil {
		t.Fatal("cannot read isolated key")
	}
	effective := source
	effective.Version = 0 // Only the isolated settings store has a fresh CAS epoch.
	effective.AutomaticTokenRefreshEnabled, effective.Upstream401RecoveryEnabled = false, false
	if err := p.Settings.Set(effective); err != nil {
		t.Fatal("cannot install production policy snapshot")
	}
	effective, err = p.Settings.Get()
	if err != nil {
		t.Fatal("cannot read installed policy")
	}
	t.Cleanup(p.closeGatewayClients)
	return p, account, effective
}

func qualityPairSettingsReport(value Settings) map[string]any {
	raw, _ := json.Marshal(value)
	var report map[string]any
	_ = json.Unmarshal(raw, &report)
	delete(report, "builtin_prompt")
	report["builtin_prompt_sha256"], report["builtin_prompt_bytes"] = qualityDigest([]byte(value.BuiltinPrompt)), len(value.BuiltinPrompt)
	return report
}

func qualityPairHeaderHashes(header http.Header) []map[string]any {
	names := make([]string, 0, len(header))
	for name := range header {
		names = append(names, name)
	}
	sort.Strings(names)
	var result []map[string]any
	for _, name := range names {
		for _, value := range header[name] {
			result = append(result, map[string]any{"name_sha256": qualityDigest([]byte(strings.ToLower(name))), "original_name_sha256": qualityDigest([]byte(name)), "value_sha256": qualityDigest([]byte(value)), "value_bytes": len(value)})
		}
	}
	return result
}

type qualityPairGate struct {
	proxy                                       *Proxy
	ctx                                         context.Context
	mu                                          sync.Mutex
	posts, refused, blockedRoutes, unauthorized int
	ingress                                     []map[string]any
	session                                     string
}

func (gate *qualityPairGate) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost || request.URL.Path != "/v1/responses" || request.URL.RawQuery != "" || request.Header.Get("Upgrade") != "" {
		gate.mu.Lock()
		gate.blockedRoutes++
		gate.mu.Unlock()
		http.Error(w, "isolated experiment route unavailable", http.StatusNotFound)
		return
	}
	if len(request.Header.Values("Authorization")) != 1 || subtle.ConstantTimeCompare([]byte(bearerToken(request)), []byte(qualityPairClientKey)) != 1 || request.Header.Get("ChatGPT-Account-Id") != "" {
		gate.mu.Lock()
		gate.unauthorized++
		gate.mu.Unlock()
		http.Error(w, "isolated customer authorization required", http.StatusUnauthorized)
		return
	}
	gate.mu.Lock()
	if gate.posts != 0 {
		gate.refused++
		gate.mu.Unlock()
		http.Error(w, "single request experiment exhausted", http.StatusConflict)
		return
	}
	gate.posts++
	gate.mu.Unlock()
	ctx, cancel := context.WithCancel(request.Context())
	stop := context.AfterFunc(gate.ctx, cancel)
	defer func() { stop(); cancel() }()
	request = request.WithContext(ctx)
	wire, err := io.ReadAll(io.LimitReader(request.Body, maxCompressedBody+1))
	_ = request.Body.Close()
	if err != nil || len(wire) > maxCompressedBody {
		http.Error(w, "request wire limit", http.StatusRequestEntityTooLarge)
		return
	}
	clone := request.Clone(ctx)
	clone.Body = io.NopCloser(bytes.NewReader(wire))
	body, err := readBody(httptest.NewRecorder(), clone)
	if err != nil {
		http.Error(w, "request decoding failed", http.StatusBadRequest)
		return
	}
	session := gatewaySessionKey(request, body)
	bound, err := gate.proxy.sessionAccount(ctx, qualityPairKeyID, session, "")
	if err != nil || bound != "" {
		http.Error(w, "unexpected existing experiment binding", http.StatusConflict)
		return
	}
	record := map[string]any{"wire_bytes": len(wire), "wire_sha256": qualityDigest(wire), "decoded_bytes": len(body), "decoded_sha256": qualityDigest(body),
		"header_hashes": qualityPairHeaderHashes(request.Header), "customer_token_sha256": qualityDigest([]byte(bearerToken(request))),
		"session_present": session != "", "session_sha256": qualityDigest([]byte(session)), "binding_before_present": false,
		"boundary": "HTTP entity decoded by production readBody before policy/preparation; headers parsed by Go HTTP"}
	gate.mu.Lock()
	gate.ingress = append(gate.ingress, record)
	gate.session = session
	gate.mu.Unlock()
	request.Body = io.NopCloser(bytes.NewReader(wire))
	gate.proxy.ServeHTTP(w, request)
}

func (gate *qualityPairGate) report(account Account, source, effective Settings) map[string]any {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	identity := map[string]any{"workspace_sha256": qualityDigest([]byte(account.AccountID)), "user_sha256": qualityDigest([]byte(account.Identity.UserID)),
		"identity_sha256": qualityDigest([]byte(account.AccountID + ":" + account.Identity.UserID)), "identity_hash_basis": "UTF8(workspace_id + ':' + user_id)",
		"token_sha256": qualityDigest([]byte(account.AccessToken)), "local_record_id_sha256": qualityDigest([]byte(account.ID)),
		"owner_ref_sha256": qualityDigest([]byte(gatewayAccountRef(account))), "owner_revision_sha256": qualityDigest([]byte(account.OwnerRevision)), "source": account.Identity.Source, "verified": account.Identity.Verified,
		"identity_loaded_by_store_import": true, "local_namespace": "isolated imported record/owner revision retained in optional state_dir; fixed synthetic ordinary API key ID"}
	var outcomes []map[string]any
	upstreamAttempts := 0
	for _, record := range gate.proxy.Metrics.RecentForAccount(account.Name, 8) {
		for _, attempt := range record.Attempts {
			if attempt.Phase == "request" {
				upstreamAttempts++
			}
		}
		outcomes = append(outcomes, map[string]any{"status": record.Status, "http_code": record.HTTPCode, "request_type": record.RequestType,
			"sent_model_sha256": qualityDigest([]byte(record.SentModel)), "returned_model_sha256": qualityDigest([]byte(record.ReturnedModel)),
			"terminal_status": record.TerminalStatus, "attempts": record.Attempts, "key_id_matches": record.KeyID == qualityPairKeyID,
			"account_record_matches": record.Account == account.Name, "error_source": record.ErrorSource, "error_phase": record.ErrorPhase})
	}
	bound, err := gate.proxy.sessionAccount(context.Background(), qualityPairKeyID, gate.session, "")
	oldRaw, _ := json.Marshal(source)
	newRaw, _ := json.Marshal(effective)
	return map[string]any{"schema": "codex-quality-account-pair-server-v1", "inference_requests": gate.posts, "extra_requests_refused": gate.refused,
		"other_routes_refused": gate.blockedRoutes, "unauthorized_requests": gate.unauthorized, "ingress": gate.ingress, "outcomes": outcomes,
		"upstream_request_attempts": upstreamAttempts, "upstream_request_limit_respected": upstreamAttempts <= 1, "account_identity": identity,
		"source_settings_sha256": qualityDigest(oldRaw), "effective_settings_sha256": qualityDigest(newRaw), "source_settings": qualityPairSettingsReport(source),
		"effective_settings": qualityPairSettingsReport(effective), "semantic_overrides": []string{"automatic_token_refresh_enabled=false", "upstream_401_recovery_enabled=false"},
		"local_settings_version_isolated": true, "binding_after_present": bound != "", "binding_after_matches_imported_owner": err == nil && bound == gatewayAccountRef(account),
		"remaining_account_inflight": gate.proxy.inFlight(account.ID), "public_catalog_enabled": false, "customer_key_id": qualityPairKeyID}
}

func qualityPairWrite(path string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(raw)
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

func qualityPairFileSHA(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 512<<20 {
		return ""
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, io.LimitReader(file, 512<<20+1)); err != nil {
		return ""
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func TestQualityAccountPairGateway(t *testing.T) {
	path := os.Getenv("CODEX_QUALITY_ACCOUNT_PAIR_CONFIG")
	if path == "" {
		t.Skip("explicit private account pair configuration required")
	}
	if runtime.GOOS != "linux" {
		t.Fatal("Linux native account pair fixture required")
	}
	config, err := qualityPairLoadConfig(path)
	if err != nil {
		t.Fatal("invalid private account pair configuration")
	}
	secret, user, err := qualityPairLoadSecret(config.CredentialFile, time.Now().Add(time.Duration(config.TimeoutSeconds+60)*time.Second))
	if err != nil {
		t.Fatal("invalid private account snapshot")
	}
	if os.Getenv(gatewayRuntimeIdentityEnv) == "" || configureGatewayRuntimeIdentity(os.Getenv) != nil || configuredGatewayRuntimeIdentity.Load() == nil {
		t.Fatal("verified runtime CLI identity required")
	}
	if config.Settings.CodexFingerprintEnabled {
		if _, err := effectiveCodexFingerprintProfile(config.Settings.CodexFingerprintProfile); err != nil {
			t.Fatal("production fingerprint profile differs from runtime")
		}
	}
	if err := os.Mkdir(config.Output, 0700); err != nil {
		t.Fatal("account pair output must be a new private directory")
	}
	helper := os.Getenv("CODEX_NATIVE_HTTP_TEST_BIN")
	if !filepath.IsAbs(helper) || qualityPairFileSHA(helper) != qualityPairHelperSHA {
		t.Fatal("pinned real native helper required")
	}
	launcher, launcherSHA := os.Getenv("CODEX_QUALITY_PAIR_HELPER_LAUNCHER"), os.Getenv("CODEX_QUALITY_PAIR_HELPER_LAUNCHER_SHA256")
	if (launcher == "") != (launcherSHA == "") || launcher != "" && (!filepath.IsAbs(launcher) || qualityPairFileSHA(launcher) != launcherSHA) {
		t.Fatal("invalid pinned native capture launcher")
	}
	stateReused := false
	if config.StateDir != "" {
		_, statErr := os.Lstat(filepath.Join(config.StateDir, "accounts.json"))
		stateReused = statErr == nil
	}
	p, account, effective := qualityPairNewProxy(t, secret, user, config.Settings, config.StateDir)
	options := gatewayNativeOptions{network: "unix", lifecycle: "cli_request"}
	if launcher != "" {
		options.command = func(string) *exec.Cmd {
			if qualityPairFileSHA(launcher) != launcherSHA {
				return exec.Command("/__quality_pair_launcher_changed__")
			}
			return exec.Command(launcher)
		}
	}
	pool, err := newGatewayNativePoolWithOptions(helper, qualityPairHelperSHA, options)
	if err != nil {
		t.Fatal("cannot create pinned helper pool")
	}
	p.NativeHTTP = pool
	ctx, cancel := context.WithTimeout(t.Context(), time.Duration(config.TimeoutSeconds)*time.Second)
	defer cancel()
	if err := pool.validateStartup(ctx, false); err != nil {
		t.Fatal("native helper startup verification failed")
	}
	gate := &qualityPairGate{proxy: p, ctx: ctx}
	server := httptest.NewUnstartedServer(gate)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal("cannot start loopback listener")
	}
	_ = server.Listener.Close()
	server.Listener = listener
	server.Start()
	defer server.Close()
	executable, _ := os.Executable()
	ready := map[string]any{"base_url": server.URL + "/v1", "responses_url": server.URL + "/v1/responses", "model_request_limit": 1,
		"catalog_requests_enabled": false, "customer_key_sha256": qualityDigest([]byte(qualityPairClientKey)), "customer_key_id": qualityPairKeyID,
		"harness_executable_sha256": qualityPairFileSHA(executable), "native_helper_sha256": qualityPairHelperSHA,
		"native_launcher_sha256": launcherSHA, "runtime_identity_source_cli_sha256": configuredGatewayRuntimeIdentity.Load().SourceCLISHA256,
		"identity_sha256": qualityDigest([]byte(account.AccountID + ":" + account.Identity.UserID))}
	if qualityPairWrite(filepath.Join(config.Output, "ready.json"), ready) != nil {
		t.Fatal("cannot publish experiment readiness")
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	stopReason := "deadline"
wait:
	for {
		select {
		case <-ctx.Done():
			break wait
		case <-ticker.C:
			if _, err := os.Lstat(filepath.Join(config.Output, "stop")); err == nil {
				stopReason = "stop_file"
				break wait
			}
		}
	}
	cancel()
	server.CloseClientConnections()
	server.Close()
	pool.mu.Lock()
	children := make([]*gatewayNativeProcess, 0, len(pool.entries))
	for _, child := range pool.entries {
		children = append(children, child)
	}
	pool.mu.Unlock()
	p.closeGatewayClients()
	report := gate.report(account, config.Settings, effective)
	report["stop_reason"] = stopReason
	report["native_helper_sha256"] = qualityPairHelperSHA
	report["native_launcher_sha256"] = launcherSHA
	report["account_state_persistent"] = config.StateDir != ""
	report["account_state_reused"] = stateReused
	report["account_state_cleanup_owner"] = "caller removes bounded private state_dir after the complete batch; sessions/metrics/helper are per process"
	report["startup_validation"] = "disposable helper with local configuration only; zero IPC requests and zero upstream network requests"
	var cleanup []map[string]any
	for _, child := range children {
		_, err := os.Lstat(child.dir)
		cleanup = append(cleanup, map[string]any{"helper_reaped": child.command != nil && child.command.ProcessState != nil, "socket_directory_absent": os.IsNotExist(err), "helper_scope_sha256": hex.EncodeToString(child.key.accountScope[:])})
	}
	report["helper_cleanup"] = cleanup
	if qualityPairWrite(filepath.Join(config.Output, "server-report.json"), report) != nil {
		t.Fatal("cannot publish experiment report")
	}
}

func qualityPairSyntheticCredential(t *testing.T, user string) (qualityPairSecret, string) {
	t.Helper()
	expiry := time.Now().Add(time.Hour).Unix()
	claims, _ := json.Marshal(map[string]any{"exp": expiry, "https://api.openai.com/auth": map[string]string{"chatgpt_account_id": "pair-workspace", "chatgpt_user_id": user}})
	token := "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".synthetic"
	secret := qualityPairSecret{AccessToken: token, AccountID: "pair-workspace", Exp: expiry}
	raw, _ := json.Marshal(secret)
	path := filepath.Join(t.TempDir(), "at.json")
	if os.WriteFile(path, raw, 0600) != nil {
		t.Fatal("fixture secret write")
	}
	return secret, path
}

func TestQualityAccountPairPolicyIdentityAndSingleDispatch(t *testing.T) {
	for _, compressed := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "zstd"}[compressed], func(t *testing.T) {
			secret, path := qualityPairSyntheticCredential(t, "actual-pair-user")
			loaded, user, err := qualityPairLoadSecret(path, time.Now().Add(10*time.Minute))
			if err != nil || loaded.AccessToken != secret.AccessToken {
				t.Fatal("credential loader failed")
			}
			settings := defaultSettings()
			settings.BuiltinPrompt = "private production prefix"
			settings.BuiltinPromptMode = "prepend"
			settings.WebsocketEnabled = true
			p, account, effective := qualityPairNewProxy(t, secret, user, settings)
			var calls atomic.Int32
			captured := make(chan []byte, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != "POST" || r.URL.Path != "/responses" || r.Header.Get("Authorization") != "Bearer "+secret.AccessToken || r.Header.Get("ChatGPT-Account-Id") != secret.AccountID {
					t.Error("wrong upstream route or owner")
				}
				body, _ := io.ReadAll(r.Body)
				if r.Header.Get("Content-Encoding") == "zstd" {
					decoder, _ := zstd.NewReader(nil)
					body, _ = decoder.DecodeAll(body, nil)
					decoder.Close()
				}
				captured <- body
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"pair-fixture\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":2,\"output_tokens\":1}}}\n\n")
			}))
			defer upstream.Close()
			p.BaseURL = upstream.URL
			p.Client = upstream.Client()
			gate := &qualityPairGate{proxy: p, ctx: t.Context()}
			body := []byte(`{"model":"fixture","stream":true,"store":false,"instructions":"client instructions","prompt_cache_key":"pair-session","client_metadata":{"session_id":"pair-session"},"input":[{"role":"user","content":"original task"}],"reasoning":{"effort":"medium"}}`)
			wire := body
			if compressed {
				encoder, _ := zstd.NewWriter(nil)
				wire = encoder.EncodeAll(body, nil)
				encoder.Close()
			}
			send := func(key string) *httptest.ResponseRecorder {
				request := httptest.NewRequest("POST", "/v1/responses", bytes.NewReader(wire))
				request.Header.Set("Authorization", "Bearer "+key)
				request.Header.Set("Session-Id", "pair-session")
				if compressed {
					request.Header.Set("Content-Encoding", "zstd")
				}
				response := httptest.NewRecorder()
				gate.ServeHTTP(response, request)
				return response
			}
			if send(secret.AccessToken).Code != 401 {
				t.Fatal("real upstream token accepted as local customer key")
			}
			blocked := httptest.NewRecorder()
			gate.ServeHTTP(blocked, httptest.NewRequest("GET", "/v1/models", nil))
			if blocked.Code != 404 || calls.Load() != 0 {
				t.Fatal("catalog request reached upstream")
			}
			if response := send(qualityPairClientKey); response.Code != 200 {
				t.Fatalf("authorized pair failed: %d", response.Code)
			}
			if send(qualityPairClientKey).Code != 409 || calls.Load() != 1 {
				t.Fatal("second inference sent or wrong limit")
			}
			prepared := <-captured
			if policyString(t, prepared, "instructions") != "private production prefix\n\nclient instructions" {
				t.Fatal("production prompt policy not applied")
			}
			if key := policyString(t, prepared, "prompt_cache_key"); len(key) != 36 || key != gatewayScopeCacheIdentityValue(account, qualityPairKeyID, "session", "pair-session") {
				t.Fatal("production account cache policy disabled")
			}
			if account.Identity.UserID != "actual-pair-user" || account.Identity.UserID == "fixture-user" {
				t.Fatal("fixture owner polluted imported identity")
			}
			want := settings
			want.Version = effective.Version
			want.AutomaticTokenRefreshEnabled = false
			want.Upstream401RecoveryEnabled = false
			if !reflect.DeepEqual(effective, want) {
				t.Fatal("unexpected production policy override")
			}
			report := gate.report(account, settings, effective)
			if report["upstream_request_attempts"] != 1 || report["binding_after_matches_imported_owner"] != true || report["remaining_account_inflight"] != int64(0) {
				t.Fatalf("request accounting or binding mismatch: attempts=%v binding=%v inflight=%v outcomes=%v", report["upstream_request_attempts"], report["binding_after_matches_imported_owner"], report["remaining_account_inflight"], report["outcomes"])
			}
			if got := gate.ingress[0]["decoded_sha256"]; got != qualityDigest(body) {
				t.Fatal("ingress hash was taken after gateway transformation")
			}
			encoded, _ := json.Marshal(report)
			for _, hidden := range []string{secret.AccessToken, secret.AccountID, user, "private production prefix", "original task"} {
				if bytes.Contains(encoded, []byte(hidden)) {
					t.Fatal("report contains private input")
				}
			}
		})
	}
}

func TestQualityAccountPairNoOAuthRetryOn401(t *testing.T) {
	secret, _ := qualityPairSyntheticCredential(t, "same-owner")
	p, _, effective := qualityPairNewProxy(t, secret, "same-owner", defaultSettings())
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/responses" {
			t.Error("OAuth route reached")
		}
		w.WriteHeader(401)
	}))
	defer server.Close()
	p.Client = server.Client()
	p.BaseURL = server.URL
	p.TokenURL = server.URL + "/oauth/token"
	gate := qualityPairGate{proxy: p, ctx: t.Context()}
	request := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"fixture","stream":true,"input":[]}`))
	request.Header.Set("Authorization", "Bearer "+qualityPairClientKey)
	response := httptest.NewRecorder()
	gate.ServeHTTP(response, request)
	if response.Code != 401 || calls.Load() != 1 || effective.Upstream401RecoveryEnabled || effective.AutomaticTokenRefreshEnabled {
		t.Fatal("single request recovered or retried")
	}
}

func TestQualityAccountPairPersistentNamespaceFreshRequestState(t *testing.T) {
	secret, _ := qualityPairSyntheticCredential(t, "persistent-user")
	state := filepath.Join(t.TempDir(), "account-only")
	p1, a1, _ := qualityPairNewProxy(t, secret, "persistent-user", defaultSettings(), state)
	if _, err := p1.sessionAccount(t.Context(), qualityPairKeyID, "old-session", gatewayAccountRef(a1)); err != nil {
		t.Fatal("cannot seed prior fixture session")
	}
	p2, a2, _ := qualityPairNewProxy(t, secret, "persistent-user", defaultSettings(), state)
	if a1.ID != a2.ID || a1.OwnerRevision != a2.OwnerRevision || gatewayAccountRef(a1) != gatewayAccountRef(a2) {
		t.Fatal("account namespace changed across same-alias cells")
	}
	if binding, err := p2.sessionAccount(t.Context(), qualityPairKeyID, "old-session", ""); err != nil || binding != "" {
		t.Fatal("session binding leaked across fixture process")
	}
	if p1.Settings.store.path == p2.Settings.store.path || p1.Keys.store.path == p2.Keys.store.path || p1.Metrics == p2.Metrics {
		t.Fatal("non-account runtime state was reused")
	}
	raw := []byte(`{"prompt_cache_key":"session","client_metadata":{"session_id":"session"}}`)
	first, err := prepareGatewayAccountPromptCache(raw, qualityPairKeyID, a1, defaultSettings())
	if err != nil {
		t.Fatal("first cache mapping failed")
	}
	second, err := prepareGatewayAccountPromptCache(raw, qualityPairKeyID, a2, defaultSettings())
	if err != nil || !bytes.Equal(first, second) {
		t.Fatal("same account cells did not keep the same mapped cache namespace")
	}
	if _, _, reused, err := qualityPairAccountStore(state, secret, "persistent-user"); err != nil || !reused {
		t.Fatal("second account-store access did not verify persisted namespace")
	}
	for _, change := range []string{"token", "user", "workspace", "expiry"} {
		next := secret
		user := "persistent-user"
		switch change {
		case "token":
			next.AccessToken += "x"
		case "user":
			user = "other"
		case "workspace":
			next.AccountID = "other"
		case "expiry":
			next.Exp++
		}
		if _, _, _, err := qualityPairAccountStore(state, next, user); err == nil {
			t.Fatal("changed credential epoch reused prior namespace")
		}
	}
	if runtime.GOOS != "windows" {
		_ = os.Chmod(state, 0755)
		if _, _, _, err := qualityPairAccountStore(state, secret, "persistent-user"); err == nil {
			t.Fatal("public persistent state accepted")
		}
		_ = os.Chmod(state, 0700)
	}
}

func TestQualityAccountPairRejectsUnsupportedRoutesAndConcurrentSecondPOST(t *testing.T) {
	secret, _ := qualityPairSyntheticCredential(t, "single-dispatch-user")
	p, _, _ := qualityPairNewProxy(t, secret, "single-dispatch-user", defaultSettings())
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		close(entered)
		<-release
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"concurrent\",\"status\":\"completed\",\"output\":[]}}\n\n")
	}))
	defer upstream.Close()
	p.BaseURL = upstream.URL
	p.Client = upstream.Client()
	gate := qualityPairGate{proxy: p, ctx: t.Context()}
	for _, route := range []struct{ method, path string }{{"GET", "/v1/models"}, {"POST", "/v1/responses/compact"}, {"POST", "/responses"}, {"POST", "/v1/responses?extra=1"}, {"GET", "/health"}} {
		request := httptest.NewRequest(route.method, route.path, nil)
		request.Header.Set("Authorization", "Bearer "+qualityPairClientKey)
		response := httptest.NewRecorder()
		gate.ServeHTTP(response, request)
		if response.Code != 404 || calls.Load() != 0 {
			t.Fatal("unsupported public route reached account")
		}
	}
	request := func() *http.Request {
		r := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"fixture","stream":true,"input":[]}`))
		r.Header.Set("Authorization", "Bearer "+qualityPairClientKey)
		return r
	}
	done := make(chan int, 1)
	go func() { w := httptest.NewRecorder(); gate.ServeHTTP(w, request()); done <- w.Code }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("first request did not arrive")
	}
	second := httptest.NewRecorder()
	gate.ServeHTTP(second, request())
	close(release)
	if second.Code != 409 || <-done != 200 || calls.Load() != 1 {
		t.Fatal("concurrent second POST bypassed single dispatch gate")
	}
}

func TestQualityAccountPairRejectsUnsafeCredentialAndConfig(t *testing.T) {
	secret, path := qualityPairSyntheticCredential(t, "loaded-user")
	raw, _ := os.ReadFile(path)
	for _, bad := range [][]byte{[]byte(strings.TrimSuffix(string(raw), "}") + `,"refresh_token":"private-refresh"}`), []byte(strings.TrimSuffix(string(raw), "}") + `,"id_token":"private-id"}`), []byte(strings.TrimSuffix(string(raw), "}") + `,"account_id":"other"}`), append(raw, []byte(`{}`)...), bytes.Repeat([]byte("x"), 64<<10+1)} {
		candidate := filepath.Join(t.TempDir(), "bad.json")
		_ = os.WriteFile(candidate, bad, 0600)
		if _, _, err := qualityPairLoadSecret(candidate, time.Now()); err == nil {
			t.Fatal("invalid secret accepted")
		}
	}
	for _, change := range []string{"workspace", "expiry", "opaque"} {
		copy := secret
		switch change {
		case "workspace":
			copy.AccountID = "other"
		case "expiry":
			copy.Exp++
		case "opaque":
			copy.AccessToken = "opaque"
		}
		data, _ := json.Marshal(copy)
		candidate := filepath.Join(t.TempDir(), "bad.json")
		_ = os.WriteFile(candidate, data, 0600)
		if _, _, err := qualityPairLoadSecret(candidate, time.Now()); err == nil {
			t.Fatal("mismatched token snapshot accepted")
		}
	}
	if _, _, err := qualityPairLoadSecret(path, time.Now().Add(2*time.Hour)); err == nil {
		t.Fatal("near expiry accepted")
	}
	config := qualityPairConfig{CredentialFile: path, Output: filepath.Join(t.TempDir(), "new-output"), Settings: defaultSettings(), TimeoutSeconds: 270}
	data, _ := json.Marshal(config)
	configPath := filepath.Join(t.TempDir(), "config.json")
	_ = os.WriteFile(configPath, data, 0600)
	if _, err := qualityPairLoadConfig(configPath); err != nil {
		t.Fatal("complete config rejected")
	}
	var top map[string]any
	_ = json.Unmarshal(data, &top)
	delete(top["settings"].(map[string]any), "prompt_cache_key_enabled")
	data, _ = json.Marshal(top)
	_ = os.WriteFile(configPath, data, 0600)
	if _, err := qualityPairLoadConfig(configPath); err == nil {
		t.Fatal("partial settings silently defaulted")
	}
	if _, err := qualityPairPrivateRead(filepath.Dir(path), 64<<10); err == nil {
		t.Fatal("directory read accepted")
	}
	if runtime.GOOS != "windows" {
		_ = os.Chmod(path, 0644)
		if _, err := qualityPairPrivateRead(path, 64<<10); err == nil {
			t.Fatal("public secret permissions accepted")
		}
		_ = os.Chmod(path, 0600)
		link := filepath.Join(t.TempDir(), "symlink")
		if os.Symlink(path, link) != nil {
			t.Fatal("symlink fixture")
		}
		if _, err := qualityPairPrivateRead(link, 64<<10); err == nil {
			t.Fatal("credential symlink accepted")
		}
	}
}
