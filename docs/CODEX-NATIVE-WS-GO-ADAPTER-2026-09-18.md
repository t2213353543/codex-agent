# Native WS Go Adapter

This records the actual `gateway_native_ws.go` adapter, separately from the earlier source-backed WS design. The implementation is being integrated with the updated official 0.155.0 Rust helper by the root and Rust owners; root's final immutable combined report is authoritative for real-helper acceptance. This document alone does not claim live upstream inference or production deployment.

## Boundaries

The existing narrow `gatewayWSUpstream` interface retains Go account/session selection, per-turn permissions/admission/leases, 401 recovery, cooldown and usage parsing. Rust owns upstream TLS, the actual WS handshake, masking/deflate/fragmentation and ping/pong. Go continues owning the independently negotiated downstream WS. The adapter never constructs a fake HTTP 101 duplex transport.

`dialNativeGatewayWebSocket` uses the existing native process pool with the target HTTP(S) origin, immutable proxy fingerprint/version, and 90-second handshake timeout. It checks the configured proxy before and after process acquisition, requires `child.capabilities["websocket_message_v1"]`, and has no Go or direct fallback. A selected account/proxy is not changed after an accepted connection. The existing pool owns process count, active slots, socket identity and helper exit; the WS adapter releases one acquired active slot exactly once.

The helper IPC operation is `websocket_message_v1`. Go passes ordered Base64 application headers, fixed origin-form target, zero HTTP body, a 32 MiB decoded-message limit and 60-second write timeout. It does not forward the downstream connection's handshake key/extension state as a new Rust framing state.

Only a real target HANDSHAKE status supplies an `http.Response`. Thus explicit target 401 can enter existing one-refresh/one-redial recovery, and 429 preserves Retry-After. Proxy, TLS, IPC and parse errors do not fabricate a target HTTP status and cannot trigger OAuth recovery. A rejected handshake returns actual captured binary tail up to 16 KiB; `ContentLength=-1` intentionally does not claim the full declared upstream body has been read. The fork's tail may be partial or include raw HTTP framing. Existing safe response-header forwarding excludes Content-Length and Transfer-Encoding.

After upgrade, a session-context lifetime replaces the temporary handshake deadline. A single IPC reader starts immediately at construction and demultiplexes MESSAGE from WRITE_ACK. Writes are context-cancellable and serialized with increasing sequence numbers. ACK proves the Rust sink completed a send, not model acceptance. Missing ACK, IPC EOF or any ambiguous write never causes replay or reconnect. Concurrent callers do not interleave frames.

## Bounded Queues

The initial WS plan suggested one message per queue; the current Go adapter uses **16 queued decoded messages with an aggregate 32 MiB payload budget**. Validated MESSAGE prefixes reserve payload capacity before allocation and body reading; that reservation includes the current in-progress message until delivery or cancellation. Caller-owned returned payloads, envelope/metadata bytes, Rust/TLS/kernel buffers and allocator capacity are additional, so this is not a 32 MiB total-process-memory claim. This permits ordinary unsolicited messages to precede ACK without an unbounded buffer.

When either the message-count queue or payload budget is full, the read pump applies backpressure for at most 60 seconds and remains cancellable. A legal second message is not rejected merely because its predecessor is still queued: capacity is awaited before reading or allocating the second payload. Dequeue and SetReadLimit wake the waiter, which rechecks the limit; an individually oversized message is still rejected. If unsolicited messages precede a late ACK while Go is only writing, that write can time out and close the connection; the adapter does not invent ACK or buffer indefinitely. An earlier complete message queued before peer CLOSE remains readable before the close error. Explicit local CloseNow, unlike peer-close alone, joins the pump and discards unread messages while returning their byte credits. Normal peer close code is retained; reserved/malformed close metadata is rejected.

`SetReadLimit` applies to decoded messages before allocation from the IPC prefix and is checked again before delivering queued data. Negative/unbounded requests cannot exceed the gateway's 32 MiB cap. Text requires valid UTF-8; binary IPC bytes remain byte-identical, leaving the gateway's existing Responses text-policy unchanged. Unexpected HELLO/END/ACK, wrong sequence, duplicate ACK or unknown frame is a protocol error. Metadata frames remain bounded at 64 KiB.

## Focused Checks

```powershell
go test . -run '^TestGatewayNativeWSIPC' -count=5
```

Result before final joint integration: 12 top-level tests, five repetitions, PASS in 0.471 seconds. These use `net.Pipe`, structured synthetic frames and fresh local stores, not the real Rust binary:

- Unsolicited MESSAGE before Write/ACK, exact payloads and exactly-once release.
- Completed MESSAGE queued before peer CLOSE without losing the result.
- Read-context cancellation and closed IPC without replay.
- Ambiguous write without ACK is an error, not success.
- Wrong ACK, sequence, UTF-8, reserved close, END and repeated handshake rejection.
- Oversized declared length rejected before allocating its payload.
- Eight concurrent writes serialize sequence numbers and ACKs.
- 101/401/429, repeated headers, Retry-After and non-UTF-8 captured tail.
- Stale proxy snapshot rejected before process acquisition.
- Full 16-message queue with a 17th pending message and late ACK respects cancellation.
- Blocked IPC write cancels promptly.
- Session context remains active after construction and cancellation stops the pump.

An additional existing-Go-transport regression found that directly returning `websocket.Dial` into the new interface boxes a nil `*websocket.Conn` as a non-nil interface on a rejected handshake. The interface owner fixed that branch to return literal nil. The new thirteenth test `TestGatewayNativeWSIPCInterfacePreservesNilGoHandshake` covers the boundary directly. All new IPC tests plus the existing `TestGatewayWebSocket*` and WebSocket observation hooks then passed three repetitions in 1.214 seconds. Earlier snapshots without the fix must not be treated as preserving Go handshake rejection behavior.

Frozen adapter SHA256: `fa7d3bfb8ceec51a3e413f9c83ad8e6117b8718a55e76e043720707454781da1`.
Frozen adapter test SHA256: `ad1ceaf991fc388532c5af127c6ad7d10d8fbd8a5e04ce66823732b85b9b7dbd`.

Actual Rust interop, official CLI WS tool/resume, route-specific SOCKS bridge tests and Linux `-race` are being run by root against a fresh immutable combined snapshot. Do not reuse the earlier HTTP-only or first WS prototype results as evidence for this final pump/queue version.
