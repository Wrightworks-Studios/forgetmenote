"""Exercise upgrade ordering and recovery through the deployment boundary."""
import copy
import datetime as dt
import hashlib
from http.server import BaseHTTPRequestHandler, HTTPServer
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tarfile
import tempfile
import threading
import unittest
from unittest.mock import patch

import deploy_instance as deploy


NOW = dt.datetime(2026, 10, 7, tzinfo=dt.timezone.utc)
OLD = 'ghcr.io/example/notes@sha256:' + 'a' * 64
NEW = 'ghcr.io/example/notes@sha256:' + 'b' * 64


class Cloud:
    """Cloud boundary: pending upload survives starting the new application."""
    def __init__(self, directory):
        self.directory = Path(directory)
        self.calls = []
        self.backup_state = 'RUNNING'
        self.snapshot_state = 'pending'
        self.snapshot_volume = 'vol-123'
        self.snapshot_start = NOW + dt.timedelta(seconds=1)
        self.warmup_state = 'COMPLETED'
        self.saved_at_warmup = None
        self.failure = None
        self.stopped = False
        self.image = OLD
        self.started_images = []
        self.now_value = NOW
        self.config = {'volume': 'vol-123', 'account': '123456789012', 'region': 'us-east-1', 'vault': 'notes'}

    def now(self):
        return self.now_value

    def save(self, name, value):
        (self.directory / name).write_text(json.dumps(value))

    def protect(self, name, value):
        self.calls.append('protect:' + name)
        if self.failure == 'protect' and name == 'capture.json':
            raise RuntimeError('protected capture failed')
        if self.failure == 'deployment-proof' and name in ('deployment.json', 'deployment-result.json'):
            raise RuntimeError('protected deployment result failed')

    def stop(self):
        self.calls.append('stop')
        self.stopped = True
        if self.failure == 'stop':
            raise RuntimeError('stop response lost')

    def start(self):
        self.calls.append('start')
        self.stopped = False
        self.started_images.append(self.image)

    def wait_stopped(self):
        self.calls.append('quiesced')

    def start_backup(self, run_id):
        self.calls.append('backup-request')
        return 'job-1'

    def start_warmup(self, run_id):
        self.calls.append('warmup-request')
        self.saved_at_warmup = json.loads((self.directory / 'deployment.json').read_text())
        if self.failure == 'warmup-request':
            raise RuntimeError('warm-up request rejected')
        return 'warmup-1'

    def wait_warmup(self, job):
        self.calls.append('warmup-wait')
        if self.failure == 'warmup-unfinished':
            raise RuntimeError('Warm-up backup did not finish before the deadline')
        return {'BackupJobId': job, 'State': self.warmup_state}

    def observe_snapshot(self, receipt):
        self.calls.append('snapshot-point')
        if self.failure == 'snapshot':
            raise RuntimeError('backup accepted without snapshot')
        if self.snapshot_volume != self.config['volume'] or self.snapshot_start < NOW:
            raise RuntimeError('snapshot does not match stopped volume')
        self.now_value = NOW + dt.timedelta(seconds=2)
        return {'id': 'snap-1', 'state': self.snapshot_state, 'observed_at': self.now().isoformat()}

    def backup_job(self, job):
        self.calls.append('backup-status')
        return {'BackupJobId': job, 'State': self.backup_state, 'ResourceArn': deploy.volume_arn(self.config),
                'BackupVaultName': 'notes', 'RecoveryPointArn': 'arn:aws:ec2:us-east-1::snapshot/snap-1'}

    def wait_backup(self, receipt):
        self.calls.append('wait-completed')
        self.backup_state = 'COMPLETED'
        return self.backup_job(receipt['backup_job'])

    def tag_completed(self, receipt):
        self.calls.append('tag-completed')

    def update(self, receipt):
        self.calls.append('update')
        if self.failure == 'update':
            raise RuntimeError('image update failed')
        self.image = receipt['image']

    def wait_ready(self, receipt):
        self.calls.append('ready')
        if self.failure == 'ready':
            raise RuntimeError('readiness failed')
        if self.image != receipt['image'] or self.stopped:
            raise AssertionError('wrong image is serving')


class UpgradeTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.cloud = Cloud(self.temp.name)
        self.receipt = {'id': 'release-1', 'image': NEW, 'previous_image': OLD, 'phase': 'prepared',
                        'compatibility': 'compatible', 'compatibility_note': 'Storage formats unchanged.',
                        'configuration': 'runs/before/config.json', 'volume': 'vol-123'}

    def test_compatible_update_starts_new_image_after_capture_without_waiting_for_upload(self):
        deploy.upgrade(self.cloud, self.receipt)
        self.assertEqual(self.cloud.started_images, [NEW])
        self.assertEqual(self.cloud.calls.count('stop'), 1)
        self.assertNotIn('wait-completed', self.cloud.calls)
        self.assertLess(self.cloud.calls.index('snapshot-point'), self.cloud.calls.index('update'))
        self.assertLess(self.cloud.calls.index('protect:capture.json'), self.cloud.calls.index('update'))
        self.assertEqual(self.receipt['phase'], 'ready')
        self.assertEqual(self.receipt['backup_state'], 'RUNNING')
        self.assertNotIn('tag-completed', self.cloud.calls)
        self.assertNotIn('warmup-request', self.cloud.calls)
        self.assertNotIn('warmup', self.receipt)

    def test_migration_warms_up_the_running_volume_then_runs_the_unchanged_stopped_sequence(self):
        receipt = {**self.receipt, 'compatibility': 'migration'}
        deploy.upgrade(self.cloud, receipt)
        self.assertEqual(self.cloud.calls, [
            'warmup-request', 'warmup-wait',
            'stop', 'quiesced', 'backup-request', 'snapshot-point', 'protect:capture.json',
            'wait-completed', 'backup-status', 'tag-completed', 'protect:backup-result.json',
            'update', 'start', 'ready', 'protect:deployment-result.json', 'backup-status'])
        self.assertEqual(self.cloud.saved_at_warmup['phase'], 'prepared')
        self.assertEqual(set(self.cloud.saved_at_warmup['warmup']), {'requested_at'}, 'intent is saved before the request')
        self.assertEqual((receipt['warmup']['job'], receipt['warmup']['state']), ('warmup-1', 'COMPLETED'))
        # The stopped capture remains the only restore point the receipt names.
        self.assertEqual((receipt['backup_job'], receipt['snapshot']['id']), ('job-1', 'snap-1'))
        self.assertEqual(receipt['backup_state'], 'COMPLETED')
        self.assertEqual(self.cloud.started_images, [NEW])

    def test_warmup_failure_falls_back_to_the_stopped_sequence(self):
        for failure, state in (('warmup-request', 'COMPLETED'), (None, 'FAILED'), (None, 'ABORTED')):
            with self.subTest(failure=failure, state=state):
                cloud = Cloud(self.temp.name)
                cloud.failure, cloud.warmup_state = failure, state
                receipt = {**self.receipt, 'compatibility': 'unknown'}
                deploy.upgrade(cloud, receipt)
                self.assertLess(cloud.calls.index('warmup-request'), cloud.calls.index('stop'))
                self.assertEqual(cloud.calls.count('stop'), 1)
                self.assertLess(cloud.calls.index('wait-completed'), cloud.calls.index('update'))
                self.assertEqual(receipt['phase'], 'ready')
                self.assertEqual(receipt['backup_state'], 'COMPLETED')
                self.assertEqual(cloud.started_images, [NEW])
                if failure:
                    self.assertEqual(receipt['warmup']['error'], 'warm-up request rejected')
                    self.assertNotIn('job', receipt['warmup'])
                else:
                    self.assertEqual(receipt['warmup']['state'], state)

    def test_unfinished_warmup_stops_the_deployment_before_the_service_stops(self):
        # AWS Backup runs one job per resource: a stopped backup started now would fail mid-outage.
        self.cloud.failure = 'warmup-unfinished'
        receipt = {**self.receipt, 'compatibility': 'migration'}
        with self.assertRaisesRegex(RuntimeError, 'warmup-1 has not finished; the service was not stopped'):
            deploy.upgrade(self.cloud, receipt)
        self.assertEqual(self.cloud.calls, ['warmup-request', 'warmup-wait'])
        self.assertEqual(self.cloud.started_images, [])
        saved = json.loads((Path(self.temp.name) / 'deployment.json').read_text())
        self.assertEqual(saved['phase'], 'prepared')
        self.assertEqual(saved['warmup']['job'], 'warmup-1')
        self.assertIn('did not finish', saved['warmup']['error'])
        self.assertNotIn('backup_job', saved)
        self.cloud.calls.clear()
        with self.assertRaisesRegex(RuntimeError, 'fresh'):
            deploy.upgrade(self.cloud, receipt, resume=True)
        self.assertEqual(self.cloud.calls, [])

    def test_migration_and_unknown_compatibility_wait_before_changing_images(self):
        for mode in ('migration', 'unknown'):
            with self.subTest(mode=mode):
                cloud = Cloud(self.temp.name)
                receipt = {**self.receipt, 'compatibility': mode}
                deploy.upgrade(cloud, receipt)
                self.assertLess(cloud.calls.index('wait-completed'), cloud.calls.index('update'))
                self.assertEqual(cloud.started_images, [NEW])
                self.assertEqual(receipt['backup_state'], 'COMPLETED')

    def test_failure_before_update_always_attempts_start_and_never_updates(self):
        for failure in ('stop', 'snapshot', 'protect'):
            with self.subTest(failure=failure):
                cloud = Cloud(self.temp.name)
                cloud.failure = failure
                with self.assertRaises(RuntimeError):
                    deploy.upgrade(cloud, copy.deepcopy(self.receipt))
                self.assertEqual(cloud.started_images, [OLD])
                self.assertNotIn('update', cloud.calls)

    def test_accepted_job_wrong_volume_or_old_snapshot_cannot_unlock_deployment(self):
        for attribute, value in [('snapshot_volume', 'vol-other'), ('snapshot_start', NOW - dt.timedelta(seconds=1))]:
            cloud = Cloud(self.temp.name)
            setattr(cloud, attribute, value)
            with self.assertRaisesRegex(RuntimeError, 'snapshot'):
                deploy.upgrade(cloud, copy.deepcopy(self.receipt))
            self.assertNotIn('update', cloud.calls)
            self.assertEqual(cloud.started_images, [OLD])

    def test_update_failure_attempts_restart_and_preserves_original_failure(self):
        self.cloud.failure = 'update'
        with self.assertRaisesRegex(RuntimeError, 'image update failed'):
            deploy.upgrade(self.cloud, self.receipt)
        self.assertIn('start', self.cloud.calls)
        self.assertNotEqual(self.receipt['phase'], 'ready')

    def test_completing_pending_backup_never_stops_or_redeploys(self):
        deploy.upgrade(self.cloud, self.receipt)
        self.cloud.calls.clear()
        self.cloud.backup_state = 'COMPLETED'
        deploy.refresh_backup(self.cloud, self.receipt)
        self.assertEqual(self.receipt['backup_state'], 'COMPLETED')
        self.assertIn('tag-completed', self.cloud.calls)
        self.assertNotIn('stop', self.cloud.calls)
        self.assertNotIn('update', self.cloud.calls)

    def test_failed_background_backup_is_persisted_and_reported_without_retry(self):
        deploy.upgrade(self.cloud, self.receipt)
        self.cloud.calls.clear()
        self.cloud.backup_state = 'FAILED'
        with self.assertRaisesRegex(RuntimeError, 'FAILED'):
            deploy.refresh_backup(self.cloud, self.receipt)
        saved = json.loads((Path(self.temp.name) / 'deployment.json').read_text())
        self.assertEqual(saved['backup_state'], 'FAILED')
        self.assertEqual(saved['phase'], 'ready', 'deployment health and backup success are distinct')
        self.assertNotIn('backup-request', self.cloud.calls)

    def test_failed_update_cannot_reuse_snapshot_after_service_was_restarted(self):
        self.cloud.failure = 'update'
        with self.assertRaisesRegex(RuntimeError, 'image update failed'):
            deploy.upgrade(self.cloud, self.receipt)
        self.cloud.failure = None
        self.cloud.calls.clear()
        with self.assertRaisesRegex(RuntimeError, 'fresh'):
            deploy.upgrade(self.cloud, self.receipt, resume=True)
        self.assertEqual(self.cloud.calls, [])

    def test_startup_resume_does_not_stop_or_recapture(self):
        self.cloud.failure = 'ready'
        with self.assertRaisesRegex(RuntimeError, 'readiness failed'):
            deploy.upgrade(self.cloud, self.receipt)
        self.cloud.failure = None
        self.cloud.calls.clear()
        deploy.upgrade(self.cloud, self.receipt, resume=True)
        self.assertEqual(self.receipt['phase'], 'ready')
        self.assertNotIn('stop', self.cloud.calls)
        self.assertNotIn('backup-request', self.cloud.calls)

    def test_prepared_resume_requires_fresh_preflight(self):
        with self.assertRaisesRegex(RuntimeError, 'fresh'):
            deploy.upgrade(self.cloud, self.receipt, resume=True)
        self.assertEqual(self.cloud.calls, [])

    def test_stop_failure_and_restart_failure_are_both_retained(self):
        self.cloud.failure = 'stop'
        with patch.object(self.cloud, 'start', side_effect=RuntimeError('restart rejected')):
            with self.assertRaisesRegex(RuntimeError, 'stop response lost; restart also failed: restart rejected'):
                deploy.upgrade(self.cloud, self.receipt)
        saved = json.loads((Path(self.temp.name) / 'deployment.json').read_text())
        self.assertEqual(saved['operation_error'], 'stop response lost')
        self.assertEqual(saved['restart_error'], 'restart rejected')

    def test_failed_final_protection_is_not_skipped_on_resume(self):
        self.cloud.failure = 'deployment-proof'
        with self.assertRaisesRegex(RuntimeError, 'protected deployment result failed'):
            deploy.upgrade(self.cloud, self.receipt)
        self.assertEqual(self.receipt['phase'], 'starting')
        self.cloud.failure = None
        self.cloud.calls.clear()
        deploy.upgrade(self.cloud, self.receipt, resume=True)
        self.assertEqual(self.receipt['phase'], 'ready')
        self.assertIn('protect:deployment-result.json', self.cloud.calls)
        self.assertNotIn('stop', self.cloud.calls)


