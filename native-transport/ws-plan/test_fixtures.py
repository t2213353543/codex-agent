import struct
import unittest

import fixtures as f


class FixtureContractTests(unittest.TestCase):
    def test_all_proposed_sequences_are_self_consistent(self):
        for case in f.fixture_cases():
            with self.subTest(case=case["name"]):
                self.assertFalse(case["runtime_verified"])
                self.assertFalse(f.trace_valid(case["steps"])["replay_allowed"])

    def test_frame_truncation_cannot_be_clean_end(self):
        raw = f.pack("HANDSHAKE", f.handshake())
        for length in (0, 1, 4, 5, len(raw) - 1):
            with self.assertRaises(ValueError):
                f.unpack(raw[:length])

    def test_huge_length_rejected_without_allocating_body(self):
        with self.assertRaisesRegex(ValueError, "frame_too_large"):
            f.unpack(struct.pack("!BI", 66, 0xFFFFFFFF))

    def test_unknown_opcode_and_trailing_bytes_rejected(self):
        for raw in (struct.pack("!BI", 255, 0), f.pack("END") + b"extra"):
            with self.assertRaises(ValueError):
                f.unpack(raw)

    def test_text_requires_utf8_binary_does_not(self):
        with self.assertRaises(UnicodeDecodeError):
            f.message(1, 1, b"\xff")
        self.assertEqual(f.message(1, 2, b"\xff")[-1:], b"\xff")

    def test_no_messages_before_accepted_handshake(self):
        with self.assertRaisesRegex(ValueError, "message_state"):
            f.trace_valid([f.step("SEND", f.message(1, 1, b"{}")), f.step("END")])

    def test_rejected_handshake_cannot_accept_a_send(self):
        with self.assertRaisesRegex(ValueError, "message_state"):
            f.trace_valid([f.step("HANDSHAKE", f.handshake(401)), f.step("SEND", f.message(1, 1, b"{}")), f.step("END")])

    def test_accepted_send_without_ack_does_not_allow_replay(self):
        steps = [f.step("HANDSHAKE", f.handshake()), f.step("SEND", f.message(1, 1, b"{}")), f.step("ERROR", {"phase": "write", "code": "lost", "accepted": True}), f.step("END")]
        result = f.trace_valid(steps)
        self.assertEqual(result["unacknowledged_sends"], 1)
        self.assertFalse(result["replay_allowed"])

    def test_repeated_send_sequence_rejected(self):
        send = f.step("SEND", f.message(1, 1, b"{}"))
        with self.assertRaisesRegex(ValueError, "replayed_send_sequence"):
            f.trace_valid([f.step("HANDSHAKE", f.handshake()), send, send])

    def test_no_completed_body_claim_for_handshake_tail(self):
        body = f.handshake(429, b"partial")
        body["body_complete"] = True
        with self.assertRaisesRegex(ValueError, "invented_complete_body"):
            f.trace_valid([f.step("HANDSHAKE", body), f.step("END")])

    def test_101_alone_not_clean_application_session(self):
        with self.assertRaisesRegex(ValueError, "end_without_terminal_transport_state"):
            f.trace_valid([f.step("HANDSHAKE", f.handshake()), f.step("END")])

    def test_pinned_source_contract(self):
        result = f.source_checks()
        self.assertTrue(result["passed"])
        self.assertFalse(result["runtime_verified"])

    def test_metadata_and_tail_limits_explicit(self):
        with self.assertRaisesRegex(ValueError, "metadata_too_large"):
            f.pack("HANDSHAKE", b"x" * (f.PROTOCOL["max_metadata_bytes"] + 1))
        body = f.handshake(401, b"x" * (f.PROTOCOL["max_handshake_tail_bytes"] + 1))
        self.assertTrue(body["tail_truncated"])
        self.assertIsNone(body["body_complete"])
        f.trace_valid([f.step("HANDSHAKE", body), f.step("END")])

    def test_reserved_synthetic_close_codes_not_transmitted(self):
        for code in (1004, 1005, 1006, 1015):
            with self.subTest(code=code), self.assertRaisesRegex(ValueError, "invalid_close_code"):
                f.trace_valid([f.step("HANDSHAKE", f.handshake()), f.step("SEND_CLOSE", {"code": code, "reason": ""}), f.step("END")])


if __name__ == "__main__":
    unittest.main()
