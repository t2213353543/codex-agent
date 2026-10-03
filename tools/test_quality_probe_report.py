"""Offline parser, classification, pairing and artifact regression tests."""
import copy
import hashlib
from functools import lru_cache
import json
from pathlib import Path
import tempfile
import unittest

from quality_probe_report import (
    arm_comparisons, build_report, extract_final, load_run, paired_summary, parse_answer,
    protocol_category, redact, safe_child, score_row,
)


def message(text, **kwargs):
    return {"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": text}], **kwargs}


def completed(text="最终答案：21个", **kwargs):
    row = {"request_id": "req_test", "case": "candy_1", "http_status": 200,
           "terminal": "response.completed", "protocol_success": True, "attempts": 1,
           "model": "model-test", "effort": "medium", "response": {"status": "completed", "output": [message(text)]}}
    row.update(kwargs)
    return row


class QualityScoringTests(unittest.TestCase):
    def test_answer_formats(self):
        for text in ("最终答案：21个", "最终答案：２１个", "**最终答案：21个**", "最终答案：二十一个",
                     "Final answer: 21 candies", "21", "结论：21颗。", "先试过29但可选形状。\n最终答案：21个"):
            with self.subTest(text=text):
                self.assertEqual(parse_answer(text)[0], 21)

    def test_inline_concluding_answer_is_scored(self):
        for text in ("均无法满足，因此答案是 21 个。", "所以答案是 29 个。",
                     "所以 20 个不能保证，答案是 21 个（9 个圆形、12 个五角星形）。"):
            with self.subTest(text=text):
                value, method = parse_answer(text)
                self.assertEqual(value, 21 if "21" in text else 29)
                self.assertEqual(method, "last_line")

    def test_ambiguous_answers_are_not_passes(self):
        for text in ("试试21，但是我不知道。", "最终答案：21个\n最终答案：29个",
                     "如果可以选形状，最终答案：21个；否则29个", "最终答案：21.5个", "没有答案",
                     "最终答案：21或29个", "最终答案：21/29个", "最终答案：21个？"):
            with self.subTest(text=text):
                self.assertIsNone(parse_answer(text)[0])

    def test_terminal_boxed_candy_answer_is_scored_without_changing_key(self):
        # Preserve the shape of the real clean CLI result, without a live artifact dependency.
        text = "若不满足条件，最多可取28个。\n\n所以至少取出：\n\n\\[\n\\boxed{29\\text{个}}\n\\]"
        self.assertEqual(parse_answer(text), (29, "terminal_boxed"))
        scored = score_row(completed(text))
        self.assertEqual((scored["answer"], scored["category"], scored["expected_answer"]),
                         (29, "wrong_answer", 21))

    def test_terminal_boxed_numeric_formats(self):
        for text in (r"\boxed{21}", r"\(\boxed{21}\)", r"$$\boxed{21\text{个}}$$",
                     r"$\boxed{２１}$", "最终答案：21个\n\\[\\boxed{21}\\]",
                     r"最终答案：\boxed{21\text{个}}"):
            with self.subTest(text=text):
                self.assertEqual(parse_answer(text)[0], 21)

    def test_terminal_boxed_chinese_concluding_labels_are_symmetric(self):
        for number in (21, 29):
            for prefix in ("所以", "因此"):
                for label in ("答案", "结论"):
                    text = prefix + label + r"是 \(\boxed{" + str(number) + r"}\)。"
                    with self.subTest(text=text):
                        self.assertEqual(parse_answer(text), (number, "terminal_boxed"))
                        result = score_row(completed(text))
                        self.assertEqual(result["answer"], number)
                        self.assertEqual(result["category"], "correct" if number == 21 else "wrong_answer")

    def test_v12_actual_final_lower_bound_proof_before_boxed_candy_unit(self):
        # Exact final text from v12/03-g-new2; no dependency on private artifacts.
        text = (
            "最少需要取出 **21 颗**。\n\n可以利用手感有意识地取：\n\n"
            "- 圆形糖果 9 颗\n- 五角星形糖果 12 颗\n\n理由：\n\n"
            "- 圆形西瓜味只有 8 颗，所以取 9 颗圆形糖时，至少有一颗是苹果味或桃子味。\n"
            "- 五角星形中：\n  - 非苹果味共有 \\(6+4=10\\) 颗，因此取 12 颗必有苹果味；\n"
            "  - 非桃子味共有 \\(7+4=11\\) 颗，因此取 12 颗也必有桃子味。\n\n"
            "所以，无论圆形糖中出现的是苹果味还是桃子味，都能在五角星形糖中找到另一种口味与之匹配。\n\n"
            "而取 20 颗或更少时，总能构造出不满足条件的取法，因此答案是：\n\n"
            "\\[\n\\boxed{21\\text{颗}}\n\\]"
        )
        self.assertEqual(hashlib.sha256(text.encode()).hexdigest(), "3d71b039184398aecf59a52bf143749d44c9ae27ea04d23477dc52fd5194b41f")
        self.assertEqual(parse_answer(text), (21, "terminal_boxed"))

    def test_boxed_count_units_and_lower_bound_proof_do_not_choose_answer_key(self):
        for number in (21, 29):
            for unit in ("个", "颗", "粒", "枚"):
                for representation in (unit, r"\text{" + unit + "}", r"\mathrm{" + unit + "}"):
                    text = "取 20 " + unit + "或更少时不能保证，因此答案是：\n\n\\[\\boxed{" + str(number) + representation + "}\\]"
                    with self.subTest(number=number, unit=representation):
                        self.assertEqual(parse_answer(text), (number, "terminal_boxed"))

    def test_boxed_unit_support_preserves_conditions_candidates_and_multiple_answers(self):
        for text in (
            "如果允许选择形状，取20颗或更少时不够，因此答案是：\n\\[\\boxed{21\\text{颗}}\\]",
            "取20颗或更少时不够；否则答案不同，因此答案是：\n\\[\\boxed{21\\text{颗}}\\]",
            "最终答案：29颗\n取20颗或更少不能保证，因此答案是：\n\\[\\boxed{21\\text{颗}}\\]",
            "候选为\\boxed{21\\text{粒}}，还需验证。",
            "取20颗或29颗均可能，因此答案是：\n\\[\\boxed{21\\text{颗}}\\]",
            "取20颗或更少时不够，因此答案是：\n\\[\\boxed{21\\text{颗}}\\]\n也可能29颗。",
            r"\boxed{21\text{颗或29颗}}", r"\boxed{21\text{粒}} 或 \boxed{29\text{粒}}",
        ):
            with self.subTest(text=text):
                self.assertIsNone(parse_answer(text)[0])

    def test_v12_actual_final_boxed_sum_then_unit_only_suffix(self):
        text = (
            "最少取出 **21 个**。\n\n利用手感控制形状：取 **9 个圆形糖果和 12 个五角星形糖果**。\n\n"
            "若仍不满足要求，则必须同时避开两种搭配。所有失败情况可归为：\n\n"
            "- 圆形中没有苹果味和桃子味：9 个圆形只能是西瓜味，但西瓜味圆形仅 8 个，不可能。\n"
            "- 所取糖中没有苹果味：五角星形只能来自桃子味和西瓜味，共 \\(6+4=10\\) 个，不足 12 个。\n"
            "- 所取糖中没有桃子味：五角星形只能来自苹果味和西瓜味，共 \\(7+4=11\\) 个，不足 12 个。\n"
            "- 五角星形中没有苹果味和桃子味：12 个五角星形只能是西瓜味，但仅有 4 个，不可能。\n\n"
            "所以取 9 圆形、12 五角星形必然成功。\n\n"
            "同时，少于 21 个无法保证：要排除“圆形全是西瓜味”，至少要取 9 个圆形；在圆形不超过 15 个时，"
            "要排除“没有桃子味”的情况，至少还需取 12 个五角星形。因此至少需要\n\n"
            "\\[\n9+12=\\boxed{21}\n\\]\n\n个糖果。"
        )
        self.assertEqual(hashlib.sha256(text.encode()).hexdigest(), "4938edd821480a7e21c9a4970633242d2610a61770556d1c46634d1ba66567e5")
        self.assertEqual(parse_answer(text), (21, "terminal_boxed"))

    def test_boxed_sum_and_unit_suffix_remain_numeric_and_symmetric(self):
        for number, expression in ((21, "9+12"), (29, "28+1"), (29, "9+12")):
            for unit in ("个糖果。", "颗糖。", "粒", "枚糖果！"):
                with self.subTest(number=number, unit=unit):
                    self.assertEqual(parse_answer("因此至少需要\n\\[" + expression + r"=\boxed{" + str(number) + "}\\]\n\n" + unit), (number, "terminal_boxed"))
        wrong = score_row(completed("因此至少需要\n\\[9+12=\\boxed{29}\\]\n个糖果。"))
        self.assertEqual((wrong['answer'], wrong['category']), (29, 'wrong_answer'))

    def test_boxed_sum_and_unit_suffix_reject_unsupported_or_conflicting_prose(self):
        for text in (
            "\\[20+1=\\boxed{21}\\]\n个糖果，或者29个。",
            "\\[20+1=\\boxed{21}\\]\n个糖果。\n仍需验证。",
            "如果能选择形状：\n\\[9+12=\\boxed{21}\\]\n个糖果。",
            "最终答案：29个\n\\[9+12=\\boxed{21}\\]\n个糖果。",
            r"\[9*12=\boxed{21}\]", r"\[21-0=\boxed{21}\]",
            r"\[9.0+12=\boxed{21}\]", r"\[9+12=\boxed{21}=29\]",
            r"\[9+12=\boxed{21}\]\n个或颗糖果。",
        ):
            with self.subTest(text=text):
                self.assertIsNone(parse_answer(text)[0])

    def test_concluding_label_does_not_relax_conditional_or_multiple_boxes(self):
        for number in (21, 29):
            final = r"所以答案是 \(\boxed{" + str(number) + r"}\)。"
            for text in (
                "如果可以选择形状，" + final,
                "如果可以选择形状：\n\n" + final,
                final + "否则需要其他数量。",
                final + r" 或者 \(\boxed{29}\)。",
                final + "\n" + final,
                r"证明中的中间数为 \boxed{28}。" + "\n" + final,
                final + "\n还需要确认。",
            ):
                with self.subTest(text=text):
                    self.assertIsNone(parse_answer(text)[0])

    def test_nonfinal_or_non_numeric_boxes_are_not_answers(self):
        for text in ("推理中得到\\boxed{21}，还需要检查。", "\\[\\boxed{21}\\]\n尚不能确定答案。",
                     r"\boxed{21.5}", r"\boxed{21+8}", r"\boxed{21/29}",
                     r"\boxed{\frac{21}{29}}", r"\boxed{21\text{或29个}}",
                     "最终答案：21个\n\\[\\boxed{29+1}\\]",
                     "最终答案：21个\n\\[\\boxed{21.5}\\]"):
            with self.subTest(text=text):
                self.assertIsNone(parse_answer(text)[0])

    def test_middle_box_does_not_override_a_later_explicit_final(self):
        self.assertEqual(parse_answer("推理得到\\boxed{28}作为失败上界。\n最终答案：29个"),
                         (29, "explicit_final"))

    def test_multiple_conditional_and_conflicting_boxes_are_rejected(self):
        for text in ("\\boxed{21}\n\\boxed{29}", "\\boxed{21}\n\\boxed{21}",
                     "如果可以选择形状：\n\n\\[\\boxed{21}\\]",
                     "If shape selection is possible:\n\\[\\boxed{21}\\]",
                     r"最终答案：21个；或者\boxed{29}",
                     "最终答案：21个\n\\[\\boxed{29}\\]",
                     "最终答案：21.5个\n\\[\\boxed{21}\\]",
                     "最终答案：-21个\n\\[\\boxed{21}\\]",
                     "如果可以选形状，最终答案：21个\n\\[\\boxed{21}\\]"):
            with self.subTest(text=text):
                self.assertIsNone(parse_answer(text)[0])

    def test_wrong_answer_not_salvaged_by_stray_21(self):
        row = score_row(completed("21并不够。\n最终答案：29个"))
        self.assertEqual((row["answer"], row["category"]), (29, "wrong_answer"))

    def test_reasoning_text_and_commentary_cannot_override_final(self):
        row = completed()
        row["response"]["output"] = [
            {"type": "reasoning", "summary": [{"type": "summary_text", "text": "最终答案：29个"}]},
            message("最终答案：29个", phase="commentary"), message("最终答案：21个", phase="final_answer")]
        self.assertEqual(score_row(row)["answer"], 21)

    def test_empty_terminal_reconstructed_from_done_item(self):
        row = completed()
        row["response"]["output"] = []
        events = [{"type": "response.output_item.done", "output_index": 3, "item": message("最终答案：21个")}]
        self.assertEqual(score_row(row, events)["category"], "correct")
        self.assertEqual(score_row(row, events)["output_source"], "sse_items")

    def test_reasoning_only_terminal_does_not_block_final_reconstruction(self):
        row = completed()
        row["response"]["output"] = [{"type": "reasoning", "encrypted_content": "not-an-answer"}]
        events = [{"type": "response.output_item.done", "output_index": 1, "item": message("最终答案：21个")}]
        self.assertEqual(score_row(row, events)["category"], "correct")

    def test_text_done_without_item_and_delta_not_doubled(self):
        row = completed()
        row["response"]["output"] = []
        events = [{"type": "response.output_text.delta", "output_index": 1, "content_index": 0, "delta": "最终答案：2"},
                  {"type": "response.output_text.delta", "output_index": 1, "content_index": 0, "delta": "1个"},
                  {"type": "response.output_text.done", "output_index": 1, "content_index": 0, "text": "最终答案：21个"}]
        self.assertEqual(extract_final(row, events)[0], "最终答案：21个")

    def test_content_done_and_item_id_resolution(self):
        row = completed()
        row["response"]["output"] = []
        events = [{"type": "response.output_item.added", "output_index": 2,
                   "item": {"id": "msg_x", "type": "message", "role": "assistant", "content": []}},
                  {"type": "response.content_part.done", "item_id": "msg_x", "content_index": 0,
                   "part": {"type": "output_text", "text": "最终答案：21个"}}]
        self.assertEqual(score_row(row, events)["category"], "correct")

    def test_conflicting_sources_are_reviewed(self):
        row = completed()
        events = [{"type": "response.output_item.done", "output_index": 0, "item": message("最终答案：29个")}]
        result = score_row(row, events)
        self.assertEqual(result["category"], "ambiguous")
        self.assertIsNone(result["answer"])

    def test_failure_categories_do_not_become_wrong_answers(self):
        cases = [(dict(http_status=401), "auth"), (dict(http_status=403), "auth"),
                 (dict(http_status=429), "rate_limit"), (dict(http_status=503), "http_error"),
                 (dict(failure_category="timeout"), "network"),
                 (dict(failure_category="transport_error"), "network"),
                 (dict(terminal="response.incomplete"), "incomplete"),
                 (dict(terminal="response.failed"), "upstream_error"),
                 (dict(protocol_success=False), "protocol_error")]
        for changes, expected in cases:
            with self.subTest(changes=changes):
                self.assertEqual(score_row(completed("最终答案：29个", **changes))["category"], expected)

    def test_terminal_event_overrides_stale_record(self):
        events = [{"type": "response.incomplete", "response": {"status": "incomplete"}}]
        self.assertEqual(protocol_category(completed(), events), "incomplete")

    def test_error_code_inside_successful_http_response(self):
        row = completed()
        row["response"]["error"] = {"code": "token_revoked"}
        self.assertEqual(protocol_category(row), "auth")

    def test_unknown_error_is_not_completed(self):
        row = completed()
        row["response"]["error"] = {"code": "server_error"}
        self.assertEqual(protocol_category(row), "upstream_error")

    def test_auth_error_event_is_recognized(self):
        events = [{"type": "error", "error": {"code": "token_revoked"}}]
        self.assertEqual(protocol_category(completed(), events), "auth")

    def test_empty_output_requires_review(self):
        self.assertEqual(score_row(completed(""))["category"], "ambiguous")

    def test_stream_without_terminal_is_not_completed(self):
        row = completed(stream=True, terminal=None)
        self.assertEqual(score_row(row)["category"], "protocol_error")

    def test_only_allowlisted_fields_are_retained(self):
        row = completed("Bearer abc-secret access_token=private user@example.com\n最终答案：21个")
        row.update(headers={"Authorization": "Bearer unused"}, refresh_token="never-copy")
        report = json.dumps(score_row(row), ensure_ascii=False)
        for secret in ("abc-secret", "private", "user@example.com", "unused", "never-copy"):
            self.assertNotIn(secret, report)
        self.assertIn("raw_final", report)

    def test_model_effort_mismatch_is_observed(self):
        row = completed()
        row["response"].update(model="different", reasoning={"effort": "low"})
        result = score_row(row)
        self.assertTrue(result["model_mismatch"])
        self.assertTrue(result["effort_mismatch"])


