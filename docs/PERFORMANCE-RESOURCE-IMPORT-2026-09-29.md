# Resource import: independent-commit pipeline (phase 4, pending PG execution)

## Scope and evidence

The target is `POST /api/accounts/import-resources`, including its supported 10000 combined group/proxy input limit. This phase does not change the frozen `af427` artifact or attribute its measurements to the new code.

The old proxy loop commits each resource separately, then calls `ProxyStore.List()` to read/decrypt every proxy and rebuild the runtime index. Restoring 10000 new proxies into an empty table consequently visits at least 50,005,000 proxy rows. Every group/proxy also required multiple serial database and invalidation round trips. This is a source-level work count, not a measured latency estimate.

Local Go compilation, non-PG eligibility/cache/file-mode resource tests and `go vet ./internal/server` passed. New PG tests are present but intentionally not executed while the separate frozen-artifact performance runner owns the database window. No phase-4 SLO claim is made yet.

## What changed

- Valid, independent, entirely new, unmapped inputs use a **32-item bounded PostgreSQL pipeline**. Each resource and its audit share one data-modifying CTE, followed by its own `SendPipelineSync`. In pgx v5.7.6 this is an implicit-transaction commit/error-recovery boundary. It is not one transaction around the request.
- A successful item is reported only after its synchronization acknowledgement. Later CHECK/unique/SQL failures cannot roll back earlier committed items. No new chunk is dispatched after cancellation. An in-flight transport failure is never retried automatically; already confirmed successes remain in the response.
- The fast path acquires one pool connection, prepares at most two statements, and sends one flush per bounded chunk instead of waiting for each SQL round trip. It retains one durable transaction per item; the resulting fsync/commit cost must still be measured at 10000 rows.
- Full existing proxy ciphertext validation runs once. Migration `018_proxy_resource_write_gate.sql` adds a shared transaction-level advisory gate before every proxy INSERT/UPDATE/DELETE statement. The importing connection holds the matching exclusive session-level gate across individual commits, so other ordinary application/direct SQL writers cannot alter validated proxy ciphertext mid-import.
- Lock order is **proxy-resource-import/name lock (981), then proxy-resource-write gate (982)**. It matches the existing restore transaction which acquires the import lock before an INSERT reaches its gate trigger. Fast-path lock acquisition is nonblocking; contention falls back to the original ordered implementation rather than holding locks in reverse order.
- The first successfully committed proxy publishes the fully validated cache; later successful commits publish one version-guarded entry, preserving credentials and disabled state. Aggregate invalidation is coalesced after dispatch rather than repeated for each item.
- Unknown enabled application triggers on groups/proxies/audit_logs, missing/disabled gate, corrupted existing proxy ciphertext, existing names, duplicate source IDs/names, mappings, invalid inputs and dry runs retain the old ordered path. In particular, corrupt existing ciphertext still permits the old committed-but-reported-failed proxy behavior; it is not silently converted to success.
- Session locks are explicitly released in reverse order before returning a connection. If unlock acknowledgement cannot be proved, the connection is hijacked out of the pool and closed. Cancellation does not return a live session-locked connection to the pool.
- File-mode resource preflight now reads redacted metadata without modifying the shared runtime proxy index. A dry-run previously risked clearing cached proxy passwords through the legacy `ListMetadata` backing slice.

The gate covers supported concurrent row writes. Administrator DDL, disabled integrity triggers and TRUNCATE are not concurrent application operations. TRUNCATE is deliberately not included in this gate: it acquires an incompatible table lock before its statement trigger, which would invert the batch gate/table lock order. No claim is made that arbitrary runtime schema changes preserve the fast-path invariant.

## Semantics and failure matrix prepared for the next isolated window

`admin_account_bundle_resources_pipeline_test.go` includes:

1. Actual HTTP group/proxy restore, ordered results, credential non-disclosure, encrypted storage, disabled/authenticated runtime index and per-item audit count.
2. `count(DISTINCT xmin)` across the new resources proving independent commits, not a hidden atomic bulk transaction; bounded pool acquisition count checks removal of the old per-item acquire pattern.
3. Middle-item CHECK failures in both groups and proxies, preserving successes before and after the failed item, original error classification and no audit for rolled-back rows.
4. Corrupted unrelated ciphertext: fast path declines before any write; original path retains committed-but-failed rows.
5. Unknown audit trigger: fallback preserves the trigger's per-item rollback and the remaining successes.
6. Dry-run, explicit mappings, duplicate IDs and invalid entry ordering.
7. Concurrent identical imports: no duplicate proxy names, one audit per durable resource, and consistent conflict results.
8. Concurrent ciphertext update observed waiting on the shared gate through `pg_locks`, not inferred from a fixed sleep; writer resumes after exclusive release.
9. Cancellation while a later row waits in a synthetic CHECK barrier: acknowledged first item stays committed and no later pipeline chunk is sent.
10. Authoritative `pg_locks` inspection for leaked session gates. Merely acquiring a recursive lock on the same pooled backend would not prove release.

## Real HTTP performance harness

`TestPerformanceResourceBundleImports` covers groups-only, proxies-only and mixed batches at **100, 1000 and 10000 total rows**, with default 3 samples each. It measures authorization, request parsing, SQL/commit work, complete TCP response read and response decoding. Post-response checks verify full resource/audit counts, distinct item transactions and exact runtime proxy state. A deny-all provider transport confirms no upstream requests.

The harness emits `ACCOUNT_WRITE` records with request size, counts, full sample latency, pool acquisitions, `functional_only`, interference and `slo_pass`. Formal mode retains the strict `<1000 ms` threshold. A slow sample remains a failure, not a successful narrowed workload.

Run only in the isolated test database window with its normal encrypted test credentials and Redis fixture:

```powershell
$env:PERFORMANCE_SLO='1'
$env:PERFORMANCE_SLO_SAMPLES='3'
$env:PERFORMANCE_RESOURCE_IMPORT_SIZES='100,1000,10000'
go test ./internal/server -run '^TestPerformanceResourceBundleImports$' -count=1 -timeout=20m -v
```

Before performance acceptance, run the new `TestPGManagedResourcePipeline*`, `TestPGManagedResourceWriteGateBlocksConcurrentCorruption`, existing `TestPGManagedBundleResourceRestore` and the full affected management regression suite against the same phase-4 source/binary. Existing phase-3 reports do not verify this implementation.
