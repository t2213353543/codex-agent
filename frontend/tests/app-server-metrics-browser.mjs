import assert from 'node:assert/strict'
import { createServer } from 'node:http'
import { readFile, mkdir, writeFile } from 'node:fs/promises'
import { fileURLToPath } from 'node:url'
import path from 'node:path'
import { chromium, expect } from 'playwright/test'
import { installMockApi } from './mock-api.mjs'

const root=fileURLToPath(new URL('../dist/',import.meta.url)), output=fileURLToPath(new URL('../test-results/app-server-metrics/',import.meta.url))
await mkdir(output,{recursive:true})
const server=createServer(async(req,res)=>{try{const pathname=new URL(req.url,'http://localhost').pathname;const file=path.resolve(root,pathname==='/'?'index.html':decodeURIComponent(pathname).slice(1));if(path.relative(root,file).startsWith('..'))throw Error();const data=await readFile(file);res.writeHead(200,{'Content-Type':({'.html':'text/html','.js':'text/javascript','.css':'text/css'})[path.extname(file)]||'application/octet-stream'}).end(data)}catch{res.writeHead(404).end()}})
await new Promise(r=>server.listen(0,'127.0.0.1',r));let browser;const errors=[];let passed=false
try{
 browser=await chromium.launch({channel:process.env.TEST_BROWSER||'chrome',headless:true});const ctx=await browser.newContext({viewport:{width:1440,height:1000},reducedMotion:'reduce'});const state=await installMockApi(ctx);state.authenticated=true
 const row={id:9,event_id:'official-metrics',time:1790490120,status:'ok',http_code:200,stream:true,request_type:'official_app_server',endpoint:'responses',model:'gpt-5.6-sol',sent_model:'gpt-5.6-sol',returned_model:'gpt-5.6-sol',upstream_model:'gpt-5.6-sol',upstream_model_source:'app_server_thread',observation_schema_version:1,input_tokens:10492,output_tokens:6,cached_tokens:10240,usage_known:true,ttft_ms:1655,latency_ms:2099,official_runtime_ms:1,official_inference_ms:2094,account:'Synthetic runtime',key_name:'Synthetic key'}
 await ctx.route('**/api/logs?*',r=>r.fulfill({json:{items:[row],total:1,total_kind:'exact',has_more:false,next_cursor:'',page:1,page_size:50,scope:'retained_request_details'}}))
 await ctx.route('**/api/logs/official-metrics*',r=>r.fulfill({json:{record:row}}))
 const page=await ctx.newPage();page.on('pageerror',e=>errors.push(e.message));await page.goto(`http://127.0.0.1:${server.address().port}/#/logs`)
 const table=page.locator('.log-table');await expect(table).toContainText('10,492');await expect(table).toContainText('10,240');await expect(table).toContainText('97.6%');await expect(table).toContainText('返回 gpt-5.6-sol');await expect(table).toContainText('首字 1.655 秒')
 await table.getByRole('button',{name:/日志详情/}).click();const drawer=page.getByRole('dialog');await expect(drawer).toBeVisible()
 const summary=drawer.locator('summary').filter({hasText:'链路'});if(await summary.count())await summary.first().click()
 const runtime=drawer.getByText('官方运行时准备',{exact:true});await expect(runtime).toBeVisible();await expect(drawer.getByText('官方推理及输出',{exact:true})).toBeVisible();await expect(drawer).not.toContainText('Unmocked endpoint');await runtime.scrollIntoViewIfNeeded()
 await page.screenshot({path:path.join(output,'metrics-desktop.png'),fullPage:true})
 await page.setViewportSize({width:390,height:844});assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth+1));await page.screenshot({path:path.join(output,'metrics-mobile.png'),fullPage:true})
 assert.deepEqual(errors,[]);passed=true
}finally{await browser?.close();await new Promise(r=>server.close(r));await writeFile(path.join(output,'report.json'),JSON.stringify({passed,errors,checks:['observed input/output/cache','actual cache percentage','returned model visible when same as requested','TTFT duration','mobile no overflow']},null,2));console.log(JSON.stringify({passed,output}))}
