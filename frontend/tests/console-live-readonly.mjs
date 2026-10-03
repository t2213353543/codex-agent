import { chromium } from 'playwright'
import crypto from 'node:crypto'
import assert from 'node:assert/strict'
let input=''; for await(const chunk of process.stdin) input+=chunk
const config=JSON.parse(input)
const report={passed:false,checks:[],errors:[],apiFailures:[],business_writes:0,gateway_requests:0}
let browser,context
try{
 browser=await chromium.launch({headless:true,channel:process.env.TEST_BROWSER||'chrome'})
 context=await browser.newContext({viewport:{width:1440,height:1000},reducedMotion:'reduce'})
 const session=await context.request.post(config.base_url+'/api/session',{data:{admin_key:config.admin_key}})
 assert.equal(session.status(),200,'Session login failed')
 assert.equal((await session.json()).authenticated,true)
 report.checks.push('real_cookie_session')
 const assetQueue=Object.entries(config.expected_files)
 await Promise.all(Array.from({length:4},async()=>{while(assetQueue.length){
  const [file,expected]=assetQueue.shift()
  const response=await context.request.get(config.base_url+'/'+(file==='index.html'?'':file),{timeout:90000})
  assert.equal(response.status(),200,`Static resource failed: ${file}`)
  assert.equal(crypto.createHash('sha256').update(await response.body()).digest('hex'),expected,`Static resource hash mismatch: ${file}`)
 }}))
 report.checks.push(`exact_candidate_assets_${Object.keys(config.expected_files).length}`)
 const page=await context.newPage()
 page.on('pageerror',error=>report.errors.push(error.message))
 page.on('response',response=>{if(response.url().includes('/api/')&&response.status()>=400)report.apiFailures.push({path:new URL(response.url()).pathname,status:response.status()})})
 await page.route('**/*',async route=>{
  const request=route.request(),url=new URL(request.url())
  const sessionAllowed=url.pathname==='/api/session'&&['POST','DELETE'].includes(request.method())
  const officialUsageQuery=url.pathname==='/api/accounts/upstream/batch'&&request.method()==='POST'&&request.postDataJSON()?.kind==='usage'
  const queryAllowed=request.method()==='POST'&&['/api/logs/snapshot','/api/accounts/last-used','/api/accounts/usage-batch'].includes(url.pathname)||officialUsageQuery
  if(!['GET','HEAD'].includes(request.method())&&!sessionAllowed&&!queryAllowed){report.business_writes++;return route.abort()}
  if(url.pathname.startsWith('/v1/')||['/responses','/models','/guardian'].includes(url.pathname)){report.gateway_requests++;return route.abort()}
  return route.continue()
 })
 const titles={'/':'运行总览','/accounts':'账号管理','/groups':'分组管理','/keys':'API KEY 管理','/proxies':'IP 代理池','/logs':'使用记录','/settings':'管理设置','/connection':'接入指南','/diagnostics':'运行诊断','/audit-logs':'操作记录'}
 const routes=Object.keys(titles)
 async function checkCenteredDetails(path,label){
  const selector=path==='/accounts'?'.account-name:visible':path==='/logs'?'button[aria-label$="的日志详情"]:visible':['/groups','/keys','/proxies'].includes(path)?'.resource-name:visible':''
  if(!selector)return
  const trigger=page.locator(selector).first()
  if(!await trigger.count())return
  await trigger.click()
  const surface=page.locator('.detail-dialog:visible').last()
  await surface.waitFor({state:'visible'})
  await page.waitForTimeout(220)
  const geometry=await surface.evaluate(el=>{const r=el.getBoundingClientRect(),footer=el.querySelector('.el-dialog__footer')?.getBoundingClientRect();return {x:r.x,y:r.y,width:r.width,height:r.height,viewportWidth:innerWidth,viewportHeight:innerHeight,footerBottom:footer?.bottom}})
  assert(Math.abs(geometry.x+geometry.width/2-geometry.viewportWidth/2)<3,`Detail not horizontally centered ${label}`)
  assert(Math.abs(geometry.y+geometry.height/2-geometry.viewportHeight/2)<3,`Detail not vertically centered ${label}`)
  assert(geometry.footerBottom<=geometry.viewportHeight,`Detail actions off screen ${label}`)
  assert.equal(await page.locator('.el-drawer:visible:not(.console-mobile-navigation)').count(),0)
  await surface.getByRole('button',{name:/^(关闭|关闭详情)$/}).click()
  await surface.waitFor({state:'hidden'})
  report.checks.push(`${label}_centered_details_visible_footer`)
 }
 for(const theme of ['light','dark']){
  await page.goto(config.base_url+'/#/')
  await page.evaluate(theme=>localStorage.setItem('theme',JSON.stringify({version:1,value:theme})),theme)
  await page.reload()
  for(const path of routes){
   const overviewResponse=path==='/'?page.waitForResponse(response=>new URL(response.url()).pathname==='/api/overview'&&new URL(response.url()).searchParams.get('include_breakdown')==='true'):null
   await page.goto(config.base_url+'/#'+path)
   await page.waitForFunction(title=>document.querySelector('.page-header-title')?.textContent.trim()===title,titles[path])
   await page.waitForLoadState('networkidle')
   const state=await page.evaluate(()=>({theme:document.documentElement.dataset.theme,path:location.hash,bodyWidth:document.documentElement.scrollWidth,width:innerWidth,visibleErrors:Array.from(document.querySelectorAll('.el-alert--error,.el-message--error,.form-error')).filter(el=>el.getBoundingClientRect().height>0).map(el=>el.className)}))
   assert.equal(state.theme,theme)
   assert(state.bodyWidth<=state.width+1,`Page overflow ${theme} ${path}`)
   assert.deepEqual(state.visibleErrors,[],`Page has an error state ${theme} ${path}`)
   if(path==='/'){
    const overview=await (await overviewResponse).json(),breakdown=overview.traffic_breakdown
    assert.equal(breakdown?.scope,'retained_request_details','Dashboard must receive real retained-detail breakdown')
    assert(breakdown.requests>0&&breakdown.errors>0,'Live dashboard should contain the recorded benchmark traffic and failures')
    assert(breakdown.models?.length>0&&breakdown.error_categories?.length>0,'Live model and error distributions must not be empty')
    const modelRows=page.locator('.dashboard-breakdown-grid .model-row')
    assert.equal(await modelRows.count(),Math.min(6,breakdown.models.length))
    assert((await modelRows.first().innerText()).includes(breakdown.models[0].name),'Model distribution must render the returned top model')
    assert.equal(await page.locator('.dashboard-breakdown-grid .failure-row').count(),breakdown.error_categories.length)
    assert.equal(await page.locator('.dashboard-breakdown-grid .breakdown-empty').count(),0,'Real dashboard distributions must not show empty placeholders')
    report.dashboard=report.dashboard||[]
    report.dashboard.push({theme,requests:breakdown.requests,errors:breakdown.errors,models:breakdown.models.map(row=>({name:row.name,requests:row.requests,errors:row.errors})),error_categories:breakdown.error_categories.map(row=>({name:row.name,requests:row.requests}))})
    report.checks.push(`${theme}_dashboard_real_model_and_error_distributions`)
   }
   if(path==='/accounts'){
    await page.getByRole('button',{name:'显示列',exact:true}).click()
    const columns=page.getByRole('dialog',{name:'显示 / 隐藏列',exact:true}),quota=columns.getByRole('checkbox',{name:'用量窗口',exact:true})
    if(!await quota.isChecked())await columns.getByText('用量窗口',{exact:true}).click()
    assert(await quota.isChecked(),'Official usage column preference must be enabled')
    await columns.getByRole('button',{name:'完成',exact:true}).click()
    const refresh=page.getByRole('button',{name:'刷新本页官方用量',exact:true})
    await refresh.scrollIntoViewIfNeeded()
    await page.waitForFunction(()=>{const button=document.querySelector('[aria-label="刷新本页官方用量"]');return button&&!button.disabled&&!button.classList.contains('is-loading')})
    const refreshStarted=Math.floor(Date.now()/1000)
    const usageResponse=page.waitForResponse(response=>new URL(response.url()).pathname==='/api/accounts/upstream/batch'&&response.request().postDataJSON()?.kind==='usage'&&response.request().postDataJSON()?.force===true)
    await refresh.click()
    const usage=await (await usageResponse).json()
    const successful=usage.results.filter(row=>row.ok&&row.snapshot?.ok&&row.snapshot.kind==='usage'&&row.snapshot.data?.rate_limit?.primary_window)
    assert(successful.length>0,'At least one live account must return an official usage window')
    const snapshot=successful[0].snapshot,rate=snapshot.data.rate_limit
    assert(snapshot.checked_at>=refreshStarted-5,'Forced official refresh must return a current query timestamp')
    const expected=[rate.primary_window,rate.secondary_window].filter(Boolean).map(window=>({label:window.limit_window_seconds%86400===0?`${window.limit_window_seconds/86400}d`:window.limit_window_seconds%3600===0?`${window.limit_window_seconds/3600}h`:`${window.limit_window_seconds/60}m`,used:typeof window.used_percent==='number'?`${Number(window.used_percent.toFixed(1))}%`:'未知'}))
    await page.waitForFunction(({checkedAt,expected})=>Array.from(document.querySelectorAll('.el-table__body-wrapper .official-usage-windows')).some(cell=>cell.title===`官方查询于 ${new Date(checkedAt*1000).toLocaleString('zh-CN')}`&&cell.querySelectorAll('.official-usage-window').length===expected.length&&expected.every((window,index)=>{const row=cell.querySelectorAll('.official-usage-window')[index];return row.querySelector('.official-usage-caption>span')?.textContent===window.label&&row.querySelector('.official-usage-caption>strong')?.textContent===window.used})),{checkedAt:snapshot.checked_at,expected})
    report.official_usage=report.official_usage||[]
    report.official_usage.push({theme,checked_at:snapshot.checked_at,source:snapshot.source,primary_window:rate.primary_window,secondary_window:rate.secondary_window??null,displayed_windows:expected})
    report.checks.push(`${theme}_official_usage_column_real_windows_and_current_timestamp`)
   }
   await checkCenteredDetails(path,`${theme}_${path}`)
   report.checks.push(`${theme}_${path}_real_api`)
  }
 }
 await page.setViewportSize({width:390,height:844})
 for(const path of ['/accounts','/logs','/settings']){
  await page.goto(config.base_url+'/#'+path)
  await page.waitForFunction(title=>document.querySelector('.page-header-title')?.textContent.trim()===title,titles[path])
  await page.waitForLoadState('networkidle')
  assert(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth+1),`Mobile overflow ${path}`)
  await checkCenteredDetails(path,`mobile_${path}`)
  report.checks.push(`mobile_${path}`)
 }
 assert.deepEqual(report.errors,[],'Browser exceptions')
 assert.deepEqual(report.apiFailures,[],'API requests failed')
 assert.equal(report.business_writes,0)
 assert.equal(report.gateway_requests,0)
 report.passed=true
}catch(error){report.failure=error.message}
finally{if(context)await context.request.delete(config.base_url+'/api/session').catch(()=>{});if(browser)await browser.close()}
console.log(JSON.stringify(report));process.exitCode=report.passed?0:1
