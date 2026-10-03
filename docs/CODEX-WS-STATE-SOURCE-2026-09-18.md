# WS State: Source And Observation

This review and fixture use only local Codex CLI 0.155.0 source and the genuine
CLI binary against a synthetic stateful WebSocket server. No real accounts or
model inference participate. The Rust release remains sealed and unchanged.

## Incremental Requests

`core/src/client.rs:1248` compares non-input request properties and the complete
previous input plus server output before returning an input delta. It explicitly
allows an empty delta in `prepare_websocket_request` and uses the latest
completed response ID. Server output is part of the baseline, not input to resend.

The upstream tests `core/tests/suite/client_websockets.rs:2336` and `:2414`
demonstrate that the CLI chooses full input and omits previous_response_id when
instructions change. They do not prove that the remote Responses service rejects
every previous_response_id request whose instructions change. Consequently the
gateway's runtime-policy behavior was not changed to invent a 409 or reconnect
requirement from this source observation alone.

The strengthened local fixture stores completed response contexts per upstream
connection. Every nonempty previous_response_id must reference the latest
completed response on that exact connection. Logical input is reconstructed as
previous input + previous output + current delta. The tool follow-up must be one
function_call_output with the expected call identity and actual CLI shell output,
and must not repeat prior input/output items. Resume is checked against the
reconstructed full context, not just a substring of the final transmitted delta.

## Turn-State Sources

The API layer can populate turn state from the handshake header when supplied
an optional OnceLock. However, CLI core `connect_websocket` passes
`turn_state: None` to that handshake call (`core/src/client.rs:1091`). The actual
CLI path instead supplies its turn OnceLock to stream_request and reads the
first response.metadata header (`codex-api/src/endpoint/responses_websocket.rs:764`).
Later metadata hints do not overwrite the first one. A new process/turn gets a
new lock; stale state must not be copied across resume.

Two scenarios distinguish these layers:

- Handshake-only hint: the gateway's downstream upgrade contains the upstream
  header, but CLI 0.155 does not echo it into client_metadata. Absence of that
  echo is not evidence that the gateway dropped the handshake header.
- Response-metadata hint: the first metadata hint is echoed on continuations,
  later deliberately different hints are ignored, and the resumed process uses
  the new connection's first hint rather than the previous process's value.

## Preliminary Actual Results

The preliminary frozen candidate ran both scenarios successfully at
`.local/native-transport-build/gateway-compare-20260918T071016-df3de7/gateway-ws-cli.json`.
Each scenario observed 2 connections, 2 warmups, 3 generated responses, 3 exact
completed-response links and 1 tool-output-only delta. Handshake-only observed
0 metadata echoes and 5 absent values; metadata-driven observed 3 correct echoes
and 2 absent pre-metadata values. Both scenarios completed actual echo-tool
execution and process resume with no Go upstream fallback.

The runtime observed **zero empty deltas**, including zero empty warmup deltas.
Empty-delta support is source-backed and present in upstream unit tests, but
this scenario did not exercise it. No claim of actual empty-delta coverage is
made. Metadata hint and previous-response counters are observed fixture facts,
not cache-hit, model-quality or live-account results.

## Warmup Records And Final Report Shape

Official source calls generate=false prewarm connection setup rather than a
model inference attempt. The fixture now emits zero synthetic usage for warmups
and preserves generated usage at input=100/output=1/cached=80. The gateway's new
passive warmup field is checked against actual records without changing this
fixture's request count or billing rules.

After the preliminary run, reporting was tightened only: the metadata-specific
assertion is omitted in the handshake-only scenario, and warmup_usage_zero is
computed from observed records, requires at least one warmup, and requires the
record count to match the observed warmup count. The final test file SHA256 is
`ae6678caf1ac94c23bc1515594225b49bbca127844d8ad6347e42dbed8bd6ec2`.
The root's subsequent final candidate run must independently establish its
success; the preliminary run does not certify a different frozen hash.

## Final Verification

The final candidate was separately executed after the reporting corrections:

- Go source SHA256: `eb242227dfba5a377d83cbaf8b6caed8432b5acdb5424beaad4b3fe39d037d87`.
- Frozen fixture SHA256: `ae6678caf1ac94c23bc1515594225b49bbca127844d8ad6347e42dbed8bd6ec2`.
- Sealed helper SHA256: `6df426c0a8eb703929f888055192e09cec0d2f7eea9391f2ae891e1ca6213740`.
- Result: `.local/native-transport-build/gateway-compare-20260918T071306-c8f465/result.json`.
- Result SHA256: `db3e5ba4423e72112bab88244058686ca5bca404eabac65e2f74afb779dcac40`.
- WS report SHA256: `44241c629e66c4fe323272cd8ecdd935a7282a2f574e16d1437316dbabd4b167`.

All 21 native-integration top-level tests passed with no skips, exit 0, no OOM,
and the temporary network-none container was removed. The actual CLI WS test
ran both state-source scenarios in 21.26 seconds; this is a local sample, not
a production latency assertion.

The final WS JSON has all applicable assertions true. The handshake-only
scenario omits the metadata-only assertion rather than reporting a misleading
false. Warmup usage is now calculated from actual records. Each scenario still
has 2 connections, 2 warmups, 3 generated responses, 3 exact previous-response
links and 1 exact tool-result-only delta. Aggregates are 4 connections, 4 warmups,
6 generated responses and 6 incremental continuations, with zero Go fallback.
Both actual CLI scenarios still observed zero empty input deltas.

Separately, `TestGatewayNativeActualWebSocketEmptyDeltaAndOpaqueItems` passed in
0.13 seconds using the genuine Rust helper over TLS. Its **handwritten protocol
fixture**, not the CLI, sent three exact-byte frames: a warmup; an empty
input-array continuation referencing the completed warmup; and mixed function/
custom-tool outputs with opaque reasoning, unknown fields, a large exact integer
and null. The upstream checked exact bytes, responses retained IDs, warmup
classification was correct, usage remained unknown when absent, and no fallback,
replay or admission residue occurred. This establishes transport preservation
for that explicit empty-delta case without falsely saying the real CLI emitted
an empty delta in the two observed scenarios.

The earlier preliminary report is retained unchanged. This final verification
does not establish remote server behavior when request policy changes during a
previous-response chain, nor model quality/cache behavior or real-account
authentication. No production policy or sealed native executable was changed
by the fixture/report work.
