#!/usr/bin/env python3
"""Move a Duplo instance with a notebook owner and gateways onto storage v2.

Storage v2 keeps every notebook in Postgres and the blob store and has no owner
process. On this stack the owner's volume holds each notebook's Git repository,
which only a process that mounts that volume can import. So the owner service
becomes the exporter: a gateway of the new release on the owner's volume. It
serves no traffic, keeps the Git exports (DATA/exports) and runs the backup
sidecars beside them. The load-balanced gateways keep their own disks.

The gateways and the owner stop; an AWS Backup of the owner's volume and an RDS
snapshot are taken; the exporter starts on the new image holding still (no
server), and the storage commands of docs/storage-cutover.md run in it:
migrate, import, verify, export and check. Only when check passes for every
notebook do the exporter and the gateways serve the new release, with outgoing
actions paused. Then the previous release is made to refuse the database
(migrate -finish-rollout), the gateways stop briefly while the exporter
retires the previous release's files, and outgoing actions resume. Each phase
records its outcome before the next begins, so --resume repeats only an
interrupted phase, and every phase is safe to repeat.
"""
import argparse
import copy
import datetime as dt
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import sys
import time
import uuid

import deploy_instance as deploy
import roll_gateways
from verification import fingerprint, release_lock


PHASES = ('prepared', 'stopped', 'captured', 'held', 'imported', 'verified', 'exported', 'checked',
          'serving', 'finished', 'retired', 'complete')
ROOT = Path(__file__).resolve().parents[2]
# The exporter holds still while the storage commands run: no server, so no gateway presence.
HOLD = ['/bin/sh', '-c', 'trap "exit 0" TERM INT; while :; do sleep 3600 & wait $!; done']
PROBES = ('LivenessProbe', 'ReadinessProbe', 'StartupProbe', 'livenessProbe', 'readinessProbe', 'startupProbe')
WORKSPACE = re.compile(r'projects/[A-Za-z0-9][A-Za-z0-9._-]*/workspace')
EXPORTS = '/exports'
# A gateway's presence outlasts a crash by 30 seconds; the storage commands refuse until it expires.
PRESENCE_SECONDS = 35
CUTOVER_BACKUP_DAYS = 90
SUCCEEDED = {
    'import': ('imported', 'already imported', 'resumed'),
    'verify': ('verified',),
    'export': ('exported',),
    'check': ('passed',),
    'retire': ('removed', 'skipped: no previous release files'),
    # retire without -yes verifies each notebook as -yes would and removes nothing
    'retire plan': ('would remove', 'skipped: no previous release files'),
}
SKIPPED = ('skipped: no notebook repository', 'skipped: not in the notebook store', 'skipped: no previous release files')


def settings(service):
    return json.loads(service['Template'].get('OtherDockerConfig') or '{}')


def with_settings(service, values, image):
    desired = copy.deepcopy(service)
    containers = desired['Template']['Containers']
    if len(containers) != 1:
        raise ValueError('Service ' + service['Name'] + ' must run one application container')
    containers[0]['Image'] = image
    desired['Template']['OtherDockerConfig'] = json.dumps(values)
    return desired


def named(items, name):
    return [item for item in items if deploy.field(item, 'name') == name]


def image_of(service):
    images = {container.get('Image') for container in service['Template'].get('Containers', [])}
    if len(images) != 1:
        raise ValueError('Service ' + service['Name'] + ' must run one image')
    return images.pop()


def runs_owner(service):
    """Whether the service runs the previous release's `forgetmenote owner`."""
    values = settings(service)
    return (values.get('Args') or values.get('args')) == ['owner']


def with_environment(values, paused):
    """values without the owner map, which storage v2 refuses, and with OUTGOING=paused while paused."""
    if 'Env' in values and 'env' in values:
        raise ValueError('Services must keep one environment list')
    key = 'env' if 'env' in values else 'Env'
    env = [item for item in values.get(key, []) if deploy.field(item, 'name') not in ('OWNER_MAP_FILE', 'OUTGOING')]
    if paused:
        env.append({'Name': 'OUTGOING', 'Value': 'paused'})
    values[key] = env
    return values


def gateway_v2(gateway, image, paused):
    """The gateways on the new release."""
    return with_settings(gateway, with_environment(settings(gateway), paused), image)


