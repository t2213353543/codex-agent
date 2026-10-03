# Cache Diagnostic Route Scope

This correction affects only the opt-in workload experiment's `codexWorkloadTransport` in a test file. It does not change gateway production forwarding, model caching, accounting, routing, retries or stored account data. All reproduction traffic used an in-process synthetic RoundTripper, with no network or accounts.

## Reproduced Defect

The workload observer attempted to register every replayable request body as a Responses request, then treated every HTTP 200 body as SSE. In a same-thread sequence of Responses SSE, compact JSON, Responses SSE, compact replaced the latest prompt-history digest and produced an unfinished-SSE diagnostic error. A JSON OAuth exchange also created a fake pending/unfinished model observation.

The two pre-fix tests actually failed:

- Responses/compact/Responses: diagnostic requests 3, responses 2, comparable pairs 2, stable pairs 0, history extensions 0, unfinished 1. Correct model-diagnostic values are 2, 2, 1, 1, 1, 0.
- OAuth-only: diagnostic requests 1, unfinished 1, diagnostic errors 1. Correct cache-diagnostic values are zero.

## Correction and Verification

Root restricted cache registration to POST requests whose structurally parsed, allowlisted route is `/responses`. `/responses`, `/v1/responses` and `/backend-api/codex/responses` are recognized. GET, compact, OAuth, models, encoded path aliases and unrelated prefix/suffix paths are excluded even when a synthetic response happens to look like SSE. General transport HTTP-status/error counters still include every request.

Three new top-level tests passed. New route/reproduction tests plus all existing cache-diagnostic and SSE-observer tests passed three repetitions in 0.186 seconds:

```powershell
go test . -run '^TestGatewayCacheReview|^TestGatewayCacheDiagnostics|^TestGatewayCacheResponseObserver' -count=3
```

The regression checks unchanged request/response bytes, method/path/query/header values, one transport call per requested operation, no compact/OAuth diagnostic errors, no retained pending entries and the expected weighted cache ratio from the two explicitly reported Responses usages only.

This does not retrospectively invalidate the earlier dedicated-account report of 12 successful Responses requests or its 84.22% reported cached-input ratio. Those historical artifacts remain unchanged; this defect requires other JSON routes to enter the same experiment's observer. No new real-account run was performed to replace that evidence.

## Remaining Interpretation Boundaries

Usage validation is intentionally limited to fields needed for the cache calculation. Unrelated malformed optional usage fields do not by themselves discard valid input/output/cached counts. Stable-configuration pairs compare the explicitly selected digest fields, not every possible provider control. Neither statistic proves a model-quality or latency change.

Retained scope digests are capped at 256 scopes and 4096 input-item digests per scope; pending request IDs are capped at 512. The SSE buffer limit is per observer/frame, not a global 32 MiB process-memory guarantee. Concurrent observers, parsed request JSON, temporary buffers and allocator overhead are additional. Raw response bytes exist only transiently while parsing the current frame; exported summaries contain counts and reported usage, not prompt, tool, credential or account data.

Frozen new test SHA256: `d2d571351150090d5da30970d72fa3483fe10c571aba05fef3027c25c924bcaf`.
Reviewed opt-in workload file SHA256: `e07349b33b043dc5a8e4a330a5ac03a0f7912c016293e7489687dea5fd4b2c2d`.
