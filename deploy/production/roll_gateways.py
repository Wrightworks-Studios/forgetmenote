#!/usr/bin/env python3
"""Roll a Postgres instance's gateways onto a verified release, one replica at a time.

Gateways keep no state of their own, so a rollout is an image update that Duplo
applies replica by replica while the others serve. Each new replica applies its
image's migrations before serving (its command is `forgetmenote migrate && exec
forgetmenote`), and every migration works with both the running and the next
binary, so old replicas keep serving meanwhile and a rollback is the same update
back to the previous image (docs/postgres.md#migrations). Deploy owners first,
with deploy_instance.py: gateways may call owner methods older owners lack.

Before the update the tool records the database's latest restorable time, the
point to restore to should a migration ever need undoing, and protects that
receipt and the image in the recovery bucket.
"""
import argparse
import datetime as dt
import json
import os
from pathlib import Path
import signal
import sys
import urllib.error
import urllib.request
import uuid

import cutover
import deploy_instance as deploy
from verification import fingerprint, release_lock


ROOT = Path(__file__).resolve().parents[2]


def rollout(cloud, receipt):
    """Update the gateways to receipt['image'] and wait until every replica serves it."""
    if receipt['phase'] == 'prepared':
        cloud.update_image(receipt['image'])
        receipt['phase'] = 'updating'
        cloud.save('rollout.json', receipt)
    if receipt['phase'] == 'updating':
        cloud.wait_rolled(receipt['image'], receipt['containers'])
        completed = {**receipt, 'phase': 'ready', 'ready_at': cloud.now().isoformat(),
                     'database_after': cloud.restore_point()}
        cloud.protect('rollout-result.json', completed)
        receipt.update(completed)
        cloud.save('rollout.json', receipt)


def rollback(cloud, receipt):
    """Return the gateways to the image they ran before this rollout."""
    cloud.update_image(receipt['previous_image'])
    cloud.wait_rolled(receipt['previous_image'], receipt['containers'])
    receipt['rolled_back_at'] = cloud.now().isoformat()
    cloud.save('rollout.json', receipt)
    cloud.protect('rollout-rollback.json', receipt)


