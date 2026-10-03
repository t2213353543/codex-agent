// Local synthetic UI acceptance. Every API request is mocked; no production data.
import assert from 'node:assert/strict'
import { createServer } from 'node:http'
import { readFile, writeFile, mkdir } from 'node:fs/promises'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { chromium, expect } from 'playwright/test'
import { installMockApi } from './mock-api.mjs'
import { testLogsWorkspace } from './log-workspace-browser.mjs'
import { logWorkspacePreviewRows } from './log-workspace-fixtures.mjs'

const root = path.resolve(fileURLToPath(new URL('../..', import.meta.url)))
const folder = path.join(root, '.local/ui-polish-20260918/logs')
const dist = path.resolve(process.env.UI_POLISH_DIST || path.join(folder, 'dist'))
const output = path.join(folder, 'browser')
await mkdir(output, { recursive: true })
const report = { status: 'running', started_at: new Date().toISOString(), dist, synthetic_data: true, behavior: [], layouts: [], errors: [], leaked_api: [] }
let browser, current = 'startup'
const server = createServer(async (request, response) => {
  const pathname = decodeURIComponent(new URL(request.url, 'http://localhost').pathname)
  if (pathname.startsWith('/api/')) {
    report.leaked_api.push(pathname)
    response.writeHead(500).end('API request escaped local mock')
    return
  }
  try {
    const filename = path.resolve(dist, pathname === '/' ? 'index.html' : pathname.slice(1))
    if (!filename.startsWith(dist + path.sep)) throw new Error('invalid_path')
    const body = await readFile(filename)
    const mime = { '.js': 'text/javascript', '.css': 'text/css', '.html': 'text/html', '.woff2': 'font/woff2', '.svg': 'image/svg+xml' }[path.extname(filename)] || 'application/octet-stream'
    response.writeHead(200, { 'Content-Type': mime, 'Cache-Control': 'no-store' }).end(body)
  } catch { response.writeHead(404).end('Not found') }
})
try {
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve))
  const origin = `http://127.0.0.1:${server.address().port}`
  browser = await chromium.launch({ headless: true, channel: process.env.TEST_BROWSER || 'chrome' })
  const context = await browser.newContext({ viewport: { width: 1440, height: 1000 }, reducedMotion: 'reduce', acceptDownloads: true, serviceWorkers: 'block' })
  const state = await installMockApi(context)
  state.authenticated = true
  const page = await context.newPage()
  page.setDefaultTimeout(15000)
  page.on('pageerror', error => report.errors.push({ phase: current, message: error.message }))
  page.on('console', message => {
    if (message.type() === 'warning' && /Failed to resolve|Invalid prop|Unhandled error/.test(message.text())) report.errors.push({ phase: current, message: message.text() })
  })
  page.on('response', response => {
    if (response.status() >= 400 && !new URL(response.url()).pathname.startsWith('/api/')) report.errors.push({ phase: current, message: `Resource ${response.status()}: ${new URL(response.url()).pathname}` })
  })
  await page.goto(origin + '/#/settings')
  await expect(page.locator('.settings-panel')).toBeVisible()
  current = 'existing-workspace-regression'
  report.behavior = await testLogsWorkspace(page)

  const rows = logWorkspacePreviewRows.map((row, index) => ({ ...row, event_id: `synthetic-event-${index}`, account_id: `synthetic-account-${index}`, groups: ['生产服务', '开发工具'], key_id: 'synthetic-key-id', ...(index === 0 ? { ttft_ms: 0 } : {}) }))
  let responseRows = rows
  await page.route('**/api/logs?*', route => route.fulfill({ json: { items: responseRows, has_more: false, next_cursor: '', total_kind: 'not_computed', total: -1 } }))
  await page.goto(origin + '/#/logs?model=gpt-5.6-luna')
  await expect(page.locator('.log-metrics-strip')).toContainText('本页指标')
  await expect(page.locator('.query-notices')).toContainText('模型：gpt-5.6-luna')
  await page.locator('.query-notices .el-tag__close').click()
  await expect(page).not.toHaveURL(/model=/)
  await expect(page.locator('.log-metrics-strip .metric-cache')).toHaveText('74.5%')
  report.behavior.push('已应用筛选可清除且更新 URL；本页缓存按 Token 加权展示并注明统计范围')
  // Reload resets the expanded advanced form and clears behavior-test toasts.
  await page.reload()
  await expect(page.locator('.log-table .el-table__row')).toHaveCount(rows.length)

  await page.locator('.log-table').getByRole('button', { name: /日志详情/ }).first().click()
  const drawer = page.getByRole('dialog', { name: '日志详情', exact: true })
  await expect(drawer).toContainText('synthetic-event-0')
  await expect(drawer).toContainText('生产服务、开发工具')
  await expect(drawer).toContainText('首字延迟')
  const ttftValue = drawer.locator('dt').filter({ hasText: '首字延迟' }).locator('xpath=following-sibling::dd[1]')
  await expect(ttftValue).toHaveText('0 ms')
  await page.screenshot({ path: path.join(output, 'detail-light-1440.png'), animations: 'disabled' })
  await drawer.getByRole('button', { name: '关闭详情', exact: true }).click()
  await expect(drawer).toBeHidden()

  responseRows = [{ ...rows[1], error: 'Synthetic long upstream error. '.repeat(100) }]
  await page.getByRole('button', { name: '刷新日志', exact: true }).click()
  await expect(page.locator('.log-table .el-table__row')).toHaveCount(1)
  await page.locator('.log-table').getByRole('button', { name: /日志详情/ }).first().click()
  await expect(ttftValue).toHaveText('未记录')
  await expect(drawer.locator('.detail-error')).toHaveClass(/error-collapsed/)
  await drawer.getByRole('button', { name: '展开完整错误', exact: true }).click()
  await expect(drawer.locator('.detail-error')).not.toHaveClass(/error-collapsed/)
  await drawer.getByRole('button', { name: '关闭详情', exact: true }).click()
  await expect(drawer).toBeHidden()
  for (const [value, expected] of [[240, '240 ms'], [null, '未记录']]) {
    responseRows = [{ ...rows[0], ttft_ms: value }]
    await page.getByRole('button', { name: '刷新日志', exact: true }).click()
    await expect(page.locator('.el-loading-mask:visible')).toHaveCount(0)
    await page.locator('.log-table').getByRole('button', { name: /日志详情/ }).first().click()
    await expect(ttftValue).toHaveText(expected)
    await drawer.getByRole('button', { name: '关闭详情', exact: true }).click()
    await expect(drawer).toBeHidden()
  }
  report.behavior.push('详情含记录/账号/KEY ID 与分组；可选首字延迟显示真实正值和零值，遗漏/null显示未记录；长错误可展开并保留全文')

  responseRows = rows
  await page.getByRole('button', { name: '刷新日志', exact: true }).click()
  await expect(page.locator('.log-table .el-table__row')).toHaveCount(rows.length)
  for (const theme of ['light', 'dark']) for (const width of [1440, 1024, 768, 390, 320]) {
    current = `${theme}-${width}`
    await page.setViewportSize({ width, height: width < 600 ? 844 : 1000 })
    await page.evaluate(value => {
      document.documentElement.dataset.theme = value
      document.documentElement.classList.toggle('dark', value === 'dark')
      window.dispatchEvent(new CustomEvent('theme-change', { detail: value }))
    }, theme)
    await page.evaluate(() => document.fonts.ready)
    await expect(page.locator('.el-loading-mask:visible')).toHaveCount(0)
    await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))))
    const overflow = await page.evaluate(() => document.documentElement.scrollWidth - innerWidth)
    assert.ok(overflow <= 1, `${current} page overflow: ${overflow}`)
    const contentWidth = await page.locator('.logs-page').evaluate(element => element.clientWidth)
    await expect(page.locator(contentWidth <= 1000 ? '.mobile-log-list' : '.log-table')).toBeVisible()
    const screenshot = `logs-${current}.png`
    await page.screenshot({ path: path.join(output, screenshot), fullPage: true, animations: 'disabled' })
    report.layouts.push({ theme, width, contentWidth, overflow, screenshot })
  }
  assert.deepEqual(report.errors, [])
  assert.deepEqual(report.leaked_api, [])
  assert.deepEqual(state.unexpected, [])
  report.status = 'pass'
} catch (error) {
  report.status = 'fail'
  report.failure = { phase: current, message: error.message }
  process.exitCode = 1
} finally {
  await browser?.close()
  await new Promise(resolve => server.close(resolve))
  report.finished_at = new Date().toISOString()
  await writeFile(path.join(folder, 'browser-report.json'), JSON.stringify(report, null, 2) + '\n')
}
console.log(JSON.stringify({ status: report.status, behavior: report.behavior.length, layouts: report.layouts.length, failure: report.failure, report: path.join(folder, 'browser-report.json') }))
