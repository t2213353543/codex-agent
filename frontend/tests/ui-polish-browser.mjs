// Visual review fixtures stay local; no production data or upstream requests.
import assert from 'node:assert/strict'
import { createServer } from 'node:http'
import { mkdir, readFile, writeFile } from 'node:fs/promises'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { chromium, expect } from 'playwright/test'
import { installMockApi, account } from './mock-api.mjs'

const root = process.env.MANAGEMENT_UI_DIST ? path.resolve(process.env.MANAGEMENT_UI_DIST) : fileURLToPath(new URL('../../.local/ui-polish-20260918/dist/', import.meta.url))
const output = fileURLToPath(new URL('../../.local/ui-polish-20260918/browser/', import.meta.url))
await mkdir(output, { recursive: true })
const errors = [], screenshots = [], checks = [], escaped = [], contrast = []
const server = createServer(async (request, response) => {
  const pathname = new URL(request.url, 'http://localhost').pathname
  if (pathname.startsWith('/api/')) { escaped.push(pathname); response.writeHead(500).end(); return }
  try {
    const file = path.resolve(root, pathname === '/' ? 'index.html' : decodeURIComponent(pathname).replace(/^\/+/, ''))
    if (path.relative(root, file).startsWith('..')) throw new Error('Outside root')
    response.writeHead(200, { 'Content-Type': ({ '.html': 'text/html', '.css': 'text/css', '.js': 'text/javascript', '.woff2': 'font/woff2' })[path.extname(file)] || 'application/octet-stream' }).end(await readFile(file))
  } catch { response.writeHead(404).end() }
})
await new Promise(resolve => server.listen(0, '127.0.0.1', resolve))
const base = `http://127.0.0.1:${server.address().port}`
let browser, failure
try {
  browser = await chromium.launch({ channel: process.env.TEST_BROWSER || 'chrome', headless: true })
  const context = await browser.newContext({ reducedMotion: 'reduce', serviceWorkers: 'block' })
  const state = await installMockApi(context)
  state.authenticated = true
  const now = Math.floor(Date.now() / 1000)
  state.groups = ['研发工作区', '日常协作', '自动化任务', '评测与验证'].map((name, i) => ({ id: `g${i + 1}`, name, description: ['面向研发团队的编码与代码审查', '通用问答与知识整理', '定时任务、持续集成与批量处理', '新模型与新配置的隔离验证'][i], accounts: 6 - i, keys: 4 - i, version: 1, created_at: now - i * 86400 }))
  state.proxies = ['香港 · 主线路', '东京 · 备用线路', '新加坡 · 研发专用', '洛杉矶 · 已停用'].map((name, i) => ({ id: `p${i + 1}`, name, type: i % 2 ? 'socks5' : 'http', host: `192.0.2.${i + 10}`, port: 8080 + i, enabled: i !== 3, username: i === 0 ? 'gateway' : '', has_password: i === 0, accounts: 5 - i, version: 1 }))
  const accounts = ['研发账号 Alpha', '研发账号 Beta', '日常协作账号', '自动化任务账号', '评测账号 · 需要处理', '备用账号'].map((name, i) => account(`a${i + 1}`, name, { enabled: i !== 5, status: i === 4 ? 'error' : 'ok', credential_status: i === 4 ? 'error' : 'ok', tags: i === 0 ? ['研发', '主力'] : [], groups: [`g${i % 4 + 1}`], proxies: [`p${i % 3 + 1}`], in_flight: i % 3, priority: 10 - i, weight: 5, quota_percent: 13 + i * 11, quota_used: 1300 + i * 1100, last_error: i === 4 ? '上游暂时不可用，请稍后重试。' : '', usage: { requests: 1240 - i * 103, ok: 1227 - i * 102, errors: 13 - i, tokens: 850000 - i * 43000, input: 750000 - i * 42000, output: 100000 - i * 1000, cached: 620000 - i * 40000, avg_latency_ms: 1320 + i * 200 } }))
  const keys = ['研发 · Codex 桌面端', '日常协作 · 共享接入', 'CI · 自动化任务', '临时测试密钥'].map((name, i) => ({ id: `k${i + 1}`, name, key_masked: `sk-••••${['7a42', '9c18', '6d21', '2b90'][i]}`, enabled: i !== 3, groups: i === 0 ? ['g1', 'g4'] : [`g${i + 1}`], version: 1, requests: 1240 - i * 207, created_at: now - i * 86400 }))
  const logs = Array.from({ length: 8 }, (_, i) => ({ id: `r${i}`, time: now - i * 89, account_id: `a${i % 6 + 1}`, account: accounts[i % 6].name, key_name: keys[i % 4].name, model: i % 3 ? 'gpt-5.4' : 'gpt-5.3-codex', endpoint: i === 6 ? 'chat.completions' : 'responses', status: i === 4 ? 'error' : i === 7 ? 'aborted' : 'ok', input_tokens: i === 4 ? null : 12400 + i * 900, output_tokens: i === 4 ? null : 520 + i * 83, cached_tokens: i === 4 ? null : 10240 + i * 512, latency_ms: 1800 + i * 123, proxy_name: state.proxies[i % 3].name, ip: `198.51.100.${i + 10}`, error: i === 4 ? '上游返回 429，请求已达到当前账号速率限制。' : i === 7 ? '客户端已断开连接' : '' }))
  await context.route('**/api/accounts?*', route => route.fulfill({ json: { accounts, has_more: false, next_cursor: '', total_kind: 'not_computed' } }))
  await context.route('**/api/keys', route => route.request().method() === 'GET' ? route.fulfill({ json: { keys } }) : route.fallback())
  await context.route('**/api/logs?*', route => route.fulfill({ json: { items: logs, total: -1, total_kind: 'not_computed', has_more: true, next_cursor: 'fixture-next', page_size: 20 } }))
  const page = await context.newPage()
  page.on('pageerror', error => errors.push(error.message))
  page.setDefaultTimeout(12000)
  await page.goto(base)
  await expect(page.locator('.page-header-title')).toHaveText('运行总览')
  await expect(page.locator('.el-loading-mask')).toHaveCount(0)
  for (const theme of ['light', 'dark', 'ocean', 'mint']) {
    await page.evaluate(value => { document.documentElement.dataset.theme = value; document.documentElement.classList.toggle('dark', value === 'dark') }, theme)
    for (const width of (['light', 'dark'].includes(theme) ? [1440, 1024, 768, 390, 320] : [1440, 390])) {
      await page.setViewportSize({ width, height: width < 700 ? 844 : 960 })
      for (const [target, title] of [['accounts', '账号管理'], ['groups', '分组管理'], ['proxies', 'IP 代理池'], ['keys', 'API KEY 管理'], ['logs', '使用记录']]) {
        await page.evaluate(value => { location.hash = '/' + value }, target)
        await expect(page.locator('.page-header-title')).toContainText(title)
        await expect(page.locator('.el-loading-mask')).toHaveCount(0)
        await page.evaluate(async () => { await document.fonts.ready; document.querySelector('.content').scrollTop = 0 })
        assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false, `${target}/${theme}/${width} page overflow`)
        assert.equal(await page.locator('.content').evaluate(el => el.scrollWidth > el.clientWidth + 1), false, `${target}/${theme}/${width} content overflow`)
        const visibleTab = await page.locator('.tags-view').evaluate(el => {
          const active = el.querySelector('.tag-item.active').getBoundingClientRect()
          const wrap = el.querySelector('.el-scrollbar__wrap').getBoundingClientRect()
          return active.left >= wrap.left - 1 && active.right <= wrap.right + 1
        })
        assert.ok(visibleTab, `${target}/${theme}/${width}: active page tab is visible`)
        if (width === 1440 && target === 'keys') {
          const primary = page.locator('.page-header-actions .el-button--primary').first()
          const measure = element => {
            const style = getComputedStyle(element)
            const luminance = color => color.match(/[\d.]+/g).slice(0, 3).map(Number).map(v => v / 255).map(v => v <= .04045 ? v / 12.92 : ((v + .055) / 1.055) ** 2.4).reduce((sum, v, i) => sum + v * [.2126, .7152, .0722][i], 0)
            const background = element.classList.contains('page-header-sub') ? getComputedStyle(document.querySelector('.content')).backgroundColor : style.backgroundColor
            const [a, b] = [luminance(style.color), luminance(background)].sort((a, b) => a - b)
            return { ratio: (b + .05) / (a + .05), foreground: style.color, background }
          }
          for (const state of ['normal', 'hover']) {
            if (state === 'hover') await primary.hover()
            const result = await primary.evaluate(measure)
            contrast.push({ theme, target: 'primary', state, ...result })
            assert.ok(result.ratio >= 4.5, `${theme} primary ${state}: contrast ${result.ratio}`)
          }
          const subtitle = await page.locator('.page-header-sub').evaluate(measure)
          contrast.push({ theme, target: 'subtitle', ...subtitle })
          assert.ok(subtitle.ratio >= 4.5, `${theme} subtitle: contrast ${subtitle.ratio}`)
          await page.mouse.move(0, 0)
        }
        if (width < 700 && target !== 'logs') {
          // Feature suites assert mobile CRUD; here verify a direct, visible action on each page.
          await expect(page.locator('.content').getByRole('button', { name: /编辑/ }).first()).toBeVisible()
        }
        const filename = `${target}-${theme}-${width}.png`
        await page.screenshot({ path: path.join(output, filename) })
        screenshots.push(filename)
        checks.push(`${target}/${theme}/${width}: no page/content overflow`)
      }
    }
  }
  assert.deepEqual(errors, [])
  assert.deepEqual(escaped, [])
  assert.deepEqual(state.unexpected, [])
  await context.close()
} catch (error) { failure = { message: error.message, stack: error.stack }; console.error(error.stack) }
finally {
  await browser?.close()
  await new Promise(resolve => server.close(resolve))
  await writeFile(path.join(output, 'results.json'), JSON.stringify({ passed: !failure, dist: root, checks, contrast, screenshots, errors, escaped, failure }, null, 2))
  await writeFile(path.join(output, 'index.html'), `<!doctype html><html lang="zh-CN"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>管理界面预览 · 2026-09-18</title><style>body{margin:24px;font:14px system-ui;background:#f4f7fc;color:#243247}main{display:grid;grid-template-columns:repeat(auto-fit,minmax(300px,1fr));gap:20px}figure{margin:0;padding:12px;border:1px solid #dfe6ef;border-radius:8px;background:white}img{width:100%;height:300px;object-fit:contain;object-position:top}figcaption{margin-top:12px}a{color:#285ed6}</style><h1>五个管理页面 · 视觉验收</h1><p>本地合成数据，四种主题，桌面与移动端。点击查看原图。</p><main>${screenshots.map(file => `<figure><a href="${file}"><img loading="lazy" src="${file}" alt="${file}"></a><figcaption>${file}</figcaption></figure>`).join('')}</main></html>`)
}
console.log(JSON.stringify({ passed: !failure, screenshots: screenshots.length, output }))
if (failure) process.exitCode = 1
