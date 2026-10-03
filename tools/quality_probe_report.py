"""Offline candy scoring and paired gateway/direct comparison; never sends requests.

python tools/quality_probe_report.py RUN_DIRECTORY [...] --output report.json
Optional --manifest maps request_id to {account_label, arm: A|B, pair_id,
experiment_id, sequence}. Arms and account identity must come from routing evidence.
Only completed final assistant messages are scored. A wrong answer is an observed
task failure, not evidence of any particular server cluster or account flag.
"""
from __future__ import annotations

import argparse
from collections import Counter, defaultdict
from datetime import datetime
import hashlib
from itertools import combinations
import json
import math
from pathlib import Path
import re
import unicodedata


SCHEMA = "codex-quality-probe-v1"
EXPECTED = 21
SCORED = {"correct", "wrong_answer"}
TERMINALS = {"response.completed", "response.failed", "response.incomplete", "error"}
SECRET_PATTERNS = [
    (re.compile(r"(?i)\bBearer\s+[A-Za-z0-9._~+/=-]+"), "Bearer [REDACTED]"),
    (re.compile(r"\beyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\b"), "[REDACTED_JWT]"),
    (re.compile(r"\bsk-[A-Za-z0-9_-]{12,}\b"), "[REDACTED_KEY]"),
    (re.compile(r'(?i)(["\']?(?:access_token|refresh_token|api_key|authorization|cookie)["\']?\s*[:=]\s*)[^\s,;]+'), r"\1[REDACTED]"),
    (re.compile(r"[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}"), "[REDACTED_EMAIL]"),
]


def redact(value):
    """Defense in depth for the allowlisted report; never copy response objects."""
    if isinstance(value, str):
        for pattern, replacement in SECRET_PATTERNS:
            value = pattern.sub(replacement, value)
    elif isinstance(value, list):
        return [redact(item) for item in value]
    elif isinstance(value, dict):
        return {key: redact(item) for key, item in value.items()}
    return value


def digest(value):
    return hashlib.sha256(json.dumps(value, ensure_ascii=False, sort_keys=True,
                                     separators=(",", ":")).encode("utf-8")).hexdigest()


def is_live_record(row):
    return row.get("live_model") is True and isinstance(row.get("account_alias"), str) and bool(row.get("arm"))


def record_id(row):
    return row.get("request_id") or "local_record_" + digest({key: row.get(key) for key in
        ("account_alias", "arm", "round", "sequence", "started_at", "model", "effort", "request_sha256")})[:24]


def observed_response(row):
    if isinstance(row.get("response"), dict):
        return row["response"]
    if is_live_record(row):
        return {key: row["response_" + key] for key in
                ("model", "status", "reasoning", "service_tier", "usage") if "response_" + key in row}
    return {}


def visible_rationale_signals(text, answer):
    """Describe explicit final prose only; this is not a diagnosis of capability."""
    signals = []
    if re.search(r"(?:手感|触感).{0,30}(?:控制|选择|分辨)|(?:主动|有意识).{0,15}(?:形状|分别|取糖)", text):
        signals.append("mentions_shape_selection")
    if answer == 29 and re.search(r"(?:最多|最大|不满足|不能形成|避免)", text) and "28" in text:
        signals.append("29_matches_blind_draw_bound_28_plus_1")
    if answer == 28 and "28" in text:
        signals.append("28_equals_largest_blind_failing_set_not_guarantee")
    return signals


def message_text(item):
    if not isinstance(item, dict) or item.get("type") != "message" or item.get("role", "assistant") != "assistant":
        return ""
    content = item.get("content")
    if not isinstance(content, list):
        return ""
    return "".join(part["text"] for part in content if isinstance(part, dict)
                   and part.get("type") == "output_text" and isinstance(part.get("text"), str))


def select_final(items):
    messages = [(item, message_text(item)) for item in items if isinstance(item, dict)]
    messages = [(item, text) for item, text in messages if text]
    final = [(item, text) for item, text in messages if item.get("phase") == "final_answer"]
    if final:
        return final[-1][1]
    messages = [(item, text) for item, text in messages if item.get("phase") != "commentary"]
    return messages[-1][1] if messages else ""


