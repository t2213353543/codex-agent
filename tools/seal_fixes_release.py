"""Seal verified September fixes for the existing journaled publisher."""
import hashlib
import json
from pathlib import Path
import tarfile

ROOT=Path(__file__).resolve().parents[1];OUT=ROOT/'.local/fixes-20260924'
def read(path):return json.loads(path.read_text(encoding='utf-8'))
def sha(path):return hashlib.sha256(path.read_bytes()).hexdigest()

def main():
 backend=read(OUT/'backend/report.json');source=read(OUT/'backend/source-manifest.json')
 linux=read(OUT/'linux-checks/run-2/report.json');native=read(OUT/'native-build/elf-inspection.json')
 assert backend['passed'] and linux['exit_code']==0 and not linux['failed']
 required=['TestGatewayNativeActualAccountConcurrency96','TestGatewayNativeActualWebSocketConcurrency96','TestPGManagedUsageBatchOfficialCache','TestOverviewBreakdownPG','TestGatewayCompactV2PreservesOpaqueContextAndFollowup']
 assert all(name in linux['passed'] for name in required)
 assert read(OUT/'native-build/test-results.json')['passed']
 assert read(OUT/'native-build/build-state.json')['status']=='complete'
 binary=OUT/'native-build/codex-native-transport-verified';assert sha(binary)==native['binary_sha256']
 archive=OUT/'native-build/native-release.tar.gz'
 with tarfile.open(archive,'w:gz') as tar:tar.add(binary,arcname='codex-native-transport')
 report={'package_path':archive.relative_to(ROOT).as_posix(),'package_sha256':sha(archive),'binary_bytes':binary.stat().st_size,'binary_sha256':sha(binary)}
 (OUT/'native-build/native-release.json').write_text(json.dumps(report,indent=2),encoding='utf-8')
 frontend=read(OUT/'frontend/build-manifest.json');live=read(OUT/'frontend/live-preflight.json')
 assert live['passed'] and read(ROOT/'.local/admin-ui-improvements/after/results.json')['passed']
 assert read(ROOT/'.local/admin-ui-improvements/dashboard/results.json')['status']=='passed'
 account_browser=read(ROOT/'.local/official-usage/account-regression/report.json')
 assert account_browser.get('passed') is True or account_browser.get('status') in ('pass','passed')
 for name,digest in source['files'].items():assert sha(ROOT/name)==digest
 for name,digest in frontend['files'].items():assert sha(OUT/'frontend/dist'/name)==digest
 acceptance={'schema':'codex-agent-full-release-acceptance-v1','passed':True,'source_sha256':source['sha256'],'binary_sha256':backend['binary_sha256'],'native_sha256':sha(binary),'frontend':frontend['files'],
 'checks':{'local_go':backend['checks'],'linux_race':linux,'native_loopback':read(OUT/'native-build/test-results.json'),'production_frontend':{'checks':len(live['browser']['checks']),'passed':True}},
 'limits':['CLI executable dependent tests skipped; real HTTP/WS/model regressions run after deployment.','Initial Linux race run had a missing Redis test prefix and a wrong expected busy status; corrected targeted race passed.']}
 (OUT/'release/acceptance.json').write_text(json.dumps(acceptance,indent=2),encoding='utf-8')
 print(json.dumps({'passed':True,'binary':backend['binary_sha256'],'native':sha(binary),'linux_race_passed':len(linux['passed'])}))

if __name__=='__main__':main()
