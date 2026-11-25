#!/usr/bin/env python3
"""Deploy one verified image through Duplo, with a quiesced AWS Backup checkpoint."""
import argparse
import copy
import datetime as dt
import gzip
import hashlib
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid

from durability_worker import assess_jobs
from release_image import ECR, GHCR, locate_package, runtime_changes
from verification import fingerprint, release_lock


UTC = dt.timezone.utc
TERMINAL_FAILURES = ('FAILED', 'ABORTED', 'EXPIRED', 'PARTIAL')
ACTIVE_JOBS = ('CREATED', 'PENDING', 'RUNNING', 'ABORTING')
# The vault lock fails jobs retained for less than its minimum.
WARMUP_DAYS = 7
# Only a storage v2 release, which keeps notebooks in the database, has the storage cutover commands.
STORAGE_V2 = 'cmd/forgetmenote/storage_admin.go'
STORAGE_CUTOVER = ('keeps notebooks in the database (storage v2), which no image update can move them into: '
                   'run the storage cutover instead (deploy/production/storage-cutover.md)')


def field_key(item, name):
    """The key item holds a Kubernetes field under. `service find` returns them
    capitalized (Name, Value, HttpGet); Duplo accepts either casing on update."""
    capitalized = name[:1].upper() + name[1:]
    return capitalized if capitalized in item and name not in item else name


def field(item, name):
    return item.get(field_key(item, name))


def environment(settings):
    """The explicit environment entries of a parsed OtherDockerConfig."""
    return [item for key in ('Env', 'env') for item in settings.get(key, [])]


def storage_v2(root, revision):
    """Whether the release built from revision is storage v2, or None when the checkout at root lacks it."""
    git = ['git', '-C', str(root), 'cat-file', '-e']
    if subprocess.run(git + [revision + '^{commit}'], capture_output=True).returncode:
        return None
    return subprocess.run(git + [revision + ':' + STORAGE_V2], capture_output=True).returncode == 0


def previous_storage_v2(directory):
    """True when the deployment recorded in directory ran storage v2, through the storage cutover or a later
    deployment, or None when its record does not say so. A replaced repository history no longer holds that
    release's commit, so its record is the only evidence left."""
    try:
        record = json.loads((Path(directory) / 'deployment.json').read_text())
    except (OSError, ValueError):
        return None
    return True if isinstance(record, dict) and (record.get('storage_cutover') is True or record.get('storage_v2') is True) else None


def check_storage_generation(root, previous, candidate, previous_v2=None):
    """Refuse an image update from a release that keeps notebooks in Git repositories to one that keeps them in
    the database. previous_v2 is what the previous release's deployment record says (previous_storage_v2); without
    it, a previous revision this history lacks comes from a repository it replaced, before storage v2."""
    v2 = storage_v2(root, candidate)
    if v2 is None:
        raise ValueError('This checkout lacks release revision ' + candidate + '; fetch it first')
    if v2 and not (previous_v2 or storage_v2(root, previous)):
        raise ValueError('Release ' + candidate[:12] + ' ' + STORAGE_CUTOVER)


def timestamp(value):
    result = dt.datetime.fromisoformat(value.replace('Z', '+00:00')) if isinstance(value, str) else value
    if result.tzinfo is None:
        raise ValueError('Backup timestamps must include a time zone')
    return result


def volume_arn(config):
    return f"arn:aws:ec2:{config['region']}:{config['account']}:volume/{config['volume']}"


def release_record(directory):
    directory = Path(directory)
    publication = json.loads((directory / 'publication.json').read_text())
    verification = json.loads((directory / 'verification.json').read_text())
    revision = publication['source_revision']
    checks = verification.get('expected_checks', [])
    if (not re.fullmatch('[0-9a-f]{40}', revision) or not verification.get('complete') or not checks or
            verification['source_revision'] != revision or verification['image_id'] != publication['image_id'] or
            any(verification['checks'].get(name, {}).get('status') != 'passed' or
                verification['checks'].get(name, {}).get('exit_code') != 0 for name in checks)):
        raise ValueError('Release must have a complete verification receipt for its exact source and image')
    repository, _, tag = publication['image'].rpartition(':')
    if (tag != 'main-' + revision or not (GHCR.fullmatch(repository) or ECR.fullmatch(repository)) or
            not re.fullmatch('sha256:[0-9a-f]{64}', publication['image_id']) or
            not re.fullmatch('sha256:[0-9a-f]{64}', publication['registry_digest'])):
        raise ValueError('Release must reference a pinned private GHCR or ECR image')
    return {**publication, 'reference': publication['image'].rsplit(':', 1)[0] + '@' + publication['registry_digest']}


def normalized_template(service):
    template = copy.deepcopy(service['Template'])
    # Duplo regenerates BaseVersion on Stop/Start as well as image updates.
    template.pop('BaseVersion', None)
    template.pop('ImageUpdateTime', None)
    # Service updates omit these optional string fields when they are empty.
    for name in ('AllocationTags', 'OtherDockerHostConfig', 'Volumes'):
        if template.get(name) == '':
            template.pop(name)
    # Duplo may fill these output fields while stopping an unchanged service.
    for load_balancer in (template.get('LBConfigurations') or {}).values():
        load_balancer.pop('DnsName', None)
        load_balancer.pop('CloudName', None)
    template['OtherDockerConfig'] = json.loads(template.get('OtherDockerConfig') or '{}')
    return template


def image_update(service, previous, image):
    desired = copy.deepcopy(service)
    containers = desired['Template']['Containers']
    if len(containers) != 1 or containers[0]['Image'] not in (previous, image):
        raise ValueError('Service no longer matches the expected application image')
    containers[0]['Image'] = image
    settings = json.loads(desired['Template'].get('OtherDockerConfig') or '{}')
    for group, image_key in (('additionalContainers', 'image'), ('initContainers', 'image'),
                             ('AdditionalContainers', 'Image'), ('InitContainers', 'Image')):
        for container in settings.get(group, []):
            if container.get(image_key) == previous:
                container[image_key] = image
    desired['Template']['OtherDockerConfig'] = json.dumps(settings)
    return desired


