"""Bounded Responses load probe. No retries, no output or credentials in reports."""
from __future__ import annotations

import argparse
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timezone
from email.utils import parsedate_to_datetime
import hashlib
import http.client
import json
import math
import os
from pathlib import Path
import re
import socket
import ssl
import threading
import time
from urllib.parse import urlsplit
import uuid


MAX_EVENT_BYTES = 1024 * 1024
MAX_RESPONSE_BYTES = 16 * 1024 * 1024
TERMINALS = {"response.completed", "response.failed", "response.incomplete", "error"}
ERROR_CODES = frozenset({"token_revoked", "invalid_token", "invalid_api_key", "authentication_error",
    "rate_limit_exceeded", "rate_limit_error", "usage_limit", "usage_limit_reached", "insufficient_quota",
    "overloaded", "overloaded_error", "server_error", "internal_server_error", "service_unavailable",
    "temporarily_unavailable", "upstream_error", "proxy_error"})
RATE_LIMIT_CODES = frozenset({"rate_limit_exceeded", "rate_limit_error", "usage_limit", "usage_limit_reached", "insufficient_quota"})


def safe_error_code(payload):
    if not isinstance(payload, dict):
        return None
    candidates = [payload]
    for key in ("error", "response"):
        value = payload.get(key)
        if isinstance(value, dict):
            candidates.append(value)
            if isinstance(value.get("error"), dict):
                candidates.append(value["error"])
    found = False
    for candidate in candidates:
        for key in ("code", "type"):
            value = candidate.get(key)
            if isinstance(value, str) and value in ERROR_CODES:
                return value
            if key == "code" and value is not None:
                found = True
    return "other" if found else None


def safe_correlation_id(value, kind):
    if not isinstance(value, str) or not 8 <= len(value) <= 128:
        return None
    prefix = r"resp[_-][A-Za-z0-9_-]+" if kind == "response" else r"(?:req(?:uest)?[_-][A-Za-z0-9_-]+|[a-fA-F0-9]{16,64}|[a-fA-F0-9]{8}(?:-[a-fA-F0-9]{4}){3}-[a-fA-F0-9]{12})"
    return value if re.fullmatch(prefix, value) else None


class SSEParser:
    def __init__(self):
        self.buffer = b""
        self.data = []
        self.event_bytes = 0

    def feed(self, chunk):
        self.buffer += chunk
        if len(self.buffer) > MAX_EVENT_BYTES:
            raise ValueError("event_limit")
        events = []
        while self.buffer:
            positions = [n for n in (self.buffer.find(b"\r"), self.buffer.find(b"\n")) if n >= 0]
            if not positions:
                break
            end = min(positions)
            if self.buffer[end:] == b"\r":
                break
            line, delimiter = self.buffer[:end], self.buffer[end:end + 2]
            self.buffer = self.buffer[end + (2 if delimiter == b"\r\n" else 1):]
            if not line:
                if self.data:
                    payload = b"\n".join(self.data)
                    if payload != b"[DONE]":
                        event = json.loads(payload)
                        if not isinstance(event, dict):
                            raise ValueError("invalid_event")
                        events.append(event)
                self.data, self.event_bytes = [], 0
            elif line.startswith(b"data:"):
                value = line[5:]
                if value.startswith(b" "):
                    value = value[1:]
                self.event_bytes += len(value)
                if self.event_bytes > MAX_EVENT_BYTES:
                    raise ValueError("event_limit")
                self.data.append(value)
        return events


def retry_seconds(value):
    if not value:
        return None
    try:
        seconds = float(value)
        return max(0.0, seconds) if math.isfinite(seconds) else None
    except ValueError:
        try:
            stamp = parsedate_to_datetime(value)
            if stamp.tzinfo is None:
                stamp = stamp.replace(tzinfo=timezone.utc)
            return max(0.0, (stamp - datetime.now(timezone.utc)).total_seconds())
        except (ValueError, TypeError, OverflowError):
            return None


