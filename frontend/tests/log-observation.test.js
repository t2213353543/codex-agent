import test from 'node:test'
import assert from 'node:assert/strict'
import { observedNumber, observedBoolean, observationVersion, observationAttempts, observationSummary, modelComparison, truncationLabel, observationStageAverage, websocketWarmupLabel, reasoningRoute } from '../src/utils/logObservationState.js'
import { managedQuerySnapshot, managementScopeParams, readManagedFilters, validUsageStats, managedFilterValueLimit, managedExactFilterError } from '../src/utils/logManagementState.js'
import { cleanupSnapshot } from '../src/utils/logCleanupState.js'
import { sanitizedLogRecord } from '../src/utils/logExport.js'

test('reasoning mapping separates requested, dispatched and explicitly returned levels', () => {
  const mapped = reasoningRoute({ requested_reasoning_effort: 'max', sent_reasoning_effort: 'xhigh', reasoning_effort: 'max', reasoning_effort_source: 'request' })
  assert.equal(mapped.label, 'max → xhigh')
  assert.equal(mapped.path, 'max → xhigh → 未记录')
  assert.equal(mapped.returned, '')
  assert.match(mapped.title, /请求：max · 发送：xhigh · 响应：未记录/)
  const confirmed = reasoningRoute({ requested_reasoning_effort: 'max', sent_reasoning_effort: 'xhigh', reasoning_effort: 'xhigh', reasoning_effort_source: 'response' })
  assert.equal(confirmed.path, 'max → xhigh → xhigh')
  assert.equal(reasoningRoute({ requested_reasoning_effort: 'high', sent_reasoning_effort: 'high' }).label, 'high')
  const legacy = reasoningRoute({ reasoning_effort: 'high', reasoning_effort_source: 'request' })
  assert.equal(legacy.label, 'high')
  assert.equal(legacy.sent, '')
  assert.equal(legacy.returned, '')
  assert.equal(reasoningRoute({ reasoning_effort: 'high' }).returned, '')
  assert.equal(reasoningRoute({ requested_reasoning_effort: 0, sent_reasoning_effort: false, reasoning_effort: null }).label, '')
  assert.equal(reasoningRoute({}).path, '未记录 → 未记录 → 未记录')
})

