/* Isolated rehearsal for the production runner; this file never accesses remote services. */
const assert = require('node:assert/strict');
const { createServer } = require('node:http');
const { readFile, writeFile, mkdir, unlink } = require('node:fs/promises');
const { createHash } = require('node:crypto');
const { spawn } = require('node:child_process');
const path = require('node:path');
(async () => {
  const root = path.resolve(__dirname, '../..'), candidate = path.join(root, '.local/management-parity/release-20260919'), dist = path.join(candidate, 'dist'), output = path.join(candidate, 'production-runner-rehearsal');
  await mkdir(output, { recursive: true });
  const manifestPath = path.join(candidate, 'build-manifest.json'), manifest = await readFile(manifestPath), now = Math.floor(Date.now() / 1000);
  const stats = { total_requests: 0, success: 0, errors: 0, total_tokens: 0, total_input_tokens: 0, total_output_tokens: 0, total_cached_tokens: 0, models: [], series: [], accounts: [], groups: [] };
  const apiRequests = [];
  const server = createServer(async (req, res) => {
    const url = new URL(req.url, 'http://127.0.0.1'), pathname = url.pathname;
    const json = (data, status = 200, headers = {}) => { res.writeHead(status, { 'content-type': 'application/json', ...headers }); res.end(JSON.stringify(data)); };
    if (pathname.startsWith('/api/')) {
      apiRequests.push({ path: pathname, method: req.method });
      if (pathname === '/api/session') {
        if (req.method === 'POST') return json({ authenticated: true }, 200, { 'set-cookie': 'codex_admin_session=synthetic; Path=/api; HttpOnly; SameSite=Strict' });
        if (req.method === 'DELETE') return json({ ok: true }, 200, { 'set-cookie': 'codex_admin_session=; Max-Age=0; Path=/api; HttpOnly; SameSite=Strict' });
        return json({ authenticated: Boolean(req.headers.cookie?.includes('codex_admin_session=synthetic')) }, req.headers.cookie?.includes('codex_admin_session=synthetic') ? 200 : 401);
      }
      if (!req.headers.cookie?.includes('codex_admin_session=synthetic')) return json({}, 401);
      if (pathname === '/api/settings') return json({ settings_version: 1, admin_key: '', admin_api: true, version: 'isolated-runner-rehearsal', uptime_seconds: 1, websocket_enabled: true, codex_fingerprint_enabled: false, codex_fingerprint_profiles: [{ id: 'linux', label: 'Linux' }] });
      if (pathname === '/api/accounts/manage' || pathname === '/api/accounts') return json({ accounts: [], total: 0, total_kind: 'exact', has_more: false });
      if (pathname === '/api/groups') return json({ groups: [] });
      if (pathname === '/api/keys') return json({ keys: [] });
      if (pathname === '/api/proxies') return json({ proxies: [] });
      if (pathname === '/api/logs/snapshot') return json({ as_of: 'usage_rehearsal', expires_at: now + 300, total: 0 });
      if (pathname === '/api/logs/stats') return json(stats);
      if (pathname === '/api/logs' || pathname === '/api/logs/errors') return json({ items: [], total: 0, total_kind: 'exact', has_more: false, next_cursor: '' });
      if (pathname === '/api/ingress-events') return json({ scope: 'retained_ingress_events', source: 'gateway_ingress_v1', items: [], total: 0, total_kind: 'exact', has_more: false, next_cursor: '', checked_at: now, retention_limit: 1000, evicted_total: 0, collector: { configured: true, running: true, counter_scope: 'current_process' } });
      if (pathname === '/api/overview') return json({ cards: { accounts: 0, accounts_enabled: 0, groups: 0, keys: 0, keys_enabled: 0, requests: 0, success: 0, tokens: 0, errors: 0, input: 0, cached: 0, cache_rate: 0, cache_rate_has_samples: false, requests_today: 0, tokens_today: 0, tokens_total: 0, tokens_retained: 0, avg_latency_ms: 0, avg_latency_today: 0, rpm: 0, tpm: 0, peak_rpm: 0, peak_tpm: 0 }, prev_cards: {}, timezone: 'UTC', retention_days: 92, series: [], account_top: [], status: [], recent_errors: [] });
      return json({ error: { message: 'unexpected rehearsal route' } }, 404);
    }
    try {
      const file = path.resolve(dist, pathname === '/' ? 'index.html' : pathname.slice(1)); assert(file.startsWith(dist + path.sep));
      res.writeHead(200, { 'content-type': file.endsWith('.js') ? 'text/javascript' : file.endsWith('.css') ? 'text/css' : 'text/html' }); res.end(await readFile(file));
    } catch { res.writeHead(404); res.end(); }
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  const configPath = path.join(output, 'synthetic-config.json');
  await writeFile(configPath, JSON.stringify({ base_url: `http://127.0.0.1:${server.address().port}`, admin_key: 'synthetic-not-a-real-key', output, frontend_manifest_path: manifestPath, frontend_manifest_sha256: createHash('sha256').update(manifest).digest('hex'), release_id: 'synthetic-rehearsal', binary_sha256: 'not-a-live-binary' }));
  let code;
  try {
    const child = spawn(process.execPath, [path.join(__dirname, 'management-release-readonly.cjs'), '--run-after-deploy', configPath], { stdio: 'inherit', windowsHide: true });
    code = await new Promise((resolve, reject) => { child.once('exit', resolve); child.once('error', reject); });
  } finally { await unlink(configPath); await new Promise(resolve => server.close(resolve)); }
  await writeFile(path.join(output, 'rehearsal.json'), JSON.stringify({ passed: code === 0, synthetic_only: true, server_closed: true, config_removed: true, api_requests: apiRequests }, null, 2));
  process.exitCode = code || 0;
})().catch(() => { console.error('Isolated read-only runner rehearsal failed'); process.exitCode = 1; });
