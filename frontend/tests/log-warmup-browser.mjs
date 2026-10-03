import assert from 'node:assert/strict'
import { expect } from 'playwright/test'
import { readFile } from 'node:fs/promises'
import { captureLogThemes } from './log-visual-fixtures.mjs'

export async function testLogWarmup(page, { screenshotDirectory } = {}) {
  assert.ok(['localhost', '127.0.0.1'].includes(new URL(page.url()).hostname))
  const requestID = 'req_' + 'x'.repeat(700), responseID = 'r'.repeat(1024)
  const rows = [true, false, null].map((value, index) => ({ id: index + 1, event_id: `warmup-${index}`, time: 1789650000, status: 'ok', request_type: 'websocket', model: 'synthetic-ws', account: ['预热合成记录', '普通生成合成记录', '未观测合成记录'][index], websocket_warmup: value, input_tokens: 0, output_tokens: 0, cached_tokens: 0, usage_known: true, request_id: requestID, response_id: responseID }))
  const calls = { list: [], stats: [], snapshots: [], exports: [], cleanup: [] }, assertions = []
  const matching = query => rows.filter(row => !query.websocket_warmup || typeof row.websocket_warmup === 'boolean' && String(row.websocket_warmup) === query.websocket_warmup)
  const handler = async route => {
    const url = new URL(route.request().url()), query = Object.fromEntries(url.searchParams), name = url.pathname
    if (name === '/api/logs/snapshot') { calls.snapshots.push(route.request().postDataJSON()); return route.fulfill({ json: { as_of: 'usage_warmup_' + calls.snapshots.length, created_at: Math.floor(Date.now() / 1000), expires_at: Math.floor(Date.now() / 1000) + 300, total: matching(route.request().postDataJSON().filters).length } }) }
    if (name === '/api/logs/stats') { calls.stats.push(query); return route.fulfill({ json: { total_requests: matching(query).length, success: matching(query).length, errors: 0, total_tokens: 0, total_input_tokens: 0, total_output_tokens: 0, total_cached_tokens: 0, usage_known_requests: matching(query).length, usage_unknown_requests: 0, average_latency_ms: 0, models: [], series: [] } }) }
    if (name === '/api/logs') { calls.list.push(query); return route.fulfill({ json: { items: matching(query), total: matching(query).length, total_kind: 'exact', has_more: false, next_cursor: '' } }) }
    if (name === '/api/logs/export') { calls.exports.push(query); const body = 'time,websocket_warmup\n1789650000,' + query.websocket_warmup + '\n'; return route.fulfill({ body, headers: { 'content-type': 'text/csv', 'content-disposition': 'attachment; filename="warmup.csv"', 'x-export-count': String(matching(query).length), 'content-length': String(Buffer.byteLength(body)) } }) }
    if (name === '/api/logs/cleanup/jobs') return route.fulfill({ json: { items: [], total: 0 } })
    if (name === '/api/logs/cleanup/preview') { const body = route.request().postDataJSON(); calls.cleanup.push(body); return route.fulfill({ json: { preview_id: 'warmup-preview', confirmation_token: 'synthetic-preview', total: 1, scope: 'retained_request_details', expires_at: Math.floor(Date.now() / 1000) + 300, filters: body.filters } }) }
    return route.fulfill({ json: { record: rows.find(row => name.endsWith(row.event_id)), scope: 'retained_request_details' } })
  }
  await page.route('**/api/logs**', handler)
  try {
    await page.setViewportSize({ width: 1440, height: 1100 })
    await page.goto(new URL('/#/settings', page.url()).href)
    await page.goto(new URL('/#/logs', page.url()).href)
    const table = page.locator('.log-table'), drawer = page.getByRole('dialog', { name: '日志详情', exact: true })
    await expect(table.locator('.el-table__row')).toHaveCount(3)
    for (const [index, label, source] of [[0, '已观测为预热', 'generate=false'], [1, '已观测为普通生成', 'generate=true 或字段缺省'], [2, '预热状态未记录', '不能据此判断为普通生成']]) {
      await table.getByRole('button', { name: /日志详情/ }).nth(index).click()
      await expect(drawer.locator('.observation-warmup')).toContainText(label)
      await expect(drawer.locator('.observation-warmup')).toContainText(source)
      if (index === 0) await captureLogThemes(page, drawer.locator('.observation-warmup'), 'websocket-warmup-detail', screenshotDirectory)
      await drawer.getByRole('button', { name: '关闭详情', exact: true }).click()
    }
    assertions.push('WebSocket 预热/普通生成/未知保持三态，详情明示generate观测来源，日间/夜间手机可读')

    await page.getByRole('button', { name: /高级筛选/ }).click()
    const select = async (name, option) => {
      const input = page.getByRole('combobox', { name, exact: true })
      await input.press('Enter')
      await expect(input).toHaveAttribute('aria-expanded', 'true')
      const listboxID = await input.getAttribute('aria-controls')
      assert.ok(listboxID, `${name} must identify its own options list`)
      await page.locator(`[id="${listboxID}"]`).getByRole('option', { name: option, exact: true }).click()
      await expect(input).toHaveAttribute('aria-expanded', 'false')
    }
    await select('WebSocket 预热', '已观测为预热')
    await page.getByRole('textbox', { name: '发送模型', exact: true }).fill('actual-sent')
    await page.getByRole('textbox', { name: '返回模型', exact: true }).fill('actual-returned')
    await select('入站标准路径', '/responses'); await select('上游标准路径', '/responses')
    await select('发送与返回模型', '已观测模型不同'); await select('错误截断', '已观测为未截断')
    await page.getByRole('textbox', { name: '开始时间', exact: true }).fill('2026-09-01T00:00')
    await page.getByRole('textbox', { name: '结束时间', exact: true }).fill('2026-09-02T00:00')
    await page.getByRole('button', { name: '刷新日志', exact: true }).click()
    await expect(table.locator('.el-table__row')).toHaveCount(1)
    await expect(table).toContainText('预热合成记录')
    const expected = { websocket_warmup: 'true', sent_model: 'actual-sent', returned_model: 'actual-returned', inbound_path: '/responses', upstream_path: '/responses', returned_model_mismatch: 'true', error_truncated: 'false' }
    for (const [key, value] of Object.entries(expected)) {
      assert.equal(calls.list.at(-1)[key], value); assert.equal(calls.stats.at(-1)[key], value); assert.equal(calls.snapshots.at(-1).filters[key], value)
      assert.equal(new URLSearchParams(new URL(page.url()).hash.split('?')[1]).get(key), value)
    }
    await page.getByRole('button', { name: '导出全部筛选', exact: true }).click()
    const download = page.waitForEvent('download')
    await page.getByRole('dialog', { name: '导出全部筛选结果' }).getByRole('button', { name: '导出全部记录', exact: true }).click(); await download
    await page.getByRole('button', { name: '清理明细', exact: true }).click()
    const cleanup = page.getByRole('dialog', { name: '清理请求明细', exact: true })
    await cleanup.getByRole('button', { name: '预览匹配记录', exact: true }).click()
    await expect.poll(() => calls.cleanup.length).toBe(1)
    for (const [key, value] of Object.entries(expected)) { assert.equal(calls.exports.at(-1)[key], value); assert.equal(calls.cleanup.at(-1).filters[key], value) }
    await cleanup.getByRole('button', { name: '关闭', exact: true }).click()
    await select('WebSocket 预热', '已观测为普通生成')
    await page.getByRole('button', { name: '刷新日志', exact: true }).click()
    await expect(table).toContainText('普通生成合成记录'); await expect(table.locator('.el-table__row')).toHaveCount(1)
    assert.equal(calls.list.at(-1).websocket_warmup, 'false')
    const currentDownload = page.waitForEvent('download')
    await page.getByRole('button', { name: '导出当前页（脱敏）', exact: true }).click()
    assert.equal(JSON.parse(await readFile(await (await currentDownload).path(), 'utf8')).websocket_warmup, false)
    assertions.push('预热筛选true/false不包含未知，贯穿URL/快照/统计/全量导出；清理同时保留新观测字段，当前页导出保留false')

    await page.context().grantPermissions(['clipboard-read', 'clipboard-write'])
    await table.getByRole('button', { name: /日志详情/ }).click()
    await expect(drawer.getByRole('button', { name: '复制响应 ID', exact: true })).toBeEnabled()
    await drawer.getByRole('button', { name: '复制响应 ID', exact: true }).click()
    await expect.poll(() => page.evaluate(() => navigator.clipboard.readText())).toBe(responseID)
    await drawer.getByRole('button', { name: '关闭详情', exact: true }).click()
    const identity = page.getByRole('textbox', { name: '记录 ID', exact: true })
    for (const id of [requestID, responseID]) { await identity.fill(id); await identity.press('Enter'); await expect.poll(() => calls.list.at(-1).request_id).toBe(id); assert.equal(calls.snapshots.at(-1).filters.request_id, id) }
    await page.getByRole('button', { name: '保存筛选', exact: true }).click()
    const save = page.getByRole('dialog', { name: '保存常用筛选', exact: true })
    await save.getByRole('textbox', { name: '筛选名称', exact: true }).fill('完整响应ID与普通生成')
    await save.getByRole('button', { name: '保存', exact: true }).click()
    await page.getByRole('button', { name: '重置', exact: true }).click()
    if (await page.getByRole('combobox', { name: '使用常用筛选', exact: true }).isHidden()) await page.getByRole('button', { name: /高级筛选/ }).click()
    await select('使用常用筛选', '完整响应ID与普通生成')
    await expect.poll(() => calls.list.at(-1).request_id).toBe(responseID)
    assert.equal(calls.list.at(-1).websocket_warmup, 'false')
    assertions.push('详情复制1024字符响应ID与704字符请求ID均完整查询，URL及收藏恢复不截断标识或false预热条件')

    for (const invalid of ['r'.repeat(1025), '请'.repeat(342)]) {
      const before = calls.snapshots.length
      await identity.fill(invalid); await identity.press('Enter')
      await expect(page.locator('.logs-page')).toContainText('请求 / 记录 ID 不能超过 1024 个 UTF-8 字节')
      assert.equal(calls.snapshots.length, before)
      await page.getByRole('button', { name: '保存筛选', exact: true }).click()
      await save.getByRole('textbox', { name: '筛选名称', exact: true }).fill('不得保存超长ID')
      await save.getByRole('button', { name: '保存', exact: true }).click(); await expect(save).toBeVisible()
      await save.getByRole('button', { name: '取消', exact: true }).click()
      await page.goto(new URL('/#/logs?request_id=' + encodeURIComponent(invalid), page.url()).href)
      await expect(page.locator('.logs-page')).toContainText('请求 / 记录 ID 不能超过 1024 个 UTF-8 字节')
      assert.equal(calls.snapshots.length, before)
    }
    await page.getByRole('button', { name: '清除已保存筛选', exact: true }).click()
    await page.getByRole('button', { name: '重置', exact: true }).click()
    await expect(table.locator('.el-table__row')).toHaveCount(3)
    assertions.push('超长ASCII/多字节ID在手动查询、URL和收藏均明确拒绝，禁止静默截成另一ID发送')
    return { assertions, calls }
  } finally { await page.unroute('**/api/logs**', handler) }
}
