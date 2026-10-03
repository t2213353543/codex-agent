# Native WS Backpressure Probe

This is an independent local observation of the sealed release helper, not a
production change or a claim that an unread consumer can receive unbounded
events. No real accounts, desktop authentication or model endpoints were used.

## Method

The executable SHA256 was
`6df426c0a8eb703929f888055192e09cec0d2f7eea9391f2ae891e1ca6213740`.
A standalone Go probe used `github.com/coder/websocket v1.8.14` as its real
WebSocket server, with context-takeover compression enabled. The Rust helper
connected through its authenticated Unix IPC and existing official connector.
No WebSocket wire parser or compressor was hand-written in the probe.

The upstream sent 20 complete synthetic text events, each with 256 KiB padding.
The IPC consumer requested a 4096-byte socket receive buffer and read only the
first MESSAGE frame prefix, leaving its 262,183-byte payload unread. It paused
for 150 ms, sent one tool-result SEND command in the reverse direction, then
waited another 150 ms without consuming output. Thus the reverse SEND occurred
while application output was backpressured, not after an acknowledged drain.

## Results

| Case | Actual Observation |
| --- | --- |
| Resume consumer within 5 s write deadline | Upstream had not received the tool result during the pause. After resume, all 20 events had exact sequence, index and content; one matching WRITE_ACK arrived after 3 ms. |
| CANCEL frame while blocked | Upstream disconnected in less than 1 ms after cancellation. |
| Parent stdin EOF while blocked | Upstream disconnected in less than 1 ms after parent EOF. |
| Deliberate 400 ms IPC write deadline | With the consumer still paused, upstream disconnected approximately 100 ms after the already elapsed 300 ms pause. This is the configured finite timeout, not a deadlock. |

Every case used exactly one upstream handshake. The probe found no lost or
reordered event in the resume case, no duplicate ACK, and no automatic reconnect
or replay. Cancelled/timed-out cases intentionally do not claim message delivery.
Zero-valued elapsed millisecond fields are omitted by the probe JSON; those two
cases completed within the timer's first millisecond.

The test ran in a network-disabled container at 2 CPU and 1 GiB, exited 0 with
no OOM, and its temporary container was removed. Raw event/tool contents and
credentials were not recorded in the report.

Evidence: `.local/native-ws-backpressure/20260918T060919Z-ccc17da6/report.json`.
Report SHA256: `3b6fcf4857a8f2a22cabbe87a9a1ce57500001d394688c865fed2f29418e4b6c`.

## Interpretation

Official CLI 0.155.0 `codex-api/src/endpoint/responses_websocket.rs` uses one
connection pump, awaits sink sends, and forwards received messages to an
unbounded application channel. It does not split the WebSocket into independent
transport-reader and transport-writer tasks at that boundary. The helper keeps
bounded message channels instead, and its independent IPC input cancellation
future remains selectable while pump/output are blocked.

The observed delayed tool send is expected bounded backpressure: an ACK cannot
be inserted into the middle of an already-started IPC MESSAGE frame. Recovery
requires the consumer to resume reading before the deadline. This successful
bounded probe does not establish arbitrary-duration lossless buffering or an
overall RSS bound; WebSocket/TLS/kernel buffers and active message copies remain
additional. No production fix was made from this experiment.

## Rerun

```powershell
python tools/native_ws_backpressure_probe.py
```
