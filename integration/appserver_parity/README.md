# App Server request capture

`capture_proxy.py` is an experiment-only CONNECT proxy addon and runner, pinned to
`mitmproxy==12.2.3`. It does not change the installed Codex binary or the running
gateway. Each experiment owns its proxy process, temporary CA, private directory,
and client environment. Real-account capture is supported only in a POSIX directory
owned by the current user with mode exactly `0700`. Raw files are created `0600`.
Windows is enabled only for generated synthetic fixtures.

The proxy observes HTTP request entity bytes, content-encoding, a separately decoded
zstd entity, response entity chunks, and both directions of WebSocket traffic. The
manifest contains hashes/lengths and header names with hashed values; original URLs,
headers, bodies, cookies and token values remain only in `private/`. Do not export
that directory, CA directory, or configuration from the experiment host. Export only
the sanitized manifest and explicit synthetic-prompt comparison results.

The request hash is over HTTP entity bytes after HTTP transfer framing removal,
before content-encoding decompression. The decoded hash is over zstd output. Response
chunks are streamed immediately and retained with their original content-encoding;
SSE is not buffered until completion. For a streamed response, `complete:true` is
written only by the response-completion hook. Process termination without that hook
does not produce a successful completion record.

WebSocket TLS plaintext frames are forwarded unchanged after the HTTP upgrade.
Each side has a raw byte file and indexed frame offsets, hashes, opcodes, masking,
FIN and RSV1 bits. Incoming messages are independently unmasked, reassembled and
deflate-decoded into private artifacts. Ping/Pong/Close and fragmented messages are
not rewritten by the capture layer. A narrow version-guarded override of
`WebsocketLayer.relay_messages` implements this boundary. Normal mitmproxy WebSocket
message forwarding is intentionally bypassed: the synthetic test demonstrated that
its bundled wsproto 1.3.2 mishandles Ping between compressed message fragments. The
fixture separately handles Ping to exercise this case without inheriting that bug.

TLS connections and HTTP/2 framing are terminated and recreated by MITM. These
artifacts cannot establish the original ClientHello, TLS session resumption,
HTTP/2 SETTINGS, TCP fingerprint or original HTTP header serialization. The original
hostname, HTTP Host and upstream SNI remain unchanged. Normal upstream certificate
verification remains enabled. This is a request-semantics experiment, not a wire
fingerprint parity experiment.

## Python runner

```python
from capture_proxy import CaptureProxy

with CaptureProxy(
    "/opt/codex-agent-integration/appserver-parity/run-unique",
    "/path/to/mitmdump",
    ["chatgpt.com", "*.chatgpt.com", "openai.com", "*.openai.com"],
    phase_file="/opt/codex-agent-integration/appserver-parity/output/phase.json",
) as capture:
    child_env = capture.client_environment(existing_isolated_child_env)
    # Launch only the test codex app-server / codex exec process with child_env.
    # The helper ignores environment proxies: configure its explicit proxy with
    # capture.proxy_url and its child CA environment with capture.ca_file.
```

`client_environment` removes inherited upper/lower-case proxy and CA overrides,
sets only the child process's proxies to loopback, clears NO_PROXY, and sets
CODEX_CA_CERTIFICATE to the temporary CA. It preserves the caller's other isolated
environment settings. No system trust store, account mode or global proxy is changed.

## Docker addon

The server can use `mitmproxy/mitmproxy:12.2.3` without installing host Python
packages. Mount only the task directory, retain the inspected image digest, and
start the container with host networking and the following addon configuration:

```json
{"directory":"/opt/codex-agent-integration/appserver-parity/run-unique","allowed_hosts":["chatgpt.com","*.chatgpt.com","openai.com","*.openai.com"],"synthetic":false,"phase_file":"/opt/codex-agent-integration/appserver-parity/output/phase.json"}
```

Set `CODEX_PARITY_CAPTURE_CONFIG` to that private JSON path. Invoke `mitmdump -q`
with `--listen-host 127.0.0.1 --listen-port PORT`,
`--set confdir=RUN/ca --set connection_strategy=lazy --set ssl_insecure=false`,
`--set termlog_verbosity=error --set flow_detail=0 -s capture_proxy.py`.
Create RUN and RUN/ca as `0700` before startup and suppress container stdout/stderr.
Wait for RUN/ready.json, then read RUN/ca/mitmproxy-ca-cert.pem. The optional phase
file contributes only bounded alphanumeric `route` and `case` labels.

Every observed CONNECT and upstream-connection attempt records host, port and the
allowlist decision. Unlisted destinations fail closed with a fixed error. Those
rejections are evidence of an altered experiment path, not successful capture of the
whole runtime. Proxy environment variables alone do not prove all process egress was
captured; subprocesses, direct sockets or ignored proxies require independent OS
network auditing. The manifest explicitly records `all_process_egress_proven:false`.
Do not equate a successful model response with complete-egress coverage.

## Synthetic verification

