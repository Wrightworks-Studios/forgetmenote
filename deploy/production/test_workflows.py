"""GitHub workflows keep the promises SECURITY.md and docs/verification.md make."""
from pathlib import Path
import re
import unittest

import yaml

ROOT = Path(__file__).resolve().parents[2]


def workflow(name):
    value = yaml.safe_load((ROOT / '.github/workflows' / name).read_text())
    # YAML 1.1 reads the bare key "on" as true.
    return value, value.get('on', value.get(True))


def assert_failed_gate_uploads_its_evidence(test, steps):
    """The gate prints only the end of a failed log; GitHub discards the rest with the runner."""
    gate = next(index for index, step in enumerate(steps) if 'release_image.py' in step.get('run', ''))
    uploads = [step for step in steps if step.get('if') == 'failure()']
    test.assertEqual(len(uploads), 1)
    upload = uploads[0]
    test.assertGreater(steps.index(upload), gate)
    test.assertTrue(upload['uses'].startswith('actions/upload-artifact@'))
    # Anyone signed in can read a public repository's artifacts: only the check logs,
    # browser report and traces of synthetic data, never the snapshots beside them.
    state = '~/.cache/forgetmenote/releases/${{ github.sha }}/'
    test.assertEqual(upload['with']['path'].splitlines(), [state + name for name in (
        '*.log', '*.report.json', 'verification.json', 'browser-*-results/')])
    test.assertNotIn('include-hidden-files', upload['with'])
    # A re-run keeps the first attempt's evidence.
    test.assertIn('${{ github.run_attempt }}', upload['with']['name'])
    test.assertNotIn('overwrite', upload['with'])
    test.assertLessEqual(upload['with']['retention-days'], 7)


class CheckWorkflowTests(unittest.TestCase):
    def test_advisory_scan_runs_on_every_push_pull_request_and_weekly(self):
        value, triggers = workflow('check.yml')
        self.assertIn('pull_request', triggers)
        self.assertEqual(sorted(triggers['push']['branches']), ['main', 'next'])
        self.assertNotIn('paths', triggers['push'])
        self.assertNotIn('paths-ignore', triggers['push'])
        self.assertTrue(re.fullmatch(r'\S+ \S+ \* \* [0-6]', triggers['schedule'][0]['cron']), triggers['schedule'])
        self.assertNotIn('if', value['jobs']['vulnerabilities'])
        # Pushes and the schedule run the scan alone; PR and manual tiers keep their events.
        self.assertEqual(value['jobs']['fast']['if'], "github.event_name == 'pull_request'")
        self.assertEqual(value['jobs']['full']['if'], "github.event_name == 'workflow_dispatch'")
        self.assertEqual(set(value['jobs']), {'vulnerabilities', 'fast', 'full'})

    def test_every_action_is_pinned_to_a_commit(self):
        for path in sorted((ROOT / '.github/workflows').glob('*.yml')):
            for line in path.read_text().splitlines():
                found = re.search(r'uses:\s*(\S+)', line)
                if found:
                    with self.subTest(workflow=path.name, action=found[1]):
                        self.assertRegex(found[1], r'^[\w.-]+/[\w.-]+@[0-9a-f]{40}$')

    def test_full_check_caches_dependencies_and_image_layers(self):
        value, _ = workflow('check.yml')
        for job in ('fast', 'full'):
            node = next(step for step in value['jobs'][job]['steps'] if step.get('uses', '').startswith('actions/setup-node@'))
            self.assertEqual(node['with'].get('cache'), 'npm', job)
        steps = value['jobs']['full']['steps']
        uses = [step.get('uses', '').split('@')[0] for step in steps]
        browsers = next(step for step in steps if step.get('uses', '').startswith('actions/cache@'))
        self.assertEqual(browsers['with']['path'], '~/.cache/ms-playwright')
        self.assertIn("hashFiles('package-lock.json')", browsers['with']['key'])
        gate = next(index for index, step in enumerate(steps) if 'release_image.py' in step.get('run', ''))
        self.assertEqual(steps[gate].get('env', {}).get('FMN_IMAGE_CACHE'), 'gha')
        # make setup-browser installs every engine the suite uses (test_makefile.py).
        setup = next(index for index, step in enumerate(steps) if 'setup-browser' in step.get('run', ''))
        self.assertLess(setup, gate)
        # The gha cache backend needs a docker-container builder and the runtime token.
        for action in ('docker/setup-buildx-action', 'crazy-max/ghaction-github-runtime', 'actions/cache'):
            self.assertLess(uses.index(action), gate, action)

    def test_failed_full_check_uploads_its_logs_reports_and_traces(self):
        value, _ = workflow('check.yml')
        assert_failed_gate_uploads_its_evidence(self, value['jobs']['full']['steps'])


