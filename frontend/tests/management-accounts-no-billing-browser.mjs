import assert from 'node:assert/strict'
import { createServer } from 'node:http'
import { createHash } from 'node:crypto'
import { readFile, writeFile, mkdir } from 'node:fs/promises'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { chromium, expect } from 'playwright/test'
import { account, installMockApi } from './mock-api.mjs'
import { accountRuntimeFixture } from './account-runtime-fixtures.mjs'
import { testAccountsManagement } from './accounts-management-browser.mjs'
import { testAccountOperations } from './account-operations-browser.mjs'
import { testAccountLifecycle } from './account-lifecycle-browser.mjs'
import { testAccountResourceMigration } from './account-resource-migration-browser.mjs'
import { testAccountRuntime } from './account-runtime-browser.mjs'
import { testAccountLeaseBackup } from './account-lease-backup-browser.mjs'
import { testExternalImportLeases } from './external-import-leases-browser.mjs'
import { testAccountsNoBilling } from './accounts-no-billing-browser.mjs'
import { testAccountScheduleResilience } from './account-schedule-resilience-browser.mjs'

const root = path.resolve(fileURLToPath(new URL('../..', import.meta.url)))
assert.ok(process.env.MANAGEMENT_UI_DIST, 'Specify the no-billing candidate with MANAGEMENT_UI_DIST')
const dist = path.resolve(process.env.MANAGEMENT_UI_DIST), output = path.resolve(process.env.ACCOUNTS_NO_BILLING_REPORT_DIR || path.join(path.dirname(dist), 'account-browser'))
await mkdir(output, { recursive: true })
const manifestPath = path.join(path.dirname(dist), 'build-manifest.json'), manifestBytes = await readFile(manifestPath), manifest = JSON.parse(manifestBytes), sha = bytes => createHash('sha256').update(bytes).digest('hex')
for (const [file, expected] of Object.entries(manifest.files)) assert.equal(sha(await readFile(path.join(dist, file))), expected, file)
const report = { status: 'running', scope: 'no_billing_account_management', synthetic_data: true, started_at: new Date().toISOString(), dist, manifest_sha256: sha(manifestBytes), static_files: Object.keys(manifest.files).length, errors: [], screenshots: [], retired_suites: ['account-cost-browser', 'account-cost-intervals-browser', 'account-cost-backup-browser', 'external-import-costs-browser', 'account-daily-stats-browser (money assertions replaced)'], retirement_reason: 'User explicitly removed all billing functionality; historical billing tests are not included in this result.' }
const server = createServer(async (request, response) => {
  try {
    const file = path.resolve(dist, decodeURIComponent(new URL(request.url, 'http://127.0.0.1').pathname).slice(1) || 'index.html')
    assert.ok(file.startsWith(dist + path.sep))
    const body = await readFile(file)
    const contentType = ({ '.js': 'text/javascript', '.css': 'text/css', '.html': 'text/html; charset=utf-8', '.svg': 'image/svg+xml', '.woff2': 'font/woff2' })[path.extname(file)] || 'application/octet-stream'
    response.writeHead(200, { 'Content-Type': contentType, 'Cache-Control': 'no-store' })
    response.end(body)
  } catch { response.writeHead(404); response.end('Not found') }
})
await new Promise(resolve => server.listen(0, '127.0.0.1', resolve))
const browser = await chromium.launch({ headless: true, channel: 'chrome' })
try {
  const context = await browser.newContext({ viewport: { width: 1440, height: 1000 }, reducedMotion: 'reduce', acceptDownloads: true, serviceWorkers: 'block' })
  const state = await installMockApi(context); state.authenticated = true
  await context.route('**/api/accounts/manage**', route => route.fulfill({ json: { accounts: state.accounts, total: state.accounts.length, total_kind: 'exact', has_more: false } }))
  const page = await context.newPage(); page.setDefaultTimeout(15000)
  const observed = new Map()
  page.on('response', async response => { if (response.status() !== 200 || !/^\/api\/accounts\/(manage|[^/]+)$/.test(new URL(response.url()).pathname)) return; try { const data = await response.json(); for (const row of data.accounts || (data.account ? [data.account] : [])) observed.set(row.id, row) } catch {} })
  await context.route('**/api/accounts/*/runtime**', route => { const id = decodeURIComponent(new URL(route.request().url()).pathname.split('/').at(-2)); return route.fulfill({ json: accountRuntimeFixture(observed.get(id) || account(id, 'synthetic')) }) })
  page.on('pageerror', error => report.errors.push(error.message))
  page.on('console', message => { if (message.type() === 'warning' && /Failed to resolve|Invalid prop|Unhandled error|inject\(\)|lifecycle/.test(message.text())) report.errors.push(message.text()) })
  await page.goto(`http://127.0.0.1:${server.address().port}/#/accounts`); await page.locator('.accounts-page').waitFor()
  const capture = async (name, locator) => { await expect(page.locator('.el-message')).toHaveCount(0); await (locator || page).screenshot({ path: path.join(output, name + '.png'), animations: 'disabled' }); report.screenshots.push(name + '.png') }
  report.management = await testAccountsManagement(page)
  report.operations = await testAccountOperations(page, { capture })
  report.lifecycle = await testAccountLifecycle(page, { capture })
  report.migration = await testAccountResourceMigration(page, { capture })
  report.runtime = await testAccountRuntime(page, { capture })
  report.lease = await testAccountLeaseBackup(page, { capture })
  report.external_leases = await testExternalImportLeases(page, { screenshotDirectory: output })
  report.no_billing = await testAccountsNoBilling(page, { capture })
  report.resilience = await testAccountScheduleResilience(page)
  assert.deepEqual(report.errors, []); assert.equal(sha(await readFile(manifestPath)), report.manifest_sha256)
  report.group_count = ['management','operations','lifecycle','migration','runtime','lease','no_billing','resilience'].reduce((total, key) => total + report[key].length, 0) + report.external_leases.assertions.length
  report.status = 'passed'; await context.close()
} catch (error) { report.status = 'failed'; report.failure = error.stack || error.message; throw error }
finally { report.finished_at = new Date().toISOString(); await writeFile(path.join(output, 'report.json'), JSON.stringify(report, null, 2)); await browser.close(); await new Promise(resolve => server.close(resolve)); console.log(JSON.stringify({ status: report.status, groups: ['management','operations','lifecycle','migration','runtime','lease','no_billing','resilience'].reduce((total, key) => total + (report[key]?.length || 0), 0) + (report.external_leases?.assertions.length || 0), report: path.join(output, 'report.json') })) }
