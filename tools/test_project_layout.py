import tempfile
from pathlib import Path
import unittest

from project_layout import backend_source_paths, migration_runner_key, package_overlay, server_package, valid_backend_source_path


class ProjectLayoutTests(unittest.TestCase):
    def test_manifest_includes_nested_packages_and_embedded_sql_only(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            sources = {"main.go", "internal/server/gateway.go", "internal/server/gateway_test.go",
                       "cmd/probe/main.go", "native-transport/tests/probe.go", "migrations/embed.go", "migrations/000_core.sql", "go.mod", "go.sum",
                       "internal/server/account_window_prices.json"}
            excluded = {"data/accounts.json", ".local/secret.go", "internal/server/.local/private.go", "frontend/main.go", "dist/test.go", "internal/server/private.json"}
            for name in sources | excluded:
                path = root / name
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text("fixture", encoding="utf-8")
            self.assertEqual({p.relative_to(root).as_posix() for p in backend_source_paths(root)}, sources)
            self.assertEqual(server_package(root), "./internal/server")

    def test_legacy_layout_and_migration_identity(self):
        with tempfile.TemporaryDirectory() as tmp:
            self.assertEqual(server_package(Path(tmp)), ".")
        self.assertEqual(migration_runner_key({"database.go": "x"}), "database.go")
        self.assertEqual(migration_runner_key({"internal/server/database.go": "x"}), "internal/server/database.go")
        with self.assertRaises(ValueError):
            migration_runner_key({"database.go": "x", "internal/server/database.go": "x"})

    def test_manifest_paths_reject_escape_and_private_sources(self):
        for name in ["internal/server/gateway.go", "cmd/probe/main.go", "migrations/embed.go", "main.go", "internal/server/account_window_prices.json"]:
            self.assertTrue(valid_backend_source_path(name), name)
        for name in ["../secret.go", "internal/../../secret.go", "C:/secret.go", "internal\\server.go", ".local/private.go", "data/token.go", "internal//server.go", "internal/server/private.json"]:
            self.assertFalse(valid_backend_source_path(name), name)

    def test_overlay_changes_only_package_for_target_snapshot(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "internal/server").mkdir(parents=True)
            source = root / "fixture.go.txt"
            source.write_text("package main\n\n// unchanged\n", encoding="utf-8")
            output = package_overlay(source, root / "generated.go.txt", root)
            self.assertEqual(output.read_text(encoding="utf-8"), "package server\n\n// unchanged\n")
            self.assertEqual(source.read_text(encoding="utf-8"), "package main\n\n// unchanged\n")


if __name__ == "__main__":
    unittest.main()