def backup_sidecars(owner, configs):
    """The owner's sidecars, each one that backs up a notebook's working tree
    turned to that notebook's Git export. It mounts DATA/exports at /exports,
    so the watcher sees a purge or a recreated export replace the repository
    and restarts on it, and it reads the storage v2 configuration the operator
    added to its secret, which names /exports/NOTEBOOK.git. Returns the
    sidecars, the secret volumes they need, and the (secret, key) pairs."""
    values = settings(owner)
    volumes = values.get('Volumes', [])
    sidecars, changed, keys = [], {}, []
    for container in values.get('AdditionalContainers', []):
        container = copy.deepcopy(container)
        name = deploy.field(container, 'name')
        mounts = deploy.field(container, 'volumeMounts') or []
        trees = [m for m in mounts if deploy.field(m, 'name') == 'data' and WORKSPACE.fullmatch(deploy.field(m, 'subPath') or '')]
        if trees:
            config = configs.get(name)
            if not isinstance(config, dict) or not all(isinstance(config.get(k), str) and config[k] for k in ('key', 'path')):
                raise ValueError('Sidecar ' + name + ' backs up a working tree; name its storage v2 configuration in storage_v2.sidecars')
            args = list(deploy.field(container, 'args') or [])
            if args.count('--config') != 1 or args.index('--config') + 1 >= len(args):
                raise ValueError('Sidecar ' + name + ' must take one --config file')
            directory = os.path.dirname(args[args.index('--config') + 1])
            secrets = [m for m in mounts if deploy.field(m, 'mountPath') == directory]
            source = secrets and named(volumes, deploy.field(secrets[0], 'name'))
            secret = source and deploy.field(source[0], 'secret')
            items = secret and deploy.field(secret, 'items')
            if len(secrets) != 1 or not isinstance(items, list):
                raise ValueError('Sidecar ' + name + ' must read its configuration from one mounted secret with listed keys')
            volume = copy.deepcopy(changed.get(deploy.field(source[0], 'name'), source[0]))
            listed = deploy.field(deploy.field(volume, 'secret'), 'items')
            if not any(deploy.field(item, 'key') == config['key'] for item in listed):
                listed.append({'Key': config['key'], 'Path': config['path']})
            changed[deploy.field(volume, 'name')] = volume
            keys.append((deploy.field(secret, 'secretName'), config['key']))
            args[args.index('--config') + 1] = directory + '/' + config['path']
            container[deploy.field_key(container, 'args')] = args
            kept = [m for m in mounts if m not in trees]
            kept.append({'MountPath': EXPORTS, 'Name': 'data', 'ReadOnlyProperty': True, 'SubPath': 'exports'})
            container[deploy.field_key(container, 'volumeMounts')] = kept
        sidecars.append(container)
    return sidecars, changed, keys


def exporter(owner, gateway, image, *, held, paused, sidecar_configs):
    """The owner service as a gateway of the new release on the owner's volume.

    It takes the gateways' settings (environment, secrets, probes, account and
    resources), so it runs what they run, with the owner's volume as its data
    directory. Held, it runs no server, sidecar or probe, only the storage
    commands run in it. Serving, it adds the owner's sidecars, turned to the
    exports."""
    values = copy.deepcopy(settings(gateway))
    owned = settings(owner)
    data = [v for v in named(owned.get('Volumes', []), 'data') if deploy.field(v, 'persistentVolumeClaim')]
    if len(data) != 1:
        raise ValueError('The owner must keep its notebooks on one persistent volume named data')
    if len(named(values.get('Volumes', []), 'data')) != 1:
        raise ValueError('The gateways must have one data volume')
    values['Volumes'] = [data[0] if deploy.field(v, 'name') == 'data' else v for v in values['Volumes']]
    for key in ('Args', 'args'):
        values.pop(key, None)
    with_environment(values, paused)
    if held:
        values['Command'] = HOLD
        for key in PROBES:
            values.pop(key, None)
        values.pop('AdditionalContainers', None)
        return with_settings(owner, values, image)
    sidecars, changed, _ = backup_sidecars(owner, sidecar_configs)
    present = {deploy.field(v, 'name') for v in values['Volumes']}
    for container in sidecars:
        container[deploy.field_key(container, 'image')] = image
        for mount in deploy.field(container, 'volumeMounts') or []:
            volume = deploy.field(mount, 'name')
            source = changed.get(volume) or next(iter(named(owned.get('Volumes', []), volume)), None)
            if volume == 'data':
                continue
            if source is None:
                raise ValueError('Sidecar volume ' + volume + " is not one of the owner's volumes")
            if volume in present:
                if named(values['Volumes'], volume) != [source]:
                    raise ValueError('Sidecar volume ' + volume + " differs from the gateways' volume of that name")
                continue
            values['Volumes'].append(source)
            present.add(volume)
    if sidecars:
        values['AdditionalContainers'] = sidecars
    return with_settings(owner, values, image)