def extract_final(row, events=()):
    """Reconstruct text by item/content indexes, without scoring reasoning items.

    A terminal response may omit output or contain only reasoning, so this also
    checks output_item.done, text.done, content_part.done and finally deltas.
    Sources that disagree are retained as an ambiguity, never silently preferred.
    """
    response = row.get("response") or {}
    if not isinstance(response, dict):
        response = {}
    terminal_events = [event for event in events if event.get("type") in TERMINALS]
    if not response and terminal_events:
        response = terminal_events[-1].get("response") or {}
    event_items, item_ids, texts, deltas = {}, {}, {}, defaultdict(list)
    for event in events:
        kind, item = event.get("type"), event.get("item")
        index = event.get("output_index")
        if not isinstance(index, int) or isinstance(index, bool):
            index = item_ids.get(event.get("item_id"))
        if kind in {"response.output_item.added", "response.output_item.done"} and isinstance(item, dict):
            if index is None:
                index = item_ids.get(item.get("id"), max(event_items, default=-1) + 1)
            if kind.endswith(".done") or index not in event_items:
                event_items[index] = dict(item)
            if item.get("id"):
                item_ids[item["id"]] = index
        if index is None:
            continue
        key = (index, event.get("content_index", 0))
        if kind == "response.output_text.done" and isinstance(event.get("text"), str):
            texts[key] = event["text"]
        elif kind == "response.content_part.done" and isinstance(event.get("part"), dict):
            part = event["part"]
            if part.get("type") == "output_text" and isinstance(part.get("text"), str):
                texts[key] = part["text"]
        elif kind == "response.output_text.delta" and isinstance(event.get("delta"), str):
            deltas[key].append(event["delta"])
    for key, parts in deltas.items():
        texts.setdefault(key, "".join(parts))
    for index, _ in texts:
        event_items.setdefault(index, {"type": "message", "role": "assistant"})
    for index, item in event_items.items():
        if not message_text(item) and item.get("type") == "message":
            item["content"] = [{"type": "output_text", "text": text}
                               for (output_index, _), text in sorted(texts.items()) if output_index == index]
    candidates = []
    for source, output in (("terminal", response.get("output") or []),
                           ("sse_items", [event_items[index] for index in sorted(event_items)]),
                           ("effective_response", (row.get("effective_response") or {}).get("output") or [])):
        text = select_final(output)
        if text:
            candidates.append((source, text))
    if not candidates and isinstance(row.get("output_text"), str) and row["output_text"]:
        candidates.append(("recorded_output_text", row["output_text"]))
    if not candidates:
        return "", "missing_final_text", False
    distinct = {text.strip() for _, text in candidates}
    return candidates[0][1], candidates[0][0], len(distinct) > 1


CONDITIONAL_ANSWER = re.compile(
    r"(?:如果|若|假如|或|否则|取决于|可能是|[?？]|\b(?:or|if|unless|depending|maybe)\b|\d\s*[/／]\s*\d)", re.I)