class Runtime(cutover.Runtime):
    """The gateway service, its pods, the database and the recovery bucket."""

    def gateway(self):
        return self.command(self.duplo + ['service', 'find', self.service_config('gateway')['name']])

    def update_image(self, image):
        self.command(self.duplo + ['service', 'update_image', self.service_config('gateway')['name'], image])

    def wait_rolled(self, image, containers):
        """Every replica runs image in each application container, ready and never
        restarted, and the public origin answers both health endpoints."""
        count = self.service_config('gateway').get('replicas', 1)
        def rolled():
            pods = self.role_pods('gateway')
            if len(pods) != count or any(pod['metadata'].get('deletionTimestamp') for pod in pods):
                return False
            for pod in pods:
                images = {c['name']: c['image'] for c in pod['spec']['containers']}
                statuses = pod.get('status', {}).get('containerStatuses', [])
                if (any(images.get(name) != image for name in containers) or len(statuses) != len(images) or
                        not all(s.get('ready') and s.get('restartCount', 0) == 0 for s in statuses)):
                    return False
            return all(self.answers(endpoint) for endpoint in ('healthz', 'readyz'))
        self.wait_for(rolled, 'Gateways did not all serve ' + image + ' before the deadline')

    def answers(self, endpoint):
        class NoRedirect(urllib.request.HTTPRedirectHandler):
            def redirect_request(self, *args, **kwargs):
                return None
        try:
            with urllib.request.build_opener(NoRedirect()).open(self.config['public_url'] + '/' + endpoint, timeout=30) as response:
                status, body = response.status, response.read(65536).decode(errors='replace')
        except urllib.error.HTTPError as error:
            with error:
                status, body = error.code, error.read(65536).decode(errors='replace')
        except OSError as error:
            status, body = None, str(error)
        self.save(endpoint + '.json', {'status': status, 'body': body})
        return status == 200

    def check(self, candidate):
        """Every read-only check of a rollout, reporting the problems together."""
        problems = []
        identity = self.command(self.aws + ['sts', 'get-caller-identity'])
        if identity['Account'] != self.config['account']:
            problems.append('AWS identity is in the wrong account')
        service = self.gateway()
        self.save('service-before.json', service)
        images = {c.get('Image') for c in service.get('Template', {}).get('Containers', [])}
        if len(images) != 1:
            problems.append('the gateway service must run one image')
        previous = next(iter(images), None)
        if previous == candidate['reference']:
            problems.append('the gateways already run the candidate')
        if 'forgetmenote migrate' not in json.dumps(service.get('Template', {})):
            problems.append('gateways must start with `forgetmenote migrate && exec forgetmenote`, so a new image migrates before it serves')
        values = cutover.environment(service)
        if values.get('PUBLIC_URL') != self.config['public_url']:
            problems.append('gateways must serve the public origin ' + self.config['public_url'])
        if values.get('OUTGOING') not in (None, '', 'enabled'):
            problems.append('serving gateways must not pause outgoing actions')
        if values.get('OWNER_MAP_FILE') and deploy.storage_v2(ROOT, candidate['source_revision']) is not False:
            problems.append('the gateways reach notebooks on an owner, and this release ' + deploy.STORAGE_CUTOVER)
        count = self.service_config('gateway').get('replicas', 1)
        if service.get('Replicas') != count or service.get('HPASpecs'):
            problems.append(f'the gateway service must have exactly {count} static replicas')
        pods = [pod for pod in self.role_pods('gateway') if not pod['metadata'].get('deletionTimestamp')]
        containers = sorted({c['name'] for pod in pods for c in pod['spec']['containers'] if c['image'] == previous})
        statuses = [s for pod in pods for s in pod.get('status', {}).get('containerStatuses', [])]
        if len(pods) != count or not containers or not statuses or not all(s.get('ready') for s in statuses):
            problems.append(f'expected {count} ready gateways running the current image')
        database = self.command(self.aws + ['rds', 'describe-db-instances', '--db-instance-identifier',
                                            self.config['database_instance']])['DBInstances'][0]
        if database.get('DBInstanceStatus') != 'available' or not database.get('BackupRetentionPeriod'):
            problems.append('the database must be available with automated backups')
        if problems:
            raise ValueError('Preflight refused the rollout: ' + '; '.join(problems))
        _, digest = deploy.locate_package(candidate['image'].rsplit(':', 1)[0], 'main-' + candidate['source_revision'],
                                          self.config.get('aws_profile'))
        if digest != candidate['registry_digest']:
            raise RuntimeError('Private registry image no longer matches its verified publication')
        return service, previous, containers

    def prepare(self, receipt, candidate):
        """The checks, then the restore point and protected assets; nothing about
        the gateways changes here."""
        service, previous, containers = self.check(candidate)
        receipt.update(previous_image=previous, containers=containers, database_before=self.restore_point(),
                       image_archive=self.protect_image(candidate))
        self.protect('service-before.json', service)
        self.protect('prepared.json', receipt)
        self.save('rollout.json', receipt)


HELP = """
  roll_gateways.py --config CONFIG --release-dir DIR --plan
      local only: the service, image and steps
  roll_gateways.py --config CONFIG --release-dir DIR --preflight
      read-only: every check a rollout makes
  roll_gateways.py --config CONFIG --release-dir DIR
      preflight, restore point, then the rolling image update
  roll_gateways.py --config CONFIG --resume
      keep waiting on the newest rollout
  roll_gateways.py --config CONFIG --rollback
      update the newest rollout's gateways back to their previous image

CONFIG is the new stack's cutover configuration with rehearsal false. After every
gateway runs a release that raises the reader version, run `forgetmenote migrate
-finish-rollout` once. Read rollout.json, failure-*.json and the command record
they name before resuming or rolling back.
"""


