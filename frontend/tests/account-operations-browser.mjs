import assert from 'node:assert/strict'
import { expect } from 'playwright/test'
import { account } from './mock-api.mjs'

export async function testAccountOperations(page, { capture = async () => {} } = {}) {
  const origin = new URL(page.url()).origin
  assert.ok(['localhost', '127.0.0.1'].includes(new URL(origin).hostname))
  const row = account('operations-1', '授权与检查合成账号', { version: 7 })
  const calls = []
  const assertions = []
  const now = Math.floor(Date.now() / 1000)
  let failApply = true
  let nextSession = 0
  let sessionTarget = ''
  let autoPolicy = { version: 0, enabled: false, threshold_percent: 95, interval_minutes: 15, phase: 'off' }
  const snapshots = { usage: { id: row.id, kind: 'usage', ok: true, checked_at: now - 600, stale: true, source: 'codex_usage', data: { plan_type: 'team', rate_limit: { allowed: null, limit_reached: null, primary_window: { used_percent: null, limit_window_seconds: 18000, reset_at: null }, secondary_window: { used_percent: 68, limit_window_seconds: 604800, reset_at: now + 10000 } }, credits: null } } }
  const snapshot = (kind, extra = {}) => ({ id: row.id, version: 7, credential_version: 7, kind, ok: true, http_code: 200, checked_at: now, latency_ms: 120, source: `codex_${kind}`, stale: false, ...extra })
  const ready = () => ({ session_id: `session-${nextSession}`, phase: 'ready', expires_at: now + 600, has_refresh_token: true, expires_at_token: now + 86400, identity_change: Boolean(sessionTarget), target_changed: false })
  const handler = async route => {
    const request = route.request()
    const url = new URL(request.url())
    const method = request.method()
    const body = request.postDataJSON()
    calls.push({ path: url.pathname, method, body })
    const reply = (json, status = 200) => route.fulfill({ status, json }).catch(() => {})
    if (url.pathname === '/api/accounts/manage') return reply({ accounts: [row], total: 1, total_kind: 'exact', has_more: false })
    if (url.pathname === `/api/accounts/${row.id}`) return reply({ account: row })
    if (url.pathname.endsWith('/metadata')) return reply({ id: row.id, version: row.version, identity: { source: 'synthetic_jwt', collected_at: now, verified: false }, privacy: { status: 'unknown', source: '', collected_at: 0, stale: false } })
    if (url.pathname.endsWith('/requests')) return reply({ items: [] })
    if (url.pathname === '/api/account-oauth/start') { nextSession++; sessionTarget = body.target_id || ''; return reply({ session_id: `session-${nextSession}`, phase: 'pending', auth_url: 'https://auth.openai.com/oauth/authorize?state=synthetic-state', redirect_uri: 'http://localhost:1455/auth/callback', expires_at: now + 600 }) }
    if (url.pathname === '/api/account-oauth/exchange') return reply(ready())
    if (url.pathname === '/api/account-oauth/import') { nextSession++; sessionTarget = body.target_id || ''; return reply(ready()) }
    if (url.pathname === '/api/account-oauth/apply') return failApply ? reply({ error: { message: '合成名称冲突' } }, 409) : reply({ ok: true, id: row.id, version: 8 })
    if (url.pathname.startsWith('/api/account-oauth/') && method === 'DELETE') return reply({ ok: true })
    if (url.pathname.endsWith('/test-stream')) {
      const result = snapshot('stream', { model: body.model, ok: false, error: { code: 'upstream_stream_failed', message: 'response.failed：合成的流式失败' }, data: null })
      snapshots.stream = result
      return route.fulfill({ status: 200, contentType: 'text/event-stream', body: `event: phase\ndata: ${JSON.stringify({ phase: 'streaming' })}\n\nevent: delta\ndata: ${JSON.stringify({ text: '合成流式输出' })}\n\nevent: result\ndata: ${JSON.stringify(result)}\n\n` })
    }
    if (url.pathname.endsWith('/upstream')) {
      if (method === 'GET') return reply({ id: row.id, persistent: true, snapshots })
      let result
      if (body.kind === 'models') result = snapshot('models', { data: { models: [{ id: 'gpt-codex-synthetic', display_name: '合成 Codex 模型', context_window: 196000 }], inference_verified: false } })
      else if (body.kind === 'usage') result = snapshot('usage', { data: { plan_type: 'team', rate_limit: { allowed: true, limit_reached: false, primary_window: { used_percent: 37, limit_window_seconds: 18000, reset_at: now + 3600 }, secondary_window: { used_percent: 68, limit_window_seconds: 604800, reset_at: now + 10000 } }, credits: { has_credits: false, unlimited: false, balance: null } } })
      else if (body.kind === 'stream') result = snapshot('stream', { model: body.model, ok: false, error: { code: 'upstream_stream_failed', message: 'response.failed：合成的流式失败' }, data: null })
      else if (body.kind === 'compact') result = snapshot('compact', { model: body.model, data: { compaction_found: true } })
      else if (body.kind === 'reset_credits') result = snapshot('reset_credits', { data: { available_count: 1, credits: [{ id: 'credit-fixture-available', reset_type: 'codex', status: 'available', expires_at: new Date((now + 86400) * 1000).toISOString(), title: '合成可用券' }, { id: 'credit-fixture-unknown', reset_type: 'codex', status: 'unknown', expires_at: null, title: '未知状态券' }] } })
      else throw new Error('Unexpected upstream kind: ' + body.kind)
      snapshots[body.kind] = result
      return reply(result)
    }
    if (url.pathname.endsWith('/privacy')) { snapshots.privacy = snapshot('privacy', { data: { training_allowed: false, verification: 'upstream_acknowledged' } }); return reply(snapshots.privacy) }
    if (url.pathname.endsWith('/auto-reset')) {
      if (method === 'PUT') autoPolicy = { ...autoPolicy, ...body, version: autoPolicy.version + 1, phase: body.acknowledge_unknown ? 'pending' : 'unknown', last_error: body.acknowledge_unknown ? '' : '合成的自动兑换未知结果' }
      return reply({ id: row.id, account_version: row.version, policy: autoPolicy })
    }
    if (url.pathname.endsWith('/reset-credit')) { snapshots.reset_credit = snapshot('reset_credit', { ok: false, data: { outcome_unknown: true }, error: { code: 'outcome_unknown', message: '上游结果未知' } }); return reply(snapshots.reset_credit) }
    return route.fallback()
  }
  await page.route('**/api/account**', handler)
  const select = async (name, option, scope = page) => { await scope.getByRole('combobox', { name, exact: true }).press('Enter'); await page.getByRole('option', { name: option, exact: true }).click() }
  const tick = async locator => locator.locator('xpath=ancestor::label').click()
  try {
    await page.goto(origin + '/#/accounts')
    await page.getByRole('button', { name: '刷新账号列表', exact: true }).click()
    await expect(page.locator('.accounts-panel')).toContainText(row.name)
    await page.getByRole('button', { name: '添加账号', exact: true }).click()
    let dialog = page.getByRole('dialog', { name: '添加 Codex 账号', exact: true })
    await dialog.getByRole('button', { name: '生成授权链接', exact: true }).click()
    await expect(dialog.getByRole('link', { name: '打开 OpenAI 授权页面' })).toHaveAttribute('href', /^https:\/\/auth\.openai\.com\//)
    await dialog.getByRole('textbox', { name: 'OAuth 回调地址或授权码', exact: true }).fill('http://localhost:1455/auth/callback?code=synthetic-code&state=synthetic-state')
    await dialog.getByRole('button', { name: '确认授权回调', exact: true }).click()
    await expect(dialog).toContainText('凭据已准备就绪')
    await dialog.getByRole('textbox', { name: '新建 Codex 账号名称', exact: true }).fill('新账号')
    await dialog.getByRole('textbox', { name: 'OAuth 新账号备注', exact: true }).fill('合成创建备注')
    await dialog.getByRole('switch', { name: 'OAuth 新账号启用', exact: true }).locator('..').click()
    await dialog.getByRole('button', { name: '创建账号', exact: true }).click()
    await expect(dialog).toContainText('授权凭据仍在会话中')
    assert.equal(calls.filter(call => call.path === '/api/account-oauth/exchange').length, 1)
    assert.equal(calls.filter(call => call.path === '/api/account-oauth/apply').at(-1).body.notes, '合成创建备注')
    assert.equal(calls.filter(call => call.path === '/api/account-oauth/apply').at(-1).body.enabled, false)
    failApply = false
    await dialog.getByRole('button', { name: '创建账号', exact: true }).click()
    await expect(dialog).toBeHidden()
    assert.equal(calls.filter(call => call.path === '/api/account-oauth/exchange').length, 1)
    assertions.push('浏览器 OAuth 官方链接、回调交换、ready 会话保留及保存冲突重试')

    await page.getByRole('button', { name: '添加账号', exact: true }).click()
    dialog = page.getByRole('dialog', { name: '添加 Codex 账号', exact: true })
    await select('Codex 授权方式', '移动端刷新令牌', dialog)
    await dialog.getByRole('textbox', { name: 'OAuth 凭据内容', exact: true }).fill('synthetic-mobile-refresh')
    await dialog.getByRole('button', { name: '校验并准备凭据', exact: true }).click()
    await expect(dialog).toContainText('必须填写签发时的原 client_id')
    assert.equal(calls.filter(call => call.path === '/api/account-oauth/import').length, 0)
    await dialog.getByRole('textbox', { name: 'OAuth client_id', exact: true }).fill('synthetic-mobile-client')
    await dialog.getByRole('button', { name: '校验并准备凭据', exact: true }).click()
    await expect(dialog).toContainText('凭据已准备就绪')
    assert.equal(calls.filter(call => call.path === '/api/account-oauth/import').at(-1).body.client_id, 'synthetic-mobile-client')
    assert.ok(!(await dialog.innerText()).includes('synthetic-mobile-refresh'))
    await dialog.getByRole('button', { name: '取消', exact: true }).click()
    await expect(dialog).toBeHidden()
    await expect.poll(() => calls.filter(call => call.method === 'DELETE').length).toBeGreaterThan(0)
    assertions.push('移动端原 client_id 必填、校验后清输入、取消销毁暂存会话')

    await page.getByRole('button', { name: `查看账号 ${row.name} 详情`, exact: true }).click()
    const details = page.getByRole('dialog', { name: '账号详情', exact: true })
    const centered = async surface => {
      const panel = surface.locator('.el-dialog')
      await expect(panel).toBeVisible()
      await expect.poll(async () => panel.evaluate(element => {
        const box = element.getBoundingClientRect()
        return Math.max(Math.abs(box.x + box.width / 2 - innerWidth / 2), Math.abs(box.y + box.height / 2 - innerHeight / 2))
      })).toBeLessThan(3)
      await expect(surface.locator('.el-dialog__footer')).toBeInViewport()
      assert.ok(await panel.evaluate(element => element.scrollWidth <= element.clientWidth + 1))
    }
    await centered(details)
    await expect(page.locator('.el-drawer:visible')).toHaveCount(0)
    await details.getByRole('button', { name: '编辑账号', exact: true }).click()
    const edit = page.getByRole('dialog', { name: /^编辑账号/ })
    await centered(edit)
    await edit.getByRole('textbox', { name: '账号备注', exact: true }).fill('暂不保存的编辑内容')
    await edit.getByRole('button', { name: '取消', exact: true }).click()
    await page.getByRole('button', { name: '继续编辑', exact: true }).click()
    await expect(edit.getByRole('textbox', { name: '账号备注', exact: true })).toHaveValue('暂不保存的编辑内容')
    await edit.locator('.el-dialog__headerbtn').click()
    await page.getByRole('button', { name: '放弃修改', exact: true }).click()
    await expect(edit).toBeHidden()
    await expect(details).toBeVisible()
    await expect(details.getByRole('button', { name: '编辑账号', exact: true })).toBeFocused()
    for (const theme of ['light', 'dark']) {
      await page.evaluate(value => { document.documentElement.dataset.theme = value; document.documentElement.classList.toggle('dark', value === 'dark'); window.dispatchEvent(new CustomEvent('theme-change', { detail: value })) }, theme)
      for (const width of [1440, 390]) {
        await page.setViewportSize({ width, height: 1000 })
        await centered(details)
        await capture(`account-centered-detail-${theme}-${width}`)
      }
    }
    await page.setViewportSize({ width: 1440, height: 1000 })
    assertions.push('账号详情和嵌套编辑居中、页脚始终可见；未保存取消保留输入、放弃后返回账号并恢复焦点；双主题/手机无溢出')
    await details.getByRole('button', { name: 'OAuth 重新授权', exact: true }).click()
    dialog = page.getByRole('dialog', { name: 'OAuth 重新授权', exact: true })
    await select('Codex 授权方式', 'Codex session / auth.json', dialog)
    await dialog.getByRole('textbox', { name: 'OAuth 凭据内容', exact: true }).fill(JSON.stringify({ tokens: { access_token: 'synthetic-session-secret' }, agent_identity: { account_id: 'synthetic-workspace', chatgpt_user_id: 'synthetic-user', agent_runtime_id: 'synthetic-runtime', agent_private_key: 'synthetic-not-a-private-key' } }))
    await dialog.getByRole('button', { name: '校验并准备凭据', exact: true }).click()
    await expect(dialog.getByRole('button', { name: '保存重新授权', exact: true })).toBeDisabled()
    await tick(dialog.getByRole('checkbox', { name: '我确认将此账号更换为新的上游登录身份', exact: true }))
    await dialog.getByRole('button', { name: '保存重新授权', exact: true }).click()
    await expect(dialog).toBeHidden()
    const imported = calls.filter(call => call.path === '/api/account-oauth/import').at(-1).body
    assert.equal(imported.target_id, row.id); assert.equal(imported.version, 7)
    assert.equal(imported.credentials.agent_identity.chatgpt_user_id, 'synthetic-user')
    assert.equal(calls.filter(call => call.path === '/api/account-oauth/apply').at(-1).body.notes, undefined)
    assert.equal(calls.filter(call => call.path === '/api/account-oauth/apply').at(-1).body.allow_identity_change, true)
    assertions.push('指定账号 OAuth/session 重授权保留目标版本，不同登录身份必须明确确认')

    await details.getByRole('button', { name: '用量与测试', exact: true }).click()
    const drawer = page.getByRole('dialog', { name: '账号用量与测试', exact: true })
    await centered(drawer)
    await capture('account-centered-upstream-1440')
    await expect(drawer).toContainText('快照已陈旧')
    await expect(drawer).toContainText('未知')
    assert.equal(calls.filter(call => call.path.endsWith('/upstream') && call.method === 'POST').length, 0)
    await drawer.getByRole('button', { name: '查询上游额度', exact: true }).click()
    await expect(drawer).toContainText('37%')
    await drawer.getByRole('tab', { name: '模型目录', exact: true }).click()
    await drawer.getByRole('button', { name: '读取模型目录', exact: true }).click()
    await expect(drawer).toContainText('合成 Codex 模型')
    await drawer.getByRole('button', { name: '选择测试', exact: true }).click()
    await expect(drawer.getByRole('button', { name: '开始测试', exact: true })).toBeDisabled()
    await tick(drawer.getByRole('checkbox', { name: '我确认此测试会实际发送请求，可能消耗上游额度', exact: true }))
    await drawer.getByRole('button', { name: '开始测试', exact: true }).click()
    await expect(drawer).toContainText('测试失败')
    await expect(drawer).toContainText('HTTP 200')
    await expect(drawer).toContainText('response.failed')
    await select('账号测试模式', 'compact 压缩响应', drawer)
    await drawer.getByRole('button', { name: '开始测试', exact: true }).click()
    await expect(drawer).toContainText('测试通过')
    await expect(drawer).toContainText('gpt-codex-synthetic')
    assert.ok(calls.filter(call => call.body?.kind === 'stream' || call.body?.kind === 'compact').every(call => call.body.confirm_inference === true && call.body.model === 'gpt-codex-synthetic'))
    assertions.push('打开仅读缓存、未知额度不伪零、真实模型目录、显式推理确认、HTTP200 failed 与 compact 分开判定')

    await drawer.getByRole('tab', { name: '隐私与重置', exact: true }).click()
    await drawer.getByRole('button', { name: '关闭训练使用', exact: true }).click()
    await page.getByRole('button', { name: '确认关闭训练', exact: true }).click()
    await expect(drawer).toContainText('上游已确认关闭训练使用')
    await drawer.getByRole('button', { name: '查询重置券', exact: true }).click()
    await expect(drawer).toContainText('上游可用数量：1')
    await select('选择额度重置券', '合成可用券 · available', drawer)
    await drawer.getByRole('button', { name: '兑换所选重置券', exact: true }).click()
    await page.getByRole('button', { name: '确认兑换一次', exact: true }).click()
    await expect(drawer).toContainText('重置券可能已消费')
    await drawer.getByRole('button', { name: '兑换所选重置券', exact: true }).click()
    await page.getByRole('button', { name: '确认兑换一次', exact: true }).click()
    await expect.poll(() => calls.filter(call => call.path.endsWith('/reset-credit')).length).toBe(2)
    const attempts = calls.filter(call => call.path.endsWith('/reset-credit'))
    assert.equal(attempts[0].body.request_id, attempts[1].body.request_id)
    assert.equal(attempts[0].body.credit_id, 'credit-fixture-available')
    assertions.push('隐私写操作明确确认、只选真实可用券、兑换不可退回确认、未知结果提示与同 ID 去重重试')
    const automatic = drawer.locator('.auto-reset-panel')
    await expect(automatic).toContainText('已关闭')
    await automatic.getByRole('switch', { name: '自动消费额度重置券', exact: true }).locator('..').click()
    await automatic.getByRole('button', { name: '保存自动重置策略', exact: true }).click()
    await expect(automatic).toContainText('必须确认会自动消费')
    assert.equal(calls.filter(call => call.path.endsWith('/auto-reset') && call.method === 'PUT').length, 0)
    await tick(automatic.getByRole('checkbox', { name: '我确认此策略会自动消费上游有效重置券，消费后不能退回', exact: true }))
    await automatic.getByRole('button', { name: '保存自动重置策略', exact: true }).click()
    await expect(automatic).toContainText('结果未知，已暂停')
    await tick(automatic.getByRole('checkbox', { name: '我确认此策略会自动消费上游有效重置券，消费后不能退回', exact: true }))
    await automatic.getByRole('button', { name: '保存自动重置策略', exact: true }).click()
    await expect(automatic).toContainText('明确确认解除未知结果暂停')
    await tick(automatic.getByRole('checkbox', { name: '我已核对上游额度与重置券，确认解除未知结果暂停并重新启用', exact: true }))
    await automatic.getByRole('button', { name: '保存自动重置策略', exact: true }).click()
    await expect(automatic).toContainText('等待检查')
    const autoCall = calls.filter(call => call.path.endsWith('/auto-reset') && call.method === 'PUT').at(-1).body
    assert.equal(autoCall.account_version, 7); assert.equal(autoCall.version, 1); assert.equal(autoCall.acknowledge_unknown, true); assert.equal(autoCall.confirm_auto_consume, true)
    assertions.push('自动重置默认关闭、自动消费显式确认、双版本、unknown暂停且须核对确认方可恢复')
    await capture('account-controls-1440')
    await page.setViewportSize({ width: 320, height: 900 })
    assert.ok(await drawer.evaluate(element => element.scrollWidth <= element.clientWidth + 1))
    await capture('account-controls-320')
    await drawer.locator('.el-dialog__headerbtn').click()
    await details.locator('.el-dialog__headerbtn').click()
    assertions.push('320px 授权与检查居中弹窗不溢出、关闭返回原账号')
    return assertions
  } finally { await page.unroute('**/api/account**', handler); await page.setViewportSize({ width: 1440, height: 1000 }) }
}
