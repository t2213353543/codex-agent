/* Actual frontend + actual isolated PG/Redis API, without mocks. */
const assert=require('node:assert/strict');
const http=require('node:http');
const fs=require('node:fs/promises');
const path=require('node:path');
const {chromium,expect:baseExpect}=require('../../frontend/node_modules/playwright/test');
const expect=baseExpect.configure({timeout:20000});
const root=path.resolve(__dirname,'../..');
const dist=path.join(root,'frontend/dist');
const output=path.resolve(process.env.INTEGRATION_UI_OUTPUT||path.join(root,'.local/integration/browser-real'));
const upstream=new URL(process.env.INTEGRATION_API_URL||'http://127.0.0.1:28882');
assert(['127.0.0.1','localhost'].includes(upstream.hostname)&&['28882'].includes(upstream.port),'Only isolated performance tunnel accepted');
(async()=>{
 await fs.mkdir(output,{recursive:true});
 const key=(await fs.readFile(path.join(root,'.local/integration/performance-data/client-key'),'utf8')).trim();
 const upstreamRequests=new Set();
 const server=http.createServer(async(req,res)=>{
  const url=new URL(req.url,'http://localhost');
  if(url.pathname.startsWith('/api/')||url.pathname.startsWith('/health')){
   const proxy=http.request({hostname:upstream.hostname,port:upstream.port,path:req.url,method:req.method,headers:req.headers},r=>{res.writeHead(r.statusCode,r.headers);r.pipe(res)});
   upstreamRequests.add(proxy);proxy.on('close',()=>upstreamRequests.delete(proxy));
   proxy.on('error',()=>{res.writeHead(502);res.end('Isolated backend unavailable')});req.pipe(proxy);return;
  }
  const filename=path.resolve(dist,'.'+decodeURIComponent(url.pathname==='/'?'/index.html':url.pathname));
  if(!filename.startsWith(dist+path.sep)){res.writeHead(403);res.end();return}
  try{const body=await fs.readFile(filename);res.writeHead(200,{'Content-Type':({'.html':'text/html','.js':'text/javascript','.css':'text/css','.svg':'image/svg+xml','.woff2':'font/woff2'})[path.extname(filename)]||'application/octet-stream'});res.end(body)}catch{res.writeHead(404);res.end()}
 });
 await new Promise(resolve=>server.listen(0,'127.0.0.1',resolve));
 const base='http://127.0.0.1:'+server.address().port;
 const report={scope:'Actual frontend and actual isolated PG/Redis backend; no API mocks; synthetic data',checks:[],resources:[],api_timings:[]};
 let browser;
 const check=(id,detail)=>report.checks.push({id,status:'pass',detail});
 const settled=async page=>{
  await expect(page.locator('.el-loading-mask:visible')).toHaveCount(0);
  const refresh=page.getByRole('button',{name:'刷新运行概览'});
  if(await refresh.count())await expect(refresh).not.toHaveClass(/is-loading/);
  await page.evaluate(()=>document.fonts.ready);
  await page.waitForLoadState('networkidle');
 };
 try{
  browser=await chromium.launch({headless:true,channel:'chrome'});
  const context=await browser.newContext({viewport:{width:1440,height:960},reducedMotion:'reduce'});
  const page=await context.newPage();page.setDefaultTimeout(20000);
  const errors=[];page.on('pageerror',e=>errors.push(e.message));
  page.on('response',r=>{if(!r.url().includes('/api/'))report.resources.push({path:new URL(r.url()).pathname,status:r.status()})});
  page.on('requestfinished',r=>{if(r.url().includes('/api/'))report.api_timings.push({path:new URL(r.url()).pathname,response_end_ms:r.timing().responseEnd})});
  await page.goto(base+'/#/accounts');
  await expect(page.getByRole('textbox',{name:'管理密钥'})).toBeVisible();
  check('real_ui.unauthorized','Unauthorized accounts route redirects to login');
  await page.getByRole('textbox',{name:'管理密钥'}).fill(key);
  await page.getByRole('button',{name:'登 录',exact:true}).click();
  await expect(page.locator('.page-header')).toContainText('账号管理');
  await expect(page.locator('.el-table__body-wrapper tbody tr').first()).toBeVisible();
  const cookies=await context.cookies();assert(cookies.some(x=>x.name==='codex_admin_session'&&x.httpOnly));
  assert(!(await page.evaluate(()=>JSON.stringify(localStorage))).includes(key));
  await page.reload();await expect(page.locator('.page-header')).toContainText('账号管理');
  await settled(page);
  if(await page.evaluate(()=>[...document.fonts].some(f=>/Noto|Codex Admin Sans/.test(f.family)))){
   const cdp=await context.newCDPSession(page);await cdp.send('DOM.enable');await cdp.send('CSS.enable');
   const {root:dom}=await cdp.send('DOM.getDocument');report.rendered_fonts=[];
   for(const selector of ['.page-header-title','.page-header-sub','.tag-link','.el-table__header-wrapper th:nth-child(2) .cell']){
    const {nodeId}=await cdp.send('DOM.querySelector',{nodeId:dom.nodeId,selector});
    const {fonts}=await cdp.send('CSS.getPlatformFontsForNode',{nodeId});
    assert(fonts.some(f=>f.isCustomFont&&f.glyphCount>0&&/Noto|Codex Admin Sans/.test(f.familyName)),JSON.stringify({selector,fonts}));
    report.rendered_fonts.push({selector,fonts});
   }
   await cdp.detach();
   const fonts=await page.evaluate(()=>performance.getEntriesByType('resource').filter(r=>new URL(r.name).pathname.endsWith('.woff2')).map(r=>({path:new URL(r.name).pathname,bytes:r.encodedBodySize})));
   assert(fonts.length>0&&fonts.every(f=>f.bytes>0),JSON.stringify(fonts));report.font_resources=fonts;
   check('real_ui.typography','Self-hosted Noto font actually renders Chinese title, subtitle, tab and table header; fonts load without external CDN');
  }
  report.first_screen=await page.evaluate(()=>({
   navigation:performance.getEntriesByType('navigation').map(({domContentLoadedEventEnd,loadEventEnd})=>({domContentLoadedEventEnd,loadEventEnd})),
   resources:performance.getEntriesByType('resource').map(({name,initiatorType,transferSize,encodedBodySize,decodedBodySize,duration})=>({path:new URL(name).pathname,initiatorType,transferSize,encodedBodySize,decodedBodySize,duration}))
  }));
  check('real_ui.session','Real Redis-backed HttpOnly cookie login and reload restore session; key absent from localStorage');
  await page.screenshot({path:path.join(output,'accounts-desktop.png'),fullPage:true});
  const apiErrors=[];
  page.on('response',r=>{if(r.url().includes('/api/')&&r.status()>=400)apiErrors.push({path:new URL(r.url()).pathname,status:r.status()})});
  for(const [route,title] of [['/','运行总览'],['/logs','使用记录'],['/proxies','IP 代理池'],['/groups','分组管理'],['/settings','管理设置']]){
   await page.goto(base+'/#'+route);await expect(page.locator('.page-header')).toContainText(title);
   await settled(page);
   await page.screenshot({path:path.join(output,(route==='/'?'overview':route.slice(1))+'.png'),fullPage:true});
  }
  assert.equal(apiErrors.length,0,JSON.stringify(apiErrors));
  check('real_ui.contract','Overview, accounts, logs, groups, proxies, settings load against real API without HTTP errors');
  if(process.env.INTEGRATION_UI_STYLE==='enterprise'){
   const geometry=await page.evaluate(()=>({
    header:document.querySelector('.topbar').getBoundingClientRect().height,
    tabs:document.querySelector('.tags-view').getBoundingClientRect().height,
    font:getComputedStyle(document.body).fontFamily,
    items:[...document.querySelectorAll('.tag-item')].map(el=>({width:el.getBoundingClientRect().width,
     gap:el.querySelector('.tag-close')?el.querySelector('.tag-close').getBoundingClientRect().left-el.querySelector('.tag-link').getBoundingClientRect().right:0}))
   }));
   assert(geometry.header<=56&&geometry.tabs<=40,JSON.stringify(geometry));
   assert(!geometry.font.includes('Times'),geometry.font);
   assert(geometry.items.every(x=>x.width<=160&&x.gap>=0&&x.gap<=12),JSON.stringify(geometry));
   report.shell_geometry=geometry;
   check('real_ui.navigation_spacing','Compact header/tabs, bounded title-close gaps and explicit system font verified in actual rendered layout');
   for(const theme of ['light','dark','ocean','mint']){
    await page.evaluate(value=>localStorage.setItem('theme',JSON.stringify({version:1,value})),theme);
    await page.reload();await page.waitForLoadState('networkidle');
    let release;const gate=new Promise(resolve=>{release=resolve});
    const matcher='**/api/accounts?**';const handler=async route=>{await gate;await route.continue()};
    await page.route(matcher,handler);
    try{
     await page.goto(base+'/#/accounts');
     const mask=page.locator('.el-loading-mask').first();await expect(mask).toBeVisible();
     const visual=await mask.evaluate(el=>{const circle=el.querySelector('.circular');const r=el.getBoundingClientRect(),c=circle?.getBoundingClientRect();return {background:getComputedStyle(el).backgroundColor,backdrop:getComputedStyle(el).backdropFilter,center:c?{dx:Math.abs((c.left+c.right-r.left-r.right)/2),dy:Math.abs((c.top+c.bottom-r.top-r.bottom)/2),width:parseFloat(getComputedStyle(circle).width),height:parseFloat(getComputedStyle(circle).height)}:null}});
     assert.equal(visual.background,'rgba(0, 0, 0, 0)');assert.equal(visual.backdrop,'none');
     assert(visual.center&&visual.center.dx<=2&&visual.center.dy<=2&&visual.center.width<=44&&visual.center.height<=44,JSON.stringify(visual));
     const arc=await mask.locator('.path').evaluate(el=>({dash:getComputedStyle(el).strokeDasharray,animation:getComputedStyle(el).animationName}));
     assert.equal(arc.animation,'none');assert(parseFloat(arc.dash)>=60,JSON.stringify(arc));
     report.loading_visuals??=[];report.loading_visuals.push({theme,...visual,reduced_motion_arc:arc});
     await page.screenshot({path:path.join(output,'loading-'+theme+'.png'),fullPage:true});
     if(theme==='light'){
      await page.emulateMedia({reducedMotion:'no-preference'});
      const animation=await mask.locator('.circular').evaluate(el=>getComputedStyle(el).animationName);
      assert.notEqual(animation,'none');
      await page.screenshot({path:path.join(output,'loading-animated.png'),fullPage:true});
      await page.emulateMedia({reducedMotion:'reduce'});
     }
    }finally{release();await page.unrouteAll({behavior:'wait'})}
    await page.waitForLoadState('networkidle');
    await page.goto(base+'/#/');await expect(page.locator('.page-header')).toContainText('运行总览');await settled(page);
    await page.screenshot({path:path.join(output,'overview-'+theme+'.png'),fullPage:true});
   }
   check('real_ui.loading','Four themes: pending real account request displays a centered circle over transparent background with no backdrop tint');
   await page.evaluate(()=>localStorage.setItem('theme',JSON.stringify({version:1,value:'light'})));await page.reload();
   await expect(page.locator('.page-header')).toContainText('运行总览');await settled(page);
  }
  await page.goto(base+'/#/accounts?search=SYNTHETIC_NO_MATCH');
  await expect(page.locator('.page-header')).toContainText('账号管理');await settled(page);
  await expect(page.locator('.el-table__empty-block')).toBeVisible();
  await page.goto(base+'/#/accounts');
  await expect(page.locator('.page-header')).toContainText('账号管理');await settled(page);
  await expect(page.locator('.el-table__body-wrapper tbody tr').first()).toBeVisible();
  check('real_ui.filters','Server search no-match and cleared-filter list verified');
  await page.setViewportSize({width:390,height:844});
  await page.screenshot({path:path.join(output,'accounts-mobile.png'),fullPage:true});
  assert(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth+1));
  assert.equal(errors.length,0,errors.join('; '));
  check('real_ui.mobile','390px account page has no document overflow; no uncaught JS errors');
  await page.evaluate(async()=>{const r=await fetch('/api/session',{method:'DELETE'});if(!r.ok)throw Error('logout failed')});
  await page.reload();await expect(page.getByRole('textbox',{name:'管理密钥'})).toBeVisible();
  check('real_ui.logout','Real session revoked and protected page redirects after reload');
 }catch(error){report.checks.push({id:'real_ui.failure',status:'fail',detail:String(error.message).replaceAll(key,'[REDACTED]')})}
 finally{if(browser)await browser.close();for(const request of upstreamRequests)request.destroy();server.closeAllConnections();await new Promise(resolve=>server.close(resolve))}
 report.status=report.checks.every(c=>c.status==='pass')?'pass':'fail';
 await fs.writeFile(path.join(output,'report.json'),JSON.stringify(report,null,2));
 console.log(JSON.stringify({status:report.status,checks:report.checks}));
 process.exitCode=report.status==='pass'?0:1;
})().catch(error=>{console.error(error.message);process.exitCode=1});
