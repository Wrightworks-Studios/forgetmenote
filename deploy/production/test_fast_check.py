"""Affected PR selection uses real Go inventories and real Git changes."""
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest
import fast_check
from verification import go_package_inputs
from verification_plan import CRITICAL_WEB, affected_go_packages


class FastOrderTest(unittest.TestCase):
    def test_longest_typical_packages_start_first(self):
        # In package order internal/workspace started last and ran alone for the
        # last five minutes of an all-package run.
        packages = ['forgetmenote/cmd/forgetmenote', 'forgetmenote/internal/automation', 'forgetmenote/internal/chat',
                    'forgetmenote/internal/notestore', 'forgetmenote/internal/workspace']
        planned = fast_check.commands(Path.cwd(), {'packages': packages})
        self.assertEqual([command[-1] for command in planned], [
            'forgetmenote/internal/workspace', 'forgetmenote/internal/chat', 'forgetmenote/internal/automation',
            'forgetmenote/cmd/forgetmenote', 'forgetmenote/internal/notestore'])

    def test_every_typical_time_names_a_current_package(self):
        # A renamed package would silently lose its place at the front.
        root = Path(__file__).resolve().parents[2]
        for package in fast_check.TYPICAL_SECONDS:
            with self.subTest(package):
                self.assertTrue(package.startswith('forgetmenote/'), package)
                self.assertTrue(list((root / package.removeprefix('forgetmenote/')).glob('*.go')), package)