def service():
    return {'Name': 'forgetmenote', 'Replicas': 1, 'Template': {
        'Containers': [{'Image': OLD, 'Env': [{'Name': 'SECRET', 'Value': 'synthetic-secret'}]}],
        'BaseVersion': 'generated-before', 'OtherDockerConfig': json.dumps({
            'envFrom': [{'secretRef': {'name': 'existing-keys'}}],
            'securityContext': {'runAsUser': 1000},
            'additionalContainers': [{'name': 'backup', 'image': OLD}, {'name': 'proxy', 'image': 'unrelated'}],
            'initContainers': [{'name': 'initialize', 'image': OLD}]}),
        'Volumes': [{'claimName': 'forgetmenote-data'}]}}


class RuntimeTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.directory = Path(self.temp.name)
        self.config = json.loads(Path(__file__).with_name('deploy_instance.example.json').read_text())
        self.config['volume'] = 'vol-123'
        self.cloud = deploy.Runtime(self.config, self.directory, timeout=1)
        self.cloud.now = lambda: NOW
        self.cloud.run_id = 'test-release'
        self.receipt = {'id': 'test-release', 'backup_job': 'job-1', 'phase': 'starting', 'image': NEW,
                        'previous_image': OLD, 'image_containers': ['app', 'backup'],
                        'quiesced_at': NOW.isoformat(), 'snapshot': {'id': 'snap-123'}}
        self.job = {'BackupJobId': 'job-1', 'State': 'RUNNING', 'ResourceArn': deploy.volume_arn(self.config),
                    'BackupVaultName': self.config['vault'], 'RecoveryPointArn': 'arn:aws:ec2:us-east-1::snapshot/snap-123'}
        self.snapshot = {'SnapshotId': 'snap-123', 'VolumeId': 'vol-123', 'State': 'pending', 'Encrypted': True,
                         'StartTime': (NOW + dt.timedelta(seconds=1)).isoformat()}
        self.pod = {'metadata': {}, 'spec': {'containers': [{'name': 'app', 'image': NEW}, {'name': 'backup', 'image': NEW}]},
                    'status': {'containerStatuses': [{'ready': True, 'restartCount': 0}, {'ready': True, 'restartCount': 0}]}}
        self.pod['spec']['volumes'] = [{'persistentVolumeClaim': {'claimName': self.config['claim']}}]

    def test_repeated_successful_responses_are_stored_once(self):
        same = [sys.executable, '-c', 'print("{}")']
        other = [sys.executable, '-c', 'print("[]")']
        failing = [sys.executable, '-c', 'import sys; print("same failure", file=sys.stderr); sys.exit(3)']
        for command in (same, same, other, same):
            self.cloud.command(command)
        for _ in range(2):
            with self.assertRaises(RuntimeError):
                self.cloud.command(failing)
        records = [json.loads(path.read_text()) for path in sorted(self.directory.glob('command-*.json'))]
        self.assertEqual(len(records), 6)
        self.assertEqual(records[1], {'same_as': 'command-0001.json'})
        self.assertEqual(records[2]['stdout'], '[]\n')
        self.assertEqual(records[3], {'same_as': 'command-0001.json'})
        self.assertTrue(all(record.get('stderr') == 'same failure\n' for record in records[4:]), 'failures keep full evidence')

    def test_duplo_reports_errors_even_when_cli_exits_successfully(self):
        for action in ('stop', 'start'):
            with self.subTest(action=action), patch.object(self.cloud, 'command', return_value={
                    'message': 'operation completed', 'details': {'success': [], 'errors': [{'error': 'denied'}]}}):
                with self.assertRaisesRegex(RuntimeError, 'Duplo'):
                    getattr(self.cloud, action)()

    def test_actual_pending_snapshot_is_required_for_the_stopped_volume(self):
        with patch.object(self.cloud, 'backup_job', return_value=self.job), \
                patch.object(self.cloud, 'command', return_value={'Snapshots': [self.snapshot]}), \
                patch.object(self.cloud, 'is_stopped', return_value=True):
            capture = self.cloud.observe_snapshot(self.receipt)
        self.assertEqual(capture['id'], 'snap-123')
        self.assertEqual(capture['state'], 'pending')

    def test_snapshot_acceptance_cannot_replace_capture_and_invalid_capture_fails(self):
        variants = [('SnapshotId', 'snap-other'), ('VolumeId', 'vol-other'), ('Encrypted', False),
                    ('StartTime', (NOW - dt.timedelta(seconds=1)).isoformat()), ('State', 'error')]
        for key, value in variants:
            with self.subTest(key=key), patch.object(self.cloud, 'backup_job', return_value=self.job), \
                    patch.object(self.cloud, 'command', return_value={'Snapshots': [{**self.snapshot, key: value}]}), \
                    patch.object(self.cloud, 'is_stopped', return_value=True):
                with self.assertRaisesRegex(RuntimeError, 'Snapshot'):
                    self.cloud.observe_snapshot(self.receipt)
        with patch.object(self.cloud, 'backup_job', return_value={**self.job, 'RecoveryPointArn': ''}), \
                patch.object(self.cloud, 'command') as commands, patch('deploy_instance.time.monotonic', side_effect=[0, 2]):
            with self.assertRaisesRegex(RuntimeError, 'no stopped snapshot point'):
                self.cloud.observe_snapshot(self.receipt)
            commands.assert_not_called()

    def test_foreign_job_or_resumed_writer_cannot_unlock_update(self):
        for job in ({**self.job, 'ResourceArn': 'wrong'}, {**self.job, 'BackupVaultName': 'wrong'}, self.job):
            with patch.object(self.cloud, 'backup_job', return_value=job), \
                    patch.object(self.cloud, 'command', return_value={'Snapshots': [self.snapshot]}), \
                    patch.object(self.cloud, 'is_stopped', return_value=False):
                with self.assertRaises(RuntimeError):
                    self.cloud.observe_snapshot(self.receipt)

    def test_image_update_preserves_secrets_mounts_and_unrelated_containers(self):
        before = service()
        self.cloud.save('service-before.json', before)
        stopped = copy.deepcopy(before)
        stopped['Replicas'] = 0
        stopped['Template']['BaseVersion'] = 'generated-after'
        with patch.object(self.cloud, 'is_stopped', return_value=True), \
                patch.object(self.cloud, 'service', return_value=stopped), \
                patch.object(self.cloud, 'command', return_value={}) as commands, patch.object(self.cloud, 'protect'):
            self.cloud.update(self.receipt)
        desired = json.loads((self.directory / 'service-desired.json').read_text())
        self.assertEqual(desired['Replicas'], 0, 'configuration update must preserve the stop')
        self.assertEqual(desired['Template']['Volumes'], before['Template']['Volumes'])
        self.assertEqual(desired['Template']['Containers'][0]['Env'], before['Template']['Containers'][0]['Env'])
        settings = json.loads(desired['Template']['OtherDockerConfig'])
        self.assertEqual(settings['envFrom'], [{'secretRef': {'name': 'existing-keys'}}])
        self.assertEqual(settings['additionalContainers'], [{'name': 'backup', 'image': NEW}, {'name': 'proxy', 'image': 'unrelated'}])
        self.assertEqual(settings['initContainers'][0]['image'], NEW)
        self.assertEqual(commands.call_count, 2)

    def test_image_update_preserves_duplo_capitalized_container_fields(self):
        before = service()
        settings = {'AdditionalContainers': [{'Name': 'backup', 'Image': OLD},
                    {'Name': 'proxy', 'Image': 'unrelated'}],
                    'InitContainers': [{'Name': 'init', 'Image': OLD}],
                    'Volumes': [{'Name': 'providers', 'Secret': {'SecretName': 'notes'}}]}
        before['Template']['OtherDockerConfig'] = json.dumps(settings)
        desired = deploy.image_update(before, OLD, NEW)
        result = json.loads(desired['Template']['OtherDockerConfig'])
        self.assertEqual(result['AdditionalContainers'], [{'Name': 'backup', 'Image': NEW},
                         {'Name': 'proxy', 'Image': 'unrelated'}])
        self.assertEqual(result['InitContainers'], [{'Name': 'init', 'Image': NEW}])
        self.assertEqual(result['Volumes'], settings['Volumes'])
        self.assertEqual(json.loads(before['Template']['OtherDockerConfig']), settings)

    def test_source_budget_update_reuses_image_and_preserves_other_settings(self):
        before = service()
        before['Replicas'] = 0
        settings = json.loads(before['Template']['OtherDockerConfig'])
        settings['Env'] = [{'name': 'PUBLIC_URL', 'value': self.config['public_url']},
                           {'name': 'SECRET', 'valueFrom': {'secretKeyRef': {'name': 'existing', 'key': 'KEY'}}}]
        before['Template']['OtherDockerConfig'] = json.dumps(settings)
        self.cloud.save('service-before.json', before)
        self.receipt.update(image=OLD, source_bytes=134217728)
        with patch.object(self.cloud, 'is_stopped', return_value=True), \
                patch.object(self.cloud, 'service', return_value=before), \
                patch.object(self.cloud, 'command', return_value={}), patch.object(self.cloud, 'protect'):
            self.cloud.update(self.receipt)
        desired = json.loads((self.directory / 'service-desired.json').read_text())
        changed = json.loads(desired['Template']['OtherDockerConfig'])
        self.assertEqual(changed['Env'], settings['Env'] + [{'name': 'FMN_LIMIT_SOURCE_BYTES', 'value': '134217728'}])
        self.assertEqual(desired['Template']['Containers'], before['Template']['Containers'])
        self.assertEqual(desired['Replicas'], 0)
        self.assertEqual(json.loads(before['Template']['OtherDockerConfig']), settings)
        self.assertEqual(deploy.source_budget_update(desired, 134217728), desired)

    def test_source_budget_rejects_ambiguous_or_secret_owned_values(self):
        for values in ([{'name': 'FMN_LIMIT_SOURCE_BYTES', 'valueFrom': {'secretKeyRef': {'name': 'existing', 'key': 'LIMIT'}}}],
                       [{'name': 'FMN_LIMIT_SOURCE_BYTES', 'value': '1'}] * 2):
            before = service()
            before['Template']['OtherDockerConfig'] = json.dumps({'Env': values})
            with self.assertRaises(ValueError):
                deploy.source_budget_update(before, 134217728)

    def test_duplo_capitalized_environment_and_probe_fields_change_in_place(self):
        # `service find` returns Kubernetes fields capitalized. Reading them in
        # lowercase missed an existing limit and appended a duplicate.
        before = service()
        settings = json.loads(before['Template']['OtherDockerConfig'])
        settings['Env'] = [{'Name': 'PUBLIC_URL', 'Value': self.config['public_url']},
                           {'Name': 'FMN_LIMIT_SOURCE_BYTES', 'Value': '1'},
                           {'Name': 'SECRET', 'ValueFrom': {'SecretKeyRef': {'Name': 'existing', 'Key': 'KEY'}}}]
        settings['ReadinessProbe'] = {'HttpGet': {'Path': '/readyz', 'Port': 8080,
                                                  'HttpHeaders': [{'Name': 'Host', 'Value': 'notes.example.com'}]}}
        before['Template']['OtherDockerConfig'] = json.dumps(settings)
        budget = json.loads(deploy.source_budget_update(before, 134217728)['Template']['OtherDockerConfig'])
        self.assertEqual(budget['Env'], [settings['Env'][0], {'Name': 'FMN_LIMIT_SOURCE_BYTES', 'Value': '134217728'},
                                         settings['Env'][2]])
        moved = json.loads(deploy.origin_update(before, self.config['public_url'], 'https://bot.example.test')
                           ['Template']['OtherDockerConfig'])
        self.assertEqual(moved['Env'][0], {'Name': 'PUBLIC_URL', 'Value': 'https://bot.example.test'})
        self.assertEqual(moved['ReadinessProbe']['HttpGet']['HttpHeaders'], [{'Name': 'Host', 'Value': 'bot.example.test'}])

    def test_duplo_capitalized_ambiguous_or_secret_owned_values_are_refused(self):
        secret = {'ValueFrom': {'SecretKeyRef': {'Name': 'existing', 'Key': 'KEY'}}}
        cases = [(deploy.source_budget_update, (134217728,), [{'Name': 'FMN_LIMIT_SOURCE_BYTES', 'Value': '1'},
                                                              {'Name': 'FMN_LIMIT_SOURCE_BYTES', 'Value': '1'}]),
                 (deploy.source_budget_update, (134217728,), [{'Name': 'FMN_LIMIT_SOURCE_BYTES', **secret}]),
                 (deploy.origin_update, (self.config['public_url'], 'https://bot.example.test'),
                  [{'Name': 'PUBLIC_URL', **secret}])]
        for update, args, env in cases:
            before = service()
            before['Template']['OtherDockerConfig'] = json.dumps({'Env': env})
            with self.subTest(update=update.__name__, env=env), self.assertRaises(ValueError):
                update(before, *args)

    def test_origin_change_updates_only_public_origin_and_matching_probe_host(self):
        before = service()
        settings = json.loads(before['Template']['OtherDockerConfig'])
        settings['Env'] = [{'name': 'PUBLIC_URL', 'value': self.cloud.config['public_url']},
                           {'name': 'SECRET', 'valueFrom': {'secretKeyRef': {'name': 'existing', 'key': 'KEY'}}}]
        settings['ReadinessProbe'] = {'httpGet': {'path': '/readyz', 'port': 8080,
            'httpHeaders': [{'name': 'Host', 'value': 'notes.example.com'}, {'name': 'X-Check', 'value': 'keep'}]}}
        before['Template']['OtherDockerConfig'] = json.dumps(settings)
        self.cloud.save('service-before.json', before)
        stopped = copy.deepcopy(before)
        stopped['Replicas'] = 0
        self.receipt['public_url'] = 'https://bot.example.test'
        with patch.object(self.cloud, 'is_stopped', return_value=True), \
                patch.object(self.cloud, 'service', return_value=stopped), \
                patch.object(self.cloud, 'command', return_value={}), patch.object(self.cloud, 'protect'):
            self.cloud.update(self.receipt)
        desired = json.loads((self.directory / 'service-desired.json').read_text())
        result = json.loads(desired['Template']['OtherDockerConfig'])
        self.assertEqual(result['Env'][0]['value'], 'https://bot.example.test')
        self.assertEqual(result['ReadinessProbe']['httpGet']['httpHeaders'][0]['value'], 'bot.example.test')
        result['Env'][0]['value'] = settings['Env'][0]['value']
        result['ReadinessProbe']['httpGet']['httpHeaders'][0]['value'] = 'notes.example.com'
        result['additionalContainers'] = settings['additionalContainers']
        result['initContainers'] = settings['initContainers']
        self.assertEqual(result, settings)
        self.assertEqual(desired['Replicas'], 0)

    def test_origin_change_rejects_missing_ambiguous_or_changed_origin(self):
        for env in ([], [{'name': 'PUBLIC_URL', 'value': 'https://unexpected.test'}],
                    [{'name': 'PUBLIC_URL', 'valueFrom': {'secretKeyRef': {'name': 'existing', 'key': 'URL'}}}],
                    [{'name': 'PUBLIC_URL', 'value': self.config['public_url']}] * 2):
            before = service()
            settings = json.loads(before['Template']['OtherDockerConfig'])
            settings['Env'] = env
            before['Template']['OtherDockerConfig'] = json.dumps(settings)
            self.cloud.save('service-before.json', before)
            self.receipt['public_url'] = 'https://bot.example.test'
            with patch.object(self.cloud, 'is_stopped', return_value=True), \
                    patch.object(self.cloud, 'service', return_value=before), \
                    patch.object(self.cloud, 'command') as command:
                with self.assertRaisesRegex(ValueError, 'PUBLIC_URL'):
                    self.cloud.update(self.receipt)
                command.assert_not_called()

    def test_configuration_drift_is_not_overwritten(self):
        self.cloud.save('service-before.json', service())
        changed = service()
        changed['Template']['Volumes'].append({'claimName': 'different'})
        with patch.object(self.cloud, 'is_stopped', return_value=True), \
                patch.object(self.cloud, 'service', return_value=changed), patch.object(self.cloud, 'command') as commands:
            with self.assertRaisesRegex(RuntimeError, 'configuration changed'):
                self.cloud.update(self.receipt)
            commands.assert_not_called()

    def test_generated_load_balancer_details_do_not_block_image_update(self):
        before = service()
        before['Template']['LBConfigurations'] = {'http|8080': {
            'Port': '8080', 'CertificateArn': 'certificate', 'HostNames': ['notes.example.com'], 'TgArn': 'target'}}
        self.cloud.save('service-before.json', before)
        stopped = copy.deepcopy(before)
        stopped['Template']['LBConfigurations']['http|8080'].update(
            DnsName='generated.us-east-1.elb.amazonaws.com', CloudName='generated')
        with patch.object(self.cloud, 'is_stopped', return_value=True), \
                patch.object(self.cloud, 'service', return_value=stopped), \
                patch.object(self.cloud, 'command', return_value={}) as commands, patch.object(self.cloud, 'protect'):
            self.cloud.update(self.receipt)
        desired = json.loads((self.directory / 'service-desired.json').read_text())
        self.assertEqual(desired['Template']['LBConfigurations'], stopped['Template']['LBConfigurations'])
        self.assertEqual(commands.call_count, 2)

    def test_omitted_empty_service_options_do_not_block_image_update(self):
        before = service()
        before['Template'].update(AllocationTags='', OtherDockerHostConfig='', Volumes='')
        self.cloud.save('service-before.json', before)
        observed = copy.deepcopy(before)
        for name in ('AllocationTags', 'OtherDockerHostConfig', 'Volumes'):
            observed['Template'].pop(name)
        with patch.object(self.cloud, 'is_stopped', return_value=True), \
                patch.object(self.cloud, 'service', return_value=observed), \
                patch.object(self.cloud, 'command', return_value={}) as commands, patch.object(self.cloud, 'protect'):
            self.cloud.update(self.receipt)
        self.assertEqual(commands.call_count, 2)

    def test_omitted_nonempty_service_options_remain_configuration_drift(self):
        for name, value in [('AllocationTags', 'dedicated'), ('OtherDockerHostConfig', '{"Privileged":false}'), ('Volumes', '[{"name":"notes"}]')]:
            before = service()
            before['Template'][name] = value
            self.cloud.save('service-before.json', before)
            observed = copy.deepcopy(before)
            observed['Template'].pop(name)
            with self.subTest(name=name), patch.object(self.cloud, 'is_stopped', return_value=True), \
                    patch.object(self.cloud, 'service', return_value=observed), patch.object(self.cloud, 'command') as commands:
                with self.assertRaisesRegex(RuntimeError, 'configuration changed'):
                    self.cloud.update(self.receipt)
                commands.assert_not_called()

    def test_load_balancer_configuration_drift_is_not_overwritten(self):
        before = service()
        before['Template']['LBConfigurations'] = {'http|8080': {
            'Port': '8080', 'CertificateArn': 'certificate', 'HostNames': ['notes.example.com'], 'TgArn': 'target'}}
        self.cloud.save('service-before.json', before)
        for key, value in [('Port', '9090'), ('CertificateArn', 'other'), ('HostNames', ['other.example.com']), ('TgArn', 'other')]:
            changed = copy.deepcopy(before)
            changed['Template']['LBConfigurations']['http|8080'][key] = value
            with self.subTest(key=key), patch.object(self.cloud, 'is_stopped', return_value=True), \
                    patch.object(self.cloud, 'service', return_value=changed), patch.object(self.cloud, 'command') as commands:
                with self.assertRaisesRegex(RuntimeError, 'configuration changed'):
                    self.cloud.update(self.receipt)
                commands.assert_not_called()

    def prepare_with_jobs(self, jobs):
        """Run preflight against listed backup jobs; the registry check stops it after backup selection."""
        previous = {'reference': OLD, 'image': 'ghcr.io/example/notes:main-' + 'c' * 40, 'source_revision': 'c' * 40}
        pod = copy.deepcopy(self.pod)
        for container in pod['spec']['containers']:
            container['image'] = OLD
        responses = [{'Account': self.config['account']}, {'spec': {'volumeName': 'pv'}},
                     {'spec': {'persistentVolumeReclaimPolicy': 'Retain', 'csi': {'volumeHandle': 'vol-123'}}},
                     {'BackupJobs': copy.deepcopy(jobs)}]
        with patch.object(self.cloud, 'service', return_value=service()), \
                patch.object(self.cloud, 'pods', return_value=[pod]), \
                patch.object(self.cloud, 'volume', return_value={'Encrypted': True}), \
                patch.object(self.cloud, 'command', side_effect=responses), \
                patch('deploy_instance.locate_package', side_effect=RuntimeError('registry reached')) as registry:
            with self.assertRaises(RuntimeError) as failure:
                self.cloud.prepare(self.receipt, previous, {}, self.directory, self.directory)
        return str(failure.exception), registry.called

    def test_preflight_rejects_stale_or_failed_backup_before_registry_or_image_work(self):
        fresh = {**self.job, 'State': 'COMPLETED', 'CreationDate': NOW.isoformat()}
        for jobs in ([], [{**fresh, 'CreationDate': (NOW - dt.timedelta(hours=31)).isoformat()}],
                     [fresh, {**fresh, 'State': 'FAILED', 'BackupJobId': 'failed'}]):
            error, registry = self.prepare_with_jobs(jobs)
            self.assertIn('No healthy recent completed backup', error)
            self.assertFalse(registry)

    def test_preflight_never_counts_a_warmup_as_a_fresh_or_fallback_backup(self):
        daily = {**self.job, 'BackupJobId': 'daily', 'State': 'COMPLETED', 'CreationDate': (NOW - dt.timedelta(hours=2)).isoformat(),
                 'CreatedBy': {'BackupPlanId': 'plan'}, 'RecoveryPointLifecycle': {'DeleteAfterDays': 14}}
        warmup = {**self.job, 'BackupJobId': 'warmup', 'State': 'COMPLETED', 'CreationDate': NOW.isoformat(),
                  'RecoveryPointLifecycle': {'DeleteAfterDays': deploy.WARMUP_DAYS}}
        error, registry = self.prepare_with_jobs([warmup])
        self.assertIn('No healthy recent completed backup', error)
        self.assertFalse(registry)
        # A failed warm-up only lost a speedup; it does not block later deployments.
        for jobs in ([daily, warmup], [daily, {**warmup, 'State': 'FAILED'}]):
            self.receipt.pop('fallback_backup', None)
            error, registry = self.prepare_with_jobs(jobs)
            self.assertEqual((error, registry), ('registry reached', True))
            self.assertEqual(self.receipt['fallback_backup'], 'daily')

    def test_preflight_accepts_only_a_named_failure_followed_by_a_completed_backup(self):
        hours_ago = lambda hours: (NOW - dt.timedelta(hours=hours)).isoformat()
        failed = {**self.job, 'BackupJobId': 'failed', 'State': 'FAILED', 'CreationDate': hours_ago(2)}
        later = {**self.job, 'BackupJobId': 'later', 'State': 'COMPLETED', 'CreationDate': hours_ago(1),
                 'CreatedBy': {'BackupPlanId': 'plan'}}
        self.receipt['accepted_backup_failures'] = ['failed']
        error, registry = self.prepare_with_jobs([failed, later])
        self.assertEqual((error, registry), ('registry reached', True))
        self.assertEqual(self.receipt['fallback_backup'], 'later')

        unnamed = {**failed, 'BackupJobId': 'unnamed'}
        error, registry = self.prepare_with_jobs([failed, unnamed, later])
        self.assertIn('No healthy recent completed backup', error)
        self.assertFalse(registry)

        earlier = {**later, 'CreationDate': hours_ago(3)}
        warmup = {**later, 'CreatedBy': {}, 'RecoveryPointLifecycle': {'DeleteAfterDays': deploy.WARMUP_DAYS}}
        for jobs in ([failed], [failed, earlier], [failed, earlier, warmup]):
            error, registry = self.prepare_with_jobs(jobs)
            self.assertIn('no later completed backup', error)
            self.assertFalse(registry)

        for name, jobs in (('missing', [failed, later]), ('later', [failed, later])):
            self.receipt['accepted_backup_failures'] = [name]
            error, registry = self.prepare_with_jobs(jobs)
            self.assertIn('not a recent failed job', error)
            self.assertFalse(registry)

    def preflight(self, *, jobs=None, archived=True, replicas=1, account=None):
        """Run the read-only preflight; any command outside its reads fails the test."""
        previous = {'reference': OLD, 'image': 'ghcr.io/example/notes:main-' + 'c' * 40, 'source_revision': 'c' * 40,
                    'registry_digest': 'sha256:' + 'a' * 64}
        candidate = {'reference': NEW, 'image': 'ghcr.io/example/notes:main-' + 'd' * 40, 'source_revision': 'd' * 40,
                     'registry_digest': 'sha256:' + 'b' * 64}
        fresh = {**self.job, 'State': 'COMPLETED', 'CreationDate': NOW.isoformat(), 'CreatedBy': {'BackupPlanId': 'plan'}}
        pod = copy.deepcopy(self.pod)
        for container in pod['spec']['containers']:
            container['image'] = OLD
        commands = []
        def boundary(args, **kwargs):
            commands.append(args)
            if 'get-caller-identity' in args:
                return {'Account': account or self.config['account']}
            if 'pvc' in args:
                return {'spec': {'volumeName': 'pv'}}
            if 'pv' in args:
                return {'spec': {'persistentVolumeReclaimPolicy': 'Retain', 'csi': {'volumeHandle': 'vol-123'}}}
            if 'list-backup-jobs' in args:
                return {'BackupJobs': copy.deepcopy([fresh] if jobs is None else jobs)}
            if 'head-object' in args:
                return {'ContentLength': 1} if archived else None
            self.fail('Preflight ran more than a read: ' + ' '.join(args))
        def registry(uri, tag, profile=None):
            return 'path', previous['registry_digest'] if tag.endswith(previous['source_revision']) else candidate['registry_digest']
        with patch.object(self.cloud, 'service', return_value={**service(), 'Replicas': replicas}), \
                patch.object(self.cloud, 'pods', return_value=[pod]), \
                patch.object(self.cloud, 'volume', return_value={'Encrypted': True}), \
                patch.object(self.cloud, 'command', side_effect=boundary), \
                patch('deploy_instance.locate_package', side_effect=registry):
            return self.cloud.preflight({'id': 'preflight'}, previous, candidate), commands

    def test_preflight_passes_on_reads_alone(self):
        problems, commands = self.preflight()
        self.assertEqual(problems, [])
        self.assertTrue(any('head-object' in args for args in commands))

    def test_preflight_reports_every_problem_together(self):
        stale = {**self.job, 'State': 'FAILED', 'CreationDate': NOW.isoformat(), 'BackupJobId': 'failed'}
        problems, _ = self.preflight(jobs=[stale], archived=False, replicas=2)
        text = ' | '.join(problems)
        self.assertEqual(len(problems), 3, text)
        self.assertIn('one static Duplo replica', text)
        self.assertIn('No healthy recent completed backup', text)
        self.assertIn('Previous image is not protected', text)

    def test_preflight_stops_at_a_bad_sign_in_because_nothing_else_can_answer(self):
        with self.assertRaisesRegex(RuntimeError, 'wrong account'):
            self.preflight(account='999999999999')

    def archive_with_docker(self, *, local):
        """Archive a release through a real `docker` boundary replaced on PATH."""
        tools = self.directory / 'tools'
        tools.mkdir()
        log = self.directory / 'docker.jsonl'
        docker = tools / 'docker'
        docker.write_text(f'#!{sys.executable}\n' + f'''
import json, sys
args = sys.argv[1:]
with open({str(log)!r}, 'a') as output:
    output.write(json.dumps(args) + '\\n')
if args[:2] == ['image', 'inspect']:
    if {local!r}:
        print('[{{}}]')
    else:
        sys.exit('Error response from daemon: No such image: ' + args[2])
elif args[0] == 'pull':
    print('sha256:pulled')
elif args[:2] == ['image', 'save']:
    sys.stdout.buffer.write(b'image layers')
else:
    sys.exit('Unexpected docker command')
''')
        docker.chmod(0o700)
        release = {'registry_digest': 'sha256:' + 'b' * 64, 'image_id': 'sha256:' + 'e' * 64, 'source_revision': 'd' * 40,
                   'reference': NEW}
        original_command = self.cloud.command
        uploaded = {}
        def boundary(args, **kwargs):
            if args[0] == 'docker':
                return original_command(args, **kwargs)
            if 's3' in args and 'cp' in args:
                source = Path(args[args.index('cp') + 1])
                metadata = args[args.index('--metadata') + 1]
                uploaded.update(size=source.stat().st_size, sha256=metadata.split('sha256=')[1])
                return {}
            self.assertIn('head-object', args)
            if not uploaded:
                return None
            return {'ContentLength': uploaded['size'], 'VersionId': 'version', 'ServerSideEncryption': 'aws:kms',
                    'SSEKMSKeyId': self.config['kms_key'], 'ObjectLockMode': 'GOVERNANCE',
                    'ObjectLockRetainUntilDate': (NOW + dt.timedelta(days=30)).isoformat(),
                    'Metadata': {'source-revision': release['source_revision'], 'sha256': uploaded['sha256']}}
        with patch.dict(os.environ, {'PATH': str(tools) + os.pathsep + os.environ['PATH']}), \
                patch.object(self.cloud, 'command', side_effect=boundary):
            self.cloud.protect_image(release)
        return [json.loads(line) for line in log.read_text().splitlines()], release

    def test_archiving_saves_the_image_this_host_built(self):
        calls, release = self.archive_with_docker(local=True)
        self.assertEqual(calls, [['image', 'inspect', release['image_id']], ['image', 'save', release['image_id']]])

    def test_archiving_pulls_a_release_this_host_never_built(self):
        calls, release = self.archive_with_docker(local=False)
        self.assertEqual(calls, [['image', 'inspect', release['image_id']], ['pull', '--quiet', release['reference']],
                                 ['image', 'save', release['reference']]])

    def test_without_a_profile_or_kubeconfig_commands_use_the_hosts_own_sign_in(self):
        ambient = {key: value for key, value in self.config.items() if key not in ('aws_profile', 'kubeconfig')}
        path = self.directory / 'ambient.json'
        path.write_text(json.dumps(ambient))
        cloud = deploy.Runtime(deploy.configuration(path), self.directory)
        self.assertNotIn('--profile', cloud.aws)
        self.assertNotIn('--kubeconfig', cloud.kube)
        self.assertEqual(cloud.kube[:3], ['kubectl', '-n', self.config['namespace']])
        self.assertIn('--profile', self.cloud.aws)
        self.assertIn('--kubeconfig', self.cloud.kube)

    def test_a_duplo_token_signs_in_without_waiting_for_a_browser(self):
        # duploctl discards its token under --interactive, and a runner has no browser to log in with.
        def duplo_command(environment):
            unrelated = {key: value for key, value in os.environ.items() if key != 'DUPLO_TOKEN'}
            with patch.dict(os.environ, {**unrelated, **environment}, clear=True):
                return deploy.Runtime(self.config, self.directory).duplo
        self.assertNotIn('--interactive', duplo_command({'DUPLO_TOKEN': 'from-the-runner'}))
        self.assertIn('--interactive', duplo_command({}))
        self.assertIn('--interactive', duplo_command({'DUPLO_TOKEN': ' '}))

    def test_an_empty_profile_is_a_configuration_mistake_not_an_ambient_sign_in(self):
        path = self.directory / 'empty.json'
        path.write_text(json.dumps({**self.config, 'aws_profile': ' '}))
        with self.assertRaisesRegex(ValueError, 'aws_profile'):
            deploy.configuration(path)

    def test_preflight_refuses_while_another_job_for_the_volume_is_active(self):
        # The stopped backup would fail after the outage began: AWS Backup runs one job per resource.
        fresh = {**self.job, 'State': 'COMPLETED', 'CreationDate': NOW.isoformat()}
        for state in deploy.ACTIVE_JOBS:
            error, registry = self.prepare_with_jobs([fresh, {**fresh, 'BackupJobId': 'warmup', 'State': state}])
            self.assertIn('still active', error)
            self.assertFalse(registry)
        self.assertNotIn('fallback_backup', self.receipt)

    def test_warmup_request_is_a_separate_short_lived_crash_point_in_the_same_vault(self):
        with patch.object(self.cloud, 'command', return_value={'BackupJobId': 'job'}) as commands:
            self.cloud.start_backup('test-release')
            self.cloud.start_warmup('test-release')
        stopped, warmup = (call.args[0] for call in commands.call_args_list)
        option = lambda args, name: args[args.index(name) + 1]
        for name in ('--backup-vault-name', '--resource-arn', '--iam-role-arn'):
            self.assertEqual(option(warmup, name), option(stopped, name))
        self.assertNotEqual(option(warmup, '--idempotency-token'), option(stopped, '--idempotency-token'))
        self.assertEqual(option(warmup, '--recovery-point-tags'), 'Consistency=crash,Purpose=release-warmup')
        self.assertEqual(option(stopped, '--recovery-point-tags'), 'Consistency=unverified,Purpose=release-upgrade')
        self.assertEqual(option(warmup, '--lifecycle'), 'DeleteAfterDays=' + str(deploy.WARMUP_DAYS))
        self.assertTrue(deploy.release_warmup({'RecoveryPointLifecycle': {'DeleteAfterDays': deploy.WARMUP_DAYS}}))
        # The vault lock fails jobs retained for less than its minimum.
        import durability
        lock = durability.storage({'name': 'notes'})['Vault']['Properties']['LockConfiguration']
        self.assertEqual(deploy.WARMUP_DAYS, lock['MinRetentionDays'])

    def test_warmup_wait_returns_failure_without_recording_a_backup_result(self):
        job = {**self.job, 'BackupJobId': 'warmup-1'}
        for state in ('COMPLETED',) + deploy.TERMINAL_FAILURES:
            with self.subTest(state=state), patch.object(self.cloud, 'backup_job', return_value={**job, 'State': state}), \
                    patch.object(self.cloud, 'protect') as protect:
                self.assertEqual(self.cloud.wait_warmup('warmup-1')['State'], state)
                protect.assert_not_called()
        self.assertEqual(list(self.directory.iterdir()), [])
        monotonic, sleep = self.poll_clock()
        with patch.object(self.cloud, 'backup_job', return_value=job), monotonic, sleep:
            with self.assertRaisesRegex(RuntimeError, 'did not finish'):
                self.cloud.wait_warmup('warmup-1')
        with patch.object(self.cloud, 'backup_job', return_value={**job, 'ResourceArn': 'wrong', 'State': 'COMPLETED'}):
            with self.assertRaisesRegex(RuntimeError, 'does not belong'):
                self.cloud.wait_warmup('warmup-1')

    def test_command_failure_keeps_raw_output_private_without_disclosing_it_in_error(self):
        command = [sys.executable, '-c', 'import sys; print("synthetic-secret"); print("original failure", file=sys.stderr); sys.exit(7)']
        with self.assertRaises(RuntimeError) as failure:
            self.cloud.command(command)
        self.assertNotIn('synthetic-secret', str(failure.exception))
        path = next(self.directory.glob('command-*.json'))
        result = json.loads(path.read_text())
        self.assertEqual(result['exit_code'], 7)
        self.assertEqual(result['stdout'], 'synthetic-secret\n')
        self.assertEqual(result['stderr'], 'original failure\n')
        self.assertEqual(path.stat().st_mode & 0o777, 0o600)

    def serve_public(self, responses):
        requests = []
        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                requests.append(self.path)
                status, body = responses.pop(0)
                self.send_response(status)
                self.end_headers()
                self.wfile.write(body)
            def log_message(self, *args):
                pass
        server = HTTPServer(('127.0.0.1', 0), Handler)
        thread = threading.Thread(target=server.serve_forever)
        thread.start()
        self.addCleanup(thread.join)
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        self.receipt['public_url'] = 'http://127.0.0.1:' + str(server.server_port)
        return requests

    def poll_clock(self):
        # Advance a fake clock on each poll instead of sleeping in real time.
        now = [0]
        return (patch('deploy_instance.time.monotonic', side_effect=lambda: now[0]),
                patch('deploy_instance.time.sleep', side_effect=lambda seconds: now.__setitem__(0, now[0] + seconds)))

    def test_load_balancer_503_is_polled_until_both_public_endpoints_return_200(self):
        balancer = (503, b'<html><head><title>503 Service Temporarily Unavailable</title></head></html>')
        requests = self.serve_public([balancer, balancer, (200, b'ok'), balancer, (200, b'ok'), (200, b'ready')])
        self.cloud.timeout = 60
        monotonic, sleep = self.poll_clock()
        with patch.object(self.cloud, 'pods', return_value=[self.pod]), \
                patch.object(self.cloud, 'command') as diagnostics, monotonic, sleep as sleeps:
            self.cloud.wait_ready(self.receipt)
        self.assertEqual(requests, ['/healthz', '/healthz', '/healthz', '/readyz', '/healthz', '/readyz'])
        self.assertEqual(sleeps.call_count, 3)
        self.assertEqual(json.loads((self.directory / 'healthz.json').read_text()), {'status': 200, 'body': 'ok'})
        self.assertEqual(json.loads((self.directory / 'readyz.json').read_text()), {'status': 200, 'body': 'ready'})
        diagnostics.assert_not_called()

    def test_public_failure_at_deadline_retains_last_status_body_and_collects_pod_context(self):
        requests = self.serve_public([(503, b'{"error":"index unavailable"}'), (502, b'bad gateway')])
        monotonic, sleep = self.poll_clock()
        with patch.object(self.cloud, 'pods', return_value=[self.pod]), \
                patch.object(self.cloud, 'command', return_value={}) as diagnostics, monotonic, sleep:
            with self.assertRaisesRegex(RuntimeError, r'^healthz still returned HTTP 502 at the readiness deadline'):
                self.cloud.wait_ready(self.receipt)
        self.assertEqual(requests, ['/healthz', '/healthz'])
        self.assertEqual(json.loads((self.directory / 'healthz.json').read_text()), {'status': 502, 'body': 'bad gateway'})
        self.assertFalse((self.directory / 'readyz.json').exists())
        self.assertEqual(diagnostics.call_count, 4, 'events, pod and both application container logs')

    def test_pod_that_never_becomes_ready_is_reported_without_public_requests(self):
        requests = self.serve_public([])
        self.pod['status']['containerStatuses'][1]['ready'] = False
        monotonic, sleep = self.poll_clock()
        with patch.object(self.cloud, 'pods', return_value=[self.pod]), \
                patch.object(self.cloud, 'command', return_value={}) as diagnostics, monotonic, sleep:
            with self.assertRaisesRegex(RuntimeError, '^New application and sidecars did not become ready'):
                self.cloud.wait_ready(self.receipt)
        self.assertEqual(requests, [])
        self.assertEqual(diagnostics.call_count, 4, 'events, pod and both application container logs')

    def test_image_archive_requires_verified_identity_and_retains_existing_version(self):
        release = {'registry_digest': 'sha256:' + 'a' * 64, 'image_id': 'sha256:' + 'b' * 64, 'source_revision': 'c' * 40}
        head = {'ContentLength': 123, 'ServerSideEncryption': 'aws:kms', 'SSEKMSKeyId': self.config['kms_key'],
                'ObjectLockMode': 'GOVERNANCE', 'VersionId': 'protected-version', 'ObjectLockRetainUntilDate': NOW.isoformat(),
                'Metadata': {'source-revision': release['source_revision'], 'sha256': 'd' * 64}}
        with patch.object(self.cloud, 'command', side_effect=[head, {}]) as commands:
            archive = self.cloud.protect_image(release, existing=True)
        self.assertEqual(archive['version'], 'protected-version')
        self.assertIn('put-object-retention', commands.call_args.args[0])
        self.assertIn('protected-version', commands.call_args.args[0])
        for invalid in (None, {**head, 'ServerSideEncryption': 'AES256'}, {**head, 'Metadata': {}},
                        {**head, 'SSEKMSKeyId': 'wrong-key'}):
            with patch.object(self.cloud, 'command', return_value=invalid), patch('deploy_instance.subprocess.Popen') as docker:
                with self.assertRaises(RuntimeError):
                    self.cloud.protect_image(release, existing=True)
                docker.assert_not_called()

    def test_completed_job_without_stopped_capture_never_gets_consistency_tag(self):
        self.receipt.pop('snapshot')
        with patch.object(self.cloud, 'backup_job', return_value={**self.job, 'State': 'COMPLETED'}), \
                patch.object(self.cloud, 'tag_completed') as tag, patch.object(self.cloud, 'protect'):
            with self.assertRaisesRegex(RuntimeError, 'consistency remains unverified'):
                deploy.refresh_backup(self.cloud, self.receipt)
            tag.assert_not_called()

    def test_protected_source_restores_committed_content_and_release_receipts(self):
        source = self.directory / 'source'
        source.mkdir()
        for args in (['init', '-b', 'main'], ['config', 'user.name', 'Fixture'], ['config', 'user.email', 'fixture@localhost']):
            subprocess.run(['git', '-C', str(source), *args], check=True, capture_output=True)
        (source / 'README.md').write_text('Committed recovery source\n')
        subprocess.run(['git', '-C', str(source), 'add', '.'], check=True, capture_output=True)
        subprocess.run(['git', '-C', str(source), 'commit', '-m', 'Source'], check=True, capture_output=True)
        revision = subprocess.check_output(['git', '-C', str(source), 'rev-parse', 'HEAD'], text=True).strip()
        (source / 'README.md').write_text('Uncommitted private content\n')
        candidate = self.directory / 'candidate'
        candidate.mkdir()
        for name in ('publication.json', 'verification.json'):
            (candidate / name).write_text(json.dumps({'source_revision': revision, 'image_id': 'sha256:fixture'}))
        original_command = self.cloud.command
        uploaded = {}
        def boundary(args, **kwargs):
            if args[0] == 'git':
                return original_command(args, **kwargs)
            self.assertIn('put-object', args)
            self.assertEqual(args[args.index('--object-lock-mode') + 1], 'GOVERNANCE')
            self.assertEqual(args[args.index('--ssekms-key-id') + 1], self.config['kms_key'])
            key = args[args.index('--key') + 1]
            uploaded[key] = Path(args[args.index('--body') + 1]).read_bytes()
            return {'VersionId': 'protected-version'}
        self.receipt['source_revision'] = revision
        with patch.object(self.cloud, 'command', side_effect=boundary):
            self.cloud.protect_release(candidate, source, self.receipt)
        assets = self.receipt['release_assets']
        archive = self.directory / 'download.tar.gz'
        archive.write_bytes(uploaded[assets['source']['key']])
        self.assertEqual(hashlib.sha256(archive.read_bytes()).hexdigest(), assets['source']['sha256'])
        restored = self.directory / 'restored'
        with tarfile.open(archive) as bundle:
            bundle.extractall(restored, filter='data')
        self.assertEqual((restored / 'README.md').read_text(), 'Committed recovery source\n')
        for name in ('publication.json', 'verification.json'):
            self.assertEqual(json.loads(uploaded[assets[name]['key']]), json.loads((candidate / name).read_text()))


