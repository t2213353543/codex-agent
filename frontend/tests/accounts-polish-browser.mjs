import assert from 'node:assert/strict'
import { createServer } from 'node:http'
import { readFile, mkdir, writeFile } from 'node:fs/promises'
import { fileURLToPath } from 'node:url'
import path from 'node:path'
import { chromium, expect } from 'playwright/test'
import { installMockApi } from './mock-api.mjs'
import { testAccounts } from './accounts-browser.mjs'

const output = fileURLToPath(new URL('../../.local/ui-polish-20260918/accounts/', import.meta.url))
const root = process.env.ACCOUNT_UI_DIST || path.join(output, 'dist')
await mkdir(output, { recursive: true })
const leakedApi = []
const server = createServer(async (request, response) => {
  const pathname = new URL(request.url, 'http://localhost').pathname
  if (pathname.startsWith('/api/')) { leakedApi.push(pathname); response.writeHead(500).end('Mock required'); return }
  try {
    const filename = pathname === '/' ? 'index.html' : decodeURIComponent(pathname).replace(/^\/+/, '')
    const resolved = path.resolve(root, filename)
    if (!resolved.startsWith(path.resolve(root) + path.sep) && resolved !== path.join(root, 'index.html')) throw new Error('Outside dist')
    const data = await readFile(resolved)
    const type = ({ '.html': 'text/html', '.js': 'text/javascript', '.css': 'text/css', '.svg': 'image/svg+xml' })[path.extname(resolved)] || 'application/octet-stream'
    response.writeHead(200, { 'Content-Type': type, 'Cache-Control': 'no-store' }).end(data)
  } catch { response.writeHead(404).end('Not found') }
})
await new Promise(resolve => server.listen(0, '127.0.0.1', resolve))
const base = `http://127.0.0.1:${server.address().port}`
let browser
let failure
const errors = []
const assertions = []
const screenshots = []
try {
  browser = await chromium.launch({ headless: true, channel: process.env.TEST_BROWSER || 'chrome' })
  const context = await browser.newContext({ viewport: { width: 1440, height: 1080 }, serviceWorkers: 'block', acceptDownloads: true, reducedMotion: 'reduce' })
  const state = await installMockApi(context)
  state.authenticated = true
  const page = await context.newPage()
  page.setDefaultTimeout(10000)
  page.on('pageerror', error => errors.push(error.message))
  page.on('console', message => { if (message.type() === 'warning' && /Failed to resolve component|Invalid prop|Unhandled error/.test(message.text())) errors.push(message.text()) })
  const go = async (query = '') => {
    await page.goto(`${base}/#/accounts${query}`)
    await expect(page.locator('.accounts-page')).toHaveAttribute('aria-busy', 'false')
  }
  const capture = async (name) => {
    await page.evaluate(() => document.fonts.ready)
    const overflow = await page.evaluate(() => {
      const content = document.querySelector('.content')
      return { document: document.documentElement.scrollWidth - innerWidth, content: content.scrollWidth - content.clientWidth }
    })
    assert.ok(overflow.document <= 1 && overflow.content <= 1, `${name}: ${JSON.stringify(overflow)}`)
    await page.screenshot({ path: path.join(output, `${name}.png`), fullPage: true })
    screenshots.push(`${name}.png`)
  }
  await go()
  assertions.push(...await testAccounts(page, state))

  await go('?search=Alpha&status=unverified&group_id=g1&proxy_id=p1&sort=name-asc&cursor=mock-cursor-page-2&page=2')
  await expect(page.locator('.filter-chip')).toHaveCount(5)
  if (await page.locator('#account-advanced-filters').isVisible()) await page.getByRole('button', { name: '更多筛选', exact: true }).click()
  await expect(page.locator('#account-advanced-filters')).toBeHidden()
  const groupChip = page.getByRole('button', { name: '移除筛选 分组：测试分组', exact: true })
  await groupChip.focus()
  await page.keyboard.press('Enter')
  await expect.poll(() => new URLSearchParams(page.url().split('?')[1]).has('group_id')).toBe(false)
  await expect.poll(() => new URLSearchParams(page.url().split('?')[1]).has('cursor')).toBe(false)
  assert.equal(new URLSearchParams(page.url().split('?')[1]).get('status'), 'unverified')
  await expect(page.locator('.filter-chip')).toHaveCount(4)
  assertions.push('高级条件收起仍可见筛选 chip；键盘移除单项保留其他筛选并重置游标')

  for (const theme of ['light', 'dark']) {
    await page.evaluate(value => localStorage.setItem('theme', JSON.stringify({ version: 1, value })), theme)
    for (const width of [1440, 390, 320]) {
      await page.setViewportSize({ width, height: width < 768 ? 1180 : 1000 })
      await go()
      await page.reload()
      await expect(page.locator('.accounts-page')).toHaveAttribute('aria-busy', 'false')
      await expect(page.locator('.account-overview-card')).toHaveCount(4)
      if (width < 768) {
        await expect(page.locator('.mobile-account')).toHaveCount(2)
        await expect(page.locator('.accounts-panel .el-table')).toHaveCount(0)
        await expect(page.locator('.mobile-account').first().getByRole('button', { name: '编辑', exact: true })).toBeVisible()
      } else {
        await expect(page.locator('.accounts-panel .el-table tbody tr')).toHaveCount(2)
        const heights = await page.locator('.accounts-panel .el-table tbody tr').evaluateAll(rows => rows.map(row => row.getBoundingClientRect().height))
        assert.ok(heights.every(height => height < 100), JSON.stringify(heights))
      }
      await capture(`accounts-${theme}-${width}`)
    }
  }
  assertions.push('浅色/深色 × 1440/390/320px：桌面全部配置列与手机可操作列表，无整页横向溢出')

  await page.setViewportSize({ width: 390, height: 1180 })
  await go()
  const first = page.locator('.mobile-account').first()
  await first.locator('.el-checkbox').click()
  await expect(page.locator('.account-summary .el-checkbox__input')).toHaveClass(/is-indeterminate/)
  await expect(page.getByRole('button', { name: '批量管理（1）', exact: true })).toBeEnabled()
  await page.locator('.account-summary .el-checkbox').click()
  await expect(page.getByRole('button', { name: '批量管理（2）', exact: true })).toBeEnabled()
  await page.getByRole('button', { name: '批量管理（2）', exact: true }).click()
  const bulk = page.getByRole('dialog', { name: '批量管理', exact: true })
  await bulk.getByRole('button', { name: '执行批量操作', exact: true }).click()
  await expect(bulk.locator('.result-table')).toContainText('成功')
  await expect(bulk.locator('.result-table')).toContainText('版本冲突')
  const batch = state.requests.filter(request => request.path === '/accounts/batch').at(-1)
  assert.deepEqual(batch.body.items.map(item => item.id), ['a1', 'a2'])
  assert.ok(batch.body.items.every(item => Number.isInteger(item.version)))
  await bulk.getByRole('button', { name: '关闭', exact: true }).click()
  await expect(page.getByRole('button', { name: '批量管理（0）', exact: true })).toHaveCount(0)
  await first.locator('.el-checkbox').click()
  await page.getByRole('button', { name: '下一页', exact: true }).click()
  await expect(page.locator('.mobile-account')).toHaveCount(1)
  await expect(page.getByRole('button', { name: '批量管理（0）', exact: true })).toHaveCount(0)
  assertions.push('手机单选/全选/半选/批量提交沿用稳定 ID 与版本；逐项冲突反馈；翻页清除旧选择')

  await go()
  await first.getByRole('button', { name: '编辑', exact: true }).click()
  const edit = page.getByRole('dialog', { name: /^编辑账号/ })
  await edit.getByRole('textbox', { name: '账号备注' }).fill('手机端维护备注')
  await edit.getByRole('button', { name: '保存', exact: true }).click()
  await expect(edit).toBeHidden()
  assert.equal(state.requests.filter(request => request.path === '/accounts/a1' && request.method === 'PUT').at(-1).body.notes, '手机端维护备注')
  await first.getByRole('button', { name: /的更多操作$/ }).focus()
  await page.keyboard.press('Enter')
  await page.getByRole('menuitem', { name: '查看完整详情', exact: true }).click()
  const detail = page.getByRole('dialog', { name: '账号详情', exact: true })
  await expect(detail).toContainText('账号 ID / 版本')
  await expect(detail).toContainText('优先级 / 权重')
  await capture('account-mobile-detail')
  await detail.locator('.el-dialog__headerbtn').click()
  await first.getByRole('button', { name: /的更多操作$/ }).click()
  await page.getByRole('menuitem', { name: '刷新令牌', exact: true }).click()
  await expect.poll(() => state.requests.filter(request => request.path === '/accounts/refresh').at(-1)?.body.id).toBe('a1')
  await first.getByRole('button', { name: '使用记录', exact: true }).click()
  await expect.poll(() => page.url()).toContain('account_id=a1')
  assertions.push('手机编辑、键盘更多菜单、完整详情、刷新令牌、稳定 ID 使用记录跳转可用')

  for (const width of [1440, 390]) {
    await page.setViewportSize({ width, height: 1000 })
    const query = '?search=does-not-exist'
    await go(query)
    await expect(page.locator('.accounts-empty')).toContainText('没有匹配的账号')
    const handler = route => route.fulfill({ status: 503, json: { error: { message: '模拟读取失败' } } })
    await page.route('**/api/accounts?*', handler)
    await page.reload()
    await expect(page.locator('.accounts-page')).toHaveAttribute('aria-busy', 'false')
    await expect(page.locator('.accounts-empty')).toContainText('账号暂时无法加载')
    await expect(page.locator('.accounts-empty')).not.toContainText('还没有导入账号')
    await expect(page.locator('.account-overview-card strong')).toHaveText(['—', '—', '—', '—'])
    await expect(page.locator('.account-pagination')).toContainText('分页信息暂不可用')
    await capture(`accounts-failed-${width}`)
    await page.unroute('**/api/accounts?*', handler)
    await page.locator('.accounts-empty').getByRole('button', { name: '重新加载', exact: true }).click()
    await expect(page.locator('.accounts-empty')).toContainText('没有匹配的账号')
    await page.locator('.accounts-empty').getByRole('button', { name: '清除筛选', exact: true }).click()
    await expect(page.locator(width < 768 ? '.mobile-account' : '.accounts-panel .el-table tbody tr')).toHaveCount(2)
  }
  assertions.push('桌面与手机读取失败不误报空库；重新加载恢复；无匹配可一键清除筛选')
  assert.deepEqual(errors, [])
  assert.deepEqual(state.unexpected, [])
  assert.deepEqual(leakedApi, [])
} catch (error) { failure = { message: error.message, stack: error.stack } }
finally {
  await browser?.close()
  await new Promise(resolve => server.close(resolve))
  const report = { status: failure ? 'fail' : 'pass', scope: 'local synthetic only', assertions, screenshots, errors, leakedApi, failure }
  await writeFile(path.join(output, 'results.json'), JSON.stringify(report, null, 2))
  console.log(JSON.stringify(report, null, 2))
  if (failure) process.exitCode = 1
}
