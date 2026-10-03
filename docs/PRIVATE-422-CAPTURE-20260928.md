# One-shot BPS 422 diagnostics

After the verified compaction fix in 0.4.7, production still reported HTTP 422 on ordinary `/responses` calls. At 10:23:02 UTC there were 13 such errors across two accounts and several clients, with prepared bodies between 7.7 KB and 10.4 MB. No post-upgrade compaction 422 was observed in that sample. The generic upstream message did not identify a rejected field, so neither request size nor cache identity is a proven cause.

Version 0.4.8 adds a normally inactive diagnostic hook at the actual BPS HTTP response boundary. The next armed HTTP 422 atomically claims a unique capture. Only its prepared request and the response bytes read by the normal error path are encrypted with the existing authenticated-encryption key. Transport authentication and cookie headers are excluded; the claim, ciphertext and metadata expire after seven days. The hook neither retries nor replays a generation. A request after the capture is not collected automatically.

The original reader is wrapped without pre-reading: returned bytes, read errors and close errors remain unchanged. Early closure is marked incomplete. Capture failures are terminal metadata rather than a retry of another account's request. Concurrent errors cannot overwrite the winning capture; copying a potentially large body happens only after the atomic claim succeeds. The running binary hash binds the operator's arm to the deployed implementation.

The operator is under `.local/latest-operations/capture-422-v047/capture_operator.py`. Its `--plan` mode does not read production. `arm --remote` verifies the binary marker and exact production Redis namespace. `status` reports metadata and expiry. `inspect` decrypts on the server and returns types, lengths, hashes, and sanitized validation paths. There is no replay API and no account/configuration writer.

Before integration, five real isolated Redis tests passed without skips: reader/close fidelity, encrypted exact capture, concurrent single winner, failed-capture status, and disabled/successful response invariance. Three operator privacy tests passed. Final 0.4.8 full Go, vet, front-end browser/build/unit, Linux race/Redis, seven real PostgreSQL, and full real BPS HTTP/WebSocket suites are bound by `.local/capture-release-20260928/acceptance.json`.

Capturing a rejected request is diagnostic progress, not proof that the remaining 422 cause has been fixed. A capture must be inspected before choosing a request transformation or reproducer.

## Activation

0.4.8 started at 2026-09-28 10:48:01 UTC, release `de8044eb7178c9ed174d20c06d1f23f9af2920913ea12bf2bfcf89d539e03833`, binary SHA256 `e3e1fdd26d3f988fa063bcb60c9d6e9ba22659c7afb650c1c878f97ce0ac5049`. The runtime remained in BPS mode with unchanged settings/account configuration. The release includes the independently verified frontend read-total correction.

Capture `f5e5e543aa43f75eeb651f146f75941c` is armed for that exact binary. At the first status read it was waiting, with no ciphertext yet. No failed generation has been invented to populate it. Check the live state before any future action:

```powershell
python -B .local/latest-operations/capture-422-v047/capture_operator.py status --capture-id f5e5e543aa43f75eeb651f146f75941c --remote
```

After `captured=true`, use the same command with `inspect` in place of `status`; the operator returns only the redacted structural report.
