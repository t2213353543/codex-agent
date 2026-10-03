#!/usr/bin/env python3
"""Run a real repair/feature/resume task through an isolated official Codex CLI."""
from __future__ import annotations

from collections import Counter
from decimal import Decimal, ROUND_HALF_UP
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import secrets
import shutil
import subprocess
import sys
import tempfile
import time

from codex_cli_smoke import (SmokeFailure, isolated_environment, parse_args, parse_events,
                             prepare_readable_workspace, run_process, validate_settings, write_config)


LEDGER = '''def totals(rows):
    result = {}
    for row in rows:
        currency = row["currency"]
        result[currency] = result.get(currency, 0.0) + float(row["amount"])
    return {key: str(round(value, 2)) for key, value in result.items()}
'''
PUBLIC_TEST = '''import unittest
from ledger import totals

class LedgerTests(unittest.TestCase):
    def test_money(self):
        self.assertEqual(totals([
            {"currency": "USD", "amount": "0.105"},
            {"currency": "USD", "amount": "0.105"},
            {"currency": "EUR", "amount": "-1.235"},
        ]), {"USD": "0.22", "EUR": "-1.24"})

    def test_empty(self):
        self.assertEqual(totals([]), {})

if __name__ == "__main__":
    unittest.main()
'''
README = '''# Transaction ledger
Python standard library only. `ledger.totals(rows)` returns a mapping from currency
to a two-decimal string. Each decimal-string amount is rounded to cents with
ROUND_HALF_UP before summation. Negative amounts are refunds. Keep zero totals.
Do not modify the input rows. Existing tests in test_ledger.py must remain intact.
Run tests with `python -m unittest discover -v`.
'''


def reference(rows: list[dict], monthly: bool) -> dict:
    result = {}
    for row in rows:
        amount = Decimal(row["amount"]).quantize(Decimal("0.01"), rounding=ROUND_HALF_UP)
        group = result
        if monthly:
            timestamp = datetime.fromisoformat(row["timestamp"].replace("Z", "+00:00"))
            month = timestamp.astimezone(timezone.utc).strftime("%Y-%m")
            group = result.setdefault(month, {})
        currency = row["currency"]
        group[currency] = group.get(currency, Decimal(0)) + amount
    if monthly:
        return {month: {key: format(value, ".2f") for key, value in group.items()}
                for month, group in result.items()}
    return {key: format(value, ".2f") for key, value in result.items()}


def oracle(workdir: Path, environment: dict, monthly: bool) -> bool:
    rows = [{"currency": ("USD", "EUR", "JPY")[i % 3],
             "amount": str(Decimal(secrets.randbelow(200001) - 100000) / 1000),
             "timestamp": ("2026-03-01T00:20:00+02:00", "2026-02-28T23:50:00-03:00",
                           "2026-01-31T23:59:00Z", "2026-02-15T12:00:00+00:00")[i % 4]}
            for i in range(200)]
    # Execute the implementation independently of model-authored tests. Only
    # generated fixture rows enter stdin; no hidden files or credentials exist.
    program = '''import copy, json, sys
sys.path.insert(0, sys.argv[1])
import ledger
rows = json.load(sys.stdin)
original = copy.deepcopy(rows)
result = getattr(ledger, sys.argv[2])(rows)
assert rows == original
assert getattr(ledger, sys.argv[2])([]) == {}
print(json.dumps(result, sort_keys=True))
'''
    safe_env = {name: value for name, value in environment.items()
                if name.upper() not in {"CODEX_SMOKE_PROVIDER_KEY", "CODEX_HOME"}}
    result = run_process([sys.executable, "-I", "-B", "-c", program, str(workdir),
                          "monthly_totals" if monthly else "totals"],
                         json.dumps(rows), workdir, safe_env, 15)
    try:
        return (result["returncode"] == 0 and not result["timed_out"]
                and json.loads(result["stdout"]) == reference(rows, monthly))
    except (ValueError, TypeError):
        return False


