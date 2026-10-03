"""Diagnostic Go overlay for the exact 956 shutdown fixture; local Docker only.

Three isolated variants compare original fixture, persisted admin key only, and
persisted key plus default settings. Original/candidate Go files remain intact.
Expected failures are retained, never treated as a passing release test.
"""
import hashlib
import io
import json
from pathlib import Path
import sys
import tarfile

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / 'tools'))
import gateway_local_race as runner

FROZEN = ROOT / '.local/joint-candidate/20260918T024841.878837Z/local-source'
FROZEN_SHA = '956fc90be91a922ac1593b283102bc5149674dac337564e6023479ec8e60a6f9'
TESTS = ['TestShutdownFixtureDiagnosticOriginal', 'TestShutdownFixtureDiagnosticPersistedKey',
         'TestShutdownFixtureDiagnosticKeyAndDefaults']
LAST_OUTPUT = None
ORIGINAL_SNAPSHOT = runner.snapshot
ORIGINAL_DOCKER = runner.docker


def rewrite(raw):
    text = raw.decode()
    old = 'func TestPGShutdownPersistsRefreshAfterClientDisconnectAndSIGTERM(t *testing.T) {'
    if text.count(old) != 1:
        raise RuntimeError('Expected original shutdown fixture signature changed')
    text = text.replace(old, 'func shutdownFixtureDiagnostic(t *testing.T, fixtureMode string) {', 1)
    old = '\tif err := NewPGSettingsStore(s.DB).Set(Settings{AdminAPI: true}); err != nil {'
    replacement = '''\tif fixtureMode != "original" {
        if _, err := s.DB.initializeAdminKey(context.Background(), shutdownTestAdminKey); err != nil {
            t.Fatal("synthetic admin initialization failed")
        }
    }
    fixtureSettings := Settings{AdminAPI: true}
    if fixtureMode == "key-and-defaults" {
        fixtureSettings = defaultSettings()
        fixtureSettings.AdminAPI = true
    }
    if err := NewPGSettingsStore(s.DB).Set(fixtureSettings); err != nil {'''
    if text.count(old) != 1:
        raise RuntimeError('Expected original fixture settings changed')
    text = text.replace(old, replacement, 1)
    old = '\t\t\t_, _ = io.Copy(io.Discard, response.Body)'
    replacement = '''            body, _ := io.ReadAll(io.LimitReader(response.Body, 8192))
            var result struct { Error struct { Code string `json:"code"`; Type string `json:"type"`; Message string `json:"message"` } `json:"error"` }
            _ = json.Unmarshal(body, &result)
            category := "other"
            switch result.Error.Message {
            case "invalid API key": category = "invalid_api_key"
            case "HTTP JSON transport disabled": category = "http_json_disabled"
            case "HTTP JSON gateway is disabled": category = "http_json_disabled"
            case "automatic credential refresh is disabled": category = "automatic_refresh_disabled"
            }
            // Status and fixed classifications only: no headers or response body.
            t.Logf("SHUTDOWN_FIXTURE_HTTP mode=%s status=%d error_category=%s", fixtureMode, response.StatusCode, category)'''
    if text.count(old) != 1:
        raise RuntimeError('Expected HTTP response discard boundary changed')
    text = text.replace(old, replacement, 1)
    for name, mode in zip(TESTS, ('original', 'persisted-key', 'key-and-defaults')):
        text += '\nfunc ' + name + '(t *testing.T) { shutdownFixtureDiagnostic(t, "' + mode + '") }\n'
    return text.encode()


def snapshot(output, manifest, source):
    global LAST_OUTPUT
    LAST_OUTPUT = output
    archive = ORIGINAL_SNAPSHOT(output, manifest, source)
    changed = rewrite((source / 'accounts_shutdown_test.go').read_bytes())
    overlay = json.dumps({'Replace': {'/work/accounts_shutdown_test.go': '/work/shutdown-fixture-diagnostic.go.txt'}}).encode()
    additions = {'shutdown-fixture-diagnostic.go.txt': changed, 'shutdown-fixture-overlay.json': overlay}
    temporary = output / 'diagnostic.tar.gz'
    with tarfile.open(archive) as original, tarfile.open(temporary, 'w:gz') as target:
        for member in original:
            target.addfile(member, original.extractfile(member))
        for name, raw in additions.items():
            member = tarfile.TarInfo(name)
            member.size, member.mode = len(raw), 0o644
            target.addfile(member, io.BytesIO(raw))
            (output / name).write_bytes(raw)
    temporary.replace(archive)
    path = output / 'source-manifest.json'
    saved = json.loads(path.read_text())
    saved['archive_sha256'] = hashlib.sha256(archive.read_bytes()).hexdigest()
    saved['diagnostic_overlay_files'] = {name: hashlib.sha256(raw).hexdigest() for name, raw in additions.items()}
    saved['baseline_source_sha256'] = FROZEN_SHA
    saved['diagnostic_only_not_release_acceptance'] = True
    path.write_text(json.dumps(saved, indent=2) + '\n')
    return archive


def docker(*arguments, **kwargs):
    values = list(arguments)
    if values and values[0] == 'run' and any('go test -race ' in value for value in values):
        values = [value.replace('go test -race ', 'go test -overlay=/work/shutdown-fixture-overlay.json -race ') for value in values]
    return ORIGINAL_DOCKER(*values, **kwargs)


def selected(manifest, source):
    if manifest['sha256'] != FROZEN_SHA or source != FROZEN:
        raise RuntimeError('Diagnostic only accepts the immutable joint 956 source')
    return TESTS


def summarize(output):
    log = (output / 'race.log').read_text()
    diagnostics = []
    outcomes = {}
    for line in log.splitlines():
        try:
            event = json.loads(line)
        except ValueError:
            continue
        if not isinstance(event, dict):
            continue
        if event.get('Test') and event.get('Action') in ('pass', 'fail', 'skip'):
            outcomes[event['Test']] = event['Action']
        if 'SHUTDOWN_FIXTURE_HTTP ' in event.get('Output', ''):
            diagnostics.append(event['Output'].strip())
    path = output / 'diagnostic-summary.json'
    path.write_text(json.dumps({'schema': 'codex-agent-shutdown-fixture-diagnostic-v1',
        'baseline_source_sha256': FROZEN_SHA, 'diagnostic_overlay': True,
        'candidate_acceptance': False, 'first_http_responses': diagnostics,
        'test_outcomes': outcomes, 'race_log_sha256': hashlib.sha256((output / 'race.log').read_bytes()).hexdigest(),
        'production_requests': 0, 'source_files_changed': False,
        'runner_sha256': hashlib.sha256(Path(__file__).read_bytes()).hexdigest()}, indent=2) + '\n')
    print(json.dumps({'diagnostics': diagnostics, 'report': str(path)}))


if __name__ == '__main__':
    if len(sys.argv) == 3 and sys.argv[1] == '--summarize':
        output = Path(sys.argv[2]).resolve()
        if not output.is_relative_to(ROOT / '.local/source-20260918'):
            raise RuntimeError('Summary path must be a project-local isolated run')
        summarize(output)
        raise SystemExit(0)
    runner.snapshot = snapshot
    runner.docker = docker
    runner.selected_tests = selected
    sys.argv = [sys.argv[0], '--run', '--expected-go-hash', FROZEN_SHA, '--source-dir', str(FROZEN)]
    result = runner.main()
    if LAST_OUTPUT is not None:
        summarize(LAST_OUTPUT)
    raise SystemExit(result)
