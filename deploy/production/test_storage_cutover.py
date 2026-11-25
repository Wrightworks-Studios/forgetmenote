"""Exercise the storage v2 cutover of an owner and gateways through its cloud boundary."""
import copy
import datetime as dt
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import deploy_instance as deploy
import storage_cutover as storage


NOW = dt.datetime(2026, 11, 24, tzinfo=dt.timezone.utc)
PREVIOUS = 'registry.example/notes@sha256:' + 'a' * 64
IMAGE = 'registry.example/notes@sha256:' + 'b' * 64
SIDECARS = {'wiki-backup': {'key': 'BACKUP_CONFIG_V2', 'path': 'backup-v2.json'}}


def service(name, values, image=PREVIOUS):
    """A Duplo service as `service find` returns it."""
    return {'Name': name, 'Replicas': 1, 'Template': {'Name': name, 'Containers': [{'Name': name, 'Image': image}],
            'LBConfigurations': {'tcp|8080': {'Port': '8080'}}, 'OtherDockerConfig': json.dumps(values)}}


def owner_service():
    return service('notes-owner', {
        'Args': ['owner'],
        'Env': [{'Name': 'DATA_DIR', 'Value': '/data'}, {'Name': 'OWNER_SECRET_FILE', 'Value': '/run/secrets/owner/token'}],
        'LivenessProbe': {'HttpGet': {'Path': '/healthz', 'Port': 8080}},
        'Volumes': [{'Name': 'data', 'PersistentVolumeClaim': {'ClaimName': 'notes-owner-data'}},
                    {'Name': 'temp', 'EmptyDir': {}},
                    {'Name': 'backup', 'Secret': {'SecretName': 'notes', 'Items': [{'Key': 'BACKUP_CONFIG', 'Path': 'backup.json'}]}}],
        'VolumesMounts': [{'MountPath': '/data', 'Name': 'data'}],
        'AdditionalContainers': [{
            'Name': 'wiki-backup', 'Image': PREVIOUS, 'Command': ['/usr/local/bin/forgetmenote-backup'],
            'Args': ['--config', '/run/secrets/backup/backup.json'],
            'VolumeMounts': [{'MountPath': '/notebook', 'Name': 'data', 'ReadOnlyProperty': True, 'SubPath': 'projects/wiki/workspace'},
                             {'MountPath': '/backup-state', 'Name': 'data', 'SubPath': 'backup-worker/wiki'},
                             {'MountPath': '/run/secrets/backup', 'Name': 'backup', 'ReadOnlyProperty': True}]}]})


def gateway_service():
    values = {
        'Command': ['/bin/sh', '-c', 'forgetmenote migrate && exec forgetmenote serve'],
        'Env': [{'Name': 'DATA_DIR', 'Value': '/data'}, {'Name': 'PUBLIC_URL', 'Value': 'https://notes.example'},
                {'Name': 'OWNER_MAP_FILE', 'Value': '/run/config/owners.json'}],
        'ReadinessProbe': {'HttpGet': {'Path': '/readyz', 'Port': 8080}},
        'StartupProbe': {'HttpGet': {'Path': '/healthz', 'Port': 8080}},
        'Volumes': [{'Name': 'data', 'EmptyDir': {'SizeLimit': '10Gi'}}, {'Name': 'temp', 'EmptyDir': {}},
                    {'Name': 'database', 'Secret': {'SecretName': 'notes-database'}}],
        'VolumesMounts': [{'MountPath': '/data', 'Name': 'data'}, {'MountPath': '/run/secrets/database', 'Name': 'database'}]}
    found = service('notes-gateway', values)
    found['Replicas'] = 2
    return found


def settings(found):
    return json.loads(found['Template']['OtherDockerConfig'])


def env(values):
    return {item['Name']: item['Value'] for item in values['Env']}


