import copy
import unittest

from quality_account_control import controlled_cell


class AccountControlTests(unittest.TestCase):
    def test_unknown_false_and_nonboolean_never_dispatch(self):
        for value in (None, False, 0, 1, "true"):
            with self.subTest(value=value):
                events = []
                def dispatch():
                    self.fail("non-true eligibility invoked model")
                result = controlled_cell(lambda: {"eligibility": {"allowed": value}}, dispatch,
                                         lambda row: events.append(copy.deepcopy(row)))
                self.assertEqual(result["status"], "skipped")
                self.assertEqual(result["usage_observations_attempted"], 1)
                self.assertFalse(result["model_dispatch_attempted"])

    def test_before_model_after_order_and_later_depletion_retains_result(self):
        order, saved = [], []
        def usage():
            order.append("usage")
            return {"eligibility": {"allowed": len(order) == 1}}
        def dispatch():
            self.assertEqual(saved[0]["usage_before"]["eligibility"]["allowed"], True)
            order.append("model")
            return 0
        result = controlled_cell(usage, dispatch, lambda row: saved.append(copy.deepcopy(row)))
        self.assertEqual(order, ["usage", "model", "usage"])
        self.assertEqual(result["model_process_returncode"], 0)
        self.assertFalse(result["usage_after"]["eligibility"]["allowed"])
        self.assertEqual(result["usage_observations_attempted"], 2)

    def test_dispatch_timeout_still_observes_after_without_retry(self):
        calls = []
        def usage():
            calls.append("usage")
            return {"eligibility": {"allowed": True}}
        def dispatch():
            calls.append("model")
            raise TimeoutError("private error text")
        result = controlled_cell(usage, dispatch, lambda _: None)
        self.assertEqual(calls, ["usage", "model", "usage"])
        self.assertEqual(result["model_error_class"], "TimeoutError")
        self.assertNotIn("private", str(result))

    def test_after_error_does_not_erase_model_process_failure(self):
        count = 0
        def usage():
            nonlocal count
            count += 1
            if count == 2:
                raise OSError("private endpoint")
            return {"eligibility": {"allowed": True}}
        result = controlled_cell(usage, lambda: 9, lambda _: None)
        self.assertEqual(result["model_process_returncode"], 9)
        self.assertIsNone(result["usage_after"]["eligibility"]["allowed"])
        self.assertEqual(result["usage_after"]["error_class"], "OSError")
        self.assertNotIn("private", str(result))

    def test_before_failure_and_persistence_failure_do_not_send_model(self):
        def dispatch():
            self.fail("invalid preflight invoked model")
        def broken():
            raise OSError("private")
        result = controlled_cell(broken, dispatch, lambda _: None)
        self.assertEqual(result["status"], "skipped")
        def no_disk(_):
            raise OSError("disk unavailable")
        with self.assertRaises(OSError):
            controlled_cell(lambda: {"eligibility": {"allowed": True}}, dispatch, no_disk)


if __name__ == "__main__":
    unittest.main()