def provider_secret_mounts(service, secret_name, keys):
    """Add reviewed keys to the existing provider mount during stopped update."""
    if not keys:
        return service
    if (not isinstance(keys, list) or len(set(keys)) != len(keys) or
            any(not isinstance(key, str) or not re.fullmatch(r'[A-Z][A-Z0-9_]{0,127}', key) for key in keys)):
        raise ValueError('Provider secret keys must be distinct uppercase key names')
    desired = copy.deepcopy(service)
    settings = json.loads(desired['Template'].get('OtherDockerConfig') or '{}')
    volumes = [(v, 'name' in v) for v in settings.get('Volumes', [])
               if v.get('name') == 'providers' or v.get('Name') == 'providers']
    if len(volumes) != 1:
        raise ValueError('Provider mount must reference the existing instance secret')
    volume, lower = volumes[0]
    secret_key, name_key, items_key, key_key, path_key = (
        ('secret', 'secretName', 'items', 'key', 'path') if lower else
        ('Secret', 'SecretName', 'Items', 'Key', 'Path'))
    if volume.get(secret_key, {}).get(name_key) != secret_name:
        raise ValueError('Provider mount must reference the existing instance secret')
    items = volume[secret_key].get(items_key)
    if not isinstance(items, list):
        raise ValueError('Provider mount must have an explicit key inventory')
    for key in keys:
        existing = [v for v in items if v.get(key_key) == key or v.get(path_key) == key]
        if existing and existing != [{key_key: key, path_key: key}]:
            raise ValueError('Provider secret key conflicts with an existing mount path')
        if not existing:
            items.append({key_key: key, path_key: key})
    desired['Template']['OtherDockerConfig'] = json.dumps(settings)
    return desired


def origin_update(service, previous, origin):
    """Change the application's origin and its own probes, never secret values."""
    desired = copy.deepcopy(service)
    settings = json.loads(desired['Template'].get('OtherDockerConfig') or '{}')
    values = [item for item in environment(settings) if field(item, 'name') == 'PUBLIC_URL']
    if len(values) != 1 or field(values[0], 'value') != previous or field(values[0], 'valueFrom') is not None:
        raise ValueError('PUBLIC_URL must be one explicit value matching the previous operator origin')
    values[0][field_key(values[0], 'value')] = origin
    old_host, new_host = urllib.parse.urlsplit(previous).netloc, urllib.parse.urlsplit(origin).netloc
    for key in ('StartupProbe', 'ReadinessProbe', 'LivenessProbe', 'startupProbe', 'readinessProbe', 'livenessProbe'):
        for header in field(field(settings.get(key, {}), 'httpGet') or {}, 'httpHeaders') or []:
            if (field(header, 'name') or '').lower() == 'host' and field(header, 'value') == old_host:
                header[field_key(header, 'value')] = new_host
    desired['Template']['OtherDockerConfig'] = json.dumps(settings)
    return desired


def source_budget_update(service, amount):
    """Set the existing source import limit without replacing other settings."""
    if amount is None:
        return service
    if type(amount) is not int or not 1 <= amount <= sys.maxsize // 64:
        raise ValueError('Source bytes must be a positive bounded integer')
    desired = copy.deepcopy(service)
    settings = json.loads(desired['Template'].get('OtherDockerConfig') or '{}')
    if 'Env' in settings and 'env' in settings:
        raise ValueError('Source bytes requires one environment list')
    key = 'env' if 'env' in settings else 'Env'
    values = settings.setdefault(key, [])
    matches = [item for item in values if field(item, 'name') == 'FMN_LIMIT_SOURCE_BYTES']
    if len(matches) > 1 or any(field(item, 'valueFrom') is not None for item in matches):
        raise ValueError('Source bytes must be one explicit environment value')
    if matches:
        matches[0][field_key(matches[0], 'value')] = str(amount)
    else:
        values.append({'name': 'FMN_LIMIT_SOURCE_BYTES', 'value': str(amount)})
    desired['Template']['OtherDockerConfig'] = json.dumps(settings)
    return desired


def validate_job(config, job, job_id):
    if (job.get('BackupJobId') != job_id or job.get('ResourceArn') != volume_arn(config) or
            job.get('BackupVaultName') != config['vault']):
        raise RuntimeError('Backup job does not belong to this volume and vault')


def release_warmup(job):
    """Only release warm-ups are on-demand jobs kept for the vault's minimum retention."""
    return not job.get('CreatedBy') and (job.get('RecoveryPointLifecycle') or {}).get('DeleteAfterDays') == WARMUP_DAYS


def without_accepted_failures(jobs, accepted):
    """Drop failures an operator reviewed and named, each only once a later backup completed."""
    for name in accepted:
        failed = next((job for job in jobs if job['BackupJobId'] == name and job['State'] in TERMINAL_FAILURES), None)
        if failed is None:
            raise RuntimeError('Accepted backup failure ' + name + ' is not a recent failed job for this volume')
        if not any(job['State'] == 'COMPLETED' and job['CreationDate'] > failed['CreationDate'] for job in jobs):
            raise RuntimeError('Accepted backup failure ' + name + ' has no later completed backup')
    return [job for job in jobs if job['BackupJobId'] not in accepted]


def warm_up(cloud, receipt):
    """Snapshot the running volume first, so the stopped snapshot copies only later changes.

    The warm-up is crash-consistent and never a restore point. If it cannot be
    requested or fails, the stopped sequence runs exactly as without it. AWS
    Backup runs one job per resource, so the service is not stopped while the
    warm-up job may still be active: a stopped backup started then would fail.
    """
    warmup = receipt['warmup'] = {'requested_at': cloud.now().isoformat()}
    cloud.save('deployment.json', receipt)
    try:
        warmup['job'] = cloud.start_warmup(receipt['id'])
    except Exception as error:
        warmup['error'] = str(error)
        cloud.save('deployment.json', receipt)
        return
    cloud.save('deployment.json', receipt)
    try:
        warmup['state'] = cloud.wait_warmup(warmup['job'])['State']
    except Exception as error:
        warmup['error'] = str(error)
        cloud.save('deployment.json', receipt)
        raise RuntimeError('Warm-up backup ' + warmup['job'] + ' has not finished; the service was not stopped. '
                           'Start a fresh deployment after the job finishes') from error
    warmup['finished_at'] = cloud.now().isoformat()
    cloud.save('deployment.json', receipt)


