# Native IPC Capacity Validation

## Reproduced Failure

The Go adapter releases its local active slot when `Body.Close` closes IPC.
Rust observes EOF and releases the corresponding task permit asynchronously.
The old Rust accept loop accepted a replacement connection first, then called
`try_acquire_owned`; if old tasks had not yet released their permits, it reset
the replacement without contacting the upstream.

- Old helper SHA256: `19db53c3b78bca7a78b0f5a0b1c1b7243c680418edcccaa0afbf984924945a2c`.
- Probe: initial 32 streams succeeded; close all, then immediately submit 32 replacements without waiting for cancellation.
- First replacement batch: 30 HEAD frames, 2 `ConnectionResetError` failures.
- Evidence: `.local/native-refill-probe/20260918T032201Z-3b44427b/report.json`.
- Container exited 1, no OOM; network disabled, 1 CPU, 1 GiB memory.

## Fix And Verification

The accept future now acquires a permit before accepting a socket. Both waits
remain inside the existing cancellation-aware `tokio::select!`, so parent EOF
can interrupt a full-capacity wait. No per-overflow task or application queue
is created; waiting connections remain in the kernel socket backlog. The
32-task admitted limit is unchanged, and no request is retried or replayed.

- New helper SHA256: `253d88c2cd4bfa8c655ff7dfce6c2ed7b70dc7dc28567516269af9dc581b670f`.
- Frozen source SHA256: `9c43666c19a085242d50509e54c831533666ebc23d671898941dd2914edfffac`.
- Build archive SHA256: `79dbd53a4f141f2dfb48ae1563e3d4fe2cd878aa1dfe22f8f95261e30bad66ab`.
- `cargo clean -p codex-native-transport`, locked build, official dependency/feature validation, and all 24 loopback tests passed with zero skips.
- Independent repeat under the same 1 CPU, 1 GiB, network-disabled conditions: 11 batches of 32 streams, including 10 immediate close/refill transitions; 352/352 HEAD frames and no reset, 12.63 seconds.
- Evidence: `.local/native-refill-probe/20260918T032703Z-e86be60b/report.json` and `.local/native-local-build/20260918T032505Z-1cbad96e/result.json`.

This is synthetic local IPC evidence, not a live-account concurrency test or
additional TLS fingerprint evidence. Kernel backlog capacity remains bounded;
this change does not provide an unlimited queue or increase admitted work.

## Go Lifecycle Review

The reviewed adapter revision moves helper startup and shutdown outside the
pool mutex. Starting and closing reservations stay counted until the process
has exited. `Close` prevents further reservations before waiting for workers;
ready/finished channels publish completion to cancellable waiters. Socket
cleanup compares `os.SameFile` and requires socket type, preserving a replaced
path. The previously identified mutex blocking and replacement-path deletion
issues are resolved in that revision. No new concrete deadlock or goroutine
leak was found in the read-only follow-up review.

## Rerun

In an isolated Linux environment with the helper and test files available:

```sh
python3 native-transport/tests/refill_probe.py --binary /absolute/path/to/codex-native-transport --report /tmp/refill-report.json --loops 10
```
