/* Production read-only page checks. No authenticated screenshots or raw API data. */
const assert = require('node:assert/strict');
const fs = require('node:fs/promises');
const path = require('node:path');
const crypto = require('node:crypto');
const { chromium, expect: baseExpect } = require('../../frontend/node_modules/playwright/test');
const expect = baseExpect.configure({ timeout: 25000 });

(async () => {
  const config = JSON.parse(await fs.readFile(process.argv[2], 'utf8'));
  const base = new URL(config.base_url);
  assert(['127.0.0.1', 'localhost'].includes(base.hostname));
  const output = config.output;
  const report = { schema: 'codex-agent-production-browser-readonly-v1', status: 'running',
    binary_sha256: config.binary_sha256, release_id: config.release_id,
    access_path: 'loopback browser origin via SSH direct-tcpip to production 127.0.0.1:18879',
    public_ingress_verified: false, checks: [], api_status_counts: {}, resource_error_count: 0,
    blocked_business_requests: 0, external_requests_blocked: 0, javascript_error_count: 0,
    authenticated_screenshots_saved: 0, session_writes_only: true, gateway_inference_requests: 0,
    raw_api_data_saved: false, account_names_saved: false, credentials_saved: false };
  let browser, context, page, stage = 'initialization', signedIn = false;
  const pass = id => report.checks.push({ id, status: 'pass' });
  const paths = new Set(['/api/session','/api/accounts','/api/groups','/api/proxies','/api/keys','/api/logs','/api/overview','/api/settings']);
  const leaks = [];
  const pending = [];
  const inspectValue = value => {
    if (!value || typeof value !== 'object') return;
    for (const [name, child] of Object.entries(value)) {
      if (['access_token','refresh_token','id_token','password','credentials_cipher'].includes(name) && child) leaks.push('sensitive_field');
      if (typeof child === 'string' && child.includes(config.admin_key)) leaks.push('admin_key');
      inspectValue(child);
    }
  };
  const settled = async () => {
    await expect(page.locator('.el-loading-mask:visible')).toHaveCount(0);
    await page.waitForLoadState('networkidle');
    await page.evaluate(() => document.fonts.ready);
  };
  const layout = async label => {
    const metrics = await page.evaluate(() => ({ viewport: innerWidth, document: document.documentElement.scrollWidth, body: document.body.scrollWidth }));
    assert(metrics.document <= metrics.viewport + 1 && metrics.body <= metrics.viewport + 1);
    report.layouts ??= []; report.layouts.push({ view: label, ...metrics });
  };
  try {
    browser = await chromium.launch({ headless: true, channel: 'chrome' });
    context = await browser.newContext({ viewport: { width: 1440, height: 960 }, reducedMotion: 'reduce' });
    await context.route('**/*', route => {
      const request = route.request(), url = new URL(request.url());
      if (url.origin !== base.origin) { report.external_requests_blocked++; return route.abort(); }
      if (url.pathname.startsWith('/v1/') || ['/responses','/models','/guardian'].includes(url.pathname)) {
        report.gateway_inference_requests++; return route.abort();
      }
      if (url.pathname.startsWith('/api/')) {
        const session = url.pathname === '/api/session' && ['GET','POST','DELETE'].includes(request.method());
        const readonly = request.method() === 'GET' && paths.has(url.pathname);
        if (!session && !readonly) { report.blocked_business_requests++; return route.abort(); }
      } else if (!['GET','HEAD'].includes(request.method())) { report.blocked_business_requests++; return route.abort(); }
      return route.continue();
    });
    page = await context.newPage();
    page.on('pageerror', () => report.javascript_error_count++);
    page.on('response', response => {
      const url = new URL(response.url());
      if (url.pathname.startsWith('/api/')) {
        const name = paths.has(url.pathname) ? url.pathname : '/api/other';
        const statusKey = name + ':' + response.status();
        report.api_status_counts[statusKey] = (report.api_status_counts[statusKey] || 0) + 1;
        if (response.ok()) pending.push(response.json().then(value => {
          inspectValue(value);
          if (name === '/api/overview') {
            assert(value.cards && Array.isArray(value.series));
            assert(Number.isFinite(value.cards.cache_rate) && value.cards.cache_rate >= 0 && value.cards.cache_rate <= 1);
            assert(typeof value.timezone === 'string');
          }
          if (name === '/api/accounts') assert(Array.isArray(value.accounts));
          if (name === '/api/logs') assert(Array.isArray(value.items));
        }).catch(() => leaks.push('api_shape_or_parse_failure')));
      } else if (response.status() >= 400) report.resource_error_count++;
    });
    stage = 'unauthorized_page';
    await page.goto(base.origin + '/#/accounts');
    await expect(page.getByRole('textbox', { name: '管理密钥' })).toBeVisible();
    await page.screenshot({ path: path.join(output, 'public-login.png'), fullPage: true });
    report.public_login_screenshot_sha256 = crypto.createHash('sha256').update(await fs.readFile(path.join(output,'public-login.png'))).digest('hex');
    pass(stage);
    stage = 'login_session';
    await page.getByRole('textbox', { name: '管理密钥' }).fill(config.admin_key);
    await page.getByRole('button', { name: '登 录', exact: true }).click();
    await expect(page.locator('.page-header')).toContainText('账号管理');
    signedIn = true; await settled();
    const cookies = await context.cookies();
    assert(cookies.some(cookie => cookie.name === 'codex_admin_session' && cookie.httpOnly && cookie.sameSite === 'Strict'));
    assert(!(await page.evaluate(() => JSON.stringify(localStorage))).includes(config.admin_key));
    pass(stage);
    stage = 'reload_session';
    await page.reload(); await expect(page.locator('.page-header')).toContainText('账号管理'); await settled(); pass(stage);
    for (const [route,title,name] of [['/accounts','账号管理','accounts'],['/','运行总览','overview'],['/logs','使用记录','logs'],['/settings','管理设置','settings']]) {
      stage = 'desktop_' + name;
      await page.goto(base.origin + '/#' + route); await expect(page.locator('.page-header')).toContainText(title); await settled(); await layout(stage); pass(stage);
    }
    for (const theme of ['light','dark','ocean','mint']) {
      stage = 'theme_' + theme;
      await page.evaluate(value => localStorage.setItem('theme', JSON.stringify({ version: 1, value })), theme);
      await page.reload(); await expect(page.locator('.page-header')).toContainText('管理设置'); await settled();
      assert.equal(await page.evaluate(() => document.documentElement.dataset.theme), theme);
      await layout(stage); pass(stage);
    }
    await page.evaluate(() => localStorage.setItem('theme', JSON.stringify({version:1,value:'light'})));
    await page.setViewportSize({ width:390, height:844 });
    for (const [route,title,name] of [['/accounts','账号管理','accounts'],['/','运行总览','overview'],['/logs','使用记录','logs'],['/settings','管理设置','settings']]) {
      stage = 'mobile_' + name;
      await page.goto(base.origin + '/#' + route); await expect(page.locator('.page-header')).toContainText(title); await settled(); await layout(stage); pass(stage);
    }
    await Promise.all(pending);
    stage = 'read_only_api_integrity';
    assert.equal(leaks.length, 0); assert.equal(report.javascript_error_count, 0); assert.equal(report.resource_error_count, 0);
    assert.equal(report.blocked_business_requests, 0); assert.equal(report.external_requests_blocked, 0); assert.equal(report.gateway_inference_requests, 0);
    for (const [name,count] of Object.entries(report.api_status_counts)) {
      const status = Number(name.slice(name.lastIndexOf(':')+1));
      assert(status < 400 || name === '/api/session:401', 'Unexpected protected-page HTTP error');
      assert(count > 0);
    }
    pass(stage);
    stage = 'logout';
    await page.evaluate(async () => { const response = await fetch('/api/session', {method:'DELETE'}); if (!response.ok) throw Error('session_delete_failed'); });
    signedIn = false; await page.reload(); await expect(page.getByRole('textbox', {name:'管理密钥'})).toBeVisible(); pass(stage);
    report.status = 'pass';
  } catch {
    report.status = 'fail'; report.checks.push({id:stage,status:'fail',detail:'Read-only browser check failed; raw DOM, response and error text intentionally not retained'});
  } finally {
    if (signedIn && page) {
      try { await page.evaluate(() => fetch('/api/session',{method:'DELETE'})); report.failure_session_cleanup = true; } catch { report.failure_session_cleanup = false; }
    }
    if (browser) await browser.close();
    report.completed_at = new Date().toISOString();
    await fs.writeFile(path.join(output,'browser-report.json'), JSON.stringify(report,null,2));
    console.log(JSON.stringify({status:report.status,checks:report.checks}));
  }
  process.exitCode = report.status === 'pass' ? 0 : 1;
})().catch(() => { console.error('Production readonly browser initialization failed'); process.exitCode=1; });
