# Bound Session Cancellation

## Fixed Boundary

Already-bound HTTP sessions and WebSocket per-turn authorization used an authoritative account reload backed by a background PostgreSQL context. Unlike unbound selection, cancelling the caller could leave a database connection-pool waiter alive until the independent storage timeout. The bound refresh error path also replaced a cancellation with a generic account-unavailable error and could write that cancellation into the file-store account's `LastError`.

The new `gatewayCurrentBoundAccountContext` reuses the existing context-aware, exact-ID account reader. It checks the caller's context after the read because database error sanitization otherwise removes the cancellation cause. The complete account owner reference, including `OwnerRevision` and unknown-owner credential identity, and the existing Enabled check remain unchanged. Cancelled requests do not fall back to the scheduling cache or select another account.

HTTP bound selection now supplies its request context. Its refresh failure branch releases admission and returns caller cancellation/deadline errors before the outer account-error annotation. WebSocket authorization also supplies its connection context; first-frame and subsequent-frame preparation exits do not turn cancellation into a fabricated authorization failure or attribute it to an unrelated pending turn.

Only three production files changed: `gateway_owner_revision.go`, `gateway_sessions.go`, and `gateway_websocket.go`. Four new test files and the local race selector cover these paths. OAuth token rotation, credential persistence, request fingerprints, cache keys, body serialization, route selection, and retry budgets were not modified.

## Reproduction

The pre-fix snapshot added only tests to the verified baseline. A real local PostgreSQL pool was restricted to one connection and that connection was held. A pgx acquire trace proved that each public consumer had reached its authoritative account read before its caller was cancelled. Settings, API-key storage, and Redis retained independent available dependencies.

Both HTTP bound selection and WebSocket authorization failed to return within the test's two-second deadline. A separate file-store refresh fixture also failed: cancellation was hidden and the account error field changed. The original failed report remains intact at `.local/management-parity/local-race-20260918t104655-e6bc6b/report.json`; it is not counted as a successful candidate.

## Verified Results

- The fixed PostgreSQL consumers cancel the waiting acquire while the held connection remains occupied. Neither admits a new request nor sends OAuth/inference traffic; credentials and the existing Redis account binding remain unchanged.
- A real loopback WebSocket keeps its first turn pending, blocks authorization of a second create, then cancels the gateway request context. The acquire completes by cancellation, admission is released, no second upstream create/reconnect/downstream error event occurs, and the first record remains aborted without an invented error source.
- Already-cancelled and expired contexts return their original error before an account read. The file-store refresh fixture preserves cancellation and avoids the outer `LastError` write.
- Full Go tests passed in 27.279 seconds, plus `go vet` and Windows/Linux builds.
- The current frozen source passed 476 joint PostgreSQL/Redis Linux race tests, 1188 pass events, with no failures, skips, or missing tests. All four new top-level tests ran; the two PG consumer subcases and the real WS case passed.
- Official CLI 0.155.0 plus the sealed Rust helper passed 29 native top-level tests and 1 Go-transport CLI test with 3 subcases. All 7 required reports passed, with zero runtime skips. This suite covers the retained CLI workflows; the new database cancellation cases are separate local protocol tests, not live model tasks.
- The native comparison container and the PG/Redis test containers, network, compiler image, and temporary credential file were independently confirmed removed.

The frozen source is `.local/source-20260918/gateway-delta-20260918t104942-4dd6aa/source`, Go SHA256 `db13679d158e6cbe1ab79d3e50f205eba04d0341d7edc6d88f09d282c5c7a0fc`. It explicitly overlays the previous verified `38a94373...` snapshot without including unrelated shared-worktree changes. Evidence hashes are in `CODEX-BOUND-CANCELLATION-2026-09-18.json`.

Rerun the joint regression from the repository root:

```powershell
python tools/passive_observation_race.py --expected-go-hash db13679d158e6cbe1ab79d3e50f205eba04d0341d7edc6d88f09d282c5c7a0fc --source-dir .local/source-20260918/gateway-delta-20260918t104942-4dd6aa/source
```

## Limits

This is not a two-second upper bound for the complete request lifecycle. File-store locks, settings access, other database operations, refresh locks, and already-started OAuth exchanges retain their existing behavior. A rotating refresh token must still be persisted when the requesting client leaves.

In particular, the existing PostgreSQL `pgRefresh` callback-failure path can still record `refresh_failed` after an OAuth callback fails, even if the caller has meanwhile cancelled. This change does not modify that storage policy and does not claim all OAuth cancellation leaves PG failure metadata untouched. The new no-mutation PG assertions cover cancellation before OAuth at the authoritative account-read boundary.

No real account, desktop authentication data, live model service, or production deployment was used. Account-dependent testing remains cancelled as requested. No production cache-rate, request-success-rate, geographic identity, or model-quality improvement is claimed from these local tests.