def environment_value(container, name):
    return next((item.get('value') for item in container.get('env', []) if item.get('name') == name), None)


def pod_matches(pod, application, image, *, held=None, paused=None):
    """A live pod of the desired release: image, mode and pause, every container ready and never restarted."""
    if pod['metadata'].get('deletionTimestamp'):
        return False
    containers = pod['spec']['containers']
    main = next((c for c in containers if c['name'] == application), None)
    statuses = pod.get('status', {}).get('containerStatuses', [])
    if main is None or main['image'] != image or len(statuses) != len(containers):
        return False
    if held is not None and (main.get('command') == HOLD) != held:
        return False
    if paused is not None and (environment_value(main, 'OUTGOING') == 'paused') != paused:
        return False
    return all(s.get('ready') and s.get('restartCount', 0) == 0 for s in statuses)


def storage(cloud, receipt, command, *extra, expect=None):
    """Run one admin storage command on the exporter. Each notebook of the owner's volume must succeed; another
    the command finds, such as a directory without a repository, may be skipped but never fail."""
    output = cloud.run_owner(['forgetmenote', 'admin', 'storage', command, '-data', '/data', *extra])
    lines = [json.loads(line) for line in output.splitlines() if line.strip().startswith('{')]
    notebooks = {line['notebook']: line for line in lines if 'notebook' in line}
    succeeded = SUCCEEDED[expect or command]
    wrong = sorted(id for id, line in notebooks.items() if line.get('error') or line.get('status') not in succeeded + SKIPPED)
    missing = sorted(id for id in receipt['notebooks'] if id not in wrong and notebooks.get(id, {}).get('status') not in succeeded)
    totals = [line for line in lines if 'summary' in line]
    if wrong or missing or command == 'check' and (len(totals) != 1 or totals[0].get('passed') is not True):
        raise RuntimeError('admin storage ' + command + ' did not succeed for ' + ', '.join(wrong + missing or ['the summary']) +
                           '; read the command record (docs/storage-cutover.md lists each refusal)')
    counts = {}
    for line in notebooks.values():
        counts[line['status']] = counts.get(line['status'], 0) + 1
    return counts


def advance(cloud, receipt, phase, **values):
    receipt.update(values, phase=phase, **{phase + '_at': cloud.now().isoformat()})
    cloud.save('storage-cutover.json', receipt)


def stop_all(cloud, receipt):
    """Stop the gateways, then the owner's service; the storage commands wait out the presence they leave."""
    cloud.stop_service('gateway')
    cloud.stop_service('owner')
    receipt['stopped_at'] = cloud.now().isoformat()
    cloud.save('storage-cutover.json', receipt)


def held_exporter(cloud, receipt):
    """The exporter on the new image without a server, for the storage commands."""
    image = receipt['image']
    cloud.update_service('owner', 'exporter-held.json', exporter(
        receipt['owner_before'], receipt['gateway_before'], image, held=True, paused=True,
        sidecar_configs=receipt['sidecar_configs']), image)
    cloud.start_service('owner')
    cloud.converged('owner', image, held=True, paused=True)
    cloud.wait_presence(receipt['stopped_at'])


def serve(cloud, receipt):
    """The exporter and the gateways serve the new release, paused as the receipt says."""
    image, paused = receipt['image'], receipt['paused']
    since = cloud.now()
    cloud.update_service('owner', 'exporter-serving.json', exporter(
        receipt['owner_before'], receipt['gateway_before'], image, held=False, paused=paused,
        sidecar_configs=receipt['sidecar_configs']), image)
    cloud.update_service('gateway', 'gateway-v2.json', gateway_v2(receipt['gateway_before'], image, paused), image)
    cloud.start_service('owner')
    cloud.start_service('gateway')
    cloud.converged('owner', image, held=False, paused=paused)
    cloud.converged('gateway', image, held=False, paused=paused)
    cloud.healthy()
    cloud.sidecars_synced(receipt['sidecars'], since)


