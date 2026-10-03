"""Read-only non-GET management route/shape ledger; no database or network."""
import csv,json
from pathlib import Path
from urllib.parse import urlsplit
from performance_slo import inventory
from performance_route_coverage import match_route,specific
from integration_env import ROOT

SHAPES={
'handleSessionLogin':['valid/invalid admin key; Redis session create'],
'handleSessionLogout':['cookie session delete; bearer no-cookie'],
'handleAdminKeyRotate':['rotation request id; repeat idempotent request; cross-instance revocation'],
'handleAccountsImportPreview':['100/1000 new; supported10000; mixed-invalid/name/token overlap'],
'handleAccountsBatch':['100 per-item different metadata patches, partial CAS failures'],
'handleManagedAccountSelection':['filters; up to10000 selection results'],
'handleManagedAccountActions':['action=update enabled/notes/bindings max100; delete100; refresh10 is upstream'],
'handleManagedAccountLastUsed':['100 stable IDs; missing IDs'],
'handleManagedAccountReauthorize':['provided credentials; same/different identity with CAS'],
'handleManagedReauthorizeBatch':['10 provided credential replacements; partial conflicts'],
'handleManagedAutoResetUpdate':['policy enabled/disabled and precondition failure; no immediate inference'],
'handleManagedAccountLeaseUpdate':['future/expired lease; disable/restore side effects'],
'handleManagedScheduleCreate':['disabled models/usage plan; enabled state stores schedule only'],
'handleManagedScheduleUpdate':['version CAS with20plans×100retained results'],
'handleManagedScheduleDelete':['existing/missing plan; stale CAS'],
'handleManagedAccountBundleImport':['100/1000accounts with resource mapping; replacement/dry_run'],
'handleManagedBundleResourcesImport':['up to10000groups/proxies; new/reuse/conflicts'],
'handleManagedBundleLeaseRestore':['many lease targets; preview/apply; stale versions; repeat receipt'],
'handleManagedOAuthStart':['create pending staged session and URL; no provider call'],
'handleManagedOAuthImport':['method=session local parse; refresh_token/mobile_refresh_token/pat upstream'],
'handleManagedOAuthApply':['apply ready credentials to new/existing target; confirmation/CAS'],
'handleManagedOAuthCancel':['pending/ready cancellation; exchanging conflict'],
'handleAccountsImport':['100/1000new and duplicate; supported10000; mixed replace/conflicts'],
'handleAccountUpdate':['metadata with/without associations; stale CAS; rename conflict'],
'handleAccountDelete':['bound/unbound single account; retained event/active cleanup'],
'handleGroupCreate':['unique/conflicting group'], 'handleGroupUpdate':['version CAS/rename'], 'handleGroupDelete':['many account/key bindings'],
'handleKeyCreate':['large encrypted key document+group memberships'], 'handleKeyUpdate':['enabled/group changes/rotation'], 'handleKeyDelete':['large encrypted key document'],
'handleProxyCreate':['credentials and initial disabled metadata'], 'handleProxyUpdate':['CAS/credential changes'], 'handleProxyDelete':['many account bindings'],
'handleSettingsPost':['large prompt/routing arrays; version conflicts'],
'handleCPAUpload':['single raw/multipart/multi-account imports; replace alias collisions'],
'handleCPADelete':['single alias/auth_index; all selected accounts'],
'handleCPAStatus':['disable/re-enable; native ID and filename resolution'],
'handleCPAFields':['metadata+proxy field updates; stale identity'],
'sub2WriteAccount':['new/update with preserved metadata, bindings and credentials'],
'sub2ImportData':['100/1000accounts+proxies data import; reuse/conflicts'],
'sub2DeleteAccount':['numeric ID, bound account'],
'sub2AccountAction':['schedulable/clear-error local; refresh/test/set-privacy upstream'],
'sub2Resources':['groups/proxies create/update/delete; numeric ID and bindings'],
'handleManagedCleanupPreview':['explicit time+filters; materialized matching IDs; 1k/100k/10m matches'],
'handleManagedCleanupCreate':['submit confirmed job; creation vs completion measured separately'],
'handleManagedCleanupCancel':['pending/running cancellation; ownership/version behavior'],
'handleManagedIPGeo':['private/cached IP local; uncached public/force external lookup'],
'handleManagedUsageReview':['resolved status/comment history; version conflict'],
'handleManagedUsageSnapshot':['time/filter selection; snapshot membership1k/100k/10m'],
'handleManagedAccountUsageBatch':['up to100accounts+time range/filter statistics'],
}

