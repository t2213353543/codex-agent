// Build only the usage-records route on top of the preserved 19:33 UI.
// No original release artifact, gateway source, or live service is modified.
import assert from 'node:assert/strict'
import { createHash, randomUUID } from 'node:crypto'
import { readFile, writeFile, mkdir, readdir, lstat, realpath, rm, rename } from 'node:fs/promises'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { parse } from '@babel/parser'
import { parse as parseCSS } from 'postcss'
import { build } from 'vite'
import vue from '@vitejs/plugin-vue'
import Components from 'unplugin-vue-components/vite'
import { bridges, bridgeSource, elementExports, legacyRelease, legacyManifestSHA, legacyMain, legacyLogs, legacyScope } from './legacy-logs-runtime/bridge.mjs'

const frontend = path.resolve(fileURLToPath(new URL('..', import.meta.url)))
const root = path.dirname(frontend)
const baseline = path.join(root, '.local/ui-releases', legacyRelease)
const baselineDist = path.join(baseline, 'frontend/dist')
const destination = path.join(root, '.local/logs-improvement')
const outputDist = path.join(destination, 'dist')
const sha = data => createHash('sha256').update(data).digest('hex')
const normalize = value => value.replaceAll('\\', '/')
const sourceSnapshots = new Map()

async function mapFiles(directory) {
  const result = {}
  async function visit(folder) {
    for (const entry of await readdir(folder, { withFileTypes: true })) {
      const file = path.join(folder, entry.name)
      assert.ok(!entry.isSymbolicLink(), `Symlink rejected: ${file}`)
      if (entry.isDirectory()) await visit(file)
      else {
        assert.ok(entry.isFile(), `Non-regular artifact: ${file}`)
        result[normalize(path.relative(directory, file))] = sha(await readFile(file))
      }
    }
  }
  await visit(directory)
  return Object.fromEntries(Object.entries(result).sort(([a], [b]) => a.localeCompare(b)))
}

const rawManifest = await readFile(path.join(baseline, 'ui-manifest.json'))
assert.equal(sha(rawManifest), legacyManifestSHA, 'Pinned legacy manifest changed')
const manifest = JSON.parse(rawManifest)
assert.equal(manifest.release_id, legacyRelease)
assert.equal(Object.keys(manifest.frontend).length, 45)
assert.deepEqual(await mapFiles(baselineDist), manifest.frontend, 'Legacy artifact is incomplete or modified')
assert.equal(sha(await readFile(path.join(frontend, 'package-lock.json'))), manifest.frontend_source.files['frontend/package-lock.json'], 'Use the dependency lock verified with the legacy release')

// Check the public module boundary mechanically, including the default export.
const parsedExports = new Map()
for (const entries of Object.values(bridges)) {
  for (const [file, alias] of Object.values(entries)) {
    if (!parsedExports.has(file)) {
      const ast = parse(await readFile(path.join(baselineDist, 'assets', file), 'utf8'), { sourceType: 'module' })
      const names = new Set(ast.program.body.flatMap(statement => statement.type === 'ExportNamedDeclaration'
        ? statement.specifiers.map(specifier => specifier.exported.name ?? specifier.exported.value) : []))
      parsedExports.set(file, names)
    }
    assert.ok(parsedExports.get(file).has(alias), `Missing legacy export ${file}:${alias}`)
  }
}

const apiFile = normalize(path.join(frontend, 'src/api'))
const headerFile = normalize(path.join(frontend, 'src/components/PageHeader.vue'))
const virtualPrefix = '\0legacy-logs:'
const bridgePlugin = {
  name: 'verified-legacy-runtime', enforce: 'pre',
  resolveId(id, importer) {
    if (id in bridges) return virtualPrefix + id
    if (importer && id.startsWith('.')) {
      const resolved = normalize(path.resolve(path.dirname(importer.split('?')[0]), id))
      if (resolved === apiFile || resolved === apiFile + '.js') return virtualPrefix + 'legacy-api'
      if (resolved === headerFile) return virtualPrefix + 'legacy-page-header'
    }
    if (/^element-plus\//.test(id) || /^@vue\//.test(id) || /^vue\//.test(id)) {
      throw new Error(`Private or duplicate UI runtime dependency rejected: ${id}`)
    }
  },
  async load(id) {
    if (id.startsWith(virtualPrefix)) return bridgeSource(bridges[id.slice(virtualPrefix.length)])
    if (!id.includes('?') && normalize(id).startsWith(normalize(path.join(frontend, 'src')) + '/') && /\.(?:vue|js|mjs)$/.test(id)) {
      const content = await readFile(id, 'utf8')
      sourceSnapshots.set(id, sha(content))
      return content
    }
  },
}

