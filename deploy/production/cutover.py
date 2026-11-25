#!/usr/bin/env python3
"""Move a file-based instance onto Postgres, an owner and gateways in one stopped window.

The file-based service stops and its volume is captured by a completed AWS
Backup, as for a deployment. A helper service of the new image then mounts that
volume read-only, so the file-based release can restart on it untouched: it
migrates the database, imports the control-plane files, checks the import
against an export of it, and streams the notebook repositories to the owner's
empty volume, whose content must hash the same. Every notebook is then placed on
the owner, the helper releases the volume, and the gateways start. DNS moves to
the gateways' load balancer afterwards, outside this tool.

A rehearsal runs the same steps against a parallel set of services whose helper
mounts a volume restored from a recovery point; it leaves the file-based service
running and requires its gateways to run with outgoing duties paused.
"""
import argparse
import datetime as dt
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import time
import uuid

import deploy_instance as deploy
from verification import fingerprint, release_lock


PHASES = ('prepared', 'stopped', 'captured', 'imported', 'verified', 'copied', 'placed', 'released', 'serving')
SOURCE, STAGING = '/source', '/data/.cutover-projects'
ROUNDTRIP, PLACEMENT = '/tmp/cutover-roundtrip', '/tmp/cutover-gateway'
# The digest and count of every notebook file under a root, so no path or
# content leaves the pods while both copies are compared.
MANIFEST = ('cd "$1" && test -d projects && find projects -type f -print0 | LC_ALL=C sort -z | '
            'xargs -0r sha256sum | sha256sum | cut -d" " -f1 && find projects -type f | wc -l')


def helper_steps(owner_entry):
    """The commands the helper runs, in order, as argument lists."""
    return {
        'migrate': ['forgetmenote', 'migrate'],
        'import': ['forgetmenote', 'import-files', '-data', SOURCE],
        'roundtrip': ['forgetmenote', 'export-files', '-output', ROUNDTRIP],
        'discard': ['rm', '-rf', ROUNDTRIP],
        'place': ['forgetmenote', 'admin', 'move-notebook', '-data', PLACEMENT, '-all-local', '-owner', owner_entry],
    }


def manifest_command(root):
    return ['sh', '-c', MANIFEST, 'manifest', root]


def last_json(output):
    lines = [line for line in output.splitlines() if line.strip()]
    if not lines:
        raise RuntimeError('Command printed no report')
    return json.loads(lines[-1])


def report_mismatches(imported, exported):
    """Areas whose export of a fresh import differs from the import."""
    before = {area['area']: (area['files'], area['digest']) for area in imported}
    after = {area['area']: (area['files'], area['digest']) for area in exported}
    return sorted(name for name in before.keys() | after.keys() if before.get(name) != after.get(name))


def manifest(output):
    lines = output.split()
    if len(lines) != 2 or len(lines[0]) != 64:
        raise RuntimeError('Notebook manifest is malformed')
    return {'digest': lines[0], 'files': int(lines[1])}


def environment(service):
    """A Duplo service's explicit environment values by name."""
    settings = json.loads(service.get('Template', {}).get('OtherDockerConfig') or '{}')
    return {deploy.field(item, 'name'): deploy.field(item, 'value') for item in deploy.environment(settings)}


def advance(cloud, receipt, phase, **values):
    receipt.update(values, phase=phase, **{phase + '_at': cloud.now().isoformat()})
    cloud.save('cutover.json', receipt)


