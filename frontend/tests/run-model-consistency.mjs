import assert from 'node:assert/strict'
import { createServer } from 'node:http'
import { mkdir, readFile } from 'node:fs/promises'
import path from 'node:path'
import { chromium, expect } from 'playwright/test'
import { installMockApi } from './mock-api.mjs'

const dist = path.resolve(process.argv[2] || '../.local/model-consistency/dist')
const output = path.resolve('../.local/model-consistency/browser')
await mkdir(output, { recursive: true })
const server = createServer(async (request, response) => {
  try {
    const pathname = decodeURIComponent(new URL(request.url, 'http://127.0.0.1').pathname)
    const file = path.resolve(dist, '.' + (pathname === '/' ? '/index.html' : pathname))
    if (!file.startsWith(dist + path.sep)) { response.writeHead(403).end(); return }
    response.setHeader('Content-Type', ({ '.html': 'text/html', '.js': 'text/javascript', '.css': 'text/css', '.svg': 'image/svg+xml' })[path.extname(file)] || 'application/octet-stream')
    response.end(await readFile(file))
  } catch { response.writeHead(404).end() }
})
await new Promise(resolve => server.listen(0, '127.0.0.1', resolve))
let browser
try {
  browser = await chromium.launch({ channel: process.env.TEST_BROWSER || 'chrome', headless: true })
  const context = await browser.newContext({ viewport: { width: 1440, height: 1100 }, timezoneId: 'America/Los_Angeles', reducedMotion: 'reduce' })
  const state = await installMockApi(context)
  state.authenticated = true
  const page = await context.newPage()
  const errors = []
  page.on('pageerror', error => errors.push(error.message))
  const base = { time: 0, status: 'ok', http_code: 200, model: 'request-alias', endpoint: 'responses', usage_known: true, input_tokens: 1_250_000, output_tokens: 2_000_000_000, cached_tokens: 1_000_000, latency_ms: 20 }
  const rows = [
    { ...base, event_id: 'match', sent_model: 'actual-model', returned_model: 'actual-model' },
    { ...base, event_id: 'mismatch', sent_model: 'actual-model', returned_model: 'other-model' },
    { ...base, event_id: 'unknown', upstream_model: 'request-alias' },
  ]
  const queries = []
  await page.route('**/api/logs**', async route => {
    const url = new URL(route.request().url())
    if (url.pathname.endsWith('/snapshot')) return route.fulfill({ json: { as_of: 'model-check', created_at: 0, expires_at: 9_999_999_999, total: rows.length } })
    if (url.pathname.endsWith('/stats')) return route.fulfill({ json: { total_requests: 3, success: 3, errors: 0, total_input_tokens: 0, total_output_tokens: 0, total_cached_tokens: 0, total_tokens: 0, series: [] } })
    if (url.pathname === '/api/logs') { queries.push(Object.fromEntries(url.searchParams)); return route.fulfill({ json: { items: rows, total: rows.length, total_kind: 'exact', has_more: false, next_cursor: '' } }) }
    return route.fallback()
  })
  // A custom statistics timezone must never change request timestamps.
  await page.goto(`http://127.0.0.1:${server.address().port}/#/logs?timezone=UTC`)
  const table = page.locator('.log-table')
  await expect(table.locator('.el-table__row')).toHaveCount(3)
  await expect(table.locator('[data-model-consistency="match"]')).toHaveText('模型一致')
  await expect(table.locator('[data-model-consistency="mismatch"]')).toHaveText('模型不一致')
  await expect(table.locator('[data-model-consistency="unknown"]')).toHaveText('未能校验')
  await expect(table.locator('.request-time').first()).toContainText('08:00:00')
  await expect(table).toContainText('1.25M')
  await expect(table).toContainText('2B')
  await expect(table.locator('.token-input').first()).toHaveAttribute('title', '输入 Token：1,250,000')
  await expect(table.locator('.token-output').first()).toHaveAttribute('title', '输出 Token：2,000,000,000')
  await table.locator('[data-model-consistency="mismatch"]').focus()
  await expect(page.getByRole('tooltip').filter({ hasText: '发送：actual-model' })).toContainText('返回：other-model')
  await page.screenshot({ path: path.join(output, 'desktop.png') })
  await table.locator('[data-model-consistency="mismatch"]').press('Tab')
  await page.setViewportSize({ width: 390, height: 844 })
  await expect(page.locator('.mobile-log-list [data-model-consistency]')).toHaveCount(3)
  await expect(page.locator('.mobile-log-top').first()).toContainText('08:00:00')
  assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true)
  await page.locator('.mobile-log-list').screenshot({ path: path.join(output, 'mobile.png') })
  await page.setViewportSize({ width: 1440, height: 1100 })
  const from = page.getByRole('textbox', { name: '开始时间', exact: true })
  const to = page.getByRole('textbox', { name: '结束时间', exact: true })
  if (!await from.isVisible()) await page.getByRole('button', { name: /高级筛选/ }).click()
  await from.fill('2026-09-27T00:00')
  await to.fill('2026-09-28T00:00')
  await page.getByRole('button', { name: '刷新日志', exact: true }).click()
  await expect.poll(() => queries.at(-1)?.from).toBe(String(Date.parse('2026-09-27T00:00:00+08:00') / 1000))
  assert.equal(queries.at(-1).to, String(Date.parse('2026-09-28T00:00:00+08:00') / 1000))
  await page.reload()
  await expect(from).toHaveValue('2026-09-27T00:00')
  await expect(to).toHaveValue('2026-09-28T00:00')
  assert.deepEqual(errors, [])
  console.log('PASS: desktop/mobile model states, focused model tooltip, Shanghai time under Los Angeles browser and UTC statistics, Shanghai filter epoch roundtrip, compact token units, no overflow or browser errors')
} finally {
  await browser?.close()
  await new Promise(resolve => server.close(resolve))
}
