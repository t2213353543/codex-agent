import assert from 'node:assert/strict'
import { cp, mkdir, readFile, writeFile, symlink, access, readdir } from 'node:fs/promises'
import path from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'
import { performance } from 'node:perf_hooks'
import { gzipSync } from 'node:zlib'
import { build, createLogger } from 'vite'

const frontend = fileURLToPath(new URL('../', import.meta.url))
const output = path.resolve(frontend, '../.local/system-perf-20260927/frontend')
const original = path.resolve(frontend, '../.local/system-perf-20260927/baseline-src/frontend')
const baseline = path.join(output, 'baseline-build')
await mkdir(baseline, { recursive: true })
for (const name of ['src', 'index.html', 'package.json', 'vite.config.js']) await cp(path.join(original, name), path.join(baseline, name), { recursive: true })
await cp(path.join(frontend, 'public'), path.join(baseline, 'public'), { recursive: true })
try { await access(path.join(baseline, 'node_modules')) } catch { await symlink(path.join(frontend, 'node_modules'), path.join(baseline, 'node_modules'), 'junction') }
const baselineRepairs = []
for (const name of ['Groups.vue', 'Keys.vue']) {
  const file = path.join(baseline, 'src/views', name)
  const text = await readFile(file, 'utf8')
  if (text.includes('function formatTime(ts)') && text.includes("import { formatTime } from '../utils/format'")) {
    await writeFile(file, text.replace("import { formatTime } from '../utils/format'", "import { formatTime as formatTimestamp } from '../utils/format'").replace('return formatTime(ts)', 'return formatTimestamp(ts)'))
    baselineRepairs.push(`${name}: alias duplicate formatTime import; keep existing zero/missing-time guard`)
  }
}

const load = (root, name) => import(pathToFileURL(path.join(root, 'src/utils', name)).href)
const before = Object.fromEntries(await Promise.all(['resourceManagement.js', 'logManagementState.js', 'accountDailyStats.js', 'logMetrics.js'].map(async name => [name, await load(baseline, name)])))
const after = Object.fromEntries(await Promise.all(Object.keys(before).map(async name => [name, await load(frontend, name)])))
const rows = Array.from({ length: 10000 }, (_, i) => ({ id: `r${i}`, name: `${['上海', '北京', '广州', '开发 App'][i % 4]} ${((i * 7919) % 10000) + 1}`, groups: [`g${(i * 17) % 2000}`, `g${(i * 31) % 2000}`], enabled: i % 3 !== 0, requests: i * 7, tokens: i * 97, errors: i % 11 }))
const groups = Array.from({ length: 2000 }, (_, i) => ({ id: `g${i}`, name: `分组 ${i}` }))
const times = Array.from({ length: 2000 }, (_, i) => 1700000000 + i * 86400)
const summary = { timezone: 'Asia/Shanghai', checked_at: times.at(-1), total_requests: 2000, total_tokens: 200000, usage_unknown_requests: 0, series: times.map(time => ({ time, total_requests: 1, total_tokens: 100, usage_known_requests: 1, usage_unknown_requests: 0 })) }
const cases = [
  ['resource_name_sort_10000', 'resourceManagement.js', module => module.sortResources(rows, 'name:asc').map(row => row.id)],
  ['key_csv_10000_with_2000_groups', 'resourceManagement.js', module => module.createResourceCSV('keys', rows, groups)],
  ['log_time_parts_2000', 'logManagementState.js', module => times.map(time => module.managedTimeParts(time, 'America/New_York'))],
  ['account_daily_summary_2000', 'accountDailyStats.js', module => module.accountDailySummary(summary)],
  ['log_percent_10000', 'logMetrics.js', module => rows.map((_, i) => module.logPercent(i / 10000))],
]
const measurements = []
for (const [name, source, operation] of cases) {
  assert.deepEqual(operation(after[source]), operation(before[source]), `${name}: output parity`)
  const samples = { before: [], after: [] }
  for (let round = 0; round < 9; round++) {
    for (const variant of round % 2 ? ['after', 'before'] : ['before', 'after']) {
      const started = performance.now()
      operation((variant === 'before' ? before : after)[source])
      const elapsed = performance.now() - started
      if (round > 1) samples[variant].push(elapsed)
    }
  }
  const median = values => [...values].sort((a, b) => a - b)[Math.floor(values.length / 2)]
  const beforeMs = median(samples.before), afterMs = median(samples.after)
  measurements.push({ name, output_parity: true, before_ms: beforeMs, after_ms: afterMs, reduction_percent: (1 - afterMs / beforeMs) * 100, speedup: beforeMs / afterMs, samples_ms: samples })
  console.log(`${name}: ${beforeMs.toFixed(3)} ms -> ${afterMs.toFixed(3)} ms (${(beforeMs / afterMs).toFixed(2)}x)`)
}

