import assert from 'node:assert/strict'
import { createServer } from 'node:http'
import { mkdir, readFile, writeFile } from 'node:fs/promises'
import path from 'node:path'
import { chromium, expect } from 'playwright/test'
import { account, installMockApi } from './mock-api.mjs'

const root = path.resolve(process.env.MANAGEMENT_UI_DIST || 'dist')
const output = path.resolve(process.env.ACCOUNT_USAGE_REPORT_DIR || '../.local/official-usage')
const report = { checks: [], layouts: [], screenshots: [] }
await mkdir(output, { recursive: true })
const server = createServer(async (request, response) => {
  try {
    const pathname = decodeURIComponent(new URL(request.url, 'http://localhost').pathname)
    const file = path.resolve(root, pathname === '/' ? 'index.html' : pathname.slice(1))
    if (path.relative(root, file).startsWith('..')) throw new Error('outside build')
    const data = await readFile(file)
    response.writeHead(200, { 'Content-Type': ({ '.html': 'text/html', '.js': 'text/javascript', '.css': 'text/css', '.svg': 'image/svg+xml' })[path.extname(file)] || 'application/octet-stream' }).end(data)
  } catch { response.writeHead(404).end() }
})
await new Promise(resolve => server.listen(0, '127.0.0.1', resolve))
let browser
try {
  browser = await chromium.launch({ channel: process.env.TEST_BROWSER || 'chrome', headless: true })
  const context = await browser.newContext({ viewport: { width: 1500, height: 1000 }, reducedMotion: 'reduce' })
  const state = await installMockApi(context)
  state.authenticated = true
  state.groups = Array.from({ length: 6 }, (_, i) => ({ id: `g${i + 1}`, name: `完整分组名称 ${i + 1} 供焦点展示` }))
  const rows = [account('official-1', 'Official weekly only', { quota_used: 999999, window_days: 30, groups: state.groups.map(group => group.id) }), account('official-2', 'Official unknown window'), account('official-3', 'Official query failure'), account('official-4', 'Official two windows'), account('official-5', 'Official large amounts')]
  rows[0].usage.avg_latency_ms = 61234
  rows[1].usage.avg_latency_ms = null
  rows[2].usage.avg_latency_ms = 0
  const calls = [], errors = []
  const resetsAt = Math.floor(Date.now() / 1000) + 604800
  const usageSnapshot = (row, body) => {
    const dual = ['official-4', 'official-5'].includes(row.id)
    const windowCost = usd => ({ source: 'local_token_estimate', scope: 'retained_request_details', usd, estimated_requests: 12, unknown_requests: dual ? 1 : 0, price_version: 'fixture' })
    return { id: row.id, kind: 'usage', ok: true, source: 'codex_usage', checked_at: Math.floor(Date.now() / 1000), local_window_costs: { primary_window: windowCost(1.2345), ...(dual ? { secondary_window: windowCost(row.id === 'official-5' ? 123456789.1234 : .125) } : {}) }, data: { plan_type: 'team', rate_limit: { primary_window: row.id === 'official-2' ? { limit_window_seconds: 18000, used_percent: null, reset_at: null } : { limit_window_seconds: dual ? 18000 : 604800, used_percent: row.version > 1 ? 49 : body.force ? 48 : 47, reset_at: resetsAt }, secondary_window: dual ? { limit_window_seconds: 604800, used_percent: 8, reset_at: resetsAt } : null } } }
  }
  let accountListReads = 0
  const page = await context.newPage()
  page.on('pageerror', error => errors.push(error.message))
  await page.route('**/api/accounts/*/auto-reset', route => {
    const id = new URL(route.request().url()).pathname.split('/')[3]
    return route.fulfill({ json: { id, account_version: rows.find(row => row.id === id).version, policy: { version: 0, enabled: false, threshold_percent: 95, interval_minutes: 15, phase: 'disabled' } } })
  })
  await page.route('**/api/accounts/*/upstream', route => {
    const id = new URL(route.request().url()).pathname.split('/')[3]
    return route.fulfill({ json: { id, persistent: true, snapshots: { usage: usageSnapshot(rows.find(row => row.id === id), {}) } } })
  })
  await page.route('**/api/accounts/manage?*', route => { accountListReads++; return route.fulfill({ json: { accounts: rows, total: rows.length, total_kind: 'exact', has_more: false } }) })
  await page.route('**/api/accounts/upstream/batch', route => {
    const body = route.request().postDataJSON()
    calls.push(body)
    return route.fulfill({ json: { results: body.items.map(row => row.version !== rows.find(target => target.id === row.id).version ? { id: row.id, ok: false, code: 'version_conflict' } : row.id === 'official-3' ? { id: row.id, ok: false, code: 'upstream_http_error' } : { id: row.id, ok: true, snapshot: usageSnapshot(row, body) }) } })
  })
  const base = `http://127.0.0.1:${server.address().port}`
  await page.goto(`${base}/#/accounts`)
  const cells = page.locator('.el-table__body-wrapper .official-usage-windows')
  await expect(cells).toHaveCount(5)
  await expect(cells.nth(0)).toContainText('7 天')
  await expect(cells.nth(0)).toContainText('47%')
  await expect(cells.nth(0)).toContainText('本地估算 $1.2345')
  await expect(cells.nth(0)).toContainText('总约 $2.63')
  const tableRows = page.locator('.el-table__body-wrapper .el-table__row')
  await expect(cells.nth(3)).toContainText('已知部分 $0.125')
  await expect(cells.nth(3)).toContainText('总约 $1.56')
  const normalHeight = await tableRows.nth(3).evaluate(row => row.getBoundingClientRect().height)
  assert.ok(normalHeight <= 76, `two official windows row too tall: ${normalHeight}`)
  report.checks.push({ double_window_row_height: normalHeight })
  await expect(tableRows.nth(0)).toContainText('1 分 1.234 秒')
  await expect(tableRows.nth(1)).toContainText('暂无样本')
  await expect(tableRows.nth(2)).toContainText('0 ms')
  const group = page.locator('.el-table__body-wrapper .account-group-list').first()
  assert.equal(await group.evaluate(node => getComputedStyle(node).whiteSpace), 'nowrap')
  assert.ok(await group.evaluate(node => node.scrollWidth > node.clientWidth), 'long groups should be ellipsized')
  await group.focus()
  await expect(page.getByRole('tooltip').filter({ hasText: '完整分组名称 6' })).toBeVisible()
  await group.evaluate(node => node.blur())
  const cell = page.locator('.el-table__body-wrapper td').first()
  await page.mouse.move(0, 0)
  const color = await cell.evaluate(node => getComputedStyle(node).backgroundColor)
  await cell.hover()
  assert.equal(await cell.evaluate(node => getComputedStyle(node).backgroundColor), color, 'row hover must not change background')
  await expect(cells.nth(0)).not.toContainText('5 小时')
  await expect(cells.nth(0)).not.toContainText('999999')
  await expect(cells.nth(1)).toContainText('未知')
  await expect(cells.nth(1).locator('progress')).toHaveCount(0)
  await expect(cells.nth(2)).toContainText('官方用量未取得')
  await expect(cells.nth(2).locator('progress')).toHaveCount(0)
  assert.equal(calls[0].force, false)
  await page.getByRole('button', { name: '刷新本页官方用量', exact: true }).click()
  await expect(cells.nth(0)).toContainText('48%')
  assert.equal(calls.at(-1).force, true)
  await page.locator('.el-table__body-wrapper .el-scrollbar__wrap').evaluate(element => { element.scrollLeft = element.scrollWidth })
  await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))))
  await page.screenshot({ path: path.join(output, 'official-account-windows-desktop.png'), fullPage: true })
  await page.setViewportSize({ width: 390, height: 844 })
  const mobile = page.locator('.mobile-account .official-usage-windows')
  await expect(mobile).toHaveCount(5)
  await expect(mobile.nth(0)).toContainText('48%')
  await expect(mobile.nth(1)).toContainText('未知')
  await mobile.nth(0).scrollIntoViewIfNeeded()
  await page.screenshot({ path: path.join(output, 'official-account-windows-mobile.png'), fullPage: true })
  for (const theme of ['light', 'dark']) for (const width of [1440, 768, 390, 320]) {
    await page.setViewportSize({ width, height: 1000 })
    await page.evaluate(value => { document.documentElement.dataset.theme = value; document.documentElement.classList.toggle('dark', value === 'dark') }, theme)
    const visibleCells = page.locator(width < 768 ? '.mobile-account .official-usage-windows' : '.el-table__body-wrapper .official-usage-windows')
    await expect(visibleCells).toHaveCount(5)
    await expect(visibleCells.nth(4)).toContainText('$123,456,789.1234')
    const layout = await visibleCells.evaluateAll(elements => elements.map(node => ({ height: node.getBoundingClientRect().height, width: node.clientWidth, contentWidth: node.scrollWidth, amounts: Array.from(node.querySelectorAll('.official-usage-cost,.official-usage-total')).map(amount => ({ text: amount.textContent, width: amount.clientWidth, contentWidth: amount.scrollWidth })) })))
    assert.ok(layout.every(cell => cell.contentWidth <= cell.width + 1 && cell.amounts.every(amount => amount.contentWidth <= amount.width + 1)), `${theme} ${width}: amounts clipped ${JSON.stringify(layout)}`)
    assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1), `${theme} ${width}: page overflow`)
    await visibleCells.nth(3).getByRole('button', { name: '查看官方 7 天 用量与重置时间', exact: true }).click()
    const details = page.locator('.official-window-detail:visible')
    await expect(details).toContainText('重置时间')
    await expect(details).toContainText('总额度估算')
    await expect(details).toContainText('部分请求未定价')
    assert.ok(await details.evaluate(node => node.scrollWidth <= node.clientWidth + 1))
    const popover = await details.locator('..').boundingBox()
    assert.ok(popover.x >= -1 && popover.x + popover.width <= width + 1, `popover exceeds ${width}px viewport`)
    const screenshot = `compact-usage-${theme}-${width}.png`
    await page.screenshot({ path: path.join(output, screenshot), fullPage: true, animations: 'disabled' })
    report.layouts.push({ theme, width, layout }); report.screenshots.push(screenshot)
    await page.keyboard.press('Escape')
    await page.locator('.account-directory-heading').click()
    await expect(details).toHaveCount(0)
  }
  await page.setViewportSize({ width: 1500, height: 1000 })
  const beforeRotation = accountListReads
  rows[0].version++
  await page.getByRole('button', { name: '刷新本页官方用量', exact: true }).click()
  await expect(cells.nth(0)).toContainText('49%')
  assert.equal(accountListReads, beforeRotation + 1, 'credential rotation must recover stale page versions once')
  assert.equal(calls.at(-1).items[0].version, 2)
  await page.getByRole('button', { name: '导入账号', exact: true }).click()
  const dialog = page.getByRole('dialog', { name: '导入账号 · 先预览再提交' })
  await dialog.locator('summary').filter({ hasText: '高级配置' }).click()
  await expect(dialog.getByLabel('导入默认窗口天数')).toHaveCount(0)
  await expect(dialog.getByLabel('导入默认 Token 额度')).toHaveCount(0)
  await expect(dialog.getByLabel('导入默认并发容量')).toBeVisible()
  await dialog.getByRole('button', { name: '关闭', exact: true }).click()
  await tableRows.nth(0).getByRole('button', { name: '编辑', exact: true }).click()
  const edit = page.getByRole('dialog', { name: /^编辑账号/ })
  await expect(edit.getByRole('spinbutton', { name: '账号 Token 额度', exact: true })).toHaveCount(0)
  await expect(edit.getByRole('spinbutton', { name: '账号窗口天数', exact: true })).toHaveCount(0)
  await expect(edit).not.toContainText('本地 Token 预算')
  await edit.getByRole('textbox', { name: '账号备注', exact: true }).fill('compact edit preserves budget')
  await edit.getByRole('button', { name: '保存', exact: true }).click()
  await expect(edit).toBeHidden()
  const patch = state.requests.findLast(request => request.path === '/accounts/official-1' && request.method === 'PUT')?.body
  assert.equal(patch.notes, 'compact edit preserves budget')
  for (const key of ['quota', 'window_days', 'reset_window']) assert.equal(Object.hasOwn(patch, key), false)
  report.checks.push({ edit_preserves_local_budget: true })
  await cells.nth(0).getByRole('button', { name: '官方额度详情', exact: true }).click()
  const upstream = page.getByRole('dialog', { name: '账号用量与测试', exact: true })
  await upstream.getByRole('tab', { name: '连通测试', exact: true }).click()
  await expect(upstream.getByRole('heading', { name: '显式连通测试', exact: true })).toBeVisible()
  await expect(upstream).not.toContainText('通过 PowerPoint 服务分别验证普通流式响应与 compact')
  await upstream.getByRole('button', { name: '关闭', exact: true }).click()
  await page.getByRole('checkbox', { name: '选择本页全部账号', exact: true }).locator('xpath=ancestor::label').click()
  await page.getByRole('button', { name: '批量编辑', exact: true }).click()
  const bulk = page.getByRole('dialog', { name: '批量管理', exact: true })
  await expect(bulk).not.toContainText(/Token 额度|窗口天数|重置统计窗口/)
  await bulk.getByRole('button', { name: '关闭', exact: true }).click()
  report.checks.push({ static_connectivity_description_removed: true, bulk_budget_fields_removed: true })
  assert.deepEqual(errors, [])
  assert.deepEqual(state.unexpected, [])
  report.status = 'passed'
  await writeFile(path.join(output, 'report.json'), JSON.stringify(report, null, 2))
  console.log('Official usage browser passed: automatic official query, actual 7d-only mapping, null/failure states, forced refresh, desktop/mobile rendering.')
} finally {
  await browser?.close()
  await new Promise(resolve => server.close(resolve))
}
