import { readdir, writeFile } from 'node:fs/promises'
const root = new URL('../test-results/', import.meta.url)
const shots = []
for (const folder of ['browser', 'charts-browser', 'modals-browser']) {
  for (const filename of (await readdir(new URL(`${folder}/`, root)).catch(error => {
    if (error.code === 'ENOENT') return []
    throw error
  })).sort()) {
    const match = /^(.*)-(light|dark|ocean|mint)-(320|390|768|1440)\.png$/.exec(filename)
    if (match) shots.push({ src: `${folder}/${filename}`, page: match[1], theme: match[2], width: match[3] })
  }
}
const options = values => [...new Set(values)].map(value => `<option>${value}</option>`).join('')
const html = `<!doctype html><html lang="zh-CN"><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Codex Agent 前端截图验收</title>
<style>body{margin:0;background:#f7f7f8;color:#27272a;font:14px/1.6 system-ui,sans-serif}header{position:sticky;top:0;background:#fffffff2;padding:16px 24px;border-bottom:1px solid #ddd;z-index:1}h1{font-size:20px;margin:0 0 8px}label{display:inline-flex;gap:8px;margin-right:16px}select{padding:6px;border:1px solid #ccc;border-radius:6px}main{display:grid;grid-template-columns:repeat(auto-fill,minmax(260px,1fr));gap:20px;padding:24px}figure{margin:0;background:#fff;border:1px solid #ddd;border-radius:10px;padding:10px;min-width:0}figure[hidden]{display:none}img{display:block;width:100%;height:330px;object-fit:contain;object-position:top;background:#eee}figcaption{padding:8px 0 0}a{color:#4f46e5}small{color:#71717a}</style>
<header><h1>Codex Agent 前端截图验收</h1><p>全部使用隔离 mock 数据。点击截图查看原图；共 ${shots.length} 张。</p><label>页面<select id="page"><option value="">全部</option>${options(shots.map(s=>s.page))}</select></label><label>主题<select id="theme"><option value="">全部</option>${options(shots.map(s=>s.theme))}</select></label><label>宽度<select id="width"><option value="">全部</option>${options(shots.map(s=>s.width))}</select></label><small id="count"></small></header>
<main>${shots.map(s=>`<figure data-page="${s.page}" data-theme="${s.theme}" data-width="${s.width}"><a href="${s.src}" target="_blank"><img loading="lazy" src="${s.src}" alt="${s.page} ${s.theme} ${s.width}px"></a><figcaption>${s.page} · ${s.theme} · ${s.width}px</figcaption></figure>`).join('')}</main>
<script>const controls=['page','theme','width'].map(id=>document.getElementById(id));function filter(){let n=0;document.querySelectorAll('figure').forEach(f=>{f.hidden=controls.some(c=>c.value&&f.dataset[c.id]!==c.value);if(!f.hidden)n++});document.getElementById('count').textContent=n+' 张'}controls.forEach(c=>c.addEventListener('change',filter));filter()</script></html>`
await writeFile(new URL('screenshots.html', root), html)
console.log(`Screenshot gallery: ${shots.length} images`)
