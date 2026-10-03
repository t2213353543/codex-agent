import assert from 'node:assert/strict'
import { spawn } from 'node:child_process'
import { mkdir, writeFile } from 'node:fs/promises'
import { fileURLToPath } from 'node:url'
import path from 'node:path'
import { chromium, expect } from 'playwright/test'

const directory = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const output = path.resolve(directory, process.env.CHARTS_TEST_OUTPUT || '../.local/console-ui-20260924/dashboard')
const port = Number(process.env.CHARTS_TEST_PORT || 4191)
const origin = `http://127.0.0.1:${port}`
const server = spawn(process.execPath, [path.join(directory, 'node_modules/vite/bin/vite.js'), '--config', path.join(directory, 'tests/charts-vite.config.mjs'), '--host', '127.0.0.1', '--port', String(port), '--strictPort'], { cwd: directory, windowsHide: true, stdio: 'pipe' })
let serverText = ''
server.stdout.on('data', (chunk) => { serverText += chunk })
server.stderr.on('data', (chunk) => { serverText += chunk })
let browser
const assertions = []
const screenshots = []
const scope = 'Dashboard only: synthetic API data, metrics, chart themes/resizing/lifecycle, and recent-error text. Logs behavior is covered separately by run-log-management.'
const now = 1800000000
const htmlText = '<img src=x onerror="window.__injected = true"> literal error'
const record = { time: now, key_name: 'private-key-name', account: 'demo-account-with-a-long-name-演示账号', account_id: 'a_demo', model: '<b>literal-model</b>', endpoint: 'responses', status: 'error', http_code: 401, input_tokens: 100, output_tokens: 10, cached_tokens: 80, latency_ms: 32, error: htmlText, proxy_name: 'private-proxy', ip: '192.0.2.1' }
const dashboard = {
  cards: { accounts: 12, accounts_enabled: 9, groups: 3, keys: 4, keys_enabled: 2, requests: 10, success: 9, tokens: 110, errors: 1, cache_rate: 0.8, cache_rate_has_samples: true, requests_today: 5, tokens_today: 55, tokens_total: 1100, tokens_retained: 990, avg_latency_ms: 32, avg_latency_today: 30, rpm: 2, tpm: 22, peak_rpm: 3, peak_tpm: 50 },
  timezone: 'UTC', retention_days: 92, totals_scope: { tokens_total: 'lifetime' },
  series: [{ ts: now - 3600, requests: 3, tokens: 60, errors: 0, input: 50, output: 10, cached: 40 }, { ts: now, requests: 7, tokens: 50, errors: 1, input: 50, output: 0, cached: 40 }],
  status: [{ name: '成功', value: 9 }, { name: '失败', value: 1 }],
  account_top: [{ name: record.account, requests: 10, tokens: 110, errors: 1 }],
  recent_errors: [record],
  traffic_breakdown: { scope: 'retained_request_details', requests: 10, errors: 1, models: [{ name: 'gpt-6-astra', requests: 7, errors: 1, tokens: 80, usage_known_requests: 6 }, { name: 'gpt-5.6-sol', requests: 2, errors: 0, tokens: 30, usage_known_requests: 2 }, { name: '<b>literal-model</b>', requests: 1, errors: 0, tokens: 0, usage_known_requests: 0 }], error_categories: [{ name: 'authentication', requests: 1, errors: 1, tokens: 0 }] },
}
dashboard.cards.in_flight = 3
dashboard.cards.latency_samples = 9