test('observation queries preserve every new exact and boolean filter through snapshot and scope', () => {
  const fields = { sent_model:'sent-model',returned_model:'returned-model',inbound_path:'/responses',upstream_path:'/responses/compact',sent_model_mismatch:'true',returned_model_mismatch:'false',error_truncated:'false',model_mismatch:'false' }
  const snapshot=managedQuerySnapshot(fields,50)
  for(const [key,value] of Object.entries(fields))assert.equal(snapshot.params[key],value,key)
  const scope=managementScopeParams({...snapshot.params,as_of:'usage-fixed'})
  for(const [key,value] of Object.entries(fields))assert.equal(scope[key],value,key)
  assert.equal(scope.as_of,'usage-fixed')
  const cleanup=cleanupSnapshot({...scope,from:'100',to:'200'},'100','200')
  for(const [key,value] of Object.entries(fields))assert.equal(cleanup[key],value,key)
  const roundTrip=readManagedFilters(Object.fromEntries(new URLSearchParams(snapshot.params)))
  for(const [key,value] of Object.entries(fields))assert.equal(roundTrip[key],value,key)
  const invalid=readManagedFilters({sent_model_mismatch:'0',returned_model_mismatch:'unknown',error_truncated:'False'})
  assert.equal(invalid.sent_model_mismatch,'');assert.equal(invalid.returned_model_mismatch,'');assert.equal(invalid.error_truncated,'')
})
test('missing model endpoints remain unknown without substitution from compatibility fields', () => {
  assert.equal(modelComparison('inbound','sent'),'不同')
  assert.equal(modelComparison('same','same'),'一致')
  assert.equal(modelComparison('same',null),'未记录完整模型对')
  const summary=observationSummary({model:'inbound',upstream_model:'compatibility',inbound_path:'/responses',upstream_path:'https://host/responses?secret=x'})
  assert.equal(summary.sent,'');assert.equal(summary.returned,'');assert.equal(summary.upstream,'');assert.equal(summary.attemptCount,null)
  assert.equal(summary.inbound,'/responses')
})
test('zero durations and explicit false differ from absent, malformed and invalid observations', () => {
  assert.equal(observedNumber(0),0)
  for(const value of [null,undefined,'0','',false,-1,NaN,Infinity])assert.equal(observedNumber(value),null)
  assert.equal(observedBoolean(false),false);assert.equal(observedBoolean(0),null)
  assert.equal(truncationLabel(false),'已观测为未截断');assert.equal(truncationLabel(null),'截断状态未记录')
  assert.equal(observationVersion({observation_schema_version:1}),1)
  assert.equal(observationVersion({observation_schema_version:0}),null)
  assert.equal(observationStageAverage({count:1,avg:0},'count','avg'),0)
  assert.equal(observationStageAverage({count:0,avg:0},'count','avg'),null)
  assert.equal(observationStageAverage({count:1,avg:null},'count','avg'),null)
})
test('attempt view retains dispatch order, at most four rows, and only bounded protocol fields', () => {
  const source={attempts:[{attempt:1,phase:'request',endpoint:'/responses',status_code:401,duration_ms:0,header_ms:0,error_source:'upstream',error_code:'http_status',prompt:'excluded',headers:{Authorization:'secret'}},{attempt:2,phase:'refresh',endpoint:'/oauth/token',status_code:200,duration_ms:24},{attempt:3,phase:'websocket',endpoint:'/responses',status_code:101,duration_ms:8},{attempt:4,phase:'request',duration_ms:9},{attempt:5,phase:'request',status_code:500}],attempts_truncated:true,error_truncated:false}
  const result=observationAttempts(source)
  assert.equal(result.length,4);assert.deepEqual(result.map(row=>row.attempt),[1,2,3,4])
  assert.equal(result[0].duration_ms,0);assert.equal(result[0].header_ms,0)
  assert.equal(result[3].status_code,null);assert.equal(result[3].header_ms,null)
  assert.equal(JSON.stringify(result).includes('secret'),false);assert.equal('prompt'in result[0],false)
  assert.equal(observationSummary(source).attemptsTruncated,true);assert.equal(observationSummary(source).errorTruncated,false)
  assert.equal(source.attempts.length,5)
})
test('independent observation distributions validate array shapes without requiring historical data', () => {
  const stats={total_requests:1,success:1,errors:0,total_input_tokens:0,total_output_tokens:0,total_cached_tokens:0,total_tokens:0}
  assert.equal(validUsageStats(stats),true)
  for(const key of ['sent_models','returned_models','inbound_paths','upstream_paths']){
    assert.equal(validUsageStats({...stats,[key]:[]}),true)
    assert.equal(validUsageStats({...stats,[key]:{name:'invalid'}}),false)
  }
})
test('all observed 512-byte model values survive exact URL and saved-filter paths without truncation', () => {
  const model='m'.repeat(512),fields=Object.fromEntries(['model','upstream_model','sent_model','returned_model'].map(key=>[key,model]))
  const snapshot=managedQuerySnapshot(fields)
  for(const key of Object.keys(fields)){assert.equal(snapshot.params[key],model);assert.equal(managedFilterValueLimit(key),512)}
  assert.equal(managementScopeParams(snapshot.params).returned_model,model)
  assert.equal(readManagedFilters(Object.fromEntries(new URLSearchParams(snapshot.params))).sent_model,model)
  assert.equal(managedExactFilterError({sent_model:model}),'')
  assert.equal(managedExactFilterError({returned_model:'型'.repeat(341)}),'')
  assert.match(managedExactFilterError({returned_model:'型'.repeat(342)}),/1024.*UTF-8/)
})

test('WebSocket warmup preserves true, false and unknown through query, cleanup and safe export', () => {
  for (const value of ['true', 'false']) {
    const snapshot = managedQuerySnapshot({ websocket_warmup: value, from: '100', to: '200' })
    assert.equal(snapshot.params.websocket_warmup, value)
    assert.equal(readManagedFilters(Object.fromEntries(new URLSearchParams(snapshot.params))).websocket_warmup, value)
    assert.equal(managementScopeParams(snapshot.params).websocket_warmup, value)
    assert.equal(cleanupSnapshot(snapshot.params, '100', '200').websocket_warmup, value)
    assert.equal(sanitizedLogRecord({ websocket_warmup: value === 'true' }).websocket_warmup, value === 'true')
  }
  for (const value of [null, undefined, 'false', 0]) {
    assert.equal(websocketWarmupLabel(value), '预热状态未记录')
    assert.equal(sanitizedLogRecord({ websocket_warmup: value }).websocket_warmup, null)
  }
  assert.equal(websocketWarmupLabel(true), '已观测为预热')
  assert.equal(websocketWarmupLabel(false), '已观测为普通生成')
  assert.equal(readManagedFilters({ websocket_warmup: '0' }).websocket_warmup, '')
})

test('request and response IDs retain the full 1024-byte identity without truncating invalid IDs', () => {
  for (const id of ['req_' + 'a'.repeat(700), 'r'.repeat(1024), '请'.repeat(341) + 'r']) {
    const snapshot = managedQuerySnapshot({ request_id: id })
    assert.equal(snapshot.params.request_id, id)
    assert.equal(managementScopeParams(snapshot.params).request_id, id)
    assert.equal(readManagedFilters(Object.fromEntries(new URLSearchParams(snapshot.params))).request_id, id)
    assert.equal(managedExactFilterError({ request_id: id }), '')
  }
  for (const id of ['r'.repeat(1025), '请'.repeat(342)]) {
    const read = readManagedFilters({ request_id: id })
    assert.equal(read.request_id, id)
    assert.match(managedExactFilterError(read), /ID.*1024.*UTF-8/)
  }
  assert.equal(managedFilterValueLimit('request_id'), 1024)
})
