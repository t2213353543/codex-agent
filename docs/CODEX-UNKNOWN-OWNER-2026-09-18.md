# Unknown Owner Credential Boundaries

This supersedes the earlier known-owner report's decision to retain unknown-owner token-rotation affinity. Historical verification remains intact; the current implementation now rejects continuity that cannot be established after unknown-owner credentials change.

## Source and Scope

Official CLI 0.155.0 `login/src/auth/change_state.rs` requires a nonblank ChatGPT user and workspace, matching on both, to establish the same owner. `manager.rs:2493` compares actual auth/token data for credential changes, and `:2616` increments local owner generation when changed credentials cannot establish the same owner. `core/src/client.rs:1403` discards cached WS routing state on owner change.

Gateway complete-owner checks require nonblank `AccountID` and `Identity.UserID`. Complete known owners keep their existing stable ID/workspace/user reference through token refresh. Incomplete owners append a deterministic domain-separated SHA256 digest over the exact AT, RT and ClientID. A known user without workspace is still incomplete. Metadata version, credential version, display name, notes and expiry are not digest inputs. Raw credentials are not stored in the reference or diagnostics. The reference derives identically from JSON and PG state and across process restart.

The implementation separates `gatewayAccountOwnerRef` (management ID plus known static claims) from `gatewayAccountRef` (conversation continuity). The request-local proxy memo uses static ownership so a valid initial expiry refresh does not switch proxies merely because its incomplete-owner credential digest changed.

## Refresh and Session Behavior

- An existing sticky binding whose unknown-owner credentials changed returns HTTP 409 and requires a new thread. Old bindings are not rewritten or migrated.
- Expiry refresh may persist and return a new credential snapshot during initial unbound selection. The initial/new thread binds only after that refresh, and inference uses the same selected proxy.
- For an already-bound thread, expiry refresh persists AT/RT first, then the changed full reference prevents the old history from being forwarded. A new thread can use the newly saved credentials without refreshing again.
- An explicit 401 can still perform the existing bounded same-account OAuth exchange. If incomplete-owner AT/RT changed, HTTP/WS recovery reports 409 after persistence and does not resend the POST or redial the WS. This also covers refresh-token-only rotation.
- Existing accepted WS turns recheck the account reference. Replacement credentials cannot silently authorize a later turn on the old socket.
- Legacy incomplete-owner bindings without a credential digest fail closed and require a new thread.

Known-owner credential rotation remains supported by the existing refresh tests. The standard synthetic `newTestProxy` fixture now supplies an explicit known user for tests whose intended contract is stable known ownership; dedicated tests explicitly clear that identity to exercise the new incomplete-owner rule.

## Important Remaining Difference

A deterministic credential digest is **not a monotonic owner generation**. If credentials change from A to B and are later restored exactly to A, the same digest can recur. It also cannot observe an intervening logout that leaves no persisted trace. The CLI's auth manager does retain these transitions. Closing that ABA/logout gap needs a separately persisted owner epoch or an authoritative invalidation mechanism, beyond a pure read-only derivation from current account state. This patch does not claim that full state-machine equivalence.

## Verification

Seven new non-PG top-level unknown-owner tests plus five existing known-owner tests passed three repetitions in 1.015 seconds. The existing expiry, 401 and account-identity regression set also passed in 0.594 seconds.

```powershell
go test . -run '^TestGatewayUnknownOwner|^TestGatewayOwnerIdentity' -count=3
```

The tests include AT/RT/ClientID changes, unchanged credentials across metadata edits and JSON reparse, user-without-workspace, old HTTP history rejection/new thread success, expiry persistence before 409, RT-only 401 persistence without replay, header/body-routed WS 409 with only one upgrade, and two-proxy initial expiry routing with sticky enabled/disabled.

`TestPGGatewayUnknownOwnerCredentialReplacementAndRestart` additionally uses real management reauthorization CAS, shared Redis and a fresh Proxy object to verify unchanged-credential metadata edits retain affinity but persisted replacement rejects old HTTP/WS bindings without rewriting Redis history. Root must execute it in the final isolated PG/Redis Linux race run; local environment skips are not counted as proof.

Frozen owned files:

- `gateway_sessions.go`: `92881be5a845eafbe6175d037254b7692e82516479e0d0230ca91ad43ada6cf0`.
- `gateway_unknown_owner.go`: `74e7f04d140bf6f4800ef6570287236d8062198fea49b862cefce8e82be8b3ff`.
- `gateway_unknown_owner_test.go`: `49cc3fcf549606674bad2df2e7be6346ff86e5c540c3c1cc5f3609fd2cf53937`.
- Updated known-owner expectation test: `45d1416b7760761af10baf1eb8b339686fd51785c8e04f4cb3d77f61cc1a99cf`.

Root owns the coordinated ensureReady/auth/retry/status/proxy-memo hooks and final immutable source snapshot. No account import/admin implementation or production account data was changed by this subtask.
