"""Single entry point for Codex Agent acceptance, never a deployment command.

Run: python tools/acceptance.py
Release gate: python tools/acceptance.py --profile release --environment-file
  .local/integration/environment.json --evidence .local/integration/evidence.json

Exit 0 = all applicable requirements passed; 1 = failure; 2 = blocked.
Reports are under .local/acceptance/<UTC-run-id> and latest.json/latest.md.
The old production browser, UI smoke and inference scripts are never invoked.
"""
from __future__ import annotations

from project_layout import backend_source_paths

import argparse
from datetime import datetime, timezone
import gzip
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import shutil
import signal
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
HANDOFFS = {"delivery.backend": "docs/BACKEND-HANDOFF.md",
            "delivery.frontend": "frontend/FRONTEND-HANDOFF.md",
            "delivery.contract": "docs/API-CONTRACT.md"}
EXTERNAL_REQUIREMENTS = {
    "database.isolation": "PG database/role grants, Redis namespace and bounded resources verified",
    "database.migrations": "fresh migration and repeat application succeed on isolated PostgreSQL",
    "database.rollback": "failed migration/startup recovery tested without changing other services",
    "integration.identity": "different JWTs, same-name distinct identities and explicit identity update",
    "integration.import_formats": "JSON, JSONL, tokens and exact invalid line numbers",
    "integration.delete_atomicity": "fault injection preserves entity and relationships together",
    "integration.version_conflict": "concurrent page edits receive a version conflict",
    "integration.key_rotation": "persistence failure and lost response retain recoverable admin access",
    "integration.refresh_durability": "concurrent refresh, disconnect and process exit preserve new tokens",
    "integration.restart": "logs, aggregates and account state match across graceful restart",
    "integration.statistics": "cache ratios, configured timezone, lifetime scope and log ordering",
    "integration.proxy_status": "403/407/429 diagnostics cannot be reported as success",
    "integration.redis_failure": "cache reads fall back and session authentication fails closed",
    "integration.ui": "unauthorized page, tooltip text, masking, four themes, viewport/filter/page/batch/import/copy",
    "performance.latency": "representative account/log counts with list/log/overview sample count and P50/P95",
    "performance.sql": "EXPLAIN (ANALYZE, BUFFERS), N+1, pools, OFFSET, COUNT and cache invalidation inspected",
    "database.retention_rebuild": "retention removes only eligible detail; aggregate rebuild remains consistent",
    "migration.dry_run": "JSON/JSONL source counts, identities, relationships, expiry and statistics reconciled",
    "deployment.rehearsal": "idempotent start/switch/rollback and single credential refresher demonstrated",
}
FORBIDDEN_SCRIPTS = ("tools/browser-test.cjs", "tools/ui-smoke.cjs", "tools/smoke.py")
API_REQUIREMENTS = ("api.health", "api.unauthorized", "api.invalid_auth", "api.cookie_session",
                    "api.jwt_collision", "api.import_formats", "api.import_invalid_line", "api.same_name_identity",
                    "api.identity_update", "api.version_conflict", "api.proxy_password", "api.account_cursor",
                    "api.sensitive_fields", "api.cleanup")
GENERATED_DIRECTORIES = {"__pycache__", "results", "artifacts", "test-results", "node_modules", "dist", ".local", ".git", "data"}


