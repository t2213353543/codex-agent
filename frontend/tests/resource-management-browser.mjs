import assert from 'node:assert/strict'
import { createServer } from 'node:http'
import { readFile, mkdir, writeFile } from 'node:fs/promises'
import { fileURLToPath } from 'node:url'
import path from 'node:path'
import { chromium, expect } from 'playwright/test'

// Isolated static build and strict mock boundary: this runner cannot use accounts
// or credentials from the real management service.
const root = process.env.MANAGEMENT_UI_DIST ? path.resolve(process.env.MANAGEMENT_UI_DIST) : fileURLToPath(new URL('../../.local/resource-management/dist/', import.meta.url))
const output = process.env.RESOURCE_BROWSER_OUTPUT ? path.resolve(process.env.RESOURCE_BROWSER_OUTPUT) : fileURLToPath(new URL('../../.local/resource-management/browser/', import.meta.url))
await mkdir(output, { recursive: true })
const escaped = []
const server = createServer(async (request, response) => {
  const pathname = new URL(request.url, 'http://localhost').pathname
  if (pathname.startsWith('/api/')) { escaped.push(pathname); response.writeHead(500).end('Unmocked API'); return }
  try {
    const resolved = path.resolve(root, pathname === '/' ? 'index.html' : decodeURIComponent(pathname).replace(/^\/+/, ''))
    if (!resolved.startsWith(root)) throw new Error('Outside static root')
    const data = await readFile(resolved)
    const contentType = ({ '.html': 'text/html', '.js': 'text/javascript', '.css': 'text/css', '.svg': 'image/svg+xml' })[path.extname(resolved)] || 'application/octet-stream'
    response.writeHead(200, { 'Content-Type': contentType, 'Cache-Control': 'no-store' }).end(data)
  } catch { response.writeHead(404).end('Not found') }
})
await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve))
const base = process.env.RESOURCE_BROWSER_URL || `http://127.0.0.1:${server.address().port}`
let browser
const errors = []
const checks = []
const writes = []
const failedResources = new Set()
const screenshots = []
let maxConcurrent = 0
let concurrent = 0
let keyConflict = true
let slowProxyTest = false
let keys = [
  { id: 'a', name: '生产应用', key: 'FULL_KEY_SECRET', key_masked: 'sk-••••1034', version: 2, enabled: true, groups: ['g1'], requests: 1862, tokens: 234901, errors: 3, created_at: 1770000000, last_used_at: 1789633000 },
  { id: 'b', name: '内部开发', key_masked: 'sk-••••8821', version: 3, enabled: true, groups: ['g2'], requests: 829, tokens: 94208, errors: 0, created_at: 1780000000, last_used_at: 1789632800 },
  { id: 'c', name: '已停用测试', key_masked: 'sk-••••0001', version: 4, enabled: false, groups: [], requests: 12, tokens: 883, errors: 1, created_at: 1781000000, last_used_at: 0 },
]
let proxies = [
  { id: 'p1', name: '美国 · 西海岸', type: 'http', host: 'us-west.example.test', port: 8899, username: 'USER_SECRET', password: 'PASSWORD_SECRET', has_password: true, version: 9, enabled: true, accounts: 3 },
  { id: 'p2', name: '东京 · 开发节点', type: 'socks5', host: 'tokyo.example.test', port: 1080, username: '', has_password: false, version: 12, enabled: false, accounts: 1 },
]
let groups = [
  { id: 'g1', name: '生产服务', description: '面向外部应用的稳定账号资源', version: 5, accounts: 3, keys: 1, created_at: 1770000000 },
  { id: 'g2', name: '开发与测试', description: '团队开发、集成测试与日常验证', version: 6, accounts: 1, keys: 1, created_at: 1780000000 },
]
try {
  browser = await chromium.launch({ channel: process.env.TEST_BROWSER || 'chrome', headless: true })
  const context = await browser.newContext({ viewport: { width: 1440, height: 1080 }, serviceWorkers: 'block', acceptDownloads: true, reducedMotion: 'reduce' })
  await context.route('**/api/**', async (route) => {
    const url = new URL(route.request().url())
    const pathname = url.pathname
    if (!pathname.startsWith('/api/')) return route.continue()
    const json = (body, status = 200) => route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(body) })
    if (pathname === '/api/session') return json({ authenticated: true })
    if (failedResources.has(pathname) && route.request().method() === 'GET') return json({ error: { message: '模拟读取暂不可用' } }, 503)
    if (pathname === '/api/settings') return json({ version: '0.2.0-mock', settings_version: 1, admin_key_masked: 'sk-••••0000' })
    if (pathname === '/api/groups' && route.request().method() === 'GET') return json({ groups })
    if (pathname === '/api/keys' && route.request().method() === 'GET') return json({ keys })
    if (pathname === '/api/proxies' && route.request().method() === 'GET') return json({ proxies })
    if (pathname === '/api/proxies/test') {
      const { id } = route.request().postDataJSON()
      if (slowProxyTest) await new Promise((resolve) => setTimeout(resolve, 1500))
      return json({ ok: true, http_code: 200, ip: id === 'p2' ? 'not-an-ip' : '192.0.2.24', latency_ms: 81, connection: 'ok', authentication: 'ok', http: 'ok', exit_ip: id === 'p2' ? 'failed' : 'ok', stage: 'complete', checked_at: 1789707600 })
    }
    if (['/api/groups', '/api/keys', '/api/proxies'].includes(pathname) && route.request().method() === 'POST') {
      const body = route.request().postDataJSON()
      const kind = pathname.split('/')[2]
      const row = { id: `new-${kind}`, ...body, enabled: true, version: 1, created_at: 1789707600, accounts: 0, keys: 0, requests: 0, tokens: 0, errors: 0 }
      if (kind === 'keys') { row.key_masked = 'sk-••••new1'; row.key = 'NEW_ONLY_ONCE_SECRET'; keys.push(row) }
      if (kind === 'groups') groups.push(row)
      if (kind === 'proxies') proxies.push(row)
      writes.push({ kind, method: 'POST', body })
      return json(kind === 'keys' ? { key: row.key } : { ok: true, id: row.id })
    }
    const groupMatch = pathname.match(/^\/api\/groups\/([^/]+)$/)
    if (groupMatch && route.request().method() === 'PUT') {
      const body = route.request().postDataJSON()
      const row = groups.find((item) => item.id === groupMatch[1])
      if (!row || row.version !== body.version) return json({ error: { message: 'version conflict' } }, 409)
      Object.assign(row, body, { version: row.version + 1 })
      writes.push({ kind: 'groups', method: 'PUT', body })
      return json({ ok: true, version: row.version })
    }
    if (groupMatch && route.request().method() === 'DELETE') {
      groups = groups.filter((row) => row.id !== groupMatch[1]); writes.push({ kind: 'groups', method: 'DELETE', id: groupMatch[1] }); return json({ ok: true })
    }
    if (pathname === '/api/accounts') return json({ accounts: [], pagination: { has_more: false, next_cursor: '' } })
    if (pathname === '/api/accounts/manage') return json({ accounts: [], total: 0, total_kind: 'exact', has_more: false, next_cursor: '' })
    if (pathname === '/api/overview') return json({ accounts: 0, keys: keys.length, groups: groups.length, proxies: proxies.length, series: [], recent_errors: [], account_status: [] })
    const match = pathname.match(/^\/api\/(keys|proxies)\/([^/]+)$/)
    if (match && route.request().method() === 'PUT') {
      const body = route.request().postDataJSON()
      writes.push({ kind: match[1], id: match[2], body })
      maxConcurrent = Math.max(maxConcurrent, ++concurrent)
      await new Promise((resolve) => setTimeout(resolve, 80))
      concurrent--
      const rows = match[1] === 'keys' ? keys : proxies
      const row = rows.find((item) => item.id === match[2])
      if (match[1] === 'keys' && row.id === 'b' && keyConflict) {
        keyConflict = false
        row.version = 8
        return json({ error: { code: 'version_conflict', message: '模拟版本冲突' } }, 409)
      }
      if (body.version !== row.version) return json({ error: { message: 'version conflict' } }, 409)
      Object.assign(row, body, { version: row.version + 1 })
      return json({ ok: true, version: row.version })
    }
    escaped.push(`${route.request().method()} ${pathname}`)
    return json({ error: { message: 'Unexpected mock request' } }, 500)
  })
  const page = await context.newPage()
  page.setDefaultTimeout(10000)
  page.on('pageerror', (error) => { errors.push(error.message); console.error('Browser error:', error.message) })
  page.on('console', (message) => { if (message.type() === 'warning' && /Failed to resolve component|Invalid prop|Unhandled error/.test(message.text())) errors.push(message.text()) })
  let navigation = 0
  const go = async (name) => { await page.goto(`${base}/?case=${++navigation}#/${name}`); await page.locator('.resource-panel').waitFor(); await expect.poll(() => page.locator('.el-loading-mask').count()).toBe(0) }
  const chooseAll = () => page.locator('.resource-panel .el-table__header-wrapper .el-checkbox').click()
  const dialog = () => page.getByRole('dialog').filter({ hasText: '将停用选中的' })

  await go('keys')
  await chooseAll()
  await page.getByRole('button', { name: '批量停用', exact: true }).click()
  await dialog().getByRole('button', { name: '取消', exact: true }).click()
  assert.equal(writes.length, 0)
  await page.getByRole('button', { name: '批量停用', exact: true }).click()
  await dialog().getByRole('button', { name: '确认停用', exact: true }).evaluate((button) => { button.click(); button.click() })
  await expect(dialog()).toContainText('成功 2 项，失败 1 项')
  await page.screenshot({ path: path.join(output, 'key-batch-result-1440.png'), fullPage: true })
  screenshots.push('key-batch-result-1440.png')
  assert.equal(writes.length, 2)
  assert.equal(maxConcurrent, 1)
  assert.deepEqual(writes.map((item) => item.body), [{ enabled: false, version: 2 }, { enabled: false, version: 3 }])
  await dialog().getByRole('button', { name: '刷新失败项', exact: true }).click()
  await expect(dialog()).toContainText('失败项已更新为最新状态')
  assert.equal(writes.length, 2)
  await dialog().getByRole('button', { name: '仅重试失败项', exact: true }).evaluate((button) => { button.click(); button.click() })
  await expect(dialog()).toContainText('成功 3 项，失败 0 项')
  assert.deepEqual(writes.at(-1), { kind: 'keys', id: 'b', body: { enabled: false, version: 8 } })
  assert.equal(writes.filter((item) => item.id === 'a').length, 1)
  checks.push('密钥取消零写入；双击只提交一次；顺序版本更新；冲突逐项呈现；刷新核对后仅重试失败项')
  await dialog().getByRole('button', { name: '关闭', exact: true }).click()

  await page.getByRole('textbox', { name: '搜索密钥', exact: true }).fill('生产')
  const downloadEvent = page.waitForEvent('download')
  await page.getByRole('button', { name: '导出筛选结果 CSV', exact: true }).click()
  const download = await downloadEvent
  const csv = await readFile(await download.path(), 'utf8')
  assert.ok(csv.includes('生产应用'))
  assert.ok(!csv.includes('内部开发'))
  assert.ok(!csv.includes('FULL_KEY_SECRET') && !csv.includes('1034'))
  await page.getByRole('button', { name: '生产应用', exact: true }).click()
  await expect(page.getByRole('dialog', { name: '密钥详情', exact: true })).toContainText('234.90K').catch(async () => {
    await expect(page.getByRole('dialog', { name: '密钥详情', exact: true })).toContainText('234.9K')
  })
  await page.screenshot({ path: path.join(output, 'key-details-1440.png'), fullPage: true })
  screenshots.push('key-details-1440.png')
  await page.getByRole('dialog', { name: '密钥详情', exact: true }).getByRole('button', { name: '关闭', exact: true }).click()
  await page.getByRole('textbox', { name: '搜索密钥', exact: true }).fill('不存在')
  await expect(page.locator('.resource-panel')).toContainText('没有符合筛选条件的密钥')
  await expect(page.getByRole('button', { name: '导出筛选结果 CSV', exact: true })).toBeDisabled()
  await page.getByRole('button', { name: '清除筛选', exact: true }).click()
  checks.push('密钥详情含访问范围与用量；CSV 仅导出筛选结果且排除完整 key 与片段；空态可恢复')

  await go('proxies')
  await page.locator('.el-switch').filter({ has: page.getByRole('switch', { name: '东京 · 开发节点 启用状态', exact: true }) }).click()
  await expect(page.getByRole('switch', { name: '东京 · 开发节点 启用状态', exact: true })).toBeChecked()
  assert.deepEqual(writes.at(-1), { kind: 'proxies', id: 'p2', body: { enabled: true, version: 12 } })
  await chooseAll()
  await page.getByRole('button', { name: '批量停用', exact: true }).click()
  await dialog().getByRole('button', { name: '确认停用', exact: true }).click()
  await expect(dialog()).toContainText('成功 2 项，失败 0 项')
  await dialog().getByRole('button', { name: '关闭', exact: true }).click()
  assert.deepEqual(writes.slice(-2).map((item) => item.body), [{ enabled: false, version: 9 }, { enabled: false, version: 13 }])
  const proxyDownloadEvent = page.waitForEvent('download')
  await page.getByRole('button', { name: '导出筛选结果 CSV', exact: true }).click()
  const proxyCSV = await readFile(await (await proxyDownloadEvent).path(), 'utf8')
  assert.ok(!proxyCSV.includes('_SECRET') && !proxyCSV.includes('example.test'))
  await page.getByRole('button', { name: '美国 · 西海岸', exact: true }).click()
  await expect(page.getByRole('dialog', { name: '代理详情', exact: true })).toContainText('已配置认证')
  assert.ok(!(await page.getByRole('dialog', { name: '代理详情', exact: true }).innerText()).includes('_SECRET'))
  await page.getByRole('button', { name: '查看绑定账号', exact: true }).click()
  await expect(page).toHaveURL(/accounts\?proxy_id=p1/)
  checks.push('代理单条与批量启停均携带版本；CSV 和详情无认证信息；绑定账号跳转带 proxy_id')

  await go('proxies')
  await page.locator('.resource-overview-item').filter({ hasText: '已停用' }).click()
  await page.getByRole('textbox', { name: '搜索代理', exact: true }).fill('东京')
  await page.evaluate(() => { location.hash = '/groups' })
  await page.getByRole('heading', { name: '分组管理', exact: true }).waitFor()
  await page.evaluate(() => { location.hash = '/proxies' })
  await expect(page.getByRole('textbox', { name: '搜索代理', exact: true })).toHaveValue('东京')
  await expect(page.locator('.resource-overview-item.is-active')).toContainText('已停用')
  await page.getByRole('button', { name: '清除筛选', exact: true }).click()
  checks.push('概览卡直接筛选启停状态；页面切换保留筛选条件，一键恢复全部资源')

  await go('groups')
  await page.getByRole('button', { name: '查看 生产服务 的密钥', exact: true }).click()
  await expect(page).toHaveURL(/keys\?group_id=g1/)
  await expect(page.locator('.resource-panel .el-table__body-wrapper tbody tr')).toHaveCount(1)
  await expect(page.locator('.resource-panel')).toContainText('生产应用')
  await go('groups')
  await page.getByRole('button', { name: '生产服务', exact: true }).click()
  await expect(page.getByRole('dialog', { name: '分组详情', exact: true })).toContainText('面向外部应用的稳定账号资源')
  await page.getByRole('button', { name: '查看关联账号', exact: true }).click()
  await expect(page).toHaveURL(/accounts\?group_id=g1/)
  checks.push('分组详情与关联资源入口；跳转密钥自动应用分组筛选；账号入口携带 group_id')

  // Test selection scope and client pagination with a meaningful multi-page list.
  keys = Array.from({ length: 25 }, (_, index) => ({ id: `page-${index}`, name: `分页密钥 ${String(index + 1).padStart(2, '0')}`, enabled: index % 2 === 0, version: 1, groups: [], key_masked: 'sk-••••0000', requests: 0, tokens: 0, errors: 0 }))
  await go('keys')
  if (await page.getByRole('button', { name: '清除筛选', exact: true }).count()) await page.getByRole('button', { name: '清除筛选', exact: true }).click()
  await expect(page.locator('.resource-panel .el-table__body-wrapper tbody tr')).toHaveCount(20)
  await chooseAll()
  await expect(page.locator('.resource-selection')).toContainText('已选 20 项')
  await page.locator('.resource-table-footer .btn-next').click()
  await expect(page.locator('.resource-panel .el-table__body-wrapper tbody tr')).toHaveCount(5)
  await expect(page.locator('.resource-selection')).toHaveCount(0)
  checks.push('资源分页只选择当前页；切页清空选择，避免操作隐藏资源')

  await page.evaluate(() => { document.querySelector('.admin-shell').dataset.density = 'compact' })
  const compactPadding = await page.locator('.resource-panel .el-table td').first().evaluate((cell) => parseFloat(getComputedStyle(cell).paddingTop))
  await page.evaluate(() => { document.querySelector('.admin-shell').dataset.density = 'comfortable' })
  const comfortablePadding = await page.locator('.resource-panel .el-table td').first().evaluate((cell) => parseFloat(getComputedStyle(cell).paddingTop))
  assert.ok(compactPadding > 0 && comfortablePadding > compactPadding, 'Compact density must make table rows shorter without hiding content')
  checks.push('资源表格遵循全局舒适 / 紧凑密度偏好')

  await expect(page.locator('.el-message')).toHaveCount(0)
  keys = [
    { id: 'mobile-a', name: '移动端生产应用', key_masked: 'sk-••••1024', version: 7, enabled: true, groups: ['g1'], requests: 182, tokens: 23040, errors: 0, created_at: 1780000000 },
    { id: 'mobile-b', name: '移动端开发应用', key_masked: 'sk-••••2048', version: 4, enabled: true, groups: ['g2'], requests: 16, tokens: 1200, errors: 2, created_at: 1780000000 },
  ]
  await page.setViewportSize({ width: 390, height: 1000 })
  await go('keys')
  await expect(page.locator('.resource-desktop-table')).toBeHidden()
  await expect(page.locator('.resource-mobile-card')).toHaveCount(2)
  await expect(page.getByRole('article', { name: '移动端生产应用', exact: true })).toContainText('最后使用：未记录')
  await page.locator('.resource-mobile-select .el-checkbox').click()
  await expect(page.getByRole('checkbox', { name: '选择本页', exact: true })).toBeChecked()
  await expect(page.locator('.resource-selection')).toContainText('已选 2 项')
  const mobileWritesBefore = writes.length
  await page.getByRole('button', { name: '批量停用', exact: true }).click()
  await dialog().getByRole('button', { name: '确认停用', exact: true }).click()
  await expect(dialog()).toContainText('成功 2 项，失败 0 项')
  await dialog().getByRole('button', { name: '关闭', exact: true }).click()
  assert.deepEqual(writes.slice(mobileWritesBefore).map((item) => item.body), [{ enabled: false, version: 7 }, { enabled: false, version: 4 }])
  await page.getByRole('textbox', { name: '搜索密钥', exact: true }).fill('无匹配')
  await expect(page.locator('.resource-selection')).toHaveCount(0)
  await expect(page.locator('.resource-mobile-list')).toContainText('没有符合筛选条件的密钥')
  await page.getByRole('button', { name: '重置筛选', exact: true }).click()
  await expect(page.locator('.resource-mobile-card')).toHaveCount(2)
  checks.push('手机卡片选择与批量启停沿用逐项版本校验；筛选清除隐藏选择，空态一键恢复')

  await page.getByRole('button', { name: '创建 KEY', exact: true }).click()
  const keyForm = page.getByRole('dialog', { name: '创建密钥', exact: true })
  await keyForm.getByRole('textbox', { name: '名称', exact: true }).fill('手机新密钥')
  await keyForm.getByRole('button', { name: '保存', exact: true }).click()
  const createdDialog = page.getByRole('dialog', { name: '密钥创建成功', exact: true })
  await expect(createdDialog.getByRole('textbox', { name: '新创建的完整密钥' })).toHaveValue('NEW_ONLY_ONCE_SECRET')
  await createdDialog.getByRole('button', { name: '我已保存', exact: true }).click()
  await expect(createdDialog).toBeHidden()
  assert.ok(!(await page.locator('.resource-panel').innerText()).includes('NEW_ONLY_ONCE_SECRET'))
  const keyCard = page.getByRole('article', { name: '手机新密钥', exact: true })
  await keyCard.getByRole('button', { name: '编辑', exact: true }).click()
  const editKeyForm = page.getByRole('dialog', { name: '编辑密钥', exact: true })
  await editKeyForm.getByRole('textbox', { name: '名称', exact: true }).fill('手机已编辑密钥')
  await editKeyForm.getByRole('button', { name: '保存', exact: true }).click()
  await expect(page.getByRole('article', { name: '手机已编辑密钥', exact: true })).toBeVisible()
  assert.deepEqual(writes.at(-1).body, { name: '手机已编辑密钥', enabled: true, groups: [], version: 1 })
  checks.push('手机创建与编辑 KEY 可完成；完整凭据仅创建弹窗显示，列表与持久化无明文')

  await go('proxies')
  const proxyCard = page.getByRole('article', { name: '美国 · 西海岸', exact: true })
  await expect(proxyCard).toContainText('尚未检测')
  await proxyCard.getByRole('button', { name: '测试', exact: true }).click()
  await expect(proxyCard).toContainText('192.0.2.24 · 81 ms')
  await expect(proxyCard).toContainText('2026')
  await proxyCard.getByRole('button', { name: '美国 · 西海岸', exact: true }).click()
  const proxyDrawer = page.getByRole('dialog', { name: '代理详情', exact: true })
  await expect(proxyDrawer).toContainText('本次检测通过')
  await expect(proxyDrawer).toContainText('检测时间')
  await proxyDrawer.getByRole('button', { name: '编辑代理', exact: true }).click()
  const proxyForm = page.getByRole('dialog', { name: '编辑代理', exact: true })
  await expect(proxyForm.getByRole('textbox', { name: '密码', exact: true })).toHaveValue('')
  await proxyForm.getByRole('textbox', { name: '主机', exact: true }).fill('updated.example.test')
  await proxyForm.getByRole('button', { name: '保存', exact: true }).click()
  await expect(proxyForm).toBeHidden()
  await expect(proxyCard).toContainText('尚未检测')
  assert.equal(writes.at(-1).body.host, 'updated.example.test')
  assert.ok(!Object.hasOwn(writes.at(-1).body, 'password'))
  await page.getByRole('article', { name: '东京 · 开发节点', exact: true }).getByRole('button', { name: '测试', exact: true }).click()
  await expect(page.getByRole('article', { name: '东京 · 开发节点', exact: true })).toContainText('检测未通过')
  checks.push('手机代理可测试、查看详情和编辑；旧密码不回填，配置保存即清除旧检测；无有效出口 IP 不显示通过')

  await go('groups')
  await page.getByRole('button', { name: '创建分组', exact: true }).click()
  const groupForm = page.getByRole('dialog', { name: '创建分组', exact: true })
  await groupForm.getByRole('textbox', { name: '名称', exact: true }).fill('移动端分组')
  await groupForm.getByRole('textbox', { name: '描述', exact: true }).fill('手机创建与修改演练')
  await groupForm.getByRole('button', { name: '保存', exact: true }).click()
  await expect(page.getByRole('article', { name: '移动端分组', exact: true })).toBeVisible()
  await page.getByRole('article', { name: '移动端分组', exact: true }).getByRole('button', { name: '编辑', exact: true }).click()
  const groupEdit = page.getByRole('dialog', { name: '编辑分组', exact: true })
  await groupEdit.getByRole('textbox', { name: '名称', exact: true }).fill('移动端已修改分组')
  await groupEdit.getByRole('button', { name: '保存', exact: true }).click()
  await expect(page.getByRole('article', { name: '移动端已修改分组', exact: true })).toBeVisible()
  assert.equal(writes.at(-1).body.version, 1)
  await page.getByRole('article', { name: '移动端已修改分组', exact: true }).getByRole('button', { name: '删除', exact: true }).click()
  await page.locator('.el-message-box').getByRole('button', { name: '删除', exact: true }).click()
  await expect(page.getByRole('article', { name: '移动端已修改分组', exact: true })).toHaveCount(0)
  checks.push('手机分组创建、版本编辑、确认删除均可直接操作')

  // Details use the same centered, keyboard-dismissible workflow on desktop and mobile.
  for (const [resource, kind, title] of [['keys', '密钥', '移动端生产应用'], ['groups', '分组', '生产服务'], ['proxies', '代理', '美国 · 西海岸']]) {
    for (const width of [1440, 390, 320]) {
      await page.setViewportSize({ width, height: 900 })
      await go(resource)
      await page.getByRole('button', { name: title, exact: true }).click()
      const detail = page.getByRole('dialog', { name: `${kind}详情`, exact: true })
      await expect(detail).toBeVisible()
      const box = await detail.boundingBox()
      assert.ok(Math.abs(box.x + box.width / 2 - width / 2) < 3, `${kind} details horizontally centered at ${width}`)
      assert.ok(box.y >= 0 && box.y + box.height <= 901, `${kind} details fit viewport at ${width}`)
      await expect(detail.locator('.el-dialog__footer')).toBeInViewport()
      await page.screenshot({ path: path.join(output, `${resource}-centered-${width}.png`), fullPage: true })
      screenshots.push(`${resource}-centered-${width}.png`)
      await page.keyboard.press('Escape')
      await expect(detail).toBeHidden()
    }
  }
  checks.push('3 类详情在桌面、390/320 手机宽度居中且按钮可见；Esc 关闭，不再从侧边弹出')

  await page.setViewportSize({ width: 1440, height: 1080 })
  for (const [resource, action, formName, closeKind] of [['groups', '创建分组', '创建分组', 'header'], ['keys', '创建 KEY', '创建密钥', 'cancel'], ['proxies', '添加代理', '添加代理', 'escape']]) {
    await go(resource)
    await page.getByRole('button', { name: action, exact: true }).click()
    const draft = page.getByRole('dialog', { name: formName, exact: true })
    await draft.getByRole('textbox', { name: '名称', exact: true }).fill('保留的草稿')
    if (closeKind === 'header') await draft.locator('.el-dialog__headerbtn').click()
    else if (closeKind === 'escape') await page.keyboard.press('Escape')
    else await draft.getByRole('button', { name: '取消', exact: true }).click()
    const confirmation = page.locator('.el-message-box')
    await expect(confirmation).toContainText('当前修改尚未保存')
    await confirmation.getByRole('button', { name: '继续编辑', exact: true }).click()
    await expect(draft.getByRole('textbox', { name: '名称', exact: true })).toHaveValue('保留的草稿')
    await draft.getByRole('button', { name: '取消', exact: true }).click()
    await confirmation.getByRole('button', { name: '放弃修改', exact: true }).click()
    await expect(draft).toBeHidden()
  }
  checks.push('3 类表单 X / Esc / 取消均保护未保存修改；继续编辑保留草稿，明确放弃后才关闭')

  await go('keys')
  await page.locator('.el-select').filter({ has: page.getByRole('combobox', { name: '密钥排序', exact: true }) }).click()
  await page.getByRole('option', { name: '失败数最多', exact: true }).click()
  await expect(page.locator('.resource-desktop-table tbody tr').first()).toContainText('移动端开发应用')
  await chooseAll()
  await page.locator('.el-select').filter({ has: page.getByRole('combobox', { name: '密钥排序', exact: true }) }).click()
  await page.getByRole('option', { name: '请求数最多', exact: true }).click()
  await expect(page.locator('.resource-selection')).toHaveCount(0)
  await expect(page.locator('.resource-desktop-table tbody tr').first()).toContainText('移动端生产应用')
  checks.push('密钥按失败/请求排序后展示正确，切换排序清除本页勾选')

  await page.locator('.el-select').filter({ has: page.getByRole('combobox', { name: '密钥分组筛选', exact: true }) }).click()
  await page.getByRole('option', { name: '生产服务', exact: true }).click()
  await page.getByRole('button', { name: '创建 KEY', exact: true }).click()
  const scopedKey = page.getByRole('dialog', { name: '创建密钥', exact: true })
  await expect(scopedKey).toContainText('生产服务')
  await expect(scopedKey.getByRole('textbox', { name: '名称', exact: true })).toBeFocused()
  const writesBeforeValidation = writes.length
  await scopedKey.getByRole('button', { name: '保存', exact: true }).click()
  await expect(scopedKey.locator('.el-form-item__error')).toContainText('请输入名称')
  await page.mouse.click(5, 5)
  await expect(scopedKey).toBeVisible()
  assert.equal(writes.length, writesBeforeValidation)
  await scopedKey.getByRole('button', { name: '取消', exact: true }).click()
  await page.getByRole('button', { name: '清除筛选', exact: true }).click()
  checks.push('创建 KEY 继承当前分组并聚焦名称；空名称就地提示、零写入；点击遮罩保留表单')

  await go('proxies')
  await page.getByRole('button', { name: '添加代理', exact: true }).click()
  const newProxy = page.getByRole('dialog', { name: '添加代理', exact: true })
  await newProxy.getByRole('textbox', { name: '名称', exact: true }).fill('验证主机输入')
  await newProxy.getByRole('textbox', { name: '主机', exact: true }).fill('http://wrong.example:8080')
  await newProxy.getByRole('button', { name: '保存', exact: true }).click()
  await expect(newProxy).toContainText('协议和端口请在对应字段填写')
  await expect(newProxy).toContainText('请输入 1–65535 之间的整数端口')
  assert.equal(writes.length, writesBeforeValidation)
  await newProxy.getByRole('button', { name: '取消', exact: true }).click()
  await page.locator('.el-message-box').getByRole('button', { name: '放弃修改', exact: true }).click()
  slowProxyTest = true
  await page.getByRole('button', { name: '美国 · 西海岸', exact: true }).click()
  const cancellableProxy = page.getByRole('dialog', { name: '代理详情', exact: true })
  await cancellableProxy.getByRole('button', { name: '测试连通性', exact: true }).click()
  await cancellableProxy.getByRole('button', { name: '取消检测', exact: true }).click()
  await expect(cancellableProxy.getByRole('button', { name: '测试连通性', exact: true })).toBeEnabled()
  slowProxyTest = false
  await cancellableProxy.getByRole('button', { name: '测试连通性', exact: true }).click()
  await expect(cancellableProxy).toContainText('本次检测通过')
  await cancellableProxy.getByRole('button', { name: '关闭', exact: true }).click()
  await page.locator('.el-select').filter({ has: page.getByRole('combobox', { name: '代理检测筛选', exact: true }) }).click()
  await page.getByRole('option', { name: '检测通过', exact: true }).click()
  await expect(page.locator('.resource-desktop-table tbody tr')).toHaveCount(1)
  await expect(page.locator('.resource-desktop-table tbody tr')).toContainText('美国 · 西海岸')
  checks.push('代理主机/端口就地校验；检测可取消后重试，结果筛选仅保留通过项')

  await expect(page.locator('.el-message')).toHaveCount(0)
  for (const name of ['keys', 'groups', 'proxies']) {
    for (const width of [1440, 768, 390, 320]) {
      await page.setViewportSize({ width, height: 1080 })
      await go(name)
      if (width <= 768) { await expect(page.locator('.resource-mobile-list')).toBeVisible(); await expect(page.locator('.resource-desktop-table')).toBeHidden() }
      else await expect(page.locator('.resource-desktop-table')).toBeVisible()
      await page.screenshot({ path: path.join(output, `${name}-${width}.png`), fullPage: true })
      screenshots.push(`${name}-${width}.png`)
      assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1), `${name} overflow at ${width}`)
    }
  }
  checks.push('3 个资源页面在 1440 / 768 / 390 / 320 像素下无整页横向溢出；桌面表格与手机卡片均可操作')
  await page.setViewportSize({ width: 390, height: 1000 })
  for (const [name, kind, action, formName] of [['groups', '分组', '创建第一个分组', '创建分组'], ['keys', '密钥', '创建第一个 KEY', '创建密钥'], ['proxies', '代理', '添加第一个代理', '添加代理']]) {
    failedResources.add(`/api/${name}`)
    await go(name)
    await expect(page.locator('.resource-mobile-list')).toContainText(`读取${kind}失败`)
    await expect(page.locator('.resource-overview')).toHaveCount(0)
    await expect(page.locator('.resource-mobile-list')).not.toContainText(`还没有${kind}`)
    failedResources.delete(`/api/${name}`)
    await page.getByRole('button', { name: '重新加载', exact: true }).click()
    await expect(page.locator('.resource-mobile-card').first()).toBeVisible()
    if (name === 'groups') groups = []
    if (name === 'keys') keys = []
    if (name === 'proxies') proxies = []
    await go(name)
    await expect(page.locator('.resource-mobile-list')).toContainText(`还没有${kind}`)
    await page.getByRole('button', { name: action, exact: true }).click()
    const form = page.getByRole('dialog', { name: formName, exact: true })
    await expect(form).toBeVisible()
    await form.getByRole('button', { name: '取消', exact: true }).click()
  }
  checks.push('资源首次读取失败不会冒充零数据：隐藏统计、显示读取失败并可重试；真实空数据有可用创建入口')
  assert.deepEqual(errors, [])
  assert.deepEqual(escaped, [])
  const storage = await page.evaluate(() => JSON.stringify({ local: { ...localStorage }, session: { ...sessionStorage } }))
  assert.ok(!storage.includes('_SECRET'))
  await writeFile(path.join(output, 'report.json'), JSON.stringify({ passed: true, checks, screenshots, apiWrites: writes, maxConcurrent, escaped, errors }, null, 2))
  console.log(JSON.stringify({ passed: true, checks: checks.length, screenshots: screenshots.length, maxConcurrent }))
} finally {
  await browser?.close()
  await new Promise((resolve) => server.close(resolve))
}
