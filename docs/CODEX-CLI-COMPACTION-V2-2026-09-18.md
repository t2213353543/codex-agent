# CLI 0.155 Compaction V2

## Actual Source Gate

Codex CLI 0.155.0 no longer selects remote compaction with the old
`remote_compaction_v2` feature flag. `features/src/lib.rs:595` explicitly ignores
that configuration key, and its feature entry at line 1753 is marked Removed.
That is not evidence that the implementation is disabled.

`model-provider/src/provider.rs:353` selects RemoteCompactionSupport::V2 for an
OpenAI-named provider or a supported Azure Responses provider. This capability
check is not gated on ChatGPT auth. `core/src/session/turn.rs:1419` and
`core/src/tasks/compact.rs:41` select V2 from provider capabilities; the alternate
case is local compaction, not the former remote compact endpoint.

V2 appends a `compaction_trigger` control to ordinary Responses input
(`compact_remote_v2_attempt.rs:76`) and uses `ModelClientSession::stream`.
The output collector requires exactly one compaction item and a completed
response (`compact_remote_v2.rs:430`). The compacted history then retains the
appropriate visible items plus the opaque compaction output.

Consequently the older `real_cli_test.go` fixture's
`remote_compaction_v2 = false` cannot force CLI 0.155 to call
`/responses/compact`. That fixture is historical evidence for its tested
version, not proof of the current gate. Its obsolete endpoint assumption is
not itself a production gateway routing bug. The legacy gateway endpoint can
remain supported independently.

## New Controlled Workflow

`gateway_native_cli_compact_test.go` runs the unmodified official CLI with a
fresh HOME/CODEX_HOME and synthetic API-key provider named OpenAI, explicitly
using HTTP rather than WS. It does not set the removed feature flag. The
upstream is a loopback TLS HTTP/2 scripted Responses server reached through Go
and the sealed native Rust UDS helper. No real accounts or model service are
used.

1. An ordinary Responses reply requests an actual CLI shell tool and reports
   synthetic input usage of 50,000 against a configured auto-compact threshold
   of 25,000.
2. The next request must remain on `/responses`, end in exactly one
   compaction_trigger, carry request_kind=compaction, and include the first
   tool's actual successful output with its exact call_id.
3. The server returns exactly one synthetic opaque compaction item. The next
   normal request must preserve it, and receives a second actual shell call.
4. The second tool result and opaque history must reach the next request before
   the initial CLI process can finish.
5. A new CLI process resumes the same thread, preserving the opaque item and
   post-compaction conversation. It receives the final controlled reply.

The test forbids `/responses/compact` and unexpected extra compactions. It does
not require old tool call/output items to remain explicit after compaction,
because V2 is intended to replace that history. The first tool output must be
verified in the compaction request before the replacement. The opaque value is
only a fixture string, not valid remote ciphertext or a decryption test.

The fixture also checks stable cache identity/instructions/session headers,
TLS 1.3/H2, no Go fallback, five usage records and admission release.
NativeCompaction must be true only for the actual trigger request and false
for the four normal requests, rather than being inferred from the returned
compaction item. Synthetic usage totals remain input=50,400/output=5. These
numbers and cache-key stability are not real cache-hit, cost or quality claims.

The first actual execution completed the expected 5 requests, 1 compaction,
2 tool outputs and 3 opaque-bearing follow-ups in 11.00 seconds. The complete
paired-suite report and frozen hashes must accompany the final verification;
this first observation alone does not certify other tests in that run.

## Final Paired Verification

The completed paired run independently confirms all 18 compaction assertions:
5 Responses requests, exactly 1 trigger/compaction request, 2 actual tool-output
returns, 3 subsequent requests retaining the same opaque item, and 2 genuine
CLI processes. NativeCompaction is true on 1 record and false on 4. No legacy
compact endpoint or removed feature override was used.

- Go source SHA256: `863040e0e9b18ab1ad0007ee2c5396804abcf4e6c868c30fa6620f30d5897e69`.
- Fixture SHA256: `6573ef5f383d0972da4eb1f754eb05bdc191e4da10acc36d5ff0c1c004bb0016`.
- Official CLI SHA256: `660e159a49e823ac8e5986cb238f73158ce4b957d40d9292f8de90862644b501`.
- Sealed helper SHA256: `6df426c0a8eb703929f888055192e09cec0d2f7eea9391f2ae891e1ca6213740`.
- Compaction report: `.local/native-transport-build/gateway-compare-20260918T074720-c718f3/gateway-compact-cli.json`.
- Compaction report SHA256: `754d5121984ab59fbfe4f33959b8c4aa1ee6392baff6ad57ccf74acf1f17c805`.
- Suite result SHA256: `a71438bee936a8804ac318d3774e41228dbb78a053d284c374cceb41a2206b99`.

The run passed **23 native top-level tests plus 1 Go CLI compatibility top-level
test**, 24 top-level tests total, with no skips or OOM. The Go compatibility test
has three actual CLI subcases: HTTP tool/resume, WS fallback tool/resume, and
automatic compaction/resume. The earlier compatibility fixture was updated by
its owner for the current V2 source contract; those are not three additional
native top-level tests.

The container used `network none`, reported no production access, completed
with exit 0 and was removed. The source hash remained unchanged during
validation. The compaction case took 11.00 seconds as a local synthetic sample,
not a production latency measure. No race result is claimed by this paired
report; that verification has its own runner and evidence.