def cutover(cloud, receipt):
    """Each phase records its outcome before the next begins, so --resume repeats
    only an interrupted phase; every phase is safe to repeat."""
    steps = helper_steps(cloud.config['owner_entry'])
    rehearsal = receipt['rehearsal']
    if receipt['phase'] == 'prepared':
        if not rehearsal:
            cloud.stop()
            cloud.wait_stopped()
        advance(cloud, receipt, 'stopped', quiesced_at=cloud.now().isoformat())
    if receipt['phase'] == 'stopped':
        if not rehearsal:
            if not receipt.get('backup_job'):
                receipt['backup_job'] = cloud.start_backup(receipt['id'])
                cloud.save('cutover.json', receipt)
            receipt['snapshot'] = cloud.observe_snapshot(receipt)
            cloud.protect('capture.json', receipt)
            receipt['recovery_point'] = cloud.complete_backup(receipt)
            cloud.protect('backup-result.json', receipt)
        advance(cloud, receipt, 'captured', database_before=cloud.restore_point())
    if receipt['phase'] == 'captured':
        helper = cloud.running('helper')
        cloud.exec(helper, steps['migrate'])
        # An import commits all or nothing and refuses a database that holds
        # this instance, so an interrupted one is repeated from the start.
        imported = last_json(cloud.exec(helper, steps['import']))['imported']
        advance(cloud, receipt, 'imported', imported=imported)
    if receipt['phase'] == 'imported':
        helper = cloud.running('helper')
        cloud.exec(helper, steps['discard'])
        exported = last_json(cloud.exec(helper, steps['roundtrip']))['exported']
        cloud.exec(helper, steps['discard'])
        mismatches = report_mismatches(receipt['imported'], exported)
        if mismatches:
            raise RuntimeError('An export of the import differs in: ' + ', '.join(mismatches) + '; keep the instance stopped and investigate')
        advance(cloud, receipt, 'verified', exported=exported)
    if receipt['phase'] == 'verified':
        helper, owner = cloud.running('helper'), cloud.running('owner')
        # Only this tool writes the staging directory; a placed notebook ends it.
        if cloud.exec(owner, ['sh', '-c', 'test ! -e /data/projects && echo empty']).strip() != 'empty':
            raise RuntimeError("The owner's volume already holds notebooks; start the cutover on an empty owner volume")
        cloud.exec(owner, ['rm', '-rf', STAGING])
        cloud.exec(owner, ['mkdir', '-m', '0700', STAGING])
        source = manifest(cloud.exec(helper, manifest_command(SOURCE)))
        cloud.copy(helper, SOURCE, owner, STAGING)
        copied = manifest(cloud.exec(owner, manifest_command(STAGING)))
        if copied != source:
            raise RuntimeError('The notebooks copied to the owner differ from the stopped volume; nothing was placed')
        cloud.exec(owner, ['mv', STAGING + '/projects', '/data/projects'])
        cloud.exec(owner, ['rmdir', STAGING])
        advance(cloud, receipt, 'copied', notebook_files=source)
    if receipt['phase'] == 'copied':
        placed = last_json(cloud.exec(cloud.running('helper'), steps['place']))
        advance(cloud, receipt, 'placed', placed=placed)
    if receipt['phase'] == 'placed':
        cloud.stop_service('helper')
        advance(cloud, receipt, 'released', database_after=cloud.restore_point())
    if receipt['phase'] == 'released':
        cloud.start_service('gateway')
        for pod in cloud.ready('gateway'):
            cloud.exec(pod, ['forgetmenote', 'healthz'])
        completed = {**receipt, 'phase': 'serving', 'serving_at': cloud.now().isoformat()}
        cloud.protect('cutover-result.json', completed)
        receipt.update(completed)
        cloud.save('cutover.json', receipt)


def rollback(cloud, receipt):
    """Before the first write, the file-based service restarts on its untouched volume."""
    if receipt['rehearsal']:
        raise RuntimeError('A rehearsal never stopped the file-based service; stop its services instead')
    if receipt['phase'] == 'serving' and not receipt.get('writes_confirmed_absent'):
        raise RuntimeError('Gateways have served; once writes resumed, roll back with export-files (docs/postgres.md). '
                           'Pass --before-writes only after confirming none were accepted')
    cloud.stop_service('gateway')
    cloud.stop_service('helper')
    cloud.start()
    cloud.wait_running()
    receipt['rolled_back_at'] = cloud.now().isoformat()
    cloud.save('cutover.json', receipt)
    cloud.protect('cutover-rollback.json', receipt)


