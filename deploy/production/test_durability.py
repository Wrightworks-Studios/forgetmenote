"""Recovery configuration and read-only export behavior."""
import datetime as dt
import importlib.util
import json
from pathlib import Path
import unittest
from unittest.mock import patch


def load(name):
    spec = importlib.util.spec_from_file_location(name, Path(__file__).with_name(name + '.py'))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


worker, templates = load('durability_worker'), load('durability')


class Runtime:
    def __init__(self, failure=None):
        self.calls, self.failure = [], failure

    def get_workload(self):
        return {'spec': {'replicas': 1}}

    def export_config(self, run_id, workload):
        self.calls.append(('config', run_id))
        if self.failure == 'config':
            raise RuntimeError('copy failed')
        return 'runs/1/config.json'

    def backup_health(self):
        return {'healthy': True}

    def put_json(self, key, value):
        self.calls.append(('receipt', key))
        if self.failure == 'receipt':
            raise RuntimeError('copy failed')

    def publish_health(self, healthy):
        self.calls.append(('health', healthy))


class BackupTests(unittest.TestCase):
    def test_scheduled_work_never_changes_application_replicas(self):
        runtime = Runtime()
        result = worker.backup(runtime, 'run-1')
        self.assertFalse(any(call[0] == 'scale' for call in runtime.calls))
        self.assertEqual(result, {'configuration': 'runs/1/config.json', 'healthy': True})
        self.assertEqual(runtime.calls, [('config', 'run-1'), ('receipt', 'runs/1/receipt.json'), ('health', True)])

    def test_failed_export_or_receipt_never_publishes_success(self):
        for failure in ('config', 'receipt'):
            runtime = Runtime(failure)
            with self.assertRaisesRegex(RuntimeError, 'copy failed'):
                worker.backup(runtime, 'run-1')
            self.assertNotIn(('health', True), runtime.calls)

    def test_backup_health_requires_recent_completed_data(self):
        now = dt.datetime.now(dt.timezone.utc)
        primary = {'State': 'COMPLETED', 'CreationDate': now - dt.timedelta(hours=25), 'BackupJobId': 'one'}
        self.assertTrue(worker.assess_jobs([primary], now)['healthy'])
        for state in ('RUNNING', 'FAILED', 'ABORTED', 'EXPIRED'):
            self.assertFalse(worker.assess_jobs([{**primary, 'State': state}], now)['healthy'])
        self.assertFalse(worker.assess_jobs([], now)['healthy'])
        self.assertFalse(worker.assess_jobs([{**primary, 'CreationDate': now - dt.timedelta(hours=31)}], now)['healthy'])

    def test_recent_failure_is_not_hidden_by_a_later_success(self):
        now = dt.datetime.now(dt.timezone.utc)
        success = {'State': 'COMPLETED', 'CreationDate': now}
        failed = {'State': 'FAILED', 'CreationDate': now, 'BackupJobId': 'failed-1'}
        health = worker.assess_jobs([success, failed], now)
        self.assertFalse(health['healthy'])
        self.assertEqual(health['recent_failures'], [{'job': 'failed-1', 'state': 'FAILED'}])

    def test_every_protected_resource_needs_its_own_recent_backup(self):
        now = dt.datetime.now(dt.timezone.utc)
        fresh = [{'State': 'COMPLETED', 'CreationDate': now - dt.timedelta(hours=2), 'BackupJobId': 'fresh'}]
        volume, database = 'arn:aws:ec2:us-east-1:1:volume/vol-1', 'arn:aws:rds:us-east-1:1:db:notes'
        health = worker.assess_resources({volume: fresh, database: fresh}, now)
        self.assertTrue(health['healthy'])
        health = worker.assess_resources({volume: fresh, database: []}, now)
        self.assertFalse(health['healthy'])
        self.assertEqual(health['stale'], [database])
        failed = [{**fresh[0], 'State': 'FAILED', 'BackupJobId': 'bucket-1'}]
        health = worker.assess_resources({volume: fresh, database: fresh + failed}, now)
        self.assertEqual(health['recent_failures'], [{'job': 'bucket-1', 'state': 'FAILED', 'resource': database}])

    def exporter(self, volume='vol-1', reclaim='Retain'):
        runtime = object.__new__(worker.AWSRuntime)
        runtime.namespace = 'notes'
        runtime.core, runtime.claim, runtime.volume, runtime.cluster = '/api/v1/namespaces/notes', 'data', 'vol-1', 'test'
        calls, saved = [], []
        def kube(method, path):
            calls.append((method, path))
            if '/persistentvolumeclaims/' in path:
                return {'spec': {'volumeName': 'pv-1'}}
            if '/persistentvolumes/' in path:
                return {'spec': {'csi': {'volumeHandle': volume}, 'persistentVolumeReclaimPolicy': reclaim}}
            return {'kind': 'ProtectedReference', 'path': path}
        runtime.kube = kube
        runtime.put_json = lambda key, value: saved.append((key, value))
        return runtime, calls, saved

    def test_configuration_export_rejects_uncovered_or_wrong_volume(self):
        for volumes, volume, reclaim in [
            ([{'persistentVolumeClaim': {'claimName': 'another-disk'}}], 'vol-1', 'Retain'),
            ([{'projected': {}}], 'vol-1', 'Retain'),
            ([{'projected': {'sources': [{'clusterTrustBundle': {'name': 'unknown'}}]}}], 'vol-1', 'Retain'),
            ([{'hostPath': {'path': '/data'}}], 'vol-1', 'Retain'),
            ([], 'vol-other', 'Retain'), ([], 'vol-1', 'Delete')]:
            runtime, _, saved = self.exporter(volume, reclaim)
            with self.assertRaises(RuntimeError):
                runtime.export_config('run-1', {'spec': {'template': {'spec': {'volumes': volumes}}}})
            self.assertEqual(saved, [])

    @patch.dict('os.environ', {'SERVICES': 'notes', 'INGRESSES': 'notes'}, clear=False)
    def test_export_includes_referenced_configuration_and_secrets(self):
        runtime, calls, saved = self.exporter()
        spec = {'volumes': [{'secret': {'secretName': 'volume-secret'}}, {'configMap': {'name': 'settings'}}],
                'imagePullSecrets': [{'name': 'registry'}],
                'containers': [{'envFrom': [{'secretRef': {'name': 'env-secret'}}],
                                'env': [{'valueFrom': {'configMapKeyRef': {'name': 'env-settings'}}}]}],
                'initContainers': [{'env': [{'valueFrom': {'secretKeyRef': {'name': 'init-secret'}}}]}]}
        runtime.export_config('run-1', {'spec': {'template': {'spec': spec}}})
        paths = [path for _, path in calls]
        for name in ('volume-secret', 'registry', 'env-secret', 'init-secret', 'settings', 'env-settings'):
            self.assertTrue(any(path.endswith('/' + name) for path in paths), name)
        self.assertTrue(any('/services/' in path for path in paths))
        self.assertTrue(any('/ingresses/' in path for path in paths))
        self.assertEqual(len(saved), 1)
        self.assertTrue(all(method == 'GET' for method, _ in calls))

    @patch.dict('os.environ', {'DEPLOYMENTS': 'notes-gateway'}, clear=False)
    def test_export_includes_gateway_deployments_and_their_references(self):
        runtime, calls, saved = self.exporter()
        gateway = {'kind': 'Deployment', 'spec': {'template': {'spec': {
            'volumes': [{'secret': {'secretName': 'database-url'}}, {'emptyDir': {}}],
            'initContainers': [{'env': [{'valueFrom': {'secretKeyRef': {'name': 'recovery-key'}}}]}]}}}}
        get = runtime.kube
        runtime.kube = lambda method, path: gateway if path.endswith('/deployments/notes-gateway') else get(method, path)
        runtime.export_config('run-1', {'spec': {'template': {'spec': {}}}})
        paths = [path for _, path in calls]
        self.assertIn(runtime.core + '/secrets/database-url', paths)
        self.assertIn(runtime.core + '/secrets/recovery-key', paths)
        self.assertIn(gateway, saved[0][1]['objects'])

    @patch.dict('os.environ', {'DEPLOYMENTS': 'notes-gateway'}, clear=False)
    def test_a_gateway_with_its_own_disk_is_not_covered(self):
        runtime, _, saved = self.exporter()
        gateway = {'spec': {'template': {'spec': {'volumes': [{'persistentVolumeClaim': {'claimName': 'gateway-disk'}}]}}}}
        get = runtime.kube
        runtime.kube = lambda method, path: gateway if path.endswith('/deployments/notes-gateway') else get(method, path)
        with self.assertRaises(RuntimeError):
            runtime.export_config('run-1', {'spec': {'template': {'spec': {}}}})
        self.assertEqual(saved, [])

    def test_export_covers_projected_references_without_reading_runtime_tokens(self):
        runtime, calls, saved = self.exporter()
        sources = [{'serviceAccountToken': {'path': 'token', 'expirationSeconds': 3600}},
                   {'configMap': {'name': 'kube-root-ca.crt', 'items': [{'key': 'ca.crt', 'path': 'ca.crt'}]}},
                   {'secret': {'name': 'projected-secret'}},
                   {'downwardAPI': {'items': [{'path': 'namespace', 'fieldRef': {'fieldPath': 'metadata.namespace'}}]}}]
        workload = {'spec': {'template': {'spec': {'volumes': [{'projected': {'sources': sources}}]}}}}
        runtime.export_config('run-1', workload)
        self.assertIn(('GET', runtime.core + '/configmaps/kube-root-ca.crt'), calls)
        self.assertIn(('GET', runtime.core + '/secrets/projected-secret'), calls)
        self.assertFalse(any('serviceaccounts' in path or path.endswith('/token') for _, path in calls))
        self.assertEqual(saved[0][1]['objects'][0], workload)

    def test_missing_secret_access_does_not_write_partial_export(self):
        runtime, _, saved = self.exporter()
        get = runtime.kube
        def deny_secret(method, path):
            if '/secrets/' in path:
                raise RuntimeError('HTTP 403')
            return get(method, path)
        runtime.kube = deny_secret
        with self.assertRaisesRegex(RuntimeError, '403'):
            runtime.export_config('run-1', {'spec': {'template': {'spec': {'imagePullSecrets': [{'name': 'denied'}]}}}})
        self.assertEqual(saved, [])

    def test_worker_http_boundary_rejects_mutation(self):
        runtime = object.__new__(worker.AWSRuntime)
        with self.assertRaisesRegex(ValueError, 'only reads'):
            runtime.kube('PATCH', '/anything')


