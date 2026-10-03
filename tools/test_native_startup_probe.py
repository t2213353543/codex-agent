import unittest
from unittest import mock

import native_startup_probe as probe


class StartupCleanupTests(unittest.TestCase):
    def test_running_owned_probe_is_removed_on_exception_cleanup(self):
        observed = {"Id": "fixture-id", "State": {"Running": True}, "Config": {"Labels": {"label": "run"}}}
        with mock.patch.object(probe.runner, "docker_json", return_value=[observed]), mock.patch.object(probe.runner, "docker") as docker:
            probe.cleanup_owned_container("fixture-id", "label", "run")
            docker.assert_called_once_with("rm", "-f", "fixture-id")

    def test_wrong_identity_or_label_is_not_removed(self):
        for observed in [
            {"Id": "different-id", "Config": {"Labels": {"label": "run"}}},
            {"Id": "fixture-id", "Config": {"Labels": {"label": "another-run"}}},
        ]:
            with mock.patch.object(probe.runner, "docker_json", return_value=[observed]), mock.patch.object(probe.runner, "docker") as docker:
                with self.assertRaisesRegex(RuntimeError, "cleanup_identity_mismatch"):
                    probe.cleanup_owned_container("fixture-id", "label", "run")
                docker.assert_not_called()


if __name__ == "__main__":
    unittest.main()
