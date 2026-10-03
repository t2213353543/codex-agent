import { createServer } from 'node:http'
import { createHash } from 'node:crypto'
import { readFile, writeFile, mkdir } from 'node:fs/promises'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { chromium } from 'playwright/test'
import { installMockApi } from './mock-api.mjs'
import { testAccountLeaseBackup } from './account-lease-backup-browser.mjs'
const root = path.resolve(fileURLToPath(new URL('../..', import.meta.url)))
const dist = path.resolve(process.env.MANAGEMENT_UI_DIST || path.join(root, '.local/management-parity/completion-20260918/dist'))
const output = path.resolve(process.env.MANAGEMENT_LEASE_REPORT_DIR || path.join(root, '.local/management-parity/completion-20260918/lease-browser'))
await mkdir(output, { recursive: true })
const manifestPath = path.join(path.dirname(dist), 'build-manifest.json')
const manifestBytes = await readFile(manifestPath)
const manifest = JSON.parse(manifestBytes)
const sha = value => createHash('sha256').update(value).digest('hex')
for (const [relative, digest] of Object.entries(manifest.files)) if (sha(await readFile(path.join(dist, relative))) !== digest) throw new Error('Candidate static file differs from build manifest')
const report = { passed: false, synthetic_data: true, dist, manifest_sha256: sha(manifestBytes), static_file_count: Object.keys(manifest.files).length, checks: [], errors: [] }
const server = createServer(async (request, response) => {
  try {
    const pathname = decodeURIComponent(new URL(request.url, 'http://127.0.0.1').pathname)
    const file = path.resolve(dist, pathname === '/' ? 'index.html' : pathname.slice(1))
    if (!file.startsWith(dist + path.sep)) throw new Error('invalid path')
    response.writeHead(200, { 'Content-Type': file.endsWith('.js') ? 'text/javascript' : file.endsWith('.css') ? 'text/css' : 'text/html; charset=utf-8' })
    response.end(await readFile(file))
  } catch { response.writeHead(404); response.end('Not found') }
})
await new Promise(resolve => server.listen(0, '127.0.0.1', resolve))
const browser = await chromium.launch({ headless: true, channel: 'chrome' })
try {
  const context = await browser.newContext({ viewport: { width: 1440, height: 1000 }, reducedMotion: 'reduce', acceptDownloads: true })
  const state = await installMockApi(context); state.authenticated = true
  const page = await context.newPage(); page.setDefaultTimeout(15000)
  page.on('pageerror', error => report.errors.push(error.message))
  await page.goto(`http://127.0.0.1:${server.address().port}/#/accounts`)
  report.checks = await testAccountLeaseBackup(page, { capture: name => page.screenshot({ path: path.join(output, name + '.png'), fullPage: true, animations: 'disabled' }) })
  if (report.errors.length) throw new Error('Browser JavaScript errors')
  if (sha(await readFile(manifestPath)) !== report.manifest_sha256) throw new Error('Candidate build changed during verification')
  report.passed = true
  await context.close()
} catch (error) { report.failure = error.message }
finally { await browser.close(); await new Promise(resolve => server.close(resolve)); await writeFile(path.join(output, 'report.json'), JSON.stringify(report, null, 2)) }
console.log(JSON.stringify(report))
if (!report.passed) process.exitCode = 1
