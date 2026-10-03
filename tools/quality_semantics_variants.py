"""Offline, answer-blind prompt variants and an exhaustive candy semantics proof.

Only request-content.json / prompt.txt are model inputs. manifest.json and the
mathematical proof contain answer keys and MUST stay outside model context.
No requests are sent. The original benchmark and its fixed-21 scorer are unchanged.
"""
from __future__ import annotations

import argparse
import hashlib
from itertools import product
import json
from pathlib import Path
import re

from mother_account_benchmark import CANDY_INSTRUCTIONS, CANDY_PROMPT
from quality_probe_report import extract_final, parse_answer, protocol_category, redact


SCHEMA = "codex-quality-semantics-v1"
ORIGINAL_CAPABILITY = "圆形和五角星形，不同的形状靠手感可以分辨"
CLARIFIED_CAPABILITY = "圆形和五角星形，取出前可以在袋内靠手感分辨并选择糖果的形状，但不能辨别或选择口味"
BLIND_CAPABILITY = "圆形和五角星形；糖果取出前，参赛者完全不能辨别或选择形状，也不能辨别或选择口味，只能不加选择地逐个取出"
CAPS = (7, 9, 8, 7, 6, 4)
FIELDS = ("round.apple", "round.peach", "round.watermelon",
          "star.apple", "star.peach", "star.watermelon")
VARIANT_IDS = ("baseline", "clarified_shape_choice", "blind_control")
EXPECTED = dict(zip(VARIANT_IDS, (21, 21, 29)))


def content_hash(content: dict) -> str:
    value = json.dumps(content, ensure_ascii=False, sort_keys=True, separators=(",", ":"))
    return hashlib.sha256(value.encode("utf-8")).hexdigest()


def request_content(variant_id: str) -> dict:
    """Return only model-visible fields, never the answer key or variant label."""
    if variant_id not in VARIANT_IDS:
        raise ValueError(f"unknown variant: {variant_id}")
    if CANDY_PROMPT.count(ORIGINAL_CAPABILITY) != 1:
        raise ValueError("baseline capability text changed; review variant construction")
    replacements = {"baseline": ORIGINAL_CAPABILITY,
                    "clarified_shape_choice": CLARIFIED_CAPABILITY,
                    "blind_control": BLIND_CAPABILITY}
    result = {"input": CANDY_PROMPT.replace(ORIGINAL_CAPABILITY, replacements[variant_id], 1),
              "instructions": CANDY_INSTRUCTIONS}
    validate_model_content(result)
    return result


def validate_model_content(content: dict) -> None:
    """Fail closed if the controlled construction gains numbers or solution hints."""
    if set(content) != {"input", "instructions"} or content["instructions"] != CANDY_INSTRUCTIONS:
        raise ValueError("model content must contain only the unchanged instructions and input")
    text = content["input"] + "\n" + content["instructions"]
    if re.findall(r"\d+", content["input"]) != re.findall(r"\d+", CANDY_PROMPT):
        raise ValueError("model input numeric tokens differ from the original table")
    if re.search(r"21|29|二十一|二十九|9\s*[+＋]\s*12|九.{0,6}十二|抽屉原理|鸽巢|pigeonhole|算法|参考答案|标准答案|预期答案", text, re.I):
        raise ValueError("model content contains an answer or a prohibited solution hint")


def success(state: tuple[int, ...]) -> bool:
    # A solution requires opposite shapes and opposite apple/peach flavors.
    return state[0] > 0 and state[4] > 0 or state[1] > 0 and state[3] > 0


