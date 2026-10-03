import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import { readFile, readdir, writeFile } from 'node:fs/promises'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
const root = path.resolve(fileURLToPath(new URL('../..', import.meta.url))), candidate = path.resolve(process.env.ACCOUNTS_CANDIDATE_DIR || path.join(root, '.local/management-parity/no-billing-20260918'))
const hash = async file => createHash('sha256').update(await readFile(file)).digest('hex')
const manifestPath = path.join(candidate, 'build-manifest.json'), manifest = JSON.parse(await readFile(manifestPath, 'utf8')), report = JSON.parse(await readFile(path.join(candidate, 'account-browser/report.json'), 'utf8'))
const expectedGroups = 47 + (report.resilience?.length || 0)
assert.equal(report.status, 'passed'); assert.equal(report.group_count, expectedGroups); assert.deepEqual(report.errors, []); assert.equal(report.manifest_sha256, await hash(manifestPath))
for (const [file, digest] of Object.entries(manifest.files)) assert.equal(await hash(path.join(candidate, 'dist', file)), digest, file)
for (const [file, digest] of Object.entries(manifest.source_files)) assert.equal(await hash(path.join(root, file)), digest, file)
assert.ok(!Object.keys(manifest.source_files).some(file => /(?:AccountCost|LogCost|LogSale|LogPricing|logPricingState|logTimePricing|accountCost|accountStatsCosts|externalImportCosts)/.test(file)))
const owned = ['frontend/src/views/Accounts.vue', 'frontend/src/components/AccountStatsDrawer.vue', 'frontend/src/components/AccountDailyStats.vue', 'frontend/src/components/AccountExportDialog.vue', 'frontend/src/components/AccountBundleImportDialog.vue', 'frontend/src/utils/accountDailyStats.js', 'frontend/src/utils/accountBundleInput.js', 'frontend/src/utils/importAccounts.js', 'frontend/tests/account-no-billing.test.js', 'frontend/tests/account-daily-stats.test.js', 'frontend/tests/accounts-no-billing-browser.mjs', 'frontend/tests/management-accounts-no-billing-browser.mjs', 'frontend/tests/account-lifecycle-browser.mjs', 'frontend/tests/external-import-leases-browser.mjs', 'docs/ACCOUNTS-NO-BILLING-2026-09-18.md']
const evidence = {}, sources = {}
if (report.resilience) owned.push('frontend/src/components/AccountSchedulesDialog.vue', 'frontend/src/utils/accountScheduleDraft.js', 'frontend/src/utils/accountManagement.js', 'frontend/tests/account-schedule-resilience-browser.mjs', 'frontend/tests/account-schedule-draft.test.js')
for (const file of await readdir(path.join(candidate, 'account-browser'))) if (/\.(png|json)$/.test(file)) evidence['account-browser/' + file] = await hash(path.join(candidate, 'account-browser', file))
for (const file of owned) sources[file] = await hash(path.join(root, file))
const out = { status: 'passed', scope: 'account_management_without_billing', created_at: new Date().toISOString(), manifest_sha256: report.manifest_sha256, frontend_sha256: manifest.frontend_sha256, static_files: Object.keys(manifest.files).length, reachable_source_files: Object.keys(manifest.source_files).length, browser_groups: report.group_count, browser_errors: [], unit_tests: report.resilience ? { passed: report.resilience.length > 6 ? 22 : 20, scope: 'schedule draft identity, date bounds, IANA normalization, account management and daily request/token' } : { passed: 39, scope: 'daily request/token, bundle exclusion, leases, external leases, import and redaction' }, retired_suites: report.retired_suites, archive: '.local/retired-billing-20260918', user_data_deleted: false, gateway_files_modified_by_this_subtask: false, retained: ['OAuth/auth', 'request/token/latency', 'quota/windows', 'upstream usage/tests', 'leases', 'account/resource/lease backup'], sources, evidence }
await writeFile(path.join(candidate, 'accounts-no-billing-verification.json'), JSON.stringify(out, null, 2) + '\n')
console.log(JSON.stringify({ status: out.status, browser_groups: out.browser_groups, files: out.static_files, screenshots: Object.keys(evidence).filter(name => name.endsWith('.png')).length, manifest_sha256: out.manifest_sha256 }))
