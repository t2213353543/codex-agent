/* Run only after the release owner confirms activation. Never save real DOM, API bodies, cookies or screenshots. */
const assert = require('node:assert/strict');
const fs = require('node:fs/promises');
const path = require('node:path');
const crypto = require('node:crypto');
const { chromium, expect: baseExpect } = require('../../frontend/node_modules/playwright/test');
const { READ_PATHS, QUERY_POSTS, requestAllowed } = require('./management-readonly-policy.cjs');
const expect = baseExpect.configure({ timeout: 25000 });
const hash = body => crypto.createHash('sha256').update(body).digest('hex');

(async () => {
  assert.equal(process.argv[2], '--run-after-deploy');
  const config = JSON.parse(await fs.readFile(process.argv[3], 'utf8'));
  const base = new URL(config.base_url);
  assert(['127.0.0.1', 'localhost'].includes(base.hostname));
  assert.equal(base.pathname, '/');
  assert(typeof config.admin_key === 'string' && config.admin_key.length > 0);
  const manifestBody = await fs.readFile(config.frontend_manifest_path), manifest = JSON.parse(manifestBody);
  assert.equal(hash(manifestBody), config.frontend_manifest_sha256);
  const output = config.output;
  await fs.mkdir(output, { recursive: true });
  const report = { schema: 'codex-management-release-readonly-browser-v1', status: 'running', release_id: config.release_id,
    binary_sha256: config.binary_sha256, frontend_manifest_sha256: hash(manifestBody), frontend_sha256: manifest.frontend_sha256,
    access_path: 'loopback browser origin through release-owner SSH forward', public_ingress_verified: false,
    checks: [], api_status_counts: {}, verified_static_resources: 0, static_hash_errors: 0, api_shape_errors: 0,
    blocked_requests: 0, external_requests_blocked: 0, javascript_error_count: 0, resource_error_count: 0,
    authenticated_screenshots_saved: 0, raw_api_data_saved: false, account_names_saved: false, credentials_saved: false,
    allowed_mutations: ['POST/DELETE /api/session'], allowed_query_posts: [...QUERY_POSTS], business_mutations: 0,
    gateway_requests: 0, screenshots_saved: 0, session_cleanup: false };
  let browser, context, page, stage = 'initialization', loggedIn = false;
  const pending = [], seenAssets = new Set();
  const pass = id => report.checks.push({ id, status: 'pass' });
  const settled = async () => { await page.waitForLoadState('networkidle'); await expect(page.locator('.el-loading-mask:visible')).toHaveCount(0); await page.evaluate(() => document.fonts.ready); };
  const layout = async label => {
    const result = await page.evaluate(() => ({ viewport: innerWidth, document: document.documentElement.scrollWidth, body: document.body.scrollWidth }));
    assert(result.document <= result.viewport + 1 && result.body <= result.viewport + 1);
    report.layouts ??= []; report.layouts.push({ view: label, ...result });
  };
  const noFailure = async () => {
    await expect(page.locator('.el-alert--error')).toHaveCount(0);
    await expect(page.locator('.log-load-error,.usage-error,.review-list-error,.ingress-error')).toHaveCount(0);
  };
  try {
    browser = await chromium.launch({ headless: true, channel: 'chrome' });
    context = await browser.newContext({ viewport: { width: 1440, height: 1000 }, reducedMotion: 'reduce', serviceWorkers: 'block' });
    await context.route('**/*', route => {
      const request = route.request(), url = new URL(request.url());
      if (url.origin !== base.origin) report.external_requests_blocked++;
      if (!requestAllowed(request.url(), request.method(), base.origin, manifest.files)) { report.blocked_requests++; return route.abort(); }
      return route.continue();
    });
    page = await context.newPage(); page.setDefaultTimeout(25000);
    page.on('pageerror', () => report.javascript_error_count++);
    page.on('response', response => {
      const url = new URL(response.url());
      if (url.origin !== base.origin) return;
      if (url.pathname.startsWith('/api/')) {
        const name = READ_PATHS.has(url.pathname) || QUERY_POSTS.has(url.pathname) ? url.pathname : '/api/unexpected';
        const key = `${name}:${response.status()}`; report.api_status_counts[key] = (report.api_status_counts[key] || 0) + 1;
        if (response.ok() && response.status() !== 204) pending.push(response.json().then(data => {
          if (name === '/api/accounts/manage') assert(Array.isArray(data.accounts) && Number.isSafeInteger(data.total) && data.total_kind === 'exact');
          if (name === '/api/logs') assert(Array.isArray(data.items) && typeof data.has_more === 'boolean');
          if (name === '/api/logs/snapshot') assert(typeof data.as_of === 'string' && Number.isFinite(data.expires_at));
          if (name === '/api/logs/stats') assert(Number.isSafeInteger(data.total_requests) && Array.isArray(data.series));
          if (name === '/api/ingress-events') assert(data.scope === 'retained_ingress_events' && data.source === 'gateway_ingress_v1' && Array.isArray(data.items));
        }).catch(() => report.api_shape_errors++));
      } else if (response.status() >= 400 && url.pathname !== '/favicon.ico') report.resource_error_count++;
      else if (response.status() === 200) {
        const file = url.pathname === '/' ? 'index.html' : url.pathname.slice(1);
        if (manifest.files[file]) pending.push(response.body().then(body => { if (hash(body) !== manifest.files[file]) report.static_hash_errors++; else seenAssets.add(file); }).catch(() => report.static_hash_errors++));
      }
    });
    stage = 'cookie_login';
    await page.goto(base.origin + '/#/accounts');
    await expect(page.getByRole('textbox', { name: '管理密钥', exact: true })).toBeVisible();
    await page.getByRole('textbox', { name: '管理密钥', exact: true }).fill(config.admin_key);
    await page.getByRole('button', { name: '登 录', exact: true }).click();
    await expect(page.locator('.accounts-page')).toBeVisible(); loggedIn = true; await settled(); await noFailure();
    assert((await context.cookies()).some(cookie => cookie.name === 'codex_admin_session' && cookie.httpOnly && cookie.sameSite === 'Strict'));
    assert.equal(await page.evaluate(() => !!localStorage.getItem('adminKey') || !!sessionStorage.getItem('adminKey')), false); pass(stage);
    stage = 'cookie_reload'; await page.reload(); await expect(page.locator('.accounts-page')).toBeVisible(); await settled(); await noFailure(); pass(stage);
    stage = 'account_readonly_sort';
    await page.getByRole('button', { name: /^筛选与排序/ }).click();
    for (const [field, label] of [['id', '账号 ID'], ['status', '账号状态'], ['last_used_at', '最近业务请求']]) {
      const control = page.getByRole('combobox', { name: '账号排序字段', exact: true }); await control.press('Enter');
      const list = page.locator('#' + await control.getAttribute('aria-controls'));
      const response = page.waitForResponse(r => new URL(r.url()).pathname === '/api/accounts/manage' && new URL(r.url()).searchParams.get('sort_by') === field && [200, 304].includes(r.status()));
      await list.getByRole('option', { name: label, exact: true }).click(); await response; await settled(); await noFailure();
    }
    pass(stage);
    const routes = [['/', '运行总览', 'overview'], ['/accounts', '账号管理', 'accounts'], ['/groups', '分组管理', 'groups'], ['/keys', 'API KEY 管理', 'keys'], ['/proxies', 'IP 代理池', 'proxies'], ['/logs', '使用记录', 'logs'], ['/settings', '管理设置', 'settings']];
    for (const [route, title, name] of routes) {
      stage = 'desktop_' + name; await page.goto(base.origin + '/#' + route); await expect(page.locator('.page-header')).toContainText(title); await settled(); await noFailure(); await layout(stage); pass(stage);
    }
    stage = 'settings_readonly_controls';
    await expect(page.getByRole('switch', { name: '启用 WebSocket', exact: true })).toBeEnabled();
    await expect(page.getByRole('switch', { name: '统一 Codex CLI 请求身份', exact: true })).toBeEnabled(); pass(stage);
    await page.goto(base.origin + '/#/logs'); await settled(); await noFailure();
    stage = 'logs_operational_analysis'; await page.getByRole('button', { name: /趋势与分布/ }).click();
    await expect(page.getByRole('group', { name: '分析指标' }).getByRole('button')).toHaveText(['请求数', 'Token', '输入', '输出', '缓存']);
    await expect(page.locator('.usage-stage-observation')).toContainText('平均网关授权'); await expect(page.locator('.usage-image-observation')).toBeVisible(); pass(stage);
    stage = 'error_review_readonly'; await page.getByRole('button', { name: '错误处理', exact: true }).click();
    const review = page.getByRole('dialog', { name: '错误处理', exact: true }); await expect(review.getByRole('combobox', { name: '错误处理列表级别' })).toBeVisible();
    await settled(); await noFailure(); await review.getByRole('button', { name: '关闭错误处理', exact: true }).click(); pass(stage);
    stage = 'ingress_readonly'; await page.getByRole('button', { name: '准入错误', exact: true }).click();
    const ingress = page.getByRole('dialog', { name: '准入错误', exact: true }); await settled(); await noFailure();
    await expect(ingress.getByRole('combobox', { name: '准入原因', exact: true }).locator('option[value="key_store_unavailable"]')).toHaveCount(1);
    await ingress.getByRole('button', { name: '关闭准入错误', exact: true }).click(); pass(stage);
    for (const width of [390, 320]) for (const route of ['/accounts', '/logs', '/settings']) {
      stage = `mobile_${route.slice(1)}_${width}`; await page.setViewportSize({ width, height: 844 }); await page.goto(base.origin + '/#' + route); await settled(); await noFailure(); await layout(stage); pass(stage);
    }
    await Promise.all(pending); report.verified_static_resources = seenAssets.size;
    for (const name of ['javascript_error_count', 'resource_error_count', 'api_shape_errors', 'static_hash_errors', 'blocked_requests', 'external_requests_blocked']) assert.equal(report[name], 0);
    for (const key of Object.keys(report.api_status_counts)) { const status = Number(key.slice(key.lastIndexOf(':') + 1)); assert(status < 400 || key === '/api/session:401'); }
    assert(seenAssets.has('index.html') && seenAssets.has('assets/' + manifest.entries.Accounts) && seenAssets.has('assets/' + manifest.entries.Logs));
    stage = 'logout'; await page.evaluate(async () => { const response = await fetch('/api/session', { method: 'DELETE' }); if (!response.ok) throw Error('logout failed'); });
    loggedIn = false; report.session_cleanup = true; await page.reload(); await expect(page.getByRole('textbox', { name: '管理密钥', exact: true })).toBeVisible(); pass(stage); report.status = 'pass';
  } catch { report.status = 'fail'; report.checks.push({ id: stage, status: 'fail', detail: 'Raw DOM, API bodies, account identities and error text intentionally not retained.' }); }
  finally {
    if (loggedIn && page) try { report.session_cleanup = await page.evaluate(async () => (await fetch('/api/session', { method: 'DELETE' })).ok); } catch { report.session_cleanup = false; }
    await context?.close(); await browser?.close(); report.completed_at = new Date().toISOString();
    await fs.writeFile(path.join(output, 'browser-report.json'), JSON.stringify(report, null, 2) + '\n');
    console.log(JSON.stringify({ status: report.status, checks: report.checks, output: path.join(output, 'browser-report.json') }));
  }
  process.exitCode = report.status === 'pass' ? 0 : 1;
})().catch(() => { console.error('Read-only browser initialization failed; private configuration and errors not retained.'); process.exitCode = 1; });