def terminal_boxed_answer(text):
    """Recognize one whole terminal math block, not arbitrary LaTeX in prose.

    Only an integer and an optional candy unit are supported inside the box. A
    small integer-addition prefix is formatting, never evaluated: the box is the
    stated answer even when the arithmetic is wrong. Other expressions,
    alternatives and multiple boxes require review.
    """
    boxes = list(re.finditer(r"\\boxed\b", text))
    if len(boxes) > 1:
        return None, "multiple_boxed_answers"
    if not boxes:
        return None, None
    integer = (r"\\boxed\s*\{\s*(?P<boxed_number>[0-9]{1,9})\s*"
               r"(?:\\(?:text|mathrm)\s*\{\s*(?:个|颗|粒|枚|candies)\s*\}|个|颗|粒|枚|candies)?\s*\}")
    addition = r"(?:(?P<boxed_sum>[0-9]{1,9}(?:[ \t]*\+[ \t]*[0-9]{1,9}){1,3})[ \t]*=[ \t]*)?"
    unit_suffix = r"(?:\s*(?:个|颗|粒|枚)(?:糖果|糖)?[。.!！]?)?"
    # A label can share the line, but cannot absorb a prior conditional paragraph.
    label = (r"(?:(?:最终答案|最终结论|final[ \t]+answer|(?:所以|因此)?(?:答案|结论)|"
             r"(?:所以|因此)?(?:最少|至少)(?:需要|取出|摸出)?)[ \t]*(?:是|为|is)?[ \t]*[:：=]?[ \t]*)?")
    wrappers = ((r"\\\[", r"\\\]"), (r"\\\(", r"\\\)"),
                (r"\$\$", r"\$\$"), (r"\$", r"\$"), ("", ""))
    for left, right in wrappers:
        pattern = r"^[ \t]*" + label + left + r"\s*" + addition + integer + r"\s*" + right + r"[。.!！]?" + unit_suffix + r"\s*\Z"
        match = re.search(pattern, text, re.I | re.M)
        if not match:
            continue
        preceding = text[:match.start()].rstrip()
        previous_line = preceding.rsplit("\n", 1)[-1].strip()
        if not left and previous_line in {r"\[", r"\(", "$", "$$"}:
            return None, "malformed_final_box"
        # The final lead-in may occupy a separate paragraph above a display block.
        lead_in = re.split(r"\n\s*\n", preceding)[-1]
        if re.search(r"(?:因此|所以)[ \t]*(?:答案|结论)[ \t]*(?:是|为)?[ \t]*[:：]?[ \t]*$", lead_in):
            # A lower-bound proof such as “20颗或更少时不能保证，因此答案是:”
            # is not an alternative final answer. Exempt only this exact count
            # comparison; 如果/若/否则 and other 或 alternatives still reject.
            lead_in = re.sub(r"([0-9]{1,9}[ \t]*(?:个|颗|粒|枚))[ \t]*或[ \t]*更少", r"\1及更少", lead_in)
        if CONDITIONAL_ANSWER.search(lead_in):
            return None, "conditional_final_answer"
        return int(match.group("boxed_number")), None
    # A middle proof box can precede an ordinary explicit final line. Otherwise
    # an unsupported last box must not be silently overridden by earlier prose.
    following = text[boxes[0].end():]
    if re.search(r"^[ \t]*(?:最终答案|最终结论|final[ \t]+answer)[ \t]*(?:是|为|is)?[ \t]*[:：=]?",
                 following, re.I | re.M):
        return None, None
    return None, "no_unambiguous_final_answer"