def cutover(cloud, receipt):
    if receipt['phase'] == 'prepared':
        stop_all(cloud, receipt)
        advance(cloud, receipt, 'stopped', quiesced_at=receipt['stopped_at'])
    if receipt['phase'] == 'stopped':
        if not receipt.get('backup_job'):
            receipt['backup_job'] = cloud.start_cutover_backup(receipt['id'])
            cloud.save('storage-cutover.json', receipt)
        receipt['snapshot'] = cloud.observe_snapshot(receipt)
        cloud.protect('capture.json', receipt)
        receipt['database_snapshot'] = cloud.database_snapshot(receipt['id'])
        receipt['recovery_point'] = cloud.complete_backup(receipt)
        advance(cloud, receipt, 'captured', database_before=cloud.restore_point())
        cloud.protect('backup-result.json', receipt)
    if receipt['phase'] == 'captured':
        held_exporter(cloud, receipt)
        cloud.run_owner(['forgetmenote', 'migrate'])
        found = cloud.run_owner(['sh', '-c', 'cd /data/projects && for d in */; do [ -d "$d/workspace/.git" ] && echo "${d%/}"; done; true'])
        notebooks = sorted(found.split())
        if not notebooks:
            raise RuntimeError("The owner's volume holds no notebook repository; keep the instance stopped and inspect it")
        advance(cloud, receipt, 'held', notebooks=notebooks)
    if receipt['phase'] == 'held':
        advance(cloud, receipt, 'imported', imported=storage(cloud, receipt, 'import'))
    if receipt['phase'] == 'imported':
        advance(cloud, receipt, 'verified', verified=storage(cloud, receipt, 'verify'))
    if receipt['phase'] == 'verified':
        advance(cloud, receipt, 'exported', exported=storage(cloud, receipt, 'export'))
    if receipt['phase'] == 'exported':
        checked = storage(cloud, receipt, 'check')
        # Retiring needs a second stop after -finish-rollout; find out now, while the previous release can still return.
        plan = storage(cloud, receipt, 'retire', expect='retire plan')
        advance(cloud, receipt, 'checked', checked=checked, retire_plan=plan)
        cloud.protect('checked.json', receipt)
    if receipt['phase'] == 'checked':
        receipt['paused'] = True
        # From here people can save to the new release, which a rollback would discard.
        receipt.setdefault('served_from', cloud.now().isoformat())
        cloud.save('storage-cutover.json', receipt)
        serve(cloud, receipt)
        advance(cloud, receipt, 'serving')
    if receipt['phase'] == 'serving':
        output = cloud.run_owner(['forgetmenote', 'migrate', '-finish-rollout'])
        if 'database requires reader version 2' not in output:
            raise RuntimeError('migrate -finish-rollout did not raise the reader version; read the command record')
        advance(cloud, receipt, 'finished')
        cloud.protect('finished.json', receipt)
    if receipt['phase'] == 'finished':
        stop_all(cloud, receipt)
        held_exporter(cloud, receipt)
        try:
            retired = storage(cloud, receipt, 'retire', '-yes')
        except Exception as error:
            serve(cloud, receipt)
            raise RuntimeError(str(error) + ". The new release serves again, outgoing actions still paused, and the previous "
                               "release's files stay; fix the cause, then --resume stops it once more to retire them") from error
        serve(cloud, receipt)
        advance(cloud, receipt, 'retired', retired=retired)
    if receipt['phase'] == 'retired':
        receipt['paused'] = False
        serve(cloud, receipt)
        completed = {**receipt, 'phase': 'complete', 'complete_at': cloud.now().isoformat()}
        cloud.protect('storage-cutover-result.json', completed)
        receipt.update(completed)
        cloud.save('storage-cutover.json', receipt)
        # The next deployment of the owner's service finds the release it runs here.
        cloud.save('deployment.json', deployment_record(receipt))


def deployment_record(receipt):
    """The ready deployment receipt deploy_instance.py reads as the live release."""
    return {'id': receipt['id'], 'phase': 'ready', 'image': receipt['image'], 'previous_image': receipt['previous_image'],
            'source_revision': receipt['source_revision'], 'volume': receipt['volume'], 'started_at': receipt['started_at'],
            'ready_at': receipt['complete_at'], 'config_fingerprint': receipt['owner_fingerprint'],
            'backup_job': receipt['backup_job'], 'snapshot': receipt['snapshot'], 'backup_state': 'COMPLETED',
            'backup_completed_at': receipt['captured_at'], 'storage_cutover': True}


