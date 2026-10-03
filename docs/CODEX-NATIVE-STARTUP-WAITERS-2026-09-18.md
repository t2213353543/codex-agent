# Shared Startup Waiter Cancellation

This local test-only investigation reproduced a shared native-helper startup lifetime bug. It uses the existing self-executing synthetic helper and loopback IPC; no accounts, public endpoints or model inference participate.

## Before the Repair

The first caller's context owned the in-progress helper startup. A second caller could already be waiting for the same route's ready notification when cancellation of the first caller terminated the shared startup. The surviving caller then failed with `native HTTP helper unavailable` despite its own context remaining live.

`TestGatewayNativeStartupFirstCallerCancellationKeepsOtherWaiter` reproduced that failure before root changed production code: FAIL in 0.05 seconds. Its command-factory barrier ensures startup is paused, and a fixture-only Context.Done notification proves the second caller has entered the shared wait. The pool is checked under its mutex to contain exactly one starting reservation. No scheduling sleep is used to infer the race.

## Verified Boundaries

Root moved startup to a pool-owned bounded context and tracks its current waiters. The new tests independently verify:

- Creator cancellation returns that caller's `context.Canceled`, while the other waiter receives the original shared helper and completes an echo. Only one startup command is constructed.
- Cancellation of the second waiter leaves the creator's startup active with one waiter.
- When both waiters leave, the startup is marked abandoned/retired but retains its reservation until cleanup. A late caller waits for `finished`, then obtains one replacement helper and completes an echo. The configured process limit remains one.
- Pool.Close signals closure but retains an in-progress startup reservation until its controlled command barrier is released and cleanup finishes. New admission fails and the pool ends empty.
- In four deterministic ready/cancel ordering checks, simultaneous readiness cannot replace the caller's cancellation result or corrupt waiter counts. Already-ready healthy state is not incorrectly marked abandoned.

Only surviving requests open IPC. Cancelled acquire calls never receive a connection on which upstream request bytes could be sent; these tests do not claim OS-wide exactly-once execution. The first abandoned attempt can be cancelled before `exec.Start`, so its cleanup is verified through the reservation/finished lifecycle, not an invented PID or process exit.

## Results

Five new top-level tests repeated three times: PASS, 0.665 seconds. The same tests plus existing concurrent pool creation and unrelated-route slow-startup tests repeated three times: PASS, 1.188 seconds.

```powershell
go test . -run '^TestGatewayNativeStartup(FirstCaller|SecondCaller|AllCancel|CloseKeeps|ReadyAnd)|^TestGatewayNativeHTTPConcurrentPoolCreation$|^TestGatewayNativeHTTPSlowStartupDoesNotBlockOtherRoutes$' -count=3
```

Test file SHA256: `6980947943da1ca5bc000fdb208661cb10f0db4e3cc4afff02c3c5133acaaa57`.

These focused local runs are not race-enabled or sealed-Rust-helper acceptance. Root's final immutable Linux race and actual-helper report must cover the production fix separately. No helper fixture or production implementation was modified by this test owner.