def usage_numbers(response):
    usage = response.get("usage") or {}
    if not isinstance(usage, dict):
        return {}
    result = {}
    for key in ("input_tokens", "output_tokens", "total_tokens"):
        value = usage.get(key)
        if isinstance(value, int) and not isinstance(value, bool) and value >= 0:
            result[key] = value
    details = usage.get("input_tokens_details") or {}
    if isinstance(details, dict) and isinstance(details.get("cached_tokens"), int):
        result["cached_tokens"] = max(0, details["cached_tokens"])
    return result


def classify_http(payload):
    error = payload.get("error") if isinstance(payload, dict) else None
    if not isinstance(error, dict):
        return "unknown", "http_error"
    if error.get("type") == "proxy_error":
        message = str(error.get("message", "")).lower()
        if "busy" in message:
            return "gateway_local_admission", "account_busy"
        if message == "upstream connection failed":
            return "gateway_transport", "upstream_connection_failed"
        return "gateway_local", "proxy_error"
    # A forwarded provider-shaped body is evidence, not proof of its origin.
    return "upstream_payload_inferred", "provider_error_payload"


class Inflight:
    def __init__(self):
        self.lock = threading.Lock()
        self.current = self.peak = 0

    def enter(self):
        with self.lock:
            self.current += 1
            self.peak = max(self.peak, self.current)

    def leave(self):
        with self.lock:
            self.current -= 1


def endpoint(base_url):
    parsed = urlsplit(base_url)
    if parsed.scheme not in ("http", "https") or not parsed.hostname or parsed.username or parsed.password or parsed.query or parsed.fragment:
        raise ValueError("base_url must be an HTTP(S) origin or /v1 base without credentials, query, or fragment")
    path = parsed.path.rstrip("/")
    if not path.endswith("/responses"):
        path += "/responses" if path.endswith("/v1") else "/v1/responses"
    return parsed, path


