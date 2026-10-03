"""Synthetic concurrent cancellation/refill probe without a helper slot cap."""
from __future__ import annotations

import argparse
import hashlib
import json
from pathlib import Path
import tempfile
import time

import loopback


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--binary", required=True, type=Path)
    parser.add_argument("--report", required=True, type=Path)
    parser.add_argument("--loops", type=int, default=10)
    parser.add_argument("--concurrency", type=int, default=96)
    args = parser.parse_args()
    if args.concurrency < 1 or args.loops < 0:
        parser.error("concurrency must be positive and loops must be nonnegative")
    loopback.BINARY = args.binary.resolve(strict=True)
    began = time.monotonic()
    server = loopback.Fixture()
    batches = []
    current = []
    failure = None
    helper = None
    with tempfile.TemporaryDirectory(prefix="native-refill-") as directory:
        try:
            helper = loopback.Helper(server.origin, unix_socket=str(Path(directory) / "s"), headers_timeout_ms=0, idle_timeout_ms=0)
            for batch in range(args.loops + 1):
                # No cancellation acknowledgments, waits, sleeps, or upstream
                # observations between closing old slots and refilling them.
                for connection in current:
                    connection.close()
                current = []
                sent = 0
                heads = 0
                errors = []
                for index in range(args.concurrency):
                    try:
                        current.append(helper.request(f"/stream_wait?refill={batch}&slot={index}"))
                        sent += 1
                    except (OSError, EOFError) as error:
                        errors.append(type(error).__name__)
                for connection in current:
                    try:
                        kind, _ = loopback.read_frame(connection)
                        if kind != loopback.HEAD:
                            errors.append("missing_head")
                            continue
                        kind, _ = loopback.read_frame(connection)
                        if kind != loopback.DATA:
                            errors.append("missing_initial_data")
                            continue
                        heads += 1
                    except (OSError, EOFError) as error:
                        errors.append(type(error).__name__)
                batches.append({"batch": batch, "sent": sent, "heads": heads, "error_classes": errors})
                if sent != args.concurrency or heads != args.concurrency:
                    failure = "capacity_refill_rejected"
                    break
        except Exception as error:
            failure = type(error).__name__
        finally:
            for connection in current:
                connection.close()
            if helper is not None:
                try:
                    helper.close()
                except Exception as error:
                    failure = failure or type(error).__name__
            server.close()
    report = {"scope": "synthetic concurrent UDS close/refill", "live_accounts": False,
              "concurrency": args.concurrency,
              "binary_sha256": hashlib.sha256(args.binary.read_bytes()).hexdigest(),
              "requested_refill_loops": args.loops, "batches": batches,
              "failure": failure, "passed": failure is None,
              "seconds": round(time.monotonic() - began, 3)}
    args.report.write_text(json.dumps(report, indent=2) + "\n")
    print(json.dumps(report), flush=True)
    raise SystemExit(0 if failure is None else 1)


if __name__ == "__main__":
    main()
