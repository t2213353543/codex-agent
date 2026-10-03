"""Observe sealed-helper startup without issuing any HTTP or WS request."""
from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import queue
import secrets
import socket
import stat
import struct
import subprocess
import tempfile
import threading
import time


class ConnectionTripwire:
    def __init__(self):
        self.socket = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        self.socket.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        self.socket.bind(("127.0.0.1", 1))
        self.socket.listen(8)
        self.socket.settimeout(0.05)
        self.connections = 0
        self.stopped = threading.Event()
        self.thread = threading.Thread(target=self.run, daemon=True)
        self.thread.start()

    def run(self):
        while not self.stopped.is_set():
            try:
                connection, _ = self.socket.accept()
            except TimeoutError:
                continue
            except OSError:
                return
            self.connections += 1
            connection.close()

    def close(self):
        self.stopped.set()
        self.socket.close()
        self.thread.join(timeout=1)
        if self.thread.is_alive():
            raise RuntimeError("tripwire_shutdown_failed")


def run_case(binary, directory, name, lifecycle, expected_error=None):
    private = directory / name
    private.mkdir(mode=0o700)
    path = private / "s"
    config = {"protocol": 1, "unix_socket": str(path), "upstream": "http://127.0.0.1:1",
              "ipc_token": secrets.token_hex(32), "proxy": None, "headers_timeout_ms": 1000,
              "idle_timeout_ms": 0, "http_client_lifecycle": lifecycle}
    env = {"PATH": os.environ.get("PATH", "/usr/bin:/bin")}
    if name == "invalid_custom_ca":
        env["CODEX_CA_CERTIFICATE"] = str(private / "missing-ca.pem")
    if name == "insecure_uds_parent":
        private.chmod(0o755)
    process = subprocess.Popen([str(binary)], stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                               stderr=subprocess.PIPE, env=env)
    encoded = json.dumps(config).encode()
    ready_queue = queue.Queue(maxsize=1)
    reader = threading.Thread(target=lambda: ready_queue.put(process.stdout.readline(4097)), daemon=True)
    reader.start()
    started = time.monotonic()
    forced_kill = False
    failure = None
    ready = None
    socket_mode = None
    startup_ms, shutdown_ms = None, None
    line = b""
    stderr = b""
    extra = b""
    try:
        process.stdin.write(struct.pack("!I", len(encoded)) + (encoded[:4] if name == "truncated_config_eof" else encoded))
        process.stdin.flush()
        if name == "truncated_config_eof":
            process.stdin.close()
            process.stdin = None
        line = ready_queue.get(timeout=5)
        startup_ms = round((time.monotonic() - started) * 1000, 3)
        if expected_error is None:
            if len(line) > 4096 or not line.endswith(b"\n"):
                raise RuntimeError("ready_record_bound")
            ready = json.loads(line)
            metadata = path.lstat()
            socket_mode = oct(stat.S_IMODE(metadata.st_mode))
            if (ready.get("protocol") != 1 or ready.get("transport") != "unix"
                    or ready.get("unix_socket") != str(path)
                    or ready.get("http_client_lifecycle") != lifecycle
                    or ready.get("max_body", 0) < 32 << 20 or ready.get("max_active") != 0
                    or not {"request_zstd_v1", "websocket_message_v1"} <= set(ready.get("capabilities", []))
                    or not stat.S_ISSOCK(metadata.st_mode) or stat.S_IMODE(metadata.st_mode) != 0o600
                    or metadata.st_uid != os.geteuid() or process.poll() is not None):
                raise RuntimeError("ready_contract")
        elif line:
            raise RuntimeError("invalid_startup_emitted_ready")
        before_shutdown = time.monotonic()
        if process.stdin is not None:
            process.stdin.close()
            process.stdin = None
        process.wait(timeout=2)
        shutdown_ms = round((time.monotonic() - before_shutdown) * 1000, 3)
        reader.join(timeout=1)
        extra = process.stdout.read()
        stderr = process.stderr.read()
        if expected_error is None:
            if process.returncode != 0 or stderr or extra or path.exists():
                raise RuntimeError("clean_exit_contract")
        elif process.returncode != 1 or stderr != ("native-transport: " + expected_error + "\n").encode() or extra or path.exists():
            raise RuntimeError("failure_exit_contract")
    except Exception as error:
        failure = type(error).__name__
    finally:
        if process.poll() is None:
            forced_kill = True
            process.kill()
            process.wait(timeout=2)
        if process.stdin is not None:
            process.stdin.close()
        reader.join(timeout=1)
        process.stdout.close()
        process.stderr.close()
    sanitized = None
    if ready:
        sanitized = {key: ready[key] for key in ("protocol", "transport", "cli_source", "helper", "max_body", "max_active", "capabilities", "http_client_lifecycle")}
    return {"case": name, "passed": failure is None and not forced_kill, "failure_class": failure,
            "expected_fixed_error": expected_error, "ready": sanitized, "socket_mode": socket_mode,
            "exit_code": process.returncode, "forced_kill": forced_kill, "socket_removed": not path.exists(),
            "startup_ms": startup_ms, "shutdown_ms": shutdown_ms, "request_envelopes_sent": 0}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, required=True)
    parser.add_argument("--report", type=Path, required=True)
    args = parser.parse_args()
    binary = args.binary.resolve(strict=True)
    tripwire = ConnectionTripwire()
    cases = []
    try:
        with tempfile.TemporaryDirectory(prefix="native-startup-probe-") as folder:
            directory = Path(folder).resolve()
            for name, lifecycle, expected in [
                ("strict_ready_and_eof", "cli_request", None),
                ("pooled_ready_and_eof", "pooled", None),
                ("invalid_custom_ca", "cli_request", "tls_or_client_config"),
                ("insecure_uds_parent", "cli_request", "unix_parent_invalid"),
                ("truncated_config_eof", "cli_request", "config_read"),
            ]:
                before = tripwire.connections
                result = run_case(binary, directory, name, lifecycle, expected)
                time.sleep(0.075)
                result["origin_connections_observed"] = tripwire.connections - before
                result["passed"] = result["passed"] and result["origin_connections_observed"] == 0
                cases.append(result)
    finally:
        tripwire.close()
    report = {"schema": "sealed-native-startup-no-request-v1", "helper_sha256": hashlib.sha256(binary.read_bytes()).hexdigest(),
              "fixed_origin": "http://127.0.0.1:1", "proxy": None, "request_envelopes_sent": 0,
              "origin_connections_observed": tripwire.connections, "cases": cases, "passed": all(case["passed"] for case in cases),
              "live_accounts": False, "upstream_tls_negotiation_verified": False,
              "limits": ["The configured origin was monitored for connections; code review separately confirms no other dial before a request.",
                         "Readiness establishes local executable/config/TLS-builder/IPC compatibility, not upstream connectivity or fingerprint identity.",
                         "Valid custom-CA target handshakes, account proxies, certificates at production destinations and model acceptance are not exercised."]}
    args.report.write_text(json.dumps(report, indent=2) + "\n")
    print(json.dumps(report), flush=True)
    raise SystemExit(0 if report["passed"] else 1)


if __name__ == "__main__":
    main()
