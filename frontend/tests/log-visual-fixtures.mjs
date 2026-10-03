import assert from 'node:assert/strict'
import { expect } from 'playwright/test'
export async function captureLogThemes(page, locator, prefix, directory, { anchor, widths = [1440, 390] } = {}) {
  if (!directory) return
  const viewport = page.viewportSize()
  const original = await page.evaluate(() => document.documentElement.dataset.theme || 'light')
  try {
    await expect(page.locator('.el-message:visible')).toHaveCount(0, { timeout: 10000 })
    for (const theme of ['light', 'dark']) for (const width of widths) {
      await page.setViewportSize({ width, height: width < 600 ? 844 : 1100 })
      await page.evaluate(value => { document.documentElement.dataset.theme = value; document.documentElement.classList.toggle('dark', value === 'dark'); window.dispatchEvent(new CustomEvent('theme-change', { detail: value })) }, theme)
      await page.evaluate(() => document.fonts.ready)
      if (anchor) await anchor.scrollIntoViewIfNeeded()
      assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true, `${prefix} ${theme} ${width} overflow`)
      if (locator) await locator.screenshot({ path: `${directory}/${prefix}-${theme}-${width}.png`, animations: 'disabled' })
      else await page.screenshot({ path: `${directory}/${prefix}-${theme}-${width}.png`, fullPage: true, animations: 'disabled' })
    }
  } finally {
    await page.setViewportSize(viewport)
    await page.evaluate(value => { document.documentElement.dataset.theme = value; document.documentElement.classList.toggle('dark', value === 'dark'); window.dispatchEvent(new CustomEvent('theme-change', { detail: value })) }, original)
  }
}