def rollback(cloud, receipt, discard_writes):
    """Both services back on the previous release, possible until migrate -finish-rollout."""
    phase = PHASES.index(receipt['phase'])
    if phase >= PHASES.index('finished'):
        raise RuntimeError('The previous release refuses this database since migrate -finish-rollout. Restore the database snapshot ' +
                           str(receipt.get('database_snapshot')) + ' and the owner volume from ' + str(receipt.get('recovery_point')) +
                           ' (deploy/production/storage-cutover.md#rollback)')
    if receipt.get('served_from') and not discard_writes:
        raise RuntimeError('The new release may have served since ' + receipt['served_from'] + ': what it saved since is only in '
                           'the database and the exports. Pass --discard-v2-writes after confirming nothing was saved, or keep it')
    previous = receipt['previous_image']
    cloud.stop_service('gateway')
    cloud.stop_service('owner')
    cloud.update_service('owner', 'owner-restored.json', receipt['owner_before'], previous)
    cloud.update_service('gateway', 'gateway-restored.json', receipt['gateway_before'], previous)
    cloud.start_service('owner')
    cloud.start_service('gateway')
    cloud.converged('owner', previous)
    cloud.converged('gateway', previous)
    cloud.healthy()
    receipt['rolled_back_at'] = cloud.now().isoformat()
    cloud.save('storage-cutover.json', receipt)
    cloud.protect('storage-cutover-rollback.json', receipt)


