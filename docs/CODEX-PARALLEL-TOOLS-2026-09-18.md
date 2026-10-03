# Official CLI Parallel Tool Verification

## Result

The unmodified official CLI 0.155.0 executed two real `exec_command` calls in a single scripted response over each native transport: TLS HTTP/2 and TLS WebSocket. Both scenarios passed. No real account, model service, desktop authentication data, or production deployment was used. The comparison container had `--network none` and fresh OS/CLI homes.

Only `gateway_native_cli_parallel_test.go` and the comparison runner/report tests were added or changed. The frozen Go source is `38a94373a0bfc64106564d574d407459861be5f71c91784e3f7025f49876d931`, based on verified `bbb0ed103f7b526f1fb131071ee5aaad44fad658aaece85c0480824ccb2715c0`. All production source and both gateway binaries are unchanged. The sealed Rust helper remains `6df426c0a8eb703929f888055192e09cec0d2f7eea9391f2ae891e1ca6213740`.

## Source Contract

In the pinned official source under `.local/codex-source-0.155.0/codex-rs/core/src/`:

- `session/turn.rs:1517` enables the prompt's `parallel_tool_calls`; `client.rs:877` excludes Responses Lite when serializing that flag.
- `tools/handlers/unified_exec/exec_command.rs:142` declares execution parallelism. `tools/parallel.rs:107` reads the actual registered tool capability, and line 148 selects a shared read lock for parallel tools rather than an exclusive write lock.
- Each `response.output_item.done` dispatches its own tool future. This does not require a fabricated parallel tool or an extra field in the advertised tool schema.

## Local Workflow

Each scenario reads the CLI's actual function tool schema and sends two distinct calls in one response. The CLI, not the fixture HTTP server, launches the worker processes.

Each worker exclusively creates its own start file. Worker A waits for B's completed marker; B waits for A's start marker. The bounded rendezvous is 4 seconds, below the requested 10-second `yield_time_ms`. A serialized execution cannot silently yield a background process and pass: both returned tool outputs must contain final, expected exit codes.

| Evidence | HTTP | WebSocket |
| --- | --- | --- |
| Generated model requests | 2 | 2 |
| Actual tools and verified outputs | 2 / 2 | 2 / 2 |
| Worker interval overlap | 120,242,022 ns | 120,275,669 ns |
| Exit codes by issue order A, B | 0, 7 | 0, 7 |
| Worker completed-marker order | B, A | B, A |
| Separate zero-usage warmup records | 0 | 1 |
| Go fallback attempts | 0 | 0 |

Both paths preserve result `call_id`, the distinct output markers and exit codes, tool/instruction stability, and the thread cache key. HTTP additionally verifies the existing input prefix and exact function name/namespace/argument echoes. WebSocket verifies exact `previous_response_id` linkage and a two-output-only follow-up delta; it does not claim argument echo or full warmup-history reconstruction.

The two generated requests, not the number or exit status of tools, produce two successful model records per scenario. Synthetic usage is 80 input and 3 output tokens. A shell exit of 7 remains a tool result, not a transport/model failure.

## Verification

- Full Go tests passed in 31.224 seconds; `go vet` and Windows/Linux builds passed. Host-side opt-in skips are not used as evidence for actual CLI execution.
- The independent Linux runtime suite passed all 29 native top-level tests plus 1 Go-transport CLI test with 3 subcases. Seven mandatory reports passed; no runtime tests skipped, no OOM, and the container was removed. Root independently confirmed no comparison containers remained.
- Comparison-runner Python tests passed 6/6. A missing or false parallel report fails acceptance whenever its source test exists; older snapshots and the TLS-only suite retain their previous report contracts.
- Historical 472-test PG/Redis race evidence is inherited only for the byte-identical production and selected test files. It was not rerun and does not cover this newly added opt-in test.
- The first run remains recorded as failed: its old fixture incorrectly expected only two WS log records and omitted the warmup. Its successful overlap/exit checks were not promoted to overall success. The new frozen version corrected the fixture, not production behavior.

Rerun from the repository root:

```powershell
python tools/native_transport_compare.py --native-run 20260918T054243Z-b43907cb --source-dir .local/source-20260918/gateway-delta-20260918t103503-253f35/source --suite gateway --with-go-cli
```

Machine-readable evidence and hashes are in `CODEX-PARALLEL-TOOLS-2026-09-18.json`. The observations prove real CLI tool execution against a scripted local model response. They do not measure model reasoning, production request success rate, real cache hit rate, OS process-exit order, or geographic/device identity. Account-dependent tests remain cancelled as requested.
