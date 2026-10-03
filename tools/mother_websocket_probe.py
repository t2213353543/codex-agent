"""Two-turn Responses WebSocket probe using only the Python standard library.

Upload alongside mother_account_benchmark.py. No retries, redirects, automatic
compression, output-token caps, or credentials in recorded artifacts.
"""
from __future__ import annotations

import argparse
import base64
import hashlib
import json
import os
from pathlib import Path
import socket
import ssl
import struct
import time
import uuid

from mother_account_benchmark import (MAX_BYTES, TERMINALS, endpoint, output_text,
                                      reconstruct_response, save_json, utc_now)


class WireSocket:
    def __init__(self, base_url, key, timeout):
        self.parsed, self.path = endpoint(base_url.replace("wss://", "https://", 1).replace("ws://", "http://", 1))
        self.key = key
        self.timeout = timeout
        self.sock = None
        self.buffer = bytearray()
        self.http_status = None
        self.headers = {}

    def connect(self):
        port = self.parsed.port or (443 if self.parsed.scheme == "https" else 80)
        self.sock = socket.create_connection((self.parsed.hostname, port), self.timeout)
        if self.parsed.scheme == "https":
            self.sock = ssl.create_default_context().wrap_socket(self.sock, server_hostname=self.parsed.hostname)
        nonce = base64.b64encode(os.urandom(16)).decode()
        session = "ws_bench_" + uuid.uuid4().hex
        lines = ["GET " + self.path + " HTTP/1.1", "Host: " + self.parsed.netloc,
                 "Upgrade: websocket", "Connection: Upgrade", "Sec-WebSocket-Version: 13",
                 "Sec-WebSocket-Key: " + nonce, "Authorization: Bearer " + self.key,
                 "Session-Id: " + session, "User-Agent: codex-agent-mother-websocket-probe/1"]
        self.sock.sendall(("\r\n".join(lines) + "\r\n\r\n").encode("ascii"))
        deadline = time.monotonic() + self.timeout
        while b"\r\n\r\n" not in self.buffer:
            self.sock.settimeout(max(.001, deadline - time.monotonic()))
            chunk = self.sock.recv(4096)
            if not chunk:
                raise ConnectionError("EOF during WebSocket handshake")
            self.buffer.extend(chunk)
            if len(self.buffer) > 65536:
                raise ValueError("WebSocket handshake headers exceed limit")
            if time.monotonic() >= deadline:
                raise TimeoutError("WebSocket handshake deadline")
        raw_headers, rest = bytes(self.buffer).split(b"\r\n\r\n", 1)
        self.buffer = bytearray(rest)
        lines = raw_headers.decode("iso-8859-1").split("\r\n")
        self.http_status = int(lines[0].split()[1])
        self.headers = {line.split(":", 1)[0].strip().lower(): line.split(":", 1)[1].strip()
                        for line in lines[1:] if ":" in line}
        if self.http_status != 101:
            raise ConnectionError("WebSocket handshake HTTP " + str(self.http_status))
        expected = base64.b64encode(hashlib.sha1((nonce + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11").encode()).digest()).decode()
        if self.headers.get("sec-websocket-accept") != expected or self.headers.get("upgrade", "").lower() != "websocket":
            raise ValueError("Invalid WebSocket upgrade response")
        if self.headers.get("sec-websocket-extensions"):
            raise ValueError("Server selected unrequested WebSocket extensions")

    def send(self, opcode, payload):
        mask = os.urandom(4)
        size = len(payload)
        if size < 126:
            header = bytes([0x80 | opcode, 0x80 | size])
        elif size <= 65535:
            header = bytes([0x80 | opcode, 0x80 | 126]) + struct.pack("!H", size)
        else:
            header = bytes([0x80 | opcode, 0x80 | 127]) + struct.pack("!Q", size)
        self.sock.sendall(header + mask + bytes(value ^ mask[index % 4] for index, value in enumerate(payload)))

    def read_exact(self, count, deadline):
        while len(self.buffer) < count:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise TimeoutError("WebSocket turn deadline")
            self.sock.settimeout(remaining)
            chunk = self.sock.recv(min(65536, count - len(self.buffer)))
            if not chunk:
                raise ConnectionError("WebSocket EOF before terminal event")
            self.buffer.extend(chunk)
        result = bytes(self.buffer[:count])
        del self.buffer[:count]
        return result

    def receive(self, deadline):
        message = bytearray()
        active_opcode = None
        while True:
            first, second = self.read_exact(2, deadline)
            final, opcode = bool(first & 0x80), first & 15
            if first & 0x70 or second & 0x80:
                raise ValueError("Unexpected RSV bits or server frame mask")
            size = second & 127
            if size == 126:
                size = struct.unpack("!H", self.read_exact(2, deadline))[0]
            elif size == 127:
                size = struct.unpack("!Q", self.read_exact(8, deadline))[0]
            if size > MAX_BYTES or len(message) + size > MAX_BYTES:
                raise ValueError("WebSocket response exceeds byte limit")
            data = self.read_exact(size, deadline)
            if opcode in (8, 9, 10):
                if not final or size > 125:
                    raise ValueError("Invalid WebSocket control frame")
                if opcode == 8:
                    code = struct.unpack("!H", data[:2])[0] if len(data) >= 2 else None
                    raise ConnectionError("WebSocket closed before terminal event; code=" + str(code))
                if opcode == 9:
                    self.send(10, data)
                continue
            if opcode == 1 and active_opcode is None:
                active_opcode = opcode
            elif opcode != 0 or active_opcode is None:
                raise ValueError("Unexpected WebSocket data frame opcode")
            message.extend(data)
            if final:
                event = json.loads(message)
                if not isinstance(event, dict):
                    raise ValueError("WebSocket event is not a JSON object")
                return event

    def close(self):
        if self.sock:
            try:
                self.send(8, struct.pack("!H", 1000))
            except OSError:
                pass
            self.sock.close()


def probe(args, key, model, index):
    wire = WireSocket(args.base_url, key, args.timeout)
    result = {"model": model, "started_at": utc_now(), "transport": "websocket", "attempts": 1,
              "handshake_status": None, "turns": [], "success": False}
    try:
        started = time.monotonic()
        wire.connect()
        result["handshake_status"] = wire.http_status
        result["handshake_ms"] = round((time.monotonic() - started) * 1000, 3)
        previous = None
        for turn_number in (1, 2):
            prompt = ("Reply exactly WS_OK." if turn_number == 1 else
                      "What exact token did I ask you to reply with in my previous message? Reply with that token only.")
            body = {"type": "response.create", "model": model, "input": [{"role": "user", "content": prompt}],
                    "instructions": "Follow the user request exactly.", "store": False,
                    "reasoning": {"effort": args.effort}, "include": ["reasoning.encrypted_content"]}
            if previous:
                body["previous_response_id"] = previous
            prefix = str(index) + "-turn-" + str(turn_number)
            save_json(args.output_dir / (prefix + "-request.json"), body)
            turn = {"turn": turn_number, "terminal": None, "protocol_success": False,
                    "content_correct": None, "ttft_ms": None, "started_at": utc_now()}
            result["turns"].append(turn)
            started = time.monotonic()
            events, deltas, response = [], [], None
            try:
                wire.send(1, json.dumps(body, ensure_ascii=False).encode())
                deadline = started + args.timeout
                with (args.output_dir / (prefix + "-events.jsonl")).open("w", encoding="utf-8") as stream:
                    while True:
                        event = wire.receive(deadline)
                        events.append(event)
                        stream.write(json.dumps(event, ensure_ascii=False) + "\n")
                        stream.flush()
                        if event.get("type") == "response.output_text.delta" and event.get("delta"):
                            deltas.append(event["delta"])
                            if turn["ttft_ms"] is None:
                                turn["ttft_ms"] = round((time.monotonic() - started) * 1000, 3)
                        if event.get("type") in TERMINALS:
                            turn["terminal"] = event["type"]
                            turn["terminal_event"] = event
                            response = event.get("response")
                            turn["protocol_success"] = (event["type"] == "response.completed" and
                                                        isinstance(response, dict) and response.get("status") == "completed")
                            break
            finally:
                turn["duration_ms"] = round((time.monotonic() - started) * 1000, 3)
                turn["events_file"] = prefix + "-events.jsonl"
                turn["response"] = response
                effective, source = reconstruct_response(response, events)
                turn["effective_response"], turn["output_reconstruction"] = effective, source
                turn["output_text"] = output_text(effective) or "".join(deltas)
            if not turn["protocol_success"]:
                break
            turn["content_correct"] = turn["output_text"].strip() == "WS_OK"
            previous = response.get("id")
            if not previous:
                turn["continuation_error"] = "completed response has no id"
                break
        result["success"] = len(result["turns"]) == 2 and all(
            turn["protocol_success"] and turn["content_correct"] for turn in result["turns"])
    except (OSError, ValueError, ConnectionError) as error:
        result["error_type"] = type(error).__name__
        result["error"] = str(error)
        result["handshake_status"] = wire.http_status
    finally:
        wire.close()
        result["finished_at"] = utc_now()
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base-url", required=True)
    parser.add_argument("--key-file", required=True, type=Path)
    parser.add_argument("--output-dir", required=True, type=Path)
    parser.add_argument("--models", nargs="+", default=["gpt-5.6-sol", "gpt-6-astra"])
    parser.add_argument("--effort", default="medium")
    parser.add_argument("--timeout", type=float, default=300)
    args = parser.parse_args()
    if args.timeout <= 0 or args.timeout > 3600:
        parser.error("timeout must be within (0, 3600]")
    key = args.key_file.read_text(encoding="utf-8-sig").strip()
    if not key or "\n" in key or "\r" in key:
        parser.error("key file must contain one nonempty line")
    args.output_dir.mkdir(parents=True, exist_ok=False)
    report = {"schema": "codex-agent-mother-websocket-probe-v1", "auto_retries": 0, "models": []}
    for index, model in enumerate(args.models):
        result = probe(args, key, model, index)
        report["models"].append(result)
        save_json(args.output_dir / "summary.json", report)
        print(json.dumps({"model": model, "handshake_status": result["handshake_status"],
                          "turns": len(result["turns"]), "success": result["success"],
                          "error_type": result.get("error_type")}), flush=True)
    return 0 if all(result["success"] for result in report["models"]) else 1


if __name__ == "__main__":
    raise SystemExit(main())
