# HTTP/2 And TLS Lifecycle Evidence

The official Linux Codex CLI 0.155.0 and the native helper were exercised against
one controlled loopback TLS origin and certificate. The CLI executed actual
fixture tools and returned their output; model responses were synthetic. No
production accounts, endpoints, host ports or live inference were used.

## Observed Difference And Fix

The original helper `fb535a5564e2125ad924ab79c9c487cf8307e1884fe960a13a0584f85f8aea29`
kept an HTTP client across requests. With custom CA/rustls and server-forced
reconnections, the CLI made four full TLS handshakes while the helper resumed
three of four sessions. Both `session_reused` and the ClientHello PSK extension
supported that difference. Initial HTTP/2 SETTINGS and connection window
increments already matched. The original observation remains in:

`.local/native-fingerprint/20260918T042215Z-dc02a08f/report.json`

CLI source explains the lifecycle: `core/src/client.rs` rebuilds the API
transport for each HTTP streaming call (lines 1495-1503), and
`build_api_transport` calls `create_client_for_route` (lines 1040-1052).
`login/src/auth/default_client.rs` constructs a fresh client. Matching a crate
version alone does not reproduce that lifetime.

The helper now defaults to `http_client_lifecycle = "cli_request"`, creating a
fresh official HTTP client per exchange while retaining the official restricted
process-wide infrastructure cookie store. `pooled` is an explicit performance
mode, not a claim of the CLI's original lifetime. The ready record reports the
selected lifecycle, and Go verifies it before admitting requests. WS connection
lifetime and the helper process pool are unchanged. TLS tickets and the TLS
library were not modified to force matching observations.

## Final Measurements

Final helper SHA256:
`200bf8758823766918fc1799e006501c6e37401e2d3f560f11038ed3a899488f`

Official CLI SHA256:
`660e159a49e823ac8e5986cb238f73158ce4b957d40d9292f8de90862644b501`

Six cases used the same loopback origin: default native/custom CA crossed with
official CLI/strict helper/pooled helper control. Each case made four completed
requests over four TLS connections in one client process, for 24 requests total.
The server forced a new connection after each response using H1 Connection:close
or H2 GOAWAY. Both official cases observed real function_call_output.

| Observation | Official | Strict Helper | Pooled Control |
| --- | --- | --- | --- |
| Default native TLS | TLS 1.3, no ALPN, H1 | Same | Same |
| Default native resumed sessions | 0/4 | 0/4 | 0/4 |
| Custom CA TLS | TLS 1.3, h2 | Same | Same |
| Custom CA resumed sessions | 0/4 | 0/4 | 3/4 |
| Custom CA initial SETTINGS, ordered | 2=0; 4=2097152; 5=16384; 6=16384 | Same | Same |
| Connection WINDOW_UPDATE increment | 5177345 | Same | Same |
| PRIORITY frames/HEADERS priority | None observed | None observed | None observed |

The strict clients received two server NewSessionTicket messages on every
connection. The pooled custom-CA control offered PSK on connections 2-4 and the
server confirmed reuse. Default native trust used only a temporary
`SSL_CERT_DIR` hash directory; custom CA used `SSL_CERT_FILE`. No system trust
store was edited, and default ALPN was not changed to force HTTP/2.

HTTP/2 parsing/state used pinned `h2 4.3.0`, `hyperframe 6.1.0`, `hpack 4.1.0` in
an isolated Python environment. Tests ran with network disabled, 2 CPU and 1 GiB
memory. Request bodies, credentials, TLS key material and header values were not
persisted; the report contains only metadata, header names and boolean/count
observations.

Final observation report:
`.local/native-fingerprint/20260918T043423Z-fee0f7cd/report.json`

SHA256: `7b32df8332b4f4918052853cf9c2a36d63e010731b08d8387474fc08bb629d45`

The final helper also passed 25 HTTP/UDS regressions and 4 real WS socket tests.
Build evidence: `.local/native-local-build/20260918T042849Z-171e64e6/result.json`.

## Boundaries

This verifies the observed initial HTTP/2 settings and forced-reconnect TLS
behavior, not every server condition or a complete device fingerprint. The
target was a loopback IP, so production SNI, production routing/geography,
cross-platform identity, long-lived adaptive flow control, TLS ticket expiry
and reconnects across real CLI restarts remain separate questions. Default
native did not negotiate H2; H2 comparison fields are explicitly inapplicable
there. A completed observation run does not by itself mean parity; the report
has a separate `observed_resumption_parity` field.

## Rerun

```powershell
python tools/native_transport_fingerprint.py --native-run 20260918T042849Z-171e64e6 --prepared-image sha256:18fb13617777f6e1c228e2b6bda8c3ce19d87c9e5aace2d59ba1313e9e687723
```
