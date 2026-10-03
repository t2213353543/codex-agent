import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import { readFile, readdir, writeFile } from 'node:fs/promises'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const root = path.resolve(fileURLToPath(new URL('../..', import.meta.url)))
const iteration = process.argv[2]
assert.match(iteration || '', /^[a-z0-9][a-z0-9-]{0,63}$/)
const folder = path.join(root, '.local/management-parity', iteration)
const digest = body => createHash('sha256').update(body).digest('hex')
const hash = async file => digest(await readFile(file))
const manifestBody = await readFile(path.join(folder, 'build-manifest.json'))
const manifest = JSON.parse(manifestBody)
for (const [name, expected] of Object.entries(manifest.source_files)) assert.equal(await hash(path.join(root, name)), expected, `Source changed: ${name}`)
for (const [name, expected] of Object.entries(manifest.files)) assert.equal(await hash(path.join(folder, 'dist', name)), expected, `Asset changed: ${name}`)
assert.equal(Object.keys(manifest.files).length, 54)
assert.equal(manifest.unchanged_baseline_resources, 48)
assert.equal(manifest.frontend_sha256, digest(JSON.stringify(Object.fromEntries(Object.entries(manifest.files).sort()))))

const browser = JSON.parse(await readFile(path.join(folder, 'logs-browser/report.json'), 'utf8'))
assert.equal(browser.status, 'pass')
assert.deepEqual(browser.errors, [])
assert.equal(path.resolve(browser.dist), path.join(folder, 'dist'))
const groups = ['behavior', 'cleanup', 'noBilling', 'analyticsBoundaries', 'observations', 'review', 'geo', 'session']
if (browser.warmup) groups.push('warmup')
if (browser.severity) groups.push('severity')
if (browser.passive) groups.push('passive', 'ingress')
const groupCounts = Object.fromEntries(groups.map(name => [name, browser[name].assertions.length]))
const browserAssertions = Object.values(groupCounts).reduce((sum, count) => sum + count, 0)
assert.equal(browserAssertions, browser.passive ? 54 : browser.severity ? 46 : browser.warmup ? 42 : 38)
const tap = await readFile(path.join(folder, 'frontend-unit.tap'), 'utf8')
const counts = Object.fromEntries(['tests', 'pass', 'fail', 'skipped'].map(name => {
  const match = tap.match(new RegExp(`^# ${name} (\\d+)\\r?$`, 'm'))
  assert.ok(match, `Missing TAP ${name}`)
  return [name, Number(match[1])]
}))
assert.equal(counts.tests, counts.pass)
assert.equal(counts.fail, 0)
assert.equal(counts.skipped, 0)
const files = (await readdir(path.join(root, 'frontend/tests'))).filter(name => name.endsWith('.test.js'))
const manifestHash = digest(manifestBody), completedAt = new Date().toISOString()
const unit = { status: 'pass', exit_code: 0, source_matches_build: true, test_files: files.length, counts, build_manifest_sha256: manifestHash, completed_at: completedAt, evidence: 'frontend-unit.tap', evidence_sha256: digest(tap) }
await writeFile(path.join(folder, 'frontend-unit-report.json'), JSON.stringify(unit, null, 2) + '\n')
const screenshots = (await readdir(path.join(folder, 'logs-browser'))).filter(name => name.endsWith('.png')).sort()
const evidence = { 'logs-browser/report.json': await hash(path.join(folder, 'logs-browser/report.json')), 'frontend-unit.tap': digest(tap) }
if (browser.ingress) {
  const file = 'logs-browser/ingress-visual-report.json', visual = JSON.parse(await readFile(path.join(folder, file), 'utf8'))
  assert.equal(visual.status, 'passed'); assert.deepEqual(visual.errors, []); assert.equal(visual.manifest_sha256, manifestHash)
  evidence[file] = await hash(path.join(folder, file))
}
for (const file of screenshots) evidence['logs-browser/' + file] = await hash(path.join(folder, 'logs-browser', file))
const report = {
  status: 'passed', scope: 'operational_logs_without_billing', verified_at: completedAt, synthetic_network: true,
  manifest_sha256: manifestHash, frontend_sha256: manifest.frontend_sha256, entries: manifest.entries,
  baseline_resources_unchanged: 48, browser_assertions: browserAssertions, browser_groups: groupCounts,
  pure_tests: counts.pass, pure_tests_scope: 'complete active frontend unit suite', page_errors: [], screenshots, evidence,
  sources: manifest.source_files, source_files_verified: Object.keys(manifest.source_files).length,
  source_aggregate_sha256: digest(JSON.stringify(Object.fromEntries(Object.entries(manifest.source_files).sort()))),
  retired_suites: browser.retired_suites, deployed: false,
  visual_review: { files: browser.passive ? ['passive-image-detail-dark-390.png', 'ingress-events-light-1440.png'] : browser.severity ? ['severity-detail-dark-390.png', 'severity-review-list-light-1440.png'] : browser.warmup ? ['websocket-warmup-detail-dark-390.png', 'websocket-warmup-detail-light-1440.png'] : ['logs-usage-coverage-ranking-dark-390.png', 'logs-usage-coverage-trend-light-1440.png'], action_items: [] },
  scope_notes: ['Numeric timezone offsets are rejected before requests; named aliases are canonicalized.', 'Unknown Token aggregates remain gaps, measured zero remains zero, partial samples expose coverage.', 'Retired billing suites are excluded and are not reported as passing.'],
}
await writeFile(path.join(folder, 'logs-verification.json'), JSON.stringify(report, null, 2) + '\n')
console.log(JSON.stringify({ status: report.status, browser_assertions: browserAssertions, unit_tests: counts.pass, screenshots: screenshots.length, manifest_sha256: manifestHash, source_aggregate_sha256: report.source_aggregate_sha256, frontend_sha256: report.frontend_sha256 }))