def parse_answer(text):
    normalized = unicodedata.normalize("NFKC", text)
    boxed, boxed_error = terminal_boxed_answer(normalized)
    if boxed_error:
        return None, boxed_error
    cleaned = re.sub(r"[*`$\\{}]", "", normalized).strip()
    # Chinese numerals are normalized only in answer-like positions below.
    number = r"(?P<number>\d+|[零〇一二两三四五六七八九十]+)"
    final_label = r"(?:最终答案|最终结论|final\s+answer)"
    explicit = re.compile(final_label + r"\s*(?:是|为|is)?\s*[:：=]?\s*" + number + r"(?![\d.])", re.I)
    matches = list(explicit.finditer(cleaned))
    if boxed is not None:
        matched_labels = {match.start() for match in matches}
        for label in re.finditer(final_label, cleaned, re.I):
            if label.start() in matched_labels:
                continue
            rest = cleaned[label.end():].split("\n", 1)[0]
            rest = re.sub(r"^\s*(?:是|为|is)?\s*[:：=]?\s*", "", rest, flags=re.I).strip(" \t[(")
            # A heading or the already-validated terminal box is harmless. An
            # unsupported explicit value (e.g. a decimal) must remain ambiguous.
            if rest and not rest.startswith("boxed"):
                return None, "no_unambiguous_final_answer"
    if not matches and boxed is None:
        last = next((line.strip() for line in reversed(cleaned.splitlines()) if line.strip()), "")
        last_pattern = re.compile(r"^(?:(?:答案|结论|最少(?:需要|取出|摸出)?|至少(?:需要|取出|摸出)?)\s*(?:是|为)?\s*[:：]?\s*)?"
                                  + number + r"\s*(?:个|颗|candies)?[。.!！]?\s*$", re.I)
        match = last_pattern.fullmatch(last)
        matches = [match] if match else []
        if not matches:
            # Models often conclude inline: “均无法……因此答案是 21 个”。
            # Treat only the final non-empty line as an explicit conclusion;
            # conditional/multiple-choice conclusions are still rejected below.
            concluding = re.compile(r"(?:答案|结论)\s*(?:是|为|is)?\s*[:：=]?\s*"
                                    + number + r"\s*(?:个|颗|candies)?"
                                    r"(?:\s*[\(（][^()（）]{0,160}[\)）])?[。.!！]?\s*$", re.I)
            match = concluding.search(last)
            matches = [match] if match else []
    if not matches and boxed is None:
        return None, "no_unambiguous_final_answer"
    values = {boxed} if boxed is not None else set()
    digits = {char: index for index, char in enumerate("零一二三四五六七八九")}
    digits.update({"〇": 0, "两": 2})
    for match in matches:
        if boxed is not None:
            line_start = cleaned.rfind("\n", 0, match.start()) + 1
            line_end = cleaned.find("\n", match.end())
            line = cleaned[line_start:line_end if line_end != -1 else len(cleaned)]
            if CONDITIONAL_ANSWER.search(line):
                return None, "conditional_final_answer"
        value = match.group("number")
        if value.isdigit():
            values.add(int(value))
        elif value.count("十") == 1:
            tens, units = value.split("十")
            if len(tens) <= 1 and len(units) <= 1:
                values.add(digits.get(tens, 1) * 10 + digits.get(units, 0))
            else:
                return None, "unsupported_numeral"
        elif len(value) == 1:
            values.add(digits[value])
        else:
            return None, "unsupported_numeral"
    if len(values) != 1:
        return None, "conflicting_final_answers"
    # Do not turn a conditional/multiple-choice conclusion into a single answer.
    last_line = next((line for line in reversed(cleaned.splitlines()) if line.strip()), "")
    if CONDITIONAL_ANSWER.search(last_line):
        return None, "conditional_final_answer"
    method = "terminal_boxed" if boxed is not None else "explicit_final" if explicit.search(cleaned) else "last_line"
    return values.pop(), method


def protocol_category(row, events=()):
    failure = row.get("failure_category")
    if failure in {"local_cooldown", "cooldown", "account_cooldown"} or row.get("local_cooldown") is True:
        return "local_cooldown"
    code = row.get("http_status")
    if code in (401, 403):
        return "auth"
    if code == 429:
        return "rate_limit"
    if isinstance(code, int) and code >= 300:
        return "http_error"
    terminal = row.get("terminal")
    terminals = [event for event in events if event.get("type") in TERMINALS]
    response = observed_response(row)
    if terminals:
        terminal = terminals[-1].get("type")
        response = terminals[-1].get("response") or response
    error = response.get("error") or row.get("error") or {}
    if terminals:
        error = terminals[-1].get("error") or error
    if isinstance(error, dict):
        error_code = str(error.get("code", ""))
        if error_code in {"token_revoked", "invalid_api_key", "authentication_error", "invalid_token"}:
            return "auth"
        if error_code in {"rate_limit_exceeded", "rate_limit_error", "insufficient_quota"}:
            return "rate_limit"
    if error:
        return "upstream_error"
    if failure in {"transport_error", "timeout", "network"}:
        return "network"
    if terminal in {"response.incomplete", "json_response.incomplete"} or response.get("status") == "incomplete":
        return "incomplete"
    if terminal in {"response.failed", "error", "json_response.failed"} or response.get("status") == "failed":
        return "upstream_error"
    if is_live_record(row):
        counts = row.get("event_counts") or {}
        # Flattened live artifacts omit the terminal object. Require all three
        # independent recorded checks; successful HTTP/answer text alone is not
        # a completed response. Conflicting terminal counts stay protocol errors.
        completed_count = counts.get("response.completed", 0)
        if (row.get("protocol_success") is True and response.get("status") == "completed"
                and isinstance(completed_count, int) and not isinstance(completed_count, bool)
                and completed_count >= 1 and not failure
                and not any(counts.get(kind) for kind in ("error", "response.failed", "response.incomplete"))):
            return "completed"
        if counts.get("response.incomplete"):
            return "incomplete"
        if counts.get("response.failed") or counts.get("error"):
            return "upstream_error"
        return "protocol_error"
    completed = response.get("status") == "completed" and terminal in {"response.completed", "json_response.completed", None}
    if row.get("stream") is True and terminal is None:
        completed = False
    if not completed or failure or row.get("protocol_success") is False:
        return "protocol_error"
    return "completed"


