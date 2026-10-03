# Verification: 2026-09-17

## Implemented and Deployed

- Runtime-verified reference frontend: port 28765 uses Vue 3 + Element Plus + Vite.
- The new project uses the same framework with one account-management and request-test page.
- Independent systemd service: `codex-agent.service`, main host `43.225.196.170:18879`.
- Service files: `/opt/codex-agent`; private state: `/var/lib/codex-agent`.
- Only this project's port was added to the host firewall. No existing app, database, Nginx, or Cloudflare configuration was modified.
- Single selected account only. No billing, quota management, account scheduling, automatic failover, gateway policies, or statistics.

## Passed

- `go test ./...`, `go vet ./...`.
- Real HTTP tests for SSE passthrough, upstream cancellation, interrupted responses, original tool payloads, non-stream aggregation, compact normalization and models.
- Account import/use/delete, atomic private storage, refresh token rotation serialization and persistence after client disconnect, redacted management API.
- Frontend production build; dependency audit reports no vulnerabilities.
- Playwright login, invalid login, JSON validation, account import/delete and cancel confirmation, request rendering/stop, logout.
- Desktop/mobile screenshots at 1440, 768, 390 and 320 pixels; no horizontal page overflow.
- Production public root and health return HTTP 200. The imported account's real upstream model catalog returns six models.

## Not Yet Passed

- Real inference has not returned a successful terminal event. The imported account returns `server_is_overloaded` or `server_error` in SSE for tested models. A second account returned HTTP 429.
- Same-account direct upstream and this proxy produced the same failed event sequence. The existing WARP egress produced the same failure. This is not evidence that inference is usable.
- Consequently, real non-stream completion, tool-call round trip and compact remain unverified; their protocol paths are covered by local fake-upstream tests.
- Race detector not run: this Windows environment has no C compiler/CGO runtime configured. Ordinary concurrent tests pass.

The source account was copied server-to-server as access token/account ID/expiry only. Its refresh token and original account state were not changed. The local client access key is in `.local/production-client-key`, excluded from version control.

Rerun live verification without importing or altering the original account:

```powershell
python tools/smoke.py --model gpt-5.6-luna
```
