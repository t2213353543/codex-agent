// Build Accounts and Logs on the verified restored shell plus Settings overlay.
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
import { bridges, bridgeSource, elementExports, legacyRelease, legacyManifestSHA, legacyMain, legacyAccounts, legacyLogs, extraMainExports, settingsManifestSHA } from './legacy-management-runtime/bridge.mjs'

const frontend = path.resolve(fileURLToPath(new URL('..', import.meta.url)))
const root = path.dirname(frontend)
const baseline = path.join(root, '.local/ui-releases', legacyRelease)
const baselineDist = path.join(baseline, 'frontend/dist')
const overlayFolder = path.join(root, '.local/settings-restore')
const overlayDist = path.join(overlayFolder, 'release-dist')
const args = process.argv.slice(2)
assert.ok(args.length === 0 || args.length === 2 && args[0] === '--iteration' && /^[a-z0-9][a-z0-9-]{0,63}$/.test(args[1]), 'Use --iteration <simple-name> for a separate candidate')
const destination = path.join(root, '.local/management-parity', args[1] || '')
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
const rawOverlayManifest = await readFile(path.join(overlayFolder, 'build-manifest.json'))
assert.equal(sha(rawOverlayManifest), settingsManifestSHA, 'Pinned Settings overlay manifest changed')
const overlayManifest = JSON.parse(rawOverlayManifest)
assert.equal(Object.keys(overlayManifest.files).length, 49)
assert.deepEqual(await mapFiles(overlayDist), overlayManifest.files, 'Settings overlay is incomplete or modified')
for (const [name, checksum] of Object.entries(manifest.frontend)) {
  if (name !== 'index.html') assert.equal(overlayManifest.files[name], checksum, `Settings overlay changed legacy resource: ${name}`)
}

// Verify and expose only the existing request/component bindings. No runtime
// implementation is rewritten and all consumers resolve one module instance.
const originalMain = await readFile(path.join(baselineDist, 'assets', legacyMain), 'utf8')
const originalAST = parse(originalMain, { sourceType: 'module' })
const bindings = new Set()
for (const statement of originalAST.program.body) {
  if (statement.type === 'FunctionDeclaration') bindings.add(statement.id?.name)
  if (statement.type === 'VariableDeclaration') for (const item of statement.declarations) if (item.id.type === 'Identifier') bindings.add(item.id.name)
}
for (const binding of Object.values(extraMainExports)) assert.ok(bindings.has(binding), `Missing verified runtime binding: ${binding}`)
assert.ok(originalMain.includes('async function Fe(e,{method:'))
const requestExports = Object.entries(extraMainExports).map(([alias, binding]) => `${binding} as ${alias}`).join(', ')
const mainCode = originalMain + `\nexport { ${requestExports} };\n`
const mainFile = `index-management-${sha(mainCode).slice(0, 16)}.js`
// Check the public module boundary mechanically, including the default export.
const parsedExports = new Map()
for (const entries of Object.values(bridges)) {
  for (const [file, alias] of Object.values(entries)) {
    if (!parsedExports.has(file)) {
      const ast = parse(file === legacyMain ? mainCode : await readFile(path.join(baselineDist, 'assets', file), 'utf8'), { sourceType: 'module' })
      const names = new Set(ast.program.body.flatMap(statement => statement.type === 'ExportNamedDeclaration'
        ? statement.specifiers.map(specifier => specifier.exported.name ?? specifier.exported.value) : []))
      parsedExports.set(file, names)
    }
    assert.ok(parsedExports.get(file).has(alias), `Missing legacy export ${file}:${alias}`)
  }
}