class SpecTests(unittest.TestCase):
    def test_the_held_exporter_is_a_gateway_on_the_owners_volume_without_a_server(self):
        held = storage.exporter(owner_service(), gateway_service(), IMAGE, held=True, paused=True, sidecar_configs=SIDECARS)
        values = settings(held)
        self.assertEqual(held['Name'], 'notes-owner')
        self.assertEqual(held['Template']['Containers'][0]['Image'], IMAGE)
        self.assertEqual(held['Template']['LBConfigurations'], owner_service()['Template']['LBConfigurations'])
        self.assertEqual(values['Command'], storage.HOLD)
        self.assertNotIn('Args', values)
        for key in ('ReadinessProbe', 'StartupProbe', 'AdditionalContainers'):
            self.assertNotIn(key, values)
        self.assertEqual([v['Name'] for v in values['Volumes']], ['data', 'temp', 'database'])
        self.assertEqual(values['Volumes'][0], {'Name': 'data', 'PersistentVolumeClaim': {'ClaimName': 'notes-owner-data'}})
        self.assertEqual(env(values), {'DATA_DIR': '/data', 'PUBLIC_URL': 'https://notes.example', 'OUTGOING': 'paused'})

    def test_the_serving_exporter_backs_up_each_export_with_its_storage_v2_configuration(self):
        serving = storage.exporter(owner_service(), gateway_service(), IMAGE, held=False, paused=False, sidecar_configs=SIDECARS)
        values = settings(serving)
        self.assertEqual(values['Command'], gateway_service() and settings(gateway_service())['Command'])
        self.assertIn('ReadinessProbe', values)
        self.assertNotIn('OUTGOING', env(values))
        [sidecar] = values['AdditionalContainers']
        self.assertEqual(sidecar['Image'], IMAGE)
        self.assertEqual(sidecar['Args'], ['--config', '/run/secrets/backup/backup-v2.json'])
        self.assertEqual(sidecar['VolumeMounts'], [
            {'MountPath': '/backup-state', 'Name': 'data', 'SubPath': 'backup-worker/wiki'},
            {'MountPath': '/run/secrets/backup', 'Name': 'backup', 'ReadOnlyProperty': True},
            {'MountPath': '/exports', 'Name': 'data', 'ReadOnlyProperty': True, 'SubPath': 'exports'}])
        [backup] = [v for v in values['Volumes'] if v['Name'] == 'backup']
        self.assertEqual(backup['Secret']['Items'], [{'Key': 'BACKUP_CONFIG', 'Path': 'backup.json'},
                                                     {'Key': 'BACKUP_CONFIG_V2', 'Path': 'backup-v2.json'}])
        # The owner's own record is never changed.
        self.assertEqual(len(settings(owner_service())['Volumes'][2]['Secret']['Items']), 1)

    def test_a_working_tree_sidecar_without_a_storage_v2_configuration_is_refused(self):
        with self.assertRaisesRegex(ValueError, 'wiki-backup backs up a working tree'):
            storage.exporter(owner_service(), gateway_service(), IMAGE, held=False, paused=True, sidecar_configs={})

    def test_a_sidecar_volume_the_gateways_name_differently_is_refused(self):
        gateway = gateway_service()
        values = settings(gateway)
        values['Volumes'].append({'Name': 'backup', 'Secret': {'SecretName': 'other'}})
        gateway['Template']['OtherDockerConfig'] = json.dumps(values)
        with self.assertRaisesRegex(ValueError, 'backup differs'):
            storage.exporter(owner_service(), gateway, IMAGE, held=False, paused=True, sidecar_configs=SIDECARS)

    def test_new_gateways_drop_the_owner_map_and_pause_only_while_asked(self):
        paused = env(settings(storage.gateway_v2(gateway_service(), IMAGE, True)))
        self.assertEqual(paused, {'DATA_DIR': '/data', 'PUBLIC_URL': 'https://notes.example', 'OUTGOING': 'paused'})
        self.assertNotIn('OUTGOING', env(settings(storage.gateway_v2(gateway_service(), IMAGE, False))))
        self.assertEqual(storage.gateway_v2(gateway_service(), IMAGE, False)['Template']['Containers'][0]['Image'], IMAGE)

    def test_only_a_ready_unrestarted_pod_of_the_desired_mode_matches(self):
        def pod(image=IMAGE, command=None, outgoing=None, ready=True, restarts=0, deleting=False):
            main = {'name': 'notes-owner', 'image': image, 'env': [{'name': 'OUTGOING', 'value': outgoing}] if outgoing else []}
            if command:
                main['command'] = command
            return {'metadata': {'deletionTimestamp': 'now'} if deleting else {}, 'spec': {'containers': [main]},
                    'status': {'containerStatuses': [{'ready': ready, 'restartCount': restarts}]}}
        self.assertTrue(storage.pod_matches(pod(command=storage.HOLD, outgoing='paused'), 'notes-owner', IMAGE, held=True, paused=True))
        self.assertTrue(storage.pod_matches(pod(), 'notes-owner', IMAGE, held=False, paused=False))
        for wrong in (pod(image=PREVIOUS), pod(command=storage.HOLD), pod(outgoing='paused'), pod(ready=False), pod(restarts=1),
                      pod(deleting=True)):
            self.assertFalse(storage.pod_matches(wrong, 'notes-owner', IMAGE, held=False, paused=False))


