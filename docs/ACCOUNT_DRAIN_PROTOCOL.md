# Account retirement drain v1 (candidate)

This protocol prevents automated quota retirement from revoking a Team member
while Codex Agent still has admitted model requests using that member. It does
not wait, retry or queue inference requests, change quota/rate-limit policy, or
modify account Enabled, ownership or credentials.

## Contract

All endpoints require the existing management administrator authentication.
The path ID is the **numeric compatibility ID**, resolved to the native account
ID by the existing Sub2API compatibility store.

`GET /api/v1/admin/capabilities` returns the normal Sub2API envelope:

```json
{"code":0,"message":"success","data":{"account_drain":"codex_agent_account_drain/v1","account_drain_scope":"in_flight_requests"}}
```

Only PostgreSQL + shared Redis mode advertises this capability. File mode does
not claim cluster guarantees and returns 503 for drain operations.

`POST /api/v1/admin/accounts/{id}/drain` requires:

```json
{"operation_id":"<64 lowercase hex>","expected_workspace_sha256":"<SHA256 of provider workspace ID>","expected_user_sha256":"<SHA256 of provider user ID>"}
```

The exact workspace and user identities must be known and match the current
stored account. An existing operation is idempotent only for the same operation
and owner. Replaced owners, unknown identities and competing operations return
409. `GET` on the same path recovers a committed operation after a lost response
and repairs its bounded Redis cache; it cannot replace the durable operation.

The response data contains `capability`, `scope: "in_flight_requests"`, numeric
`account_id`, `status`, `in_flight`, `owner_ref_sha`, current
`credential_version`, `drain_token` (64 lowercase hex), `remaining_safety_ms`
and `pending_tool_sessions: null`. The owner digest includes the stored owner
incarnation, preventing an A-to-B-to-A replacement from authorizing an old
deletion. Normal same-owner token refresh does not invalidate the operation.

`status: "draining"` with `in_flight: 0` is valid. It includes
`safety_reason: "lease_expiry_observation"` until zero has been observed for one
full lease TTL (currently 30 seconds). Redis TIME supplies both observations and
lease expiry scores. Every positive GET, POST or fenced DELETE observation
clears the durable zero timestamp. This observation interval applies only to
automatic retirement; it adds no inference wait. The caller checks again in its
existing rotation cycle and must not infer readiness from `in_flight` alone.

This interval is required because a missing Redis lease key does not prove the
old request has stopped: its independent expiry watchdog may still be alive.
Only `status: "drained"` with zero leases permits Team retirement. Unknown
future client-owned tool callbacks are not counted or fabricated as zero; they
remain bound to their original owner and are never silently migrated.

Final `DELETE /api/v1/admin/accounts/{id}` requires both:

```text
X-Codex-Drain-Token: <drain_token>
X-Codex-Owner-Ref-Sha: <owner_ref_sha>
```

The delete transaction locks current credentials/account, verifies the owner and
token, recounts valid leases and rechecks the safety interval. A missing token,
changed owner, active lease or incomplete observation returns 409. Non-draining
compatibility deletion retains its prior behavior, including deletion of
unreadable legacy credentials. Explicit native/batch administrator deletion
retains its separate existing semantics; this protocol fences automated quota
retirement through the compatibility endpoint.

## Storage and concurrency

Migration `027_account_drains.sql` adds at most one short row per existing
account, with `ON DELETE CASCADE`. No request bodies or historical operations are
stored. Its Redis fence uses the admission key's hash slot and expires after one
minute, even if deletion cleanup is interrupted. Redis fence eviction does not
reopen admission because PostgreSQL is authoritative.

Admission uses an account primary-key `FOR KEY SHARE NOWAIT` and primary-key
drain lookup until its existing Redis acquisition Lua finishes. Drain uses the
established credentials-then-account lock order, with `NOWAIT`. A drain does not
cancel a lease already acquired; existing renew scripts are unchanged. New
unbound selection skips draining accounts, while owner-bound calls fail
unavailable without selecting a different owner or producing 429.

Ordinary compatibility edits, management-lease updates, reauthorization and CRS
updates retain their original credential-then-account lock order and use
`FOR NO KEY UPDATE` for the immutable account ID. Their writer serialization is
preserved while admission's key-share lock remains compatible. Actual drain and
deletion still take `FOR UPDATE`; editing notes does not become a drain signal.

An aborted transaction can leave a short-lived cache fence. A new admission may
clear only the exact orphan token it read while holding the PostgreSQL lock.
A Redis command delivered after timeout cannot remove a newer drain token.
An idempotent drain read/write restores the current durable token under the
exclusive account lock.

The same admission covers management `stream`/`compact` checks (including the
official runtime branch) and CPA raw-proxy POSTs to the standard Responses,
Responses compact, Chat Completions and Completions endpoints. Catalog/usage
reads and unrelated raw-proxy calls retain existing behavior. A generic raw
proxy URL cannot be asserted to be an arbitrary vendor's generation endpoint.

## Release and verification

Prepare both Codex Agent nodes and migration before enabling the Loophub caller.
Every traffic-serving generation, including old WebSocket handlers, must use
this protocol before advertising the deployment as safe for automatic drain.
An older process lacking the PostgreSQL admission guard cannot participate in
the guarantee. No user held-seat or disabled-mother restoration is part of this
change.

Run the isolated synthetic PostgreSQL/Redis suite from the workspace root:

```powershell
python .local/root-cause-20260930/drain_pg.py
```

The suite exercises cross-instance leases, renewal, owner replacement,
response-loss recovery, real Redis key loss, delayed acquisition commands,
PostgreSQL deferred commit failure, zero-observation reset, management inference
admission, warm candidate selection and compatibility deletion. It never reads
or writes production account records and never calls the upstream provider.
