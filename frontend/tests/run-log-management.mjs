import assert from 'node:assert/strict'
import { createServer } from 'node:http'
import { mkdir, readFile, writeFile } from 'node:fs/promises'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { chromium } from 'playwright/test'
import { installMockApi } from './mock-api.mjs'
import { testLogsManagement } from './log-management-browser.mjs'
import { testLogCleanup } from './log-cleanup-browser.mjs'
import { testLogDownloadSession } from './log-download-session-browser.mjs'
import { testLogErrorReview } from './log-error-review-browser.mjs'
import { testLogIpGeo } from './log-ip-geo-browser.mjs'
import { testLogObservations } from './log-observation-browser.mjs'
import { testLogsNoBilling } from './log-no-billing-browser.mjs'
import { testLogAnalyticsBoundaries } from './log-analytics-boundaries-browser.mjs'
import { testLogWarmup } from './log-warmup-browser.mjs'
import { testLogSeverity } from './log-severity-browser.mjs'
import { testLogPassiveObservations } from './log-passive-observation-browser.mjs'
import { testLogIngress } from './log-ingress-browser.mjs'

const root = path.resolve(fileURLToPath(new URL('../..', import.meta.url)))
const arguments_ = process.argv.slice(2)
const option = name => { const index = arguments_.indexOf(name); return index < 0 ? '' : arguments_[index + 1] || '' }
const dist = path.resolve(root, option('--dist') || (arguments_[0]?.startsWith('--') ? '' : arguments_[0]) || '.local/logs-improvement/dist')
const output = path.resolve(root, option('--output') || '.local/logs-management/browser')
await mkdir(output, { recursive: true })
const server = createServer(async (request, response) => {
  try {
    const pathname = decodeURIComponent(new URL(request.url, 'http://127.0.0.1').pathname)
    const file = path.resolve(dist, pathname === '/' ? 'index.html' : pathname.slice(1))
    assert.ok(file.startsWith(dist + path.sep))
    const body = await readFile(file)
    response.writeHead(200, { 'content-type': file.endsWith('.js') ? 'text/javascript' : file.endsWith('.css') ? 'text/css' : 'text/html; charset=utf-8' })
    response.end(body)
  } catch { response.writeHead(404); response.end('Not found') }
})
await new Promise(resolve => server.listen(0, '127.0.0.1', resolve))
let browser, page
const report = { status: 'running', scope: 'operational_logs_without_billing', retired_suites: ['reference_pricing', 'time_pricing', 'procurement', 'sale_policy', 'cost_policy'], retirement_reason: 'Removed from the active workflow by the user request to remove all billing functionality.', dist, screenshots: output, errors: [] }
try {
  browser = await chromium.launch({ channel: process.env.TEST_BROWSER || 'chrome', headless: true })
  const context = await browser.newContext({ acceptDownloads: true, viewport: { width: 1440, height: 1100 }, reducedMotion: 'reduce', serviceWorkers: 'block' })
  const state = await installMockApi(context)
  state.authenticated = true
  page = await context.newPage()
  page.setDefaultTimeout(12000)
  page.on('pageerror', error => report.errors.push(error.message))
  await page.goto(`http://127.0.0.1:${server.address().port}/#/settings`)
  await page.locator('.settings-panel').waitFor()
  const suites = { behavior: testLogsManagement, cleanup: testLogCleanup, noBilling: testLogsNoBilling, analyticsBoundaries: testLogAnalyticsBoundaries, warmup: testLogWarmup, severity: testLogSeverity, passive: testLogPassiveObservations, ingress: testLogIngress, observations: testLogObservations, review: testLogErrorReview, geo: testLogIpGeo, session: () => testLogDownloadSession(page, state) }
  const selected = option('--suites') ? option('--suites').split(',') : Object.keys(suites)
  assert.ok(selected.length && selected.every(name => Object.hasOwn(suites, name)), 'Unknown log suite')
  report.selected_suites = selected
  for (const name of selected) report[name] = await suites[name](page, { screenshotDirectory: output })
  assert.deepEqual(report.errors, [])
  report.status = 'pass'
  console.log(JSON.stringify({ status: report.status, selected_suites: selected, assertions: selected.flatMap(name => report[name].assertions), screenshots: output }))
} catch (error) { report.status = 'fail'; report.failure = String(error.stack || error); if (page) { report.pageText = await page.locator('body').innerText(); await page.screenshot({ path: path.join(output, 'failure.png'), fullPage: true }) }; throw error }
finally {
  await writeFile(path.join(output, 'report.json'), JSON.stringify(report, null, 2))
  await browser?.close()
  await new Promise(resolve => server.close(resolve))
}
