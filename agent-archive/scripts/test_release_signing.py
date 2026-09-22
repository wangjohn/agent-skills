"""The release gate must reject each absent secret without exposing values."""
import os
from pathlib import Path
import subprocess
import unittest


class ReleaseSigningTest(unittest.TestCase):
    def test_required_configuration(self):
        names = (
            'APPLE_CERTIFICATE_P12_BASE64 APPLE_CERTIFICATE_PASSWORD '
            'APPLE_SIGNING_IDENTITY APPLE_ID APPLE_TEAM_ID '
            'APPLE_APP_SPECIFIC_PASSWORD'
        ).split()
        env = dict(os.environ, APPLE_SIGNING_ENABLED='true',
                   **dict.fromkeys(names, 'synthetic-secret'))
        command = ['bash', str(Path(__file__).with_name('check-release-signing.sh'))]
        self.assertEqual(subprocess.run(command, env=env, capture_output=True).returncode, 0)
        for name in ['APPLE_SIGNING_ENABLED'] + names:
            with self.subTest(missing=name):
                missing = env.copy()
                missing.pop(name)
                result = subprocess.run(command, env=missing, capture_output=True)
                self.assertNotEqual(result.returncode, 0)
                self.assertNotIn(b'synthetic-secret', result.stdout + result.stderr)


if __name__ == '__main__':
    unittest.main()
