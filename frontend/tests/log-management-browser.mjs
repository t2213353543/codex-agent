import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import { expect } from 'playwright/test'
import { logWorkspacePreviewRows } from './log-workspace-fixtures.mjs'
import { captureLogThemes } from './log-visual-fixtures.mjs'

// The caller supplies an isolated local server and authenticated mock shell.
// No live identities, credentials or production requests are used.
export async function testLogsManagement(page, { screenshotDirectory } = {}) {
  assert.ok(['localhost', '127.0.0.1'].includes(new URL(page.url()).hostname))
  const rows = logWorkspacePreviewRows.map((row, index) => ({ ...row, id: index + 1, event_id: `e_fixture_${index + 1}`, usage_known: index !== 8, key_id: 'key-1', account_id: `account-${index + 1}`, groups: ['group-1'] }))
  Object.assign(rows[0], { upstream_model: 'gpt-5.6-sol-v2', request_type: 'websocket', service_tier: 'priority', reasoning_effort: 'xhigh', request_id: 'req_fixture_1', response_id: 'resp_fixture_1', cache_write_tokens: 100, reasoning_tokens: 500, ttft_ms: 350, user_agent: 'Codex Synthetic Fixture' })
  const requests = { list: [], stats: [], detail: [], detailQueries: [], export: [], options: [], snapshots: [] }
  const assertions = []
  let failStats = false, failDetail = false, exportMode = 'normal', releaseExport
  let expiredPage = false, failSnapshot = false
  let expiredDetail = false
  const totals = (count, errors = 0, unknown = 0) => ({ total_requests: count, success: count - errors, errors, aborted: errors ? 1 : 0, total_input_tokens: count * 150, total_output_tokens: count * 12, total_cached_tokens: count * 75, total_tokens: count * 162, average_latency_ms: 1450, usage_known_requests: count - unknown, usage_unknown_requests: unknown })
  const statsFor = query => {
    const count = query.key_id ? 23 : 1001
    const model = Math.floor(count * .7), endpoint = Math.floor(count * .9)
    const buckets = [Math.floor(count * .1), Math.floor(count * .2), Math.floor(count * .3)]
    buckets.push(count - buckets.reduce((sum, value) => sum + value, 0))
    return { ...totals(count, 3, 1), scope: 'retained_request_details', available: { tokens: true, ttft: false }, checked_at: 1789650300, step: 3600,
      models: [{ name: 'gpt-5.6-sol', ...totals(model, 2, 1) }, { name: 'gpt-5.6-luna', ...totals(count - model, 1) }],
      upstream_models: [{ name: 'gpt-5.6-sol-v2', ...totals(7, 1) }],
      endpoints: [{ name: 'responses', ...totals(endpoint, 2, 1) }, { name: 'compact', ...totals(count - endpoint, 1) }],
      groups: [{ id: 'group-stable', name: '历史分组', ...totals(count) }],
      error_categories: [{ name: 'rate_limit', ...totals(3, 3) }],
      series: buckets.map((value, index) => ({ time: 1789638000 + index * 3600, ...totals(value, index ? 1 : 0, index ? 0 : 1) })),
    }
  }
  const handler = async route => {
    const url = new URL(route.request().url())
    if (url.pathname === '/api/logs/snapshot') {
      requests.snapshots.push(route.request().postDataJSON())
      if (failSnapshot) return route.fulfill({ status: 503, json: { error: { message: '快照准备暂时失败' } } })
      return route.fulfill({ json: { as_of: 'usage_fixture_' + requests.snapshots.length, created_at: Math.floor(Date.now() / 1000), expires_at: Math.floor(Date.now() / 1000) + 300, scope: 'retained_request_details', total: 1001 } })
    }
    const query = Object.fromEntries(url.searchParams)
    const reply = async data => { try { await route.fulfill(data) } catch { /* Expected cancellation. */ } }
    if (url.pathname === '/api/logs/options') {
      requests.options.push(query)
      return reply({ json: { items: query.q === 'empty' ? [] : [{ value: 'same-name-one', label: '同名历史资源', deleted: true }, { value: 'same-name-two', label: '同名历史资源', deleted: false }], has_more: query.q === '', scope: 'retained_request_details' } })
    }
    if (url.pathname === '/api/logs/coverage') return reply({ json: { detail_count: 1100, first_record_at: 1700000000, last_record_at: 1789650300, checked_at: 1789650310, scope: 'retained_request_details', storage: 'postgres', retention_days: null, retention_policy: 'explicit_pruning_and_cleanup', cumulative_events_preserved: true, historical_completeness: 'not_inferable_from_retained_details', export_schema: 'codex-usage-v2' } })
    if (url.pathname === '/api/logs/stats') {
      requests.stats.push(query)
      if (query.account === 'slow') await new Promise(resolve => setTimeout(resolve, 250))
      if (failStats) return reply({ status: 503, json: { error: { message: '统计暂时不可用' } } })
      return reply({ json: statsFor(query) })
    }
    if (url.pathname === '/api/logs/export') {
      requests.export.push(query)
      if (exportMode === 'blocked') await new Promise(resolve => { releaseExport = resolve })
      if (exportMode === 'bad') return reply({ json: { error: { message: '错误 JSON 不能被下载' } } })
      const body = 'time,status\n' + Array.from({ length: 1001 }, () => '1789650300,ok').join('\n') + '\n'
      return reply({ body, headers: { 'content-type': 'text/csv; charset=utf-8', 'content-disposition': 'attachment; filename="usage.csv"', 'x-export-count': '1001', 'content-length': String(Buffer.byteLength(body)) } })
    }
    if (url.pathname.startsWith('/api/logs/')) {
      requests.detail.push(url.pathname)
      requests.detailQueries.push(query)
      if (expiredDetail) return reply({ status: 409, json: { error: { code: 'version_conflict', message: 'snapshot expired' } } })
      if (failDetail) return reply({ status: 404, json: { error: { message: '记录已清理或不存在' } } })
      const row = rows.find(item => url.pathname.endsWith(item.event_id)) || rows[0]
      return reply({ json: { scope: 'retained_request_details', record: { ...row, proxy_name: '完整服务端详情', error: row.error || '' } } })
    }
    requests.list.push(query)
    if (expiredPage) return reply({ status: 409, json: { error: { message: 'snapshot expired' } } })
    if (query.account === 'slow') await new Promise(resolve => setTimeout(resolve, 250))
    const items = query.account ? [{ ...rows[0], account: query.account }] : query.cursor ? [{ ...rows[0], account: '第二页记录' }] : rows
    return reply({ json: { items, total: query.key_id ? 23 : 1001, total_kind: 'exact', has_more: !query.cursor, next_cursor: query.cursor ? '' : 'fixture-cursor-2', scope: 'retained_request_details' } })
  }
  await page.route('**/api/logs**', handler)
  try {
    await page.setViewportSize({ width: 1440, height: 1100 })
    await page.goto(new URL('/#/settings', page.url()).href)
    await expect(page.locator('.settings-panel')).toBeVisible()
    const firstStatsResponse = page.waitForResponse(response => new URL(response.url()).pathname === '/api/logs/stats' && response.status() === 200)
    await page.goto(new URL('/#/logs', page.url()).href)
    const rawStats = await (await firstStatsResponse).json()
    assert.equal(rawStats.total_requests, 1001)
    assert.equal(rawStats.total_tokens, 162162)
    const table = page.locator('.log-table')
    const summary = page.locator('.usage-summary')
    const account = page.getByRole('textbox', { name: '账号名称', exact: true })
    const next = page.getByRole('button', { name: '下一页', exact: true })
    await expect(table.locator('.el-table__row')).toHaveCount(10)
    await expect(summary.locator('strong').first()).toHaveText('1K')
    await expect(page.locator('.query-scope')).toContainText('匹配 1001 条')
    assert.equal((await table.locator('.el-table__row').nth(1).locator('.token-line strong').allTextContents()).join('/'), '0/0/0')
    assert.equal((await table.locator('.el-table__row').nth(8).locator('.token-line strong').allTextContents()).join('/'), '—/—/—')
    const denseLayout = await table.locator('.el-table__row').first().evaluate(row => {
      const pills = [...row.querySelectorAll('.token-pills > .token-line')]
      const boxes = pills.map(el => el.getBoundingClientRect())
      const parent = row.querySelector('.token-pills')
      return { count: pills.length, y: boxes.map(box => box.y), height: row.getBoundingClientRect().height,
        fits: parent.scrollWidth <= parent.clientWidth + 1,
        backgrounds: pills.map(el => getComputedStyle(el).backgroundColor) }
    })
    assert.equal(denseLayout.count, 3)
    assert.ok(Math.max(...denseLayout.y) - Math.min(...denseLayout.y) <= 1, 'Input/output/cache must share one line')
    assert.equal(denseLayout.fits, true, 'All three token values must be visible without local scrolling')
    const typicalHeight = await table.locator('.el-table__row').nth(5).evaluate(row => row.getBoundingClientRect().height)
    assert.ok(typicalHeight <= 76, `Typical success row must stay compact: ${typicalHeight}px`)
    await expect(table.locator('.el-table__row').first().locator('.model-chip-row')).toContainText('priority')
    await expect(table.locator('.el-table__row').first().locator('.model-chip-row')).toContainText('xhigh')
    await expect(table.locator('.el-table__row').first().locator('.token-input')).toHaveAttribute('title', '输入 Token：16,832')
    await expect(table.locator('.el-table__row').first().locator('.token-cache')).toHaveAttribute('title', /^缓存读取 Token：12,288/)
    assert.equal(new Set(denseLayout.backgrounds).size, 3, 'Token types need distinct semantic backgrounds')
    assertions.push('1440px 普通成功行不高于76px；增强行保留服务层和推理强度；Token 缩写同一行、三种背景色、完整数值在 tooltip 保留')
    await expect(summary).toContainText('1 条未记录用量')
    const statisticsRequests = requests.stats.length
    await next.click()
    await expect(table).toContainText('第二页记录')
    await expect(summary.locator('strong').first()).toHaveText('1K')
    assert.equal(requests.stats.length, statisticsRequests)
    assert.equal(requests.list.at(-1).cursor, 'fixture-cursor-2')
    assert.equal(requests.stats.at(-1).as_of, requests.list.at(-1).as_of)
    assert.equal(new URLSearchParams(new URL(page.url()).hash.split('?')[1]).has('as_of'), false)
    assertions.push('服务端 1001 条全量统计不随 10 条当前页或翻页变化；真实零与未记录用量区分')

    await page.getByRole('button', { name: /高级筛选/ }).click()
    await page.getByRole('textbox', { name: 'KEY ID', exact: true }).fill('key-exact')
    await page.getByRole('textbox', { name: '上游模型', exact: true }).fill('gpt-5.6-sol-v2')
    await page.getByRole('textbox', { name: '服务层', exact: true }).fill('priority')
    await page.getByRole('textbox', { name: '推理强度', exact: true }).fill('xhigh')
    await page.getByRole('combobox', { name: '请求类型', exact: true }).press('Enter')
    await page.getByRole('option', { name: 'WebSocket', exact: true }).click()
    await page.getByRole('combobox', { name: '模型观测差异', exact: true }).press('Enter')
    await page.getByRole('option', { name: '观测模型不同', exact: true }).click()
    await page.getByRole('combobox', { name: '原生压缩', exact: true }).press('Enter')
    await page.getByRole('option', { name: '已观测为未触发', exact: true }).click()
    await page.getByRole('textbox', { name: '错误阶段', exact: true }).fill('upstream')
    await expect(next).toBeDisabled()
    await page.getByRole('button', { name: '刷新日志', exact: true }).click()
    await expect(summary).toContainText('23')
    assert.equal(requests.list.at(-1).key_id, 'key-exact')
    assert.equal('cursor' in requests.list.at(-1), false)
    assert.equal(requests.stats.at(-1).key_id, 'key-exact')
    for (const key of ['upstream_model', 'request_type', 'service_tier', 'reasoning_effort', 'model_mismatch', 'native_compaction', 'error_phase']) assert.equal(requests.stats.at(-1)[key], requests.list.at(-1)[key])
    assert.equal(requests.list.at(-1).native_compaction, 'false')
    assert.equal('cursor' in requests.stats.at(-1), false)
    await page.getByRole('combobox', { name: '日志排序字段' }).press('Enter')
    await page.getByRole('option', { name: '总 Token', exact: true }).click()
    await expect.poll(() => requests.list.at(-1).sort_by).toBe('tokens')
    assert.equal('cursor' in requests.list.at(-1), false)
    assertions.push('高级精确 ID、排序和 URL 使用共享筛选；修改 draft 后刷新从第一页开始')

    await page.getByRole('textbox', { name: '上游模型', exact: true }).fill('')
    await page.getByRole('textbox', { name: '上游模型', exact: true }).press('Enter')
    await expect.poll(() => requests.stats.at(-1).upstream_model).toBeUndefined()
    await page.getByRole('button', { name: /趋势与分布/ }).click()
    await expect(page.locator('.usage-trend svg')).toBeVisible()
    const upstreamDistribution = page.locator('.usage-distributions > section').filter({ has: page.getByRole('heading', { name: /^上游声明模型分布/ }) })
    await expect(page.getByRole('heading', { name: /^请求模型分布/ })).toBeVisible()
    await expect(upstreamDistribution).toContainText('缺失记录不以请求模型补齐')
    await expect(upstreamDistribution.locator('.distribution-row')).toHaveCount(1)
    await expect(upstreamDistribution.locator('.distribution-row strong')).toHaveText('7')
    const beforeUpstreamFilter = requests.stats.length
    await upstreamDistribution.getByRole('button', { name: /gpt-5.6-sol-v2 7/ }).click()
    await expect.poll(() => requests.stats.length).toBe(beforeUpstreamFilter + 1)
    assert.equal(requests.stats.at(-1).upstream_model, 'gpt-5.6-sol-v2')
    assert.equal(requests.list.at(-1).upstream_model, 'gpt-5.6-sol-v2')
    assert.equal(requests.stats.at(-1).as_of, requests.list.at(-1).as_of)
    assertions.push('请求模型与上游声明模型独立分布；仅 7 条明确观测值不补成 23 条，点选复用同快照上游筛选')
    if (screenshotDirectory) await page.locator('.usage-analytics').screenshot({ path: `${screenshotDirectory}/logs-analysis-1440.png` })
    await page.getByRole('button', { name: /responses 20/ }).click()
    await expect.poll(() => requests.stats.at(-1).endpoint).toBe('responses')
    await expect(page.locator('.query-notices')).toContainText('端点：responses')
    failStats = true
    await page.getByRole('button', { name: '刷新日志', exact: true }).click()
    await expect(page.locator('.usage-error')).toContainText('统计暂时不可用')
    await expect(page.locator('.usage-scope')).toContainText('上次统计结果')
    failStats = false
    await page.getByRole('button', { name: '重试统计' }).click()
    await expect(page.locator('.usage-error')).toHaveCount(0)
    assertions.push('趋势和可点选维度基于服务端全量统计；统计错误独立反馈并可重试')

    failDetail = true
    await table.getByRole('button', { name: /查看 .* 的日志详情/ }).first().click()
    const drawer = page.getByRole('dialog', { name: '日志详情', exact: true })
    await expect(drawer).toContainText('记录已清理或不存在')
    await expect(drawer).toContainText(rows[0].account)
    failDetail = false
    await drawer.getByRole('button', { name: '重试详情' }).click()
    await expect(drawer).toContainText('完整服务端详情')
    await expect(drawer).toContainText('服务端留存记录详情')
    await expect(drawer).toContainText('req_fixture_1')
    await expect(drawer).toContainText('resp_fixture_1')
    await expect(drawer).toContainText('Codex Synthetic Fixture')
    await expect(drawer).toContainText('350 ms')
    assert.equal(requests.detailQueries.at(-1).as_of, requests.list.at(-1).as_of)
    assert.equal(requests.detailQueries.at(-1).key_id, 'key-exact')
    await expect(drawer).toContainText('与列表共享固定记录快照')
    await drawer.getByRole('button', { name: '关闭详情' }).click()
    assertions.push('详情独立获取服务端记录；404 保留并标注列表快照，重试后更新')
    expiredDetail = true
    await table.getByRole('button', { name: /查看 .* 的日志详情/ }).first().click()
    await expect(drawer).toContainText('详情查询快照已过期')
    await expect(drawer).toContainText('以下保留列表快照')
    await expect(drawer.getByRole('button', { name: '重试详情', exact: true })).toHaveCount(0)
    const scopeBeforeDetailExpiry = requests.list.at(-1).as_of
    expiredDetail = false
    await drawer.getByRole('button', { name: '关闭并刷新日志', exact: true }).click()
    await expect.poll(() => requests.list.at(-1).as_of).not.toBe(scopeBeforeDetailExpiry)
    await expect(page.getByRole('button', { name: '导出全部筛选', exact: true })).toBeEnabled()
    assertions.push('详情沿用打开记录对应的完整filter和as_of；409保留原列表记录并显式刷新')

    exportMode = 'bad'
    const downloads = []
    const onDownload = download => downloads.push(download)
    page.on('download', onDownload)
    await page.getByRole('button', { name: '导出全部筛选', exact: true }).click()
    const dialog = page.getByRole('dialog', { name: '导出全部筛选结果' })
    await dialog.getByRole('button', { name: '导出全部记录', exact: true }).click()
    await expect(dialog).toContainText('服务器未返回有效导出文件')
    assert.equal(downloads.length, 0)
    exportMode = 'normal'
    const downloadPromise = page.waitForEvent('download')
    await dialog.getByRole('button', { name: '导出全部记录', exact: true }).click()
    const downloaded = await downloadPromise
    const content = await readFile(await downloaded.path(), 'utf8')
    assert.equal(content.trim().split('\n').length, 1002)
    assert.equal(requests.export.at(-1).key_id, 'key-exact')
    assert.equal(requests.export.at(-1).sort_by, 'tokens')
    assert.equal(requests.export.at(-1).redacted, 'true')
    assert.equal(requests.export.at(-1).as_of, requests.list.at(-1).as_of)
    assert.equal('cursor' in requests.export.at(-1), false)
    exportMode = 'blocked'
    await page.getByRole('button', { name: '导出全部筛选', exact: true }).click()
    await dialog.getByRole('button', { name: '导出全部记录', exact: true }).click()
    await expect(dialog).toContainText('正在服务端准备')
    await expect.poll(() => typeof releaseExport).toBe('function')
    await dialog.getByRole('button', { name: '取消导出', exact: true }).click()
    await expect(dialog).toContainText('导出已取消，未生成文件')
    releaseExport()
    await dialog.getByRole('button', { name: '取消', exact: true }).click()
    assert.equal(downloads.length, 1)
    exportMode = 'normal'
    await page.getByRole('button', { name: '导出全部筛选', exact: true }).click()
    await dialog.getByRole('checkbox', { name: /包含身份管理字段/ }).check()
    const fullDownload = page.waitForEvent('download')
    await dialog.getByRole('button', { name: '导出全部记录', exact: true }).click()
    await fullDownload
    assert.equal(requests.export.at(-1).redacted, 'false')
    page.off('download', onDownload)
    assertions.push('全量导出获取 1001 条而非当前页；复用筛选排序，错误 JSON 和取消均不保存文件')

    await account.fill('slow')
    await account.press('Enter')
    await expect.poll(() => requests.list.at(-1).account).toBe('slow')
    await account.fill('fast')
    await account.press('Enter')
    await expect(table).toContainText('fast')
    await expect.poll(() => requests.stats.at(-1).account).toBe('fast')
    await page.waitForTimeout(300)
    await expect(table).not.toContainText('slow')
    assertions.push('列表和统计慢请求不能覆盖更新查询；加载期间 Enter 仍可提交')

    await page.getByRole('button', { name: '重置', exact: true }).click()
    await expect(table.locator('.el-table__row')).toHaveCount(10)
    await page.getByRole('button', { name: /高级筛选/ }).click()
    if (await page.getByRole('button', { name: /收起分析/ }).count()) await page.getByRole('button', { name: /收起分析/ }).click()
    await page.evaluate(() => document.fonts.ready)
    await expect(page.locator('.el-message:visible')).toHaveCount(0, { timeout: 8000 })
    if (screenshotDirectory) await page.screenshot({ path: `${screenshotDirectory}/logs-management-1440.png`, fullPage: true })
    await captureLogThemes(page, null, 'logs-shell', screenshotDirectory)
    for (const width of [390, 320]) {
      await page.setViewportSize({ width, height: 844 })
      await expect(page.locator('.mobile-log-list')).toBeVisible()
      await expect(page.locator('.log-table')).toBeHidden()
      assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true)
      if (screenshotDirectory) await page.screenshot({ path: `${screenshotDirectory}/logs-management-${width}.png`, fullPage: true })
      if (screenshotDirectory) await page.locator('.mobile-log-card').first().screenshot({ path: `${screenshotDirectory}/logs-card-${width}.png` })
    }
    assertions.push('1440 桌面与 390/320 手机完整页面均无横向溢出，手机使用卡片')
    await page.setViewportSize({ width: 1440, height: 1100 })
    await page.getByRole('button', { name: '查找账号', exact: true }).click()
    const lookup = page.getByRole('dialog', { name: '查找账号', exact: true })
    await expect(lookup).toContainText('已删除 · 历史记录')
    assert.equal(requests.options.at(-1).as_of, requests.list.at(-1).as_of)
    await expect(lookup).toContainText('前 50 项')
    await lookup.getByRole('textbox', { name: '搜索账号候选' }).fill('empty')
    await expect(lookup).toContainText('没有匹配候选')
    await lookup.getByRole('textbox', { name: '搜索账号候选' }).fill('同名')
    await expect(lookup.locator('.lookup-option')).toHaveCount(2)
    await lookup.getByRole('button', { name: /same-name-one/ }).click()
    await expect.poll(() => requests.list.at(-1).account_id).toBe('same-name-one')
    assert.equal(requests.list.at(-1).account, undefined)
    await page.getByRole('button', { name: /高级筛选/ }).click()
    await page.getByRole('textbox', { name: '统计时区', exact: true }).fill('America/New_York')
    await page.getByRole('combobox', { name: '统计粒度', exact: true }).press('Enter')
    await page.getByRole('option', { name: '按日历月', exact: true }).click()
    await page.getByRole('button', { name: '刷新日志', exact: true }).click()
    await expect.poll(() => requests.stats.at(-1).timezone).toBe('America/New_York')
    assert.equal(requests.stats.at(-1).step, '2592000')
    assert.equal(requests.list.at(-1).timezone, 'America/New_York')
    await page.getByRole('button', { name: /趋势与分布/ }).click()
    await expect(page.locator('.usage-distributions')).toContainText('分组之和不等于去重总数')
    await page.getByRole('combobox', { name: '排行维度', exact: true }).selectOption('groups')
    await page.getByRole('combobox', { name: '排行排序', exact: true }).selectOption('success_rate')
    await expect(page.locator('.usage-ranking')).toContainText('成功率')
    await page.locator('.usage-ranking').getByRole('button', { name: '历史分组', exact: true }).click()
    await expect.poll(() => requests.list.at(-1).group_id).toBe('group-stable')
    assertions.push('历史全量候选支持同名稳定 ID、删除标记与空态；时区/月粒度和分组重叠口径共享快照')
    await page.getByRole('button', { name: '重置', exact: true }).click()
    await page.getByRole('button', { name: /收起分析/ }).click()
    const coverageResponse = page.waitForResponse(response => new URL(response.url()).pathname === '/api/logs/coverage' && response.status() === 200)
    await page.getByText('数据范围与留存说明', { exact: true }).click()
    assert.equal((await (await coverageResponse).json()).detail_count, 1100)
    await expect(page.locator('.log-coverage')).toContainText('1.1K 条')
    await expect(page.locator('.log-coverage')).toContainText('没有保证的留存天数')
    await expect(page.locator('.log-coverage')).toContainText('codex-usage-v2')
    await page.getByText('数据范围与留存说明', { exact: true }).click()
    const previousScope = requests.list.at(-1).as_of
    expiredPage = true
    await next.click()
    await expect(page.locator('.log-load-error')).toContainText('查询快照已过期')
    await expect(page.getByRole('button', { name: '导出全部筛选', exact: true })).toBeDisabled()
    expiredPage = false
    await page.getByRole('button', { name: '刷新日志', exact: true }).click()
    await expect(page.locator('.log-load-error')).toHaveCount(0)
    assert.notEqual(requests.list.at(-1).as_of, previousScope)
    assert.equal(requests.list.at(-1).cursor, undefined)
    assert.equal(requests.list.at(-1).as_of, requests.stats.at(-1).as_of)
    failSnapshot = true
    const beforeFailure = requests.list.length
    await page.getByRole('button', { name: '刷新日志', exact: true }).click()
    await expect(page.locator('.log-load-error')).toContainText('快照准备暂时失败')
    assert.equal(requests.list.length, beforeFailure)
    failSnapshot = false
    await page.getByRole('button', { name: '重新加载', exact: true }).click()
    await expect(page.locator('.log-load-error')).toHaveCount(0)
    assertions.push('固定快照贯穿列表/统计/候选/导出且不写URL；过期409从首页刷新新快照，创建失败不混旧结果')
    return { assertions, requests }
  } finally { releaseExport?.(); await page.unroute('**/api/logs**', handler) }
}
