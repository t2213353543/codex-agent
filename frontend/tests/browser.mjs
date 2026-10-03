import assert from 'node:assert/strict'
import { createServer } from 'node:http'
import { readFile, mkdir, writeFile } from 'node:fs/promises'
import { fileURLToPath } from 'node:url'
import path from 'node:path'
import { chromium, expect } from 'playwright/test'
import { installMockApi, mockKey } from './mock-api.mjs'
import { testManagement } from './management-browser.mjs'

const root = fileURLToPath(new URL('../dist/', import.meta.url))
const output = fileURLToPath(new URL('../test-results/browser/', import.meta.url))
await mkdir(output, { recursive: true })
const leakedApi = []
const server = createServer(async (request, response) => {
  const pathname = new URL(request.url, 'http://localhost').pathname
  if (pathname.startsWith('/api/')) {
    leakedApi.push(pathname)
    response.writeHead(500, { 'Content-Type': 'application/json' }).end('{"error":{"message":"API escaped mock interception"}}')
    return
  }
  try {
    const filename = pathname === '/' ? 'index.html' : decodeURIComponent(pathname).replace(/^\/+/, '')
    const resolved = path.resolve(root, filename)
    if (!resolved.startsWith(root)) throw new Error('Outside dist')
    const data = await readFile(resolved)
    const contentType = ({ '.html': 'text/html', '.js': 'text/javascript', '.css': 'text/css', '.svg': 'image/svg+xml' })[path.extname(resolved)] || 'application/octet-stream'
    response.writeHead(200, { 'Content-Type': contentType, 'Cache-Control': 'no-store' }).end(data)
  } catch { response.writeHead(404).end('Not found') }
})
await new Promise(resolve => server.listen(0, '127.0.0.1', resolve))
const base = `http://127.0.0.1:${server.address().port}`
let browser
const assertions = []
const screenshots = []
const errors = []
let failure
try {
  browser = await chromium.launch({ headless: true, channel: process.env.TEST_BROWSER || 'chrome' })
  const context = await browser.newContext({ viewport: { width: 1440, height: 1000 }, serviceWorkers: 'block', acceptDownloads: true, reducedMotion: 'reduce' })
  const state = await installMockApi(context)
  const page = await context.newPage()
  page.setDefaultTimeout(10000)
  page.on('pageerror', error => errors.push(error.message))
  page.on('console', message => { if (message.type() === 'warning' && /Failed to resolve component|Invalid prop|Unhandled error/.test(message.text())) errors.push(message.text()) })
  await page.addInitScript(() => {
    if (!sessionStorage.getItem('mock-seeded')) {
      localStorage.setItem('adminKey', 'obsolete-key-must-be-removed')
      localStorage.setItem('visitedTabs', '{broken')
      localStorage.setItem('accountColumns', '{broken')
      sessionStorage.setItem('mock-seeded', '1')
    }
  })
  await page.goto(`${base}/#/accounts`)
  await expect(page.getByRole('textbox', { name: '管理密钥' })).toBeVisible()
  await expect(page.locator('html')).toHaveAttribute('data-theme', 'dark')
  await expect(page.getByRole('button', { name: '切换留白主题', exact: true })).toBeVisible()
  await expect(page.getByRole('button', { name: '切换夜航主题', exact: true })).toBeVisible()
  await page.getByRole('textbox', { name: '管理密钥' }).fill('invalid')
  await page.getByRole('button', { name: '登 录', exact: true }).click()
  await expect(page.getByRole('alert')).toContainText('管理密钥无效')
  await page.getByRole('textbox', { name: '管理密钥' }).fill(mockKey)
  await page.getByRole('button', { name: '登 录', exact: true }).evaluate(button => { button.click(); button.click() })
  await expect(page.locator('.page-header')).toContainText('账号管理')
  assert.equal(state.loginCount, 2, 'Only one valid login request despite double submission')
  assert.equal(await page.evaluate(() => localStorage.getItem('adminKey')), null)
  assert.equal(await page.evaluate(() => sessionStorage.getItem('adminKey')), null)
  assert.ok(state.requests.every(r => r.path === '/session' || !r.body?.admin_key))
  await page.reload()
  await expect(page.locator('.page-header')).toContainText('账号管理')
  await expect(page.locator('.console-version').first()).toHaveText('v0.2.0-mock')
  assertions.push('无效登录、双击防重复、刷新恢复 Cookie 会话、迁移清理旧 adminKey、后端版本')
  await expect(page.locator('.notice-badge')).toHaveCount(0)

  // Account-specific checks own their temporary routes and restore the fixture.
  const { testAccounts } = await import('./accounts-browser.mjs')
  assertions.push(...await testAccounts(page, state))
  assertions.push(...await testManagement(page))
  await page.goto(`${base}/#/accounts?search=Alpha`)
  await expect(page.locator('.page-header')).toContainText('账号管理')
  await page.getByRole('button', { name: '刷新当前页面', exact: true }).click()
  await expect(page.locator('.page-header')).toContainText('账号管理')
  assert.ok(!page.url().includes('redirect'))
  assert.equal(new URL(page.url()).hash, '#/accounts?search=Alpha')
  await expect(page.getByRole('navigation', { name: '打开的页面' })).toHaveCount(0)
  await page.getByRole('button', { name: '切换留白主题', exact: true }).click()
  await expect(page.locator('html')).toHaveAttribute('data-theme', 'light')
  assert.deepEqual(JSON.parse(await page.evaluate(() => localStorage.getItem('theme'))), { version: 1, value: 'light' })
  await page.reload()
  await expect(page.locator('html')).toHaveAttribute('data-theme', 'light')
  await expect(page.locator('.console-shell')).toHaveAttribute('data-console-theme', 'light')
  assert.equal(new URL(page.url()).hash, '#/accounts?search=Alpha')
  await page.getByRole('button', { name: '切换夜航主题', exact: true }).click()
  await expect(page.locator('html')).toHaveAttribute('data-theme', 'dark')
  await page.locator('.console-topbar').getByRole('button', { name: '收起侧栏', exact: true }).click()
  await expect(page.locator('.console-shell')).toHaveClass(/console-is-collapsed/)
  await page.reload()
  await expect(page.locator('.console-shell')).toHaveClass(/console-is-collapsed/)
  await expect(page.locator('html')).toHaveClass(/dark/)
  await page.locator('.console-topbar').getByRole('button', { name: '展开侧栏', exact: true }).click()
  assertions.push('默认夜航；刷新保留当前路由和筛选；留白/夜航通过按钮切换并持久化；侧栏折叠刷新保持；无旧页签栏')

  // Both selected themes and breakpoints, including long names and local table scroll.
  for (const theme of ['light', 'dark']) {
    for (const width of [320, 390, 768, 1440]) {
      await page.setViewportSize({ width, height: 1000 })
      await page.evaluate(value => localStorage.setItem('theme', JSON.stringify({ version: 1, value })), theme)
      await page.reload()
      await expect(page.locator('.page-header')).toBeVisible()
      for (const view of ['accounts', 'groups', 'proxies', 'settings']) {
        await page.evaluate(value => { window.location.hash = `/${value}` }, view)
        const title = { accounts: '账号管理', groups: '分组管理', proxies: 'IP 代理池', settings: '管理设置' }[view]
        await expect(page.locator('.page-header-title')).toHaveText(title)
        await page.waitForFunction(() => !document.querySelector('.el-loading-mask'))
        await page.evaluate(() => document.fonts.ready)
        assert.equal(await page.locator('html').getAttribute('data-theme'), theme)
        const overflow = await page.evaluate(() => {
          const content = document.querySelector('.content')
          return { document: document.documentElement.scrollWidth - innerWidth, content: content.scrollWidth - content.clientWidth }
        })
        assert.ok(overflow.document <= 1 && overflow.content <= 1, `${view} ${theme} ${width}: ${JSON.stringify(overflow)}`)
        const filename = `${view}-${theme}-${width}.png`
        await page.screenshot({ path: path.join(output, filename), fullPage: true })
        screenshots.push(filename)
      }
      if (width < 768) {
        await page.getByRole('button', { name: '打开导航', exact: true }).click()
        await expect(page.locator('.console-mobile-navigation')).toBeVisible()
        await page.screenshot({ path: path.join(output, `navigation-${theme}-${width}.png`) })
        screenshots.push(`navigation-${theme}-${width}.png`)
        await page.locator('.console-mobile-navigation').getByRole('link', { name: '分组管理', exact: true }).click()
        await expect(page.locator('.console-mobile-navigation')).toBeHidden()
        const trigger = page.getByRole('button', { name: '创建分组', exact: true })
        await trigger.focus()
        await trigger.click()
        const dialog = page.getByRole('dialog', { name: '创建分组' })
        await expect(dialog).toBeVisible()
        const box = await dialog.boundingBox()
        assert.ok(box.x >= 0 && box.x + box.width <= width + 1)
        await page.screenshot({ path: path.join(output, `dialog-${theme}-${width}.png`) })
        screenshots.push(`dialog-${theme}-${width}.png`)
        await dialog.getByRole('button', { name: '取消', exact: true }).click()
        await expect(trigger).toBeFocused()
      }
    }
  }
  assertions.push('账号、分组、代理、设置 2主题×4断点截图；无整页横向溢出；移动导航及弹窗可达、焦点恢复')
  await page.setViewportSize({ width: 1440, height: 1000 })
  await page.getByRole('button', { name: '管理员菜单' }).click()
  await page.getByRole('menuitem', { name: '退出登录' }).click()
  await page.locator('.el-message-box').getByRole('button', { name: '取消', exact: true }).click()
  assert.equal(state.authenticated, true)
  await page.getByRole('button', { name: '管理员菜单' }).click()
  await page.getByRole('menuitem', { name: '退出登录' }).click()
  await page.locator('.el-message-box').getByRole('button', { name: '退出', exact: true }).click()
  await expect(page.getByRole('textbox', { name: '管理密钥' })).toBeVisible()
  assert.equal(state.authenticated, false)
  await page.reload()
  await expect(page.getByRole('textbox', { name: '管理密钥' })).toBeVisible()
  assertions.push('退出取消保持会话；确认调用 DELETE session；刷新不能恢复已注销会话')
  assert.deepEqual(leakedApi, [], 'No API reached the static server')
  assert.deepEqual(state.unexpected, [], 'No unmocked or external requests')
  assert.deepEqual(errors, [], 'No browser runtime errors or unresolved Vue components')
  await context.close()
} catch (error) { failure = error; console.error(error) }
finally {
  if (browser) await browser.close()
  await new Promise(resolve => server.close(resolve))
  const report = { passed: !failure, assertions, screenshots, errors, leakedApi, ...(failure ? { failure: failure.stack } : {}) }
  await writeFile(path.join(output, 'results.json'), JSON.stringify(report, null, 2))
  console.log(JSON.stringify({ passed: report.passed, assertions: assertions.length, screenshots: screenshots.length, report: path.join(output, 'results.json') }))
}
if (failure) process.exitCode = 1
