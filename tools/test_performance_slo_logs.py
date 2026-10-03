import base64
import gzip
import unittest
from unittest.mock import patch

import performance_slo


class ContainerLogTransportTests(unittest.TestCase):
    def test_preserves_entire_utf8_evidence(self):
        raw = ('EXPLAIN ' + 'synthetic-array-element,' * 10000 + '\n中文证据\n').encode()
        encoded = base64.encodebytes(gzip.compress(raw, compresslevel=1)).decode()
        with patch.object(performance_slo, 'run', return_value=encoded) as run:
            self.assertEqual(performance_slo.read_container_logs(object(), 'a' * 64), raw.decode())
        command = run.call_args.args[1]
        self.assertIn('pipefail', command)
        self.assertIn('docker logs ' + 'a' * 64, command)
        self.assertIn('gzip -1', command)

    def test_rejects_non_container_argument(self):
        with patch.object(performance_slo, 'run') as run:
            with self.assertRaises(ValueError):
                performance_slo.read_container_logs(object(), 'not-an-id; echo bad')
            run.assert_not_called()

    def test_preserves_remote_failure(self):
        with patch.object(performance_slo, 'run', side_effect=RuntimeError('docker log read failed')):
            with self.assertRaisesRegex(RuntimeError, 'docker log read failed'):
                performance_slo.read_container_logs(object(), 'b' * 64)


if __name__ == '__main__':
    unittest.main()
