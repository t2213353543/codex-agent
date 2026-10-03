import assert from 'node:assert/strict'
import { expect } from 'playwright/test'
import { captureLogThemes } from './log-visual-fixtures.mjs'
export async function testLogIpGeo(page, { screenshotDirectory } = {}) {
  assert.ok(['localhost', '127.0.0.1'].includes(new URL(page.url()).hostname))
  const rows = ['8.8.8.8', '1.1.1.1', '192.0.2.9'].map((ip, index) => ({ id: index + 1, event_id: `geo-${index + 1}`, time: 1789650000 - index, model: 'synthetic-geo-model', account: 'GeoJS 合成测试', status: 'ok', http_code: 200, ip, input_tokens: 0, output_tokens: 0, cached_tokens: 0, usage_known: true }))
  const calls = [], assertions = []
  let currentRows = rows
  let blocked = false, release, fail = false
  const handler = async route => {
    const url = new URL(route.request().url())
    const reply = async value => { try { await route.fulfill(value) } catch { /* Closing details aborts the lookup. */ } }
    if (url.pathname === '/api/logs/snapshot') return reply({ json: { as_of: 'usage_geo_main', created_at: 1789650000, expires_at: Math.floor(Date.now() / 1000) + 300, total: 3 } })
    if (url.pathname === '/api/logs/stats') return reply({ json: { total_requests: 3, success: 3, errors: 0, total_input_tokens: 0, total_output_tokens: 0, total_cached_tokens: 0, total_tokens: 0, models: [], endpoints: [], error_categories: [], series: [] } })
    if (url.pathname === '/api/logs/ip-geo') {
      const body = route.request().postDataJSON(); calls.push(body)
      if (blocked) await new Promise(resolve => { release = resolve })
      return reply({ json: { source: 'GeoJS', cache_ttl_seconds: 86400, items: body.ips.map(ip => fail || body.ips.length > 1 && ip === '1.1.1.1' ? { ip, status: 'error', error_code: 'upstream_unavailable', error: '合成 GeoJS 暂时不可用' } : { ip, status: 'success', country: '合成国家', country_code: 'US', region: '合成地区', city: '合成城市', organization: 'Synthetic Geo Fixture', timezone: 'UTC', source: 'GeoJS', fetched_at: 1789650000, expires_at: 1789736400, cached: !body.force }) } })
    }
    if (url.pathname === '/api/logs') return reply({ json: { items: currentRows, total: currentRows.length, total_kind: 'exact', has_more: false, next_cursor: '' } })
    return reply({ json: { record: rows.find(row => url.pathname.endsWith(row.event_id)) || rows[0], scope: 'retained_request_details' } })
  }
  await page.route('**/api/logs**', handler)
  try {
    await page.setViewportSize({ width: 1440, height: 1100 })
    await page.goto(new URL('/#/settings', page.url()).href)
    await page.goto(new URL('/#/logs', page.url()).href)
    await expect(page.locator('.log-table .el-table__row')).toHaveCount(3)
    const open = index => page.locator('.log-table').getByRole('button', { name: /日志详情/ }).nth(index).click()
    const drawer = page.getByRole('dialog', { name: '日志详情', exact: true }), geo = drawer.locator('.log-ip-geo')
    await open(0)
    await expect(geo).toContainText('点击后将此公网 IP 发送给 GeoJS')
    assert.equal(calls.length, 0)
    await geo.getByRole('button', { name: '通过 GeoJS 查询归属地', exact: true }).click()
    await expect(geo).toContainText('合成国家 · 合成地区 · 合成城市')
    await expect(geo).toContainText('使用 24 小时内缓存')
    assert.deepEqual(calls, [{ ips: ['8.8.8.8'], force: false }])
    await geo.getByRole('button', { name: '重新通过 GeoJS 查询', exact: true }).click()
    await expect(geo).toContainText('Synthetic Geo Fixture')
    await expect(geo).not.toContainText('使用 24 小时内缓存')
    assert.equal(calls.at(-1).force, true)
    assertions.push('IP归属地默认零请求；显式查询只发选中IP，显示真实来源/缓存时间，刷新明确force')

    fail = true
    await geo.getByRole('button', { name: '重新通过 GeoJS 查询', exact: true }).click()
    await expect(geo).toContainText('合成 GeoJS 暂时不可用')
    await expect(geo).not.toContainText('合成国家')
    fail = false
    await geo.getByRole('button', { name: '重试 GeoJS 查询归属地', exact: true }).click()
    await expect(geo).toContainText('合成国家')
    await captureLogThemes(page, geo, 'geo-detail', screenshotDirectory)
    await page.setViewportSize({ width: 390, height: 844 })
    if (screenshotDirectory) await geo.screenshot({ path: `${screenshotDirectory}/logs-ip-geo-390.png` })
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true)
    await page.setViewportSize({ width: 1440, height: 1100 })
    await drawer.getByRole('button', { name: '关闭详情' }).click()
    const beforePrivate = calls.length
    await open(2)
    await expect(geo).toContainText('私有、保留或非公网地址，不发送至 GeoJS')
    assert.equal(await geo.getByRole('button').count(), 0)
    assert.equal(calls.length, beforePrivate)
    await drawer.getByRole('button', { name: '关闭详情' }).click()
    assertions.push('GeoJS失败不保留旧成功归属地，可显式重试；保留地址无外发按钮，手机详情正常')

    blocked = true
    await open(0)
    await geo.getByRole('button', { name: '通过 GeoJS 查询归属地', exact: true }).click()
    await expect.poll(() => typeof release).toBe('function')
    await drawer.getByRole('button', { name: '关闭详情' }).click()
    await open(1)
    await expect(geo).toContainText('点击后将此公网 IP 发送给 GeoJS')
    blocked = false; release(); release = undefined
    await expect(geo).not.toContainText('合成国家')
    await geo.getByRole('button', { name: '通过 GeoJS 查询归属地', exact: true }).click()
    await expect(geo).toContainText('合成国家')
    assert.deepEqual(calls.at(-1).ips, ['1.1.1.1'])
    await drawer.getByRole('button', { name: '关闭详情' }).click()
    assertions.push('关闭详情取消查询；切换IP后迟到结果不回填，下一次显式查询使用新IP')

    currentRows = [...rows, { ...rows[0], id: 4, event_id: 'geo-4' }]
    await page.getByRole('button', { name: '刷新日志', exact: true }).click()
    await expect(page.locator('.log-table .el-table__row')).toHaveCount(4)
    const beforeBatch = calls.length
    await page.getByRole('button', { name: '批量查询本页 IP 归属地', exact: true }).click()
    const batch = page.getByRole('dialog', { name: '本页 IP 归属地', exact: true })
    await expect(batch).toContainText('2 个可查询公网地址')
    await expect(batch).toContainText('合并重复地址 1 个')
    assert.equal(calls.length, beforeBatch)
    await batch.getByRole('button', { name: '通过 GeoJS 查询 2 个公网 IP', exact: true }).click()
    await expect(batch).toContainText('已完成 2 / 2 个，成功 1 个，未成功 1 个')
    assert.deepEqual(calls.at(-1), { ips: ['8.8.8.8', '1.1.1.1'], force: false })
    await expect(batch).toContainText('查询失败')
    await captureLogThemes(page, batch, 'geo-batch', screenshotDirectory)
    await page.setViewportSize({ width: 320, height: 844 })
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true)
    if (screenshotDirectory) await batch.screenshot({ path: `${screenshotDirectory}/logs-ip-geo-batch-320.png` })
    await batch.getByRole('button', { name: '关闭', exact: true }).click()
    await page.setViewportSize({ width: 1440, height: 1100 })
    assertions.push('批量当前页先预览不外发；公网去重且排除保留IP，部分失败逐项反馈，320手机结果无溢出')

    blocked = true
    await page.getByRole('button', { name: '批量查询本页 IP 归属地', exact: true }).click()
    await batch.getByRole('button', { name: '通过 GeoJS 查询 2 个公网 IP', exact: true }).click()
    await expect.poll(() => typeof release).toBe('function')
    await batch.getByRole('button', { name: '取消未完成查询', exact: true }).click()
    await expect(batch).toContainText('已取消，未完成部分没有结果')
    blocked = false; release(); release = undefined
    await expect(batch.locator('.geo-batch-row')).toHaveCount(0)
    await batch.getByRole('button', { name: '关闭', exact: true }).click()
    await page.getByRole('textbox', { name: '账号名称', exact: true }).fill('尚未应用的筛选')
    await expect(page.getByRole('button', { name: '批量查询本页 IP 归属地', exact: true })).toBeDisabled()
    await page.getByRole('textbox', { name: '账号名称', exact: true }).fill('')
    await page.getByRole('button', { name: '刷新日志', exact: true }).click()
    await expect(page.getByRole('button', { name: '导出全部筛选', exact: true })).toBeEnabled()
    assertions.push('批量取消后迟到结果不当成功；草稿筛选未提交时禁止查询旧页IP')
    return { assertions, calls }
  } finally { release?.(); await page.unroute('**/api/logs**', handler) }
}