def exhaustive_proof() -> dict:
    """Check every legal six-bin composition, independently of old verifier.

    Reverse lexicographic enumeration visits each successor (one more candy)
    first. Success is terminal. Otherwise each shape's next flavor is adversarial;
    the shape-selecting contestant minimizes this worst remaining distance.
    The blind mode lets the adversary select from every nonempty bin instead.
    """
    ranges = [range(cap, -1, -1) for cap in CAPS]
    shape_cost, blind_cost = {}, {}
    witnesses = {}
    failed_count = 0
    blind_max = -1
    blind_witness = None
    examined = 0
    for state in product(*ranges):
        examined += 1
        if success(state):
            shape_cost[state] = blind_cost[state] = 0
            continue
        failed_count += 1
        totals = (sum(state[:3]), sum(state[3:]))
        witnesses.setdefault(totals, state)
        if sum(state) > blind_max:
            blind_max, blind_witness = sum(state), state
        shape_options, blind_options = [], []
        for indexes in ((0, 1, 2), (3, 4, 5)):
            children = []
            for index in indexes:
                if state[index] < CAPS[index]:
                    following = list(state)
                    following[index] += 1
                    child = tuple(following)
                    children.append(shape_cost[child])
                    blind_options.append(blind_cost[child])
            if children:
                shape_options.append(1 + max(children))
        if not shape_options or not blind_options:
            raise AssertionError("the exhausted bag must be a successful state")
        shape_cost[state] = min(shape_options)
        blind_cost[state] = 1 + max(blind_options)

    strategies = [{"round": r, "star": s, "total": r + s,
                   "guarantees_success": (r, s) not in witnesses,
                   "failure_witness": dict(zip(FIELDS, witnesses[(r, s)])) if (r, s) in witnesses else None}
                  for r in range(sum(CAPS[:3]) + 1) for s in range(sum(CAPS[3:]) + 1)]
    minimum_fixed = min(item["total"] for item in strategies if item["guarantees_success"])
    optimal = [item for item in strategies if item["guarantees_success"] and item["total"] == minimum_fixed]
    origin = (0,) * len(CAPS)
    assert examined == 201_600
    assert minimum_fixed == shape_cost[origin] == 21
    assert [(item["round"], item["star"]) for item in optimal] == [(9, 12)]
    assert blind_cost[origin] == blind_max + 1 == 29
    return {
        "schema": SCHEMA + "/mathematical-proof",
        "model_context": False,
        "capacities": dict(zip(FIELDS, CAPS)),
        "assumptions": {"without_replacement": True, "flavor_choice_before_draw": False,
                        "goal": "round apple + star peach OR round peach + star apple",
                        "adaptive_mode": "shape can be selected; previous drawn flavors may be observed",
                        "blind_mode": "neither shape nor flavor can be selected"},
        "compositions_examined": examined,
        "failure_compositions": failed_count,
        "successful_compositions": examined - failed_count,
        "fixed_shape_splits_examined": len(strategies),
        "minimum_fixed_shape_choice": minimum_fixed,
        "minimum_adaptive_shape_choice": shape_cost[origin],
        "minimum_blind_draws": blind_cost[origin],
        "minimax_states_per_mode": examined,
        "optimal_fixed_strategies": optimal,
        "largest_blind_failure": {"total": blind_max, "composition": dict(zip(FIELDS, blind_witness))},
        "failure_states_with_at_least_29_candies": sum(sum(state) >= 29 for state, cost in blind_cost.items() if cost > 0),
        "all_fixed_shape_strategies": strategies,
        "recurrence": {"terminal_success": 0,
                       "shape_choice": "1 + min(nonempty shape: max(available flavor: cost(successor)))",
                       "blind": "1 + max(available candy bin: cost(successor))"},
    }


