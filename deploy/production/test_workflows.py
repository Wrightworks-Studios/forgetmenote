"""GitHub workflows keep the promises SECURITY.md and docs/verification.md make."""
from pathlib import Path
import re
import subprocess
import tempfile
import unittest

import yaml

ROOT = Path(__file__).resolve().parents[2]


def workflow(name):
    value = yaml.safe_load((ROOT / '.github/workflows' / name).read_text())
    # YAML 1.1 reads the bare key "on" as true.
    return value, value.get('on', value.get(True))


def assert_failed_gate_uploads_its_evidence(test, steps):
    """The gate prints only a summary of a failed log; GitHub discards the rest with the runner."""
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
        self.assertEqual(value['jobs']['full']['if'], "github.event_name == 'workflow_dispatch' || github.event_name == 'merge_group'")
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

    def test_queue_entries_decide_the_full_gate_inside_the_required_job(self):
        # next can require full: a pull request run skips it, and GitHub reports a
        # skipped job as Success. The same holds for a job whose `needs` failed, so
        # the decision is a step of full itself, where an error fails the check.
        value, _ = workflow('check.yml')
        job = value['jobs']['full']
        self.assertNotIn('needs', job)
        self.assertEqual(job['permissions'], {'contents': 'read', 'pull-requests': 'read'})
        steps = job['steps']
        self.assertTrue(steps[0]['uses'].startswith('actions/checkout@'))
        self.assertIs(steps[0]['with']['persist-credentials'], False)
        decide = steps[1]
        self.assertEqual(decide['id'], 'gate')
        self.assertEqual(decide['run'], '.github/scripts/full-gate-needed.sh')
        self.assertEqual(decide['env'], {'GH_TOKEN': '${{ github.token }}'})
        self.assertNotIn('if', decide)
        self.assertNotIn('continue-on-error', decide)
        # An entry without the label succeeds at once; every later step runs only
        # for the gate, and still stops at the first failure.
        upload = next(step for step in steps if step.get('if') == 'failure()')
        for step in steps[2:]:
            if step is not upload:
                with self.subTest(step=step.get('uses') or step.get('run')):
                    self.assertEqual(step.get('if'), "steps.gate.outputs.run == 'true'")
                    self.assertNotIn('continue-on-error', step)
        # The queue drops an entry whose checks report nothing within 90 minutes. A
        # stuck gate fails first, with time left to pick up a runner and upload its logs.
        gate = next(step for step in steps if 'release_image.py' in step.get('run', ''))
        self.assertLessEqual(gate['timeout-minutes'], 65)
        self.assertLess(gate['timeout-minutes'], job['timeout-minutes'])
        self.assertLessEqual(job['timeout-minutes'], 80)

    def test_every_action_is_pinned_to_a_commit(self):
        for path in sorted((ROOT / '.github/workflows').glob('*.yml')):
            for line in path.read_text().splitlines():
                found = re.match(r'\s*(?:-\s+)?uses:\s*(\S+)', line)
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


