# Official CLI Compression Capture

Official Codex CLI 0.155.0 was executed in a Docker `--network none` namespace against a loopback fixture. The test additionally rejects any active non-loopback network interface. Each case has a fresh HOME/CODEX_HOME and newly generated synthetic `auth.json`; no existing desktop authentication, real account, remote OAuth endpoint or public model was used.

## Source and Actual Gates

`core/src/client.rs:1450` enables zstd only when all three conditions hold: request-compression feature enabled, authentication uses the Codex backend, and provider `is_openai()`. `model-provider-info/src/lib.rs:526` compares the provider's display name with `OpenAI`; it does not require the provider map key to be named `openai`. `features/src/lib.rs:1231` declares request compression enabled by default.

The fixture uses a custom provider entry with `requires_openai_auth=true`, explicit loopback base URL and WS disabled. ChatGPT cases use synthetic JWT-shaped access/id tokens and a fresh last-refresh timestamp; the API-key case uses an explicit synthetic API-key auth mode. This demonstrates local CLI auth/compression branches, not authentication success against an external server.

| Case | Requests | Actual Content-Encoding | Actual Tool Follow-Up |
| --- | --- | --- | --- |
| ChatGPT + OpenAI name + enabled | 2 | zstd, zstd | Yes |
| ChatGPT + OpenAI name + disabled | 2 | absent, absent | Yes |
| API key + OpenAI name + enabled | 2 | absent, absent | Yes |
| ChatGPT + custom name + enabled | 2 | absent, absent | Yes |

All four cases passed, zero skipped, CLI exit 0, total 22.33 seconds. The compressed positive case measured 36,863 decoded bytes -> 13,659 wire bytes on its first request, and 37,844 -> 14,055 bytes on the tool-result follow-up. Different cases have independently generated context, so these are within-request size measurements, not a latency or cache-improvement comparison.

Only compression headers, byte counts/digests, schema keys, input/tool type counts and authentication-header-presence booleans persist. Zstd decoding and JSON parsing happen in memory. Tool arguments, prompt, output, JWT, account identifiers and auth files are not copied into the report.

## Immutable Evidence

- Run: `.local/native-compression/20260918T054235-af38b4/`.
- Official Linux CLI SHA256: `660e159a49e823ac8e5986cb238f73158ce4b957d40d9292f8de90862644b501`.
- Existing gateway source baseline: `57a7ded3c036485f44d7b96d8e659d2579cb05c15f49db10dc73ac6afe67ae90`.
- Baseline plus only the capture test: `e2d01877c47bb6d9d49a60afac288c2eb4d135ef61445ad3a7cd6e1a8b853668`, unchanged during compilation.
- Original capture test SHA256: `4b3ae75617b9d307627bf39ca45440318bca35b0613f510f0b1a27259d4a1f48`.
- `report.json` SHA256: `8760aa7feb2711e852d552286c9213bc1ef3e3fa97179a872f95f8d104cb528c`.
- `result.json` SHA256: `adc1218f61f6d471168e83a20ae8da9a595f497bc706700b8f3dc4a9435f3ee9`.

The earlier run `20260918T054010-63fbe0` also observed the four gates successfully, but shared source changed while its probe compiled. That result is retained and explicitly not used as the immutable source acceptance. A subsequent snapshot of incomplete concurrent management code did not compile and made no network requests. The final run above used the already verified baseline plus one test file and removed its container after completion.

## Native Gateway Chain Fixture

The current source test has since gained an optional fixture-only gateway callback. New `TestGatewayNativeActualOAuthCompressedCLI` uses it to connect the official synthetic-ChatGPT CLI through the Go gateway, actual Rust UDS helper and loopback upstream. It requires `CODEX_NATIVE_HTTP_TEST_BIN`, `CODEX_NATIVE_CAPTURE_BIN` and `CODEX_NATIVE_COMPRESSION_NETWORK_NONE=1`.

The chain fixture requires zstd on both request hops, actual tool follow-up, two Responses usage records, unchanged synthetic usage totals, and zero invocations of a deliberately failing Go fallback transport. Optional gateway prompt injection and compatibility normalization are disabled only for this fixture. Full JSON is canonicalized with `UseNumber` and hashed independently at ingress/egress, so instructions/input/tools/cache state must remain semantically identical without requiring identical compressed bytes or JSON key order. Independent model-catalog GETs are not counted as model turns.

The actual chain passed in the final combined native-helper run: all 11 assertions true, official CLI exit 0, two inbound and two outbound Responses requests, actual tool-result follow-up, identical per-request structured JSON digests, unchanged usage totals, and zero Go fallback calls. The CLI scenario completed in 5.610 seconds; the enclosing Go test passed in 5.78 seconds. Both incoming requests and both Rust-generated outgoing requests carried zstd. Independent model-catalog GET records remain separate from the two Responses turns.

This is additional actual chain evidence, not a relabeling of the direct four-gate capture:

- Run directory: `.local/native-transport-build/gateway-compare-20260918T055515-eb2d44/`.
- Final Go snapshot: `a6d5994719dca2ff1f9fcb1037ec5d858e47f1e7329ac82c64466f5dd5f027d0`.
- Release Rust helper SHA256: `6df426c0a8eb703929f888055192e09cec0d2f7eea9391f2ae891e1ca6213740`.
- `gateway-compression-cli.json` SHA256: `ed34cab4765750d748fffb760f7ef869814118e8c2aefdf4d30c7d6a1dad04ac`.
- Combined `result.json` SHA256: `72e1af4fbf41ac1b846c666b763fab584770dd1ac75d9b6f2261a836ed336f3b`.

The final combined run passed 18 actual native top-level tests without skips, exited 0 without OOM, and removed its network-disabled container. The compression chain used synthetic local authentication and HTTP loopback endpoints; it adds no real-account, public-network, model-inference or additional TLS claim.

```powershell
python tools/native_cli_compression.py --source-dir .local/source-20260918/gateway-overlay-20260918t052835-6936e4/source
```

The runner creates a fresh immutable Go snapshot plus the current capture test before compilation, verifies the official CLI artifact, and executes only loopback synthetic fixtures in a network-disabled container. The current test file may differ from the historical run; every rerun records its own effective source and probe hashes.
