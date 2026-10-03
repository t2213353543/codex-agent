/* Mock controller only. The production script is evaluated without its CLI main. */
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const { EventEmitter } = require('node:events');
const { test } = require('node:test');

function fixture(options = {}) {
  const group = '\u9009\u62e9\u8282\u70b9';
  const names = {hk: '\u9999\u6e2fS01 synthetic', jp: '\u65e5\u672cS01 synthetic', sg: '\u65b0\u52a0\u5761S01 synthetic', original: 'synthetic-original'};
  const selected = {type: 'Selector', now: names.original, all: Object.values(names)};
  const proxies = {[group]: selected, ...Object.fromEntries(Object.values(names).map(name => [name, {type: 'Shadowsocks'}]))};
  if (options.ambiguous) proxies[group + '-second'] = {...selected};
  const calls = [];
  const mockedHTTP = {request(requestOptions, callback) {
    calls.push({method: requestOptions.method, path: requestOptions.path});
    const request = new EventEmitter();
    request.setTimeout = () => request;
    request.destroy = error => request.emit('error', error);
    request.end = body => queueMicrotask(() => {
      const response = new EventEmitter();
      response.statusCode = 200;
      let payload;
      if (requestOptions.path === '/proxies') payload = {proxies};
      else if (requestOptions.method === 'GET') payload = {...selected};
      else {
        const requested = JSON.parse(body).name;
        if (options.rejectPut) response.statusCode = 503;
        else selected.now = requested;
        if (options.failAfterApply && requested === names.hk) {
          if (options.operatorAfterFailedPut) selected.now = names.jp;
          return request.emit('error', new Error('synthetic_lost_put_response'));
        }
        payload = {};
      }
      callback(response);
      response.emit('data', Buffer.from(JSON.stringify(payload)));
      response.emit('end');
    });
    return request;
  }};
  const source = fs.readFileSync(path.join(__dirname, 'clash_route.cjs'), 'utf8');
  const boundary = source.lastIndexOf('\nmain().then(');
  assert(boundary > 0, 'CLI wrapper boundary changed; update the mock loader');
  const sandbox = {Buffer, URL, setTimeout, clearTimeout, console,
    require: name => {if (name === 'node:http') return mockedHTTP; if (name === 'node:net') return {connect() {throw Error('Real network forbidden in mock test');}}; throw Error('Unexpected module: ' + name);}};
  vm.runInNewContext(source.slice(0, boundary) + '\nglobalThis.routeTests = {select,restore,selector};', sandbox, {filename: 'clash_route.mock.cjs'});
  return {api: sandbox.routeTests, selected, names, calls, group};
}

test('allowed regional choices preserve prior selector state', async () => {
  for (const region of ['hk', 'jp', 'sg']) {
    const f = fixture(); const state = await f.api.select(region);
    assert.equal(f.selected.now, f.names[region]);
    assert.equal(state.previous, f.names.original);
    await f.api.restore(state); assert.equal(f.selected.now, f.names.original);
  }
});

test('invalid region and ambiguous selector never issue PUT', async () => {
  for (const options of [{}, {ambiguous: true}]) {
    const f = fixture(options);
    await assert.rejects(f.api.select('us'));
    assert(!f.calls.some(call => call.method === 'PUT'));
  }
});

test('operator selection after SSH is not overwritten by restore', async () => {
  const f = fixture(); const state = await f.api.select('hk');
  f.selected.now = f.names.jp;
  const puts = f.calls.filter(call => call.method === 'PUT').length;
  await assert.rejects(f.api.restore(state), /selector_changed_by_another_operator/);
  assert.equal(f.selected.now, f.names.jp);
  assert.equal(f.calls.filter(call => call.method === 'PUT').length, puts);
});

test('controller rejection before selection leaves previous value', async () => {
  const f = fixture({rejectPut: true});
  await assert.rejects(f.api.select('hk'), /controller_http_503/);
  assert.equal(f.selected.now, f.names.original);
});

test('lost selection PUT response restores original selector before rejecting', async () => {
  const f = fixture({failAfterApply: true});
  await assert.rejects(f.api.select('hk'), /synthetic_lost_put_response/);
  assert.equal(f.selected.now, f.names.original, 'A successful selector change followed by a lost response must not strand the global route');
});

test('lost selection response recovery must not overwrite a newer operator selection', async () => {
  const f = fixture({failAfterApply: true, operatorAfterFailedPut: true});
  await assert.rejects(f.api.select('hk'));
  assert.equal(f.selected.now, f.names.jp);
  assert.equal(f.calls.filter(call => call.method === 'PUT').length, 1);
});
