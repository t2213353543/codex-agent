// Offline contract audit against the already downloaded, runtime-loaded public
// BPS bundle. No credentials or HTTP/model requests are used.
import fs from 'node:fs';
import path from 'node:path';
import crypto from 'node:crypto';
import vm from 'node:vm';
import assert from 'node:assert/strict';
import { fileURLToPath } from 'node:url';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const assets = path.join(root, '.local/office-proxy-verification/powerpoint');
const clientFile = path.join(assets, 'client-info-BZSGgJF-.js');
const mainFile = path.join(assets, 'x-square-DUrhLSGN.js');
const client = fs.readFileSync(clientFile, 'utf8');
const main = fs.readFileSync(mainFile, 'utf8');
const gateway = fs.readFileSync(path.join(root, 'internal/server/gateway_bps_config.go'), 'utf8');
const request = fs.readFileSync(path.join(root, 'internal/bps/request.go'), 'utf8');
const sha = value => crypto.createHash('sha256').update(value).digest('hex');
const known = JSON.parse(fs.readFileSync(path.join(root, '.local/latest-operations/bps-current-public-bundle.json'), 'utf8'));
for (const [file, text] of [[clientFile, client], [mainFile, main]]) {
  const prior = known.artifacts.find(x => x.file === path.basename(file));
  assert.equal(sha(text), prior.sha256, 'bundle differs from the recorded public artifact');
}

