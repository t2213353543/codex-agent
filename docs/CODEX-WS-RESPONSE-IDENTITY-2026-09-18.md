# WebSocket Response Identity

## Repair

The gateway previously discarded a response ID after consuming its terminal event. If a second turn was pending but had not received its own ID, a duplicate terminal for the first response could be assigned to the second turn. This prematurely released admission, recorded stale usage under the new model, and could apply a model or connection cooldown from an old rate-limit failure.

The fix retains at most 256 completed response identities per connection, as SHA256 digests in a FIFO set/ring. Duplicate events do not refresh the FIFO position. The same check runs before observation changes and before cooldown attribution; only an actually consumed terminal with an accepted response ID enters the history. The first genuine rate-limit event is still processed before its terminal is remembered. Redis publication remains outside the turn mutex.

`gateway_ws_response_history.go` owns the bounded history; `gateway_websocket.go` checks it before setting identity, timing, metadata, usage or releasing a slot; `gateway_cooldown.go` checks it before assigning a rate-limit event to a current model or the whole connection. No request headers, cache keys, body policy, account ownership checks, transport fingerprints or retry behavior changed.

## Reproduction

The pre-fix frozen snapshot `5d427bcb0866e9b0738db1c952edba49ffbd53d9e9f72dffcc08557e193dba99` contains only added tests over verified baseline `db13679d...`. A direct test run failed 7 of 8 stale-terminal subcases. It covered untagged streams, repeated stream IDs, and stale IDs arriving on the old or current lane. The existing active-ID-mismatch control passed. The reproduction summary is retained under `.local/source-20260918/gateway-delta-20260918t110110-c947aa/reproduction.json`.

Four older multi-turn fixtures reused one constant response ID for different responses and failed after the guard was added. They were corrected to use per-response IDs, preserving all original business assertions, timing checks and exact-frame comparisons. Changes to the passive management test are limited to those generated fixture IDs and its `fmt` import. The earlier candidate is not treated as full-suite success merely because its native suite passed.

Two additional fixtures in `gateway_websocket_test.go` could pass after the second turn was aborted during connection close rather than completed normally. A final test-only delta gives those responses unique IDs and requires two successful records with released admission before closing or revoking access. Other constant-ID WS fixtures were checked; those with only one successful response per connection were left alone.

## Test Scope

- Recent duplicate completed/failed events cannot consume the next turn, duplicate usage or release its slot, including cross-lane stale events.
- Recent stale created/output-delta/top-level error events cannot assign the next turn's identity, first-output timing, upstream model or error metadata.
- A first genuine 429 failure still creates one error record and applies only the correct model cooldown. Repeating that failure does not broaden the cooldown to the connection.
- ID-less terminal events keep the previous compatibility behavior. Exact 1024-byte IDs are retained; empty and longer IDs are not. IDs are neither trimmed, case-folded nor truncated.
- FIFO tests cover repeated insertion, duplicate non-refresh, multiple complete wraps, exact latest-window membership and connection isolation.
- The sealed native Rust helper is exercised over loopback TLS WebSocket. After A completes, B starts; the upstream emits A's duplicate completion or stale 429 failure while B is held at a barrier. The downstream receives the exact original bytes, while B retains one active slot, the record count remains one, and all relevant cooldowns remain empty. The real B terminal then completes only B. Each scenario requires one handshake, two creates, two correctly attributed records and zero Go fallback attempts.

The integrated candidate `.local/source-20260918/gateway-delta-20260918t110818-cb5ad4/source`, SHA256 `241bdfb0479a5f6da6f3d06f2d8d1a3ebefb30b5a3a5daaeb0af22ab168b244c`, passed full Go tests, vet, both builds, all 482 joint Linux race tests (1207 pass events) and all 31 native/CLI runtime tests. No runtime or joint-race tests were skipped. Its overlay has three production files, three new test files and four existing fixture updates.

The final test-strengthening snapshot is `.local/source-20260918/gateway-delta-20260918t111622-51c409/source`, SHA256 `d7ba78b4cf67e07fdffabf63f7c48de6f6ed120fbc790101385d45d0d7036b45`. Only the two test functions in `gateway_websocket_test.go` differ from the integrated candidate; every production file and all native/CLI test files are byte-identical. Full local tests passed in 26.535 seconds, vet and both builds passed, and the targeted Linux race run passed both changed tests and their two subcases with no skips. Both binaries are byte-identical to the integrated candidate. The 482-test and 31-test reports belong to the integrated candidate and are not presented as reruns on the final hash. Final results and artifact hashes are recorded in `CODEX-WS-RESPONSE-IDENTITY-2026-09-18.json`. All owned test containers, networks, temporary compiler images and credential files were independently confirmed removed.

## Source And Limits

The pinned official 0.155.0 source requires a string ID for `ResponseCompleted` in `codex-rs/codex-api/src/sse/responses.rs:118`. Its WS response consumer stops on `ResponseEvent::Completed` in `endpoint/responses_websocket.rs:817`. These parser facts do not prove that stale duplicate terminals commonly occur in normal CLI traffic. The bad sequence here is deliberately scripted to verify gateway state integrity.

Frames are never suppressed or rewritten by this fix. The CLI can still react to an old frame received from an upstream. In particular, a top-level `error` still triggers the gateway's existing forward-and-close behavior, even when its ID is recognized as old; pending work is then aborted. This is not automatic conversation repair.

No de-duplication guarantee is made for empty, oversized, unknown, evicted or other-connection IDs. The rule assumes an exact response identity still refers to the same response within its retained connection window; deliberate reuse for a different response is indistinguishable from a duplicate. Ring and set storage are bounded, with 256 SHA256 values in each plus container overhead. The guarantee is not persistent or global.

No real accounts, desktop authentication data, live model calls or production deployment were used. Real cache hit rate, production request success rate, device geography and model quality were not measured.

Rerun the native/CLI suite from the repository root:

```powershell
python tools/native_transport_compare.py --native-run 20260918T054243Z-b43907cb --source-dir .local/source-20260918/gateway-delta-20260918t111622-51c409/source --suite gateway --with-go-cli
```
