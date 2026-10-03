import assert from 'node:assert/strict'
import { expect } from 'playwright/test'

// Called by the isolated browser runner after mock login. Every override is removed
// before returning so the parent fixture remains the sole network boundary.
export async function testManagement(page) {
  const base = new URL(page.url()).origin
  assert.ok(['localhost', '127.0.0.1', '[::1]'].includes(new URL(base).hostname), 'Management checks require a local mock server')
  await page.locator('.page-header').waitFor()
  const handlers = []
  const assertions = []
  let group = { id: 'management-group', name: '管理测试分组', description: '隔离测试', version: 7, accounts: 3, keys: 2, created_at: 1700000000 }
  let proxy = { id: 'management-proxy', name: '管理测试代理', type: 'http', host: '127.0.0.1', port: 8899, username: 'test', has_password: true, password: 'legacy-mock-password', version: 11, enabled: true, accounts: 2 }
  const groupUpdates = []
  const proxyUpdates = []
  const rotationIDs = []
  const keyUpdates = []
  const settingsUpdates = []
  let setting = { admin_key: '', admin_key_masked: 'sk-••••1234', admin_api: false, version: '0.2.0-mock', settings_version: 3, uptime_seconds: 0 }
  let conflict = false
  let diagnosticIndex = 0
  let key = { id: 'management-key', name: '管理测试KEY', key_masked: 'sk-***mock', groups: [group.id], version: 21, enabled: true, requests: 0, tokens: 0, errors: 0, last_used_at: 0 }
  const generatedKey = 'sk-management-mock-new-key-only'
  const diagnosticCases = [
    { ok: true, http_code: 403, ip: '203.0.113.1', stage: 'http', error_code: 'target_http_status', http: 'failed' },
    { ok: true, http_code: 407, ip: '203.0.113.1', stage: 'authentication', error_code: 'proxy_authentication_failed', authentication: 'failed' },
    { ok: true, http_code: 200, ip: '', stage: 'exit_ip', error_code: 'invalid_exit_ip', http: 'ok', exit_ip: 'failed' },
    { ok: true, http_code: 200, ip: '2001:db8::1', stage: 'complete', connection: 'ok', authentication: 'ok', http: 'ok', exit_ip: 'ok', latency_ms: 25 },
  ]
  const json = (route, body, status = 200) => route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(body) })
  async function mock(pattern, handler) { handlers.push([pattern, handler]); await page.route(pattern, handler) }
  const titles = { groups: '分组管理', proxies: 'IP 代理池', keys: 'API KEY 管理', settings: '管理设置' }
  const go = async (path) => { await page.evaluate((value) => { window.location.hash = `/${value}` }, path); await page.getByRole('heading', { name: titles[path], exact: true }).waitFor() }
  const table = () => page.locator('.el-table').first()
  const editDialog = () => page.getByRole('dialog').filter({ has: page.getByRole('button', { name: '保存', exact: true }) }).last()
  const save = async () => {
    // Same-event-loop double click verifies the handler guard, not just the spinner.
    await editDialog().getByRole('button', { name: '保存', exact: true }).evaluate((button) => { button.click(); button.click() })
    await editDialog().waitFor({ state: 'hidden' })
  }
  const confirmRotation = async (label) => {
    await page.getByRole('button', { name: label, exact: true }).click()
    await page.locator('.el-message-box').getByRole('button', { name: '重新生成', exact: true }).click()
  }
  try {
    await mock('**/api/groups', (route) => json(route, { groups: [group] }))
    await mock('**/api/groups/management-group', async (route) => {
      const body = route.request().postDataJSON()
      groupUpdates.push(body)
      if (conflict) return json(route, { error: { code: 'version_conflict', message: '模拟版本冲突' } }, 409)
      await new Promise((resolve) => setTimeout(resolve, 70))
      group = { ...group, ...body, version: group.version + 1 }
      return json(route, group)
    })
    await mock('**/api/proxies', (route) => json(route, { proxies: [proxy] }))
    await mock('**/api/proxies/management-proxy', async (route) => {
      const body = route.request().postDataJSON()
      proxyUpdates.push(body)
      await new Promise((resolve) => setTimeout(resolve, 70))
      proxy = { ...proxy, ...body, version: proxy.version + 1 }
      return json(route, { ...proxy, password: undefined })
    })
    await mock('**/api/proxies/test', (route) => json(route, { checked_at: 1700000000, connection: 'ok', authentication: 'not_checked', http: 'not_checked', exit_ip: 'not_checked', ...diagnosticCases[diagnosticIndex++] }))
    await mock('**/api/settings', async (route) => {
      if (route.request().method() === 'GET') return json(route, setting)
      const body = route.request().postDataJSON()
      settingsUpdates.push(body)
      if (body.settings_version !== setting.settings_version) return json(route, { error: { code: 'version_conflict', message: '模拟设置版本冲突' } }, 409)
      await new Promise((resolve) => setTimeout(resolve, 70))
      setting = { ...setting, admin_api: body.admin_api, settings_version: setting.settings_version + 1 }
      return json(route, { ok: true, settings_version: setting.settings_version })
    })
    await mock('**/api/settings/rotate-key', (route) => {
      rotationIDs.push(route.request().postDataJSON().request_id)
      return rotationIDs.length === 1
        ? json(route, { error: { message: '模拟响应丢失，请重试' } }, 500)
        : json(route, { ok: true, admin_key: generatedKey, request_id: rotationIDs.at(-1) })
    })
    await mock('**/api/keys', (route) => json(route, { keys: [key] }))
    await mock('**/api/keys/management-key', async (route) => {
      const body = route.request().postDataJSON()
      keyUpdates.push(body)
      if (body.version !== key.version) return json(route, { error: { code: 'version_conflict', message: '模拟密钥版本冲突' } }, 409)
      await new Promise((resolve) => setTimeout(resolve, 70))
      key = { ...key, ...body, version: key.version + 1 }
      return json(route, key)
    })

    await go('groups')
    await table().getByRole('button', { name: '编辑', exact: true }).click()
    await editDialog().getByPlaceholder('例如：Plus 账号').fill('管理测试分组更新')
    await save()
    assert.equal(groupUpdates.length, 1)
    assert.equal(groupUpdates[0].version, 7)
    assertions.push('分组双击保存只提交一次，并携带读取的 version')
    await page.getByRole('textbox', { name: '搜索分组' }).fill('管理测试')
    await go('proxies')
    await go('groups')
    assert.equal(await page.getByRole('textbox', { name: '搜索分组' }).inputValue(), '管理测试')
    conflict = true
    await table().getByRole('button', { name: '编辑', exact: true }).click()
    await editDialog().getByRole('button', { name: '保存', exact: true }).click()
    await editDialog().waitFor({ state: 'hidden' })
    await page.getByText('数据已被修改，列表已刷新。请重新打开编辑核对最新内容。', { exact: true }).first().waitFor()
    assert.equal(await page.getByRole('dialog').count(), 0)
    assertions.push('分组筛选跨页签保留；版本冲突刷新列表且不自动覆盖')

    await go('proxies')
    await table().getByRole('button', { name: '编辑', exact: true }).click()
    assert.equal(await editDialog().locator('input[type="password"]').inputValue(), '')
    await save()
    assert.equal(proxyUpdates.length, 1)
    assert.equal(proxyUpdates[0].version, 11)
    assert.equal(Object.hasOwn(proxyUpdates[0], 'password'), false)
    assert.equal(Object.hasOwn(proxyUpdates[0], 'clear_password'), false)
    await table().getByRole('button', { name: '编辑', exact: true }).click()
    await editDialog().getByText('明确清除已保存的密码', { exact: true }).click()
    assert.equal(await editDialog().getByRole('checkbox', { name: '明确清除已保存的密码' }).isChecked(), true)
    await save()
    assert.equal(proxyUpdates[1].clear_password, true)
    assert.equal(proxyUpdates[1].version, 12)
    assertions.push('代理旧密码不回显；留空保持，显式清除发送 clear_password，编辑携带 version')
    for (let index = 0; index < diagnosticCases.length; index++) {
      await table().getByRole('button', { name: '测试', exact: true }).click()
      await page.locator('.management-error').filter({ hasText: index === 3 ? '检测通过：出口 IP 2001:db8::1' : '检测未通过（HTTP' }).waitFor()
      await page.waitForFunction(() => !document.querySelector('.el-table .el-button.is-loading'))
    }
    assertions.push('代理 403、407、未知 IP 不误报成功；2xx + IPv6 通过并展示诊断阶段')

    await go('keys')
    await table().locator('.el-switch').evaluate((button) => { button.click(); button.click() })
    await page.waitForFunction(() => document.querySelector('.el-table [role="switch"]')?.getAttribute('aria-checked') === 'false')
    assert.deepEqual(keyUpdates, [{ enabled: false, version: 21 }])
    await table().getByRole('button', { name: '编辑', exact: true }).click()
    await editDialog().getByPlaceholder('例如：对外服务 A').fill('管理测试KEY更新')
    await save()
    assert.equal(keyUpdates.length, 2)
    assert.equal(keyUpdates[1].version, 22)
    key = { ...key, version: 26, name: '服务端最新KEY' }
    await table().getByRole('button', { name: '编辑', exact: true }).click()
    await save()
    await page.getByText('密钥已被修改，列表已刷新。请重新打开编辑核对最新内容。', { exact: true }).first().waitFor()
    await table().getByText('服务端最新KEY', { exact: true }).waitFor()
    assert.equal(keyUpdates.length, 3)
    assert.equal(keyUpdates[2].version, 23)
    await table().locator('.el-switch').click()
    await page.waitForFunction(() => document.querySelector('.el-table [role="switch"]')?.getAttribute('aria-checked') === 'true')
    assert.equal(keyUpdates[3].version, 26)
    key = { ...key, version: 30, enabled: false }
    await table().locator('.el-switch').click()
    await page.getByText('密钥已被修改，列表已刷新。请核对最新状态后重新操作。', { exact: true }).first().waitFor()
    assert.equal(keyUpdates.length, 5)
    assert.equal(keyUpdates[4].version, 27)
    assert.equal(await table().getByRole('switch').getAttribute('aria-checked'), 'false')
    assertions.push('密钥启停/编辑携带连续 version、双击防重；两种版本冲突均重读且不自动覆盖')

    await go('settings')
    await page.getByText('0.2.0-mock', { exact: true }).last().waitFor()
    await page.getByText('0 分钟', { exact: true }).waitFor()
    assert.equal(await page.getByRole('button', { name: '复制', exact: true }).isDisabled(), true)
    const settingsSwitch = page.locator('.el-switch').filter({ has: page.getByRole('switch', { name: '允许 admin key 调用网关' }) })
    await settingsSwitch.evaluate((button) => { button.click(); button.click() })
    await page.waitForFunction(() => document.querySelector('.settings-row-control [role="switch"]')?.getAttribute('aria-checked') === 'true')
    assert.deepEqual(settingsUpdates, [{ admin_api: true, settings_version: 3 }])
    await settingsSwitch.click()
    await page.waitForFunction(() => document.querySelector('.settings-row-control [role="switch"]')?.getAttribute('aria-checked') === 'false')
    assert.deepEqual(settingsUpdates[1], { admin_api: false, settings_version: 4 })
    setting = { ...setting, admin_api: true, settings_version: 8 }
    await settingsSwitch.click()
    await page.getByText('设置已被修改，已读取最新设置。请核对后重新操作。', { exact: true }).waitFor()
    assert.equal(settingsUpdates.length, 3)
    assert.equal(settingsUpdates[2].settings_version, 5)
    assert.equal(await settingsSwitch.getByRole('switch').getAttribute('aria-checked'), 'true')
    await settingsSwitch.click()
    await page.waitForFunction(() => document.querySelector('.settings-row-control [role="switch"]')?.getAttribute('aria-checked') === 'false')
    assert.deepEqual(settingsUpdates[3], { admin_api: false, settings_version: 8 })
    assert.equal(setting.version, '0.2.0-mock')
    assertions.push('设置提交独立 settings_version 并连续更新、双击只提交一次；409重读最新版本且不自动覆盖')
    await confirmRotation('重新生成')
    await expect(page.getByRole('button', { name: '重试轮换结果', exact: true })).toBeEnabled()
    await go('proxies')
    await go('settings')
    await confirmRotation('重试轮换结果')
    await page.getByText(generatedKey, { exact: true }).waitFor()
    assert.equal(rotationIDs.length, 2)
    assert.ok(rotationIDs[0])
    assert.equal(rotationIDs[0], rotationIDs[1])
    assert.equal(await page.evaluate(() => localStorage.getItem('adminKey')), null)
    assertions.push('设置显示后端版本/零运行时长；轮换失败复用 request_id，密钥不持久化')
    await page.evaluate(() => {
      window.__managementClipboardDescriptor = Object.getOwnPropertyDescriptor(navigator, 'clipboard')
      Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText: async () => { throw new Error('Mock clipboard denied') } } })
    })
    await page.getByRole('button', { name: '复制', exact: true }).click()
    const fallback = page.getByRole('dialog', { name: '手动复制' })
    const text = fallback.getByRole('textbox', { name: '待复制的密钥' })
    assert.equal(await text.inputValue(), generatedKey)
    await page.waitForFunction(() => {
      const input = document.querySelector('textarea[aria-label="待复制的密钥"]')
      return input && document.activeElement === input && input.selectionEnd === input.value.length && input.selectionStart === 0
    })
    assert.equal((await page.locator('.el-message').allTextContents()).some((value) => value.includes(generatedKey)), false)
    await fallback.getByRole('button', { name: '完成', exact: true }).click()
    await fallback.waitFor({ state: 'hidden' })
    assert.equal(await page.locator('textarea[aria-label="待复制的密钥"]').inputValue(), '')
    await page.waitForFunction(() => document.activeElement?.textContent.trim() === '复制')
    assertions.push('复制失败使用自动选中文本弹窗；toast 不含密钥，关闭清空回退文本并恢复焦点')
    return assertions
  } finally {
    await page.evaluate(() => {
      if (Object.hasOwn(window, '__managementClipboardDescriptor')) {
        if (window.__managementClipboardDescriptor) Object.defineProperty(navigator, 'clipboard', window.__managementClipboardDescriptor)
        else delete navigator.clipboard
        delete window.__managementClipboardDescriptor
      }
    })
    for (const [pattern, handler] of handlers) await page.unroute(pattern, handler)
  }
}
