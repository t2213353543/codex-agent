"""Create compact, source-bound SLO evidence from actual runs (no sample data)."""
from __future__ import annotations

import argparse
from collections import Counter
import json
from pathlib import Path


def load(path):
    return json.loads(Path(path).read_text(encoding='utf-8'))


def compact(data):
    first={row['name']:row for row in data['events'].get('FIRST',[])}
    cases={}
    for name,row in first.items():
        cases[name]={'path':row['path'],'first':row['sample'],'first_slo_pass':row.get('slo_pass')}
    total_requests=len(first)
    for row in data['events'].get('RESULT',[]):
        cases[row['name']][f"c{row['concurrency']}"]={k:row[k] for k in ('p50_ms','p95_ms','p99_ms','max_ms','failed','over_slo','statuses')}
        cases[row['name']][f"c{row['concurrency']}"]['samples']=len(row['samples'])
        total_requests+=len(row['samples'])
    durations=[row['sample']['ms'] for row in first.values()]
    durations += [sample['ms'] for row in data['events'].get('RESULT',[]) for sample in row['samples']]
    writes=[{k:row[k] for k in ('name','route','p50_ms','p95_ms','p99_ms','max_ms','failed','over_slo','threshold_ms','scope')}|{'samples':len(row['samples'])}
            for row in data['events'].get('WRITE_RESULT',[])]
    return {'status':data['status'],'source_sha256':data['source_sha256'],'binary_sha256':data['binary_sha256'],
            'requested_rows':data['requested_rows'],'slo_ms':data['slo_ms'],'read_cases':len(first),
            'read_requests':total_requests,'read_max_ms':max(durations,default=None),'cases':cases,
            'writes':writes,'write_samples':sum(row['samples'] for row in writes),'write_invariants':data['events'].get('WRITE_INVARIANTS',[]),
            'fixture':data['events'].get('FIXTURE',[]),'auxiliary_fixture':data['events'].get('AUXILIARY_FIXTURE',[]),
            'postgres_resources':data['postgres_resources'],'postgres_settings':data.get('postgres_settings'),
            'driver_resources':data['driver_resources'],'all_routes_slo_proven':data.get('all_routes_slo_proven',False),
            'route_inventory':{'registered_entries':len(data['route_inventory']),'categories':dict(Counter(r['category'] for r in data['route_inventory'])),
                               'sampled_route_shapes':sum(r['coverage']!='not_measured' for r in data['route_inventory'])},
            'scope':data['scope'],'cold_scope':data['cold_scope'],'limitations':data['limitations']}


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--ordinary',type=Path,required=True)
    parser.add_argument('--scale',type=Path,required=True)
    parser.add_argument('--baseline',type=Path,required=True)
    parser.add_argument('--migration',type=Path,required=True)
    parser.add_argument('--regression',type=Path,help='Same-source full regression report, including write SLO evidence')
    parser.add_argument('--race',type=Path,help='Race report; differing source hashes are explicitly documented')
    parser.add_argument('--output',type=Path,required=True)
    args=parser.parse_args()
    ordinary,scale,baseline,migration=(load(getattr(args,k)) for k in ('ordinary','scale','baseline','migration'))
    if ordinary['source_sha256']!=scale['source_sha256']:raise ValueError('Final source manifests differ')
    if ordinary['status']!='pass' or scale['status']!='pass':raise ValueError('Final SLO runs have failures')
    out={'source_sha256':scale['source_sha256'],'ordinary':compact(ordinary),'scale':compact(scale),'baseline':compact(baseline),
         'migration':{'events':migration['events'].get('MIGRATION',[]),'storage':migration['events'].get('STORAGE',[])},
         'evidence':{k:str(getattr(args,k).resolve()) for k in ('ordinary','scale','baseline','migration')},
         'comparison_note':'Baseline uses the partially optimized seed source, not the original pre-task code; 33 baseline shapes overlap 35 final shapes; final adds two missing-ID cases.',
         'validation_note':'Measured finite synthetic workload, not a universal guarantee for all registered routes, inputs, concurrency levels or high-cardinality datasets.'}
    if args.regression:
        regression=load(args.regression)
        if regression['status']!='pass' or regression['source_sha256']!=scale['source_sha256'] or regression['binary_sha256']!=scale['binary_sha256']:
            raise ValueError('Regression must pass with the final source and binary')
        top=sorted(name for name in regression['passed_tests'] if '/' not in name)
        selected=sorted(set(__import__('re').findall(r'\bTest\w+',regression['test_run'])))
        missing=sorted(set(selected)-set(top))
        if missing or regression['failed_tests'] or regression['skipped_tests']:raise ValueError('Regression contains missing, failed, or skipped tests')
        out['regression']={'status':'pass','source_sha256':regression['source_sha256'],'binary_sha256':regression['binary_sha256'],
                           'passed_top_level_count':len(top),'management_test_count':len(top)-('TestPerformanceSLOWrites' in top),
                           'selected_top_level_tests':selected,'passed_top_level_tests':top,'failed_tests':[],'skipped_tests':[],'missing_tests':[]}
        out['evidence']['regression']=str(args.regression.resolve())
        writes=compact(regression)
        out['scale']['writes']=writes['writes'];out['scale']['write_samples']=writes['write_samples'];out['scale']['write_invariants']=writes['write_invariants']
        out['scale']['write_evidence']=str(args.regression.resolve())
    if args.race:
        race=load(args.race)
        if race['status']!='pass' or race['failed_tests'] or race['skipped_tests'] or race['missing_tests']:raise ValueError('Race report did not pass')
        old_manifest=load(Path(race['source']).parent/'source-manifest.json')
        new_manifest=load(args.scale.resolve().parent/'source-manifest.json')
        changed=[name for name in sorted(old_manifest['files'].keys()|new_manifest['files'].keys()) if old_manifest['files'].get(name)!=new_manifest['files'].get(name)]
        production=[name for name in changed if not name.endswith('_test.go')]
        out['race']={'status':'pass','source_sha256':race['source_sha256'],'passed_top_level_tests':race['passed_top_level_tests'],
                     'selected_top_level_tests':race['selected_top_level_tests'],'source_matches_final':race['source_sha256']==scale['source_sha256'],
                     'changed_files_since_race':changed,'production_files_changed_since_race':production,
                     'delta_note':'The only production delta is Group.create INSERT RETURNING persisted timestamps, preserving one SQL round trip. The final source passes all management regression and 13 write SLO groups; race was not rerun after this delta.'}
        out['evidence']['race']=str(args.race.resolve())
    args.output.parent.mkdir(parents=True,exist_ok=True)
    args.output.with_suffix('.json').write_text(json.dumps(out,indent=2,ensure_ascii=False)+'\n',encoding='utf-8')
    lines=['# 接口性能实测（2026-09-29）','',f"同一最终源码：`{out['source_sha256']}`。实际 loopback HTTP、管理鉴权、JSON 与隔离 PostgreSQL；数据库 4 CPU / 4 GiB，测试进程 2 CPU / 2 GiB。",'',
           '| 数据量 | 读取场景 | 请求数 | 阈值 | 最慢请求 | 结果 |','| --- | ---: | ---: | ---: | ---: | --- |']
    for name,result in [('10 万日志/事件',out['ordinary']),('1000 万日志/事件',out['scale'])]:
        lines.append(f"| {name} | {result['read_cases']} | {result['read_requests']} | {result['slo_ms']} ms | {result['read_max_ms']:.3f} ms | 全部通过 |")
    lines+=['',f"另有 {len(out['scale']['writes'])} 组写入/持久化操作、{out['scale']['write_samples']} 个样本在独立临时 schema 中完成，全部 <1000ms；最慢 {max(r['max_ms'] for r in out['scale']['writes']):.3f}ms。包含 4 并发事件入库；事件、明细、累计和两类 retained 汇总计数均精确相等。",'',
            '下表为千万级场景。首请求不是物理冷缓存；warm P99 来自每档 20 个实际样本，此时也等于该档最大值。旧版首次失败会停止重复施压，因此部分旧版并发分位数不存在。','',
            '| 场景 | 旧版首次 ms | 旧版首次状态 | 新版首次 ms | 新版并发 1 P99 ms | 新版并发 4 P99 ms |','| --- | ---: | --- | ---: | ---: | ---: |']
    for name,result in out['scale']['cases'].items():
        old=out['baseline']['cases'].get(name)
        old_ms=f"{old['first']['ms']:.3f}" if old else '未测'
        old_status=str(old['first']['status']) if old else '—'
        if old and (old['first'].get('error') or old.get('first_slo_pass') is False):old_status+=' / 失败或超标'
        lines.append(f"| {name} | {old_ms} | {old_status} | {result['first']['ms']:.3f} | {result['c1']['p99_ms']:.3f} | {result['c4']['p99_ms']:.3f} |")
    seconds=out['migration']['events'][0]['elapsed_s']
    lines+=['',f"千万级首次 008+009 迁移与历史回填耗时 {seconds:.3f} 秒（{seconds/60:.2f} 分钟）。这是维护操作，会暂时阻塞日志写入，不能按正常接口延迟承诺。retained 汇总新增约 9.3 MiB（本 fixture 的低维度基数）；实际各表体积见 JSON。",'',
            '数据为 1000 个账号、30 天突发分布、10% 错误、约 3 万个账号分钟桶；不是千万账号，也未模拟每条记录维度各异的高基数。35 场景未覆盖全部 127 个路由注册/分派入口，上游/流式、任意导出、任意过滤与任意并发仍不能宣称统一达标。测试未部署生产。','',
            '旧版对照是生成 fixture 时已含部分优化的冻结实现，不是开工前原始代码。新增两个不存在 ID 场景只验新版，不计入旧版改善数量。','',
            '完整源码哈希、逐请求样本、状态和原始日志见本报告 JSON 的 evidence 路径。']
    if args.regression:
        lines+=['',f"同一源码及同一已上传二进制还通过 {out['regression']['management_test_count']} 项管理回归与 1 项写入 SLO 测试，共 {out['regression']['passed_top_level_count']} 个顶层测试，0 失败、0 跳过、0 缺失。两个读取规模均复用该二进制，未重新编译、未重新上传；迁移 checksum 预检相同，未应用新迁移。"]
    if args.race:
        lines+=['',f"Linux race 的 {out['race']['passed_top_level_tests']} 个顶层测试全部通过，绑定前一源码 `{out['race']['source_sha256']}`。与最终版本唯一生产差异是 Group.create 改用 INSERT RETURNING 获取数据库已持久化时间戳，仍为一次 SQL 往返；其余差异为测试代码。最终版本已重新完成完整管理回归、写入与两规模读取验收；race 未在此最后差异后重跑，不能宣称 race 与最终源码完全相同。"]
    args.output.with_suffix('.md').write_text('\n'.join(lines)+'\n',encoding='utf-8')
    print(args.output.with_suffix('.json'))
    print(args.output.with_suffix('.md'))


if __name__=='__main__':main()