def refresh_backup(cloud, receipt, job=None):
    job = job or cloud.backup_job(receipt['backup_job'])
    validate_job(cloud.config, job, receipt['backup_job'])
    receipt['backup_state'] = job['State']
    cloud.save('deployment.json', receipt)
    # A terminal failure remains failure evidence even when the service is healthy.
    if job['State'] in TERMINAL_FAILURES:
        cloud.protect('backup-result.json', receipt)
        raise RuntimeError('Upgrade backup ' + receipt['backup_job'] + ' is ' + job['State'])
    if job['State'] == 'COMPLETED' and not receipt.get('backup_completed_at'):
        if not receipt.get('snapshot'):
            cloud.protect('backup-result.json', receipt)
            raise RuntimeError('Backup completed without confirmed stopped capture; consistency remains unverified')
        if not job.get('RecoveryPointArn', '').endswith('/' + receipt['snapshot']['id']):
            raise RuntimeError('Completed backup does not match the captured snapshot')
        cloud.tag_completed(receipt)
        completed = {**receipt, 'backup_completed_at': cloud.now().isoformat()}
        cloud.protect('backup-result.json', completed)
        receipt.update(completed)
        cloud.save('deployment.json', receipt)


def upgrade(cloud, receipt, *, resume=False):
    """One stop/capture/update/start; upload completion is a separate outcome."""
    if receipt['phase'] == 'ready':
        refresh_backup(cloud, receipt)
        return
    if resume and receipt['phase'] == 'starting':
        cloud.start()
        finish_upgrade(cloud, receipt)
        return
    if resume or receipt['phase'] != 'prepared':
        raise RuntimeError('Inspect the interrupted operation and current service image; use a fresh deployment with fresh preflight and capture')
    if receipt['compatibility'] != 'compatible':
        warm_up(cloud, receipt)
    stopped_requested, operation_error = False, None
    try:
        # Persist intent before calling Stop: its response can be lost after it succeeds.
        receipt['phase'] = 'stopping'
        cloud.save('deployment.json', receipt)
        stopped_requested = True
        cloud.stop()
        cloud.wait_stopped()
        receipt['quiesced_at'] = cloud.now().isoformat()
        cloud.save('deployment.json', receipt)
        receipt['backup_job'] = cloud.start_backup(receipt['id'])
        cloud.save('deployment.json', receipt)
        receipt['snapshot'] = cloud.observe_snapshot(receipt)
        cloud.protect('capture.json', receipt)
        cloud.save('deployment.json', receipt)
        if receipt['compatibility'] != 'compatible':
            refresh_backup(cloud, receipt, cloud.wait_backup(receipt))
        else:
            # Catch a failure already known at capture; do not wait for transfer.
            refresh_backup(cloud, receipt)
        receipt['phase'] = 'updating'
        cloud.save('deployment.json', receipt)
        cloud.update(receipt)
        receipt['phase'] = 'starting'
        cloud.save('deployment.json', receipt)
    except BaseException as error:
        operation_error = error
        receipt['operation_error'] = str(error)
        cloud.save('deployment.json', receipt)
        raise
    finally:
        try:
            if stopped_requested:
                cloud.start()
        except BaseException as error:
            receipt['restart_error'] = str(error)
            cloud.save('deployment.json', receipt)
            if operation_error is not None:
                raise RuntimeError(str(operation_error) + '; restart also failed: ' + str(error)) from operation_error
            raise
    finish_upgrade(cloud, receipt)


def finish_upgrade(cloud, receipt):
    cloud.wait_ready(receipt)
    completed = {**receipt, 'phase': 'ready', 'ready_at': cloud.now().isoformat()}
    cloud.protect('deployment-result.json', completed)
    receipt.update(completed)
    cloud.save('deployment.json', receipt)
    refresh_backup(cloud, receipt)


