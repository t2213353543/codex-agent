# App Server CLI adapter: verification status

The existing mode name `official_codex_cli` is not proof that `codex exec` runs.
Its public Responses route uses the gateway's direct HTTP/native transport.
The official executable is used for the separate hosted-task API.

The opt-in `CODEX_APP_SERVER_CLI=1` adapter rebuilds plain client Responses
requests with the pinned 0.155.0 writer and the account's actual model catalog.
It does not launch an official App Server process or execute client tools.
The production default remains unchanged, but setting `CODEX_APP_SERVER_CLI=1`
now wires the optional `official_codex_cli` profile back into the running
service. Startup requires the pinned native HTTP/WS helper capabilities and a
captured identity profile; missing capabilities fail closed before listening.
This still does not start the official executable or a hosted agent.

## What has been measured

* Frozen official binary SHA256:
  `660e159a49e823ac8e5986cb238f73158ce4b957d40d9292f8de90862644b501`.
* Native App Server / native `codex exec` paired pilot:
  `20260927T102700.613738Z`. Same account and access-token hashes, model
  `gpt-6-astra`, reasoning `xhigh`, synthetic instructions and prompts.
* Both generated the requested first reply and recovered the marker on resume.
  Their first-turn context has the same text after normalizing only their
  isolated runtime directories; identities and timestamps are separate fields.
* App Server full run `20260927T102851.057254Z` completed dynamic tool return,
  manual compaction, post-compaction recall, and web search. Corrected native
  CLI full run `20260927T103209.804581Z` completed the MCP tool return,
  automatic compaction, recall, and web search. The first CLI full run is kept
  as a failed result because its MCP tool initially required approval.
* Actual `compaction_trigger` requests and subsequent `compaction` history
  items were observed for both native programs. Their public invocation
  mechanisms differ; a successful turn alone was not counted as compaction.
* Both programs encountered some empty HTTP 403 WebSocket handshake responses
  and subsequently used HTTP. There was no response error code establishing
  an authentication, quota or model-quality cause.
* Decoding the turn-metadata JSON found a real stable difference:
  native CLI included `thread_source: "user"`, while App Server did not.
  Originator/user-agent also differed. These fields remain in the evidence;
  the context-text comparison does not assert whole-request equality.
* Lightweight capture `20260927T103733.106984Z` recovered the synthetic memory
  and tool-result markers and preserved model/input/output/cached/reasoning
  usage. Its initial web-search check was a false positive: the answer said
  live search was unavailable, with no search call. Same-account standard
  HTTP run `20260927T104549.212356Z` produced a completed `web_search_call`
  and a citation, determining the separate standard-tool wire profile.
* Extended capture `20260927T104718.095538Z` verified strict named JSON output.
  The next request exposed unsupported auto-added `tool_choice.namespace`.
  Field injection and terminal-error-to-EOF defects have been fixed and
  covered with real HTTP-boundary tests. Its `execve` audit saw three native
  transport helper launches and zero official Codex executable launches.

These results do not establish a quality difference or a hidden model change.
Identical model labels are likewise not proof of identical server execution.
Raw request captures are private and contain credentials; sanitized reports
contain hashes, shapes, operation inventories and explicitly synthetic text.

## Adapter contract

Plain Responses requests use actual catalog capability flags for Lite,
reasoning defaults, encrypted reasoning, image detail and verbosity. The
writer's prefix UUIDs and captured header/body construction have an independent
executable-capture oracle. Explicit client tools, reasoning, schema names,
history and controls remain client owned. Custom grammar tools retain their
native type instead of becoming string-wrapped function calls.

The native WebSocket exchange performs a `generate:false` prewarm and uses
`previous_response_id` only after verifying the exact input prefix and stable
request properties. A 403/426 handshake or unavailable native WebSocket
capability can fall back once to HTTP before any generation is sent. An
ambiguous generation write or interrupted output never triggers replay.

Incoming requests already containing Lite `additional_tools` or an explicit
`previous_response_id` keep their original model input and tool definitions.
Their existing canonical turn metadata is mirrored by removing the measured
CLI-only `thread_source` field; no second instruction prefix is added.

