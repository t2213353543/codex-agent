import assert from 'node:assert/strict'
import { createServer } from 'node:http'
import { mkdir, readFile, writeFile } from 'node:fs/promises'
import { fileURLToPath } from 'node:url'
import path from 'node:path'
import { chromium, expect } from 'playwright/test'
import { operationsFixture } from './operations-fixture.mjs'

const directory = fileURLToPath(new URL('../', import.meta.url))
const root = path.join(directory, 'dist')
const output = path.resolve(directory, '../.local/operations-monitor-20260927')
await mkdir(output, { recursive: true })
const escaped = []
const server = createServer(async (request, response) => {
  const pathname = new URL(request.url, 'http://localhost').pathname
  if (pathname.startsWith('/api/')) { escaped.push(pathname); response.writeHead(500).end(); return }
  try {
    const resolved = path.resolve(root, pathname === '/' ? 'index.html' : decodeURIComponent(pathname).replace(/^\/+/, ''))
    if (!resolved.startsWith(root + path.sep) && resolved !== path.join(root, 'index.html')) throw new Error('outside root')
    const body = await readFile(resolved)
    response.writeHead(200, { 'Content-Type': ({ '.html': 'text/html', '.js': 'text/javascript', '.css': 'text/css', '.svg': 'image/svg+xml' })[path.extname(resolved)] || 'application/octet-stream' }).end(body)
  } catch { response.writeHead(404).end() }
})
await new Promise(resolve => server.listen(0, '127.0.0.1', resolve))
const base = `http://127.0.0.1:${server.address().port}`
let browser, failure
const errors = [], unexpected = [], assertions = [], screenshots = [], requests = []
try {
  browser = await chromium.launch({ headless: true, channel: process.env.TEST_BROWSER || 'chrome' })
  const context = await browser.newContext({ viewport: { width: 1440, height: 1080 }, timezoneId: 'America/Los_Angeles', reducedMotion: 'reduce', serviceWorkers: 'block' })
  let mode = 'ok'
  await context.route('**/api/**', async route => {
    const url = new URL(route.request().url())
    let json
    if (url.pathname === '/api/session') json = { authenticated: true, expires_at: 2000000000 }
    else if (url.pathname === '/api/settings') json = { version: 'operations-ui-fixture', uptime_seconds: 120, admin_api: true }
    else if (url.pathname === '/api/diagnostics') json = { version: 'operations-ui-fixture', checked_at: 1800000000, uptime_seconds: 120, storage_mode: 'postgres', status: 'ok', inference: { backend: 'powerpoint_bps', web_search: 'enabled', codex_fallback: false }, dependencies: { postgres: { configured: true, status: 'ok', latency_ms: 2 }, redis: { configured: true, status: 'ok', latency_ms: 1 } }, capabilities: { audit_logs: true } }
    else if (url.pathname === '/api/operations') {
      requests.push(url.searchParams.get('range'))
      if (mode === 'failure') { await route.fulfill({ status: 503, json: { error: { message: 'Synthetic monitor failure' } } }); return }
      json = operationsFixture(url.searchParams.get('range'))
      if (mode === 'empty') {
        json.traffic = { ...json.series[0] }
        json.series = json.series.map(point => ({ ...json.traffic, time: point.time }))
        json.latency_histogram = json.latency_histogram.map(bucket => ({ ...bucket, requests: 0 }))
        json.error_categories = []; json.recent_errors = []
        json.runtime.host.cpu_percent = null; json.runtime.host.cpu_status = 'warming_up'
        json.runtime.postgres_pool = null; json.runtime.redis_pool = null
      }
    } else if (url.pathname === '/api/groups') json = { groups: [] }
    else { unexpected.push(url.pathname); await route.fulfill({ status: 404, json: { error: { message: 'Unexpected mock API' } } }); return }
    await route.fulfill({ json }).catch(() => {})
  })
  const page = await context.newPage()
  page.setDefaultTimeout(10000)
  page.on('pageerror', error => errors.push(error.message))
  await page.goto(`${base}/#/diagnostics`)
  const monitor = page.getByRole('region', { name: '运维监控', exact: true })
  await expect(monitor.getByRole('heading', { name: '服务资源与存储' })).toBeVisible()
  await expect(monitor.locator('.ops-kpi').first()).toContainText('98%')
  await expect(monitor.locator('.ops-kpi').nth(2)).toContainText('2,400,000 Token')
  await expect(monitor.locator('.ops-resources')).toContainText('23.5%')
  await expect(monitor.locator('.ops-resources')).toContainText('16.00 GiB')
  await expect(monitor.locator('.ops-errors')).toContainText('<b>literal-model</b>')
  assert.equal(await monitor.locator('.ops-errors b').count(), 0, 'Model text must stay escaped')
  const expectedTime = new Date(1800000000 * 1000).toLocaleString('sv-SE', { timeZone: 'Asia/Shanghai' })
  await expect(monitor.locator('.ops-errors')).toContainText(expectedTime)
  await expect(monitor.locator('canvas')).toHaveCount(2)
  assertions.push('Synthetic KPIs, observed resource values, literal text, charts, and Shanghai time remain correct with Los Angeles browser timezone')
  await expect(page.locator('.diagnostic-facts > div')).toHaveCount(4)
  await expect(page.locator('.diagnostic-dependency')).toHaveCount(2)
  await expect(page.locator('.diagnostic-progress')).toContainText('2 个依赖连接正常')
  await expect(monitor.locator('.ops-kpi')).toHaveCount(6)
  await expect(monitor.locator('.ops-resources > div')).toHaveCount(12)
  await expect(monitor.locator('.ops-pools > div')).toHaveCount(2)
  await expect(monitor.getByRole('link', { name: '查看失败记录 2' })).toHaveAttribute('href', '#/logs?request_id=2')
  const compactTop = await page.locator('.diagnostic-top-grid').boundingBox()
  assert.ok(compactTop.height < 290, `Service summary and dependencies are too tall: ${compactTop.height}`)
  const kpiRows = await monitor.locator('.ops-kpi').evaluateAll(elements => [...new Set(elements.map(element => Math.round(element.getBoundingClientRect().top)))])
  assert.equal(kpiRows.length, 1, 'Desktop KPIs should share one row')
  assertions.push('Compact overview retains all service/dependency facts, six KPIs, twelve resource metrics, pool details, and per-record error links')
  await page.screenshot({ path: path.join(output, 'diagnostics-dark-desktop.png') }); screenshots.push('diagnostics-dark-desktop.png')
  await monitor.locator('.el-checkbox').click()
  await expect(monitor.getByRole('checkbox', { name: '自动刷新 · 30 秒' })).not.toBeChecked()
  await monitor.locator('.ops-heading').evaluate(element => element.scrollIntoView({ block: 'start' }))
  await page.screenshot({ path: path.join(output, 'operations-dark-desktop.png') }); screenshots.push('operations-dark-desktop.png')
  await monitor.locator('.ops-resource-card').evaluate(element => element.scrollIntoView({ block: 'start' }))
  await page.screenshot({ path: path.join(output, 'operations-dark-resources.png') }); screenshots.push('operations-dark-resources.png')
  await monitor.locator('.el-select__wrapper').click()
  await page.getByRole('option', { name: '近 15 分钟' }).click()
  await expect(monitor.locator('.ops-scope')).toContainText('近 15 分钟')
  assert.equal(requests.at(-1), '15m')
  const oldTotal = await monitor.locator('.ops-kpi').first().innerText()
  mode = 'failure'
  await monitor.getByRole('button', { name: '刷新监控' }).click()
  await expect(monitor.getByRole('alert')).toContainText('下方保留上次成功结果')
  assert.equal(await monitor.locator('.ops-kpi').first().innerText(), oldTotal)
  assertions.push('Range change refreshes the correct query; failed refresh retains and labels the last successful result')
  mode = 'empty'
  await monitor.getByRole('button', { name: '刷新监控' }).click()
  await expect(monitor.locator('.ops-kpi').first()).toContainText('暂无样本')
  await expect(monitor.locator('.ops-resources')).toContainText('等待下次采样')
  await expect(monitor.locator('.ops-pools')).toContainText('未配置 PostgreSQL')
  await expect(monitor.locator('.ops-pools')).toContainText('未配置 Redis')
  assertions.push('Empty traffic and first CPU sample remain unknown; unconfigured pools are explicitly labeled')
  mode = 'ok'
  await monitor.getByRole('button', { name: '刷新监控' }).click()
  await expect(monitor.locator('.ops-kpi').first()).toContainText('98%')
  await page.getByRole('button', { name: '切换留白主题', exact: true }).first().click()
  for (const width of [768, 390, 320]) {
    await page.setViewportSize({ width, height: 844 })
    await page.waitForTimeout(150)
    const overflow = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth)
    assert.ok(overflow <= 1, `Horizontal overflow at ${width}: ${overflow}`)
    const contentOverflow = await page.locator('.console-content').evaluate(element => element.scrollWidth - element.clientWidth)
    assert.ok(contentOverflow <= 1, `Content overflow at ${width}: ${contentOverflow}`)
    await page.locator('.diagnostic-top-grid').evaluate(element => element.scrollIntoView({ block: 'start' }))
    const screenshot = `diagnostics-light-${width}.png`
    await page.screenshot({ path: path.join(output, screenshot) }); screenshots.push(screenshot)
  }
  await page.setViewportSize({ width: 390, height: 844 })
  await monitor.locator('.ops-heading').evaluate(element => element.scrollIntoView({ block: 'start' }))
  await page.screenshot({ path: path.join(output, 'operations-light-mobile.png') }); screenshots.push('operations-light-mobile.png')
  await monitor.locator('.ops-resource-card').evaluate(element => element.scrollIntoView({ block: 'start' }))
  await page.screenshot({ path: path.join(output, 'operations-light-resources-mobile.png') }); screenshots.push('operations-light-resources-mobile.png')
  await page.reload()
  await expect(page.getByRole('checkbox', { name: '自动刷新 · 30 秒' })).not.toBeChecked()
  assertions.push('Both themes render without browser errors, mobile has no page overflow, and auto-refresh preference persists')
  await page.goto(`${base}/#/groups`)
  await expect(page.getByRole('heading', { name: '分组管理', exact: true })).toBeVisible()
  await page.waitForTimeout(100)
  assert.deepEqual(errors, [])
  assert.deepEqual(unexpected, [])
  assert.deepEqual(escaped, [])
} catch (error) { failure = error; console.error(error) }
finally {
  await browser?.close()
  await new Promise(resolve => server.close(resolve))
  await writeFile(path.join(output, 'browser-report.json'), JSON.stringify({ passed: !failure, scope: 'Isolated UI verification with synthetic API responses; backend correctness is covered by Go operations tests.', assertions, screenshots, requests, errors, unexpected, escaped, failure: failure?.message || null }, null, 2))
}
if (failure) process.exitCode = 1
else console.log(JSON.stringify({ passed: true, assertions: assertions.length, output }))
