"""Standalone Responses benchmark: functional coverage, blind candy trials, and bursts.

Uses only the Python standard library. There are no automatic retries, redirects,
environment proxies, or output-token caps. The key is read from a private file and
never written to results. Client concurrency does not prove upstream concurrency.
"""
from __future__ import annotations

import argparse
import copy
from collections import Counter
from concurrent.futures import ThreadPoolExecutor, as_completed
from datetime import datetime, timezone
import hashlib
import http.client
import json
import math
from pathlib import Path
import re
import socket
import threading
import time
from urllib.parse import urlsplit
import uuid


CANDY_PROMPT = """在一个黑色的袋子里放有三种口味的糖果，每种糖果有两种不同的形状（圆形和五角星形，不同的形状靠手感可以分辨）。现已知不同口味的糖和不同形状的数量统计如下表。参赛者需要在活动前决定摸出的糖果数目，那么，最少取出多少个糖果才能保证手中同时拥有不同形状的苹果味和桃子味的糖？（同时手中有圆形苹果味匹配五角星桃子味糖果，或者有圆形桃子味匹配五角星苹果味糖果都满足要求）
苹果味 桃子味 西瓜味
圆形 7 9 8
五角星形 7 6 4"""
CANDY_INSTRUCTIONS = "请独立解答用户给出的题目，解释推理过程，并在最后单独一行写“最终答案：N个”，其中N替换为你的整数答案。"
TERMINALS = {"response.completed", "response.failed", "response.incomplete", "error"}
MAX_BYTES = 32 * 1024 * 1024


def utc_now():
    return datetime.now(timezone.utc).isoformat()


def endpoint(base_url):
    parsed = urlsplit(base_url)
    if (parsed.scheme not in ("http", "https") or not parsed.hostname
            or parsed.username or parsed.password or parsed.query or parsed.fragment):
        raise ValueError("base-url must be an HTTP(S) origin or Responses URL")
    path = parsed.path.rstrip("/")
    if not path.endswith("/responses"):
        path += "/responses" if path.endswith("/v1") else "/v1/responses"
    return parsed, path


class SSEParser:
    """Incremental SSE parser, including CRLF and split UTF-8 boundaries."""

    def __init__(self):
        self.buffer = b""
        self.data = []
        self.event_bytes = 0

    def feed(self, chunk):
        self.buffer += chunk
        events = []
        if len(self.buffer) > MAX_BYTES:
            raise ValueError("SSE line exceeds byte limit")
        while self.buffer:
            positions = [n for n in (self.buffer.find(b"\r"), self.buffer.find(b"\n")) if n >= 0]
            if not positions:
                break
            end = min(positions)
            if self.buffer[end:] == b"\r":
                break
            line = self.buffer[:end]
            width = 2 if self.buffer[end:end + 2] == b"\r\n" else 1
            self.buffer = self.buffer[end + width:]
            if not line:
                if self.data:
                    raw = b"\n".join(self.data)
                    if raw != b"[DONE]":
                        event = json.loads(raw)
                        if not isinstance(event, dict):
                            raise ValueError("SSE data must be a JSON object")
                        events.append(event)
                self.data, self.event_bytes = [], 0
            elif line.startswith(b"data:"):
                value = line[5:]
                if value.startswith(b" "):
                    value = value[1:]
                self.event_bytes += len(value)
                if self.event_bytes > MAX_BYTES:
                    raise ValueError("SSE event exceeds byte limit")
                self.data.append(value)
        return events


class Inflight:
    def __init__(self):
        self.lock = threading.Lock()
        self.current = self.peak = 0

    def enter(self):
        with self.lock:
            self.current += 1
            self.peak = max(self.current, self.peak)

    def leave(self):
        with self.lock:
            self.current -= 1


def output_text(response):
    if not isinstance(response, dict):
        return ""
    parts = []
    for item in response.get("output", []):
        contents = item.get("content", [])
        if not isinstance(contents, list):
            continue
        for content in contents:
            if not isinstance(content, dict):
                continue
            if content.get("type") == "output_text" and isinstance(content.get("text"), str):
                parts.append(content["text"])
    return "".join(parts)