def inspect_turn(result: dict, name: str, expected_thread: str | None,
                 memory_marker: str | None = None) -> tuple[dict, str | None]:
    check = {"name": name, "passed": False,
             **{key: result[key] for key in ("returncode", "timed_out", "elapsed_seconds")}}
    try:
        events = parse_events(result["stdout"])
    except SmokeFailure:
        check["failure"] = "invalid_cli_event_stream"
        return check, None
    threads = [event.get("thread_id") for event in events if event["type"] == "thread.started"]
    thread = threads[0] if len(threads) == 1 and isinstance(threads[0], str) else None
    items = [event.get("item", {}) for event in events if event["type"] == "item.completed"]
    commands = [item for item in items if item.get("type") == "command_execution"]
    assertions = {
        "process_exited_successfully": result["returncode"] == 0 and not result["timed_out"],
        "thread_started": bool(thread),
        "turn_completed": any(event["type"] == "turn.completed" for event in events),
        "no_error_events": not any(event["type"] in ("turn.failed", "error") for event in events),
    }
    if expected_thread:
        assertions["same_thread_resumed"] = thread == expected_thread
    if memory_marker:
        messages = [item.get("text", "") for item in items if item.get("type") == "agent_message"]
        assertions["history_marker_recalled"] = bool(messages) and messages[-1].strip() == memory_marker
        assertions["memory_without_tools"] = all(item.get("type") in ("agent_message", "reasoning") for item in items)
    else:
        assertions["successful_shell_execution"] = any(
            item.get("status") == "completed" and item.get("exit_code") == 0 for item in commands)
        assertions["test_suite_executed"] = any(
            item.get("exit_code") == 0 and "unittest" in item.get("command", "")
            and "OK" in item.get("aggregated_output", "") and "Ran " in item.get("aggregated_output", "")
            for item in commands)
    check.update({"assertions": assertions, "completed_command_count": len(commands),
                  "item_counts": dict(Counter(item.get("type", "unknown") for item in items)),
                  "passed": all(assertions.values())})
    # No stdout/stderr, command strings, replies, thread IDs or error text are exported.
    return check, thread