class DeploymentCLITests(unittest.TestCase):
    """Real CLI/Git/files, with subprocess cloud commands replaced at their boundary."""
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.repo = self.root / 'repo'
        source = Path(__file__).parent
        target = self.repo / 'deploy/production'
        target.mkdir(parents=True)
        for name in ('deploy_instance.py', 'release_image.py', 'verification.py', 'verification_plan.py', 'durability_worker.py'):
            shutil.copyfile(source / name, target / name)
        (self.repo / '.gitignore').write_text('__pycache__/\n')
        (self.repo / 'main.go').write_text('package main\n')
        self.git('init', '-b', 'main')
        self.git('config', 'user.name', 'Deployment fixture')
        self.git('config', 'user.email', 'fixture@localhost')
        self.git('add', '.')
        self.git('commit', '-m', 'Initial source')
        self.revision = self.git('rev-parse', 'HEAD').stdout.strip()
        self.previous = self.root / 'previous'
        self.previous.mkdir()
        self.publication = {'source_revision': self.revision, 'image_id': 'sha256:' + 'c' * 64,
                            'image': 'ghcr.io/example/notes:main-' + self.revision, 'registry_digest': 'sha256:' + 'a' * 64}
        (self.previous / 'publication.json').write_text(json.dumps(self.publication))
        (self.previous / 'verification.json').write_text(json.dumps({**self.publication, 'complete': True,
            'expected_checks': ['synthetic'], 'checks': {'synthetic': {'status': 'passed', 'exit_code': 0}}}))
        self.config = json.loads((source / 'deploy_instance.example.json').read_text())
        self.config_path = self.root / 'operator.json'
        self.config_path.write_text(json.dumps(self.config))
        (self.root / 'service.json').write_text(json.dumps(service()))
        tools = self.root / 'tools'
        tools.mkdir()
        for name in ('aws', 'duploctl', 'kubectl', 'gh', 'docker'):
            tool = tools / name
            tool.write_text(f'#!{sys.executable}\n' + '''
import json, os, pathlib, sys
root = pathlib.Path(os.environ['DEPLOY_FIXTURE'])
tool = pathlib.Path(sys.argv[0]).name
args = sys.argv[1:]
with (root / 'calls.jsonl').open('a') as log:
    log.write(json.dumps({'tool': tool, 'args': args}) + '\\n')
if tool == 'aws' and ('--output' not in args or args[args.index('--output') + 1] != 'json'):
    print('AWS PROFILE DEFAULTS TO TABLE OUTPUT')
    sys.exit(0)
responses = root / 'responses.json'
if responses.exists():
    for name, needles, output in json.loads(responses.read_text()):
        if tool == name and all(needle in args for needle in needles):
            if output is None:
                sys.exit('An error occurred (404) when calling the HeadObject operation: Not Found')
            print(json.dumps(output))
            sys.exit(0)
if tool == 'duploctl' and args[-3:] == ['service', 'find', 'forgetmenote']:
    print((root / 'service.json').read_text())
elif tool == 'aws' and 'describe-backup-job' in args:
    sequence = root / 'jobs.json'
    if sequence.exists():
        jobs = json.loads(sequence.read_text())
        print(json.dumps(jobs[0]))
        sequence.write_text(json.dumps(jobs[1:] or jobs))
    else:
        print((root / 'job.json').read_text())
elif tool == 'aws' and 'create-tags' in args:
    print('{}')
elif tool == 'aws' and 'put-object' in args:
    print('{"VersionId":"fixture-version"}')
else:
    sys.exit('Unexpected external command: ' + tool)
''')
            tool.chmod(0o700)
        self.env = {**os.environ, 'HOME': str(self.root / 'home'), 'DEPLOY_FIXTURE': str(self.root),
                    'PATH': str(tools) + os.pathsep + os.environ['PATH']}
        self.state = self.root / 'state'

    def git(self, *args):
        return subprocess.run(['git', '-C', str(self.repo), *args], check=True, text=True, capture_output=True)

    def cli(self, *args):
        return subprocess.run([sys.executable, str(self.repo / 'deploy/production/deploy_instance.py'),
            '--config', str(self.config_path), '--previous-release', str(self.previous),
            '--state-dir', str(self.state), *args], env=self.env, text=True, capture_output=True)

    def calls(self):
        path = self.root / 'calls.jsonl'
        return [json.loads(line) for line in path.read_text().splitlines()] if path.exists() else []

    def change(self, path, content):
        (self.repo / path).write_text(content)
        self.git('add', '.')
        self.git('commit', '-m', 'Change candidate')

    def test_tooling_only_cli_checks_live_image_without_snapshot_update_or_candidate_build(self):
        self.change('deploy/production/README.md', 'Operator change\n')
        result = self.cli()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('no image update, snapshot or service restart', result.stdout)
        self.assertEqual([(c['tool'], c['args'][-3:]) for c in self.calls()],
                         [('duploctl', ['service', 'find', 'forgetmenote'])])

    def test_runtime_plan_needs_no_cloud_access_or_candidate_image(self):
        self.change('main.go', 'package main\n// change\n')
        result = self.cli('--plan')
        self.assertEqual(result.returncode, 0, result.stderr)
        plan = json.loads(result.stdout)
        self.assertTrue(plan['deployment_required'])
        self.assertTrue(plan['wait_for_completed_backup'])
        self.assertTrue(plan['warm_up_before_stop'])
        self.assertEqual(plan['runtime_changes'], ['main.go'])
        compatible = json.loads(self.cli('--plan', '--compatibility', 'compatible').stdout)
        self.assertFalse(compatible['warm_up_before_stop'])
        self.assertEqual(self.calls(), [])

    def test_origin_only_plan_and_deployment_reuse_the_verified_image(self):
        result = self.cli('--plan', '--public-url', 'https://bot.example.test')
        self.assertEqual(result.returncode, 0, result.stderr)
        plan = json.loads(result.stdout)
        self.assertTrue(plan['deployment_required'])
        self.assertEqual(plan['public_url'], 'https://bot.example.test')
        self.assertEqual(plan['runtime_changes'], [])
        self.assertEqual(self.calls(), [])
        result = self.cli('--public-url', 'https://bot.example.test', '--release-dir', str(self.previous))
        # This fixture stops at cloud authentication, after the verified image
        # has been accepted. It must not take the ordinary no-runtime-change exit.
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn('Candidate release does not match', result.stderr)
        self.assertEqual(len(self.calls()), 1)
        self.assertIn('get-caller-identity', self.calls()[0]['args'])

    def test_source_budget_only_plan_and_deployment_reuse_the_verified_image(self):
        result = self.cli('--plan', '--source-bytes', '134217728')
        self.assertEqual(result.returncode, 0, result.stderr)
        plan = json.loads(result.stdout)
        self.assertTrue(plan['deployment_required'])
        self.assertEqual(plan['source_bytes'], 134217728)
        self.assertEqual(plan['runtime_changes'], [])
        self.assertEqual(self.calls(), [])
        result = self.cli('--source-bytes', '134217728', '--release-dir', str(self.previous))
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn('Candidate release does not match', result.stderr)
        self.assertEqual(len(self.calls()), 2)
        self.assertIn('get-caller-identity', self.calls()[-1]['args'])

    def test_source_budget_invalid_values_do_not_contact_cloud(self):
        for value in ('0', '-1', str(sys.maxsize)):
            result = self.cli('--source-bytes', value)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn('--source-bytes must be', result.stderr)
        self.assertEqual(self.calls(), [])

    def candidate(self, repository='ghcr.io/example/notes'):
        self.change('main.go', 'package main\n// verified runtime change\n')
        revision = self.git('rev-parse', 'HEAD').stdout.strip()
        directory = self.root / 'candidate'
        directory.mkdir()
        publication = {**self.publication, 'source_revision': revision, 'image': repository + ':main-' + revision,
                       'registry_digest': 'sha256:' + 'e' * 64}
        (directory / 'publication.json').write_text(json.dumps(publication))
        (directory / 'verification.json').write_text(json.dumps({**publication, 'complete': True,
            'expected_checks': ['synthetic'], 'checks': {'synthetic': {'status': 'passed', 'exit_code': 0}}}))
        return directory

    def test_operator_change_reuses_verified_runtime_candidate(self):
        candidate = self.candidate()
        self.change('deploy/production/README.md', 'Updated operator instructions\n')
        result = self.cli('--release-dir', str(candidate))
        # The synthetic cloud stops at authentication. Reaching this boundary
        # proves the committed operator change did not demand a rebuilt image.
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn('Candidate release does not match', result.stderr)
        self.assertEqual(len(self.calls()), 1)
        self.assertIn('get-caller-identity', self.calls()[0]['args'])

    def test_runtime_change_cannot_reuse_older_candidate(self):
        candidate = self.candidate()
        self.change('main.go', 'package main\n// unverified runtime change\n')
        result = self.cli('--release-dir', str(candidate))
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('Candidate release does not match', result.stderr)
        self.assertEqual(self.calls(), [])

    def test_compatible_update_requires_explanation_before_external_calls(self):
        self.change('main.go', 'package main\n// change\n')
        result = self.cli('--compatibility', 'compatible')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('--compatibility-note', result.stderr)
        self.assertEqual(self.calls(), [])

    def preflight_responses(self, candidate, *, archived=True):
        publication = json.loads((candidate / 'publication.json').read_text())
        versions = [[{'name': self.publication['registry_digest'],
                      'metadata': {'container': {'tags': ['main-' + self.publication['source_revision']]}}},
                     {'name': publication['registry_digest'],
                      'metadata': {'container': {'tags': ['main-' + publication['source_revision']]}}}]]
        pod = {'metadata': {'name': 'forgetmenote-0'},
               'spec': {'containers': [{'name': 'app', 'image': OLD}],
                        'volumes': [{'persistentVolumeClaim': {'claimName': self.config['claim']}}]},
               'status': {'containerStatuses': [{'ready': True, 'restartCount': 0}]}}
        job = {'BackupJobId': 'daily', 'State': 'COMPLETED', 'CreationDate': dt.datetime.now(dt.timezone.utc).isoformat(),
               'CreatedBy': {'BackupPlanId': 'plan'}}
        (self.root / 'responses.json').write_text(json.dumps([
            ['aws', ['get-caller-identity'], {'Account': self.config['account']}],
            ['kubectl', ['get', 'pods'], {'items': [pod]}],
            ['kubectl', ['get', 'pvc'], {'spec': {'volumeName': 'pv-1'}}],
            ['kubectl', ['get', 'pv'], {'spec': {'persistentVolumeReclaimPolicy': 'Retain', 'csi': {'volumeHandle': self.config['volume']}}}],
            ['aws', ['describe-volumes'], {'Volumes': [{'Encrypted': True}]}],
            ['aws', ['list-backup-jobs'], {'BackupJobs': [job]}],
            ['aws', ['head-object'], {'ContentLength': 1} if archived else None],
            ['gh', ['users/example'], {'type': 'User'}],
            ['gh', ['users/example/packages?package_type=container&per_page=1'], []],
            ['gh', ['users/example/packages/container/notes'], {'visibility': 'private'}],
            ['gh', ['users/example/packages/container/notes/versions?per_page=100'], versions]]))

    def preflight_cli(self, candidate):
        return self.cli('--preflight', '--release-dir', str(candidate),
                        '--compatibility', 'compatible', '--compatibility-note', 'reviewed')

    def assert_nothing_changed(self):
        writes = {'put-object', 'put-object-retention', 'invoke', 'start-backup-job', 'cp', 'update', 'update_image', 'stop', 'start'}
        self.assertEqual([call for call in self.calls() if writes & set(call['args'])], [])

    def test_preflight_passes_without_changing_anything(self):
        candidate = self.candidate()
        self.preflight_responses(candidate)
        result = self.preflight_cli(candidate)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('Preflight passed', result.stdout)
        self.assert_nothing_changed()
        self.assertFalse((self.state / 'deployment.json').exists())

    def test_preflight_moves_from_a_ghcr_release_to_ecr_with_the_configured_profile(self):
        ecr = '123456789012.dkr.ecr.us-east-1.amazonaws.com/notes'
        candidate = self.candidate(ecr)
        self.preflight_responses(candidate)
        responses = json.loads((self.root / 'responses.json').read_text())
        revision = json.loads((candidate / 'publication.json').read_text())['source_revision']
        responses.append(['aws', ['describe-images', 'imageTag=main-' + revision], {'imageDetails': [{'imageDigest': 'sha256:' + 'e' * 64}]}])
        (self.root / 'responses.json').write_text(json.dumps(responses))
        result = self.preflight_cli(candidate)
        self.assertEqual(result.returncode, 0, result.stderr)
        lookup = next(c['args'] for c in self.calls() if 'describe-images' in c['args'])
        self.assertEqual(lookup[:4], ['--profile', self.config['aws_profile'], '--region', 'us-east-1'])
        self.assertEqual(lookup[lookup.index('--registry-id') + 1], '123456789012')
        self.assertEqual(lookup[lookup.index('--repository-name') + 1], 'notes')
        # The release it replaces is still checked where it was published.
        self.assertTrue(any(c['tool'] == 'gh' and 'users/example/packages/container/notes' in c['args'] for c in self.calls()))
        self.assert_nothing_changed()

    def test_release_receipts_name_only_private_ghcr_or_ecr_images(self):
        for image in ('docker.io/example/notes', '123456789012.dkr.ecr.us-east-1.amazonaws.com/notes',
                      'ghcr.io/example/notes'):
            publication = {**self.publication, 'image': image + ':main-' + self.revision}
            (self.previous / 'publication.json').write_text(json.dumps(publication))
            if image.startswith('docker.io'):
                with self.assertRaisesRegex(ValueError, 'pinned private'):
                    deploy.release_record(self.previous)
            else:
                self.assertEqual(deploy.release_record(self.previous)['reference'], image + '@' + publication['registry_digest'])

    def test_preflight_failure_names_the_problem_and_exits_nonzero(self):
        candidate = self.candidate()
        self.preflight_responses(candidate, archived=False)
        result = self.preflight_cli(candidate)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('Previous image is not protected', result.stderr)
        self.assert_nothing_changed()

    def test_preflight_has_its_own_state_directory(self):
        candidate = self.candidate()
        self.preflight_responses(candidate)
        states = self.root / 'states'
        self.config_path.write_text(json.dumps({**self.config, 'state_root': str(states)}))
        result = subprocess.run([sys.executable, str(self.repo / 'deploy/production/deploy_instance.py'),
            '--config', str(self.config_path), '--previous-release', str(self.previous), '--preflight',
            '--release-dir', str(candidate), '--compatibility', 'compatible', '--compatibility-note', 'reviewed'],
            env=self.env, text=True, capture_output=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual([path.name.split('-')[0] for path in states.iterdir()], ['preflight'])

    def test_preflight_cannot_be_combined_with_another_mode(self):
        result = self.cli('--preflight', '--plan')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('mutually exclusive', result.stderr)
        self.assertEqual(self.calls(), [])

    def test_accepted_backup_failure_requires_explanation_before_external_calls(self):
        self.change('main.go', 'package main\n// change\n')
        for flags in (('--accept-failed-backup', 'job'), ('--accepted-failure-note', 'reviewed')):
            result = self.cli(*flags)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn('--accepted-failure-note', result.stderr)
            self.assertEqual(self.calls(), [])

    def test_incomplete_release_evidence_never_reaches_cloud(self):
        (self.previous / 'verification.json').write_text(json.dumps({**self.publication, 'complete': False}))
        result = self.cli('--plan')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('complete verification', result.stderr)
        self.assertEqual(self.calls(), [])

    def bare_cli(self, *args):
        return subprocess.run([sys.executable, str(self.repo / 'deploy/production/deploy_instance.py'),
            '--config', str(self.config_path), *args], env=self.env, text=True, capture_output=True)

    def deployment(self, name, receipt, *, release=True):
        directory = self.root / 'deployments' / name
        directory.mkdir(parents=True)
        receipt = {'config_fingerprint': deploy.fingerprint(deploy.configuration(self.config_path)), **receipt}
        (directory / 'deployment.json').write_text(json.dumps(receipt))
        if release:
            for name in ('publication.json', 'verification.json'):
                shutil.copyfile(self.previous / name, directory / name)
        return directory

    def test_help_explains_workflow_options_and_diagnostics(self):
        result = self.bare_cli('--help')
        self.assertEqual(result.returncode, 0, result.stderr)
        for text in ('--plan', '--status', '--wait', 'state_root', 'compatible starts', 'deployment.json', 'failure-'):
            self.assertIn(text, result.stdout)

    def test_paths_default_to_the_state_root_and_newest_ready_release(self):
        self.config['state_root'] = str(self.root / 'deployments')
        self.config_path.write_text(json.dumps(self.config))
        reference = 'ghcr.io/example/notes@' + self.publication['registry_digest']
        ready = self.deployment('deploy-20261008-aaaaaaa', {'phase': 'ready', 'image': reference, 'started_at': '2026-10-08T00:00:00+00:00'})
        self.deployment('deploy-20261009-bbbbbbb', {'phase': 'starting', 'image': reference, 'started_at': '2026-10-09T00:00:00+00:00'})
        self.deployment('deploy-20261010-ccccccc', {'phase': 'ready', 'image': 'ghcr.io/example/notes@sha256:' + 'f' * 64,
                                                    'started_at': '2026-10-10T00:00:00+00:00'})
        self.change('main.go', 'package main\n// change\n')
        result = self.bare_cli('--plan')
        self.assertEqual(result.returncode, 0, result.stderr)
        plan = json.loads(result.stdout)
        self.assertEqual(plan['previous_release'], str(ready.resolve()))
        revision = self.git('rev-parse', 'HEAD').stdout.strip()
        self.assertRegex(plan['state_dir'], r'/deployments/deploy-\d{8}-' + revision[:7] + '$')
        self.assertEqual(self.calls(), [])

    def test_status_defaults_to_the_newest_deployment(self):
        self.config['state_root'] = str(self.root / 'deployments')
        self.config_path.write_text(json.dumps(self.config))
        self.deployment('older', {'id': 'old', 'phase': 'ready', 'image': NEW, 'backup_job': 'job-1', 'started_at': '2026-10-08T00:00:00+00:00'})
        newest = self.deployment('newer', {'id': 'new', 'phase': 'ready', 'image': NEW, 'started_at': '2026-10-09T00:00:00+00:00'})
        result = self.bare_cli('--status')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn(str(newest.resolve()), result.stdout)
        self.assertIn('no backup has been requested', result.stdout)
        self.assertEqual(self.calls(), [])

    def test_status_never_reports_a_warmup_as_the_upgrade_backup(self):
        self.state.mkdir()
        receipt = {'id': 'release-1', 'phase': 'prepared', 'image': NEW,
                   'warmup': {'requested_at': NOW.isoformat(), 'job': 'warmup-1', 'state': 'COMPLETED'},
                   'config_fingerprint': deploy.fingerprint(deploy.configuration(self.config_path))}
        (self.state / 'deployment.json').write_text(json.dumps(receipt))
        for args in (('--status',), ('--status', '--wait')):
            result = self.cli(*args)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn('Service phase: prepared; no backup has been requested.', result.stdout)
        self.assertEqual(self.calls(), [])

    def test_status_wait_blocks_until_the_backup_completes(self):
        self.state.mkdir()
        receipt = {'id': 'release-1', 'phase': 'ready', 'image': NEW, 'backup_job': 'job-1', 'snapshot': {'id': 'snap-1'},
                   'config_fingerprint': deploy.fingerprint(deploy.configuration(self.config_path))}
        (self.state / 'deployment.json').write_text(json.dumps(receipt))
        job = {'BackupJobId': 'job-1', 'ResourceArn': deploy.volume_arn(self.config), 'BackupVaultName': self.config['vault'],
               'RecoveryPointArn': 'arn:aws:ec2:us-east-1::snapshot/snap-1'}
        (self.root / 'jobs.json').write_text(json.dumps([{**job, 'State': 'RUNNING'}, {**job, 'State': 'COMPLETED'}]))
        result = self.cli('--status', '--wait')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('backup: COMPLETED', result.stdout)
        self.assertEqual(sum('describe-backup-job' in c['args'] for c in self.calls()), 2)
        self.assertEqual(json.loads((self.state / 'deployment.json').read_text())['backup_state'], 'COMPLETED')
        self.assertNotEqual(self.bare_cli('--wait').returncode, 0)

    def test_completed_deployment_status_reports_backup_failure_without_redeploying(self):
        self.state.mkdir()
        receipt = {'id': 'release-1', 'phase': 'ready', 'image': NEW, 'backup_job': 'failed',
                   'config_fingerprint': deploy.fingerprint(deploy.configuration(self.config_path))}
        (self.state / 'deployment.json').write_text(json.dumps(receipt))
        result = self.cli('--plan')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.calls(), [])
        (self.root / 'job.json').write_text(json.dumps({'State': 'FAILED', 'BackupJobId': 'failed',
            'ResourceArn': deploy.volume_arn(self.config), 'BackupVaultName': self.config['vault']}))
        result = self.cli('--status')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('Upgrade backup failed is FAILED', result.stderr)
        saved = json.loads((self.state / 'deployment.json').read_text())
        self.assertEqual(saved['phase'], 'ready')
        self.assertEqual(saved['backup_state'], 'FAILED')
        self.assertTrue(all(call['tool'] == 'aws' for call in self.calls()))
        self.assertEqual(sum('describe-backup-job' in c['args'] for c in self.calls()), 1)



