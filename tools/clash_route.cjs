/* Existing Clash controller only. No config rewrite, process startup or secrets. */
const http = require('node:http');
const net = require('node:net');
const controller = '\\\\.\\pipe\\verge-mihomo';

function api(path, method = 'GET', body) {
  return new Promise((resolve, reject) => {
    const data = body === undefined ? null : Buffer.from(JSON.stringify(body));
    const request = http.request({socketPath: controller, path, method,
      headers: data ? {'Content-Type': 'application/json', 'Content-Length': data.length} : {}}, response => {
      const chunks = [];
      response.on('data', chunk => chunks.push(chunk));
      response.on('end', () => {
        if (response.statusCode < 200 || response.statusCode >= 300) return reject(new Error('controller_http_' + response.statusCode));
        try { resolve(chunks.length ? JSON.parse(Buffer.concat(chunks).toString()) : {}); }
        catch { reject(new Error('controller_invalid_response')); }
      });
    });
    request.on('error', reject);
    request.setTimeout(5000, () => request.destroy(new Error('controller_timeout')));
    request.end(data);
  });
}

async function selector() {
  const proxies = (await api('/proxies')).proxies;
  const entries = Object.entries(proxies).filter(([name, item]) => name.includes('\u9009\u62e9\u8282\u70b9') && item.type === 'Selector');
  if (entries.length !== 1) throw new Error('ambiguous_node_selector');
  return {proxies, group: entries[0][0], selected: entries[0][1]};
}

async function select(node) {
  const {proxies, group, selected} = await selector();
  const regions = {hk: /\u9999\u6e2fS01/, jp: /\u65e5\u672cS01/, sg: /\u65b0\u52a0\u5761S01/};
  if (regions[node]) node = Object.keys(proxies).find(name => regions[node].test(name) && !proxies[name].all);
  if (!proxies[node] || proxies[node].all || !selected.all.includes(node) || !/\u9999\u6e2f|\u65e5\u672c|\u65b0\u52a0\u5761/.test(node)) {
    throw new Error('node_not_in_authorized_regions');
  }
  const state = {group, previous: selected.now, selected: node};
  try {
    await api('/proxies/' + encodeURIComponent(group), 'PUT', {name: node});
  } catch (error) {
    // A lost PUT response can follow a committed selection. Do not overwrite
    // a third-party selection while recovering this ambiguous result.
    try {
      const current = await api('/proxies/' + encodeURIComponent(group));
      if (current.now === node) await restore(state);
    } catch {
      throw new Error('selection_failed_and_restoration_unconfirmed');
    }
    throw error;
  }
  return state;
}

async function restore(state) {
  const current = await api('/proxies/' + encodeURIComponent(state.group));
  if (current.now !== state.selected) throw new Error('selector_changed_by_another_operator');
  await api('/proxies/' + encodeURIComponent(state.group), 'PUT', {name: state.previous});
  return {restored: true};
}

function banner() {
  return new Promise(resolve => {
    const socket = net.connect(7897, '127.0.0.1');
    let data = Buffer.alloc(0), tunnel = false, finished = false;
    const complete = result => { if (finished) return; finished = true; clearTimeout(timer); socket.destroy(); resolve(result); };
    const timer = setTimeout(() => complete({connected: false, reason: 'ssh_banner_timeout'}), 12000);
    socket.on('connect', () => socket.write('CONNECT 43.225.196.170:12865 HTTP/1.1\r\nHost: 43.225.196.170:12865\r\n\r\n'));
    socket.on('data', chunk => {
      data = Buffer.concat([data, chunk]);
      if (data.length > 16384) return complete({connected: false, reason: 'oversized_handshake'});
      if (!tunnel) {
        const end = data.indexOf('\r\n\r\n');
        if (end < 0) return;
        if (!/^HTTP\/1\.[01] 200 /.test(data.subarray(0, end).toString())) return complete({connected: false, reason: 'connect_rejected'});
        tunnel = true; data = data.subarray(end + 4);
      }
      if (data.includes(10)) complete({connected: data.toString().startsWith('SSH-2.0-'), reason: 'banner_received'});
    });
    socket.on('error', () => complete({connected: false, reason: 'proxy_socket_error'}));
    socket.on('end', () => complete({connected: false, reason: 'connection_closed'}));
  });
}

async function main() {
  const [action, value] = process.argv.slice(2);
  if (action === 'select') return select(value);
  if (action === 'restore') return restore(JSON.parse(value));
  if (action !== 'probe') throw new Error('expected_probe_select_or_restore');
  const {proxies} = await selector();
  const patterns = [/\u9999\u6e2fS01/, /\u65e5\u672cS01/, /\u65b0\u52a0\u5761S01/];
  const results = [];
  for (const pattern of patterns) {
    const node = Object.keys(proxies).find(name => pattern.test(name) && !proxies[name].all);
    if (!node) continue;
    const state = await select(node);
    const started = Date.now();
    let result;
    try { result = await banner(); }
    finally { await restore(state); }
    results.push({node, ...result, milliseconds: Date.now() - started, original_selection_restored: true});
    if (result.connected) break;
  }
  return {target: '43.225.196.170:12865', results};
}

main().then(value => console.log(JSON.stringify(value))).catch(error => {
  console.error(error.code || error.message); process.exitCode = 1;
});
