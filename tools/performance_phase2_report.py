"""Summarize four actual SLO runs across burst and uniform data distributions."""
from __future__ import annotations

import argparse
import json
from pathlib import Path

from performance_slo_report import compact, load


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    for name in ('uniform_100k','uniform_1m','burst_100k','burst_10m'):
        parser.add_argument('--'+name.replace('_','-'),dest=name,type=Path,required=True)
    parser.add_argument('--additional-100k',type=Path,help='Same-binary Additional reads rerun after migrations; replaces pre-migration Additional samples')
    parser.add_argument('--additional-10m',type=Path,help='Same-binary Additional reads rerun after migrations; replaces pre-migration Additional samples')
    parser.add_argument('--output',type=Path,required=True)
    args=parser.parse_args()
    runs={name:load(getattr(args,name)) for name in ('uniform_100k','uniform_1m','burst_100k','burst_10m')}
    supplemental={}
    for label,argument in (('burst_100k',args.additional_100k),('burst_10m',args.additional_10m)):
        if not argument:continue
        extra=load(argument);base=runs[label]
        if (extra['source_sha256'],extra['binary_sha256'])!=(base['source_sha256'],base['binary_sha256']):raise ValueError('Additional rerun source/binary differ')
        names={row['name'] for row in extra['events']['FIRST']}
        for kind in ('FIRST','RESULT'):
            base['events'][kind]=[row for row in base['events'][kind] if row['name'] not in names]+extra['events'].get(kind,[])
        if extra['status']!='pass':base['status']='fail'
        supplemental[label]={'report':str(argument.resolve()),'replaced_case_names':sorted(names),'note':'Combined binary executes Additional tests before Scale migrations; these dedicated reruns occurred after migration checksum verification and replace those pre-migration samples in the final combined view.'}
    hashes={r['source_sha256'] for r in runs.values()}
    binaries={r['binary_sha256'] for r in runs.values()}
    if len(hashes)!=1 or len(binaries)!=1:raise ValueError('All final runs must use exactly one source and binary')
    results={name:compact(report) for name,report in runs.items()}
    for name,report in runs.items():
        results[name]['migrations']=report['events'].get('MIGRATION',[])+report['events'].get('UNIFORM_MIGRATION',[])
        results[name]['storage']=report['events'].get('STORAGE',[])
        results[name]['failed_tests']=report.get('failed_tests',[])
        results[name]['evidence']=str(getattr(args,name).resolve())
    out={'source_sha256':next(iter(hashes)),'binary_sha256':next(iter(binaries)),
         'status':'pass' if all(r['status']=='pass' for r in runs.values()) else 'fail',
         'synthetic_only':True,'production_touched':False,'runs':results,
         'total_read_samples':sum(r['read_requests'] for r in results.values()),
         'total_write_samples':sum(r['write_samples'] for r in results.values()),
         'all_routes_slo_proven':False,
         'post_migration_additional_evidence':supplemental,
         'limits':['Finite synthetic HTTP scenarios at concurrency 1 and 4; not an all-input/all-concurrency guarantee',
                   'Burst 10M and uniform 1M are distinct scale/distribution evidence; 10M uniform is not measured',
                   'All fixtures use 1000 accounts; high cardinality within each account-minute, not 10M accounts',
                   'Initial request is process-cold by endpoint only; OS/PG caches are not evicted',
                   'No live upstream generation, network-dependent OAuth/proxy tests, or arbitrary full exports measured']}
    args.output.parent.mkdir(parents=True,exist_ok=True)
    args.output.with_suffix('.json').write_text(json.dumps(out,indent=2,ensure_ascii=False)+'\n',encoding='utf-8')
    labels={'uniform_100k':'均匀 10 万','uniform_1m':'均匀 100 万','burst_100k':'突发 10 万','burst_10m':'突发 1000 万'}
    lines=['# 第二阶段接口性能验收','',f"同一源码 `{out['source_sha256']}`，同一二进制 `{out['binary_sha256']}`。测试进程 2 CPU / 2 GiB，隔离 PostgreSQL 4 CPU / 4 GiB，真实 loopback HTTP。",'',
           '| 数据分布与规模 | 读取场景 | 实际请求 | 单请求阈值 | 实测最慢 | 状态 |','| --- | ---: | ---: | ---: | ---: | --- |']
    for name,r in results.items():lines.append(f"| {labels[name]} | {r['read_cases']} | {r['read_requests']} | {r['slo_ms']} ms | {r['read_max_ms']:.3f} ms | {r['status']} |")
    lines+=['',f"共 {out['total_read_samples']} 个读取样本、{out['total_write_samples']} 个写入/持久化样本。失败、非 200、数据语义不符与阈值超限均按失败保留；没有执行的场景不生成通过数字。",'',
            '均匀分布覆盖 43200 个分钟，每事件对应不同账号分钟；突发分布约 3 万个账号分钟。两者不能互相替代。此前 009 分钟汇总在均匀 100 万数据上 SQL 9.280 秒、HTTP 统计 8.292 秒，日层汇总和查询规划优化针对这个实际瓶颈。','',
            '以下逐项列出初次响应及并发 4 的 P50/P95/P99；每档 20 个样本时 P99 等于该档最大值。','']
    for name,r in results.items():
        lines += [labels[name],'','| 场景 | 首次 ms | c4 P50 ms | c4 P95 ms | c4 P99 ms |','| --- | ---: | ---: | ---: | ---: |']
        for case,value in r['cases'].items():
            c=value.get('c4',{})
            metric=lambda k:f"{c[k]:.3f}" if k in c else '未测'
            lines.append(f"| {case} | {value['first']['ms']:.3f} | {metric('p50_ms')} | {metric('p95_ms')} | {metric('p99_ms')} |")
        lines.append('')
    lines += ['维护阶段的迁移、回填和聚合重建耗时不计作 API 响应速度。原版百万均匀数据的账号补全与真实聚合重建约 609.056 秒；新版单独实际重建及总量核对 14.783 秒。两次计时范围略不同，不将比值当作严格独立重建加速比。','',
              '限制：未验证千万均匀数据、千万账号、所有参数/排序/过滤组合、任意并发以及模型上游生成耗时；未部署生产。路由清单中的未测项目仍未获得性能承诺。完整逐请求证据路径见 JSON。']
    if supplemental:lines+=['','兼容接口 Additional 测试在组合测试的执行顺序中先于 Scale 迁移，因此两组突发数据又在全部迁移完成后用同一二进制单独重测 27 个场景。本报告采用迁移后 Additional 样本替换迁移前样本，不重复计数；原始组合报告仍完整保留，补测路径列在 JSON。']
    args.output.with_suffix('.md').write_text('\n'.join(lines)+'\n',encoding='utf-8')
    print(args.output.with_suffix('.json'))
    print(args.output.with_suffix('.md'))


if __name__=='__main__':main()