class Runtime(roll_gateways.Runtime):
    """The owner (its volume, backups and pod), the gateways, the database and the bucket."""

    def find(self, role):
        return self.command(self.duplo + ['service', 'find', self.service_config(role)['name']])

    def update_service(self, role, name, desired, image):
        service = self.service_config(role)['name']
        path = self.save(name, desired)
        self.command(self.duplo + ['service', 'update', service, '-f', str(path)])
        self.command(self.duplo + ['service', 'update_image', service, image])
        self.protect(name, desired)

    def stop_service(self, role):
        if role == 'owner':
            self.stop()
            self.wait_stopped()
        else:
            super().stop_service(role)

    def start_service(self, role):
        # Starting runs only on a stopped service; an update rolls a running one.
        if not self.role_pods(role):
            super().start_service(role)

    def converged(self, role, image, held=None, paused=None):
        """Wait until the role's every replica runs the desired release, and no other pod is left."""
        count = self.service_config(role).get('replicas', 1)
        application = self.service_config(role)['name']
        def done():
            pods = self.role_pods(role)
            return len(pods) == count and all(pod_matches(p, application, image, held=held, paused=paused) for p in pods)
        self.wait_for(done, role + ' did not converge on ' + image)

    def start_cutover_backup(self, run_id):
        return self.backup_request(run_id, CUTOVER_BACKUP_DAYS, 'Consistency=unverified,Purpose=storage-v2-cutover')

    def database_snapshot(self, run_id):
        """A manual RDS snapshot of the stopped instance's database, one per cutover."""
        instance = self.config['database_instance']
        name = instance + '-pre-storage-v2-' + run_id.rsplit('-', 1)[-1][:12]
        listed = self.command(self.aws + ['rds', 'describe-db-snapshots', '--db-instance-identifier', instance,
                                          '--snapshot-type', 'manual'])['DBSnapshots']
        if not any(s['DBSnapshotIdentifier'] == name for s in listed):
            self.command(self.aws + ['rds', 'create-db-snapshot', '--db-instance-identifier', instance,
                                     '--db-snapshot-identifier', name, '--tags', 'Key=Purpose,Value=storage-v2-cutover'])
        def available():
            found = self.command(self.aws + ['rds', 'describe-db-snapshots', '--db-snapshot-identifier', name])['DBSnapshots'][0]
            if found['Status'] in ('failed', 'deleted'):
                raise RuntimeError('The database snapshot ' + name + ' is ' + found['Status'])
            return found['Status'] == 'available' and name
        return self.wait_for(available, 'The database snapshot did not become available')

    def wait_presence(self, stopped_at):
        remaining = PRESENCE_SECONDS - (self.now() - dt.datetime.fromisoformat(stopped_at)).total_seconds()
        if remaining > 0:
            time.sleep(remaining)

    def run_owner(self, args, container=None):
        """Run args in the exporter's application container, or another of its containers; the output is the command's own."""
        pods = [p['metadata']['name'] for p in self.role_pods('owner') if not p['metadata'].get('deletionTimestamp')]
        if len(pods) != 1:
            raise RuntimeError('Expected one exporter pod, found ' + str(len(pods)))
        self.sequence += 1
        name = f'command-{self.sequence:04d}.json'
        full = self.kube + ['exec', pods[0], '-c', container or self.service_config('owner')['name'], '--', *args]
        try:
            result = subprocess.run(full, capture_output=True, text=True, timeout=self.timeout)
        except subprocess.TimeoutExpired:
            self.save(name, {'command': full, 'error': 'timeout'})
            raise RuntimeError('In-cluster command timed out; inspect ' + str(self.directory / name)) from None
        self.save(name, {'command': full, 'exit_code': result.returncode, 'stdout': result.stdout, 'stderr': result.stderr})
        if result.returncode:
            raise RuntimeError('In-cluster command failed (' + str(result.returncode) + '); inspect ' + str(self.directory / name))
        return result.stdout

    def healthy(self):
        self.wait_for(lambda: all(self.answers(endpoint) for endpoint in ('healthz', 'readyz')),
                      'The public origin did not answer healthz and readyz')

    def sidecar_status(self, container, state):
        try:
            return json.loads(self.run_owner(['cat', state + '/status.json'], container) or '{}')
        except (RuntimeError, ValueError):
            return {}

    def sidecars_synced(self, sidecars, since):
        """Each backup sidecar has pushed, or found pushed, its export since the exporter started."""
        for container, state in sidecars.items():
            def synced():
                status = self.sidecar_status(container, state)
                updated = status.get('updated_at')
                return status.get('status') == 'synced' and updated and dt.datetime.fromisoformat(updated) >= since
            self.wait_for(synced, 'Backup sidecar ' + container + ' did not report its export pushed')

    def check(self, candidate, configs):
        """Every read-only check before the stop, the problems reported together."""
        problems = []
        self.check_identity()
        owner, gateway = self.find('owner'), self.find('gateway')
        previous = None
        try:
            previous = image_of(owner)
            if image_of(gateway) != previous:
                problems.append('the owner and the gateways must run the same release')
        except ValueError as error:
            problems.append(str(error))
        if previous == candidate['reference']:
            problems.append('the services already run the candidate')
        if not runs_owner(owner):
            problems.append('the owner service does not run `forgetmenote owner`; this instance may already be on storage v2')
        v2 = deploy.storage_v2(ROOT, candidate['source_revision'])
        if v2 is None:
            problems.append('this checkout lacks the candidate revision ' + candidate['source_revision'] + '; fetch it first')
        elif not v2:
            problems.append('the candidate is not a storage v2 release; deploy it as usual')
        if any(deploy.field(item, 'name') == 'OUTGOING' for item in deploy.environment(settings(gateway))):
            problems.append('the gateways must not set OUTGOING; the cutover pauses and resumes it')
        if 'forgetmenote migrate' not in json.dumps(settings(gateway).get('Command')):
            problems.append('the gateways must start with `forgetmenote migrate && exec forgetmenote serve`')
        keys = []
        for held in (True, False):
            try:
                self.save('exporter-' + ('held' if held else 'serving') + '.json',
                          exporter(owner, gateway, candidate['reference'], held=held, paused=True, sidecar_configs=configs))
                keys = keys or backup_sidecars(owner, configs)[2]
            except ValueError as error:
                problems.append(str(error))
        self.save('gateway-v2.json', gateway_v2(gateway, candidate['reference'], True))
        for secret, key in keys:
            template = '{"present":{{if index .data ' + json.dumps(key) + '}}true{{else}}false{{end}}}'
            if self.command(self.kube + ['get', 'secret', secret, '-o', 'go-template=' + template]).get('present') is not True:
                problems.append('secret ' + secret + ' lacks ' + key + ', the storage v2 configuration of its backup sidecar')
        sidecars = {}
        for container in settings(owner).get('AdditionalContainers', []):
            mounts = deploy.field(container, 'volumeMounts') or []
            state = [deploy.field(m, 'mountPath') for m in mounts if (deploy.field(m, 'subPath') or '').startswith('backup-worker/')]
            if len(state) == 1:
                sidecars[deploy.field(container, 'name')] = state[0]
        for container, state in sidecars.items():
            if self.sidecar_status(container, state).get('status') != 'synced':
                problems.append('backup sidecar ' + container + ' has not pushed the latest revision')
        for check in (lambda: self.check_storage({}, {'reference': previous}), lambda: self.check_backups({})):
            try:
                check()
            except Exception as error:
                problems.append(str(error))
        count = self.service_config('gateway').get('replicas', 1)
        if len([p for p in self.role_pods('gateway') if pod_matches(p, self.service_config('gateway')['name'], previous)]) != count:
            problems.append(f'expected {count} ready gateways on the current release')
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
        if self.command(self.aws + ['s3api', 'get-bucket-versioning', '--bucket', self.config['blob_bucket']]).get('Status') != 'Enabled':
            problems.append('the blob bucket needs versioning')
        _, digest = deploy.locate_package(candidate['image'].rsplit(':', 1)[0], 'main-' + candidate['source_revision'],
                                          self.config.get('aws_profile'))
        if digest != candidate['registry_digest']:
            problems.append('the private registry image no longer matches its verified publication')
        return problems, owner, gateway, previous, sidecars


