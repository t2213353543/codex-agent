import assert from 'node:assert/strict'
import { spawn } from 'node:child_process'
import { createHash } from 'node:crypto'
import { createServer } from 'node:http'
import { mkdir, readFile, writeFile } from 'node:fs/promises'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { chromium, expect } from 'playwright/test'

const frontend = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const suppliedDist = process.env.MANAGEMENT_UI_DIST ? path.resolve(process.env.MANAGEMENT_UI_DIST) : ''
const stamp = new Date().toISOString().replace(/[-:.]/g, '')
const output = suppliedDist ? path.resolve(process.env.KEY_STORAGE_REPORT_DIR || path.join(path.dirname(suppliedDist), 'key-storage-browser')) : path.resolve(frontend, '../.local/gateway-key-auth-ui', stamp)
const dist = suppliedDist || path.join(output, 'dist')
await mkdir(output, { recursive: true })
const sourceHash = createHash('sha256').update(await readFile(path.join(frontend, 'src/utils/logIngressState.js'))).digest('hex')
const componentHash = createHash('sha256').update(await readFile(path.join(frontend, 'src/components/LogIngressErrorsDialog.vue'))).digest('hex')
let manifestHash = '', manifest
if (suppliedDist) {
  const raw = await readFile(path.join(path.dirname(dist), 'build-manifest.json')); manifest = JSON.parse(raw); manifestHash = createHash('sha256').update(raw).digest('hex')
  for (const [name, digest] of Object.entries(manifest.files)) assert.equal(createHash('sha256').update(await readFile(path.join(dist, name))).digest('hex'), digest)
} else {
const build = spawn(process.execPath, [path.join(frontend, 'node_modules/vite/bin/vite.js'), 'build', '--outDir', dist, '--emptyOutDir'], { cwd: frontend, windowsHide: true, stdio: 'pipe' })
let buildLog = ''
build.stdout.on('data', chunk => { buildLog += chunk })
build.stderr.on('data', chunk => { buildLog += chunk })
const buildExit = await new Promise((resolve, reject) => { build.once('error', reject); build.once('close', resolve) })
await writeFile(path.join(output, 'build.log'), buildLog)
assert.equal(buildExit, 0, `Isolated UI build failed; see ${output}`)
}

