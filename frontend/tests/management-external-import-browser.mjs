import assert from 'node:assert/strict'
import { createServer } from 'node:http'
import { mkdir, readFile, writeFile } from 'node:fs/promises'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { createHash } from 'node:crypto'
import { chromium, expect } from 'playwright/test'
import { installMockApi } from './mock-api.mjs'
import { testExternalImportLeases } from './external-import-leases-browser.mjs'
const root=path.resolve(fileURLToPath(new URL('../..',import.meta.url))),dist=path.resolve(process.env.MANAGEMENT_UI_DIST||path.join(root,'.local/management-parity/portability-20260918/dist')),output=path.resolve(process.env.EXTERNAL_IMPORT_REPORT_DIR||path.join(root,'.local/management-parity/portability-20260918/external-import-browser'))
await mkdir(output,{recursive:true})
const manifestRaw=await readFile(path.join(path.dirname(dist),'build-manifest.json')),manifest=JSON.parse(manifestRaw),sha=data=>createHash('sha256').update(data).digest('hex')
const report={status:'running',synthetic_data:true,manifest_sha256:sha(manifestRaw),dist,errors:[],assets:{},screenshots:output}
const server=createServer(async(req,res)=>{try{const pathname=decodeURIComponent(new URL(req.url,'http://127.0.0.1').pathname),file=path.resolve(dist,pathname==='/'?'index.html':pathname.slice(1));assert.ok(file.startsWith(dist+path.sep));const data=await readFile(file),name=path.relative(dist,file).replaceAll('\\','/');assert.equal(sha(data),manifest.files[name]);report.assets[name]=sha(data);res.writeHead(200,{'content-type':file.endsWith('.js')?'text/javascript':file.endsWith('.css')?'text/css':'text/html; charset=utf-8'});res.end(data)}catch{res.writeHead(404);res.end('Not found')}})
await new Promise(resolve=>server.listen(0,'127.0.0.1',resolve))
let browser,page
try{
  browser=await chromium.launch({channel:process.env.TEST_BROWSER||'chrome',headless:true});const context=await browser.newContext({viewport:{width:1440,height:1100},acceptDownloads:true,reducedMotion:'reduce',serviceWorkers:'block'}),state=await installMockApi(context);state.authenticated=true
  await context.route('**/api/accounts/manage**',route=>route.fulfill({json:{accounts:state.accounts,total:state.accounts.length,total_kind:'exact',has_more:false,page:1,page_size:20}}))
  page=await context.newPage();page.setDefaultTimeout(12000);page.on('pageerror',cause=>report.errors.push(cause.message))
  await page.goto(`http://127.0.0.1:${server.address().port}/#/accounts`);await expect(page.locator('.accounts-page')).toBeVisible()
  report.behavior=await testExternalImportLeases(page,{screenshotDirectory:output});assert.deepEqual(report.errors,[]);assert.equal(sha(await readFile(path.join(path.dirname(dist),'build-manifest.json'))),report.manifest_sha256);report.status='passed'
}catch(cause){report.status='failed';report.failure=cause.stack||cause.message;throw cause}
finally{await writeFile(path.join(output,'report.json'),JSON.stringify(report,null,2)+'\n');await browser?.close();await new Promise(resolve=>server.close(resolve));console.log(JSON.stringify({status:report.status,assertions:report.behavior?.assertions.length||0,errors:report.errors.length,report:path.join(output,'report.json')}))}