class ProviderSecretMountTests(unittest.TestCase):
    def test_adds_only_explicit_keys_without_overwriting_existing_mounts(self):
        original = {'Template': {'OtherDockerConfig': json.dumps({'Volumes': [
            {'name': 'providers', 'secret': {'secretName': 'notes', 'items': [{'key': 'CHAT_CONFIG', 'path': 'chat.json'}]}},
            {'name': 'other', 'secret': {'secretName': 'unrelated'}},
        ]})}}
        desired = deploy.provider_secret_mounts(original, 'notes', ['CLOUDFLARE_ACCOUNT_ID', 'CLOUDFLARE_BROWSER_RUN_TOKEN'])
        volumes = json.loads(desired['Template']['OtherDockerConfig'])['Volumes']
        self.assertEqual(volumes[0]['secret']['items'], [{'key': 'CHAT_CONFIG', 'path': 'chat.json'},
            {'key': 'CLOUDFLARE_ACCOUNT_ID', 'path': 'CLOUDFLARE_ACCOUNT_ID'},
            {'key': 'CLOUDFLARE_BROWSER_RUN_TOKEN', 'path': 'CLOUDFLARE_BROWSER_RUN_TOKEN'}])
        self.assertEqual(volumes[1], json.loads(original['Template']['OtherDockerConfig'])['Volumes'][1])
        self.assertEqual(deploy.provider_secret_mounts(desired, 'notes', ['CLOUDFLARE_ACCOUNT_ID']), desired)
        self.assertNotIn('CLOUDFLARE', original['Template']['OtherDockerConfig'])
        with self.assertRaises(ValueError):
            deploy.provider_secret_mounts(original, 'another-secret', ['CLOUDFLARE_ACCOUNT_ID'])
        for keys in [['../token'], ['TOKEN','TOKEN'], 'TOKEN']:
            with self.assertRaises(ValueError):
                deploy.provider_secret_mounts(original, 'notes', keys)

    def test_retains_duplo_capitalized_secret_inventory(self):
        original = {'Template': {'OtherDockerConfig': json.dumps({'Volumes': [
            {'Name': 'providers', 'Secret': {'SecretName': 'notes', 'DefaultMode': 288,
             'Items': [{'Key': 'CHAT_CONFIG', 'Path': 'chat.json'}]}},
            {'Name': 'other', 'Secret': {'SecretName': 'unrelated'}},
        ]})}}
        desired = deploy.provider_secret_mounts(original, 'notes', ['CLOUDFLARE_ACCOUNT_ID'])
        volumes = json.loads(desired['Template']['OtherDockerConfig'])['Volumes']
        self.assertEqual(volumes[0]['Secret']['Items'], [{'Key': 'CHAT_CONFIG', 'Path': 'chat.json'},
                         {'Key': 'CLOUDFLARE_ACCOUNT_ID', 'Path': 'CLOUDFLARE_ACCOUNT_ID'}])
        self.assertEqual(volumes[0]['Secret']['DefaultMode'], 288)
        self.assertEqual(volumes[1], json.loads(original['Template']['OtherDockerConfig'])['Volumes'][1])
        self.assertEqual(deploy.provider_secret_mounts(desired, 'notes', ['CLOUDFLARE_ACCOUNT_ID']), desired)
        self.assertNotIn('CLOUDFLARE', original['Template']['OtherDockerConfig'])
        with self.assertRaises(ValueError):
            deploy.provider_secret_mounts(original, 'another-secret', ['CLOUDFLARE_ACCOUNT_ID'])

class ProviderSecretInventoryTests(unittest.TestCase):
    def test_presence_preflight_logs_only_booleans(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            fake = root / 'kubectl.py'
            fake.write_text('import json,sys\nif not sys.argv[-1].startswith("go-template="): print(json.dumps({"data":{"CLOUDFLARE_ACCOUNT_ID":"synthetic-private-value"}}))\nelse: print(json.dumps({"CLOUDFLARE_ACCOUNT_ID":True}))\n')
            config = json.loads(Path(__file__).with_name('deploy_instance.example.json').read_text())
            config['provider_secret_keys'] = ['CLOUDFLARE_ACCOUNT_ID']
            cloud = deploy.Runtime(config, root, timeout=1)
            cloud.kube = [sys.executable, str(fake)]
            cloud.check_provider_secret_keys()
            logged = (root / 'command-0001.json').read_text()
            self.assertNotIn('synthetic-private-value', logged)
            self.assertEqual(json.loads(json.loads(logged)['stdout']), {'CLOUDFLARE_ACCOUNT_ID': True})


if __name__ == '__main__':
    unittest.main()