def request_once(config, key, request_id, inflight, barrier=None):
    if barrier:
        barrier.wait(timeout=30)
    started = time.monotonic()
    deadline = started + config["timeout_seconds"]
    result = {"request_id": request_id, "http_status": None, "success": False,
              "origin": "client", "outcome": "client_error", "terminal": None,
              "ttft_ms": None, "first_event_ms": None, "headers_ms": None,
              "retry_after_seconds": None, "usage": {}, "attempts": 1,
              "error_code": None, "observed_request_id": None, "observed_response_id": None}
    parsed, path = endpoint(config["base_url"])
    connection_class = http.client.HTTPSConnection if parsed.scheme == "https" else http.client.HTTPConnection
    connection = connection_class(parsed.hostname, parsed.port, timeout=config["timeout_seconds"])
    expired = threading.Event()
    sockets = []
    def terminate_at_deadline():
        expired.set()
        for active_socket in sockets:
            try:
                active_socket.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass
            active_socket.close()
    watchdog = threading.Timer(config["timeout_seconds"], terminate_at_deadline)
    watchdog.daemon = True
    watchdog.start()
    # Each request has a separate connection: observed concurrency is not an
    # artifact of a smaller client-side connection pool or HTTP/2 multiplexing.
    inflight.enter()
    try:
        connection.connect()
        sockets.append(connection.sock)
        if expired.is_set():
            raise TimeoutError
        payload = json.dumps({"model": config["model"], "input": "Reply with exactly OK.",
                              "instructions": "This is a synthetic bounded availability check.",
                              "stream": True, "store": False}).encode()
        connection.request("POST", path, payload, {"Authorization": "Bearer " + key,
            "Content-Type": "application/json", "Accept": "text/event-stream",
            "User-Agent": "codex-agent-environment-probe/1", "X-Request-Id": request_id,
            "Session-Id": request_id})
        if connection.sock:
            connection.sock.settimeout(max(0.001, deadline - time.monotonic()))
        response = connection.getresponse()
        result["headers_ms"] = round((time.monotonic() - started) * 1000, 3)
        result["http_status"] = response.status
        result["retry_after_seconds"] = retry_seconds(response.getheader("Retry-After"))
        for header in ("X-Request-Id", "Request-Id", "OpenAI-Request-Id"):
            correlation = safe_correlation_id(response.getheader(header), "request")
            if correlation:
                result["observed_request_id"] = correlation
                result["observed_request_id_header"] = header.lower()
                break
        parser, total = SSEParser(), 0
        content_type = response.getheader("Content-Type", "").lower()
        if response.status != 200:
            data = b""
            while len(data) <= MAX_EVENT_BYTES:
                if expired.is_set() or time.monotonic() >= deadline:
                    raise TimeoutError
                chunk = response.read1(min(16384, MAX_EVENT_BYTES + 1 - len(data)))
                if not chunk:
                    break
                data += chunk
            try:
                body = json.loads(data) if len(data) <= MAX_EVENT_BYTES else None
            except (json.JSONDecodeError, UnicodeDecodeError):
                body = None
            result["origin"], result["outcome"] = classify_http(body)
            result["error_code"] = safe_error_code(body)
            return result
        if "text/event-stream" not in content_type:
            result.update(origin="protocol", outcome="unexpected_content_type")
            return result
        while True:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise TimeoutError
            if connection.sock:
                connection.sock.settimeout(remaining)
            chunk = response.read1(16384)
            if not chunk:
                if expired.is_set():
                    raise TimeoutError
                result.update(origin="protocol", outcome="missing_terminal_event")
                return result
            total += len(chunk)
            if total > MAX_RESPONSE_BYTES:
                result.update(origin="client_limit", outcome="response_byte_limit")
                return result
            for event in parser.feed(chunk):
                elapsed = round((time.monotonic() - started) * 1000, 3)
                if result["first_event_ms"] is None:
                    result["first_event_ms"] = elapsed
                kind = event.get("type")
                body = event.get("response")
                if isinstance(body, dict):
                    correlation = safe_correlation_id(body.get("id"), "response")
                    if correlation:
                        result["observed_response_id"] = correlation
                correlation = safe_correlation_id(event.get("request_id"), "request")
                if correlation:
                    result["observed_request_id"] = correlation
                    result["observed_request_id_header"] = "sse.request_id"
                if kind == "response.output_text.delta" and event.get("delta") and result["ttft_ms"] is None:
                    result["ttft_ms"] = elapsed
                if kind in TERMINALS:
                    result["terminal"] = kind
                    result["error_code"] = safe_error_code(event)
                    body = event.get("response")
                    if isinstance(body, dict):
                        result["usage"] = usage_numbers(body)
                    success = kind == "response.completed" and isinstance(body, dict) and body.get("status") == "completed"
                    result.update(success=success, origin="upstream_terminal_event", outcome="completed" if success else "terminal_failure")
                    return result
    except (TimeoutError, socket.timeout):
        result.update(origin="client", outcome="timeout")
    except (OSError, http.client.HTTPException, ssl.SSLError) as error:
        result.update(origin="client", outcome="timeout" if expired.is_set() else "transport_error", exception_type=type(error).__name__)
    except (ValueError, UnicodeDecodeError):
        result.update(origin="protocol", outcome="invalid_sse")
    finally:
        watchdog.cancel()
        result["duration_ms"] = round((time.monotonic() - started) * 1000, 3)
        connection.close()
        inflight.leave()
    return result


def percentile(values, fraction):
    values = sorted(value for value in values if value is not None)
    return values[max(0, math.ceil(len(values) * fraction) - 1)] if values else None