def score_row(row, events=(), request=None, metadata=None):
    metadata = metadata or {}
    request = request or {}
    text, source, conflict = extract_final(row, events)
    answer, method = parse_answer(text)
    category = protocol_category(row, events)
    if category == "completed":
        category = "ambiguous" if conflict or answer is None else "correct" if answer == EXPECTED else "wrong_answer"
    if conflict:
        answer, method = None, "conflicting_output_sources"
    account = metadata.get("account_label") or row.get("account_label") or row.get("account_id") or row.get("account_alias")
    response = observed_response(row)
    requested_model = request.get("model") or row.get("model")
    effort = (request.get("reasoning") or {}).get("effort") or row.get("effort")
    response_model = row.get("response_model") or response.get("model")
    response_effort = (response.get("reasoning") or {}).get("effort")
    usage = row.get("usage") or response.get("usage") or {}
    usage_summary = {key: usage.get(key) for key in ("input_tokens", "output_tokens", "total_tokens")}
    usage_summary["reasoning_tokens"] = (usage.get("output_tokens_details") or {}).get("reasoning_tokens")
    usage_summary["cached_input_tokens"] = (usage.get("input_tokens_details") or {}).get("cached_tokens")
    result = {key: metadata.get(key, row.get(key)) for key in ("case", "arm", "pair_id", "experiment_id", "sequence", "round", "started_at", "duration_ms", "controlled_field")}
    result["request_id"] = record_id(row)
    result["record_id_source"] = "request_id" if row.get("request_id") else "local_metadata_digest_not_wire_id"
    result.update(account_label="account_" + digest(str(account))[:16] if account else None,
                  model=requested_model, response_model=response_model, effort=effort,
                  response_effort=response_effort, category=category, answer=answer,
                  expected_answer=EXPECTED, extraction=method, output_source=source,
                  raw_final=text, rationale=text, rationale_scope="visible_final_assistant_output_only",
                  rationale_signals=visible_rationale_signals(text, answer),
                  http_status=row.get("http_status"), terminal=row.get("terminal"),
                  attempts=row.get("attempts"), usage=usage_summary,
                  response_service_tier=response.get("service_tier"),
                  completion_evidence="live_protocol_status_and_event_count" if is_live_record(row) else "response_and_terminal",
                  model_mismatch=bool(response_model and requested_model != response_model),
                  effort_mismatch=bool(response_effort and effort != response_effort))
    if is_live_record(row):
        result["account_alias"] = row["account_alias"] if re.fullmatch(r"Q\d+", row["account_alias"]) else None
        result["response_status"] = row.get("response_status")
        result["response_completed_events"] = (row.get("event_counts") or {}).get("response.completed", 0)
        result["request_sha256"] = row.get("request_sha256")
        result["predicted_upstream_body_sha256"] = row.get("predicted_upstream_body_sha256")
        result["credential_epoch"] = digest(row["credential_sha256"]) if row.get("credential_sha256") else None
        result["gateway_attempt_count"] = row.get("gateway_attempt_count")
        result["gateway_attempt_evidence"] = row.get("gateway_attempt_evidence") or "unverified_record_correlation"
    if "input" in request:
        result["prompt_sha256"] = digest({"input": request.get("input"), "instructions": request.get("instructions")})
        # These fields affect routing/wire behavior, not the semantic task. All
        # remaining fields must match; no hidden effort/tool/output-limit change.
        settings = {key: value for key, value in request.items() if key not in
                    {"input", "instructions", "stream", "store", "prompt_cache_key"}}
        result["settings_sha256"] = digest(settings)
        result["request_sha256"] = digest(request)
    else:
        result["prompt_sha256"] = metadata.get("prompt_sha256") or row.get("prompt_sha256")
        result["settings_sha256"] = metadata.get("settings_sha256") or row.get("settings_sha256")
    return redact(result)


