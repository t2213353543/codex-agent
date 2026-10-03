import assert from 'node:assert/strict'
import { createServer } from 'node:http'
import { readFile, writeFile } from 'node:fs/promises'
import { createHash } from 'node:crypto'
import path from 'node:path'
import { chromium, expect } from 'playwright/test'
import { installMockApi } from './mock-api.mjs'
import { testLogIngress } from './log-ingress-browser.mjs'

const candidate = path.resolve(process.argv[2]), dist = path.join(candidate, 'dist'), output = path.join(candidate, 'logs-browser')
const manifestPath = path.join(candidate, 'build-manifest.json'), manifestBody = await readFile(manifestPath), manifest = JSON.parse(manifestBody)
const hash = value => createHash('sha256').update(value).digest('hex')
async function verify() { assert.equal(hash(await readFile(manifestPath)), hash(manifestBody)); for (const [name, digest] of Object.entries(manifest.files)) assert.equal(hash(await readFile(path.join(dist, name))), digest) }
await verify()
const server = createServer(async (request, response) => {
  try {
    const pathname = decodeURIComponent(new URL(request.url, 'http://localhost').pathname), file = path.resolve(dist, pathname === '/' ? 'index.html' : pathname.slice(1))
    assert.ok(file.startsWith(dist + path.sep)); const body = await readFile(file)
    response.writeHead(200, { 'content-type': file.endsWith('.js') ? 'text/javascript' : file.endsWith('.css') ? 'text/css' : 'text/html' }); response.end(body)
  } catch { response.writeHead(404); response.end('Not found') }
})
await new Promise(resolve => server.listen(0, '127.0.0.1', resolve))
let browser
const report = { status: 'running', manifest_sha256: hash(manifestBody), errors: [], scope: 'additional synthetic ingress visual coverage only; main Logs54 report remains authoritative' }
try {
  browser = await chromium.launch({ channel: 'chrome', headless: true })
  const context = await browser.newContext({ viewport: { width: 1440, height: 1100 }, serviceWorkers: 'block', reducedMotion: 'reduce' })
  const state = await installMockApi(context); state.authenticated = true
  const page = await context.newPage(); page.setDefaultTimeout(12000); page.on('pageerror', error => report.errors.push(error.message))
  await page.goto(`http://127.0.0.1:${server.address().port}/#/settings`)
  await expect(page.locator('.settings-panel')).toBeVisible()
  report.ingress = await testLogIngress(page, { screenshotDirectory: output })
  assert.deepEqual(report.errors, []); await verify(); report.status = 'passed'
} catch (error) { report.status = 'failed'; report.failure = String(error); throw error }
finally { await browser?.close(); await new Promise(resolve => server.close(resolve)); await writeFile(path.join(output, 'ingress-visual-report.json'), JSON.stringify(report, null, 2) + '\n') }
console.log(JSON.stringify({ status: report.status, manifest_sha256: report.manifest_sha256, assertions: report.ingress.assertions.length }))