def line(notebook, status, **values):
    return json.dumps({'notebook': notebook, 'status': status, **values})


class Cloud:
    """The two services, the backups and the exporter; storage commands answer per notebook."""
    def __init__(self, directory):
        self.directory = Path(directory)
        self.calls = []
        self.answers = {}
        self.fail = set()
        self.saved = {}

    def now(self):
        return NOW

    def save(self, name, value):
        self.saved[name] = copy.deepcopy(value)
        (self.directory / name).write_text(json.dumps(value))

    def protect(self, name, value):
        self.calls.append('protect:' + name)

    def stop_service(self, role):
        self.calls.append('stop:' + role)

    def start_service(self, role):
        self.calls.append('start:' + role)

    def update_service(self, role, name, desired, image):
        values = settings(desired)
        mode = 'held' if values.get('Command') == storage.HOLD else 'paused' if env(values).get('OUTGOING') else 'serving'
        self.calls.append('update:' + role + ':' + (mode if image == IMAGE else 'previous'))
        self.saved[name] = desired

    def converged(self, role, image, held=None, paused=None):
        self.calls.append('converged:' + role)

    def wait_presence(self, stopped_at):
        self.calls.append('presence')

    def healthy(self):
        self.calls.append('healthy')

    def sidecars_synced(self, sidecars, since):
        self.calls.append('synced')

    def start_cutover_backup(self, run_id):
        self.calls.append('backup')
        return 'job-1'

    def observe_snapshot(self, receipt):
        self.calls.append('snapshot')
        return {'id': 'snap-1'}

    def database_snapshot(self, run_id):
        self.calls.append('database-snapshot')
        return 'notes-pre-storage-v2'

    def complete_backup(self, receipt):
        self.calls.append('completed')
        return 'arn:aws:ec2:us-east-1::snapshot/snap-1'

    def restore_point(self):
        return {'instance': 'notes', 'latest_restorable_time': NOW.isoformat()}

    def run_owner(self, args, container=None):
        name = ' '.join(args)
        command = next((word for word in ('import', 'verify', 'export', 'check', 'retire') if 'admin storage ' + word in name), None)
        self.calls.append(('retire -yes' if '-yes' in args else command) if command else name)
        for failure in list(self.fail):
            if failure in name:
                self.fail.discard(failure)
                raise RuntimeError('in-cluster command failed: ' + failure)
        if name.startswith('sh -c'):
            return 'team\nwiki\n'
        if 'finish-rollout' in name:
            return 'database requires reader version 2\n'
        if command is None:
            return ''
        if command in self.answers:
            return self.answers[command]
        status = {'import': 'imported', 'verify': 'verified', 'export': 'exported', 'check': 'passed',
                  'retire': 'removed' if '-yes' in args else 'would remove'}[command]
        output = [line('team', status), line('wiki', status)]
        if command == 'check':
            output.append(json.dumps({'summary': {'passed': 2}, 'notebooks': 2, 'passed': True}))
        return '\n'.join(output) + '\n'


def prepared():
    return {'id': 'storage-cutover-1', 'phase': 'prepared', 'image': IMAGE, 'previous_image': PREVIOUS, 'source_revision': 'd' * 40,
            'volume': 'vol-1', 'started_at': NOW.isoformat(), 'owner_fingerprint': 'f' * 64, 'sidecar_configs': SIDECARS,
            'sidecars': {'wiki-backup': '/backup-state'}, 'owner_before': owner_service(), 'gateway_before': gateway_service(),
            'paused': True}


class CutoverTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.cloud = Cloud(self.directory.name)
        self.receipt = prepared()

    def stored(self):
        return json.loads((Path(self.directory.name) / 'storage-cutover.json').read_text())

    def test_every_phase_runs_in_order_and_records_its_outcome(self):
        storage.cutover(self.cloud, self.receipt)
        self.assertEqual(self.cloud.calls, [
            'stop:gateway', 'stop:owner', 'backup', 'snapshot', 'protect:capture.json', 'database-snapshot', 'completed',
            'protect:backup-result.json',
            'update:owner:held', 'start:owner', 'converged:owner', 'presence', 'forgetmenote migrate',
            'sh -c cd /data/projects && for d in */; do [ -d "$d/workspace/.git" ] && echo "${d%/}"; done; true',
            'import', 'verify', 'export', 'check', 'retire', 'protect:checked.json',
            'update:owner:paused', 'update:gateway:paused', 'start:owner', 'start:gateway', 'converged:owner',
            'converged:gateway', 'healthy', 'synced',
            'forgetmenote migrate -finish-rollout', 'protect:finished.json',
            'stop:gateway', 'stop:owner', 'update:owner:held', 'start:owner', 'converged:owner', 'presence', 'retire -yes',
            'update:owner:paused', 'update:gateway:paused', 'start:owner', 'start:gateway', 'converged:owner',
            'converged:gateway', 'healthy', 'synced',
            'update:owner:serving', 'update:gateway:serving', 'start:owner', 'start:gateway', 'converged:owner',
            'converged:gateway', 'healthy', 'synced', 'protect:storage-cutover-result.json'])
        stored = self.stored()
        self.assertEqual(stored['phase'], 'complete')
        self.assertEqual(stored['notebooks'], ['team', 'wiki'])
        self.assertEqual(stored['imported'], {'imported': 2})
        self.assertEqual(stored['checked'], {'passed': 2})
        self.assertEqual(stored['retire_plan'], {'would remove': 2})
        self.assertEqual(stored['retired'], {'removed': 2})
        self.assertFalse(stored['paused'])
        self.assertEqual(stored['database_snapshot'], 'notes-pre-storage-v2')
        self.assertEqual(stored['recovery_point'], 'arn:aws:ec2:us-east-1::snapshot/snap-1')

    def test_the_completed_cutover_is_the_live_release_of_the_next_deployment(self):
        storage.cutover(self.cloud, self.receipt)
        record = json.loads((Path(self.directory.name) / 'deployment.json').read_text())
        self.assertEqual(record['phase'], 'ready')
        self.assertEqual(record['image'], IMAGE)
        self.assertEqual(record['config_fingerprint'], 'f' * 64)
        self.assertEqual(record['backup_state'], 'COMPLETED')
        found = deploy.deployments(Path(self.directory.name).parent)
        self.assertEqual(found[0][2]['image'], IMAGE)

    def test_a_failing_notebook_stops_the_cutover_with_the_gateways_down(self):
        self.cloud.answers['verify'] = line('team', 'verified') + '\n' + line('wiki', 'verified', error='history differs') + '\n'
        with self.assertRaisesRegex(RuntimeError, 'verify did not succeed for wiki'):
            storage.cutover(self.cloud, self.receipt)
        self.assertEqual(self.stored()['phase'], 'imported')
        self.assertNotIn('start:gateway', self.cloud.calls)

    def test_every_notebook_of_the_owners_volume_must_succeed(self):
        self.cloud.answers['import'] = line('team', 'imported') + '\n'
        with self.assertRaisesRegex(RuntimeError, 'import did not succeed for wiki'):
            storage.cutover(self.cloud, self.receipt)
        self.cloud.answers['import'] = (line('team', 'imported') + '\n' + line('wiki', 'skipped: no notebook repository') + '\n')
        with self.assertRaisesRegex(RuntimeError, 'import did not succeed for wiki'):
            storage.cutover(self.cloud, self.receipt)

    def test_another_notebook_the_command_finds_may_be_skipped_but_never_fail(self):
        self.cloud.answers['import'] = '\n'.join([line('team', 'imported'), line('wiki', 'imported'),
                                                  line('empty', 'skipped: no notebook repository')]) + '\n'
        self.cloud.answers['verify'] = '\n'.join([line('team', 'verified'), line('wiki', 'verified'),
                                                  line('empty', 'skipped: not in the notebook store')]) + '\n'
        self.cloud.answers['export'] = '\n'.join([line('team', 'exported'), line('wiki', 'exported'),
                                                  line('empty', 'exported', error='no such notebook')]) + '\n'
        with self.assertRaisesRegex(RuntimeError, 'export did not succeed for empty'):
            storage.cutover(self.cloud, self.receipt)
        self.assertEqual(self.stored()['phase'], 'verified')
        self.assertEqual(self.stored()['imported'], {'imported': 2, 'skipped: no notebook repository': 1})

    def test_check_must_pass_in_its_summary(self):
        self.cloud.answers['check'] = line('team', 'passed') + '\n' + line('wiki', 'passed') + '\n'
        with self.assertRaisesRegex(RuntimeError, 'check did not succeed for the summary'):
            storage.cutover(self.cloud, self.receipt)
        self.assertEqual(self.stored()['phase'], 'exported')

    def test_a_retire_that_would_keep_files_stops_before_anyone_can_write(self):
        self.cloud.answers['retire'] = line('team', 'would remove') + '\n' + line('wiki', 'kept', error='live edits') + '\n'
        with self.assertRaisesRegex(RuntimeError, 'retire did not succeed for wiki'):
            storage.cutover(self.cloud, self.receipt)
        self.assertEqual(self.stored()['phase'], 'exported')
        self.assertNotIn('served_from', self.stored())

    def test_resuming_repeats_only_the_interrupted_phase(self):
        self.cloud.fail.add('admin storage export')
        with self.assertRaises(RuntimeError):
            storage.cutover(self.cloud, self.receipt)
        self.assertEqual(self.stored()['phase'], 'verified')
        self.cloud.calls.clear()
        storage.cutover(self.cloud, self.stored())
        self.assertNotIn('import', self.cloud.calls)
        self.assertNotIn('backup', self.cloud.calls)
        self.assertEqual(self.cloud.calls[0], 'export')
        self.assertEqual(self.stored()['phase'], 'complete')

    def test_a_failed_retire_serves_again_and_resumes_with_another_stop(self):
        self.cloud.fail.add('-yes')
        with self.assertRaisesRegex(RuntimeError, 'serves again'):
            storage.cutover(self.cloud, self.receipt)
        self.assertEqual(self.stored()['phase'], 'finished')
        self.assertEqual(self.cloud.calls[-4:], ['converged:owner', 'converged:gateway', 'healthy', 'synced'])
        self.cloud.calls.clear()
        storage.cutover(self.cloud, self.stored())
        self.assertEqual(self.cloud.calls[:2], ['stop:gateway', 'stop:owner'])
        self.assertIn('retire -yes', self.cloud.calls)
        self.assertEqual(self.stored()['phase'], 'complete')

    def test_finish_rollout_must_raise_the_reader_version(self):
        original = self.cloud.run_owner
        self.cloud.run_owner = lambda args, container=None: '' if '-finish-rollout' in args else original(args, container)
        with self.assertRaisesRegex(RuntimeError, 'did not raise the reader version'):
            storage.cutover(self.cloud, self.receipt)
        self.assertEqual(self.stored()['phase'], 'serving')


class RollbackTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.cloud = Cloud(self.directory.name)

    def test_before_serving_both_services_return_to_the_previous_release(self):
        receipt = {**prepared(), 'phase': 'checked'}
        storage.rollback(self.cloud, receipt, False)
        self.assertEqual(self.cloud.calls, ['stop:gateway', 'stop:owner', 'update:owner:previous', 'update:gateway:previous',
                                            'start:owner', 'start:gateway', 'converged:owner', 'converged:gateway', 'healthy',
                                            'protect:storage-cutover-rollback.json'])
        self.assertEqual(self.cloud.saved['owner-restored.json'], owner_service())
        self.assertEqual(self.cloud.saved['gateway-restored.json'], gateway_service())

    def test_once_the_new_release_may_have_served_rollback_needs_confirmation(self):
        receipt = {**prepared(), 'phase': 'checked', 'served_from': NOW.isoformat()}
        with self.assertRaisesRegex(RuntimeError, '--discard-v2-writes'):
            storage.rollback(self.cloud, receipt, False)
        self.assertEqual(self.cloud.calls, [])
        storage.rollback(self.cloud, receipt, True)
        self.assertIn('update:owner:previous', self.cloud.calls)

    def test_after_finish_rollout_only_a_restore_returns(self):
        receipt = {**prepared(), 'phase': 'finished', 'served_from': NOW.isoformat(), 'database_snapshot': 'notes-pre-storage-v2'}
        with self.assertRaisesRegex(RuntimeError, 'Restore the database snapshot notes-pre-storage-v2'):
            storage.rollback(self.cloud, receipt, True)
        self.assertEqual(self.cloud.calls, [])


class ConfigurationTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        owner = json.loads(Path(__file__).with_name('deploy_instance.example.json').read_text())
        self.owner = {**owner, 'state_root': str(self.root)}
        self.gateways = {**json.loads(Path(__file__).with_name('cutover.example.json').read_text()), 'state_root': str(self.root)}
        self.gateways['services'] = {**self.gateways['services'], 'owner': {'name': owner['name']}}

    def configure(self, **changes):
        (self.root / 'owner.json').write_text(json.dumps(self.owner))
        (self.root / 'gateways.json').write_text(json.dumps({**self.gateways, **changes}))
        return storage.configuration(self.root / 'owner.json', self.root / 'gateways.json')

    def test_the_owner_configuration_gains_the_services_database_and_sidecars(self):
        config = self.configure(storage_v2={'sidecars': SIDECARS})
        self.assertEqual(config['services']['owner']['name'], self.owner['name'])
        self.assertEqual(config['storage_v2'], {'sidecars': SIDECARS})
        self.assertEqual(config['volume'], self.owner['volume'])

    def test_mismatched_configurations_are_refused(self):
        for change in ({'services': {**self.gateways['services'], 'owner': {'name': 'other'}}}, {'blob_bucket': ''},
                       {'storage_v2': {'sidecars': ['wiki-backup']}}, {'state_root': str(self.root / 'other')}):
            with self.assertRaises(ValueError):
                self.configure(**change)


class PreflightTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        config = {**json.loads(Path(__file__).with_name('deploy_instance.example.json').read_text()), 'name': 'notes-owner',
                  'services': {'owner': {'name': 'notes-owner'}, 'gateway': {'name': 'notes-gateway', 'replicas': 2}},
                  'database_instance': 'notes', 'blob_bucket': 'notes-blobs'}
        self.cloud = storage.Runtime(config, Path(self.directory.name))
        self.candidate = {'reference': IMAGE, 'image': 'registry.example/notes:main-' + 'd' * 40, 'source_revision': 'd' * 40,
                          'registry_digest': 'sha256:' + 'b' * 64}

    def check(self, owner=None, gateway=None, secret=True, status='synced', v2=True, gateways_ready=2):
        services = {'owner': owner or owner_service(), 'gateway': gateway or gateway_service()}
        def pod(application):
            return {'metadata': {}, 'spec': {'containers': [{'name': application, 'image': PREVIOUS}]},
                    'status': {'containerStatuses': [{'ready': True, 'restartCount': 0}]}}

        def command(args, **kwargs):
            if 'go-template' in ' '.join(args):
                return {'present': secret}
            if 'describe-db-instances' in args:
                return {'DBInstances': [{'DBInstanceStatus': 'available', 'StorageEncrypted': True, 'PubliclyAccessible': False,
                                         'BackupRetentionPeriod': 7, 'DeletionProtection': True}]}
            if 'get-bucket-versioning' in args:
                return {'Status': 'Enabled'}
            return {}
        with patch.object(self.cloud, 'find', side_effect=lambda role: services[role]), \
                patch.object(self.cloud, 'command', side_effect=command), \
                patch.object(self.cloud, 'check_identity'), patch.object(self.cloud, 'check_storage'), \
                patch.object(self.cloud, 'check_backups'), \
                patch.object(self.cloud, 'role_pods', side_effect=lambda role: [pod('notes-gateway')] * gateways_ready),\
                patch.object(self.cloud, 'sidecar_status', return_value={'status': status}), \
                patch('deploy_instance.storage_v2', return_value=v2), \
                patch('deploy_instance.locate_package', return_value=(None, self.candidate['registry_digest'])):
            return self.cloud.check(self.candidate, SIDECARS)

    def test_a_ready_owner_and_gateways_pass(self):
        problems, owner, gateway, previous, sidecars = self.check()
        self.assertEqual(problems, [])
        self.assertEqual(previous, PREVIOUS)
        self.assertEqual(sidecars, {'wiki-backup': '/backup-state'})
        held = json.loads((Path(self.directory.name) / 'exporter-held.json').read_text())
        self.assertEqual(settings(held)['Command'], storage.HOLD)

    def test_each_unready_part_is_named(self):
        gateway = gateway_service()
        values = settings(gateway)
        values['Env'].append({'Name': 'OUTGOING', 'Value': 'paused'})
        gateway['Template']['OtherDockerConfig'] = json.dumps(values)
        problems = self.check(gateway=gateway, secret=False, status='error', v2=False, gateways_ready=1)[0]
        for expected in ('must not set OUTGOING', 'lacks BACKUP_CONFIG_V2', 'wiki-backup has not pushed', 'not a storage v2 release',
                         'expected 2 ready gateways'):
            self.assertTrue(any(expected in problem for problem in problems), (expected, problems))

    def test_an_owner_already_on_storage_v2_is_refused(self):
        owner = owner_service()
        values = settings(owner)
        values.pop('Args')
        owner['Template']['OtherDockerConfig'] = json.dumps(values)
        problems = self.check(owner=owner)[0]
        self.assertTrue(any('does not run `forgetmenote owner`' in problem for problem in problems), problems)


