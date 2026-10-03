"""Final gateway capacity and structured failures use metadata, never prose."""
from copy import deepcopy
import unittest

from codex_agent_live_monitor import classify_error


class LocalFailureMonitorTests(unittest.TestCase):
    def test_capacity_keeps_final_gateway_provenance(self):
        for status in (429, "429"):
            row = {"status": "error", "error_source": "gateway", "error_phase": "account_selection",
                   "http_code": status, "error": "unrelated text", "final_upstream_error_source": "upstream",
                   "final_upstream_error_code": "rateLimitExceeded",
                   "attempts": [{"phase": "request", "observed_http_status_code": 429, "error_code": "rateLimitExceeded"}]}
            before = deepcopy(row)
            self.assertEqual(classify_error(row), "gateway_capacity")
            self.assertEqual(row, before)

    def test_capacity_requires_exact_source_phase_status_and_http_code(self):
        base = {"status": "error", "error_source": "gateway", "error_phase": "account_selection", "http_code": 429}
        for key, values in {"status": (None, "ok", "aborted", "ERROR"),
                            "error_source": (None, "upstream", "client", "Gateway"),
                            "error_phase": (None, "response_headers", "account_selection_extra"),
                            "http_code": (None, 503, 200, 429.0, True, "429 ")}.items():
            for value in values:
                with self.subTest(key=key, value=value):
                    self.assertNotEqual(classify_error({**base, key: value}), "gateway_capacity")
            missing = dict(base)
            del missing[key]
            self.assertNotEqual(classify_error(missing), "gateway_capacity")
        self.assertEqual(classify_error({"error": "gateway_capacity"}), "other_error")

    def test_provider_quota_remains_upstream(self):
        row = {"status": "error", "error_source": "upstream", "error_phase": "response_headers", "http_code": 429,
               "final_upstream_error_source": "upstream", "final_upstream_error_code": "rateLimitExceeded"}
        self.assertEqual(classify_error(row), "upstream_rate_limit")

    def test_structured_existing_conflict_shapes(self):
        neutral = {"formatter_count": 1, "client_call_count": 0, "other_call_count": 0, "has_text": False, "has_refusal": False}
        for conflict in ({"formatter_count": 2}, {"client_call_count": 1}, {"other_call_count": 1}, {"has_text": True}, {"has_refusal": True}):
            for http_code in (200, "502"):
                row = {"status": "error", "error_source": "gateway", "error_phase": "response_validation", "http_code": http_code,
                       "structured_mixed": {**neutral, **conflict}, "error": "unrelated text"}
                before = deepcopy(row)
                self.assertEqual(classify_error(row), "structured_validation")
                self.assertEqual(row, before)

    def test_structured_requires_complete_valid_metadata(self):
        mixed = {"formatter_count": 1, "client_call_count": 0, "other_call_count": 1, "has_text": False, "has_refusal": False}
        base = {"status": "error", "error_source": "gateway", "error_phase": "response_validation", "http_code": 200}
        for key in mixed:
            missing = dict(mixed)
            del missing[key]
            self.assertNotEqual(classify_error({**base, "structured_mixed": missing}), "structured_validation")
        for invalid in ({"formatter_count": True}, {"formatter_count": -1}, {"other_call_count": "1"},
                        {"other_call_count": 9223372036854775808}, {"has_text": 1}, {"has_refusal": "false"}):
            self.assertNotEqual(classify_error({**base, "structured_mixed": {**mixed, **invalid}}), "structured_validation")
        for value in (None, [], "structured_validation", {}, {**mixed, "formatter_count": 0}, {**mixed, "other_call_count": 0}):
            self.assertNotEqual(classify_error({**base, "structured_mixed": value}), "structured_validation")
        self.assertEqual(classify_error({**base, "error": "structured output mixed incompatible final outputs"}), "other_error")

    def test_structured_requires_final_local_error_provenance(self):
        base = {"status": "error", "error_source": "gateway", "error_phase": "response_validation", "http_code": 200,
                "structured_mixed": {"formatter_count": 1, "client_call_count": 0, "other_call_count": 0, "has_text": True, "has_refusal": False}}
        for key, values in {"status": (None, "ok", "aborted"), "error_source": (None, "client", "upstream"),
                            "error_phase": (None, "request_validation", "response_validation_extra"), "http_code": (None, 429, 503)}.items():
            for value in values:
                self.assertNotEqual(classify_error({**base, key: value}), "structured_validation")
        self.assertEqual(classify_error({**base, "final_upstream_error_source": "upstream", "final_upstream_error_code": "rateLimitExceeded"}), "structured_validation")

    def test_client_parameter_keeps_existing_exact_allowlist(self):
        base = {"status": "error", "error_source": "client", "error_phase": "request_validation", "http_code": 400}
        self.assertEqual(classify_error({**base, "error_param": "top_p"}), "request_parameter")
        for value in (None, "TOP_P", "top_p.extra", " top_p", "inline_image"):
            self.assertEqual(classify_error({**base, "error_param": value}), "other_error")
        self.assertEqual(classify_error({**base, "error": "invalid inline image: expected valid base64 PNG, JPEG, GIF or WebP data"}), "other_error")


if __name__ == "__main__":
    unittest.main()
