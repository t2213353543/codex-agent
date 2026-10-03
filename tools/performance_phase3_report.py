"""Summarize source-bound phase-three SLO evidence without discarding failures."""
from __future__ import annotations

import argparse
import json
import math
from pathlib import Path


def summary(samples,threshold=None,require_http_status=True):
    values=[];issues=[];errors=0
    if not samples:issues.append('empty_performance_sample_set')
    valid_threshold=threshold is None or (isinstance(threshold,(int,float)) and not isinstance(threshold,bool) and math.isfinite(threshold) and threshold>0)
    if not valid_threshold:issues.append('invalid_slo_threshold')
    for index,sample in enumerate(samples):
        bad=bool(sample.get('error')) or sample.get('semantic_pass') is False
        value=sample.get('ms')
        if isinstance(value,bool) or not isinstance(value,(int,float)) or not math.isfinite(value) or value<0:
            issues.append(f'sample_{index}_invalid_latency');bad=True
        else:values.append(float(value))
        if require_http_status:
            status=sample.get('status')
            if not isinstance(status,int) or isinstance(status,bool) or not 200<=status<300:bad=True
        errors+=int(bad)
    values.sort()
    over=sum(value>=threshold for value in values) if threshold is not None and valid_threshold else 0
    quantile=lambda p:values[math.ceil(len(values)*p)-1] if values else None
    return {'samples':len(samples),'valid_latency_samples':len(values),'p50_ms':quantile(.50),
            'p95_ms':quantile(.95),'p99_ms':quantile(.99),'max_ms':values[-1] if values else None,
            'failed':errors,'over_slo':over,'threshold_ms':threshold,'validation_errors':issues,
            'status':'pass' if not errors and not over and not issues else 'fail'}


