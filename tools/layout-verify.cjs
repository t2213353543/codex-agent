// 企业框架验证：分组菜单/面包屑/页签/折叠/页头/KPI卡/工具条
const { chromium } = require('../frontend/node_modules/playwright/test')
const fs = require('fs')

const BASE = 'http://127.0.0.1:18879'
const checks = []
function check(name, ok) {
  checks.push(`${ok ? 'PASS' : 'FAIL'} ${name}`)
}

async function main() {
  const key = fs.readFileSync(process.argv[2], 'utf8').trim()
  const browser = await chromium.launch({ channel: 'chrome' })
  const page = await browser.newPage({ viewport: { width: 1680, height: 1000 } })
  await page.goto(BASE + '/')
  await page.getByPlaceholder('请输入 admin key').fill(key)
  await page.getByRole('button', { name: '登 录' }).click()
  await page.waitForSelector('.kpi-card', { timeout: 10000 })
  await page.waitForTimeout(1000)

  // 1. 框架元素
  check('侧边栏分组菜单 4 组', await page.locator('.menu-group').count() === 4)
  check('顶栏面包屑', await page.locator('.topbar .el-breadcrumb').count() === 1)
  check('多页签存在', await page.locator('.tags-view').count() === 1)
  check('首页页签固定', await page.locator('.tag-item').first().textContent().then((t) => t.includes('仪表盘')))
  check('用户头像区', await page.locator('.user-chip').count() === 1)
  check('通知铃铛', await page.locator('.notice-badge').count() === 1)

  // 2. 仪表盘
  check('PageHeader 标题', await page.locator('.page-header-title').textContent().then((t) => t === '运行总览'))
  check('时间分段 5 档', await page.locator('.range-seg-item').count() === 5)
  check('概览横条 7 项', await page.locator('.overview-item').count() === 7)
  check('KPI 大卡 4 张', await page.locator('.kpi-card').count() === 4)
  check('KPI sparkline 4 个', await page.locator('.kpi-spark canvas').count() === 4)
  check('主趋势图 canvas', await page.locator('.panel canvas').count() >= 2)
  check('环形图渲染', await page.locator('.panel canvas').count() >= 2)

  // 3. 折叠侧边栏
  const beforeWidth = await page.locator('.sidebar').evaluate((el) => el.offsetWidth)
  await page.locator('.collapse-trigger').click()
  await page.waitForTimeout(400)
  const afterWidth = await page.locator('.sidebar').evaluate((el) => el.offsetWidth)
  check(`侧边栏折叠 ${beforeWidth}px -> ${afterWidth}px`, beforeWidth > 200 && afterWidth <= 70)
  await page.locator('.collapse-trigger').click()
  await page.waitForTimeout(400)

  // 4. 页签开关
  await page.locator('.side-menu .el-menu-item', { hasText: '账号管理' }).click()
  await page.waitForFunction(() => document.querySelector('.page-header-title')?.textContent === '账号管理', null, { timeout: 10000 })
  await page.waitForSelector('.el-table__body-wrapper tbody tr', { timeout: 10000 })
  check('打开账号页签', await page.locator('.tag-item').count() === 2)
  check('账号页 PageHeader', await page.locator('.page-header-title').textContent().then((t) => t === '账号管理'))
  check('账号页工具条', await page.locator('.table-toolbar').count() === 1)
  check('账号页表格行 3', await page.locator('.el-table__body-wrapper tbody tr').count() === 3)
  await page.locator('.tag-item', { hasText: '账号管理' }).locator('.tag-close').click()
  await page.waitForTimeout(300)
  check('关闭页签后回到仪表盘', await page.locator('.tag-item').count() === 1)

  // 5. 其余列表页页头
  for (const [menu, title] of [['分组管理', '分组管理'], ['API KEY 管理', 'API KEY 管理'], ['IP 代理池', 'IP 代理池'], ['使用记录', '使用记录'], ['系统设置', '系统设置']]) {
    await page.locator('.side-menu .el-menu-item', { hasText: menu }).click()
    await page.waitForTimeout(700)
    const h = await page.locator('.page-header-title').textContent().catch(() => '')
    check(`${menu} 页头 = ${h}`, h === title)
  }

  // 6. 搜索过滤
  await page.locator('.side-menu .el-menu-item', { hasText: '账号管理' }).click()
  await page.waitForTimeout(600)
  await page.getByPlaceholder('搜索账号名称').fill('pro-01')
  await page.waitForTimeout(300)
  check('账号搜索过滤', await page.locator('.el-table__body-wrapper tbody tr').count() === 1)

  // 7. 主题切换后组件仍正常渲染
  await page.locator('.theme-switcher .theme-dot').nth(2).click()
  await page.waitForTimeout(600)
  check('moon 主题下页头/工具条/表格仍渲染', await page.locator('.page-header-title').count() === 1 && await page.locator('.table-toolbar').count() === 1 && await page.locator('.el-table__body-wrapper tbody tr').count() === 1)
  await page.locator('.theme-switcher .theme-dot').nth(0).click()
  await page.waitForTimeout(300)

  console.log(checks.join('\n'))
  const failed = checks.filter((c) => c.startsWith('FAIL')).length
  console.log(`\n${checks.length - failed}/${checks.length} passed`)
  await browser.close()
  if (failed) process.exit(1)
}

main().catch((e) => {
  console.error(e)
  process.exit(1)
})
