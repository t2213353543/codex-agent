# Passive Attempt Observation Contract

The standalone `gateway_attempt_observation.go` collector is complete. This document describes its source/API, not a claim that production handlers already invoke every hook. It does not depend on new `RequestRecord` fields and does not alter requests, responses, retry policy, account selection, refresh locks or accounting.

## API

`withGatewayAttemptObservation(ctx)` returns the derived context and one shared collector; an existing collector is reused. `gatewayAttemptObservationFrom(ctx)` returns nil when absent. Collector and handle methods accept nil receivers as no-ops so a non-instrumented caller needs no alternate request path.

`Begin(phase, requestURI, started)` is called only immediately before an actual send. Supported phases are `request`, `refresh` and `websocket`. It returns a handle with `Headers(status, observed)` and `Finish(source, code, finished)`. Starts determine local attempt order; finish order does not reorder the records. Headers are recorded once, Finish is idempotent, and a timeout without headers has nil status/header time. Missing or backwards timestamps remain unknown rather than becoming zero. A real same-instant interval is measured zero.

For HTTP dispatch, finishing at the response/header boundary measures dispatch duration. For OAuth, its owner may finish after parsing the refresh response, retaining a distinct earlier header time. Cache hits, refresh lock waits, concurrent credential reloads and already persisted rotations are not actual network sends and must not create refresh attempts. The collector cannot infer those distinctions; the hook owner must place Begin at the writer/client boundary.

Maximum retained attempts: **4**, in a fixed array. Additional Begin calls set `AttemptsTruncated=true` and return no retained handle. The flag is nil before any actual observed send, then explicit false/true. No account/session/URL/body state is saved for omitted attempts. This cap bounds collector state, not gateway retry count.

`ObserveInbound(path, payload)`, `ObserveSent(path, preparedPayload)` and `ObserveReturned(responsePayload)` record only their respective model/path source. No fallback copies inbound model into sent or returned model. A nested response object overrides event scope; null/malformed/missing nested model cannot be interpreted as the event's top-level returned model. Model strings are capped at 512 UTF-8 bytes. The passed byte slices are never modified or retained.

Paths are structurally parsed and only the canonical `/responses`, `/responses/compact`, `/models`, `/oauth/token`, `/guardian` and `/guardian-classifier` values survive. `/v1` and `/backend-api/codex` prefixes are canonicalized. Scheme, host, userinfo, query, fragment, unknown routes and encoded path aliases never enter a snapshot. Consequently these fields are route categories, not a full URL chain or verbatim endpoint.

`ObserveStage` accepts only explicitly measured `request_preparation`, `account_selection` and `proxy_selection` intervals. First valid measurements win. There is no DNS, connect or TLS-stage inference. `ObserveErrorTruncation(bool)` must be called from the actual diagnostic truncation boundary; nil means unobserved, false means observed untruncated, and true cannot be erased by a later false.

Error source allows only `gateway`, `client`, `transport`, `upstream`; other strings are omitted. Error code allows `unknown`, `canceled`, `deadline`, `timeout`, `http_status`, `proxy_unavailable`, `capacity`, `protocol`, `transport`, `refresh_failed`, `credential_changed`, `disabled`, `invalid_request` or empty success. Other strings become `unknown`; `error.Error()` must never be passed through as a retained diagnostic. This enum reports the actual hook branch, not a inferred remote cause.

`Snapshot()` returns `GatewayAttemptSnapshot`, with deep copies of slices and optional scalar pointers. Mutating an export cannot modify the collector or another snapshot. Mutex protection covers concurrent starts, headers, completion, stages and snapshots. Root's handler applies the snapshot to its existing usage record once at that record's existing lifecycle boundary.

## Verification

```powershell
go test . -run '^TestGatewayAttemptObservation' -count=5
```

Nine top-level tests, five repetitions: PASS, 0.027 seconds. Coverage includes request-401/refresh-200/retry-200 with separate timing, handshake and cancellation, 64 concurrent Begin/Finish/Snapshot calls bounded to four records, first-header/idempotent-finish semantics, nil/zero/backwards time, snapshot alias protection, fixed path/error allowlists, model provenance, invalid/null response metadata, unchanged input bytes and no retained prompt/output/credential strings.

This focused run is not a race-enabled run or an end-to-end forwarding result. Root must add the new tests to its frozen Linux race selector and validate real hook placement, including actual refresh attempts versus cached recovery and unchanged forwarded bytes.

Frozen file hashes:

- `gateway_attempt_observation.go`: `5d5e07a12c4c05c8a390b7f7b9ebbce41330884030494e79bed5c780ba1cede5`.
- `gateway_attempt_observation_test.go`: `78776bc0bac3ee4c5c907693431cec6cc9b2e91d71b7e99326965b85e07e5689`.