def summarize(rows):
    codes, outcomes, origins, usage, error_codes = {}, {}, {}, {}, {}
    for row in rows:
        code = str(row["http_status"]) if row["http_status"] is not None else "no_http_response"
        codes[code] = codes.get(code, 0) + 1
        outcomes[row["outcome"]] = outcomes.get(row["outcome"], 0) + 1
        origins[row["origin"]] = origins.get(row["origin"], 0) + 1
        if row.get("error_code"):
            error_codes[row["error_code"]] = error_codes.get(row["error_code"], 0) + 1
        for key, value in row["usage"].items():
            usage[key] = usage.get(key, 0) + value
    success = [row for row in rows if row["success"]]
    ttfts = [row["ttft_ms"] for row in success]
    durations = [row["duration_ms"] for row in rows]
    return {"requests": len(rows), "completed": len(success), "failed": len(rows) - len(success),
            "http_codes": codes, "outcomes": outcomes, "origins": origins, "error_codes": error_codes,
            "latency_all_p50_ms": percentile(durations, .5), "latency_all_p95_ms": percentile(durations, .95),
            "ttft_completed_p50_ms": percentile(ttfts, .5), "ttft_completed_p95_ms": percentile(ttfts, .95),
            "ttft_samples": sum(value is not None for value in ttfts), "observed_usage": usage,
            "usage_scope": "Only received terminal-event usage; failed/missing streams can have unreported billable usage.",
            "observed_gateway_local_rejections": origins.get("gateway_local_admission", 0),
            "upstream_peak_inflight": None, "upstream_admitted_requests": None}


def load_metadata(path):
    if path is None:
        return {}
    data = json.loads(path.read_text(encoding="utf-8"))
    allowed = {"binary_sha256", "source_sha256", "snapshot_sha256"}
    if not isinstance(data, dict) or set(data) - allowed:
        raise ValueError("metadata permits binary_sha256/source_sha256/snapshot_sha256 only")
    for value in data.values():
        if not isinstance(value, str) or len(value) != 64 or any(ch not in "0123456789abcdef" for ch in value.lower()):
            raise ValueError("metadata values must be SHA-256 hex digests")
    return data


def save_report(path, report):
    temporary = path.with_suffix(".tmp")
    temporary.write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    temporary.replace(path)


