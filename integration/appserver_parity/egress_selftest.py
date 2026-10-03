#!/usr/bin/env python3
"""Validate process-tree syscall capture using loopback and synthetic data only."""
import argparse
import json
import os
from pathlib import Path
import socket
import subprocess
import sys

SENTINEL = b"SYNTHETIC_STRACE_PAYLOAD_MUST_NOT_APPEAR"


def fixture():
    with socket.socket() as tcp, socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as udp:
        tcp.bind(("127.0.0.1", 0))
        tcp.listen(1)
        tcp.settimeout(10)
        udp.bind(("127.0.0.1", 0))
        udp.settimeout(10)
        child = "import socket,sys;data=bytes.fromhex(sys.argv[3]);s=socket.create_connection(('127.0.0.1',int(sys.argv[1])),5);s.sendall(data);s.close();u=socket.socket(socket.AF_INET,socket.SOCK_DGRAM);u.sendto(data,('127.0.0.1',int(sys.argv[2])));u.close()"
        process = subprocess.Popen([sys.executable, "-c", child, str(tcp.getsockname()[1]), str(udp.getsockname()[1]), SENTINEL.hex()])
        accepted, _ = tcp.accept()
        with accepted:
            tcp_ok = accepted.recv(1024) == SENTINEL
        udp_ok = udp.recv(1024) == SENTINEL
        process.wait(timeout=10)
        return {"tcp_payload_arrived": tcp_ok, "udp_payload_arrived": udp_ok, "child_exit_zero": process.returncode == 0}


def audit(binary, directory):
    os.umask(0o077)
    directory = Path(directory).absolute()
    directory.mkdir(mode=0o700)
    output = directory / "private-net.log"
    command = [binary, "-f", "-qq", "-ttt", "-e", "trace=connect,sendto,sendmsg", "-e", "raw=sendto,sendmsg",
               "-o", str(output), "--", sys.executable, str(Path(__file__).resolve()), "--fixture"]
    result = subprocess.run(command, capture_output=True, timeout=30)
    log = output.read_bytes()
    received = json.loads(result.stdout)
    checks = dict(received, tracer_exit_zero=result.returncode == 0,
                  child_connect_recorded=b"connect(" in log and b"127.0.0.1" in log,
                  send_syscall_recorded=b"sendto(" in log or b"sendmsg(" in log,
                  send_payload_not_recorded=SENTINEL not in log and SENTINEL.hex().encode() not in log,
                  private_log_mode=(output.stat().st_mode & 0o777) == 0o600)
    report = {"schema": 1, "passed": all(checks.values()), "synthetic_only": True, "checks": checks,
              "coverage": "connect sockaddrs for traced process descendants; sendto/sendmsg raw arguments only",
              "limitations": ["Raw sendto/sendmsg does not decode UDP destination or DNS payload.",
                              "This test does not establish coverage of unrelated/preexisting processes, io_uring or all possible egress methods."]}
    (directory / "selftest-summary.json").write_text(json.dumps(report, indent=2))
    return report


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--fixture", action="store_true")
    parser.add_argument("--strace")
    parser.add_argument("--directory")
    args = parser.parse_args()
    print(json.dumps(fixture() if args.fixture else audit(args.strace, args.directory), indent=2))
