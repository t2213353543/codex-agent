import assert from 'node:assert/strict'
import { expect } from 'playwright/test'
import { account } from './mock-api.mjs'

export async function testAccountRuntime(page, { capture = async () => {}, themes = false } = {}) {
  const origin = new URL(page.url()).origin
  assert.ok(['localhost', '127.0.0.1'].includes(new URL(origin).hostname))
  const row = account('runtime-fixture', '只读运行状态合成账号', { version: 5 })
  const second = account('runtime-unused', '本范围没有记录的账号')
  const calls = [], assertions = []
  let unavailable = false, fileScope = false, failLastUsed = false, slowResolve
  const snapshot = model => ({
    id: row.id, version: row.version, checked_at: Date.now(),
    admission: { scope: 'process', in_flight: 8, limit: 8, saturated: true },
    cooldowns: { coverage: 'connection_models_and_requested_model', model, shared_state: unavailable ? 'unavailable' : 'ok', entries: unavailable ? [] : [{ kind: 'rate_limit', scope: model ? `model:${model}` : 'connection', source: 'shared_redis', until: Date.now() + 90000 }, { kind: 'oauth_refresh_http', scope: 'token_refresh', source: 'process', http_code: 503, until: Date.now() + 45000 }] },
    effective_policy: { source: 'global_settings', settings_version: 19, per_account_override: false, fingerprint: { enabled: true, scope: 'application_headers_only', configured_profile: 'linux', profile: { id: 'linux', label: 'Linux CLI（源码参考配置）', user_agent: 'codex_cli_rs/synthetic (Linux synthetic; x86_64) unknown', originator: 'codex_cli_rs', version: 'synthetic' } }, websocket_enabled: false, http_stream_enabled: true, http_json_enabled: true, compact_enabled: true, models_enabled: true, compatibility_normalization_enabled: false, automatic_token_refresh_enabled: true, upstream_401_recovery_enabled: true, sticky_sessions_enabled: true, prompt_cache_key_enabled: true, builtin_prompt_enabled: false, builtin_prompt_mode: 'prepend', account: { enabled: false, concurrency: 8, has_refresh_token: false, expires_at: 0 } },
    last_used: { at: 1750000000, scope: fileScope ? 'retained_request_details' : 'durable_usage_events', source: 'gateway_request_records' },
  })
  const handler = async route => {
    const request = route.request(), url = new URL(request.url()), method = request.method()
    calls.push({ path: url.pathname, query: Object.fromEntries(url.searchParams), method, kind: method === 'POST' ? request.postDataJSON()?.kind : undefined })
    const reply = (json, status = 200) => route.fulfill({ status, json }).catch(() => {})
    if (url.pathname === '/api/accounts/manage') return reply({ accounts: [row, second], total: 2, total_kind: 'exact', has_more: false })
    if (url.pathname === `/api/accounts/${row.id}`) return reply({ account: row })
    if (url.pathname.endsWith('/metadata')) return reply({ id: row.id, identity: { organization_id: 'synthetic-org', source: 'import_metadata', verified: false }, privacy: { status: 'unknown', stale: true } })
    if (url.pathname.endsWith('/requests')) return reply({ items: [] })
    if (url.pathname.endsWith('/runtime')) {
      const model = url.searchParams.get('model') || '', result = snapshot(model)
      if (model === 'slow-model') await new Promise(resolve => { slowResolve = resolve })
      return reply(result)
    }
    if (url.pathname === '/api/accounts/last-used') return failLastUsed ? reply({ error: { message: '合成最近请求读取失败' } }, 503) : reply({ items: request.postDataJSON().ids.map(id => ({ id, at: id === row.id ? 1750000000 : null })), scope: fileScope ? 'retained_request_details' : 'durable_usage_events', checked_at: Math.floor(Date.now() / 1000) })
    return route.fallback()
  }
  await page.route('**/api/**', handler)
  try {
    await page.evaluate(() => { localStorage.setItem('accountColumns', JSON.stringify({ version: 1, value: ['name', 'status', 'actions'] })); localStorage.setItem('accountsTodayUsage', JSON.stringify({ version: 1, value: false })); localStorage.setItem('accountsAutoRefresh', JSON.stringify({ version: 1, value: 0 })) })
    await page.setViewportSize({ width: 1440, height: 1000 })
    await page.goto(origin + '/#/accounts')
    await expect(page.locator('.accounts-panel')).toContainText(row.name)
    assert.equal(calls.filter(call => call.path === '/api/accounts/last-used').length, 0)
    await page.getByRole('button', { name: '显示列', exact: true }).click()
    const columns = page.getByRole('dialog', { name: '显示 / 隐藏列', exact: true })
    await columns.getByRole('checkbox', { name: '最近业务请求', exact: true }).locator('xpath=ancestor::label').click()
    await columns.getByRole('button', { name: '完成', exact: true }).click()
    await expect(page.locator('.accounts-panel')).toContainText('全部已持久化请求事件')
    await expect(page.locator('.accounts-panel')).toContainText('此范围无匹配记录')
    assertions.push('最近业务请求列按需批量读取稳定ID，持久事件范围与null未匹配清楚分开')

    await page.getByRole('button', { name: `查看账号 ${row.name} 详情`, exact: true }).click()
    const drawer = page.getByRole('dialog', { name: '账号详情', exact: true }), panel = drawer.locator('.account-runtime-panel')
    await expect(panel).toContainText('容量已占满')
    await expect(panel).toContainText('共享 Redis 记录')
    await expect(panel).toContainText('HTTP 503')
    await expect(panel).toContainText('不代表当前访问令牌一定不可用')
    await expect(panel).toContainText('尚未指定模型')
    await expect(drawer.locator('.identity-panel')).toContainText('synthetic-org')
    assertions.push('详情真实并发/连接冷却/共享来源/OAuth等待分别展示，未检查模型不冒充健康')

    await panel.locator('.runtime-policy summary').click()
    await expect(panel).toContainText('账号没有独立覆盖')
    await expect(panel).toContainText('Linux CLI（源码参考配置）')
    await expect(panel).toContainText('不代表 iOS 客户端')
    await expect(panel).toContainText('无刷新令牌，开启系统刷新仍无法刷新该账号')
    await expect(panel.locator('.runtime-fields > div').filter({ has: page.locator('dt', { hasText: /^WebSocket$/ }) })).toContainText('系统已关闭')
    await panel.locator('.runtime-policy').evaluate(element => element.scrollIntoView({ block: 'start' }))
    await capture('account-runtime-policy-1440')
    await page.setViewportSize({ width: 320, height: 1000 })
    assert.ok(await panel.evaluate(element => element.scrollWidth <= element.clientWidth + 1))
    await capture('account-runtime-policy-320')
    await page.setViewportSize({ width: 1440, height: 1000 })
    assertions.push('策略显示实际全局版本/开关与账号参数，关闭WS/无RT不被系统默认能力掩盖')

    await panel.getByRole('textbox', { name: '检查指定模型的冷却', exact: true }).fill('slow-model')
    await panel.getByRole('textbox', { name: '检查指定模型的冷却', exact: true }).press('Enter')
    await expect.poll(() => Boolean(slowResolve)).toBe(true)
    await panel.getByRole('textbox', { name: '检查指定模型的冷却', exact: true }).fill('fast-model')
    await panel.getByRole('textbox', { name: '检查指定模型的冷却', exact: true }).press('Enter')
    await expect(panel).toContainText('模型 fast-model')
    slowResolve(); slowResolve = undefined
    await expect(panel).not.toContainText('模型 slow-model')
    assertions.push('模型专属只读查询支持Enter，旧慢查询取消且不能覆盖新结果')

    await panel.locator('.runtime-heading').evaluate(element => element.scrollIntoView({ block: 'start' }))
    await capture('account-runtime-1440')
    await page.setViewportSize({ width: 320, height: 1000 })
    assert.ok(await drawer.evaluate(element => element.scrollWidth <= element.clientWidth + 1))
    assert.ok(await panel.evaluate(element => element.scrollWidth <= element.clientWidth + 1))
    await panel.scrollIntoViewIfNeeded()
    await capture('account-runtime-320')
    if (themes) {
      for (const theme of ['light', 'dark']) for (const width of [1440, 390]) {
        await page.setViewportSize({ width, height: 1000 })
        await page.evaluate(value => { document.documentElement.dataset.theme = value; document.documentElement.classList.toggle('dark', value === 'dark'); window.dispatchEvent(new CustomEvent('theme-change', { detail: value })) }, theme)
        await panel.locator('.runtime-policy').evaluate(element => { element.open = false })
        await panel.locator('.runtime-heading').evaluate(element => element.scrollIntoView({ block: 'start' }))
        assert.ok(await drawer.evaluate(element => element.scrollWidth <= element.clientWidth + 1))
        await capture(`account-runtime-${theme}-${width}`)
        await panel.locator('.runtime-policy summary').click()
        await panel.locator('.runtime-policy').evaluate(element => element.scrollIntoView({ block: 'start' }))
        assert.ok(await panel.evaluate(element => element.scrollWidth <= element.clientWidth + 1))
        await capture(`account-runtime-policy-${theme}-${width}`)
      }
      await page.evaluate(() => { document.documentElement.dataset.theme = 'light'; document.documentElement.classList.remove('dark'); window.dispatchEvent(new CustomEvent('theme-change', { detail: 'light' })) })
      await page.setViewportSize({ width: 1440, height: 1000 })
    }
    assertions.push('运行状态与系统策略在320px可读，无抽屉/面板横向溢出')

    unavailable = true
    await panel.getByRole('button', { name: '读取运行状态', exact: true }).click()
    await expect(panel).toContainText('共享冷却状态未取得')
    await expect(panel).not.toContainText('已读取范围内未发现活动冷却记录')
    assertions.push('共享存储不可用显示未知，不伪造无冷却或恢复')
    await drawer.locator('.el-dialog__headerbtn').click()
    fileScope = true
    await page.setViewportSize({ width: 1440, height: 1000 })
    await page.getByRole('button', { name: '刷新账号列表', exact: true }).click()
    await expect(page.locator('.accounts-panel')).toContainText('仅留存请求详情')
    failLastUsed = true
    await page.getByRole('button', { name: '刷新账号列表', exact: true }).click()
    await expect(page.locator('.accounts-page')).toContainText('合成最近请求读取失败')
    await expect(page.locator('.accounts-panel')).toContainText('未取得')
    assert.ok(calls.every(call => call.method === 'GET' || call.path === '/api/accounts/last-used' || call.path === '/api/accounts/upstream/batch' && call.kind === 'usage'), 'read-only panel issued a mutation')
    assertions.push('文件范围明确留存限制，读取失败不伪装从未使用，全部流程无调度/凭据/上游写操作')
    return assertions
  } finally {
    slowResolve?.()
    await page.unroute('**/api/**', handler)
    await page.evaluate(() => localStorage.removeItem('accountColumns'))
    await page.setViewportSize({ width: 1440, height: 1000 })
  }
}
