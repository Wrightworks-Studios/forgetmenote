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
        self.assertEqual(value['jobs']['fast']['if'], "github.event_name == 'pull_request' || github.event_name == 'merge_group'")
        self.assertEqual(value['jobs']['full']['if'], "github.event_name == 'workflow_dispatch'")
        self.assertEqual(set(value['jobs']), {'vulnerabilities', 'fast', 'full'})

    def test_the_merge_queue_runs_the_fast_tier_against_its_base(self):
        # next requires fast through its merge queue: a queue entry that never
        # reports fast would wait until it times out.
        value, triggers = workflow('check.yml')
        self.assertIn('merge_group', triggers)
        self.assertIn("github.event_name == 'merge_group'", value['jobs']['fast']['if'])
        # fast_check diffs from the merge base of HEAD and BASE; a queue entry's
        # base is next, or the entry ahead of it.
        self.assertEqual(value['jobs']['fast']['env']['BASE'],
                         '${{ github.event.pull_request.base.sha || github.event.merge_group.base_sha }}')
        fetch = next(step for step in value['jobs']['fast']['steps'] if step.get('uses', '').startswith('actions/checkout@'))
        self.assertEqual(fetch['with']['fetch-depth'], 0)
        # Only pull request runs give way to a newer push; queue entries finish.
        self.assertEqual(value['concurrency']['cancel-in-progress'], "${{ github.event_name == 'pull_request' }}")

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


class ScorecardWorkflowTests(unittest.TestCase):
    def test_scorecard_publishes_from_the_default_branch_and_writes_nothing_else(self):
        value, triggers = workflow('scorecard.yml')
        # Scorecard publishes only runs of the default branch (CONTRIBUTING.md: next).
        self.assertEqual(triggers['push']['branches'], ['next'])
        self.assertIn('schedule', triggers)
        self.assertEqual(value['permissions'], 'read-all')
        job = value['jobs']['analysis']
        writes = {name for name, level in job['permissions'].items() if level == 'write'}
        self.assertEqual(writes, {'id-token'})
        scorecard = next(step for step in job['steps'] if step.get('uses', '').startswith('ossf/scorecard-action@'))
        self.assertIs(scorecard['with']['publish_results'], True)
        checkout = next(step for step in job['steps'] if step.get('uses', '').startswith('actions/checkout@'))
        self.assertIs(checkout['with']['persist-credentials'], False)


class NotifyDeploymentsWorkflowTests(unittest.TestCase):
    def test_main_hands_deployments_the_revision_and_github_publishes_no_image(self):
        value, triggers = workflow('notify-deployments.yml')
        self.assertEqual(set(triggers), {'push'})
        self.assertEqual(triggers['push']['branches'], ['main'])
        job = value['jobs']['dispatch']
        self.assertEqual(job['environment'], 'deployments')
        run = job['steps'][0]['run']
        # A deployment repository builds and verifies its own image from this revision.
        self.assertIn('gh workflow run release.yml', run)
        self.assertIn('-f revision="$GITHUB_SHA"', run)
        self.assertNotIn('image', run)
        # Self-hosters build from source: no workflow pushes or attests an image.
        for path in sorted((ROOT / '.github/workflows').glob('*.yml')):
            with self.subTest(workflow=path.name):
                text = path.read_text()
                self.assertNotRegex(text, r'packages:\s*write')
                self.assertNotIn('attestations:', text)


class DependabotTests(unittest.TestCase):
    def test_only_actions_get_version_updates_and_they_wait_two_weeks(self):
        value = yaml.safe_load((ROOT / '.github/dependabot.yml').read_text())
        self.assertEqual([update['package-ecosystem'] for update in value['updates']], ['github-actions'])
        self.assertGreaterEqual(value['updates'][0]['cooldown']['default-days'], 14)


if __name__ == '__main__':
    unittest.main()
