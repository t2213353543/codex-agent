import assert from 'node:assert/strict'
import { mkdir } from 'node:fs/promises'
import path from 'node:path'
import { expect } from 'playwright/test'

export async function testManagementPageLayout(page, output) {
  const base = new URL(page.url()).origin
  assert.ok(['localhost', '127.0.0.1'].includes(new URL(base).hostname))
  await mkdir(output, { recursive: true })
  const screenshots = []
  const assertions = []
  const capture = async (name) => {
    await page.evaluate(() => document.fonts.ready)
    await expect.poll(() => page.locator('.el-loading-mask').count()).toBe(0)
    const overflow = await page.evaluate(() => {
      const content = document.querySelector('.content')
      return { document: document.documentElement.scrollWidth - innerWidth, content: content.scrollWidth - content.clientWidth }
    })
    assert.ok(overflow.document <= 1 && overflow.content <= 1, `${name}: overflow ${JSON.stringify(overflow)}`)
    const viewport = page.viewportSize()
    const requiredHeight = await page.evaluate(() => document.querySelector('.content').scrollHeight + 130)
    if (requiredHeight > viewport.height && !await page.getByRole('dialog').isVisible().catch(() => false)) await page.setViewportSize({ ...viewport, height: Math.min(requiredHeight, 5000) })
    await page.screenshot({ path: path.join(output, `${name}.png`), fullPage: true })
    await page.setViewportSize(viewport)
    screenshots.push(`${name}.png`)
  }
  for (const theme of ['light', 'dark']) {
    await page.evaluate((value) => localStorage.setItem('theme', JSON.stringify({ version: 1, value })), theme)
    for (const width of [1440, 390, 320]) {
      await page.setViewportSize({ width, height: 1100 })
      await page.goto(`${base}/#/?range=24h`)
      await page.reload()
      await expect(page.locator('.dashboard-page')).toHaveAttribute('aria-busy', 'false')
      await expect(page.locator('.kpi-card')).toHaveCount(4)
      assert.equal(await page.locator('.scope-summary').getAttribute('open'), null)
      assert.equal(await page.locator('.details-panel').getAttribute('open'), null)
      await expect(page.locator('.traffic-chart')).toBeVisible()
      await expect(page.locator('.incident-list')).toBeVisible()
      await capture(`dashboard-${theme}-${width}`)
      await page.goto(`${base}/#/accounts`)
      await expect(page.locator('.account-overview-card')).toHaveCount(4)
      await expect(page.locator('#account-advanced-filters')).toBeHidden()
      await expect.poll(() => page.locator('.el-loading-mask').count()).toBe(0)
      const rowHeights = await page.locator('.accounts-panel .el-table tbody tr').evaluateAll((rows) => rows.map((row) => row.getBoundingClientRect().height))
      assert.ok(rowHeights.every((height) => height < 130), `Account names must not expand rows indefinitely: ${rowHeights}`)
      if (width < 768) await expect(page.locator('.accounts-panel .el-table-fixed-column--left, .accounts-panel .el-table-fixed-column--right')).toHaveCount(0)
      await capture(`accounts-${theme}-${width}`)
    }
  }
  assertions.push('运行概览与账号目录：浅色/深色 × 1440/390/320px，无页面横向溢出，保留所有主操作')

  await page.setViewportSize({ width: 1440, height: 1100 })
  await page.getByRole('button', { name: '更多筛选', exact: true }).click()
  await expect(page.getByRole('combobox', { name: '分组筛选', exact: true })).toBeVisible()
  await expect(page.getByRole('combobox', { name: '代理筛选', exact: true })).toBeVisible()
  await expect(page.getByRole('combobox', { name: '当前页账号排序', exact: true })).toBeVisible()
  await capture('accounts-advanced-filters')
  await page.locator('.accounts-panel .el-table tbody tr').first().getByRole('button', { name: /查看账号 .* 详情/ }).click()
  const drawer = page.getByRole('dialog', { name: '账号详情', exact: true })
  await expect(drawer.locator('.detail-quick-actions')).toBeVisible()
  await expect(drawer.getByRole('button', { name: '编辑账号', exact: true })).toBeEnabled()
  await capture('account-detail-desktop')
  await page.setViewportSize({ width: 390, height: 1000 })
  await capture('account-detail-mobile')
  await drawer.getByRole('button', { name: '查看使用记录', exact: true }).click()
  await expect(page.locator('.logs-page')).toBeVisible()
  assert.ok(new URLSearchParams(new URL(page.url()).hash.split('?')[1]).get('account_id'))
  assertions.push('高级筛选按需展开；账号详情快速编辑与按稳定 ID 直达使用记录可用')

  await page.goto(`${base}/#/?range=24h`)
  await expect(page.locator('.dashboard-page')).toHaveAttribute('aria-busy', 'false')
  await page.locator('.details-panel > summary').click()
  await expect(page.locator('[data-metric="input"]')).toBeVisible()
  await page.locator('.scope-summary > summary').click()
  await expect(page.locator('[data-metric="tokens_total"]')).toBeVisible()
  await capture('dashboard-expanded-mobile')
  assertions.push('详细吞吐与今日/累计指标折叠保留，展开后移动端可读取')

  const emptyAccountHandler = (route) => route.fulfill({ json: { accounts: [], has_more: false, next_cursor: '' } })
  await page.route('**/api/accounts?**', emptyAccountHandler)
  try {
    await page.goto(`${base}/#/accounts`)
    await expect(page.getByText('还没有导入账号', { exact: true })).toBeVisible()
    await page.getByRole('button', { name: '开始导入', exact: true }).click()
    await expect(page.getByRole('dialog', { name: '导入账号 · 先预览再提交' })).toBeVisible()
    await page.getByRole('dialog', { name: '导入账号 · 先预览再提交' }).getByRole('button', { name: '关闭', exact: true }).click()
    await capture('accounts-empty-mobile')
    assertions.push('无账号时展示导入引导，空态入口可打开完整导入流程')
  } finally { await page.unroute('**/api/accounts?**', emptyAccountHandler) }
  const emptyOverviewHandler = (route) => route.fulfill({ json: { cards: { requests: 0, success: 0, errors: 0, tokens: 0, cache_rate: 0, cache_rate_has_samples: false }, series: [], status: [], account_top: [], recent_errors: [] } })
  await page.route('**/api/overview?**', emptyOverviewHandler)
  try {
    await page.goto(`${base}/#/?range=1h`)
    await expect(page.locator('.chart-empty')).toContainText('这个时段还没有请求')
    await expect(page.locator('.quiet-state')).toContainText('所选范围暂无错误记录')
    await expect(page.locator('.metric-cache_rate .kpi-value')).toHaveText('暂无样本')
    await capture('dashboard-empty-mobile')
    await page.getByRole('button', { name: '查看接入密钥', exact: true }).click()
    await expect(page).toHaveURL(/#\/keys/)
    assertions.push('零请求时概览显示真实空态与无样本缓存率，接入引导可直达密钥管理')
  } finally { await page.unroute('**/api/overview?**', emptyOverviewHandler) }
  return { assertions, screenshots }
}