@unittest.skipUnless(shutil.which('go'), 'Go toolchain required')
class FastPlanTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.git('init', '-b', 'main')
        self.git('config', 'user.name', 'Synthetic CI')
        self.git('config', 'user.email', 'ci@example.test')
        self.write('go.mod', 'module ci.example/notes\n\ngo 1.24\n')
        self.write('leaf/leaf.go', 'package leaf\nfunc Value() int { return 1 }\n')
        self.write('reader/reader.go', 'package reader\nimport "ci.example/notes/leaf"\nfunc Value() int { return leaf.Value() }\n')
        self.write('other/other.go', 'package other\nfunc Value() int { return 2 }\n')
        self.git('add', '.')
        self.git('commit', '-m', 'Baseline')
        self.base = self.git('rev-parse', 'HEAD')

    def git(self, *args):
        return subprocess.check_output(['git', *args], cwd=self.root, text=True, stderr=subprocess.PIPE).strip()

    def write(self, name, text):
        path = self.root / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(text)

    def commit(self):
        self.git('add', '-A')
        self.git('commit', '-m', 'Candidate')

    def test_selects_importers_and_leaves_unrelated_package(self):
        self.write('leaf/leaf.go', 'package leaf\nfunc Value() int { return 3 }\n')
        self.commit()
        self.assertEqual(fast_check.plan(self.root, self.base)['packages'], ['ci.example/notes/leaf', 'ci.example/notes/reader'])

    def test_deleted_file_keeps_its_importers(self):
        self.write('leaf/extra.go', 'package leaf\nconst Extra = 1\n')
        self.commit()
        self.base = self.git('rev-parse', 'HEAD')
        (self.root / 'leaf/extra.go').unlink()
        self.commit()
        self.assertEqual(fast_check.plan(self.root, self.base)['packages'], ['ci.example/notes/leaf', 'ci.example/notes/reader'])

    def test_documentation_does_not_select_go_and_unknown_base_does(self):
        self.write('docs/help.md', 'Operator instructions\n')
        self.commit()
        self.assertEqual(fast_check.plan(self.root, self.base)['packages'], [])
        self.assertEqual(len(fast_check.plan(self.root, 'unavailable-base')['packages']), 3)
        self.assertEqual(len(fast_check.plan(self.root)['packages']), 3)

    def test_test_document_and_operator_inputs_select_no_go_but_unknown_paths_select_all(self):
        # check-fast still formats browser files, runs the fixture, frontend and operator
        # tests, and every Make target; the Go lane has no test that reads these.
        for name in ('scripts/browser/chat.spec.mjs', 'playwright.config.mjs', 'scripts/browser/app-fixture.mjs',
                     'scripts/browser/no-netlink.c', 'scripts/browser-fixture.test.mjs', 'scripts/browser-summary.mjs',
                     'scripts/smoke.mjs', 'Makefile', 'compose.yaml', 'CHANGELOG.md', '.github/workflows/check.yml'):
            with self.subTest(name):
                self.git('reset', '--hard', self.base)
                self.write(name, '// Synthetic harness change\n')
                self.commit()
                self.assertEqual(fast_check.plan(self.root, self.base)['packages'], [])
        self.write('leaf/leaf.go', 'package leaf\nfunc Value() int { return 6 }\n')
        self.commit()
        self.assertEqual(fast_check.plan(self.root, self.base)['packages'], ['ci.example/notes/leaf', 'ci.example/notes/reader'])
        for name in ('new-input.txt', 'tools/generate.mjs', 'cmd/schema.json'):
            with self.subTest(name):
                self.git('reset', '--hard', self.base)
                self.write('scripts/browser/chat.spec.mjs', '// Synthetic browser harness change\n')
                self.write(name, '# Unknown input\n')
                self.commit()
                selection = fast_check.plan(self.root, self.base)
                self.assertEqual(selection['reason'], 'Unknown input: all packages')
                self.assertEqual(len(selection['packages']), 3)

    def test_bundle_sources_select_web_and_importers_but_unknown_paths_still_select_all(self):
        # npm run build bundles these into internal/web/ui/dist, which internal/web embeds.
        self.write('internal/web/web.go', 'package web\nfunc Value() int { return 7 }\n')
        self.write('cmd/server/main.go', 'package main\nimport "ci.example/notes/internal/web"\nfunc main() { _ = web.Value() }\n')
        self.commit()
        self.base = self.git('rev-parse', 'HEAD')
        packages = fast_check.output(self.root, 'go', 'list', '-race', './...').splitlines()
        inputs = go_package_inputs(self.root, packages, fast_check.output(self.root, 'go', 'list', '-race', '-deps', '-test', '-json', './...'))
        web = ['ci.example/notes/cmd/server', 'ci.example/notes/internal/web']
        for name in ('frontend/x.mjs', 'package.json', 'package-lock.json', 'internal/web/ui/styles/x.css'):
            with self.subTest(name):
                self.git('reset', '--hard', self.base)
                self.write(name, '// Synthetic bundle source change\n')
                self.commit()
                self.assertEqual(fast_check.plan(self.root, self.base)['packages'], web)
                # A routine release that reached this input would select the same packages.
                self.assertEqual(sorted(affected_go_packages({'mode': 'routine', 'changed_paths': [name]}, inputs)[0]), web)
        for name in ('new-input.txt', 'frontend.mjs'):
            with self.subTest(name):
                self.git('reset', '--hard', self.base)
                self.write('frontend/x.mjs', '// Synthetic bundle source change\n')
                self.write(name, '# Unknown input\n')
                self.commit()
                selection = fast_check.plan(self.root, self.base)
                self.assertEqual(selection['reason'], 'Unknown input: all packages')
                self.assertEqual(len(selection['packages']), 5)

    def test_operator_only_change_uses_operator_checks_without_unrelated_go(self):
        self.write('deploy/production/release_image.py','# Operator selection update\n')
        self.commit()
        self.assertEqual(fast_check.plan(self.root,self.base)['packages'],[])

    def test_test_only_importers_are_selected(self):
        self.write('other/other_test.go', 'package other\nimport ("testing"; "ci.example/notes/leaf")\nfunc TestRead(t *testing.T) { if leaf.Value() == 0 { t.Fatal("empty") } }\n')
        self.commit()
        self.base = self.git('rev-parse', 'HEAD')
        self.write('leaf/leaf.go', 'package leaf\nfunc Value() int { return 4 }\n')
        self.commit()
        self.assertEqual(fast_check.plan(self.root, self.base)['packages'], ['ci.example/notes/leaf', 'ci.example/notes/other', 'ci.example/notes/reader'])

    def test_deleted_runtime_fixture_still_selects_owner(self):
        self.write('reader/testdata/input.txt', 'evidence\n')
        self.write('reader/reader_test.go', 'package reader\nimport ("os"; "testing")\nfunc TestInput(t *testing.T) { if _, e := os.ReadFile("testdata/input.txt"); e != nil { t.Fatal(e) } }\n')
        self.commit()
        self.base = self.git('rev-parse', 'HEAD')
        (self.root / 'reader/testdata/input.txt').unlink()
        self.commit()
        self.assertIn('ci.example/notes/reader', fast_check.plan(self.root, self.base)['packages'])
        packages=fast_check.output(self.root,'go','list','-race','./...').splitlines()
        inventory=fast_check.output(self.root,'go','list','-race','-deps','-test','-json','./...')
        inputs=go_package_inputs(self.root,packages,inventory)
        selected,_=affected_go_packages({'mode':'routine','changed_paths':['reader/testdata/input.txt']},inputs)
        self.assertIn('ci.example/notes/reader',selected)

    def test_race_tagged_test_importer_is_selected(self):
        self.write('other/race_test.go', '//go:build race\n\npackage other\nimport ("testing"; "ci.example/notes/leaf")\nfunc TestRaceRead(t *testing.T) { if leaf.Value() == 0 { t.Fatal("empty") } }\n')
        self.commit()
        self.base = self.git('rev-parse', 'HEAD')
        self.write('leaf/leaf.go', 'package leaf\nfunc Value() int { return 4 }\n')
        self.commit()
        self.assertIn('ci.example/notes/other', fast_check.plan(self.root, self.base)['packages'])

    def test_dirty_input_is_included_in_local_feedback(self):
        self.write('leaf/leaf.go', 'package leaf\nfunc Value() int { return 5 }\n')
        self.assertEqual(fast_check.plan(self.root, self.base)['packages'], ['ci.example/notes/leaf', 'ci.example/notes/reader'])

    def test_web_selection_retains_changed_cases_and_reports_fallback(self):
        self.write('internal/web/new_test.go', 'package web\nfunc TestNewBoundary(t *testing.T) {}\n')
        self.commit()
        available = [*CRITICAL_WEB, 'TestNewBoundary', 'TestUnchangedLongMatrix']
        selection = fast_check.plan(self.root, self.base)
        names, _ = fast_check.selected_web_tests(self.root, selection, available)
        self.assertIn('TestNewBoundary', names)
        self.assertTrue(set(CRITICAL_WEB).issubset(names))
        self.assertNotIn('TestUnchangedLongMatrix', names)
        self.assertEqual(fast_check.selected_web_tests(self.root, fast_check.plan(self.root), available)[0], sorted(available))
        self.assertEqual(fast_check.selected_web_tests(self.root, selection, ['TestNewBoundary'])[0], ['TestNewBoundary'])

    def test_feature_change_runs_the_same_web_callers_as_a_routine_release(self):
        self.write('internal/web/slack_link_test.go', 'package web\nfunc TestSlackLinkCaller(t *testing.T) {}\n')
        self.write('internal/web/trash_test.go', 'package web\nfunc TestUnrelatedTrash(t *testing.T) {}\n')
        self.commit()
        self.base = self.git('rev-parse', 'HEAD')
        self.write('internal/connectors/google.go', 'package connectors\nconst Scope = "drive"\n')
        self.commit()
        selection = fast_check.plan(self.root, self.base)
        self.assertEqual(selection['features'], ['connectors'])
        available = [*CRITICAL_WEB, 'TestSlackLinkCaller', 'TestUnrelatedTrash']
        names, _ = fast_check.selected_web_tests(self.root, selection, available)
        self.assertIn('TestSlackLinkCaller', names)
        self.assertNotIn('TestUnrelatedTrash', names)

    def test_no_command_relies_on_short_mode(self):
        # No test calls testing.Short, so -short would only suggest a smaller suite.
        self.write('leaf/leaf.go', 'package leaf\nfunc Value() int { return 8 }\n')
        self.commit()
        planned = fast_check.commands(self.root, fast_check.plan(self.root, self.base))
        self.assertEqual(len(planned), 2)
        self.assertFalse(any('-short' in command for command in planned))
