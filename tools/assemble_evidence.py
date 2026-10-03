"""Merge exact-source acceptance evidence without relabeling older snapshots.

Reports are supplied oldest to newest. A later valid observation for the same
check supersedes the earlier observation and both remain in provenance. Missing,
hash-mismatched, unbound or stale results stay blocked. This command never runs
tests, invents passes, migrates data, or deploys a service.
"""
from __future__ import annotations

import argparse
from datetime import datetime, timezone
import json
from pathlib import Path

from acceptance import API_REQUIREMENTS, EXTERNAL_REQUIREMENTS, ROOT, sha, source_manifest

DERIVED = {
    "integration.identity": ("api.jwt_collision", "api.same_name_identity", "api.identity_update"),
    "integration.import_formats": ("api.jwt_collision", "api.import_formats", "api.import_invalid_line"),
    "integration.version_conflict": ("api.version_conflict",),
    "integration.ui": ("frontend.unit", "frontend.browser"),
}
REFRESH_TESTS = {
    "TestPGAccountRefreshSerializesOnlySameAccount",
    "TestPGEnsureReadyRefreshesDifferentAccountsConcurrently",
    "TestPGAccountRefreshCASAndRestartRecovery",
    "TestPGShutdownPersistsRefreshAfterClientDisconnectAndSIGTERM",
}


def artifact(path: Path) -> dict:
    path = path.resolve()
    try:
        name = path.relative_to(ROOT).as_posix()
    except ValueError:
        name = str(path)
    return {"path": name, "sha256": sha(path)}


def validate_artifacts(items) -> list[dict]:
    result = []
    for item in items:
        path = Path(item.get("path", ""))
        if not path.is_absolute():
            path = ROOT / path
        if not path.is_file() or item.get("sha256") != sha(path):
            raise ValueError("an attached artifact is missing or its SHA256 changed")
        result.append(artifact(path))
    return result


