import { build } from 'vite'
import { spawn } from 'node:child_process'
import { access, readdir, writeFile, mkdir } from 'node:fs/promises'
import { fileURLToPath } from 'node:url'
import path from 'node:path'

const frontend = fileURLToPath(new URL('../', import.meta.url))
const output = path.resolve(frontend, '../.local/management-completion')
await mkdir(output, { recursive: true })
const existing = process.argv.indexOf('--dist')
if (existing >= 0 && !process.argv[existing + 1]) throw new Error('--dist requires a built frontend directory')
const dist = existing >= 0 ? path.resolve(process.cwd(), process.argv[existing + 1]) : path.join(output, `suite-dist-${Date.now()}`)
if (existing < 0) await build({ root: frontend, build: { outDir: dist } })
await access(path.join(dist, 'index.html'))
const environment = { ...process.env, MANAGEMENT_UI_DIST: dist }
const unitFiles = (await readdir(path.join(frontend, 'tests'))).filter(name => name.endsWith('.test.js')).map(name => `tests/${name}`)
const checks = [
  ['unit', ['--test', ...unitFiles]],
  ['management', ['tests/management-completion-browser.mjs']],
  ['resources', ['tests/resource-management-browser.mjs']],
  ['settings', ['tests/settings-browser.mjs']],
  ['charts_and_logs', ['tests/charts-browser.mjs']],
]
const results = []
for (const [name, args] of checks) {
  const started = Date.now()
  const code = await new Promise(resolve => {
    const child = spawn(process.execPath, args, { cwd: frontend, env: environment, windowsHide: true, stdio: 'inherit' })
    child.on('error', () => resolve(1))
    child.on('exit', status => resolve(status ?? 1))
  })
  results.push({ name, exit_code: code, elapsed_seconds: (Date.now() - started) / 1000 })
  if (code !== 0) break
}
const passed = results.length === checks.length && results.every(result => result.exit_code === 0)
await writeFile(path.join(output, 'frontend-suite.json'), JSON.stringify({ passed, dist, results, completed_at: new Date().toISOString() }, null, 2))
if (!passed) process.exitCode = 1
