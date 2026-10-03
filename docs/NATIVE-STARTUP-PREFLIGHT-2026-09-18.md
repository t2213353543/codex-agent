# Native Startup Preflight

This local probe runs the sealed release helper without sending an IPC request
envelope, HTTP request, WebSocket handshake, account credential or model input.
It is not an upstream connectivity or production fingerprint check.

## Source Findings

Rust startup reads its bounded configuration, then calls `build_state` before
binding IPC and emitting ready. It builds the HTTP reqwest client even in
`cli_request` mode, where that initial validation client is discarded, and
constructs the official WebSocket ExplicitCodexTls configuration. Local CA
files/native roots and TLS-provider setup are therefore exercised at startup.
The origin is only parsed; the network send/connect paths are not called until
an authenticated request arrives.

A fixed `http://127.0.0.1:1` origin with `proxy: null` is sufficient for this
bounded local check. The production Go path uses a private 0700 directory and
0600 Unix socket. Its existing ready validation checks protocol, transport,
socket path/type/mode, body and active limits, lifecycle agreement and bounded
capability metadata. Startup additionally requires `request_zstd_v1`, and
`websocket_message_v1` only when native WS is requested. No exact CLI version
string is imposed as a new runtime lock.

The disposable process receives stdin EOF and must exit successfully. An
unresponsive helper is terminated/reaped but does not pass. The private socket
directory must be gone before publishing the pool. Failure closes the unused
pool/reaper and leaves `p.NativeHTTP` unattached. No serving-pool reservation is
created by preflight.

Shutdown now also cleans a socket bound before an invalid or absent ready
record: only after the child has exited, and only inside its private startup
directory. A socket identity already validated at ready is never re-sampled;
the existing SameFile protection continues to preserve a replaced path.

## Independent Runtime Evidence

Sealed helper SHA256:
`6df426c0a8eb703929f888055192e09cec0d2f7eea9391f2ae891e1ca6213740`.

The Python probe ran in a network-none container at 1 CPU and 512 MiB, with a
loopback listener monitoring the configured port 1. Its five cases passed:

- Strict lifecycle ready, socket 0600, normal EOF exit and socket cleanup.
- Pooled lifecycle with the same startup/exit checks.
- Missing custom CA rejected with fixed `tls_or_client_config` and no ready.
- Nonprivate UDS parent rejected with fixed `unix_parent_invalid` and no ready.
- Truncated configuration followed by EOF rejected with fixed `config_read`.

Across all cases: zero request envelopes, zero observed connections to the
configured origin, no forced kills and no live accounts. The observer only
monitored the fixed origin; the no-other-dial conclusion additionally relies on
the inspected startup call graph, not on a claim of system-wide packet capture.

Final report:
`.local/native-startup-probe/20260918T065226Z-115d0caf/report.json`.

SHA256: `c397abadae02134c77b206e2230fc0600ddfb5ae8aeb6168be7ac8eab71a8833`.

The runner removes only its exact ID/label-owned disposable container from a
finally block, including copy/wait/report failure paths. Two offline cleanup
tests pass. The final actual probe exited 0, no OOM, and its container was
removed. New external test files do not alter or rebuild the sealed release;
the current full native source tree is not asserted equal to its earlier build
snapshot.

The separate Go integration test covers both lifecycles with WS off/on and
invalid `CODEX_CA_CERTIFICATE`/`SSL_CERT_FILE`, checking zero retained entries,
no private directory or child-process residue, and failed-startup nonattachment.
Its actual Linux result belongs to the root's final Go/helper pairing report,
not to the standalone Rust probe above.

## Boundaries And Rerun

This does not dial an account proxy, authenticate an upstream certificate,
negotiate TLS/HTTP with a target, or prove model acceptance. Runtime CA changes,
later binary replacement, destination routing and per-request lifecycle remain
separate checks. An empty native root store can still permit client construction;
ready is not proof that any particular HTTPS certificate will be trusted.

```powershell
python tools/native_startup_probe.py
```
