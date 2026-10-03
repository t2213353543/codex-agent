/* Real immutable candidate + retained runtime frontend, no API mocks. */
const assert = require('node:assert/strict');
const crypto = require('node:crypto');
const fs = require('node:fs/promises');
const path = require('node:path');
const { chromium, request, expect: baseExpect } = require('../../frontend/node_modules/playwright/test');
const expect = baseExpect.configure({ timeout: 20000 });
const sha = value => crypto.createHash('sha256').update(value).digest('hex');

(async () => {
  const configPath = process.argv[2];
  assert(configPath, 'Pass private run-config JSON path');
  const config = JSON.parse(await fs.readFile(configPath, 'utf8'));
  const base = new URL(config.base_url);
  assert(base.hostname === '127.0.0.1' && base.port === '28887', 'Only isolated candidate loopback 28887 accepted');
  const key = (await fs.readFile(config.key_file, 'utf8')).trim();
  assert(key.length > 0);
  const output = path.resolve(config.output);
  await fs.mkdir(output, { recursive: true });
  const manifest = JSON.parse(await fs.readFile(config.frontend_evidence, 'utf8'));
  const frontendFiles = manifest.frontend;
  const report = { schema: 'codex-agent-legacy-frontend-real-api-v1', status: 'running',
    source_sha256: config.source_sha256, candidate_binary_sha256: config.candidate_binary_sha256,
    frontend_files: frontendFiles, frontend_manifest_sha256: sha(Buffer.from(JSON.stringify(Object.fromEntries(Object.entries(frontendFiles).sort())))),
    frontend_evidence_sha256: sha(await fs.readFile(config.frontend_evidence)),
    synthetic_only: true, api_mocked: false, production_requests: 0, gateway_inference_requests: 0,
    checks: [], resource_responses: [], api_responses: [], screenshots: [], errors: [],
    scope: 'Exact retained frontend bytes and frozen candidate, actual isolated PostgreSQL and Redis; no production deployment' };
  const pass = (id, detail) => report.checks.push({ id, status: 'pass', detail });
  const nonce = crypto.randomBytes(6).toString('hex');
  const accountName = 'synthetic-browser-' + nonce;
  const access = 'synthetic-browser-at-' + nonce;
  const refresh = 'synthetic-browser-rt-' + nonce;
  const privateValues = [key, access, refresh, ...(config.synthetic_secrets || [])];
  const redacted = text => privateValues.reduce((value, secret) => secret ? value.split(secret).join('[REDACTED]') : value, String(text));
  const noSecrets = text => { for (const secret of privateValues) assert(!secret || !String(text).includes(secret), 'Sensitive value appeared outside credential entry'); };
  const noCredentialFields = value => {
    if (!value || typeof value !== 'object') return;
    for (const [name, child] of Object.entries(value)) {
      if (['access_token', 'refresh_token', 'id_token', 'password', 'credentials_cipher', 'token_fingerprint'].includes(name)) {
        assert(child === '' || child === null || child === undefined, `Credential field leaked: ${name}`);
      }
      noCredentialFields(child);
    }
  };
  const api = await request.newContext({ baseURL: base.origin, extraHTTPHeaders: { Authorization: 'Bearer ' + key } });
  let browser, accountID;
  const apiJSON = async (method, target, data, status = 200) => {
    const response = await api.fetch(target, { method, data });
    assert.equal(response.status(), status, `${method} ${target} HTTP ${response.status()} expected ${status}`);
    const text = await response.text(); noSecrets(text);
    const payload = text ? JSON.parse(text) : {}; noCredentialFields(payload); return payload;
  };
  const settled = async page => {
    await expect(page.locator('.el-loading-mask:visible')).toHaveCount(0);
    await page.evaluate(() => document.fonts.ready);
    await page.waitForLoadState('networkidle');
  };
  const screenshot = async (page, name) => {
    await settled(page); noSecrets(await page.locator('body').innerText());
    await expect(page.locator('.el-message:visible')).toHaveCount(0);
    const filename = name + '.png'; await page.screenshot({ path: path.join(output, filename), fullPage: true });
    report.screenshots.push({ path: filename, sha256: sha(await fs.readFile(path.join(output, filename))) });
  };
  const geometry = async (page, name) => {
    const value = await page.evaluate(() => ({ viewport: innerWidth, document: document.documentElement.scrollWidth,
      body: document.body.scrollWidth, title: document.querySelector('.page-header-title')?.textContent || '',
      horizontalScrollContainers: [...document.querySelectorAll('.el-scrollbar__wrap,.el-table__body-wrapper')].filter(el => el.scrollWidth > el.clientWidth + 2).length }));
    assert(value.document <= value.viewport + 1 && value.body <= value.viewport + 1, `${name} document overflow ${JSON.stringify(value)}`);
    report.geometry ??= []; report.geometry.push({ page: name, ...value });
  };
  try {
    for (const [filename, expected] of Object.entries(frontendFiles)) {
      const local = await fs.readFile(path.join(config.frontend_dist, filename));
      assert.equal(sha(local), expected, `Local retained asset changed: ${filename}`);
      const response = await api.get('/' + (filename === 'index.html' ? '' : filename));
      assert.equal(response.status(), 200, `Missing runtime asset ${filename}`);
      assert.equal(sha(await response.body()), expected, `Served asset changed: ${filename}`);
    }
    pass('retained_frontend_bytes', `${Object.keys(frontendFiles).length} retained files served byte-for-byte, including root document and old Logs importmap`);
    const unauthenticated = await request.newContext({ baseURL: base.origin });
    assert.equal((await unauthenticated.get('/api/accounts')).status(), 401);
    assert.equal((await unauthenticated.get('/api/session')).status(), 401);
    await unauthenticated.dispose();
    pass('api_unauthorized', 'Actual backend denies uncredentialed accounts/session access');
    const before = await apiJSON('GET', '/api/accounts?limit=500');
    assert(before.accounts.every(account => account.enabled === false), 'Fixture must contain only disabled synthetic accounts');
    const logsBefore = await apiJSON('GET', '/api/logs?size=20');
    const overviewBefore = await apiJSON('GET', '/api/overview');
    assert(logsBefore.items.length >= 1, 'Synthetic durable log fixture missing');
    assert(Number(overviewBefore.cards?.tokens_total) >= 17, 'Synthetic durable token statistics missing');
    report.initial_fixture = { accounts: before.accounts.length, logs_sample: logsBefore.items.length, tokens_total: overviewBefore.cards.tokens_total };
    const imported = await apiJSON('POST', '/api/accounts', { raw: JSON.stringify({ name: accountName, enabled: false,
      access_token: access, refresh_token: refresh, account_id: 'synthetic-browser-identity-' + nonce,
      expires_at: 2000000000, notes: 'synthetic browser seed' }) });
    assert.equal(imported.added, 1);
    const accounts = await apiJSON('GET', '/api/accounts?search=' + accountName);
    assert.equal(accounts.accounts.length, 1); accountID = accounts.accounts[0].id;
    assert.equal(accounts.accounts[0].enabled, false);
    assert.equal(accounts.accounts[0].has_refresh_token, true);
    pass('synthetic_account_create', 'Actual PG import creates one disabled synthetic account; responses contain no access/refresh credentials');

    browser = await chromium.launch({ headless: true, channel: 'chrome' });
    const context = await browser.newContext({ viewport: { width: 1440, height: 960 }, reducedMotion: 'reduce' });
    await context.route('**/*', route => {
      const url = new URL(route.request().url());
      if (url.origin !== base.origin) { report.errors.push('Blocked unexpected external browser request'); return route.abort(); }
      if (url.pathname.startsWith('/v1/') || ['/responses', '/responses/compact', '/models'].includes(url.pathname)) {
        report.gateway_inference_requests++; return route.abort();
      }
      return route.continue();
    });
    const observe = page => {
      page.on('pageerror', error => report.errors.push(redacted(error.message)));
      page.on('response', response => {
        const url = new URL(response.url());
        const row = { path: url.pathname, status: response.status() };
        (url.pathname.startsWith('/api/') ? report.api_responses : report.resource_responses).push(row);
      });
    };
    const page = await context.newPage(); observe(page);
    await page.goto(base.origin + '/#/accounts');
    await expect(page.getByRole('textbox', { name: '管理密钥' })).toBeVisible();
    pass('page_unauthorized', 'Retained Accounts route redirects to real login screen');
    await page.getByRole('textbox', { name: '管理密钥' }).fill(key);
    await page.getByRole('button', { name: '登 录', exact: true }).click();
    await expect(page.locator('.page-header')).toContainText('账号管理');
    await settled(page);
    const cookies = await context.cookies();
    assert(cookies.some(cookie => cookie.name === 'codex_admin_session' && cookie.httpOnly && cookie.sameSite === 'Strict'));
    noSecrets(await page.evaluate(() => JSON.stringify(localStorage)));
    await page.reload(); await expect(page.locator('.page-header')).toContainText('账号管理'); await settled(page);
    pass('cookie_session_reload', 'Real Redis-backed HttpOnly/SameSite Strict login survives reload; management key absent from localStorage');
    await screenshot(page, 'accounts-desktop'); await geometry(page, 'accounts-desktop');

    await page.getByRole('textbox', { name: '搜索账号名称' }).fill(accountName);
    await expect(page.locator('.el-table__body-wrapper tbody tr').filter({ hasText: accountName }).first()).toBeVisible();
    const rowFor = target => target.locator('.el-table__body-wrapper tbody tr').filter({ hasText: accountName }).first();
    await rowFor(page).getByRole('button', { name: '编辑', exact: true }).click();
    const editor = page.getByRole('dialog', { name: '编辑账号：' + accountName, exact: true });
    await expect(editor).toBeVisible();
    await editor.getByRole('textbox', { name: '账号备注', exact: true }).fill('browser saved first');
    const second = await context.newPage(); observe(second);
    await second.goto(base.origin + '/#/accounts'); await expect(second.locator('.page-header')).toContainText('账号管理'); await settled(second);
    await second.getByRole('textbox', { name: '搜索账号名称' }).fill(accountName);
    await expect(rowFor(second)).toBeVisible(); await rowFor(second).getByRole('button', { name: '编辑', exact: true }).click();
    const secondEditor = second.getByRole('dialog', { name: '编辑账号：' + accountName, exact: true });
    await expect(secondEditor).toBeVisible();
    await secondEditor.getByRole('textbox', { name: '账号备注', exact: true }).fill('stale browser overwrite');
    await editor.getByRole('button', { name: '保存', exact: true }).click(); await expect(editor).toBeHidden();
    const conflict = second.waitForResponse(response => response.request().method() === 'PUT' && new URL(response.url()).pathname === '/api/accounts/' + accountID);
    await secondEditor.getByRole('button', { name: '保存', exact: true }).click();
    assert.equal((await conflict).status(), 409);
    await expect(secondEditor).toContainText('账号已被其他操作修改');
    const current = await apiJSON('GET', '/api/accounts/' + accountID);
    assert.equal(current.account.notes, 'browser saved first'); assert.equal(current.account.enabled, false);
    await screenshot(second, 'account-version-conflict');
    await secondEditor.getByRole('button', { name: '取消', exact: true }).click(); await second.close();
    pass('two_page_account_cas', 'Two actual retained-page editors read the same version; first edit commits, second receives 409 and reloads current values without overwrite');
    await apiJSON('POST', '/api/accounts/' + accountID + '/reauthorize', { version: current.account.version,
      credentials: { access_token: access + '-renewed', account_id: 'synthetic-browser-identity-' + nonce, expires_at: 2000000000 } });
    privateValues.push(access + '-renewed');
    const reauthorized = await apiJSON('GET', '/api/accounts/' + accountID);
    assert.equal(reauthorized.account.enabled, false); assert.equal(reauthorized.account.notes, 'browser saved first');
    await apiJSON('POST', '/api/accounts/' + accountID + '/reauthorize', { version: current.account.version,
      credentials: { access_token: access + '-stale', account_id: 'synthetic-browser-identity-' + nonce } }, 409);
    pass('reauthorize_cas_compatibility', 'Actual stable-ID reauthorize endpoint updates synthetic credentials while preserving disabled state/metadata; stale version fails');

    for (const [route, title, name] of [['/', '运行总览', 'overview'], ['/logs', '使用记录', 'logs'], ['/groups', '分组管理', 'groups'], ['/proxies', 'IP 代理池', 'proxies'], ['/settings', '管理设置', 'settings']]) {
      await page.goto(base.origin + '/#' + route); await expect(page.locator('.page-header')).toContainText(title); await settled(page);
      await screenshot(page, name + '-desktop'); await geometry(page, name + '-desktop');
    }
    const failedNavigation = report.api_responses.filter(item => item.status >= 400 && !['/api/session', '/api/accounts/' + accountID].includes(item.path));
    assert.equal(failedNavigation.length, 0, JSON.stringify(failedNavigation));
    pass('legacy_routes_real_api', 'Overview, retained Logs importmap module, groups, proxies and old settings page load against candidate API');
    await page.setViewportSize({ width: 390, height: 844 });
    for (const [route, title, name] of [['/accounts', '账号管理', 'accounts'], ['/', '运行总览', 'overview'], ['/logs', '使用记录', 'logs'], ['/settings', '管理设置', 'settings']]) {
      await page.goto(base.origin + '/#' + route); await expect(page.locator('.page-header')).toContainText(title); await settled(page);
      await screenshot(page, name + '-mobile'); await geometry(page, name + '-mobile');
    }
    pass('desktop_mobile_geometry', '1440px and 390px retained page shells have no document/body horizontal overflow; wide tables retain internal scroll');
    await page.setViewportSize({ width: 1440, height: 960 });
    await page.goto(base.origin + '/#/accounts'); await expect(page.locator('.page-header')).toContainText('账号管理'); await settled(page);
    await page.getByRole('textbox', { name: '搜索账号名称' }).fill('SYNTHETIC_NO_MATCH_' + nonce);
    await expect(page.locator('.el-table__empty-block')).toBeVisible();
    await page.getByRole('textbox', { name: '搜索账号名称' }).fill(accountName); await expect(rowFor(page)).toBeVisible();
    await rowFor(page).getByRole('button', { name: '删除', exact: true }).click();
    await page.getByRole('button', { name: '删除', exact: true }).last().click();
    await expect(rowFor(page)).toHaveCount(0);
    await apiJSON('GET', '/api/accounts/' + accountID, undefined, 404); accountID = null;
    pass('account_delete_and_filters', 'Retained UI search/no-match and confirmed account delete succeed against actual database');
    const logsAfter = await apiJSON('GET', '/api/logs?size=20'); const overviewAfter = await apiJSON('GET', '/api/overview');
    assert.equal(logsAfter.items.length, logsBefore.items.length);
    assert.equal(overviewAfter.cards.tokens_total, overviewBefore.cards.tokens_total);
    pass('logs_statistics_preserved', 'Synthetic fixture logs and cumulative token statistics unchanged by account CRUD');
    noSecrets(await page.locator('body').innerText());
    assert.equal(report.errors.length, 0, JSON.stringify(report.errors));
    assert.equal(report.gateway_inference_requests, 0);
    assert(report.resource_responses.every(item => item.status < 400));
    pass('sensitive_fields_and_resources', 'No credential text in rendered pages/API bodies; no JS errors, missing assets or gateway inference requests');
    await page.evaluate(async () => { const response = await fetch('/api/session', { method: 'DELETE' }); if (!response.ok) throw Error('logout failed'); });
    await page.reload(); await expect(page.getByRole('textbox', { name: '管理密钥' })).toBeVisible();
    pass('logout_revokes_session', 'Real Redis session revoked and protected page returns to login');
    report.status = 'pass';
  } catch (error) {
    report.checks.push({ id: 'browser_failure', status: 'fail', detail: redacted(error.message) }); report.status = 'fail';
  } finally {
    if (accountID) { try { await apiJSON('DELETE', '/api/accounts/' + accountID); report.synthetic_account_cleanup = true; } catch { report.synthetic_account_cleanup = false; report.status = 'fail'; } }
    if (browser) await browser.close(); await api.dispose();
    report.completed_at = new Date().toISOString();
    await fs.writeFile(path.join(output, 'report.json'), JSON.stringify(report, null, 2));
    console.log(JSON.stringify({ status: report.status, checks: report.checks, report: path.join(output, 'report.json') }));
  }
  process.exitCode = report.status === 'pass' ? 0 : 1;
})().catch(error => { console.error(error.name + ': browser runner initialization failed'); process.exitCode = 1; });
