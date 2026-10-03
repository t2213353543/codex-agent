# Non-Billing Account Management Audit

Scope: current account management UI and its connected management APIs, compared with `../sub2api/current-source`. Billing is excluded by user instruction. Gateway routing, scheduling decisions, cooldown mutation and credential acquisition behavior were not changed by this frontend task. All browser requests used synthetic local fixtures.

## Reproduced and Fixed

| Problem | Original reproduction | Current behavior and evidence |
| --- | --- | --- |
| Ambiguous scheduled creation could produce two enabled plans | Store the first `POST /accounts/{id}/schedules`, drop its response, then press Save again. The former UI sent a second unrelated creation; the original server generated another ID. | `AccountSchedulesDialog.vue` and `accountScheduleDraft.js` retain one `request_id` and exact submitted parameters. A session draft survives a page reload. Unknown results lock editing and permit only an explicit replay of the same creation. A conflict retains that draft; clearing it requires confirmation. The server owner implemented request identity replay and deletion tombstones separately. |
| History remained loading after refreshing plans | Hold `GET /account-schedules/{id}/results`, click Refresh Plans, then deliver a successful history response. Shared generation invalidated the history finalizer. | List and history now have independent generations and abort controllers. Repeated history requests cancel older requests. Both the overlapping list refresh and slow-old/fast-new history cases pass. |
| Extreme expiry URL crashed Accounts setup | Open `#/accounts?search=kept&expires_before=99999999999999`. Date normalization called `toISOString()` on an invalid Date and threw `RangeError`. | Invalid dates cannot throw. The page explicitly blocks the query, offers to clear invalid dates, and preserves valid search/status/other expiry fields. |
| Recent-request sorting was missing despite a display column | Reference `AccountsView.vue:1803` exposes sortable `last_used_at`, backed by `account_repo.go:1124`; the local sort whitelist did not include it. | The local selector supports `last_used_at`; changing its direction resets pagination. The column becomes visible without changing saved column preferences. Selection and export receive the same sort. Source labels distinguish durable PostgreSQL usage events from retained file details. Backend ordering keeps unknown timestamps last in either direction. |
| Browser-valid lowercase IANA names could fail the statistics API | `Intl` accepts `asia/shanghai`, but sending the lowercase original to Go does not resolve that zone. | Account daily statistics send the canonical IANA name returned by `Intl`. Numeric offsets, `Local`, surrounding whitespace and invalid zones remain rejected. |

## Verification

- Candidate: `.local/management-parity/operational-fixes-20260918/dist`.
- Build manifest SHA-256: `0e286edbfa6e169513ab62d8bdfa57552867e8a7a7ad3cdf664a140805ab36af`.
- Full account browser suite: 53 groups, zero page errors. It contains the previous 47 non-billing groups plus six new resilience/filter/sort groups.
- Focused Node checks: 20 passed across schedule draft identity, account filtering/batches and daily request/Token statistics.
- Eight additional scheduled-creation screenshots cover light/dark/ocean/mint at 1440 and 390 pixels. Existing 48 account screenshots were regenerated in the full suite. No baseline UI assets or earlier frozen package was replaced.
- Machine evidence: `accounts-no-billing-verification.json` and `account-browser/report.json` in the candidate directory; the additional visual report is `account-browser/schedule-visual-report.json`.

Repeat the browser suite with `MANAGEMENT_UI_DIST` set to the candidate dist, then run `node frontend/tests/management-accounts-no-billing-browser.mjs`. Output defaults to that candidate's own `account-browser` directory. Set `ACCOUNTS_CANDIDATE_DIR` before running `node frontend/scripts/record-accounts-no-billing-verification.mjs` to bind the report and sources to that candidate.

## Remaining Reference Differences

This audit does not claim literal parity with every reference switch. The reference also offers ID and status sorting (`account_repo.go:1109`, `:1112`), which are smaller management-only differences; they are not required to fix the reproduced workflows above.

Reference runtime actions remain outside this frontend task: recovering account error/cooldown state (`routes/admin.go:386`, `:398`, `:401`), scheduled automatic recovery (`scheduled_test_runner_service.go:133`), a separate schedulable flag (`AccountsView.vue:2013`), and linked Spark shadow accounts (`AccountsView.vue:2439`). These change eligibility or credential routing and cannot be equated with the local read-only runtime view. The local runtime endpoint reads cooldown/admission/settings facts without clearing state (`admin_account_runtime.go`). Non-OAuth duplicate/reset-quota actions and other-platform authentication paths were not reported as missing Codex OAuth features.

CRS atomic commit, account deletion/schedule cleanup and lease no-op updates were reviewed and corrected by the separate management backend owner. They are not counted as frontend fixes or as unverified frontend completion claims here.
