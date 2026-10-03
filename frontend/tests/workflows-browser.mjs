import assert from 'node:assert/strict'
import { createServer } from 'node:http'
import { readFile, writeFile, mkdir } from 'node:fs/promises'
import { fileURLToPath } from 'node:url'
import path from 'node:path'
import { chromium, expect } from 'playwright/test'
import { installMockApi } from './mock-api.mjs'
import { settingsRestoredFixture, testSettingsRestored } from './settings-restored-browser.mjs'
import { testLogsManagement } from './log-management-browser.mjs'

const root = path.resolve(process.env.MANAGEMENT_UI_DIST || fileURLToPath(new URL('../dist/', import.meta.url)))
const output = fileURLToPath(new URL('../../.local/workflows-ui-20260924/', import.meta.url))
await mkdir(output, { recursive: true })
const report = { passed: false, checks: [], screenshots: [], errors: [], escaped: [] }
const server = createServer(async (req, res) => {
  const pathname = new URL(req.url, 'http://localhost').pathname
  if (pathname.startsWith('/api/')) { report.escaped.push(pathname); res.writeHead(500).end(); return }
  try {
    const file = path.resolve(root, pathname === '/' ? 'index.html' : decodeURIComponent(pathname).slice(1))
    assert(file === path.join(root, 'index.html') || file.startsWith(root + path.sep))
    res.writeHead(200, { 'Content-Type': ({ '.html': 'text/html', '.js': 'text/javascript', '.css': 'text/css', '.svg': 'image/svg+xml', '.woff2': 'font/woff2' })[path.extname(file)] || 'application/octet-stream' }).end(await readFile(file))
  } catch { res.writeHead(404).end() }
})
await new Promise(resolve => server.listen(0, '127.0.0.1', resolve))
const base = `http://127.0.0.1:${server.address().port}`
let browser, page
try {
  browser = await chromium.launch({ headless: true, channel: process.env.TEST_BROWSER || 'chrome' })
  const context = await browser.newContext({ viewport: { width: 1440, height: 1000 }, reducedMotion: 'reduce', serviceWorkers: 'block' })
  const state = await installMockApi(context); state.authenticated = true
  page = await context.newPage(); page.setDefaultTimeout(12000); page.on('pageerror', error => report.errors.push(error.message))
  let settings = structuredClone(settingsRestoredFixture), failWrite = false, conflictWrite = false, diagnosticFail = false
  const writes = [], audits = []
  const record = { id: 'log-workflow', event_id: 'log-workflow', time: Math.floor(Date.now() / 1000), account: '示例生产账号', model: 'gpt-6-astra', status: 'error', http_code: 429, endpoint: 'responses', request_type: 'http_sse', input_tokens: 8200, output_tokens: 1200, cached_tokens: 6400, latency_ms: 2084, request_id: 'req_synthetic_workflow', response_id: 'resp_synthetic_workflow', key_name: '开发工作空间', error: 'Rate limit reached. Please retry after the account usage window resets.', error_source: 'upstream', error_truncated: false }
  await page.route('**/api/settings', route => {
    if (route.request().method() === 'GET') return route.fulfill({ json: settings })
    const body = route.request().postDataJSON(); writes.push(body)
    if (failWrite) return route.fulfill({ status: 500, json: { error: { message: '模拟保存失败，草稿应保留' } } })
    if (conflictWrite) { conflictWrite = false; settings.settings_version++; return route.fulfill({ status: 409, json: { error: { message: 'settings conflict' } } }) }
    assert.equal(body.settings_version, settings.settings_version)
    settings = { ...settings, ...body, settings_version: settings.settings_version + 1 }
    return route.fulfill({ json: { ok: true, settings_version: settings.settings_version } })
  })
  await page.route('**/api/logs**', route => {
    const p = new URL(route.request().url()).pathname
    if (p === '/api/logs/snapshot') return route.fulfill({ json: { as_of: 'workflow-snapshot', expires_at: Math.floor(Date.now() / 1000) + 300 } })
    if (p === '/api/logs/stats') return route.fulfill({ json: { total_requests: 1, success: 0, errors: 1, total_input_tokens: 8200, total_output_tokens: 1200, total_cached_tokens: 6400, total_tokens: 9400, models: [], series: [], scope: 'retained_request_details' } })
    if (p === '/api/logs/coverage') return route.fulfill({ json: { detail_count: 1, scope: 'retained_request_details', storage: 'postgres' } })
    if (p.endsWith('/review')) return route.fulfill({ json: { review: { resolved: false, note: '', version: 1 } } })
    return route.fulfill({ json: p === '/api/logs' ? { items: [record], has_more: false, next_cursor: '', total_kind: 'exact', total: 1 } : { record } })
  })
  await page.route('**/api/audit-logs?*', route => {
    const query = Object.fromEntries(new URL(route.request().url()).searchParams); audits.push(query)
    return route.fulfill({ json: { supported: true, items: [{ id: 'audit-fixture', action: 'account.update', entity_id: query.entity_id || 'account-workflow', actor: 'admin', result: 'ok', created_at: record.time }], has_more: false, next_cursor: '' } })
  })
  await page.route('**/api/diagnostics', route => diagnosticFail ? route.fulfill({ status: 503, json: { error: { message: '诊断暂不可用' } } }) : route.fulfill({ json: { version: '0.2.0-fixture', checked_at: record.time, uptime_seconds: 7300, storage_mode: 'postgres', status: 'ok', dependencies: { postgres: { status: 'ok', latency_ms: 2 }, redis: { status: 'ok', latency_ms: 1 } }, capabilities: { audit_logs: true } } }))
  const go = target => page.evaluate(value => { location.hash = value }, target)
  const capture = async (name, theme, width) => {
    await page.setViewportSize({ width, height: width < 768 ? 844 : 1000 })
    await page.evaluate(value => { document.documentElement.dataset.theme = value; document.documentElement.classList.toggle('dark', value === 'dark') }, theme)
    await page.evaluate(() => document.fonts.ready)
    await expect(page.locator('.el-message:visible')).toHaveCount(0, { timeout: 10000 })
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth && document.querySelector('.content').scrollWidth <= document.querySelector('.content').clientWidth + 1), true, `${name}/${width} overflow`)
    const file = `${name}-${theme}-${width}.png`; await page.screenshot({ path: path.join(output, file), animations: 'disabled' }); report.screenshots.push(file)
  }
  await page.goto(base + '/#/logs'); await expect(page.locator('.log-table .el-table__row')).toHaveCount(1)
  await page.getByRole('textbox', { name: '账号名称', exact: true }).fill('pending-filter'); await page.getByRole('button', { name: '撤销筛选修改', exact: true }).click()
  await expect(page.getByRole('textbox', { name: '账号名称', exact: true })).toHaveValue('')
  await page.getByRole('button', { name: '近 24 小时', exact: true }).click(); await expect(page.locator('.query-notices')).toContainText('时间：')
  const trigger = page.locator('.log-table').getByRole('button', { name: /查看 .* 的日志详情/ }).first(); await trigger.click()
  const detail = page.getByRole('dialog', { name: '日志详情', exact: true }); await expect(detail.locator('.el-dialog')).toHaveClass(/detail-dialog/); await expect(detail.locator('.detail-error')).toHaveText(record.error)
  for (const theme of ['light', 'dark']) for (const width of [1440, 390]) {
    await capture('log-detail', theme, width)
    const box = await detail.locator('.el-dialog').boundingBox(), viewport = page.viewportSize()
    assert(Math.abs(box.x + box.width / 2 - viewport.width / 2) < 2); assert(box.y >= 0 && box.y + box.height <= viewport.height + 1)
  }
  await page.setViewportSize({ width: 1440, height: 1000 }); await detail.getByRole('button', { name: '关闭详情', exact: true }).click(); await expect(trigger).toBeFocused()
  report.checks.push('日志筛选撤销、时间标签、中心详情、两主题桌面手机边界和焦点恢复')
  await go('/settings'); const prompt = page.getByRole('textbox', { name: '内置提示词内容' }), ttl = page.getByRole('spinbutton', { name: '会话绑定有效期（秒）' }); await expect(prompt).toBeEnabled()
  const search = page.getByRole('textbox', { name: '查找设置' }); await search.fill('超时'); await search.press('Enter'); await expect(page.getByRole('navigation', { name: '设置分类' }).getByRole('button')).toHaveCount(1); await search.fill('')
  await prompt.fill('Draft kept across refresh'); await ttl.fill('3600'); await expect(page.getByRole('region', { name: '未保存的设置' })).toContainText('2 项修改未保存')
  assert.equal(await page.evaluate(() => !dispatchEvent(new Event('beforeunload', { cancelable: true }))), true)
  await page.evaluate(() => dispatchEvent(new CustomEvent('page-refresh-request', { cancelable: true }))); await expect(prompt).toBeEnabled(); await expect(prompt).toHaveValue('Draft kept across refresh')
  await go('/connection'); const leave = page.getByRole('dialog', { name: '离开设置页面？', exact: true }); await expect(leave).toBeVisible(); await leave.getByRole('button', { name: '继续编辑', exact: true }).click()
  failWrite = true; await page.getByRole('button', { name: '保存全部修改', exact: true }).click(); await expect(page.locator('.management-error')).toContainText('模拟保存失败')
  failWrite = false; conflictWrite = true; await page.getByRole('button', { name: '保存全部修改', exact: true }).click(); await expect(page.locator('.management-error')).toContainText('保留草稿')
  await expect(prompt).toHaveValue('Draft kept across refresh'); await page.getByRole('button', { name: '保存全部修改', exact: true }).click(); await expect(page.getByRole('region', { name: '未保存的设置' })).toHaveCount(0)
  assert.equal(writes.at(-1).builtin_prompt, 'Draft kept across refresh'); assert.equal(writes.at(-1).sticky_session_ttl_seconds, 3600)
  await prompt.fill('Discard me'); await page.getByRole('button', { name: '恢复已保存', exact: true }).click(); await page.getByRole('dialog', { name: '恢复已保存设置', exact: true }).getByRole('button', { name: '恢复', exact: true }).click(); await expect(prompt).toHaveValue('Draft kept across refresh')
  await prompt.fill('Mobile draft'); for (const width of [1440, 390]) { await page.getByRole('region', { name: '内置提示词设置' }).scrollIntoViewIfNeeded(); await capture('settings-drafts', 'dark', width) }
  await page.getByRole('button', { name: '保存全部修改', exact: true }).click(); await expect(page.getByRole('region', { name: '未保存的设置' })).toHaveCount(0)
  report.checks.push('设置搜索、离页/关窗保护、刷新保留、失败与409恢复、批量保存、撤销')
  await page.setViewportSize({ width: 1440, height: 1000 }); await go('/audit-logs'); await expect(page.locator('.audit-table .el-table__row')).toHaveCount(1)
  await page.getByRole('textbox', { name: '操作对象 ID', exact: true }).fill('pending'); await page.getByRole('button', { name: '撤销筛选修改', exact: true }).click(); await expect(page.getByRole('textbox', { name: '操作对象 ID', exact: true })).toHaveValue('')
  await page.getByRole('button', { name: '近 24 小时', exact: true }).click(); await expect(page.locator('.audit-applied')).toContainText('时间：'); await page.locator('.audit-table').getByRole('button', { name: '详情', exact: true }).click()
  await capture('audit-detail', 'light', 1440); await capture('audit-detail', 'dark', 390); await page.getByRole('dialog', { name: '操作详情', exact: true }).getByRole('button', { name: '查看此对象的操作', exact: true }).click(); await expect(page.locator('.audit-applied')).toContainText('account-workflow')
  report.checks.push('审计快捷时间、筛选撤销、中心详情、对象回查')
  await go('/diagnostics'); await expect(page.locator('.diagnostic-progress')).toContainText('2 个依赖连接正常')
  await page.evaluate(() => Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText: async text => { window.__copied = text } } }))
  await page.getByRole('button', { name: '复制摘要', exact: true }).click(); assert((await page.evaluate(() => window.__copied)).includes('PostgreSQL：连接正常'))
  diagnosticFail = true; await page.getByRole('button', { name: '重新检查运行状态' }).click(); await expect(page.getByRole('button', { name: '复制摘要', exact: true })).toBeDisabled()
  diagnosticFail = false; await page.getByRole('button', { name: '重新检查运行状态' }).click(); await expect(page.locator('.diagnostic-progress')).toContainText('结果已更新'); await capture('diagnostics', 'dark', 1440); await capture('diagnostics', 'light', 390)
  report.checks.push('诊断复制与状态更新、错误不冒充当前结果')
  await go('/connection'); await expect(page.getByRole('textbox', { name: '接入模型名称' })).toBeVisible(); await page.getByRole('button', { name: 'gpt-6-astra', exact: true }).click(); await expect(page.getByRole('textbox', { name: '接入模型名称' })).toHaveValue('gpt-6-astra')
  await page.locator('.el-radio-button').filter({ hasText: 'macOS / Linux' }).click(); await page.getByRole('button', { name: '复制密钥命令模板' }).click(); assert.match(await page.evaluate(() => window.__copied), /export .*\ncodex$/); await expect(page.getByRole('button', { name: '复制密钥命令模板' })).toHaveText('已复制')
  await capture('connection-guide', 'light', 1440); await capture('connection-guide', 'dark', 390); await page.reload(); await expect(page.getByRole('radio', { name: 'macOS / Linux' })).toBeChecked()
  report.checks.push('接入模型切换、终端偏好、完整命令复制反馈')
  if (process.env.WORKFLOW_REGRESSION !== '0') { await page.setViewportSize({ width: 1440, height: 1000 }); report.settingsRegression = await testSettingsRestored(page); if (await page.getByRole('region', { name: '未保存的设置' }).count()) { await page.getByRole('button', { name: '恢复已保存', exact: true }).click(); await page.getByRole('dialog', { name: '恢复已保存设置', exact: true }).getByRole('button', { name: '恢复', exact: true }).click() }; report.logsRegression = await testLogsManagement(page) }
  assert.deepEqual(report.errors, []); assert.deepEqual(report.escaped, []); report.passed = true
} catch (error) { report.failure = error.stack; if (page) { report.visibleText = await page.locator('body').innerText(); await page.screenshot({ path: path.join(output, 'failure.png') }) }; console.error(error) }
finally { await browser?.close(); await new Promise(resolve => server.close(resolve)); await writeFile(path.join(output, 'report.json'), JSON.stringify(report, null, 2)); console.log(JSON.stringify({ passed: report.passed, checks: report.checks.length, screenshots: report.screenshots.length, report: path.join(output, 'report.json') })) }
if (!report.passed) process.exitCode = 1