const leaked = [], external = [], unexpected = [], errors = [], requests = [], screenshots = [], viewports = []
const server = createServer(async (request, response) => {
  const pathname = new URL(request.url, 'http://localhost').pathname
  if (pathname.startsWith('/api/')) {
    leaked.push(pathname)
    response.writeHead(500).end('API escaped mock interception')
    return
  }
  try {
    const filename = pathname === '/' ? 'index.html' : decodeURIComponent(pathname).replace(/^\/+/, '')
    const resolved = path.resolve(dist, filename)
    const relative = path.relative(dist, resolved)
    if (relative.startsWith('..') || path.isAbsolute(relative)) throw new Error('Outside isolated dist')
    const mime = { '.html': 'text/html', '.js': 'text/javascript', '.css': 'text/css', '.svg': 'image/svg+xml' }[path.extname(resolved)] || 'application/octet-stream'
    response.writeHead(200, { 'Content-Type': mime, 'Cache-Control': 'no-store' }).end(await readFile(resolved))
  } catch { response.writeHead(404).end('Not found') }
})
await new Promise(resolve => server.listen(0, '127.0.0.1', resolve))
const origin = `http://127.0.0.1:${server.address().port}`
const now = Math.floor(Date.now() / 1000)
const reason = 'key_store_unavailable', label = '密钥授权存储不可用'
let browser, failure
try {
  browser = await chromium.launch({ headless: true, channel: process.env.TEST_BROWSER || 'chrome' })
  const context = await browser.newContext({ serviceWorkers: 'block', reducedMotion: 'reduce' })
  await context.route('**/*', async route => {
    const request = route.request(), url = new URL(request.url())
    if (url.origin !== origin) { external.push(url.origin); await route.abort(); return }
    if (!url.pathname.startsWith('/api/')) { await route.continue(); return }
    const query = Object.fromEntries(url.searchParams)
    requests.push({ path: url.pathname, method: request.method(), query })
    const expectedMethod = url.pathname === '/api/logs/snapshot' ? 'POST' : 'GET'
    if (request.method() !== expectedMethod || expectedMethod === 'GET' && request.postData() !== null) {
      unexpected.push(`${request.method()} ${url.pathname}`)
      await route.fulfill({ status: 400, json: { error: { message: 'Unexpected synthetic API method' } } })
      return
    }
    let json
    if (url.pathname === '/api/session') json = { authenticated: true, expires_at: now + 3600 }
    else if (url.pathname === '/api/settings') json = { version: 'synthetic-key-storage-ui', admin_api: true, timezone: 'UTC', uptime_seconds: 1 }
    else if (url.pathname === '/api/logs/snapshot') json = { as_of: 'synthetic-key-storage-snapshot', expires_at: now + 3600, total: 0 }
    else if (url.pathname === '/api/logs/stats') json = { total_requests: 0, success: 0, errors: 0, total_tokens: 0, total_input_tokens: 0, total_output_tokens: 0, total_cached_tokens: 0, models: [], series: [] }
    else if (url.pathname === '/api/logs') json = { items: [], total: 0, total_kind: 'exact', has_more: false, next_cursor: '' }
    else if (url.pathname === '/api/ingress-events') json = {
      scope: 'retained_ingress_events', source: 'gateway_ingress_v1', total: 1, total_kind: 'exact', has_more: false, next_cursor: '', checked_at: now, retention_limit: 1000, evicted_total: 0,
      collector: { configured: true, running: true, counter_scope: 'current_process', queue_capacity: 256, queued: 0, pending: 0, queue_dropped: 0 },
      items: [{ id: 'synthetic-key-storage-event', sequence: 1, time_ms: now * 1000, http_code: 503, reason, endpoint: '/responses', authentication_ms: 0, client_request_id: 'synthetic-key-storage-request' }],
    }
    else if (url.pathname === '/api/accounts') json = { accounts: [], total: 0, total_kind: 'exact', has_more: false, next_cursor: '' }
    else if (url.pathname === '/api/groups') json = { groups: [] }
    else if (url.pathname === '/api/keys') json = { keys: [] }
    else if (url.pathname === '/api/proxies') json = { proxies: [] }
    else { unexpected.push(url.pathname); await route.fulfill({ status: 404, json: { error: { message: 'Unexpected synthetic API route' } } }); return }
    await route.fulfill({ json })
  })
  for (const width of [1440, 390, 320]) {
    const page = await context.newPage({ viewport: { width, height: 1100 } })
    await page.setViewportSize({ width, height: 1100 })
    page.on('pageerror', error => errors.push(error.message))
    await page.goto(`${origin}/#/logs`)
    await expect(page.getByRole('button', { name: '准入错误', exact: true })).toBeVisible()
    await page.getByRole('button', { name: '准入错误', exact: true }).click()
    const dialog = page.getByRole('dialog', { name: '准入错误', exact: true })
    await expect(dialog.locator('.ingress-event')).toHaveCount(1)
    await expect(dialog.locator('.ingress-event-heading strong')).toHaveText(label)
    await expect(dialog.locator('.ingress-event-heading')).toContainText('HTTP 503')
    await expect(dialog.locator('.ingress-error')).toHaveCount(0)
    const select = dialog.getByRole('combobox', { name: '准入原因', exact: true })
    await expect(select.locator(`option[value="${reason}"]`)).toHaveText(label)
    await select.selectOption(reason)
    await dialog.getByRole('textbox', { name: '准入 HTTP 状态', exact: true }).fill('503')
    const before = requests.filter(request => request.path === '/api/ingress-events').length
    await dialog.getByRole('button', { name: '刷新准入事件', exact: true }).click()
    await expect.poll(() => requests.filter(request => request.path === '/api/ingress-events').length).toBe(before + 1)
    const refreshed = requests.filter(request => request.path === '/api/ingress-events').at(-1)
    assert.equal(refreshed.query.reason, reason)
    assert.equal(refreshed.query.status, '503')
    await expect(dialog.locator('.ingress-events')).toHaveAttribute('aria-busy', 'false')
    await expect(dialog.locator('.ingress-error')).toHaveCount(0)
    await dialog.locator('.ingress-event summary').click()
    await expect(dialog.locator('.ingress-event')).toContainText(reason)
    await page.evaluate(() => document.fonts.ready)
    const geometry = await dialog.evaluate(element => {
      const heading = element.querySelector('.ingress-event-heading strong')
      const range = document.createRange()
      range.selectNodeContents(heading)
      const parent = heading.getBoundingClientRect()
      const textFits = [...range.getClientRects()].every(rect => rect.left >= parent.left - 1 && rect.right <= parent.right + 1)
      const select = element.querySelector('select[aria-label="准入原因"]')
      const style = getComputedStyle(select), canvas = document.createElement('canvas'), painter = canvas.getContext('2d')
      painter.font = style.font
      const selectedLabelWidth = painter.measureText(select.selectedOptions[0].textContent).width
      const selectedLabelAvailableWidth = select.clientWidth - parseFloat(style.paddingLeft) - parseFloat(style.paddingRight) - 20
      return { documentOverflow: document.documentElement.scrollWidth - innerWidth, dialogOverflow: element.scrollWidth - element.clientWidth, headingTextFits: textFits, selectedLabelWidth, selectedLabelAvailableWidth, selectedLabelFits: selectedLabelWidth <= selectedLabelAvailableWidth, selectWidth: select.clientWidth }
    })
    assert.ok(geometry.documentOverflow <= 1 && geometry.dialogOverflow <= 1 && geometry.headingTextFits && geometry.selectedLabelFits, `UI overflow at ${width}: ${JSON.stringify(geometry)}`)
    const screenshot = `key-storage-${width}.png`
    await page.screenshot({ path: path.join(output, screenshot), fullPage: true })
    screenshots.push(screenshot)
    viewports.push({ width, ...geometry, rendered503: true, filterRefetched: true, validatorError: false })
    await dialog.getByRole('button', { name: '关闭准入错误', exact: true }).click()
    await page.close()
  }
  assert.deepEqual(errors, [])
  assert.deepEqual(external, [])
  assert.deepEqual(leaked, [])
  assert.deepEqual(unexpected, [])
  if (suppliedDist) {
    assert.equal(createHash('sha256').update(await readFile(path.join(path.dirname(dist), 'build-manifest.json'))).digest('hex'), manifestHash)
    for (const [name, digest] of Object.entries(manifest.files)) assert.equal(createHash('sha256').update(await readFile(path.join(dist, name))).digest('hex'), digest)
  }
} catch (error) { failure = error }
finally {
  await browser?.close()
  await new Promise(resolve => server.close(resolve))
  const report = { schema: 'gateway-key-storage-ui-v1', passed: !failure, reason, label, sourceSha256: sourceHash, componentSha256: componentHash, output, isolatedBuild: !suppliedDist, dist, manifest_sha256: manifestHash || undefined, sharedDistModified: false, serverClosed: true, allAPIsMocked: true, realAccounts: false, viewports, screenshots, requests, errors, external, leaked, unexpected, failure: failure?.message || null }
  await writeFile(path.join(output, 'report.json'), `${JSON.stringify(report, null, 2)}\n`)
  console.log(JSON.stringify({ passed: report.passed, output, screenshots }))
}
if (failure) throw failure
