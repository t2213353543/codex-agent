#!/usr/bin/env python3
"""Offline server-side comparison; raw account capture never leaves run-dir.

Only sanitized types, lengths, hashes, known protocol values and operation paths
are emitted. No production or model requests are made by this program.
"""
from __future__ import annotations

import argparse
import base64
from collections import Counter, defaultdict
import hashlib
import ipaddress
import json
from pathlib import Path
import re
from urllib.parse import parse_qsl, urlsplit

ROUTES = ("official_app_server", "official_codex_cli")
KNOWN_KEYS = set("model input instructions tools type role content text output id status response previous_response_id generate stream store include reasoning effort summary tool_choice parallel_tool_calls prompt_cache_key prompt_cache_options service_tier client_metadata metadata name description parameters properties required additionalProperties arguments call_id encrypted_content encrypted_function_args format verbosity max_output_tokens temperature top_p user item output_index content_index delta sequence_number usage input_tokens output_tokens total_tokens input_tokens_details output_tokens_details cached_tokens reasoning_tokens function namespace defer_loading session_id thread_id turn_id traceparent tracestate conversation_id originator version features model_reasoning_effort message recipient phase timestamp created_at completed_at created time limit query results url title snippet action search execution items tool_use_id result error code detail request_id default_reasoning_level supported_reasoning_levels slug display_name availability priority upgrade_info is_default supports_reasoning_summaries supports_verbosity default_verbosity supports_parallel_tool_calls supports_image_detail supports_search_tool supports_compaction context_window effective_context_window_percent truncation_policy mode web_search search_context_size user_location completion_tokens prompt_tokens body tool_calls enabled".split())
SAFE_VALUES = set("auto none required low medium high xhigh max minimal disabled concise detailed summary final analysis commentary assistant user system developer message input_text output_text function function_call function_call_output custom custom_tool_call custom_tool_call_output tool_search tool_search_call tool_search_output web_search web_search_call reasoning compaction compaction_summary response.create response.created response.completed response.in_progress response.output_item.done response.output_text.delta response.output_text.done json_object json_schema text code completed in_progress failed cancelled live read-only never default premium priority flex reasoning.encrypted_content".split())
SAFE_SEGMENTS = set("backend-api codex models responses compact plugins featured ps installed suggested wham settings user repos openai git ref heads main zipball legacy.zip access health".split())
DYNAMIC_KEYS = set("id call_id previous_response_id prompt_cache_key session_id thread_id turn_id conversation_id request_id created_at completed_at timestamp traceparent tracestate".split())
TIME_KEYS = set("created_at completed_at timestamp time".split())
SAFE_QUERY_KEYS = {"client_version", "platform", "limit", "scope", "include_models"}
KNOWN_KEYS.update("context additional_tools configuration_update internal_chat_message_metadata_passthrough root_turn_id x-codex-ws-stream-request-start-ms x-codex-window-id x-codex-turn-metadata x-codex-installation-id ws_request_header_x_openai_internal_codex_responses_lite turn_metadata client_version".split())
SAFE_VALUES.update(("additional_tools", "configuration_update", "disabled", "enabled"))
DYNAMIC_KEYS.update(("root_turn_id", "x-codex-window-id", "x-codex-installation-id", "x-codex-turn-metadata"))
TIME_KEYS.add("x-codex-ws-stream-request-start-ms")
KNOWN_KEYS.update("installation_id agent_name window_id window_number context_window_id request_kind thread_source sandbox sandbox_mode auto_review_enabled node_repl_auto_review_required node_repl_disabled turn_started_at_unix_ms".split())
DYNAMIC_KEYS.update(("installation_id", "window_id", "context_window_id"))
TIME_KEYS.add("turn_started_at_unix_ms")
SAFE_VALUES.update(("prewarm", "turn"))
MISSING = object()


def digest(raw):
    return hashlib.sha256(raw).hexdigest()


def canonical(value):
    return json.dumps(value, sort_keys=True, ensure_ascii=False, separators=(",", ":"), allow_nan=False).encode()


def parse(raw):
    def pairs(items):
        out = {}
        for key, value in items:
            if key in out:
                raise ValueError("duplicate_json_key")
            out[key] = value
        return out
    return json.loads(raw, object_pairs_hook=pairs,
                      parse_constant=lambda _: (_ for _ in ()).throw(ValueError("nonfinite_json")))


