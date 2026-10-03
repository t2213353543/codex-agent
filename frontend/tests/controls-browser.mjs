import assert from 'node:assert/strict'
import { createServer } from 'node:http'
import { mkdir, readFile, writeFile } from 'node:fs/promises'
import { fileURLToPath } from 'node:url'
import path from 'node:path'
import { chromium, expect } from 'playwright/test'
import { installMockApi } from './mock-api.mjs'

const root = fileURLToPath(new URL('../dist/', import.meta.url))
const before = process.env.CONTROLS_BEFORE === '1'
const output = fileURLToPath(new URL(`../../.local/admin-ui-improvements/${before ? 'before' : 'after'}/`, import.meta.url))
await mkdir(output, { recursive: true })
const server = createServer(async (request, response) => {
  try {
    const pathname = decodeURIComponent(new URL(request.url, 'http://localhost').pathname)
    const file = path.resolve(root, pathname === '/' ? 'index.html' : pathname.replace(/^\/+/, ''))
    if (path.relative(root, file).startsWith('..') || pathname.startsWith('/api/')) throw new Error('Unexpected request')
    response.writeHead(200, { 'Content-Type': ({ '.html': 'text/html', '.css': 'text/css', '.js': 'text/javascript', '.svg': 'image/svg+xml' })[path.extname(file)] || 'application/octet-stream' }).end(await readFile(file))
  } catch { response.writeHead(404).end() }
})
await new Promise(resolve => server.listen(0, '127.0.0.1', resolve))
const origin = `http://127.0.0.1:${server.address().port}`
const browser = await chromium.launch({ headless: true, channel: process.env.TEST_BROWSER || 'chrome' })
const errors = [], checks = [], geometry = []
try {
  const context = await browser.newContext({ viewport: { width: 1440, height: 1000 }, reducedMotion: 'reduce' })
  const state = await installMockApi(context)
  state.authenticated = true
  await context.route('**/api/accounts/manage**', route => route.fulfill({ json: { accounts: state.accounts, total: state.accounts.length, total_kind: 'exact', has_more: false } }))
  await context.route('**/api/accounts/upstream/batch', route => route.fulfill({ json: { results: route.request().postDataJSON().items.map(row => ({ ...row, ok: true, snapshot: { id: row.id, ok: true, kind: 'usage', checked_at: 1800000000, data: { rate_limit: { primary_window: { limit_window_seconds: 18000, used_percent: 24, reset_at: 1800007200 }, secondary_window: { limit_window_seconds: 604800, used_percent: 48, reset_at: 1800259200 } } } } })) } }))
  await context.route('**/api/audit-logs?**', route => route.fulfill({ json: { supported: true, storage_mode: 'postgres', items: [], has_more: false, next_cursor: '', limit: 50 } }))
  const page = await context.newPage()
  page.setDefaultTimeout(10000)
  page.on('pageerror', error => errors.push(error.message))
  for (const theme of ['light', 'dark']) {
    await page.goto(`${origin}/#/accounts`)
    await page.evaluate(value => localStorage.setItem('theme', JSON.stringify({ version: 1, value })), theme)
    await page.reload()
    await expect(page.locator('html')).toHaveAttribute('data-theme', theme)
    await expect(page.locator('.accounts-page')).toHaveAttribute('aria-busy', 'false')
    if (!before) await expect(page.locator('.console-workspace')).toHaveCount(0)
    const input = page.locator('.account-search input')
    const search = await input.count() ? input : page.locator('.accounts-page .el-input__inner').first()
    await search.click()
    const focus = await search.evaluate(element => ({ innerOutline: getComputedStyle(element).outlineStyle, wrapperShadow: getComputedStyle(element.closest('.el-input__wrapper')).boxShadow }))
    geometry.push({ theme, focus })
    if (!before) assert.equal(focus.innerOutline, 'none', 'Inner input must not draw a second outline around only the text')
    await page.screenshot({ path: path.join(output, `input-focus-${theme}.png`) })
    await page.getByRole('button', { name: '显示列', exact: true }).click()
    const dialog = page.getByRole('dialog', { name: '显示 / 隐藏列' })
    await expect(dialog).toBeVisible()
    const ticks = await dialog.locator('.el-checkbox__input.is-checked .el-checkbox__inner').evaluateAll(elements => elements.map(element => {
      const inner = getComputedStyle(element), tick = getComputedStyle(element, '::after')
      return { width: element.clientWidth, height: element.clientHeight, left: parseFloat(tick.left), top: parseFloat(tick.top), color: tick.borderColor, disabled: element.parentElement.classList.contains('is-disabled') }
    }))
    geometry.push({ theme, ticks })
    if (!before) for (const tick of ticks) {
      assert.ok(Math.abs(tick.left - tick.width / 2) <= 0.5, 'Checkbox tick horizontally centered')
      assert.ok(Math.abs(tick.top - tick.height / 2) <= 0.5, 'Checkbox tick vertically centered')
    }
    await dialog.screenshot({ path: path.join(output, `column-checkbox-${theme}.png`) })
    await page.keyboard.press('Tab')
    const keyboardFocus = await dialog.getByRole('checkbox', { name: '状态', exact: true }).evaluate(element => {
      element.focus()
      return getComputedStyle(element.nextElementSibling).outlineStyle
    })
    if (!before) assert.equal(keyboardFocus, 'solid', 'Checkbox keyboard focus remains visible')
    await dialog.getByRole('button', { name: '完成', exact: true }).click()
    if (!before) {
      await page.getByRole('button', { name: '编辑', exact: true }).first().click()
      const edit = page.getByRole('dialog', { name: /^编辑账号：/ })
      const numeric = edit.getByRole('spinbutton', { name: '账号并发容量' })
      await numeric.click()
      assert.equal(await numeric.evaluate(element => getComputedStyle(element).outlineStyle), 'none')
      await edit.screenshot({ path: path.join(output, `numeric-focus-${theme}.png`) })
      const textarea = edit.getByRole('textbox', { name: '账号备注' })
      await textarea.click()
      assert.equal(await textarea.evaluate(element => getComputedStyle(element).outlineStyle), 'none')
      const select = edit.getByRole('combobox', { name: '账号标签' })
      await edit.locator('.el-select__wrapper').filter({ has: page.locator('input[aria-label="账号标签"]') }).click()
      assert.equal(await select.evaluate(element => getComputedStyle(element).outlineStyle), 'none')
      await edit.screenshot({ path: path.join(output, `select-focus-${theme}.png`) })
      await page.keyboard.press('Escape')
      await edit.getByRole('button', { name: '取消', exact: true }).click()
      await page.goto(`${origin}/#/audit-logs`)
      const date = page.getByRole('combobox', { name: '操作开始时间' })
      await date.click()
      assert.equal(await date.evaluate(element => getComputedStyle(element).outlineStyle), 'none')
      await page.screenshot({ path: path.join(output, `date-focus-${theme}.png`) })
    }
    checks.push(`${theme}: sidebar card removed; one input boundary; centered checked and disabled checkboxes; keyboard checkbox focus retained; number/textarea/select/date focus verified`)
  }
  assert.deepEqual(errors, [])
  assert.deepEqual(state.unexpected, [])
  await writeFile(path.join(output, 'results.json'), JSON.stringify({ passed: true, before, checks, geometry, errors }, null, 2))
  console.log(JSON.stringify({ passed: true, output, checks: checks.length }))
} finally { await browser.close(); await new Promise(resolve => server.close(resolve)) }