def reconstruct_response(response, events):
    """Build usable output without modifying the terminal response or events."""
    if not isinstance(response, dict):
        return response, {"source": "no_response", "items": []}
    effective = copy.deepcopy(response)
    if response.get("output"):
        return effective, {"source": "terminal_response", "items": []}
    items, item_ids, sources = {}, {}, {}
    argument_values, argument_deltas, text_values = {}, {}, {}
    for event in events:
        kind = event.get("type")
        item = event.get("item")
        index = event.get("output_index")
        if not isinstance(index, int) or isinstance(index, bool):
            index = item_ids.get(event.get("item_id") or (item.get("id") if isinstance(item, dict) else None))
        if kind in ("response.output_item.added", "response.output_item.done") and isinstance(item, dict):
            if index is None:
                index = max(items, default=-1) + 1
            if kind == "response.output_item.done" or index not in items:
                items[index] = copy.deepcopy(item)
                sources[index] = kind
            if item.get("id"):
                item_ids[item["id"]] = index
        if index is None:
            continue
        if kind == "response.function_call_arguments.done" and isinstance(event.get("arguments"), str):
            argument_values[index] = event["arguments"]
        elif kind == "response.function_call_arguments.delta" and isinstance(event.get("delta"), str):
            argument_deltas.setdefault(index, []).append(event["delta"])
        elif kind == "response.output_text.done" and isinstance(event.get("text"), str):
            text_values[(index, event.get("content_index", 0))] = event["text"]
        elif kind == "response.content_part.done" and isinstance(event.get("part"), dict):
            part = event["part"]
            if part.get("type") == "output_text" and isinstance(part.get("text"), str):
                text_values[(index, event.get("content_index", 0))] = part["text"]
    details = []
    for index in sorted(items):
        item, source = items[index], sources[index]
        repairs = []
        if item.get("type") == "function_call" and not item.get("arguments"):
            if index in argument_values:
                item["arguments"] = argument_values[index]
                repairs.append("response.function_call_arguments.done")
            elif index in argument_deltas:
                item["arguments"] = "".join(argument_deltas[index])
                repairs.append("response.function_call_arguments.delta")
        if item.get("type") == "message" and not output_text({"output": [item]}):
            texts = [(content_index, text) for (output_index, content_index), text in text_values.items()
                     if output_index == index]
            if texts:
                item["content"] = [{"type": "output_text", "text": text, "annotations": []}
                                   for _, text in sorted(texts)]
                repairs.append("response.output_text.done_or_content_part.done")
        details.append({"output_index": index, "source": source, "repairs": repairs})
    if items:
        effective["output"] = [items[index] for index in sorted(items)]
        return effective, {"source": "sse_output_items", "items": details}
    return effective, {"source": "terminal_response_empty_without_items", "items": []}


def effective_response(row):
    return row.get("effective_response") or row.get("response") or {}


def payload(model, input_value, effort, stream=True, **extra):
    body = {"model": model, "input": input_value, "instructions": "Follow the user request exactly.",
            "store": False, "stream": stream, "reasoning": {"effort": effort}}
    body.update(extra)
    return body


def candy_body(model, effort):
    return payload(model, CANDY_PROMPT, effort, instructions=CANDY_INSTRUCTIONS)


