# Passive Hook Review

Independent review added only `gateway_attempt_review_test.go`; runtime changes were made by the root/owning implementation agent. This evidence complements the standalone collector tests and root's hook tests. It does not claim live upstream inference or deployment.

## Verified Corrections

- A rejected WS upgrade or rejected OAuth response has both an HTTP response and a non-nil error. Error classification now preserves the observed upstream status instead of labeling these ordinary rejections as network faults. Explicit cancellation/deadline and known local guard errors retain their distinct priority.
- HTTP 200 with malformed OAuth JSON is an upstream protocol failure, not an unknown network fault.
- Error truncation now follows the actual structured diagnostic parser. A syntactically valid JSON string/array or incompatible field type can still enter its bounded plain-text fallback; `json.Valid` alone did not establish whether truncation occurred.
- Persisted diagnostics are truncated after redaction at 2000 bytes. The extracted existing-redaction helper reports that actual boundary, so a 3000-byte plain message is marked truncated, while a long bearer secret removed before the limit is not. Existing error text and redaction output remain unchanged.
- Model catalog HTTP 304 remains a successful validator result and is not marked as a failed attempt.

## Additional Coverage

The review exercises real local handlers for both header-identified and body-identified WebSockets. A first-turn 401 recovery produces exactly `websocket 401`, `refresh 200`, `websocket 101`; the second turn does not receive copied handshake attempts or header timing. Existing model-turn and usage-record counts remain unchanged.

An expired account whose refresh is rejected produces one actual refresh attempt and no inferred model send. Repeating the request while the existing failure cache applies produces no second network attempt. A rotating OAuth exchange that deliberately outlives its original cancelled request still records headers before delayed body parsing and completes normally, matching the existing refresh lifetime.

## Interpretation Limits

An attempt means the corresponding send/dial function was actually called. It does not prove the request reached or was accepted by the upstream: local transport/proxy guards, native capacity and cancellation may fail inside that function before any network response. Missing response headers/status remain nil.

Request preparation starts after gateway authentication/settings lookup and includes body reading plus preparation/policy. Account-selection duration includes existing `ensureReady` and thus may include OAuth refresh and lock waits. These intervals can overlap attempt durations and must not be summed as exclusive components of total request latency. No independent auth, DNS or TLS timing was inferred.

ErrorTruncated reports observed diagnostic capture/parser/storage bounds. It is not a guarantee that the upstream sent its entire original error body; a broken transport or the native WS fork's partial handshake tail can have a separate unknown completeness boundary.

## Result

```powershell
go test . -run '^TestGatewayAttemptReview' -count=3
```

PASS, 0.699 seconds, eight top-level tests repeated three times. The file SHA256 is `87674977183619a9a01c11b2644f12bfab7492454682a740d382c99d9b7cb8f3`.

Actual Linux race, final source hashes and native-helper interop belong to root's combined immutable acceptance. This local focused command does not substitute for that run.
