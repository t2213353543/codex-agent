"""Contract checks for actual synthetic writer captures and source-only labels."""
import json
from pathlib import Path
import unittest


class ConditionalVectorsTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.document = json.loads((Path(__file__).resolve().parents[1] / "integration/appserver_parity/conditional_vectors.json").read_text(encoding="utf-8"))
        cls.rows = {item["name"]: item for item in cls.document["vectors"]}

    def test_both_official_frontends_capture_each_conditional_writer(self):
        for route in ("app", "exec"):
            for endpoint in ("images-generations", "images-edits", "guardian", "guardian-classifier"):
                row = self.rows[route + "-" + endpoint]
                self.assertEqual(row["evidence"], "official_binary_loopback_capture")
                self.assertEqual(row["network_namespace"], "isolated")
                self.assertFalse(row["real_upstream"])
                self.assertEqual(row["headers"]["authorization"], "Bearer SYNTHETIC_FIXTURE_ACCESS_TOKEN")

    def test_image_writers_are_json_and_agree_across_frontends(self):
        for endpoint in ("images-generations", "images-edits"):
            left, right = [self.rows[route + "-" + endpoint] for route in ("app", "exec")]
            self.assertEqual(left["body"], right["body"])
            self.assertEqual(left["headers"]["content-type"], "application/json")
            self.assertEqual(left["body"]["model"], "gpt-image-2")
            self.assertEqual(left["body"]["prompt"], "SYNTHETIC_PROVIDER_IMAGE_MARKER")
            self.assertIn("x-codex-image-turn-id", left["headers"])
        self.assertTrue(self.rows["app-images-edits"]["body"]["images"][0]["image_url"].startswith("data:image/png;base64,"))

    def test_guardian_has_distinct_model_schema_and_transport(self):
        for route in ("app", "exec"):
            classifier = self.rows[route + "-guardian-classifier"]
            review = self.rows[route + "-guardian"]
            self.assertEqual(classifier["transport"], "websocket_response_create")
            self.assertEqual(classifier["body"]["model"], "gpt-5.6-luna")
            self.assertEqual(classifier["body"]["type"], "response.create")
            self.assertEqual(classifier["headers"]["x-openai-subagent"], "guardian")
            self.assertIn("parent_response_id", classifier["body"]["client_metadata"])
            self.assertEqual(review["transport"], "http_json")
            self.assertEqual(review["body"]["model"], "codex-auto-review")
            self.assertEqual(review["body"]["text"]["format"]["schema"]["required"], ["outcome"])

    def test_memory_vector_never_claims_runtime_execution(self):
        row = self.rows["source-only-memories-trace-summarize"]
        self.assertFalse(row["runtime_capture"])
        self.assertFalse(row["source_unit_test_executed"])
        self.assertEqual(row["body"]["traces"][0]["metadata"]["source_path"], "/fixture/trace.json")
        self.assertNotIn("raw_memories", row["body"])
        self.assertFalse(self.document["claims"]["all_operating_system_egress_covered"])


if __name__ == "__main__":
    unittest.main()
