"""Exercise the gateway rollout's checks, order and rollback through its cloud boundary."""
import datetime as dt
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import roll_gateways


NOW = dt.datetime(2026, 11, 4, tzinfo=dt.timezone.utc)
OLD = 'ghcr.io/example/notes@sha256:' + 'a' * 64
NEW = 'ghcr.io/example/notes@sha256:' + 'b' * 64
CANDIDATE = {'reference': NEW, 'image': 'ghcr.io/example/notes:main-' + 'c' * 40, 'source_revision': 'c' * 40,
             'registry_digest': 'sha256:' + 'b' * 64}


def pod(image, ready=True, restarts=0, terminating=False):
    return {'metadata': {'name': 'notes-gateway-x', **({'deletionTimestamp': 'now'} if terminating else {})},
            'spec': {'containers': [{'name': 'app', 'image': image}]},
            'status': {'containerStatuses': [{'ready': ready, 'restartCount': restarts}]}}


class Cloud:
    def __init__(self, directory):
        self.directory = Path(directory)
        self.calls = []

    def now(self):
        return NOW

    def save(self, name, value):
        (self.directory / name).write_text(json.dumps(value))

    def protect(self, name, value):
        self.calls.append('protect:' + name)

    def update_image(self, image):
        self.calls.append('update:' + image)

    def wait_rolled(self, image, containers):
        self.calls.append('rolled:' + image)

    def restore_point(self):
        return {'latest_restorable_time': 'later'}


class RolloutTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.cloud = Cloud(self.directory.name)
        self.receipt = {'id': 'gateways-1', 'image': NEW, 'previous_image': OLD, 'containers': ['app'], 'phase': 'prepared'}

    def tearDown(self):
        self.directory.cleanup()

    def test_the_update_waits_for_every_replica_before_reporting_ready(self):
        roll_gateways.rollout(self.cloud, self.receipt)
        self.assertEqual(self.cloud.calls, ['update:' + NEW, 'rolled:' + NEW, 'protect:rollout-result.json'])
        self.assertEqual(self.receipt['phase'], 'ready')

    def test_resuming_an_update_only_waits(self):
        self.receipt['phase'] = 'updating'
        roll_gateways.rollout(self.cloud, self.receipt)
        self.assertEqual(self.cloud.calls, ['rolled:' + NEW, 'protect:rollout-result.json'])

    def test_rollback_returns_to_the_previous_image(self):
        roll_gateways.rollback(self.cloud, self.receipt)
        self.assertEqual(self.cloud.calls, ['update:' + OLD, 'rolled:' + OLD, 'protect:rollout-rollback.json'])


BASE = {'name': 'forgetmenote', 'namespace': 'duploservices-notes', 'claim': 'data', 'volume': 'vol-1', 'region': 'us-east-1',
        'account': '123456789012', 'vault': 'v', 'backup_role': 'r', 'bucket': 'b', 'kms_key': 'k', 'export_function': 'f',
        'aws_profile': 'p', 'kubeconfig': '/k', 'duplo_host': 'https://duplo.example', 'duplo_tenant': 't',
        'public_url': 'https://notes.example.com', 'owner_entry': 'pool', 'database_instance': 'notes', 'blob_bucket': 'blobs',
        'services': {'helper': {'name': 'notes-cutover'}, 'owner': {'name': 'notes-owner'},
                     'gateway': {'name': 'notes-gateway', 'replicas': 2}}}
# Duplo returns Kubernetes fields capitalized; the refusal cases below use lowercase.
MIGRATING = json.dumps({'Command': ['/bin/sh', '-c', 'forgetmenote migrate && exec forgetmenote'],
                        'Env': [{'Name': 'PUBLIC_URL', 'Value': BASE['public_url']}]})


class RuntimeTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.cloud = roll_gateways.Runtime(BASE, Path(self.directory.name), timeout=1)

    def tearDown(self):
        self.directory.cleanup()

    def test_preflight_reports_problems_without_touching_the_recovery_bucket(self):
        with patch.object(self.cloud, 'protect') as protect, patch.object(self.cloud, 'protect_image') as image:
            self.prepare(preflight=True)
            with self.assertRaisesRegex(ValueError, 'static replicas'):
                self.prepare(replicas=3, preflight=True)
        protect.assert_not_called()
        image.assert_not_called()
        self.assertFalse(list(Path(self.directory.name).glob('rollout.json')))

    def test_preflight_refuses_an_image_the_registry_no_longer_has(self):
        with self.assertRaisesRegex(RuntimeError, 'no longer matches'):
            self.prepare(preflight=True, digest='sha256:' + 'f' * 64)

    def prepare(self, docker=MIGRATING, image=OLD, replicas=2, pods=None, account='123456789012', preflight=False, digest=None):
        service = {'Replicas': replicas, 'Template': {'Containers': [{'Image': image}], 'OtherDockerConfig': docker}}
        pods = pods if pods is not None else [pod(OLD), pod(OLD)]

        def command(args, **kwargs):
            if 'get-caller-identity' in args:
                return {'Account': account}
            if 'find' in args:
                return service
            if 'pods' in args:
                return {'items': pods}
            if 'describe-db-instances' in args:
                return {'DBInstances': [{'DBInstanceIdentifier': 'notes', 'DBInstanceStatus': 'available',
                                         'BackupRetentionPeriod': 7, 'LatestRestorableTime': 'before'}]}
            return {}
        receipt = {'id': 'gateways-1'}
        self.cloud.run_id = 'gateways-1'
        with patch.object(self.cloud, 'command', side_effect=command), \
                patch.object(self.cloud, 'protect') as protect, \
                patch.object(self.cloud, 'protect_image', return_value={'key': 'images/x'}), \
                patch('deploy_instance.locate_package', return_value=('p', digest or CANDIDATE['registry_digest'])):
            if preflight:
                self.cloud.check(CANDIDATE)
            else:
                self.cloud.prepare(receipt, CANDIDATE)
        return receipt, [call.args[0] for call in protect.call_args_list]

    def test_a_ready_service_records_its_restore_point_and_previous_image(self):
        receipt, protected = self.prepare()
        self.assertEqual(receipt['previous_image'], OLD)
        self.assertEqual(receipt['containers'], ['app'])
        self.assertEqual(receipt['database_before']['latest_restorable_time'], 'before')
        self.assertEqual(protected, ['service-before.json', 'prepared.json'])

    def test_each_unready_part_is_named_before_anything_changes(self):
        cases = [
            ({'docker': json.dumps({'Env': [{'name': 'PUBLIC_URL', 'value': BASE['public_url']}]})}, 'forgetmenote migrate'),
            ({'docker': json.dumps({'Command': ['forgetmenote migrate'], 'Env': [{'name': 'PUBLIC_URL', 'value': 'https://other.example'}]})},
             'public origin'),
            ({'docker': json.dumps({'Command': ['forgetmenote migrate'], 'Env': [{'name': 'PUBLIC_URL', 'value': BASE['public_url']},
                                                                               {'name': 'OUTGOING', 'value': 'paused'}]})}, 'pause'),
            ({'image': NEW}, 'already run the candidate'),
            ({'replicas': 3}, 'static replicas'),
            ({'pods': [pod(OLD), pod(OLD, ready=False)]}, 'ready gateways'),
            ({'account': '999'}, 'wrong account'),
        ]
        for changes, expected in cases:
            with self.subTest(expected=expected), self.assertRaisesRegex(ValueError, expected):
                self.prepare(**changes)

    def test_gateways_that_reach_an_owner_never_roll_onto_storage_v2(self):
        owners = json.dumps({'Command': ['forgetmenote migrate'], 'Env': [{'name': 'PUBLIC_URL', 'value': BASE['public_url']},
                                                                        {'name': 'OWNER_MAP_FILE', 'value': '/run/config/owners.json'}]})
        with patch('deploy_instance.storage_v2', return_value=True), self.assertRaisesRegex(ValueError, 'run the storage cutover'):
            self.prepare(docker=owners)
        with patch('deploy_instance.storage_v2', return_value=False):
            self.assertEqual(self.prepare(docker=owners)[0]['previous_image'], OLD)

    def test_a_rollout_is_done_only_when_every_replica_serves_the_image_and_the_origin_answers(self):
        cases = [([pod(NEW), pod(OLD)], True), ([pod(NEW)], True), ([pod(NEW), pod(NEW, restarts=1)], True),
                 ([pod(NEW), pod(NEW, terminating=True)], True), ([pod(NEW), pod(NEW)], False)]
        for pods, healthy in cases:
            with self.subTest(pods=len(pods), healthy=healthy), patch('deploy_instance.time.sleep'), \
                    patch.object(self.cloud, 'role_pods', return_value=pods), \
                    patch.object(self.cloud, 'answers', return_value=healthy), \
                    self.assertRaisesRegex(RuntimeError, 'did not all serve'):
                self.cloud.wait_rolled(NEW, ['app'])
        with patch.object(self.cloud, 'role_pods', return_value=[pod(NEW), pod(NEW)]), \
                patch.object(self.cloud, 'answers', return_value=True):
            self.cloud.wait_rolled(NEW, ['app'])


if __name__ == '__main__':
    unittest.main()
