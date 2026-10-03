"""Shared source-layout rules for current sources and immutable legacy snapshots.

Only explicit source directories are enumerated. Credentials, runtime data,
private captures, dependency caches, and build outputs are never discovered.
"""
from pathlib import Path, PurePosixPath
import re

SOURCE_DIRECTORIES = ("internal", "cmd", "migrations", "native-transport/tests")
EXCLUDED_PARTS = {".git", ".local", "data", "vendor", "node_modules", "dist", "__pycache__"}
EMBEDDED_BACKEND_ASSETS = {"internal/server/account_window_prices.json",
                           "internal/server/appserver_source_templates_0155.json"}
BRIDGE_FIXTURES = {"tools/loophub_bridge_contract_q01.py", "tools/loophub_bridge_reauth_contract.py",
                   "tools/codex_cli_smoke.py", "tools/codex_cli_workload.py", "deploy/loophub_codex_bridge.py"}


def backend_source_paths(root: Path) -> list[Path]:
    root = Path(root)
    paths = list(root.glob("*.go")) + [root / "go.mod", root / "go.sum"]
    paths.extend(root / name for name in EMBEDDED_BACKEND_ASSETS)
    for directory in SOURCE_DIRECTORIES:
        folder = root / directory
        if folder.is_dir():
            paths.extend(path for path in folder.rglob("*") if path.is_file()
                         and not EXCLUDED_PARTS.intersection(path.relative_to(root).parts)
                         and (path.suffix == ".go" or directory == "migrations" and path.suffix == ".sql"))
    result = sorted(set(path for path in paths if path.is_file()))
    if any(path.is_symlink() or any(parent.is_symlink() for parent in path.parents if parent != root and root in parent.parents)
           for path in result):
        raise ValueError("Backend source cannot contain symlinks")
    return result


def valid_backend_source_path(name: str) -> bool:
    if not isinstance(name, str) or not name or any(c in name for c in "\\:*?[]"):
        return False
    path = PurePosixPath(name)
    if path.is_absolute() or ".." in path.parts or str(path) != name or EXCLUDED_PARTS.intersection(path.parts):
        return False
    if name in {"go.mod", "go.sum"} | BRIDGE_FIXTURES | EMBEDDED_BACKEND_ASSETS:
        return True
    if len(path.parts) == 1:
        return path.suffix == ".go"
    return ((path.parts[0] in {"internal", "cmd", "migrations"} or name.startswith("native-transport/tests/"))
            and (path.suffix == ".go" or path.parts[0] == "migrations" and path.suffix == ".sql"))


def server_source_dir(root: Path) -> Path:
    root = Path(root)
    folder = root / "internal/server"
    return folder if folder.is_dir() else root


def server_package(root: Path) -> str:
    return "./internal/server" if server_source_dir(root) != Path(root) else "."


def server_source(root: Path, filename: str) -> Path:
    return server_source_dir(root) / filename


def migration_runner_key(files: dict) -> str:
    candidates = [name for name in ("internal/server/database.go", "database.go") if name in files]
    if len(candidates) != 1:
        raise ValueError("Source manifest must identify exactly one database migration runner")
    return candidates[0]


def package_overlay(template: Path, output: Path, root: Path) -> Path:
    """Materialize a test overlay for the target snapshot's actual Go package."""
    raw = Path(template).read_text(encoding="utf-8")
    package = "server" if server_package(root) != "." else "main"
    raw, count = re.subn(r"(?m)^package (?:main|server)[ \t]*$", "package " + package, raw, count=1)
    if count != 1:
        raise ValueError("Overlay template has no supported package declaration")
    output = Path(output)
    output.write_text(raw, encoding="utf-8")
    return output
