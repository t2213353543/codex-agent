import assert from 'node:assert/strict'
import { expect } from 'playwright/test'
import { mockKey } from './mock-api.mjs'

export async function testLogDownloadSession(page, state) {
  assert.ok(['localhost', '127.0.0.1'].includes(new URL(page.url()).hostname))
  let release, mode = 'stale'
  const handler = async route => {
    if (mode === 'stale') await new Promise(resolve => { release = resolve })
    try { await route.fulfill({ status: 401, json: { error: { message: '会话过期测试' } } }) } catch { /* An aborted request must not affect the new session. */ }
  }
  await page.route('**/api/logs/export?**', handler)
  const assertions = []
  try {
    await page.setViewportSize({ width: 1440, height: 1100 })
    await page.getByRole('button', { name: '导出全部筛选', exact: true }).click()
    const dialog = page.getByRole('dialog', { name: '导出全部筛选结果' })
    await dialog.getByRole('button', { name: '导出全部记录', exact: true }).click()
    await expect.poll(() => typeof release).toBe('function')
    const loginCount = state.loginCount
    const generations = await page.evaluate(async key => {
      const module = await import(document.querySelector('script[type="module"][src]').src)
      // Resolve the function actually shipped in this build; chunk aliases change
      // with the module graph and are not a public authentication contract.
      const login = Object.values(module).filter(value => typeof value === 'function' && value.toString().includes('服务器未确认登录'))
      if (login.length !== 1) throw new Error('Current runtime loginSession binding is not uniquely identifiable')
      const numbers = Object.fromEntries(Object.entries(module).filter(([, value]) => typeof value === 'number'))
      await login[0](key)
      const changed = Object.entries(numbers).filter(([name, value]) => module[name] === value + 1)
      if (changed.length !== 1) throw new Error('Current runtime authentication generation did not increment exactly once')
      const [name, before] = changed[0]
      return { name, before, after: module[name] }
    }, mockKey)
    assert.ok(generations.after > generations.before)
    assert.equal(state.loginCount, loginCount + 1)
    release()
    await expect(dialog).toContainText('认证失效，请重新登录后导出')
    assert.ok(new URL(page.url()).hash.startsWith('#/logs'))
    assert.equal(await page.evaluate(async name => (await import(document.querySelector('script[type="module"][src]').src))[name], generations.name), generations.after)
    await dialog.getByRole('button', { name: '取消', exact: true }).click()
    assertions.push('当前 runtime 真实 loginSession 完成登录并更新 generation 后，迟到的下载 401 不清除新会话或跳转登录')

    mode = 'current'
    state.authenticated = false
    await page.getByRole('button', { name: '导出全部筛选', exact: true }).click()
    await dialog.getByRole('button', { name: '导出全部记录', exact: true }).click()
    await expect(page.getByRole('textbox', { name: '管理密钥', exact: true })).toBeVisible()
    assert.ok(new URL(page.url()).hash.startsWith('#/login'))
    await page.waitForTimeout(200)
    assert.ok(new URL(page.url()).hash.startsWith('#/login'))
    await page.getByRole('textbox', { name: '管理密钥', exact: true }).fill(mockKey)
    await page.getByRole('button', { name: '登 录', exact: true }).click()
    await expect(page.getByRole('textbox', { name: '管理密钥', exact: true })).toHaveCount(0)
    assert.equal(state.authenticated, true)
    assertions.push('当前下载 401 清除当前会话状态并留在真实登录页，重新登录后可恢复管理页面')
    return { assertions }
  } finally { release?.(); await page.unroute('**/api/logs/export?**', handler) }
}
