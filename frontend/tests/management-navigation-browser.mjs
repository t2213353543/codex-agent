import assert from 'node:assert/strict'
import { expect } from 'playwright/test'

// All fixtures belong to this page and are removed before returning to the
// shared browser runner. No request reaches a gateway or a real account.
export async function testManagementNavigation(page) {
  const base = new URL(page.url()).origin
  assert.ok(['127.0.0.1', 'localhost'].includes(new URL(base).hostname))
  const assertions = []
  const requests = []
  const now = 1800000000
  const fixture = (id, name) => ({ id, name, version: 1, enabled: true, status: 'error', groups: [], proxies: [], concurrency: 1, priority: 0, weight: 1, quota: 0, window_days: 30, expires_at: now + 86400, usage: {} })
  const rows = [fixture('navigation-alpha', 'navigation Alpha'), fixture('navigation-beta', 'navigation Beta')]
  let currentRange
  let failOverview = false
  let omitRange = false
  const savedFilters = [
    { name: '导航回归筛选', search: 'navigation', status: 'error', group: '', proxy: '', sort: 'name-desc' },
    { name: '同值回归筛选', search: 'navigation', status: 'error', group: '', proxy: '', sort: 'name-desc' },
  ]
  const originalPreference = await page.evaluate(() => localStorage.getItem('accountFilters'))
  const handler = async (route) => {
    const url = new URL(route.request().url())
    const endpoint = url.pathname
    const params = Object.fromEntries(url.searchParams)
    requests.push({ endpoint, params })
    const reply = (json, status = 200) => route.fulfill({ status, json }).catch(() => {})
    if (endpoint === '/api/session') return reply({ authenticated: true, expires_at: now + 3600 })
    if (endpoint === '/api/settings') return reply({ version: 'navigation-fixture', settings_version: 1 })
    if (endpoint === '/api/groups') return reply({ groups: [] })
    if (endpoint === '/api/proxies') return reply({ proxies: [] })
    if (endpoint === '/api/accounts/manage') return reply({ accounts: Number(params.page) > 1 ? [fixture('navigation-next', 'navigation next page')] : rows, total: 21, total_kind: 'exact', page: Number(params.page) || 1, page_size: 20, has_more: Number(params.page) < 2 })
    if (endpoint === '/api/accounts/upstream/batch') return reply({ results: route.request().postDataJSON().items.map(row => ({ id: row.id, ok: false, code: 'synthetic_no_upstream' })) })
    if (endpoint === '/api/overview') {
      if (failOverview) return reply({ error: { message: 'navigation mock overview unavailable' } }, 503)
      // Deliberately differ from request seconds so the test proves the UI
      // follows the server's normalized window instead of recalculating it.
      const range = { from: Math.floor(Number(params.from) / 60) * 60, to: Math.ceil(Number(params.to) / 60) * 60, step: 60 }
      if (!currentRange || range.to > currentRange.to) currentRange = { ...range }
      return reply({ ...(omitRange ? {} : { range }), cards: { requests: 2, errors: 1, success: 1 }, account_top: [{ name: 'navigation Alpha', requests: 2, errors: 1 }], recent_errors: [{ account: 'navigation Alpha', error: 'navigation fixture error' }], series: [], status: [] })
    }
    if (endpoint === '/api/logs/snapshot') return reply({ as_of: 'navigation-snapshot', expires_at: Math.floor(Date.now() / 1000) + 600, scope: 'retained_request_details' })
    if (endpoint === '/api/logs/stats') return reply({ scope: 'retained_request_details', total_requests: 0, success: 0, errors: 0, aborted: 0, total_input_tokens: 0, total_output_tokens: 0, total_cached_tokens: 0, total_tokens: 0, average_latency_ms: null, usage_known_requests: 0, usage_unknown_requests: 0, series: [] })
    if (endpoint === '/api/logs/coverage') return reply({ scope: 'retained_request_details', detail_count: 0, retention_policy: 'explicit_pruning_and_cleanup' })
    if (endpoint === '/api/logs') return reply({ scope: 'retained_request_details', items: [], has_more: false, next_cursor: '', total_kind: 'not_computed' })
    return route.fallback()
  }
  await page.route('**/api/**', handler)
  const table = () => page.locator('.accounts-panel .el-table')
  const loadedAccounts = async () => {
    await expect(page.locator('.page-header')).toContainText('账号管理')
    await expect(table().locator('.el-loading-mask')).toHaveCount(0)
  }
  const selectSaved = async (name) => {
    await page.getByRole('combobox', { name: '常用筛选', exact: true }).press('Enter')
    await page.getByRole('option', { name, exact: true }).click()
  }
  const queryFromPage = () => new URLSearchParams(new URL(page.url()).hash.split('?')[1] || '')
  const assertFirstPage = async (requestStart) => {
    await expect(page.locator('.account-pagination')).toContainText('第 1 / 2 页')
    await expect(table()).toContainText('navigation Alpha')
    const query = queryFromPage()
    assert.equal(query.has('cursor'), false)
    assert.equal(query.has('page'), false)
    await expect.poll(() => requests.slice(requestStart).filter((item) => item.endpoint === '/api/accounts/manage').length).toBe(1)
    const calls = requests.slice(requestStart).filter((item) => item.endpoint === '/api/accounts/manage')
    assert.ok(calls.every((item) => !item.params.cursor && item.params.page === '1'))
  }
  const nextPage = async () => {
    await page.getByRole('button', { name: '下一页', exact: true }).click()
    await expect(table()).toContainText('navigation next page')
    await expect(page.locator('.account-pagination')).toContainText('第 2 / 2 页')
  }
  const goDashboard = async (range) => {
    currentRange = undefined
    await page.goto(`${base}/#/?range=${range}`)
    await expect(page.locator('.dashboard-page')).toHaveAttribute('aria-busy', 'false')
    await expect(page.getByRole('button', { name: '查看日志', exact: true })).toBeEnabled()
    assert.ok(currentRange)
    return { ...currentRange }
  }
  const assertLogRange = async (expectedRange, account) => {
    await expect(page.locator('.logs-page')).toBeVisible()
    const query = queryFromPage()
    assert.equal(query.get('from'), String(expectedRange.from))
    assert.equal(query.get('to'), String(expectedRange.to))
    assert.equal(query.get('status'), 'failed')
    assert.equal(query.get('account'), account || null)
    await expect.poll(() => requests.filter(item => item.endpoint === '/api/logs').at(-1)?.params.from).toBe(String(expectedRange.from))
    const request = requests.filter(item => item.endpoint === '/api/logs').at(-1)
    assert.equal(request.params.to, String(expectedRange.to))
    assert.equal(request.params.status, 'failed')
  }
  try {
    await page.evaluate((value) => localStorage.setItem('accountFilters', JSON.stringify({ version: 1, value })), savedFilters)
    await page.goto(`${base}/#/accounts?cursor=navigation-page-2&page=2`)
    await page.reload()
    await loadedAccounts()
    await expect(table()).toContainText('navigation next page')
    let requestStart = requests.length
    await selectSaved('导航回归筛选')
    await assertFirstPage(requestStart)
    assert.equal(queryFromPage().get('search'), 'navigation')
    assert.equal(queryFromPage().get('sort_by'), 'name')
    assert.equal(queryFromPage().get('sort_order'), 'desc')
    assert.equal(queryFromPage().get('status'), 'error')
    assertions.push('账号保存筛选同时修改排序：一次导航回首页，新筛选请求不携旧 cursor')

    await nextPage()
    requestStart = requests.length
    await selectSaved('同值回归筛选')
    await assertFirstPage(requestStart)
    assertions.push('保存筛选值与当前相同：仍回首页并清除游标历史')

    await nextPage()
    requestStart = requests.length
    await page.getByRole('combobox', { name: '账号排序方向', exact: true }).press('Enter')
    await page.getByRole('option', { name: '升序', exact: true }).click()
    await assertFirstPage(requestStart)
    assert.equal(queryFromPage().get('sort_by'), 'name')
    assert.equal(queryFromPage().get('sort_order'), 'asc')
    assertions.push('单独变更账号排序：统一回首页，不沿用旧分页')

    for (const [period, entry] of [['1h', 'all'], ['7d', 'rank'], ['30d', 'recent']]) {
      const expectedRange = await goDashboard(period)
      if (entry === 'all') await page.getByRole('button', { name: '查看日志', exact: true }).click()
      else if (entry === 'rank') await page.getByRole('button', { name: '查看 navigation Alpha 的错误日志', exact: true }).click()
      else await page.locator('.incident-list').getByRole('button', { name: 'navigation Alpha', exact: true }).click()
      await assertLogRange(expectedRange, entry === 'all' ? undefined : 'navigation Alpha')
    }
    assertions.push('仪表盘 1h/7d/30d 三个错误日志入口：URL 和日志 API 使用服务端归一化范围')

    const previousRange = await goDashboard('1h')
    failOverview = true
    await page.locator('.range-options').getByText('近 7 天', { exact: true }).click()
    await expect(page.locator('.load-error')).toContainText('navigation mock overview unavailable')
    await page.getByRole('button', { name: '查看日志', exact: true }).click()
    await assertLogRange(previousRange)
    assertions.push('概览切换范围失败时，已有数据的错误日志仍使用该数据原范围')

    failOverview = false
    omitRange = true
    const requestStartIndex = requests.length
    await goDashboard('6h')
    const overviewCalls = requests.slice(requestStartIndex).filter((item) => item.endpoint === '/api/overview')
    const requested = overviewCalls.reduce((latest, item) => Number(item.params.to) > Number(latest.params.to) ? item : latest)
    await page.getByRole('button', { name: '查看日志', exact: true }).click()
    await assertLogRange(requested.params)
    assertions.push('旧概览接口未返回 range 时，日志深链使用已成功请求的固定范围')
    return assertions
  } finally {
    await page.unroute('**/api/**', handler)
    await page.evaluate((value) => { if (value === null) localStorage.removeItem('accountFilters'); else localStorage.setItem('accountFilters', value) }, originalPreference)
  }
}
