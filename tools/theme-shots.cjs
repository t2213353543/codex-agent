// 主题截图：登录后逐套切换主题，截仪表盘 + 账号管理 + 分组管理页
const { chromium } = require('../frontend/node_modules/playwright/test')
const fs = require('fs')
const path = require('path')

const BASE = 'http://127.0.0.1:18879'
const OUT = path.join(__dirname, '..', '.local', 'themes')
const THEMES = ['light', 'dark', 'ocean', 'mint']

async function main() {
  const key = fs.readFileSync(process.argv[2] || '/tmp/codex-preview-data/client-key', 'utf8').trim()
  fs.mkdirSync(OUT, { recursive: true })
  const browser = await chromium.launch({ channel: 'chrome' })
  const page = await browser.newPage({ viewport: { width: 1680, height: 1000 } })

  await page.goto(BASE + '/')
  await page.getByPlaceholder('请输入 admin key').fill(key)
  await page.getByRole('button', { name: '登 录' }).click()
  await page.waitForSelector('.kpi-card', { timeout: 10000 })
  await page.waitForTimeout(1500)

  async function switchTheme(i) {
    await page.locator('.theme-switcher .theme-dot').nth(i).click()
    await page.waitForTimeout(800)
    await page.mouse.move(840, 1000)
    await page.waitForTimeout(300)
  }

  async function gotoMenu(name) {
    await page.locator('.side-menu .el-menu-item', { hasText: name }).click()
    await page.waitForFunction(
      (n) => document.querySelector('.page-header-title')?.textContent === n,
      name,
      { timeout: 10000 },
    )
    await page.waitForTimeout(900)
  }

  // 仪表盘
  for (let i = 0; i < THEMES.length; i++) {
    await switchTheme(i)
    await page.screenshot({ path: path.join(OUT, `${THEMES[i]}-dashboard.png`) })
  }

  // 账号管理
  await switchTheme(0)
  await gotoMenu('账号管理')
  await page.waitForSelector('.el-table__body-wrapper tbody tr', { timeout: 10000 })
  for (let i = 0; i < THEMES.length; i++) {
    await switchTheme(i)
    await page.screenshot({ path: path.join(OUT, `${THEMES[i]}-accounts.png`) })
  }

  // 分组管理
  await switchTheme(0)
  await gotoMenu('分组管理')
  for (let i = 0; i < THEMES.length; i++) {
    await switchTheme(i)
    await page.screenshot({ path: path.join(OUT, `${THEMES[i]}-groups.png`) })
  }

  // 系统设置
  await switchTheme(0)
  await gotoMenu('系统设置')
  for (let i = 0; i < THEMES.length; i++) {
    await switchTheme(i)
    await page.screenshot({ path: path.join(OUT, `${THEMES[i]}-settings.png`) })
  }

  await browser.close()
  console.log('OK ->', OUT)
}

main().catch((e) => {
  console.error(e)
  process.exit(1)
})
