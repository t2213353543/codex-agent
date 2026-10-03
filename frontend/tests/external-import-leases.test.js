import test from 'node:test'
import assert from 'node:assert/strict'
import { parseImportDocuments, importRequestSource, importReport } from '../src/utils/importAccounts.js'
import { externalLeasePreviews, externalLeaseCommitReceipt, externalLeaseSummary, canPrepareDuplicateLease } from '../src/utils/externalImportLeases.js'

const metadata = (account_index=1) => ({ account_index,source:'sub2api_outer_account',expires_at:1900000000,auto_disable:false,restore_on_extend:false,expiry_format:'unix_seconds',auto_disable_source:'explicit' })
const preview = (index=1) => ({dry_run:true,external_leases:[metadata(index)],lease_restore_source:'sub2api_outer_account',lease_restore:{dry_run:true,items:[{account_index:index,status:'pending_import',expires_at:1900000000,auto_disable:false,restore_on_extend:false}]}})
const commit = id => ({dry_run:false,external_leases:[metadata()],lease_restore_source:'sub2api_outer_account',lease_restore:{dry_run:false,receipt_id:'receipt-one',expires_at:1900003600,items:[{account_index:1,status:'pending',id,account_version:2,lease_version:0,expires_at:1900000000,auto_disable:false,restore_on_extend:false}]}})
test('raw source preserves Sub2API lease wrapper independently of nested credential expiry and correct document row', () => {
  const outer={name:'external',platform:'openai',type:'oauth',expires_at:'2030-03-01T00:00:00Z',auto_pause_on_expired:false,credentials:{tokens:{access_token:'SYNTHETIC_PRIVATE_TOKEN',expires_at:1800000000}}}
  const rows=parseImportDocuments([{name:'first.jsonl',text:'{"name":"plain","access_token":"plain-token"}\n\n{broken}'},{name:'external.json',text:JSON.stringify({type:'sub2api-data',version:1,accounts:[outer]},null,2)}])
  assert.equal(rows.length,3);assert.equal(rows[1].kind,'failed');assert.equal(rows[2].sourceIndex,2)
  assert.equal(rows[2].credentials.expires_at,1800000000)
  assert.deepEqual(importRequestSource(rows[2]),outer)
  assert.deepEqual(importRequestSource(rows[2],'confirmed-name'),{...outer,name:'confirmed-name'})
  assert.equal(rows[2].rawSource.name,'external')
  assert.equal(rows[2].sourceFile,'external.json')
  assert.ok(!JSON.stringify(importReport(rows)).includes('SYNTHETIC_PRIVATE_TOKEN'))
})
test('server lease preview indices map only into valid submitted row sequence and preserve false/default sources', () => {
  assert.deepEqual([...externalLeasePreviews(preview(2),2,true).keys()],[2])
  assert.equal(externalLeasePreviews(preview(),1,true).get(1).auto_disable,false)
  const defaults=preview();defaults.external_leases[0].auto_disable_source='sub2api_default';defaults.external_leases[0].auto_disable=true;defaults.lease_restore.items[0].auto_disable=true
  assert.equal(externalLeasePreviews(defaults,1,true).get(1).auto_disable_source,'sub2api_default')
  assert.equal(externalLeasePreviews({results:[]},1,false).size,0)
  assert.equal(externalLeasePreviews({dry_run:true},1,true).size,0)
  assert.throws(()=>externalLeasePreviews(preview(2),1,true),/不匹配/)
  const duplicated=preview();duplicated.external_leases.push(metadata());assert.throws(()=>externalLeasePreviews(duplicated,2,true),/不匹配/)
})
test('lease preview rejects source confusion and inconsistent receipt without echoing credential text', () => {
  for(const patch of [{source:'token_expiry'},{expires_at:-1},{auto_disable:'false'},{restore_on_extend:true},{expiry_format:'guessed'},{auto_disable_source:'assumed'}]){
    const response=preview();Object.assign(response.external_leases[0],patch);assert.throws(()=>externalLeasePreviews(response,1,true))
  }
  const response=preview();response.lease_restore.items[0].expires_at++
  assert.throws(()=>externalLeasePreviews(response,1,true),/不一致/)
})
test('single-row commit keeps independent receipt index1 and validates target identity before restoring', () => {
  const response=commit('new-account')
  assert.equal(externalLeaseCommitReceipt(response,metadata(),'new-account'),response.lease_restore)
  assert.throws(()=>externalLeaseCommitReceipt(response,metadata(),'other-account'),/目标与账号不匹配/)
  assert.throws(()=>externalLeaseCommitReceipt(response,{...metadata(),auto_disable:true},'new-account'),/预览不一致/)
  assert.throws(()=>externalLeaseCommitReceipt({dry_run:false},metadata(),'new-account'),/预览不一致/)
  assert.equal(response.lease_restore.items[0].account_index,1)
})
test('lease impact keeps unlimited/future/expired and explicit auto-disable distinctions', () => {
  assert.equal(externalLeaseSummary({...metadata(),expires_at:0},100),'不限期')
  assert.match(externalLeaseSummary({...metadata(),expires_at:10,auto_disable:true},100),/立即停用/)
  assert.match(externalLeaseSummary({...metadata(),expires_at:10,auto_disable:false},100),/未开启/)
  assert.match(externalLeaseSummary({...metadata(),expires_at:200,auto_disable:true},100),/未来到期/)
})
test('existing duplicate credentials permit only an explicit lease preparation action when valid external lease is opted in', () => {
  const row={kind:'duplicate',externalLease:metadata()}
  assert.equal(canPrepareDuplicateLease(row,true),true)
  assert.equal(canPrepareDuplicateLease(row,false),false)
  assert.equal(canPrepareDuplicateLease({...row,kind:'failed'},true),false)
  assert.equal(canPrepareDuplicateLease({kind:'duplicate'},true),false)
})