def score_variant_record(row: dict, variant_id: str, request: dict, events=()) -> dict:
    """Score completed raw evidence against its exact registered task only.

    Reuse terminal/text parsing, never the legacy hardcoded-21 score_row. The
    caller must provide captured model-visible input and instructions; a label
    alone cannot relabel a baseline request as a blind-control success.
    """
    expected_content = request_content(variant_id)
    captured_input = request.get("input")
    prompt = captured_input
    if isinstance(captured_input, list):
        # Match the existing Go live harness envelope without accepting extra
        # conversation context, assistant text, hidden fields, or content parts.
        if (len(captured_input) != 1 or not isinstance(captured_input[0], dict)
                or set(captured_input[0]) - {"role", "type", "content"}
                or captured_input[0].get("role") != "user"
                or captured_input[0].get("type", "message") != "message"):
            raise ValueError("variant requires exactly one user message without additional context")
        parts = captured_input[0].get("content")
        if (not isinstance(parts, list) or len(parts) != 1 or not isinstance(parts[0], dict)
                or set(parts[0]) != {"type", "text"} or parts[0]["type"] != "input_text"):
            raise ValueError("variant requires exactly one input_text part")
        prompt = parts[0]["text"]
    actual_content = {"input": prompt, "instructions": request.get("instructions")}
    if actual_content != expected_content:
        raise ValueError("captured prompt/instructions do not match the declared variant")
    text, source, conflicting = extract_final(row, events)
    answer, extraction = parse_answer(text)
    category = protocol_category(row, events)
    if conflicting:
        answer, extraction = None, "conflicting_output_sources"
    if category == "completed":
        category = "ambiguous" if answer is None else "correct" if answer == EXPECTED[variant_id] else "wrong_answer"
    return redact({"schema": SCHEMA + "/score", "variant_id": variant_id,
                   "task_relation": "original" if variant_id == "baseline" else "diagnostic_only",
                   "original_benchmark_eligible": variant_id == "baseline",
                   "original_benchmark_pass": variant_id == "baseline" and category == "correct",
                   "category": category, "answer": answer, "expected_answer": EXPECTED[variant_id],
                   "prompt_sha256": content_hash(actual_content),
                   "captured_input_sha256": content_hash({"input": captured_input}),
                   "raw_final": text, "output_source": source, "extraction": extraction})


def generate(output: Path) -> dict:
    output.mkdir(parents=True, exist_ok=True)
    variants = []
    for variant_id in VARIANT_IDS:
        content = request_content(variant_id)
        directory = output / variant_id
        directory.mkdir(exist_ok=True)
        (directory / "prompt.txt").write_text(content["input"], encoding="utf-8")
        (directory / "request-content.json").write_text(json.dumps(content, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
        variants.append({"variant_id": variant_id, "expected_answer": EXPECTED[variant_id],
                         "original_benchmark_eligible": variant_id == "baseline",
                         "task_relation": "original" if variant_id == "baseline" else "diagnostic_only",
                         "request_content_file": f"{variant_id}/request-content.json",
                         "prompt_file": f"{variant_id}/prompt.txt", "prompt_sha256": content_hash(content),
                         "model_input_has_expected_answer": False})
    proof = exhaustive_proof()
    (output / "mathematical-proof.json").write_text(json.dumps(proof, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    manifest = {"schema": SCHEMA, "model_context": False,
                "purpose": "Separate interpretation of shape choice from original-task performance",
                "variants": variants, "mathematical_proof_file": "mathematical-proof.json",
                "execution": {"network_requests_by_generator": 0,
                              "planned_max_attempts_per_account_per_variant": 1,
                              "new_session_per_request": True, "automatic_retries": False,
                              "instructions_identical": True, "model_and_effort_identical": True,
                              "stop_on_rate_limit": True},
                "scoring": {"legacy_quality_probe_report": "fixed to 21; do not import controls into its baseline pool",
                            "variant_scorer": "quality_semantics_variants.score_variant_record",
                            "rate_limit_or_incomplete": "unscored, not wrong_answer",
                            "controls_are_original_benchmark_passes": False,
                            "answer_key_must_not_enter_model_context": True}}
    (output / "manifest.json").write_text(json.dumps(manifest, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    return manifest


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output-dir", type=Path, default=Path(__file__).resolve().parents[1] / ".local" / "quality-20260924" / "semantics-control")
    args = parser.parse_args()
    manifest = generate(args.output_dir)
    print(json.dumps({"manifest": str((args.output_dir / "manifest.json").resolve()),
                      "variants": [variant["variant_id"] for variant in manifest["variants"]],
                      "network_requests": 0}, ensure_ascii=False, indent=2))


if __name__ == "__main__":
    main()