def safe_key(key):
    return key if key in KNOWN_KEYS else "<key-sha256:" + digest(key.encode())[:16] + ">"


def desc(value, key=""):
    if value is MISSING:
        return {"type": "missing"}
    result = {"type": type(value).__name__, "canonical_sha256": digest(canonical(value))}
    if isinstance(value, (str, list, dict)):
        result["length"] = len(value)
    if value is None or isinstance(value, bool):
        result["value"] = value
    elif isinstance(value, str) and (value in SAFE_VALUES or (key == "model" and re.fullmatch(r"gpt-[a-z0-9.\-]{1,48}", value))):
        result["value"] = value
    return result


def artifact(root, record):
    target = (root / record["file"]).resolve()
    if root.resolve() not in target.parents or target.stat().st_size > 256 << 20:
        raise ValueError("invalid_capture_artifact")
    raw = target.read_bytes()
    if "sha256" in record and digest(raw) != record["sha256"]:
        raise ValueError("capture_artifact_hash_mismatch")
    return raw


def body_json(root, record):
    try:
        return parse(artifact(root, record)), None
    except (ValueError, UnicodeError):
        return None, "not_unambiguous_json"


def safe_url(raw):
    url = urlsplit(raw)
    segments = [value if value in SAFE_SEGMENTS else "<segment-sha256:" + digest(value.encode())[:16] + ">"
                for value in url.path.split("/") if value]
    query = [{"name": key if key in SAFE_QUERY_KEYS else "<key-sha256:" + digest(key.encode())[:16] + ">",
              "value": desc(value)} for key, value in parse_qsl(url.query, keep_blank_values=True)]
    return {"host": url.hostname, "path": "/" + "/".join(segments), "path_sha256": digest(url.path.encode()),
            "query": query, "url_sha256": digest(raw.encode())}


def category(raw):
    url = urlsplit(raw)
    if "plugins" in url.path or url.hostname in ("api.github.com", "codeload.github.com"):
        return "plugin_discovery_or_download"
    if url.path.endswith("/models"):
        return "model_catalog"
    if "/wham/" in url.path:
        return "account_settings"
    if "/responses" in url.path:
        return "model_inference_or_upgrade"
    return "other_observed_operation"


def semantic_diff(left, right, path="$", key="", output=None, dynamic=None):
    if output is None:
        output, dynamic = [], []
    if left is not MISSING and right is not MISSING and canonical(left) == canonical(right):
        return output, dynamic
    kind = "time_candidate" if key in TIME_KEYS else "dynamic_identity_or_state_candidate" if key in DYNAMIC_KEYS else None
    if kind:
        dynamic.append({"path": path, "classification": kind, "left": desc(left, key), "right": desc(right, key),
                        "excluded_from_stable_diff": True, "discarded_from_evidence": False,
                        "rationale": "Value varies by call/session; remains recorded, not evidence of semantic irrelevance."})
        return output, dynamic
    if isinstance(left, dict) and isinstance(right, dict):
        for child in sorted(set(left) | set(right)):
            semantic_diff(left.get(child, MISSING), right.get(child, MISSING), path + "." + safe_key(child), child, output, dynamic)
    elif isinstance(left, list) and isinstance(right, list):
        if len(left) != len(right):
            output.append({"path": path + ".length", "left": len(left), "right": len(right)})
        for index in range(max(len(left), len(right))):
            semantic_diff(left[index] if index < len(left) else MISSING, right[index] if index < len(right) else MISSING,
                          path + "[" + str(index) + "]", key, output, dynamic)
    else:
        output.append({"path": path, "left": desc(left, key), "right": desc(right, key)})
    return output, dynamic


def field_tree(value, path="$", key=""):
    """Expose structure and safe protocol values, never arbitrary text."""
    rows = [{"path": path, **desc(value, key)}]
    if isinstance(value, dict):
        for child, item in value.items():
            rows.extend(field_tree(item, path + "." + safe_key(child), child))
    elif isinstance(value, list):
        for index, item in enumerate(value):
            rows.extend(field_tree(item, path + "[" + str(index) + "]", key))
    return rows


