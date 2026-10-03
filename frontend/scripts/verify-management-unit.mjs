import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import { readdir, readFile, writeFile } from 'node:fs/promises'
import path from 'node:path'
import { spawnSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'

const root = path.resolve(fileURLToPath(new URL('../..', import.meta.url)))
const iteration = process.argv[2]
assert.match(iteration || '', /^[a-z0-9][a-z0-9-]{0,63}$/)
const output = path.join(root, '.local/management-parity', iteration)
const digest = body => createHash('sha256').update(body).digest('hex')
const rawManifest = await readFile(path.join(output, 'build-manifest.json'))
const manifest = JSON.parse(rawManifest)
async function verifySource() {
  for (const [name, expected] of Object.entries(manifest.source_files)) {
    const file = path.resolve(root, name)
    assert.ok(file.startsWith(root + path.sep))
    assert.equal(digest(await readFile(file)), expected, `Build source changed: ${name}`)
  }
}
await verifySource()
const files = (await readdir(path.join(root, 'frontend/tests'))).filter(name => name.endsWith('.test.js')).sort()
const allowlist = new Set(['PATH', 'SYSTEMROOT', 'WINDIR', 'COMSPEC', 'PATHEXT', 'TEMP', 'TMP', 'USERPROFILE', 'APPDATA', 'LOCALAPPDATA', 'PROGRAMDATA', 'PROGRAMFILES', 'PROGRAMFILES(X86)', 'SYSTEMDRIVE', 'NUMBER_OF_PROCESSORS', 'PROCESSOR_ARCHITECTURE'])
const env = Object.fromEntries(Object.entries(process.env).filter(([key]) => allowlist.has(key.toUpperCase())))
const result = spawnSync(process.execPath, ['--test', '--test-reporter=tap', ...files.map(name => path.join('tests', name))], { cwd: path.join(root, 'frontend'), env, encoding: 'utf8', timeout: 120000, maxBuffer: 8 * 1024 * 1024 })
await writeFile(path.join(output, 'frontend-unit-tests.log'), (result.stdout || '') + (result.stderr || ''))
assert.equal(result.status, 0, `Node tests failed: ${result.error || result.stderr || result.status}`)
const counts = Object.fromEntries(['tests', 'pass', 'fail', 'skipped'].map(name => {
  const match = result.stdout.match(new RegExp(`^# ${name} (\\d+)$`, 'm'))
  assert.ok(match, `Missing ${name} result`)
  return [name, Number(match[1])]
}))
await verifySource()
const report = { status: 'pass', exit_code: 0, source_matches_build: true, test_files: files.length, counts, build_manifest_sha256: digest(rawManifest), completed_at: new Date().toISOString() }
await writeFile(path.join(output, 'frontend-unit-report.json'), JSON.stringify(report, null, 2) + '\n')
console.log(JSON.stringify(report))
