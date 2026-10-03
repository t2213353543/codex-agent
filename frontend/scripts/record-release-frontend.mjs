import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import { readFile, writeFile } from 'node:fs/promises'
import path from 'node:path'
const root = path.resolve(import.meta.dirname, '../..'), directory = path.join(root, '.local/management-parity/release-20260919')
const hash = bytes => createHash('sha256').update(bytes).digest('hex')
const read = async file => JSON.parse(await readFile(path.join(directory, file), 'utf8'))
const raw = await readFile(path.join(directory, 'build-manifest.json')), manifest = JSON.parse(raw), manifestHash = hash(raw)
const reports = { accounts: await read('accounts-no-billing-verification.json'), logs: await read('logs-verification.json'), units: await read('frontend-unit-report.json'), keyStorage: await read('key-storage-browser/report.json') }
for (const name of ['accounts', 'logs']) { assert.equal(reports[name].status, 'passed'); assert.equal(reports[name].manifest_sha256, manifestHash) }
assert.equal(reports.accounts.browser_groups, 55); assert.equal(reports.logs.browser_assertions, 54)
assert.deepEqual(reports.units.counts, { tests: 164, pass: 164, fail: 0, skipped: 0 }); assert.equal(reports.units.build_manifest_sha256, manifestHash)
assert.equal(reports.keyStorage.passed, true); assert.equal(reports.keyStorage.manifest_sha256, manifestHash)
for (const key of ['errors', 'external', 'leaked', 'unexpected']) assert.deepEqual(reports.keyStorage[key], [])
for (const [name, digest] of Object.entries(manifest.source_files)) assert.equal(hash(await readFile(path.join(root, name))), digest)
for (const [name, digest] of Object.entries(manifest.files)) assert.equal(hash(await readFile(path.join(directory, 'dist', name))), digest)
const sourceHash = hash(JSON.stringify(Object.fromEntries(Object.entries(manifest.source_files).sort())))
assert.equal(sourceHash, '9ab5ef587cecab2870a6c4ecaa79fe4ea59c11863bb8c122da1cfd09fbb71586')
const paths = ['build-manifest.json', 'accounts-no-billing-verification.json', 'logs-verification.json', 'frontend-unit-report.json', 'frontend-unit.tap', 'key-storage-browser/report.json', ...reports.keyStorage.screenshots.map(name => 'key-storage-browser/' + name)]
const rehearsal = await read('production-runner-rehearsal/browser-report.json')
assert.equal(rehearsal.status, 'pass'); assert.equal(rehearsal.frontend_manifest_sha256, manifestHash)
assert.equal(rehearsal.checks.length, 21); assert.equal(rehearsal.screenshots_saved, 0)
paths.push('production-runner-rehearsal/browser-report.json', 'production-runner-rehearsal/rehearsal.json')
const evidence = Object.fromEntries(await Promise.all(paths.map(async name => [name, hash(await readFile(path.join(directory, name)))])))
const verificationTools = Object.fromEntries(await Promise.all(['integration/tests/management-release-readonly.cjs', 'integration/tests/management-readonly-policy.cjs', 'integration/tests/management-release-readonly-local-check.cjs'].map(async name => [name, hash(await readFile(path.join(root, name)))])))
const out = { schema: 'codex-final-frontend-release-v1', status: 'passed', completed_at: new Date().toISOString(), manifest_sha256: manifestHash, frontend_sha256: manifest.frontend_sha256, source_before_and_after_sha256: sourceHash, source_matches_prebuild: true, static_files: Object.keys(manifest.files).length, source_files: Object.keys(manifest.source_files).length, unchanged_baseline_resources: manifest.unchanged_baseline_resources, accounts_browser_groups: 55, logs_browser_groups: 54, key_storage_viewports: reports.keyStorage.viewports, frontend_unit: reports.units.counts, page_errors: [], old_candidates_modified: false, billing_included: false, deployed_by_this_subtask: false, synthetic_test_only: true, production_browser_run: false, visual_review: ['key-storage-320.png'], evidence }
out.production_runner_synthetic_checks = rehearsal.checks.length
out.verification_tools = verificationTools
await writeFile(path.join(directory, 'frontend-release-verification.json'), JSON.stringify(out, null, 2) + '\n')
console.log(JSON.stringify({ status: out.status, manifest_sha256: manifestHash, frontend_sha256: out.frontend_sha256, sources: out.source_files }))