def headers_diff(left, right):
    def indexed(headers):
        result = defaultdict(list)
        for value in headers:
            result[value["name"]].append({"length": value["length"], "sha256": value["sha256"]})
        return result
    a, b = indexed(left), indexed(right)
    return [{"name": name, "left": a.get(name, []), "right": b.get(name, []),
             "classification": "dynamic_time_or_identity" if name in ("date", "authorization", "chatgpt-account-id", "session-id", "conversation-id", "x-request-id", "traceparent", "sec-websocket-key", "sec-websocket-accept") else "request_or_response_header"}
            for name in sorted(set(a) | set(b)) if a.get(name) != b.get(name)]


def summarize_json(value):
    if not isinstance(value, dict):
        return desc(value)
    return {safe_key(key): desc(item, key) for key, item in value.items()}


def turn_metadata(raw):
    if raw is None:
        return None, "absent"
    try:
        value = parse(raw)
    except (ValueError, UnicodeError, TypeError):
        return None, "invalid_json"
    return (value, "decoded_object") if isinstance(value, dict) else (None, "not_object")


def compare_turn_metadata(left, right):
    result = {}
    for label, raw_left, raw_right in (
        ("header", left.get("header_turn_metadata"), right.get("header_turn_metadata")),
        ("client_metadata", (left["body"].get("client_metadata") or {}).get("x-codex-turn-metadata"),
         (right["body"].get("client_metadata") or {}).get("x-codex-turn-metadata")),
    ):
        a, left_status = turn_metadata(raw_left)
        b, right_status = turn_metadata(raw_right)
        entry = {"left_status": left_status, "right_status": right_status,
                 "left_field_tree": field_tree(a) if a is not None else None,
                 "right_field_tree": field_tree(b) if b is not None else None}
        if a is not None and b is not None:
            entry["stable_diff"], entry["dynamic_fields"] = semantic_diff(a, b)
        result[label] = entry
    return result


def sse_terminal(raw):
    terminal = []
    for line in raw.splitlines():
        if not line.startswith(b"data:"):
            continue
        try:
            item = parse(line[5:].strip())
        except (ValueError, UnicodeError):
            continue
        if isinstance(item, dict) and item.get("type") in ("response.completed", "response.failed", "response.incomplete", "error"):
            terminal.append(item["type"])
    return terminal


def network_summary(path, proxy_port):
    if not path.exists():
        return {"captured": False}
    raw = path.read_text(errors="replace")
    addresses, unix, partial = Counter(), 0, 0
    for line in raw.splitlines():
        if "connect(" not in line:
            continue
        if "AF_UNIX" in line:
            unix += 1
            continue
        ipv4 = re.search(r'sin_port=htons\((\d+)\), sin_addr=inet_addr\("([^"]+)"\)', line)
        ipv6 = re.search(r'sin6_port=htons\((\d+)\).*inet_pton\(AF_INET6, "([^"]+)"', line)
        match = ipv4 or ipv6
        if match:
            port, address = int(match[1]), match[2]
            addresses[(address, port)] += 1
        else:
            partial += 1
    rows = []
    for (address, port), count in sorted(addresses.items()):
        loopback = ipaddress.ip_address(address).is_loopback
        rows.append({"address": address if loopback else None, "address_sha256": digest(address.encode()),
                     "port": port, "attempt_count": count, "loopback": loopback,
                     "matches_capture_proxy": loopback and port == proxy_port})
    return {"captured": True, "file_sha256": digest(path.read_bytes()), "destinations": rows,
            "unix_socket_connect_count": unix, "unparsed_or_unfinished_connect_lines": partial,
            "non_loopback_connect_attempt_count": sum(x["attempt_count"] for x in rows if not x["loopback"]),
            "all_possible_egress_proven": False,
            "limitations": ["-f connect trace covers descendants spawned under tracer.", "Unconnected UDP, preexisting processes and other I/O mechanisms are not proven covered."]}


