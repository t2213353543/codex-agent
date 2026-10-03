"""Read-only, server-local gateway admission and Linux process sampler."""
from __future__ import annotations

import argparse
from datetime import datetime, timezone
import hashlib
import hmac
import http.client
import json
import os
from pathlib import Path
import secrets
import socket
import threading
import time
from urllib.parse import urlsplit


TCP_STATES = {"01": "established", "02": "syn_sent", "03": "syn_received", "04": "fin_wait_1",
              "05": "fin_wait_2", "06": "time_wait", "07": "closed", "08": "close_wait",
              "09": "last_ack", "0A": "listen", "0B": "closing"}


def number(value):
    return value if isinstance(value, int) and not isinstance(value, bool) and value >= 0 else None


def anonymous_accounts(payload, salt):
    if not isinstance(payload, dict) or not isinstance(payload.get("accounts"), list):
        raise ValueError("invalid_accounts_shape")
    accounts, unknown = {}, 0
    for item in payload["accounts"]:
        if not isinstance(item, dict):
            unknown += 1
            continue
        identity = item.get("id") or item.get("name")
        if not isinstance(identity, str) or not identity:
            unknown += 1
            continue
        digest = hmac.new(salt, identity.encode(), hashlib.sha256).hexdigest()[:24]
        inflight = number(item.get("in_flight"))
        if inflight is None:
            unknown += 1
        accounts[digest] = {"in_flight": inflight, "configured_concurrency": number(item.get("concurrency")), "enabled": item.get("enabled") is True}
    complete = payload.get("has_more") is not True and unknown == 0
    return {"accounts": accounts, "account_count": len(accounts), "gateway_admitted_observed": sum(item["in_flight"] or 0 for item in accounts.values()),
            "scope_complete": complete, "pagination_has_more": payload.get("has_more") is True, "unknown_rows": unknown}


def account_sample(base, key, salt, timeout):
    parsed = urlsplit(base)
    if parsed.scheme not in ("http", "https") or not parsed.hostname or parsed.username or parsed.password or parsed.query or parsed.fragment:
        raise ValueError("invalid_admin_base")
    connection_class = http.client.HTTPSConnection if parsed.scheme == "https" else http.client.HTTPConnection
    connection = connection_class(parsed.hostname, parsed.port, timeout=timeout)
    sockets = []
    expired = threading.Event()
    def expire():
        expired.set()
        for active_socket in sockets:
            try:
                active_socket.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass
            active_socket.close()
    watchdog = threading.Timer(timeout, expire)
    watchdog.daemon = True
    watchdog.start()
    try:
        connection.connect()
        sockets.append(connection.sock)
        if expired.is_set():
            raise TimeoutError
        prefix = parsed.path.rstrip("/")
        path = prefix + ("/accounts" if prefix.endswith("/api") else "/api/accounts") + "?limit=500"
        connection.request("GET", path, headers={"Authorization": "Bearer " + key, "Accept": "application/json"})
        response = connection.getresponse()
        if response.status != 200:
            return {"ok": False, "http_status": response.status, "error": "admin_http_failure"}
        data = response.read(2 * 1024 * 1024 + 1)
        if expired.is_set():
            raise TimeoutError
        if len(data) > 2 * 1024 * 1024:
            return {"ok": False, "http_status": 200, "error": "response_limit"}
        result = anonymous_accounts(json.loads(data), salt)
        result.update(ok=True, http_status=200)
        return result
    except (OSError, ValueError, http.client.HTTPException) as error:
        return {"ok": False, "error": type(error).__name__}
    finally:
        watchdog.cancel()
        connection.close()


