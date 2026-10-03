import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import { expect } from 'playwright/test'
import { account } from './mock-api.mjs'

export async function testAccountsManagement(page) {
  const origin = new URL(page.url()).origin
  assert.ok(['127.0.0.1', 'localhost'].includes(new URL(origin).hostname))
  const assertions = []
  const requests = []
  const rows = Array.from({ length: 43 }, (_, index) => account(`managed-${String(index + 1).padStart(2, '0')}`, `账号 ${String(index + 1).padStart(2, '0')}`, { version: 2, enabled: index % 3 !== 0, status: index % 5 === 0 ? 'error' : 'ok', tags: ['work'], notes: `合成备注 ${index}` }))
  let failList = false
  let failExport = false
  let failReauthorization = false
  const filterRows = filters => rows.filter(row => (!filters.search || row.name.includes(filters.search) || row.id.includes(filters.search)) && (!filters.status || row.status === filters.status) && (filters.enabled === undefined || row.enabled === (String(filters.enabled) === 'true')))
  const handler = async route => {
    const request = route.request()
    const url = new URL(request.url())
    const method = request.method()
    const body = request.postDataJSON()
    const params = Object.fromEntries(url.searchParams)
    requests.push({ path: url.pathname, method, body, params })
    const reply = (json, status = 200) => route.fulfill({ status, json }).catch(() => {})
    if (url.pathname === '/api/accounts/manage') {
      if (params.search === 'slow') await new Promise(resolve => setTimeout(resolve, 350))
      if (failList) return reply({ error: { message: '合成列表读取失败' } }, 503)
      const filtered = filterRows(params)
      const size = Number(params.page_size) || 20
      const current = Number(params.page) || 1
      const sorted = [...filtered].sort((a, b) => a.name.localeCompare(b.name) * (params.sort_order === 'asc' ? 1 : -1))
      return reply({ accounts: sorted.slice((current - 1) * size, current * size), total: filtered.length, total_kind: 'exact', page: current, page_size: size, has_more: current * size < filtered.length })
    }
    if (url.pathname === '/api/accounts/selection') {
      for (const key of ['enabled', 'has_refresh_token', 'ungrouped', 'no_proxy']) if (Object.hasOwn(body.filters, key)) assert.equal(typeof body.filters[key], 'boolean')
      const filtered = filterRows(body.filters)
      return reply({ items: filtered.map(({ id, version }) => ({ id, version })), total: filtered.length, filter_hash: 'synthetic', selected_at: 1800000000, limit: 10000 })
    }
    if (url.pathname === '/api/accounts/actions') return reply({ results: body.items.map(item => ({ id: item.id, ok: true, version: item.version + 1 })), succeeded: body.items.length, failed: 0 })
    if (url.pathname.startsWith('/api/accounts/export')) {
      if (failExport) return reply({ error: { message: '合成导出失败' } }, 503)
      const filtered = body.items ? rows.filter(row => body.items.some(item => item.id === row.id)) : filterRows(body.filters)
      const credentials = url.pathname.endsWith('/credentials')
      return reply({ type: credentials ? 'codex-agent-accounts' : 'codex-agent-account-metadata', version: 1, accounts: filtered.map(row => ({ name: row.name, notes: row.notes, ...(credentials ? { credentials: { access_token: 'synthetic-export-credential', refresh_token: 'synthetic-refresh-credential' } } : {}) })) })
    }
    if (/\/reauthorize$/.test(url.pathname)) return failReauthorization ? reply({ error: { message: '账号版本冲突' } }, 409) : reply({ id: url.pathname.split('/')[3], version: 3, ok: true })
    if (url.pathname === '/api/accounts/import-bundle') return reply({ imported: body.dry_run ? 0 : 1, added: 1, duplicate: 0, replaced: 0, failed: 0, dry_run: body.dry_run, results: [{ index: 1, name: '备份账号', id: 'backup-example', status: 'added' }] })
    const id = url.pathname.split('/')[3]
    if (method === 'GET' && rows.some(row => row.id === id)) return reply({ account: rows.find(row => row.id === id) })
    return route.fallback()
  }
  await page.route('**/api/accounts**', handler)
  const closeDialog = async name => { const dialog = page.getByRole('dialog', { name, exact: true }); await dialog.getByRole('button', { name: '关闭', exact: true }).last().click(); await expect(dialog).toBeHidden() }
  const settle = async () => { await expect(page.locator('.el-loading-mask:visible')).toHaveCount(0) }
  const openExport = async () => { await page.getByRole('button', { name: '迁移备份', exact: true }).click(); await page.getByRole('menuitem', { name: '导出账号', exact: true }).click(); return page.getByRole('dialog', { name: '导出账号', exact: true }) }
  try {
    await page.goto(origin + '/#/accounts?sort_by=name&sort_order=asc&page=2&page_size=10')
    await expect(page.locator('.account-directory-heading')).toContainText('共 43 个')
    await expect(page.locator('.account-pagination')).toContainText('第 2 / 5 页')
    await settle()
    await page.getByRole('checkbox', { name: '选择本页全部账号', exact: true }).locator('xpath=ancestor::label').click()
    await expect(page.locator('.account-summary')).toContainText('已选择 10 个')
    await page.getByRole('button', { name: '下一页', exact: true }).click()
    await expect(page.locator('.account-pagination')).toContainText('第 3 / 5 页')
    await settle()
    await expect(page.locator('.account-summary')).toContainText('已选择 10 个')
    await page.getByRole('button', { name: '选择全部 43 个', exact: true }).click()
    await expect(page.locator('.account-summary')).toContainText('已选择 43 个（全部结果）')
    await page.getByRole('button', { name: '批量编辑', exact: true }).click()
    let dialog = page.getByRole('dialog', { name: '批量管理', exact: true })
    await dialog.getByRole('checkbox', { name: '并发容量', exact: true }).locator('xpath=ancestor::label').click()
    await dialog.getByRole('spinbutton', { name: '批量并发容量', exact: true }).fill('7')
    await dialog.getByRole('checkbox', { name: '分组', exact: true }).locator('xpath=ancestor::label').click()
    await dialog.getByRole('button', { name: '执行编辑配置', exact: true }).click()
    await expect(dialog).toContainText('成功 43')
    const update = requests.find(entry => entry.path === '/api/accounts/actions')
    assert.deepEqual(update.body.patch, { concurrency: 7, groups: [] })
    assert.equal(update.body.items.length, 43)
    assert.equal(update.body.items[0].version, 2)
    await closeDialog('批量管理')
    assertions.push('服务端精确分页、跨页选择、全部 ID+version 快照、只修改勾选字段')

    await page.getByRole('button', { name: '选择全部 43 个', exact: true }).click()
    await expect(page.locator('.account-summary')).toContainText('已选择 43 个')
    await page.locator('.selection-actions').getByRole('button', { name: '更多操作', exact: true }).click()
    await page.getByRole('menuitem', { name: '批量刷新令牌', exact: true }).click()
    dialog = page.getByRole('dialog', { name: '批量管理', exact: true })
    await dialog.getByRole('button', { name: '执行刷新令牌', exact: true }).click()
    await expect(dialog).toContainText('成功 43')
    assert.deepEqual(requests.filter(entry => entry.path === '/api/accounts/actions' && entry.body.action === 'refresh').map(entry => entry.body.items.length), [10, 10, 10, 10, 3])
    await closeDialog('批量管理')
    assertions.push('批量刷新按 10 个一组，逐项结果与进度准确')

    await page.getByRole('button', { name: '筛选与排序', exact: true }).click()
    await page.getByRole('combobox', { name: '启用状态筛选', exact: true }).press('Enter')
    await page.getByRole('option', { name: '已停用', exact: true }).click()
    await expect(page.locator('.account-pagination')).toContainText('第 1 / 2 页')
    await page.getByRole('button', { name: '选择全部 15 个', exact: true }).click()
    await expect(page.locator('.account-summary')).toContainText('已选择 15 个')
    assert.equal(requests.filter(entry => entry.path === '/api/accounts/selection').at(-1).body.filters.enabled, false)
    await page.getByRole('button', { name: '取消选择', exact: true }).click()
    await page.getByRole('button', { name: '清除筛选（1）', exact: true }).click()
    await expect(page.locator('.account-directory-heading')).toContainText('共 43 个')
    await page.getByRole('textbox', { name: '搜索账号', exact: true }).fill('slow')
    await page.waitForRequest(request => new URL(request.url()).pathname === '/api/accounts/manage' && new URL(request.url()).searchParams.get('search') === 'slow')
    await page.getByRole('textbox', { name: '搜索账号', exact: true }).fill('账号 01')
    await expect(page.locator('.account-directory-heading')).toContainText('共 1 个')
    await expect(page.locator('.accounts-panel .el-table__body')).toContainText('账号 01')
    await page.waitForTimeout(400)
    await expect(page.locator('.account-directory-heading')).toContainText('共 1 个')
    assertions.push('筛选原子回首页、JSON false 正确、取消旧查询防覆盖')

    const shortcuts = page.getByRole('group', { name: '快速筛选账号状态', exact: true })
    await shortcuts.getByRole('button', { name: '异常', exact: true }).click()
    await expect(shortcuts.getByRole('button', { name: '异常', exact: true })).toHaveAttribute('aria-pressed', 'true')
    await expect.poll(() => new URLSearchParams(page.url().split('?')[1]).get('status')).toBe('error')
    await expect(page.getByRole('textbox', { name: '搜索账号', exact: true })).toHaveValue('账号 01')
    await shortcuts.getByRole('button', { name: '正常', exact: true }).click()
    await expect(page.locator('.account-directory-heading')).toContainText('共 0 个')
    await shortcuts.getByRole('button', { name: '全部状态', exact: true }).click()
    await expect(page.locator('.account-directory-heading')).toContainText('共 1 个')
    assertions.push('状态快捷筛选按服务端状态查询，保留搜索词并正确显示无匹配结果')

    dialog = await openExport()
    let downloadPromise = page.waitForEvent('download')
    await dialog.getByRole('button', { name: '生成并下载', exact: true }).click()
    let download = await downloadPromise
    const metadata = await readFile(await download.path(), 'utf8')
    assert.equal(JSON.parse(metadata).accounts.length, 1)
    assert.ok(!/access_token|refresh_token|synthetic-export/.test(metadata))
    await dialog.getByRole('checkbox', { name: '导出可迁移凭据备份', exact: true }).locator('xpath=ancestor::label').click()
    await dialog.getByRole('button', { name: '生成并下载', exact: true }).click()
    downloadPromise = page.waitForEvent('download')
    await page.getByRole('button', { name: '确认导出凭据', exact: true }).click()
    download = await downloadPromise
    const sensitive = await readFile(await download.path(), 'utf8')
    assert.equal(JSON.parse(sensitive).type, 'codex-agent-accounts')
    assert.ok(sensitive.includes('synthetic-export-credential'))
    assert.ok(!(await page.locator('body').innerText()).includes('synthetic-export-credential'))
    await closeDialog('导出账号')
    failExport = true
    dialog = await openExport()
    await dialog.getByRole('button', { name: '生成并下载', exact: true }).click()
    await expect(dialog).toContainText('合成导出失败')
    await closeDialog('导出账号')
    failExport = false
    assertions.push('完整筛选导出、默认无凭据、凭据显式确认下载、不回显、导出失败可见')

    await page.getByRole('button', { name: '查看账号 账号 01 详情', exact: true }).click()
    const drawer = page.getByRole('dialog', { name: '账号详情', exact: true })
    await drawer.getByRole('button', { name: '重新授权', exact: true }).click()
    dialog = page.getByRole('dialog', { name: '重新授权账号', exact: true })
    await dialog.getByRole('textbox', { name: '重新授权凭据 JSON', exact: true }).fill(JSON.stringify({ tokens: { access_token: 'synthetic-reauth-secret', account_id: 'synthetic-upstream-id' }, agent_identity: { account_id: 'synthetic-upstream-id', chatgpt_user_id: 'synthetic-user', agent_runtime_id: 'synthetic-runtime', agent_private_key: 'synthetic-not-a-key' } }))
    failReauthorization = true
    await dialog.getByRole('button', { name: '替换凭据', exact: true }).click()
    await expect(dialog).toContainText('账号版本或登录身份已变化')
    await expect(dialog).toBeVisible()
    failReauthorization = false
    await dialog.getByRole('button', { name: '替换凭据', exact: true }).click()
    await expect(dialog).toBeHidden()
    const replacement = requests.filter(entry => entry.path.endsWith('/reauthorize')).at(-1)
    assert.equal(replacement.path, '/api/accounts/managed-01/reauthorize')
    assert.equal(replacement.body.version, 2)
    assert.equal(replacement.body.allow_identity_change, false)
    assert.equal(replacement.body.credentials.tokens.access_token, 'synthetic-reauth-secret')
    assert.equal(replacement.body.credentials.agent_identity.chatgpt_user_id, 'synthetic-user')
    await drawer.locator('.el-dialog__headerbtn').click()
    assertions.push('目标 ID 重授权、版本冲突保留输入、成功清除凭据')

    await page.getByRole('button', { name: '迁移备份', exact: true }).click()
    await page.getByRole('menuitem', { name: '恢复凭据备份', exact: true }).click()
    dialog = page.getByRole('dialog', { name: '恢复账号备份', exact: true })
    await dialog.getByLabel('账号备份文件', { exact: true }).setInputFiles({ name: 'synthetic-backup.json', mimeType: 'application/json', buffer: Buffer.from(JSON.stringify({ type: 'codex-agent-accounts', version: 1, accounts: [{ name: '备份账号', groups: ['old-group'], proxies: [], credentials: { access_token: 'synthetic-backup-secret' } }] })) })
    await expect(dialog).toContainText('1 个账号')
    await dialog.getByRole('combobox', { name: '备份分组映射 old-group', exact: true }).press('Enter')
    await page.getByRole('option', { name: '测试分组', exact: true }).click()
    await dialog.getByRole('button', { name: '生成预览', exact: true }).click()
    await expect(dialog).toContainText('尚未写入账号')
    const preview = requests.filter(entry => entry.path === '/api/accounts/import-bundle').at(-1)
    assert.equal(preview.body.dry_run, true)
    assert.deepEqual(preview.body.group_mapping, { 'old-group': 'g1' })
    await dialog.getByRole('button', { name: '确认恢复', exact: true }).click()
    await expect(dialog).toContainText('恢复已执行')
    assert.equal(requests.filter(entry => entry.path === '/api/accounts/import-bundle').at(-1).body.dry_run, false)
    assert.ok(!(await dialog.innerText()).includes('synthetic-backup-secret'))
    await closeDialog('恢复账号备份')
    assertions.push('凭据备份读取、关联映射、dry-run 零写入、明确确认恢复与结果')

    failList = true
    await page.getByRole('button', { name: '刷新账号列表', exact: true }).click()
    await expect(page.locator('.accounts-page')).toContainText('合成列表读取失败')
    await expect(page.locator('.accounts-panel .el-table__body')).toContainText('账号 01')
    failList = false
    await page.getByRole('button', { name: '刷新账号列表', exact: true }).click()
    await settle()
    for (const width of [390, 320]) {
      await page.setViewportSize({ width, height: 900 })
      await expect(page.locator('.mobile-account')).toHaveCount(1)
      await page.getByRole('checkbox', { name: '选择本页全部账号', exact: true }).locator('xpath=ancestor::label').click()
      await expect(page.locator('.account-summary')).toContainText('已选择 1 个')
      assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1), `${width}px overflow`)
      await page.getByRole('button', { name: '取消选择', exact: true }).click()
    }
    assertions.push('列表失败保留数据、390/320 连续手机列表与选择无溢出')
    return assertions
  } catch (error) { error.message += `\nAccount fixture location: ${page.url()}\nRecent list queries: ${JSON.stringify(requests.filter(entry => entry.path === '/api/accounts/manage').slice(-5).map(entry => entry.params))}`; throw new Error(error.message, { cause: error }) }
  finally { await page.unroute('**/api/accounts**', handler); await page.setViewportSize({ width: 1440, height: 1000 }) }
}
