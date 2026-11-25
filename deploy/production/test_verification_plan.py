"""Fail-closed scope selection against real committed changes."""
import os
from pathlib import Path
import re
import subprocess
import tempfile
import unittest

import verification_plan as verification_plan_module
from verification_plan import CRITICAL_BROWSER, verification_plan
from release_image import runtime_changes


class PlanTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.git('init', '-b', 'main')
        self.git('config', 'user.name', 'Release test')
        self.git('config', 'user.email', 'release@example.test')
        self.git('commit', '--allow-empty', '-m', 'Baseline')
        self.baseline = self.git('rev-parse', 'HEAD')

    def git(self, *args):
        return subprocess.check_output(['git', *args], cwd=self.root, stderr=subprocess.PIPE, text=True).strip()

    def change(self, *paths):
        for name in paths:
            path = self.root / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text('changed\n')
        self.git('add', '.')
        self.git('commit', '-m', 'Candidate')

    def plan(self, mode='auto'):
        return verification_plan(self.root, self.baseline, 'HEAD', mode,
                                 runtime_paths=runtime_changes(self.root, self.baseline, 'HEAD'))

    def test_feature_change_keeps_critical_journeys_and_complete_affected_matrix(self):
        self.change('internal/decision/client.go')
        plan = self.plan()
        self.assertEqual(plan['mode'], 'routine')
        self.assertEqual(plan['jev_profile'], 'full')
        self.assertEqual(plan['source_profile'], 'routine')
        for title in CRITICAL_BROWSER:
            self.assertRegex(title, plan['browser_grep'])
        self.assertRegex('jev-integration.spec.mjs organizer real debounce', plan['browser_grep'])
        self.assertNotRegex('unrelated.spec.mjs unknown journey', plan['browser_grep'])

    def test_jev_package_selects_the_decisions_feature_and_the_jev_scenario(self):
        self.change('internal/jev/jobs.go')
        plan = self.plan()
        self.assertEqual(plan['mode'], 'routine')
        self.assertEqual(plan['features'], ['decisions'])
        self.assertEqual(plan['containers'], ['core', 'jev'])
        self.assertEqual(plan['jev_profile'], 'full')
        self.assertRegex('jev-integration.spec.mjs organizer real debounce', plan['browser_grep'])
        self.assertIn('jev', verification_plan_module.escalated_containers(['internal/jev/organizer_runner.go']))

    def test_unknown_shared_and_security_inputs_escalate_even_with_ui_changes(self):
        for path in ('Dockerfile', 'go.mod', 'internal/catalog/assistant.go', 'internal/web/ui/app.mjs',
                     'internal/web/ui/index.html', 'internal/chat/permissions.go', 'internal/chat/store.go', 'internal/chat/admission.go', 'internal/chat/config.go',
                     'deploy/iwork/convert.cpp', 'internal/future/new.go', 'internal/web/automation_script_capabilities.go',
                     'scripts/browser/app-fixture.mjs', 'frontend/editor.mjs'):
            with self.subTest(path=path):
                self.git('reset', '--hard', self.baseline)
                self.change('internal/web/ui/style.css', path)
                self.assertEqual(self.plan()['mode'], 'full')
                with self.assertRaisesRegex(ValueError, 'Routine verification is not allowed'):
                    self.plan('routine')

    def test_automatic_full_runs_slow_containers_only_for_their_own_inputs(self):
        quick = ['core', 'headless', 'resource', 'chat', 'cluster']
        for paths, slow in ((['internal/workspace/store.go'], []),
                            (['internal/web/ui/app.mjs'], []),
                            (['internal/sources/store.go'], ['source']),
                            (['internal/decision/store.go'], ['jev']),
                            (['internal/web/proxy_login.go'], ['oidc', 'totp']),
                            (['internal/catalog/password.go'], ['oidc', 'totp']),
                            (['internal/workspace/store.go', 'Dockerfile'], ['source', 'jev', 'oidc', 'totp']),
                            (['go.sum'], ['source', 'jev', 'oidc', 'totp']),
                            (['cmd/forgetmenote/main.go'], ['source', 'jev', 'oidc', 'totp'])):
            with self.subTest(paths=paths):
                self.git('reset', '--hard', self.baseline)
                self.change(*paths)
                plan = self.plan()
                self.assertEqual(plan['mode'], 'full')
                self.assertEqual(sorted(plan['containers']), sorted(quick + slow))
        # An explicit request, or a change nobody could classify, keeps every container.
        self.assertEqual(len(self.plan('full')['containers']), 9)
        self.assertEqual(len(verification_plan(self.root, None, 'HEAD')['containers']), 9)

    def test_automatic_full_runs_every_browser_case(self):
        # A full plan deploys; the browser suite is the only caller check of the whole UI.
        for paths in (['internal/workspace/store.go'], ['go.mod'], ['internal/workspace/store.go', 'scripts/browser/organizer.spec.mjs'],
                      ['scripts/browser/app-fixture.mjs']):
            with self.subTest(paths=paths):
                self.git('reset', '--hard', self.baseline)
                self.change(*paths)
                plan = self.plan()
                self.assertEqual(plan['mode'], 'full')
                self.assertEqual(plan['browser_grep'], '')
        self.assertEqual(self.plan('full')['browser_grep'], '')
        self.assertEqual(verification_plan(self.root, None, 'HEAD')['browser_grep'], '')

    def test_storage_v2_packages_always_take_the_full_gate(self):
        for path in ('internal/gitobj/object.go', 'internal/notestore/apply.go', 'internal/gitexport/export.go',
                     'internal/workspace/db_notebook.go', 'internal/workspace/workspacetest/workspacetest.go'):
            with self.subTest(path=path):
                self.git('reset', '--hard', self.baseline)
                self.change('internal/web/ui/style.css', path)
                plan = self.plan()
                self.assertEqual((plan['mode'], plan['reason']), ('full', 'storage input: ' + path))
                with self.assertRaisesRegex(ValueError, 'Routine verification is not allowed: storage input'):
                    self.plan('routine')

    def test_multiple_features_select_the_union_of_affected_coverage(self):
        self.change('internal/decision/client.go', 'internal/connectors/google.go')
        plan=self.plan()
        self.assertEqual(plan['mode'], 'routine')
        self.assertEqual(plan['features'], ['connectors','decisions'])
        self.assertEqual(plan['containers'], ['chat','core','jev'])

    def test_provider_changes_do_not_select_unrelated_container_checks(self):
        self.change('internal/chat/provider.go')
        plan=self.plan()
        self.assertEqual(plan['containers'], ['chat','core'])
        self.assertRegex('website-capture.spec.mjs',plan['browser_grep'])

    def test_routine_web_coverage_keeps_callers_critical_boundaries_and_changed_tests(self):
        from verification_plan import CRITICAL_WEB, selected_web_tests
        self.change('internal/chat/provider.go')
        path=self.root/'internal/web/automation_cloudflare_test.go';path.parent.mkdir(parents=True,exist_ok=True)
        path.write_text('package web\nfunc TestCloudflareCaller(t *testing.T) {}\n')
        available=list(CRITICAL_WEB)+['TestCloudflareCaller','TestUnrelatedTrash']
        names,_=selected_web_tests(self.root,self.plan(),available)
        self.assertIn('TestCloudflareCaller',names)
        self.assertTrue(set(CRITICAL_WEB)<=set(names))
        self.assertNotIn('TestUnrelatedTrash',names)
        self.assertEqual(selected_web_tests(self.root,{'mode':'full'},available)[0], sorted(available))

    def test_scoped_retry_retains_every_failed_browser_case_and_preserves_other_blocks(self):
        import json
        from verification_plan import retained_browser_failures
        self.change('internal/chat/provider.go');plan=self.plan()
        report=self.root/'browser.json';report.write_text(json.dumps({'suites':[{'specs':[{'ok':False,'file':'scripts/browser/quicknotes.spec.mjs','title':CRITICAL_BROWSER[5]}]}]}))
        previous={'plan':{'mode':'full'},'checks':{'browser':{'status':'failed','artifacts':{'FMN_BROWSER_REPORT':str(report)}}}}
        with self.assertRaisesRegex(ValueError,'explicitly retrying'):retained_browser_failures(previous,plan,False)
        self.assertEqual(len(retained_browser_failures(previous,plan,True)),1)
        report.write_text(json.dumps({'suites':[{'specs':[{'ok':False,'file':'other.spec.mjs','title':'omitted failure'}]}]}))
        with self.assertRaisesRegex(ValueError,'omits'):retained_browser_failures(previous,plan,True)
        previous['checks']['go-web-1']={'status':'failed'}
        with self.assertRaisesRegex(ValueError,'Cannot narrow'):retained_browser_failures(previous,plan,True)

    def test_unresolved_original_browser_failure_survives_routine_resume(self):
        import json
        from verification_plan import retained_browser_failures
        self.change('internal/chat/provider.go');plan=self.plan()
        report=self.root/'original-browser.json';case='quicknotes.spec.mjs '+CRITICAL_BROWSER[5]
        report.write_text(json.dumps({'suites':[{'specs':[{'ok':False,'file':'quicknotes.spec.mjs','title':CRITICAL_BROWSER[5]}]}]}))
        previous={'plan':{'mode':'routine','retained_failed_browser_tests':[case],'original_failed_browser_report':str(report)},'checks':{'browser':{'status':'running'}}}
        with self.assertRaisesRegex(ValueError,'explicitly retrying'):retained_browser_failures(previous,plan,False)
        self.assertEqual(retained_browser_failures(previous,plan,True),[case])
        self.assertEqual(plan['original_failed_browser_report'],str(report))
        with self.assertRaisesRegex(ValueError,'omits'):retained_browser_failures(previous,{**plan,'browser_grep':'other'},True)

    def test_resolved_browser_failure_does_not_hide_a_different_failed_check(self):
        import json
        from verification_plan import retained_browser_failures
        self.change('internal/chat/provider.go');plan=self.plan()
        report=self.root/'passed-browser.json';case='quicknotes.spec.mjs '+CRITICAL_BROWSER[5]
        report.write_text(json.dumps({'suites':[{'specs':[{'ok':True,'file':'quicknotes.spec.mjs','title':CRITICAL_BROWSER[5]}]}]}))
        previous={'plan':{'mode':'full','retained_failed_browser_tests':[case]},'checks':{'browser':{'status':'passed','artifacts':{'FMN_BROWSER_REPORT':str(report)}},'go-workspace':{'status':'failed'}}}
        with self.assertRaisesRegex(ValueError,'Cannot narrow'):retained_browser_failures(previous,plan,False)

    def test_deleted_runtime_input_still_requires_coverage(self):
        self.change('internal/sources/parse.go')
        self.baseline = self.git('rev-parse', 'HEAD')
        self.git('rm', 'internal/sources/parse.go')
        self.git('commit', '-m', 'Remove parser')
        self.assertEqual(self.plan()['source_profile'], 'full')

    def test_changed_browser_test_is_selected_without_weakening_baseline_journeys(self):
        self.change('scripts/browser/new-feature.spec.mjs', 'internal/web/ui/style.css')
        self.assertRegex('new-feature.spec.mjs', self.plan()['browser_grep'])

    def test_stylesheet_partials_are_appearance_changes(self):
        self.change('internal/web/ui/styles/editor.css')
        plan = self.plan()
        self.assertEqual((plan['mode'], plan['features']), ('routine', ['appearance']))

    def test_missing_or_unrelated_baseline_cannot_narrow(self):
        self.assertEqual(verification_plan(self.root, None, 'HEAD')['mode'], 'full')
        with self.assertRaisesRegex(ValueError, 'no deployed baseline'):
            verification_plan(self.root, None, 'HEAD', 'routine')
        self.change('internal/web/ui/style.css')
        future = self.git('rev-parse', 'HEAD')
        self.assertEqual(verification_plan(self.root, future, self.baseline)['mode'], 'full')

    def test_critical_titles_still_exist_in_real_browser_cases(self):
        browser = Path(__file__).resolve().parents[2] / 'scripts/browser'
        source = '\n'.join(path.read_text() for path in browser.glob('*.spec.mjs'))
        for title in CRITICAL_BROWSER:
            self.assertIn("test('" + title + "'", source, title)