class CompletedOutput:
    """Restore Responses Lite output from completed items without inventing data.

The terminal array wins for present fields. Only matching item IDs justify
merging omitted items/fields; explicit null and nested values stay authoritative.
Conflicting indexes, IDs or response IDs make later state expansion unresolved.
"""
    def __init__(self):
        self.items, self.by_id, self.implicit = {}, {}, set()
        self.started_indices, self.started_ids = set(), set()
        self.response_id, self.issue = None, None

    def accept_response(self, response_id):
        if not response_id:
            return
        if self.response_id and response_id != self.response_id:
            self.issue = "conflicting_response_ids"
        self.response_id = response_id

    def observe(self, event):
        self.accept_response(event.get("response_id"))
        item = event.get("item")
        if not isinstance(item, dict):
            self.issue = "invalid_completed_output_item"
            return
        identifier = item.get("id")
        if identifier is not None and not isinstance(identifier, str):
            self.issue = "invalid_output_item_id"
            return
        explicit = "output_index" in event
        index = event.get("output_index") if explicit else self.by_id.get(identifier)
        if index is None and not explicit:
            index = max(self.items, default=-1) + 1
        if type(index) is not int or index < 0:
            self.issue = "invalid_output_index"
            return
        known = self.by_id.get(identifier) if identifier else None
        if known is not None and known != index:
            self.issue = "output_id_index_conflict"
            return
        prior = self.items.get(index)
        if prior:
            old_id = prior.get("id")
            if (old_id and identifier and old_id != identifier) or (explicit and index in self.implicit and not (old_id and old_id == identifier)):
                self.issue = "output_index_identity_conflict"
                return
            for field in ("type", "call_id", "name", "namespace", "role"):
                if field in prior and field in item and prior[field] != item[field]:
                    self.issue = "output_item_identity_conflict"
                    return
        self.items[index] = item
        if identifier:
            self.by_id[identifier] = index
        if explicit:
            self.implicit.discard(index)
        elif prior is None:
            self.implicit.add(index)

    def started(self, event):
        self.accept_response(event.get("response_id"))
        index, item = event.get("output_index"), event.get("item")
        if type(index) is int and index >= 0:
            self.started_indices.add(index)
        if isinstance(item, dict) and isinstance(item.get("id"), str):
            self.started_ids.add(item["id"])

    def complete(self, response):
        self.accept_response(response.get("id"))
        terminal = response.get("output")
        if self.issue:
            return None, self.issue
        if self.started_indices - self.items.keys() or self.started_ids - self.by_id.keys():
            return None, "started_output_missing_done"
        if terminal is not None and not isinstance(terminal, list):
            return None, "invalid_terminal_output"
        terminal = terminal or []
        if not terminal:
            return [self.items[index] for index in sorted(self.items)], ("completed_items_terminal_empty" if self.items else "empty_terminal_output")
        if any(not isinstance(item, dict) for item in terminal):
            return None, "invalid_terminal_output_item"
        if not self.items:
            return terminal, "terminal_output_only"
        merged, terminal_order = {}, []
        for item in terminal:
            identifier = item.get("id")
            index = self.by_id.get(identifier) if isinstance(identifier, str) else None
            if index is None:
                return None, "terminal_output_identity_unreconciled"
            if index in merged:
                return None, "duplicate_terminal_output_id"
            previous = self.items[index]
            for field in ("type", "call_id", "name", "namespace", "role"):
                if field in previous and field in item and previous[field] != item[field]:
                    return None, "terminal_output_identity_conflict"
            merged[index] = dict(previous, **item)
            terminal_order.append(index)
        order = terminal_order if len(merged) == len(self.items) else sorted(self.items)
        return [merged.get(index, self.items[index]) for index in order], "completed_items_merged_with_terminal_overrides"


