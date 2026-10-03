import assert from 'node:assert/strict'
import { expect } from 'playwright/test'

// Synthetic settings and credentials for the current PowerPoint gateway settings UI.
export const settingsRestoredFixture = {
  inference_mode: 'powerpoint_bps',
  inference_modes: [{ value: 'powerpoint_bps', label: 'PowerPoint + Codex' }, { value: 'official_codex_cli', label: 'Codex 直连' }],
  inference: { backend: 'powerpoint_bps', profile: 'powerpoint', base_url: 'https://bps.openai.com/basispoints/api', tools_version: 'tools-fixture', web_search: 'enabled', codex_fallback: false },
  admin_key: '', admin_key_masked: 'sk-••••1234', admin_api: false,
  websocket_enabled: false, builtin_prompt_enabled: true, builtin_prompt: 'Stable instruction.',
  sticky_session_ttl_seconds: 604800,
  prompt_cache_key_enabled: true, bps_prompt_cache_scope: 'client',
  cache_creation_as_read_enabled: true,
  http_stream_enabled: true, http_json_enabled: true, compact_enabled: true, models_enabled: true,
  compatibility_normalization_enabled: true, automatic_token_refresh_enabled: true, upstream_401_recovery_enabled: true,
  rate_limit_cooldown_enabled: false,
  bps_model_routes: 'gpt-5.5=gpt-5.6-sol\ncodex-auto=gpt-6-astra\n*=gpt-6-astra',
  bps_model_route_targets: ['gpt-5.6-luna', 'gpt-6-luna', 'gpt-5.6-terra', 'gpt-5.6-sol', 'gpt-6-sol', 'gpt-6-astra'],
  builtin_prompt_mode: 'prepend', upstream_stream_idle_timeout_seconds: 300,
  upstream_response_header_timeout_seconds: 90, upstream_compact_timeout_seconds: 1200,
  settings_version: 1, version: '0.2.0-settings-test', uptime_seconds: 3660,
}

