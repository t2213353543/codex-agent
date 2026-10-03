# Cache accounting and streaming reliability — 2026-09-28

This change separates reported cache creation from cache reads and fixes two reproduced gateway protocol failures. The production mode, account priority, weights and concurrency are preserved.

## Reproduced failures

- Compaction: the default cache-key filler ran after `bps.Prepare` had reduced compact requests to `model`, `input`, and `metadata`. It reintroduced `prompt_cache_key`, producing actual HTTP 422 in both HTTP and WebSocket CLI compaction. The filler now skips compaction. Real HTTP testing subsequently returned encrypted compaction and correctly resumed the previous marker.
- Terminal errors: BPS `type:error` without status was ignored by the official Responses client. A real WebSocket test recorded the upstream error in about 1.6 seconds but the client waited 301 seconds. The adapter now emits the equivalent `response.failed` terminal for HTTP; WebSocket rate-limit failures use the official wrapped 429 form. Provider error details remain on the wire; logs retain known machine codes without arbitrary provider text. Successful output, model identifiers and usage are not fabricated.
- Valid `stream_options.include_usage` is handled as a downstream control. It is not forwarded to BPS, and actual terminal usage remains available.
- Locally rejected requests retain the requested model and validation phase. They do not acquire a fictitious returned model.

## Cache semantics

`cached_tokens` represents cache reads. `cache_write_tokens` represents independently reported cache creation. Creation is never inferred from input minus reads and is never added to the hit numerator.

The read ratio uses input from the same requests that report usable read counts. Explicit zero remains zero. Missing counts remain unknown, including historical records without a known flag and with zero reads. Sample counts and unknown counts accompany the ratio. The dashboard, records, statistics and exports use separate read/create fields. Model catalog requests and WebSocket warmups are excluded from inference cache accounting.

BPS now records hashes and prefix-length observations from the final outbound request after account binding. It does not persist prompt text or cache keys. Because BPS moves instructions and tools into `input`, `shared_input_wire_prefix_bytes` and `input_history` are the relevant prefix evidence; a top-level configuration hash alone does not establish prefix reuse.

## Evidence boundaries

The natural production traffic exceeded 70% reads before this release; that observation must not be credited to an undeployed change. The 09:53:59 UTC snapshot reported 43,225,212 reads / 61,202,172 input tokens (70.6269%), independently reporting 17,783,316 created tokens. Later windows may differ with workload.

The official CLI 0.155 synthetic tests used a private network namespace with loopback only and fake credentials. Before the error fix, both error shapes exceeded the eight-second test deadline. After the fix, HTTP and WebSocket error cases finished in 208–246 ms, with one upstream fixture call and no HTTP fallback. Evidence: `.local/bps-ws-error-fixture/20260928T094920Z/`.

Final release evidence and live results are under `.local/cache-reliability-20260928/`, `.local/reliability-modes/live/20260928T095356.163677Z/`, `.local/powerpoint-migration/race/20260928T095356.170412Z/`, and `.local/cache-observation-pg-linux/20260928T095356.153222Z/`. A running test or prepared artifact is not a deployed release.

## Continued monitoring

The minute observer separately tracks admitted requests, control requests and retained pre-admission failures. It reports observed read/create samples independently. Read-only rerun:

```powershell
python tools/cache_rate_report.py --minutes 20
```

Unsupported upstream models, structured-output schemas, service tiers and reasoning levels remain explicit failures; they are not silently replaced or discarded. The compaction fix explains a reproducible 422 class, not every historical 422. Subsequent monitoring must distinguish remaining classes before claiming all errors resolved.

## Deployment receipt

Version 0.4.7 was activated at 2026-09-28 10:03:25 UTC. Release ID: `051eddd0c669e27f1d05e58c927a1615d2caddd191623e2c759d528198b23093`; running binary SHA256: `0132d7c32663871fc51902bfa091aaabd70a78cc5aa93c5e8b7ca0cb91ec3a7e`.

The final full HTTP and WebSocket suites passed, including real encrypted compaction followed by correct continuation. Both candy cases returned 21. The final Linux race/Redis run passed without source drift; all seven isolated PostgreSQL checks passed. Frontend verification included 195 passing unit tests, a production build, and three browser suites.

The deployed gateway accepted `stream_options.include_usage` and returned HTTP 200. Its reported 37,025 cache reads and 0 cache writes matched persisted fields, and final-request cache observation was present. Mode/settings/account concurrency, weight and priority were unchanged. At 10:05:44 UTC only the two release smoke requests existed after activation; this is not broad natural-traffic validation. The minute observer remains active. See `.local/cache-reliability-20260928/release-result.json` for the combined receipt.
