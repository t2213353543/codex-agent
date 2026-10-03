# Known Owner Identity

Official CLI 0.155.0 source separates credential generation from owner generation. `login/src/auth/change_state.rs:22` requires both ChatGPT user and workspace to identify the same owner; `change_state_tests.rs:29` verifies that a token refresh keeps the owner generation while a different user in the same workspace changes it. `core/src/client.rs:1403` resets the cached WebSocket/turn state when ownership changes. This generation is local CLI state, not a field the gateway should synthesize on the wire.

## Gateway Repair

`gatewayAccountRef` already included immutable management ID and a hash of `AccountID` (workspace). It now also appends a SHA256 hash of `Identity.UserID` when that ID is nonblank. No raw user ID, workspace ID, bearer or refresh token enters the reference. Account name, token, refresh token, metadata version and credential version do not affect a known user's reference.

Consequences for the existing routing/authorization paths:

- Same workspace, different known user: old HTTP sticky bindings no longer match; old WS authorization is rejected before another turn is sent.
- Previously known user becomes unknown: the old user-qualified reference no longer matches.
- Same user and workspace with token/RT rotation: reference remains stable, including management CAS reauthorization.
- Old-format bindings without the user suffix cannot prove ownership for a currently known user. They fail closed and remain unchanged; the client must start a new thread. The gateway does not migrate encrypted history to the new reference.

Missing user identity is deliberately **not** interpreted as proof of ownership. Existing unknown-user behavior is retained without adding CredentialVersion/token hashes to affinity, because that would invalidate AT-only conversations on routine rotations. Fully reproducing CLI's conservative unknown-owner generation requires a separately persisted ownership lifecycle; this repair does not claim that broader behavior is implemented.

No admin behavior or request/response payload changed. The shared reference is consumed by sticky selection, request-route identity, WS per-turn authorization and refresh identity checks; this repair does not introduce a transport retry, account switch or history rewrite.

## Verification

```powershell
go test . -run '^TestGatewayOwnerIdentity' -count=3
```

Five top-level tests passed in 0.194 seconds: known-user reference stability, HTTP changed/removed user rejection without upstream forwarding, old-format binding rejection without migration, actual WS second-turn rejection, and same-owner credential rotation retaining sticky/WS authorization.

Combined owner/session/refresh/401/existing-WS regression command passed in 0.388 seconds:

```powershell
go test . -run '^TestGatewayOwnerIdentity|^TestGatewaySession|^TestSourceEnsureReady|^TestSource401Recovery|^TestGatewayWebSocket' -count=1
```

`TestPGGatewayOwnerIdentityReplacementRejectsSharedSession` additionally exercises real management CAS credential replacement and a shared Redis binding across two Proxy objects. It must run with isolated `TEST_DATABASE_URL` and `TEST_REDIS_URL` in root's final Linux race acceptance; this document's local results do not count its environment-dependent skip as a pass.

Frozen production file SHA256: `gateway_sessions.go` = `cf40364dd70f83e8293b6a77f8029e13075f70bcabe5e66eb4b9072afda41bb9`.
Frozen test file SHA256: `gateway_owner_identity_test.go` = `df1e861b16c9c20eaed7f5e7e9a4c3e213e37d343199084e2364c1434a941cf2`.

## Metadata Filter

CLI 0.155 `core/src/client.rs:892` filters raw `tool_result_metadata` at send time based on the resolved endpoint's HTTPS official-host allowlist. Both HTTP (`:1533`) and WS (`:1689`) invoke it. `protocol/src/models/executed_tool_calls.rs:489` removes only the raw results, retaining existing call/source/completion metadata. The gateway should preserve what the real CLI sends; it must not reconstruct omitted metadata or add an `owner_generation` field. Current RawMessage-based policy/preparation leaves these nested fields opaque.
