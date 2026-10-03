"""The observed BPS route quota is an exact protocol code, never free text."""
from copy import deepcopy
import unittest

from codex_agent_live_monitor import classify_error


CODE = "basispoints_route_rate_limit_exceeded"


class BPSRouteQuotaMonitorTests(unittest.TestCase):
    def test_exact_protocol_sources_and_canonical_alias(self):
        for code in (CODE, "rateLimitExceeded"):
            for evidence in (
                {"final_upstream_error_code": code, "final_upstream_error_source": "upstream"},
                {"error_code": code},
                {"attempts": [{"phase": "websocket", "status_code": 429,
                               "observed_http_status_code": 101, "error_code": code,
                               "error_source": "upstream"}]},
                {"error": "bps response terminated with response.failed (status failed) (code=" + code + ")"},
            ):
                row = {"status": "error", "http_code": 200, "error_source": "upstream", **evidence}
                before = deepcopy(row)
                with self.subTest(code=code, evidence=evidence):
                    self.assertEqual(classify_error(row), "upstream_rate_limit")
                    self.assertEqual(row, before)

    def test_near_match_and_plain_message_are_not_protocol_evidence(self):
        for code in (CODE + "_other", CODE.upper(), "prefix_" + CODE, CODE[:-1]):
            for evidence in ({"error_code": code},
                             {"final_upstream_error_code": code, "final_upstream_error_source": "upstream"},
                             {"attempts": [{"error_code": code}]},
                             {"error": "bps response terminated with error (status failed) (code=" + code + ")"}):
                with self.subTest(code=code, evidence=evidence):
                    self.assertEqual(classify_error({"status": "error", "error_source": "upstream", **evidence}), "other_error")
        for text in (CODE, "provider mentioned " + CODE, "Rate limit exceeded", "code=" + CODE):
            self.assertEqual(classify_error({"error_source": "upstream", "error": text}), "other_error")

    def test_local_final_or_aborted_request_never_uses_prior_quota(self):
        evidence = {"final_upstream_error_code": CODE, "final_upstream_error_source": "upstream",
                    "error_code": CODE, "attempts": [{"error_code": CODE, "error_source": "upstream"}],
                    "error": "bps response terminated with error (status failed) (code=" + CODE + ")"}
        for source in ("gateway", "client", "transport", "storage"):
            with self.subTest(source=source):
                self.assertEqual(classify_error({"status": "error", "error_source": source, **evidence}), "other_error")
        for source in ("upstream", "client", None):
            self.assertEqual(classify_error({"status": "aborted", "error_source": source, **evidence}), "other_error")
        self.assertEqual(classify_error({"status": "error", "error_source": "gateway",
                                        **evidence, "error": "session account changed during authentication failover"}), "account_selection")

    def test_truncated_or_prior_attempt_is_not_final_quota(self):
        self.assertEqual(classify_error({"status": "error", "error_source": "upstream",
                                        "attempts": [{"error_code": CODE}], "attempts_truncated": True}), "other_error")
        self.assertEqual(classify_error({"status": "error", "error_source": "upstream",
                                        "attempts": [{"error_code": CODE}, {"error_code": "other"}]}), "other_error")
        self.assertEqual(classify_error({"status": "error", "error_source": "upstream",
                                        "final_upstream_error_source": "upstream", "final_upstream_error_code": CODE,
                                        "attempts": [{"error_code": "other"}], "attempts_truncated": True}), "upstream_rate_limit")


if __name__ == "__main__":
    unittest.main()