def main():
    parser = argparse.ArgumentParser(description=__doc__, epilog=HELP, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('--config', required=True, help="private configuration of the new stack's services")
    parser.add_argument('--release-dir', help='verified publication of the image to roll out')
    parser.add_argument('--state-dir', help='private directory for this rollout; defaults under state_root')
    parser.add_argument('--plan', action='store_true', help='print the steps without contacting or changing services')
    parser.add_argument('--preflight', action='store_true', help='run every read-only check of a rollout and change nothing')
    parser.add_argument('--resume', action='store_true', help='continue the newest rollout')
    parser.add_argument('--rollback', action='store_true', help='return the newest rollout to its previous image')
    parser.add_argument('--timeout', type=int, default=900, help='maximum seconds for the rollout to become ready')
    args = parser.parse_args()
    if sum((args.plan, args.preflight, args.resume, args.rollback)) > 1:
        parser.error('--plan, --preflight, --resume and --rollback are mutually exclusive')
    if not 1 <= args.timeout <= 7200:
        parser.error('--timeout must be between 1 and 7200 seconds')
    os.umask(0o077)
    cloud = receipt = None
    signal.signal(signal.SIGTERM, lambda signum, frame: (_ for _ in ()).throw(KeyboardInterrupt(signal.Signals(signum).name)))
    try:
        config = cutover.configuration(args.config)
        if config.get('rehearsal'):
            raise ValueError('A rehearsal configuration has no serving gateways to roll; use the production configuration')
        root = deploy.state_root(config, args.config)
        if args.state_dir:
            directory = Path(args.state_dir).expanduser().resolve()
        elif args.resume or args.rollback:
            found = sorted(root.glob('gateways-*/rollout.json'), key=lambda path: path.stat().st_mtime, reverse=True)
            if not found:
                raise ValueError('No gateway rollouts under ' + str(root) + '; pass --state-dir')
            directory = found[0].parent
        elif args.preflight:
            directory = root / ('preflight-gateways-' + dt.datetime.now(dt.timezone.utc).strftime('%Y%m%d-%H%M%S'))
        else:
            directory = root / ('gateways-' + dt.datetime.now(dt.timezone.utc).strftime('%Y%m%d-%H%M%S'))
        candidate = deploy.release_record(args.release_dir) if args.release_dir else None
        if args.plan:
            if candidate is None:
                raise ValueError('--plan needs --release-dir')
            print(json.dumps({'state_dir': str(directory), 'image': candidate['reference'],
                              'gateway_service': config['services']['gateway'], 'database_instance': config['database_instance'],
                              'steps': ['preflight', 'restore point and protected image', 'rolling image update',
                                        'every replica ready on the image', 'public healthz and readyz']}, indent=2))
            return 0
        directory.mkdir(parents=True, exist_ok=True, mode=0o700)
        directory.chmod(0o700)
        cloud = Runtime(config, directory, args.timeout)
        if args.preflight:
            if candidate is None:
                raise ValueError('--preflight needs --release-dir')
            cloud.check(candidate)
            print('Preflight passed: the gateways can roll to ' + candidate['reference'] + '.')
            return 0
        lock = Path.home() / '.cache/forgetmenote/deployments/locks' / fingerprint(
            {key: config[key] for key in ('duplo_host', 'duplo_tenant')} | {'name': config['services']['gateway']['name']})
        with release_lock(lock):
            state = directory / 'rollout.json'
            if state.exists():
                receipt = json.loads(state.read_text())
                if receipt['config_fingerprint'] != fingerprint(config):
                    raise ValueError('Configuration changed; use the original configuration for this rollout')
                cloud.run_id = receipt['id']
                if args.rollback:
                    rollback(cloud, receipt)
                    print('Rolled back: the gateways serve ' + receipt['previous_image'] + ' again.')
                    return 0
                if not args.resume:
                    raise ValueError('Existing rollout: inspect its diagnostics, then use --resume or --rollback')
            else:
                if args.resume or args.rollback:
                    raise ValueError('Resume and rollback require an existing rollout')
                if candidate is None:
                    raise ValueError('A rollout needs --release-dir')
                receipt = {'id': 'gateways-' + uuid.uuid4().hex, 'image': candidate['reference'],
                           'source_revision': candidate['source_revision'], 'phase': 'prepared',
                           'config_fingerprint': fingerprint(config), 'started_at': cloud.now().isoformat()}
                cloud.run_id = receipt['id']
                cloud.prepare(receipt, candidate)
            print('State: ' + str(directory))
            rollout(cloud, receipt)
            print('Serving: every gateway runs ' + receipt['image'] + '.')
    except (Exception, KeyboardInterrupt) as error:
        if cloud is not None:
            path = cloud.save('failure-' + uuid.uuid4().hex + '.json', {'error': str(error), 'time': cloud.now().isoformat(),
                              'phase': receipt.get('phase') if receipt else None})
            print('Rollout stopped: ' + str(error) + '\nDiagnostic: ' + str(path), file=sys.stderr)
        else:
            print('Rollout stopped: ' + str(error), file=sys.stderr)
        return 1
    return 0


if __name__ == '__main__':
    sys.exit(main())
