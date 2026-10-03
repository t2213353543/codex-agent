// 新管理后台 UI 冒烟：登录、7 个菜单页渲染、账号导入/编辑/删除闭环。
const { chromium } = require('../frontend/node_modules/playwright/test');
const fs = require('node:fs');
const path = require('node:path');

const root = path.resolve(__dirname, '..');
const base = process.env.TEST_URL || 'http://127.0.0.1:18879';
const key = fs.readFileSync(process.env.TEST_KEY_FILE || path.join(root, 'data/client-key'), 'utf8').trim();

(async () => {
  const browser = await chromium.launch({ headless: true, channel: process.env.TEST_BROWSER || 'chrome' });
  const page = await browser.newPage({ viewport: { width: 1500, height: 950 } });
  const errors = [];
  page.on('pageerror', (error) => errors.push(error.message));

  const step = (name) => console.log(`[ok] ${name}`);
  try {
    // 1. 未登录跳转登录页
    await page.goto(base);
    await page.waitForSelector('.login-card', { timeout: 10000 });
    step('未登录重定向到登录页');

    // 2. 错误 key 提示失败
    await page.getByPlaceholder('请输入 admin key').fill('wrong-key');
    await page.getByRole('button', { name: '登 录' }).click();
    await page.waitForSelector('.el-message--error', { timeout: 10000 });
    step('错误 key 被拒绝');

    // 3. 正确 key 进入仪表盘
    await page.getByPlaceholder('请输入 admin key').fill(key);
    await page.getByRole('button', { name: '登 录' }).click();
    await page.waitForSelector('.sidebar', { timeout: 10000 });
    await page.waitForSelector('.kpi-card', { timeout: 10000 });
    step('登录成功，仪表盘 KPI 卡片渲染');

    // 4. 七个菜单页面逐个渲染
    const menus = [
      ['分组管理', '.el-table'],
      ['API KEY 管理', '.el-table'],
      ['IP 代理池', '.el-table'],
      ['账号管理', '.el-table'],
      ['使用记录', '.el-table'],
      ['系统设置', '.settings-panel'],
    ];
    for (const [title, selector] of menus) {
      await page.getByRole('menuitem', { name: title }).click();
      await page.waitForSelector(selector, { timeout: 10000 });
      step(`菜单「${title}」渲染`);
    }

    // 5. 账号管理：导入 → 编辑 → 删除闭环
    await page.getByRole('menuitem', { name: '账号管理' }).click();
    const name = 'ui-smoke-' + Date.now();
    await page.getByRole('button', { name: '导入账号' }).click();
    await page.waitForSelector('.el-dialog', { timeout: 5000 });
    await page.locator('.el-dialog textarea').fill(JSON.stringify({ name, access_token: 'ui-smoke-dummy-token', expires_at: 4102444800 }));
    await page.getByRole('button', { name: '导入', exact: true }).click();
    await page.waitForSelector('.el-message--success', { timeout: 10000 });
    const row = page.locator('.el-table__row').filter({ hasText: name });
    await row.waitFor({ timeout: 5000 });
    step('账号导入成功并出现在列表');

    // 展开行点图（无请求时为占位文本，有请求时为 canvas）
    await row.locator('.el-table__expand-icon').click();
    await page.waitForSelector(`#dot-${name}`, { timeout: 10000 });
    await page.waitForTimeout(800);
    const dotContent = await page.locator(`#dot-${name}`).innerHTML();
    if (!dotContent.includes('canvas') && !dotContent.includes('暂无请求记录')) throw new Error('点图区域未渲染');
    step('最近请求点图渲染');

    // 编辑：改并发
    await row.getByRole('button', { name: '编辑' }).click();
    await page.waitForSelector('.el-dialog', { timeout: 5000 });
    const concurrencyInput = page.locator('.el-dialog .el-input-number input').first();
    await concurrencyInput.fill('5');
    await concurrencyInput.blur();
    await page.getByRole('button', { name: '保存' }).click();
    await page.waitForSelector('.el-message--success', { timeout: 10000 });
    await page.waitForTimeout(800);
    step('账号编辑保存');

    // 删除
    await row.getByRole('button', { name: '删除' }).click();
    await page.waitForSelector('.el-message-box', { timeout: 5000 });
    await page.locator('.el-message-box').getByRole('button', { name: '删除', exact: true }).click();
    await page.waitForSelector('.el-message--success', { timeout: 10000 });
    await page.waitForTimeout(800);
    if ((await row.count()) > 0) throw new Error('账号删除后仍存在');
    step('账号删除闭环');

    // 6. 分组创建与删除
    await page.getByRole('menuitem', { name: '分组管理' }).click();
    const groupName = '冒烟分组' + Date.now();
    await page.getByRole('button', { name: '创建分组' }).click();
    await page.locator('.el-dialog input').fill(groupName);
    await page.getByRole('button', { name: '保存' }).click();
    await page.waitForSelector('.el-message--success', { timeout: 10000 });
    const groupRow = page.locator('.el-table__row').filter({ hasText: groupName });
    await groupRow.waitFor({ timeout: 5000 });
    await groupRow.getByRole('button', { name: '删除' }).click();
    await page.locator('.el-message-box').getByRole('button', { name: '删除', exact: true }).click();
    await page.waitForSelector('.el-message--success', { timeout: 10000 });
    step('分组创建/删除闭环');

    // 7. 代理添加
    await page.getByRole('menuitem', { name: 'IP 代理池' }).click();
    await page.getByRole('button', { name: '添加代理' }).click();
    await page.waitForSelector('.el-dialog', { timeout: 5000 });
    await page.locator('.el-dialog input').first().fill('冒烟代理');
    await page.locator('.el-dialog input[placeholder="代理服务器 IP 或域名"]').fill('127.0.0.1');
    await page.locator('.el-dialog .el-input-number input').fill('1080');
    await page.getByRole('button', { name: '保存' }).click();
    await page.waitForSelector('.el-message--success', { timeout: 10000 });
    const proxyRow = page.locator('.el-table__row').filter({ hasText: '冒烟代理' });
    await proxyRow.waitFor({ timeout: 5000 });
    await proxyRow.getByRole('button', { name: '删除' }).click();
    await page.locator('.el-message-box').getByRole('button', { name: '删除', exact: true }).click();
    await page.waitForSelector('.el-message--success', { timeout: 10000 });
    step('代理添加/删除闭环');

    // 8. API KEY 创建（验证完整 key 弹窗 + 掩码列表）
    await page.getByRole('menuitem', { name: 'API KEY 管理' }).click();
    await page.getByRole('button', { name: '创建 KEY' }).click();
    await page.locator('.el-dialog input').first().fill('冒烟KEY');
    await page.getByRole('button', { name: '保存' }).click();
    await page.waitForSelector('.el-dialog', { timeout: 5000 });
    const fullKey = await page.locator('.el-dialog .mono').textContent();
    if (!fullKey || !fullKey.startsWith('sk-')) throw new Error('创建 key 后未显示完整 key');
    await page.getByRole('button', { name: '我已保存' }).click();
    const keyRow = page.locator('.el-table__row').filter({ hasText: '冒烟KEY' });
    await keyRow.waitFor({ timeout: 5000 });
    const masked = await keyRow.locator('.mono').textContent();
    if (!masked.includes('****')) throw new Error('key 列表未掩码');
    await keyRow.getByRole('button', { name: '删除' }).click();
    await page.locator('.el-message-box').getByRole('button', { name: '删除', exact: true }).click();
    await page.waitForSelector('.el-message--success', { timeout: 10000 });
    step('API KEY 创建/掩码/删除闭环');

    if (errors.length) {
      throw new Error('页面 JS 错误: ' + errors.join(' | '));
    }
    console.log('\n全部 UI 冒烟通过 ✓');
  } catch (err) {
    const shot = path.join(root, '.local/ui-smoke-fail.png');
    fs.mkdirSync(path.dirname(shot), { recursive: true });
    await page.screenshot({ path: shot, fullPage: true }).catch(() => {});
    console.error('UI 冒烟失败:', err.message);
    console.error('截图:', shot);
    process.exitCode = 1;
  } finally {
    await browser.close();
  }
})();
