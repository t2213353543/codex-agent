import assert from 'node:assert/strict'
import { expect } from 'playwright/test'
import { readFile } from 'node:fs/promises'
import { captureLogThemes } from './log-visual-fixtures.mjs'

export async function testLogPassiveObservations(page, { screenshotDirectory } = {}) {
  assert.ok(['localhost', '127.0.0.1'].includes(new URL(page.url()).hostname))
  const clientID = 'caller_' + 'a'.repeat(121)
  const base = { time: 1789650000, status: 'ok', http_code: 200, model: 'passive-model', endpoint: 'responses', input_tokens: 100, output_tokens: 10, cached_tokens: 0, usage_known: true, latency_ms: 70 }
  const rows = [
    { ...base, id: 1, event_id: 'passive-http', account: 'HTTP图片观测', request_type: 'http_sse', client_request_id: clientID, client_request_id_source: 'request_header', client_request_id_scope: 'request', request_id: 'upstream-distinct', response_id: 'response-distinct', image_input_tokens: 0, image_input_tokens_source: 'usage.input_tokens_details.image_tokens', image_output_tokens: 12, image_output_tokens_source: 'usage.output_tokens_details.image_tokens', authentication_ms: 0, response_transfer_ms: 20, timing_source: 'gateway_handler_v1' },
    { ...base, id: 2, event_id: 'passive-ws', account: 'WS图像工具观测', request_type: 'websocket', client_request_id: 'ws-caller', client_request_id_source: 'request_header', client_request_id_scope: 'connection', image_input_tokens: 51, image_input_tokens_source: 'tool_usage.image_gen.input_tokens_details.image_tokens', authentication_ms: 3, response_transfer_ms: 0, timing_source: 'gateway_handler_v1' },
    { ...base, id: 3, event_id: 'passive-unknown', account: '历史缺少被动观测', request_id: 'must-not-be-caller', image_input_tokens: null, image_output_tokens: null, authentication_ms: null, response_transfer_ms: null },
  ]
  const totals = { total_requests: 3, success: 3, errors: 0, total_tokens: 330, total_input_tokens: 300, total_output_tokens: 30, total_cached_tokens: 0, usage_known_requests: 3, usage_unknown_requests: 0, average_latency_ms: 70, image_input_samples: 2, total_image_input_tokens: 51, image_output_samples: 1, total_image_output_tokens: 12, authentication_samples: 2, average_authentication_ms: 1.5, response_transfer_samples: 2, average_response_transfer_ms: 10 }
  const calls = { list: [], stats: [], snapshots: [], export: [], cleanup: [] }, assertions = []
  let imageOverflow = false
  const handler = async route => {
    const url = new URL(route.request().url()), query = Object.fromEntries(url.searchParams), path = url.pathname
    if (path === '/api/logs/snapshot') { calls.snapshots.push(route.request().postDataJSON()); return route.fulfill({ json: { as_of: 'usage_passive_' + calls.snapshots.length, created_at: Math.floor(Date.now() / 1000), expires_at: Math.floor(Date.now() / 1000) + 300, total: 3 } }) }
    if (path === '/api/logs/stats') { calls.stats.push(query); return route.fulfill({ json: { ...totals, ...(imageOverflow ? { total_image_input_tokens: null, image_input_total_overflow: true } : {}), models: [], series: [], available: { image_input_tokens: true, image_output_tokens: true, authentication_ms: true, response_transfer_ms: true } } }) }
    if (path === '/api/logs') { calls.list.push(query); return route.fulfill({ json: { items: rows, total: 3, total_kind: 'exact', has_more: false, next_cursor: '' } }) }
    if (path === '/api/logs/export') { calls.export.push(query); const body = 'client_request_id,image_input_tokens\nredacted,0\n'; return route.fulfill({ body, headers: { 'content-type': 'text/csv', 'content-disposition': 'attachment; filename="passive.csv"', 'x-export-count': '1', 'content-length': String(Buffer.byteLength(body)) } }) }
    if (path === '/api/logs/cleanup/jobs') return route.fulfill({ json: { items: [], total: 0 } })
    if (path === '/api/logs/cleanup/preview') { const body = route.request().postDataJSON(); calls.cleanup.push(body); return route.fulfill({ json: { preview_id: 'passive-preview', confirmation_token: 'synthetic-passive', total: 1, scope: 'retained_request_details', expires_at: Math.floor(Date.now() / 1000) + 300, filters: body.filters } }) }
    return route.fulfill({ json: { record: rows.find(row => path.endsWith(row.event_id)), scope: 'retained_request_details' } })
  }
  await page.route('**/api/logs**', handler)
  try {
    await page.setViewportSize({ width: 1440, height: 1100 }); await page.goto(new URL('/#/settings', page.url()).href); await page.goto(new URL('/#/logs', page.url()).href)
    const table = page.locator('.log-table'), drawer = page.getByRole('dialog', { name: '日志详情', exact: true })
    await expect(table.locator('.el-table__row')).toHaveCount(3)
    await table.getByRole('button', { name: /日志详情/ }).first().click()
    await expect(drawer.locator('.client-correlation')).toContainText(clientID)
    await expect(drawer.locator('.client-correlation')).toContainText('本次 HTTP 请求头')
    await expect(drawer.locator('.image-token-details dd').first()).toContainText('0')
    await expect(drawer.locator('.image-token-details')).toContainText('usage.input_tokens_details.image_tokens')
    await expect(drawer.locator('.observation-stage-times')).toContainText('网关授权')
    await expect(drawer.locator('.observation-stage-times')).toContainText('0 ms')
    await expect(drawer.locator('.log-observation-details')).toContainText('上游响应头返回至处理结束')
    await captureLogThemes(page, drawer.locator('.client-correlation'), 'passive-client-correlation', screenshotDirectory)
    await captureLogThemes(page, drawer.locator('.image-token-details'), 'passive-image-detail', screenshotDirectory)
    await drawer.getByRole('button', { name: '关闭详情', exact: true }).click()
    await table.getByRole('button', { name: /日志详情/ }).nth(1).click()
    await expect(drawer.locator('.client-correlation')).toContainText('多个轮次可能共用')
    await expect(drawer.locator('.image-token-details')).toContainText('不假定包含在模型输入或输出内')
    await expect(drawer.locator('.log-observation-details')).toContainText('不包含终态帧向下游的最后写出')
    await captureLogThemes(page, drawer.locator('.observation-stage-times'), 'passive-ws-timing', screenshotDirectory)
    await drawer.getByRole('button', { name: '关闭详情', exact: true }).click()
    await table.getByRole('button', { name: /日志详情/ }).nth(2).click()
    await expect(drawer.locator('.client-correlation')).not.toContainText('must-not-be-caller')
    await expect(drawer.locator('.client-correlation')).toContainText('未记录')
    await expect(drawer.locator('.image-token-details dd').first()).toContainText('—')
    await expect(drawer.locator('.observation-stage-times')).toContainText('未记录')
    await drawer.getByRole('button', { name: '关闭详情', exact: true }).click()
    assertions.push('图片真零/未知及模型和工具来源独立；客户端关联不回填上游ID；HTTP与WS阶段时序分别说明且不相加')

    await page.getByRole('button', { name: /高级筛选/ }).click()
    const client = page.getByRole('textbox', { name: '客户端关联 ID', exact: true })
    await client.fill(clientID); await page.getByRole('textbox', { name: '记录 ID', exact: true }).fill('upstream-distinct')
    await page.getByRole('textbox', { name: '开始时间', exact: true }).fill('2026-09-01T00:00'); await page.getByRole('textbox', { name: '结束时间', exact: true }).fill('2026-09-02T00:00'); await client.press('Enter')
    await expect.poll(() => calls.list.at(-1).client_request_id).toBe(clientID)
    for (const query of [calls.list.at(-1), calls.stats.at(-1), calls.snapshots.at(-1).filters]) { assert.equal(query.client_request_id, clientID); assert.equal(query.request_id, 'upstream-distinct') }
    await page.getByRole('button', { name: '保存筛选', exact: true }).click(); const save = page.getByRole('dialog', { name: '保存常用筛选', exact: true })
    await save.getByRole('textbox', { name: '筛选名称', exact: true }).fill('客户端被动关联'); await save.getByRole('button', { name: '保存', exact: true }).click()
    await page.getByRole('button', { name: '导出全部筛选', exact: true }).click(); const download = page.waitForEvent('download')
    await page.getByRole('dialog', { name: '导出全部筛选结果' }).getByRole('button', { name: '导出全部记录', exact: true }).click(); await download
    assert.equal(calls.export.at(-1).client_request_id, clientID)
    await page.getByRole('button', { name: '清理明细', exact: true }).click(); const cleanup = page.getByRole('dialog', { name: '清理请求明细', exact: true })
    await cleanup.getByRole('button', { name: '预览匹配记录', exact: true }).click(); await expect.poll(() => calls.cleanup.length).toBe(1)
    assert.equal(calls.cleanup.at(-1).filters.client_request_id, clientID); await cleanup.getByRole('button', { name: '关闭', exact: true }).click()
    await page.getByRole('button', { name: '重置', exact: true }).click()
    if (await page.getByRole('combobox', { name: '使用常用筛选', exact: true }).isHidden()) await page.getByRole('button', { name: /高级筛选/ }).click()
    await page.getByRole('combobox', { name: '使用常用筛选', exact: true }).press('Enter'); await page.getByRole('option', { name: '客户端被动关联', exact: true }).click(); await expect.poll(() => calls.list.at(-1).client_request_id).toBe(clientID)
    assert.equal(new URLSearchParams(new URL(page.url()).hash.split('?')[1]).get('client_request_id'), clientID)
    assertions.push('客户端128字节精确筛选独立于上游ID，完整经过URL/收藏/snapshot/统计/导出及清理预览')

    const before = calls.snapshots.length
    for (const invalid of ['a'.repeat(129), '带空 格', 'https://not-an-id']) { await client.fill(invalid); await client.press('Enter'); await expect(page.locator('.logs-page')).toContainText('客户端关联 ID 最多 128'); assert.equal(calls.snapshots.length, before) }
    const listsBefore = calls.list.length
    await client.fill(clientID); await client.press('Enter'); await expect.poll(() => calls.list.length).toBe(listsBefore + 1)
    const currentDownload = page.waitForEvent('download'); await page.getByRole('button', { name: '导出当前页（脱敏）', exact: true }).click()
    const exported = JSON.parse((await readFile(await (await currentDownload).path(), 'utf8')).split('\n')[0])
    assert.equal(exported.image_input_tokens, 0); assert.equal(exported.authentication_ms, 0); assert.equal('client_request_id' in exported, false)
    assertions.push('非法/超长客户端ID明确阻止查询，当前页脱敏仅保留图片与阶段白名单且不泄露关联ID')

    await page.getByRole('button', { name: /趋势与分布/ }).click()
    await expect(page.locator('.usage-image-observation')).toContainText('51'); await expect(page.locator('.usage-image-observation')).toContainText('2 / 3 条请求有记录')
    await expect(page.locator('.usage-image-observation')).toContainText('1 / 3 条请求有记录')
    await expect(page.locator('.usage-summary')).toContainText('330')
    await expect(page.locator('.usage-stage-observation')).toContainText('平均网关授权')
    await expect(page.locator('.usage-stage-observation')).toContainText('平均响应传递')
    await captureLogThemes(page, page.locator('.usage-image-observation'), 'passive-image-coverage', screenshotDirectory)
    imageOverflow = true; await page.getByRole('button', { name: '刷新日志', exact: true }).click()
    await expect(page.locator('.usage-image-observation')).toContainText('合计超出支持范围')
    await expect(page.locator('.usage-image-observation')).toContainText('2 / 3 条请求有记录')
    await page.setViewportSize({ width: 320, height: 844 }); assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true)
    await page.setViewportSize({ width: 1440, height: 1100 }); await page.getByRole('button', { name: '清除已保存筛选', exact: true }).click(); await page.getByRole('button', { name: '重置', exact: true }).click()
    assertions.push('全量图片与阶段显示实际样本覆盖及合计溢出状态，不重复加入总Token，320手机无溢出')
    return { assertions, calls }
  } finally { await page.unroute('**/api/logs**', handler) }
}
