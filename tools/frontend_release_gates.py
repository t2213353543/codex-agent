"""Run and bind the existing UI release checks to the actual source and build."""
import argparse
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
from acceptance import ROOT, source_manifest


EVIDENCE_NAMES = ('final-frontend-unit.log', 'final-frontend-build.log',
                  'final-settings-browser.log', 'final-usage-browser.log',
                  'final-metrics-browser.log', 'settings-results.json', 'metrics-results.json')


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def frontend_sources():
    return {name: value for name, value in source_manifest()['files'].items() if name.startswith('frontend/')}


def frontend_artifacts():
    dist = ROOT / 'frontend/dist'
    if not (dist / 'index.html').is_file():
        raise RuntimeError('Current frontend build has no index.html')
    return {path.relative_to(ROOT).as_posix(): digest(path) for path in dist.rglob('*') if path.is_file()}


def verify_report(work, source_files):
    """Validate sealed UI evidence against the complete current source/build maps."""
    def require(value, message):
        if not value:
            raise RuntimeError(message)

    work = Path(work).resolve()
    report = json.loads((work / 'frontend-gates.json').read_text(encoding='utf-8'))
    require(report.get('status') == 'pass' and report.get('source_unchanged') is True,
            'Frontend report did not complete successfully')
    expected_sources = {name: value for name, value in source_files.items() if name.startswith('frontend/')}
    require(expected_sources and report.get('source_files') == expected_sources,
            'Sealed frontend source differs from current source')
    require(report.get('artifact_files') == frontend_artifacts(),
            'Sealed frontend artifacts differ from current build')

    expected_paths = {(work / name).relative_to(ROOT).as_posix(): name for name in EVIDENCE_NAMES}
    evidence = report.get('evidence_files')
    require(isinstance(evidence, list) and len(evidence) == len(EVIDENCE_NAMES),
            'Frontend evidence list is incomplete')
    require(all(isinstance(entry, dict) and isinstance(entry.get('path'), str) for entry in evidence),
            'Frontend evidence reference is malformed')
    require({entry['path'] for entry in evidence} == set(expected_paths),
            'Frontend evidence paths differ from the sealed work directory')
    contents = {}
    for entry in evidence:
        name = expected_paths[entry['path']]
        raw = (work / name).read_bytes()
        require(hashlib.sha256(raw).hexdigest() == entry.get('sha256'), 'Frontend evidence changed: ' + name)
        contents[name] = raw.decode('utf-8')

    checks = report.get('checks')
    require(isinstance(checks, dict), 'Frontend checks are missing')
    for name in ('build', 'settings_browser', 'official_usage_browser', 'app_server_metrics_browser'):
        require(checks.get(name) is True, 'Frontend check is incomplete: ' + name)
    unit = contents['final-frontend-unit.log']
    passed = re.search(r'pass (\d+)\b', unit)
    failed = re.search(r'fail (\d+)\b', unit)
    require(passed and failed and int(failed.group(1)) == 0 and int(passed.group(1)) > 0,
            'Sealed frontend unit log did not pass')
    require(type(checks.get('unit_pass')) is int and checks['unit_pass'] == int(passed.group(1))
            and type(checks.get('unit_fail')) is int and checks['unit_fail'] == 0,
            'Frontend unit counts differ from the sealed log')
    require(json.loads(contents['settings-results.json']).get('passed') is True,
            'Sealed settings browser did not pass')
    require(json.loads(contents['metrics-results.json']).get('passed') is True,
            'Sealed metrics browser did not pass')
    require('Official usage browser passed:' in contents['final-usage-browser.log'],
            'Sealed official usage browser did not pass')
    return report


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--work', type=Path, required=True)
    args = parser.parse_args()
    work = args.work.resolve()
    work.mkdir(parents=True, exist_ok=True)
    path = work / 'frontend-gates.json'
    path.unlink(missing_ok=True)
    before = frontend_sources()
    (work / 'frontend-source-before.json').write_text(json.dumps(before, indent=2), encoding='utf-8')
    npm = ['cmd', '/c', 'npm'] if os.name == 'nt' else ['npm']

    def run(name, command):
        with (work / name).open('wb') as log:
            result = subprocess.run(command, cwd=ROOT / 'frontend', stdout=log, stderr=subprocess.STDOUT, timeout=300)
        if result.returncode:
            raise RuntimeError('frontend check failed: ' + name)
        return name

    run('final-frontend-unit.log', npm + ['test'])
    run('final-frontend-build.log', npm + ['run', 'build'])
    artifacts = frontend_artifacts()
    checks = [('final-settings-browser.log', 'tests/settings-browser.mjs'),
              ('final-usage-browser.log', 'tests/account-official-usage-browser.mjs'),
              ('final-metrics-browser.log', 'tests/app-server-metrics-browser.mjs')]
    with ThreadPoolExecutor(max_workers=3) as executor:
        futures = [executor.submit(run, name, ['node', script]) for name, script in checks]
        failures = []
        for future in futures:
            try:
                future.result()
            except Exception as error:
                failures.append(str(error))
        if failures:
            raise RuntimeError('; '.join(failures))
    assert before == frontend_sources(), 'Frontend source changed during verification'
    if artifacts != frontend_artifacts():
        raise RuntimeError('Frontend build changed during browser verification')
    unit = (work / 'final-frontend-unit.log').read_text(encoding='utf-8')
    assert re.search(r'fail 0\b', unit)
    passed = int(re.search(r'pass (\d+)\b', unit).group(1))
    settings = ROOT / 'frontend/test-results/settings/results.json'
    metrics = ROOT / 'frontend/test-results/app-server-metrics/report.json'
    assert json.loads(settings.read_text(encoding='utf-8'))['passed'] is True
    assert json.loads(metrics.read_text(encoding='utf-8'))['passed'] is True
    assert 'Official usage browser passed:' in (work / 'final-usage-browser.log').read_text(encoding='utf-8')
    shutil.copyfile(settings, work / 'settings-results.json')
    shutil.copyfile(metrics, work / 'metrics-results.json')
    files = [work / name for name in EVIDENCE_NAMES]
    report = {'status': 'pass', 'checked_at': datetime.now(timezone.utc).isoformat(),
              'source_unchanged': True, 'source_files': before, 'artifact_files': artifacts,
              'checks': {'unit_pass': passed, 'unit_fail': 0, 'build': True, 'settings_browser': True,
                         'official_usage_browser': True, 'app_server_metrics_browser': True},
              'evidence_files': [{'path': p.relative_to(ROOT).as_posix(), 'sha256': digest(p)} for p in files]}
    path.write_text(json.dumps(report, ensure_ascii=False, indent=2), encoding='utf-8')
    print(json.dumps({'passed': True, 'unit_pass': passed, 'report': str(path)}))


if __name__ == '__main__':
    main()
