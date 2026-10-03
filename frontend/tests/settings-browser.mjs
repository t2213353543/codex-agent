import assert from 'node:assert/strict'
import { createServer } from 'node:http'
import { mkdir, readFile, writeFile } from 'node:fs/promises'
import { fileURLToPath } from 'node:url'
import path from 'node:path'
import { chromium, expect } from 'playwright/test'
import { installMockApi } from './mock-api.mjs'
import { testSettingsRestored, settingsRestoredFixture } from './settings-restored-browser.mjs'

const root = path.resolve(process.env.MANAGEMENT_UI_DIST || fileURLToPath(new URL('../dist/', import.meta.url)))
const output = fileURLToPath(new URL('../test-results/settings/', import.meta.url))
await mkdir(output, { recursive: true })
const leakedApi = []
const errors = []
const assertions = []
let behavior
const server = createServer(async (request, response) => {
  const pathname = new URL(request.url, 'http://localhost').pathname
  if (pathname.startsWith('/api/')) { leakedApi.push(pathname); response.writeHead(500).end('API escaped mock interception'); return }
  try {
    const file = path.resolve(root, pathname === '/' ? 'index.html' : decodeURIComponent(pathname).replace(/^\/+/, ''))
    if (file !== path.join(root, 'index.html') && !file.startsWith(root + path.sep)) throw new Error('Outside dist')
    const body = await readFile(file)
    response.writeHead(200, { 'Content-Type': ({ '.html': 'text/html', '.js': 'text/javascript', '.css': 'text/css', '.svg': 'image/svg+xml', '.woff2': 'font/woff2' })[path.extname(file)] || 'application/octet-stream' }).end(body)
  } catch { response.writeHead(404).end('Not found') }
})
await new Promise(resolve => server.listen(0, '127.0.0.1', resolve))
let browser
let failure
let page
try {
  browser = await chromium.launch({ headless: true, channel: process.env.TEST_BROWSER || 'chrome' })
  const context = await browser.newContext({ viewport: { width: 1440, height: 1000 }, serviceWorkers: 'block', reducedMotion: 'reduce' })
  const state = await installMockApi(context)
  state.authenticated = true
  page = await context.newPage()
  page.setDefaultTimeout(10000)
  page.on('pageerror', error => errors.push(error.message))
  await page.goto(`http://127.0.0.1:${server.address().port}/#/settings`)
  behavior = await testSettingsRestored(page)
  assertions.push(...behavior.assertions)
  await page.route('**/api/settings', route => route.fulfill({ json: settingsRestoredFixture }))
  await page.reload()
  await expect(page.getByRole('switch', { name: '启用 WebSocket', exact: true })).toBeEnabled()
  for (const width of [320, 768, 1440]) {
    await page.setViewportSize({ width, height: 1000 })
    await page.locator('.content').evaluate(node => { node.scrollTop = 0 })
    const overflow = await page.evaluate(() => ({
      document: document.documentElement.scrollWidth - innerWidth,
      content: document.querySelector('.content').scrollWidth - document.querySelector('.content').clientWidth,
    }))
    assert.ok(overflow.document <= 1 && overflow.content <= 1, `${width}: ${JSON.stringify(overflow)}`)
    await page.screenshot({ path: path.join(output, `settings-${width}.png`), fullPage: true })
    await page.getByRole('region', { name: '上游超时设置', exact: true }).scrollIntoViewIfNeeded()
    await page.screenshot({ path: path.join(output, `settings-${width}-timeouts.png`), fullPage: true })
  }
  assertions.push('320、768、1440px 无整页横向溢出并生成截图')
  await page.getByRole('region', { name: '推理服务配置' }).getByText('高级部署配置', { exact: true }).click()
  await expect(page.getByRole('region', { name: '推理服务配置' })).toContainText('tools-fixture')
  await expect(page.getByRole('switch', { name: '启用粘性会话', exact: true })).toHaveCount(0)
  await expect(page.getByRole('switch', { name: '缓存键隔离', exact: true })).toHaveCount(0)
  await page.goto(`http://127.0.0.1:${server.address().port}/#/connection`)
  // The current router has no connection page; keep this retired-route check
  // separate from settings behavior instead of claiming guide UI coverage.
  await expect(page).toHaveURL(/#\/$/)
  await expect(page.getByRole('heading', { name: '运行总览', exact: true })).toBeVisible()
  await expect(page.getByRole('link', { name: '接入指南', exact: true })).toHaveCount(0)
  await expect(page).toHaveTitle('高级反代系统')
  for (const width of [320, 1440]) {
    await page.setViewportSize({ width, height: 1000 })
    assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1))
    await page.screenshot({ path: path.join(output, `retired-guide-fallback-${width}.png`), fullPage: true })
  }
  assertions.push('PowerPoint 实际配置只读展示；旧指纹/缓存/会话关闭入口隐藏；移除的接入页面回到仪表盘（不包含旧指南验收）')
  assert.deepEqual(errors, [])
  assert.deepEqual(leakedApi, [])
  assert.deepEqual(state.unexpected, [])
  await context.close()
} catch (error) {
  failure = error; console.error(error)
  if (page) {
    await page.screenshot({ path: path.join(output, 'settings-failure.png'), fullPage: true }).catch(() => {})
    await writeFile(path.join(output, 'settings-failure.txt'), `${page.url()}\n${await page.locator('body').innerText()}`).catch(() => {})
  }
}
finally {
  if (browser) await browser.close()
  await new Promise(resolve => server.close(resolve))
  await writeFile(path.join(output, 'results.json'), JSON.stringify({ passed: !failure, assertions, behavior, errors, leakedApi, ...(failure ? { failure: failure.stack } : {}) }, null, 2))
  console.log(JSON.stringify({ passed: !failure, assertions: assertions.length, report: path.join(output, 'results.json') }))
}
if (failure) process.exitCode = 1
