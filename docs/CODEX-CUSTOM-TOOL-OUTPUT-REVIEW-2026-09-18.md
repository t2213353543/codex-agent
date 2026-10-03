# Custom Tool Output Review

Scope: local synthetic SSE input to the existing JSON response collector. No real account, public upstream, model inference or deployment was used. This does not assert that the official service normally emits conflicting or mixed-index item events.

Official CLI 0.155.0 parses `response.output_item.done` as a complete `ResponseItem` (`codex-api/src/sse/responses.rs:357`). Custom-tool items include `call_id`, `name`, optional `namespace`, and raw string `input` (`protocol/src/models.rs:1119`). Input deltas are separately dispatched; their final text must not be appended a second time to an already complete item.

## Reproduced Defect

A completed custom tool with ID `custom-one` first occupied an implicit index 0, then an event assigned the same ID explicit index 2. The collector retained both slots and could return the same executable call twice. The same problem occurred when one explicit ID moved from index 0 to 2. Existing tests covered a same-ID promotion to the same index but not this different-index case.

Pre-fix `go test . -run '^TestGatewayCustomTool' -count=1` produced three failures: duplicate completed call, absent index-identity conflict, and JSON gateway returning executable duplicated tool output instead of a failure. Two additional positive cases passed. This original command result was observed before root's production fix.

## Minimal Repair and Verification

Root added a guard before accumulator mutation: a previously observed nonempty item ID cannot claim a different output index. The fixed error is `upstream output item ID conflicts with an earlier output_index`. The collector does not guess a new location or silently move existing opaque output. Same-ID/same-index updates and implicit-to-explicit promotion at the same index remain valid; distinct item IDs with sparse/descending indices remain ordered correctly.

Five new tests repeated three times passed in 0.073 seconds. Combined new tests and existing `TestCollectedResponse*` tests repeated three times passed in 1.526 seconds. Coverage includes unchanged state after rejection, namespaces/custom input/large exact JSON numbers, no duplicate delta/done input concatenation, one upstream request with JSON HTTP 502 on conflict, and byte-identical streaming output HTTP 200 for the same source events.

```powershell
go test . -run '^TestGatewayCustomTool|^TestCollectedResponse' -count=3
```

This affects JSON collection only. SSE forwarding is unchanged and does not begin interpreting or rewriting tool content. Native CLI streaming execution and real `apply_patch` workflows require their separate root-owned fixture; these collector tests do not substitute for it.

Frozen test: `gateway_custom_tool_test.go`, SHA256 `7ee4d4a59bca7ff0a76ab5f308c4b3c88b30d4b92207bf026192aae3c8ff788d`.
Reviewed production file: `response_output.go`, SHA256 `9672fd631d8f66c7c52eacb42288a276685e9f17fed414715644f3eb4cc2b499`.