class Runtime:
    """Existing CLI credentials own the AWS, Duplo and Kubernetes boundaries.

    Without aws_profile or kubeconfig the CLIs use the host's own sign-in, as on a
    runner. duploctl discards DUPLO_TOKEN under --interactive and waits for a browser,
    so that flag is only passed without a token.
    """
    def __init__(self, config, directory, timeout=900):
        self.config, self.directory, self.timeout = config, directory, timeout
        profile = ['--profile', config['aws_profile']] if 'aws_profile' in config else []
        self.aws = ['aws', *profile, '--region', config['region'], '--output', 'json', '--no-cli-pager']
        login = [] if os.environ.get('DUPLO_TOKEN', '').strip() else ['--interactive']
        self.duplo = ['duploctl', '--host', config['duplo_host'], '--tenant', config['duplo_tenant'],
                      '--admin', *login, '--output', 'json']
        kubeconfig =['--kubeconfig', str(Path(config['kubeconfig']).expanduser())] if 'kubeconfig' in config else []
        self.kube = ['kubectl', *kubeconfig, '-n', config['namespace']]
        self.sequence = len(list(directory.glob('command-*.json')))
        # Status polls repeat identical responses; keep one full copy per process.
        self.seen = {}

    def now(self):
        return dt.datetime.now(UTC)

    def save(self, name, value):
        path = self.directory / name
        temporary = path.with_suffix('.tmp')
        temporary.write_text(json.dumps(value, indent=2) + '\n')
        temporary.chmod(0o600)
        temporary.replace(path)
        return path

    def command(self, args, *, allow_missing=False, timeout=120, parse=True):
        self.sequence += 1
        name = f'command-{self.sequence:04d}.json'
        try:
            result = subprocess.run(args, capture_output=True, text=True, timeout=timeout)
        except subprocess.TimeoutExpired as error:
            text = lambda value: value.decode(errors='replace') if isinstance(value, bytes) else (value or '')
            self.save(name, {'command': args, 'error': 'timeout',
                             'stdout': text(error.stdout), 'stderr': text(error.stderr)})
            raise RuntimeError('Operator command timed out; inspect ' + str(self.directory / name)) from None
        record = {'command': args, 'exit_code': result.returncode, 'stdout': result.stdout, 'stderr': result.stderr}
        digest = hashlib.sha256(json.dumps(record, sort_keys=True).encode()).hexdigest()
        if not result.returncode and digest in self.seen:
            self.save(name, {'same_as': self.seen[digest]})
        else:
            self.save(name, record)
            if not result.returncode:
                self.seen[digest] = name
        if result.returncode:
            if allow_missing and re.search(r'\b(?:404|Not Found|NoSuchKey|No such image)\b', result.stderr):
                return None
            raise RuntimeError('Operator command failed (' + str(result.returncode) + '); inspect ' + str(self.directory / name))
        return json.loads(result.stdout) if parse and result.stdout.strip() else {}

    def protect(self, name, value):
        return self.protect_file(name, self.save(name, value))

    def protect_file(self, name, path):
        return self.store('upgrades/' + self.run_id + '/' + name, path)

    def store(self, key, path):
        result = self.command(self.aws + ['s3api', 'put-object', '--bucket', self.config['bucket'],
            '--key', key, '--body', str(path),
            '--server-side-encryption', 'aws:kms', '--ssekms-key-id', self.config['kms_key'],
            '--object-lock-mode', 'GOVERNANCE', '--object-lock-retain-until-date', (self.now() + dt.timedelta(days=14)).isoformat()])
        return {'key': key, 'version': result['VersionId']}

    def service(self):
        return self.command(self.duplo + ['service', 'find', self.config['name']])

    def pods(self):
        return self.command(self.kube + ['get', 'pods', '--field-selector',
            'metadata.name=' + self.config['name'] + '-0', '-o', 'json'])['items']

    def volume(self):
        return self.command(self.aws + ['ec2', 'describe-volumes', '--volume-ids', self.config['volume']])['Volumes'][0]

    def wait_for(self, check, message):
        deadline = time.monotonic() + self.timeout
        while True:
            result = check()
            if result:
                return result
            if time.monotonic() >= deadline:
                raise RuntimeError((message() if callable(message) else message) + '; original command responses retained')
            time.sleep(5)

    def is_stopped(self):
        volume = self.volume()
        return not self.pods() and volume['State'] == 'available' and not volume['Attachments']

    def wait_stopped(self):
        self.wait_for(self.is_stopped, 'Service and volume did not quiesce')

    def stop(self):
        self.service_action('stop')

    def start(self):
        self.service_action('start')

    def service_action(self, action):
        result = self.command(self.duplo + ['service', action, self.config['name']])
        details = result.get('details', {})
        if details.get('errors') or self.config['name'] not in details.get('success', []):
            raise RuntimeError('Duplo ' + action + ' did not succeed; inspect the original command response')

    def start_backup(self, run_id):
        return self.backup_request(run_id, 14, 'Consistency=unverified,Purpose=release-upgrade')

    def start_warmup(self, run_id):
        # A distinct token: the upgrade's token would return this job for the stopped backup.
        return self.backup_request(run_id + '-warmup', WARMUP_DAYS, 'Consistency=crash,Purpose=release-warmup')

    def backup_request(self, token, days, tags):
        result = self.command(self.aws + ['backup', 'start-backup-job', '--backup-vault-name', self.config['vault'],
            '--resource-arn', volume_arn(self.config), '--iam-role-arn', self.config['backup_role'],
            '--lifecycle', 'DeleteAfterDays=' + str(days), '--idempotency-token', token,
            '--recovery-point-tags', tags])
        return result['BackupJobId']

    def backup_job(self, job):
        return self.command(self.aws + ['backup', 'describe-backup-job', '--backup-job-id', job])

    def wait_warmup(self, job_id):
        def finished():
            job = self.backup_job(job_id)
            validate_job(self.config, job, job_id)
            return job if job['State'] == 'COMPLETED' or job['State'] in TERMINAL_FAILURES else None
        return self.wait_for(finished, 'Warm-up backup did not finish before the deadline')

    def observe_snapshot(self, receipt):
        def observe():
            job = self.backup_job(receipt['backup_job'])
            validate_job(self.config, job, receipt['backup_job'])
            if job['State'] in TERMINAL_FAILURES:
                raise RuntimeError('Upgrade backup is ' + job['State'])
            match = re.fullmatch(r'arn:aws:ec2:' + re.escape(self.config['region']) + r':(?:[0-9]{12})?:snapshot/(snap-[a-f0-9]+)',
                                 job.get('RecoveryPointArn', ''))
            if not match:
                return None
            snapshot = self.command(self.aws + ['ec2', 'describe-snapshots', '--snapshot-ids', match[1]])['Snapshots'][0]
            if (snapshot['SnapshotId'] != match[1] or snapshot['VolumeId'] != self.config['volume'] or not snapshot['Encrypted'] or
                    timestamp(snapshot['StartTime']) < timestamp(receipt['quiesced_at'])):
                raise RuntimeError('Snapshot does not match the stopped encrypted volume')
            if snapshot['State'] == 'error':
                raise RuntimeError('Snapshot creation failed')
            if snapshot['State'] not in ('pending', 'completed'):
                return None
            if not self.is_stopped():
                raise RuntimeError('Service resumed before snapshot capture was observed')
            return {'id': match[1], 'state': snapshot['State'], 'started_at': snapshot['StartTime'],
                    'observed_at': self.now().isoformat()}
        return self.wait_for(observe, 'AWS accepted the job but no stopped snapshot point was established')

    def wait_backup(self, receipt):
        def completed():
            job = self.backup_job(receipt['backup_job'])
            validate_job(self.config, job, receipt['backup_job'])
            if job['State'] in TERMINAL_FAILURES:
                refresh_backup(self, receipt, job)
            return job if job['State'] == 'COMPLETED' else None
        return self.wait_for(completed, 'Backup did not complete before the protected upgrade deadline')

    def tag_completed(self, receipt):
        self.command(self.aws + ['ec2', 'create-tags', '--resources', receipt['snapshot']['id'],
                                '--tags', 'Key=Consistency,Value=application'])

    def check_provider_secret_keys(self):
        keys = self.config.get('provider_secret_keys', [])
        # Filter at kubectl, before command() persists output. Never request or
        # record credential values just to verify the mount inventory.
        template = '{' + ','.join(json.dumps(key) + ':{{if index .data ' + json.dumps(key) + '}}true{{else}}false{{end}}' for key in keys) + '}'
        present = self.command(self.kube + ['get', 'secret', self.config['name'], '-o', 'go-template=' + template])
        if any(present.get(key) is not True for key in keys):
            raise RuntimeError('Requested provider secret keys must exist before deployment')

    def update(self, receipt):
        if not self.is_stopped():
            raise RuntimeError('Service resumed during backup; refusing to update a running writer')
        current = self.service()
        before = json.loads((self.directory / 'service-before.json').read_text())
        expected = image_update(before, receipt['previous_image'], receipt['image'])
        reconciled = image_update(current, receipt['previous_image'], receipt['image'])
        if normalized_template(reconciled) != normalized_template(expected):
            raise RuntimeError('Service configuration changed during deployment; refusing to overwrite it')
        origin = receipt.get('public_url', self.config['public_url'])
        if origin != self.config['public_url']:
            reconciled = origin_update(reconciled, self.config['public_url'], origin)
        reconciled = source_budget_update(reconciled, receipt.get('source_bytes'))
        reconciled = provider_secret_mounts(reconciled, self.config['name'], self.config.get('provider_secret_keys', []))
        desired = self.save('service-desired.json', reconciled)
        self.command(self.duplo + ['service', 'update', self.config['name'], '-f', str(desired)])
        self.command(self.duplo + ['service', 'update_image', self.config['name'], receipt['image']])
        self.protect('service-desired.json', reconciled)

    def wait_ready(self, receipt):
        class NoRedirect(urllib.request.HTTPRedirectHandler):
            def redirect_request(self, *args, **kwargs):
                return None
        opener = urllib.request.build_opener(NoRedirect())
        failure = ['']
        def ready():
            failure[0] = 'New application and sidecars did not become ready'
            pods = self.pods()
            if len(pods) != 1:
                return False
            pod = pods[0]
            self.save('rollout-pod.json', pod)
            statuses = pod.get('status', {}).get('containerStatuses', [])
            images = {c['name']: c['image'] for c in pod['spec']['containers']}
            if not (not pod['metadata'].get('deletionTimestamp') and
                    all(images.get(name) == receipt['image'] for name in receipt['image_containers']) and
                    len(statuses) == len(images) and all(c.get('ready') and c.get('restartCount', 0) == 0 for c in statuses)):
                return False
            # The load balancer answers 503 itself until it marks the new target
            # healthy, so poll until both endpoints return 200 on one ready pod.
            for endpoint in ('healthz', 'readyz'):
                try:
                    with opener.open(receipt.get('public_url', self.config['public_url']).rstrip('/') + '/' + endpoint, timeout=30) as response:
                        status, body = response.status, response.read(65536).decode(errors='replace')
                except urllib.error.HTTPError as error:
                    with error:
                        status, body = error.code, error.read(65536).decode(errors='replace')
                self.save(endpoint + '.json', {'status': status, 'body': body})
                if status != 200:
                    failure[0] = endpoint + ' still returned HTTP ' + str(status) + ' at the readiness deadline'
                    return False
            return True
        try:
            self.wait_for(ready, lambda: failure[0])
        except BaseException:
            # Keep the primary failure even if diagnostic collection itself fails.
            diagnostics = [['get', 'events', '--field-selector', 'involvedObject.name=' + self.config['name'] + '-0', '-o', 'json'],
                           ['get', 'pods', '--field-selector', 'metadata.name=' + self.config['name'] + '-0', '-o', 'json']]
            diagnostics.extend(['logs', self.config['name'] + '-0', '-c', name, '--timestamps', '--tail=200']
                               for name in receipt['image_containers'])
            for args in diagnostics:
                try:
                    self.command(self.kube + args)
                except Exception:
                    pass
            raise

    def docker_name(self, release):
        """The name docker saves a release by, pulling it when this host never built it."""
        if self.command(['docker', 'image', 'inspect', release['image_id']], allow_missing=True) is not None:
            return release['image_id']
        self.command(['docker', 'pull', '--quiet', release['reference']], timeout=1800, parse=False)
        return release['reference']

    def protect_image(self, release, *, existing=False):
        key = 'images/' + release['registry_digest'].replace(':', '-') + '.docker.tar.gz'
        args = self.aws + ['s3api', 'head-object', '--bucket', self.config['bucket'], '--key', key]
        head = self.command(args, allow_missing=True)
        if head is None:
            if existing:
                raise RuntimeError('Previous image is not protected in the recovery bucket; archive it before deploying')
            archive = self.directory / (release['image_id'].replace(':', '-') + '.docker.tar.gz')
            if not archive.exists():
                image = self.docker_name(release)
                partial = archive.with_suffix('.partial')
                with partial.open('wb') as output, (self.directory / 'image-save.log').open('wb') as errors:
                    process = subprocess.Popen(['docker', 'image', 'save', image], stdout=subprocess.PIPE, stderr=errors)
                    try:
                        with gzip.GzipFile(fileobj=output, mode='wb', compresslevel=1, mtime=0) as compressed:
                            while data := process.stdout.read(1024 * 1024):
                                compressed.write(data)
                    except BaseException:
                        process.kill()
                        process.wait()
                        raise
                    finally:
                        process.stdout.close()
                    if process.wait():
                        raise RuntimeError('Cannot archive the checked local image; inspect image-save.log')
                partial.replace(archive)
            with archive.open('rb') as stream:
                digest = hashlib.file_digest(stream, 'sha256').hexdigest()
            self.command(self.aws + ['s3', 'cp', str(archive), 's3://' + self.config['bucket'] + '/' + key,
                '--sse', 'aws:kms', '--sse-kms-key-id', self.config['kms_key'], '--only-show-errors',
                '--metadata', 'source-revision=' + release['source_revision'] + ',sha256=' + digest], timeout=1800)
            head = self.command(args)
            if head['ContentLength'] != archive.stat().st_size or head.get('Metadata', {}).get('sha256') != digest:
                raise RuntimeError('Protected image does not match its local archive')
        if (head.get('ServerSideEncryption') != 'aws:kms' or head.get('SSEKMSKeyId') != self.config['kms_key'] or
                head.get('ObjectLockMode') != 'GOVERNANCE' or not head.get('ContentLength') or
                head.get('Metadata', {}).get('source-revision') != release['source_revision'] or
                not re.fullmatch('[a-f0-9]{64}', head.get('Metadata', {}).get('sha256', ''))):
            raise RuntimeError('Recovery image protection or source identity is missing')
        retain_until = self.now() + dt.timedelta(days=14)
        if timestamp(head['ObjectLockRetainUntilDate']) < retain_until:
            self.command(self.aws + ['s3api', 'put-object-retention', '--bucket', self.config['bucket'],
                '--key', key, '--version-id', head['VersionId'], '--retention',
                json.dumps({'Mode': 'GOVERNANCE', 'RetainUntilDate': retain_until.isoformat()})])
        return {'key': key, 'version': head['VersionId'], 'sha256': head['Metadata']['sha256'],
                'image_id': release['image_id'], 'source_revision': release['source_revision']}

    def protect_release(self, candidate_dir, root, receipt):
        archive = self.directory / 'source.tar.gz'
        # Generate from Git rather than archiving a possibly dirty working directory.
        self.command(['git', '-C', str(root), 'archive', '--format=tar.gz', '--output', str(archive), receipt['source_revision']])
        assets = {'source': self.protect_file('source.tar.gz', archive)}
        with archive.open('rb') as stream:
            assets['source']['sha256'] = hashlib.file_digest(stream, 'sha256').hexdigest()
        for name in ('publication.json', 'verification.json'):
            assets[name] = self.protect(name, json.loads((candidate_dir / name).read_text()))
        receipt['release_assets'] = assets

    def check_identity(self):
        identity = self.command(self.aws + ['sts', 'get-caller-identity'])
        if identity['Account'] != self.config['account']:
            raise RuntimeError('AWS identity is in the wrong account')

    def check_service(self, receipt, previous):
        service = self.service()
        if service['Template']['Containers'][0]['Image'] != previous['reference']:
            raise RuntimeError('Live service does not match the previous release receipt')
        if service.get('Replicas') != 1 or service.get('HPASpecs'):
            raise RuntimeError('Deployment requires one static Duplo replica')
        if self.config.get('provider_secret_keys'):
            provider_secret_mounts(service, self.config['name'], self.config['provider_secret_keys'])
            self.check_provider_secret_keys()
        if receipt.get('public_url', self.config['public_url']) != self.config['public_url']:
            # Reject an ambiguous origin before creating any outage or backup job.
            origin_update(service, self.config['public_url'], receipt['public_url'])
        source_budget_update(service, receipt.get('source_bytes'))
        self.save('service-before.json', service)
        return service

    def check_storage(self, receipt, previous):
        pods = self.pods()
        statuses = pods[0].get('status', {}).get('containerStatuses', []) if len(pods) == 1 else []
        if (len(pods) != 1 or pods[0]['metadata'].get('deletionTimestamp') or not statuses or
                len(statuses) != len(pods[0]['spec']['containers']) or not all(c.get('ready') for c in statuses)):
            raise RuntimeError('Expected one healthy existing application pod')
        receipt['image_containers'] = [c['name'] for c in pods[0]['spec']['containers'] if c['image'] == previous['reference']]
        if not receipt['image_containers']:
            raise RuntimeError('Running pod does not match the previous image')
        claims = {v['persistentVolumeClaim']['claimName'] for v in pods[0]['spec'].get('volumes', [])
                  if 'persistentVolumeClaim' in v}
        if claims != {self.config['claim']}:
            raise RuntimeError('Application persistent volumes do not match the single protected claim')
        pvc = self.command(self.kube + ['get', 'pvc', self.config['claim'], '-o', 'json'])
        pv = self.command(self.kube + ['get', 'pv', pvc['spec']['volumeName'], '-o', 'json'])
        if pv['spec']['persistentVolumeReclaimPolicy'] != 'Retain' or pv['spec'].get('csi', {}).get('volumeHandle') != self.config['volume']:
            raise RuntimeError('PVC does not match the retained backup volume')
        volume = self.volume()
        if not volume['Encrypted']:
            raise RuntimeError('Instance volume is not encrypted')

    def check_backups(self, receipt):
        jobs = self.command(self.aws + ['backup', 'list-backup-jobs', '--by-resource-arn', volume_arn(self.config),
            '--by-backup-vault-name', self.config['vault'], '--by-created-after', (self.now() - dt.timedelta(hours=48)).isoformat()])['BackupJobs']
        for job in jobs:
            job['CreationDate'] = timestamp(job['CreationDate'])
        if any(job['State'] in ACTIVE_JOBS for job in jobs):
            raise RuntimeError('Another backup job for this volume is still active; AWS Backup runs one job per resource, '
                               'so wait for it to finish before deploying')
        # A warm-up is not a restore point: completed, it is no fallback; failed, it only lost a speedup.
        jobs = [job for job in jobs if not release_warmup(job)]
        jobs = without_accepted_failures(jobs, receipt.get('accepted_backup_failures', []))
        health = assess_jobs(jobs, self.now())
        if not health['healthy']:
            self.save('backup-health.json', health)
            raise RuntimeError('No healthy recent completed backup; inspect backup-health.json before deploying')
        receipt['fallback_backup'] = max((j for j in jobs if j['State'] == 'COMPLETED'), key=lambda j: j['CreationDate'])['BackupJobId']

    def check_registry(self, previous, candidate):
        for release in (previous, candidate):
            _, digest = locate_package(release['image'].rsplit(':', 1)[0], 'main-' + release['source_revision'],
                                       self.config.get('aws_profile'))
            if digest != release['registry_digest']:
                raise RuntimeError('Private registry image no longer matches its verified publication')

    def check_previous_archive(self, previous):
        key = 'images/' + previous['registry_digest'].replace(':', '-') + '.docker.tar.gz'
        if self.command(self.aws + ['s3api', 'head-object', '--bucket', self.config['bucket'], '--key', key], allow_missing=True) is None:
            raise RuntimeError('Previous image is not protected in the recovery bucket; archive it before deploying')

    def preflight(self, receipt, previous, candidate):
        """Prepare's read-only checks, returning every problem so one pass shows them all."""
        # Nothing else can answer truthfully without a valid sign-in.
        self.check_identity()
        problems = []
        for check in (lambda: self.check_service(receipt, previous), lambda: self.check_storage(receipt, previous),
                      lambda: self.check_backups(receipt), lambda: self.check_registry(previous, candidate),
                      lambda: self.check_previous_archive(previous)):
            try:
                check()
            except Exception as error:
                problems.append(str(error))
        return problems

    def prepare(self, receipt, previous, candidate, candidate_dir, root):
        self.run_id = receipt['id']
        self.check_identity()
        service = self.check_service(receipt, previous)
        self.check_storage(receipt, previous)
        self.check_backups(receipt)
        self.check_registry(previous, candidate)
        receipt['previous_image_archive'] = self.protect_image(previous, existing=True)
        receipt['image_archive'] = self.protect_image(candidate)
        result_path = self.directory / 'configuration-export.json'
        result = self.command(self.aws + ['lambda', 'invoke', '--function-name', self.config['export_function'],
            '--cli-binary-format', 'raw-in-base64-out', '--payload', json.dumps({'id': receipt['id']}), str(result_path)])
        if result.get('FunctionError'):
            raise RuntimeError('Protected configuration export failed; inspect configuration-export.json')
        receipt['configuration'] = json.loads(result_path.read_text())['configuration']
        if not re.fullmatch('runs/[a-f0-9]{64}/config.json', receipt['configuration']):
            raise RuntimeError('Configuration exporter did not return a protected configuration key')
        self.protect_release(candidate_dir, root, receipt)
        self.protect('service-before.json', service)
        self.protect('prepared.json', receipt)
        self.save('deployment.json', receipt)


