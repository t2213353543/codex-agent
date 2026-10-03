import assert from 'node:assert/strict'
import { createServer } from 'node:http'
import { readFile, mkdir, writeFile } from 'node:fs/promises'
import { fileURLToPath } from 'node:url'
import path from 'node:path'
import { chromium, expect } from 'playwright/test'
import { installMockApi } from './mock-api.mjs'
import { managementPages } from '../src/utils/navigation.js'

// Exercise production CSS on real teleported components, with synthetic data.
// No request may escape this local static server and the mock API boundary.
const root = process.env.MANAGEMENT_UI_DIST ? path.resolve(process.env.MANAGEMENT_UI_DIST) : fileURLToPath(new URL('../dist/', import.meta.url))
const output = process.env.MODALS_BROWSER_OUTPUT ? path.resolve(process.env.MODALS_BROWSER_OUTPUT) : fileURLToPath(new URL('../test-results/modals-browser/', import.meta.url))
await mkdir(output, { recursive: true })
const errors = [], screenshots = [], checks = [], leakedApi = []
const server = createServer(async (request, response) => {
  const pathname = new URL(request.url, 'http://localhost').pathname
  if (pathname.startsWith('/api/')) { leakedApi.push(pathname); response.writeHead(500).end(); return }
  try {
    const filename = pathname === '/' ? 'index.html' : decodeURIComponent(pathname).replace(/^\/+/, '')
    const resolved = path.resolve(root, filename)
    if (!resolved.startsWith(root)) throw new Error('Outside dist')
    const contentType = ({ '.html': 'text/html', '.js': 'text/javascript', '.css': 'text/css' })[path.extname(resolved)] || 'application/octet-stream'
    response.writeHead(200, { 'Content-Type': contentType }).end(await readFile(resolved))
  } catch { response.writeHead(404).end() }
})
await new Promise(resolve => server.listen(0, '127.0.0.1', resolve))
const base = `http://127.0.0.1:${server.address().port}`
let browser, failure
try {
  browser = await chromium.launch({ channel: process.env.TEST_BROWSER || 'chrome', headless: true })
  for (const theme of ['light', 'dark', 'ocean', 'mint']) {
    for (const width of [320, 1440]) {
      const context = await browser.newContext({ viewport: { width, height: 900 }, reducedMotion: 'reduce', serviceWorkers: 'block' })
      const state = await installMockApi(context)
      state.authenticated = true
      await context.addInitScript(value => localStorage.setItem('theme', JSON.stringify({ version: 1, value })), theme)
      const page = await context.newPage()
      page.setDefaultTimeout(10000)
      page.on('pageerror', error => errors.push(error.message))
      page.on('console', message => { if (/Failed to resolve component|Invalid prop|Unhandled error/.test(message.text())) errors.push(message.text()) })
      const go = async route => {
        await page.goto(`${base}/#/${route}`)
        await expect(page.locator('.page-header-title')).toHaveText(managementPages.find(item => item.path === `/${route}`).title)
        await page.waitForFunction(() => !document.querySelector('.el-loading-mask'))
      }
      const capture = async (kind, locator) => {
        await expect(locator).toBeVisible()
        await page.evaluate(() => document.fonts.ready)
        await locator.evaluate(element => Promise.all(element.getAnimations({ subtree: true }).filter(animation => Number.isFinite(animation.effect?.getTiming().iterations)).map(animation => animation.finished.catch(() => {}))))
        const box = await locator.boundingBox()
        assert.ok(box.x >= -1 && box.y >= -1 && box.x + box.width <= width + 1 && box.y + box.height <= 901, `${kind}/${theme}/${width}: overlay fits viewport ${JSON.stringify(box)}`)
        const metrics = await locator.evaluate(element => {
          const surface = element.matches('.el-dialog, .el-message-box, .el-drawer') ? element : element.querySelector('.el-dialog, .el-message-box, .el-drawer')
          const style = getComputedStyle(surface || element)
          const body = element.querySelector('.el-dialog__body, .el-drawer__body, .el-message-box__content')
          return { radius: parseFloat(style.borderRadius), shadow: style.boxShadow, bodyOverflow: body ? body.scrollWidth - body.clientWidth : 0 }
        })
        if (!kind.includes('drawer')) assert.ok(metrics.radius >= 12, `${kind}: custom radius survives route CSS`)
        assert.notEqual(metrics.shadow, 'none', `${kind}: surface shadow loaded`)
        assert.ok(metrics.bodyOverflow <= 1, `${kind}/${theme}/${width}: no horizontal body overflow (${metrics.bodyOverflow})`)
        const file = `${kind}-${theme}-${width}.png`
        await page.screenshot({ path: path.join(output, file) })
        screenshots.push(file)
        checks.push(`${kind}/${theme}/${width}`)
      }
      const cancel = async dialog => { await dialog.getByRole('button', { name: '取消', exact: true }).click(); await expect(dialog).toBeHidden() }

      // Start directly on this route, so no account page can supply missing CSS.
      await go('proxies')
      await page.getByRole('button', { name: '添加代理', exact: true }).click()
      let dialog = page.getByRole('dialog', { name: '添加代理', exact: true })
      await expect(dialog.getByRole('radio', { name: 'HTTP', exact: true })).toBeChecked()
      await dialog.getByText('SOCKS5', { exact: true }).click()
      await expect(dialog.getByRole('radio', { name: 'SOCKS5', exact: true })).toBeChecked()
      assert.ok(await dialog.locator('.el-radio-button__inner').first().evaluate(el => parseFloat(getComputedStyle(el).paddingLeft)) >= 16)
      assert.ok(await dialog.locator('.el-input__wrapper').first().evaluate(el => el.getBoundingClientRect().height) >= 36)
      await capture('proxy-form', dialog)
      await cancel(dialog)

      await go('groups')
      const trigger = page.getByRole('button', { name: '创建分组', exact: true })
      await trigger.click()
      dialog = page.getByRole('dialog', { name: '创建分组', exact: true })
      await capture('group-form', dialog)
      await cancel(dialog)
      await expect(trigger).toBeFocused()
      // Narrow layouts expose real card actions while their desktop table is
      // hidden. Resolve the visible surface, including older table-only builds.
      const mobileGroups = page.locator('.resource-mobile-list:visible')
      const groupSurface = await mobileGroups.count() ? mobileGroups.getByRole('article', { name: '测试分组', exact: true }) : page.locator('.el-table:visible')
      const deleteGroup = groupSurface.getByRole('button', { name: '删除', exact: true })
      await expect(deleteGroup).toBeVisible()
      await deleteGroup.click()
      await capture('confirm', page.locator('.el-message-box'))
      await cancel(page.locator('.el-message-box'))
      await expect(deleteGroup).toBeFocused()

      await go('keys')
      await page.getByRole('button', { name: '创建 KEY', exact: true }).click()
      dialog = page.getByRole('dialog', { name: '创建密钥', exact: true })
      await dialog.getByPlaceholder('例如：对外服务 A').fill('界面验收 · 合成数据')
      await dialog.locator('.el-select').click()
      const dropdown = page.locator('.el-select__popper:visible')
      await expect(dropdown).toBeVisible()
      assert.equal(await dropdown.evaluate(el => getComputedStyle(el).borderRadius), '10px')
      assert.notEqual(await dropdown.evaluate(el => getComputedStyle(el).backgroundColor), 'rgba(0, 0, 0, 0)')
      await capture('key-select', dialog)
      await page.getByRole('option', { name: '测试分组', exact: true }).click()
      await dialog.getByPlaceholder('例如：对外服务 A').click()
      await page.route('**/api/keys', async route => {
        if (route.request().method() === 'POST') await route.fulfill({ json: { key: 'sk-synthetic-modal-test-only' } })
        else await route.fallback()
      })
      await dialog.getByRole('button', { name: '保存', exact: true }).click()
      dialog = page.getByRole('dialog', { name: '密钥创建成功', exact: true })
      await capture('key-created', dialog)
      await page.evaluate(() => Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText: async () => { throw new Error('Synthetic clipboard failure') } } }))
      await dialog.getByRole('button', { name: '复制完整 key', exact: true }).click()
      const copyDialog = page.getByRole('dialog', { name: '手动复制', exact: true })
      await capture('manual-copy', copyDialog)
      await expect(copyDialog.getByRole('textbox')).toBeFocused()
      await copyDialog.getByRole('button', { name: '完成', exact: true }).click()
      await expect(dialog.getByRole('button', { name: '复制完整 key', exact: true })).toBeFocused()
      await dialog.getByRole('button', { name: '我已保存', exact: true }).click()

      await go('accounts')
      await expect(page.locator('.el-message')).toHaveCount(0, { timeout: 5000 })
      await page.getByRole('button', { name: '导入账号', exact: true }).click()
      dialog = page.getByRole('dialog', { name: '导入账号 · 先预览再提交', exact: true })
      await capture('account-import', dialog)
      await dialog.getByRole('button', { name: '粘贴内容', exact: true }).click()
      await dialog.getByRole('textbox', { name: '账号导入内容', exact: true }).fill(JSON.stringify({ exported_at: '2026-09-17T12:00:00Z', proxies: [], accounts: [{ name: '导入示例 · 合成数据', platform: 'openai', type: 'oauth', concurrency: 8, priority: 2, credentials: { access_token: 'synthetic-import-visual-test-only' } }] }, null, 2))
      await dialog.getByRole('button', { name: '生成预览', exact: true }).click()
      await expect(dialog.getByRole('button', { name: '确认导入 1 项', exact: true })).toBeEnabled()
      await capture('account-import-preview', dialog)
      await dialog.getByRole('button', { name: '关闭', exact: true }).click()
      await page.getByRole('button', { name: '显示列', exact: true }).click()
      dialog = page.getByRole('dialog', { name: '显示 / 隐藏列', exact: true })
      await capture('column-checkboxes', dialog)
      await dialog.getByRole('button', { name: '完成', exact: true }).click()
      const mobileAccounts = page.locator('.mobile-account-list:visible')
      const accountSurface = await mobileAccounts.count() ? mobileAccounts.locator('.mobile-account').filter({ has: page.getByRole('button', { name: '查看账号 示例账号 Alpha 详情', exact: true }) }) : page.locator('.el-table:visible')
      const editAccount = accountSurface.getByRole('button', { name: '编辑', exact: true }).first()
      await expect(editAccount).toBeVisible()
      await editAccount.click()
      dialog = page.getByRole('dialog', { name: '编辑账号：示例账号 Alpha', exact: true })
      await capture('account-form', dialog)
      await cancel(dialog)

      await go('logs')
      const advanced = page.getByRole('button', { name: /^高级筛选/ })
      if (await advanced.count() && await advanced.getAttribute('aria-expanded') === 'false') await advanced.click()
      await page.getByRole('button', { name: '保存筛选', exact: true }).click()
      dialog = page.getByRole('dialog', { name: '保存常用筛选', exact: true })
      await capture('log-filter', dialog)
      await cancel(dialog)
      await page.getByRole('button', { name: /^查看 .* 的日志详情$/ }).click()
      await capture('log-drawer', page.getByRole('dialog', { name: '日志详情', exact: true }))
      assert.deepEqual(state.unexpected, [])
      await context.close()
    }
  }
  assert.deepEqual(errors, [])
  assert.deepEqual(leakedApi, [])
} catch (error) { failure = error; console.error(error) }
finally {
  await browser?.close()
  await new Promise(resolve => server.close(resolve))
  await writeFile(path.join(output, 'results.json'), JSON.stringify({ passed: !failure, checks, screenshots, errors, leakedApi, ...(failure ? { failure: failure.stack } : {}) }, null, 2))
  console.log(JSON.stringify({ passed: !failure, checks: checks.length, screenshots: screenshots.length, output }))
}
if (failure) process.exitCode = 1
