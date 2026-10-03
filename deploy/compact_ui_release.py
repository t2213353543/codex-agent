#!/usr/bin/env python3
"""Publish a verified UI index without restarting the 0.4.0 gateway.

The original backend release manifest stays intact. The journal explicitly
records its static-directory overlay; old hashed assets remain usable by tabs.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import subprocess
import tempfile
import time
import urllib.request

ROOT = Path('/opt/codex-agent')
SERVICE = 'codex-agent.service'
BASE_URL = 'http://127.0.0.1:18879'
SHA = re.compile(r'[0-9a-f]{64}')


def require(ok, message):
    if not ok:
        raise RuntimeError(message)


def sha(path):
    digest = hashlib.sha256()
    with Path(path).open('rb') as source:
        for block in iter(lambda: source.read(1024 * 1024), b''):
            digest.update(block)
    return digest.hexdigest()


def object_sha(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True).encode()).hexdigest()


def file_map(folder):
    result = {}
    require(not folder.is_symlink(), 'artifact root is a symlink')
    for path in sorted(folder.rglob('*')):
        require(not path.is_symlink(), 'artifact contains a symlink')
        if path.is_file():
            result[path.relative_to(folder).as_posix()] = sha(path)
    return result


def write_atomic(path, value):
    path.parent.mkdir(parents=True, exist_ok=True)
    descriptor, name = tempfile.mkstemp(prefix='.ui-', dir=path.parent)
    try:
        with os.fdopen(descriptor, 'wb') as target:
            target.write(value)
            target.flush()
            os.fsync(target.fileno())
        os.chmod(name, 0o644)
        os.replace(name, path)
    finally:
        if os.path.exists(name):
            os.unlink(name)


def save(path, value):
    write_atomic(path, (json.dumps(value, indent=2, sort_keys=True) + '\n').encode())


def load(path):
    return json.loads(path.read_text(encoding='utf-8'))


def run(*args):
    result = subprocess.run(args, capture_output=True, text=True, timeout=30)
    require(result.returncode == 0, 'runtime inspection failed')
    return result.stdout.strip()


def runtime():
    pid = int(run('systemctl', 'show', SERVICE, '--value', '-p', 'MainPID'))
    require(pid > 0, 'service is not running')
    proc = Path('/proc') / str(pid)
    argv = [entry.decode() for entry in (proc / 'cmdline').read_bytes().split(b'\0') if entry]
    web = Path(argv[argv.index('--web') + 1])
    require(web.is_absolute() and web.resolve() == web and web.is_dir(), 'invalid live static directory')
    units = [run('systemctl', 'show', SERVICE, '--value', '-p', 'FragmentPath')]
    units += run('systemctl', 'show', SERVICE, '--value', '-p', 'DropInPaths').split()
    return {'pid': pid, 'binary': str((proc / 'exe').resolve()), 'binary_sha256': sha(proc / 'exe'),
            'web': str(web), 'cmdline_sha256': sha(proc / 'cmdline'),
            'environment_sha256': sha(proc / 'environ'), 'units': {name: sha(name) for name in units}}


def verify_runtime(manifest):
    facts = runtime()
    expected = manifest['baseline']
    for key in ('binary', 'binary_sha256', 'web'):
        require(facts[key] == expected[key], 'runtime changed from the selected backend release')
    require(sha(Path(expected['manifest'])) == expected['manifest_sha256'], 'backend release manifest changed')
    return facts


def verify_package(release):
    manifest = load(release / 'manifest.json')
    identity = {key: value for key, value in manifest.items() if key != 'release_id'}
    require(object_sha(identity) == manifest['release_id'] == release.name, 'release identity mismatch')
    require(SHA.fullmatch(release.name), 'invalid release id')
    require(file_map(release / 'dist') == manifest['dist'], 'frontend artifact changed')
    require(sha(release / 'deploy.py') == manifest['remote_script_sha256'], 'publisher changed')
    require(sha(release / 'evidence.json') == manifest['evidence_sha256'], 'validation evidence changed')
    require(sha(release / 'source.json') == manifest['source_manifest_sha256'], 'source manifest changed')
    for name in manifest['dist']:
        path = PurePosixPath(name)
        require(not path.is_absolute() and '..' not in path.parts and '\\' not in name, 'invalid asset path')
        require(name in ('index.html', 'favicon.svg') or name.startswith('assets/'), 'unexpected public file')
    require('index.html' in manifest['dist'], 'missing frontend entry point')
    return manifest


def health(expected_index, files):
    with urllib.request.urlopen(BASE_URL + '/health', timeout=15) as response:
        require(response.status == 200 and json.load(response).get('status') == 'ok', 'health check failed')
    with urllib.request.urlopen(BASE_URL + '/', timeout=15) as response:
        require(hashlib.sha256(response.read()).hexdigest() == expected_index, 'served frontend differs')
    for name, digest in files.items():
        if name == 'index.html':
            continue
        with urllib.request.urlopen(BASE_URL + '/' + name, timeout=15) as response:
            require(hashlib.sha256(response.read()).hexdigest() == digest, 'served static asset differs')


def preflight(release, manifest):
    facts = verify_runtime(manifest)
    web = Path(facts['web'])
    before = file_map(web)
    require(before == manifest['baseline']['frontend'], 'live frontend changed before preflight')
    for name, digest in manifest['dist'].items():
        if name != 'index.html' and name in before:
            require(before[name] == digest, 'existing asset name has different content')
    health(before['index.html'], {})
    result = {'runtime': facts, 'frontend': before, 'release_id': manifest['release_id'], 'at': time.time()}
    save(release / 'preflight.json', result)
    return {'passed': True, **result}


def activate(release, manifest):
    before = load(release / 'preflight.json')
    require(before['release_id'] == manifest['release_id'], 'preflight belongs to another release')
    facts = verify_runtime(manifest)
    require(facts == before['runtime'], 'runtime changed after preflight')
    web = Path(facts['web'])
    require(file_map(web) == before['frontend'], 'live frontend changed after preflight')
    journal = {'release_id': manifest['release_id'], 'runtime': facts, 'at': time.time(),
               'original_frontend': before['frontend'], 'published_frontend': manifest['dist'],
               'static_overlay_of': manifest['baseline']['manifest'],
               'backend_manifest_unchanged': True, 'old_assets_retained': True,
               'restart_required': False, 'status': 'preparing'}
    require(not (release / 'journal.json').exists(), 'release already has an activation journal')
    write_atomic(release / 'original-index.html', (web / 'index.html').read_bytes())
    save(release / 'journal.json', journal)
    switched = False
    try:
        for name, digest in manifest['dist'].items():
            if name == 'index.html':
                continue
            target = web / name
            if target.exists():
                require(sha(target) == digest, 'static file collision during activation')
            else:
                write_atomic(target, (release / 'dist' / name).read_bytes())
            require(sha(target) == digest, 'copied static asset differs')
        require(verify_runtime(manifest) == facts, 'runtime changed while staging assets')
        require(sha(web / 'index.html') == before['frontend']['index.html'], 'entry point changed during activation')
        write_atomic(web / 'index.html', (release / 'dist/index.html').read_bytes())
        switched = True
        health(manifest['dist']['index.html'], manifest['dist'])
        require(verify_runtime(manifest) == facts, 'runtime changed during activation')
        journal.update(status='active', activated_at=time.time(), pid_unchanged=True)
        save(release / 'journal.json', journal)
    except Exception:
        if switched:
            require(sha(web / 'index.html') == manifest['dist']['index.html'], 'concurrent index change prevents automatic rollback')
            write_atomic(web / 'index.html', (release / 'original-index.html').read_bytes())
        journal.update(status='rolled_back' if switched else 'not_activated', failed_at=time.time())
        save(release / 'journal.json', journal)
        raise
    return journal


def rollback(release, manifest):
    journal = load(release / 'journal.json')
    facts = verify_runtime(manifest)
    require(facts == journal['runtime'], 'runtime changed since publication')
    web = Path(facts['web'])
    require(journal['status'] == 'active', 'release is not active')
    require(sha(web / 'index.html') == manifest['dist']['index.html'], 'another frontend is active')
    require(sha(release / 'original-index.html') == journal['original_frontend']['index.html'], 'original index changed')
    write_atomic(web / 'index.html', (release / 'original-index.html').read_bytes())
    health(journal['original_frontend']['index.html'], {})
    journal.update(status='rolled_back', rolled_back_at=time.time())
    save(release / 'journal.json', journal)
    return journal


def main():
    import fcntl
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=['preflight', 'activate', 'rollback', 'status'])
    parser.add_argument('--release', required=True)
    args = parser.parse_args()
    release = Path(args.release)
    require(release.resolve() == release and release.parent == ROOT / 'ui-releases', 'unexpected release directory')
    with (ROOT / '.deployment.lock').open('a+b') as lock:
        fcntl.flock(lock.fileno(), fcntl.LOCK_EX)
        manifest = verify_package(release)
        if args.action == 'status':
            facts = verify_runtime(manifest)
            index = sha(Path(facts['web']) / 'index.html')
            active = index == manifest['dist']['index.html']
            health(index, manifest['dist'] if active else {})
            result = {'runtime': facts, 'index_sha256': index, 'release_active': active,
                      'journal': load(release / 'journal.json') if (release / 'journal.json').exists() else None}
        else:
            result = globals()[args.action](release, manifest)
        print(json.dumps(result, indent=2, sort_keys=True))


if __name__ == '__main__':
    main()
