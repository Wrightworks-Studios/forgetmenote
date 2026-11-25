"""Exercise the one-command deployment through its tools and its recovery bucket boundary."""
import contextlib
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

import deploy_instance as deploy
import deploy_release


DIGEST = 'sha256:' + 'b' * 64
IMAGE = 'ghcr.io/example/notes@' + DIGEST
OTHER = 'ghcr.io/example/notes@sha256:' + 'c' * 64
TAG = 'ghcr.io/example/notes:main-' + 'd' * 40
PUBLICATION = {'source_revision': 'd' * 40, 'image_id': 'sha256:' + 'e' * 64, 'registry_digest': DIGEST, 'image': TAG}
VERIFICATION = {**PUBLICATION, 'complete': True, 'expected_checks': ['synthetic'],
                'checks': {'synthetic': {'status': 'passed', 'exit_code': 0}}}
FLAGS = ['--image', IMAGE, '--compatibility', 'compatible', '--compatibility-note', 'reviewed']


def value(args, flag):
    return args[args.index(flag) + 1]


class BucketFixture(unittest.TestCase):
    """Example configurations sharing one state root, with a recorded recovery bucket and tools."""

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        source = Path(__file__).parent
        self.state = (self.root / 'state').resolve()
        self.state.mkdir()
        self.owner = self.write('owner.json', 'deploy_instance.example.json')
        self.gateway = self.write('gateway.json', 'cutover.example.json')
        self.stored = {'releases/sha256-' + 'b' * 64 + '/publication.json': json.dumps(PUBLICATION),
                       'releases/sha256-' + 'b' * 64 + '/verification.json': json.dumps(VERIFICATION)}
        self.commands = []
        self.tools = []
        self.failing = set()

    def write(self, name, example):
        config = json.loads((Path(__file__).parent / example).read_text())
        path = self.root / name
        path.write_text(json.dumps({**config, 'state_root': str(self.state)}))
        return path

    def aws(self, args, **kwargs):
        self.commands.append(args)
        if 'get-object' in args:
            key = args[args.index('--key') + 1]
            if key not in self.stored:
                return None
            Path(args[-1]).write_text(self.stored[key])
        if 'put-object' in args:
            self.stored[args[args.index('--key') + 1]] = Path(args[args.index('--body') + 1]).read_text()
            return {'VersionId': 'version'}
        return {}

    def tool(self, name, args):
        self.tools.append((name, args))
        return 1 if (name, '--preflight' in args) in self.failing or (name, '--status' in args) in self.failing else 0

    def sync(self):
        return [args for args in self.commands if 'sync' in args]