def request_once(config, key, body, case, inflight, barrier=None, session_id=None, compact=False):
    request_id = "req_bench_" + uuid.uuid4().hex
    encoded = json.dumps(body, ensure_ascii=False).encode("utf-8")
    # Save synthetic input before the synchronized start so disk I/O does not
    # inflate the latency or the observed count of active HTTP requests.
    (config["output_dir"] / "requests" / (request_id + ".json")).write_bytes(encoded + b"\n")
    events_directory = config["output_dir"] / "events"
    events_directory.mkdir(exist_ok=True)
    result = {"request_id": request_id, "case": case, "model": body["model"],
              "phase": config["phase"], "effort": config["effort"], "stream": body["stream"],
              "attempts": 1, "http_status": None, "terminal": None, "protocol_success": False,
              "failure_category": None, "outcome": "pending", "ttft_ms": None,
              "first_event_ms": None, "headers_ms": None, "first_byte_ms": None,
              "response": None, "output_text": "", "event_counts": {}, "usage": {}}
    if barrier:
        barrier.wait(timeout=60)
    started = time.monotonic()
    result["started_at"] = utc_now()
    deadline = started + config["timeout"]
    parsed, path = endpoint(config["base_url"])
    if compact:
        path += "/compact"
    result["endpoint"] = path
    connection_class = http.client.HTTPSConnection if parsed.scheme == "https" else http.client.HTTPConnection
    connection = connection_class(parsed.hostname, parsed.port, timeout=config["timeout"])
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

    watchdog = threading.Timer(config["timeout"], expire)
    watchdog.daemon = True
    inflight.enter()
    watchdog.start()
    result["request_sha256"] = hashlib.sha256(encoded).hexdigest()
    result["input_bytes"] = len(encoded)
    deltas = []
    events = []
    event_counts = Counter()

    def elapsed():
        return round((time.monotonic() - started) * 1000, 3)

    def fail(category, outcome):
        result.update(failure_category=category, outcome=outcome)

    try:
        connection.connect()
        sockets.append(connection.sock)
        if expired.is_set():
            raise TimeoutError("absolute request deadline")
        connection.request("POST", path, encoded, {
            "Authorization": "Bearer " + key, "Content-Type": "application/json",
            "Accept": "text/event-stream" if body["stream"] else "application/json",
            "User-Agent": "codex-agent-mother-benchmark/1", "X-Request-Id": request_id,
            "X-Client-Request-ID": request_id,
            "Session-Id": session_id or request_id})
        response = connection.getresponse()
        result.update(http_status=response.status, headers_ms=elapsed(),
                      content_type=response.getheader("Content-Type", ""),
                      retry_after=response.getheader("Retry-After"))
        result["response_headers"] = {name: response.getheader(name) for name in
            ("X-Request-Id", "Request-Id", "OpenAI-Request-Id", "Server", "CF-Ray")
            if response.getheader(name) is not None}
        is_sse = body["stream"] and "text/event-stream" in result["content_type"].lower()
        parser, raw, total = SSEParser(), bytearray(), 0
        terminal_seen = False
        while True:
            remaining = deadline - time.monotonic()
            if remaining <= 0 or expired.is_set():
                raise TimeoutError("absolute request deadline")
            if connection.sock:
                connection.sock.settimeout(remaining)
            chunk = response.read1(16384)
            if not chunk:
                break
            if result["first_byte_ms"] is None:
                result["first_byte_ms"] = elapsed()
            total += len(chunk)
            if total > MAX_BYTES:
                fail("client_limit", "response_byte_limit")
                break
            if not is_sse or response.status != 200:
                raw.extend(chunk)
                continue
            for event in parser.feed(chunk):
                events.append(event)
                kind = event.get("type", "unknown")
                event_counts[kind] += 1
                if result["first_event_ms"] is None:
                    result["first_event_ms"] = elapsed()
                if kind == "response.output_text.delta" and event.get("delta"):
                    deltas.append(event["delta"])
                    if result["ttft_ms"] is None:
                        result["ttft_ms"] = elapsed()
                if kind in TERMINALS:
                    result["terminal"] = kind
                    result["response"] = event.get("response")
                    result["terminal_event"] = event
                    terminal_seen = True
                    completed = (kind == "response.completed" and isinstance(result["response"], dict)
                                 and result["response"].get("status") == "completed")
                    result["protocol_success"] = completed
                    if completed:
                        result["outcome"] = "completed"
                    else:
                        fail("response_error", kind)
            if terminal_seen:
                break
        result["response_bytes"] = total
        if response.status != 200:
            fail("http_error", "http_" + str(response.status))
            result["error_body"] = raw.decode("utf-8", errors="replace")
        elif result["failure_category"] == "client_limit":
            pass
        elif body["stream"] and not is_sse:
            fail("protocol_error", "expected_text_event_stream")
            result["error_body"] = raw.decode("utf-8", errors="replace")
        elif is_sse and not terminal_seen:
            fail("protocol_error", "missing_terminal_event")
        elif not body["stream"]:
            parsed_response = json.loads(raw)
            if not isinstance(parsed_response, dict):
                raise ValueError("response must be a JSON object")
            result["response"] = parsed_response
            if compact:
                result["terminal"] = "json_compaction"
                result["protocol_success"] = (isinstance(parsed_response.get("output"), list)
                                               and not parsed_response.get("error"))
            else:
                result["terminal"] = "json_response." + str(parsed_response.get("status", "missing"))
                result["protocol_success"] = parsed_response.get("status") == "completed"
            if result["protocol_success"]:
                result["outcome"] = "completed"
            else:
                fail("response_error", result["terminal"])
    except (TimeoutError, socket.timeout):
        fail("timeout", "client_deadline_or_socket_timeout")
    except (OSError, http.client.HTTPException) as error:
        fail("timeout" if expired.is_set() else "transport_error", type(error).__name__)
    except (ValueError, UnicodeError) as error:
        fail("protocol_error", type(error).__name__)
    finally:
        watchdog.cancel()
        connection.close()
        inflight.leave()
        result["duration_ms"] = elapsed()
        result["finished_at"] = utc_now()
        result["event_counts"] = dict(event_counts)
        result["effective_response"], result["output_reconstruction"] = reconstruct_response(result["response"], events)
        result["output_text"] = output_text(result["effective_response"]) or "".join(deltas)
        if events:
            event_path = events_directory / (request_id + ".jsonl")
            with event_path.open("w", encoding="utf-8") as event_file:
                for event in events:
                    event_file.write(json.dumps(event, ensure_ascii=False) + "\n")
            result["events_file"] = "events/" + event_path.name
        if isinstance(result["response"], dict):
            result["usage"] = result["response"].get("usage") or {}
            result["response_model"] = result["response"].get("model")
            result["response_id"] = result["response"].get("id")
        if expired.is_set() and not result["protocol_success"]:
            fail("timeout", "absolute_request_deadline")
    return result


