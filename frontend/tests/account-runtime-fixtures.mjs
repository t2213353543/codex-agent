// Default read-only observation for pre-existing account workflows. Dedicated
// runtime tests override this with saturation, cooldown, unknown and race cases.
export function accountRuntimeFixture(account) {
  return {
    id: account.id, version: account.version, checked_at: Date.now(),
    admission: { scope: 'process', in_flight: account.in_flight || 0, limit: account.concurrency || 1, saturated: (account.in_flight || 0) >= (account.concurrency || 1) },
    cooldowns: { coverage: 'connection_models_and_requested_model', shared_state: 'not_configured', entries: [] },
    effective_policy: {
      source: 'global_settings', settings_version: 1, per_account_override: false,
      fingerprint: { enabled: false, configured_profile: 'linux', profile: null, scope: 'application_headers_only' },
      websocket_enabled: false, http_stream_enabled: true, http_json_enabled: true, compact_enabled: true, models_enabled: true,
      compatibility_normalization_enabled: true, automatic_token_refresh_enabled: true, upstream_401_recovery_enabled: true,
      sticky_sessions_enabled: true, prompt_cache_key_enabled: true, builtin_prompt_enabled: false, builtin_prompt_mode: 'prepend',
      account: { enabled: account.enabled === true, concurrency: account.concurrency || 1, has_refresh_token: account.has_refresh_token === true, expires_at: account.expires_at || 0 },
    },
    last_used: { at: null, scope: 'retained_request_details', source: 'gateway_request_records' },
  }
}
