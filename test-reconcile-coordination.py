import base64
import importlib.util
import unittest
from pathlib import Path

MODULE_PATH = Path(__file__).with_name('reconcile-coordination.py')
SPEC = importlib.util.spec_from_file_location('reconcile_coordination', MODULE_PATH)
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)


class HandoverHeadTests(unittest.TestCase):
    def encoded(self, text):
        return base64.b64encode(text.encode('utf-8')).decode('ascii')

    def test_agent_a_target_head_field(self):
        sha = 'a' * 40
        self.assertEqual(MODULE.extract_handover_head(self.encoded('Target PR head SHA observed: ' + sha)), sha)

    def test_agent_b_head_field_and_uppercase(self):
        sha = 'B' * 40
        self.assertEqual(MODULE.extract_handover_head(self.encoded('- Head: `' + sha + '`')), sha.lower())

    def test_short_sha_is_rejected(self):
        self.assertIsNone(MODULE.extract_handover_head(self.encoded('Target PR head SHA observed: abc123')))

    def test_missing_sha_is_rejected(self):
        self.assertIsNone(MODULE.extract_handover_head(self.encoded('# handover without a target SHA')))


if __name__ == '__main__':
    unittest.main()