def process_sample(pid, proc_root=Path("/proc"), clock_ticks=None, page_size=None):
    directory = proc_root / str(pid)
    clock_ticks = clock_ticks or os.sysconf("SC_CLK_TCK")
    page_size = page_size or os.sysconf("SC_PAGE_SIZE")
    stat = (directory / "stat").read_text()
    # The process comm field can itself contain spaces and parentheses.
    fields = stat[stat.rfind(")") + 2:].split()
    result = {"start_ticks": int(fields[19]), "cpu_ticks": int(fields[11]) + int(fields[12]), "clock_ticks_per_second": clock_ticks,
              "rss_bytes": int(fields[21]) * page_size, "threads": int(fields[17])}
    try:
        descriptors = list((directory / "fd").iterdir())
        result["fd_count"] = len(descriptors)
        inodes = set()
        for descriptor in descriptors:
            try:
                target = descriptor.readlink().as_posix()
                if target.startswith("socket:[") and target.endswith("]"):
                    inodes.add(target[8:-1])
            except OSError:
                pass
        states = {}
        for name in ("tcp", "tcp6"):
            path = directory / "net" / name
            if not path.exists():
                continue
            for line in path.read_text().splitlines()[1:]:
                columns = line.split()
                if len(columns) > 9 and columns[9] in inodes:
                    state = TCP_STATES.get(columns[3], "other")
                    states[state] = states.get(state, 0) + 1
        result["process_owned_tcp_states"] = states
        result["socket_fds"] = len(inodes)
    except OSError as error:
        result["fd_error"] = type(error).__name__
    return result


def cgroup_sample(pid, proc_root=Path("/proc"), cgroup_root=Path("/sys/fs/cgroup")):
    try:
        lines = (proc_root / str(pid) / "cgroup").read_text().splitlines()
        for line in lines:
            _, controllers, relative = line.split(":", 2)
            roots = [cgroup_root] if not controllers else ([cgroup_root / "cpu", cgroup_root / "cpu,cpuacct"] if "cpu" in controllers.split(",") else [])
            for root in roots:
                directory = (root / relative.lstrip("/")).resolve()
                if not directory.is_relative_to(root.resolve()):
                    continue
                path = directory / "cpu.stat"
                if not path.is_file():
                    continue
                values = {}
                for entry in path.read_text().splitlines():
                    key, raw = entry.split()
                    if key in {"usage_usec", "user_usec", "system_usec", "nr_periods", "nr_throttled", "throttled_usec", "throttled_time"}:
                        values[key] = int(raw)
                if "throttled_time" in values:
                    values["throttled_usec"] = values.pop("throttled_time") // 1000
                return {"available": True, "version": 2 if not controllers else 1, "cpu_stat": values}
        return {"available": False, "reason": "cpu_stat_unavailable"}
    except (OSError, ValueError) as error:
        return {"available": False, "reason": type(error).__name__}


def derived_sample(current, previous, elapsed):
    if previous and elapsed > 0 and current["start_ticks"] == previous["start_ticks"]:
        delta = max(0, current["cpu_ticks"] - previous["cpu_ticks"])
        current["cpu_one_core_percent"] = round(delta / current["clock_ticks_per_second"] / elapsed * 100, 3)
    else:
        current["cpu_one_core_percent"] = None
    return current


