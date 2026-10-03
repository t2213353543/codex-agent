#!/usr/bin/env python3
"""Reclassify retained CLI safe events locally; never issue a live request."""
from __future__ import annotations

import argparse
import json
from pathlib import Path
import sys

from quality_official_cli_probe import ProbeFailure, executable_sha256, summarize_prior_safe_evidence, write_private


def reclassify(directory):
    report_path = directory / "report.json"
    events_path = directory / "events.safe.jsonl"
    report = json.loads(report_path.read_text(encoding="utf-8"))
    events = [json.loads(line) for line in events_path.read_text(encoding="utf-8").splitlines() if line.strip()]
    summary = summarize_prior_safe_evidence(report, events)
    summary["source_report_sha256"] = executable_sha256(report_path)
    summary["source_events_sha256"] = executable_sha256(events_path)
    output = directory / "summary_from_prior_safe_evidence.json"
    write_private(output, json.dumps(summary, ensure_ascii=False, indent=2) + "\n")
    return summary


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("directories", type=Path, nargs="+")
    args = parser.parse_args(argv)
    try:
        for directory in args.directories:
            summary = reclassify(directory)
            print(json.dumps({"status": summary["status"], "new_live_request": False,
                              "source_report_sha256": summary["source_report_sha256"]}))
    except (ProbeFailure, OSError, ValueError, KeyError, TypeError):
        print(json.dumps({"status": "reclassification_failed"}))
        return 2
    return 0


if __name__ == "__main__":
    sys.exit(main())
