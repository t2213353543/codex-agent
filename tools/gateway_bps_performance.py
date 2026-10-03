#!/usr/bin/env python3
"""Run isolated BPS loopback A/B measurements and preserve all samples/logs.

Example (PowerShell):
  python tools/gateway_bps_performance.py --baseline .local/gateway-perf-20260927/baseline-src --candidate .local/gateway-perf-20260927/candidate-src --output .local/gateway-perf-20260927/ab --samples 50 --rounds 3 --gomaxprocs 2

This does not contact production or an actual model provider. Compilation is
outside timed tests; test binaries run sequentially in alternating AB/BA order.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import math
import os
from pathlib import Path
import statistics
import subprocess
import sys
import time


def percentile(values: list[float], q: float) -> float:
    ordered = sorted(values)
    at = (len(ordered) - 1) * q
    low = math.floor(at)
    high = math.ceil(at)
    return ordered[low] + (ordered[high] - ordered[low]) * (at - low)


def checked(command: list[str], cwd: Path, env: dict[str, str], log: Path) -> None:
    print(f"Running {' '.join(command)} (cwd={cwd})", flush=True)
    with log.open("w", encoding="utf-8") as output:
        result = subprocess.run(command, cwd=cwd, env=env, stdout=output, stderr=subprocess.STDOUT)
    if result.returncode:
        raise RuntimeError(f"command failed ({result.returncode}); see {log}\n{log.read_text(encoding='utf-8')[-6000:]}")


def source_manifest(root: Path) -> dict[str, str]:
    files = sorted(root.rglob("*.go")) + sorted((root / "internal").rglob("*.json")) + sorted((root / "migrations").rglob("*.sql")) + [root / "go.mod", root / "go.sum"]
    return {
        path.relative_to(root).as_posix(): hashlib.sha256(path.read_bytes()).hexdigest()
        for path in files
        if not any(part in {".git", ".local", "node_modules"} for part in path.relative_to(root).parts)
    }


def summarize(output: Path) -> None:
    measurements: dict[str, dict[str, list[dict]]] = {}
    documents = sorted(output.glob("samples-*.json"))
    if not documents:
        raise RuntimeError("no samples-*.json files found")
    for path in documents:
        report = json.loads(path.read_text(encoding="utf-8"))
        label = report["label"].split("-round-")[0]
        for run in report["runs"]:
            has_text = run["scenario"]["transport"] in {"sse", "ws", "json"}
            for sample in run["samples"]:
                if sample["total_ms"] <= 0 or has_text and sample.get("ttft_ms", 0) <= 0:
                    raise RuntimeError(f"Invalid/nonpositive timing in {path.name}, {run['scenario']['name']}; rerun with a verified high-resolution clock")
            measurements.setdefault(run["scenario"]["name"], {}).setdefault(label, []).append(run)
    summary = {"method": "Per-request distributions pooled across repeated process runs. Improvement = (baseline - candidate) / baseline; negative means regression. Round medians retained to expose noise. Allocations include fixture/client/server background work.", "scenarios": {}}
    markdown = [
        "# BPS synthetic loopback A/B performance",
        "",
        "TTFT is first real text delta. JSON TTFT means complete answer availability. Compact/models have no token TTFT. All measurements validate complete content and success terminals.",
        "",
        "| Scenario | n/side | TTFT p50 ms B → C | Δ | Total p50 ms B → C | Δ | Total p95 ms B → C | Alloc KiB/request B → C |",
        "|---|---:|---:|---:|---:|---:|---:|---:|",
    ]
    for scenario, versions in measurements.items():
        record = {}
        for label, runs in versions.items():
            samples = [sample for run in runs for sample in run["samples"]]
            total = [sample["total_ms"] for sample in samples]
            ttft = [sample["ttft_ms"] for sample in samples if sample.get("ttft_ms")]
            record[label] = {
                "n": len(samples),
                "total_p50_ms": percentile(total, 0.5),
                "total_p95_ms": percentile(total, 0.95),
                "total_mean_ms": statistics.mean(total),
                "ttft_p50_ms": percentile(ttft, 0.5) if ttft else None,
                "ttft_p95_ms": percentile(ttft, 0.95) if ttft else None,
                "ttft_mean_ms": statistics.mean(ttft) if ttft else None,
                "alloc_bytes_per_request": sum(run["alloc_bytes"] for run in runs) / len(samples),
                "mallocs_per_request": sum(run["mallocs"] for run in runs) / len(samples),
                "throughput_rps": len(samples) * 1000 / sum(run["measured_wall_ms"] for run in runs),
                "round_total_p50_ms": [percentile([sample["total_ms"] for sample in run["samples"]], 0.5) for run in runs],
                "round_ttft_p50_ms": [percentile([sample["ttft_ms"] for sample in run["samples"]], 0.5) if ttft else None for run in runs],
            }
        if "baseline" in record and "candidate" in record:
            baseline, candidate = record["baseline"], record["candidate"]
            metrics = ("total_p50_ms", "total_p95_ms", "total_mean_ms", "ttft_p50_ms", "ttft_p95_ms", "ttft_mean_ms", "alloc_bytes_per_request", "mallocs_per_request")
            record["improvement_percent"] = {metric: (baseline[metric] - candidate[metric]) / baseline[metric] * 100 if baseline[metric] else None for metric in metrics}
            improvement = record["improvement_percent"]
            ttft = f"{baseline['ttft_p50_ms']:.3f} → {candidate['ttft_p50_ms']:.3f}" if baseline["ttft_p50_ms"] else "—"
            ttft_delta = f"{improvement['ttft_p50_ms']:+.1f}%" if baseline["ttft_p50_ms"] else "—"
            markdown.append(f"| {scenario} | {baseline['n']}/{candidate['n']} | {ttft} | {ttft_delta} | {baseline['total_p50_ms']:.3f} → {candidate['total_p50_ms']:.3f} | {improvement['total_p50_ms']:+.1f}% | {baseline['total_p95_ms']:.3f} → {candidate['total_p95_ms']:.3f} | {baseline['alloc_bytes_per_request']/1024:.1f} → {candidate['alloc_bytes_per_request']/1024:.1f} |")
        summary["scenarios"][scenario] = record
    (output / "summary.json").write_text(json.dumps(summary, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    (output / "summary.md").write_text("\n".join(markdown) + "\n", encoding="utf-8")
    print(output / "summary.md", flush=True)


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--baseline", type=Path)
    parser.add_argument("--candidate", type=Path)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--samples", type=int, default=50)
    parser.add_argument("--warmup", type=int, default=5)
    parser.add_argument("--rounds", type=int, default=3)
    parser.add_argument("--gomaxprocs", type=int, default=2)
    parser.add_argument("--filter", default="")
    parser.add_argument("--summarize-only", action="store_true")
    args = parser.parse_args()
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=True)
    if args.summarize_only:
        summarize(output)
        return
    if not args.baseline or not args.candidate:
        parser.error("--baseline and --candidate are required unless --summarize-only")
    if min(args.samples, args.warmup, args.rounds, args.gomaxprocs) < 1:
        parser.error("numeric options must be positive")
    if list(output.glob("samples-*.json")):
        parser.error("output already contains samples; use a fresh directory to avoid mixing revisions")
    roots = {"baseline": args.baseline.resolve(), "candidate": args.candidate.resolve()}
    for harness in ("gateway_bps_e2e_performance_test.go", "gateway_bps_e2e_performance_windows_test.go"):
        path = Path("internal/server") / harness
        if (roots["baseline"] / path).read_bytes() != (roots["candidate"] / path).read_bytes():
            parser.error("baseline and candidate performance harness/clock must be byte-identical")
    environment = os.environ.copy()
    environment.update(GOMAXPROCS=str(args.gomaxprocs), GATEWAY_PERF_SAMPLES=str(args.samples), GATEWAY_PERF_WARMUP=str(args.warmup), GATEWAY_PERF_FILTER=args.filter)
    binaries = {}
    for label, root in roots.items():
        manifest = source_manifest(root)
        (output / f"manifest-{label}.json").write_text(json.dumps(manifest, indent=2) + "\n", encoding="utf-8")
        binaries[label] = output / (f"server-{label}.test" + (".exe" if os.name == "nt" else ""))
        checked(["go", "test", "-c", "-o", str(binaries[label]), "./internal/server"], root, environment, output / f"build-{label}.log")
    execution = []
    for round_index in range(args.rounds):
        for label in ("baseline", "candidate") if round_index % 2 == 0 else ("candidate", "baseline"):
            tag = f"{label}-round-{round_index + 1}"
            environment.update(GATEWAY_PERF_OUTPUT=str(output / f"samples-{tag}.json"), GATEWAY_PERF_LABEL=tag)
            started = time.time()
            checked([str(binaries[label]), "-test.run=^TestBPSGatewayE2EPerformance$", "-test.count=1", "-test.v", "-test.timeout=10m"], roots[label], environment, output / f"run-{tag}.log")
            execution.append({"label": label, "round": round_index + 1, "started_unix": started, "elapsed_seconds": time.time() - started})
            (output / "execution.json").write_text(json.dumps(execution, indent=2) + "\n", encoding="utf-8")
    summarize(output)


if __name__ == "__main__":
    try:
        main()
    except (OSError, RuntimeError) as error:
        print(error, file=sys.stderr)
        sys.exit(1)