def inspect(path):
    report=json.loads(path.read_text(encoding='utf-8'));events=report.get('events',{});groups={};issues=[]
    def add_group(kind,name,method,route,item,samples):
        threshold=item.get('threshold_ms',report.get('slo_ms'))
        key=(kind,name,method,route)
        group=groups.setdefault(key,{'kind':kind,'name':name,'method':method,'path':route,'threshold_ms':threshold,'raw_samples':[],'functional_only':False,'reported_failures':[]})
        if group['threshold_ms']!=threshold:group['reported_failures'].append('inconsistent_threshold')
        functional=bool(report.get('functional_only') or item.get('functional_only'))
        group['functional_only']|=functional
        if item.get('semantic_pass') is False or item.get('failed',0)>0:group['reported_failures'].append('reported_semantic_or_request_failure')
        if not functional and (item.get('slo_pass') is False or item.get('over_slo',0)>0):group['reported_failures'].append('reported_slo_failure')
        group['raw_samples'].extend(samples)
    for kind in ('FILTER_SAMPLE','ACCOUNT_WRITE'):
        for item in events.get(kind,[]):
            add_group(kind,item['name'],item.get('method','GET'),item['path'],item,[dict(item.get('sample',item))])
    for kind in ('FIRST','RESULT','WRITE_RESULT'):
        for item in events.get(kind,[]):
            route=item.get('route') or {};method=route.get('method','GET');path_value=item.get('path',route.get('path',''))
            category='WRITE_RESULT' if kind=='WRITE_RESULT' else 'READ'
            name=item['name']+('/c'+str(item['concurrency']) if kind=='RESULT' else '/first' if kind=='FIRST' else '')
            samples=[item['sample']] if kind=='FIRST' else item.get('samples',[])
            add_group(category,name,method,path_value,item,samples)
    result=[]
    for group in groups.values():
        if group['threshold_ms'] is None:group['reported_failures'].append('missing_http_slo_threshold')
        group.update(summary(group.pop('raw_samples'),group['threshold_ms']))
        if group['reported_failures']:group['status']='fail'
        if group.get('functional_only'):group['status']='functional_fail_not_slo' if group['failed'] or group['validation_errors'] or group['reported_failures'] else 'functional_only_not_slo'
        result.append(group)
    cost_groups={}
    for item in events.get('MODEL_WRITE_COST',[]):
        key=(item['model_enabled'],item['concurrency'])
        group=cost_groups.setdefault(key,{'model_enabled':item['model_enabled'],'concurrency':item['concurrency'],'repetitions':[],'raw_samples':[],'wall_ms_by_repetition':[]})
        group['repetitions'].append(item['repetition'])
        group['raw_samples'].extend({'ms':value} for value in item.get('samples_ms',[]))
        group['wall_ms_by_repetition'].append(item.get('wall_ms'))
    for group in cost_groups.values():
        group.update(summary(group.pop('raw_samples'),require_http_status=False))
        if len(set(group['repetitions']))!=len(group['repetitions']):
            group['validation_errors'].append('duplicate_cost_repetition');group['status']='fail'
        group['scope']='persistRecord transaction cost; not HTTP SLO evidence'
    comparisons=[]
    for concurrency in sorted({key[1] for key in cost_groups}):
        baseline,model=cost_groups.get((False,concurrency)),cost_groups.get((True,concurrency))
        if not baseline or not model or baseline['repetitions']!=model['repetitions']:
            issues.append(f'incomplete_model_write_cost_pair_c{concurrency}');continue
        comparisons.append({'concurrency':concurrency,'baseline_samples':baseline['samples'],'model_samples':model['samples'],
                            'p50_ratio':model['p50_ms']/baseline['p50_ms'] if baseline['p50_ms'] and model['p50_ms'] is not None else None,
                            'p95_ratio':model['p95_ms']/baseline['p95_ms'] if baseline['p95_ms'] and model['p95_ms'] is not None else None,
                            'scope':'Pooled recorded repetitions; model enabled / baseline; not HTTP latency.'})
    if result:kind='http_samples'
    elif cost_groups:kind='write_cost_only_not_http'
    elif events.get('MAINTENANCE_COMPLETE') or events.get('UNIFORM_MAINTENANCE_COMPLETE'):kind='maintenance_only_not_http'
    else:kind='no_performance_evidence';issues.append('missing_performance_evidence')
    if not report.get('source_sha256') or not report.get('binary_sha256'):issues.append('missing_source_or_binary_identity')
    if report.get('failed_tests') or report.get('skipped_tests'):issues.append('failed_or_skipped_selected_tests')
    evidence_valid=report.get('status')=='pass' and not issues and all(g['status']=='pass' for g in result) and all(g['status']=='pass' for g in cost_groups.values())
    return {'report_status':report.get('status'),'source_sha256':report.get('source_sha256'),'binary_sha256':report.get('binary_sha256'),
            'evidence_kind':kind,'evidence_valid':evidence_valid,'validation_errors':issues,
            'schema':report.get('schema'),'rows':report.get('requested_rows'),
            'evidence':str(path.resolve()),'groups':result,
            'model_write_cost':list(cost_groups.values()),'model_write_cost_comparisons':comparisons,
            'maintenance':{key:events.get(key,[]) for key in ('MIGRATION','MAINTENANCE_START','MAINTENANCE_COMPLETE','UNIFORM_MIGRATION','UNIFORM_REBUILD','UNIFORM_HTTP_PREPARE','UNIFORM_MAINTENANCE_COMPLETE','WORKER_COMPLETION','RECENT_SEED','MIXED_SEED_PROGRESS') if key in events},
            'fixture':events.get('FIXTURE',events.get('FILTER_FIXTURE',[])),
            'extra_fixture_evidence':{key:events[key] for key in ('RECENT_FIXTURE','MIXED_FIXTURE','UNIFORM_WINDOW','OBSERVED_WINDOW') if key in events},
            'failed_tests':report.get('failed_tests',[]),'skipped_tests':report.get('skipped_tests',[]),
            'limitations':report.get('limitations',[]),'postgres_resources':report.get('postgres_resources'),
            'driver_resources':report.get('driver_resources')}