def configuration(owner_path, gateway_path):
    """The owner's deployment configuration with the services, database and blob bucket of the gateways'."""
    owner = deploy.configuration(owner_path)
    gateways = json.loads(Path(gateway_path).read_text())
    services = gateways.get('services') or {}
    if not all(isinstance(services.get(role), dict) and services[role].get('name') for role in ('owner', 'gateway')):
        raise ValueError('The gateway configuration requires services.owner and services.gateway')
    if services['owner']['name'] != owner['name']:
        raise ValueError("services.owner must be the owner configuration's service")
    for key in ('database_instance', 'blob_bucket'):
        if not isinstance(gateways.get(key), str) or not gateways[key].strip():
            raise ValueError('The gateway configuration requires ' + key)
    sidecars = (gateways.get('storage_v2') or {}).get('sidecars') or {}
    if not isinstance(sidecars, dict):
        raise ValueError('storage_v2.sidecars must map each backup sidecar to its storage v2 configuration key and path')
    if deploy.state_root(owner, owner_path) != deploy.state_root(gateways, gateway_path):
        raise ValueError('The owner and gateway configurations must share one state_root')
    return {**owner, 'services': {role: services[role] for role in ('owner', 'gateway')},
            'database_instance': gateways['database_instance'], 'blob_bucket': gateways['blob_bucket'],
            'storage_v2': {'sidecars': sidecars}}