class PublishWorkflowTests(unittest.TestCase):
    def setUp(self):
        self.value, self.triggers = workflow('publish.yml')
        self.jobs = self.value['jobs']

    def test_each_main_release_verifies_publishes_and_deploys_in_order(self):
        self.assertEqual(set(self.triggers), {'push'})
        self.assertEqual(self.triggers['push']['branches'], ['main'])
        self.assertEqual(set(self.jobs), {'verify', 'publish', 'deploy'})
        self.assertEqual(self.jobs['publish']['needs'], 'verify')
        self.assertEqual(self.jobs['deploy']['needs'], 'publish')
        self.assertIs(self.value['concurrency']['cancel-in-progress'], False)
        # Publication replaced the old notification: one dispatch per release.
        self.assertFalse((ROOT / '.github/workflows/notify-deployments.yml').exists())

    def test_the_gate_holds_no_registry_or_signing_credentials(self):
        self.assertEqual(self.value['permissions'], {'contents': 'read'})
        verify = self.jobs['verify']
        self.assertNotIn('permissions', verify)
        self.assertNotIn('environment', verify)
        steps = yaml.safe_dump(verify['steps'])
        self.assertNotIn('secrets.', steps)
        self.assertNotIn('github.token', steps)
        gate = next(step for step in verify['steps'] if 'release_image.py' in step.get('run', ''))
        self.assertIn('--verify-only --since-published', gate['run'])
        self.assertEqual(gate['env'], {'FMN_IMAGE_CACHE': 'gha'})

    def test_failed_gate_uploads_its_logs_reports_and_traces(self):
        assert_failed_gate_uploads_its_evidence(self, self.jobs['verify']['steps'])

    def test_publication_signs_only_what_the_gate_verified_and_runs_no_project_checks(self):
        publish = self.jobs['publish']
        self.assertEqual(publish['permissions'], {'contents': 'read', 'packages': 'write', 'id-token': 'write',
                                                  'attestations': 'write'})
        actions = [step['uses'].split('@')[0] for step in publish['steps'] if 'uses' in step]
        self.assertEqual(actions, ['actions/checkout', 'actions/setup-python', 'actions/download-artifact',
                                   'actions/download-artifact', 'actions/attest-build-provenance', 'actions/attest'])
        runs = ' '.join(step.get('run', '') for step in publish['steps'])
        for tool in ('make ', 'npm ', 'npx ', 'go ', 'pip '):
            self.assertNotIn(tool, runs)
        step = next(step for step in publish['steps'] if step.get('id') == 'publish')
        self.assertIn('release_image.py --publish-verified', step['run'])
        self.assertEqual(step['env'], {'GH_TOKEN': '${{ github.token }}'})
        for name in ('attest-build-provenance', 'attest'):
            attest = next(s for s in publish['steps'] if s.get('uses', '').startswith('actions/' + name + '@'))
            self.assertEqual(attest['with']['subject-digest'], '${{ steps.publish.outputs.digest }}')
        summary = next(s for s in publish['steps'] if s.get('uses', '').startswith('actions/attest@'))
        self.assertTrue(summary['with']['predicate-path'].endswith('/verification-summary.json'))

    def test_deployment_receives_the_pinned_public_digest(self):
        deploy = self.jobs['deploy']
        self.assertEqual(deploy['environment'], 'deployments')
        self.assertNotIn('permissions', deploy)
        runs = [step['run'] for step in deploy['steps']]
        self.assertIn('imagetools inspect "$IMAGE@$DIGEST"', runs[0])
        self.assertIn('-f image="$IMAGE@$DIGEST"', runs[1])
        self.assertEqual(deploy['env']['DIGEST'], '${{ needs.publish.outputs.digest }}')


if __name__ == '__main__':
    unittest.main()