def extract_candy_answer(text):
    """Extract the final conclusion, never infer correctness from a stray '21'."""
    cleaned = re.sub(r"[*`${}\\]", "", text)
    patterns = [r"最终答案\s*[：:]?\s*(?:为|是)?\s*(\d+)\s*个?",
                r"(?:最少|至少)(?:需要|应|要|取出|摸出|抽取|拿出|拿|取|摸|抽|糖果|的|数量|数目|为|是|\s)*[：:]?\s*(\d+)\s*(?:个|颗)",
                r"(?:答案|结论)\s*(?:为|是)?\s*[：:]?\s*(\d+)\s*(?:个|颗)?"]
    for pattern in patterns:
        matches = list(re.finditer(pattern, cleaned))
        if matches:
            return int(matches[-1].group(1)), "final_conclusion_regex"
    # Preserve an unparsed answer for manual review instead of treating any
    # occurrence of the expected number as a correct conclusion.
    match = re.fullmatch(r"\s*(\d+)\s*(?:个|颗)?[。.!！]?\s*", cleaned)
    return (int(match.group(1)), "integer_only") if match else (None, "manual_review_required")


def assess(row, kind, expected=None):
    row["content_kind"] = kind
    row["content_correct"] = None
    if not row["protocol_success"]:
        row["content_assessment"] = "not_scored_protocol_failure"
        return row
    text = row["output_text"].strip()
    if kind == "candy":
        answer, method = extract_candy_answer(text)
        row.update(candy_answer=answer, candy_expected_answer=21, answer_extraction=method)
        row["content_correct"] = None if answer is None else answer == 21
    elif kind == "exact":
        row["content_correct"] = text == expected
    elif kind == "json":
        try:
            row["content_correct"] = json.loads(text) == expected
        except ValueError:
            row["content_correct"] = False
    elif kind == "function_call":
        calls = [item for item in effective_response(row).get("output", [])
                 if item.get("type") == "function_call"]
        correct = len(calls) == 1 and calls[0].get("name") == "add"
        if correct:
            try:
                correct = json.loads(calls[0].get("arguments", "")) == {"a": 17, "b": 25}
            except ValueError:
                correct = False
        row["content_correct"] = correct
    elif kind == "compaction":
        row["content_correct"] = any(item.get("type") == "compaction" and item.get("encrypted_content")
                                      for item in effective_response(row).get("output", []))
    row["content_assessment"] = ("correct" if row["content_correct"] is True else
                                 "incorrect" if row["content_correct"] is False else "manual_review_required")
    return row


def percentile(values, fraction):
    values = sorted(value for value in values if value is not None)
    return values[max(0, math.ceil(len(values) * fraction) - 1)] if values else None