def load_module(name: str, path: Path):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def sha(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def source_manifest() -> dict:
    paths = backend_source_paths(ROOT)
    for directory in ("frontend/src", "frontend/tests", "frontend/scripts", "frontend/public", "internal", "cmd", "migrations", "integration", "tools", "deploy"):
        folder = ROOT / directory
        if folder.exists():
            paths.extend(p for p in folder.rglob("*") if p.is_file() and not GENERATED_DIRECTORIES.intersection(p.parts)
                         and (directory == "frontend/public" or p.suffix.lower() in
                              (".go", ".py", ".cjs", ".js", ".mjs", ".vue", ".css", ".svg",
                               ".sql", ".json", ".md", ".sh", ".ps1", ".service", ".yaml", ".yml", ".conf")))
    paths.extend(ROOT / p for p in ("frontend/package.json", "frontend/package-lock.json",
                                  "frontend/vite.config.js", "frontend/index.html", "tools/acceptance.py"))
    files = {p.relative_to(ROOT).as_posix(): sha(p) for p in sorted(set(paths)) if p.is_file()}
    digest = hashlib.sha256(json.dumps(files, sort_keys=True).encode()).hexdigest()
    return {"sha256": digest, "files": files}


def bundle_metrics() -> dict:
    directory = ROOT / "frontend/dist"
    if not (directory / "index.html").is_file():
        return {"available": False}
    assets = []
    for path in sorted(directory.rglob("*")):
        if path.is_file() and path.suffix in (".js", ".css", ".html"):
            raw = path.read_bytes()
            assets.append({"path": path.relative_to(directory).as_posix(), "bytes": len(raw),
                           "gzip_bytes": len(gzip.compress(raw, mtime=0)), "sha256": hashlib.sha256(raw).hexdigest()})
    html = (directory / "index.html").read_text(encoding="utf-8")
    initial = set(re.findall(r'(?:src|href)=["\']/?(assets/[^"\']+)["\']', html))
    # Follow static imports; dynamic import() stays route-lazy and is not counted.
    pending = list(initial)
    while pending:
        name = pending.pop()
        path = directory / name
        if path.suffix != ".js" or not path.is_file():
            continue
        for reference in re.findall(r'\b(?:import|export)\s*(?:[^;\n]*?\sfrom\s*)?["\']([^"\']+)["\']', path.read_text(encoding="utf-8")):
            resolved = (path.parent / reference).resolve()
            if resolved.is_file() and directory.resolve() in resolved.parents:
                relative = resolved.relative_to(directory.resolve()).as_posix()
                if relative not in initial:
                    initial.add(relative)
                    pending.append(relative)
    return {"available": True, "assets": assets,
            "total_js_bytes": sum(a["bytes"] for a in assets if a["path"].endswith(".js")),
            "total_js_gzip_bytes": sum(a["gzip_bytes"] for a in assets if a["path"].endswith(".js")),
            "total_css_bytes": sum(a["bytes"] for a in assets if a["path"].endswith(".css")),
            "initial_static_resources": sorted(initial),
            "initial_static_bytes": sum(a["bytes"] for a in assets if a["path"] in initial),
            "initial_static_gzip_bytes": sum(a["gzip_bytes"] for a in assets if a["path"] in initial),
            "note": "Static graph measurement; actual first-screen requests and usability require browser evidence."}


class Runner:
    def __init__(self, args):
        self.args, self.checks, self.secrets = args, [], []
        self.started = datetime.now(timezone.utc)
        self.run_id = self.started.strftime("%Y%m%dT%H%M%S") + f"-{os.getpid()}"
        self.output = ROOT / ".local/acceptance" / self.run_id
        self.output.mkdir(parents=True, exist_ok=False)
        self.environment, self.env = {}, os.environ.copy()
        self.process, self.service_log, self.binary_sha256 = None, None, None
        self.before_source = source_manifest()
        self.before_bundle = bundle_metrics()

    def sanitize(self, text: str) -> str:
        for secret in self.secrets:
            if secret and len(secret) > 6:
                text = text.replace(secret, "[REDACTED]")
        text = re.sub(r'(?i)(postgres(?:ql)?|redis(?:s)?)://[^\s"\']+', r'\1://[REDACTED]', text)
        text = re.sub(r'\beyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\b', '[REDACTED-JWT]', text)
        return re.sub(r'\b(?:ca_|sk-)[A-Za-z0-9_-]{20,}\b', '[REDACTED-KEY]', text)

    def add(self, identifier: str, status: str, detail: str, **extra):
        assert status in ("pass", "fail", "blocked")
        check = {"id": identifier, "status": status, "detail": self.sanitize(detail), **extra}
        self.checks.append(check)
        print(f"[{status.upper()}] {identifier}: {check['detail'][:250]}", flush=True)

    def remote_check(self, identifier: str) -> bool:
        if not self.args.use_remote_checks:
            return False
        reason = "matching remote evidence is unavailable"
        try:
            if not self.args.evidence:
                raise ValueError("--use-remote-checks requires --evidence")
            evidence = json.loads(self.args.evidence.read_text(encoding="utf-8-sig"))
            if evidence.get("source_sha256") != self.before_source["sha256"]:
                raise ValueError("remote evidence source_sha256 differs from this source snapshot")
            item = next((c for c in evidence.get("checks", []) if c.get("id") == identifier), None)
            if not item or item.get("status") != "pass" or not item.get("detail"):
                raise ValueError("remote pass result with specific detail is missing")
            artifacts = item.get("artifacts", [])
            if not artifacts:
                raise ValueError("remote evidence has no artifacts")
            for artifact in artifacts:
                path = Path(artifact.get("path", ""))
                if not path.is_absolute():
                    path = ROOT / path
                if not path.is_file() or artifact.get("sha256") != sha(path):
                    raise ValueError("remote evidence artifact is missing or its SHA256 changed")
            self.add(identifier, "pass", item["detail"], artifacts=artifacts,
                     execution="verified_remote", source_sha256=evidence["source_sha256"])
            return True
        except (ValueError, OSError) as error:
            reason = str(error)
        self.add(identifier, "blocked", reason + "; remote execution cannot substitute this check")
        return True

    def command(self, identifier: str, command: list[str], timeout: int = 600, env=None) -> bool:
        executable = shutil.which(command[0])
        if not executable:
            self.add(identifier, "blocked", f"executable unavailable: {command[0]}")
            return False
        print(f"[RUN] {identifier}", flush=True)
        started = time.perf_counter()
        try:
            result = subprocess.run([executable, *command[1:]], cwd=ROOT, env=env or self.env,
                                    stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                                    timeout=timeout, text=True, encoding="utf-8", errors="replace")
            code, output = result.returncode, result.stdout
        except subprocess.TimeoutExpired as error:
            code = -1
            output = error.stdout or b""
            if isinstance(output, bytes):
                output = output.decode("utf-8", "replace")
            output += f"\nTimed out after {timeout} seconds"
        except OSError as error:
            self.add(identifier, "blocked", str(error))
            return False
        log = self.output / (identifier + ".log")
        log.write_text(self.sanitize(output), encoding="utf-8")
        status = "pass" if code == 0 else "fail"
        skipped = []
        if code == 0 and identifier in ("go.test", "go.race"):
            for line in output.splitlines():
                try:
                    event = json.loads(line)
                except ValueError:
                    continue
                if not isinstance(event, dict):
                    continue
                if event.get("Action") == "skip":
                    skipped.append(event.get("Test") or event.get("Package"))
        if code == 0 and identifier == "frontend.browser" and re.search(r"\b[1-9]\d* skipped\b", output):
            skipped.append("browser runner reported skipped tests")
        if skipped:
            status = "blocked"
        self.add(identifier, status, f"exit={code}; skipped={len(skipped)}; log={log.name}",
                 command=command, exit_code=code, duration_seconds=round(time.perf_counter() - started, 3),
                 log=str(log.relative_to(ROOT)), log_sha256=sha(log), skipped_tests=skipped)
        return status == "pass"

    def setup_environment(self):
        if not self.args.environment_file:
            self.add("environment.manifest", "blocked", "isolated environment manifest not supplied")
            return
        try:
            self.environment = json.loads(self.args.environment_file.read_text(encoding="utf-8-sig"))
            contract = load_module("acceptance_contract", ROOT / "integration/tests/contract.py")
            base = self.args.base_url or self.environment.get("base_url", "")
            contract.validate_environment(self.environment, base)
            self.args.base_url = base
            values = {key: value for key, value in self.environment.items() if key.isupper()}
            values.update(self.environment.get("env", {}))
            for key, value in values.items():
                if not isinstance(value, (str, int, float)):
                    raise ValueError(f"environment variable {key} is not a scalar")
                self.env[key] = str(value)
                if any(part in key.upper() for part in ("KEY", "TOKEN", "PASSWORD", "URL", "DSN")):
                    self.secrets.append(str(value))
            # TEST_ aliases allow the integration environment to coexist with
            # normal application settings without overwriting the user's shell.
            for name in ("DATABASE_URL", "REDIS_URL"):
                if "TEST_" + name in self.env:
                    self.env[name] = self.env["TEST_" + name]
            self.add("environment.manifest", "pass", "isolated synthetic loopback target declared; secrets kept out of reports",
                     manifest_sha256=sha(self.args.environment_file))
        except (ValueError, OSError) as error:
            self.add("environment.manifest", "blocked", str(error))
            self.environment = {}

    def start_backend(self):
        if not self.args.binary:
            return
        if not self.environment:
            self.add("backend.start", "blocked", "managed backend requires the isolated environment manifest")
            return
        executable = self.args.binary.resolve()
        if not executable.is_file():
            self.add("backend.start", "blocked", "explicit backend binary does not exist")
            return
        address = urllib.parse.urlparse(self.args.base_url)
        port = address.port or (443 if address.scheme == "https" else 80)
        # Never attach to an existing listener while claiming the supplied
        # binary was tested. The caller may instead omit --binary explicitly.
        import socket
        with socket.socket() as probe:
            probe.settimeout(0.5)
            occupied = probe.connect_ex((address.hostname, port)) == 0
        if occupied:
            self.add("backend.start", "blocked", "target port already has a listener; cannot attribute it to the supplied binary")
            self.environment = {}
            return
        private_data = self.output / "service-data"
        private_data.mkdir()
        key = self.env.get("TEST_ADMIN_KEY", "") or self.env.get("ADMIN_KEY", "")
        if self.args.admin_key_file:
            key = self.args.admin_key_file.read_text(encoding="utf-8").strip()
        if not key:
            import secrets
            key = "ca_integration_" + secrets.token_hex(24)
        self.secrets.append(key)
        self.env["TEST_ADMIN_KEY"] = key
        self.env["ADMIN_KEY"] = key
        (private_data / "client-key").write_text(key, encoding="utf-8")
        os.chmod(private_data / "client-key", 0o600)
        if self.args.backend_args_file:
            arguments = json.loads(self.args.backend_args_file.read_text(encoding="utf-8-sig"))
            if not isinstance(arguments, list) or not all(isinstance(a, str) for a in arguments):
                raise ValueError("backend args file must be a JSON array of strings")
        else:
            arguments = ["--data", str(private_data), "--listen", f"{address.hostname}:{port}",
                         "--web", str(ROOT / "frontend/dist")]
        self.binary_sha256 = sha(executable)
        # Temporary service stdout may contain secrets, so it is only copied
        # into the public report directory after applying the same redaction.
        import tempfile
        self.service_log = tempfile.TemporaryFile()
        self.process = subprocess.Popen([str(executable), *arguments], cwd=ROOT, env=self.env,
                                        stdout=self.service_log, stderr=subprocess.STDOUT,
                                        creationflags=subprocess.CREATE_NEW_PROCESS_GROUP if os.name == "nt" else 0)
        ready, deadline = False, time.monotonic() + self.args.startup_timeout
        while time.monotonic() < deadline and self.process.poll() is None:
            try:
                with urllib.request.urlopen(self.args.base_url + "/health", timeout=1) as response:
                    ready = response.status == 200
                if ready:
                    break
            except (OSError, urllib.error.URLError):
                pass
            time.sleep(0.15)
        self.add("backend.start", "pass" if ready else "fail",
                 "explicit binary became healthy on the isolated loopback port" if ready else "explicit binary exited or did not become healthy before timeout",
                 binary_sha256=self.binary_sha256)
        if not ready:
            self.environment = {}

    def stop_backend(self):
        if not self.process:
            return
        if self.process.poll() is None:
            if os.name == "nt":
                self.process.send_signal(signal.CTRL_BREAK_EVENT)
            else:
                self.process.send_signal(signal.SIGTERM)
            try:
                self.process.wait(timeout=60)
            except subprocess.TimeoutExpired:
                self.process.kill()
                self.process.wait(timeout=10)
                self.add("backend.stop", "fail", "managed test process did not exit in 60 seconds and was killed")
            else:
                self.add("backend.stop", "pass", "managed test process stopped; this alone does not validate restart/credential durability")
        self.service_log.seek(0)
        log = self.output / "backend.service.log"
        log.write_text(self.sanitize(self.service_log.read().decode("utf-8", "replace")), encoding="utf-8")
        self.service_log.close()

    def deliveries(self):
        for identifier, relative in HANDOFFS.items():
            path = ROOT / relative
            if path.is_file() and path.stat().st_size:
                self.add(identifier, "pass", relative, sha256=sha(path),
                         note="Artifact presence only; functional acceptance remains separately required.")
            else:
                self.add(identifier, "blocked", f"delivery pending: {relative}")

    def fixtures(self):
        module = load_module("acceptance_synthetic", ROOT / "integration/fixtures/synthetic.py")
        manifest = module.build(self.output / "fixtures", self.args.accounts, self.args.logs, self.args.anchor)
        self.add("fixtures.synthetic", "pass", f"{manifest['accounts']} accounts / {manifest['totals']['requests']} logs; invalid JWT signatures only",
                 manifest=str((self.output / "fixtures/manifest.json").relative_to(ROOT)))

    def local_checks(self):
        if not self.remote_check("go.test"):
            self.command("go.test", ["go", "test", "-json", "-count=1", "./..."])
        self.command("go.vet", ["go", "vet", "./..."])
        if not self.remote_check("go.race"):
            self.local_race()
        self.frontend_checks()

    def local_race(self):
        go = shutil.which("go")
        try:
            result = subprocess.run([go, "env", "-json", "GOOS", "GOARCH", "CGO_ENABLED", "CC"],
                                    capture_output=True, text=True, timeout=30, env=self.env)
            config = json.loads(result.stdout)
            if config.get("CGO_ENABLED") != "1":
                self.add("go.race", "blocked", f"CGO_ENABLED={config.get('CGO_ENABLED')}; race detector requires CGO and a C compiler", runtime=config)
            elif not shutil.which(config.get("CC", "cc").split()[0]):
                self.add("go.race", "blocked", "configured C compiler is unavailable", runtime=config)
            else:
                self.command("go.race", ["go", "test", "-json", "-race", "-count=1", "./..."], timeout=900)
        except (OSError, TypeError, ValueError, subprocess.TimeoutExpired) as error:
            self.add("go.race", "blocked", f"cannot inspect Go race runtime: {error}")

    def frontend_checks(self):
        npm = "npm.cmd" if os.name == "nt" else "npm"
        build_ok = self.command("frontend.build", [npm, "--prefix", "frontend", "run", "build"])
        package = json.loads((ROOT / "frontend/package.json").read_text(encoding="utf-8"))
        scripts = package.get("scripts", {})
        unit = scripts.get("test", "").replace("\\", "/")
        if unit and not any(name in unit for name in FORBIDDEN_SCRIPTS):
            self.command("frontend.unit", [npm, "--prefix", "frontend", "run", "test"])
        else:
            self.add("frontend.unit", "blocked", "isolated frontend unit test script has not been delivered")
        selected = self.args.frontend_test_script or next((s for s in ("test:mock", "test:acceptance", "test:browser") if s in scripts), "")
        script = scripts.get(selected, "").replace("\\", "/")
        if not selected or not script:
            self.add("frontend.browser", "blocked", "isolated mock browser test script has not been delivered")
        elif any(name in script for name in FORBIDDEN_SCRIPTS):
            self.add("frontend.browser", "blocked", f"{selected} still invokes a retired production/old-selector script")
        elif not build_ok:
            self.add("frontend.browser", "blocked", "frontend build must succeed before browser acceptance")
        else:
            env = self.env.copy()
            env["ACCEPTANCE_OUTPUT_DIR"] = str(self.output / "browser")
            env["ACCEPTANCE_SYNTHETIC_ONLY"] = "1"
            self.command("frontend.browser", [npm, "--prefix", "frontend", "run", selected], timeout=900, env=env)
        current = bundle_metrics()
        bundle = {"before_build": self.before_bundle, "after_build": current,
                  "review_baseline": {"source": "docs/history/2026-09/REVIEW-2026-09-17.md", "approx_large_js_chunks_bytes": [1130000, 1210000],
                                      "approx_combined_large_js_gzip_bytes": 768000, "approx_css_bytes": 382000}}
        (self.output / "bundle.json").write_text(json.dumps(bundle, indent=2), encoding="utf-8")
        self.add("frontend.bundle", "pass" if build_ok and current.get("available") else "blocked",
                 "bundle.json records raw/gzip totals, static entry resources and review baseline; browser result is separate")

    def api_checks(self):
        if self.args.use_remote_checks:
            for identifier in API_REQUIREMENTS:
                self.remote_check(identifier)
            return
        if not self.environment:
            self.add("api.contract", "blocked", "isolated environment not ready")
            return
        if not (ROOT / HANDOFFS["delivery.contract"]).is_file():
            self.add("api.contract", "blocked", "API contract has not been delivered")
            return
        key = self.env.get("TEST_ADMIN_KEY", "") or self.env.get("ADMIN_KEY", "")
        if self.args.admin_key_file:
            try:
                key = self.args.admin_key_file.read_text(encoding="utf-8").strip()
            except OSError as error:
                self.add("api.contract", "blocked", str(error))
                return
        self.secrets.append(key)
        contract = load_module("acceptance_contract", ROOT / "integration/tests/contract.py")
        try:
            for check in contract.run(self.environment, self.args.base_url, key):
                self.add(**{"identifier": check.pop("id"), **check})
        except Exception as error:
            self.add("api.contract", "fail", f"{type(error).__name__}: {error}")

    def external_evidence(self):
        evidence = {}
        if self.args.evidence:
            try:
                evidence = json.loads(self.args.evidence.read_text(encoding="utf-8-sig"))
            except (ValueError, OSError) as error:
                self.add("evidence.file", "fail", str(error))
        index = {item.get("id"): item for item in evidence.get("checks", [])}
        # Linux may fulfill the same race requirement when local Windows lacks
        # CGO. Preserve that local limitation inside the verified replacement.
        for identifier in ("go.test", "go.race"):
            alternative = index.get(identifier)
            if not (alternative and alternative.get("status") == "pass" and evidence.get("source_sha256") == self.before_source["sha256"]):
                continue
            artifacts = alternative.get("artifacts", [])
            good = bool(artifacts) and bool(alternative.get("detail"))
            for artifact in artifacts:
                path = Path(artifact.get("path", ""))
                if not path.is_absolute():
                    path = ROOT / path
                good = good and path.is_file() and artifact.get("sha256") == sha(path)
            old = next((check for check in self.checks if check["id"] == identifier), None)
            if good and old and old["status"] == "blocked":
                local_attempt = dict(old)
                old.update(status="pass", detail=self.sanitize(alternative["detail"]), artifacts=artifacts,
                           local_attempt=local_attempt, fulfilled_by="verified Linux race evidence")
                print(f"[PASS] {identifier}: source-bound Linux evidence fulfills the blocked Windows run", flush=True)
        for identifier, requirement in EXTERNAL_REQUIREMENTS.items():
            item = index.get(identifier)
            if not item:
                self.add(identifier, "blocked", requirement + "; evidence pending")
                continue
            if evidence.get("source_sha256") != self.before_source["sha256"]:
                self.add(identifier, "blocked", "evidence source_sha256 does not match this source snapshot")
                continue
            artifacts = item.get("artifacts", [])
            validated = []
            for artifact in artifacts:
                path = Path(artifact.get("path", ""))
                if not path.is_absolute():
                    path = ROOT / path
                if path.is_file() and artifact.get("sha256") == sha(path):
                    validated.append(artifact)
            status = item.get("status", "blocked")
            if status not in ("pass", "fail", "blocked"):
                status = "fail"
            if status == "pass" and (not artifacts or len(validated) != len(artifacts) or not item.get("detail")):
                self.add(identifier, "blocked", "passing evidence requires a detail and readable artifacts with matching SHA256")
                continue
            self.add(identifier, status, item.get("detail", requirement), artifacts=validated)

    def finish(self) -> int:
        after = source_manifest()
        self.add("source.stable", "pass" if after["sha256"] == self.before_source["sha256"] else "blocked",
                 "source unchanged during run" if after["sha256"] == self.before_source["sha256"] else "source changed while checks ran; rerun the final delivered snapshot")
        counts = {status: sum(c["status"] == status for c in self.checks) for status in ("pass", "fail", "blocked")}
        status = "fail" if counts["fail"] else "blocked" if counts["blocked"] else "pass"
        report = {"run_id": self.run_id, "started_at": self.started.isoformat(),
                  "completed_at": datetime.now(timezone.utc).isoformat(), "profile": self.args.profile,
                  "status": status, "counts": counts,
                  "release_ready": self.args.profile == "release" and status == "pass",
                  "migration_status": "not_performed_by_this_command", "deployment_status": "not_performed_by_this_command",
                  "source": self.before_source, "checks": self.checks}
        raw = json.dumps(report, ensure_ascii=False, indent=2) + "\n"
        (self.output / "report.json").write_text(raw, encoding="utf-8")
        lines = [f"# Codex Agent acceptance — {self.run_id}", "", f"Status: **{status.upper()}**; profile: `{self.args.profile}`.",
                 f"Pass: {counts['pass']}; fail: {counts['fail']}; blocked: {counts['blocked']}.",
                 f"Release ready: **{str(report['release_ready']).lower()}**. Migration/deployment: not performed by this command.",
                 "", f"Source SHA256: `{self.before_source['sha256']}`", "",
                 "| Check | Status | Evidence / limitation |", "|---|---|---|"]
        for check in self.checks:
            detail = check["detail"].replace("|", "\\|").replace("\n", " ")
            lines.append(f"| {check['id']} | {check['status']} | {detail} |")
        markdown = "\n".join(lines) + "\n"
        (self.output / "report.md").write_text(markdown, encoding="utf-8")
        (self.output.parent / "latest.json").write_text(raw, encoding="utf-8")
        (self.output.parent / "latest.md").write_text(markdown, encoding="utf-8")
        print(f"Report: {self.output / 'report.md'}", flush=True)
        return 1 if status == "fail" else 2 if status == "blocked" else 0


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--profile", choices=("local", "release"), default="local")
    parser.add_argument("--environment-file", type=Path)
    parser.add_argument("--base-url")
    parser.add_argument("--admin-key-file", type=Path)
    parser.add_argument("--binary", type=Path, help="start this explicit binary on the isolated loopback base URL, then stop it")
    parser.add_argument("--backend-args-file", type=Path, help="JSON string array of binary arguments; default uses a fresh run-local data directory")
    parser.add_argument("--startup-timeout", type=int, default=30)
    parser.add_argument("--frontend-test-script")
    parser.add_argument("--evidence", type=Path)
    parser.add_argument("--use-remote-checks", action="store_true", help="use source/artifact-hash verified remote Go/race/API results instead of rerunning them locally")
    parser.add_argument("--accounts", type=int, default=100)
    parser.add_argument("--logs", type=int, default=10000)
    parser.add_argument("--anchor", type=int, default=1789601400)
    parser.add_argument("--source-manifest", action="store_true", help="print current source fingerprints without running tests")
    args = parser.parse_args()
    if args.source_manifest:
        print(json.dumps(source_manifest(), indent=2))
        return 0
    runner = Runner(args)
    try:
        runner.setup_environment()
        runner.deliveries()
        runner.fixtures()
        runner.local_checks()
        runner.start_backend()
        runner.api_checks()
        runner.external_evidence()
    except KeyboardInterrupt:
        runner.add("runner.interrupted", "blocked", "run interrupted before completing all required checks")
    except Exception as error:
        runner.add("runner.exception", "fail", f"{type(error).__name__}: {error}")
    finally:
        try:
            runner.stop_backend()
        except Exception as error:
            runner.add("backend.stop", "fail", f"{type(error).__name__}: {error}")
    return runner.finish()


if __name__ == "__main__":
    sys.exit(main())