class Runtime(deploy.Runtime):
    """The deployment's AWS, Duplo and Kubernetes boundaries, plus the new services."""

    def service_config(self, role):
        return self.config['services'][role]

    def selector(self, role):
        return self.service_config(role).get('selector', 'app=' + self.service_config(role)['name'])

    def role_pods(self, role):
        return self.command(self.kube + ['get', 'pods', '-l', self.selector(role), '-o', 'json'])['items']

    def start_service(self, role):
        self.named_action('start', self.service_config(role)['name'])

    def stop_service(self, role):
        self.named_action('stop', self.service_config(role)['name'])
        self.wait_for(lambda: not self.role_pods(role), role + ' service did not stop')

    def named_action(self, action, name):
        result = self.command(self.duplo + ['service', action, name])
        details = result.get('details', {})
        if details.get('errors') or name not in details.get('success', []):
            raise RuntimeError('Duplo ' + action + ' ' + name + ' did not succeed; inspect the original command response')

    def ready(self, role):
        """Wait for the role's configured replicas, every container ready."""
        count = self.service_config(role).get('replicas', 1)
        def check():
            pods = [pod for pod in self.role_pods(role) if not pod['metadata'].get('deletionTimestamp')]
            statuses = [status for pod in pods for status in pod.get('status', {}).get('containerStatuses', [])]
            if len(pods) == count and statuses and all(status.get('ready') for status in statuses):
                return [pod['metadata']['name'] for pod in pods]
            return None
        return self.wait_for(check, role + ' pods did not become ready')

    def running(self, role):
        if not self.role_pods(role):
            self.start_service(role)
        return self.ready(role)[0]

    def wait_running(self):
        self.wait_for(lambda: self.pods() and all(c.get('ready') for c in self.pods()[0].get('status', {}).get('containerStatuses', [])),
                      'The file-based service did not become ready')

    def container_args(self, pod):
        for role, service in self.config['services'].items():
            if service.get('container') and pod.startswith(service['name']):
                return ['-c', service['container']]
        return []

    def exec(self, pod, args):
        """Run args in pod; the output is the command's own, never file content."""
        self.sequence += 1
        name = f'command-{self.sequence:04d}.json'
        full = self.kube + ['exec', pod, *self.container_args(pod), '--', *args]
        try:
            result = subprocess.run(full, capture_output=True, text=True, timeout=self.timeout)
        except subprocess.TimeoutExpired:
            self.save(name, {'command': full, 'error': 'timeout'})
            raise RuntimeError('In-cluster command timed out; inspect ' + str(self.directory / name)) from None
        self.save(name, {'command': full, 'exit_code': result.returncode, 'stdout': result.stdout, 'stderr': result.stderr})
        if result.returncode:
            raise RuntimeError('In-cluster command failed (' + str(result.returncode) + '); inspect ' + str(self.directory / name))
        return result.stdout

    def copy(self, source_pod, source_root, target_pod, target_root):
        """Stream the notebooks between pods; their bytes are never recorded."""
        read = self.kube + ['exec', source_pod, *self.container_args(source_pod), '--', 'tar', '-C', source_root, '-cf', '-', 'projects']
        write = self.kube + ['exec', '-i', target_pod, *self.container_args(target_pod), '--', 'tar', '-C', target_root, '-xf', '-']
        reader = subprocess.Popen(read, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        writer = subprocess.Popen(write, stdin=reader.stdout, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        reader.stdout.close()
        try:
            _, write_error = writer.communicate(timeout=self.timeout * 4)
            read_error = reader.stderr.read()
            reader.wait(timeout=60)
        except subprocess.TimeoutExpired:
            reader.kill()
            writer.kill()
            raise RuntimeError('Copying the notebooks timed out') from None
        self.sequence += 1
        self.save(f'command-{self.sequence:04d}.json', {'command': [read, write], 'exit_codes': [reader.returncode, writer.returncode],
                                                        'stderr': [read_error.decode(errors='replace'), write_error.decode(errors='replace')]})
        if reader.returncode or writer.returncode:
            raise RuntimeError('Copying the notebooks failed; inspect the last command record')

    def complete_backup(self, receipt):
        """Wait for the stopped backup to complete and mark its snapshot
        application-consistent; deployment receipts stay untouched."""
        def completed():
            job = self.backup_job(receipt['backup_job'])
            deploy.validate_job(self.config, job, receipt['backup_job'])
            if job['State'] in deploy.TERMINAL_FAILURES:
                raise RuntimeError('The stopped backup is ' + job['State'] + '; the file-based volume is untouched')
            return job if job['State'] == 'COMPLETED' else None
        job = self.wait_for(completed, 'The stopped backup did not complete before the deadline')
        if not job.get('RecoveryPointArn', '').endswith('/' + receipt['snapshot']['id']):
            raise RuntimeError('The completed backup does not match the captured snapshot')
        self.tag_completed(receipt)
        return job['RecoveryPointArn']

    def restore_point(self):
        instance = self.command(self.aws + ['rds', 'describe-db-instances', '--db-instance-identifier',
                                            self.config['database_instance']])['DBInstances'][0]
        return {'instance': instance['DBInstanceIdentifier'], 'latest_restorable_time': instance.get('LatestRestorableTime')}

    def preflight(self, candidate, rehearsal):
        """Read-only checks of the services, database and bucket before stopping anything."""
        problems = []
        for role, service in self.config['services'].items():
            found = self.command(self.duplo + ['service', 'find', service['name']])
            images = {container.get('Image') for container in found.get('Template', {}).get('Containers', [])}
            if candidate['reference'] not in images:
                problems.append(role + ' service does not run the candidate image')
            values = environment(found)
            if role == 'gateway' and rehearsal:
                if values.get('OUTGOING') != 'paused':
                    problems.append('rehearsal gateways must set OUTGOING=paused')
                if values.get('PUBLIC_URL') in (None, self.config['public_url']):
                    problems.append('rehearsal gateways need an origin of their own')
            elif role == 'gateway' and values.get('PUBLIC_URL') != self.config['public_url']:
                problems.append('gateways must serve the public origin ' + self.config['public_url'])
        if self.role_pods('gateway'):
            problems.append('gateways are running; stop them before the cutover')
        if not rehearsal and self.role_pods('helper'):
            problems.append('the helper is running; it can mount the volume only once the file-based service stops')
        if not rehearsal and not self.pods():
            problems.append('the file-based service is not running')
        # Every helper and owner step is an exec; finding out after the stop
        # would leave the instance stopped.
        try:
            self.command(self.kube + ['auth', 'can-i', 'create', 'pods', '--subresource=exec', '--quiet'])
        except RuntimeError:
            problems.append('the kubeconfig cannot run commands in pods (pods/exec)')
        database = self.command(self.aws + ['rds', 'describe-db-instances', '--db-instance-identifier',
                                            self.config['database_instance']])['DBInstances'][0]
        if database.get('DBInstanceStatus') != 'available':
            problems.append('the database is not available')
        if not database.get('StorageEncrypted') or database.get('PubliclyAccessible'):
            problems.append('the database must be encrypted and private')
        if not database.get('BackupRetentionPeriod') or not database.get('DeletionProtection'):
            problems.append('the database needs automated backups and deletion protection')
        bucket = self.config['blob_bucket']
        versioning = self.command(self.aws + ['s3api', 'get-bucket-versioning', '--bucket', bucket])
        if versioning.get('Status') != 'Enabled':
            problems.append('the blob bucket needs versioning')
        block = self.command(self.aws + ['s3api', 'get-public-access-block', '--bucket', bucket])['PublicAccessBlockConfiguration']
        if not all(block.get(key) for key in ('BlockPublicAcls', 'IgnorePublicAcls', 'BlockPublicPolicy', 'RestrictPublicBuckets')):
            problems.append('the blob bucket must block public access')
        self.command(self.aws + ['s3api', 'get-bucket-encryption', '--bucket', bucket])
        return problems


def configuration(path):
    config = deploy.configuration(path)
    services = config.get('services')
    if not isinstance(services, dict) or set(services) != {'helper', 'owner', 'gateway'}:
        raise ValueError('Cutover configuration requires services.helper, services.owner and services.gateway')
    for role, service in services.items():
        if not isinstance(service.get('name'), str) or not service['name'].strip() or service['name'] == config['name']:
            raise ValueError(role + ' service needs its own name')
    if len({service['name'] for service in services.values()}) != len(services):
        raise ValueError('helper, owner and gateway services need distinct names')
    replicas = services['gateway'].get('replicas', 1)
    if not isinstance(replicas, int) or not 1 <= replicas <= 16:
        raise ValueError('services.gateway.replicas must be between 1 and 16')
    for key in ('owner_entry', 'database_instance', 'blob_bucket'):
        if not isinstance(config.get(key), str) or not config[key].strip():
            raise ValueError('Cutover configuration requires ' + key)
    if not isinstance(config.get('rehearsal', False), bool):
        raise ValueError('rehearsal must be true or false')
    return config


HELP = """
  cutover.py --config CONFIG --release-dir DIR --plan
      local only: the services, checks and steps
  cutover.py --config CONFIG --release-dir DIR
      preflight, then stop, capture, import, verify, copy, place and serve
  cutover.py --config CONFIG --resume
      repeat the interrupted phase of the newest cutover and continue
  cutover.py --config CONFIG --rollback [--before-writes]
      stop the new services and restart the file-based one on its volume

The instance stays stopped after a failure; read cutover.json, failure-*.json
and the command record it names, then --resume or --rollback. Never retry a
failed step blindly. DNS moves to the gateways only after this reports serving.
"""


def main():
    parser = argparse.ArgumentParser(description=__doc__, epilog=HELP, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('--config', required=True, help='private cutover configuration: the deployment configuration plus the new services')
    parser.add_argument('--release-dir', help='verified publication of the image the new services run')
    parser.add_argument('--state-dir', help='private directory for this cutover; defaults under state_root')
    parser.add_argument('--plan', action='store_true', help='print the steps without contacting or changing services')
    parser.add_argument('--resume', action='store_true', help='continue the newest cutover from its recorded phase')
    parser.add_argument('--rollback', action='store_true', help='restart the file-based service before any write')
    parser.add_argument('--before-writes', action='store_true', help='with --rollback after serving: confirm no write was accepted')
    parser.add_argument('--timeout', type=int, default=900, help='maximum seconds for each wait or in-cluster command')
    args = parser.parse_args()
    if sum((args.plan, args.resume, args.rollback)) > 1:
        parser.error('--plan, --resume and --rollback are mutually exclusive')
    if args.before_writes and not args.rollback:
        parser.error('--before-writes applies only to --rollback')
    if not 1 <= args.timeout <= 7200:
        parser.error('--timeout must be between 1 and 7200 seconds')
    os.umask(0o077)
    cloud = receipt = None
    signal.signal(signal.SIGTERM, lambda signum, frame: (_ for _ in ()).throw(KeyboardInterrupt(signal.Signals(signum).name)))
    try:
        config = configuration(args.config)
        root = deploy.state_root(config, args.config)
        rehearsal = config.get('rehearsal', False)
        if args.state_dir:
            directory = Path(args.state_dir).expanduser().resolve()
        else:
            found = sorted(root.glob('cutover-*/cutover.json'), key=lambda path: path.stat().st_mtime, reverse=True)
            if args.resume or args.rollback:
                if not found:
                    raise ValueError('No cutover receipts under ' + str(root) + '; pass --state-dir')
                directory = found[0].parent
            else:
                directory = root / ('cutover-' + dt.datetime.now(dt.timezone.utc).strftime('%Y%m%d-%H%M%S'))
        candidate = deploy.release_record(args.release_dir) if args.release_dir else None
        if args.plan:
            if candidate is None:
                raise ValueError('--plan needs --release-dir')
            print(json.dumps({'state_dir': str(directory), 'rehearsal': rehearsal, 'image': candidate['reference'],
                              'file_based_service': config['name'], 'services': config['services'],
                              'stops_file_based_service': not rehearsal, 'phases': PHASES,
                              'helper_steps': helper_steps(config['owner_entry'])}, indent=2))
            return 0
        directory.mkdir(parents=True, exist_ok=True, mode=0o700)
        directory.chmod(0o700)
        cloud = Runtime(config, directory, args.timeout)
        lock = Path.home() / '.cache/forgetmenote/deployments/locks' / fingerprint({key: config[key] for key in ('duplo_host', 'duplo_tenant', 'name')})
        with release_lock(lock):
            state = directory / 'cutover.json'
            if state.exists():
                receipt = json.loads(state.read_text())
                if receipt['config_fingerprint'] != fingerprint(config):
                    raise ValueError('Cutover configuration changed; use the original configuration for this receipt')
                cloud.run_id = receipt['id']
                if args.rollback:
                    receipt['writes_confirmed_absent'] = args.before_writes or receipt.get('writes_confirmed_absent', False)
                    rollback(cloud, receipt)
                    print('Rolled back: ' + config['name'] + ' serves its volume again.')
                    return 0
                if not args.resume:
                    raise ValueError('Existing cutover: inspect its diagnostics, then use --resume or --rollback')
            else:
                if args.resume or args.rollback:
                    raise ValueError('Resume and rollback require an existing cutover')
                if candidate is None:
                    raise ValueError('A cutover needs --release-dir')
                receipt = {'id': 'cutover-' + uuid.uuid4().hex, 'rehearsal': rehearsal, 'image': candidate['reference'],
                           'source_revision': candidate['source_revision'], 'volume': config['volume'], 'phase': 'prepared',
                           'config_fingerprint': fingerprint(config), 'started_at': cloud.now().isoformat()}
                cloud.run_id = receipt['id']
                problems = cloud.preflight(candidate, rehearsal)
                if problems:
                    raise ValueError('Preflight refused the cutover: ' + '; '.join(problems))
                cloud.save('cutover.json', receipt)
            print('State: ' + str(directory))
            cutover(cloud, receipt)
            print('Serving: ' + config['services']['gateway']['name'] + '. Move DNS to its load balancer, then check the public origin.')
    except (Exception, KeyboardInterrupt) as error:
        if cloud is not None:
            path = cloud.save('failure-' + uuid.uuid4().hex + '.json', {'error': str(error), 'time': cloud.now().isoformat(),
                              'phase': receipt.get('phase') if receipt else None})
            print('Cutover stopped: ' + str(error) + '\nDiagnostic: ' + str(path), file=sys.stderr)
        else:
            print('Cutover stopped: ' + str(error), file=sys.stderr)
        return 1
    return 0


if __name__ == '__main__':
    sys.exit(main())
