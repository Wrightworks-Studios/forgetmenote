"""Exercise the cutover's order, refusals and recovery through its cloud boundary."""
import datetime as dt
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import cutover


NOW = dt.datetime(2026, 11, 4, tzinfo=dt.timezone.utc)
IMAGE = 'ghcr.io/example/notes@sha256:' + 'b' * 64
REPORT = [{'area': 'catalog', 'files': 3, 'digest': 'c' * 64}, {'area': 'chat', 'files': 1, 'digest': 'd' * 64}]
MANIFEST = 'e' * 64 + '\n12\n'


class Cloud:
    """The services, database and pods; exec answers by command."""
    def __init__(self, directory):
        self.directory = Path(directory)
        self.calls = []
        self.config = {'owner_entry': 'pool'}
        self.exported = REPORT
        self.copied = MANIFEST
        self.owner_empty = True
        self.fail = set()

    def now(self):
        return NOW

    def save(self, name, value):
        (self.directory / name).write_text(json.dumps(value))

    def protect(self, name, value):
        self.calls.append('protect:' + name)

    def stop(self):
        self.calls.append('stop')

    def wait_stopped(self):
        self.calls.append('stopped')

    def start(self):
        self.calls.append('start')

    def wait_running(self):
        self.calls.append('running')

    def start_backup(self, run_id):
        self.calls.append('backup')
        return 'job-1'

    def observe_snapshot(self, receipt):
        self.calls.append('snapshot')
        return {'id': 'snap-1'}

    def complete_backup(self, receipt):
        self.calls.append('completed')
        return 'arn:aws:ec2:us-east-1::snapshot/snap-1'

    def restore_point(self):
        self.calls.append('restore-point')
        return {'instance': 'notes', 'latest_restorable_time': NOW.isoformat()}

    def running(self, role):
        self.calls.append('running:' + role)
        return role + '-0'

    def start_service(self, role):
        self.calls.append('start:' + role)

    def stop_service(self, role):
        self.calls.append('stop:' + role)

    def ready(self, role):
        self.calls.append('ready:' + role)
        return [role + '-a', role + '-b']

    def copy(self, source_pod, source_root, target_pod, target_root):
        self.calls.append('copy')

    def exec(self, pod, args):
        name = ' '.join(args)
        self.calls.append(pod + ': ' + name)
        for failure in self.fail:
            if failure in name:
                self.fail.discard(failure)
                raise RuntimeError('in-cluster command failed: ' + failure)
        if 'import-files' in name:
            return 'migrating\n' + json.dumps({'imported': REPORT}) + '\n'
        if 'export-files' in name:
            return json.dumps({'exported': self.exported}) + '\n'
        if 'manifest' in name:
            return MANIFEST if cutover.SOURCE in args else self.copied
        if 'test ! -e /data/projects' in name:
            return 'empty\n' if self.owner_empty else ''
        if 'move-notebook' in name:
            return json.dumps({'notebooks': ['default', 'team'], 'owner': 'http://owner:8080'}) + '\n'
        return ''


class CutoverTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.cloud = Cloud(self.directory.name)
        self.receipt = {'id': 'cutover-1', 'rehearsal': False, 'phase': 'prepared'}

    def tearDown(self):
        self.directory.cleanup()

    def stored(self):
        return json.loads((Path(self.directory.name) / 'cutover.json').read_text())

    def test_every_phase_runs_in_order_and_records_its_outcome(self):
        cutover.cutover(self.cloud, self.receipt)
        calls = self.cloud.calls
        order = ['stop', 'stopped', 'backup', 'snapshot', 'protect:capture.json', 'completed', 'protect:backup-result.json',
                 'helper-0: forgetmenote migrate', 'helper-0: forgetmenote import-files -data /source',
                 'helper-0: forgetmenote export-files -output /tmp/cutover-roundtrip', 'copy',
                 'owner-0: mv /data/.cutover-projects/projects /data/projects',
                 'helper-0: forgetmenote admin move-notebook -data /tmp/cutover-gateway -all-local -owner pool',
                 'stop:helper', 'start:gateway', 'gateway-a: forgetmenote healthz', 'gateway-b: forgetmenote healthz',
                 'protect:cutover-result.json']
        self.assertEqual([call for call in calls if call in order], order)
        # The volume is captured before anything reads it, and released before gateways start.
        self.assertLess(calls.index('completed'), calls.index('running:helper'))
        self.assertLess(calls.index('stop:helper'), calls.index('start:gateway'))
        stored = self.stored()
        self.assertEqual(stored['phase'], 'serving')
        self.assertEqual(stored['imported'], REPORT)
        self.assertEqual(stored['notebook_files'], {'digest': 'e' * 64, 'files': 12})
        self.assertEqual(stored['placed']['notebooks'], ['default', 'team'])
        self.assertEqual(stored['recovery_point'], 'arn:aws:ec2:us-east-1::snapshot/snap-1')
        self.assertIn('database_before', stored)
        self.assertIn('database_after', stored)

    def test_an_export_that_differs_from_the_import_stops_before_any_copy(self):
        self.cloud.exported = [REPORT[0], {**REPORT[1], 'digest': 'f' * 64}]
        with self.assertRaisesRegex(RuntimeError, 'differs in: chat'):
            cutover.cutover(self.cloud, self.receipt)
        self.assertNotIn('copy', self.cloud.calls)
        self.assertEqual(self.stored()['phase'], 'imported')
        self.assertNotIn('start:gateway', self.cloud.calls)

    def test_an_owner_that_already_holds_notebooks_is_never_written(self):
        self.cloud.owner_empty = False
        with self.assertRaisesRegex(RuntimeError, 'already holds notebooks'):
            cutover.cutover(self.cloud, self.receipt)
        self.assertFalse([call for call in self.cloud.calls if call.startswith('owner-0: rm') or call == 'copy'])

    def test_a_copy_that_hashes_differently_places_nothing(self):
        self.cloud.copied = 'a' * 64 + '\n11\n'
        with self.assertRaisesRegex(RuntimeError, 'differ from the stopped volume'):
            cutover.cutover(self.cloud, self.receipt)
        self.assertFalse([call for call in self.cloud.calls if ' mv ' in call or 'move-notebook' in call])
        self.assertEqual(self.stored()['phase'], 'verified')

    def test_resuming_repeats_only_the_interrupted_phase(self):
        self.cloud.fail = {'move-notebook'}
        with self.assertRaisesRegex(RuntimeError, 'move-notebook'):
            cutover.cutover(self.cloud, self.receipt)
        self.assertEqual(self.stored()['phase'], 'copied')
        self.cloud.calls.clear()
        cutover.cutover(self.cloud, self.stored())
        self.assertFalse([call for call in self.cloud.calls if 'import-files' in call or call in ('stop', 'backup', 'copy')])
        self.assertIn('helper-0: forgetmenote admin move-notebook -data /tmp/cutover-gateway -all-local -owner pool', self.cloud.calls)
        self.assertEqual(self.stored()['phase'], 'serving')

    def test_an_interrupted_copy_restarts_from_an_empty_staging_directory(self):
        self.cloud.fail = {'manifest /data/.cutover-projects'}
        with self.assertRaises(RuntimeError):
            cutover.cutover(self.cloud, self.receipt)
        self.cloud.calls.clear()
        cutover.cutover(self.cloud, self.stored())
        calls = self.cloud.calls
        self.assertLess(calls.index('owner-0: rm -rf /data/.cutover-projects'), calls.index('copy'))
        self.assertEqual(self.stored()['phase'], 'serving')

    def test_a_rehearsal_leaves_the_file_based_service_running(self):
        self.receipt['rehearsal'] = True
        cutover.cutover(self.cloud, self.receipt)
        for call in ('stop', 'stopped', 'backup', 'snapshot', 'completed', 'start'):
            self.assertNotIn(call, self.cloud.calls)
        self.assertEqual(self.stored()['phase'], 'serving')

    def test_rollback_after_serving_needs_confirmation_that_no_write_was_accepted(self):
        cutover.cutover(self.cloud, self.receipt)
        self.cloud.calls.clear()
        with self.assertRaisesRegex(RuntimeError, 'export-files'):
            cutover.rollback(self.cloud, self.stored())
        self.assertEqual(self.cloud.calls, [])
        cutover.rollback(self.cloud, {**self.stored(), 'writes_confirmed_absent': True})
        self.assertEqual(self.cloud.calls, ['stop:gateway', 'stop:helper', 'start', 'running', 'protect:cutover-rollback.json'])

    def test_rollback_before_serving_restarts_the_file_based_service(self):
        self.cloud.fail = {'export-files'}
        with self.assertRaises(RuntimeError):
            cutover.cutover(self.cloud, self.receipt)
        self.cloud.calls.clear()
        cutover.rollback(self.cloud, self.stored())
        self.assertEqual(self.cloud.calls[:4], ['stop:gateway', 'stop:helper', 'start', 'running'])


class ManifestTests(unittest.TestCase):
    def test_reports_compare_by_files_and_digest(self):
        self.assertEqual(cutover.report_mismatches(REPORT, REPORT), [])
        self.assertEqual(cutover.report_mismatches(REPORT, REPORT[:1]), ['chat'])
        self.assertEqual(cutover.report_mismatches(REPORT, [REPORT[0], {**REPORT[1], 'files': 2}]), ['chat'])

    def test_a_malformed_manifest_is_refused(self):
        self.assertEqual(cutover.manifest(MANIFEST), {'digest': 'e' * 64, 'files': 12})
        for bad in ('', 'e' * 63 + '\n1\n', MANIFEST + 'extra'):
            with self.assertRaises(RuntimeError):
                cutover.manifest(bad)


BASE = {'name': 'forgetmenote', 'namespace': 'duploservices-notes', 'claim': 'data', 'volume': 'vol-1', 'region': 'us-east-1',
        'account': '123456789012', 'vault': 'v', 'backup_role': 'r', 'bucket': 'b', 'kms_key': 'k', 'export_function': 'f',
        'aws_profile': 'p', 'kubeconfig': '/k', 'duplo_host': 'https://duplo.example', 'duplo_tenant': 't',
        'public_url': 'https://notes.example.com', 'owner_entry': 'pool', 'database_instance': 'notes', 'blob_bucket': 'blobs',
        'services': {'helper': {'name': 'notes-cutover'}, 'owner': {'name': 'notes-owner'},
                     'gateway': {'name': 'notes-gateway', 'replicas': 2}}}


