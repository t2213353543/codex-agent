const { chromium, expect } = require('../frontend/node_modules/playwright/test');
const fs = require('node:fs');
const path = require('node:path');
const root = path.resolve(__dirname, '..');
const base = process.env.TEST_URL || 'http://127.0.0.1:18879';
const keyPath = process.env.TEST_KEY_FILE || path.join(root, 'data/client-key');
const key = fs.readFileSync(keyPath, 'utf8').trim();
const output = path.join(root, '.local/screenshots');
fs.mkdirSync(output, { recursive: true });

(async () => {
  const browser = await chromium.launch({ headless: true, channel: process.env.TEST_BROWSER || 'chrome' });
  const page = await browser.newPage({ viewport: { width: 1440, height: 1000 } });
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  try {
    await page.goto(base);
    await page.locator('#access-key').fill('invalid-key');
    await page.getByRole('button', { name: '\u767b\u5f55', exact: true }).click();
    await expect(page.locator('.login-view .error-text')).not.toBeEmpty();
    await page.locator('#access-key').fill(key);
    await page.getByRole('button', { name: '\u767b\u5f55', exact: true }).click();
    await expect(page.locator('.main-content')).toBeVisible();
    await expect(page.locator('.login-view')).toHaveCount(0);

    // UI lifecycle tests use dummy credentials without selecting them on production.
    const name = 'browser-test-' + Date.now();
    await page.getByRole('button', { name: '\u5bfc\u5165\u8d26\u53f7', exact: true }).click();
    await page.locator('#account-name').fill(name);
    await page.locator('#account-json').fill('{invalid');
    await page.getByRole('button', { name: '\u5bfc\u5165', exact: true }).click();
    await expect(page.locator('.el-dialog .error-text')).toBeVisible();
    await page.locator('#account-json').fill(JSON.stringify({access_token:'browser-dummy',expires_at:1}));
    await page.getByRole('button', { name: '\u5bfc\u5165', exact: true }).click();
    await expect(page.locator('.el-dialog')).toBeHidden();
    const row = page.locator('.desktop-accounts .el-table__row').filter({hasText:name});
    await expect(row).toBeVisible();
    await expect(row.getByRole('button', {name:'\u5237\u65b0\u4ee4\u724c '+name,exact:true})).toBeDisabled();
    await row.getByRole('button', {name:'\u5220\u9664\u8d26\u53f7 '+name,exact:true}).click();
    await page.getByRole('button', {name:'\u53d6\u6d88',exact:true}).click();
    await expect(row).toBeVisible();
    await row.getByRole('button', {name:'\u5220\u9664\u8d26\u53f7 '+name,exact:true}).click();
    await page.getByRole('button', {name:'\u5220\u9664',exact:true}).click();
    await expect(row).toHaveCount(0);
    await expect(page.locator('.el-message-box')).toBeHidden();
    await expect(page.locator('.el-message').first()).toBeHidden({timeout:10000});

    // Exercise request rendering and stop without spending an additional live request.
    await page.route('**/v1/responses', route => route.fulfill({json:{status:'completed',output:[{type:'message',content:[{type:'output_text',text:'BROWSER_TEST_OK'}]}]}}));
    const active = await page.request.get(base+'/api/accounts',{headers:{Authorization:'Bearer '+key}}).then(r=>r.json());
    if (active.active) {
      await page.locator('#request-input').fill('Reply BROWSER_TEST_OK.');
      await page.getByRole('button',{name:'\u53d1\u9001',exact:true}).click();
      await expect(page.locator('.response-output')).toContainText('BROWSER_TEST_OK');
      await page.unroute('**/v1/responses');
      await page.route('**/v1/responses', async route => {
        await new Promise(resolve => setTimeout(resolve, 1500));
        await route.fulfill({json:{status:'completed',output:[]}}).catch(() => {});
      });
      await page.getByRole('button',{name:'\u53d1\u9001',exact:true}).click();
      await page.getByRole('button',{name:'\u505c\u6b62',exact:true}).click();
      await expect(page.locator('.request-status')).toHaveText('\u5df2\u505c\u6b62');
    }
    await page.screenshot({path:path.join(output,'desktop.png'),fullPage:true});
    for (const width of [390, 320, 768]) {
      await page.setViewportSize({width,height:900});
      await page.waitForTimeout(150);
      const overflow = await page.evaluate(() => document.documentElement.scrollWidth > innerWidth);
      if (overflow) throw new Error(`Horizontal overflow at ${width}px`);
      await page.screenshot({path:path.join(output,`viewport-${width}.png`),fullPage:true});
    }
    await page.getByRole('button',{name:'\u5bfc\u5165\u8d26\u53f7',exact:true}).click();
    await page.setViewportSize({width:390,height:844});
    await expect(page.locator('.el-dialog')).toBeVisible();
    await page.waitForTimeout(350);
    await page.screenshot({path:path.join(output,'mobile-import.png'),fullPage:true});
    const box = await page.locator('.el-dialog').boundingBox();
    if (!box || box.x < 0 || box.x+box.width > 390) throw new Error('Import dialog overflows mobile');
    await page.getByRole('button',{name:'\u53d6\u6d88',exact:true}).click();
    await page.getByRole('button',{name:'\u9000\u51fa',exact:true}).click();
    await expect(page.locator('.login-view')).toBeVisible();
    if (await page.evaluate(() => sessionStorage.getItem('codex-agent-client-key'))) throw new Error('Logout did not clear key');
    if (errors.length) throw new Error(errors.join('\n'));
    console.log('PASS: login, account import/delete, validation, request rendering/stop, logout, desktop/mobile layouts');
  } finally {
    await browser.close();
  }
})().catch(error => { console.error(error); process.exitCode = 1; });