class StorageGenerationTests(unittest.TestCase):
    """deploy_instance.py refuses to cross into storage v2 with an image update."""
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        git = lambda *args: subprocess.run(['git', '-C', str(self.root), *args], check=True, capture_output=True, text=True).stdout.strip()
        git('init', '-q')
        git('config', 'user.email', 'test@example.com')
        git('config', 'user.name', 'Test')
        (self.root / 'README').write_text('v1\n')
        git('add', '.')
        git('commit', '-qm', 'v1')
        self.v1 = git('rev-parse', 'HEAD')
        marker = self.root / deploy.STORAGE_V2
        marker.parent.mkdir(parents=True)
        marker.write_text('package main\n')
        git('add', '.')
        git('commit', '-qm', 'v2')
        self.v2 = git('rev-parse', 'HEAD')

    def test_only_the_step_into_storage_v2_is_refused(self):
        deploy.check_storage_generation(self.root, self.v1, self.v1)
        deploy.check_storage_generation(self.root, self.v2, self.v2)
        with self.assertRaisesRegex(ValueError, 'run the storage cutover'):
            deploy.check_storage_generation(self.root, self.v1, self.v2)
        # A live release from a history this one replaced predates storage v2.
        with self.assertRaisesRegex(ValueError, 'run the storage cutover'):
            deploy.check_storage_generation(self.root, 'e' * 40, self.v2)

    def test_a_storage_v2_deployment_record_outlasts_a_replaced_history(self):
        # After the repository's history is replaced, the live release's commit is gone, but
        # its deployment record still says it runs storage v2.
        missing = 'e' * 40
        deploy.check_storage_generation(self.root, missing, self.v2, previous_v2=True)
        with self.assertRaisesRegex(ValueError, 'run the storage cutover'):
            deploy.check_storage_generation(self.root, missing, self.v2, previous_v2=None)
        records = Path(self.directory.name) / 'records'
        records.mkdir()
        self.assertIsNone(deploy.previous_storage_v2(records))
        receipt = {'id': 'x', 'image': 'new', 'previous_image': 'old', 'source_revision': missing,
                   'previous_revision': 'f' * 40, 'started_at': 'now', 'complete_at': 'now', 'owner_fingerprint': 'fp',
                   'backup_job': 'job', 'captured_at': 'then', 'volume': 'vol', 'snapshot': {'id': 'snap'}}
        (records / 'deployment.json').write_text(json.dumps(storage.deployment_record(receipt)))
        self.assertIs(deploy.previous_storage_v2(records), True)
        (records / 'deployment.json').write_text(json.dumps({'phase': 'ready', 'storage_v2': True}))
        self.assertIs(deploy.previous_storage_v2(records), True)
        (records / 'deployment.json').write_text(json.dumps({'phase': 'ready', 'storage_v2': False}))
        self.assertIsNone(deploy.previous_storage_v2(records))

    def test_a_candidate_the_checkout_lacks_is_refused(self):
        with self.assertRaisesRegex(ValueError, 'lacks release revision'):
            deploy.check_storage_generation(self.root, self.v1, 'e' * 40)


if __name__ == '__main__':
    unittest.main()