ROOT = Path(__file__).resolve().parents[2]


def repository_files():
    """Committed and generated files of this checkout, without dependency caches."""
    skip = {'.git', 'node_modules', '.venv', '__pycache__', 'test-results', 'playwright-report'}
    found = []
    for directory, names, files in os.walk(ROOT):
        names[:] = [name for name in names if name not in skip]
        found.extend(str((Path(directory) / name).relative_to(ROOT)) for name in files)
    return found


def web_cases(path):
    return re.findall(r'^func\s+((?:Test|Example|Fuzz)\w*)\s*\(', path.read_text(), re.M)


class MappingCoverageTests(unittest.TestCase):
    """A mapping that names no real file silently narrows the gate."""

    @classmethod
    def setUpClass(cls):
        cls.files = repository_files()

    def matching(self, pattern):
        return [name for name in self.files if verification_plan_module.path_matches(name, pattern)]

    def test_every_feature_path_and_browser_suite_exists(self):
        for feature, (patterns, suites) in verification_plan_module.FEATURES.items():
            for pattern in patterns:
                with self.subTest(feature=feature, pattern=pattern):
                    self.assertTrue(self.matching(pattern), pattern + ' matches no file')
            for suite in suites:
                with self.subTest(feature=feature, suite=suite):
                    self.assertTrue((ROOT / 'scripts/browser' / (suite + '.spec.mjs')).is_file(), suite)

    def test_every_feature_web_glob_selects_a_current_case(self):
        self.assertEqual(set(verification_plan_module.FEATURE_WEB_TESTS), set(verification_plan_module.FEATURES))
        for feature, patterns in verification_plan_module.FEATURE_WEB_TESTS.items():
            for pattern in patterns:
                with self.subTest(feature=feature, pattern=pattern):
                    cases = [case for path in (ROOT / 'internal/web').glob(pattern) for case in web_cases(path)]
                    self.assertTrue(cases, pattern + ' selects no web test case')

    def test_every_container_mapping_names_a_scenario_and_a_real_input(self):
        plan = verification_plan_module
        self.assertEqual(set(plan.FEATURE_CONTAINERS), set(plan.FEATURES))
        for feature, containers in plan.FEATURE_CONTAINERS.items():
            self.assertLessEqual(containers, set(plan.ALL_CONTAINERS), feature)
        self.assertEqual(plan.QUICK_CONTAINERS | set(plan.SLOW_CONTAINERS), set(plan.ALL_CONTAINERS))
        for name, patterns in plan.SLOW_CONTAINERS.items():
            for pattern in patterns:
                with self.subTest(container=name, pattern=pattern):
                    self.assertTrue(any(re.search(pattern, path, re.I) for path in self.files), pattern)
        for path, feature in plan.SMOKE_FEATURES.items():
            self.assertIn(feature, plan.FEATURES)
            self.assertTrue((ROOT / path).is_file(), path)

    def test_every_full_gate_path_exists(self):
        for feature, patterns in verification_plan_module.FULL_GATE.items():
            for pattern in patterns:
                with self.subTest(feature=feature, pattern=pattern):
                    self.assertTrue(self.matching(pattern), pattern + ' matches no file')

    def test_critical_web_boundaries_still_exist(self):
        cases = {case for path in (ROOT / 'internal/web').glob('*_test.go') for case in web_cases(path)}
        for name in verification_plan_module.CRITICAL_WEB:
            self.assertIn(name, cases)

    def test_routine_connector_release_runs_the_slack_and_google_callers(self):
        available = sorted({case for path in (ROOT / 'internal/web').glob('*_test.go') for case in web_cases(path)})
        plan = {'mode': 'routine', 'features': ['connectors'], 'changed_paths': []}
        selected = set(verification_plan_module.selected_web_tests(ROOT, plan, available)[0])
        callers = {case for name in ('slack*_test.go', 'data_google_test.go')
                   for path in (ROOT / 'internal/web').glob(name) for case in web_cases(path)}
        self.assertTrue(callers)
        self.assertLessEqual(callers, selected)


