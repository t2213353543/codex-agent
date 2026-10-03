/* Read-only production UI verification. Export no account data or authenticated screenshots. */
const fs=require('node:fs/promises');
const path=require('node:path');
const assert=require('node:assert/strict');
const {chromium,expect}=require('../../frontend/node_modules/playwright/test');
(async()=>{
 const root=path.resolve(__dirname,'../..');
 const dir=path.resolve(process.env.INTEGRATION_PUBLIC_UI_OUTPUT||path.join(root,'.local/ui/production'));await fs.mkdir(dir,{recursive:true});
 const keyfile=path.join(dir,'admin-key');const key=(await fs.readFile(keyfile,'utf8')).trim();
 const base='http://43.225.196.170:18879';
 const proxy=process.env.CODEX_AGENT_SSH_PROXY;
 if(proxy)assert(['127.0.0.1','localhost'].includes(new URL(proxy).hostname));
 let browser;const report={address:base,started_at:new Date().toISOString(),checks:[],requests:[]};
 const check=(id,detail)=>report.checks.push({id,status:'pass',detail});
 try{
  browser=await chromium.launch({headless:true,channel:'chrome',...(proxy?{proxy:{server:proxy}}:{})});
  const context=await browser.newContext({viewport:{width:1440,height:960},reducedMotion:'reduce'});
  const page=await context.newPage();page.setDefaultTimeout(25000);const errors=[];
  page.on('pageerror',e=>errors.push(e.message));
  page.on('response',r=>report.requests.push({path:new URL(r.url()).pathname,status:r.status()}));
  const settle=async()=>{
   await expect(page.locator('.el-loading-mask:visible')).toHaveCount(0,{timeout:25000});
   const refresh=page.getByRole('button',{name:'刷新运行概览'});
   if(await refresh.count())await expect(refresh).not.toHaveClass(/is-loading/,{timeout:25000});
   await page.evaluate(()=>document.fonts.ready);
   await page.waitForLoadState('networkidle');
  };
  await page.goto(base+'/#/accounts');await expect(page.getByRole('textbox',{name:'管理密钥'})).toBeVisible();
  await page.screenshot({path:path.join(dir,'public-login.png'),fullPage:true});
  await page.getByRole('textbox',{name:'管理密钥'}).fill(key);await page.getByRole('button',{name:'登 录',exact:true}).click();
  await expect(page.locator('.page-header')).toContainText('账号管理');await settle();
  assert((await context.cookies()).some(c=>c.name==='codex_admin_session'&&c.httpOnly));
  assert(!(await page.evaluate(()=>JSON.stringify(localStorage))).includes(key));
  if(await page.evaluate(()=>[...document.fonts].some(f=>/Noto|Codex Admin Sans/.test(f.family)))){
   const cdp=await context.newCDPSession(page);await cdp.send('DOM.enable');await cdp.send('CSS.enable');
   const {root:dom}=await cdp.send('DOM.getDocument');
   const {nodeId}=await cdp.send('DOM.querySelector',{nodeId:dom.nodeId,selector:'.page-header-title'});
   const {fonts}=await cdp.send('CSS.getPlatformFontsForNode',{nodeId});
   assert(fonts.some(f=>f.isCustomFont&&/Noto|Codex Admin Sans/.test(f.familyName)&&f.glyphCount>0));
   report.rendered_title_fonts=fonts;await cdp.detach();
   check('public.typography','Production Chinese title uses downloaded self-hosted Noto font');
  }
  const headers=await page.locator('.el-table__header-wrapper th').allTextContents();
  for(const label of ['账号名称','状态','并发','优先级','权重','分组','代理','令牌到期','用量窗口'])assert(headers.some(v=>v.includes(label)),label);
  check('public.login_restored_fields','Protected route, real HttpOnly login and restored default account headers verified; account values not exported');
  for(const [route,title] of [['/','运行总览'],['/logs','使用记录'],['/proxies','IP 代理池'],['/groups','分组管理'],['/settings','管理设置']]){
   await page.goto(base+'/#'+route);await expect(page.locator('.page-header')).toContainText(title);await settle();
   if(route==='/'){
    for(const metric of ['accounts','requests_today','tokens_today','tokens_total','tokens_retained','rpm','tpm'])await expect(page.locator('[data-metric="'+metric+'"]').first()).toBeVisible();
   }
  }
  check('public.management_cards','Production overview restored metrics and management pages render with no API mutations');
  const geometry=await page.evaluate(()=>({header:document.querySelector('.topbar').getBoundingClientRect().height,tabs:document.querySelector('.tags-view').getBoundingClientRect().height}));
  assert.equal(geometry.header,56);assert.equal(geometry.tabs,36);report.shell_geometry=geometry;
  await page.setViewportSize({width:390,height:844});await page.goto(base+'/#/accounts');
  await expect(page.locator('.page-header')).toContainText('账号管理');await settle();
  assert(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth+1));assert.equal(errors.length,0);
  check('public.layout','56px header, 36px tabs, mobile 390px no document overflow or uncaught script errors');
  const failures=report.requests.filter(r=>r.status>=400&&!['/api/session'].includes(r.path));
  assert.equal(failures.length,0,JSON.stringify(failures));
  await page.evaluate(async()=>{const r=await fetch('/api/session',{method:'DELETE'});if(!r.ok)throw Error('logout failed')});
  await page.reload();await expect(page.getByRole('textbox',{name:'管理密钥'})).toBeVisible();
  check('public.logout','Session revoked; reload returns to login');report.status='pass';
 }catch(error){report.status='fail';report.error=String(error.message).replaceAll(key,'[REDACTED]')}
 finally{
  if(browser)await browser.close();await fs.unlink(keyfile);
  await fs.writeFile(path.join(dir,'browser-report.json'),JSON.stringify(report,null,2));
 }
 console.log(JSON.stringify({status:report.status,checks:report.checks,error:report.error}));
 process.exitCode=report.status==='pass'?0:1;
})().catch(error=>{console.error(error.message);process.exitCode=1});