class TemplateTests(unittest.TestCase):
    def setUp(self):
        self.config = json.loads(Path(__file__).with_name('durability.example.json').read_text())
        self.resources = templates.primary_template(self.config)['Resources']

    def test_daily_backup_keeps_fourteen_days_without_regional_copies(self):
        rules = self.resources['HourlyPlan']['Properties']['BackupPlan']['BackupPlanRule']
        self.assertEqual(len(rules), 1)
        self.assertEqual(rules[0]['ScheduleExpression'], 'cron(0 9 * * ? *)')
        self.assertEqual(rules[0]['Lifecycle']['DeleteAfterDays'], 14)
        self.assertNotIn('CopyActions', rules[0])
        self.assertFalse(any(resource['Type'] == 'AWS::StepFunctions::StateMachine' for resource in self.resources.values()))

    def test_worker_permissions_are_read_only_and_alarms_have_no_recipients(self):
        for item in templates.rbac(self.config)['items']:
            for rule in item.get('rules', []):
                self.assertEqual(rule['verbs'], ['get'])
        statements = self.resources['WorkerRole']['Properties']['Policies'][0]['PolicyDocument']['Statement']
        actions = [action for statement in statements for action in
                   (statement['Action'] if isinstance(statement['Action'], list) else [statement['Action']])]
        self.assertFalse(any(action in actions for action in ('iam:PassRole', 'backup:StartBackupJob', 'backup:StopBackupJob')))
        for name in ('FailureAlarm', 'FreshnessAlarm'):
            self.assertEqual(self.resources[name]['Properties']['AlarmActions'], [])

    def test_the_new_stack_backs_up_its_database_and_blob_bucket_with_the_volume(self):
        selection = self.resources['Selection']['Properties']['BackupSelection']['Resources']
        self.assertEqual(selection, ['arn:aws:ec2:us-east-1:123456789012:volume/vol-REPLACE'])
        config = {**self.config, 'database_instance': 'forgetmenote-db', 'blob_bucket': 'forgetmenote-blobs',
                  'deployments': ['forgetmenote-gateway']}
        resources = templates.primary_template(config)['Resources']
        protected = ['arn:aws:ec2:us-east-1:123456789012:volume/vol-REPLACE',
                     'arn:aws:rds:us-east-1:123456789012:db:forgetmenote-db', 'arn:aws:s3:::forgetmenote-blobs']
        self.assertEqual(resources['Selection']['Properties']['BackupSelection']['Resources'], protected)
        self.assertEqual(resources['Worker']['Properties']['Environment']['Variables']['RESOURCES'], ' '.join(protected))
        self.assertIn('arn:aws:iam::aws:policy/AWSBackupServiceRolePolicyForS3Backup',
                      resources['BackupRole']['Properties']['ManagedPolicyArns'])
        self.assertIn('arn:aws:iam::aws:policy/AWSBackupServiceRolePolicyForS3Restore',
                      resources['RestoreRole']['Properties']['ManagedPolicyArns'])
        rules = [rule for item in templates.rbac(config)['items'] for rule in item.get('rules', [])]
        self.assertIn({'apiGroups': ['apps'], 'resources': ['deployments'], 'verbs': ['get'],
                       'resourceNames': ['forgetmenote-gateway']}, rules)

    def worker_for(self, config):
        """The worker as the rendered stack configures it, with AWS faked."""
        resources = templates.primary_template(config)['Resources']
        variables = resources['Worker']['Properties']['Environment']['Variables']
        environment = {key: value if isinstance(value, str) else 'from-stack' for key, value in variables.items()}
        published = []
        class Client:
            def describe_cluster(self, name):
                return {'cluster': {'endpoint': 'https://cluster.test', 'certificateAuthority': {'data': ''}}}
            def put_metric_data(self, **request):
                published.append(request)
        class Session:
            def client(self, name):
                return Client()
        boto3 = type('boto3', (), {'Session': Session})
        with patch.dict('os.environ', {**environment, 'AWS_REGION': config['region']}), \
                patch.dict('sys.modules', {'boto3': boto3}), \
                patch.object(worker.ssl, 'create_default_context', lambda **options: None):
            runtime = worker.AWSRuntime()
        runtime.publish_health(True)
        alarm = resources['FreshnessAlarm']['Properties']
        return runtime, published[0]['MetricData'][0], alarm

    def test_a_stack_exports_its_named_workload_and_keeps_its_health_alarm(self):
        # The Postgres stack keeps the file-based stack's name, which names
        # its locked vault and key, but exports the owner's StatefulSet.
        config = {**self.config, 'workload': 'forgetmenote-owner'}
        runtime, metric, alarm = self.worker_for(config)
        self.assertTrue(runtime.api.endswith('/statefulsets/forgetmenote-owner'))
        self.assertEqual((metric['MetricName'], metric['Dimensions']), (alarm['MetricName'], alarm['Dimensions']))
        self.assertEqual(alarm['Dimensions'], [{'Name': 'Instance', 'Value': 'forgetmenote'}])
        resources = templates.primary_template(config)['Resources']
        self.assertEqual(resources['Vault']['Properties']['BackupVaultName'], 'forgetmenote')
        rules = [rule for item in templates.rbac(config)['items'] for rule in item.get('rules', [])]
        self.assertIn({'apiGroups': ['apps'], 'resources': ['statefulsets'], 'verbs': ['get'],
                       'resourceNames': ['forgetmenote-owner']}, rules)

    def test_the_workload_defaults_to_the_stack_name(self):
        runtime, metric, alarm = self.worker_for(self.config)
        self.assertTrue(runtime.api.endswith('/statefulsets/forgetmenote'))
        self.assertEqual(metric['Dimensions'], alarm['Dimensions'])
        rules = [rule for item in templates.rbac(self.config)['items'] for rule in item.get('rules', [])]
        self.assertIn({'apiGroups': ['apps'], 'resources': ['statefulsets'], 'verbs': ['get'],
                       'resourceNames': ['forgetmenote']}, rules)


if __name__ == '__main__':
    unittest.main()