def wilson(successes, total):
    if not total:
        return None
    z = 1.959963984540054
    proportion, denominator = successes / total, 1 + z * z / total
    centre = (proportion + z * z / (2 * total)) / denominator
    width = z * math.sqrt(proportion * (1 - proportion) / total + z * z / (4 * total * total)) / denominator
    return [max(0, centre - width), min(1, centre + width)]


def count_summary(rows):
    counts = Counter(row["category"] for row in rows)
    scorable = sum(counts[category] for category in SCORED)
    return {"requests": len(rows), "categories": dict(counts), "scorable_completed": scorable,
            "correct_among_scorable": counts["correct"] / scorable if scorable else None,
            "correct_all_attempts": counts["correct"] / len(rows) if rows else None,
            "correct_wilson95_descriptive": wilson(counts["correct"], scorable)}


def paired_summary(rows):
    groups = defaultdict(list)
    for row in rows:
        if row.get("pair_id"):
            groups[(row.get("experiment_id"), row["pair_id"])].append(row)
    pairs, excluded = [], []
    required = ("account_label", "model", "effort", "prompt_sha256")
    for (experiment, pair_id), group in sorted(groups.items(), key=lambda item: str(item[0])):
        reason = None
        if len(group) != 2 or Counter(row.get("arm") for row in group) != {"A": 1, "B": 1}:
            reason = "requires_exactly_one_A_and_one_B"
        elif any(not row.get(key) for row in group for key in required):
            reason = "missing_comparability_metadata"
        elif any(group[0][key] != group[1][key] for key in required):
            reason = "account_model_effort_prompt_or_settings_mismatch"
        elif not (group[0].get("settings_sha256") and group[0]["settings_sha256"] == group[1].get("settings_sha256")):
            same_live_bodies = all(group[0].get(field) and group[0][field] == group[1].get(field)
                                   for field in ("request_sha256", "predicted_upstream_body_sha256"))
            if not same_live_bodies:
                reason = "settings_or_predicted_body_comparability_unproven"
        if not reason and any(row.get("model_mismatch") or row.get("effort_mismatch") for row in group):
            reason = "response_model_or_effort_mismatch"
        if not reason and any(row.get("attempts") != 1 for row in group):
            reason = "attempt_count_missing_or_not_one"
        if not reason and any(isinstance(row.get("gateway_attempt_count"), int) and row["gateway_attempt_count"] > 1 for row in group):
            reason = "gateway_retries_reported"
        if not reason and group[0].get("credential_epoch") and group[1].get("credential_epoch") and group[0]["credential_epoch"] != group[1]["credential_epoch"]:
            reason = "credential_epoch_changed"
        if reason:
            excluded.append({"experiment_id": experiment, "pair_id": pair_id, "reason": reason})
            continue
        a = next(row for row in group if row["arm"] == "A")
        b = next(row for row in group if row["arm"] == "B")
        sequences = [row.get("sequence") for row in group]
        times = [row.get("started_at") for row in group]
        if all(isinstance(value, (int, float)) and not isinstance(value, bool) for value in sequences) and len(set(sequences)) == 2:
            ordered = sorted(group, key=lambda row: row["sequence"])
        elif all(isinstance(value, str) and value for value in times) and len(set(times)) == 2:
            try:
                parsed_times = [datetime.fromisoformat(value.replace("Z", "+00:00")) for value in times]
                if any(value.tzinfo is None for value in parsed_times) or parsed_times[0] == parsed_times[1]:
                    ordered = []
                else:
                    ordered = [row for _, row in sorted(zip(parsed_times, group), key=lambda item: item[0])]
            except ValueError:
                ordered = []
        else:
            ordered = []
        pairs.append({"experiment_id": experiment, "pair_id": pair_id,
                      "account_label": a["account_label"], "model": a["model"], "effort": a["effort"],
                      "order": "".join(row["arm"] for row in ordered) or "unknown",
                      "A": a["category"], "B": b["category"],
                      "quality_comparable": a["category"] in SCORED and b["category"] in SCORED})
    def comparison(subset):
        quality = [pair for pair in subset if pair["quality_comparable"]]
        improved = sum(pair["A"] == "wrong_answer" and pair["B"] == "correct" for pair in quality)
        regressed = sum(pair["A"] == "correct" and pair["B"] == "wrong_answer" for pair in quality)
        discordant = improved + regressed
        # Exact McNemar/binomial sign test: descriptive for repeated correlated
        # requests. A single item cannot establish general intelligence or cause.
        p_value = min(1.0, 2 * sum(math.comb(discordant, i) for i in range(min(improved, regressed) + 1)) / (2 ** discordant)) if discordant else None
        return {"pairs": len(subset), "quality_pairs": len(quality), "B_improved": improved,
                "B_regressed": regressed, "discordant_pairs": discordant,
                "mcnemar_exact_two_sided_descriptive": p_value,
                "transitions_all_attempts": dict(Counter(pair["A"] + " -> " + pair["B"] for pair in subset))}
    strata = defaultdict(list)
    for pair in pairs:
        strata[(pair["account_label"], pair["model"], pair["effort"])].append(pair)
    return {"summary": comparison(pairs), "by_order": {order: comparison([pair for pair in pairs if pair["order"] == order])
                                                       for order in ("AB", "BA", "unknown")},
            "by_stratum": [{"account_label": key[0], "model": key[1], "effort": key[2], **comparison(group)}
                           for key, group in sorted(strata.items())],
            "pairs": pairs, "excluded": excluded,
            "unpaired_rows": sum(not row.get("pair_id") for row in rows)}


