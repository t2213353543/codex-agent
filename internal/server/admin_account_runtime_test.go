package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestManagedAccountRuntimeReadOnly(t *testing.T) {
	runManagedAccountRuntimeReadOnly(t, webTestSetup)
}
func TestPGManagedAccountRuntimeReadOnly(t *testing.T) {
	runManagedAccountRuntimeReadOnly(t, adminPGTestProxy)
}
func runManagedAccountRuntimeReadOnly(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	p, h := setup(t)
	a := managedTestAccount(t, p, "runtime-observation", func(a *Account) { a.Concurrency = 1 })
	settings := defaultSettings()
	settings.WebsocketEnabled = true
	settings.CodexFingerprintEnabled = true
	settings.CodexFingerprintProfile = "macos"
	settings.CompatibilityNormalizationEnabled = false
	if err := p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	keyAccount := a
	if p.Store.DB == nil {
		keyAccount.CredentialVersion = 0
	}
	modelKey := gatewayCooldownKey(keyAccount, "model:synthetic-model")
	expiredKey := gatewayCooldownKey(keyAccount, "models")
	p.cooldowns = map[string]time.Time{modelKey: now.Add(30 * time.Second), expiredKey: now.Add(-time.Minute)}
	p.cooldownOverflowUntil = now.Add(-time.Minute)
	refreshKey := gatewayRefreshFailureKey(a, p.TokenURL)
	p.refreshCooldown.entries = map[string]gatewayRefreshFailure{refreshKey: {Kind: "http", StatusCode: 503, Until: now.Add(15 * time.Second).UnixMilli(), use: 8}, "expired": {Kind: "http", StatusCode: 429, Until: now.Add(-time.Minute).UnixMilli(), use: 2}}
	p.refreshCooldown.use = 8
	// This endpoint reports process observations only. Seed the local counter
	// directly; production PG admission additionally requires a Redis lease.
	admissionKey := gatewayAdmissionKey(a)
	if !p.acquire(admissionKey, a.effectiveConcurrency()) {
		t.Fatal("test admission")
	}
	defer p.release(admissionKey)
	cooldownBefore := map[string]time.Time{modelKey: p.cooldowns[modelKey], expiredKey: p.cooldowns[expiredKey]}
	refreshBefore := map[string]gatewayRefreshFailure{refreshKey: p.refreshCooldown.entries[refreshKey], "expired": p.refreshCooldown.entries["expired"]}
	p.Metrics.Record(RequestRecord{AccountID: a.ID, Account: a.Name, Time: now.Unix() - 100, Status: "ok"})
	if p.Metrics.db != nil {
		if _, err := p.Metrics.Prune(context.Background(), now, 100); err != nil {
			t.Fatal(err)
		}
	}
	response := managedTestRequest(t, h, "GET", "/api/accounts/"+a.ID+"/runtime?model=synthetic-model", nil, 200)
	var got struct {
		ID        string
		Admission struct {
			Scope     string
			InFlight  int64 `json:"in_flight"`
			Saturated bool
		}
		Cooldowns managedRuntimeCooldowns
		Policy    struct {
			Source        string
			PerAccount    bool `json:"per_account_override"`
			Websocket     bool `json:"websocket_enabled"`
			Compatibility bool `json:"compatibility_normalization_enabled"`
			Fingerprint   struct {
				Enabled bool
				Profile CodexFingerprintProfile
			}
		} `json:"effective_policy"`
		LastUsed struct {
			At    int64
			Scope string
		} `json:"last_used"`
	}
	if json.Unmarshal(response.Body.Bytes(), &got) != nil || got.ID != a.ID || !got.Admission.Saturated || got.Admission.InFlight != 1 || got.Admission.Scope != "process" {
		t.Fatalf("runtime admission mismatch: %s", response.Body.String())
	}
	if got.Policy.Source != "global_settings" || got.Policy.PerAccount || !got.Policy.Websocket || got.Policy.Compatibility || !got.Policy.Fingerprint.Enabled || got.Policy.Fingerprint.Profile.ID != "macos" {
		t.Fatal("runtime ignored effective settings")
	}
	if len(got.Cooldowns.Entries) != 2 || got.Cooldowns.Model != "synthetic-model" || got.Cooldowns.Entries[0].Scope != "model:synthetic-model" || got.Cooldowns.Entries[1].HTTPCode != 503 {
		t.Fatalf("wrong real cooldowns: %+v", got.Cooldowns)
	}
	if got.LastUsed.At != now.Unix()-100 {
		t.Fatalf("last business event lost after prune: %+v", got.LastUsed)
	}
	for _, secret := range []string{a.AccessToken, a.RefreshToken, a.AccountID, a.ClientID, refreshKey, modelKey, settings.BuiltinPrompt} {
		if secret != "" && strings.Contains(response.Body.String(), secret) {
			t.Fatal("runtime exposed credential/internal key/prompt")
		}
	}
	if !reflect.DeepEqual(cooldownBefore, p.cooldowns) || !reflect.DeepEqual(refreshBefore, p.refreshCooldown.entries) || p.refreshCooldown.use != 8 || p.inFlight(gatewayAdmissionKey(a)) != 1 {
		t.Fatal("management read changed gateway runtime")
	}
	current, _ := p.Store.Get(a.ID)
	if current.Version != a.Version || current.AccessToken != a.AccessToken {
		t.Fatal("read mutated account")
	}
	response = managedTestRequest(t, h, "GET", "/api/accounts/"+a.ID+"/runtime", nil, 200)
	var defaultScope struct{ Cooldowns managedRuntimeCooldowns }
	_ = json.Unmarshal(response.Body.Bytes(), &defaultScope)
	if len(defaultScope.Cooldowns.Entries) != 1 || defaultScope.Cooldowns.Model != "" {
		t.Fatal("unspecified model cooldown was fabricated")
	}
	managedTestRequest(t, h, "GET", "/api/accounts/"+a.Name+"/runtime", nil, 404)
	managedTestRequest(t, h, "GET", "/api/accounts/"+a.ID+"/runtime?model=a&model=b", nil, 400)
	managedTestRequest(t, h, "GET", "/api/accounts/"+a.ID+"/runtime?unknown=x", nil, 400)
}

