const READ_PATHS = new Set(['/api/session', '/api/accounts', '/api/accounts/manage', '/api/groups', '/api/proxies', '/api/keys', '/api/overview', '/api/settings', '/api/logs', '/api/logs/stats', '/api/logs/errors', '/api/logs/coverage', '/api/ingress-events']);
const QUERY_POSTS = new Set(['/api/logs/snapshot', '/api/accounts/last-used', '/api/accounts/usage-batch', '/api/accounts/upstream/batch']);
function requestAllowed(raw, method, origin, assets = {}) {
  let url;
  try { url = new URL(raw); } catch { return false; }
  if (url.origin !== origin) return false;
  if (url.pathname.startsWith('/api/')) return method === 'GET' && READ_PATHS.has(url.pathname) || ['POST', 'DELETE'].includes(method) && url.pathname === '/api/session' || method === 'POST' && QUERY_POSTS.has(url.pathname);
  return ['GET', 'HEAD'].includes(method) && (url.pathname === '/' || Object.hasOwn(assets, url.pathname.slice(1)) || url.pathname === '/favicon.ico');
}
module.exports = { READ_PATHS, QUERY_POSTS, requestAllowed };
