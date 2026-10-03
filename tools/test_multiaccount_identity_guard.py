import unittest
from pathlib import Path
import sys


ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "tools"))
from quality_account_identity import require_distinct_accounts


def row(identity="a", token="b", workspace="c"):
    return {
        "identity_sha256": identity * 64,
        "token_sha256": token * 64,
        "workspace_sha256": workspace * 64,
    }


class MultiAccountIdentityGuardTests(unittest.TestCase):
    def test_same_identity_with_different_token_is_rejected(self):
        self.assertFalse(require_distinct_accounts([
            row("a", "b", "c"), row("a", "d", "e")
        ]))

    def test_same_token_with_different_identity_is_rejected(self):
        self.assertFalse(require_distinct_accounts([
            row("a", "b", "c"), row("d", "b", "e")
        ]))

    def test_distinct_users_may_share_workspace(self):
        self.assertTrue(require_distinct_accounts([
            row("a", "b", "c"), row("d", "e", "c")
        ]))

    def test_missing_or_wrong_type_is_rejected(self):
        missing = row("a", "b", "c")
        del missing["token_sha256"]
        self.assertFalse(require_distinct_accounts([missing, row("d", "e", "f")]))
        wrong_type = row("a", "b", "c")
        wrong_type["identity_sha256"] = 1
        self.assertFalse(require_distinct_accounts([wrong_type, row("d", "e", "f")]))

    def test_invalid_hash_is_rejected(self):
        malformed = row("a", "b", "c")
        malformed["workspace_sha256"] = "not-a-sha256"
        self.assertFalse(require_distinct_accounts([malformed, row("d", "e", "f")]))

    def test_empty_input_is_rejected_even_for_capture_only(self):
        self.assertFalse(require_distinct_accounts([], allow_single=True))

    def test_single_capture_only_row_is_explicitly_allowed(self):
        self.assertFalse(require_distinct_accounts([row()]))
        self.assertTrue(require_distinct_accounts([row()], allow_single=True))


if __name__ == '__main__':
    unittest.main()