def summarize(rows):
    total = len(rows)
    completed = sum(row["protocol_success"] for row in rows)
    scored = [row for row in rows if row.get("content_correct") is not None]
    incorrect = sum(row["content_correct"] is False for row in scored)
    candies = [row for row in rows if row.get("content_kind") == "candy"]
    correct_candies = sum(row.get("content_correct") is True for row in candies)
    failed = [row for row in rows if not row["protocol_success"]]
    return {"requests": total, "protocol_completed": completed, "request_failures": len(failed),
            "request_error_rate": len(failed) / total if total else None,
            "protocol_errors": sum(row.get("failure_category") == "protocol_error" for row in rows),
            "failure_categories": dict(Counter(row.get("failure_category") for row in failed)),
            "http_statuses": dict(Counter(str(row["http_status"]) for row in rows)),
            "terminal_counts": dict(Counter(str(row["terminal"]) for row in rows)),
            "content_scored": len(scored), "content_errors": incorrect,
            "content_error_rate_among_scored": incorrect / len(scored) if scored else None,
            "content_unparsed_completed": sum(row["protocol_success"] and row.get("content_correct") is None for row in rows),
            "candy_requests": len(candies), "candy_correct": correct_candies,
            "candy_accuracy_all_requests": correct_candies / len(candies) if candies else None,
            "candy_answers": dict(Counter(str(row.get("candy_answer")) for row in candies)),
            "duration_ms": {"p50": percentile([row["duration_ms"] for row in rows], .5),
                            "p95": percentile([row["duration_ms"] for row in rows], .95),
                            "max": max((row["duration_ms"] for row in rows), default=None)},
            "ttft_ms_completed": {"p50": percentile([row["ttft_ms"] for row in rows if row["protocol_success"]], .5),
                                   "p95": percentile([row["ttft_ms"] for row in rows if row["protocol_success"]], .95),
                                   "samples": sum(row["protocol_success"] and row["ttft_ms"] is not None for row in rows)},
            "usage_tokens_observed": {name: sum(row.get("usage", {}).get(name, 0) or 0 for row in rows)
                                      for name in ("input_tokens", "output_tokens", "total_tokens")}}