try {
  await mkdir(output, { recursive: true })
  for (let attempt = 0; attempt < 100; attempt++) {
    if (server.exitCode !== null) throw new Error(`Vite failed: ${serverText}`)
    try { if ((await fetch(origin)).ok) break } catch { /* Wait for our own server. */ }
    if (attempt === 99) throw new Error(`Vite did not become ready: ${serverText}`)
    await new Promise((resolve) => setTimeout(resolve, 100))
  }
  browser = await chromium.launch({ headless: true, channel: process.env.TEST_BROWSER || 'chrome' })
  const page = await browser.newPage({ viewport: { width: 1440, height: 1000 }, acceptDownloads: true })
  const pageErrors = []
  const unexpectedAPI = []
  page.on('pageerror', (error) => pageErrors.push(error.message))
  // Catch every API call. No API request reaches a backend or a production host.
  await page.route('**/api/**', async (route) => {
    const url = new URL(route.request().url())
    let json
    if (url.pathname === '/api/session') json = { authenticated: true, expires_at: now + 3600 }
    else if (url.pathname === '/api/settings') json = { version: 'mock-charts-test', uptime_seconds: 100, admin_api: true }
    else if (url.pathname === '/api/overview') json = dashboard
    else if (url.pathname === '/api/groups') json = { groups: [] }
    else { unexpectedAPI.push({ method: route.request().method(), path: url.pathname }); await route.fulfill({ status: 404, json: { error: { message: `Unexpected mock endpoint: ${url.pathname}` } } }); return }
    await route.fulfill({ json }).catch(() => {})
  })
  // Warm the route used only to unmount Dashboard before capturing chart
  // instances, so cold Vite optimization cannot reset lifecycle references.
  await page.goto(`${origin}/#/groups`)
  await expect(page.getByRole('heading', { name: '分组管理', exact: true })).toBeVisible()
  await page.waitForLoadState('networkidle')
  const overviewResponse = page.waitForResponse(response => new URL(response.url()).pathname === '/api/overview' && new URL(response.url()).searchParams.get('include_breakdown') === 'true' && response.status() === 200)
  await page.goto(`${origin}/#/`)
  assert.equal((await (await overviewResponse).json()).cards.tokens_total, 1100)
  await expect(page.getByRole('checkbox', { name: '自动刷新 · 30 秒' })).toBeChecked()
  await page.locator('.dashboard-auto-refresh').click()
  await page.reload()
  await expect(page.getByRole('checkbox', { name: '自动刷新 · 30 秒' })).not.toBeChecked()
  await page.locator('.dashboard-auto-refresh').click()
  assertions.push('automatic refresh can be paused and the preference survives reload')
  await expect(page.locator('.kpi-cache_rate').locator('..').locator('.kpi-value')).toHaveText('80%')
  await expect(page.locator('.scope-summary')).toContainText('今日（UTC）')
  await expect(page.locator('.scope-summary')).toContainText('持久事件累计')
  for (const [key, value] of Object.entries({ accounts: '12', accounts_enabled: '9', groups: '3', keys: '4', success: '9', input: '100', output: '10', cached: '80', avg_latency_ms: '32 ms', peak_rpm: '3', peak_tpm: '50', requests_today: '5', tokens_today: '55', avg_latency_today: '30 ms', tokens_total: '1.1K', tokens_retained: '990' })) await expect(page.locator(`[data-metric="${key}"] .metric-value`)).toHaveText(value)
  await expect(page.locator('[data-metric="rpm"]')).toContainText('2')
  await expect(page.locator('[data-metric="tpm"]')).toContainText('22')
  assertions.push('16 resource/throughput/today/lifetime metric values, RPM/TPM, selected token sums and timezone/lifetime scope verified')
  await expect(page.locator('.kpi-card')).toHaveCount(4)
  await expect(page.locator('[data-metric="in_flight"] strong')).toHaveText('3请求')
  await expect(page.locator('[data-metric="error_rate"] strong')).toHaveText('10%')
  await expect(page.locator('.model-row')).toHaveCount(3)
  await expect(page.locator('.model-row').first()).toContainText('80 Token · 部分已知')
  await expect(page.locator('.model-row').last()).toContainText('Token 未知')
  await expect(page.locator('.model-row').last()).toContainText('<b>literal-model</b>')
  assert.equal(await page.locator('.model-list b').count(), 0)
  await expect(page.locator('.failure-row')).toContainText('认证失败')
  assertions.push('live admission, failure ratio, retained model distribution, partial/unknown token coverage and error category metrics use response fields')
  assert.equal(await page.locator('.details-panel').getAttribute('open'), null)
  assert.equal(await page.locator('.scope-summary').getAttribute('open'), null)
  await expect(page.locator('.incident-list .incident-item p')).toHaveText(htmlText)
  assert.equal(await page.locator('.incident-list img, .incident-list script').count(), 0)
  assert.equal(await page.evaluate(() => window.__injected), undefined)
  assertions.push('four KPI cards and default collapsed detailed metrics retained; recent-error incident list renders untrusted error as literal text')
  const chartStats = await page.evaluate(async () => {
    const { getInstanceByDom } = await import('/src/utils/echarts.js')
    const elements = [...document.querySelectorAll('.traffic-chart, .status-chart, .kpi-spark')]
    window.__chartsUnderTest = elements.map((element) => getInstanceByDom(element))
    return { count: window.__chartsUnderTest.length, initialized: window.__chartsUnderTest.every(Boolean), tooltipModes: window.__chartsUnderTest.slice(0, 2).map((chart) => chart.getOption().tooltip?.[0]?.renderMode) }
  })
  assert.equal(chartStats.count, 6)
  assert.equal(chartStats.initialized, true)
  const modes = await page.evaluate(async () => {
    const { getInstanceByDom } = await import('/src/utils/echarts.js')
    return ['.traffic-chart', '.status-chart'].map((selector) => getInstanceByDom(document.querySelector(selector)).getOption().tooltip[0].renderMode)
  })
  assert.deepEqual(modes, ['richText', 'richText'])
  assertions.push('cache_rate=0.8 renders 80%; dashboard scope matches API; all six charts initialized; tooltips use richText')

  for (const theme of ['light', 'dark']) {
    await page.evaluate(async (value) => (await import('/src/theme.js')).applyTheme(value), theme)
    await expect.poll(() => page.evaluate(async () => {
      const { getInstanceByDom } = await import('/src/utils/echarts.js')
      const color = getComputedStyle(document.documentElement).getPropertyValue('--primary').trim()
      return getInstanceByDom(document.querySelector('.traffic-chart')).getOption().series[0].itemStyle.color === color
    })).toBe(true)
    for (const width of [320, 390, 768, 1440]) {
      await page.setViewportSize({ width, height: 1000 })
      await page.locator('.content').evaluate((element) => { element.scrollTop = 0 })
      await page.waitForTimeout(120)
      await expect.poll(() => page.evaluate(async () => {
        const { getInstanceByDom } = await import('/src/utils/echarts.js')
        const el = document.querySelector('.traffic-chart')
        return Math.abs(getInstanceByDom(el).getWidth() - el.clientWidth)
      })).toBeLessThanOrEqual(1)
      assert.equal(await page.evaluate(() => {
        const content = document.querySelector('.content')
        return document.documentElement.scrollWidth <= innerWidth && content.scrollWidth <= content.clientWidth + 1
      }), true, `Dashboard overflow: ${theme}/${width}`)
      const topImage = `dashboard-${theme}-${width}.png`
      await page.screenshot({ path: path.join(output, topImage), fullPage: true })
      screenshots.push(topImage)
      await page.locator('.traffic-chart').scrollIntoViewIfNeeded()
      const chartImage = `dashboard-charts-${theme}-${width}.png`
      await page.screenshot({ path: path.join(output, chartImage), fullPage: true })
      screenshots.push(chartImage)
      if ([390, 1440].includes(width)) {
        await page.locator('.dashboard-breakdown-grid').scrollIntoViewIfNeeded()
        const distributionImage = `dashboard-distributions-${theme}-${width}.png`
        await page.screenshot({ path: path.join(output, distributionImage), fullPage: true })
        screenshots.push(distributionImage)
      }
    }
  }
  assertions.push('Linen/light and Orbit/dark × 320/390/768/1440px: no document/content overflow, charts resize to their containers, theme event recolors the request series')
  await page.setViewportSize({ width: 390, height: 1000 })
  await page.locator('.details-panel > summary').click()
  await expect(page.locator('[data-metric="input"]')).toBeVisible()
  await page.locator('.scope-summary > summary').click()
  await expect(page.locator('[data-metric="tokens_total"]')).toBeVisible()
  assertions.push('detailed traffic and lifetime sections expand and expose their original metrics on mobile')
  await page.evaluate(() => { location.hash = '/groups' })
  await expect(page.getByRole('heading', { name: '分组管理', exact: true })).toBeVisible()
  assert.equal(await page.evaluate(() => window.__chartsUnderTest.every((chart) => chart.isDisposed())), true)
  assertions.push('all six ECharts instances disposed after leaving Dashboard')
  await page.evaluate(() => { location.hash = '/' })
  await expect(page.locator('.dashboard-page')).toHaveAttribute('aria-busy', 'false')
  await expect.poll(() => page.evaluate(async () => {
    const { getInstanceByDom } = await import('/src/utils/echarts.js')
    return [...document.querySelectorAll('.traffic-chart, .status-chart, .kpi-spark')].filter(element => {
      const chart = getInstanceByDom(element)
      return chart && !chart.isDisposed() && !window.__chartsUnderTest.includes(chart)
    }).length
  })).toBe(6)
  assertions.push('returning to Dashboard initializes six fresh chart instances without reviving disposed charts')
  assert.deepEqual(unexpectedAPI, [])
  assert.deepEqual(pageErrors, [])
  assertions.push('no uncaught browser errors')
  await writeFile(path.join(output, 'results.json'), JSON.stringify({ schema: 'codex-dashboard-browser-v1', status: 'passed', scope, synthetic_data: true, themes: ['light', 'dark'], widths: [320, 390, 768, 1440], assertions, check_count: assertions.length, screenshots: screenshots.length, screenshot_files: screenshots }, null, 2))
  console.log(`PASS: ${assertions.join('\nPASS: ')}`)
  console.log(`Screenshots and results: ${output}`)
} catch (error) {
  await writeFile(path.join(output, 'results.json'), JSON.stringify({ schema: 'codex-dashboard-browser-v1', status: 'failed', scope, synthetic_data: true, assertions, screenshots: screenshots.length, screenshot_files: screenshots, error: error.stack }, null, 2)).catch(() => {})
  throw error
} finally {
  await browser?.close()
  server.kill()
}
