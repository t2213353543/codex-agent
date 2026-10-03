import assert from 'node:assert/strict'
import { expect } from 'playwright/test'
import { captureLogThemes } from './log-visual-fixtures.mjs'

export async function testLogAnalyticsBoundaries(page, { screenshotDirectory } = {}) {
  assert.ok(['localhost', '127.0.0.1'].includes(new URL(page.url()).hostname))
  const requests = { snapshots: [], list: [], stats: [] }, assertions = []
  let allUnknown = false
  const totals = (known, unknown, tokens = 0) => ({ total_requests: known + unknown, success: known, errors: unknown, usage_known_requests: known, usage_unknown_requests: unknown, total_tokens: tokens, total_input_tokens: tokens, total_output_tokens: 0, total_cached_tokens: 0, average_latency_ms: 100 })
  const row = { id: 1, event_id: 'analytics-boundary', time: 1789650000, account: '合成边界记录', status: 'error', input_tokens: 0, output_tokens: 0, cached_tokens: 0, usage_known: null }
  const handler = async route => {
    const url = new URL(route.request().url()), query = Object.fromEntries(url.searchParams)
    if (url.pathname === '/api/logs/snapshot') {
      requests.snapshots.push(route.request().postDataJSON())
      return route.fulfill({ json: { as_of: `usage_boundary_${requests.snapshots.length}`, created_at: Math.floor(Date.now() / 1000), expires_at: Math.floor(Date.now() / 1000) + 300, total: 4 } })
    }
    if (url.pathname === '/api/logs/stats') {
      requests.stats.push(query)
      const rows = allUnknown ? [{ name: 'all-unknown', id: 'all-unknown', ...totals(0, 1) }] : [
        { name: 'a-unknown', id: 'a-unknown', ...totals(0, 1) },
        { name: 'b-confirmed-zero', id: 'b-confirmed-zero', ...totals(1, 0) },
        { name: 'c-partial', id: 'c-partial', ...totals(1, 1, 80) },
      ]
      return route.fulfill({ json: { ...totals(allUnknown ? 0 : 2, allUnknown ? 1 : 2, allUnknown ? 0 : 80), models: rows, accounts: rows, groups: [], endpoints: [], error_categories: [], series: rows.map((value, index) => ({ ...value, time: 1789638000 + index * 3600 })), step: 3600 } })
    }
    if (url.pathname === '/api/logs') {
      requests.list.push(query)
      return route.fulfill({ json: { items: [row], total: 4, total_kind: 'exact', has_more: false, next_cursor: '' } })
    }
    return route.fulfill({ status: 404, json: { error: { message: 'No such synthetic endpoint' } } })
  }
  await page.route('**/api/logs**', handler)
  try {
    await page.setViewportSize({ width: 1440, height: 1100 })
    await page.goto(new URL('/#/settings', page.url()).href)
    await page.goto(new URL('/#/logs', page.url()).href)
    await expect(page.locator('.log-table .el-table__row')).toHaveCount(1)
    await page.getByRole('button', { name: /高级筛选/ }).click()
    const timezone = page.getByRole('textbox', { name: '统计时区', exact: true })
    for (const value of ['+08:00', 'Local']) {
      const before = requests.snapshots.length
      await timezone.fill(value)
      await timezone.press('Enter')
      await expect(page.locator('.logs-page')).toContainText('请输入有效的 IANA 时区')
      assert.equal(requests.snapshots.length, before)
    }
    await timezone.fill('america/new_york')
    await timezone.press('Enter')
    await expect.poll(() => requests.stats.at(-1)?.timezone).toBe('America/New_York')
    assert.equal(requests.snapshots.at(-1).filters.timezone, 'America/New_York')
    assert.equal(requests.list.at(-1).timezone, 'America/New_York')
    assertions.push('时区输入拒绝数字偏移和 Local 且不发请求，合法命名时区规范后用于快照/列表/统计')

    await page.getByRole('button', { name: /趋势与分布/ }).click()
    const metrics = page.getByRole('group', { name: '分析指标' })
    const models = page.locator('.usage-distributions > section').filter({ has: page.getByRole('heading', { name: /请求模型分布/ }) })
    const rankRow = name => page.locator('.ranking-row').filter({ has: page.getByRole('button', { name, exact: true }) })
    for (const metric of ['Token', '输入', '输出', '缓存']) {
      await metrics.getByRole('button', { name: metric, exact: true }).click()
      await expect(models.getByRole('button', { name: /a-unknown/ }).locator('strong')).toHaveText('—')
      await expect(models.getByRole('button', { name: /b-confirmed-zero/ }).locator('strong')).toHaveText('0')
      await expect(page.locator('.chart-point')).toHaveCount(2)
      await expect(page.locator('.trend-coverage')).toHaveText('1 个时段用量未记录，曲线保留空缺；1 个时段仅显示已记录合计')
    }
    await metrics.getByRole('button', { name: 'Token', exact: true }).click()
    await expect(page.locator('.ranking-name')).toHaveText(['c-partial', 'b-confirmed-zero', 'a-unknown'])
    await expect(rankRow('a-unknown')).toContainText('用量未记录（1 条请求）')
    await expect(rankRow('a-unknown').locator('span').nth(2)).toHaveText('Token—')
    await expect(rankRow('b-confirmed-zero').locator('span').nth(2)).toHaveText('Token0')
    await expect(rankRow('c-partial')).toContainText('已记录 1 / 2 条请求')
    await expect(models.getByRole('button', { name: /c-partial/ })).toContainText('已记录 1 / 2 条请求')
    await expect(page.locator('.chart-point title').last()).toContainText('80 · 已记录 1 / 2 条请求')
    await expect(page.locator('.usage-summary')).toContainText('已记录合计')
    await captureLogThemes(page, page.locator('.usage-ranking'), 'logs-usage-coverage-ranking', screenshotDirectory)
    await captureLogThemes(page, page.locator('.usage-trend'), 'logs-usage-coverage-trend', screenshotDirectory)
    await metrics.getByRole('button', { name: '请求数', exact: true }).click()
    await expect(page.locator('.chart-point')).toHaveCount(3)
    await expect(page.locator('.trend-coverage')).toHaveCount(0)
    assertions.push('四种 Token 分析区分未知和真零，未知排名置后，部分采样保留合计与覆盖率，请求统计不受影响')

    allUnknown = true
    await page.getByRole('button', { name: '刷新日志', exact: true }).click()
    await expect(page.locator('.usage-summary')).toContainText('尚无已确认的用量')
    await metrics.getByRole('button', { name: 'Token', exact: true }).click()
    await expect(page.locator('.chart-point')).toHaveCount(0)
    await expect(page.locator('.trend-heading')).toContainText('当前范围尚无已记录用量')
    await expect(models.getByRole('button', { name: /all-unknown/ }).locator('strong')).toHaveText('—')
    await expect(rankRow('all-unknown').locator('span').nth(2)).toHaveText('Token—')
    assertions.push('全未记录范围在汇总/趋势/分布/排行一致显示未知，不生成虚假零趋势')
    return { assertions, requests }
  } finally { await page.unroute('**/api/logs**', handler) }
}