class PairingTests(unittest.TestCase):
    def pair(self, a="wrong_answer", b="correct", order="AB", pair_id="pair1"):
        common = {"account_label": "account_x", "model": "model-test", "effort": "medium",
                  "prompt_sha256": "prompt", "settings_sha256": "settings", "attempts": 1,
                  "experiment_id": "experiment", "pair_id": pair_id}
        return [{**common, "arm": arm, "sequence": order.index(arm), "category": value}
                for arm, value in (("A", a), ("B", b))]

    def test_ab_ba_pairs_count_discordances(self):
        rows = self.pair() + self.pair("correct", "wrong_answer", "BA", "pair2")
        result = paired_summary(rows)
        self.assertEqual(result["summary"]["pairs"], 2)
        self.assertEqual(result["summary"]["B_improved"], 1)
        self.assertEqual(result["summary"]["B_regressed"], 1)
        self.assertEqual(result["by_order"]["BA"]["pairs"], 1)
        self.assertEqual(result["summary"]["mcnemar_exact_two_sided_descriptive"], 1)

    def test_different_accounts_models_efforts_prompts_settings_excluded(self):
        for field in ("account_label", "model", "effort", "prompt_sha256", "settings_sha256"):
            rows = self.pair()
            rows[1][field] = "other"
            with self.subTest(field=field):
                self.assertEqual(paired_summary(rows)["summary"]["pairs"], 0)

    def test_missing_attempts_and_automatic_retries_excluded(self):
        for attempts in (None, 2):
            rows = self.pair()
            rows[1]["attempts"] = attempts
            self.assertEqual(paired_summary(rows)["excluded"][0]["reason"], "attempt_count_missing_or_not_one")

    def test_failed_transport_preserved_but_not_quality_discordance(self):
        result = paired_summary(self.pair("rate_limit", "correct"))
        self.assertEqual(result["summary"]["pairs"], 1)
        self.assertEqual(result["summary"]["quality_pairs"], 0)
        self.assertEqual(result["summary"]["B_improved"], 0)

    def test_missing_pair_metadata_never_invented(self):
        result = paired_summary([score_row(completed())])
        self.assertEqual(result["unpaired_rows"], 1)
        self.assertEqual(result["summary"]["pairs"], 0)

    def test_duplicate_arm_and_missing_identity_excluded(self):
        rows = self.pair()
        rows[1]["arm"] = "A"
        self.assertEqual(paired_summary(rows)["summary"]["pairs"], 0)
        rows = self.pair()
        rows[0]["account_label"] = None
        self.assertEqual(paired_summary(rows)["summary"]["pairs"], 0)

    def test_correct_all_attempts_and_scorable_have_distinct_denominators(self):
        report = build_report(self.pair("network", "correct"))
        self.assertEqual(report["summary"]["correct_all_attempts"], .5)
        self.assertEqual(report["summary"]["correct_among_scorable"], 1)

    def test_timestamps_are_compared_as_instants(self):
        rows = self.pair()
        for row in rows:
            row.pop("sequence")
        rows[0]["started_at"] = "2026-09-24T12:00:00+08:00"
        rows[1]["started_at"] = "2026-09-24T05:00:00Z"
        self.assertEqual(paired_summary(rows)["pairs"][0]["order"], "AB")

    def test_invalid_timestamps_do_not_invent_an_order(self):
        rows = self.pair()
        for index, row in enumerate(rows):
            row.pop("sequence")
            row["started_at"] = "invalid-" + str(index)
        self.assertEqual(paired_summary(rows)["pairs"][0]["order"], "unknown")

    def test_output_limits_or_reasoning_settings_prevent_pairing(self):
        request = {"input": "same candy", "instructions": "same", "model": "model-test", "reasoning": {"effort": "medium"}}
        a = score_row(completed(), request=request, metadata={"account_label": "a", "pair_id": "p", "arm": "A"})
        changed = {**request, "max_output_tokens": 30}
        b = score_row(completed(), request=changed, metadata={"account_label": "a", "pair_id": "p", "arm": "B"})
        self.assertEqual(paired_summary([a, b])["summary"]["pairs"], 0)