const apiFile = normalize(path.join(frontend, 'src/api'))
const headerFile = normalize(path.join(frontend, 'src/components/PageHeader.vue'))
const managementFile = normalize(path.join(frontend, 'src/utils/management'))
const virtualPrefix = '\0legacy-management:'
const bridgePlugin = {
  name: 'verified-legacy-runtime', enforce: 'pre',
  resolveId(id, importer) {
    if (id in bridges) return virtualPrefix + id
    if (importer && id.startsWith('.')) {
      const resolved = normalize(path.resolve(path.dirname(importer.split('?')[0]), id))
      if (resolved === normalize(path.join(frontend, 'src/utils/echarts')) || resolved === normalize(path.join(frontend, 'src/utils/echarts.js'))) return virtualPrefix + 'legacy-echarts'
      if (resolved === apiFile || resolved === apiFile + '.js') return virtualPrefix + 'legacy-api'
      if (resolved === headerFile) return virtualPrefix + 'legacy-page-header'
      if (resolved === managementFile || resolved === managementFile + '.js') return virtualPrefix + 'legacy-settings-management'
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

const outputs = []
const routeEntries = {}
for (const route of ['Accounts', 'Logs']) {
const result = await build({
  configFile: false, root: frontend, publicDir: false,
  // The restored runtime tree-shook createStaticVNode; ordinary VNodes keep
  // new templates compatible without introducing a second Vue runtime.
  plugins: [bridgePlugin, vue({ template: { compilerOptions: { hoistStatic: false } } }), Components({
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
    lib: { entry: path.join(frontend, `src/views/${route}.vue`), formats: ['es'] },
    rollupOptions: {
      external: id => /^\/assets\/[^/]+\.js$/.test(id),
      output: { entryFileNames: `${route.toLowerCase()}-managed-[hash].js`, assetFileNames: `${route.toLowerCase()}-managed-[hash][extname]`, inlineDynamicImports: true },
    },
  },
})
const routeOutputs = (Array.isArray(result) ? result : [result]).flatMap(item => item.output)
outputs.push(...routeOutputs)
const chunks = routeOutputs.filter(item => item.type === 'chunk')
const entry = chunks.find(item => item.isEntry)
assert.ok(entry && entry.exports.includes('default'), `${route} must export a route component`)
assert.equal(chunks.length, 1, `${route} must contain one isolated route module`)
assert.ok(entry.imports.includes(`/assets/${legacyMain}`), 'Missing shared legacy Vue runtime')
for (const module of Object.keys(entry.modules)) {
  assert.ok(!/\/node_modules\/(?:@vue\/|vue\/|vue-router\/|element-plus\/)/.test(normalize(module)), `Duplicate UI runtime bundled: ${module}`)
}
for (const imported of entry.imports) {
  assert.ok(imported.startsWith('/assets/') && manifest.frontend[imported.slice(1)], `Unverified runtime import: ${imported}`)
}
routeEntries[route] = entry.fileName
}
const css = outputs.filter(item => item.type === 'asset' && item.fileName.endsWith('.css'))
assert.ok(css.length > 0, 'Management pages must provide scoped styles')
for (const file of css) {
  const content = String(file.source)
  assert.ok(content.includes('[data-v-'), 'Management styles must be scoped')
  parseCSS(content).walkRules(rule => {
    if (rule.parent?.type === 'atrule' && /keyframes$/i.test(rule.parent.name)) return
    assert.ok(rule.selectors.every(selector => selector.includes('[data-v-')), `Unscoped selector could alter the old UI: ${rule.selector}`)
  })
}
for (const [file, checksum] of sourceSnapshots) assert.equal(sha(await readFile(file)), checksum, `Source changed during build: ${file}`)

const oldIndex = await readFile(path.join(overlayDist, 'index.html'), 'utf8')
assert.equal((oldIndex.match(/<script type="module"/g) || []).length, 1)
const maps = [...oldIndex.matchAll(/<script type="importmap">(.*?)<\/script>/gs)]
assert.equal(maps.length, 1, 'Expected one existing overlay import map')
const importMap = JSON.parse(maps[0][1])
assert.deepEqual(importMap, overlayManifest.import_map, 'Overlay import map differs from its manifest')
importMap.imports[`/assets/${legacyAccounts}`] = `/assets/${routeEntries.Accounts}`
importMap.imports[`/assets/${legacyLogs}`] = `/assets/${routeEntries.Logs}`
importMap.imports[`/assets/${legacyMain}`] = `/assets/${mainFile}`
const index = oldIndex.replace(`src="/assets/${legacyMain}"`, `src="/assets/${mainFile}"`).replace(maps[0][0], `<script type="importmap">${JSON.stringify(importMap)}</script>`)
  .replace('  </head>', `${css.map(file => `    <link rel="stylesheet" href="/assets/${file.fileName}">`).join('\n')}\n  </head>`)

await mkdir(destination, { recursive: true })
assert.equal(await realpath(destination), destination, 'Output parent must not contain symlinks')
const staging = path.join(destination, `.staging-${randomUUID()}`)
await mkdir(staging)
try {
  for (const name of Object.keys(overlayManifest.files)) {
    const target = path.join(staging, name)
    await mkdir(path.dirname(target), { recursive: true })
    await writeFile(target, await readFile(path.join(overlayDist, name)))
  }
  for (const artifact of outputs) {
    assert.ok(!artifact.fileName.includes('/') && !artifact.fileName.includes('\\'), 'Unexpected library artifact path')
    assert.ok(!overlayManifest.files[`assets/${artifact.fileName}`], 'New artifact collides with a legacy filename')
    await writeFile(path.join(staging, 'assets', artifact.fileName), artifact.type === 'chunk' ? artifact.code : artifact.source)
  }
  await writeFile(path.join(staging, 'assets', mainFile), mainCode)
  await writeFile(path.join(staging, 'index.html'), index)
  const files = await mapFiles(staging)
  for (const [name, checksum] of Object.entries(overlayManifest.files)) {
    if (name !== 'index.html') assert.equal(files[name], checksum, `Legacy resource changed: ${name}`)
  }
  const sources = Object.fromEntries([...sourceSnapshots].map(([file, checksum]) => [normalize(path.relative(root, file)), checksum]))
  for (const name of ['frontend/scripts/build-management-parity.mjs', 'frontend/scripts/legacy-management-runtime/bridge.mjs', 'frontend/scripts/legacy-settings-runtime/bridge.mjs', 'frontend/scripts/legacy-logs-runtime/bridge.mjs', 'frontend/package-lock.json']) sources[name] = sha(await readFile(path.join(root, name)))
  const report = {
    schema: 'codex-agent-management-parity-build-v1', built_at: new Date().toISOString(),
    legacy_release: legacyRelease, legacy_manifest_sha256: legacyManifestSHA,
    baseline_file_count: 49, unchanged_baseline_resources: 48, unchanged_legacy_resources: 44, settings_overlay_manifest_sha256: settingsManifestSHA, only_modified_legacy_file: 'index.html',
    import_map: importMap, entries: routeEntries, styles: css.map(file => file.fileName),
    shared_runtime: `/assets/${mainFile}`, original_runtime: `/assets/${legacyMain}`, runtime_append_only_exports: extraMainExports, bundled_second_runtime: false,
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
  console.log(JSON.stringify({ status: 'pass', output: outputDist, legacy_resources_unchanged: 44, baseline_resources_unchanged: 48, file_count: Object.keys(files).length, entries: routeEntries, styles: report.styles }))
} finally {
  await rm(staging, { recursive: true, force: true })
}
