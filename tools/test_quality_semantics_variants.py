"""Offline checks for prompt isolation, exhaustive proof and variant-safe scoring."""
import copy
import json
from pathlib import Path
import tempfile
import unittest

import quality_semantics_variants as variants
from mother_account_benchmark import CANDY_INSTRUCTIONS, CANDY_PROMPT


def completed(answer):
    return {"http_status": 200, "terminal": "response.completed", "protocol_success": True,
            "response": {"status": "completed", "output": [message(f"最终答案：{answer}个")]}}


def message(text, **extra):
    return {"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": text}], **extra}


class SemanticsPromptTests(unittest.TestCase):
    def test_baseline_is_exact_original(self):
        self.assertEqual(variants.request_content("baseline"),
                         {"input": CANDY_PROMPT, "instructions": CANDY_INSTRUCTIONS})

    def test_only_capability_clause_changes(self):
        for variant_id, replacement in (("clarified_shape_choice", variants.CLARIFIED_CAPABILITY),
                                        ("blind_control", variants.BLIND_CAPABILITY)):
            with self.subTest(variant=variant_id):
                content = variants.request_content(variant_id)
                self.assertEqual(content["input"].replace(replacement, variants.ORIGINAL_CAPABILITY), CANDY_PROMPT)
                self.assertEqual(content["instructions"], CANDY_INSTRUCTIONS)

    def test_model_content_has_no_answer_key_or_variant_label(self):
        hashes = set()
        for variant_id in variants.VARIANT_IDS:
            content = variants.request_content(variant_id)
            self.assertEqual(set(content), {"input", "instructions"})
            variants.validate_model_content(content)
            hashes.add(variants.content_hash(content))
            for prohibited in ("expected_answer", "21", "29", "9+12", "二十一", "二十九", "抽屉原理", "pigeonhole"):
                self.assertNotIn(prohibited, json.dumps(content, ensure_ascii=False))
        self.assertEqual(len(hashes), 3)

    def test_guard_rejects_numeric_and_nonnumeric_hints(self):
        for addition in ("取21个。", "可考虑二十一个。", "考虑九个和十二个。", "使用鸽巢原理。", "给出算法。"):
            with self.subTest(addition=addition):
                content = variants.request_content("baseline")
                content["input"] += addition
                with self.assertRaises(ValueError):
                    variants.validate_model_content(content)

    def test_unknown_variant_rejected(self):
        with self.assertRaises(ValueError):
            variants.request_content("fixed_baseline")

    def test_answer_key_and_changed_instructions_rejected_from_model_content(self):
        for field, value in (("expected_answer", 21), ("instructions", "答案是21")):
            content = variants.request_content("baseline")
            content[field] = value
            with self.assertRaises(ValueError):
                variants.validate_model_content(content)


class SemanticsProofTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.proof = variants.exhaustive_proof()

    def test_complete_state_space_and_adaptive_bound(self):
        proof = self.proof
        self.assertEqual(proof["compositions_examined"], 201600)
        self.assertEqual(proof["minimax_states_per_mode"], 201600)
        self.assertEqual(proof["failure_compositions"] + proof["successful_compositions"], 201600)
        self.assertEqual(proof["minimum_fixed_shape_choice"], 21)
        self.assertEqual(proof["minimum_adaptive_shape_choice"], 21)
        self.assertEqual(proof["minimum_blind_draws"], 29)

    def test_fixed_split_minimality_and_each_failure_witness(self):
        strategies = self.proof["all_fixed_shape_strategies"]
        self.assertEqual(len(strategies), 25 * 18)
        self.assertEqual([(x["round"], x["star"]) for x in self.proof["optimal_fixed_strategies"]], [(9, 12)])
        for strategy in strategies:
            if strategy["total"] < 21:
                self.assertFalse(strategy["guarantees_success"])
            witness = strategy["failure_witness"]
            if witness:
                values = tuple(witness[field] for field in variants.FIELDS)
                self.assertFalse(variants.success(values))
                self.assertEqual(sum(values[:3]), strategy["round"])
                self.assertEqual(sum(values[3:]), strategy["star"])
                self.assertTrue(all(0 <= value <= cap for value, cap in zip(values, variants.CAPS)))

    def test_blind_control_lower_and_upper_bounds(self):
        witness = self.proof["largest_blind_failure"]
        self.assertEqual(witness["total"], 28)
        values = tuple(witness["composition"][field] for field in variants.FIELDS)
        self.assertEqual(sum(values), 28)
        self.assertFalse(variants.success(values))
        self.assertEqual(self.proof["failure_states_with_at_least_29_candies"], 0)

    def test_export_keeps_keys_out_of_model_inputs(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)
            manifest = variants.generate(path)
            self.assertEqual(manifest, json.loads((path / "manifest.json").read_text(encoding="utf-8")))
            self.assertFalse(manifest["model_context"])
            for variant in manifest["variants"]:
                content = json.loads((path / variant["request_content_file"]).read_text(encoding="utf-8"))
                self.assertEqual(variants.content_hash(content), variant["prompt_sha256"])
                self.assertEqual((path / variant["prompt_file"]).read_text(encoding="utf-8"), content["input"])
                self.assertEqual(set(content), {"input", "instructions"})
            self.assertEqual(manifest["execution"]["network_requests_by_generator"], 0)