```sh
python capture_selftest.py --directory /private/new-selftest --mitmdump /path/to/mitmdump
```

The test uses only loopback listeners and generated certificates/tokens. A private
synthetic-only route maps `capture.invalid` to loopback while preserving Host/SNI.
It covers zstd double hashes, exact streaming SSE bytes, a rejected unlisted CONNECT,
WSS compressed/fragmented messages including interleaved Ping, all four captured
frame directions, byte-identical forwarding, decoded message hashes, and absence
of secret values/body text in the public manifest. On Linux it additionally checks
0700/0600 modes. It reports official CLI, App Server and native-helper environment
coverage as not executed; the real gateway pilot must supply those observations.

Local rerun from the repository root:

```powershell
.local/appserver-capture-venv/Scripts/python.exe integration/appserver_parity/capture_selftest.py --directory .local/appserver-parity-capture-selftest-unique --mitmdump .local/appserver-capture-venv/Scripts/mitmdump.exe
```

## Process-tree egress cross-check

The experiment server is CentOS Stream 9 x86_64. Its official Codex 0.155.0 is
static-PIE musl, so dynamic-loader interception is not an appropriate boundary.
A matching CentOS `strace-6.12-1.el9.x86_64.rpm` was downloaded with `dnf download`
from the configured official BaseOS/AppStream repositories, signature-checked,
and extracted into the task directory without installing a system package.

- RPM SHA256: `0c448be96bbbcde796ed4df01c153ca08d1bd059f335967482ebde43df723510`
- Extracted binary SHA256: `2fbb77e8e9948b8697df6f6cc73890279ff762f443f6c8e9d2b3c6d57efe6d5b`
- Binary: `/opt/codex-agent-integration/appserver-parity/strace-tool/root/usr/bin/strace`
- Synthetic evidence: `strace-tool/selftest-20260927-01/selftest-summary.json`

Wrap the isolated experiment executable before it starts any Codex/helper children:

```sh
umask 077
/opt/codex-agent-integration/appserver-parity/strace-tool/root/usr/bin/strace -f -qq -ttt -e trace=connect,sendto,sendmsg -e raw=sendto,sendmsg -o /private/run/private-net.log -- /path/to/isolated-test-executable
```

`connect` records destination sockaddrs and return status without request payload.
`-f` follows newly spawned descendant processes/threads. Raw `sendto,sendmsg` records
numeric arguments, including buffer pointers/lengths, without decoding payload.
This adds evidence that descendant connections use the expected local proxy and
reveals ordinary direct `connect` attempts. It does not decode destinations or DNS
names of unconnected UDP sends and does not establish coverage of preexisting
processes, io_uring, raw packet I/O or every possible egress method. Keep the log
private: Unix socket paths and process identifiers may be present. Compare its
destinations with CONNECT records; do not set an unconditional all-egress claim.

`egress_selftest.py` proved a spawned child's real loopback TCP connection is recorded
and its TCP/UDP synthetic payload arrives but appears neither in plain text nor hex
in the syscall log. All eight checks passed, including `0600` log permissions. The
official `codex --version` also completed under this extracted tracer without
calling a model or changing the installed Codex binary.

## Offline sanitized comparison

Run tools/appserver_parity_report.py on the capture server:

    python3 appserver_parity_report.py --run-dir /private/completed-run --output /private/completed-run/sanitized-report.json

Only the resulting sanitized JSON may be copied to the workspace. It retains
every observed HTTP operation, including plugin discovery/downloads, model
catalogs, account settings, inference and WebSocket upgrades. URL path segments
outside the fixed protocol vocabulary and all query values are hashed. Headers
retain names, value lengths and hashes. Body trees expose JSON paths, types,
lengths, hashes and a restricted list of known protocol/model values.

Inference comparisons align by recorded phase case and generation occurrence.
WebSocket generate:false warmups remain in the inventory. Exact
previous_response_id links are expanded using the completed prior input and
output on that same captured connection. Unresolved references remain marked;
the report does not guess prior context. Raw and expanded diffs are both retained.
WS envelope fields are separated in an explicit normalization record. Dynamic
identity/time candidates remain listed separately rather than disappearing from
the evidence. Positional array comparison is not semantic role alignment.

HTTP errors distinguish a completed response entity, a model terminal event before
HTTP entity termination, and an incomplete/unanswered operation. Identical repeated
requests are marked as retry-or-reinitialization candidates, never asserted to be
retries without a cause. A connect-syscall summary reports proxy destinations,
other loopback connections, nonloopback destinations and unparsed records.

The report proposes a review category/rationale for every operation group; it does
not automatically remove plugin/account requests or decide which network state is
unnecessary. Five synthetic tests cover secret redaction in values/keys/signed
URLs, explicit dynamic fields, duplicate-JSON rejection, direct-egress detection,
exact WS state expansion and model-complete-but-entity-incomplete classification:

    python -m unittest discover -s tools -p test_appserver_parity_report.py -v
