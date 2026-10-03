# Lightweight App Server account failover

The opt-in `AppServerCLI` Responses route now uses the same outer account failover wrapper as the hosted App Server route. The legacy direct route is unchanged when `AppServerCLI` is absent. Admission remains in `pickSessionAccount` / `pickFailoverAccount`, so existing authorization, account priority/weight, cooldown and per-account concurrency rules remain authoritative. A failed attempt releases its concurrency lease before the next account is admitted.

An explicit upstream HTTP 401/429, structured SSE authentication/quota rejection before visible output, unavailable bound account, or unavailable model may select another eligible account. Network ambiguity, arbitrary error text, other HTTP errors and failures after visible text/tool/reasoning output do not authorize replay. Each account is tried at most once per incoming request; exhaustion returns the last real account failure instead of a generic selection error.

The original incoming decoded body is reapplied through policy/preparation for each admitted account. Account-specific cache keys, credentials, proxy selection and actual model metadata are rebuilt for the new owner; context and encrypted history are never invented or rewritten. The inference mode is fixed through retries even if settings change concurrently.

The failed owner's sticky binding remains until the replacement produces usable output or a successful terminal result. `switchSessionAccount` performs compare-and-swap before delivery. A concurrent request's newer owner wins; CAS failure returns HTTP 409 and suppresses the replacement output. An empty successful stream also commits before its terminal response is exposed. One final request record contains the bounded attempt summaries.

Requests with a nonempty `previous_response_id` are not replayed across accounts. That server-owned delta may depend on context absent from the client request, and this lightweight route deliberately does not store complete conversation history. It preserves the real account error so the client can send complete history; it never clears the response ID and pretends the delta was a complete request.

The WebSocket-to-SSE pump treats a complete `error`, `response.failed` or `response.incomplete` event as a complete protocol exchange: it writes that event and ends with normal EOF. Only a socket close without a terminal remains a truncated transport error. This lets the outer buffer deliver real structured rejections and lets pre-output authentication/quota failures reach the account classifier. Structured prewarm rejections use the same SSE boundary. Tests run through `httptest.NewServer` and a real HTTP client, which catches the previous `ErrAbortHandler`/empty-EOF failure that an in-memory response recorder missed.

Verification includes first-invalid/second-valid, every account invalid, quota rejection before output, no replay after visible output, collected JSON, CAS conflict, legacy-disabled behavior, pinned response deltas, exact admission release and single-record accounting. The existing hosted-runtime failover suite also passes.

Rerun: `go test ./internal/server -run '^(TestAppServer|TestOfficialFailover|TestGatewayAccountFailover)' -count=1`.
