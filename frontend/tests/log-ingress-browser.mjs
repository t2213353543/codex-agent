import assert from 'node:assert/strict'
import { expect } from 'playwright/test'
import { captureLogThemes } from './log-visual-fixtures.mjs'

export async function testLogIngress(page, { screenshotDirectory } = {}) {
  assert.ok(['localhost', '127.0.0.1'].includes(new URL(page.url()).hostname))
  const calls = [], assertions = [], liveCalls = []
  let mode = '', lateResolve
  const row = (id, caller = 'caller-1') => ({ id, sequence: 20, time_ms: 1789650000123, http_code: 401, reason: 'invalid_api_key', endpoint: '/responses', client_request_id: caller, authentication_ms: 0, token: 'must-never-render', ip: 'must-never-render' })
  const result = (items, query) => ({ items, total: 55, total_kind: 'exact', has_more: !query.cursor, next_cursor: query.cursor ? '' : 'ingress-second', scope: 'retained_ingress_events', source: 'gateway_ingress_v1', checked_at: 1789650100, retention_limit: 1000, evicted_total: 15, collector: { configured: true, running: true, queue_capacity: 256, queued: 0, pending: 2, queue_dropped: 3, persistence_failures: 1, persistence_unconfirmed: 2, unpersisted_at_shutdown: 0, last_persisted_at: 1789650000, last_error: '', counter_scope: 'current_process' } })
  const handler = async route => {
    const url = new URL(route.request().url()), query = Object.fromEntries(url.searchParams)
    assert.equal(route.request().method(), 'GET'); assert.equal(route.request().postData(), null)
    calls.push(query)
    if (mode === 'old-cursor' && query.cursor) return route.fulfill({ status: 409, json: { error: { message: 'retained floor changed' } } })
    if (query.client_request_id === 'slow') await new Promise(resolve => { lateResolve = resolve })
    if (mode === 'error') return route.fulfill({ status: 503, json: { error: { message: 'storage_unavailable' } } })
    const items = query.reason === 'unknown_endpoint' ? [] : [row(query.cursor ? 'second-page' : query.client_request_id === 'fast' ? 'fast-record' : 'first-page', query.client_request_id || 'caller-1')]
    try { await route.fulfill({ json: result(items, query) }) } catch { /* Canceled response is deliberately ignored. */ }
  }
  const logHandler = route => {
    const path = new URL(route.request().url()).pathname
    liveCalls.push(path)
    if (path === '/api/logs/snapshot') return route.fulfill({ json: { as_of: 'usage_ingress_parent', expires_at: Math.floor(Date.now() / 1000) + 300, total: 0 } })
    if (path === '/api/logs/stats') return route.fulfill({ json: { total_requests: 0, success: 0, errors: 0, total_tokens: 0, total_input_tokens: 0, total_output_tokens: 0, total_cached_tokens: 0, models: [], series: [] } })
    return route.fulfill({ json: { items: [], total: 0, total_kind: 'exact', has_more: false, next_cursor: '' } })
  }
  await page.route('**/api/ingress-events**', handler); await page.route('**/api/logs**', logHandler)
  try {
    await page.setViewportSize({ width: 1440, height: 1100 }); await page.goto(new URL('/#/settings', page.url()).href)
    await page.goto(new URL('/#/logs?account_id=not-ingress&from=1788220800&to=1788307200&client_request_id=caller-1', page.url()).href)
    await expect(page.getByRole('button', { name: '准入错误', exact: true })).toBeVisible()
    assert.equal(calls.length, 0)
    const firstIngressResponse = page.waitForResponse(response => new URL(response.url()).pathname === '/api/ingress-events' && response.status() === 200)
    await page.getByRole('button', { name: '准入错误', exact: true }).click(); const dialog = page.getByRole('dialog', { name: '准入错误', exact: true })
    assert.equal((await (await firstIngressResponse).json()).retention_limit, 1000)
    await expect(dialog.locator('.ingress-event')).toHaveCount(1)
    assert.equal(calls[0].client_request_id, 'caller-1'); assert.equal(calls[0].from, '1788220800'); assert.equal(calls[0].to, '1788307200')
    for (const key of ['account_id', 'key_id', 'model', 'as_of', 'error_severity']) assert.equal(key in calls[0], false)
    await expect(dialog).toContainText('不计入使用记录、Token 或请求成功率')
    await expect(dialog).toContainText('匹配 55 条留存事件 · 当前页 1 条')
    await expect(dialog).toContainText('全局最近最多 1K 条'); await expect(dialog).toContainText('淘汰 15 条')
    await dialog.locator('.ingress-collector summary').click(); await expect(dialog).toContainText('重启后清零')
    await expect(dialog.locator('.ingress-collector')).toContainText('队列丢弃')
    await expect(dialog.locator('.ingress-collector')).toContainText('2 条；可能已保存或未保存，不等于确定丢弃')
    await dialog.locator('.ingress-event summary').click(); await expect(dialog.locator('.ingress-event')).toContainText('0 ms')
    await expect(dialog).not.toContainText('must-never-render')
    await expect(dialog.getByRole('button', { name: /探测|重放|删除|保存处理/ })).toHaveCount(0)
    await captureLogThemes(page, dialog, 'ingress-events', screenshotDirectory)
    await captureLogThemes(page, dialog.locator('.ingress-collector'), 'ingress-collector', screenshotDirectory, { widths: [1440, 390, 320] })
    await captureLogThemes(page, dialog.locator('.ingress-event'), 'ingress-event-detail', screenshotDirectory, { widths: [1440, 390, 320] })
    assertions.push('准入入口默认零请求，只GET读取独立留存事件，继承时间/客户端ID而非业务筛选；留存上限、淘汰及进程丢弃明确')

    const next = dialog.getByRole('button', { name: '下一页准入事件', exact: true })
    await next.click(); await expect(dialog.locator('.ingress-event')).toContainText('second-page')
    assert.equal(calls.at(-1).cursor, 'ingress-second')
    const caller = dialog.getByRole('textbox', { name: '准入客户端关联 ID', exact: true })
    await caller.fill('new-caller'); await expect(next).toBeDisabled()
    await dialog.getByRole('button', { name: '刷新准入事件', exact: true }).click(); await expect.poll(() => calls.at(-1).client_request_id).toBe('new-caller')
    assert.equal(calls.at(-1).cursor, undefined)
    mode = 'old-cursor'; await next.click(); await expect(dialog).toContainText('留存窗口已变化')
    await expect(dialog).toContainText('上次成功加载的第 1 页')
    await dialog.getByRole('button', { name: '从第一页重试', exact: true }).click(); await expect(dialog.locator('.ingress-error')).toHaveCount(0)
    assert.equal(calls.at(-1).cursor, undefined); mode = ''
    assertions.push('准入分页采用独立cursor，草稿修改暂停翻页，重新查询与409留存变更从第一页恢复且保留旧结果')

    const before = calls.length
    await dialog.getByRole('textbox', { name: '准入 HTTP 状态', exact: true }).fill('200')
    await dialog.getByRole('button', { name: '刷新准入事件', exact: true }).click(); await expect(dialog).toContainText('400–599'); assert.equal(calls.length, before)
    await dialog.getByRole('textbox', { name: '准入 HTTP 状态', exact: true }).fill('401')
    await dialog.getByRole('combobox', { name: '准入原因', exact: true }).selectOption('unknown_endpoint')
    await dialog.getByRole('button', { name: '刷新准入事件', exact: true }).click(); await expect(dialog).toContainText('当前范围没有留存的准入错误')
    await expect(dialog).toContainText('不证明历史上未发生拒绝或采集丢弃')
    await dialog.getByRole('combobox', { name: '准入原因', exact: true }).selectOption('')
    mode = 'error'; await dialog.getByRole('button', { name: '刷新准入事件', exact: true }).click(); await expect(dialog.locator('.ingress-error')).toContainText('storage_unavailable')
    mode = ''; await dialog.getByRole('button', { name: '从第一页重试', exact: true }).click(); await expect(dialog.locator('.ingress-event')).toHaveCount(1)
    assertions.push('准入HTTP/原因独立校验，非法条件零请求；空态不声称历史无拒绝，存储失败可明确重试')

    await caller.fill('slow'); await caller.press('Enter'); await expect.poll(() => typeof lateResolve).toBe('function')
    await caller.fill('fast'); await caller.press('Enter'); await expect(dialog.locator('.ingress-event')).toContainText('fast-record')
    lateResolve(); lateResolve = undefined; await page.waitForTimeout(80); await expect(dialog.locator('.ingress-event')).toContainText('fast-record')
    await caller.fill('slow'); await caller.press('Enter'); await expect.poll(() => typeof lateResolve).toBe('function')
    await dialog.getByRole('button', { name: '关闭准入错误', exact: true }).click(); lateResolve(); lateResolve = undefined
    await page.getByRole('button', { name: '准入错误', exact: true }).click(); await expect(caller).toHaveValue('caller-1')
    await expect(dialog.locator('.ingress-event')).toContainText('first-page')
    await page.setViewportSize({ width: 320, height: 844 }); assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true)
    await dialog.getByRole('button', { name: '关闭准入错误', exact: true }).click(); await page.setViewportSize({ width: 1440, height: 1100 })
    assert.ok(liveCalls.every(path => ['/api/logs/snapshot', '/api/logs', '/api/logs/stats'].includes(path)))
    assertions.push('准入慢查询/关闭后的迟到响应不覆盖新结果，再打开按主条件重新读；320手机无溢出且不发主动探测')
    return { assertions, calls }
  } finally { lateResolve?.(); await page.unroute('**/api/ingress-events**', handler); await page.unroute('**/api/logs**', logHandler) }
}