def run(args, key: str, report: dict) -> None:
    executable = shutil.which(args.codex)
    if not executable:
        raise SmokeFailure("cli_not_found")
    with tempfile.TemporaryDirectory(prefix="codex-gateway-workload-") as temporary:
        # Resolve Windows 8.3 TEMP paths before passing the cwd to the CLI; its
        # writable-root comparison must see the same canonical workspace path.
        root = Path(temporary).resolve()
        temp_home, workdir = root / "codex-home", root / "workspace"
        temp_home.mkdir()
        workdir.mkdir()
        prepare_readable_workspace(root, workdir)
        if os.name == "nt":
            # Only this disposable synthetic workspace needs write access for
            # the restricted token. The parent stays traverse/read-only and
            # this grant cannot reach the sibling private CLI home.
            writable = subprocess.run(
                ["icacls.exe", str(workdir), "/grant", "*S-1-1-0:(OI)(CI)(M)"],
                capture_output=True, timeout=10, check=False)
            if writable.returncode != 0:
                raise SmokeFailure("temporary_workspace_write_acl_failed")
        (workdir / "ledger.py").write_text(LEDGER, encoding="utf-8")
        (workdir / "test_ledger.py").write_text(PUBLIC_TEST, encoding="utf-8")
        (workdir / "README.md").write_text(README, encoding="utf-8")
        environment = isolated_environment(temp_home, key)
        environment.update({"LANG": "en_US.UTF-8", "LC_ALL": "en_US.UTF-8", "TZ": "America/New_York"})
        write_config(temp_home / "config.toml", args)
        config = (temp_home / "config.toml").read_text(encoding="utf-8")
        config = config.replace('sandbox_mode = "read-only"', 'sandbox_mode = "workspace-write"')
        if os.name == "nt":
            # This workload only needs its workspace as a writable root;
            # keep inherited temporary directories outside the write policy.
            config += ('\n[sandbox_workspace_write]\n'
                       'exclude_tmpdir_env_var = true\nexclude_slash_tmp = true\n')
        (temp_home / "config.toml").write_text(config, encoding="utf-8")
        version = run_process([executable, "--version"], None, workdir, environment, 15)
        report["cli_version"] = version["stdout"].strip()
        if version["returncode"] != 0 or not report["cli_version"].startswith("codex-cli "):
            raise SmokeFailure("cli_version_failed")
        marker = "ledger_" + secrets.token_hex(16)
        flags = ["--json", "--skip-git-repo-check", "--ignore-rules"]
        prompts = [
            ("Repair ledger.py according to README.md. Use Decimal correctly and preserve inputs. "
             "Keep test_ledger.py and README.md unchanged; you may add tests in test_extra.py. "
             "Run python -m unittest discover -v and ensure all tests pass. Work only in the current "
             "workspace; no network or external files. Remember this marker only in conversation "
             "history, never in a file: " + marker + ". Briefly report the fix and test result."),
            ("Extend ledger.py with monthly_totals(rows), returning {UTC YYYY-MM: {currency: two-decimal string}}. "
             "Each row also has an ISO-8601 timestamp with Z or an explicit UTC offset. Group by UTC month, "
             "including timestamps crossing month boundaries after conversion. Use the same per-transaction "
             "ROUND_HALF_UP rule; keep zero totals and do not mutate inputs. Preserve totals. Add tests in "
             "test_extra.py, leaving test_ledger.py and README.md unchanged. Run python -m unittest discover -v. "
             "Work only in this workspace; no network or external files. Briefly report changes and tests."),
            ("Return exactly the marker I asked you to remember in our first turn. Use conversation history "
             "only, no tools, files, quotes or other text."),
        ]
        thread = None
        for index, prompt in enumerate(prompts):
            command = ([executable, "exec", *flags, "-"] if thread is None else
                       [executable, "exec", "resume", *flags, thread, "-"])
            result = run_process(command, prompt, workdir, environment, args.timeout)
            check, actual_thread = inspect_turn(result, ("repair", "feature", "resume_memory")[index],
                                                 thread, marker if index == 2 else None)
            if index < 2:
                check["assertions"]["independent_money_oracle"] = oracle(workdir, environment, False)
                if index == 1:
                    check["assertions"]["independent_utc_month_oracle"] = oracle(workdir, environment, True)
                check["assertions"]["source_changed"] = (workdir / "ledger.py").read_text(encoding="utf-8") != LEDGER
                check["assertions"]["fixtures_unchanged"] = (
                    (workdir / "test_ledger.py").read_text(encoding="utf-8") == PUBLIC_TEST
                    and (workdir / "README.md").read_text(encoding="utf-8") == README)
                check["assertions"]["marker_not_saved_in_source"] = all(
                    marker not in path.read_text(encoding="utf-8") for path in workdir.glob("*.py"))
                check["passed"] = all(check["assertions"].values())
            report["checks"].append(check)
            if not check["passed"]:
                raise SmokeFailure("workload_" + check["name"] + "_failed")
            thread = actual_thread
        report["passed"] = True


def main() -> int:
    report = {"schema_version": 1, "passed": False, "checks": [],
              "scope": "real_cli_repair_feature_resume", "locale_env": "en_US.UTF-8",
              "timezone_env": "America/New_York", "os_locale_or_geolocation_changed": False}
    started = time.monotonic()
    args = None
    try:
        args = parse_args()
        run(args, validate_settings(args), report)
    except SmokeFailure as exc:
        report["failure"] = str(exc)
    except Exception as exc:
        report["failure"] = "unexpected_" + type(exc).__name__
    report["elapsed_seconds"] = round(time.monotonic() - started, 3)
    rendered = json.dumps(report, indent=2)
    if args and args.output:
        args.output.write_text(rendered + "\n", encoding="utf-8")
    print(rendered)
    return 0 if report["passed"] else 1


if __name__ == "__main__":
    sys.exit(main())