def https_origin(value, name):
    url = urllib.parse.urlsplit(value)
    if (url.scheme != 'https' or not url.hostname or url.username or url.password or url.query or url.fragment or
            url.path not in ('', '/')):
        raise ValueError(name + ' must be an HTTPS origin without credentials')
    return value.rstrip('/')


def configuration(path):
    config = json.loads(Path(path).read_text())
    required = ('name', 'namespace', 'claim', 'volume', 'region', 'account', 'vault', 'backup_role', 'bucket',
                'kms_key', 'export_function', 'duplo_host', 'duplo_tenant', 'public_url')
    if any(not isinstance(config.get(key), str) or not config[key].strip() for key in required):
        raise ValueError('Deployment configuration requires nonempty strings: ' + ', '.join(required))
    for key in ('aws_profile', 'kubeconfig'):
        if key in config and (not isinstance(config[key], str) or not config[key].strip()):
            raise ValueError(key + ' must be a nonempty string, or absent to use the host sign-in')
    for name in ('public_url', 'duplo_host'):
        config[name] = https_origin(config[name], name)
    return config


HELP = """
Typical use, with paths resolved from the configuration:
  deploy_instance.py --config CONFIG --plan
      local only: runtime changes, chosen previous release and state directory
  deploy_instance.py --config CONFIG --preflight --compatibility compatible --compatibility-note '...'
      read-only: every check a deployment makes, all problems reported together
  deploy_instance.py --config CONFIG --compatibility compatible --compatibility-note '...'
      stop once, capture, update, start; prints readiness and backup state
  deploy_instance.py --config CONFIG --compatibility migration
      warm-up snapshot while running (never a restore point), then stop once,
      wait for the completed stopped backup, update, start
  deploy_instance.py --config CONFIG --status [--wait]
      record backup completion; --wait blocks until it completes or fails
  deploy_instance.py --config CONFIG --accept-failed-backup JOB --accepted-failure-note '...' ...
      deploy past one reviewed failed backup job that a later backup superseded

State lives under the configuration's state_root (default: the configuration's
directory). A new deployment uses STATE_ROOT/deploy-YYYYMMDD-REVISION; the previous
release is the newest ready deployment whose release receipts match its image.
--status and --resume default to the newest deployment. Explicit --state-dir and
--previous-release override these choices.

Diagnostics: read deployment.json (the receipt and phase) first, then failure-*.json
(first error) and the command-NNNN.json it names. Repeated identical responses are
stored as {"same_as": ...} references. Do not retry failures blindly.
"""