class SemanticsScoreTests(unittest.TestCase):
    def score(self, row, variant_id="baseline", **extra):
        return variants.score_variant_record(row, variant_id, variants.request_content(variant_id), **extra)

    def test_only_original_exact_answer_can_pass_original(self):
        for variant_id, answer in (("baseline", 21), ("clarified_shape_choice", 21), ("blind_control", 29)):
            with self.subTest(variant_id=variant_id):
                result = self.score(completed(answer), variant_id)
                self.assertEqual(result["category"], "correct")
                self.assertEqual(result["original_benchmark_pass"], variant_id == "baseline")
                self.assertEqual(result["original_benchmark_eligible"], variant_id == "baseline")

    def test_baseline_29_wrong_blind_29_correct_blind_21_wrong(self):
        self.assertEqual(self.score(completed(29))["category"], "wrong_answer")
        self.assertEqual(self.score(completed(29), "blind_control")["category"], "correct")
        self.assertEqual(self.score(completed(21), "blind_control")["category"], "wrong_answer")

    def test_cannot_relabel_different_prompt_or_instructions(self):
        for request in (variants.request_content("baseline"), {"input": "unrelated", "instructions": CANDY_INSTRUCTIONS},
                        {"input": variants.request_content("blind_control")["input"], "instructions": "answer key"}):
            with self.assertRaises(ValueError):
                variants.score_variant_record(completed(29), "blind_control", request)

    def test_model_settings_are_allowed_but_never_added_to_prompt(self):
        request = variants.request_content("blind_control")
        request.update(model="test-model", reasoning={"effort": "medium"}, store=False, stream=True)
        result = variants.score_variant_record(completed(29), "blind_control", request)
        self.assertEqual(result["category"], "correct")

    def test_exact_go_harness_single_user_message_envelope(self):
        request = variants.request_content("blind_control")
        request["input"] = [{"role": "user", "content": [{"type": "input_text", "text": request["input"]}]}]
        result = variants.score_variant_record(completed(29), "blind_control", request)
        self.assertEqual(result["category"], "correct")
        self.assertEqual(result["prompt_sha256"], variants.content_hash(variants.request_content("blind_control")))

    def test_array_input_cannot_smuggle_extra_context(self):
        valid = variants.request_content("blind_control")
        message = {"role": "user", "content": [{"type": "input_text", "text": valid["input"]}]}
        for bad_input in ([message, copy.deepcopy(message)],
                          [{**message, "role": "assistant"}],
                          [{**message, "context": "answer key"}],
                          [{**message, "content": message["content"] * 2}],
                          [{**message, "content": [{**message["content"][0], "hint": "answer key"}]}]):
            with self.assertRaises(ValueError):
                variants.score_variant_record(completed(29), "blind_control", {**valid, "input": bad_input})

    def test_protocol_failures_are_not_wrong_answers_or_passes(self):
        for changes, expected in (({"http_status": 429}, "rate_limit"),
                                  ({"failure_category": "timeout"}, "network"),
                                  ({"terminal": "response.incomplete"}, "incomplete"),
                                  ({"protocol_success": False}, "protocol_error")):
            with self.subTest(changes=changes):
                row = completed(29)
                row.update(changes)
                result = self.score(row, "blind_control")
                self.assertEqual(result["category"], expected)
                self.assertFalse(result["original_benchmark_pass"])

    def test_conflicting_final_sources_are_ambiguous(self):
        event = {"type": "response.output_item.done", "output_index": 0, "item": message("最终答案：21个")}
        result = self.score(completed(29), "blind_control", events=[event])
        self.assertEqual(result["category"], "ambiguous")
        self.assertIsNone(result["answer"])

    def test_sse_reconstruction_scores_own_variant(self):
        row = completed(29)
        row["response"]["output"] = []
        event = {"type": "response.output_item.done", "output_index": 0, "item": message("最终答案：29个")}
        result = self.score(row, "blind_control", events=[event])
        self.assertEqual(result["category"], "correct")
        self.assertEqual(result["output_source"], "sse_items")

    def test_commentary_and_reasoning_do_not_override_final(self):
        row = completed(29)
        row["response"]["output"] = [{"type": "reasoning", "summary": [{"text": "最终答案：21个"}]},
                                       message("最终答案：21个", phase="commentary"),
                                       message("最终答案：29个", phase="final_answer")]
        self.assertEqual(self.score(row, "blind_control")["category"], "correct")

    def test_flat_live_completion_requires_all_evidence(self):
        row = {"live_model": True, "account_alias": "Q1", "arm": "native", "http_status": 200,
               "response_status": "completed", "protocol_success": True,
               "event_counts": {"response.completed": 1}, "output_text": "最终答案：29个"}
        self.assertEqual(self.score(row, "blind_control")["category"], "correct")
        for key in ("response_status", "protocol_success", "event_counts"):
            invalid = copy.deepcopy(row)
            invalid.pop(key)
            self.assertEqual(self.score(invalid, "blind_control")["category"], "protocol_error")

    def test_conditional_answer_is_not_selectively_scored(self):
        row = completed(29)
        row["response"]["output"] = [message("如果能选择形状则21，否则29。")]
        self.assertEqual(self.score(row, "blind_control")["category"], "ambiguous")


if __name__ == "__main__":
    unittest.main()
