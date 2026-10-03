"""Install an isolated official Node.js LTS runtime for the main node's Codex MCPs."""
import argparse
import json
import shlex

from main_node_codex import connect, run


REMOTE_SCRIPT = r'''
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import platform
import re
import subprocess
import tarfile
import urllib.parse


def fetch(url, path=None):
    parts = urllib.parse.urlsplit(url)
    if parts.scheme != 'https' or parts.hostname != 'nodejs.org':
        raise RuntimeError('Only the official Node.js HTTPS origin is allowed')
    command = ['curl', '--fail', '--silent', '--show-error', '--location',
               '--proto', '=https', '--tlsv1.2', '--connect-timeout', '15',
               '--max-time', '300' if path else '45', url]
    if path:
        command.extend(['--output', str(path)])
    return subprocess.check_output(command, timeout=310 if path else 55)


if platform.system() != 'Linux' or platform.machine() != 'x86_64':
    raise RuntimeError('This installer is limited to Linux x86_64')
releases = json.loads(fetch('https://nodejs.org/dist/index.json'))
release = next(row for row in releases if row.get('lts') and 'linux-x64' in row['files'])
version = release['version']
if not re.fullmatch(r'v[0-9]+\.[0-9]+\.[0-9]+', version):
    raise RuntimeError('Unexpected official release version')
root = Path('/opt') / ('codex-node-' + version)
runtime = root / 'runtime'
marker = root / '.codex-node-runtime.json'
archive_name = 'node-' + version + '-linux-x64.tar.xz'
origin = 'https://nodejs.org/dist/' + version + '/'
checksums = fetch(origin + 'SHASUMS256.txt').decode('ascii')
matches = [line.split()[0] for line in checksums.splitlines()
           if len(line.split()) == 2 and line.split()[1] == archive_name]
if len(matches) != 1 or not re.fullmatch(r'[a-f0-9]{64}', matches[0]):
    raise RuntimeError('Official checksum entry is missing or ambiguous')
expected = matches[0]
metadata = {'source': origin + archive_name, 'version': version,
            'lts': release['lts'], 'sha256': expected, 'runtime': str(runtime)}

# Never replace any existing executable or adopt an unrelated installation.
for name in ('node', 'npm', 'npx'):
    link = Path('/usr/local/bin') / name
    target = runtime / 'bin' / name
    if os.path.lexists(link) and (not link.is_symlink() or os.readlink(link) != str(target)):
        raise RuntimeError('Existing /usr/local/bin/' + name + ' will not be changed')
if root.exists():
    if not marker.is_file() or json.loads(marker.read_text()) != metadata:
        raise RuntimeError('Existing target is not a completed matching installation: ' + str(root))
else:
    root.mkdir(mode=0o755)
    archive = root / archive_name
    fetch(origin + archive_name, archive)
    actual = hashlib.sha256(archive.read_bytes()).hexdigest()
    if actual != expected:
        raise RuntimeError('Downloaded archive does not match official SHA256')
    (root / 'SHASUMS256.txt').write_text(checksums, encoding='ascii')
    prefix = archive_name[:-7]
    with tarfile.open(archive, 'r:xz') as bundle:
        for entry in bundle:
            member = PurePosixPath(entry.name)
            if member.is_absolute() or '..' in member.parts or member.parts[0] != prefix:
                raise RuntimeError('Unexpected archive member path')
            if entry.ischr() or entry.isblk() or entry.isfifo():
                raise RuntimeError('Unexpected archive device member')
            if entry.issym() or entry.islnk():
                destination = member.parent / entry.linkname if entry.issym() else PurePosixPath(entry.linkname)
                normalized = os.path.normpath(str(destination))
                if normalized != prefix and not normalized.startswith(prefix + '/'):
                    raise RuntimeError('Archive link escapes runtime directory')
    runtime.mkdir(mode=0o755)
    subprocess.run(['tar', '-xJf', str(archive), '--strip-components=1', '-C', str(runtime)], check=True)
    installed = subprocess.check_output([str(runtime / 'bin/node'), '--version'], text=True).strip()
    if installed != version:
        raise RuntimeError('Extracted Node.js version does not match release')
    marker.write_text(json.dumps(metadata, indent=2) + '\n', encoding='ascii')

for name in ('node', 'npm', 'npx'):
    link = Path('/usr/local/bin') / name
    if not os.path.lexists(link):
        link.symlink_to(runtime / 'bin' / name)
environment = dict(os.environ, PATH='/usr/local/bin:' + os.environ.get('PATH', '/usr/bin:/bin'))
versions = {name: subprocess.check_output(['/usr/local/bin/' + name, '--version'],
                                          env=environment, text=True).strip()
            for name in ('node', 'npm', 'npx')}
result = dict(metadata, verified_versions=versions)
if os.environ.get('CODEX_NODE_VERIFY_MCP') == '1':
    environment.update(NPM_CONFIG_CACHE=str(root / 'npm-cache'),
                       NPM_CONFIG_UPDATE_NOTIFIER='false')
    command = ['/usr/local/bin/npx', '--yes', '@playwright/mcp@latest']
    result['playwright_mcp_version'] = subprocess.check_output(
        command + ['--version'], env=environment, text=True, timeout=100).strip()
    help_output = subprocess.check_output(command + ['--help'], env=environment,
                                          text=True, timeout=40)
    if 'Usage: Playwright MCP' not in help_output:
        raise RuntimeError('Playwright MCP did not return its expected help output')
    result['playwright_mcp_help_verified'] = True
print(json.dumps(result, sort_keys=True))
'''


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--verify-mcp', action='store_true',
                        help='Check Playwright MCP version and help without starting a browser')
    args = parser.parse_args()
    client = connect()
    try:
        command = 'python3 -c ' + shlex.quote(REMOTE_SCRIPT)
        if args.verify_mcp:
            command = 'CODEX_NODE_VERIFY_MCP=1 ' + command
        print(json.dumps(json.loads(run(client, command, timeout=420)), indent=2))
    finally:
        client.close()


if __name__ == '__main__':
    main()
