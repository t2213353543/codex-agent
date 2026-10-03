import assert from 'node:assert/strict'
import { expect } from 'playwright/test'
import { readFile } from 'node:fs/promises'
import { captureLogThemes } from './log-visual-fixtures.mjs'

export async function testExternalImportLeases(page, { screenshotDirectory } = {}) {
  assert.ok(['localhost','127.0.0.1'].includes(new URL(page.url()).hostname))
  const now=Math.floor(Date.now()/1000),future=now+86400,past=now-3600
  const futureRow={name:'future-rental',platform:'openai',type:'oauth',expires_at:future,credentials:{tokens:{access_token:'synthetic-future-access',refresh_token:'synthetic-future-refresh',expires_at:future+86400}},extra:{note:'ignored setting'}}
  const invalidRow={name:'invalid-rental',platform:'openai',type:'oauth',expires_at:'not-a-date',credentials:{access_token:'synthetic-invalid-access'}}
  const expiredRow={name:'expired-rental',platform:'openai',type:'oauth',expires_at:new Date(past*1000).toISOString(),auto_pause_on_expired:false,credentials:{access_token:'synthetic-expired-access',expires_at:future}}
  const tokenRow={name:'token-only',tokens:{access_token:'synthetic-plain-access',expires_at:future}}
  const calls={preview:[],imports:[],restore:[],receipts:[]},receipts=new Map(),assertions=[]
  let malformed=false,restoreConflict=false,unknownWrite=false,release,holdRestore=false,omitReceipt=false,duplicateName=''
  function external(row,index){if(row.platform!=='openai'||row.type!=='oauth'||!row.credentials||!('expires_at'in row||'auto_pause_on_expired'in row))return null;if(row.expires_at==='not-a-date')return {invalid:true};return {account_index:index,source:'sub2api_outer_account',expires_at:typeof row.expires_at==='string'?Math.floor(Date.parse(row.expires_at)/1000):row.expires_at||0,auto_disable:typeof row.auto_pause_on_expired==='boolean'?row.auto_pause_on_expired:true,restore_on_extend:false,expiry_format:typeof row.expires_at==='string'?'rfc3339':row.expires_at?'unix_seconds':'unlimited',auto_disable_source:typeof row.auto_pause_on_expired==='boolean'?'explicit':'sub2api_default'}}
  const handler=async route=>{
    const request=route.request(),url=new URL(request.url()),path=url.pathname,body=request.method()==='POST'?request.postDataJSON():{}
    if(path==='/api/accounts/import/preview'||path==='/api/accounts'){
      const dry=path.endsWith('/preview'),raw=JSON.parse(body.raw),rows=Array.isArray(raw)?raw:[raw];(dry?calls.preview:calls.imports).push(body)
      const metas=rows.map((row,index)=>external(row,index+1))
      const result=rows.map((row,index)=>({index:index+1,name:row.name,id:`target-${row.name}`,version:2,status:body.include_external_leases&&metas[index]?.invalid?'failed':row.name===duplicateName?'duplicate':'added',...(body.include_external_leases&&metas[index]?.invalid?{code:'invalid_external_lease',error:'外部租期字段无效'}:{}),warnings:body.include_external_leases&&metas[index]&&!metas[index].invalid?['external_lease_pending_confirmation','sub2api_settings_not_imported']:['account_expiry_not_imported']}))
      const response={dry_run:dry,results:result,imported:dry?0:result.filter(row=>row.status==='added').length}
      if(body.include_external_leases){response.external_leases=metas.filter(meta=>meta&&!meta.invalid);response.lease_restore_source='sub2api_outer_account';if(response.external_leases.length){const items=response.external_leases.map(meta=>({...meta,name:rows[meta.account_index-1].name,status:dry?'pending_import':'pending',...(!dry?{id:result[meta.account_index-1].id,account_version:2,lease_version:0}:{}),expired:meta.expires_at>0&&meta.expires_at<=now}));response.lease_restore={dry_run:dry,items,...(!dry?{receipt_id:`receipt-${rows[0].name}`,expires_at:now+3600}:{})};if(!dry){receipts.set(response.lease_restore.receipt_id,structuredClone(response.lease_restore));if(omitReceipt)delete response.lease_restore}if(malformed&&dry)response.external_leases[0].account_index=99}}
      return route.fulfill({json:response})
    }
    if(path.startsWith('/api/accounts/import-leases/receipts/')){const id=decodeURIComponent(path.split('/').at(-1));calls.receipts.push(id);return route.fulfill({json:receipts.get(id)})}
    if(path==='/api/accounts/import-leases'){
      calls.restore.push(body)
      if(holdRestore)await new Promise(resolve=>{release=resolve})
      if(restoreConflict){restoreConflict=false;return route.fulfill({status:409,json:{error:{message:'version changed'}}})}
      if(unknownWrite&&!body.dry_run)return route.fulfill({status:503,json:{error:{message:'保存结果未确认'}}})
      const receipt=receipts.get(body.receipt_id),items=receipt.items.map(item=>({...item,status:body.dry_run?'pending':'applied'})),response={...receipt,dry_run:body.dry_run,items};receipts.set(body.receipt_id,response);return route.fulfill({json:response})
    }
    return route.fallback()
  }
  for(const pattern of ['**/api/accounts','**/api/accounts/import/preview','**/api/accounts/import-leases**'])await page.route(pattern,handler)
  try{
    await page.setViewportSize({width:1440,height:1100});await page.goto(new URL('/#/accounts',page.url()).href);await page.getByRole('button',{name:'导入账号',exact:true}).click()
    const dialog=page.getByRole('dialog',{name:'导入账号 · 先预览再提交',exact:true}),option=dialog.getByRole('checkbox',{name:'识别 Sub2API 外部管理租期，导入账号后单独确认',exact:true})
    await expect(option).not.toBeChecked()
    await dialog.getByLabel('选择账号导入文件',{exact:true}).setInputFiles([{name:'bundle.json',mimeType:'application/json',buffer:Buffer.from(JSON.stringify({type:'sub2api-data',version:1,accounts:[futureRow,invalidRow]},null,2))},{name:'mixed.jsonl',mimeType:'application/json',buffer:Buffer.from('{broken}\n'+JSON.stringify(expiredRow)+'\n'+JSON.stringify(tokenRow))}])
    await dialog.getByRole('button',{name:'生成预览',exact:true}).click();await expect(dialog.locator('.preview-table .el-table__row')).toHaveCount(5)
    assert.equal(calls.preview.at(-1).include_external_leases,false);assert.equal(calls.restore.length,0)
    const raw=JSON.parse(calls.preview.at(-1).raw);assert.equal(raw.length,4);assert.deepEqual(raw[0],futureRow);assert.equal(raw[0].credentials.tokens.expires_at,future+86400)
    await expect(dialog.locator('.external-lease-preview')).toHaveCount(0)
    await option.locator('xpath=ancestor::label[1]').click();await expect(dialog.locator('.preview-table')).toHaveCount(0);await expect(dialog.getByRole('button',{name:/确认导入/})).toBeDisabled()
    await dialog.getByRole('button',{name:'生成预览',exact:true}).click();await expect(dialog.locator('.external-lease-preview article')).toHaveCount(2)
    await expect(dialog.locator('.external-lease-preview')).toContainText('Sub2API 默认开启');await expect(dialog.locator('.external-lease-preview')).toContainText('自动停用：否');await expect(dialog.locator('.external-lease-preview')).toContainText('mixed.jsonl · 第 2 行')
    await expect(dialog.locator('.preview-table')).toContainText('外部管理租期无效')
    await expect(dialog.locator('.preview-table')).toContainText('原系统的扩展配置未自动导入')
    await captureLogThemes(page,dialog.locator('.external-lease-preview'),'external-lease-preview',screenshotDirectory)
    assertions.push('默认不识别外部租期；多文件原始wrapper和两种expiry完整发送，勾选后旧预览失效，来源/默认与显式false/行号影响独立展示')

    const tokenTableRow=dialog.locator('.preview-table .el-table__row').filter({hasText:'token-only'})
    await tokenTableRow.getByRole('combobox').press('Enter');await page.getByRole('option',{name:'跳过',exact:true}).click()
    await dialog.getByRole('button',{name:'确认导入 2 项',exact:true}).click();await expect(dialog.locator('.external-lease-result')).toHaveCount(2)
    assert.deepEqual(calls.imports.map(body=>JSON.parse(body.raw).name),['future-rental','expired-rental'])
    for(const body of calls.imports)assert.equal(body.include_external_leases,true)
    assert.deepEqual(JSON.parse(calls.imports[0].raw),futureRow);assert.deepEqual(JSON.parse(calls.imports[1].raw),expiredRow)
    assert.equal(calls.restore.length,0)
    const futurePanel=dialog.locator('.external-lease-result').filter({has:page.getByRole('heading',{name:/^future-rental/})}),expiredPanel=dialog.locator('.external-lease-result').filter({has:page.getByRole('heading',{name:/^expired-rental/})})
    await expect(futurePanel).toContainText('target-future-rental');await expect(expiredPanel).toContainText('target-expired-rental')
    await expect(futurePanel.getByRole('button',{name:'确认恢复租期',exact:true})).toBeDisabled()
    await futurePanel.getByRole('checkbox',{name:'我确认将备份租期应用到以上固定 ID 的账号',exact:true}).locator('xpath=ancestor::label[1]').click();await futurePanel.getByRole('button',{name:'预览租期恢复',exact:true}).click();await expect(futurePanel).toContainText('租期预览完成')
    holdRestore=true;await futurePanel.getByRole('button',{name:'确认恢复租期',exact:true}).click();await expect.poll(()=>typeof release).toBe('function');await expect(dialog.getByRole('button',{name:'关闭',exact:true})).toBeDisabled();await expect(expiredPanel.getByRole('button',{name:'预览租期恢复',exact:true})).toBeDisabled();holdRestore=false;release();release=undefined
    await expect(futurePanel).toContainText('租期已恢复');assert.deepEqual(calls.restore[0].items.map(row=>row.account_index),[1]);assert.equal(calls.restore[1].receipt_id,'receipt-future-rental')
    restoreConflict=true;await expiredPanel.getByRole('button',{name:'预览租期恢复',exact:true}).click();await expect(expiredPanel).toContainText('回执或目标版本已变化')
    await expiredPanel.getByRole('button',{name:'读取租期恢复回执',exact:true}).click();await expiredPanel.getByRole('button',{name:'预览租期恢复',exact:true}).click();await expiredPanel.getByRole('checkbox',{name:'我确认将备份租期应用到以上固定 ID 的账号',exact:true}).locator('xpath=ancestor::label[1]').click();await expect(expiredPanel.getByRole('checkbox',{name:/可能立即停用账号/})).toHaveCount(0)
    unknownWrite=true;await expiredPanel.getByRole('button',{name:'确认恢复租期',exact:true}).click();await expect(expiredPanel).toContainText('部分租期结果尚未确认');await expect(expiredPanel.getByRole('button',{name:'确认恢复租期',exact:true})).toBeDisabled();unknownWrite=false
    assert.equal(calls.restore.at(-1).receipt_id,'receipt-expired-rental');assert.equal(calls.restore.at(-1).items[0].account_index,1);assert.equal(calls.restore.at(-1).items[0].id,'target-expired-rental')
    await captureLogThemes(page,expiredPanel,'external-lease-independent-receipt',screenshotDirectory)
    assertions.push('逐项选择只提交两账号，导入不自动恢复；两份receipt均保留本批index1和固定不同ID，独立确认/冲突/未知暂停且忙时锁定其它操作')

    const downloadPromise=page.waitForEvent('download');await dialog.getByRole('button',{name:'下载脱敏结果',exact:true}).click();const download=await downloadPromise,content=await readFile(await download.path(),'utf8');for(const secret of ['synthetic-future-access','synthetic-future-refresh','synthetic-expired-access'])assert.equal(content.includes(secret),false)
    await dialog.getByRole('button',{name:'关闭',exact:true}).click();await page.getByRole('button',{name:'导入账号',exact:true}).click();await expect(option).not.toBeChecked();await expect(dialog.locator('.external-lease-result')).toHaveCount(0)
    const expiredAuto={...futureRow,name:'expired-auto',expires_at:past,auto_pause_on_expired:true}
    await dialog.getByLabel('选择账号导入文件',{exact:true}).setInputFiles({name:'expired-auto.json',mimeType:'application/json',buffer:Buffer.from(JSON.stringify(expiredAuto))});await option.locator('xpath=ancestor::label[1]').click();malformed=true;await dialog.getByRole('button',{name:'生成预览',exact:true}).click();await expect(dialog).toContainText('外部租期与本次导入行不匹配');await expect(dialog.getByRole('button',{name:/确认导入/})).toBeDisabled();malformed=false
    await dialog.getByRole('button',{name:'生成预览',exact:true}).click();await expect(dialog.locator('.external-lease-preview')).toContainText('可能立即停用账号');await dialog.getByRole('button',{name:'确认导入 1 项',exact:true}).click()
    const autoPanel=dialog.locator('.external-lease-result');await autoPanel.getByRole('button',{name:'预览租期恢复',exact:true}).click();await autoPanel.getByRole('checkbox',{name:'我确认将备份租期应用到以上固定 ID 的账号',exact:true}).locator('xpath=ancestor::label[1]').click();await expect(autoPanel.getByRole('button',{name:'确认恢复租期',exact:true})).toBeDisabled();await autoPanel.getByRole('checkbox',{name:'我确认已过期且开启自动停用的租期可能立即停用账号',exact:true}).locator('xpath=ancestor::label[1]').click();await expect(autoPanel.getByRole('button',{name:'确认恢复租期',exact:true})).toBeEnabled()
    const beforeApply=calls.restore.filter(body=>!body.dry_run).length
    await page.setViewportSize({width:320,height:844});assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth),true);if(screenshotDirectory)await autoPanel.locator('.lease-confirmations').screenshot({path:`${screenshotDirectory}/external-lease-expired-confirm-320.png`})
    await dialog.getByRole('button',{name:'关闭',exact:true}).click();assert.equal(calls.restore.filter(body=>!body.dry_run).length,beforeApply)
    assertions.push('脱敏报告不含凭据、重开恢复默认关闭；错行预览阻止提交，已过期自动停用必须额外确认，关闭不应用且320布局可用')

    await page.setViewportSize({width:1440,height:1100});await page.getByRole('button',{name:'导入账号',exact:true}).click();await dialog.getByRole('button',{name:'粘贴内容',exact:true}).click();await dialog.getByRole('textbox',{name:'账号导入内容',exact:true}).fill(JSON.stringify(futureRow));await option.locator('xpath=ancestor::label[1]').click();await dialog.getByRole('button',{name:'生成预览',exact:true}).click();omitReceipt=true;await dialog.getByRole('button',{name:'确认导入 1 项',exact:true}).click();await expect(dialog.locator('.result-table')).toContainText('成功');await expect(dialog.locator('.external-lease-result')).toContainText('回执不完整');await expect(dialog.locator('.external-lease-result .lease-restore')).toHaveCount(0);omitReceipt=false
    assertions.push('粘贴源同样保留外层；账号成功但租期回执缺失单独报错，不误判账号失败或提供无确认租期按钮')
    await dialog.getByRole('button',{name:'关闭',exact:true}).click()
    duplicateName='future-rental';await page.getByRole('button',{name:'导入账号',exact:true}).click();await dialog.getByLabel('选择账号导入文件',{exact:true}).setInputFiles({name:'existing.json',mimeType:'application/json',buffer:Buffer.from(JSON.stringify(futureRow))});await option.locator('xpath=ancestor::label[1]').click();await dialog.getByRole('button',{name:'生成预览',exact:true}).click()
    await expect(dialog.getByRole('button',{name:'确认导入 0 项',exact:true})).toBeDisabled();await expect(dialog.locator('.preview-table')).toContainText('可明确保留账号')
    await dialog.locator('.preview-table').getByRole('combobox').press('Enter');await page.getByRole('option',{name:'保留账号，仅准备租期确认',exact:true}).click();await dialog.getByRole('button',{name:'确认导入 1 项',exact:true}).click();await expect(dialog.locator('.external-lease-result')).toContainText('target-future-rental')
    await expect(dialog.locator('.result-table')).toContainText('原账号和凭据已保留');assert.equal(calls.imports.at(-1).replace,false)
    assertions.push('已有同凭据账号默认跳过；显式租期准备动作不替换账号，仍生成固定原ID租期回执供独立确认')
    await dialog.getByRole('button',{name:'关闭',exact:true}).click();return {assertions,requests:{preview_count:calls.preview.length,import_count:calls.imports.length,lease_restore_count:calls.restore.length},sensitive_payloads_logged:false}
  }finally{release?.();for(const pattern of ['**/api/accounts','**/api/accounts/import/preview','**/api/accounts/import-leases**'])await page.unroute(pattern,handler)}
}
