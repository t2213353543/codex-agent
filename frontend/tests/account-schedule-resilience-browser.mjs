import assert from 'node:assert/strict'
import { expect } from 'playwright/test'
import { account } from './mock-api.mjs'

export async function testAccountScheduleResilience(page, { capture = null } = {}) {
  const origin = new URL(page.url()).origin
  assert.ok(['localhost', '127.0.0.1'].includes(new URL(origin).hostname))
  const row = account('schedule-resilience', '计划异常路径合成账号', { version: 11 })
  const statuses = ['disabled', 'error', 'expired', 'ok', 'unverified']
  const rows = [row, ...Array.from({ length: 22 }, (_, i) => account(`schedule-extra-${i}`, `合成账号 ${i}`, { status: i === 1 ? 'future_state' : statuses[i % statuses.length] }))]
  const sortedRows = query => ['id', 'status'].includes(query.sort_by) ? [...rows].sort((a, b) => {
    const compare = (left, right) => left < right ? -1 : left > right ? 1 : 0
    return (compare(a[query.sort_by], b[query.sort_by]) || compare(a.id, b.id)) * (query.sort_order === 'asc' ? 1 : -1)
  }) : rows
  const plans = new Map(), creates = [], calls = [], histories = [], assertions = []
  let loseResponse = true, createConflict = false
  const handler = async route => {
    const request = route.request(), url = new URL(request.url()), method = request.method(), body = request.postDataJSON()
    calls.push({ path: url.pathname, method, query: Object.fromEntries(url.searchParams), body })
    const reply = (json, status = 200) => route.fulfill({ status, json }).catch(() => {})
    if (url.pathname === '/api/accounts/manage') {
      const current = Number(url.searchParams.get('page') || 1), size = Number(url.searchParams.get('page_size') || 20)
      return reply({ accounts: sortedRows(Object.fromEntries(url.searchParams)).slice((current - 1) * size, current * size), total: rows.length, total_kind: 'exact', has_more: current * size < rows.length })
    }
    if (url.pathname === '/api/accounts/last-used') return reply({ items: body.ids.map(id => ({ id, at: id === row.id ? 1800000000 : null })), scope: 'durable_usage_events' })
    if (url.pathname === '/api/accounts/selection') return reply({ items: sortedRows(body.filters).map(({ id, version }) => ({ id, version })), total: rows.length })
    if (url.pathname === '/api/accounts/export') return reply({ type: 'codex-agent-account-metadata', version: 1, accounts: [] })
    if (url.pathname === `/api/accounts/${row.id}`) return reply({ account: row })
    if (url.pathname.endsWith('/metadata')) return reply({ id: row.id, version: 11, identity: {}, privacy: { status: 'unknown' } })
    if (url.pathname.endsWith('/requests')) return reply({ items: [] })
    if (url.pathname.endsWith('/schedules')) {
      if (method !== 'POST') return reply({ plans: [...plans.values()] })
      creates.push(body)
      assert.match(body.request_id, /^[A-Za-z0-9_-]{10,100}$/)
      if (createConflict) return reply({ error: { message: 'original schedule deleted' } }, 409)
      if (!plans.has(body.request_id)) plans.set(body.request_id, { id: `schedule-${plans.size + 1}`, account_id: row.id, version: 1, ...body, next_run_at: 1900000000 })
      if (loseResponse) { loseResponse = false; return route.abort('connectionreset') }
      return reply(plans.get(body.request_id))
    }
    if (url.pathname.endsWith('/results')) {
      await new Promise(resolve => histories.push({ finish: async status => { await reply({ results: [{ id: `history-${histories.length}`, status, scheduled_at: 1800000000 }] }); resolve() } }))
      return
    }
    return route.fallback()
  }
  await page.route('**/api/**', handler)
  const openPlans = async () => {
    await page.getByRole('button', { name: `查看账号 ${row.name} 详情`, exact: true }).click()
    await page.getByRole('dialog', { name: '账号详情', exact: true }).getByRole('button', { name: '定时检查', exact: true }).click()
    return page.getByRole('dialog', { name: '定时检查与测试', exact: true })
  }
  try {
    await page.goto(origin + '/#/accounts')
    let dialog = await openPlans()
    await dialog.getByRole('button', { name: '创建计划', exact: true }).click()
    await dialog.getByRole('combobox', { name: '计划检查类型', exact: true }).press('Enter')
    await page.getByRole('option', { name: '普通流式推理', exact: true }).click()
    await dialog.getByRole('textbox', { name: '计划模型 ID', exact: true }).fill('synthetic-model')
    await dialog.getByRole('checkbox', { name: '我确认此计划会周期性发送推理请求，可能消耗上游额度', exact: true }).locator('xpath=ancestor::label').click()
    await dialog.getByRole('button', { name: '保存计划', exact: true }).click()
    await expect(dialog).toContainText('创建结果未确认')
    await expect(dialog.getByRole('textbox', { name: '计划 Cron', exact: true })).toBeDisabled()
    assert.equal(creates.length, 1); assert.equal(plans.size, 1)
    if (capture) {
      for (const theme of ['light', 'dark']) for (const width of [1440, 390]) {
        await page.setViewportSize({ width, height: 1000 })
        await page.evaluate(value => { document.documentElement.dataset.theme = value; document.documentElement.classList.toggle('dark', value === 'dark'); window.dispatchEvent(new CustomEvent('theme-change', { detail: value })) }, theme)
        assert.ok(await dialog.evaluate(element => element.scrollWidth <= element.clientWidth + 1))
        await capture(`account-schedule-unknown-${theme}-${width}`, dialog)
      }
      await page.setViewportSize({ width: 1440, height: 1000 })
      await page.evaluate(() => { document.documentElement.dataset.theme = 'light'; document.documentElement.classList.remove('dark'); window.dispatchEvent(new CustomEvent('theme-change', { detail: 'light' })) })
    }
    await page.reload()
    dialog = await openPlans()
    await expect(dialog.getByRole('textbox', { name: '计划模型 ID', exact: true })).toHaveValue('synthetic-model')
    await expect(dialog.getByRole('textbox', { name: '计划模型 ID', exact: true })).toBeDisabled()
    await dialog.getByRole('button', { name: '重读原创建结果', exact: true }).click()
    await expect(dialog.locator('.schedule-plan')).toHaveCount(1)
    await expect(dialog.locator('.schedule-editor')).toHaveCount(0)
    assert.equal(creates.length, 2); assert.deepEqual(creates[1], creates[0]); assert.equal(plans.size, 1)
    assertions.push('创建响应丢失后冻结参数，刷新页面恢复同一操作ID，重读只产生一个启用计划')

    await dialog.getByRole('button', { name: '运行历史', exact: true }).click()
    await expect.poll(() => histories.length).toBe(1)
    await dialog.getByRole('button', { name: '刷新计划', exact: true }).click()
    await expect(dialog.getByRole('button', { name: '刷新计划', exact: true })).not.toHaveClass(/is-loading/)
    await histories[0].finish('ok')
    await expect(dialog.locator('.schedule-history details')).toHaveCount(1)
    await expect(dialog.locator('.schedule-history')).not.toContainText('正在读取')
    assertions.push('历史读取期间刷新计划列表，历史响应仍正常结束加载并展示结果')

    await dialog.getByRole('button', { name: '运行历史', exact: true }).click()
    await expect.poll(() => histories.length).toBe(2)
    await dialog.getByRole('button', { name: '运行历史', exact: true }).click()
    await expect.poll(() => histories.length).toBe(3)
    await histories[2].finish('failed')
    await expect(dialog.locator('.schedule-history details')).toContainText('失败')
    await histories[1].finish('ok')
    await expect(dialog.locator('.schedule-history details')).toContainText('失败')
    assertions.push('同计划重复读取历史取消旧请求，旧成功响应不能覆盖最新失败历史')

    await dialog.getByRole('button', { name: '创建计划', exact: true }).click()
    createConflict = true
    await dialog.getByRole('button', { name: '保存计划', exact: true }).click()
    await expect(dialog).toContainText('原创建请求发生冲突')
    const conflictRequest = creates.at(-1)
    await expect(dialog.getByRole('textbox', { name: '计划 Cron', exact: true })).toBeDisabled()
    await dialog.getByRole('button', { name: '重读原创建结果', exact: true }).click()
    await expect.poll(() => creates.length).toBe(4)
    assert.deepEqual(creates.at(-1), conflictRequest)
    await dialog.getByRole('button', { name: '清除未确认草稿', exact: true }).click()
    await page.getByRole('button', { name: '继续保留', exact: true }).click()
    await expect(dialog.getByRole('button', { name: '重读原创建结果', exact: true })).toBeVisible()
    await dialog.getByRole('button', { name: '清除未确认草稿', exact: true }).click()
    await page.getByRole('button', { name: '已核对，清除草稿', exact: true }).click()
    await expect(dialog.locator('.schedule-editor')).toHaveCount(0)
    assertions.push('创建409保留原请求，不暗换ID；清除未确认草稿需要单独确认')
    await dialog.getByRole('button', { name: '关闭', exact: true }).click()
    await expect(dialog).toBeHidden()
    await page.getByRole('dialog', { name: '账号详情', exact: true }).locator('.el-dialog__headerbtn').click()
    await expect(page.getByRole('dialog', { name: '账号详情', exact: true })).toBeHidden()

    const beforeInvalid = calls.filter(call => call.path === '/api/accounts/manage').length
    await page.goto(origin + '/#/accounts?search=kept&enabled=false&expires_before=99999999999999&expires_after=1700000000')
    await expect(page.locator('.accounts-page')).toContainText('超出范围的日期')
    assert.equal(calls.filter(call => call.path === '/api/accounts/manage').length, beforeInvalid)
    await page.getByRole('button', { name: '清除无效日期并查询', exact: true }).click()
    await expect(page.locator('.account-directory-heading')).toContainText('共 23 个')
    const validQuery = calls.filter(call => call.path === '/api/accounts/manage').at(-1).query
    assert.equal(validQuery.search, 'kept'); assert.equal(validQuery.enabled, 'false'); assert.equal(validQuery.expires_after, '1700000000'); assert.equal(validQuery.expires_before, undefined)
    assertions.push('超界日期深链不崩溃且阻止查询，明确清除后保留其余有效筛选')

    await page.goto(origin + '/#/accounts?sort_by=name&sort_order=desc&page=2&page_size=10')
    await expect(page.locator('.account-pagination')).toContainText('第 2 / 3 页')
    const originalColumns = await page.evaluate(() => localStorage.getItem('accountColumns'))
    await page.getByRole('button', { name: '筛选与排序', exact: true }).click()
    await page.getByRole('combobox', { name: '账号排序字段', exact: true }).press('Enter')
    await page.getByRole('option', { name: '最近业务请求', exact: true }).click()
    await expect(page.locator('.account-pagination')).toContainText('第 1 / 3 页')
    await expect(page.locator('.accounts-panel')).toContainText('全部已持久化请求事件')
    assert.equal(calls.filter(call => call.path === '/api/accounts/manage').at(-1).query.sort_by, 'last_used_at')
    await page.getByRole('button', { name: '下一页', exact: true }).click()
    await expect(page.locator('.account-pagination')).toContainText('第 2 / 3 页')
    await page.getByRole('combobox', { name: '账号排序方向', exact: true }).press('Enter')
    await page.getByRole('option', { name: '升序', exact: true }).click()
    await expect(page.locator('.account-pagination')).toContainText('第 1 / 3 页')
    await page.getByRole('button', { name: '选择全部 23 个', exact: true }).click()
    await expect(page.locator('.account-summary')).toContainText('已选择 23 个')
    const selectedFilters = calls.filter(call => call.path === '/api/accounts/selection').at(-1).body.filters
    assert.equal(selectedFilters.sort_by, 'last_used_at'); assert.equal(selectedFilters.sort_order, 'asc')
    await page.getByRole('button', { name: '取消选择', exact: true }).click()
    await page.getByRole('button', { name: '迁移备份', exact: true }).click()
    await page.getByRole('menuitem', { name: '导出账号', exact: true }).click()
    const exportDialog = page.getByRole('dialog', { name: '导出账号', exact: true })
    const download = page.waitForEvent('download')
    await exportDialog.getByRole('button', { name: '生成并下载', exact: true }).click()
    await download
    const exportedFilters = calls.filter(call => call.path === '/api/accounts/export').at(-1).body.filters
    assert.equal(exportedFilters.sort_by, 'last_used_at'); assert.equal(exportedFilters.sort_order, 'asc')
    await exportDialog.getByRole('button', { name: '关闭', exact: true }).click()
    assert.equal(await page.evaluate(() => localStorage.getItem('accountColumns')), originalColumns)
    assertions.push('最近请求全结果排序和切序回首页，自动显示来源列但不改列偏好，选择与导出沿用排序')

    for (const [key, label, heading] of [['id', '账号 ID', '账号 ID / 版本'], ['status', '账号状态', '状态']]) {
      await page.getByRole('button', { name: '下一页', exact: true }).click()
      await expect(page.locator('.account-pagination')).toContainText('第 2 / 3 页')
      await page.getByRole('combobox', { name: '账号排序字段', exact: true }).press('Enter')
      await page.getByRole('option', { name: label, exact: true }).click()
      await expect(page.locator('.account-pagination')).toContainText('第 1 / 3 页')
      await expect(page.getByRole('columnheader', { name: heading, exact: true })).toBeVisible()
      for (const direction of ['asc', 'desc']) {
        if (direction === 'desc') {
          await page.getByRole('button', { name: '下一页', exact: true }).click()
          await expect(page.locator('.account-pagination')).toContainText('第 2 / 3 页')
          await page.getByRole('combobox', { name: '账号排序方向', exact: true }).press('Enter')
          await page.getByRole('option', { name: '降序', exact: true }).click()
          await expect(page.locator('.account-pagination')).toContainText('第 1 / 3 页')
        } else if (key === 'status') {
          await page.getByRole('combobox', { name: '账号排序方向', exact: true }).press('Enter')
          await page.getByRole('option', { name: '升序', exact: true }).click()
        }
        await expect.poll(() => calls.filter(call => call.path === '/api/accounts/manage').at(-1).query.sort_order).toBe(direction)
        const requested = calls.filter(call => call.path === '/api/accounts/manage').at(-1).query
        assert.equal(requested.sort_by, key)
        await page.getByRole('button', { name: '选择全部 23 个', exact: true }).click()
        await expect(page.locator('.account-summary')).toContainText('已选择 23 个')
        const selected = calls.filter(call => call.path === '/api/accounts/selection').at(-1).body.filters
        assert.equal(selected.sort_by, key); assert.equal(selected.sort_order, direction)
        await page.getByRole('button', { name: '取消选择', exact: true }).click()
        await page.getByRole('button', { name: '迁移备份', exact: true }).click()
        await page.getByRole('menuitem', { name: '导出账号', exact: true }).click()
        const dialog = page.getByRole('dialog', { name: '导出账号', exact: true }), download = page.waitForEvent('download')
        await dialog.getByRole('button', { name: '生成并下载', exact: true }).click(); await download
        const exported = calls.filter(call => call.path === '/api/accounts/export').at(-1).body.filters
        assert.equal(exported.sort_by, key); assert.equal(exported.sort_order, direction)
        await dialog.getByRole('button', { name: '关闭', exact: true }).click()
      }
      assertions.push(`${label}服务端升降序、切序回首页、对应列可见、全结果选择与导出同条件`)
    }
    await page.goto(origin + '/#/accounts?sort_by=status&sort_order=desc&page_size=50')
    await expect(page.locator('.account-directory-heading')).toContainText('共 23 个')
    const unknownRow = page.locator('.el-table__body tr').filter({ has: page.getByRole('button', { name: '查看账号 合成账号 1 详情', exact: true }) })
    await expect(unknownRow.locator('.el-tag').filter({ hasText: '状态未知' })).toHaveClass(/el-tag--info/)
    await expect(unknownRow).not.toContainText('最近状态正常')
    await page.setViewportSize({ width: 390, height: 1000 })
    const unknownMobile = page.locator('.mobile-account').filter({ has: page.getByRole('button', { name: '查看账号 合成账号 1 详情', exact: true }) })
    await expect(unknownMobile.locator('.mobile-account-status')).toContainText('状态未知')
    await page.goto(origin + '/#/accounts?sort_by=id&sort_order=asc&page_size=50')
    await expect(page.locator('.mobile-account').first()).toContainText('账号 ID：schedule-extra-0')
    assert.equal(await page.evaluate(() => localStorage.getItem('accountColumns')), originalColumns)
    await page.setViewportSize({ width: 1440, height: 1000 })
  } finally {
    for (const history of histories) await history.finish('ok')
    await page.unroute('**/api/**', handler)
  }
  return assertions
}
