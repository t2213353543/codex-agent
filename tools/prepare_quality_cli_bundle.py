#!/usr/bin/env python3
"""Install the exact official Linux x64 CLI reference package, with its host.

No model endpoint, auth store, or credential is used. This is intentionally not a
general installer: artifact URL, version, target, and SHA-512 are pinned.
"""
from __future__ import annotations

import argparse
import base64
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import platform
import shutil
import sys
import tarfile
import tempfile
import urllib.error
import urllib.request

VERSION = "0.155.0"
TARGET = "x86_64-unknown-linux-musl"
URL = "https://registry.npmjs.org/@openai/codex/-/codex-0.155.0-linux-x64.tgz"
SHA512 = "CiXkVTy4ERdFSXXP8n98/iOyVUCOYFPQ8cuwgeOEEbLhe1LnlQbgH/H8ZeuxO9WXKIZLJg0x626UgR9axY2zSA=="
MAX_ARCHIVE = 512 * 1024 * 1024
MAX_UNPACKED = 2 * 1024 * 1024 * 1024
PREFIX = ("package", "vendor", TARGET)


class InstallFailure(Exception):
    pass


class Parser(argparse.ArgumentParser):
    def error(self, message):
        raise InstallFailure("invalid_arguments")


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        raise InstallFailure("unexpected_artifact_redirect")


def archive_digest(path):
    digest = hashlib.sha512()
    total = 0
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            total += len(chunk)
            if total > MAX_ARCHIVE:
                raise InstallFailure("artifact_too_large")
            digest.update(chunk)
    if base64.b64encode(digest.digest()).decode("ascii") != SHA512:
        raise InstallFailure("official_artifact_integrity_mismatch")
    return total


def download(path):
    # The artifact's digest is checked independently of transport configuration.
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
    with opener.open(URL, timeout=60) as response, path.open("xb") as output:
        if response.status != 200:
            raise InstallFailure("official_artifact_download_failed")
        total = 0
        while True:
            chunk = response.read(1024 * 1024)
            if not chunk:
                break
            total += len(chunk)
            if total > MAX_ARCHIVE:
                raise InstallFailure("artifact_too_large")
            output.write(chunk)


def extract_bundle(archive, destination):
    """Validate the entire tar index, then copy regular target assets explicitly."""
    with tarfile.open(archive, "r:gz") as package:
        members = package.getmembers()
        if len(members) > 10000 or sum(member.size for member in members) > MAX_UNPACKED:
            raise InstallFailure("artifact_expansion_limit")
        selected = []
        seen = set()
        for member in members:
            path = PurePosixPath(member.name)
            if (path.is_absolute() or ".." in path.parts or "\\" in member.name
                    or not path.parts or path.parts[0] != "package"):
                raise InstallFailure("unsafe_artifact_member_path")
            if path in seen:
                raise InstallFailure("duplicate_artifact_member")
            seen.add(path)
            if not member.isfile() and not member.isdir():
                raise InstallFailure("artifact_links_or_special_files_forbidden")
            if tuple(path.parts[:3]) == PREFIX and len(path.parts) > 3:
                selected.append((member, Path(*path.parts[3:])))
        if not selected:
            raise InstallFailure("official_target_assets_missing")
        destination.mkdir(mode=0o700)
        for member, relative in selected:
            output = destination / relative
            if member.isdir():
                output.mkdir(mode=0o755, parents=True, exist_ok=True)
                continue
            output.parent.mkdir(mode=0o755, parents=True, exist_ok=True)
            with package.extractfile(member) as source, output.open("xb") as target:
                shutil.copyfileobj(source, target, 1024 * 1024)
            os.chmod(output, 0o755 if member.mode & 0o111 else 0o644)
    manifest_path = destination / "codex-package.json"
    try:
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
    except (OSError, ValueError) as exc:
        raise InstallFailure("official_package_manifest_missing_or_invalid") from exc
    if (not isinstance(manifest, dict) or manifest.get("version") != VERSION
            or manifest.get("target") != TARGET or manifest.get("entrypoint") != "bin/codex"
            or manifest.get("layoutVersion") != 1):
        raise InstallFailure("official_package_manifest_mismatch")
    cli = destination / "bin" / "codex"
    hosts = [destination / "codex-resources" / "codex-code-mode-host",
             destination / "bin" / "codex-code-mode-host"]
    host = next((path for path in hosts if path.is_file()), None)
    if not cli.is_file() or host is None:
        raise InstallFailure("official_cli_or_host_missing")
    if not os.access(cli, os.X_OK) or not os.access(host, os.X_OK):
        raise InstallFailure("official_cli_or_host_not_executable")
    hashes = {}
    for path in (cli, host, manifest_path):
        digest = hashlib.sha256()
        with path.open("rb") as handle:
            for chunk in iter(lambda: handle.read(1024 * 1024), b""):
                digest.update(chunk)
        hashes[path.relative_to(destination).as_posix()] = digest.hexdigest()
    return {"package_version": VERSION, "target": TARGET, "asset_sha256": hashes,
            "extracted_regular_files": sum(member.isfile() for member, _ in selected)}


def execute(argv=None):
    parser = Parser(description=__doc__)
    parser.add_argument("--output-dir", required=True, type=Path, help="new absolute package directory")
    parser.add_argument("--archive", type=Path, help="optional local copy; identical pinned SHA-512 required")
    args = parser.parse_args(argv)
    if sys.platform != "linux" or platform.machine().lower() not in {"x86_64", "amd64"}:
        raise InstallFailure("requires_linux_x64")
    if not args.output_dir.is_absolute():
        raise InstallFailure("output_directory_must_be_absolute")
    if os.path.lexists(args.output_dir):
        raise InstallFailure("output_directory_already_exists")
    args.output_dir.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="quality-cli-install-", dir=args.output_dir.parent) as temporary:
        work = Path(temporary)
        archive = args.archive or work / "official.tgz"
        if args.archive is None:
            download(archive)
        archive_bytes = archive_digest(archive)
        staged = work / "package"
        report = extract_bundle(archive, staged)
        report.update({"status": "prepared", "source_url": URL, "archive_sha512_base64": SHA512,
                       "archive_bytes": archive_bytes, "integrity_verified": True,
                       "cli": str(args.output_dir / "bin" / "codex"), "model_requests": 0})
        (staged / "quality-bundle-manifest.json").write_text(
            json.dumps(report, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
        if os.path.lexists(args.output_dir):
            raise InstallFailure("output_directory_already_exists")
        staged.rename(args.output_dir)
    return report


def main(argv=None):
    try:
        report = execute(argv)
    except InstallFailure as exc:
        print(json.dumps({"status": "prepare_error", "error": str(exc)}))
        return 2
    except (OSError, ValueError, tarfile.TarError, urllib.error.URLError):
        print(json.dumps({"status": "prepare_error", "error": "official_bundle_preparation_failed"}))
        return 2
    print(json.dumps(report, ensure_ascii=False))
    return 0


if __name__ == "__main__":
    sys.exit(main())
