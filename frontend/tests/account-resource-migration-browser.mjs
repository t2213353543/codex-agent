import assert from 'node:assert/strict'
import { expect } from 'playwright/test'
import { account } from './mock-api.mjs'

export async function testAccountResourceMigration(page, { capture = async () => {} } = {}) {
  const origin = new URL(page.url()).origin
  assert.ok(['localhost', '127.0.0.1'].includes(new URL(origin).hostname))
  const row = account('resource-account-fixture', '关联迁移合成账号', { version: 4 })
  const calls = [], assertions = []
  let resourcesCreated = false
  const resources = { version: 1, groups: [{ id: 'source-group', name: '备份分组', description: '合成' }], proxies: [{ id: 'source-proxy', name: '备份代理', type: 'http', host: 'proxy.example.test', port: 8080, enabled: false, has_credentials: true, credentials_included: true, username: 'synthetic-proxy-user', password: 'synthetic-proxy-password' }] }
  const backup = { type: 'codex-agent-accounts', version: 1, accounts: [{ name: '关联恢复账号', groups: ['source-group'], proxies: ['source-proxy'], credentials: { access_token: 'synthetic-resource-account-token' } }], resources }
  const handler = async route => {
    const request = route.request(), url = new URL(request.url()), body = request.postDataJSON()
    calls.push({ path: url.pathname, method: request.method(), body })
    const reply = json => route.fulfill({ json })
    if (url.pathname === '/api/accounts/manage') return reply({ accounts: [row], total: 1, total_kind: 'exact', has_more: false })
    if (url.pathname === '/api/groups') return reply({ groups: resourcesCreated ? [{ id: 'created-group', name: '备份分组' }] : [] })
    if (url.pathname === '/api/proxies') return reply({ proxies: resourcesCreated ? [{ id: 'created-proxy', name: '备份代理', enabled: false }] : [] })
    if (url.pathname === '/api/accounts/export/credentials') return reply(backup)
    if (url.pathname === '/api/accounts/import-resources') {
      resourcesCreated = !body.dry_run
      return reply({ dry_run: body.dry_run, results: [{ kind: 'group', source_id: 'source-group', status: body.dry_run ? 'create' : 'created', ...(!body.dry_run ? { id: 'created-group' } : {}) }, { kind: 'proxy', source_id: 'source-proxy', status: body.dry_run ? 'create' : 'created', ...(!body.dry_run ? { id: 'created-proxy' } : {}) }], group_mapping: body.dry_run ? {} : { 'source-group': 'created-group' }, proxy_mapping: body.dry_run ? {} : { 'source-proxy': 'created-proxy' }, succeeded: 2, failed: 0 })
    }
    if (url.pathname === '/api/accounts/import-bundle') return reply({ dry_run: body.dry_run, imported: 1, added: 1, failed: 0, results: [{ index: 1, name: '关联恢复账号', id: 'restored-account', status: 'added' }] })
    return route.fallback()
  }
  await page.route('**/api/**', handler)
  const tick = async locator => locator.locator('xpath=ancestor::label').click()
  try {
    await page.goto(origin + '/#/accounts')
    await page.getByRole('button', { name: '刷新账号列表', exact: true }).click()
    await expect(page.locator('.accounts-panel')).toContainText(row.name)
    await page.getByRole('button', { name: '迁移备份', exact: true }).click()
    await page.getByRole('menuitem', { name: '导出账号', exact: true }).click()
    let dialog = page.getByRole('dialog', { name: '导出账号', exact: true })
    await tick(dialog.getByRole('checkbox', { name: '导出可迁移凭据备份', exact: true }))
    await tick(dialog.getByRole('checkbox', { name: '包含所选账号关联的分组与代理定义', exact: true }))
    await tick(dialog.getByRole('checkbox', { name: '同时包含关联代理的用户名和密码', exact: true }))
    await dialog.getByRole('button', { name: '生成并下载', exact: true }).click()
    await page.getByRole('button', { name: '确认导出凭据', exact: true }).click()
    assert.equal(calls.filter(call => call.path === '/api/accounts/export/credentials').length, 0)
    const download = page.waitForEvent('download')
    await page.getByRole('button', { name: '确认包含代理密码', exact: true }).click()
    await download
    await expect(dialog).toContainText('已生成 1 个账号的凭据备份')
    const exported = calls.find(call => call.path === '/api/accounts/export/credentials').body
    assert.equal(exported.include_resources, true); assert.equal(exported.include_proxy_credentials, true); assert.equal(exported.confirm_proxy_credentials, true)
    assert.ok(!(await page.locator('body').innerText()).includes('synthetic-proxy-password'))
    await dialog.getByRole('button', { name: '关闭', exact: true }).click()
    assertions.push('关联资源导出显式选择，账号令牌/代理密码分别确认，下载前不请求且DOM不显示密码')
    await page.getByRole('button', { name: '迁移备份', exact: true }).click()
    await page.getByRole('menuitem', { name: '恢复凭据备份', exact: true }).click()
    dialog = page.getByRole('dialog', { name: '恢复账号备份', exact: true })
    await dialog.getByLabel('账号备份文件', { exact: true }).setInputFiles({ name: 'resources-fixture.json', mimeType: 'application/json', buffer: Buffer.from(JSON.stringify(backup)) })
    await dialog.getByRole('button', { name: '预览资源恢复', exact: true }).click()
    await expect(dialog).toContainText('请明确确认导入')
    assert.equal(calls.filter(call => call.path === '/api/accounts/import-resources').length, 0)
    await tick(dialog.getByRole('checkbox', { name: '我确认导入备份中的代理用户名和密码，用于创建所选关联代理', exact: true }))
    await dialog.getByRole('button', { name: '预览资源恢复', exact: true }).click()
    await expect(dialog).toContainText('尚未创建资源')
    assert.equal(resourcesCreated, false)
    await dialog.getByRole('button', { name: '确认创建或映射资源', exact: true }).click()
    await expect(dialog).toContainText('资源阶段已执行')
    await expect(dialog.locator('.resource-results')).toContainText('created-proxy')
    await expect(dialog).toContainText('不会因后续账号失败而自动撤销')
    await dialog.getByRole('button', { name: '生成预览', exact: true }).click()
    await expect(dialog).toContainText('预览完成，尚未写入账号')
    const accountPreview = calls.filter(call => call.path === '/api/accounts/import-bundle').at(-1).body
    assert.equal(accountPreview.group_mapping['source-group'], 'created-group')
    assert.equal(accountPreview.proxy_mapping['source-proxy'], 'created-proxy')
    assert.equal(accountPreview.dry_run, true)
    assert.ok(!(await dialog.innerText()).includes('synthetic-proxy-password'))
    await capture('account-resource-restore-1440')
    await page.setViewportSize({ width: 320, height: 900 })
    assert.ok(await dialog.evaluate(element => element.scrollWidth <= element.clientWidth + 1))
    await capture('account-resource-restore-320')
    await dialog.getByRole('button', { name: '确认恢复', exact: true }).click()
    await expect(dialog).toContainText('恢复已执行')
    assert.deepEqual(calls.filter(call => call.path === '/api/accounts/import-resources').map(call => call.body.dry_run), [true, false])
    assert.deepEqual(calls.filter(call => call.path === '/api/accounts/import-bundle').map(call => call.body.dry_run), [true, false])
    await dialog.getByRole('button', { name: '关闭', exact: true }).click()
    assertions.push('资源密码导入明确确认、资源dry-run零写/创建映射、账号独立dry-run和恢复、分阶段说明/320px/密码无回显')
    return assertions
  } finally { await page.unroute('**/api/**', handler); await page.setViewportSize({ width: 1440, height: 1000 }) }
}
