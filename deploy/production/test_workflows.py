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


if __name__ == '__main__':
    unittest.main()