def save_json(path, data):
    temporary = path.with_suffix(".tmp")
    temporary.write_text(json.dumps(data, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    temporary.replace(path)


def functional(config, key, model, record):
    inflight = Inflight()

    def check(case, body, kind="exact", expected=None, session_id=None):
        row = assess(request_once(config, key, body, case, inflight, session_id=session_id), kind, expected)
        record(row)
        return row

    effort = config["effort"]
    check("sse", payload(model, "Reply exactly SSE_OK.", effort), expected="SSE_OK")
    check("non_stream", payload(model, "Reply exactly NONSTREAM_OK.", effort, stream=False), expected="NONSTREAM_OK")
    check("chinese", payload(model, "请只回复：中文测试通过", effort), expected="中文测试通过")
    schema = {"type": "object", "properties": {"city": {"type": "string"}, "count": {"type": "integer"}},
              "required": ["city", "count"], "additionalProperties": False}
    check("json_schema", payload(model, 'Return JSON with city equal to 北京 and count equal to 3.', effort,
          text={"format": {"type": "json_schema", "name": "benchmark_result", "strict": True, "schema": schema}}),
          "json", {"city": "北京", "count": 3})
    marker = "NEEDLE_" + uuid.uuid4().hex[:16]
    filler = "This is irrelevant background for the long input check.\n" * 700
    check("long_input", payload(model, filler + "\nSecret marker: " + marker + "\n" + filler
          + "\nReply with the secret marker only.", effort), expected=marker)
    session = "session_bench_" + uuid.uuid4().hex
    memory = "MEMORY_" + uuid.uuid4().hex[:16]
    history = [{"role": "user", "content": "Remember this code: " + memory + ". Reply exactly SAVED."}]
    first = check("multi_turn_1", payload(model, history, effort, include=["reasoning.encrypted_content"]),
                  expected="SAVED", session_id=session)
    if first["protocol_success"]:
        followup = history + effective_response(first).get("output", []) + [
            {"role": "user", "content": "What code did I ask you to remember? Reply with the code only."}]
        check("multi_turn_2", payload(model, followup, effort), expected=memory, session_id=session)
    tool = {"type": "function", "name": "add", "description": "Add two integers.", "strict": True,
            "parameters": {"type": "object", "properties": {"a": {"type": "integer"}, "b": {"type": "integer"}},
                           "required": ["a", "b"], "additionalProperties": False}}
    tool_input = [{"role": "user", "content": "Use add to calculate 17 + 25, then reply with the result only."}]
    tool_session = "session_bench_" + uuid.uuid4().hex
    first = check("function_call", payload(model, tool_input, effort, tools=[tool],
                  tool_choice={"type": "function", "name": "add"}, include=["reasoning.encrypted_content"]),
                  "function_call", session_id=tool_session)
    if first["protocol_success"] and first["content_correct"]:
        output = effective_response(first).get("output", [])
        call = next(item for item in output if item.get("type") == "function_call")
        followup = tool_input + output + [{"type": "function_call_output", "call_id": call["call_id"], "output": "42"}]
        check("function_call_output", payload(model, followup, effort, tools=[tool], tool_choice="auto"),
              expected="42", session_id=tool_session)
    return {"peak_inflight": inflight.peak,
            "expected_cases": ["sse", "non_stream", "chinese", "json_schema", "long_input",
                               "multi_turn_1", "multi_turn_2", "function_call", "function_call_output"]}


def compact_check(config, key, model, record):
    """Check the optional compaction endpoint and preservation of usable history."""
    inflight = Inflight()
    marker = "COMPACT_MEMORY_" + uuid.uuid4().hex[:16]
    history = [{"role": "user", "content": "Remember my verification code: " + marker},
               {"role": "assistant", "content": [{"type": "output_text", "text": "I will remember that code."}]}]
    body = {"model": model, "input": history, "instructions": "Preserve the verification code exactly.", "stream": False}
    session = "session_bench_" + uuid.uuid4().hex
    first = assess(request_once(config, key, body, "compact", inflight,
                                session_id=session, compact=True), "compaction")
    record(first)
    skipped = []
    if first["protocol_success"] and first["content_correct"]:
        followup = effective_response(first)["output"] + [
            {"role": "user", "content": "What verification code did I ask you to remember? Reply with the code only."}]
        row = request_once(config, key, payload(model, followup, config["effort"]),
                           "compact_followup", inflight, session_id=session)
        record(assess(row, "exact", marker))
    else:
        skipped.append("compact_followup")
    return {"peak_inflight": inflight.peak, "skipped_dependent_cases": skipped}


def run(config, key):
    out = config["output_dir"]
    out.mkdir(parents=True, exist_ok=True)
    if any((out / name).exists() for name in ("results.jsonl", "summary.json", "requests", "events")):
        raise ValueError("output directory already contains benchmark results; choose a fresh directory")
    (out / "requests").mkdir()
    (out / "candy-prompt.txt").write_text(CANDY_PROMPT + "\n", encoding="utf-8")
    rows = []
    report = {"schema": "codex-agent-mother-benchmark-v1", "started_at": utc_now(), "status": "running",
              "phase": config["phase"], "models": config["models"], "effort": config["effort"],
              "base_url": config["base_url"], "timeout_seconds": config["timeout"], "auto_retries": 0,
              "max_output_tokens": None, "client_peak_inflight": 0, "upstream_peak_inflight": None,
              "measurement_boundary": "client HTTP requests, not upstream admission or execution",
              "ttft_definition": "first nonempty response.output_text.delta; null for JSON and tool-only responses",
              "candy_expected_answer": 21, "expected_answer_sent_to_model": False,
              "script_sha256": hashlib.sha256(Path(__file__).read_bytes()).hexdigest(), "model_runs": []}

    def update():
        report["summary"] = summarize(rows)
        report["by_model"] = {model: summarize([row for row in rows if row["model"] == model]) for model in config["models"]}
        save_json(out / "summary.json", report)

    def record(row):
        with (out / "results.jsonl").open("a", encoding="utf-8") as stream:
            stream.write(json.dumps(row, ensure_ascii=False) + "\n")
        rows.append(row)
        update()
        print(json.dumps({"model": row["model"], "case": row["case"], "http_status": row["http_status"],
                          "outcome": row["outcome"], "duration_ms": row["duration_ms"],
                          "content": row.get("content_assessment"), "candy_answer": row.get("candy_answer")},
                         ensure_ascii=True), flush=True)

    update()
    for model in config["models"]:
        model_started = time.monotonic()
        if config["phase"] == "functional":
            info = functional(config, key, model, record)
            seen = {row["case"] for row in rows if row["model"] == model}
            info["skipped_dependent_cases"] = sorted(set(info["expected_cases"]) - seen)
        elif config["phase"] == "compact":
            info = compact_check(config, key, model, record)
        elif config["phase"] == "candy":
            inflight = Inflight()
            for index in range(config["candy_runs"]):
                row = request_once(config, key, candy_body(model, config["effort"]),
                                   "candy_" + str(index + 1), inflight)
                record(assess(row, "candy"))
            info = {"peak_inflight": inflight.peak}
        else:
            count, concurrency = config["load_requests"], config["concurrency"]
            inflight = Inflight()
            # Separate synchronized waves keep every requested burst real, even
            # when request count is larger than the thread-pool size.
            waves = []
            for start in range(0, count, concurrency):
                size = min(concurrency, count - start)
                barrier = threading.Barrier(size + 1)
                wave_started = time.monotonic()
                with ThreadPoolExecutor(max_workers=size) as executor:
                    futures = {}
                    for index in range(start, start + size):
                        is_candy = index < config["load_candy"]
                        expected = "OK_" + str(index)
                        body = candy_body(model, config["effort"]) if is_candy else payload(
                            model, "Reply exactly " + expected + ".", config["effort"])
                        future = executor.submit(request_once, config, key, body,
                            ("load_candy_" if is_candy else "load_short_") + str(index), inflight, barrier)
                        futures[future] = ("candy" if is_candy else "exact", expected)
                    barrier.wait(timeout=60)
                    for future in as_completed(futures):
                        kind, expected = futures[future]
                        record(assess(future.result(), kind, expected))
                waves.append({"requests": size, "duration_ms": round((time.monotonic() - wave_started) * 1000, 3)})
            info = {"peak_inflight": inflight.peak, "requested_concurrency": concurrency,
                    "load_requests": count, "load_candy": config["load_candy"], "waves": waves}
        info.update(model=model, duration_ms=round((time.monotonic() - model_started) * 1000, 3))
        report["model_runs"].append(info)
        report["client_peak_inflight"] = max(report["client_peak_inflight"], info["peak_inflight"])
        update()
    report.update(status="completed", finished_at=utc_now())
    update()
    return report


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base-url", required=True)
    parser.add_argument("--key-file", required=True, type=Path)
    parser.add_argument("--output-dir", required=True, type=Path)
    parser.add_argument("--models", nargs="+", default=["gpt-5.6-sol", "gpt-6-astra"])
    parser.add_argument("--phase", choices=["functional", "candy", "load", "compact"], required=True)
    parser.add_argument("--effort", choices=["none", "minimal", "low", "medium", "high", "xhigh"], default="medium")
    parser.add_argument("--timeout", type=float, default=900)
    parser.add_argument("--candy-runs", type=int, default=3)
    parser.add_argument("--concurrency", type=int, default=80)
    parser.add_argument("--load-requests", type=int, default=80)
    parser.add_argument("--load-candy", type=int, default=16)
    args = parser.parse_args()
    config = vars(args).copy()
    config.pop("key_file")
    config["output_dir"] = args.output_dir.resolve()
    try:
        endpoint(args.base_url)
        if not 1 <= args.concurrency <= 256 or not 1 <= args.load_requests <= 1024:
            raise ValueError("concurrency must be 1..256 and load-requests 1..1024")
        if not 0 <= args.load_candy <= args.load_requests or not 1 <= args.candy_runs <= 100 or not 1 <= args.timeout <= 3600:
            raise ValueError("invalid request count or timeout")
        if len(set(args.models)) != len(args.models):
            raise ValueError("duplicate models")
        key = args.key_file.read_text(encoding="utf-8-sig").strip()
        if not key or "\n" in key or "\r" in key:
            raise ValueError("key file must contain one nonempty line")
        report = run(config, key)
    except (ValueError, OSError) as error:
        parser.exit(2, "Benchmark configuration/local I/O failed (" + type(error).__name__ + ").\n")
    print(json.dumps({"report": str(config["output_dir"] / "summary.json"),
                      "client_peak_inflight": report["client_peak_inflight"], "summary": report["summary"]},
                     ensure_ascii=True, indent=2))
    return 0 if report["summary"]["request_failures"] == 0 else 1


if __name__ == "__main__":
    raise SystemExit(main())