def run_load(config, key, output, metadata=None):
    output.mkdir(parents=True, exist_ok=False)
    report_path = output / "report.json"
    parsed, path = endpoint(config["base_url"])
    endpoint_hash = hashlib.sha256((parsed.scheme + "://" + parsed.netloc + path).encode()).hexdigest()
    report = {"schema_version": 1, "run_id": "load-" + uuid.uuid4().hex[:16],
        "started_utc": datetime.now(timezone.utc).isoformat(), "status": "running", "metadata": metadata or {},
        "target_sha256": endpoint_hash, "model": config["model"], "config": {k:v for k,v in config.items() if k != "base_url"},
        "upstream_peak_inflight": None, "auto_retries": 0, "waves": [], "cooldowns": [],
        "summary_scope": "load_waves_only; sanity request is reported separately",
        "measurement_boundary": "client HTTP requests; upstream concurrency/admission require separate server evidence",
        "script_sha256": hashlib.sha256(Path(__file__).read_bytes()).hexdigest()}
    save_report(report_path, report)
    inflight = Inflight()
    sanity = request_once(config, key, report["run_id"] + "-sanity", inflight)
    report["sanity"] = sanity
    if not sanity["success"]:
        report.update(status="stopped_sanity_failed", finished_utc=datetime.now(timezone.utc).isoformat())
        save_report(report_path, report)
        return report
    all_rows = []
    stopped = False
    for level in config["levels"]:
        for wave in range(1, config["waves"] + 1):
            inflight = Inflight()
            barrier = threading.Barrier(level + 1)
            begin = time.monotonic()
            with ThreadPoolExecutor(max_workers=level) as executor:
                futures = [executor.submit(request_once, config, key, f"{report['run_id']}-c{level}-w{wave}-r{index}", inflight, barrier) for index in range(level)]
                barrier.wait(timeout=30)
                rows = [future.result() for future in futures]
            all_rows.extend(rows)
            result = {"level": level, "wave": wave, "client_peak_inflight": inflight.peak,
                      "duration_seconds": round(time.monotonic() - begin, 3), "summary": summarize(rows), "requests": rows}
            report["waves"].append(result)
            report["summary"] = summarize(all_rows)
            save_report(report_path, report)
            limited = [row for row in rows if row["http_status"] == 429 or row.get("error_code") in RATE_LIMIT_CODES]
            auth_error = any(row["http_status"] in (401, 403) for row in rows)
            timeout_count = sum(row["outcome"] == "timeout" for row in rows)
            if auth_error or timeout_count == level:
                report["status"] = "stopped_auth_failure" if auth_error else "stopped_all_timeout"
                stopped = True
                break
            retry_delay = max((row["retry_after_seconds"] or 0 for row in limited), default=0)
            if limited and retry_delay > config["max_cooldown_seconds"]:
                report.update(status="stopped_retry_after_exceeds_budget", next_allowed_after_seconds=retry_delay)
                stopped = True
                break
            pause = max(config["cooldown_seconds"], retry_delay)
            has_next = wave < config["waves"] or level != config["levels"][-1]
            if limited or has_next:
                report["cooldowns"].append({"after_level": level, "after_wave": wave, "seconds": pause, "reason": "429_retry_after" if limited else "between_waves"})
                save_report(report_path, report)
                time.sleep(pause)
            if limited and config["stop_on_429"]:
                report["status"] = "stopped_rate_limited"
                stopped = True
                break
        if stopped:
            break
    if not stopped:
        report["status"] = "completed" if all(row["success"] for row in all_rows) else "completed_with_failures"
    report["finished_utc"] = datetime.now(timezone.utc).isoformat()
    save_report(report_path, report)
    return report


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base-url", required=True)
    parser.add_argument("--model", required=True)
    credentials = parser.add_mutually_exclusive_group(required=True)
    credentials.add_argument("--key-env", help="Name of environment variable holding gateway API key")
    credentials.add_argument("--key-file", type=Path, help="Existing private file containing only gateway API key")
    parser.add_argument("--levels", default="30,50,70,100")
    parser.add_argument("--waves", type=int, default=2)
    parser.add_argument("--timeout", type=float, default=120)
    parser.add_argument("--cooldown", type=float, default=10)
    parser.add_argument("--max-cooldown", type=float, default=120)
    parser.add_argument("--continue-after-429", action="store_true", help="Wait Retry-After then continue remaining finite waves; default stops after cooling down")
    parser.add_argument("--metadata-file", type=Path)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    try:
        levels = [int(value) for value in args.levels.split(",")]
        if not levels or len(levels) > 8 or any(level < 1 or level > 100 for level in levels) or sorted(set(levels)) != levels:
            raise ValueError("levels must be strictly ascending integers from 1 to 100, at most eight levels")
        if not 1 <= args.waves <= 3 or not 1 <= args.timeout <= 600 or not 0 <= args.cooldown <= args.max_cooldown <= 600:
            raise ValueError("waves must be 1..3, timeout 1..600, cooldown 0..max-cooldown<=600")
        endpoint(args.base_url)
        key = args.key_file.read_text(encoding="utf-8").strip() if args.key_file else os.environ.get(args.key_env, "").strip()
        if not key or "\n" in key or "\r" in key:
            raise ValueError("key must be present and contain no line breaks")
        metadata = load_metadata(args.metadata_file)
        config = {"base_url": args.base_url, "model": args.model, "levels": levels, "waves": args.waves,
                  "timeout_seconds": args.timeout, "cooldown_seconds": args.cooldown,
                  "max_cooldown_seconds": args.max_cooldown, "stop_on_429": not args.continue_after_429}
        report = run_load(config, key, args.output.resolve(), metadata)
    except (ValueError, OSError) as error:
        # Do not echo arbitrary exception text containing URLs or credential paths.
        parser.exit(2, f"Invalid configuration or local I/O failure ({type(error).__name__}); check arguments and private input files.\n")
    print(json.dumps({"status": report["status"], "report": str(args.output.resolve() / "report.json"), "summary": report.get("summary", {})}, ensure_ascii=False, indent=2))
    return 0 if report["status"] == "completed" else 1


if __name__ == "__main__":
    raise SystemExit(main())
