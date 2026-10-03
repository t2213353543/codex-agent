import assert from 'node:assert/strict'
import { expect } from 'playwright/test'
import { account } from './mock-api.mjs'

export async function testAccountLeaseBackup(page, { capture = async () => {} } = {}) {
  const origin = new URL(page.url()).origin
  assert.ok(['127.0.0.1', 'localhost'].includes(new URL(origin).hostname))
  const now = Math.floor(Date.now() / 1000)
  const configs = [{ account_index: 1, expires_at: now + 86400, auto_disable: true, restore_on_extend: true }, { account_index: 2, expires_at: now - 60, auto_disable: true, restore_on_extend: true }]
  const bundle = { type: 'codex-agent-accounts', version: 1, accounts: [{ name: '新租期目标', groups: [], proxies: [], credentials: { access_token: 'synthetic-lease-secret-one' } }, { name: '过期租期目标', groups: [], proxies: [], credentials: { access_token: 'synthetic-lease-secret-two' } }], management: { version: 1, leases: configs } }
  bundle.accounts[0].groups = ['g1']; bundle.accounts[0].proxies = ['p1']
  bundle.resources = { version: 1, groups: [{ id: 'g1', name: '测试分组', description: '仅合成数据' }], proxies: [{ id: 'p1', name: '测试代理', type: 'http', host: '127.0.0.1', port: 8080, enabled: true, has_credentials: false, credentials_included: false }] }
  const receipt = { receipt_id: 'lease_restore_fixture', expires_at: now + 1800, dry_run: false, items: configs.map((item, index) => ({ ...item, name: bundle.accounts[index].name, id: `restored-id-${index}`, account_version: 3 + index, lease_version: index, status: 'pending', expired: index === 1, would_disable: index === 1, replaces_existing: index === 1 })) }
  const calls = [], assertions = []
  let committed = false, unknown = false
  let holdCommit = true, releaseCommit
  const handler = async route => {
    const req = route.request(), url = new URL(req.url()), body = req.postDataJSON()
    calls.push({ path: url.pathname, method: req.method(), body })
    const reply = json => route.fulfill({ json })
    if (url.pathname === '/api/accounts/manage') return reply({ accounts: [account('lease-export-source', '租期导出样例')], total: 1, total_kind: 'exact', has_more: false })
    if (url.pathname === '/api/accounts/export/credentials') return reply(bundle)
    if (url.pathname === '/api/accounts/import-bundle') return reply({ dry_run: body.dry_run, added: 1, duplicate: 1, replaced: 0, failed: 0, imported: 1, results: bundle.accounts.map((item, i) => ({ index: i + 1, name: item.name, id: receipt.items[i].id, status: i ? 'duplicate' : 'added' })), lease_restore: body.dry_run ? { dry_run: true, items: configs.map((item, i) => ({ ...item, name: bundle.accounts[i].name, status: 'pending_import', expired: i === 1, would_disable: i === 1 })) } : JSON.parse(JSON.stringify(receipt)) })
    if (url.pathname === '/api/accounts/import-leases') {
      assert.equal(body.receipt_id, receipt.receipt_id)
      assert.deepEqual(body.items.map(item => [item.id, item.account_version, item.lease_version]), [['restored-id-0', 3, 0], ['restored-id-1', 4, 1]])
      if (!body.dry_run) { assert.equal(body.confirm_restore, true); assert.equal(body.confirm_expired, true); assert.equal(body.confirm_replace, true); committed = true; if (holdCommit) await new Promise(resolve => { releaseCommit = resolve }) }
      if (!body.dry_run && unknown) return route.abort('failed')
      return reply({ ...receipt, dry_run: body.dry_run, items: receipt.items.map((item, i) => ({ ...item, status: body.dry_run ? 'pending' : i ? 'failed' : 'applied', ...(!body.dry_run && i ? { code: 'version_conflict' } : {}) })) })
    }
    if (url.pathname === '/api/accounts/import-leases/receipts/' + receipt.receipt_id) return reply({ ...receipt, items: receipt.items.map(item => ({ ...item, status: 'unknown', code: 'result_not_confirmed' })) })
    return route.fallback()
  }
  const tick = async locator => locator.locator('xpath=ancestor::label').click()
  const openRestore = async () => {
    await page.getByRole('button', { name: '迁移备份', exact: true }).click()
    await page.getByRole('menuitem', { name: '恢复凭据备份', exact: true }).click()
    const dialog = page.getByRole('dialog', { name: '恢复账号备份', exact: true })
    await dialog.getByLabel('账号备份文件', { exact: true }).setInputFiles({ name: 'lease-fixture.json', mimeType: 'application/json', buffer: Buffer.from(JSON.stringify(bundle)) })
    return dialog
  }
  await page.route('**/api/**', handler)
  try {
    await page.goto(origin + '/#/accounts')
    await page.getByRole('button', { name: '刷新账号列表', exact: true }).click()
    await page.getByRole('button', { name: '迁移备份', exact: true }).click()
    await page.getByRole('menuitem', { name: '导出账号', exact: true }).click()
    let dialog = page.getByRole('dialog', { name: '导出账号', exact: true })
    await tick(dialog.getByRole('checkbox', { name: '导出可迁移凭据备份', exact: true }))
    await tick(dialog.getByRole('checkbox', { name: '包含账号独立租期配置', exact: true }))
    await dialog.getByRole('button', { name: '生成并下载', exact: true }).click()
    assert.equal(calls.filter(item => item.path === '/api/accounts/export/credentials').length, 0)
    const download = page.waitForEvent('download')
    await page.getByRole('button', { name: '确认导出凭据', exact: true }).click()
    await download
    assert.equal(calls.find(item => item.path === '/api/accounts/export/credentials').body.include_leases, true)
    await dialog.getByRole('button', { name: '关闭', exact: true }).click()
    assertions.push('租期导出独立选择，凭据确认后才POST，文件含纯配置管理段')
    dialog = await openRestore()
    await expect(dialog).toContainText('不会自动应用租期')
    await dialog.getByRole('button', { name: '生成预览', exact: true }).click()
    await expect(dialog.locator('.lease-restore')).toContainText('等待账号导入')
    assert.equal(calls.filter(item => item.path === '/api/accounts/import-leases').length, 0)
    await dialog.getByRole('button', { name: '确认恢复', exact: true }).click()
    await expect(dialog.locator('.lease-restore')).toContainText('restored-id-0')
    await expect(dialog.locator('.lease-restore')).toContainText('目标已有租期配置')
    assert.equal(committed, false)
    await dialog.getByRole('button', { name: '预览租期恢复', exact: true }).click()
    await expect(dialog).toContainText('租期预览完成，尚未修改租期')
    await expect(dialog.getByRole('button', { name: '确认恢复租期', exact: true })).toBeDisabled()
    await tick(dialog.getByRole('checkbox', { name: '我确认将备份租期应用到以上固定 ID 的账号', exact: true }))
    await tick(dialog.getByRole('checkbox', { name: '我确认覆盖这些账号已有的本地租期配置', exact: true }))
    await expect(dialog.getByRole('button', { name: '确认恢复租期', exact: true })).toBeDisabled()
    await tick(dialog.getByRole('checkbox', { name: '我确认已过期且开启自动停用的租期可能立即停用账号', exact: true }))
    await capture('lease-backup-ready-1440')
    await page.setViewportSize({ width: 320, height: 1000 })
    assert.ok(await dialog.evaluate(element => element.scrollWidth <= element.clientWidth + 1))
    await capture('lease-backup-ready-320')
    await dialog.getByRole('button', { name: '确认恢复租期', exact: true }).click()
    await expect.poll(() => Boolean(releaseCommit)).toBe(true)
    await expect(dialog.getByRole('checkbox', { name: '允许替换同名账号的凭据', exact: true })).toBeDisabled()
    await expect(dialog.getByRole('combobox', { name: '备份分组映射 g1', exact: true })).toBeDisabled()
    await expect(dialog.getByRole('combobox', { name: '备份代理映射 p1', exact: true })).toBeDisabled()
    await expect(dialog.getByRole('button', { name: '预览资源恢复', exact: true })).toBeDisabled()
    await expect(dialog.getByRole('button', { name: '确认创建或映射资源', exact: true })).toBeDisabled()
    await expect(dialog.getByLabel('账号备份文件', { exact: true })).toBeDisabled()
    await expect(dialog.getByRole('button', { name: '关闭', exact: true })).toBeDisabled()
    await expect(dialog.locator('.lease-restore')).toHaveCount(1)
    holdCommit = false; releaseCommit()
    await expect(dialog.locator('.lease-restore')).toContainText('租期已恢复')
    await expect(dialog.locator('.lease-restore')).toContainText('版本已变化')
    await expect(dialog.getByRole('button', { name: '确认恢复租期', exact: true })).toBeDisabled()
    assert.equal(committed, true)
    assert.ok(!(await dialog.innerText()).includes('synthetic-lease-secret'))
    await dialog.getByRole('button', { name: '关闭', exact: true }).click()
    assertions.push('账号与租期双预览、固定ID双版本、覆盖/过期确认；租期提交锁定replace/映射/资源/文件/关闭，部分失败与320正常')
    await page.setViewportSize({ width: 1440, height: 1000 })
    unknown = true; dialog = await openRestore()
    await dialog.getByRole('button', { name: '生成预览', exact: true }).click()
    await dialog.getByRole('button', { name: '确认恢复', exact: true }).click()
    await dialog.getByRole('button', { name: '预览租期恢复', exact: true }).click()
    for (const name of ['我确认将备份租期应用到以上固定 ID 的账号', '我确认覆盖这些账号已有的本地租期配置', '我确认已过期且开启自动停用的租期可能立即停用账号']) await tick(dialog.getByRole('checkbox', { name, exact: true }))
    await dialog.getByRole('button', { name: '确认恢复租期', exact: true }).click()
    await expect(dialog.locator('.lease-restore')).toContainText('部分租期结果尚未确认')
    await expect(dialog.getByRole('button', { name: '确认恢复租期', exact: true })).toBeDisabled()
    await dialog.getByRole('button', { name: '读取租期恢复回执', exact: true }).click()
    await expect(dialog.locator('.lease-restore')).toContainText('结果未知')
    await expect(dialog.getByRole('button', { name: '确认恢复租期', exact: true })).toBeDisabled()
    await dialog.getByRole('button', { name: '关闭', exact: true }).click()
    assertions.push('提交断线不自动重试，GET读取回执，unknown项仍不可重新执行')
    return assertions
  } finally { releaseCommit?.(); await page.unroute('**/api/**', handler) }
}
