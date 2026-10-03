import ast
from pathlib import Path
import sys
import unittest


ROOT = Path(__file__).resolve().parents[1]
RUNNER = ROOT / '.local/research-20260925/exact-account-control/run_remote.py'
sys.path.insert(0, str(ROOT / 'tools'))
from quality_account_identity import require_distinct_accounts


def _row(identity, token, workspace):
    return {'identity_sha256': identity * 64, 'token_sha256': token * 64,
            'workspace_sha256': workspace * 64}


def _runner_preflight():
    tree = ast.parse(RUNNER.read_text(encoding='utf-8'))
    function = next(node for node in tree.body
                    if isinstance(node, ast.FunctionDef)
                    and node.name == 'preflight_selected_epochs')
    namespace = {'require_distinct_accounts': require_distinct_accounts}
    module = ast.Module(body=[function], type_ignores=[])
    exec(compile(ast.fix_missing_locations(module), str(RUNNER), 'exec'), namespace)
    return namespace['preflight_selected_epochs']


class ExactAccountPreflightTests(unittest.TestCase):
    def test_runner_preflight_rejects_folded_identity_before_side_effects(self):
        preflight = _runner_preflight()
        duplicate = {'q0': _row('a', 'b', 'c'), 'q5': _row('a', 'd', 'e')}
        events = {'credential_writes': 0, 'dispatches': 0}
        if preflight(duplicate):
            events['credential_writes'] += 1
            events['dispatches'] += 1
        self.assertEqual(events, {'credential_writes': 0, 'dispatches': 0})

    def test_runner_preflight_allows_capture_only_single_and_rejects_empty(self):
        preflight = _runner_preflight()
        self.assertTrue(preflight({'q0': _row('a', 'b', 'c')}, capture_only=True))
        self.assertFalse(preflight({}, capture_only=True))

    def test_preflight_call_is_before_credential_write_and_model_process_in_runner_ast(self):
        tree = ast.parse(RUNNER.read_text(encoding='utf-8'))
        run_function = next(node for node in tree.body
                            if isinstance(node, ast.FunctionDef) and node.name == 'run')
        calls = list(ast.walk(run_function))
        guard_lines = [node.lineno for node in calls
                       if isinstance(node, ast.Call)
                       and isinstance(node.func, ast.Name)
                       and node.func.id == 'preflight_selected_epochs']
        self.assertEqual(len(guard_lines), 1)
        materialize_lines = [node.lineno for node in calls
                             if isinstance(node, ast.Call)
                             and isinstance(node.func, ast.Name)
                             and node.func.id == 'save'
                             and any(isinstance(child, ast.Constant)
                                     and isinstance(child.value, str)
                                     and 'access_token' in child.value
                                     for child in ast.walk(node))]
        dispatch_lines = [node.lineno for node in calls
                          if isinstance(node, ast.Call)
                          and isinstance(node.func, ast.Attribute)
                          and isinstance(node.func.value, ast.Name)
                          and node.func.value.id == 'subprocess'
                          and node.func.attr == 'run']
        self.assertTrue(materialize_lines and dispatch_lines)
        self.assertLess(guard_lines[0], min(materialize_lines))
        self.assertLess(guard_lines[0], min(dispatch_lines))
        self.assertIn('credentials_removed', RUNNER.read_text(encoding='utf-8'))
        self.assertIn('automatic_retries', RUNNER.read_text(encoding='utf-8'))

    def test_runner_keeps_identity_tuple_fields_explicit(self):
        tree = ast.parse(RUNNER.read_text(encoding='utf-8'))
        source = RUNNER.read_text(encoding='utf-8')
        identity_source = (ROOT / 'tools/quality_account_identity.py').read_text(encoding='utf-8')
        for field in ('identity_sha256', 'token_sha256', 'workspace_sha256'):
            self.assertIn(field, identity_source)
        self.assertIn('require_distinct_accounts', source)
        self.assertTrue(any(isinstance(node, ast.FunctionDef)
                            and node.name == 'preflight_selected_epochs'
                            for node in tree.body))
        self.assertIsInstance(tree, ast.Module)


if __name__ == '__main__':
    unittest.main()
