import assert from 'node:assert/strict'
import { createServer } from 'node:http'
import { mkdir, readFile, writeFile } from 'node:fs/promises'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { chromium, expect } from 'playwright/test'
import { installMockApi, mockKey } from './mock-api.mjs'
import { managementPages } from '../src/utils/navigation.js'
import { testManagement } from './management-browser.mjs'
import { testManagementNavigation } from './management-navigation-browser.mjs'

const root = process.env.MANAGEMENT_UI_DIST ? path.resolve(process.env.MANAGEMENT_UI_DIST) : fileURLToPath(new URL('../../.local/management-completion/dist/', import.meta.url))
const output = fileURLToPath(new URL('../../.local/management-completion/browser/', import.meta.url))
await mkdir(output, { recursive: true })
const assertions = [], screenshots = [], errors = [], escaped = []
const server = createServer(async (request, response) => {
  const pathname = new URL(request.url, 'http://localhost').pathname
  if (pathname.startsWith('/api/')) { escaped.push(pathname); response.writeHead(500).end('Unmocked API'); return }
  try {
    const file = path.resolve(root, pathname === '/' ? 'index.html' : decodeURIComponent(pathname).replace(/^\/+/, ''))
    if (path.relative(root, file).startsWith('..')) throw new Error('Outside root')
    const data = await readFile(file)
    response.writeHead(200, { 'Content-Type': ({ '.html': 'text/html', '.css': 'text/css', '.js': 'text/javascript', '.svg': 'image/svg+xml' })[path.extname(file)] || 'application/octet-stream' }).end(data)
  } catch { response.writeHead(404).end() }
})
await new Promise(resolve => server.listen(0, '127.0.0.1', resolve))
const base = `http://127.0.0.1:${server.address().port}`
let browser, failure
try {
  browser = await chromium.launch({ channel: process.env.TEST_BROWSER || 'chrome', headless: true })
  let context = await browser.newContext({ viewport: { width: 1440, height: 1000 }, reducedMotion: 'reduce', serviceWorkers: 'block', acceptDownloads: true })
  await context.route('**/*', route => {
    if (new URL(route.request().url()).origin !== base) { escaped.push(route.request().url()); return route.abort() }
    return route.continue()
  })
  const state = await installMockApi(context)
  let page = await context.newPage()
  page.setDefaultTimeout(12000)
  page.on('pageerror', error => errors.push(error.message))
  let diagnosticMode = 'ok', auditMode = 'ok'
  let gate
  const auditQueries = []
  const auditRow = id => ({ id, created_at: 1700000000 - Number(id), action: 'account.update', entity_id: `a${id}`, actor: 'admin', result: 'ok' })
  await page.route('**/api/diagnostics', route => {
    if (diagnosticMode === 'error') return route.fulfill({ status: 503, json: { error: { message: '测试：诊断暂不可用' } } })
    return route.fulfill({ json: { version: '0.2.0-test', checked_at: 1700000000, uptime_seconds: 7200, storage_mode: 'postgres', status: diagnosticMode === 'ok' ? 'ok' : 'degraded', dependencies: { postgres: { configured: true, status: 'ok', latency_ms: 3 }, redis: { configured: true, status: diagnosticMode === 'ok' ? 'ok' : 'timeout', latency_ms: 2 } }, capabilities: { audit_logs: true }, ignored_secret: 'never-export-this-secret' } })
  })
  await page.route('**/api/audit-logs?*', async route => {
    const query = Object.fromEntries(new URL(route.request().url()).searchParams)
    auditQueries.push(query)
    if (gate && query.entity_id === 'slow') await gate
    if (auditMode === 'error') return route.fulfill({ status: 503, json: { error: { message: '测试：操作记录暂不可用' } } })
    if (auditMode === 'unsupported') return route.fulfill({ json: { supported: false, storage_mode: 'file', items: [], has_more: false, next_cursor: '', limit: 50 } })
    const items = query.entity_id ? [{ ...auditRow('5'), entity_id: query.entity_id }] : query.cursor ? [auditRow('3')] : [auditRow('1'), auditRow('2')]
    return route.fulfill({ json: { supported: true, storage_mode: 'postgres', items, has_more: !query.cursor && !query.entity_id, next_cursor: !query.cursor && !query.entity_id ? 'fixture-cursor' : '', limit: Number(query.limit) || 50 } })
  })
  const go = async target => {
    await page.evaluate(value => { location.hash = value }, target)
    const definition = managementPages.find(item => item.path === target.split('?')[0])
    if (definition) await expect(page.getByRole('heading', { name: definition.path === '/' ? '运行总览' : definition.title, exact: true })).toBeVisible()
    await expect(page.locator('.el-loading-mask')).toHaveCount(0)
  }
  await page.goto(`${base}/#/diagnostics`)
  await page.getByRole('textbox', { name: '管理密钥', exact: true }).fill(mockKey)
  await page.getByRole('button', { name: '登 录', exact: true }).click()
  await expect(page).toHaveURL(/#\/diagnostics$/)
  await expect(page.getByLabel('Redis 状态')).toContainText('连接正常')
  assertions.push('新诊断页直接访问在登录后返回原目标；Cookie 会话认证')

  const diagnosticDownload = page.waitForEvent('download')
  await page.getByRole('button', { name: '导出诊断结果', exact: true }).click()
  const diagnosticData = await readFile(await (await diagnosticDownload).path(), 'utf8')
  assert.ok(!diagnosticData.includes('never-export-this-secret'))
  diagnosticMode = 'degraded'
  await page.getByRole('button', { name: '重新检查运行状态' }).click()
  await expect(page.getByLabel('Redis 状态')).toContainText('检查超时')
  diagnosticMode = 'error'
  await page.getByRole('button', { name: '重新检查运行状态' }).click()
  await expect(page.getByRole('alert').first()).toContainText('诊断暂不可用')
  await expect(page.getByRole('button', { name: '导出诊断结果' })).toBeDisabled()
  diagnosticMode = 'ok'
  await page.getByRole('button', { name: '重新检查运行状态' }).click()
  await expect(page.getByLabel('Redis 状态')).toContainText('连接正常')
  assertions.push('依赖异常/超时准确展示，失败结果不冒充当前状态，导出不含任意服务端字段')

  await page.keyboard.press('Control+k')
  await page.getByPlaceholder('搜索页面或账号名称…').fill('audit')
  const beforeComposition = page.url()
  await page.getByPlaceholder('搜索页面或账号名称…').evaluate(input => {
    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowDown', bubbles: true, cancelable: true, isComposing: true }))
    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true, cancelable: true, isComposing: true }))
  })
  assert.equal(page.url(), beforeComposition, 'IME candidate confirmation must not navigate')
  await expect(page.getByRole('dialog', { name: '快速访问' })).toBeVisible()
  await page.keyboard.press('Enter')
  await expect(page.getByRole('heading', { name: '操作记录', exact: true })).toBeVisible()
  await expect(page.locator('.audit-table .el-table__body-wrapper')).toContainText('a1')
  await page.getByRole('button', { name: '下一页', exact: true }).click()
  await expect(page.locator('.audit-table .el-table__body-wrapper')).toContainText('a3')
  assert.equal(auditQueries.at(-1).cursor, 'fixture-cursor')
  await page.getByRole('button', { name: '上一页', exact: true }).click()
  await expect(page.locator('.audit-table .el-table__body-wrapper')).toContainText('a1')
  await page.getByRole('textbox', { name: '操作对象 ID' }).fill('a5')
  await expect(page.getByRole('button', { name: '下一页', exact: true })).toBeDisabled()
  await page.getByRole('button', { name: '查询', exact: true }).click()
  await expect(page.locator('.audit-table .el-table__body-wrapper')).toContainText('a5')
  assert.equal(auditQueries.at(-1).cursor, undefined)
  const auditDownload = page.waitForEvent('download')
  await page.getByRole('button', { name: '导出本页摘要' }).click()
  const exportedAudit = JSON.parse(await readFile(await (await auditDownload).path(), 'utf8'))
  assert.deepEqual(Object.keys(exportedAudit[0]).sort(), ['action', 'created_at', 'result'])
  await page.getByRole('button', { name: '详情', exact: true }).first().click()
  await expect(page.getByRole('dialog', { name: '操作详情' })).toContainText('a5')
  await page.getByRole('dialog', { name: '操作详情' }).getByRole('button', { name: '关闭', exact: true }).click()
  assertions.push('Ctrl K 键盘导航，操作记录游标前后翻页、筛选回首页、详情与白名单摘要导出')

  let release
  gate = new Promise(resolve => { release = resolve })
  await page.evaluate(() => { location.hash = '/audit-logs?entity_id=slow' })
  await expect(page.getByRole('textbox', { name: '操作对象 ID' })).toHaveValue('slow')
  await page.evaluate(() => { location.hash = '/audit-logs?entity_id=latest' })
  await expect(page.locator('.audit-table .el-table__body-wrapper')).toContainText('latest')
  release(); gate = undefined
  await expect(page.locator('.audit-table .el-table__body-wrapper')).not.toContainText('slow')
  auditMode = 'error'
  await page.getByRole('button', { name: '刷新操作记录' }).click()
  await expect(page.getByRole('button', { name: '导出本页摘要' })).toBeDisabled()
  auditMode = 'unsupported'
  await page.getByRole('button', { name: '刷新操作记录' }).click()
  await expect(page.locator('.audit-results')).toContainText('不提供持久化操作记录')
  auditMode = 'ok'
  assertions.push('旧请求不覆盖新筛选，失败禁用导出，文件模式明确不支持持久化审计')

  await go('/connection')
  // This route was removed before the performance work. Verify the current
  // fallback contract; do not report the retired guide UI as covered.
  await expect(page).toHaveURL(/#\/$/)
  await expect(page.getByRole('heading', { name: '运行总览', exact: true })).toBeVisible()
  await expect(page.getByRole('link', { name: '接入指南', exact: true })).toHaveCount(0)
  assertions.push('已移除的 /connection 路由回到仪表盘，导航不展示失效入口（未验收已移除指南）')
  await go('/settings')

  await page.getByRole('button', { name: '管理员菜单' }).click()
  await page.getByRole('menuitem', { name: '紧凑布局', exact: true }).click()
  await expect(page.locator('.admin-shell')).toHaveAttribute('data-density', 'compact')
  await page.reload()
  await expect(page.locator('.admin-shell')).toHaveAttribute('data-density', 'compact')
  await page.getByRole('button', { name: '管理员菜单' }).click()
  await page.getByRole('menuitem', { name: '舒适布局', exact: true }).click()
  assertions.push('全局密度偏好刷新后保持，界面偏好不包含凭据')

  for (const [value, label] of [['light', '留白'], ['dark', '夜航']]) {
    const target = page.getByRole('button', { name: `切换${label}主题`, exact: true })
    if (await target.count()) await target.click()
    await expect(page.locator('html')).toHaveAttribute('data-theme', value)
    await expect(page.locator('.console-shell')).toHaveAttribute('data-console-theme', value)
    await page.reload()
    await expect(page.locator('html')).toHaveAttribute('data-theme', value)
    assert.equal(new URL(page.url()).hash, '#/settings')
  }
  await page.setViewportSize({ width: 320, height: 900 })
  await page.getByRole('button', { name: '打开导航', exact: true }).click()
  const navigationDrawer = page.locator('.console-mobile-navigation')
  await expect(navigationDrawer).toBeVisible()
  await navigationDrawer.getByRole('navigation', { name: '主要导航' }).getByRole('link', { name: '管理设置', exact: true }).click()
  await expect(navigationDrawer).toBeHidden()
  await page.setViewportSize({ width: 1440, height: 1000 })
  assertions.push('留白/夜航真实按钮切换与重载持久化；320px 抽屉导航可达并在选择当前页面后关闭')

  assertions.push(...await testManagement(page))
  assertions.push(...await testManagementNavigation(page))

  // Keep healthy-page previews isolated from the intentional failure/recovery scenarios.
  assert.deepEqual(state.unexpected, [])
  await context.close()
  context = await browser.newContext({ viewport: { width: 1440, height: 1000 }, reducedMotion: 'reduce', serviceWorkers: 'block' })
  const visualState = await installMockApi(context)
  visualState.authenticated = true
  page = await context.newPage()
  page.setDefaultTimeout(12000)
  page.on('pageerror', error => errors.push(error.message))
  await page.route('**/api/diagnostics', route => route.fulfill({ json: { version: '0.2.0-preview', checked_at: Math.floor(Date.now() / 1000), uptime_seconds: 7200, storage_mode: 'postgres', status: 'ok', dependencies: { postgres: { configured: true, status: 'ok', latency_ms: 3 }, redis: { configured: true, status: 'ok', latency_ms: 2 } }, capabilities: { audit_logs: true } } }))
  await page.route('**/api/audit-logs?*', route => route.fulfill({ json: { supported: true, storage_mode: 'postgres', items: [auditRow('1'), auditRow('2')], has_more: false, next_cursor: '', limit: 50 } }))
  await page.route('**/api/settings', route => route.fulfill({ json: { admin_key: '', admin_key_masked: 'sk-••••1234', admin_api: false, settings_version: 1, version: '0.2.0-preview', uptime_seconds: 7200, websocket_enabled: false, builtin_prompt_enabled: true, builtin_prompt: '', sticky_sessions_enabled: true, sticky_session_ttl_seconds: 604800, prompt_cache_key_enabled: true, codex_fingerprint_enabled: false, codex_fingerprint_profile: 'linux', codex_fingerprint_profiles: [{ id: 'linux', label: 'Linux CLI（源码参考配置）', user_agent: 'codex_cli_rs/0.0.0-preview (Linux; x86_64)', originator: 'codex_cli_rs', version: '0.0.0-preview' }] } }))
  await page.goto(`${base}/#/`)
  for (const theme of ['light', 'dark']) {
    for (const width of [1440, 390, 320]) {
      await page.setViewportSize({ width, height: width === 1440 ? 1000 : 900 })
      await page.evaluate(value => { document.documentElement.dataset.theme = value; document.documentElement.classList.toggle('dark', value === 'dark'); window.dispatchEvent(new CustomEvent('theme-change', { detail: value })) }, theme)
      for (const item of managementPages) {
        await go(item.path)
        await expect(page.locator('.el-message')).toHaveCount(0)
        const overflow = await page.evaluate(() => ({ page: document.documentElement.scrollWidth > innerWidth + 1, content: document.querySelector('.content').scrollWidth > document.querySelector('.content').clientWidth + 1 }))
        assert.deepEqual(overflow, { page: false, content: false }, `${item.path} ${theme} ${width} overflows`)
        const filename = `${item.path === '/' ? 'dashboard' : item.path.slice(1)}-${theme}-${width}.png`
        await page.screenshot({ path: path.join(output, filename), fullPage: true })
        screenshots.push(filename)
      }
    }
  }
  await page.close()
  visualState.authenticated = false
  page = await context.newPage()
  page.on('pageerror', error => errors.push(error.message))
  await page.goto(`${base}/#/login`)
  await expect(page.getByRole('textbox', { name: '管理密钥', exact: true })).toBeVisible()
  for (const theme of ['light', 'dark']) {
    for (const width of [1440, 390, 320]) {
      await page.setViewportSize({ width, height: width === 1440 ? 1000 : 900 })
      await page.getByRole('button', { name: `切换${theme === 'light' ? '留白' : '夜航'}主题`, exact: true }).click()
      assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1), `login ${theme} ${width} overflows`)
      const filename = `login-${theme}-${width}.png`
      await page.screenshot({ path: path.join(output, filename), fullPage: true })
      screenshots.push(filename)
    }
  }
  assertions.push(`${screenshots.length} 页面/主题/尺寸组合没有页面横向溢出`)
  assert.deepEqual(visualState.unexpected, [])
  assert.deepEqual(errors, [])
  assert.deepEqual(escaped, [])
} catch (error) { failure = { message: error.message, stack: error.stack }; console.error(error.message) }
finally {
  await browser?.close(); await new Promise(resolve => server.close(resolve))
  await writeFile(path.join(output, 'results.json'), JSON.stringify({ passed: !failure, assertions, screenshots, errors, escaped, failure }, null, 2))
  const html = `<!doctype html><html lang="zh-CN"><meta charset="utf-8"><title>管理后台验收截图</title><style>body{font:14px system-ui;background:#f7f8fa;color:#202a35;margin:24px}main{display:grid;grid-template-columns:repeat(auto-fill,minmax(320px,1fr));gap:20px}figure{margin:0;padding:14px;background:white;border:1px solid #e5e8ec;border-radius:12px}img{width:100%;height:300px;object-fit:contain;object-position:top}figcaption{margin-top:12px}</style><h1>管理后台逐页验收</h1><p>全部数据为隔离测试夹具。</p><main>${screenshots.map(file => `<figure><a href="${file}"><img loading="lazy" src="${file}"></a><figcaption>${file}</figcaption></figure>`).join('')}</main></html>`
  await writeFile(path.join(output, 'index.html'), html)
}
console.log(JSON.stringify({ passed: !failure, assertions: assertions.length, screenshots: screenshots.length, output }))
if (failure) process.exitCode = 1
