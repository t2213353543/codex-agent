import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import { expect } from 'playwright/test'
import { account } from './mock-api.mjs'
import { accountRuntimeFixture } from './account-runtime-fixtures.mjs'
import { dailyStatsFixture, emptyDailyTotals } from './account-daily-stats-fixtures.mjs'

export async function testAccountsNoBilling(page, { capture = async () => {} } = {}) {
  const origin = new URL(page.url()).origin, row = account('nobilling-account', '用量管理合成账号')
  const calls = [], assertions = [], now = Math.floor(Date.now() / 1000)
  const lease = { account_index: 1, expires_at: now + 86400, auto_disable: false, restore_on_extend: false }
  const bundle = { type: 'codex-agent-accounts', version: 1, accounts: [{ name: '旧备份合成账号', groups: [], proxies: [], quota: 10000, credentials: { access_token: 'synthetic-old-secret' } }], management: { version: 1, leases: [lease], costs: [{ obsolete: true, malformed_price_metadata: 'keep original file only' }] } }
  const originalBundle = JSON.stringify(bundle)
  const handler = async route => {
    const req = route.request(), url = new URL(req.url()), body = req.postDataJSON()
    calls.push({ method: req.method(), path: url.pathname, body, query: Object.fromEntries(url.searchParams) })
    const reply = value => route.fulfill({ json: value })
    if (url.pathname === '/api/accounts/manage') return reply({ accounts: [row], total: 1, total_kind: 'exact', has_more: false })
    if (url.pathname === `/api/accounts/${row.id}`) return reply({ account: row })
    if (url.pathname.endsWith('/runtime')) return reply(accountRuntimeFixture(row))
    if (url.pathname.endsWith('/metadata')) return reply({ id: row.id, identity: {}, privacy: {} })
    if (url.pathname.endsWith('/requests')) return reply({ items: [] })
    if (url.pathname === '/api/accounts/usage-batch') return reply({ items: body.ids.map(id => ({ id, total_requests: 12, total_tokens: 4000, estimated_cost_usd: '987654.321' })), from: body.from, to: body.to, checked_at: now, scope: 'retained_request_details' })
    if (url.pathname === '/api/logs/snapshot') return reply({ as_of: 'no-billing-snapshot', scope: 'retained_request_details' })
    if (url.pathname === '/api/logs/stats') return reply(dailyStatsFixture(Object.fromEntries(url.searchParams), { ...emptyDailyTotals(), total_requests: 6, success: 4, errors: 1, aborted: 1, total_input_tokens: 700, total_output_tokens: 200, total_cached_tokens: 500, total_tokens: 900, usage_known_requests: 5, usage_unknown_requests: 1, average_latency_ms: 650, ttft_samples: 2, average_ttft_ms: 0, models: [{ name: 'synthetic-model', total_requests: 6, total_tokens: 900 }] }))
    if (url.pathname === '/api/accounts/export/credentials') return reply(bundle)
    if (url.pathname === '/api/accounts/import-bundle') {
      assert.equal(Object.hasOwn(body.data.management, 'costs'), false)
      assert.deepEqual(body.data.management.leases, [lease])
      assert.equal(body.data.accounts[0].credentials.access_token, 'synthetic-old-secret')
      return reply({ dry_run: body.dry_run, added: 1, failed: 0, imported: 1, results: [{ index: 1, id: 'new-import-id', name: '旧备份合成账号', status: 'added' }], lease_restore: { dry_run: body.dry_run, ...body.dry_run ? {} : { receipt_id: 'nobilling-lease', expires_at: now + 1800 }, items: [{ ...lease, name: '旧备份合成账号', status: body.dry_run ? 'pending_import' : 'pending', ...body.dry_run ? {} : { id: 'new-import-id', account_version: 1, lease_version: 0 } }] }, cost_restore: { receipt_id: 'retired-must-ignore', dry_run: false, items: [] } })
    }
    if (url.pathname === '/api/accounts/import/preview' || url.pathname === '/api/accounts' && req.method() === 'POST') {
      assert.equal(Object.hasOwn(body, 'include_external_costs'), false)
      const raw = JSON.parse(body.raw), value = Array.isArray(raw) ? raw[0] : raw
      return reply({ dry_run: url.pathname.endsWith('/preview'), imported: 1, results: [{ index: 1, name: value.name, id: 'raw-id', status: 'added', warnings: ['sub2api_settings_not_imported'] }] })
    }
    return route.fallback()
  }
  const noBilling = async scope => { await expect(scope).not.toContainText(/采购|售价|计费|未定价|费率|成本|USD|987654\.321/) }
  const tick = locator => locator.locator('xpath=ancestor::label[1]').click()
  await page.route('**/api/**', handler)
  try {
    await page.goto(origin + '/#/accounts'); await page.getByRole('button', { name: '刷新账号列表', exact: true }).click()
    await expect(page.locator('.accounts-panel')).toContainText(row.name)
    await noBilling(page.locator('.accounts-page'))
    for (const name of ['添加账号', '导入账号', '迁移备份']) await expect(page.getByRole('button', { name, exact: true })).toBeVisible()
    const today = page.getByRole('checkbox', { name: '今日用量', exact: true })
    if (!(await today.isChecked())) await tick(today)
    await expect(page.locator('.accounts-panel')).toContainText('4000'); await noBilling(page.locator('.accounts-panel'))
    assertions.push('账号列表无账务入口，旧响应即使带金额也不显示，今日请求/Token和账号管理操作保留')
    await page.getByRole('button', { name: `查看账号 ${row.name} 详情`, exact: true }).click()
    const detail = page.getByRole('dialog', { name: '账号详情', exact: true })
    for (const name of ['OAuth 重新授权', '用量与测试', '账号到期管理', '账号统计']) await expect(detail.getByRole('button', { name, exact: true })).toBeVisible()
    await noBilling(detail)
    await detail.getByRole('button', { name: '账号统计', exact: true }).click()
    const stats = page.getByRole('dialog', { name: '账号使用统计', exact: true })
    await expect(stats.locator('.account-daily-stats')).toBeVisible(); await expect(stats).toContainText('650 ms'); await expect(stats).toContainText('0 ms'); await noBilling(stats)
    assert.deepEqual(await stats.getByLabel('账号日趋势指标', { exact: true }).locator('option').allTextContents(), ['请求', 'Token'])
    await stats.getByRole('button', { name: '按所选自然日', exact: true }).click(); await stats.getByLabel('账号日趋势指标', { exact: true }).selectOption('tokens')
    await expect(stats.locator('.daily-chart canvas')).toHaveCount(1)
    assert.ok(await stats.locator('.daily-chart canvas').evaluate(canvas => { const pixels = canvas.getContext('2d').getImageData(0, 0, canvas.width, canvas.height).data; return pixels.some((value, index) => index % 4 === 3 && value > 0) }))
    await stats.locator('.daily-table-details summary').click(); assert.deepEqual(await stats.locator('.daily-table-scroll th').allTextContents(), ['日期', '请求', '已记录 Token'])
    assertions.push('日统计无需金额字段，保留活跃/自然日、请求Token峰值、TTFT真0、两指标非空图及非金额明细')
    for (const theme of ['light', 'dark']) for (const width of [1440, 390]) {
      await page.setViewportSize({ width, height: 1000 }); await page.evaluate(value => { document.documentElement.dataset.theme = value; document.documentElement.classList.toggle('dark', value === 'dark'); window.dispatchEvent(new CustomEvent('theme-change', { detail: value })) }, theme)
      assert.ok(await stats.evaluate(element => element.scrollWidth <= element.clientWidth + 1))
      await stats.locator('.daily-overview').scrollIntoViewIfNeeded(); await capture(`account-usage-summary-${theme}-${width}`, stats.locator('.daily-overview'))
      await stats.locator('.daily-chart').scrollIntoViewIfNeeded(); await capture(`account-usage-trend-${theme}-${width}`, stats.locator('.daily-chart'))
    }
    await stats.locator('.el-dialog__headerbtn').click(); await detail.locator('.el-dialog__headerbtn').click(); await page.setViewportSize({ width: 1440, height: 1000 })
    assertions.push('留白/夜航桌面与390手机用量日摘要与趋势无重叠，账号统计功能保留')
    await page.getByRole('button', { name: '迁移备份', exact: true }).click(); await page.getByRole('menuitem', { name: '导出账号', exact: true }).click()
    let dialog = page.getByRole('dialog', { name: '导出账号', exact: true })
    await tick(dialog.getByRole('checkbox', { name: '导出可迁移凭据备份', exact: true })); await noBilling(dialog)
    await tick(dialog.getByRole('checkbox', { name: '包含账号独立租期配置', exact: true }))
    await expect(dialog.getByRole('checkbox', { name: '包含所选账号关联的分组与代理定义', exact: true }).locator('xpath=ancestor::label[1]')).toBeVisible()
    await dialog.getByRole('button', { name: '生成并下载', exact: true }).click(); const pending = page.waitForEvent('download'); await page.getByRole('button', { name: '确认导出凭据', exact: true }).click(); const downloaded = await pending
    const exported = JSON.parse(await readFile(await downloaded.path(), 'utf8'))
    assert.equal(Object.hasOwn(exported.management, 'costs'), false); assert.deepEqual(exported.management.leases, [lease])
    assert.equal(Object.hasOwn(calls.find(call => call.path.endsWith('/export/credentials')).body, 'include_costs'), false)
    await dialog.getByRole('button', { name: '关闭', exact: true }).click()
    assertions.push('导出仅账号/关联资源/租期，无include_costs，旧服务回传金额段不进入下载文件')
    await page.getByRole('button', { name: '迁移备份', exact: true }).click(); await page.getByRole('menuitem', { name: '恢复凭据备份', exact: true }).click()
    dialog = page.getByRole('dialog', { name: '恢复账号备份', exact: true })
    await dialog.getByLabel('账号备份文件', { exact: true }).setInputFiles({ name: 'legacy-backup.json', mimeType: 'application/json', buffer: Buffer.from(originalBundle) })
    await dialog.getByRole('button', { name: '生成预览', exact: true }).click(); await expect(dialog).toContainText('尚未写入账号')
    await dialog.getByRole('button', { name: '确认恢复', exact: true }).click(); await expect(dialog.locator('.lease-restore')).toContainText('new-import-id')
    await noBilling(dialog); assert.equal(JSON.stringify(bundle), originalBundle)
    await dialog.getByRole('button', { name: '关闭', exact: true }).click()
    assertions.push('含旧成本段备份在内存剥离后预览/恢复，原文件对象不改，凭据及租期回执正常且成本回执被忽略')
    await page.getByRole('button', { name: '导入账号', exact: true }).click(); dialog = page.getByRole('dialog', { name: '导入账号 · 先预览再提交', exact: true })
    await noBilling(dialog); await expect(dialog.getByRole('checkbox', { name: '识别 Sub2API 外部管理租期，导入账号后单独确认', exact: true }).locator('xpath=ancestor::label[1]')).toBeVisible()
    await dialog.getByRole('button', { name: '粘贴内容', exact: true }).click()
    await dialog.getByRole('textbox', { name: '账号导入内容', exact: true }).fill(JSON.stringify({ name: 'raw-nobilling', platform: 'openai', type: 'oauth', rate_multiplier: 3, weight: 7, credentials: { access_token: 'synthetic-raw-secret' } }))
    await dialog.getByRole('button', { name: '生成预览', exact: true }).click(); await expect(dialog).toContainText('原系统的扩展配置未自动导入')
    await dialog.getByRole('button', { name: '确认导入 1 项', exact: true }).click(); await expect(dialog.locator('.result-table')).toContainText('成功')
    assert.equal(JSON.parse(calls.filter(call => call.path === '/api/accounts' && call.method === 'POST').at(-1).body.raw).weight, 7)
    await dialog.getByRole('button', { name: '关闭', exact: true }).click()
    assert.ok(!calls.some(call => /pricing|cost-policy|cost-rules|sale-policy|import-costs/.test(call.path)))
    assertions.push('原始导入不发送include_external_costs，无倍率操作，保留外租选项与原权重，无任何账务API调用')
    return assertions
  } finally { await page.unroute('**/api/**', handler); await page.setViewportSize({ width: 1440, height: 1000 }); await page.evaluate(() => { document.documentElement.dataset.theme = 'light'; document.documentElement.classList.remove('dark'); window.dispatchEvent(new CustomEvent('theme-change', { detail: 'light' })) }) }
}