def route_report(root):
    capture = root / "capture"
    records = [parse(line) for line in (capture / "manifest.jsonl").read_bytes().splitlines()]
    flows, order, inference, messages, completed_states, pending = {}, [], [], [], {}, {}
    errors, response_bodies, outputs, metadata_headers = [], {}, {}, {}
    for record in records:
        flow_id = record.get("flow_id")
        event = record["event"]
        if event == "request":
            private = parse(artifact(capture, record["private_head"]))
            for key, value in private.get("headers", []):
                if base64.b64decode(key).lower() == b"x-codex-turn-metadata":
                    metadata_headers[flow_id] = base64.b64decode(value).decode("utf-8", "replace")
            value, issue = body_json(capture, record["decoded_body"]) if record["decoded_body"]["length"] else (None, None)
            item = {"operation": len(order) + 1, "flow_id": flow_id, "phase": record.get("phase", {}).get("case", "unassigned"),
                    "method": record["method"], "url": safe_url(private["url"]), "category": category(private["url"]),
                    "http_version": record["http_version"], "headers": record["headers"],
                    "body_raw": {key: record["body"][key] for key in ("length", "sha256")},
                    "body_decoded": {key: record["decoded_body"][key] for key in ("length", "sha256")},
                    "decode_status": record["decode_status"], "json_parse_issue": issue,
                    "body_fields": summarize_json(value) if value is not None else None,
                    "response_status": None, "response_headers": [], "response_entity_complete": False,
                    "flow_error_count": 0, "response_terminal_events": []}
            flows[flow_id] = item
            order.append(item)
            if item["category"] == "model_inference_or_upgrade" and value is not None:
                inference.append({"case": item["phase"], "transport": "http", "operation": item["operation"],
                                  "body": value, "effective_body": value, "headers": item["headers"],
                                  "header_turn_metadata": metadata_headers.get(flow_id), "expansion": "full_http_request"})
        elif event == "response_headers" and flow_id in flows:
            flows[flow_id].update(response_status=record["status"], response_headers=record["headers"], response_http_version=record["http_version"])
        elif event == "stream_end" and record["stream"] == "response-entity" and flow_id in flows:
            flows[flow_id].update(response_entity_complete=record["complete"], response_entity_sha256=record["sha256"], response_entity_length=record["length"])
            raw_response = artifact(capture, record)
            flows[flow_id]["response_terminal_events"] = sse_terminal(raw_response)
            if flows[flow_id]["category"] in ("model_catalog", "account_settings"):
                try:
                    response_bodies[flow_id] = parse(raw_response)
                except (ValueError, UnicodeError):
                    pass
        elif event == "flow_error":
            errors.append({"flow_id": flow_id, "phase": record.get("phase", {}).get("case", "unassigned")})
            if flow_id in flows:
                flows[flow_id]["flow_error_count"] += 1
        elif event == "websocket_message":
            value, issue = body_json(capture, record["body"])
            message = {"message": len(messages) + 1, "flow_id": flow_id, "from_client": record["from_client"],
                       "case": record.get("phase", {}).get("case", "unassigned"), "body_sha256": record["body"]["sha256"],
                       "body_length": record["body"]["length"], "json_parse_issue": issue,
                       "type": value.get("type") if isinstance(value, dict) and value.get("type") in SAFE_VALUES else None}
            messages.append(message)
            if not isinstance(value, dict):
                continue
            if record["from_client"] and value.get("type") == "response.create":
                effective = dict(value)
                previous = value.get("previous_response_id")
                status = "standalone_full_ws_input"
                if previous:
                    prior = completed_states.get((flow_id, previous))
                    if prior:
                        effective["input"] = prior["input"] + prior["output"] + value.get("input", [])
                        status = "expanded_exact_previous_response_id_on_same_connection"
                    else:
                        status = "unresolved_previous_response_id"
                inference_item = {"case": message["case"], "transport": "websocket", "message": message["message"],
                                  "body": value, "effective_body": effective, "headers": flows.get(flow_id, {}).get("headers", []),
                                  "header_turn_metadata": metadata_headers.get(flow_id),
                                  "expansion": status, "warmup": value.get("generate") is False}
                inference.append(inference_item)
                pending[flow_id] = inference_item
                outputs[flow_id] = CompletedOutput()
                message["request_fields"] = summarize_json(value)
                message["warmup"] = inference_item["warmup"]
                message["expansion"] = status
            elif not record["from_client"] and value.get("type") == "response.output_item.done":
                if flow_id in outputs:
                    outputs[flow_id].observe(value)
            elif not record["from_client"] and value.get("type") == "response.output_item.added":
                if flow_id in outputs:
                    outputs[flow_id].started(value)
            elif not record["from_client"] and value.get("type") in ("response.created", "response.in_progress", "response.completed"):
                response = value.get("response", {})
                current = pending.get(flow_id)
                if current and isinstance(response, dict) and response.get("id"):
                    rid = response["id"]
                    outputs[flow_id].accept_response(rid)
                    current["response_id_sha256"] = digest(rid.encode())
                    if value.get("type") == "response.completed":
                        output, provenance = outputs[flow_id].complete(response)
                        current["response_output_provenance"] = provenance
                        current["response_completed_item_count"] = len(outputs[flow_id].items)
                        current["response_terminal_output_count"] = len(response.get("output") or []) if isinstance(response.get("output"), (list, type(None))) else None
                        current["response_effective_output_count"] = len(output) if output is not None else None
                        if output is not None and current["expansion"] != "unresolved_previous_response_id":
                            completed_states[(flow_id, rid)] = {"input": current["effective_body"].get("input", []), "output": output}
    group_seen = Counter()
    for item in order:
        key = (item["phase"], item["method"], item["url"]["url_sha256"], item["body_decoded"]["sha256"])
        group_seen[key] += 1
        item["identical_request_occurrence_in_phase"] = group_seen[key]
        item["retry_or_repeated_initialization_candidate"] = group_seen[key] > 1
        item["repeat_causality_proven"] = False
        item["error_classification"] = ("after_complete_entity" if item["response_entity_complete"] else
                                          "after_model_terminal_before_http_entity_end" if item["response_terminal_events"] else
                                          "incomplete_or_unanswered") if item["flow_error_count"] else None
    for error in errors:
        flow = flows.get(error["flow_id"])
        error["classification"] = flow["error_classification"] if flow else "unmatched_flow_error"
        error["operation"] = flow["operation"] if flow else None
    ready = parse((capture / "ready.json").read_bytes())
    public = {"route": root.name, "events": dict(Counter(x["event"] for x in records)), "operations": order,
              "websocket_messages": messages, "flow_errors": errors,
              "observed_connects": [x for x in records if x["event"] in ("connect", "upstream_connect")],
              "network": network_summary(root / "private-net.log", ready["port"]),
              "inventory_counts": dict(Counter(x["category"] for x in order))}
    return public, inference, response_bodies


