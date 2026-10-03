import { readdir, readFile, mkdir, writeFile } from 'node:fs/promises'
import { gzipSync } from 'node:zlib'
const assets = new URL('../dist/assets/', import.meta.url)
const files = []
for (const name of (await readdir(assets)).sort()) {
  if (!/\.(js|css)$/.test(name)) continue
  const data = await readFile(new URL(name, assets))
  files.push({ name, bytes: data.length, gzip: gzipSync(data).length })
}
const totals = Object.fromEntries(['js', 'css'].map(ext => [ext, files.filter(f => f.name.endsWith(`.${ext}`)).reduce((sum, f) => ({ bytes: sum.bytes + f.bytes, gzip: sum.gzip + f.gzip }), { bytes: 0, gzip: 0 })]))
const report = { files, totals }
const output = process.argv[2] || 'after'
if (!/^[a-z-]+$/.test(output)) throw new Error('Invalid report name')
await mkdir(new URL('../artifacts/', import.meta.url), { recursive: true })
await writeFile(new URL(`../artifacts/bundle-${output}.json`, import.meta.url), JSON.stringify(report, null, 2))
console.log(JSON.stringify(totals, null, 2))
