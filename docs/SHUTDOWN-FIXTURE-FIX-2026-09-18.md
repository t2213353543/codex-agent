# Shutdown Fixture Repair

The formal `accounts_shutdown_test.go` now initializes the authoritative PostgreSQL admin key and starts from `defaultSettings()` with `AdminAPI=true`, matching the existing PG gateway test pattern. No production Go implementation, signal/drain behavior, token-refresh logic or test assertion changed.

This fixes the previously diagnosed fixture's pre-refresh 401/403. The original failure had not reached the refresh/disconnect/SIGTERM stages and was not evidence of lost rotating credentials. Historical failed diagnostic artifacts remain intact.

## Actual Verification

`TestPGShutdownPersistsRefreshAfterClientDisconnectAndSIGTERM` ran under Linux `-race` with independent local PostgreSQL 18 and Redis 8 containers: one top-level test passed, zero failed/skipped/missing, exit 0. It exercised the unchanged assertions for client cancellation during OAuth, SIGTERM stopping admission while allowing drain, rotated AT/RT persistence, and exactly one usage event/log/cumulative request.

Run: `.local/source-20260918/local-linux-race-20260918t033415-2f8e87/report.json`. Total setup/build/test/cleanup time: 68.028 seconds. PostgreSQL non-superuser and Redis ACL boundary checks passed; no host ports were published. Temporary containers/network/credentials were cleaned. No production traffic or deployment was performed.

## Immutable Handoff

- Original Go baseline: `956fc90be91a922ac1593b283102bc5149674dac337564e6023479ec8e60a6f9`, unchanged.
- Formal repaired `accounts_shutdown_test.go` SHA256: `88df21f151a9cdce3e6967d85d41463731d03ea38f42af172f700bed2937a9a3`.
- Baseline plus **only this test file** Go hash: `17731bed22af18ee5426f625e4e04ea08bf9f9a778cffd3554bd9973a6e437d9`.
- Frozen directory: `.local/source-20260918/shutdown-fixture-fixed-20260918t033414-18971e/`.
- `source.tar.gz` SHA256: `f9c521797cb837dac85ce0da0ac62c490cf871a584309a48dee9df845b46114f`.
- Race log SHA256: `0a3b3aca1d0a1fe574ea658854fc3a03ef1eaf5c9b16835d354c689de0eb407c`.

`fixture-manifest.json` records the single-file difference and verifies the formal test differs only by the reviewed initialization block. This is a passing repaired fixture, not a claim that the earlier unchanged 956 test or every remaining PG test passed. No new production binary is needed for a test-only repair; release evidence should identify both the unchanged production source and this explicit fixture overlay.

```powershell
python integration/gateway_environment/shutdown_fixture_check.py
```

The rerun uses the existing local Docker runner with a process-local single-test selector; it does not edit the general race selector, any previous frozen archive or other sessions' resources.
