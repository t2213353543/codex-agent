// 间距扫描：检测页面主要块级元素之间的垂直间距异常（<12px 或重叠）
const { chromium } = require('../frontend/node_modules/playwright/test')
const fs = require('fs')

const BASE = 'http://127.0.0.1:18879'

async function scan(page, label) {
  const issues = await page.evaluate(() => {
    const out = []
    const sel = '.panel, .page-header, .toolbar, .table-toolbar, .kpi-card, .overview-strip, .el-table, .el-pagination, .tags-view, .el-dialog__body'
    const els = [...document.querySelectorAll(sel)].filter((e) => e.offsetParent !== null && e.getBoundingClientRect().height > 0)
    // 按页面 y 排序，检查相邻块级容器间距
    const blocks = els
      .map((e) => ({ el: e, rect: e.getBoundingClientRect(), cls: e.className.toString().slice(0, 50) }))
      .sort((a, b) => a.rect.top - b.rect.top)
    for (let i = 0; i < blocks.length - 1; i++) {
      const a = blocks[i]
      const b = blocks[i + 1]
      // 只比较垂直方向相邻（水平重叠 > 30px 视为同一列）
      const overlapX = Math.min(a.rect.right, b.rect.right) - Math.max(a.rect.left, b.rect.left)
      if (overlapX < 30) continue
      const gap = b.rect.top - a.rect.bottom
      if (gap < 12 && gap > -50) {
        out.push(`${a.cls} → ${b.cls}: gap=${gap.toFixed(1)}px (a.bottom=${a.rect.bottom.toFixed(0)}, b.top=${b.rect.top.toFixed(0)})`)
      }
    }
    // 表格与其上方工具条
    const toolbar = document.querySelector('.table-toolbar')
    const table = document.querySelector('.el-table')
    if (toolbar && table) {
      const tr = toolbar.getBoundingClientRect()
      const tb = table.getBoundingClientRect()
      const gap = tb.top - tr.bottom
      if (gap < 12) out.push(`table-toolbar → el-table: gap=${gap.toFixed(1)}px`)
    }
    return out
  })
  console.log(`--- ${label} ---`)
  if (!issues.length) console.log('  无异常间距')
  for (const line of issues) console.log('  ' + line)
}

async function main() {
  const key = fs.readFileSync(process.argv[2], 'utf8').trim()
  const browser = await chromium.launch({ channel: 'chrome' })
  const page = await browser.newPage({ viewport: { width: 1680, height: 1000 } })
  await page.goto(BASE + '/')
  await page.getByPlaceholder('请输入 admin key').fill(key)
  await page.getByRole('button', { name: '登 录' }).click()
  await page.waitForSelector('.kpi-card', { timeout: 10000 })
  await page.waitForTimeout(1200)

  await scan(page, '仪表盘')
  for (const [name, title] of [['账号管理', '账号管理'], ['分组管理', '分组管理'], ['使用记录', '使用记录'], ['系统设置', '系统设置'], ['API KEY 管理', 'API KEY 管理'], ['IP 代理池', 'IP 代理池']]) {
    await page.locator('.side-menu .el-menu-item', { hasText: name }).click()
    await page.waitForFunction((n) => document.querySelector('.page-header-title')?.textContent === n, title, { timeout: 10000 })
    await page.waitForTimeout(800)
    await scan(page, title)
  }
  await browser.close()
}

main().catch((e) => {
  console.error(e)
  process.exit(1)
})