func TestManagedAccountLastUsedStableID(t *testing.T) {
	runManagedAccountLastUsedStableID(t, webTestSetup)
}
func TestPGManagedAccountLastUsedStableID(t *testing.T) {
	runManagedAccountLastUsedStableID(t, adminPGTestProxy)
}
func runManagedAccountLastUsedStableID(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	p, h := setup(t)
	a := managedTestAccount(t, p, "last-used", nil)
	b := managedTestAccount(t, p, "never-matched", nil)
	now := time.Now().Unix()
	at := now - 120
	p.Metrics.Record(RequestRecord{AccountID: a.ID, Account: "old-display-name", Time: at, Status: "error"})
	p.Metrics.Record(RequestRecord{AccountID: "deleted-distinct-id", Account: a.Name, Time: now - 1, Status: "ok"})
	if p.Store.DB == nil {
		p.Metrics.Record(RequestRecord{Account: a.Name, Time: now, Status: "ok"})
	} else {
		if _, err := p.Store.DB.Pool.Exec(context.Background(), "UPDATE usage_totals SET last_at=$1 WHERE scope='account' AND subject=$2", now+999, a.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := p.Metrics.Prune(context.Background(), time.Now(), 100); err != nil {
			t.Fatal(err)
		}
	}
	r := managedTestRequest(t, h, "POST", "/api/accounts/last-used", map[string]any{"ids": []string{a.ID, b.ID}}, 200)
	var got struct {
		Items []managedAccountLastUsed
		Scope string
	}
	if json.Unmarshal(r.Body.Bytes(), &got) != nil || len(got.Items) != 2 || got.Items[0].At == nil || *got.Items[0].At != at || got.Items[1].At != nil {
		t.Fatalf("incorrect stable-ID match: %s", r.Body.String())
	}
	expectedScope := "retained_request_details"
	if p.Store.DB != nil {
		expectedScope = "durable_usage_events"
	}
	if got.Scope != expectedScope {
		t.Fatal("misrepresented retention coverage")
	}
	for _, ids := range [][]string{{}, {a.ID, a.ID}, {""}, make([]string, 101)} {
		managedTestRequest(t, h, "POST", "/api/accounts/last-used", map[string]any{"ids": ids}, 400)
	}
}

type managedRuntimeRedisHook struct {
	t           *testing.T
	now         time.Time
	scores      map[string]float64
	failure     string
	unavailable bool
}

func (h managedRuntimeRedisHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h managedRuntimeRedisHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(context.Context, redis.Cmder) error {
		h.t.Error("unexpected non-pipelined Redis call")
		return errors.New("unexpected call")
	}
}
func (h managedRuntimeRedisHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, commands []redis.Cmder) error {
		if h.unavailable {
			for _, cmd := range commands {
				cmd.SetErr(errors.New("synthetic unavailable"))
			}
			return errors.New("synthetic unavailable")
		}
		for _, cmd := range commands {
			switch cmd.Name() {
			case "time":
				cmd.(*redis.TimeCmd).SetVal(h.now)
			case "zscore":
				if value, ok := h.scores[cmd.Args()[2].(string)]; ok {
					cmd.(*redis.FloatCmd).SetVal(value)
				} else {
					cmd.SetErr(redis.Nil)
				}
			case "get":
				if h.failure != "" {
					cmd.(*redis.StringCmd).SetVal(h.failure)
				} else {
					cmd.SetErr(redis.Nil)
				}
			default:
				h.t.Errorf("management issued Redis write/nonallowed command %s", cmd.Name())
			}
		}
		return redis.Nil
	}
}
func TestManagedAccountRuntimeSharedReadOnly(t *testing.T) {
	now := time.Now()
	a := Account{ID: "synthetic-id", AccessToken: "synthetic-token", CredentialVersion: 4}
	for _, unavailable := range []bool{false, true} {
		t.Run(map[bool]string{false: "shared", true: "unavailable"}[unavailable], func(t *testing.T) {
			client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
			defer client.Close()
			client.AddHook(managedRuntimeRedisHook{t: t, now: now.Add(20 * time.Minute), scores: map[string]float64{gatewayCooldownKey(a, "model:m"): float64(now.Add(20*time.Minute + 30*time.Second).UnixMilli()), "!capacity": float64(now.Add(20*time.Minute + 10*time.Second).UnixMilli())}, failure: `{"kind":"http","status":429,"until":` + fmtRuntimeInt(now.Add(10*time.Second).UnixMilli()) + `}`, unavailable: unavailable})
			p := &Proxy{Store: Store{DB: &Database{Redis: client, Namespace: "test:"}}}
			got := p.managedRuntimeCooldowns(context.Background(), a, "m", now)
			if unavailable {
				if got.SharedState != "unavailable" || len(got.Entries) != 0 {
					t.Fatal("unavailable shared storage invented healthy state")
				}
				return
			}
			if got.SharedState != "ok" || len(got.Entries) != 3 || got.Entries[0].Until != now.Add(30*time.Second).UnixMilli() {
				t.Fatalf("Redis server-time deadline incorrect: %+v", got)
			}
			if len(p.cooldowns) != 0 || len(p.refreshCooldown.entries) != 0 {
				t.Fatal("read warmed or modified gateway state")
			}
		})
	}
}
func fmtRuntimeInt(n int64) string { raw, _ := json.Marshal(n); return string(raw) }
