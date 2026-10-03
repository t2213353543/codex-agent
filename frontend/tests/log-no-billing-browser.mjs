import assert from 'node:assert/strict'
import { expect } from 'playwright/test'
import { readFile } from 'node:fs/promises'
import { captureLogThemes } from './log-visual-fixtures.mjs'

export async function testLogsNoBilling(page, { screenshotDirectory } = {}) {
  assert.ok(['localhost', '127.0.0.1'].includes(new URL(page.url()).hostname))
  const forbidden = /费率|成本|售价|采购|计费|USD|\$\d/
  const row = { id: 1, event_id: 'operations-only', time: 1789650000, account_id: 'account-1', account: '运行指标合成账号', model: 'gpt-synthetic', endpoint: 'responses', status: 'ok', http_code: 200, input_tokens: 100, output_tokens: 20, cached_tokens: 40, usage_known: true, latency_ms: 500, request_preparation_ms: 0, observation_schema_version: 1, cost: { status: 'known', amount_usd: '111111' }, account_cost: { status: 'known', amount_usd: '222222' }, sale_cost: { status: 'known', amount_usd: '333333' } }
  const totals = { total_requests: 4, success: 3, errors: 1, total_input_tokens: 400, total_output_tokens: 80, total_cached_tokens: 160, total_tokens: 480, average_latency_ms: 500, usage_known_requests: 4, usage_unknown_requests: 0, request_preparation_samples: 2, average_request_preparation_ms: 0, account_selection_samples: 1, average_account_selection_ms: 5, proxy_selection_samples: 0, average_proxy_selection_ms: null, estimated_cost_usd: '111111', account_cost_usd: '222222', sale_cost_usd: '333333' }
  const calls = [], errors = [], assertions = []
  const handler = async route => {
    const url = new URL(route.request().url()), path = url.pathname
    calls.push(path)
    if (/pricing|cost-rules|cost-policy|sale-policy/.test(path)) { errors.push(path); return route.fulfill({ status: 410, json: { error: { message: 'removed' } } }) }
    if (path === '/api/logs/snapshot') return route.fulfill({ json: { as_of: 'usage_operations_' + calls.length, created_at: Math.floor(Date.now()/1000), expires_at: Math.floor(Date.now()/1000)+300, total: 4 } })
    if (path === '/api/logs/stats') return route.fulfill({ json: { ...totals, models: [{ name: 'gpt-synthetic', ...totals }], endpoints: [], groups: [], error_categories: [], series: [0,1].map(index => ({ ...totals, time: 1789642800+index*3600 })), accounts: [{ ...totals, id: 'account-1', name: '运行指标合成账号' }] } })
    if (path === '/api/logs') return route.fulfill({ json: { items: [row], total: 4, total_kind: 'exact', has_more: false, next_cursor: '' } })
    return route.fulfill({ json: { record: row, scope: 'retained_request_details' } })
  }
  await page.route('**/api/logs**', handler)
  try {
    await page.setViewportSize({ width: 1440, height: 1100 })
    await page.goto(new URL('/#/settings', page.url()).href)
    await page.goto(new URL('/#/logs', page.url()).href)
    const workspace = page.locator('.logs-page')
    await expect(page.locator('.log-table .el-table__row')).toHaveCount(1)
    assert.doesNotMatch(await workspace.innerText(), forbidden)
    await expect(page.getByRole('button', { name: '刷新日志', exact: true })).toBeEnabled()
    await expect(page.getByRole('button', { name: '清理明细', exact: true })).toBeEnabled()
    await expect(page.getByRole('button', { name: '错误处理', exact: true })).toBeEnabled()
    await captureLogThemes(page, null, 'logs-operations-only', screenshotDirectory)
    assertions.push('日志首屏不含任何费率/成本/售价/采购入口或金额，旧响应金额字段不会显示，保留刷新/错误处理/清理')

    await page.getByRole('button', { name: /趋势与分布/ }).click()
    await expect(page.getByRole('group', { name: '分析指标' }).getByRole('button')).toHaveText(['请求数','Token','输入','输出','缓存'])
    await page.getByRole('group', { name: '分析指标' }).getByRole('button', { name: '输入', exact: true }).click()
    await expect(page.locator('.trend-heading')).toContainText('最高 400')
    await page.getByRole('combobox', { name: '排行排序', exact: true }).selectOption('success_rate')
    await expect(page.locator('.ranking-row')).toContainText('75%')
    assert.doesNotMatch(await workspace.innerText(), forbidden)
    await captureLogThemes(page, page.locator('.usage-ranking'), 'logs-operational-ranking', screenshotDirectory)
    await page.getByRole('button', { name: '显示列', exact: true }).click()
    const columns = page.getByRole('dialog', { name: '使用记录显示列', exact: true })
    await expect(columns.getByRole('checkbox', { name: '耗时', exact: true })).toBeChecked()
    assert.doesNotMatch(await columns.innerText(), forbidden)
    await columns.getByRole('checkbox', { name: '链路观测', exact: true }).check()
    await columns.getByRole('button', { name: '应用显示设置', exact: true }).click()
    await expect(page.locator('.observation-cell')).toHaveCount(2)
    assertions.push('分析模式和排行只保留请求/Token/缓存/成功率，观测与耗时列仍可配置，日间/夜间排行可读')

    await page.locator('.log-table').getByRole('button', { name: /日志详情/ }).click()
    const drawer = page.getByRole('dialog', { name: '日志详情', exact: true })
    await expect(drawer).toContainText('请求准备')
    await expect(drawer).toContainText('0 ms')
    assert.doesNotMatch(await drawer.innerText(), forbidden)
    await drawer.getByRole('button', { name: '关闭详情', exact: true }).click()
    const downloadEvent = page.waitForEvent('download')
    await page.getByRole('button', { name: '导出当前页（脱敏）', exact: true }).click()
    const download = await downloadEvent, content = await readFile(await download.path(), 'utf8'), record = JSON.parse(content)
    assert.equal(record.input_tokens, 100)
    assert.ok(Object.keys(record).every(key => !/cost|price|sale|usd/.test(key)))
    assert.deepEqual(errors, [])
    for (const width of [390,320]) { await page.setViewportSize({ width, height: 844 }); assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true); assert.doesNotMatch(await workspace.innerText(), forbidden) }
    await page.setViewportSize({ width: 1440, height: 1100 })
    await page.getByRole('button', { name: '显示列', exact: true }).click();await columns.getByRole('button', { name: '恢复默认显示', exact: true }).click();await columns.getByRole('button', { name: '应用显示设置', exact: true }).click()
    assertions.push('详情仅显示请求/用量/耗时/观测，当前页导出拒绝所有金额字段，无计费API请求，390/320布局正常')
    return { assertions, requests: calls }
  } finally { await page.unroute('**/api/logs**', handler) }
}
