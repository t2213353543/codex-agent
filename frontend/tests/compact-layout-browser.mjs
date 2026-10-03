import assert from 'node:assert/strict'
import { createServer } from 'node:http'
import { mkdir, readFile, writeFile } from 'node:fs/promises'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { chromium, expect } from 'playwright/test'
import { installMockApi, account } from './mock-api.mjs'

const directory = fileURLToPath(new URL('../', import.meta.url))
const root = path.resolve(process.env.MANAGEMENT_UI_DIST || path.join(directory, 'dist'))
const baseline = process.argv.includes('--baseline')
const output = path.resolve(directory, 'test-results/compact')
await mkdir(output, { recursive: true })
const server = createServer(async (req, res) => {
  try {
    const pathname = decodeURIComponent(new URL(req.url, 'http://localhost').pathname)
    const file = path.resolve(root, pathname === '/' ? 'index.html' : pathname.slice(1))
    if (path.relative(root, file).startsWith('..')) throw new Error('outside build')
    const raw = await readFile(file)
    res.writeHead(200, { 'Content-Type': ({ '.html': 'text/html', '.js': 'text/javascript', '.css': 'text/css', '.svg': 'image/svg+xml' })[path.extname(file)] || 'application/octet-stream' }).end(raw)
  } catch { res.writeHead(404).end() }
})
await new Promise(resolve => server.listen(0, '127.0.0.1', resolve))
const browser = await chromium.launch({ channel: process.env.TEST_BROWSER || 'chrome', headless: true })
const report = { baseline, passed: false, pages: {}, errors: [], screenshots: [] }
try {
  const context = await browser.newContext({ viewport: { width: 1440, height: 1000 }, reducedMotion: 'reduce' })
  const state = await installMockApi(context)
  state.authenticated = true
  state.accounts = [account('compact-1', 'Primary account'), account('compact-2', 'Second account')]
  const now = 1790487645
  await context.route('**/api/diagnostics', route => route.fulfill({ json: { version: '0.4.0', status: 'ok', checked_at: now, storage_mode: 'postgres', uptime_seconds: 3661, dependencies: { postgres: { status: 'ok', latency_ms: 2 }, redis: { status: 'ok', latency_ms: 1 } }, capabilities: { audit_logs: true }, inference: { backend: 'powerpoint_bps', profile: 'powerpoint', base_url: 'https://bps.openai.com/basispoints/api', tools_version: 'fixture', web_search: 'enabled', codex_fallback: false } } }))
  await context.route('**/api/audit-logs?*', route => route.fulfill({ json: { supported: true, items: [{ id: 'audit-1', time: now, created_at: now, action: 'account.update', entity_id: 'compact-1', result: 'ok' }], total: 1, total_kind: 'exact', has_more: false, next_cursor: '' } }))
  await context.route('**/api/accounts/upstream/batch', async route => {
    const body = route.request().postDataJSON()
    await route.fulfill({ json: { results: body.items.map(row => ({ id: row.id, ok: true, snapshot: { id: row.id, kind: 'usage', ok: true, source: 'codex_usage', checked_at: now, local_window_costs: { primary_window: { usd: 0.125, estimated_requests: 12, unknown_requests: 2, source: 'local_token_estimate', scope: 'retained_request_details' }, secondary_window: { usd: 0.125, estimated_requests: 12, unknown_requests: 2, source: 'local_token_estimate', scope: 'retained_request_details' } }, data: { plan_type: 'team', rate_limit: { primary_window: { limit_window_seconds: 18000, used_percent: 8, reset_at: now + 18000 }, secondary_window: { limit_window_seconds: 604800, used_percent: 8, reset_at: now + 604800 } } } } })) } })
  })
  const page = await context.newPage()
  page.on('pageerror', error => report.errors.push(error.message))
  for (const route of ['accounts', 'groups', 'keys', 'proxies', 'logs', 'audit-logs', 'diagnostics']) {
    await page.goto(`http://127.0.0.1:${server.address().port}/#/${route}`)
    await expect(page.locator('.page-header')).toBeVisible()
    await page.waitForTimeout(450)
    if (route === 'accounts') await expect(page.locator('.el-table__body-wrapper .official-usage-windows').first()).toContainText('8%')
    if (route === 'diagnostics') {
      await expect(page.getByRole('region', { name: 'PostgreSQL 状态' })).toBeVisible()
      await expect(page.getByRole('region', { name: '运维监控', exact: true }).locator('canvas')).toHaveCount(2)
    }
    report.pages[route] = await page.evaluate(() => {
      const visible = node => node.getBoundingClientRect().width > 0 && node.getBoundingClientRect().height > 0
      const rows = [...document.querySelectorAll('.el-table__body-wrapper .el-table__row')].filter(visible)
      const content = document.querySelector('.console-content') || document.querySelector('.content')
      return { rowHeights: rows.map(row => Math.round(row.getBoundingClientRect().height * 10) / 10), tableCount: [...document.querySelectorAll('.el-table')].filter(visible).length, pageScrollHeight: content.scrollHeight, viewportOverflow: document.documentElement.scrollWidth - innerWidth }
    })
    assert.ok(report.pages[route].viewportOverflow <= 1, `${route} desktop page overflow`)
    const name = `${baseline ? 'baseline' : 'compact'}-${route}-desktop.png`
    await page.screenshot({ path: path.join(output, name), fullPage: true }); report.screenshots.push(name)
    if (route === 'accounts') {
      await page.locator('.el-table__body-wrapper .el-scrollbar__wrap').evaluate(element => { element.scrollLeft = element.scrollWidth })
      await page.waitForTimeout(80)
      await page.screenshot({ path: path.join(output, `${baseline ? 'baseline' : 'compact'}-accounts-windows.png`), fullPage: true })
    }
  }
  for (const route of ['accounts', 'diagnostics', 'groups', 'keys', 'proxies', 'logs', 'audit-logs']) {
    await page.setViewportSize({ width: 390, height: 844 })
    await page.goto(`http://127.0.0.1:${server.address().port}/#/${route}`)
    await page.waitForTimeout(350)
    const overflow = await page.evaluate(() => document.documentElement.scrollWidth - innerWidth)
    assert.ok(overflow <= 1, `${route} mobile page overflow`)
    if (['accounts', 'diagnostics'].includes(route)) {
      const name = `${baseline ? 'baseline' : 'compact'}-${route}-mobile.png`
      await page.screenshot({ path: path.join(output, name), fullPage: true }); report.screenshots.push(name)
    }
  }
  assert.deepEqual(report.errors, [])
  if (!baseline) {
    const before = JSON.parse(await readFile(path.join(output, 'baseline.json'), 'utf8'))
    for (const route of ['accounts', 'groups', 'keys', 'proxies', 'logs', 'audit-logs']) {
      const heights = report.pages[route].rowHeights
      assert.ok(heights.length && before.pages[route].rowHeights.length, `${route} missing fixture rows`)
      const mean = values => values.reduce((a, b) => a + b, 0) / values.length
      assert.ok(mean(heights) < mean(before.pages[route].rowHeights), `${route} rows did not become more compact`)
    }
    assert.ok(report.pages.diagnostics.pageScrollHeight < before.pages.diagnostics.pageScrollHeight, 'diagnostics did not become more compact')
  }
  report.passed = true
} finally {
  await browser.close()
  await new Promise(resolve => server.close(resolve))
  await writeFile(path.join(output, baseline ? 'baseline.json' : 'report.json'), JSON.stringify(report, null, 2))
  console.log(JSON.stringify({ passed: report.passed, baseline, pages: report.pages, output }))
}