def normalized(item):
    body = dict(item["effective_body"])
    removed = {}
    if item["transport"] == "websocket":
        for key in ("type", "generate", "previous_response_id"):
            if key in body:
                removed[key] = desc(body.pop(key), key)
    return body, removed


def compare_inference(a, b):
    groups = []
    left, right = defaultdict(list), defaultdict(list)
    for item in a:
        if not item.get("warmup"):
            left[item["case"]].append(item)
    for item in b:
        if not item.get("warmup"):
            right[item["case"]].append(item)
    for case in sorted(set(left) | set(right)):
        for occurrence in range(max(len(left[case]), len(right[case]))):
            l = left[case][occurrence] if occurrence < len(left[case]) else None
            r = right[case][occurrence] if occurrence < len(right[case]) else None
            result = {"case": case, "occurrence": occurrence + 1, "alignment": "phase_case_then_generation_request_occurrence"}
            for name, value in (("left", l), ("right", r)):
                result[name] = {key: item for key, item in value.items() if key not in ("body", "effective_body", "headers", "header_turn_metadata")} if value else None
                if value:
                    result[name]["request_field_tree"] = field_tree(value["body"])
                    result[name]["effective_input_field_tree"] = field_tree(value["effective_body"].get("input"), "$.input", "input")
            if l and r:
                raw_diff, raw_dynamic = semantic_diff(l["body"], r["body"])
                ln, lr = normalized(l)
                rn, rr = normalized(r)
                diff, dynamic = semantic_diff(ln, rn)
                result.update(raw_body_diff=raw_diff, raw_dynamic_fields=raw_dynamic, effective_body_diff=diff,
                              effective_dynamic_fields=dynamic, header_diff=headers_diff(l["headers"], r["headers"]),
                              decoded_turn_metadata=compare_turn_metadata(l, r),
                              normalization={"left_envelope_fields": lr, "right_envelope_fields": rr,
                                             "rationale": "WS response.create envelope separated only after recorded same-connection previous-response expansion.",
                                             "expansion_resolved": all(x["expansion"] != "unresolved_previous_response_id" for x in (l, r))},
                              effective_body_exact_equal=canonical(ln) == canonical(rn))
            groups.append(result)
    return groups


