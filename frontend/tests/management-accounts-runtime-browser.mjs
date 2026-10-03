import assert from 'node:assert/strict'
import { createServer } from 'node:http'
import { readFile, writeFile, mkdir } from 'node:fs/promises'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { chromium, expect } from 'playwright/test'
import { account, installMockApi } from './mock-api.mjs'
import { testAccountsManagement } from './accounts-management-browser.mjs'
import { testAccountOperations } from './account-operations-browser.mjs'
import { testAccountLifecycle } from './account-lifecycle-browser.mjs'
import { testAccountResourceMigration } from './account-resource-migration-browser.mjs'
import { accountRuntimeFixture } from './account-runtime-fixtures.mjs'

const root = path.resolve(fileURLToPath(new URL('../..', import.meta.url)))
const dist = path.resolve(process.env.MANAGEMENT_UI_DIST || path.join(root, '.local/management-parity/dist'))
const output = path.resolve(process.env.MANAGEMENT_ACCOUNT_REPORT_DIR || path.join(root, '.local/accounts-parity/browser'))
await mkdir(output, { recursive: true })
const report = { status: 'running', synthetic_data: true, dist, started_at: new Date().toISOString(), errors: [] }
const server = createServer(async (request, response) => {
  try {
    const pathname = decodeURIComponent(new URL(request.url, 'http://127.0.0.1').pathname)
    const file = path.resolve(dist, pathname === '/' ? 'index.html' : pathname.slice(1))
    if (!file.startsWith(dist + path.sep)) throw new Error('invalid_path')
    const body = await readFile(file)
    response.writeHead(200, { 'Content-Type': file.endsWith('.js') ? 'text/javascript' : file.endsWith('.css') ? 'text/css' : 'text/html; charset=utf-8', 'Cache-Control': 'no-store' }); response.end(body)
  } catch { response.writeHead(404); response.end('Not found') }
})
await new Promise(resolve => server.listen(0, '127.0.0.1', resolve))
const browser = await chromium.launch({ headless: true, channel: process.env.TEST_BROWSER || 'chrome' })
try {
  const context = await browser.newContext({ viewport: { width: 1440, height: 1000 }, reducedMotion: 'reduce', acceptDownloads: true, serviceWorkers: 'block' })
  const state = await installMockApi(context)
  state.authenticated = true
  const page = await context.newPage()
  const observedAccounts = new Map()
  page.on('response', async response => {
    const pathname = new URL(response.url()).pathname
    if (response.status() !== 200 || response.request().method() !== 'GET' || !/^\/api\/accounts\/(manage|[^/]+)$/.test(pathname)) return
    try { const body = await response.json(); for (const row of body.accounts || (body.account ? [body.account] : [])) if (row.id) observedAccounts.set(row.id, row) } catch { /* Canceled fixture responses have no body. */ }
  })
  await context.route('**/api/accounts/*/runtime**', route => {
    const id = decodeURIComponent(new URL(route.request().url()).pathname.split('/').at(-2))
    return route.fulfill({ json: accountRuntimeFixture(observedAccounts.get(id) || account(id, '合成账号')) })
  })
  page.setDefaultTimeout(15000)
  page.on('pageerror', error => report.errors.push(error.message))
  page.on('console', message => { if (message.type() === 'warning' && /Failed to resolve|Invalid prop|Unhandled error|inject\(\)|lifecycle/.test(message.text())) report.errors.push(message.text()) })
  await page.goto(`http://127.0.0.1:${server.address().port}/#/accounts`)
  await page.locator('.accounts-page').waitFor({ state: 'visible' })
  const capture = async name => { await expect(page.locator('.el-message')).toHaveCount(0); await page.screenshot({ path: path.join(output, `${name}.png`), fullPage: true, animations: 'disabled' }) }
  report.assertions = await testAccountsManagement(page)
  report.operations = await testAccountOperations(page, { capture })
  report.lifecycle = await testAccountLifecycle(page, { capture })
  report.migration = await testAccountResourceMigration(page, { capture })
  const preview = Array.from({ length: 10 }, (_, index) => account(`preview-${index}`, index === 2 ? 'Codex 团队工作区 · example-engineering-production@example.test' : `Codex ${['主力团队', '日常开发', '备用账号', '个人工作区', '审核账号'][index % 5]} ${index + 1}`, { enabled: index !== 7, status: index === 7 ? 'disabled' : index === 3 ? 'expired' : index === 1 || index === 6 ? 'error' : 'ok', last_error: index === 1 ? '最近请求失败：HTTP 429，限流或额度不足' : index === 6 ? '最近请求失败：upstream overloaded' : '', has_refresh_token: index !== 4, expires_at: Math.floor(Date.now() / 1000) + (index === 3 ? -3600 : 86400 * (index + 1)), usage: { requests: 320 - index * 21, ok: 310 - index * 21, errors: 10, tokens: 123456 - index * 4300, input: 110000, output: 13456, cached: 88000, avg_latency_ms: 1900 + index * 310 } }))
  await page.route('**/api/accounts/manage**', route => route.fulfill({ json: { accounts: preview, total: 10, total_kind: 'exact', has_more: false, page: 1, page_size: 20 } }))
  await page.goto(`http://127.0.0.1:${server.address().port}/#/accounts`)
  await expect(page.locator('.account-directory-heading')).toContainText('共 10 个')
  if (await page.getByRole('button', { name: '筛选与排序', exact: true }).getAttribute('aria-expanded') === 'true') await page.getByRole('button', { name: '筛选与排序', exact: true }).click()
  await expect(page.locator('.el-message')).toHaveCount(0)
  report.screenshots = []
  for (const theme of ['light', 'dark', 'ocean', 'mint']) for (const width of [1440, 390]) {
    await page.setViewportSize({ width, height: 1000 })
    await page.evaluate(value => { document.documentElement.dataset.theme = value; document.documentElement.classList.toggle('dark', value === 'dark'); window.dispatchEvent(new CustomEvent('theme-change', { detail: value })) }, theme)
    await page.evaluate(() => document.fonts.ready)
    await expect(page.locator('.el-loading-mask:visible')).toHaveCount(0)
    await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))))
    assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1), `${theme} ${width}px overflow`)
    const screenshot = `accounts-${theme}-${width}.png`
    await page.screenshot({ path: path.join(output, screenshot), fullPage: true, animations: 'disabled' })
    report.screenshots.push(screenshot)
  }
  assert.deepEqual(report.errors, [])
  report.status = 'passed'
  await context.close()
} catch (error) { report.status = 'failed'; report.failure = error.stack || error.message; throw error }
finally { report.finished_at = new Date().toISOString(); await writeFile(path.join(output, 'report.json'), JSON.stringify(report, null, 2)); await browser.close(); await new Promise(resolve => server.close(resolve)); console.log(JSON.stringify({ status: report.status, assertions: report.assertions?.length || 0, report: path.join(output, 'report.json') })) }
