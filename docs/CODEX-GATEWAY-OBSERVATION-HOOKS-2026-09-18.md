# Gateway Observation Hooks

Date: 2026-09-18. Scope: passive observations on existing HTTP and WebSocket usage records. No live inference or deployment was performed for this change.

## Implemented Boundaries

- HTTP Responses, compact and models use `doObservedAccountRequest`, which delegates the existing `doAccountRequest` without changing its retry/account/body/header behavior. Header time is measured from dispatch of the first attempt to receipt of the response actually returned. Successful 401 recovery therefore includes recovery and the second request. Failed OAuth recovery preserves the first 401's actual header-arrival time; it does not add the later OAuth failure delay. A retry that fails without final headers leaves header time unknown, rather than publishing its discarded first 401 as the final response.
- Header timing is separate from TTFT. Models, compact and failed responses without actual output keep TTFT unknown. A real measured zero remains a pointer to zero.
- A WebSocket's first admitted turn receives its connection's actual handshake duration, including successful pre-upgrade 401 recovery. Subsequent turns do not repeat this measurement. Both header-session and body-session routing are covered. Handshake duration can exceed the first turn's latency because that turn starts after the handshake.
- Existing HTTP failures now identify the branch actually encountered: request body/policy/settings, account selection, proxy route, request construction/headers, response headers, upstream HTTP status, upstream response event/body, and client response write.
- Existing pending WebSocket records retain the first observed failure origin for downstream reads/writes, upstream reads/writes/events, session lease failure or prepare/frame rejection. Cancellation following a known failure cannot overwrite it. An unscoped upstream error still aborts pending turns as before; observation does not infer a successful terminal response.
- Existing request/response provenance and native compaction parsing are exercised end-to-end. An explicit endpoint must be `responses` to observe native compaction; guardian, guardian-classifier, models and legacy compact cannot inherit the meaning of a same-named item. Standalone parser calls with no endpoint preserve the existing management test contract; runtime always sets the endpoint. Returned model/tier/effort do not silently replace their requested-source fields. `usage_metadata.amount` is retained only as the existing bounded opaque decimal scalar, with no currency interpretation.

## Unchanged Semantics

No request/response bytes, forwarding decisions, replay rules, account or proxy selection, admission limits, token totals or usage-record cardinality were changed. No new prompt, output, tool argument, encrypted history, account credential or arbitrary metadata is persisted.

Pre-authentication rejects, unrecognized routes and WebSocket failures before an admitted turn still produce no usage record, matching the existing accounting boundary. This is not a claim that every rejected request has a persisted phase. Existing ignored downstream write errors in models/non-stream JSON are not promoted into different outcomes. No bounded attempt history, endpoint chain or separate OAuth timing field is introduced.

## Verification

Focused command:

```powershell
go test . -run '^TestGatewayObservationHooks' -count=5
```

Result: PASS, 3.086 seconds, all nine top-level tests repeated five times:

- `TestGatewayObservationHooksHTTPProvenanceAndBytes`
- `TestGatewayObservationHooksNativeCompactionEndpointScope`
- `TestGatewayObservationHooksFinalHeadersAfter401`
- `TestGatewayObservationHooksTimeoutHasNoHeaders`
- `TestGatewayObservationHooksErrorsKeepExistingRecords`
- `TestGatewayObservationHooksWebSocketFirstHandshakeOnly`
- `TestGatewayObservationHooksTransportErrorPreservesProxyOrigin`
- `TestGatewayObservationHooksStreamingReadAndWriteFailures`
- `TestGatewayObservationHooksWebSocketAbortOrigins`

Combined existing observer, official-source 401 behavior, WebSocket and management parser contract tests also passed in 1.150 seconds before the endpoint-only refinement. After that refinement, hooks plus existing observer and management parser contracts passed three repetitions in 2.911 seconds. The EOF fixture exposed that `sseScanner.Finish()` sets aborted itself; the hook now observes that outcome without changing it. A temporary unrelated management compilation failure was resolved by its owner before the successful runs.

The gateway race runner owner has the nine names for required-test selection. Whole-tree, vet, immutable snapshot and Linux race validation belong to the coordinated root acceptance and must reference the final shared source snapshot rather than this focused result.

Frozen code files for this subtask: `gateway.go`, `gateway_websocket.go`, `gateway_observation.go`, new `gateway_observation_transport.go`, new `gateway_observation_hooks_test.go`. No changes were made to `stats.go`, admin code, client pools, custom settings or `gateway_upstream.go`.
