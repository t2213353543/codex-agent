import assert from 'node:assert/strict'
import { createServer } from 'node:http'
import { readFile, writeFile } from 'node:fs/promises'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { chromium, expect } from 'playwright/test'
import { account } from '../tests/mock-api.mjs'

const output = fileURLToPath(new URL('../../.local/system-perf-20260927/frontend/', import.meta.url))
const requests = [], errors = [], samples = []
const now = Math.floor(Date.now() / 1000)
const rows = Array.from({ length: 20 }, (_, i) => account(`perf-${i}`, `性能样本 ${i}`, { groups: ['g1'], proxies: ['p1'], status: 'ok' }))
const server = createServer(async (request, response) => {
  const pathname = decodeURIComponent(new URL(request.url, 'http://localhost').pathname)
  const root = path.join(output, pathname.startsWith('/before/') ? 'before-dist' : 'after-dist')
  const relative = pathname.replace(/^\/(before|after)\//, '') || 'index.html'
  const file = path.resolve(root, relative)
  try {
    assert.ok(!path.relative(root, file).startsWith('..'))
    const data = await readFile(file)
    response.writeHead(200, { 'Content-Type': ({ '.html': 'text/html', '.js': 'text/javascript', '.css': 'text/css', '.svg': 'image/svg+xml' })[path.extname(file)] || 'application/octet-stream', 'Cache-Control': 'no-store' }).end(data)
  } catch { response.writeHead(404).end() }
})
await new Promise(resolve => server.listen(0, '127.0.0.1', resolve))
const origin = `http://127.0.0.1:${server.address().port}`
const browser = await chromium.launch({ channel: process.env.TEST_BROWSER || 'chrome', headless: true })
async function contextFor(variant) {
  const context = await browser.newContext({ viewport: { width: 1440, height: 1000 }, reducedMotion: 'reduce', serviceWorkers: 'block' })
  await context.route('**/*', async route => {
    const request = route.request(), url = new URL(request.url()), pathname = url.pathname
    assert.equal(url.origin, origin)
    if (pathname.startsWith('/assets/')) return route.continue({ url: `${origin}/${variant}${pathname}` })
    if (!pathname.startsWith('/api/')) return route.continue()
    requests.push({ variant, method: request.method(), path: pathname })
    const reply = json => route.fulfill({ json })
    if (pathname === '/api/session') return reply({ authenticated: true, expires_at: now + 1800 })
    if (pathname === '/api/settings') return reply({ version: 'perf-synthetic', settings_version: 1 })
    if (pathname === '/api/groups') return reply({ groups: [{ id: 'g1', name: '性能分组', accounts: 20, keys: 1 }] })
    if (pathname === '/api/proxies') return reply({ proxies: [{ id: 'p1', name: '性能代理', type: 'http', host: 'example.test', port: 8080, enabled: true }] })
    if (pathname === '/api/accounts/manage') return reply({ accounts: rows, total: rows.length, total_kind: 'exact', page: 1, page_size: 20, has_more: false })
    if (pathname === '/api/accounts/upstream/batch') return reply({ results: request.postDataJSON().items.map(row => ({ id: row.id, ok: false, code: 'synthetic_no_upstream' })) })
    if (/^\/api\/accounts\/perf-\d+$/.test(pathname)) return reply({ account: rows.find(row => pathname.endsWith(`/${row.id}`)) })
    if (pathname.endsWith('/requests')) return reply({ items: [{ time: now, status: 'ok', model: 'synthetic', input_tokens: 100, output_tokens: 10, cached_tokens: 50, latency_ms: 30 }], has_more: false })
    if (pathname.endsWith('/runtime')) return reply({ id: 'perf-0', effective_policy: {}, account: rows[0] })
    if (pathname.endsWith('/metadata')) return reply({ account: rows[0] })
    if (pathname === '/api/logs/snapshot') return reply({ scope: 'retained_request_details', as_of: 'perf-snapshot', created_at: now, expires_at: now + 600 })
    if (pathname === '/api/logs/stats') {
      const from = Number(url.searchParams.get('from')), to = Number(url.searchParams.get('to'))
      const start = Math.floor((from + 8 * 3600) / 86400) * 86400 - 8 * 3600
      const series = []
      for (let time = start; time < to; time += 86400) series.push({ time, total_requests: 1, total_tokens: 10, usage_known_requests: 1, usage_unknown_requests: 0 })
      return reply({ scope: 'retained_request_details', from, to, step: 86400, timezone: url.searchParams.get('timezone'), checked_at: now, total_requests: series.length, total_tokens: series.length * 10, usage_known_requests: series.length, usage_unknown_requests: 0, success: series.length, errors: 0, aborted: 0, series })
    }
    throw new Error(`Unmocked API: ${request.method()} ${pathname}`)
  })
  return context
}
try {
  for (let round = 0; round < 5; round++) for (const variant of round % 2 ? ['after', 'before'] : ['before', 'after']) {
    const context = await contextFor(variant), page = await context.newPage()
    page.on('pageerror', error => errors.push(error.message))
    const started = performance.now()
    await page.goto(`${origin}/${variant}/#/accounts`)
    await expect(page.locator('.account-directory-heading')).toContainText('共 20 个')
    await expect(page.locator('.el-loading-mask:visible')).toHaveCount(0)
    const readyMs = performance.now() - started
    const js = await page.evaluate(() => performance.getEntriesByType('resource').filter(row => row.name.split('?')[0].endsWith('.js')).map(row => ({ name: new URL(row.name).pathname.split('/').at(-1), bytes: row.decodedBodySize })))
    const chartFiles = js.filter(row => /^(echarts-|chart-renderer-)/.test(row.name))
    assert.equal(chartFiles.length, variant === 'before' ? 2 : 0, `${variant}: lazy chart dependency boundary`)
    samples.push({ round, variant, ready_ms: readyMs, javascript_bytes: js.reduce((sum, row) => sum + row.bytes, 0), chart_files: chartFiles, files: js })
    await context.close()
  }
  const context = await contextFor('after'), page = await context.newPage()
  page.on('pageerror', error => errors.push(error.message))
  let release
  const gate = new Promise(resolve => { release = resolve })
  await page.route('**/assets/echarts-*.js', async route => { await gate; return route.fallback() })
  await page.goto(`${origin}/after/#/accounts`)
  await expect(page.locator('.account-directory-heading')).toContainText('共 20 个')
  const chartRequest = page.waitForRequest(request => /\/assets\/echarts-.*\.js$/.test(request.url()))
  await page.getByRole('button', { name: '查看账号 性能样本 0 详情', exact: true }).first().click()
  await chartRequest
  const dialog = page.getByRole('dialog', { name: '账号详情', exact: true })
  await dialog.getByRole('button', { name: '关闭', exact: true }).last().click()
  const chartResponse = page.waitForResponse(response => /\/assets\/echarts-.*\.js$/.test(response.url()))
  release()
  await chartResponse
  await page.waitForLoadState('networkidle')
  assert.equal(await page.locator('.account-chart canvas').count(), 0)
  await page.getByRole('button', { name: '查看账号 性能样本 0 详情', exact: true }).first().click()
  await expect(page.locator('.account-chart canvas')).toHaveCount(1)
  await dialog.getByRole('button', { name: '关闭', exact: true }).last().click()
  await page.getByRole('button', { name: '账号 性能样本 0 的更多操作', exact: true }).first().click()
  await page.getByRole('menuitem', { name: '账号统计', exact: true }).click()
  await expect(page.locator('.account-daily-stats canvas')).toHaveCount(1)
  await context.close()
  assert.deepEqual(errors, [])
  const median = values => [...values].sort((a, b) => a - b)[Math.floor(values.length / 2)]
  const summary = Object.fromEntries(['before', 'after'].map(variant => [variant, { ready_ms_median: median(samples.filter(sample => sample.variant === variant).map(sample => sample.ready_ms)), javascript_bytes: samples.find(sample => sample.variant === variant).javascript_bytes }]))
  const report = { synthetic_data: true, real_backend: false, measurements: summary, samples, checks: ['initial account list loads charts only before optimization', 'closing details during chart download does not recreate disposed charts', 'reopened account details initialize the chart', 'account daily statistics lazily initialize the chart'], errors, requests }
  await writeFile(path.join(output, 'account-loading.json'), JSON.stringify(report, null, 2))
  console.log(JSON.stringify({ ...summary, checks: report.checks }, null, 2))
} finally { await browser.close(); await new Promise(resolve => server.close(resolve)) }