class ReleaseTests(BucketFixture):
    def run_release(self, command, *flags):
        arguments = [command, '--owner-config', str(self.owner)]
        if command != 'store':
            arguments += ['--gateway-config', str(self.gateway)]
        with patch.object(deploy.Runtime, 'command', side_effect=self.aws), \
                patch('deploy_release.run_tool', side_effect=self.tool):
            return deploy_release.main([*arguments, *flags])

    def steps(self):
        return [(name, '--preflight' in args, '--status' in args) for name, args in self.tools]

    def deployment(self, name, image):
        directory = self.state / name
        directory.mkdir()
        (directory / 'deployment.json').write_text(json.dumps({'image': image, 'phase': 'starting',
                                                               'started_at': '2026-11-05T10:00:00+00:00'}))
        return directory

    def rollout(self, name, image, started_at='2026-11-05T10:05:00+00:00'):
        directory = self.state / name
        directory.mkdir()
        (directory / 'rollout.json').write_text(json.dumps({'image': image, 'phase': 'updating', 'started_at': started_at}))
        return directory

    def test_a_release_preflights_both_roles_then_deploys_the_owner_first(self):
        self.assertEqual(self.run_release('deploy', *FLAGS), 0)
        self.assertEqual(self.steps(), [('deploy_instance.py', True, False), ('roll_gateways.py', True, False),
                                        ('deploy_instance.py', False, False), ('roll_gateways.py', False, False),
                                        ('deploy_instance.py', False, True)])
        owner = self.tools[2][1]
        self.assertEqual(owner[owner.index('--compatibility') + 1], 'compatible')
        self.assertEqual(owner[owner.index('--compatibility-note') + 1], 'reviewed')
        gateway = self.tools[3][1]
        self.assertEqual(gateway[gateway.index('--config') + 1], str(self.gateway))
        self.assertNotIn('--compatibility', gateway)
        status = self.tools[4][1]
        self.assertIn('--wait', status)
        self.assertNotIn('--release-dir', status)

    def test_accepted_failures_reach_the_owner_tool(self):
        self.run_release('deploy', *FLAGS, '--accept-failed-backup', 'job-1', '--accepted-failure-note', 'reviewed job')
        for args in (self.tools[0][1], self.tools[2][1]):
            self.assertEqual(args[args.index('--accept-failed-backup') + 1], 'job-1')
            self.assertEqual(args[args.index('--accepted-failure-note') + 1], 'reviewed job')

    def test_preflight_runs_both_roles_even_when_the_owner_fails_and_changes_nothing(self):
        self.failing.add(('deploy_instance.py', True))
        self.assertEqual(self.run_release('preflight', *FLAGS), 1)
        self.assertEqual(self.steps(), [('deploy_instance.py', True, False), ('roll_gateways.py', True, False)])

    def test_a_failed_preflight_stops_before_the_owner_is_touched(self):
        self.failing.add(('roll_gateways.py', True))
        self.assertEqual(self.run_release('deploy', *FLAGS), 1)
        self.assertEqual(self.steps(), [('deploy_instance.py', True, False), ('roll_gateways.py', True, False)])

    def test_a_failed_owner_never_rolls_the_gateways_and_keeps_its_records(self):
        self.failing.add(('deploy_instance.py', False))
        self.assertEqual(self.run_release('deploy', *FLAGS), 1)
        self.assertNotIn(('roll_gateways.py', False, False), self.steps())
        self.assertEqual(len(self.sync()), 2)

    def test_resume_finishes_the_owner_then_rolls_gateways_the_release_never_reached(self):
        owner = self.deployment('deploy-20261105-abcdef0', IMAGE)
        # Resuming last release's finished rollout would report success and leave the old image serving.
        self.rollout('gateways-last-release', OTHER)
        self.assertEqual(self.run_release('resume', '--image', IMAGE), 0)
        (first, finish), (second, gateways), (third, status) = self.tools
        self.assertEqual((first, second, third), ('deploy_instance.py', 'roll_gateways.py', 'deploy_instance.py'))
        self.assertIn('--resume', finish)
        self.assertEqual(value(finish, '--state-dir'), str(owner))
        self.assertIn('--release-dir', gateways)
        self.assertNotIn('--resume', gateways)
        self.assertIn('--wait', status)
        self.assertEqual(value(status, '--state-dir'), str(owner))

    def test_resume_continues_the_rollout_of_this_release(self):
        self.deployment('deploy-20261105-abcdef0', IMAGE)
        rollout = self.rollout('gateways-this-release', IMAGE)
        self.assertEqual(self.run_release('resume', '--image', IMAGE), 0)
        gateways = self.tools[1][1]
        self.assertIn('--resume', gateways)
        self.assertEqual(value(gateways, '--state-dir'), str(rollout))
        self.assertNotIn('--release-dir', gateways)

    def test_resume_stops_at_the_role_that_cannot_continue_and_keeps_its_records(self):
        self.deployment('deploy-20261105-abcdef0', IMAGE)
        self.failing.add(('deploy_instance.py', False))
        self.assertEqual(self.run_release('resume', '--image', IMAGE), 1)
        self.assertEqual([name for name, _ in self.tools], ['deploy_instance.py'])
        self.assertEqual(len(self.sync()), 2)

    def test_resume_refuses_a_release_that_never_started(self):
        self.deployment('deploy-20261105-abcdef0', OTHER)
        self.assertEqual(self.run_release('resume', '--image', IMAGE), 1)
        self.assertEqual(self.tools, [])

    def test_rollback_returns_only_the_gateways_of_this_release(self):
        # Receipts cannot be allowed to stand between an operator and a rollback.
        self.stored.clear()
        self.deployment('deploy-20261105-abcdef0', IMAGE)
        rollout = self.rollout('gateways-this-release', IMAGE)
        self.assertEqual(self.run_release('rollback-gateways', '--image', IMAGE), 0)
        ((name, args),) = self.tools
        self.assertEqual(name, 'roll_gateways.py')
        self.assertIn('--rollback', args)
        self.assertEqual(value(args, '--state-dir'), str(rollout))
        self.assertEqual(len(self.sync()), 2)

    def test_the_storage_cutover_runs_with_the_stored_receipts_and_keeps_its_records(self):
        self.assertEqual(self.run_release('storage-cutover', '--image', IMAGE, '--preflight'), 0)
        ((name, args),) = self.tools
        self.assertEqual(name, 'storage_cutover.py')
        self.assertEqual(value(args, '--owner-config'), str(self.owner))
        self.assertEqual(value(args, '--gateway-config'), str(self.gateway))
        self.assertIn('--preflight', args)
        self.assertNotIn('--resume', args)
        self.assertEqual(len(self.sync()), 2)
        self.tools.clear()
        self.assertEqual(self.run_release('storage-cutover', '--image', IMAGE, '--rollback', '--discard-v2-writes'), 0)
        self.assertIn('--rollback', self.tools[0][1])
        self.assertIn('--discard-v2-writes', self.tools[0][1])
        with self.assertRaises(SystemExit), contextlib.redirect_stderr(io.StringIO()):
            self.run_release('storage-cutover', '--image', IMAGE, '--resume', '--rollback')

    def test_rollback_refuses_a_rollout_a_later_release_replaced(self):
        # Its previous image is older still, so going back would skip a release.
        self.rollout('gateways-old', IMAGE, '2026-11-05T09:00:00+00:00')
        self.rollout('gateways-new', OTHER, '2026-11-05T10:00:00+00:00')
        self.assertEqual(self.run_release('rollback-gateways', '--image', IMAGE), 1)
        self.assertEqual(self.tools, [])

    def test_rollback_refuses_when_no_rollout_exists(self):
        self.assertEqual(self.run_release('rollback-gateways', '--image', IMAGE), 1)
        self.assertEqual(self.tools, [])

    def test_a_rerun_meets_the_stopped_attempts_refusal_unless_a_fresh_attempt_is_asked_for(self):
        self.run_release('deploy', *FLAGS)
        self.assertNotIn('--state-dir', self.tools[2][1])
        self.tools.clear()
        self.assertEqual(self.run_release('deploy', *FLAGS, '--fresh-attempt'), 0)
        self.assertNotIn('--state-dir', self.tools[0][1])
        attempt = Path(value(self.tools[2][1], '--state-dir'))
        self.assertEqual(attempt.parent, self.state)
        self.assertTrue(attempt.name.startswith('deploy-'))

    def test_an_unfinished_upgrade_backup_fails_the_run(self):
        original = self.tool
        def tool(name, args):
            code = original(name, args)
            return 1 if '--status' in args else code
        self.tool = tool
        self.assertEqual(self.run_release('deploy', *FLAGS), 1)

    def test_records_come_from_the_bucket_first_and_return_after_a_run(self):
        self.run_release('preflight', *FLAGS)
        pull, push = self.sync()
        self.assertEqual(pull[pull.index('sync') + 1:pull.index('sync') + 3], ['s3://forgetmenote-recovery-123456789012-us-east-1/deployments/', str(self.state)])
        self.assertEqual(push[push.index('sync') + 1:push.index('sync') + 3], [str(self.state), 's3://forgetmenote-recovery-123456789012-us-east-1/deployments/'])
        for name in ('deploy-*/*', 'gateways-*/*', 'preflight-*/*', 'release-*/*'):
            self.assertIn(name, push)
        self.assertIn('*.tar.gz', push)
        self.assertEqual(push[push.index('--sse') + 1], 'aws:kms')
        for args in (pull, push):
            self.assertNotIn('--delete', args)

    def test_records_are_returned_even_when_a_tool_cannot_start(self):
        def broken(name, args):
            raise OSError('no python')
        self.tool = broken
        self.assertEqual(self.run_release('deploy', *FLAGS), 1)
        self.assertEqual(len(self.sync()), 2)

    def test_an_image_nobody_stored_is_refused_before_any_tool_runs(self):
        self.stored.clear()
        self.assertEqual(self.run_release('deploy', *FLAGS), 1)
        self.assertEqual(self.tools, [])

    def test_receipts_must_describe_the_requested_image(self):
        other = 'sha256:' + 'c' * 64
        for key in list(self.stored):
            self.stored[key] = self.stored[key].replace(DIGEST, other)
        self.assertEqual(self.run_release('deploy', *FLAGS), 1)
        self.assertEqual(self.tools, [])

    def test_only_a_pinned_image_can_be_deployed(self):
        self.assertEqual(self.run_release('deploy', '--image', TAG, '--compatibility', 'migration'), 1)
        self.assertEqual(self.tools, [])
        self.assertEqual(self.commands, [])

    def test_storing_a_release_locks_its_receipts_under_its_digest(self):
        self.stored.clear()
        release = self.root / 'release'
        release.mkdir()
        (release / 'publication.json').write_text(json.dumps(PUBLICATION))
        (release / 'verification.json').write_text(json.dumps(VERIFICATION))
        self.assertEqual(self.run_release('store', '--release-dir', str(release)), 0)
        self.assertEqual(sorted(self.stored), ['releases/sha256-' + 'b' * 64 + '/publication.json',
                                               'releases/sha256-' + 'b' * 64 + '/verification.json'])
        upload = next(args for args in self.commands if 'put-object' in args)
        self.assertEqual(upload[upload.index('--object-lock-mode') + 1], 'GOVERNANCE')

    def test_storing_refuses_a_release_without_complete_verification(self):
        release = self.root / 'release'
        release.mkdir()
        (release / 'publication.json').write_text(json.dumps(PUBLICATION))
        (release / 'verification.json').write_text(json.dumps({**VERIFICATION, 'complete': False}))
        self.assertEqual(self.run_release('store', '--release-dir', str(release)), 1)
        self.assertFalse([args for args in self.commands if 'put-object' in args])

    def test_both_roles_must_share_one_state_root(self):
        self.gateway.write_text(json.dumps({**json.loads(self.gateway.read_text()), 'state_root': str(self.root / 'other')}))
        self.assertEqual(self.run_release('preflight', *FLAGS), 1)
        self.assertEqual(self.tools, [])