def service(name, image=IMAGE, env=None, keys=('Name', 'Value')):
    """A service as `duploctl service find` returns it, with Kubernetes fields capitalized."""
    return {'Name': name, 'Template': {'Containers': [{'Image': image}],
                                      'OtherDockerConfig': json.dumps({'Env': [dict(zip(keys, item)) for item in (env or {}).items()]})}}


class PreflightTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.path = Path(self.directory.name) / 'cutover.json'

    def tearDown(self):
        self.directory.cleanup()

    def configure(self, **changes):
        self.path.write_text(json.dumps({**BASE, **changes}))
        return cutover.configuration(self.path)

    def test_configuration_names_three_new_services(self):
        self.assertEqual(self.configure()['services']['gateway']['replicas'], 2)
        for change in ({'services': {'helper': {'name': 'a'}, 'owner': {'name': 'b'}}},
                       {'services': {**BASE['services'], 'owner': {'name': 'forgetmenote'}}},
                       {'services': {**BASE['services'], 'gateway': {'name': 'g', 'replicas': 0}}},
                       {'services': {**BASE['services'], 'gateway': {'name': 'notes-owner'}}},
                       {'blob_bucket': ''}, {'rehearsal': 'yes'}):
            with self.assertRaises(ValueError):
                self.configure(**change)

    def test_the_example_configuration_rehearses(self):
        config = cutover.configuration(Path(__file__).with_name('cutover.example.json'))
        self.assertTrue(config['rehearsal'])

    def preflight(self, rehearsal=False, gateway_env=None, database=None, versioning='Enabled', running=(), exec_allowed=True,
                  keys=('Name', 'Value')):
        config = self.configure()
        cloud = cutover.Runtime(config, Path(self.directory.name))
        services = {'notes-cutover': service('notes-cutover'), 'notes-owner': service('notes-owner'),
                    'notes-gateway': service('notes-gateway', keys=keys,
                                             env=gateway_env if gateway_env is not None else {'PUBLIC_URL': config['public_url']})}
        instance = {'DBInstanceIdentifier': 'notes', 'DBInstanceStatus': 'available', 'StorageEncrypted': True,
                    'PubliclyAccessible': False, 'BackupRetentionPeriod': 7, 'DeletionProtection': True, **(database or {})}

        def command(args, **kwargs):
            if 'can-i' in args:
                if not exec_allowed:
                    raise RuntimeError('Operator command failed (1)')
                return {}
            if 'service' in args and 'find' in args:
                return services[args[-1]]
            if 'get' in args and 'pods' in args:
                selector = args[args.index('-l') + 1] if '-l' in args else ''
                return {'items': [{}] if any(name in selector for name in running) or ('--field-selector' in args and 'file' in running) else []}
            if 'describe-db-instances' in args:
                return {'DBInstances': [instance]}
            if 'get-bucket-versioning' in args:
                return {'Status': versioning}
            if 'get-public-access-block' in args:
                return {'PublicAccessBlockConfiguration': dict.fromkeys(('BlockPublicAcls', 'IgnorePublicAcls', 'BlockPublicPolicy', 'RestrictPublicBuckets'), True)}
            return {}
        with patch.object(cloud, 'command', side_effect=command):
            return cloud.preflight({'reference': IMAGE}, rehearsal)

    def test_a_ready_stack_passes(self):
        self.assertEqual(self.preflight(running=('file',)), [])

    def test_each_unready_part_is_named(self):
        problems = self.preflight(database={'PubliclyAccessible': True, 'DeletionProtection': False}, versioning='Suspended',
                                  running=('notes-gateway', 'notes-cutover'), gateway_env={'PUBLIC_URL': 'https://other.example'},
                                  exec_allowed=False)
        for expected in ('gateways must serve the public origin', 'gateways are running', 'the helper is running', 'run commands in pods',
                         'the file-based service is not running', 'encrypted and private', 'deletion protection', 'versioning'):
            self.assertTrue(any(expected in problem for problem in problems), (expected, problems))

    def test_rehearsal_gateways_must_pause_outgoing_duties_on_an_origin_of_their_own(self):
        problems = self.preflight(rehearsal=True, gateway_env={'PUBLIC_URL': BASE['public_url']})
        self.assertTrue(any('OUTGOING=paused' in problem for problem in problems), problems)
        self.assertTrue(any('origin of their own' in problem for problem in problems), problems)
        for keys in (('Name', 'Value'), ('name', 'value')):
            with self.subTest(keys=keys):
                self.assertEqual(self.preflight(rehearsal=True, keys=keys,
                                                gateway_env={'PUBLIC_URL': 'https://rehearsal.example.com', 'OUTGOING': 'paused'}), [])


if __name__ == '__main__':
    unittest.main()