class FullGateDecisionTests(unittest.TestCase):
    """full-gate-needed.sh: whether a full job runs the gate or succeeds at once."""

    SCRIPT = ROOT / '.github/scripts/full-gate-needed.sh'
    QUEUE_REF = 'refs/heads/gh-readonly-queue/next/pr-63-' + '1802893ecaaf4e433c682c65976add8f9114bcd7'

    def decide(self, event, ref=QUEUE_REF, labels='', gh_fails=False):
        with tempfile.TemporaryDirectory() as directory:
            directory = Path(directory)
            gh = directory / 'gh'
            gh.write_text('#!/bin/sh\nprintf \'%s\\n\' "$*" >>"$FAKE_GH_CALLS"\n'
                          '[ -z "$FAKE_GH_FAILS" ] || exit 1\nprintf \'%s\' "$FAKE_GH_LABELS"\n')
            gh.chmod(0o755)
            output = directory / 'output'
            calls = directory / 'calls'
            output.touch()
            calls.touch()
            result = subprocess.run([str(self.SCRIPT)], capture_output=True, text=True, env={
                'PATH': f'{directory}:/usr/bin:/bin',
                'GITHUB_EVENT_NAME': event,
                'GITHUB_REF': ref,
                'GITHUB_REPOSITORY': 'example/forgetmenote',
                'GITHUB_OUTPUT': str(output),
                'GH_TOKEN': 'test-token',
                'FAKE_GH_CALLS': str(calls),
                'FAKE_GH_LABELS': labels,
                'FAKE_GH_FAILS': '1' if gh_fails else '',
            })
            return result, output.read_text(), calls.read_text().splitlines()

    def test_a_manual_run_always_runs_the_gate(self):
        result, output, calls = self.decide('workflow_dispatch', ref='refs/heads/feature', gh_fails=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(output, 'run=true\n')
        self.assertEqual(calls, [])

    def test_a_queue_entry_runs_the_gate_when_its_own_pull_request_has_the_label(self):
        result, output, calls = self.decide('merge_group', labels='documentation\nfull-gate\n')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(output, 'run=true\n')
        # The entry's ref names its own pull request; the label is read when the entry runs.
        self.assertEqual(calls, ["api repos/example/forgetmenote/pulls/63 --jq .labels[].name"])

    def test_a_queue_entry_without_the_label_succeeds_at_once(self):
        for labels in ('', 'documentation\n', 'full-gate-later\nnot-full-gate\n', 'Full-Gate\n'):
            with self.subTest(labels=labels):
                result, output, _ = self.decide('merge_group', labels=labels)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(output, 'run=false\n')

    def test_a_queue_entry_fails_when_its_pull_request_cannot_be_read(self):
        result, output, _ = self.decide('merge_group', labels='full-gate\n', gh_fails=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(output, '')

    def test_an_unrecognised_ref_or_event_fails_instead_of_passing(self):
        sha = '1802893ecaaf4e433c682c65976add8f9114bcd7'
        for event, ref in (
                ('merge_group', 'refs/heads/next'),
                ('merge_group', f'refs/heads/gh-readonly-queue/next/pr--{sha}'),
                ('merge_group', f'refs/heads/gh-readonly-queue/next/pr-63-{sha}\nrefs/heads/next'),
                ('pull_request', self.QUEUE_REF),
                ('push', 'refs/heads/next'),
                ('', self.QUEUE_REF)):
            with self.subTest(event=event, ref=ref):
                result, output, calls = self.decide(event, ref=ref, labels='full-gate\n')
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(output, '')
                self.assertEqual(calls, [])


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


class CLAWorkflowTests(unittest.TestCase):
    def test_the_cla_check_never_runs_pull_request_code_with_its_write_token(self):
        value, triggers = workflow('cla.yml')
        self.assertEqual(set(triggers), {'pull_request_target', 'issue_comment', 'merge_group'})
        self.assertEqual(value['permissions'], {})
        # A check run named cla could satisfy the required cla status by itself.
        self.assertNotIn('cla', value['jobs'])
        job, = value['jobs'].values()
        self.assertEqual(job['permissions'], {'contents': 'write', 'pull-requests': 'write', 'statuses': 'write'})
        checkout, run = job['steps']
        self.assertTrue(checkout['uses'].startswith('actions/checkout@'))
        # Without a ref, pull_request_target checks out the base branch, not the pull request.
        self.assertEqual(set(checkout['with']), {'persist-credentials', 'sparse-checkout'})
        self.assertIs(checkout['with']['persist-credentials'], False)
        self.assertEqual(run['run'], 'python3 .github/scripts/cla.py')
        # Comment and pull request text reach the script only through the event file.
        self.assertNotRegex((ROOT / '.github/workflows/cla.yml').read_text(),
                            r'\$\{\{\s*github\.event\.(comment|issue|pull_request)\.(body|title|head)')


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
