import assert from 'node:assert/strict'
import { createServer } from 'node:http'
import { readFile, writeFile, mkdir } from 'node:fs/promises'
import { createHash } from 'node:crypto'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { chromium, expect } from 'playwright/test'
import { installMockApi } from './mock-api.mjs'
import { testAccountRuntime } from './account-runtime-browser.mjs'

const root = path.resolve(fileURLToPath(new URL('../..', import.meta.url)))
const dist = path.resolve(process.env.MANAGEMENT_UI_DIST || path.join(root, '.local/management-parity/dist'))
const output = path.resolve(process.env.MANAGEMENT_RUNTIME_REPORT_DIR || path.join(root, '.local/accounts-runtime/browser'))
await mkdir(output, { recursive: true })
const report = { status: 'running', synthetic_data: true, dist, started_at: new Date().toISOString(), errors: [], asset_sha256: {}, screenshots: [] }
const server = createServer(async (request, response) => {
  try {
    const pathname = decodeURIComponent(new URL(request.url, 'http://127.0.0.1').pathname)
    const file = path.resolve(dist, pathname === '/' ? 'index.html' : pathname.slice(1))
    if (!file.startsWith(dist + path.sep)) throw new Error('invalid_path')
    const body = await readFile(file)
    report.asset_sha256[path.relative(dist, file).replaceAll('\\', '/')] = createHash('sha256').update(body).digest('hex')
    response.writeHead(200, { 'Content-Type': file.endsWith('.js') ? 'text/javascript' : file.endsWith('.css') ? 'text/css' : 'text/html; charset=utf-8', 'Cache-Control': 'no-store' }); response.end(body)
  } catch { response.writeHead(404); response.end('Not found') }
})
await new Promise(resolve => server.listen(0, '127.0.0.1', resolve))
const browser = await chromium.launch({ headless: true, channel: process.env.TEST_BROWSER || 'chrome' })
try {
  const context = await browser.newContext({ viewport: { width: 1440, height: 1000 }, reducedMotion: 'reduce', serviceWorkers: 'block' })
  const state = await installMockApi(context); state.authenticated = true
  const page = await context.newPage(); page.setDefaultTimeout(15000)
  page.on('pageerror', error => report.errors.push(error.message))
  page.on('console', message => { if (message.type() === 'warning' && /Failed to resolve|Invalid prop|Unhandled error|inject\(\)|lifecycle/.test(message.text())) report.errors.push(message.text()) })
  await page.goto(`http://127.0.0.1:${server.address().port}/#/accounts`)
  await page.locator('.accounts-page').waitFor({ state: 'visible' })
  report.assertions = await testAccountRuntime(page, { themes: true, capture: async name => {
    await expect(page.locator('.el-message')).toHaveCount(0)
    await page.screenshot({ path: path.join(output, `${name}.png`), fullPage: true, animations: 'disabled' })
    report.screenshots.push(`${name}.png`)
  } })
  assert.deepEqual(report.errors, []); report.status = 'passed'; await context.close()
} catch (error) { report.status = 'failed'; report.failure = error.stack || error.message; throw error }
finally {
  report.finished_at = new Date().toISOString()
  await writeFile(path.join(output, 'report.json'), JSON.stringify(report, null, 2))
  await browser.close(); await new Promise(resolve => server.close(resolve))
  console.log(JSON.stringify({ status: report.status, assertions: report.assertions?.length || 0, report: path.join(output, 'report.json') }))
}
