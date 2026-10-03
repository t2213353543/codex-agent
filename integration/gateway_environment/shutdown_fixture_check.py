"""Verify the formal shutdown fixture on 956 production source, in local Docker.

Only accounts_shutdown_test.go differs from the immutable verified source.
The shared generic race selector and existing source archives are unchanged.
"""
from datetime import datetime, timezone
import hashlib
import json
from pathlib import Path
import secrets
import sys

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "tools"))
import gateway_local_race as runner

BASE = ROOT / ".local/joint-candidate/20260918T024841.878837Z/local-source"
BASE_SHA = "956fc90be91a922ac1593b283102bc5149674dac337564e6023479ec8e60a6f9"
NAME = "accounts_shutdown_test.go"
TEST = "TestPGShutdownPersistsRefreshAfterClientDisconnectAndSIGTERM"


def freeze():
    baseline = runner.go_manifest(BASE)
    if baseline["sha256"] != BASE_SHA:
        raise RuntimeError("The immutable verified 956 baseline has changed")
    formal = (ROOT / NAME).read_bytes()
    old = '\tif err := NewPGSettingsStore(s.DB).Set(Settings{AdminAPI: true}); err != nil {'
    new = '''\tif _, err := s.DB.initializeAdminKey(context.Background(), shutdownTestAdminKey); err != nil {
\t\tt.Fatal("cannot initialize synthetic admin key")
\t}
\tsettings := defaultSettings()
\tsettings.AdminAPI = true
\tif err := NewPGSettingsStore(s.DB).Set(settings); err != nil {'''
    text = (BASE / NAME).read_text(encoding="utf-8")
    if text.count(old) != 1 or text.replace(old, new, 1) != formal.decode().replace("\r\n", "\n"):
        raise RuntimeError("Formal fixture differs beyond the reviewed initialization repair")
    stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%S").lower() + "-" + secrets.token_hex(3)
    output = ROOT / ".local/source-20260918" / ("shutdown-fixture-fixed-" + stamp)
    source = output / "source"
    source.mkdir(parents=True, exist_ok=False)
    for relative, expected in baseline["files"].items():
        raw = (BASE / relative).read_bytes()
        if hashlib.sha256(raw).hexdigest() != expected:
            raise RuntimeError("Baseline changed during snapshot")
        destination = source / relative
        destination.parent.mkdir(parents=True, exist_ok=True)
        destination.write_bytes(formal if relative == NAME else raw)
    frozen = runner.go_manifest(source)
    changes = [name for name in frozen["files"] if frozen["files"][name] != baseline["files"].get(name)]
    if changes != [NAME]:
        raise RuntimeError("Unexpected source changes in fixture-only snapshot")
    archive = runner.snapshot(output, frozen, source)
    manifest = {
        "schema": "codex-agent-formal-shutdown-fixture-repair-v1",
        "baseline_go_sha256": BASE_SHA, "go_source_sha256": frozen["sha256"],
        "formal_test_sha256": hashlib.sha256(formal).hexdigest(),
        "changed_files": changes, "production_go_changed": False,
        "test_assertions_unchanged": True, "source_directory": str(source.relative_to(ROOT)),
        "archive": str(archive.relative_to(ROOT)), "archive_sha256": hashlib.sha256(archive.read_bytes()).hexdigest(),
        "selected_test": TEST,
    }
    (output / "fixture-manifest.json").write_text(json.dumps(manifest, indent=2) + "\n", encoding="utf-8")
    print(json.dumps(manifest), flush=True)
    return source, frozen["sha256"]


def main():
    source, digest = freeze()
    original_selector = runner.selected_tests
    original_argv = sys.argv[:]

    def selected(manifest, root):
        if root != source or manifest["sha256"] != digest:
            raise RuntimeError("Dedicated fixture run received another source")
        if "func " + TEST + "(t *testing.T)" not in (source / NAME).read_text():
            raise RuntimeError("The formal lifecycle test is absent")
        return [TEST]

    try:
        # This process-local selector chooses one explicitly reported test.
        # No broad-suite selector file or archived test list is modified.
        runner.selected_tests = selected
        sys.argv = [sys.argv[0], "--run", "--source-dir", str(source), "--expected-go-hash", digest]
        return runner.main()
    finally:
        runner.selected_tests = original_selector
        sys.argv = original_argv
        if runner.go_manifest(BASE)["sha256"] != BASE_SHA:
            raise RuntimeError("Original immutable baseline changed")


if __name__ == "__main__":
    raise SystemExit(main())
