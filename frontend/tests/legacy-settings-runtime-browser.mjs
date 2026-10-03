// Local synthetic-data acceptance for the restored shell plus complete Settings and the retained Logs route.
// Never connect this runner to a production server.
import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import { createServer } from 'node:http'
import { readFile, writeFile, mkdir } from 'node:fs/promises'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { chromium, expect } from 'playwright/test'
import { installMockApi } from './mock-api.mjs'
import { logWorkspacePreviewRows } from './log-workspace-fixtures.mjs'
import { legacyRelease, legacyMain } from '../scripts/legacy-logs-runtime/bridge.mjs'

const root = path.resolve(fileURLToPath(new URL('../..', import.meta.url)))
const folder = path.join(root, '.local/settings-restore')
const dist = path.join(folder, 'release-dist')
const output = path.join(folder, 'browser')
const sha = value => createHash('sha256').update(value).digest('hex')
const report = { status: 'running', started_at: new Date().toISOString(), synthetic_data: true,
  legacy_release: legacyRelease, pages: [], screenshots: [], errors: [], failed_requests: [], unmocked_api: [] }
await mkdir(output, { recursive: true })
const manifest = JSON.parse(await readFile(path.join(folder, 'build-manifest.json'), 'utf8'))
report.frontend_sha256 = manifest.frontend_sha256
report.source_files = manifest.source_files
for (const [name, checksum] of Object.entries(manifest.files)) assert.equal(sha(await readFile(path.join(dist, name))), checksum, `Build artifact changed: ${name}`)
assert.equal(manifest.unchanged_legacy_resources, 44)
assert.equal(manifest.unchanged_baseline_resources, 46)
assert.equal(manifest.shared_runtime, `/assets/${legacyMain}`)

