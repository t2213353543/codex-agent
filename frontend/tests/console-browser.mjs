import assert from 'node:assert/strict'
import { createServer } from 'node:http'
import { readFile, writeFile, mkdir } from 'node:fs/promises'
import { fileURLToPath } from 'node:url'
import path from 'node:path'
import { chromium, expect } from 'playwright/test'
import { installMockApi, mockKey } from './mock-api.mjs'

const root=fileURLToPath(new URL('../dist/',import.meta.url))
const output=process.env.CODEX_UI_OUTPUT ? path.resolve(process.env.CODEX_UI_OUTPUT,'console-browser') : fileURLToPath(new URL('../../.local/console-ui-20260924/console-browser/',import.meta.url))
await mkdir(output,{recursive:true})
const report={passed:false,checks:[],screenshots:[],errors:[],escaped:[]}
const server=createServer(async(req,res)=>{
 const pathname=new URL(req.url,'http://localhost').pathname
 if(pathname.startsWith('/api/')){report.escaped.push(pathname);res.writeHead(500).end();return}
 try{const file=path.resolve(root,pathname==='/'?'index.html':decodeURIComponent(pathname).slice(1));assert(file.startsWith(root));const body=await readFile(file);res.writeHead(200,{'Content-Type':({'.html':'text/html','.js':'text/javascript','.css':'text/css','.woff2':'font/woff2','.svg':'image/svg+xml'})[path.extname(file)]||'application/octet-stream'}).end(body)}catch{res.writeHead(404).end()}
})
await new Promise(resolve=>server.listen(0,'127.0.0.1',resolve))
const base=`http://127.0.0.1:${server.address().port}`
let browser
try{
 browser=await chromium.launch({headless:true,channel:'chrome'})
 const context=await browser.newContext({viewport:{width:1440,height:1000},reducedMotion:'reduce'})
 const state=await installMockApi(context),page=await context.newPage()
 page.setDefaultTimeout(10000)
 page.on('pageerror',e=>report.errors.push(e.message))
 await page.goto(base+'/#/groups')
 await expect(page.getByRole('textbox',{name:'管理密钥',exact:true})).toBeVisible()
 await expect(page.locator('html')).toHaveAttribute('data-theme','dark')
 await page.getByRole('textbox',{name:'管理密钥',exact:true}).fill('incorrect-demo-key')
 await page.locator('.login-submit').click()
 await expect(page.locator('.form-error')).toBeVisible()
 await page.getByRole('textbox',{name:'管理密钥',exact:true}).fill(mockKey)
 await page.locator('.login-submit').click()
 await expect(page.locator('.page-header-title')).toHaveText('分组管理')
 await expect(page.locator('.console-version').first()).toHaveText('v0.2.0-mock')
 await page.reload()
 await expect(page.locator('.page-header-title')).toHaveText('分组管理')
 report.checks.push('default Orbit, invalid login feedback, real login flow, redirect restoration, version, session reload')
 for(const theme of ['light','dark']){
  await page.evaluate(theme=>localStorage.setItem('theme',JSON.stringify({version:1,value:theme})),theme)
  await page.reload()
  await expect(page.locator('html')).toHaveAttribute('data-theme',theme)
  await expect(page.locator('.console-rail, .console-connect-card')).toHaveCount(0)
  assert.equal(await page.locator('a[href="#/connection"]').count(),0)
  for(const width of [1440,768,390,320]){
   await page.setViewportSize({width,height:1000})
   await page.locator('.content').evaluate(el=>el.scrollTo(0,0))
   await page.evaluate(()=>document.fonts.ready)
   const geometry=await page.evaluate(()=>({doc:document.documentElement.scrollWidth-innerWidth,content:document.querySelector('.content').scrollWidth-document.querySelector('.content').clientWidth}))
   assert(geometry.doc<=1&&geometry.content<=1,`${theme}/${width}: ${JSON.stringify(geometry)}`)
   const shot=`${theme}-groups-${width}.png`;await page.screenshot({path:path.join(output,shot),animations:'disabled'});report.screenshots.push(shot)
   const trigger=page.getByRole('button',{name:'创建分组',exact:true})
   await trigger.click();const dialog=page.getByRole('dialog',{name:'创建分组',exact:true});await expect(dialog).toBeVisible()
   const dialogSurface=dialog.locator('.el-dialog')
   const box=await dialogSurface.boundingBox();assert(box.x>=-1&&box.x+box.width<=width+1)
   assert(Math.abs(box.x+box.width/2-width/2)<=2,'Dialog must be centered horizontally')
   await expect.poll(async()=>{ const settled=await dialogSurface.boundingBox(); return Math.abs(settled.y+settled.height/2-500) },{message:'Dialog must settle at the vertical center'}).toBeLessThanOrEqual(3)
   await page.keyboard.press('Control+k');await expect(page.getByRole('dialog',{name:'快速访问',exact:true})).toBeHidden();await expect(dialog).toBeVisible()
   const overlay=await dialogSurface.evaluate(el=>({bg:getComputedStyle(el).backgroundColor,text:getComputedStyle(el).color}));assert.notEqual(overlay.bg,'rgba(0, 0, 0, 0)');assert.notEqual(overlay.bg,overlay.text)
   const dialogShot=`${theme}-dialog-${width}.png`;await page.screenshot({path:path.join(output,dialogShot),animations:'disabled'});report.screenshots.push(dialogShot)
   await dialog.getByRole('button',{name:'取消',exact:true}).click();await expect(trigger).toBeFocused()
   if(width<768){await page.getByRole('button',{name:'打开导航',exact:true}).click();const nav=page.locator('.console-mobile-navigation');await expect(nav).toBeVisible();await nav.getByRole('link',{name:'分组管理',exact:true}).click();await expect(nav).toBeHidden()}
   report.checks.push(`${theme}_${width}_layout_dialog_focus_navigation`)
  }
 }
 await page.setViewportSize({width:1440,height:1000})
 await page.getByRole('button',{name:'切换留白主题',exact:true}).click()
 await expect(page.locator('html')).toHaveAttribute('data-theme','light')
 await page.reload();await expect(page.locator('html')).toHaveAttribute('data-theme','light')
 await page.getByRole('button',{name:'切换夜航主题',exact:true}).click()
 await expect(page.locator('html')).toHaveAttribute('data-theme','dark')
 await page.getByRole('button',{name:'收起侧栏',exact:true}).first().click()
 await page.reload();await expect(page.locator('.console-shell')).toHaveClass(/console-is-collapsed/);await expect(page.locator('.console-sidebar').getByRole('link',{name:'账号管理',exact:true})).toBeVisible()
 await page.getByRole('button',{name:'展开侧栏',exact:true}).first().click()
 await page.keyboard.press('Control+k')
 await expect(page.getByRole('dialog',{name:'快速访问',exact:true})).toBeVisible()
 await page.getByRole('textbox',{name:'搜索管理功能'}).fill('分组')
 await page.getByRole('textbox',{name:'搜索管理功能'}).press('Enter')
 await expect(page.getByRole('dialog',{name:'快速访问',exact:true})).toBeHidden()
 await page.getByRole('button',{name:'刷新当前页面',exact:true}).click();await expect(page.locator('.page-header-title')).toHaveText('分组管理')
 report.checks.push('theme toggle and persistence, collapse persistence, Ctrl+K navigation, refresh retains route')
 await page.getByRole('button',{name:'管理员菜单'}).click();await page.getByRole('menuitem',{name:'退出登录',exact:true}).click()
 await page.locator('.el-message-box').getByRole('button',{name:'取消',exact:true}).click();assert.equal(state.authenticated,true)
 await page.getByRole('button',{name:'管理员菜单'}).click();await page.getByRole('menuitem',{name:'退出登录',exact:true}).click()
 await page.locator('.el-message-box').getByRole('button',{name:'退出',exact:true}).click();await expect(page.getByRole('textbox',{name:'管理密钥',exact:true})).toBeVisible();assert.equal(state.authenticated,false)
 for(const theme of ['light','dark']){await page.getByRole('button',{name:`切换${theme==='light'?'留白':'夜航'}主题`,exact:true}).click();for(const width of [1440,390]){await page.setViewportSize({width,height:1000});const name=`${theme}-login-${width}.png`;await page.screenshot({path:path.join(output,name),animations:'disabled'});report.screenshots.push(name);assert(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth+1))}}
 report.checks.push('logout cancellation, successful logout, login responsive themes')
 assert.deepEqual(state.unexpected,[]);assert.deepEqual(report.errors,[]);assert.deepEqual(report.escaped,[])
 report.passed=true
}catch(error){report.failure=error.stack;console.error(error)}
finally{await browser?.close();await new Promise(resolve=>server.close(resolve));await writeFile(path.join(output,'report.json'),JSON.stringify(report,null,2));console.log(JSON.stringify({passed:report.passed,checks:report.checks.length,screenshots:report.screenshots.length,report:path.join(output,'report.json')}))}
process.exitCode=report.passed?0:1