def unique_artifacts(checks) -> list[dict]:
    return list({(a["path"], a["sha256"]): a for check in checks for a in check.get("artifacts", [])}.values())


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--report", type=Path, action="append", default=[], help="source-bound report; repeat in chronological order")
    parser.add_argument("--output", type=Path, default=ROOT / ".local/integration/evidence.json")
    parser.add_argument("--init-attestations", type=Path, help="write a blocked template for independently observed deployment/performance/isolation evidence")
    args = parser.parse_args()
    snapshot = source_manifest()
    if args.init_attestations:
        if args.init_attestations.exists():
            parser.error("attestation template already exists; refusing to overwrite observations")
        template = {"source_sha256": snapshot["sha256"], "recorded_at": datetime.now(timezone.utc).isoformat(),
                    "instructions": "Fill only after observing this exact source snapshot. Attach raw report artifacts and their SHA256. Older source reports must be rerun, never relabeled.",
                    "checks": [{"id": name, "status": "blocked", "detail": text + "; observation pending", "artifacts": []}
                               for name, text in EXTERNAL_REQUIREMENTS.items() if name not in DERIVED]}
        args.init_attestations.parent.mkdir(parents=True, exist_ok=True)
        args.init_attestations.write_text(json.dumps(template, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
        print(f"Blocked attestation template: {args.init_attestations}")
        return 0
    required = ["go.test", "go.race", *API_REQUIREMENTS, *EXTERNAL_REQUIREMENTS]
    merged = {name: {"id": name, "status": "blocked", "detail": "no matching-source observation supplied", "artifacts": []} for name in required}
    provenance, histories = [], []
    for path in args.report:
        entry = {"report": str(path), "accepted": False}
        try:
            report = json.loads(path.read_text(encoding="utf-8-sig"))
            expected = report.get("source_sha256") or report.get("source", {}).get("sha256")
            entry.update(report_sha256=sha(path), source_sha256=expected)
            if expected != snapshot["sha256"]:
                entry["reason"] = "older/different or absent source fingerprint; retained as historical evidence only"
                histories.append(entry)
                continue
            checks = report.get("checks", [])
            if not isinstance(checks, list):
                entry["reason"] = "raw measurement report needs explicit reviewed check mapping; no pass inferred"
                histories.append(entry)
                continue
            report_artifact = artifact(path)
            accepted = []
            for check in checks:
                identifier = check.get("id")
                if not identifier:
                    continue
                status = check.get("status")
                if status not in ("pass", "fail", "blocked"):
                    status = "blocked"
                detail = check.get("detail", "")
                try:
                    attached = validate_artifacts(check.get("artifacts", []))
                    # A local Runner report binds its own command log separately.
                    if check.get("log") and check.get("log_sha256"):
                        attached += validate_artifacts([{"path": check["log"], "sha256": check["log_sha256"]}])
                    if status == "pass" and not detail:
                        raise ValueError("passing observation has no detail")
                except ValueError as error:
                    status, detail, attached = "blocked", str(error), []
                current = {"id": identifier, "status": status, "detail": detail or "observation pending",
                           "artifacts": [report_artifact, *attached], "observed_in": str(path)}
                if identifier in merged and merged[identifier]["status"] != "blocked":
                    provenance.append({"id": identifier, "superseded": merged[identifier], "replaced_by": str(path)})
                merged[identifier] = current
                accepted.append(identifier)
            entry.update(accepted=True, checks=accepted)
        except (OSError, ValueError, TypeError) as error:
            entry["reason"] = str(error)
        provenance.append(entry)
    for identifier, components in DERIVED.items():
        if merged.get(identifier, {}).get("status") in ("pass", "fail"):
            continue
        checks = [merged.get(name, {}) for name in components]
        if all(c.get("status") == "pass" for c in checks):
            merged[identifier] = {"id": identifier, "status": "pass",
                                  "detail": "All required concrete checks passed: " + ", ".join(components),
                                  "artifacts": unique_artifacts(checks), "derived_from": list(components)}
    race = merged.get("go.race", {})
    if race.get("status") == "pass" and merged["integration.refresh_durability"]["status"] == "blocked":
        passed = set()
        for item in race.get("artifacts", []):
            path = Path(item["path"])
            if not path.is_absolute():
                path = ROOT / path
            if path.suffix != ".log":
                continue
            for line in path.read_text(encoding="utf-8").splitlines():
                try:
                    event = json.loads(line)
                except ValueError:
                    continue
                if not isinstance(event, dict):
                    continue
                if event.get("Action") == "pass" and event.get("Test"):
                    passed.add(event["Test"])
        if REFRESH_TESTS.issubset(passed):
            merged["integration.refresh_durability"] = {
                "id": "integration.refresh_durability", "status": "pass",
                "detail": "Race run passed real PG concurrent-account, credential CAS/restart and child-process disconnect+SIGTERM persistence tests",
                "artifacts": race["artifacts"], "passed_tests": sorted(REFRESH_TESTS)}
    if source_manifest()["sha256"] != snapshot["sha256"]:
        parser.error("source changed during evidence assembly; freeze and rerun")
    checks = list(merged.values())
    counts = {name: sum(c["status"] == name for c in checks) for name in ("pass", "fail", "blocked")}
    result = {"source_sha256": snapshot["sha256"], "assembled_at": datetime.now(timezone.utc).isoformat(),
              "status": "fail" if counts["fail"] else "blocked" if counts["blocked"] else "pass", "counts": counts,
              "checks": checks, "provenance": provenance, "historical_reports": histories,
              "migration_status": "not_performed_by_assembly", "deployment_status": "not_performed_by_assembly"}
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(result, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    print(f"Evidence {result['status'].upper()}: {counts}; historical reports excluded={len(histories)}; {args.output}")
    return 1 if counts["fail"] else 2 if counts["blocked"] else 0


if __name__ == "__main__":
    raise SystemExit(main())