const bundles = {}
for (const [name, root] of [['before', baseline], ['after', frontend]]) {
  const outDir = path.join(output, `${name}-dist`)
  if (!process.argv.includes('--skip-build')) {
    const messages = [], logger = createLogger()
    for (const method of ['info', 'warn', 'error']) {
      const log = logger[method]
      logger[method] = (message, ...args) => { messages.push(message); log(message, ...args) }
    }
    try { await build({ root, configFile: path.join(root, 'vite.config.js'), customLogger: logger, build: { outDir, emptyOutDir: true, manifest: true } }) }
    finally { await writeFile(path.join(output, `${name}-build-success.txt`), messages.join('\n')) }
  }
  const files = []
  for (const file of (await readdir(path.join(outDir, 'assets'))).sort()) {
    if (!/\.(js|css)$/.test(file)) continue
    const data = await readFile(path.join(outDir, 'assets', file))
    files.push({ name: file, bytes: data.length, gzip_bytes: gzipSync(data).length })
  }
  const manifest = JSON.parse(await readFile(path.join(outDir, '.vite/manifest.json'), 'utf8'))
  const imports = key => {
    const visited = new Set()
    const visit = id => { if (visited.has(id)) return; visited.add(id); for (const child of manifest[id]?.imports || []) visit(child) }
    visit(key)
    return [...visited].map(id => manifest[id]?.file).filter(Boolean)
  }
  const accountEntry = Object.keys(manifest).find(key => manifest[key].name === 'Accounts' && manifest[key].isDynamicEntry)
  assert.ok(accountEntry, `${name}: account route manifest entry`)
  const route = imports('index.html').concat(imports(accountEntry))
  const accountFiles = [...new Set(route)].map(file => files.find(item => `assets/${item.name}` === file)).filter(Boolean)
  const chartFiles = accountFiles.filter(file => /^(echarts-|chart-renderer-)/.test(file.name))
  bundles[name] = { files, account_first_load: { files: accountFiles.map(file => file.name), js_bytes: accountFiles.reduce((sum, file) => sum + file.bytes, 0), gzip_js_bytes: accountFiles.reduce((sum, file) => sum + file.gzip_bytes, 0), chart_files: chartFiles.map(file => file.name), chart_js_bytes: chartFiles.reduce((sum, file) => sum + file.bytes, 0), chart_gzip_js_bytes: chartFiles.reduce((sum, file) => sum + file.gzip_bytes, 0) }, js_bytes: files.filter(file => file.name.endsWith('.js')).reduce((sum, file) => sum + file.bytes, 0) }
}
const report = { generated_at: new Date().toISOString(), runtime: process.version, platform: `${process.platform}/${process.arch}`, methodology: 'Synthetic utility CPU benchmarks; 2 warmup rounds then 7 alternating samples, median; every workload output compared by deep equality. No network/server-latency claim. Account bundle counts entry plus route static JS imports, excluding deferred charts.', baseline_repairs: baselineRepairs, measurements, bundles }
await writeFile(path.join(output, 'benchmark.json'), JSON.stringify(report, null, 2))
console.log(JSON.stringify({ account_first_load: { before: bundles.before.account_first_load, after: bundles.after.account_first_load } }, null, 2))
if (process.argv.includes('--browser')) await import('./benchmark-account-loading.mjs')
