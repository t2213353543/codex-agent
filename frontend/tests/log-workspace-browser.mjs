import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import { expect } from 'playwright/test'

// Run only against a locally served, isolated UI. The caller owns mock login and
// the static server; all log requests are intercepted and restored on return.
export async function testLogsWorkspace(page) {
  assert.ok(['localhost', '127.0.0.1', '[::1]'].includes(new URL(page.url()).hostname))
  const assertions = []
  const queries = []
  const rawError = '<img src=x onerror="window.__logInjected=true"> unsupported parameter: temperature; Bearer MOCK_SECRET must remain literal'
  const firstRows = [
    { time: 1789648512, status: 'error', http_code: 400, account: 'production-account-long-name@example.test', account_id: 'private-id', key_name: 'private-key-name', model: 'gpt-5.6-luna-with-long-configuration-name', endpoint: 'responses', stream: true, input_tokens: 0, output_tokens: 0, cached_tokens: 0, latency_ms: 345, error: rawError, proxy_name: 'private-proxy', ip: '192.0.2.1' },
    { time: 1789648400, status: 'error', http_code: 429, account: 'rate-limited@example.test', key_name: 'second-key', model: 'gpt-5.5', endpoint: 'responses', stream: true, input_tokens: 0, output_tokens: 0, cached_tokens: 0, latency_ms: 1100, error: 'Rate limit reached. Please try again later.' },
    { time: 1789648300, status: 'ok', http_code: 200, account: 'ok@example.test', key_name: 'successful-key', model: 'gpt-5.6-luna', endpoint: 'responses', stream: true, input_tokens: 12000, output_tokens: 321, cached_tokens: 9000, latency_ms: 2145 },
    { time: 1789648200, status: 'error', http_code: 200, account: 'policy@example.test', key_name: 'policy-key', model: 'gpt-5.5', endpoint: 'responses', stream: true, input_tokens: 0, output_tokens: 0, cached_tokens: 0, latency_ms: 612, error: 'This request was flagged for possible policy violation.' },
    { time: 1789648100, status: 'aborted', http_code: 499, account: 'cancelled@example.test', key_name: 'cancel-key', model: 'gpt-5.6-luna', endpoint: 'compact', stream: false, latency_ms: 0, error: 'context canceled' },
  ]
  let fail = false
  let invalid = false
  let unblockSlow
  let delayedResolved
  const slowResolved = new Promise((resolve) => { delayedResolved = resolve })
  const handler = async (route) => {
    const query = Object.fromEntries(new URL(route.request().url()).searchParams)
    queries.push(query)
    if (query.account === 'slow') {
      await new Promise((resolve) => { unblockSlow = resolve })
      try { await route.fulfill({ json: { items: [{ ...firstRows[2], account: 'slow' }], has_more: false, next_cursor: '', total_kind: 'not_computed' } }) } catch { /* Abort is expected. */ }
      delayedResolved()
      return
    }
    if (fail) return route.fulfill({ status: 503, json: { error: { message: '模拟日志查询失败' } } })
    if (invalid) return route.fulfill({ json: { items: null } })
    let items
    if (query.account === 'empty') items = []
    else if (query.account) items = [{ ...firstRows[2], account: query.account }]
    else if (query.cursor) items = [{ ...firstRows[2], account: 'second-page-account', error: undefined }]
    else items = firstRows
    if (query.status) items = items.filter((row) => row.status === query.status)
    return route.fulfill({ json: { items, has_more: !query.cursor && !query.account && !query.status, next_cursor: !query.cursor && !query.account && !query.status ? 'cursor-page-2' : '', total_kind: 'not_computed', total: -1 } })
  }
  await page.route('**/api/logs?*', handler)
  const table = page.locator('.log-table')
  const account = page.getByRole('textbox', { name: '账号名称', exact: true })
  const exportButton = page.getByRole('button', { name: '导出当前页（脱敏）', exact: true })
  const next = page.getByRole('button', { name: '下一页', exact: true })
  const refresh = page.getByRole('button', { name: '刷新日志', exact: true })
  try {
    await page.setViewportSize({ width: 1440, height: 1000 })
    await page.evaluate(() => { location.hash = '/logs' })
    await expect(table).toContainText(firstRows[0].account)
    await expect(page.locator('.query-scope')).toHaveText('当前页 5 条；未计算全量总数')
    await expect(table.locator('thead th')).toHaveCount(7)
    assert.equal(await page.locator('.log-overview').count(), 0)
    await expect(table.locator('.el-table__row').nth(3)).toContainText('失败')
    await expect(table.locator('.el-table__row').nth(3)).toContainText('HTTP 200')
    await expect(table.locator('.el-table__row').nth(3)).toContainText('上游拒绝')
    await expect(table.locator('.el-table__row').first()).toContainText('参数不兼容')
    assert.equal(await table.locator('.el-table__row').first().locator('.token-line strong').allTextContents().then((values) => values.join('/')), '0/0/0')
    assertions.push('列表聚合为 7 列；范围明确标注当前页；零用量保留 0；HTTP 200 的失败请求仍显示失败及分类')

    const trigger = table.getByRole('button', { name: /查看 .* 的日志详情/ }).first()
    await trigger.click()
    const drawer = page.getByRole('dialog', { name: '日志详情', exact: true })
    await expect(drawer.locator('.detail-error')).toHaveText(rawError)
    await expect(drawer.locator('.detail-failure')).toContainText('客户端发送了上游不接受的参数')
    assert.equal(await drawer.locator('img, script').count(), 0)
    assert.equal(await page.evaluate(() => window.__logInjected), undefined)
    await drawer.getByRole('button', { name: '关闭详情', exact: true }).click()
    await expect(drawer).toBeHidden()
    await expect(trigger).toBeFocused()
    assertions.push('详情保留完整原始错误、分类解释及请求信息；HTML 以文本显示；关闭恢复焦点')

    await next.click()
    await expect(table).toContainText('second-page-account')
    assert.equal(queries.at(-1).cursor, 'cursor-page-2')
    assert.equal('page' in queries.at(-1), false)
    await account.fill('new-filter')
    await expect(next).toBeDisabled()
    await expect(page.getByRole('button', { name: '上一页', exact: true })).toBeDisabled()
    await expect(exportButton).toBeDisabled()
    await expect(page.locator('.draft-notice')).toContainText('筛选已修改')
    const queryCount = queries.length
    await page.getByRole('button', { name: '上一页', exact: true }).evaluate((button) => button.click())
    assert.equal(queries.length, queryCount)
    await refresh.click()
    await expect(table).toContainText('new-filter')
    assert.equal(queries.at(-1).account, 'new-filter')
    assert.equal('cursor' in queries.at(-1), false)
    assert.equal('page' in queries.at(-1), false)
    await expect(page).not.toHaveURL(/cursor=|page=2/)
    await expect(page.locator('.log-page-controls')).toContainText('第 1 页')
    assertions.push('翻页携带正确游标；未查询的筛选禁用分页与导出；刷新将新条件应用到首页并移除旧游标')

    fail = true
    await account.fill('failed-query')
    await page.getByRole('button', { name: '查询', exact: true }).click()
    await expect(page.locator('.log-load-error')).toContainText('模拟日志查询失败')
    await expect(table).toContainText('new-filter')
    await expect(page.locator('.query-scope')).toContainText('上次成功加载的第 1 页')
    await expect(exportButton).toBeDisabled()
    await expect(next).toBeDisabled()
    fail = false
    invalid = true
    await refresh.click()
    await expect(page.locator('.log-load-error')).toContainText('日志响应格式无效')
    await expect(exportButton).toBeDisabled()
    invalid = false
    await page.getByRole('button', { name: '重新加载', exact: true }).click()
    await expect(table).toContainText('failed-query')
    await expect(exportButton).toBeEnabled()
    assertions.push('网络失败与无效成功响应保留旧记录并明确标识，禁止假作新结果导出；重试成功恢复导出')

    await account.fill('slow')
    await account.press('Enter')
    await expect.poll(() => queries.some((query) => query.account === 'slow')).toBe(true)
    await account.fill('fast')
    await account.press('Enter')
    await expect(table).toContainText('fast')
    unblockSlow()
    await slowResolved
    await expect(table).not.toContainText('slow')
    await expect(page).toHaveURL(/account=fast/)
    assertions.push('慢查询后提交快查询，旧响应不会覆盖新结果；筛选同步 URL')

    const downloadEvent = page.waitForEvent('download')
    await exportButton.click()
    const download = await downloadEvent
    const exported = await readFile(await download.path(), 'utf8')
    for (const secret of ['private-key-name', 'private-proxy', '192.0.2.1', rawError, 'gpt-5.6', 'private-id', 'fast']) assert.equal(exported.includes(secret), false)
    assert.equal(JSON.parse(exported.trim()).input_tokens, 12000)
    assert.match(download.suggestedFilename(), /page-1/)
    assertions.push('脱敏导出仅含已成功加载的当前页指标，排除身份、模型、代理及原始错误')

    await page.getByRole('button', { name: '高级筛选', exact: false }).click()
    // Native datetime-local has no implicit textbox role in Chromium.
    const fromInput = page.locator('input[aria-label="开始时间"]')
    const toInput = page.locator('input[aria-label="结束时间"]')
    await fromInput.fill('2026-09-17T20:30')
    await toInput.fill('2026-09-17T19:30')
    const beforeInvalid = queries.length
    await page.getByRole('button', { name: '查询', exact: true }).click()
    await expect(page.locator('.log-load-error')).toContainText('开始时间必须早于结束时间')
    assert.equal(queries.length, beforeInvalid)
    await toInput.fill('2026-09-17T21:30')
    await page.getByRole('button', { name: '查询', exact: true }).click()
    await expect.poll(() => queries.at(-1).from).toBe(String(new Date(2026, 8, 17, 20, 30).getTime() / 1000))
    assert.equal(Number(queries.at(-1).to) - Number(queries.at(-1).from), 3600)
    await page.getByRole('button', { name: '保存筛选', exact: true }).click()
    const savedDialog = page.getByRole('dialog', { name: '保存常用筛选', exact: true })
    await savedDialog.getByRole('textbox', { name: '筛选名称', exact: true }).fill('工作时间请求')
    await savedDialog.getByRole('button', { name: '保存', exact: true }).click()
    await expect(savedDialog).toBeHidden()
    await expect(page.getByRole('combobox', { name: '使用常用筛选', exact: true })).toBeVisible()
    assertions.push('高级日期按本地时区转 Unix 秒；无效范围不请求；常用筛选可保存')

    await page.getByRole('button', { name: '重置', exact: true }).click()
    await expect(table).toContainText(firstRows[0].account)
    await page.getByRole('button', { name: '失败', exact: true }).click()
    await expect(page.locator('.query-scope')).toHaveText('当前页 3 条；未计算全量总数')
    assert.equal(queries.at(-1).status, 'error')
    await account.fill('empty')
    await account.press('Enter')
    await expect(table).toContainText('没有找到匹配的请求')
    await expect(exportButton).toBeDisabled()
    await page.getByRole('button', { name: '重置', exact: true }).click()
    await expect(table).toContainText(firstRows[0].account)
    assertions.push('状态页签真实查询，空结果有清楚恢复入口且不能导出')

    for (const width of [390, 320]) {
      await page.setViewportSize({ width, height: 1000 })
      await expect(table).toBeHidden()
      await expect(page.locator('.mobile-log-list')).toBeVisible()
      await expect(page.locator('.mobile-log-card')).toHaveCount(5)
      assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1), `Logs overflow at ${width}`)
      await page.locator('.mobile-log-card').first().getByRole('button', { name: /日志详情/ }).click()
      await expect(drawer.locator('.detail-error')).toHaveText(rawError)
      await drawer.getByRole('button', { name: '关闭详情', exact: true }).click()
      await expect(drawer).toBeHidden()
    }
    assertions.push('390/320 移动端使用请求卡片，状态与错误可读、详情可达，无整页横向溢出')
    await page.setViewportSize({ width: 1440, height: 1000 })
    return assertions
  } finally {
    unblockSlow?.()
    await page.unroute('**/api/logs?*', handler)
  }
}