class ArtifactTests(unittest.TestCase):
    def test_real_format_loading_does_not_modify_inputs(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / "requests").mkdir()
            row = completed()
            source = json.dumps(row)
            (root / "results.jsonl").write_text(source + "\n", encoding="utf-8")
            (root / "requests/req_test.json").write_text(json.dumps({"input": "candy", "model": "model-test"}), encoding="utf-8")
            rows = load_run(root, {})
            self.assertEqual(rows[0]["category"], "correct")
            self.assertTrue(rows[0]["prompt_sha256"])
            self.assertEqual((root / "results.jsonl").read_text(encoding="utf-8"), source + "\n")

    def test_path_traversal_rejected(self):
        with tempfile.TemporaryDirectory() as temporary:
            with self.assertRaises(ValueError):
                safe_child(Path(temporary), "../outside.json")

    def test_score_does_not_mutate_raw_response_or_events(self):
        row = completed()
        row["response"]["output"] = []
        events = [{"type": "response.output_item.added", "output_index": 1, "item": message("")},
                  {"type": "response.output_text.done", "output_index": 1, "text": "最终答案：21个"}]
        before = copy.deepcopy((row, events))
        score_row(row, events)
        self.assertEqual((row, events), before)


class FlattenedLiveArtifactTests(unittest.TestCase):
    fixture = Path(__file__).parent / "testdata/quality_live_q0.jsonl"

    def raw_rows(self):
        return [json.loads(line) for line in self.fixture.read_text(encoding="utf-8").splitlines()]

    def test_actual_live_schema_without_terminal_imported_and_scored(self):
        rows = load_run(self.fixture, {})
        self.assertEqual([row["category"] for row in rows], ["correct", "correct", "wrong_answer"])
        self.assertEqual([row["answer"] for row in rows], [21, 21, 29])
        self.assertEqual(rows[-1]["account_alias"], "Q0")
        self.assertEqual(rows[-1]["response_effort"], "medium")
        self.assertEqual(rows[-1]["usage"]["reasoning_tokens"], 1002)
        self.assertIn("29_matches_blind_draw_bound_28_plus_1", rows[-1]["rationale_signals"])
        self.assertNotIn("credential_sha256", json.dumps(rows))

    def test_all_three_completion_evidence_fields_required(self):
        original = self.raw_rows()[0]
        for field in ("protocol_success", "response_status", "event_counts"):
            row = copy.deepcopy(original)
            row.pop(field)
            with self.subTest(field=field):
                self.assertEqual(score_row(row)["category"], "protocol_error")

    def test_invalid_or_conflicting_terminal_counts_not_completed(self):
        for counts in ({"response.completed": 0}, {"response.completed": True},
                       {"response.completed": "1"}, {"response.completed": 1, "error": 1},
                       {"response.completed": 1, "response.incomplete": 1}):
            row = self.raw_rows()[0]
            row["event_counts"] = counts
            with self.subTest(counts=counts):
                self.assertNotIn(score_row(row)["category"], ("correct", "wrong_answer"))

    def test_four_upstream_429_and_local_cooldown_not_scored(self):
        rows = []
        for index in range(5):
            row = self.raw_rows()[0]
            row.update(account_alias="Q2", sequence=index, http_status=429,
                       protocol_success=False, failure_category="http_error")
            if index == 4:
                row.update(http_status=503, failure_category="local_cooldown", gateway_attempt_count=1)
            rows.append(score_row(row))
        report = build_report(rows)
        self.assertEqual(report["summary"]["categories"], {"rate_limit": 4, "local_cooldown": 1})
        self.assertEqual(report["summary"]["scorable_completed"], 0)

    def test_candidate_changed_body_retained_as_descriptive_contrast(self):
        rows = load_run(self.fixture, {})
        report = arm_comparisons(rows, controlled_fields=["gateway_policy"])
        self.assertEqual(len(report["contrasts"]), 3)
        direct_transparent = next(row for row in report["contrasts"] if row["arms"] == ["direct", "transparent"])
        self.assertEqual(direct_transparent["comparability"], "same_request_and_predicted_body")
        self.assertTrue(direct_transparent["same_body_quality_contrast"])
        candidate_direct = next(row for row in report["contrasts"] if row["arms"] == ["candidate", "direct"])
        self.assertEqual(candidate_direct["comparability"], "predicted_upstream_body_changed")
        self.assertFalse(candidate_direct["same_body_quality_contrast"])
        self.assertTrue(candidate_direct["quality_outcomes_observed"])
        self.assertEqual(candidate_direct["declared_controlled_fields"], ["gateway_policy"])

    def test_changed_prompt_is_labeled_instead_of_silently_merged(self):
        rows = load_run(self.fixture, {})
        rows[-1]["prompt_sha256"] = "translated-prompt"
        comparisons = arm_comparisons(rows)["contrasts"]
        changed = [row for row in comparisons if "direct" in row["arms"]]
        self.assertTrue(all(row["comparability"] == "prompt_changed" for row in changed))
        self.assertFalse(any(row["same_body_quality_contrast"] for row in changed))

    def test_explicit_ab_pair_can_use_equal_live_request_and_predicted_body(self):
        rows = load_run(self.fixture, {})[1:]
        for arm, row in zip(("A", "B"), rows):
            row.update(arm=arm, pair_id="live-pair")
        report = paired_summary(rows)
        self.assertEqual(report["summary"]["quality_pairs"], 1)
        self.assertEqual(report["summary"]["B_regressed"], 1)

    def test_equal_client_hash_does_not_hide_changed_upstream_body_in_ab_pair(self):
        rows = load_run(self.fixture, {})[::2]
        for arm, row in zip(("A", "B"), rows):
            row.update(arm=arm, pair_id="live-pair")
        self.assertEqual(paired_summary(rows)["summary"]["quality_pairs"], 0)

    def test_retries_or_credential_change_do_not_qualify_as_same_body_quality(self):
        for field, value in (("attempts", 2), ("gateway_attempt_count", 2), ("credential_epoch", "different")):
            rows = load_run(self.fixture, {})
            rows[-1][field] = value
            comparison = next(row for row in arm_comparisons(rows)["contrasts"] if row["arms"] == ["direct", "transparent"])
            with self.subTest(field=field):
                self.assertFalse(comparison["same_body_quality_contrast"])

    def test_duplicate_arm_in_round_not_arbitrarily_paired(self):
        rows = load_run(self.fixture, {})
        rows.append(copy.deepcopy(rows[-1]))
        report = arm_comparisons(rows)
        self.assertEqual(len(report["excluded"]), 2)

    def test_distinct_runs_same_round_not_cross_paired(self):
        rows = load_run(self.fixture, {})
        rows[-1]["source_run"] = "next-day"
        self.assertEqual(len(arm_comparisons(rows)["contrasts"]), 1)

    def test_28_is_wrong_not_guaranteed_answer(self):
        row = self.raw_rows()[0]
        row["output_text"] = "最大无法满足的集合是28。\n最终答案：28个"
        result = score_row(row)
        self.assertEqual(result["category"], "wrong_answer")
        self.assertIn("28_equals_largest_blind_failing_set_not_guarantee", result["rationale_signals"])