ECR = '123456789012.dkr.ecr.us-east-1.amazonaws.com/notes'
# Answers ECR's describe-images for the tags listed in PUBLISHED_TAGS, as the AWS CLI does.
AWS = r'''
import json, os, sys
args = sys.argv[1:]
with open(os.environ['AWS_CALLS'], 'a') as log:
    log.write(json.dumps(args) + '\n')
tag = args[args.index('--image-ids') + 1].removeprefix('imageTag=')
if tag not in os.environ['PUBLISHED_TAGS'].split():
    sys.exit('An error occurred (ImageNotFoundException) when calling the DescribeImages operation')
print(json.dumps({'imageDetails': [{'imageDigest': 'sha256:' + tag[-1] * 64}]}))
'''


class PipelineQueryTests(BucketFixture):
    """What a release pipeline asks before it verifies or deploys, against real Git history."""

    def setUp(self):
        super().setUp()
        self.repo = self.root / 'repo'
        (self.repo / 'docs').mkdir(parents=True)
        self.git('init', '-b', 'main')
        self.git('config', 'user.name', 'Pipeline fixture')
        self.git('config', 'user.email', 'fixture@localhost')
        self.commit('main.go', 'package main\n')
        tools = self.root / 'tools'
        tools.mkdir()
        (tools / 'aws').write_text('#!' + sys.executable + '\n' + AWS)
        (tools / 'aws').chmod(0o755)
        self.lookups = self.root / 'aws.jsonl'
        environment = patch.dict(os.environ, {'PATH': str(tools) + os.pathsep + os.environ['PATH'],
                                              'AWS_CALLS': str(self.lookups), 'PUBLISHED_TAGS': ''})
        environment.start()
        self.addCleanup(environment.stop)
        root = patch('deploy_release.ROOT', self.repo)
        root.start()
        self.addCleanup(root.stop)

    def git(self, *args):
        return subprocess.run(['git', '-C', str(self.repo), *args], check=True, capture_output=True, text=True).stdout.strip()

    def commit(self, path, content):
        (self.repo / path).write_text(content)
        self.git('add', '.')
        self.git('commit', '-m', 'Change ' + path)
        return self.git('rev-parse', 'HEAD')

    def query(self, command, *flags):
        output = io.StringIO()
        with contextlib.redirect_stdout(output):
            code = self.run_release(command, *flags)
        return code, output.getvalue()

    def run_release(self, command, *flags):
        with patch.object(deploy.Runtime, 'command', side_effect=self.aws), \
                patch('deploy_release.run_tool', side_effect=self.tool):
            return deploy_release.main([command, '--owner-config', str(self.owner), *flags])

    def ready(self, revision):
        directory = self.state / ('deploy-20261105-' + revision[:7])
        directory.mkdir()
        publication = {**PUBLICATION, 'source_revision': revision, 'image': 'ghcr.io/example/notes:main-' + revision}
        (directory / 'publication.json').write_text(json.dumps(publication))
        (directory / 'verification.json').write_text(json.dumps({**VERIFICATION, **publication}))
        (directory / 'deployment.json').write_text(json.dumps({'image': IMAGE, 'phase': 'ready',
                                                               'started_at': '2026-11-05T10:00:00+00:00'}))

    def test_deployed_reports_the_live_release_and_the_runtime_changes_since(self):
        live = self.git('rev-parse', 'HEAD')
        self.ready(live)
        self.commit('main.go', 'package main\n// next\n')
        self.commit('docs/notes.md', 'Operator notes\n')
        code, output = self.query('deployed')
        self.assertEqual(code, 0)
        self.assertEqual(json.loads(output), {'revision': live, 'image': IMAGE, 'runtime_changes': ['main.go']})
        # It reads the bucket's records and returns nothing to it.
        self.assertEqual([args[args.index('sync') + 1] for args in self.sync()], ['s3://forgetmenote-recovery-123456789012-us-east-1/deployments/'])
        self.assertEqual(self.tools, [])

    def test_deployed_needs_a_ready_release(self):
        self.assertEqual(self.query('deployed'), (1, ''))

    def test_prepared_names_the_image_published_for_the_checkouts_runtime(self):
        published = self.git('rev-parse', 'HEAD')
        os.environ['PUBLISHED_TAGS'] = 'main-' + published
        self.commit('docs/notes.md', 'Operator notes\n')
        code, output = self.query('prepared', '--repository', ECR)
        self.assertEqual(code, 0)
        # Only the reference reaches stdout, so a workflow can capture it.
        self.assertEqual(output, ECR + '@sha256:' + published[-1] * 64 + '\n')
        lookup = json.loads(self.lookups.read_text().splitlines()[-1])
        self.assertEqual(lookup[:2], ['--profile', 'notes-admin'])
        self.assertIn('imageTag=main-' + published, lookup)

    def test_prepared_never_offers_an_image_older_than_the_runtime(self):
        os.environ['PUBLISHED_TAGS'] = 'main-' + self.git('rev-parse', 'HEAD')
        newer = self.commit('main.go', 'package main\n// unpublished\n')
        self.assertEqual(self.query('prepared', '--repository', ECR), (1, ''))
        self.assertEqual([json.loads(line)[-1] for line in self.lookups.read_text().splitlines()], ['imageTag=main-' + newer])

    def test_prepared_accepts_only_a_private_registry_repository(self):
        self.assertEqual(self.query('prepared', '--repository', 'docker.io/example/notes'), (1, ''))
        self.assertFalse(self.lookups.exists())


if __name__ == '__main__':
    unittest.main()