// Execute exact source bytes for J_/eE/IE, the official client-info writer.
// Only the selected editor and host-environment inputs are supplied by the
// fixture; function bodies are neither copied by hand nor rewritten.
const start = client.indexOf('function J_(){');
const end = client.indexOf('export{', start);
assert(start >= 0 && end > start);
const functions = client.slice(start, end);
const platforms = ['PC', 'OfficeOnline', 'Mac', 'iOS', 'Android', 'Universal'];
const cases = [undefined, ...platforms].map(platform => {
  const sandbox = {
    X_: () => 'powerpoint', Li: () => 'powerpoint', q_: () => 'office',
    de: { SHEETS: 'sheets', POWERPOINT: 'powerpoint', WORD: 'word', EXCEL: 'excel' },
    window: {},
  };
  if (platform) sandbox.Office = {
    PlatformType: Object.fromEntries(platforms.map(x => [x, x])),
    context: { host: 'PowerPoint', platform },
  };
  const output = vm.runInNewContext(`${functions}; JSON.stringify(IE())`, sandbox, { timeout: 1000 });
  return { environment: platform ? `explicit Office fixture: ${platform}` : 'browser without Office context', headers: JSON.parse(output) };
});
const field = 'X-Openai-Internal-Basispoints-Client-Platform-Class';
const defaultValue = cases[0].headers[field];
assert.equal(defaultValue, 'OfficeOnline');
assert(cases.every(x => x.headers[field] !== 'web'));
const escaped = field.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
const gatewayValue = gateway.match(new RegExp(`h\\.Set\\("${escaped}", "([^"]+)"\\)`))?.[1];
assert(gatewayValue);
const toolsVersion = request.match(/DefaultToolsVersion = "([^"]+)"/)?.[1];
assert(toolsVersion && client.includes(toolsVersion));
const buildCodeStart = main.indexOf('function ZTe(');
const buildCodeEnd = main.indexOf('const Gee=', buildCodeStart);
assert(buildCodeStart > 0 && buildCodeEnd > buildCodeStart);
const sseStart = main.indexOf('function _Or(');
const sseEnd = main.indexOf('function vOr(', sseStart);
assert(sseStart > 0 && sseEnd > sseStart);
const authStart = main.indexOf('async function JG(');
const authEnd = main.indexOf('const HA=', authStart);
assert(authStart > 0 && authEnd > authStart);
const authFunctions = main.slice(authStart, authEnd);
const prodClientID = main.match(/a6=ay==="staging"\?"[^"]+":"([^"]+)"/)?.[1];
const authScope = main.match(/oMe="([^"]+)"/)?.[1];
const authAudience = main.match(/iMe="([^"]+)"/)?.[1];
assert(prodClientID && authScope && authAudience);
const authRequests = [];
const authSandbox = { URLSearchParams, a6: prodClientID,
  nMe: 'https://auth.openai.com/oauth/token?unified=true',
  ui: async options => { authRequests.push(options); return { access_token: 'SYNTHETIC_ACCESS_TOKEN' }; },
};
await vm.runInNewContext(`${authFunctions}; JG('SYNTHETIC_CODE', 'SYNTHETIC_VERIFIER', 'https://example.invalid/auth/callback')`, authSandbox, { timeout: 1000 });
await vm.runInNewContext(`cJt('SYNTHETIC_REFRESH_TOKEN')`, authSandbox, { timeout: 1000 });
assert.equal(authRequests.length, 2);
assert.deepEqual(authRequests.map(x => x.body.get('grant_type')), ['authorization_code', 'refresh_token']);
assert(authRequests.every(x => x.body.get('client_id') === prodClientID && x.requireAuth === false));
const readJSONL = name => fs.readFileSync(path.join(root, '.local/office-proxy-verification', name), 'utf8').trim().split(/\r?\n/).map(x => JSON.parse(x));
const earlierGeneration = readJSONL('production-generate.jsonl');
const policyPairPath = '.local/latest-operations/bps403-account-pair/20260927T142733Z/verified-pair.json';
const policyPair = JSON.parse(fs.readFileSync(path.join(root, policyPairPath), 'utf8'));
const pairRows = policyPair.correct_transport_evidence.suites.flatMap(x => x.results).filter(x => x.kind === 'request');
const pairAliases = [...new Set(pairRows.map(x => x.account_alias))];
const minimalPath = '.local/latest-operations/bps-minimal-policy/20260927T204611.242194Z/evidence.jsonl';
const minimalRows = fs.readFileSync(path.join(root, minimalPath), 'utf8').trim().split(/\r?\n/).map(x => JSON.parse(x)).filter(x => x.case);
assert.equal(pairRows.length, 4);
assert.equal(minimalRows.length, 3);
const report = {
  captured_at: new Date().toISOString(), mode: 'offline exact-source function execution',
  generation_requests: 0, network_requests: 0,
  assets: [clientFile, mainFile].map(file => ({ file: path.relative(root, file).replaceAll('\\', '/'), sha256: sha(fs.readFileSync(file)) })),
  client_info_functions: { start_offset: start, end_offset: end, sha256: sha(functions) },
  current_tools_version: toolsVersion, version_in_official_bundle: true,
  platform_class: { field, gateway: gatewayValue, official_without_office: defaultValue,
    differs: gatewayValue !== defaultValue, cases },
  sse_defaults_source: { offset: sseStart, source: main.slice(sseStart, sseEnd) },
  build_version: { semantics: 'HTML meta read and minimum-build frontend reload; no build header is set by this code',
    offset: buildCodeStart, source: main.slice(buildCodeStart, buildCodeEnd) },
  authentication: {
    client_id: prodClientID,
    authorize_url: 'https://auth.openai.com/api/accounts/authorize',
    audience: authAudience, scopes: authScope.split(' '), pkce: 'S256',
    grant_functions: { start_offset: authStart, end_offset: authEnd, sha256: sha(authFunctions) },
    offline_observed_grants: authRequests.map(x => ({ url: x.path, method: x.method, content_type: x.headers['Content-Type'],
      form_fields: [...x.body.keys()], client_id: x.body.get('client_id'), grant_type: x.body.get('grant_type'), require_auth: x.requireAuth })),
    device_flow: { request: 'POST /api/accounts/deviceauth/usercode', poll: 'POST /api/accounts/deviceauth/token',
      result: 'authorization_code + code_verifier -> same code exchange', redirect_uri: 'https://auth.openai.com/basispoints/deviceauth/callback' },
    token_path: 'OAuth response access_token -> EYt sessionInfo.access_token -> ke resolver -> BA Authorization Bearer; no additional BPS token exchange in this path',
    account_headers: { source: 'B8(access_token), reading https://api.openai.com/auth or auth claims',
      account_id: ['ChatGPT-Account-ID', 'X-OpenAI-Account-Id'], account_user_id: ['X-OpenAI-Account-User-Id'] },
    account_check: 'GET accounts/check enriches workspace display from matching JWT account ID; it does not replace the access token or select a different account',
    refresh_behavior: 'same original client_id; preserve prior refresh_token only if response omits one; refresh near expiry (300 seconds); 401/403 retries only when expired/near expiry',
    missing_claim_behavior: 'missing account claim logs diagnostics; it does not obtain an alternate token',
    causal_status: 'official OAuth client/scopes differ from imported Codex token; earlier successful Codex-issued token requests mean this difference alone does not prove current denial cause',
    cheapest_remaining_identity_check: 'compare successful-token and failed-token SHA256, iat/exp, client_id/azp, scopes and account claim hashes before any refresh; GET access permission does not prove inference authorization',
    separate_early_history: {
      relevance: 'Older Office validation used a different account-record set; it does not replace the later current-account pair evidence below',
      successful_powerpoint_calls: earlierGeneration.filter(x => x.kind === 'generation' && x.host === 'powerpoint')
        .map(x => ({ alias: x.alias, model: x.request_model, http: x.result?.status, terminal: x.result?.terminal_type })),
      credential_metadata: earlierGeneration.filter(x => x.kind === 'credential_metadata'),
    },
  },
  current_account_timeline: [
    { run: '20260927T142733Z', evidence: policyPairPath,
      source: 'Go gateway test imports selected production AT and AccountID into isolated store; clears refresh token and disables refresh. This is an upstream call, not a synthetic response fixture.',
      results: pairRows.map(x => ({ account: `pair-account-${pairAliases.indexOf(x.account_alias) + 1}`,
        builtin_prompt_enabled: x.builtin_prompt_enabled, http: x.http, completed: x.completed === true,
        usage_policy_message: x.usage_policy_message === true })),
    },
    { run: '20260927T204611.242194Z', evidence: minimalPath,
      source: 'Direct Go HTTP minimal generation probe; both current enabled accounts reject generation; one same-body platform-class control also rejects',
      results: minimalRows.map(x => ({ case: x.case, account: x.account_alias, http: x.http,
        platform_class: x.platform_class, request_body_sha256: x.request_body_sha256, usage_policy_message: x.usage_policy_message })),
      credential_epoch_caveat: 'The probe label credential_epoch falls back to account record version when credential_version is absent from API output. Values 13/17 cannot be treated as database credential generations 4/3.',
    },
  ],
  single_field_comparison: {
    hold_constant: ['account', 'token epoch', 'model', 'body bytes', 'egress route', 'other headers'],
    change_only: { header: field, from: minimalRows.find(x => x.case === 'official_minimal')?.platform_class,
      to: minimalRows.find(x => x.case === 'same_body_platform_class_only')?.platform_class },
    causal_status: 'Tested by the separate 20260927T204611.242194Z minimal probe; same body and credential with web and OfficeOnline both return the identical 403 signature. This one-field correction does not restore generation.',
  },
};
const dest = path.join(root, '.local/latest-operations/bps403-contract-audit.json');
fs.writeFileSync(dest, JSON.stringify(report, null, 2) + '\n');
console.log(JSON.stringify({ result: 'passed', output: dest, network_requests: 0,
  platform_class: report.platform_class.gateway, official_without_office: defaultValue,
  tools_version_matches: true, causal_status: 'unproven' }));