def save(path, data):
    temporary = path.with_suffix(".tmp")
    temporary.write_text(json.dumps(data, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    temporary.replace(path)


def run_monitor(pid, base, key, duration, interval, request_timeout, output, stop_file=None):
    output.mkdir(parents=True, exist_ok=False)
    salt = secrets.token_bytes(32)  # Never persisted; aliases are stable within this run only.
    report = {"schema_version": 1, "started_utc": datetime.now(timezone.utc).isoformat(), "status": "running", "pid": pid,
              "duration_limit_seconds": duration, "interval_seconds": interval,
              "script_sha256": hashlib.sha256(Path(__file__).read_bytes()).hexdigest(), "samples": 0, "api_errors": 0,
              "gateway_admitted_peak_observed": 0, "by_account_peak_observed": {}, "resource_peaks": {},
              "upstream_peak_inflight": None, "alias_scope": "HMAC-random-key stable within this run; key is not persisted",
              "measurement_boundary": "gateway account in_flight includes admitted requests refreshing credentials/waiting for transport; not actual upstream concurrent inference",
              "tcp_scope": "Only TCP socket inodes owned by this PID; directions/endpoints are not recorded; not an upstream concurrency count"}
    save(output / "report.json", report)
    previous, previous_time, first_start = None, None, None
    previous_cgroup, first_cgroup = None, None
    started = time.monotonic()
    with (output / "samples.jsonl").open("w", encoding="utf-8", buffering=1) as stream:
        while time.monotonic() - started < duration:
            sample_started = time.monotonic()
            if stop_file and stop_file.exists():
                report["status"] = "stopped_by_file"
                break
            try:
                process = process_sample(pid)
            except (OSError, ValueError) as error:
                report.update(status="stopped_process_unavailable", process_error=type(error).__name__)
                break
            if first_start is None:
                first_start = process["start_ticks"]
            elif process["start_ticks"] != first_start:
                report["status"] = "stopped_pid_reused"
                break
            process = derived_sample(process, previous, sample_started - previous_time if previous_time else 0)
            previous, previous_time = process, sample_started
            remaining = duration - (time.monotonic() - started)
            admin = account_sample(base, key, salt, min(request_timeout, max(.01, remaining)))
            cgroup = cgroup_sample(pid)
            if cgroup.get("available"):
                current = cgroup["cpu_stat"]
                if first_cgroup is None:
                    first_cgroup = current.copy()
                cgroup["delta_since_previous"] = {name: max(0, value - previous_cgroup.get(name, value)) for name, value in current.items()} if previous_cgroup else {}
                previous_cgroup = current.copy()
                report["cgroup_cpu_delta"] = {name: max(0, value - first_cgroup.get(name, value)) for name, value in current.items()}
            sample = {"timestamp_utc": datetime.now(timezone.utc).isoformat(), "elapsed_seconds": round(sample_started - started, 3),
                      "process": process, "gateway_admission": admin, "cgroup": cgroup,
                      "sample_duration_ms": round((time.monotonic() - sample_started) * 1000, 3)}
            stream.write(json.dumps(sample, separators=(",", ":")) + "\n")
            report["samples"] += 1
            if admin["ok"]:
                report["gateway_admitted_peak_observed"] = max(report["gateway_admitted_peak_observed"], admin["gateway_admitted_observed"])
                for alias, account in admin["accounts"].items():
                    if account["in_flight"] is not None:
                        report["by_account_peak_observed"][alias] = max(report["by_account_peak_observed"].get(alias, 0), account["in_flight"])
                report["all_api_samples_complete"] = report.get("all_api_samples_complete", True) and admin["scope_complete"]
            else:
                report["api_errors"] += 1
                report["all_api_samples_complete"] = False
            for name in ("rss_bytes", "fd_count", "threads", "cpu_one_core_percent"):
                value = process.get(name)
                if value is not None:
                    report["resource_peaks"][name] = max(report["resource_peaks"].get(name, 0), value)
            report["actual_duration_seconds"] = round(time.monotonic() - started, 3)
            save(output / "report.json", report)
            if admin.get("http_status") in (401, 403):
                report["status"] = "stopped_admin_auth_failure"
                break
            wait_until = min(started + duration, sample_started + interval)
            while time.monotonic() < wait_until:
                if stop_file and stop_file.exists():
                    break
                time.sleep(min(.05, max(0, wait_until - time.monotonic())))
    if report["status"] == "running":
        report["status"] = "completed"
    report["finished_utc"] = datetime.now(timezone.utc).isoformat()
    save(output / "report.json", report)
    return report


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--pid", type=int, required=True)
    parser.add_argument("--admin-base", required=True)
    parser.add_argument("--key-file", type=Path, required=True, help="Existing private ADMIN key file; never printed")
    parser.add_argument("--interval", type=float, default=.2)
    parser.add_argument("--duration", type=float, default=600)
    parser.add_argument("--request-timeout", type=float, default=1)
    parser.add_argument("--stop-file", type=Path)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    if args.pid <= 0 or not .05 <= args.interval <= 10 or not 0 < args.duration <= 7200 or not .01 <= args.request_timeout <= 5:
        parser.error("pid must be positive, interval .05..10, duration 0..7200, request-timeout .01..5")
    try:
        key = args.key_file.read_text(encoding="utf-8").strip()
        if not key or "\r" in key or "\n" in key:
            raise ValueError("invalid_key_file")
        report = run_monitor(args.pid, args.admin_base, key, args.duration, args.interval, args.request_timeout, args.output.resolve(), args.stop_file)
    except (OSError, ValueError) as error:
        parser.exit(2, f"Local monitor input/read failed ({type(error).__name__}); no private inputs were printed.\n")
    print(json.dumps({"status": report["status"], "samples": report["samples"], "gateway_admitted_peak_observed": report["gateway_admitted_peak_observed"], "report": str(args.output.resolve() / "report.json")}, indent=2))
    return 0 if report["status"] in ("completed", "stopped_by_file") and report["api_errors"] == 0 else 1


if __name__ == "__main__":
    raise SystemExit(main())
