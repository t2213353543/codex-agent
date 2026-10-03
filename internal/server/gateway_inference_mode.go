package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	inferencePowerPoint = "powerpoint_bps"
	inferenceCodexCLI   = "official_codex_cli"
	inferenceAppServer  = "official_app_server"
)

type inferenceModeContextKey struct{}

func validInferenceMode(mode string) bool {
	return mode == inferencePowerPoint || mode == inferenceCodexCLI || mode == inferenceAppServer
}

func requestInferenceMode(ctx context.Context) string {
	mode, _ := ctx.Value(inferenceModeContextKey{}).(string)
	return mode
}

func withInferenceMode(ctx context.Context, mode string) context.Context {
	return context.WithValue(ctx, inferenceModeContextKey{}, mode)
}

// A request selects its backend once. Settings changes never move an in-flight
// stream to a different provider or mutate shared clients and account locks.
func (p *Proxy) gatewayInferenceBaseURL(ctx context.Context) string {
	if requestInferenceMode(ctx) == inferenceCodexCLI || requestInferenceMode(ctx) == inferenceAppServer {
		return codexURL
	}
	return p.BaseURL
}

func (p *Proxy) isBPSUpstreamRequest(r *http.Request) bool {
	if p.BPS == nil || requestInferenceMode(r.Context()) == inferenceCodexCLI || requestInferenceMode(r.Context()) == inferenceAppServer {
		return false
	}
	return strings.HasPrefix(r.URL.String(), strings.TrimRight(p.BaseURL, "/")+"/")
}

func (p *Proxy) nativeHTTPFor(ctx context.Context) *gatewayNativePool {
	if requestInferenceMode(ctx) == inferenceCodexCLI && p.CodexTransport != nil {
		return p.CodexTransport.NativeHTTP
	}
	return p.NativeHTTP
}

func (p *Proxy) nativeWebSocketFor(ctx context.Context) bool {
	// Native WebSocket must be enabled at startup (requires Rust helper).
	// The PreferWebsocket setting then controls whether to actually use it.
	var nativeEnabled bool
	if requestInferenceMode(ctx) == inferenceCodexCLI && p.CodexTransport != nil {
		nativeEnabled = p.CodexTransport.NativeWebSocket
	} else {
		nativeEnabled = p.NativeWebSocket
	}

	// If native WebSocket is not available, return false immediately.
	if !nativeEnabled {
		return false
	}

	// Check the runtime PreferWebsocket setting.
	settings, err := p.settingsValue()
	if err != nil {
		// On error, fall back to the startup configuration.
		return nativeEnabled
	}

	// Only use WebSocket if both native support is enabled AND the setting is on.
	return settings.PreferWebsocket
}

func (p *Proxy) inferenceClient(ctx context.Context, proxy *ProxyEntry, mode gatewayClientMode, timeout *time.Duration) *http.Client {
	owner := p
	if requestInferenceMode(ctx) == inferenceCodexCLI && p.CodexTransport != nil {
		owner = p.CodexTransport
	}
	return gatewayPinnedRequestClient(ctx, proxy, owner.gatewayClientForConfigured(proxy, mode, timeout))
}

func inferenceModeOptions() []map[string]any {
	return []map[string]any{
		{"value": inferencePowerPoint, "label": "PowerPoint + Codex", "client_tools": true, "hosted_runtime": false},
		{"value": inferenceCodexCLI, "label": "Codex 直连", "client_tools": true, "hosted_runtime": false},
	}
}

func (p *Proxy) inferenceModeOptions() []map[string]any {
	options := inferenceModeOptions()
	for _, option := range options {
		if option["value"] == inferenceCodexCLI {
			option["hosted_runtime"] = false
			option["label"] = "Codex 直连"
		}
	}
	return options
}

type inferenceSessionBinding struct {
	mode    string
	expires time.Time
}

var errInferenceSessionChanged = errors.New("this session belongs to a different inference mode; start a new client conversation after changing modes")

// Ciphertext, response identifiers and pending tools belong to the selected
// provider. Bind explicit sessions independently of the shorter account lease.
func (p *Proxy) bindInferenceSession(ctx context.Context, keyID, session string) error {
	mode := requestInferenceMode(ctx)
	if !p.RuntimeModes || mode == "" || session == "" {
		return nil
	}
	hash := sha256.Sum256([]byte(keyID + "\x00" + session))
	key := hex.EncodeToString(hash[:])
	migrate := bpsAllowsModeMigration(ctx, mode)
	if db := p.Store.DB; db != nil {
		if db.Redis == nil {
			return errors.New("inference session store unavailable")
		}
		result, err := db.Redis.Eval(ctx, `local prior=redis.call('GET',KEYS[1]); if prior and prior~=ARGV[1] and not (ARGV[3]=='true' and (prior=='official_app_server' or prior=='official_codex_cli')) then return 0 end; redis.call('SET',KEYS[1],ARGV[1],'EX',ARGV[2]); return 1`, []string{db.Namespace + "inference:session:" + key}, mode, 3600, fmt.Sprint(migrate)).Int()
		if err != nil {
			return errors.New("inference session store unavailable")
		}
		if result != 1 {
			return errInferenceSessionChanged
		}
		return nil
	}
	p.inferenceSessionMu.Lock()
	defer p.inferenceSessionMu.Unlock()
	now := time.Now()
	if binding, ok := p.inferenceSessions[key]; ok && binding.expires.After(now) && binding.mode != mode && !(migrate && (binding.mode == inferenceAppServer || binding.mode == inferenceCodexCLI)) {
		return errInferenceSessionChanged
	}
	if p.inferenceSessions == nil {
		p.inferenceSessions = map[string]inferenceSessionBinding{}
	}
	if len(p.inferenceSessions) >= 4096 {
		for k, v := range p.inferenceSessions {
			if !v.expires.After(now) {
				delete(p.inferenceSessions, k)
			}
		}
	}
	if _, ok := p.inferenceSessions[key]; !ok && len(p.inferenceSessions) >= 4096 && !gatewayUnboundedPayload(ctx) {
		return errors.New("inference session capacity reached")
	}
	p.inferenceSessions[key] = inferenceSessionBinding{mode: mode, expires: now.Add(time.Hour)}
	return nil
}
