import assert from 'node:assert/strict'
import { expect } from 'playwright/test'
import { captureLogThemes } from './log-visual-fixtures.mjs'

export async function testLogErrorReview(page, { screenshotDirectory } = {}) {
  assert.ok(['localhost', '127.0.0.1'].includes(new URL(page.url()).hostname))
  const rows = Array.from({ length: 23 }, (_, index) => ({ id: index + 1, event_id: `review-${index + 1}`, time: 1789650000 - index, account: '合成错误处理账号-long@example.test', account_id: 'review-account', model: 'gpt-5.6-sol', status: index === 1 ? 'aborted' : 'error', http_code: index === 0 ? 200 : index === 1 ? 499 : 429, endpoint: 'responses', error: 'Synthetic error <img src=x onerror="window.__reviewXSS=true">', input_tokens: 0, output_tokens: 0, cached_tokens: 0, usage_known: false }))
  const state = new Map(rows.map(row => [row.event_id, { record_ref: row.event_id, version: 0, resolved: false, note: '', history: [], history_total: 0, history_truncated: false }]))
  const calls = { errors: [], get: [], put: [], details: [] }, assertions = []
  let conflict = false, missing = false, failSave = false, changedCursor = false, blocked = '', releaseBlocked
  const handler = async route => {
    const url = new URL(route.request().url()), query = Object.fromEntries(url.searchParams)
    const reply = async result => { try { await route.fulfill(result) } catch { /* Deliberately canceled detail read. */ } }
    if (url.pathname === '/api/logs/snapshot') return reply({ json: { as_of: 'usage_review_main', created_at: 1789650000, expires_at: Math.floor(Date.now() / 1000) + 300, scope: 'retained_request_details', total: 23 } })
    if (url.pathname === '/api/logs/stats') return reply({ json: { total_requests: 23, success: 0, errors: 23, total_input_tokens: 0, total_output_tokens: 0, total_cached_tokens: 0, total_tokens: 0, usage_known_requests: 0, usage_unknown_requests: 23, models: [], endpoints: [], error_categories: [], series: [] } })
    if (url.pathname === '/api/logs/errors') {
      calls.errors.push(query)
      if (changedCursor && query.cursor) return reply({ status: 409, json: { error: { code: 'review_query_changed', message: 'set changed' } } })
      const filtered = rows.filter(row => !query.resolved || state.get(row.event_id).resolved === (query.resolved === 'true'))
      const offset = query.cursor ? 20 : 0, items = filtered.slice(offset, offset + 20).map(record => ({ record, review: state.get(record.event_id) }))
      return reply({ json: { items, total: filtered.length, total_kind: 'exact', has_more: offset + 20 < filtered.length, next_cursor: offset + 20 < filtered.length ? 'review-page-2' : '', scope: 'management_review', checked_at: 1789650010 } })
    }
    if (url.pathname.endsWith('/review')) {
      const ref = decodeURIComponent(url.pathname.split('/').at(-2))
      if (route.request().method() === 'GET') {
        calls.get.push(ref)
        if (ref === blocked) await new Promise(resolve => { releaseBlocked = resolve })
        if (missing) return reply({ status: 404, json: { error: { message: 'deleted' } } })
        return reply({ json: { review: state.get(ref), record: rows.find(row => row.event_id === ref), scope: 'management_review' } })
      }
      const body = route.request().postDataJSON(); calls.put.push({ ref, body })
      if (missing) return reply({ status: 404, json: { error: { message: 'deleted' } } })
      if (failSave) return reply({ status: 503, json: { error: { message: '处理状态保存暂时失败' } } })
      if (conflict) {
        conflict = false
        state.set(ref, { ...state.get(ref), version: state.get(ref).version + 1, note: '另一个会话的记录' })
        return reply({ status: 409, json: { error: { code: 'version_conflict', message: 'version changed' } } })
      }
      assert.equal(body.version, state.get(ref).version)
      const at = 1789650010, actor = 'admin-session-synthetic', version = body.version + 1
      const review = { ...state.get(ref), ...body, version, updated_at: at, updated_by: actor, resolved_at: body.resolved ? at : 0, resolved_by: body.resolved ? actor : '', history: [{ version, resolved: body.resolved, at, actor, note: body.note }, ...state.get(ref).history], history_total: version }
      state.set(ref, review)
      return reply({ json: { review, record: rows.find(row => row.event_id === ref), scope: 'management_review' } })
    }
    if (url.pathname === '/api/logs') return reply({ json: { items: rows.slice(0, 3), total: 23, total_kind: 'exact', has_more: false, next_cursor: '' } })
    calls.details.push(query)
    return reply({ json: { record: rows.find(row => url.pathname.endsWith(row.event_id)) || rows[0], scope: 'retained_request_details' } })
  }
  await page.route('**/api/logs**', handler)
  try {
    await page.setViewportSize({ width: 1440, height: 1100 })
    await page.goto(new URL('/#/settings', page.url()).href)
    await page.goto(new URL('/#/logs?status=ok&account_id=review-account', page.url()).href)
    await expect(page.locator('.log-table .el-table__row')).toHaveCount(3)
    await page.getByRole('button', { name: '错误处理', exact: true }).click()
    const dialog = page.getByRole('dialog', { name: '错误处理', exact: true })
    await expect(dialog.locator('.review-list-row')).toHaveCount(20)
    await captureLogThemes(page, dialog, 'error-review-list', screenshotDirectory)
    assert.equal(calls.errors.at(-1).account_id, 'review-account'); assert.equal(calls.errors.at(-1).resolved, 'false')
    assert.equal(calls.errors.at(-1).as_of, undefined); assert.equal(calls.errors.at(-1).status, undefined)
    changedCursor = true
    await dialog.getByRole('button', { name: '下一页错误' }).click()
    await expect(dialog).toContainText('旧分页不再有效')
    await expect(dialog).toContainText('上次成功加载的第 1 页')
    await dialog.getByRole('button', { name: '刷新错误列表' }).click()
    await expect(dialog.locator('.review-list-error')).toHaveCount(0)
    assert.equal(calls.errors.at(-1).cursor, undefined)
    changedCursor = false
    await dialog.getByRole('combobox', { name: '错误处理列表状态' }).selectOption('true')
    await expect(dialog).toContainText('没有已处理的错误记录')
    await dialog.getByRole('combobox', { name: '错误处理列表状态' }).selectOption('false')
    await expect(dialog.locator('.review-list-row')).toHaveCount(20)
    assertions.push('独立错误处理列表继承应用筛选、排除主快照/成功状态；已处理空态及集合变化409从首页刷新')

    await dialog.getByRole('button', { name: '处理记录 review-1', exact: true }).click()
    const drawer = page.getByRole('dialog', { name: '日志详情', exact: true }), panel = drawer.locator('.error-review-panel')
    await expect(panel).toContainText('未处理')
    await expect(drawer).toContainText('HTTP 200')
    await expect(drawer).toContainText('独立错误处理视图读取当前留存记录')
    assert.equal(calls.details.at(-1).as_of, undefined)
    await panel.getByRole('checkbox', { name: '标记为已处理' }).check()
    await panel.getByRole('textbox', { name: '错误处理备注' }).fill('已核对参数 <img src=x onerror="window.__reviewXSS=true">')
    await panel.getByRole('button', { name: '保存处理状态' }).click()
    await expect(panel).toContainText('处理状态已保存')
    assert.equal(calls.put.at(-1).body.version, 0); assert.equal(calls.put.at(-1).body.resolved, true)
    assert.equal(rows[0].status, 'error'); assert.equal(rows[0].http_code, 200)
    assert.equal(await page.evaluate(() => window.__reviewXSS), undefined)
    await panel.getByText('处理历史 · 1 次变更', { exact: true }).click()
    await expect(panel.locator('.review-history')).toContainText('admin-session-synthetic')
    await captureLogThemes(page, panel, 'error-review-panel', screenshotDirectory)
    assertions.push('HTTP200失败可标记已处理；精确版本和备注持久化但原始失败不变，历史/XSS均只显示文本')

    conflict = true
    await panel.getByRole('textbox', { name: '错误处理备注' }).fill('本地冲突草稿')
    await panel.getByRole('checkbox', { name: '标记为已处理' }).uncheck()
    await panel.getByRole('button', { name: '保存处理状态' }).click()
    await expect(panel).toContainText('草稿已保留')
    await expect(panel.getByRole('button', { name: '保存处理状态' })).toBeDisabled()
    await panel.getByRole('button', { name: '重新读取状态' }).click()
    await expect(panel.getByRole('textbox', { name: '错误处理备注' })).toHaveValue('本地冲突草稿')
    await expect(panel.getByRole('button', { name: '保存处理状态' })).toBeEnabled()
    await panel.getByRole('button', { name: '保存处理状态' }).click()
    await expect(panel).toContainText('处理状态已保存')
    assert.equal(calls.put.at(-1).body.version, 2); assert.equal(calls.put.at(-1).body.resolved, false)
    assertions.push('并发版本409保留备注与撤销处理草稿；显式重读后使用新版本保存，不自动覆盖其他会话')

    failSave = true
    await panel.getByRole('textbox', { name: '错误处理备注' }).fill('网络失败仍保留')
    const beforeFailure = calls.put.length
    await panel.getByRole('button', { name: '保存处理状态' }).click()
    await expect(panel).toContainText('处理状态保存暂时失败')
    assert.equal(calls.put.length, beforeFailure + 1)
    await expect(panel.getByRole('textbox', { name: '错误处理备注' })).toHaveValue('网络失败仍保留')
    failSave = false; missing = true
    await panel.getByRole('button', { name: '保存处理状态' }).click()
    await expect(panel).toContainText('原始记录已清理或不存在')
    await expect(panel.getByRole('button', { name: '保存处理状态' })).toBeDisabled()
    missing = false
    await drawer.getByRole('button', { name: '关闭详情' }).click()
    assertions.push('保存失败不自动重试且保留草稿；原始明细404后阻止继续保存')

    blocked = 'review-1'
    await page.locator('.log-table').getByRole('button', { name: /日志详情/ }).first().click()
    await expect.poll(() => typeof releaseBlocked).toBe('function')
    await drawer.getByRole('button', { name: '关闭详情' }).click()
    await page.locator('.log-table').getByRole('button', { name: /日志详情/ }).nth(1).click()
    await expect(panel.getByRole('textbox', { name: '错误处理备注' })).toHaveValue('')
    blocked = ''; releaseBlocked(); releaseBlocked = undefined
    await expect(panel).toContainText('版本 0')
    await expect(panel.getByRole('textbox', { name: '错误处理备注' })).toHaveValue('')
    await page.setViewportSize({ width: 390, height: 844 })
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true)
    if (screenshotDirectory) await panel.screenshot({ path: `${screenshotDirectory}/logs-review-panel-390.png` })
    await drawer.getByRole('button', { name: '关闭详情' }).click()
    for (const width of [390, 320]) {
      await page.setViewportSize({ width, height: 844 })
      await page.getByRole('button', { name: '错误处理', exact: true }).click()
      await expect(dialog.locator('.review-list-row')).toHaveCount(20)
      assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true)
      if (screenshotDirectory) await dialog.screenshot({ path: `${screenshotDirectory}/logs-review-list-${width}.png` })
      await dialog.getByRole('button', { name: '关闭错误处理' }).click()
    }
    assertions.push('切换记录后迟到处理状态不覆盖新记录；390/320手机错误列表与详情无横向溢出')
    await page.setViewportSize({ width: 1440, height: 1100 })
    return { assertions, calls }
  } finally { releaseBlocked?.(); await page.unroute('**/api/logs**', handler) }
}