class MathematicalOracleTests(unittest.TestCase):
    def test_fixed_shape_and_blind_interpretations_have_different_optima(self):
        from candy_strategy_verify import verify
        result = verify()
        self.assertEqual(result["minimum_guaranteed_draws"], 21)
        self.assertEqual(result["if_shape_selection_is_ignored"]["minimum_guaranteed_draws"], 29)
        self.assertEqual([(item["round"], item["star"]) for item in result["optimal_strategies"]], [(9, 12)])

    def test_adaptive_shape_selection_still_needs_21(self):
        from candy_strategy_verify import CAPACITIES, FIELDS, successful
        capacities = tuple(CAPACITIES[shape][flavor] for shape, flavor in FIELDS)

        @lru_cache(None)
        def worst_case_draws(taken):
            if successful(taken):
                return 0
            shape_choices = []
            for shape in (range(3), range(3, 6)):
                flavor_outcomes = []
                for index in shape:
                    if taken[index] < capacities[index]:
                        next_taken = list(taken)
                        next_taken[index] += 1
                        flavor_outcomes.append(worst_case_draws(tuple(next_taken)))
                if flavor_outcomes:
                    shape_choices.append(1 + max(flavor_outcomes))
            return min(shape_choices) if shape_choices else 0

        self.assertEqual(worst_case_draws((0,) * 6), 21)
        self.assertEqual(worst_case_draws.cache_info().currsize, 29340)


if __name__ == "__main__":
    unittest.main()
