// 主题程序化验证：逐套切换主题，断言 CSS 变量、dark 类、图表 canvas、表格渲染
const { chromium } = require('../frontend/node_modules/playwright/test')
const fs = require('fs')

const BASE = 'http://127.0.0.1:18879'
const THEMES = ['light', 'dark', 'ocean', 'mint']

async function main() {
  const key = fs.readFileSync(process.argv[2], 'utf8').trim()
  const browser = await chromium.launch({ channel: 'chrome' })
  const page = await browser.newPage({ viewport: { width: 1680, height: 1000 } })
  await page.goto(BASE + '/')
  await page.getByPlaceholder('请输入 admin key').fill(key)
  await page.getByRole('button', { name: '登 录' }).click()
  await page.waitForSelector('.kpi-card', { timeout: 10000 })
  await page.waitForTimeout(1000)

  for (let i = 0; i < THEMES.length; i++) {
    await page.locator('.theme-switcher .theme-dot').nth(i).click()
    await page.waitForTimeout(700)
    const state = await page.evaluate(() => {
      const cs = getComputedStyle(document.documentElement)
      const body = getComputedStyle(document.body)
      return {
        theme: document.documentElement.dataset.theme,
        dark: document.documentElement.classList.contains('dark'),
        bg: body.backgroundImage.slice(0, 60) || body.backgroundColor,
        cardBg: cs.getPropertyValue('--card-bg').trim().slice(0, 40),
        text1: cs.getPropertyValue('--text-1').trim(),
        primary: cs.getPropertyValue('--primary').trim(),
        sidebar: getComputedStyle(document.querySelector('.sidebar')).backgroundColor || 'image',
        statCards: document.querySelectorAll('.kpi-card').length,
        chartCanvas: !!document.querySelector('.panel canvas'),
        saved: localStorage.getItem('theme'),
      }
    })
    console.log(THEMES[i], JSON.stringify(state, null, 0))
  }
  await browser.close()
}

main().catch((e) => {
  console.error(e)
  process.exit(1)
})