let server, browser, context, current = 'startup'
const contentType = name => name.endsWith('.js') ? 'text/javascript' : name.endsWith('.css') ? 'text/css' : name.endsWith('.woff2') ? 'font/woff2' : 'text/html; charset=utf-8'
try {
  server = createServer(async (request, response) => {
    try {
      const pathname = decodeURIComponent(new URL(request.url, 'http://127.0.0.1').pathname)
      if (pathname.startsWith('/api/')) {
        report.unmocked_api.push(pathname)
        response.writeHead(500).end('API escaped local interception')
        return
      }
      const name = pathname === '/' ? 'index.html' : pathname.slice(1)
      const file = path.resolve(dist, name)
      if (file !== path.join(dist, 'index.html') && !file.startsWith(dist + path.sep)) throw new Error('invalid_path')
      const body = await readFile(file)
      response.writeHead(200, { 'Content-Type': contentType(name), 'Cache-Control': 'no-store' })
      response.end(body)
    } catch { response.writeHead(404); response.end('Not found') }
  })
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve))
  const origin = `http://127.0.0.1:${server.address().port}`
  browser = await chromium.launch({ headless: true, channel: process.env.TEST_BROWSER || 'chrome' })
  context = await browser.newContext({ viewport: { width: 1440, height: 1000 }, reducedMotion: 'reduce', acceptDownloads: true, serviceWorkers: 'block' })
  const state = await installMockApi(context)
  state.authenticated = true
  const page = await context.newPage()
  page.setDefaultTimeout(15000)
  const watchPage = watched => {
    watched.on('pageerror', error => report.errors.push({ phase: current, message: error.message }))
    watched.on('console', message => {
      if (message.type() === 'warning' && /Failed to resolve|Invalid prop|Unhandled error|inject\(\)|lifecycle/.test(message.text())) report.errors.push({ phase: current, message: message.text() })
    })
    watched.on('response', response => {
      const url = new URL(response.url())
      if (response.status() >= 400 && !url.pathname.startsWith('/api/')) report.failed_requests.push({ phase: current, path: url.pathname, status: response.status() })
    })
  }
  watchPage(page)
  const settle = async () => {
    await page.evaluate(() => document.fonts.ready)
    await expect(page.locator('.el-loading-mask:visible')).toHaveCount(0)
    await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))))
  }
  current = 'runtime-smoke'
  await page.goto(origin + '/#/settings')
  await expect(page.locator('.settings-panel')).toBeVisible()
  await settle()
  const runtime = await page.evaluate(async filename => {
    const shared = await import(`/assets/${filename}`)
    return { refs: typeof shared.j === 'function', router: typeof shared.e === 'function' && typeof shared.u === 'function', api: typeof shared.L?.logs === 'function', apps: document.querySelectorAll('#app').length }
  }, legacyMain)
  assert.deepEqual(runtime, { refs: true, router: true, api: true, apps: 1 })
  report.shared_runtime = runtime
  current = 'settings-restored-behavior'
  const { testSettingsRestored, settingsRestoredFixture } = await import('./settings-restored-browser.mjs')
  report.behavior = await testSettingsRestored(page)
  assert.ok(report.behavior, 'Settings behavior must return acceptance evidence')
  await page.route('**/api/settings', route => {
    assert.equal(route.request().method(), 'GET', 'Visual verification must not mutate settings')
    return route.fulfill({ json: settingsRestoredFixture })
  })
  // Use mixed, realistically sized synthetic records for visual acceptance.
  // The behavior suite owns separate fixtures and removes them before this.
  await page.route('**/api/logs?*', route => route.fulfill({ json: {
    items: logWorkspacePreviewRows, has_more: false, next_cursor: '',
    total_kind: 'not_computed', total: -1, page_size: 20,
  } }))
  report.preview_record_count = logWorkspacePreviewRows.length
  await page.evaluate(async filename => { (await import(`/assets/${filename}`)).k.closeAll() }, legacyMain)
  await expect(page.locator('.el-message:visible')).toHaveCount(0)

  const routes = [
    ['dashboard', '/', '.dashboard-page'], ['accounts', '/accounts', '.accounts-page'],
    ['groups', '/groups', '.content .panel'], ['keys', '/keys', '.content .panel'],
    ['proxies', '/proxies', '.content .panel'], ['logs', '/logs', '.logs-page'],
    ['settings', '/settings', '.settings-panel'],
  ]
  for (const theme of ['light', 'dark', 'ocean', 'mint']) {
    for (const width of [1440, 320]) {
      await page.setViewportSize({ width, height: width < 600 ? 844 : 1000 })
      for (const [name, route, selector] of routes) {
        current = `${name}-${theme}-${width}`
        await page.goto(origin + '/#' + route)
        await page.evaluate(value => {
          document.documentElement.dataset.theme = value
          document.documentElement.classList.toggle('dark', value === 'dark')
          localStorage.setItem('theme', JSON.stringify({ version: 1, value }))
          window.dispatchEvent(new CustomEvent('theme-change', { detail: value }))
        }, theme)
        await expect(page.locator(selector).first()).toBeVisible()
        await settle()
        assert.equal(await page.locator('#app').count(), 1)
        const layout = await page.evaluate(() => ({
          width: innerWidth, document_overflow: document.documentElement.scrollWidth - innerWidth,
          content_overflow: (() => { const content = document.querySelector('.content'); return content ? content.scrollWidth - content.clientWidth : 0 })(),
          theme: document.documentElement.dataset.theme,
        }))
        assert.ok(layout.document_overflow <= 1 && layout.content_overflow <= 1, `${current}: horizontal page overflow`)
        if (name === 'settings') {
          assert.equal(await page.locator('.settings-navigation').count(), 0, 'Restored Settings must keep the old panel layout')
          report.settings_control_counts ??= {}
          report.settings_control_counts[`${theme}-${width}`] = await page.getByRole('switch').count()
          assert.ok(report.settings_control_counts[`${theme}-${width}`] >= 13, 'Settings controls missing from old shell')
        }
        if (name === 'logs') {
          await expect(page.locator(width < 600 ? '.mobile-log-list' : '.log-table')).toBeVisible()
          assert.ok((await page.locator('.page-header-title').innerText()).includes('使用记录'))
        }
        const screenshot = `${current}.png`
        await page.screenshot({ path: path.join(output, screenshot), fullPage: true, animations: 'disabled' })
        report.pages.push({ name, route, theme, width, layout })
        report.screenshots.push(screenshot)
        if (name === 'settings') {
          const sections = page.locator('.settings-section')
          for (let i = 0; i < await sections.count(); i++) {
            await sections.nth(i).scrollIntoViewIfNeeded()
            const sectionShot = `settings-${theme}-${width}-section-${i + 1}.png`
            await page.screenshot({ path: path.join(output, sectionShot), animations: 'disabled' })
            report.screenshots.push(sectionShot)
          }
        }
      }
    }
  }

  // New context also proves the unchanged login page and default route guard.
  const loginContext = await browser.newContext({ reducedMotion: 'reduce' })
  const loginState = await installMockApi(loginContext)
  const loginPage = await loginContext.newPage()
  watchPage(loginPage)
  for (const theme of ['light', 'dark', 'ocean', 'mint']) for (const width of [1440, 320]) {
    current = `login-${theme}-${width}`
    await loginPage.setViewportSize({ width, height: width < 600 ? 844 : 1000 })
    await loginPage.goto(origin + '/#/login')
    await loginPage.evaluate(value => {
      document.documentElement.dataset.theme = value
      document.documentElement.classList.toggle('dark', value === 'dark')
    }, theme)
    await expect(loginPage.locator('input[type="password"]')).toBeVisible()
    await loginPage.evaluate(() => document.fonts.ready)
    assert.ok(await loginPage.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1))
    const screenshot = `${current}.png`
    await loginPage.screenshot({ path: path.join(output, screenshot), fullPage: true, animations: 'disabled' })
    report.pages.push({ name: 'login', theme, width })
    report.screenshots.push(screenshot)
  }
  report.unexpected_requests = [...state.unexpected, ...loginState.unexpected]
  assert.deepEqual(report.unexpected_requests, [])
  assert.deepEqual(report.errors, [])
  assert.deepEqual(report.failed_requests, [])
  assert.deepEqual(report.unmocked_api, [])
  report.verification_sources = {}
  for (const name of ['legacy-settings-runtime-browser.mjs', 'settings-restored-browser.mjs', 'mock-api.mjs', 'log-workspace-fixtures.mjs']) report.verification_sources[`frontend/tests/${name}`] = sha(await readFile(path.join(root, 'frontend/tests', name)))
  for (const [name, checksum] of Object.entries(manifest.files)) assert.equal(sha(await readFile(path.join(dist, name))), checksum, `Artifact changed during verification: ${name}`)
  for (const [name, checksum] of Object.entries(manifest.source_files)) assert.equal(sha(await readFile(path.join(root, name))), checksum, `Source changed during verification: ${name}`)
  report.status = 'pass'
  report.finished_at = new Date().toISOString()
} catch (error) {
  report.status = 'fail'
  report.failure = { phase: current, message: error.message }
  process.exitCode = 1
} finally {
  await browser?.close()
  if (server) await new Promise(resolve => server.close(resolve))
  await writeFile(path.join(folder, 'browser-report.json'), JSON.stringify(report, null, 2) + '\n')
}
console.log(JSON.stringify({ status: report.status, pages: report.pages.length, screenshots: report.screenshots.length, behavior: report.behavior, failure: report.failure, report: path.join(folder, 'browser-report.json') }))