def field_comparability(a, b):
    fields = ("account_label", "model", "effort", "credential_epoch", "prompt_sha256",
              "request_sha256", "predicted_upstream_body_sha256", "settings_sha256")
    result = {}
    for field in fields:
        left, right = a.get(field), b.get(field)
        result[field] = "missing" if left is None or right is None else "same" if left == right else "changed"
    return result


def arm_comparisons(rows, controlled_fields=()):
    """Descriptive contrasts between named live arms, including changed bodies.

    Round blocks are used only when each selected arm appears exactly once.
    This deliberately does not label a changed prompt/body a causal TLS trial.
    """
    blocks = defaultdict(list)
    for row in rows:
        if row.get("round") is not None:
            blocks[(row.get("experiment_id") or row.get("source_run"), row.get("account_label"),
                    row.get("model"), row.get("effort"), row.get("round"))].append(row)
    comparisons, excluded = [], []
    for key, group in sorted(blocks.items(), key=lambda item: str(item[0])):
        for arm_a, arm_b in combinations(sorted({row["arm"] for row in group if row.get("arm")}), 2):
            left = [row for row in group if row.get("arm") == arm_a]
            right = [row for row in group if row.get("arm") == arm_b]
            if len(left) != 1 or len(right) != 1:
                excluded.append({"block": list(key), "arms": [arm_a, arm_b], "reason": "multiple_rows_per_arm_in_round"})
                continue
            a, b = left[0], right[0]
            fields = field_comparability(a, b)
            same_body = fields["request_sha256"] == "same" and fields["predicted_upstream_body_sha256"] == "same"
            if fields["prompt_sha256"] == "changed":
                label = "prompt_changed"
            elif fields["predicted_upstream_body_sha256"] == "changed":
                label = "predicted_upstream_body_changed"
            elif same_body:
                label = "same_request_and_predicted_body"
            else:
                label = "body_comparability_unproven"
            declared = set(controlled_fields)
            for row in (a, b):
                value = row.get("controlled_field")
                if isinstance(value, str):
                    declared.add(value)
                elif isinstance(value, list):
                    declared.update(value)
            attempt_state = "client_one_each" if a.get("attempts") == b.get("attempts") == 1 else "client_attempt_count_not_one_or_missing"
            if any(isinstance(row.get("gateway_attempt_count"), int) and row["gateway_attempt_count"] > 1 for row in (a, b)):
                attempt_state = "gateway_retries_reported_correlation_requires_review"
            mismatch = any(row.get("model_mismatch") or row.get("effort_mismatch") for row in (a, b))
            quality_observed = a["category"] in SCORED and b["category"] in SCORED
            comparisons.append({"source_run": key[0], "account_label": key[1], "account_alias": a.get("account_alias"),
                                "model": key[2], "effort": key[3], "round": key[4], "arms": [arm_a, arm_b],
                                "sequences": [a.get("sequence"), b.get("sequence")],
                                "categories": [a["category"], b["category"]], "answers": [a["answer"], b["answer"]],
                                "comparability": label, "fields": fields,
                                "declared_controlled_fields": sorted(declared),
                                "attempt_evidence": attempt_state,
                                "quality_outcomes_observed": quality_observed,
                                "same_body_quality_contrast": quality_observed and same_body and not mismatch
                                and attempt_state == "client_one_each" and fields["credential_epoch"] != "changed"
                                and fields["prompt_sha256"] == "same",
                                "causal_attribution": "not_established; predicted_body_is_not_wire_capture"})
    return {"contrasts": comparisons, "excluded": excluded,
            "comparability_counts": dict(Counter(row["comparability"] for row in comparisons)),
            "same_body_quality_contrasts": sum(row["same_body_quality_contrast"] for row in comparisons),
            "interpretation": "Descriptive named-arm contrasts. Changed prompts/bodies remain visible; no automatic cluster or TLS causal inference."}


