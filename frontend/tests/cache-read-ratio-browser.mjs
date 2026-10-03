import assert from 'node:assert/strict'
import { createServer } from 'node:http'
import { mkdir, readFile, writeFile } from 'node:fs/promises'
import { fileURLToPath } from 'node:url'
import path from 'node:path'
import { chromium, expect } from 'playwright/test'
import { installMockApi } from './mock-api.mjs'
import { settingsRestoredFixture } from './settings-restored-browser.mjs'

const dist = fileURLToPath(new URL('../dist/', import.meta.url))
const output = fileURLToPath(new URL('../test-results/cache-read-ratio/', import.meta.url))
await mkdir(output, { recursive: true })
const leakedApi = []
const errors = []
const assertions = []
const writes = []
const server = createServer(async (request, response) => {
  const pathname = new URL(request.url, 'http://localhost').pathname
  if (pathname.startsWith('/api/')) { leakedApi.push(pathname); response.writeHead(500).end(); return }
  try {
    const file = path.resolve(dist, pathname === '/' ? 'index.html' : decodeURIComponent(pathname).replace(/^\/+/, ''))
    assert.ok(file.startsWith(dist))
    const body = await readFile(file)
    response.writeHead(200, { 'Content-Type': ({ '.html': 'text/html', '.js': 'text/javascript', '.css': 'text/css', '.svg': 'image/svg+xml' })[path.extname(file)] || 'application/octet-stream' }).end(body)
  } catch { response.writeHead(404).end() }
})
await new Promise(resolve => server.listen(0, '127.0.0.1', resolve))
let browser
let page
let failure
try {
  browser = await chromium.launch({ headless: true, channel: process.env.TEST_BROWSER || 'chrome' })
  const context = await browser.newContext({ viewport: { width: 1440, height: 1000 }, serviceWorkers: 'block', reducedMotion: 'reduce' })
  const state = await installMockApi(context)
  state.authenticated = true
  page = await context.newPage()
  page.on('pageerror', error => errors.push(error.message))
  let settings = structuredClone(settingsRestoredFixture)
  let readFailure = true
  let writeFailure = false
  await page.route('**/api/settings', async route => {
    if (route.request().method() === 'GET') {
      return route.fulfill(readFailure ? { status: 503, json: { error: { message: '模拟读取失败' } } } : { json: settings })
    }
    assert.equal(route.request().method(), 'POST')
    const body = route.request().postDataJSON()
    writes.push(body)
    await new Promise(resolve => setTimeout(resolve, 100))
    if (writeFailure) return route.fulfill({ status: 500, json: { error: { message: '模拟保存失败' } } })
    if (body.settings_version !== settings.settings_version) return route.fulfill({ status: 409, json: { error: { message: '模拟设置冲突' } } })
    settings = { ...settings, ...body, settings_version: settings.settings_version + 1 }
    return route.fulfill({ json: { ok: true, settings_version: settings.settings_version } })
  })
  await page.goto(`http://127.0.0.1:${server.address().port}/#/settings`)
  const toggle = page.getByRole('switch', { name: '启用缓存率控制', exact: true })
  const input = page.getByRole('spinbutton', { name: '缓存读取比例（%）', exact: true })
  const save = page.getByRole('button', { name: '保存比例', exact: true })
  const refresh = page.getByRole('button', { name: '刷新系统设置', exact: true })
  const saveAll = page.getByRole('button', { name: '保存全部修改', exact: true })
  const row = page.locator('[data-setting="cache_read_ratio_enabled"]')
  const error = page.locator('.management-error')
  await expect(error).toContainText('模拟读取失败')
  await expect(toggle).toBeDisabled()
  await expect(input).toBeDisabled()
  readFailure = false
  await refresh.click()
  await expect(toggle).toBeEnabled()
  await expect(toggle).not.toBeChecked()
  await expect(input).toHaveValue('0')
  await expect(save).toBeDisabled()
  await expect(page.locator('[data-setting="cache_creation_as_read_enabled"] + [data-setting="cache_read_ratio_enabled"]')).toHaveCount(1)
  await expect(row).toContainText('仅调整对外返回的缓存读取量，本系统保留原始用量；按输入 Token × 比例取整')
  assertions.push('原开关下方展示缓存率控制；缺失新字段时默认关闭、0%；读取失败禁止编辑')

  for (const invalid of ['', '-1', '101', '12.5']) {
    await input.fill(invalid)
    await expect(input).toHaveAttribute('aria-invalid', 'true')
    await expect(save).toBeDisabled()
    await expect(saveAll).toBeDisabled()
  }
  assert.equal(writes.length, 0)
  for (const percent of [100, 0, 40]) {
    await input.fill(String(percent))
    await expect(input).toHaveAttribute('aria-invalid', 'false')
    const version = settings.settings_version
    await save.click()
    await expect(input).toBeDisabled()
    await expect(toggle).toBeDisabled()
    await expect(input).toBeEnabled()
    await expect(save).toBeDisabled()
    assert.deepEqual(writes.at(-1), { cache_read_ratio_percent: percent, settings_version: version })
    await expect(toggle).not.toBeChecked()
  }
  assertions.push('空值、越界、小数禁止保存；0%/100%边界有效，关闭时可预设比例，保存中锁定控件')

  writeFailure = true
  await toggle.locator('..').click()
  await expect(error).toContainText('模拟保存失败')
  await expect(toggle).not.toBeChecked()
  writeFailure = false
  let version = settings.settings_version
  await toggle.locator('..').evaluate(button => { button.click(); button.click() })
  await expect(toggle).toBeChecked()
  await expect(toggle).toBeEnabled()
  assert.deepEqual(writes.at(-1), { cache_read_ratio_enabled: true, settings_version: version })
  assert.equal(settings.cache_read_ratio_percent, 40)
  version = settings.settings_version
  await toggle.locator('..').click()
  await expect(toggle).not.toBeChecked()
  await expect(toggle).toBeEnabled()
  assert.deepEqual(writes.at(-1), { cache_read_ratio_enabled: false, settings_version: version })
  await refresh.click()
  await expect(input).toBeEnabled()
  await expect(input).toHaveValue('40')
  await expect(toggle).not.toBeChecked()
  assertions.push('开关失败保持原值；启停仅写布尔字段，关闭及刷新保留已保存比例')

  await input.fill('70')
  writeFailure = true
  await save.click()
  await expect(error).toContainText('模拟保存失败')
  await expect(input).toHaveValue('70')
  await expect(save).toBeEnabled()
  writeFailure = false
  settings = { ...settings, cache_read_ratio_percent: 20, settings_version: settings.settings_version + 3 }
  const writesBeforeConflict = writes.length
  await save.click()
  await expect(error).toContainText('已读取最新设置')
  await expect(error).toContainText('设置草稿已保留')
  await expect(input).toHaveValue('70')
  assert.equal(writes.length, writesBeforeConflict + 1)
  version = settings.settings_version
  await save.click()
  await expect(save).toBeDisabled()
  assert.deepEqual(writes.at(-1), { cache_read_ratio_percent: 70, settings_version: version })
  await input.fill('65')
  await refresh.click()
  await expect(input).toBeEnabled()
  await expect(input).toHaveValue('65')
  version = settings.settings_version
  await saveAll.click()
  await expect(saveAll).toHaveCount(0)
  assert.deepEqual(writes.at(-1), { cache_read_ratio_percent: 65, settings_version: version })
  await input.fill('80')
  await row.getByRole('button', { name: '撤销修改', exact: true }).click()
  await expect(input).toHaveValue('65')
  await expect(saveAll).toHaveCount(0)
  assertions.push('保存失败、409、刷新保留比例草稿；409不自动重试；显式重试携带新版本，支持保存全部/撤销')

  await expect(page.locator('.el-message')).toHaveCount(0, { timeout: 10000 })
  for (const width of [320, 768, 1440]) {
    await page.setViewportSize({ width, height: 1000 })
    await row.scrollIntoViewIfNeeded()
    const overflow = await page.evaluate(() => ({
      document: document.documentElement.scrollWidth - innerWidth,
      content: document.querySelector('.content').scrollWidth - document.querySelector('.content').clientWidth,
    }))
    assert.ok(overflow.document <= 1 && overflow.content <= 1, `${width}: ${JSON.stringify(overflow)}`)
    await page.screenshot({ path: path.join(output, `cache-ratio-${width}.png`) })
  }
  assertions.push('320/768/1440px 无页面横向溢出，已保存截图')
  assert.deepEqual(errors, [])
  assert.deepEqual(leakedApi, [])
  assert.deepEqual(state.unexpected, [])
  await context.close()
} catch (error) {
  failure = error
  console.error(error)
  if (page) await page.screenshot({ path: path.join(output, 'failure.png') }).catch(() => {})
} finally {
  if (browser) await browser.close()
  await new Promise(resolve => server.close(resolve))
  const report = { passed: !failure, assertions, writes, errors, leakedApi, ...(failure ? { failure: failure.stack } : {}) }
  await writeFile(path.join(output, 'results.json'), JSON.stringify(report, null, 2))
  console.log(JSON.stringify({ passed: !failure, assertions: assertions.length, report: path.join(output, 'results.json') }))
}
if (failure) process.exitCode = 1
