"""The timing overlay must reject source drift and retain transaction behavior."""
import unittest
from build_ledger_experiment import REPO, instrument

class OverlayTests(unittest.TestCase):
    def test_only_adds_timing_statements(self):
        original = (REPO / 'actionhost/files/store.go').read_text(encoding='utf-8')
        patched = instrument(original)
        removed = '\n'.join(line for line in patched.splitlines() if not (
            'validationMutexStart :=' in line or 'validationConnectionStart :=' in line
            or 'validationDatabaseStart :=' in line or 'validationObserve(' in line
            or 'func init() { ledgerTimingOverlay' in line)).rstrip() + '\n'
        self.assertEqual(removed, original)
        with self.assertRaises(ValueError):
            instrument(original.replace('"BEGIN IMMEDIATE"', '"BEGIN"'))
        with self.assertRaises(ValueError):
            instrument(original + '\ts.mu.Lock()\n')

if __name__ == '__main__':
    unittest.main()