Backend-owned `web_search`/`web_search_preview` use standard HTTP Responses
with top-level tools/instructions and no Lite header or prefixes. Client
functions/custom grammars in the same request remain declared. Separately,
the native client's standalone `web.run` can call `/v1/alpha/search`; the
gateway forwards its JSON unchanged to `/backend-api/codex/alpha/search`,
with the existing account authentication, priority, weight and concurrency.
It neither selects search queries nor runs a local search loop.

## Differences that remain explicit

* This client-owned tool profile does not inject undeclared runtime functions,
  clock, collaboration tools, server environment or server sandbox rules.
  The source-derived context builder exists for explicitly supplied profiles;
  it is not automatically applied to a different client's environment.
* Native default CodeMode tools and the hosted bridge's forced direct tools
  are different reference profiles. Native web search also generated
  `/backend-api/codex/alpha/search`; it is not equivalent evidence for a plain
  Responses `web_search` declaration.
* Cross-HTTP socket reuse now requires verified input/output prefix hashes,
  current account/proxy authorization and successful delivery/owner CAS.
  New turns reset turn-state tokens. No complete conversation is retained.
* Captures cover the exercised features, not every possible conditional
  Guardian, memory, realtime, telemetry, plugin and authentication operation.
* TLS interception reconstructs TLS/HTTP transport. The capture does not prove
  an identical TLS fingerprint or exhaustive coverage of every OS egress API.

The adapter remains opt-in until the functional and operational regression
matrix is complete. It must not be described as perfect App Server parity.

Earlier candidate verification: run `20260927T105836.078019Z` passed 14 live
cases, including an actual hosted search call/citation and 12 standalone search
results. The same sources passed 49 offline Linux race tests with zero skips.
`reports/final-functional.json` is the authoritative reviewed report; the
earlier `lightweight-functional-final.json` is retained as an initial review
with an incorrect compact-model assertion. No production mode was changed.

The next revision adds account/key/proxy scoped connection reuse, explicit
prewarm metadata, native metadata mirroring, captured App Server identity,
image/memory auxiliary JSON forwarding and Guardian WebSocket endpoints.
Its pilot `20260927T114002.409532Z` completed first turn and resume on one WS
handshake. The same frozen sources passed 128 offline Linux race tests.
The complete functional rerun is recorded separately; older evidence does
not stand in for changes to the connection lifecycle.

Set `CODEX_APP_SERVER_IDENTITY_FILE` to a snapshot created once with
`tools/capture_appserver_identity.py` and the pinned official binary. Capture
uses a network namespace and only `initialize`, without login or generation.
Per-request inference does not start that binary. The target snapshot's UA
and originator matched the real bridge App Server capture exactly.

The latest candidate also negotiates `unbounded_payload_v1` and
`websocket_message_v2` with the native helper. This removes the prior local
32 MiB body/message ceiling through ingress, serialization, compression, IPC,
WS/SSE and response assembly. The account concurrency setting remains active.
It requires the new helper; an older helper returns a local capability error
instead of silently accepting a request it cannot carry. Realtime uses the
separate `websocket_no_compression_v1` capability because its official writer
does not negotiate the Responses permessage-deflate extension.

Latest live evidence: `20260927T123407.756914Z`, 14 functional cases passed,
including two real web searches, a citation and actual returned cache usage.
Report: `.local/appserver-parity/reports/functional-unbounded-realtime-reviewed.json`.
The initial report is retained: it rejected the valid `npm i` alias, corrected
after checking npm's own help and the unchanged captured output. This was an
audit-parser correction, not a replayed model response.

## Rerun

```powershell
python -B tools/appserver_parity_capture.py --route official_codex_cli --lightweight --native-helper /opt/codex-agent-integration/checks/native-unbounded-20260927T121556Z/work/target/release/codex-native-transport --native-helper-sha256 361b0df32ce0e74d5a07bad697abf49cb59e295eb1795797ae4c2a18548948ee
```

The runner freezes and hashes the current Go test executable, captures only
isolated synthetic traffic, and leaves production settings unchanged.
The explicit helper override selects the tested candidate without replacing
the production helper. Do not enable this profile against an older helper
that lacks the negotiated payload and Realtime compression capabilities.
