"""Build and verify the console navigation, usage, accounts and operations changes."""
from pathlib import Path
import json
import os
import shutil
import subprocess
import sys


def main():
    root = Path(__file__).resolve().parents[1]
    frontend = root / "frontend"
    output = root / ".local" / "console-polish-verification"
    output.mkdir(parents=True, exist_ok=True)
    npm = shutil.which("npm.cmd" if os.name == "nt" else "npm")
    node, go = shutil.which("node"), shutil.which("go")
    if not all((npm, node, go)):
        raise SystemExit("npm, node and go must be available on PATH")
    checks = [
        ("frontend-unit", frontend, [npm, "test"]),
        ("frontend-build", frontend, [npm, "run", "build"]),
        ("go", root, [go, "test", "./..."]),
        ("navigation-browser", frontend, [node, "tests/console-browser.mjs"]),
        ("usage-browser", frontend, [node, "tests/run-model-consistency.mjs", str(frontend / "dist")]),
        ("accounts-browser", frontend, [node, "tests/account-official-usage-browser.mjs"]),
        ("operations-browser", frontend, [node, "tests/operations-browser.mjs"]),
    ]
    results = []
    for name, cwd, command in checks:
        print(f"Running {name}", flush=True)
        log = output / f"{name}.log"
        with log.open("w", encoding="utf-8") as stream:
            result = subprocess.run(command, cwd=cwd, stdout=stream, stderr=subprocess.STDOUT, check=False)
        results.append({"check": name, "passed": result.returncode == 0, "log": str(log)})
        print(f"{'PASS' if result.returncode == 0 else 'FAIL'} {name}: {log}", flush=True)
        if result.returncode:
            print(log.read_text(encoding="utf-8", errors="replace")[-6000:])
            break
    passed = len(results) == len(checks) and all(item["passed"] for item in results)
    (output / "report.json").write_text(json.dumps({
        "passed": passed,
        "database_integration_configured": bool(os.environ.get("TEST_DATABASE_URL")),
        "browser_data": "synthetic API fixtures; no production writes",
        "checks": results,
    }, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    return 0 if passed else 1


if __name__ == "__main__":
    sys.exit(main())
