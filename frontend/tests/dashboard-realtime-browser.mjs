import assert from 'node:assert/strict'
import { createServer } from 'node:http'
import { readFile, mkdir, writeFile } from 'node:fs/promises'
import { fileURLToPath } from 'node:url'
import path from 'node:path'
import { chromium, expect } from 'playwright/test'
import { installMockApi } from './mock-api.mjs'

const root = fileURLToPath(new URL('../dist/', import.meta.url))
const output = fileURLToPath(new URL('../test-results/dashboard-realtime/', import.meta.url))
await mkdir(output, { recursive: true })
const server = createServer(async (req, res) => {
  try {
    const pathname = new URL(req.url, 'http://localhost').pathname
    const file = path.resolve(root, pathname === '/' ? 'index.html' : decodeURIComponent(pathname).slice(1))
    if (path.relative(root, file).startsWith('..')) throw Error()
    const data = await readFile(file)
    res.writeHead(200, { 'Content-Type': ({ '.html': 'text/html', '.js': 'text/javascript', '.css': 'text/css' })[path.extname(file)] || 'application/octet-stream' }).end(data)
  } catch { res.writeHead(404).end() }
})
await new Promise(resolve => server.listen(0, '127.0.0.1', resolve))
let browser, passed = false
const errors = [], calls = [], screenshots = []
try {
  browser = await chromium.launch({ channel: process.env.TEST_BROWSER || 'chrome', headless: true })
  const context = await browser.newContext({ viewport: { width: 1440, height: 1000 }, reducedMotion: 'reduce' })
  const state = await installMockApi(context)
  state.authenticated = true
  let mode = 'known'
  await context.route('**/api/overview/realtime', async route => {
    calls.push(route.request().url())
    if (mode === 'failure') { await route.fulfill({ status: 409, json: { error: { message: 'Synthetic stale realtime result' } } }); return }
    await route.fulfill({ json: { scope: 'current_process', window_seconds: 60, rpm: 12, tpm: mode === 'unknown' ? null : 3456, updated_at: 1790519000, coverage_seconds: 60, warming_up: false, tokens_partial: false } })
  })
  const page = await context.newPage()
  page.on('pageerror', error => errors.push(error.message))
  await page.goto(`http://127.0.0.1:${server.address().port}/#/`)
  const card = page.locator('[data-metric="realtime_rates"]')
  await expect(card).toBeVisible()
  await expect(card.locator('[data-metric="realtime_rpm"]')).toHaveText('12RPM')
  await expect(card.locator('[data-metric="realtime_tpm"]')).toHaveText('3,456TPM')
  await expect(page.getByText('请求失败率', { exact: true })).toHaveCount(0)
  await expect(card).toContainText('最近 60 秒 · 5 秒刷新')
  await expect(card.locator('time')).toHaveCount(1)
  assert.ok((await card.boundingBox()).height < 100, 'The compact rate card must not enlarge its row')
  await page.locator('.range-options .el-radio-button').filter({ hasText: '近 7 天' }).click()
  await expect(page).toHaveURL(/range=7d/)
  await expect(card.locator('[data-metric="realtime_rpm"]')).toHaveText('12RPM')
  assert.ok(calls.every(url => new URL(url).search === ''), 'Realtime must not inherit historical range')
  const before = calls.length
  await expect.poll(() => calls.length, { timeout: 8000 }).toBeGreaterThan(before)
  await page.screenshot({ path: path.join(output, 'dashboard-rates-desktop.png'), fullPage: true })
  screenshots.push('dashboard-rates-desktop.png')
  mode = 'unknown'
  await page.getByRole('button', { name: '刷新运行概览', exact: true }).click()
  await expect(card.locator('[data-metric="realtime_tpm"]')).toHaveText('—TPM')
  await expect(card).toContainText('Token 等待上报')
  mode = 'failure'
  await page.getByRole('button', { name: '刷新运行概览', exact: true }).click()
  await expect(card).toContainText('更新失败')
  await expect(card.locator('[data-metric="realtime_rpm"]')).toHaveText('—RPM')
  mode = 'known'
  await page.getByRole('button', { name: '刷新运行概览', exact: true }).click()
  await expect(card.locator('[data-metric="realtime_tpm"]')).toHaveText('3,456TPM')
  await page.setViewportSize({ width: 390, height: 844 })
  assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1), 'Mobile horizontal overflow')
  await expect(card).toBeVisible()
  await page.screenshot({ path: path.join(output, 'dashboard-rates-mobile.png'), fullPage: true })
  screenshots.push('dashboard-rates-mobile.png')
  await page.evaluate(() => { Object.defineProperty(document, 'hidden', { configurable: true, value: true }); document.dispatchEvent(new Event('visibilitychange')) })
  await expect(card).toContainText('后台已暂停')
  const hiddenCalls = calls.length
  await page.waitForTimeout(5200)
  assert.equal(calls.length, hiddenCalls, 'Hidden page continued polling')
  await page.evaluate(() => { Object.defineProperty(document, 'hidden', { configurable: true, value: false }); document.dispatchEvent(new Event('visibilitychange')) })
  await expect.poll(() => calls.length).toBeGreaterThan(hiddenCalls)
  assert.deepEqual(errors, [])
  passed = true
} finally {
  await browser?.close()
  await new Promise(resolve => server.close(resolve))
  await writeFile(path.join(output, 'report.json'), JSON.stringify({ passed, errors, screenshots, realtime_requests: calls.length, checks: ['fixed 60-second card', 'five-second refresh', 'range independent', 'unknown usage is not zero', '409 stale result withheld', 'hidden-tab pause and resume', 'desktop/mobile no overflow'] }, null, 2))
  console.log(JSON.stringify({ passed, output, realtime_requests: calls.length }))
}
