import assert from 'node:assert/strict'
import { expect } from 'playwright/test'
import { captureLogThemes } from './log-visual-fixtures.mjs'

export async function testLogSeverity(page, { screenshotDirectory } = {}) {
  assert.ok(['localhost', '127.0.0.1'].includes(new URL(page.url()).hostname))
  const source = 'management_rules_v1'
  const cases = [
    ['business-200', 200, 'overloaded', 'P1', 'error'], ['upstream-500', 500, 'server_error', 'P1', 'error'],
    ['type-exception', 500, 'unsupported_model', 'P3', 'error'], ['request-large', 413, 'request_too_large', 'P2', 'error'],
    ['no-http', 0, 'unknown', 'unknown', 'aborted'], ['success', 200, '', undefined, 'ok'],
    ['bad-source', 502, 'server_error', 'P1', 'error'],
  ]
  const rows = cases.map(([name, http_code, error_category, error_severity, status], index) => ({ id: index + 1, event_id: name, account: name, model: 'severity-synthetic', endpoint: 'responses', time: 1789650000 + index, http_code, status, error_category, error_severity, error_severity_source: name === 'bad-source' ? '<img src=x onerror="window.__severityXSS=true">' : status === 'ok' ? undefined : source, usage_known: false, input_tokens: 0, output_tokens: 0, cached_tokens: 0, error: status === 'ok' ? '' : 'Synthetic retained failure' }))
  const calls = { snapshots: [], list: [], stats: [], export: [], cleanup: [], errors: [] }, assertions = []
  let badStatsSource = false
  const filtered = query => rows.filter(row => !query.error_severity || row.status !== 'ok' && row.error_severity_source === source && row.error_severity === query.error_severity)
  const totals = count => ({ total_requests: count, success: 0, errors: count, total_input_tokens: 0, total_output_tokens: 0, total_cached_tokens: 0, total_tokens: 0, usage_known_requests: 0, usage_unknown_requests: count, average_latency_ms: 0 })
  const handler = async route => {
    const url = new URL(route.request().url()), path = url.pathname, query = Object.fromEntries(url.searchParams)
    if (path === '/api/logs/snapshot') { const body = route.request().postDataJSON(); calls.snapshots.push(body); return route.fulfill({ json: { as_of: 'usage_severity_' + calls.snapshots.length, created_at: Math.floor(Date.now() / 1000), expires_at: Math.floor(Date.now() / 1000) + 300, total: filtered(body.filters).length } }) }
    if (path === '/api/logs/stats') { calls.stats.push(query); return route.fulfill({ json: { ...totals(filtered(query).length), models: [], series: [], error_severity_source: badStatsSource ? 'untrusted' : source, error_severities: ['P1', 'P2', 'P3', 'unknown'].map(name => ({ name, ...totals(rows.filter(row => row.error_severity === name && row.error_severity_source === source).length) })) } }) }
    if (path === '/api/logs') { calls.list.push(query); return route.fulfill({ json: { items: filtered(query), total: filtered(query).length, total_kind: 'exact', has_more: false, next_cursor: '' } }) }
    if (path === '/api/logs/errors') {
      calls.errors.push(query)
      const items = query.error_severity === 'P1' ? Array.from({ length: 23 }, (_, index) => ({ ...rows[0], event_id: 'review-severity-' + index })) : filtered(query).filter(row => row.status !== 'ok')
      const offset = query.cursor ? 20 : 0
      return route.fulfill({ json: { items: items.slice(offset, offset + 20).map(record => ({ record, review: { resolved: false } })), total: items.length, has_more: offset + 20 < items.length, next_cursor: offset + 20 < items.length ? 'severity-page-2' : '', checked_at: 1789650100 } })
    }
    if (path === '/api/logs/export') { calls.export.push(query); const body = 'error_severity,error_severity_source\n' + query.error_severity + ',' + source + '\n'; return route.fulfill({ body, headers: { 'content-type': 'text/csv', 'content-disposition': 'attachment; filename="severity.csv"', 'x-export-count': '1', 'content-length': String(Buffer.byteLength(body)) } }) }
    if (path === '/api/logs/cleanup/jobs') return route.fulfill({ json: { items: [], total: 0 } })
    if (path === '/api/logs/cleanup/preview') { const body = route.request().postDataJSON(); calls.cleanup.push(body); return route.fulfill({ json: { preview_id: 'severity-preview', confirmation_token: 'synthetic-only', total: 1, scope: 'retained_request_details', expires_at: Math.floor(Date.now() / 1000) + 300, filters: body.filters } }) }
    if (path.endsWith('/review')) return route.fulfill({ json: { review: { record_ref: path.split('/').at(-2), version: 0, resolved: false, note: '', history: [], history_total: 0 } } })
    return route.fulfill({ json: { record: rows.find(row => path.endsWith(row.event_id)) || rows[0], scope: 'retained_request_details' } })
  }
  await page.route('**/api/logs**', handler)
  try {
    await page.setViewportSize({ width: 1440, height: 1100 })
    await page.goto(new URL('/#/settings', page.url()).href); await page.goto(new URL('/#/logs', page.url()).href)
    const table = page.locator('.log-table'), bodyRows = table.locator('.el-table__row')
    await expect(bodyRows).toHaveCount(7)
    for (const [index, expected] of [[0, 'P1'], [1, 'P1'], [2, 'P3'], [3, 'P2'], [4, '级别未知'], [6, '级别未知']]) await expect(bodyRows.nth(index).locator('.log-severity')).toHaveText(expected)
    await expect(bodyRows.nth(5).locator('.log-severity')).toHaveCount(0)
    await expect(bodyRows.nth(0)).toContainText('HTTP 200')
    await expect(bodyRows.nth(2)).toContainText('HTTP 500')
    await expect(bodyRows.nth(0).locator('.log-severity')).toHaveAttribute('title', /管理规则 v1.*不是上游原生/)
    await expect(bodyRows.nth(6).locator('.log-severity')).toHaveAttribute('title', /来源未验证/)
    await bodyRows.nth(0).getByRole('button', { name: /日志详情/ }).click()
    const drawer = page.getByRole('dialog', { name: '日志详情', exact: true })
    await expect(drawer.locator('.detail-severity')).toContainText('management_rules_v1')
    await expect(drawer.locator('.detail-severity')).toContainText('不改变请求结果或用量')
    await captureLogThemes(page, drawer.locator('.detail-severity'), 'severity-detail', screenshotDirectory)
    await drawer.getByRole('button', { name: '关闭详情', exact: true }).click()
    assert.equal(await page.evaluate(() => window.__severityXSS), undefined)
    assertions.push('列表与详情消费真实管理级别，HTTP200失败/P1、HTTP500类型例外/P3原结果不变，成功无badge，来源异常为未知')

    await page.getByRole('button', { name: /高级筛选/ }).click()
    const select = async (name, value) => { await page.getByRole('combobox', { name, exact: true }).press('Enter'); await page.getByRole('option', { name: value, exact: true }).click() }
    await select('错误级别', 'P1')
    await page.getByRole('textbox', { name: '开始时间', exact: true }).fill('2026-09-01T00:00'); await page.getByRole('textbox', { name: '结束时间', exact: true }).fill('2026-09-02T00:00')
    await page.getByRole('button', { name: '刷新日志', exact: true }).click(); await expect(bodyRows).toHaveCount(2)
    for (const value of [calls.list.at(-1), calls.stats.at(-1), calls.snapshots.at(-1).filters]) assert.equal(value.error_severity, 'P1')
    assert.equal(new URLSearchParams(new URL(page.url()).hash.split('?')[1]).get('error_severity'), 'P1')
    await page.getByRole('button', { name: '保存筛选', exact: true }).click()
    const saved = page.getByRole('dialog', { name: '保存常用筛选', exact: true })
    await saved.getByRole('textbox', { name: '筛选名称', exact: true }).fill('P1错误复核'); await saved.getByRole('button', { name: '保存', exact: true }).click()
    await page.getByRole('button', { name: '重置', exact: true }).click()
    if (await page.getByRole('combobox', { name: '使用常用筛选', exact: true }).isHidden()) await page.getByRole('button', { name: /高级筛选/ }).click()
    await select('使用常用筛选', 'P1错误复核'); await expect.poll(() => calls.list.at(-1).error_severity).toBe('P1')
    await page.getByRole('button', { name: '导出全部筛选', exact: true }).click(); const download = page.waitForEvent('download')
    await page.getByRole('dialog', { name: '导出全部筛选结果' }).getByRole('button', { name: '导出全部记录', exact: true }).click(); await download
    assert.equal(calls.export.at(-1).error_severity, 'P1')
    await page.getByRole('button', { name: '清理明细', exact: true }).click(); const cleanup = page.getByRole('dialog', { name: '清理请求明细', exact: true })
    await cleanup.getByRole('button', { name: '预览匹配记录', exact: true }).click(); await expect.poll(() => calls.cleanup.length).toBe(1)
    assert.equal(calls.cleanup.at(-1).filters.error_severity, 'P1'); await cleanup.getByRole('button', { name: '关闭', exact: true }).click()
    assertions.push('主级别筛选贯穿URL/收藏/快照/列表/统计/导出与清理，重置和收藏恢复保持完整条件')

    await page.getByRole('button', { name: '错误处理', exact: true }).click(); const review = page.getByRole('dialog', { name: '错误处理', exact: true })
    await expect(review.getByRole('combobox', { name: '错误处理列表级别' })).toHaveValue('P1')
    await expect(review.locator('.review-list-row')).toHaveCount(20)
    await review.getByRole('button', { name: '下一页错误', exact: true }).click(); await expect(review.locator('.review-list-row')).toHaveCount(3)
    assert.equal(calls.errors.at(-1).cursor, 'severity-page-2')
    await review.getByRole('combobox', { name: '错误处理列表级别' }).selectOption('P2'); await expect(review.locator('.review-list-row')).toHaveCount(1)
    assert.equal(calls.errors.at(-1).cursor, undefined); assert.equal(calls.errors.at(-1).error_severity, 'P2'); assert.equal(calls.errors.at(-1).as_of, undefined)
    await expect(review.locator('.review-list-row .log-severity')).toHaveText('P2')
    await review.getByRole('combobox', { name: '错误处理列表级别' }).selectOption('unknown'); await expect(review.locator('.review-list-row')).toHaveCount(1)
    await expect(review.locator('.log-severity')).toHaveText('级别未知')
    await review.getByRole('combobox', { name: '错误处理列表级别' }).selectOption(''); await expect(review.locator('.review-list-row')).toHaveCount(6)
    assert.equal(calls.errors.at(-1).error_severity, undefined)
    await captureLogThemes(page, review, 'severity-review-list', screenshotDirectory)
    await review.getByRole('button', { name: '关闭错误处理', exact: true }).click(); assert.equal(calls.list.at(-1).error_severity, 'P1')
    await page.getByRole('button', { name: '错误处理', exact: true }).click(); await expect(review.getByRole('combobox', { name: '错误处理列表级别' })).toHaveValue('P1')
    await review.getByRole('button', { name: '关闭错误处理', exact: true }).click()
    assertions.push('错误处理继承主级别，独立切换P2/未知/全部均重置游标且不改变主查询，再打开重新继承')

    await page.getByRole('button', { name: /趋势与分布/ }).click()
    const distribution = page.locator('.usage-distributions > section').filter({ has: page.getByRole('heading', { name: /^错误级别分布/ }) })
    await expect(distribution.getByRole('button')).toHaveCount(4); await expect(distribution).toContainText('不代表上游原生')
    await expect(distribution.locator('.distribution-name')).toHaveText(['P1', 'P2', 'P3', '级别未知'])
    await distribution.getByTitle('筛选错误级别：P3', { exact: true }).click(); await expect.poll(() => calls.list.at(-1).error_severity).toBe('P3')
    badStatsSource = true; await page.getByRole('button', { name: '刷新日志', exact: true }).click(); await expect(distribution).toContainText('来源未验证')
    await expect(distribution.getByRole('button')).toHaveCount(0)
    for (const width of [390, 320]) { await page.setViewportSize({ width, height: 844 }); await expect(page.locator('.mobile-log-error .log-severity')).toHaveText('P3'); assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true) }
    await page.setViewportSize({ width: 1440, height: 1100 })
    await page.getByRole('button', { name: '清除已保存筛选', exact: true }).click(); await page.getByRole('button', { name: '重置', exact: true }).click()
    assertions.push('可信级别分布可精确点筛，来源异常取消分级及点击；390/320手机badge有文字且无横向溢出')
    return { assertions, calls }
  } finally { await page.unroute('**/api/logs**', handler) }
}