HELP = """
  storage_cutover.py --owner-config CONFIG --gateway-config CONFIG --release-dir DIR --plan
      local only: the phases and services
  storage_cutover.py --owner-config CONFIG --gateway-config CONFIG --release-dir DIR --preflight
      read-only: every check, all problems together; writes the derived services for review
  storage_cutover.py --owner-config CONFIG --gateway-config CONFIG --release-dir DIR
      preflight, then the whole cutover (deploy/production/storage-cutover.md)
  storage_cutover.py --owner-config CONFIG --gateway-config CONFIG --resume
      repeat the interrupted phase of the newest cutover and continue
  storage_cutover.py --owner-config CONFIG --gateway-config CONFIG --rollback [--discard-v2-writes]
      until migrate -finish-rollout: both services back on the previous release

After a failure the instance stays as it stopped; read storage-cutover.json,
failure-*.json and the command record it names, then --resume or --rollback.
Never retry a failed step blindly.
"""


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__, epilog=HELP, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('--owner-config', required=True, help="the owner's private deployment configuration")
    parser.add_argument('--gateway-config', required=True, help='the private configuration with the gateway service')
    parser.add_argument('--release-dir', help='verified publication of the storage v2 image')
    parser.add_argument('--state-dir', help='private directory for this cutover; defaults under state_root')
    parser.add_argument('--plan', action='store_true', help='print the phases without contacting or changing services')
    parser.add_argument('--preflight', action='store_true', help='run every read-only check and change nothing')
    parser.add_argument('--resume', action='store_true', help='continue the newest cutover from its recorded phase')
    parser.add_argument('--rollback', action='store_true', help='return both services to the previous release')
    parser.add_argument('--discard-v2-writes', action='store_true', help='with --rollback after serving: keep nothing saved since')
    parser.add_argument('--timeout', type=int, default=1800, help='maximum seconds for each wait or in-cluster command')
    args = parser.parse_args(argv)
    if sum((args.plan, args.preflight, args.resume, args.rollback)) > 1:
        parser.error('--plan, --preflight, --resume and --rollback are mutually exclusive')
    if args.discard_v2_writes and not args.rollback:
        parser.error('--discard-v2-writes applies only to --rollback')
    if not 1 <= args.timeout <= 7200:
        parser.error('--timeout must be between 1 and 7200 seconds')
    os.umask(0o077)
    cloud = receipt = None
    signal.signal(signal.SIGTERM, lambda signum, frame: (_ for _ in ()).throw(KeyboardInterrupt(signal.Signals(signum).name)))
    try:
        config = configuration(args.owner_config, args.gateway_config)
        root = deploy.state_root(config, args.owner_config)
        stamp = dt.datetime.now(dt.timezone.utc).strftime('%Y%m%d-%H%M%S')
        if args.state_dir:
            directory = Path(args.state_dir).expanduser().resolve()
        elif args.resume or args.rollback:
            found = sorted(root.glob('deploy-storage-v2-*/storage-cutover.json'),
                           key=lambda path: json.loads(path.read_text()).get('started_at', ''), reverse=True)
            if not found:
                raise ValueError('No storage cutover under ' + str(root) + '; pass --state-dir')
            directory = found[0].parent
        else:
            directory = root / (('preflight-storage-v2-' if args.preflight else 'deploy-storage-v2-') + stamp)
        candidate = deploy.release_record(args.release_dir) if args.release_dir else None
        if args.plan:
            if candidate is None:
                raise ValueError('--plan needs --release-dir')
            print(json.dumps({'state_dir': str(directory), 'image': candidate['reference'], 'services': config['services'],
                              'database_instance': config['database_instance'], 'phases': PHASES,
                              'storage_commands': ['migrate', 'admin storage import', 'verify', 'export', 'check',
                                                   'migrate -finish-rollout', 'admin storage retire -yes']}, indent=2))
            return 0
        directory.mkdir(parents=True, exist_ok=True, mode=0o700)
        directory.chmod(0o700)
        cloud = Runtime(config, directory, args.timeout)
        configs = config['storage_v2']['sidecars']
        if args.preflight:
            if candidate is None:
                raise ValueError('--preflight needs --release-dir')
            cloud.run_id = 'preflight-' + uuid.uuid4().hex
            problems = cloud.check(candidate, configs)[0]
            if problems:
                raise ValueError('Preflight refused the cutover: ' + '; '.join(problems))
            print('Preflight passed. The derived services are in ' + str(directory) + ' for review.')
            return 0
        lock = Path.home() / '.cache/forgetmenote/deployments/locks' / fingerprint(
            {key: config[key] for key in ('duplo_host', 'duplo_tenant', 'name')})
        with release_lock(lock):
            state = directory / 'storage-cutover.json'
            if state.exists():
                receipt = json.loads(state.read_text())
                if receipt['config_fingerprint'] != fingerprint(config):
                    raise ValueError('Configuration changed; use the original configuration for this cutover')
                cloud.run_id = receipt['id']
                if args.rollback:
                    rollback(cloud, receipt, args.discard_v2_writes)
                    print('Rolled back: the owner and the gateways run ' + receipt['previous_image'] + ' again.')
                    return 0
                if not args.resume:
                    raise ValueError('Existing cutover: inspect its diagnostics, then use --resume or --rollback')
            else:
                if args.resume or args.rollback:
                    raise ValueError('Resume and rollback require an existing cutover')
                if candidate is None:
                    raise ValueError('A cutover needs --release-dir')
                receipt = {'id': 'storage-cutover-' + uuid.uuid4().hex, 'image': candidate['reference'],
                           'source_revision': candidate['source_revision'], 'volume': config['volume'], 'phase': 'prepared',
                           'config_fingerprint': fingerprint(config), 'started_at': cloud.now().isoformat(),
                           'owner_fingerprint': fingerprint(deploy.configuration(args.owner_config)),
                           'sidecar_configs': configs, 'paused': True}
                cloud.run_id = receipt['id']
                problems, owner, gateway, previous, sidecars = cloud.check(candidate, configs)
                if problems:
                    raise ValueError('Preflight refused the cutover: ' + '; '.join(problems))
                cloud.check_previous_archive({'registry_digest': previous.rsplit('@', 1)[1]})
                receipt.update(previous_image=previous, owner_before=owner, gateway_before=gateway, sidecars=sidecars,
                               image_archive=cloud.protect_image(candidate))
                cloud.protect_release(Path(args.release_dir), ROOT, receipt)
                cloud.protect('owner-before.json', owner)
                cloud.protect('gateway-before.json', gateway)
                cloud.save('storage-cutover.json', receipt)
            print('State: ' + str(directory))
            cutover(cloud, receipt)
            print('Complete: every notebook is in the database; the exporter keeps the exports and runs the backups.')
    except (Exception, KeyboardInterrupt) as error:
        if cloud is not None:
            path = cloud.save('failure-' + uuid.uuid4().hex + '.json', {'error': str(error), 'time': cloud.now().isoformat(),
                              'phase': receipt.get('phase') if receipt else None})
            print('Storage cutover stopped: ' + str(error) + '\nDiagnostic: ' + str(path), file=sys.stderr)
        else:
            print('Storage cutover stopped: ' + str(error), file=sys.stderr)
        return 1
    return 0


if __name__ == '__main__':
    sys.exit(main())