export async function testSettingsRestored(page) {
  const assertions = []
  const updates = []
  const rotations = []
  let settings = structuredClone(settingsRestoredFixture)
  let readFailure = true
  let writeFailure = false
  let malformedWrite = false
  let rotateFailure = false
  let readGate
  const settingsHandler = async route => {
    if (route.request().method() === 'GET') {
      if (readGate) await readGate
      if (readFailure) return route.fulfill({ status: 503, json: { error: { message: '读取设置暂时失败' } } })
      return route.fulfill({ json: settings })
    }
    const body = route.request().postDataJSON()
    updates.push(body)
    if (writeFailure) return route.fulfill({ status: 500, json: { error: { message: '模拟保存失败' } } })
    if (body.settings_version !== settings.settings_version) return route.fulfill({ status: 409, json: { error: { message: '模拟设置版本冲突' } } })
    await new Promise(resolve => setTimeout(resolve, 80))
    settings = { ...settings, ...body, settings_version: settings.settings_version + 1 }
    if (body.inference_mode) {
      settings.inference = body.inference_mode === 'powerpoint_bps' ? structuredClone(settingsRestoredFixture.inference) : { backend: body.inference_mode, profile: body.inference_mode === 'official_codex_cli' ? 'codex_cli' : 'app_server', base_url: 'https://chatgpt.com/backend-api/codex', web_search: 'enabled', codex_fallback: false }
    }
    return route.fulfill({ json: malformedWrite ? { ok: true } : { ok: true, settings_version: settings.settings_version } })
  }
  const rotationHandler = async route => {
    rotations.push(route.request().postDataJSON())
    if (rotateFailure) return route.fulfill({ status: 500, json: { error: { message: '模拟轮换结果未确认' } } })
    await new Promise(resolve => setTimeout(resolve, 80))
    return route.fulfill({ json: { admin_key: 'synthetic-rotated-admin-key-never-real', request_id: rotations.at(-1).request_id } })
  }
  await page.route('**/api/settings', settingsHandler)
  await page.route('**/api/settings/rotate-key', rotationHandler)
  try {
    const origin = new URL(page.url()).origin
    await page.goto(origin + '/#/settings')
    await page.reload()
    const refresh = page.getByRole('button', { name: '刷新系统设置' })
    const ws = page.getByRole('switch', { name: '启用 WebSocket', exact: true })
    const prompt = page.getByRole('textbox', { name: '内置提示词内容' })
    const savePrompt = page.getByRole('button', { name: '保存提示词', exact: true })
    const ttl = page.getByRole('spinbutton', { name: '会话绑定有效期（秒）' })
    const saveTTL = page.getByRole('button', { name: '保存有效期', exact: true })
    const error = page.locator('.management-error')
    await expect(error).toContainText('读取设置暂时失败')
    await expect(ws).toBeDisabled()
    await expect(prompt).toBeDisabled()
    await expect(page.getByRole('button', { name: '重新生成', exact: true })).toBeDisabled()
    readFailure = false
    let releaseRead
    readGate = new Promise(resolve => { releaseRead = resolve })
    await refresh.click()
    await expect(ws).toBeDisabled()
    await expect(refresh).toBeDisabled()
    releaseRead()
    readGate = undefined
    await expect(ws).toBeEnabled()
    await expect(prompt).toHaveValue('Stable instruction.')
    assertions.push('首次读取失败及读取中禁止编辑和轮换密钥，刷新成功才启用')

    assert.equal(await page.locator('.settings-navigation').count(), 0)
    assert.equal(await page.getByRole('switch').count(), 12)
    await expect(page.getByRole('switch', { name: '启用限流冷却', exact: true })).not.toBeChecked()
    await expect(page.getByRole('switch', { name: '缓存创建按读取返回', exact: true })).toBeChecked()
    await expect(page.getByRole('region', { name: '会话与缓存设置', exact: true })).toContainText('对外返回的缓存读取量包含创建量，便于下游按读取价格计费；本系统保留原始读写用量。')
    for (const name of ['密钥管理设置', '网关传输设置', '请求兼容与账号恢复设置', '内置提示词设置', '会话与缓存设置', '上游超时设置', '系统信息']) await expect(page.getByRole('region', { name, exact: true })).toBeVisible()
    await expect(page.getByRole('region', { name: '推理服务配置' })).toBeVisible()
    await expect(page.getByRole('combobox', { name: 'Codex CLI 指纹配置' })).toHaveCount(0)
    await expect(page.getByLabel('会话固定使用同一账号')).toBeVisible()
    await expect(page.getByText('0.2.0-settings-test', { exact: true })).toBeVisible()
    await expect(page.getByText('1 小时 1 分', { exact: true })).toBeVisible()
    await expect(page.getByRole('button', { name: '显示', exact: true })).toBeDisabled()
    await expect(page.getByRole('button', { name: '复制', exact: true })).toBeDisabled()
    assertions.push('管理设置展示实际 PowerPoint 服务，移除 CLI 指纹与缓存键开关；会话绑定固定开启')

    writeFailure = true
    await ws.locator('..').click()
    await expect(error).toContainText('模拟保存失败')
    await expect(ws).not.toBeChecked()
    assert.equal(await page.locator('.el-message--success').count(), 0)
    writeFailure = false
    await ws.locator('..').evaluate(button => { button.click(); button.click() })
    await expect(ws).toBeChecked()
    await expect(ws).toBeEnabled()
    assert.equal(updates.length, 2)
    assert.deepEqual(updates[1], { websocket_enabled: true, settings_version: 1 })
    assertions.push('开关失败回滚且不报成功；双击仅提交一次并携带版本')

    const toggles = [
      ['允许 admin key 调用网关', 'admin_api'], ['启用内置提示词', 'builtin_prompt_enabled'],
      ['启用 HTTP 流式响应', 'http_stream_enabled'], ['启用 HTTP 非流式响应', 'http_json_enabled'],
      ['启用上下文压缩', 'compact_enabled'], ['启用模型列表', 'models_enabled'],
      ['启用客户端参数兼容', 'compatibility_normalization_enabled'],
      ['启用临期令牌自动刷新', 'automatic_token_refresh_enabled'], ['启用上游 401 恢复', 'upstream_401_recovery_enabled'],
      ['启用限流冷却', 'rate_limit_cooldown_enabled'],
      ['缓存创建按读取返回', 'cache_creation_as_read_enabled'],
    ]
    for (const [name, field] of toggles) {
      const toggle = page.getByRole('switch', { name, exact: true })
      const version = settings.settings_version
      const value = !settings[field]
      await toggle.locator('..').click()
      await expect(toggle).toBeChecked({ checked: value })
      await expect(toggle).toBeEnabled()
      assert.deepEqual(updates.at(-1), { [field]: value, settings_version: version })
    }
    assertions.push('12 个实际生效开关独立保存并携带版本，限流冷却默认关闭、缓存创建按读取返回默认开启，不覆盖其他配置')

    const creationAsRead = page.getByRole('switch', { name: '缓存创建按读取返回', exact: true })
    await expect(creationAsRead).not.toBeChecked()
    writeFailure = true
    await creationAsRead.locator('..').click()
    await expect(error).toContainText('模拟保存失败')
    await expect(creationAsRead).not.toBeChecked()
    writeFailure = false
    await refresh.click()
    await expect(creationAsRead).toBeEnabled()
    await expect(creationAsRead).not.toBeChecked()
    const creationVersion = settings.settings_version
    await creationAsRead.locator('..').click()
    await expect(creationAsRead).toBeChecked()
    await expect(creationAsRead).toBeEnabled()
    assert.deepEqual(updates.at(-1), { cache_creation_as_read_enabled: true, settings_version: creationVersion })
    assertions.push('缓存创建按读取返回显式关闭重读后保持，保存失败不误改，重新开启带正确版本')

    const mode = page.getByRole('combobox', { name: '提示词注入方式', exact: true })
    for (const [select, field, value, original] of [[mode, 'builtin_prompt_mode', 'append', 'prepend']]) {
      writeFailure = true
      await select.selectOption(value)
      await expect(error).toContainText('模拟保存失败')
      await expect(select).toBeEnabled()
      await expect(select).toHaveValue(original)
      writeFailure = false
      const version = settings.settings_version
      await select.selectOption(value)
      await expect(select).toHaveValue(value)
      await expect(select).toBeEnabled()
      assert.deepEqual(updates.at(-1), { [field]: value, settings_version: version })
    }
    for (const value of ['if_empty', 'prepend']) {
      const version = settings.settings_version
      await mode.selectOption(value)
      await expect(mode).toHaveValue(value)
      await expect(mode).toBeEnabled()
      assert.deepEqual(updates.at(-1), { builtin_prompt_mode: value, settings_version: version })
    }
    assertions.push('三种提示词模式均可保存；失败恢复已保存选项')

    const cacheScope = page.getByRole('combobox', { name: '缓存复用范围', exact: true })
    writeFailure = true
    await cacheScope.selectOption('api_key')
    await expect(error).toContainText('模拟保存失败')
    await expect(cacheScope).toBeEnabled()
    await expect(cacheScope).toHaveValue('client')
    writeFailure = false
    const cacheVersion = settings.settings_version
    await cacheScope.selectOption('api_key')
    await expect(cacheScope).toHaveValue('api_key')
    await expect(cacheScope).toBeEnabled()
    assert.deepEqual(updates.at(-1), { bps_prompt_cache_scope: 'api_key', settings_version: cacheVersion })
    await refresh.click()
    await expect(cacheScope).toBeEnabled()
    await expect(cacheScope).toHaveValue('api_key')
    assertions.push('缓存范围保存携带版本并在重读后保持；失败恢复客户端分组选项')

    const routes = page.getByRole('textbox', { name: '固定模型映射规则', exact: true })
    const saveRoutes = page.getByRole('button', { name: '保存模型映射', exact: true })
    await routes.fill('broken=gpt-unknown')
    await expect(saveRoutes).toBeDisabled()
    const routeDraft = 'gpt-5.5=gpt-6-astra\ncodex-auto=gpt-6-astra\n*=gpt-5.6-sol'
    await routes.fill(routeDraft)
    writeFailure = true
    await saveRoutes.click()
    await expect(error).toContainText('模拟保存失败')
    await expect(routes).toHaveValue(routeDraft)
    await expect(saveRoutes).toBeEnabled()
    writeFailure = false
    const routeVersion = settings.settings_version
    await saveRoutes.click()
    await expect(saveRoutes).toBeDisabled()
    assert.deepEqual(updates.at(-1), { bps_model_routes: routeDraft, settings_version: routeVersion })
    await refresh.click()
    await expect(routes).toBeEnabled()
    await expect(routes).toHaveValue(routeDraft)
    assertions.push('固定模型映射校验目标与兜底；保存失败保留草稿，成功携带版本并重读保持')

    const beforeDraft = updates.length
    await prompt.fill('中'.repeat(10923))
    await expect(page.getByText('32,769 / 32,768 字节（UTF-8），内容超出上限', { exact: true })).toBeVisible()
    await expect(savePrompt).toBeDisabled()
    assert.equal(updates.length, beforeDraft)
    await prompt.fill('Keep this unsaved draft. 中文')
    settings = { ...settings, builtin_prompt: 'Changed in another session.', settings_version: settings.settings_version + 5 }
    await savePrompt.click()
    await expect(error).toContainText('已读取最新设置')
    await expect(error).toContainText('未保存的提示词、会话有效期及超时草稿已保留')
    await expect(prompt).toHaveValue('Keep this unsaved draft. 中文')
    assert.equal(updates.length, beforeDraft + 1)
    let version = settings.settings_version
    await savePrompt.click()
    await expect(prompt).toBeEnabled()
    await expect(savePrompt).toBeDisabled()
    assert.deepEqual(updates.at(-1), { builtin_prompt: 'Keep this unsaved draft. 中文', settings_version: version })
    await prompt.fill('')
    await savePrompt.click()
    await expect(prompt).toBeEnabled()
    await expect(savePrompt).toBeDisabled()
    assert.equal(settings.builtin_prompt, '')
    assertions.push('提示词按 UTF-8 字节限制、可清空；409 不自动重试，保留草稿并使用新版本重存')

    const numbers = [
      ['会话绑定有效期（秒）', '保存有效期', 'sticky_session_ttl_seconds', 60, 2592000],
      ['流式读取空闲超时（秒）', '保存空闲超时', 'upstream_stream_idle_timeout_seconds', 10, 3600],
      ['响应头等待超时（秒）', '保存响应头超时', 'upstream_response_header_timeout_seconds', 5, 600],
      ['上下文压缩超时（秒）', '保存压缩超时', 'upstream_compact_timeout_seconds', 30, 3600],
    ]
    for (const [name, saveLabel, field, min, max] of numbers) {
      const input = page.getByRole('spinbutton', { name, exact: true })
      const save = page.getByRole('button', { name: saveLabel, exact: true })
      const before = updates.length
      for (const invalid of ['', String(min - 1), String(max + 1), `${min}.5`]) {
        await input.fill(invalid)
        await input.blur()
        await expect(save).toBeDisabled()
        assert.equal(updates.length, before)
      }
      for (const value of [min, max]) {
        await input.fill(String(value))
        await input.blur()
        const beforeSave = updates.length
        version = settings.settings_version
        assert.equal(settings[field] === value, false)
        await save.click()
        await expect(input).toBeEnabled()
        await expect(save).toBeDisabled()
        assert.equal(updates.length, beforeSave + 1)
        assert.deepEqual(updates.at(-1), { [field]: value, settings_version: version })
      }
    }
    assertions.push('TTL 与 3 个超时只显式保存：空值、越界、小数均禁止，最小/最大边界均能逐字段保存')

    await prompt.fill('All drafts survive reload and conflict.')
    for (const [name, , , min] of numbers) await page.getByRole('spinbutton', { name, exact: true }).fill(String(min + 2))
    const beforeRefresh = updates.length
    await refresh.click()
    await expect(prompt).toBeEnabled()
    await expect(prompt).toHaveValue('All drafts survive reload and conflict.')
    for (const [name, , , min] of numbers) await expect(page.getByRole('spinbutton', { name, exact: true })).toHaveValue(String(min + 2))
    assert.equal(updates.length, beforeRefresh)
    settings = { ...settings, settings_version: settings.settings_version + 3 }
    await saveTTL.click()
    await expect(error).toContainText('已读取最新设置')
    for (const [name, , , min] of numbers) await expect(page.getByRole('spinbutton', { name, exact: true })).toHaveValue(String(min + 2))
    await expect(prompt).toHaveValue('All drafts survive reload and conflict.')
    assert.equal(updates.length, beforeRefresh + 1)
    for (const [name, saveLabel, field, min] of numbers) {
      version = settings.settings_version
      await page.getByRole('button', { name: saveLabel, exact: true }).click()
      await expect(page.getByRole('spinbutton', { name, exact: true })).toBeEnabled()
      assert.deepEqual(updates.at(-1), { [field]: min + 2, settings_version: version })
    }
    assertions.push('刷新及数值保存 409 保留全部 5 项文本/数值草稿；重新保存使用最新版本')

    // Wait for prior success notifications to expire instead of mutating Vue-owned DOM.
    await expect(page.locator('.el-message--success')).toHaveCount(0, { timeout: 5000 })
    malformedWrite = true
    await ws.locator('..').click()
    await expect(error).toContainText('服务端未确认保存结果')
    await expect(ws).not.toBeChecked()
    assert.equal(await page.locator('.el-message--success').count(), 0)
    malformedWrite = false
    readFailure = true
    await refresh.click()
    await expect(error).toContainText('读取设置暂时失败')
    await expect(ws).toBeDisabled()
    readFailure = false
    await refresh.click()
    await expect(prompt).toBeEnabled()
    await expect(prompt).toHaveValue('All drafts survive reload and conflict.')
    assertions.push('缺少新版本不假报成功；刷新失败锁定编辑，再刷新仍保留草稿')

    const regenerate = page.getByRole('button', { name: '重新生成', exact: true })
    await regenerate.click()
    const confirmation = page.getByRole('dialog', { name: '重新生成 admin key', exact: true })
    await expect(ws).toBeDisabled()
    await confirmation.getByRole('button', { name: '取消', exact: true }).click()
    await expect(confirmation).toBeHidden()
    await expect(ws).toBeEnabled()
    assert.equal(rotations.length, 0)
    // Plain HTTP production origins do not expose randomUUID, unlike localhost.
    await page.evaluate(() => Object.defineProperty(crypto, 'randomUUID', { configurable: true, value: undefined }))
    rotateFailure = true
    await regenerate.click()
    await confirmation.getByRole('button', { name: '重新生成', exact: true }).click()
    await expect(error).toContainText('模拟轮换结果未确认')
    assert.equal(rotations.length, 1)
    assert.match(rotations[0].request_id, /^[0-9a-f-]{36}$/)
    rotateFailure = false
    await page.getByRole('button', { name: '重试轮换结果', exact: true }).click()
    await confirmation.getByRole('button', { name: '重新生成', exact: true }).click()
    await expect(page.locator('.settings-key')).toHaveText('synthetic-rotated-admin-key-never-real')
    assert.equal(rotations.length, 2)
    assert.deepEqual(rotations[1], rotations[0])
    await expect(ws).toBeEnabled()
    await page.getByRole('button', { name: '隐藏', exact: true }).click()
    await expect(page.locator('.settings-key')).toHaveText('••••••••••••••••')
    await page.getByRole('button', { name: '显示', exact: true }).click()
    await page.evaluate(() => Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText: async value => { window.__syntheticCopiedKey = value } } }))
    await page.getByRole('button', { name: '复制', exact: true }).click()
    assert.equal(await page.evaluate(() => window.__syntheticCopiedKey), 'synthetic-rotated-admin-key-never-real')
    await page.evaluate(() => Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText: async () => { throw new Error('synthetic clipboard denied') } } }))
    await page.getByRole('button', { name: '复制', exact: true }).click()
    const manualCopy = page.getByRole('dialog', { name: '手动复制', exact: true })
    await expect(manualCopy.getByRole('textbox', { name: '待复制的密钥' })).toHaveValue('synthetic-rotated-admin-key-never-real')
    await manualCopy.getByRole('button', { name: '完成', exact: true }).click()
    await expect(manualCopy).not.toBeVisible()
    assert.equal(await page.evaluate(() => JSON.stringify({ ...localStorage, ...sessionStorage }).includes('synthetic-rotated-admin-key-never-real')), false)
    await refresh.click()
    await expect(ws).toBeEnabled()
    await expect(page.locator('.settings-key')).toHaveText('sk-••••1234')
    await expect(page.getByRole('button', { name: '复制', exact: true })).toBeDisabled()
    assertions.push('密钥轮换需确认且取消不提交；普通 HTTP 缺少 randomUUID 时仍可轮换，结果不确定复用同一操作 ID；新密钥支持隐藏/复制/手动复制且不落浏览器存储')

    const runtime = page.getByRole('region', { name: '推理服务配置' })
    for (const [mode, label] of [['official_codex_cli', 'Codex 直连'], ['powerpoint_bps', 'PowerPoint + Codex']]) {
      await runtime.locator('.el-select__wrapper').click()
      await page.getByRole('option', { name: label, exact: true }).click()
      await expect(runtime.locator('.service-heading')).toContainText(label)
      await expect.poll(() => settings.inference_mode).toBe(mode)
      assert.equal(updates.at(-1).inference_mode, mode)
      await page.reload()
      await expect(runtime.locator('.service-heading')).toContainText(label)
    }
    assertions.push('三种运行模式保存携带版本，重读实际推理信息且刷新后保持选择')

    const fields = [...new Set(updates.flatMap(update => Object.keys(update).filter(field => field !== 'settings_version')))].sort()
    assert.equal(fields.length, 21)
    for (const retired of ['codex_fingerprint_enabled', 'codex_fingerprint_profile', 'sticky_sessions_enabled', 'prompt_cache_key_enabled']) assert.equal(fields.includes(retired), false)
    return { assertions, setting_fields_verified: fields, field_count: fields.length, switch_count: 12, setting_writes: updates.length, synthetic_key_rotation_requests: rotations.length }
  } finally {
    await page.unroute('**/api/settings', settingsHandler)
    await page.unroute('**/api/settings/rotate-key', rotationHandler)
  }
}
