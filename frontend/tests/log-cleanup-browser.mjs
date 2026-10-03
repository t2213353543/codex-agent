import assert from 'node:assert/strict'
import { expect } from 'playwright/test'
import { logWorkspacePreviewRows } from './log-workspace-fixtures.mjs'

export async function testLogCleanup(page, { screenshotDirectory } = {}) {
  assert.ok(['localhost', '127.0.0.1'].includes(new URL(page.url()).hostname))
  const now = Math.floor(Date.now() / 1000)
  const scope = 'retained_request_details'
  const requests = { preview: [], create: [], cancel: [], list: [] }
  const existing = { id: 'job-existing', scope, status: 'failed', total: 100, deleted_rows: 25, remaining_rows: 75, created_by: '管理会话', created_at: now - 600, finished_at: now - 500, error: '<img src=x onerror="window.__cleanupInjected=true"> 合成批次失败，保留真实进度', filters: { from: String(now - 86400), to: String(now - 3600) } }
  const jobs = [existing]
  let attempt = 0
  const handler = async route => {
    const url = new URL(route.request().url())
    if (url.pathname === '/api/logs/snapshot') return route.fulfill({ json: { as_of: 'usage_fixture_' + Date.now(), created_at: Math.floor(Date.now() / 1000), expires_at: Math.floor(Date.now() / 1000) + 300, scope: 'retained_request_details', total: 1001 } })
    const body = route.request().postDataJSON()
    if (url.pathname.endsWith('/preview')) {
      requests.preview.push(body)
      return route.fulfill({ json: { preview_id: 'preview-fixture', confirmation_token: 'synthetic-confirmation-token', scope, expires_at: now + 300, total: 37, filters: body.filters } })
    }
    if (url.pathname.endsWith('/cancel')) {
      requests.cancel.push(url.pathname)
      const job = jobs.find(item => url.pathname.includes(item.id))
      job.status = 'canceled'
      job.finished_at = now
      return route.fulfill({ json: { job } })
    }
    if (route.request().method() === 'POST') {
      requests.create.push(body)
      if (++attempt === 1) return route.fulfill({ status: 503, json: { error: { message: '任务提交结果暂未确认，请重试' } } })
      const job = { id: 'job-created', scope, status: 'pending', total: 37, deleted_rows: 3, remaining_rows: 34, created_by: '管理会话', created_at: now, filters: requests.preview.at(-1).filters }
      jobs.unshift(job)
      return route.fulfill({ json: { job } })
    }
    requests.list.push(Object.fromEntries(url.searchParams))
    return route.fulfill({ json: { items: jobs, total: jobs.length } })
  }
  await page.route('**/api/logs/cleanup/**', handler)
  const listHandler = route => route.fulfill({ json: { items: logWorkspacePreviewRows.map(row => ({ ...row, usage_known: true })), total: 1001, total_kind: 'exact', has_more: false, next_cursor: '' } })
  const statsHandler = route => route.fulfill({ json: { total_requests: 1001, success: 998, errors: 3, total_input_tokens: 150150, total_output_tokens: 12012, total_cached_tokens: 75075, total_tokens: 162162, average_latency_ms: 1450, usage_known_requests: 1001, usage_unknown_requests: 0, models: [], endpoints: [], series: [], error_categories: [] } })
  await page.route('**/api/logs?*', listHandler)
  await page.route('**/api/logs/stats?*', statsHandler)
  const assertions = []
  try {
    await page.setViewportSize({ width: 1440, height: 1100 })
    await page.getByRole('button', { name: '清理明细', exact: true }).click()
    const dialog = page.getByRole('dialog', { name: '清理请求明细', exact: true })
    await expect(dialog).toContainText('累计用量与账号额度会保留')
    await expect(dialog).not.toContainText('账务')
    await dialog.getByRole('button', { name: '预览匹配记录' }).click()
    await expect(dialog).toContainText('请选择明确的开始与结束时间')
    assert.equal(requests.preview.length, 0)
    await dialog.getByRole('textbox', { name: '清理开始时间' }).fill('2026-09-16T00:00')
    await dialog.getByRole('textbox', { name: '清理结束时间' }).fill('2026-09-17T00:00')
    await dialog.getByRole('button', { name: '预览匹配记录' }).click()
    await expect(dialog).toContainText('确认 37 条请求明细')
    await expect(dialog.getByRole('button', { name: '创建清理任务' })).toBeDisabled()
    assert.equal(requests.create.length, 0)
    await dialog.getByRole('button', { name: '关闭', exact: true }).click()
    await expect(dialog).toBeHidden()
    assert.equal(requests.create.length, 0)
    assertions.push('明确时间范围才允许预览；预览和关闭不会创建删除任务')

    await page.getByRole('button', { name: '清理明细', exact: true }).click()
    await dialog.getByRole('textbox', { name: '清理开始时间' }).fill('2026-09-16T00:00')
    await dialog.getByRole('textbox', { name: '清理结束时间' }).fill('2026-09-17T00:00')
    await dialog.getByRole('button', { name: '预览匹配记录' }).click()
    await dialog.getByRole('checkbox', { name: /我确认删除以上/ }).check()
    await dialog.getByRole('textbox', { name: '清理结束时间' }).fill('2026-09-17T01:00')
    await expect(dialog.locator('.cleanup-confirmation')).toHaveCount(0)
    await dialog.getByRole('button', { name: '预览匹配记录' }).click()
    await dialog.getByRole('checkbox', { name: /我确认删除以上/ }).check()
    await dialog.getByRole('button', { name: '创建清理任务' }).click()
    await expect(dialog).toContainText('任务提交结果暂未确认')
    await dialog.getByRole('button', { name: '创建清理任务' }).click()
    await expect(dialog).toContainText('job-created')
    assert.equal(requests.create.length, 2)
    assert.equal(requests.create[0].request_id, requests.create[1].request_id)
    assert.match(requests.create[0].request_id, /^[\da-f-]{36}$/)
    assertions.push('修改时间使预览和确认失效；模糊提交失败重试沿用 UUID，不另造任务')

    await dialog.getByRole('button', { name: '取消任务', exact: true }).click()
    const cancel = page.getByRole('dialog', { name: '取消清理任务', exact: true })
    await expect(cancel).toContainText('3 条明细不会恢复')
    await cancel.getByRole('button', { name: '继续任务' }).click()
    assert.equal(requests.cancel.length, 0)
    await dialog.getByRole('button', { name: '取消任务', exact: true }).click()
    await cancel.getByRole('button', { name: '确认取消任务' }).click()
    await expect(dialog).toContainText('已取消')
    assert.equal(requests.cancel.length, 1)
    const failed = dialog.locator('.cleanup-job').filter({ hasText: 'job-existing' })
    await expect(failed.getByRole('progressbar')).toHaveAttribute('aria-valuenow', '25')
    await expect(failed).toContainText('25 / 100 条')
    assert.equal(await failed.locator('img,script').count(), 0)
    assert.equal(await page.evaluate(() => window.__cleanupInjected), undefined)
    assertions.push('取消需独立确认且保留已删进度；失败任务保持真实 25% 而非虚假完成，错误仅文本')

    await expect(page.locator('.el-message:visible')).toHaveCount(0, { timeout: 8000 })
    if (screenshotDirectory) await dialog.screenshot({ path: `${screenshotDirectory}/logs-cleanup-1440.png` })
    await page.setViewportSize({ width: 390, height: 844 })
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true)
    if (screenshotDirectory) await dialog.screenshot({ path: `${screenshotDirectory}/logs-cleanup-390.png` })
    await dialog.getByRole('button', { name: '关闭', exact: true }).click()
    return { assertions, requests }
  } finally { await page.unroute('**/api/logs/cleanup/**', handler); await page.unroute('**/api/logs?*', listHandler); await page.unroute('**/api/logs/stats?*', statsHandler) }
}