def build_report(rows, controlled_fields=()):
    groups = defaultdict(list)
    for row in rows:
        groups[(row.get("account_label"), row.get("model"), row.get("effort"), row.get("arm"))].append(row)
    return {"schema": SCHEMA, "expected_answer": EXPECTED, "summary": count_summary(rows),
            "strata": [{"account_label": key[0], "model": key[1], "effort": key[2], "arm": key[3],
                        **count_summary(group)} for key, group in sorted(groups.items(), key=lambda item: str(item[0]))],
            "paired": paired_summary(rows), "arm_comparisons": arm_comparisons(rows, controlled_fields), "rows": rows,
            "interpretation": "Observed candy-task accuracy only. No cluster, account-flag, or causal attribution. Confidence intervals and paired tests are descriptive; requests sharing an account/prompt are correlated."}


def safe_child(directory, relative):
    path = (directory / relative).resolve()
    if directory.resolve() not in path.parents:
        raise ValueError("artifact path escapes run directory")
    return path


def load_run(path, manifest):
    path = path / "results.jsonl" if path.is_dir() else path
    rows = []
    for line in path.read_text(encoding="utf-8-sig").splitlines():
        if not line.strip():
            continue
        row = json.loads(line)
        if row.get("content_kind") != "candy" and "candy" not in row.get("case", "") and not is_live_record(row):
            continue
        identifier = record_id(row)
        metadata = manifest.get(identifier, {})
        request_path = safe_child(path.parent, "requests/" + row["request_id"] + ".json") if row.get("request_id") else None
        request = json.loads(request_path.read_text(encoding="utf-8")) if request_path and request_path.exists() else None
        event_path = safe_child(path.parent, row["events_file"]) if row.get("events_file") else (
            safe_child(path.parent, "events/" + row["request_id"] + ".jsonl") if row.get("request_id") else None)
        events = [json.loads(item) for item in event_path.read_text(encoding="utf-8").splitlines() if item.strip()] if event_path and event_path.exists() else []
        scored = score_row(row, events, request, metadata)
        scored["source_run"] = path.parent.name if path.name == "results.jsonl" else path.stem
        rows.append(scored)
    return rows


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("runs", type=Path, nargs="+")
    parser.add_argument("--manifest", type=Path)
    parser.add_argument("--controlled-field", action="append", default=[],
                        help="Declared experimental field (repeatable); disclosed, not assumed verified")
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    manifest = json.loads(args.manifest.read_text(encoding="utf-8-sig")) if args.manifest else {}
    rows = [row for run in args.runs for row in load_run(run, manifest)]
    request_ids = [row.get("request_id") for row in rows]
    if len(request_ids) != len(set(request_ids)):
        parser.error("duplicate request IDs; do not include a run twice")
    report = build_report(rows, args.controlled_field)
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    print(json.dumps({"output": str(args.output.resolve()), "summary": report["summary"],
                      "paired": report["paired"]["summary"]}, ensure_ascii=False))


if __name__ == "__main__":
    main()
