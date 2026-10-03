# Native Request Compression

## Source Contract

Codex CLI 0.155.0 `core/src/client.rs:1450` chooses zstd only when all three
conditions hold: `enable_request_compression`, auth `uses_codex_backend()`, and
provider `is_openai()`. The feature is stable and enabled by default in
`features/src/lib.rs:1231`; the provider check is its OpenAI provider name.
Backend auth modes include ChatGPT, externally supplied ChatGPT tokens,
Headers, AgentIdentity and PersonalAccessToken. API-key and Bedrock modes do
not pass that auth gate. A header or user-agent that merely resembles Codex is
not evidence that this gate was selected.

The official `http-client/src/request.rs` exposes `EncodedJsonBody::encode` and
`Request::prepare_body_for_send`, but does not expose a raw-bytes constructor
for EncodedJsonBody. Its Raw request variant explicitly rejects compression.
The JSON compression path uses `zstd::stream::encode_all(Cursor, 3)`, inserts
Content-Encoding:zstd and supplies application/json when Content-Type is absent.

## Helper Protocol

Ready capabilities now include `request_zstd_v1`. HTTP RequestHead accepts
`request_compression: "none" | "zstd"`, defaulting to none. Missing capability
must not be interpreted as successful silent preservation by the Go adapter.

In zstd mode, the gateway-prepared JSON is borrowed as serde_json RawValue,
then passed through the official EncodedJsonBody and prepared-body APIs. This
preserves internal key order, duplicate keys, numeric spelling and escapes.
The helper compares the encoded bytes with the input and rejects surrounding
whitespace that RawValue would otherwise trim; it does not silently reformat.
The official serde_json raw_value feature is enabled without changing its
version or patching vendored HTTP code.

The input bound remains 32 MiB decoded bytes. A supplied Content-Length is
validated against that input first, then removed before sending compressed
bytes, so reqwest generates the actual wire length. Compressed output is
bounded at 33 MiB to allow framing overhead. Existing Content-Encoding together
with an explicit zstd request is rejected before upstream I/O. None mode keeps
already encoded body bytes and headers opaque. WS rejects HTTP compression
intent; its permessage-deflate protocol is separate.

The Go owner determines which inbound Responses requests carry proven zstd
intent. This does not authorize helper-side guessing for API-key, OAuth,
compact, or WS requests. Deterministic recompression of the same prepared body
produces the same bytes; this is not a claim that retry CPU work is shared in
the exact same way as a CLI PreparedRequestBody.

## Local Evidence

Debug run `.local/native-local-build/20260918T054014Z-8c4c0b00/` completed 26
HTTP/UDS regressions, 4 WS socket tests and 3 compression tests. Compression
tests compare exact bytes with the same official zstd level-3 primitive and
decode back to the unmodified JSON; network echo tests verify encoding,
Content-Length, duplicate-attempt determinism and failure-before-upstream cases.
These are synthetic local tests, not real-account or public-model requests.

The dependency versions remain the official 265-package graph with 30 unchanged
upstream Rust files and one previously recorded WS route adapter. The lock's
helper dependency edges now include zstd for the test oracle; no registry or
fork version was upgraded. A release build and official-CLI paired observations
must be reported separately by their actual artifact hashes.

## Final Release Package

Release run `.local/native-local-build/20260918T054243Z-b43907cb/` completed with
exit 0 and no OOM. All 26 HTTP/UDS tests used the release executable; 4 WS socket
tests and 3 compression tests ran under cargo test --release, with no skips.
The source snapshot SHA256 is
`c82640ecab502fcc2d75c50b95bd63ec4e390fa9f63fe1d942e251ffc19fd75a`.

- Binary SHA256: `6df426c0a8eb703929f888055192e09cec0d2f7eea9391f2ae891e1ca6213740`.
- Binary size: 14,872,608 bytes.
- Package: `.local/native-releases/0.155.0-linux-x86_64-6df426c0a8eb.tar.gz`.
- Package size: 5,935,205 bytes.
- Package SHA256: `b994f94d784c064502ff3d883ad004c390abae0676bf31c362612acbd46e1015`.
- Manifest SHA256: `f50a63c2fc426d6b5bbec0dff9f0e878b52ab37943949145947e382d7e859e2e`.

All 19 manifest-covered files were verified from the completed 20-entry tar
archive, and the executable mode is 0755. Ready capabilities include both
`websocket_message_v1` and `request_zstd_v1`; HTTP defaults to `cli_request`.
The ELF is stripped x86-64 static PIE, with no PT_INTERP or DT_NEEDED dependency.
The same binary also started and exited normally in a read-only, network-none
rootfs containing no files except the executable. That temporary container and
image were confirmed removed after the interrupted tool invocation completed.
This does not claim a production CentOS runtime, account or upstream-model test.

The package contains the exact official-source provenance, locked graph,
recorded WS adapter patch, capabilities, linkage inspection, release test
evidence and example deployment environment. It has not been deployed. Paired
Go and official-CLI evidence must identify this digest, not borrow the earlier
debug or pre-compression release results.
