"""Make targets the release gate and check-fast run, against disposable trees."""
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]


def browser_engines():
    """Every browser engine the browser suite launches: the configuration's
    Chromium and each engine a spec file asks for."""
    specs = (ROOT / 'scripts/browser').glob('*.spec.mjs')
    return {'chromium'} | {name for spec in specs for name in re.findall(r"browserName:\s*'(\w+)'", spec.read_text())}


@unittest.skipUnless(shutil.which('make') and shutil.which('gofmt'), 'make and gofmt required')
class FormatTests(unittest.TestCase):
    def test_format_covers_every_go_directory_including_scripts(self):
        for directory in ('cmd/app', 'internal/store', 'plugins', 'scripts/mcp-smoke'):
            with self.subTest(directory=directory), tempfile.TemporaryDirectory() as temporary:
                tree = Path(temporary)
                shutil.copyfile(ROOT / 'Makefile', tree / 'Makefile')
                for name in ('cmd', 'internal', 'plugins', 'scripts'):
                    (tree / name).mkdir()
                (tree / directory).mkdir(parents=True, exist_ok=True)
                source = tree / directory / 'main.go'
                source.write_text('package main\n\nfunc main() {}\n')
                self.assertEqual(subprocess.run(['make', '-s', 'format'], cwd=tree, capture_output=True).returncode, 0)
                source.write_text('package main\n\nfunc main()   {}\n')
                self.assertNotEqual(subprocess.run(['make', '-s', 'format'], cwd=tree, capture_output=True).returncode, 0)


@unittest.skipUnless(shutil.which('make'), 'make required')
class OperatorTestTargetTests(unittest.TestCase):
    def test_host_python_tests_include_the_github_app_manifest(self):
        with tempfile.TemporaryDirectory() as temporary:
            tree = Path(temporary)
            shutil.copyfile(ROOT / 'Makefile', tree / 'Makefile')
            passing = 'import unittest\nclass T(unittest.TestCase):\n    def test_ok(self):\n        pass\n'
            for name in ('deploy/standalone/test_operator.py', 'deploy/production/test_release.py'):
                (tree / name).parent.mkdir(parents=True, exist_ok=True)
                (tree / name).write_text(passing)
            manifest = tree / 'scripts/test_github_app_manifest.py'
            manifest.parent.mkdir()
            run = lambda: subprocess.run(['make', '-s', 'test-python-host', 'PYTHON=' + sys.executable],
                                         cwd=tree, capture_output=True, text=True)
            manifest.write_text(passing)
            self.assertEqual(run().returncode, 0)
            manifest.write_text(passing.replace('pass', 'self.fail("manifest regression")'))
            result = run()
            self.assertNotEqual(result.returncode, 0)
            self.assertIn('manifest regression', result.stderr)


@unittest.skipUnless(shutil.which('make'), 'make required')
class BrowserSetupTests(unittest.TestCase):
    def test_setup_installs_every_browser_engine_the_suite_uses(self):
        # GitHub's full check runs make setup-browser. A spec in an engine it
        # did not install fails to launch: "Executable doesn't exist".
        with tempfile.TemporaryDirectory() as temporary:
            shutil.copyfile(ROOT / 'Makefile', Path(temporary) / 'Makefile')
            result = subprocess.run(['make', '--no-print-directory', '-n', 'setup-browser'], cwd=temporary, capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        install = result.stdout.split()
        self.assertEqual(install[:5], ['npx', '--no-install', 'playwright', 'install', '--with-deps'])
        self.assertEqual(set(install[5:]), browser_engines())


if __name__ == '__main__':
    unittest.main()
