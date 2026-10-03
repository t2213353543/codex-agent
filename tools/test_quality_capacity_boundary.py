"""Capacity diagnostics only count an actual CLI error event."""
import unittest
import quality_failure_summary as summary
import quality_official_cli_probe as probe

MESSAGE = 'Selected model is at capacity. Please try a different model.'

class CapacityBoundary(unittest.TestCase):
    def test_actual_error_is_capacity_without_inventing_http_status(self):
        value = summary.summarize_failure({'type': 'error', 'error': {'message_excerpt': MESSAGE}})
        self.assertEqual(value['category'], 'upstream_unavailable')
        self.assertEqual(value['http_statuses'], [])
        self.assertEqual(probe.safe_error_details(MESSAGE, [])['classification'], 'model_capacity')

    def test_answer_or_usage_cannot_claim_capacity(self):
        for value in ({'output_text': MESSAGE}, {'usage': {'reasoning_tokens': 429}},
                      {'type': 'item.completed', 'item_type': 'agent_message', 'text': MESSAGE}):
            self.assertEqual(summary.summarize_failure(value)['category'], 'unknown')

if __name__ == '__main__':
    unittest.main()