def tracked_files():
    """The committed paths, or None outside a Git checkout (an archived release source)."""
    result = subprocess.run(['git', 'ls-files', '-z'], cwd=ROOT, capture_output=True)
    return [name for name in result.stdout.decode().split('\0') if name] if result.returncode == 0 else None


class PathClassTests(unittest.TestCase):
    """One classifier decides image inputs for PR feedback, releases and deployment."""

    def test_representative_paths(self):
        for path, expected in {
            'CHANGELOG.md': 'docs', 'SECURITY.md': 'docs', 'docs/verification.md': 'docs',
            'deploy/production/releasing.md': 'docs',
            'deploy/production/release_image.py': 'operator', '.github/workflows/check.yml': 'operator',
            'internal/sources/testdata/scan.pdf': 'test', 'internal/store/store_test.go': 'test',
            'internal/web/ui/state.test.mjs': 'test', 'scripts/oidc_smoke.py': 'test',
            'scripts/mcp-smoke/main.go': 'test', 'scripts/browser/chat.spec.mjs': 'test',
            'playwright.config.mjs': 'test', 'Makefile': 'test', 'compose.yaml': 'test',
            'deploy/git-backup/test_watcher.py': 'test', 'deploy/integrations/slack.example.json': 'test',
            'Dockerfile': 'runtime', '.dockerignore': 'runtime', 'go.sum': 'runtime', 'package-lock.json': 'runtime',
            'internal/web/server.go': 'runtime', 'internal/web/ui/style.css': 'runtime', 'frontend/editor.mjs': 'runtime',
            'plugins/forgetmenote/skills/workspace/SKILL.md': 'runtime', 'deploy/iwork/convert.cpp': 'runtime',
            'deploy/git-backup/watcher.py': 'runtime', 'deploy/debian.sources': 'runtime',
            # Unreviewed inputs and Go implementations outside the image fail closed.
            'new-runtime-input.txt': 'runtime', 'deploy/production/helper.go': 'runtime', 'lib/x.go': 'runtime',
            'docs/example.go': 'runtime',
        }.items():
            with self.subTest(path=path):
                self.assertEqual(verification_plan_module.path_class(path), expected)

    def test_every_committed_path_has_a_reviewed_class(self):
        files = tracked_files()
        if files is None:
            self.skipTest('not a Git checkout')
        unreviewed = [path for path in files if verification_plan_module.reviewed_class(path) is None]
        self.assertEqual(unreviewed, [])

    def test_dockerfile_copies_exactly_the_image_sources(self):
        copied = set()
        for line in (ROOT / 'Dockerfile').read_text().splitlines():
            words = line.split()
            if words[:1] == ['COPY'] and not any(word.startswith('--from=') for word in words):
                copied.update(word.rstrip('/') for word in words[1:-1] if not word.startswith('--'))
        sources = verification_plan_module.IMAGE_SOURCES
        for path in copied:
            self.assertTrue(any(path == source.rstrip('/') or path.startswith(source) for source in sources), path)
        for source in sources:
            self.assertIn(source.rstrip('/'), copied)

    def test_dockerignore_keeps_test_inputs_out_of_the_image(self):
        ignored = (ROOT / '.dockerignore').read_text().splitlines()
        for pattern in verification_plan_module.IMAGE_EXCLUDED:
            self.assertIn(pattern, ignored)

    def test_deployment_notification_ignores_exactly_the_documents(self):
        workflow = (ROOT / '.github/workflows/notify-deployments.yml').read_text()
        block = re.search(r'^    paths-ignore:\n((?:      - .*\n|      #.*\n)+)', workflow, re.M)
        ignored = re.findall(r"^      - '([^']+)'$", block[1], re.M)
        self.assertEqual(sorted(ignored), sorted(verification_plan_module.DOCUMENTS))
        files = tracked_files() or repository_files()
        documents = [path for path in files if verification_plan_module.path_class(path) == 'docs']
        self.assertIn('CHANGELOG.md', documents)
        self.assertIn('deploy/production/releasing.md', documents)
        for path in files:
            skipped = any(verification_plan_module.github_path_match(path, pattern) for pattern in ignored)
            self.assertEqual(skipped, path in documents, path)