def aggregate(reports):
    sources={r['source_sha256'] for r in reports};binaries={r['binary_sha256'] for r in reports}
    same_source=len(sources)==1 and None not in sources and '' not in sources
    same_binary=len(binaries)==1 and None not in binaries and '' not in binaries
    groups=[g for r in reports for g in r['groups']]
    return {'same_source':same_source,'same_binary':same_binary,'source_sha256':next(iter(sources)) if same_source else None,
         'binary_sha256':next(iter(binaries)) if same_binary else None,'http_groups':len(groups),'http_samples':sum(g['samples'] for g in groups),
         'synthetic_only':True,'production_deployed':False,'all_routes_slo_proven':False,'reports':reports,
         'status':'pass' if same_source and same_binary and groups and all(r['evidence_valid'] for r in reports) else 'fail_or_incomplete',
         'scope':'Finite measured shapes only; failures/functional-only samples remain explicit. Maintenance and transaction costs are not HTTP latency.'}


def render_markdown(out):
    fmt=lambda value:'—' if value is None else f'{value:.3f}'
    lines=['# 第三阶段性能证据','',f"源码一致：{out['same_source']}；二进制一致：{out['same_binary']}。总体状态：{out['status']}。",'',
           f"HTTP 场景组：{out['http_groups']}；请求样本：{out['http_samples']}。不等于全接口达标。",'',
           '| 证据 | 场景 | 请求数 | P50 ms | P95 ms | P99 ms | 最大 ms | 阈值 ms | 失败 | 超标 | 状态 |','| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | --- |']
    for r in out['reports']:
        label=Path(r['evidence']).parent.name
        for g in r['groups']:
            lines.append(f"| {label} | {g['name']} | {g['samples']} | {fmt(g['p50_ms'])} | {fmt(g['p95_ms'])} | {fmt(g['p99_ms'])} | {fmt(g['max_ms'])} | {fmt(g['threshold_ms'])} | {g['failed']} | {g['over_slo']} | {g['status']} |")
    lines+=['','## 非 HTTP 成本与完整性','']
    for r in out['reports']:
        label=Path(r['evidence']).parent.name
        lines.append(f"- {label}：测试 {r['report_status']}；证据 {r['evidence_kind']}；校验 {'通过' if r['evidence_valid'] else '未通过'}。")
        if r['validation_errors']:lines.append('  - '+'; '.join(r['validation_errors']))
        for g in r['model_write_cost']:
            lines.append(f"  - model={g['model_enabled']} / c{g['concurrency']}：{g['samples']} 次事务，P50 {fmt(g['p50_ms'])} ms，P95 {fmt(g['p95_ms'])} ms，最大 {fmt(g['max_ms'])} ms；非 HTTP。")
        for c in r['model_write_cost_comparisons']:
            lines.append(f"  - c{c['concurrency']} 新旧事务成本比：P50 {fmt(c['p50_ratio'])}×，P95 {fmt(c['p95_ratio'])}×。")
    lines+=['','所有数值来自实际样本；首次失败后停止的场景没有补造 warm 或并发数据。不同分布、过滤组合、数据量及资源配置不得互相替代。只完成 fixture 追加或维护不能宣称 API SLO 达标。完整源码、原始日志、逐请求样本及维护数据路径见 JSON。']
    return '\n'.join(lines)+'\n'


def main():
    parser=argparse.ArgumentParser(description=__doc__);parser.add_argument('--reports',nargs='+',type=Path,required=True);parser.add_argument('--output',type=Path,required=True)
    args=parser.parse_args();out=aggregate([inspect(path) for path in args.reports])
    args.output.parent.mkdir(parents=True,exist_ok=True)
    args.output.with_suffix('.json').write_text(json.dumps(out,ensure_ascii=False,indent=2,allow_nan=False)+'\n',encoding='utf-8')
    args.output.with_suffix('.md').write_text(render_markdown(out),encoding='utf-8');print(args.output.with_suffix('.json'))


if __name__=='__main__':main()
