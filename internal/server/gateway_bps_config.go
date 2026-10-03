package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"codex-agent/internal/bps"
)

type bpsRuntime struct {
	ToolsVersion   string
	SearchDisabled bool
	history        bpsMemoryHistory
	modelCatalog   bpsModelCatalogCache
	attachments    bpsAttachmentCache
}

func (p *Proxy) inferenceInfo() map[string]any {
	if p.RuntimeModes {
		settings, err := p.settingsValue()
		if err != nil {
			return map[string]any{"backend": "unavailable", "codex_fallback": false}
		}
		if settings.InferenceMode != inferencePowerPoint {
			profile := "codex_cli"
			if settings.InferenceMode == inferenceAppServer {
				profile = "app_server"
			} else if p.AppServerCLI != nil {
				profile = "app_server_cli"
			}
			info := map[string]any{"backend": settings.InferenceMode, "profile": profile, "base_url": codexURL,
				"responses_hosted_runtime": settings.InferenceMode == inferenceAppServer,
				"web_search":               "enabled", "codex_fallback": false, "sticky_sessions_required": true,
			}
			if p.AppServerCLI != nil {
				info["app_server_cli_capabilities"] = p.AppServerCLI.capabilityManifest()
			}
			return info
		}
	}
	if p.BPS == nil {
		return map[string]any{"backend": "protocol_fixture", "profile": "fixture", "codex_fallback": false}
	}
	search := "enabled"
	if p.BPS.SearchDisabled {
		search = "disabled"
	}
	return map[string]any{"backend": "powerpoint_bps", "profile": "powerpoint", "base_url": p.BaseURL, "tools_version": p.BPS.ToolsVersion, "web_search": search, "codex_fallback": false, "sticky_sessions_required": true, "continuation_ttl_seconds": int64(bpsHistoryTTL / time.Second)}
}

func newBPSRuntime(getenv func(string) string) (*bpsRuntime, string, error) {
	base := getenv("BPS_BASE_URL")
	if base == "" {
		base = bps.BaseURL
	}
	u, err := url.Parse(base)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Hostname() == "" {
		return nil, "", errors.New("invalid BPS_BASE_URL")
	}
	if u.Scheme != "https" {
		ip := net.ParseIP(u.Hostname())
		if u.Scheme != "http" || (u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback())) {
			return nil, "", errors.New("BPS_BASE_URL requires HTTPS except for loopback test servers")
		}
	}
	version := getenv("BPS_TOOLS_VERSION")
	if version == "" {
		version = bps.DefaultToolsVersion
	}
	if !strings.HasPrefix(version, "tools-powerpoint-") || len(version) > 200 || strings.ContainsAny(version, "\r\n\t ") {
		return nil, "", errors.New("BPS_TOOLS_VERSION must identify a PowerPoint tool catalog")
	}
	search := getenv("BPS_WEB_SEARCH")
	if search != "" && search != "enabled" && search != "disabled" {
		return nil, "", errors.New("BPS_WEB_SEARCH must be enabled or disabled")
	}
	return &bpsRuntime{ToolsVersion: version, SearchDisabled: search == "disabled"}, strings.TrimRight(base, "/"), nil
}

func bpsUUID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic("system random source unavailable")
	}
	raw[6] = raw[6]&0x0f | 0x40
	raw[8] = raw[8]&0x3f | 0x80
	x := hex.EncodeToString(raw[:])
	return x[:8] + "-" + x[8:12] + "-" + x[12:16] + "-" + x[16:20] + "-" + x[20:]
}

func bpsTaskID(keyID, session, owner string) string {
	h := sha256.Sum256([]byte(keyID + "\x00" + session + "\x00" + owner))
	x := hex.EncodeToString(h[:16])
	return x[:8] + "-" + x[8:12] + "-" + x[12:16] + "-" + x[16:20] + "-" + x[20:]
}

func applyBPSHeaders(h http.Header, account Account) {
	h.Del("ChatGPT-Account-ID")
	h.Del("X-OpenAI-Account-Id")
	h.Del("X-OpenAI-Account-User-Id")
	h.Set("Authorization", "Bearer "+account.AccessToken)
	h.Set("Content-Type", "application/json")
	h.Set("X-Basispoints-Auth-Mode", "chatgpt")
	h.Set("X-Openai-Internal-Basispoints-Client-Product", "basispoints-powerpoint-plugin")
	h.Set("X-Openai-Internal-Basispoints-Client-Platform", "powerpoint")
	h.Set("X-Openai-Internal-Basispoints-Client-Agent-Profile", "powerpoint")
	h.Set("X-Openai-Internal-Basispoints-Client-Editor", "powerpoint")
	h.Set("X-Openai-Internal-Basispoints-Client-Host", "office")
	h.Set("X-Openai-Internal-Basispoints-Client-Runtime", "web")
	// Preserve the deployed gateway profile independently of Office-host
	// identity experiments, which have not established a production fix.
	h.Set("X-Openai-Internal-Basispoints-Client-Platform-Class", "web")
	if account.AccountID != "" {
		h.Set("ChatGPT-Account-ID", account.AccountID)
		h.Set("X-OpenAI-Account-Id", account.AccountID)
	}
	claims := managedIdentityJWT(account.AccessToken)
	var auth map[string]json.RawMessage
	if json.Unmarshal(claims["https://api.openai.com/auth"], &auth) == nil {
		var user string
		if json.Unmarshal(auth["chatgpt_account_user_id"], &user) == nil && !strings.ContainsAny(user, "\r\n") && len(user) < 512 {
			h.Set("X-OpenAI-Account-User-Id", user)
		}
	}
}