def main():
 routes=[dict(r)for r in inventory(ROOT)if r['method']not in('GET','ANY')]
 for r in routes:
  h,p=r['handler'],r['path'];category=r['category']
  if h in {'handleManagedAccountReauthorize','handleManagedReauthorizeBatch','handleManagedOAuthStart','handleManagedOAuthApply','handleManagedOAuthCancel'}:category='local_write'
  if h in {'handleManagedAccountPrivacy','handleManagedAccountResetCredit','handleManagedOAuthExchange'}:category='upstream_or_streaming'
  if h=='sub2AccountAction':category='upstream_or_streaming'if p.endswith(('/test','/set-privacy','/refresh'))else'local_write'
  if h in {'handleManagedAccountActions','handleManagedOAuthImport','handleManagedIPGeo'}:category='mixed_by_input'
  if h in {'handleManagedAccountSelection','handleManagedAccountLastUsed','handleManagedAccountUsageBatch'}:category='local_post_read'
  r.update(category=category,measured=False,unmeasured=True,slo_verified=False,functional_or_diagnostic_observed=False,measurements=[],remaining_input_shapes=SHAPES.get(h,['provider or export work: validate separately']))
  r.pop('coverage',None)
 reports=[ROOT/'.local/performance-slo/20260928t234319-fda3a5/report.json',ROOT/'.local/account-bulk-writes-20260929/report.json',
          ROOT/'.local/performance-slo/20260929t002715-c09518/report.json',
          ROOT/'.local/performance-slo/20260929t004945-8abc64/report.json',
          ROOT/'.local/performance-slo/20260929t005511-9fece8/report.json',
          ROOT/'.local/performance-slo/20260929t011443-004941/report.json',
          ROOT/'.local/performance-slo/20260929t012117-368c35/report.json']
 for path in reports:
  report=json.loads(path.read_text());events=report.get('events',{})
  values=events.get('WRITE_RESULT',[])+events.get('ACCOUNT_WRITE',[])if isinstance(events,dict)else events
  for v in values:
   route=v.get('route')or {};method,path_value=route.get('method',v.get('method')),route.get('path',v.get('path'))
   if not method or not path_value:continue
   candidates=[r for r in routes if r['method']==method and match_route(r,urlsplit(path_value).path)]
   if not candidates:continue
   candidates.sort(key=specific,reverse=True);r=candidates[0]
   functional=bool(v.get('functional_only'))
   interference=v.get('interference','')
   failure=bool(v.get('failed',0)or v.get('over_slo',0)or v.get('sample',{}).get('error'))
   passed=report.get('status')=='pass' and not failure and v.get('slo_pass',True)
   verified=passed and not functional and not interference
   status='functional_or_diagnostic'if functional or interference else 'verified_sampled_slo'if verified else 'failed_sampled_slo'
   r['measurements'].append({'case':v['name'],'method':method,'request_path':path_value,'report':str(path),'source_sha256':report.get('source_sha256'),'sample_count':len(v.get('samples',[]))or 1,'max_ms':v.get('max_ms',v.get('sample',{}).get('ms')),'evidence_status':status,'functional_only':functional,'interference':interference,'slo_verified':verified,'semantic_pass':not bool(v.get('failed',0)or v.get('sample',{}).get('error')),'request_bytes':v.get('request_bytes')})
   r['measured'],r['unmeasured']=True,False
   r['slo_verified']|=verified
   r['functional_or_diagnostic_observed']|=functional or bool(interference)
 result={'scope':'non-GET management registrations; body branches override path-keyword guesses','all_parameter_shapes_proven':False,'mixed_sources':True,'routes':routes,'route_count':len(routes)}
 result['counts_by_category']={c:{'routes':sum(r['category']==c for r in routes),'sampled_routes':sum(r['category']==c and r['measured']for r in routes),'slo_verified_routes':sum(r['category']==c and r['slo_verified']for r in routes),'functional_or_diagnostic_only_routes':sum(r['category']==c and r['functional_or_diagnostic_observed']and not r['slo_verified']for r in routes)}for c in sorted({r['category']for r in routes})}
 out=ROOT/'integration/performance/results/non-get-coverage-20260929';out.with_suffix('.json').write_text(json.dumps(result,ensure_ascii=False,indent=2)+'\n')
 with out.with_suffix('.csv').open('w',encoding='utf-8-sig',newline='')as f:
  w=csv.DictWriter(f,fieldnames=['method','route','handler','category','measured','unmeasured','slo_verified','evidence_statuses','actual_shapes','remaining_input_shapes']);w.writeheader()
  for r in routes:w.writerow({'method':r['method'],'route':r['path'],'handler':r['handler'],'category':r['category'],'measured':str(r['measured']).lower(),'unmeasured':str(r['unmeasured']).lower(),'slo_verified':str(r['slo_verified']).lower(),'evidence_statuses':'\n'.join(sorted({m['evidence_status']for m in r['measurements']})),'actual_shapes':'\n'.join(sorted({m['case'] for m in r['measurements']})),'remaining_input_shapes':'\n'.join(r['remaining_input_shapes'])})
 print(json.dumps(result['counts_by_category']))

if __name__=='__main__':main()