def compare_operations(left, right, response_left, response_right):
    grouped = []
    a, b = defaultdict(list), defaultdict(list)
    for target, operations in ((a, left), (b, right)):
        for item in operations:
            target[(item["phase"], item["method"], item["url"]["host"], item["url"]["path_sha256"])].append(item)
    for key in sorted(set(a) | set(b)):
        for index in range(max(len(a[key]), len(b[key]))):
            l = a[key][index] if index < len(a[key]) else None
            r = b[key][index] if index < len(b[key]) else None
            item = {"case": key[0], "method": key[1], "host": key[2], "path": (l or r)["url"]["path"],
                    "occurrence": index + 1, "left_operation": l["operation"] if l else None,
                    "right_operation": r["operation"] if r else None,
                    "alignment": "phase_method_host_path_then_occurrence",
                    "presence": "both" if l and r else "left_only" if l else "right_only"}
            if l and r:
                item.update(request_header_diff=headers_diff(l["headers"], r["headers"]),
                            response_header_diff=headers_diff(l["response_headers"], r["response_headers"]),
                            left_status=l["response_status"], right_status=r["response_status"],
                            query_equal=l["url"]["query"] == r["url"]["query"],
                            response_entity_sha256_equal=l.get("response_entity_sha256") == r.get("response_entity_sha256"))
                if l["flow_id"] in response_left and r["flow_id"] in response_right:
                    diff, dynamic = semantic_diff(response_left[l["flow_id"]], response_right[r["flow_id"]])
                    item.update(response_json_diff=diff, response_json_dynamic_fields=dynamic)
            grouped.append(item)
    return grouped


def build_report(run_dir, routes=ROUTES):
    root = Path(run_dir).resolve()
    reports, requests, responses = {}, {}, {}
    for route in routes:
        if (root / route / "capture/manifest.jsonl").exists():
            reports[route], requests[route], responses[route] = route_report(root / route)
    summary = parse((root / "summary.json").read_bytes()) if (root / "summary.json").exists() else {}
    categories = sorted({category for report in reports.values() for category in report["inventory_counts"]})
    decisions = []
    for name in categories:
        rationale = {"plugin_discovery_or_download": "Observed official startup/plugin maintenance; client-owned plugin lifecycle may make server replay redundant, but omission is an architectural decision requiring review.",
                     "model_catalog": "Model capability/version negotiation; compare contents and defaults before reuse/caching.",
                     "account_settings": "Account/server configuration discovery; causal effect on inference not established.",
                     "model_inference_or_upgrade": "Required model request or WebSocket handshake; preserve validated semantics, state and response contract.",
                     "other_observed_operation": "Unclassified actual egress; inspect before any replay or omission."}[name]
        decisions.append({"category": name, "counts": {route: report["inventory_counts"].get(name, 0) for route, report in reports.items()},
                          "decision": "review_required_no_automatic_omission", "rationale": rationale})
    return {"schema": "appserver-parity-sanitized-v1", "state_reconstruction": "completed-output-items-v2", "run": root.name,
            "report_program_sha256": digest(Path(__file__).read_bytes()),
            "input_capture_summary_passed": summary.get("passed"), "routes": reports,
            "inference_comparisons": compare_inference(requests.get(routes[0], []), requests.get(routes[1], [])),
            "operation_comparisons": compare_operations(reports.get(routes[0], {}).get("operations", []),
                                                        reports.get(routes[1], {}).get("operations", []),
                                                        responses.get(routes[0], {}), responses.get(routes[1], {})),
            "operation_disposition_review": decisions,
            "limits": ["Offline report; raw evidence remains private on the server.",
                       "Capture TLS and HTTP2 framing are MITM-recreated and do not establish fingerprint parity.",
                       "Array items are aligned positionally; semantic role-equivalence is not inferred.",
                       "Repeated identical requests are candidates, not proven retries.",
                       "Dynamic fields remain explicitly reported; they are not automatically safe to omit.",
                       "State expansion requires exact previous_response_id completion on the same captured connection, including output_item.done and reconciled terminal overrides.",
                       "Connection syscall audit does not prove every possible egress mechanism."]}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--run-dir", required=True)
    parser.add_argument("--output", required=True)
    parser.add_argument("--route", action="append", choices=("official_app_server", "official_codex_cli", "native_app_server", "native_codex_exec"),
                        help="Two ordered routes to compare; defaults to the two gateway routes")
    args = parser.parse_args()
    if args.route and len(args.route) != 2:
        parser.error("select exactly two routes for comparison")
    report = build_report(args.run_dir, tuple(args.route) if args.route else ROUTES)
    output = Path(args.output)
    output.parent.mkdir(parents=True, exist_ok=True)
    output.write_text(json.dumps(report, ensure_ascii=False, indent=2), encoding="utf-8")
    print(json.dumps({"report": output.name, "schema": report["schema"], "routes": list(report["routes"]),
                      "comparisons": len(report["inference_comparisons"]), "raw_data_exported": False}))


if __name__ == "__main__":
    main()