const result = await build({
  configFile: false, root: frontend, publicDir: false,
  plugins: [bridgePlugin, vue(), Components({
    dts: false, dirs: [], directives: true,
    resolvers: [
      { type: 'component', resolve(name) {
        if (name in elementExports) return { name, from: 'element-plus' }
        if (name.startsWith('El')) throw new Error(`Unbridged legacy component: ${name}`)
      } },
      { type: 'directive', resolve(name) {
        if (name === 'Loading') return { name: 'ElLoadingDirective', from: 'element-plus' }
        throw new Error(`Unbridged legacy directive: ${name}`)
      } },
    ],
  })],
  build: {
    write: false, minify: false, cssCodeSplit: false, emptyOutDir: false,
    lib: { entry: path.join(frontend, 'src/views/Logs.vue'), formats: ['es'] },
    rollupOptions: {
      external: id => /^\/assets\/[^/]+\.js$/.test(id),
      output: { entryFileNames: 'logs-improved-[hash].js', assetFileNames: 'logs-improved-[hash][extname]', inlineDynamicImports: true },
    },
  },
})
const outputs = (Array.isArray(result) ? result : [result]).flatMap(item => item.output)
const chunks = outputs.filter(item => item.type === 'chunk')
const entry = chunks.find(item => item.isEntry)
assert.ok(entry && entry.exports.includes('default'), 'Logs must export a route component')
assert.equal(chunks.length, 1, 'Logs library must contain one isolated route module')
assert.ok(entry.imports.includes(`/assets/${legacyMain}`), 'Missing shared legacy Vue runtime')
for (const module of Object.keys(entry.modules)) {
  assert.ok(!/\/node_modules\/(?:@vue\/|vue\/|vue-router\/|element-plus\/)/.test(normalize(module)), `Duplicate UI runtime bundled: ${module}`)
}
for (const imported of entry.imports) {
  assert.ok(imported.startsWith('/assets/') && manifest.frontend[imported.slice(1)], `Unverified runtime import: ${imported}`)
}
const css = outputs.filter(item => item.type === 'asset' && item.fileName.endsWith('.css'))
assert.ok(css.length > 0, 'Logs must provide scoped styles')
for (const file of css) {
  const content = String(file.source)
  assert.ok(content.includes('[data-v-'), 'Logs styles must be scoped')
  assert.ok(!content.includes(`[data-v-${legacyScope}]`), 'New Logs scoped ID collides with the old route')
  parseCSS(content).walkRules(rule => {
    if (rule.parent?.type === 'atrule' && /keyframes$/i.test(rule.parent.name)) return
    assert.ok(rule.selectors.every(selector => selector.includes('[data-v-')), `Unscoped selector could alter the old UI: ${rule.selector}`)
  })
}
for (const [file, checksum] of sourceSnapshots) assert.equal(sha(await readFile(file)), checksum, `Source changed during build: ${file}`)

const oldIndex = await readFile(path.join(baselineDist, 'index.html'), 'utf8')
assert.equal((oldIndex.match(/<script type="module"/g) || []).length, 1)
assert.ok(!oldIndex.includes('type="importmap"'))
const importMap = { imports: { [`/assets/${legacyLogs}`]: `/assets/${entry.fileName}` } }
const index = oldIndex.replace('<script type="module"', `<script type="importmap">${JSON.stringify(importMap)}</script>\n    <script type="module"`)
  .replace('  </head>', `${css.map(file => `    <link rel="stylesheet" href="/assets/${file.fileName}">`).join('\n')}\n  </head>`)

await mkdir(destination, { recursive: true })
assert.equal(await realpath(destination), destination, 'Output parent must not contain symlinks')
const staging = path.join(destination, `.staging-${randomUUID()}`)
await mkdir(staging)
try {
  for (const name of Object.keys(manifest.frontend)) {
    const target = path.join(staging, name)
    await mkdir(path.dirname(target), { recursive: true })
    await writeFile(target, await readFile(path.join(baselineDist, name)))
  }
  for (const artifact of outputs) {
    assert.ok(!artifact.fileName.includes('/') && !artifact.fileName.includes('\\'), 'Unexpected library artifact path')
    assert.ok(!manifest.frontend[`assets/${artifact.fileName}`], 'New artifact collides with a legacy filename')
    await writeFile(path.join(staging, 'assets', artifact.fileName), artifact.type === 'chunk' ? artifact.code : artifact.source)
  }
  await writeFile(path.join(staging, 'index.html'), index)
  const files = await mapFiles(staging)
  for (const [name, checksum] of Object.entries(manifest.frontend)) {
    if (name !== 'index.html') assert.equal(files[name], checksum, `Legacy resource changed: ${name}`)
  }
  const sources = Object.fromEntries([...sourceSnapshots].map(([file, checksum]) => [normalize(path.relative(root, file)), checksum]))
  for (const name of ['frontend/scripts/build-logs-ui.mjs', 'frontend/scripts/legacy-logs-runtime/bridge.mjs', 'frontend/package-lock.json']) sources[name] = sha(await readFile(path.join(root, name)))
  const report = {
    schema: 'codex-agent-legacy-logs-build-v1', built_at: new Date().toISOString(),
    legacy_release: legacyRelease, legacy_manifest_sha256: legacyManifestSHA,
    baseline_file_count: 45, unchanged_legacy_resources: 44, only_modified_legacy_file: 'index.html',
    import_map: importMap, entry: entry.fileName, styles: css.map(file => file.fileName),
    shared_runtime: `/assets/${legacyMain}`, bundled_second_runtime: false,
    source_files: sources, files,
    frontend_sha256: sha(JSON.stringify(Object.fromEntries(Object.entries(files).sort()))),
  }
  const existing = await lstat(outputDist).catch(error => { if (error.code !== 'ENOENT') throw error })
  if (existing) {
    assert.ok(existing.isDirectory() && !existing.isSymbolicLink())
    assert.equal(await realpath(outputDist), outputDist, 'Refuse to replace an unexpected directory')
    await rm(outputDist, { recursive: true })
  }
  await rename(staging, outputDist)
  await writeFile(path.join(destination, 'build-manifest.json'), JSON.stringify(report, null, 2) + '\n')
  console.log(JSON.stringify({ status: 'pass', output: outputDist, legacy_resources_unchanged: 44, file_count: Object.keys(files).length, entry: entry.fileName, styles: report.styles }))
} finally {
  await rm(staging, { recursive: true, force: true })
}