def state_root(config, config_path):
    return Path(config.get('state_root') or Path(config_path).expanduser().resolve().parent).expanduser().resolve()


def deployments(root):
    """Deployment receipts under the state root, newest first."""
    found = []
    for path in root.glob('*/deployment.json'):
        try:
            receipt = json.loads(path.read_text())
        except (OSError, ValueError):
            continue
        found.append((str(receipt.get('started_at', '')), path.parent, receipt))
    return sorted(found, key=lambda item: item[0], reverse=True)


def latest_ready_release(root):
    """The newest ready deployment whose release receipts describe the image it deployed."""
    for _, directory, receipt in deployments(root):
        if receipt.get('phase') != 'ready':
            continue
        try:
            if release_record(directory)['reference'] == receipt.get('image'):
                return directory
        except (OSError, ValueError, KeyError):
            continue
    raise ValueError('No ready deployment with matching release receipts under ' + str(root) + '; pass --previous-release')


def main():
    parser = argparse.ArgumentParser(description=__doc__, epilog=HELP, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('--config', required=True, help='private operator configuration; contains profile names, never keys')
    parser.add_argument('--previous-release', help='deployed publication/verification directory; defaults to the newest ready deployment')
    parser.add_argument('--release-dir', help='candidate publication/verification directory; defaults to the release cache for HEAD')
    parser.add_argument('--state-dir', help='private directory for this deployment; defaults under state_root (see below)')
    parser.add_argument('--compatibility', choices=('unknown', 'compatible', 'migration'), default='unknown',
                        help='unknown or migration wait for the stopped backup to complete before updating; '
                             'compatible starts the new image once the snapshot is captured and needs --compatibility-note')
    parser.add_argument('--compatibility-note', default='', help='review evidence required for the compatible fast path')
    parser.add_argument('--accept-failed-backup', action='append', default=[], metavar='JOB',
                        help='failed backup job to ignore in preflight, once a later backup of the volume completed; repeatable')
    parser.add_argument('--accepted-failure-note', default='', help='review evidence required with --accept-failed-backup')
    parser.add_argument('--public-url', help='explicit new canonical HTTPS origin; prepare DNS, TLS and external callbacks first')
    parser.add_argument('--source-bytes', type=int, help='explicit per-source import byte limit; reuses the verified image when runtime code is unchanged')
    parser.add_argument('--plan', action='store_true', help='inspect committed inputs without contacting or changing services')
    parser.add_argument('--preflight', action='store_true', help='run every read-only check of a deployment and change nothing')
    parser.add_argument('--status', action='store_true', help='record backup completion/failure; never stop or redeploy')
    parser.add_argument('--wait', action='store_true', help='with --status, wait up to --timeout for the backup to complete or fail')
    parser.add_argument('--resume', action='store_true', help='finish startup/readiness after a successful image update; retain failures')
    parser.add_argument('--timeout', type=int, default=900, help='maximum seconds for each readiness or snapshot wait')
    args = parser.parse_args()
    if args.source_bytes is not None and not 1 <= args.source_bytes <= sys.maxsize // 64:
        parser.error('--source-bytes must be a positive bounded integer')
    if not 1 <= args.timeout <= 7200:
        parser.error('--timeout must be between 1 and 7200 seconds')
    if sum((args.plan, args.preflight, args.status, args.resume)) > 1:
        parser.error('--plan, --preflight, --status and --resume are mutually exclusive')
    if args.wait and not args.status:
        parser.error('--wait applies only to --status')
    if bool(args.accept_failed_backup) != bool(args.accepted_failure_note.strip()):
        parser.error('--accept-failed-backup and --accepted-failure-note must be given together')
    os.umask(0o077)
    root = Path(__file__).resolve().parents[2]
    receipt = None
    cloud = None
    def interrupted(signum, frame):
        raise KeyboardInterrupt(signal.Signals(signum).name)
    signal.signal(signal.SIGTERM, interrupted)
    try:
        config = configuration(args.config)
        states = state_root(config, args.config)
        if args.state_dir:
            directory = Path(args.state_dir).expanduser().resolve()
        elif args.status or args.resume:
            newest = deployments(states)
            if not newest:
                raise ValueError('No deployment receipts under ' + str(states) + '; pass --state-dir')
            directory = newest[0][1]
        elif args.preflight:
            directory = states / ('preflight-' + dt.datetime.now(UTC).strftime('%Y%m%d-%H%M%S'))
        else:
            head = subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=root, text=True).strip()
            directory = states / ('deploy-' + dt.datetime.now(UTC).strftime('%Y%m%d') + '-' + head[:7])
        directory.mkdir(parents=True, exist_ok=True, mode=0o700)
        directory.chmod(0o700)
        origin = https_origin(args.public_url, 'public URL') if args.public_url else config['public_url']
        origin_change = origin != config['public_url']
        cloud = Runtime(config, directory, args.timeout)
        identity = {key: config[key] for key in ('duplo_host', 'duplo_tenant', 'name')}
        lock = Path.home() / '.cache/forgetmenote/deployments/locks' / fingerprint(identity)
        with release_lock(lock):
            state = directory / 'deployment.json'
            if state.exists() and not args.preflight:
                receipt = json.loads(state.read_text())
                if receipt['config_fingerprint'] != fingerprint(config):
                    raise ValueError('Deployment configuration changed; use the original configuration for this receipt')
                if args.public_url and origin != receipt.get('public_url', config['public_url']):
                    raise ValueError('Requested public URL does not match the existing deployment receipt')
                if args.source_bytes is not None and args.source_bytes != receipt.get('source_bytes'):
                    raise ValueError('Requested source bytes does not match the existing deployment receipt')
                cloud.run_id = receipt['id']
                if args.plan:
                    print(json.dumps({'state_dir': str(directory), 'phase': receipt['phase'], 'image': receipt['image'],
                                      'backup_state': receipt.get('backup_state', 'not requested')}, indent=2))
                    return
                if args.status or receipt['phase'] == 'ready':
                    print('State: ' + str(directory))
                    if not receipt.get('backup_job'):
                        print('Service phase: ' + receipt['phase'] + '; no backup has been requested.')
                        return
                    refresh_backup(cloud, receipt, cloud.wait_backup(receipt) if args.wait else None)
                    print('Service phase: ' + receipt['phase'] + '; backup: ' + receipt['backup_state'])
                    return
                if not args.resume:
                    raise ValueError('Existing deployment: inspect its diagnostics, then use --resume or --status')
                upgrade(cloud, receipt, resume=True)
            else:
                if args.status or args.resume:
                    raise ValueError('Status and resume require an existing deployment state')
                previous_dir = Path(args.previous_release).expanduser() if args.previous_release else latest_ready_release(states)
                previous = release_record(previous_dir)
                revision = subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=root, text=True).strip()
                changes = runtime_changes(root, previous['source_revision'], revision)
                if args.plan:
                    print(json.dumps({'state_dir': str(directory), 'previous_release': str(previous_dir),
                                      'source_revision': revision, 'previous_revision': previous['source_revision'],
                                      'runtime_changes': changes, 'public_url': origin, 'source_bytes': args.source_bytes, 'deployment_required': bool(changes) or origin_change or args.source_bytes is not None,
                                      'accepted_backup_failures': args.accept_failed_backup,
                                      'wait_for_completed_backup': args.compatibility != 'compatible',
                                      'warm_up_before_stop': args.compatibility != 'compatible'}, indent=2))
                    return
                if subprocess.check_output(['git', 'status', '--porcelain'], cwd=root, text=True).strip():
                    raise ValueError('Deployment requires a clean committed checkout')
                if not changes and not origin_change:
                    live = cloud.service()
                    if live['Template']['Containers'][0]['Image'] != previous['reference']:
                        raise ValueError('Live service no longer matches the previous release')
                    if normalized_template(source_budget_update(live, args.source_bytes)) == normalized_template(live):
                        print('No runtime changes; no image update, snapshot or service restart required.')
                        return
                if args.compatibility == 'compatible' and not args.compatibility_note.strip():
                    raise ValueError('Compatible deployment requires --compatibility-note with the reviewed rollback assumptions')
                candidate_dir = Path(args.release_dir).expanduser() if args.release_dir else Path(os.environ.get('XDG_CACHE_HOME', str(Path.home() / '.cache'))) / 'forgetmenote/releases' / revision
                candidate = release_record(candidate_dir)
                if candidate['source_revision'] != revision and runtime_changes(root, candidate['source_revision'], revision):
                    raise ValueError('Candidate release does not match the current committed source')
                check_storage_generation(root, previous['source_revision'], candidate['source_revision'],
                                         previous_storage_v2(previous_dir))
                receipt = {'id': 'upgrade-' + uuid.uuid4().hex, 'source_revision': candidate['source_revision'],
                           'operator_revision': revision, 'previous_revision': previous['source_revision'],
                           'public_url': origin, 'source_bytes': args.source_bytes,
                           'image': candidate['reference'], 'previous_image': previous['reference'], 'volume': config['volume'],
                           'storage_v2': storage_v2(root, candidate['source_revision']),
                           'phase': 'prepared', 'compatibility': args.compatibility, 'compatibility_note': args.compatibility_note,
                           'accepted_backup_failures': args.accept_failed_backup, 'accepted_failure_note': args.accepted_failure_note,
                           'config_fingerprint': fingerprint(config), 'started_at': cloud.now().isoformat()}
                print('State: ' + str(directory) + '; previous release: ' + str(previous_dir))
                if args.preflight:
                    problems = cloud.preflight(receipt, previous, candidate)
                    if problems:
                        raise RuntimeError('Preflight refused the deployment: ' + '; '.join(problems))
                    print('Preflight passed: ' + receipt['image'] + ' can replace ' + receipt['previous_image'] + '.')
                    return
                cloud.prepare(receipt, previous, candidate, candidate_dir, root)
                upgrade(cloud, receipt)
            print('Deployed: ' + receipt['image'])
            print('Service ready. Upgrade backup: ' + receipt['backup_state'] + ' (' + receipt['backup_job'] + ').')
            if receipt['backup_state'] != 'COMPLETED':
                print('AWS Backup and existing console monitoring track completion; --status records the final protected receipt.')
    except (Exception, KeyboardInterrupt) as error:
        if cloud is not None:
            path = cloud.save('failure-' + uuid.uuid4().hex + '.json', {'error': str(error), 'time': cloud.now().isoformat(),
                              'phase': receipt.get('phase') if receipt else None})
            print('Deployment stopped: ' + str(error) + '\nDiagnostic: ' + str(path), file=sys.stderr)
        else:
            print('Deployment stopped: ' + str(error), file=sys.stderr)
        return 1
    return 0


if __name__ == '__main__':
    sys.exit(main())
