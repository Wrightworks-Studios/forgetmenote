#!/usr/bin/env python3
"""The cutover tool moves a file-based release's volume onto the new stack.

The file-based release (ROLLBACK_IMAGE, release A by default) serves a
disposable volume with accounts, a private notebook and notes, then stops.
deploy/production/cutover.py's own phases then run against containers in place
of Duplo services: a helper of the current image (SMOKE_IMAGE) mounts that
volume read-only, imports it into Postgres and an S3 stand-in, checks the
import against an export, copies the notebooks to an owner and places them
there, and two gateways serve the result. The stopped volume is unchanged, and
the file-based release serves it again after the tool's rollback. Like
smoke-rollback it never pulls an image.
"""
import json
import os
from pathlib import Path
import secrets
import shutil
import subprocess
import sys
import tempfile
import time
import uuid

from cluster_smoke import BUCKET, ORIGIN, REGION, Client, free_port
from rollback_smoke import RELEASE_A
from smoke_database import Database

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / 'deploy' / 'production'))
import cutover  # noqa: E402

HARDENED = ['--init', '--read-only', '--tmpfs', '/tmp:size=64m', '--cap-drop', 'ALL', '--security-opt', 'no-new-privileges:true']


def run(*args):
    return subprocess.check_output(args, text=True, stderr=subprocess.STDOUT).strip()


def wait_healthy(port, container):
    for _ in range(300):
        try:
            if Client().request(port, '/healthz')[0] == 200:
                return
        except OSError:
            pass
        time.sleep(0.1)
    raise RuntimeError(container + ' failed to become ready:\n' + subprocess.run(['docker', 'logs', container], text=True, capture_output=True).stderr)


class Containers:
    """cutover.py's cloud boundary over Docker: services are containers, pods
    are their names, and the stopped backup is the untouched volume itself."""

    def __init__(self, name, image, release_a, source, db, files, aws):
        self.name, self.image, self.release_a, self.source, self.db, self.files, self.aws = name, image, release_a, source, db, files, aws
        self.directory = Path(tempfile.mkdtemp(prefix='fmn-cutover-state-'))
        self.config = {'owner_entry': 'pool'}
        self.volumes, self.ports, self.started = [], {}, []

    def volume(self, label):
        self.volumes.append(f'{self.name}-{label}')
        run('docker', 'volume', 'create', self.volumes[-1])
        return self.volumes[-1]

    def now(self):
        import datetime
        return datetime.datetime.now(datetime.timezone.utc)

    def save(self, name, value):
        (self.directory / name).write_text(json.dumps(value, indent=2))
        return self.directory / name

    def protect(self, name, value):
        self.save('protected-' + name, value)

    def restore_point(self):
        return {'instance': self.db.name}

    def container(self, role, *args, port=None):
        name = f'{self.name}-{role}'
        published = ['-p', f'127.0.0.1:{port}:8080'] if port else []
        run('docker', 'run', '-d', '--name', name, *HARDENED, *published, *args)
        self.started.append(name)
        return name

    def running(self, role):
        name = f'{self.name}-{role}'
        if subprocess.run(['docker', 'inspect', name], capture_output=True).returncode == 0:
            return name
        shared = ['-v', f'{self.files}:/run/cluster:ro', *self.aws]
        if role == 'helper':
            self.container(role, *self.db.args(), '-v', f'{self.source}:{cutover.SOURCE}:ro', *shared,
                           '-e', 'OWNER_MAP_FILE=/run/cluster/owners.json', '-e', 'BLOBS_CONFIG_FILE=/run/cluster/blobs.json',
                           '--entrypoint', 'sleep', self.image, 'infinity')
        elif role == 'owner':
            self.container(role, '--network', self.db.network, '-v', f'{self.volume("owner")}:/data', *shared, '-e', 'OWNER_SECRET_FILE=/run/cluster/owner.token',
                           '-e', 'OWNER_ALLOW_HTTP=true', self.image, 'owner')
            for _ in range(300):
                if subprocess.run(['docker', 'exec', name, 'forgetmenote', 'healthz', '-url', 'http://127.0.0.1:8080/readyz'], capture_output=True).returncode == 0:
                    break
                time.sleep(0.1)
            else:
                raise RuntimeError('owner failed to become ready')
        return name

    def exec(self, pod, args):
        result = subprocess.run(['docker', 'exec', pod, *args], text=True, capture_output=True)
        if result.returncode:
            raise RuntimeError(f'{" ".join(args)} failed in {pod}: {result.stdout}{result.stderr}')
        return result.stdout

    def copy(self, source_pod, source_root, target_pod, target_root):
        reader = subprocess.Popen(['docker', 'exec', source_pod, 'tar', '-C', source_root, '-cf', '-', 'projects'], stdout=subprocess.PIPE)
        writer = subprocess.run(['docker', 'exec', '-i', target_pod, 'tar', '-C', target_root, '-xf', '-'], stdin=reader.stdout)
        reader.stdout.close()
        if reader.wait() or writer.returncode:
            raise RuntimeError('copying the notebooks failed')

    def stop_service(self, role):
        subprocess.run(['docker', 'rm', '-f', f'{self.name}-{role}'], capture_output=True)
        if role == 'gateway':
            for label in ('gateway-a', 'gateway-b'):
                subprocess.run(['docker', 'rm', '-f', f'{self.name}-{label}'], capture_output=True)

    def start_service(self, role):
        assert role == 'gateway'
        for label in ('gateway-a', 'gateway-b'):
            self.ports[label] = free_port()
            self.container(label, *self.db.args(), '-v', f'{self.volume(label)}:/data', '-v', f'{self.files}/admin.token:/data/admin.token:ro',
                           '-v', f'{self.files}:/run/cluster:ro', *self.aws, '-e', 'PUBLIC_URL=' + ORIGIN,
                           '-e', 'OWNER_MAP_FILE=/run/cluster/owners.json', '-e', 'BLOBS_CONFIG_FILE=/run/cluster/blobs.json',
                           self.image, port=self.ports[label])
            wait_healthy(self.ports[label], f'{self.name}-{label}')

    def ready(self, role):
        return [f'{self.name}-gateway-a', f'{self.name}-gateway-b']

    def start(self):
        self.ports['release-a'] = free_port()
        run('docker', 'run', '-d', '--name', f'{self.name}-release-a', *HARDENED, '-p', f'127.0.0.1:{self.ports["release-a"]}:8080',
            '-e', 'PUBLIC_URL=' + ORIGIN, '-v', f'{self.source}:/data', self.release_a)
        self.started.append(f'{self.name}-release-a')

    def wait_running(self):
        wait_healthy(self.ports['release-a'], f'{self.name}-release-a')

    def close(self):
        for name in self.started:
            subprocess.run(['docker', 'rm', '-f', name], capture_output=True)
        for volume in self.volumes:
            subprocess.run(['docker', 'volume', 'rm', volume], capture_output=True)
        shutil.rmtree(self.directory, ignore_errors=True)


def volume_digest(image, volume):
    """Every file under a volume, by path, mode and content."""
    return run('docker', 'run', '--rm', '-v', f'{volume}:/v:ro', '--entrypoint', 'sh', image, '-c',
               'cd /v && find . -print0 | LC_ALL=C sort -z | xargs -0 stat -c "%n %a %s %Y" | sha256sum && '
               'find . -type f -print0 | LC_ALL=C sort -z | xargs -0 sha256sum | sha256sum')


def main():
    image = os.environ.get('SMOKE_IMAGE', 'forgetmenote:poc')
    release_a = os.environ.get('ROLLBACK_IMAGE', RELEASE_A)
    if subprocess.run(['docker', 'image', 'inspect', release_a], capture_output=True).returncode != 0:
        raise SystemExit(f'{release_a} is not present locally; pull it once with operator approval, then rerun')
    name = 'forgetmenote-cutover-' + uuid.uuid4().hex[:10]
    files = Path(tempfile.mkdtemp(prefix='fmn-cutover-'))
    # Containers run as another user; every value here is synthetic and disposable.
    files.chmod(0o755)
    access_key, secret_key = 'AKIASMOKE' + secrets.token_hex(6).upper(), secrets.token_urlsafe(30)
    for file, content in {
        'admin.token': secrets.token_urlsafe(32) + '\n',
        'owner.token': secrets.token_urlsafe(32) + '\n',
        'owners.json': json.dumps({'pool': {'url': f'http://{name}-owner:8080', 'secretFile': '/run/cluster/owner.token', 'allowHTTP': True, 'placeNew': True}}),
        'blobs.json': json.dumps({'s3': {'bucket': BUCKET, 'prefix': 'instance', 'region': REGION, 'endpoint': f'http://{name}-s3:9000', 'allowHTTP': True}}),
    }.items():
        (files / file).write_text(content)
        (files / file).chmod(0o644)
    aws = ['-e', 'AWS_ACCESS_KEY_ID=' + access_key, '-e', 'AWS_SECRET_ACCESS_KEY=' + secret_key, '-e', 'AWS_EC2_METADATA_DISABLED=true']
    db = Database()
    cloud = None
    try:
        cloud = Containers(name, image, release_a, None, db, files, aws)
        cloud.source = cloud.volume('release-a')
        run('docker', 'run', '-d', '--name', f'{name}-s3', '--network', db.network, '--read-only', '--cap-drop', 'ALL',
            '-v', f'{Path(__file__).resolve().parent / "smoke_s3.py"}:/opt/smoke_s3.py:ro',
            '-e', 'SMOKE_S3_ACCESS_KEY=' + access_key, '-e', 'SMOKE_S3_SECRET_KEY=' + secret_key,
            '--entrypoint', 'python3', image, '-I', '/opt/smoke_s3.py', '9000', BUCKET, REGION)
        cloud.started.append(f'{name}-s3')

        # The file-based release's own state, written by that release.
        cloud.start()
        cloud.wait_running()
        port = cloud.ports['release-a']
        recovery = run('docker', 'exec', f'{name}-release-a', 'forgetmenote', 'token')
        admin = Client()
        assert admin.request(port, '/login', {'token': recovery}, form=True)[0] in (200, 303)
        password = secrets.token_urlsafe(24)
        status, _, body = admin.request(port, '/account/setup', {'username': 'first-admin', 'password': password})
        assert status == 200, body
        admin_id = json.loads(body)['id']
        assert admin.request(port, '/p/default/api/note', {'path': 'notes/before.md', 'content': '# Written by release A\n', 'revision': ''})[0] == 200
        alice_password = secrets.token_urlsafe(24)
        bearer = {'Authorization': 'Bearer ' + recovery}
        status, _, body = admin.request(port, '/api/local-users', {'username': 'alice', 'password': alice_password}, headers=bearer)
        assert status == 200, body
        alice_id = json.loads(body)['id']
        assert admin.request(port, '/api/users', {'id': alice_id, 'canCreateProjects': True}, headers=bearer)[0] == 200
        alice = Client()
        assert alice.request(port, '/auth/password/login', {'username': 'alice', 'password': alice_password}, form=True)[0] == 303
        assert alice.request(port, '/api/projects', {'id': 'private-alice', 'name': 'Private Alice'})[0] == 200
        assert alice.request(port, '/p/private-alice/api/note', {'path': 'mine.md', 'content': '# Alice before the cutover\n', 'revision': ''})[0] == 200
        run('docker', 'stop', '-t', '30', f'{name}-release-a')
        run('docker', 'rm', f'{name}-release-a')
        cloud.started.remove(f'{name}-release-a')
        before = volume_digest(image, cloud.source)

        # The tool's own phases, from the stopped volume to serving gateways.
        db.migrate(image)
        receipt = {'id': 'cutover-smoke', 'rehearsal': True, 'phase': 'prepared'}
        cutover.cutover(cloud, receipt)
        assert receipt['phase'] == 'serving' and not cutover.report_mismatches(receipt['imported'], receipt['exported']), receipt
        assert {'default', 'private-alice'} <= set(receipt['placed']['notebooks']), receipt['placed']
        assert receipt['notebook_files']['files'] > 0, receipt['notebook_files']
        print('PASS: cutover.py imported', ', '.join(f"{area['area']} ({area['files']})" for area in receipt['imported'] if area['files']),
              f"and copied {receipt['notebook_files']['files']} notebook files, matching their digest", flush=True)

        a, b = cloud.ports['gateway-a'], cloud.ports['gateway-b']
        returning = Client()
        assert returning.request(a, '/auth/password/login', {'username': 'first-admin', 'password': password}, form=True)[0] == 303
        status, _, body = returning.request(b, '/api/me')
        assert status == 200 and json.loads(body)['id'] == admin_id and json.loads(body)['admin'], body
        status, _, body = returning.request(b, '/p/default/api/note?path=notes/before.md')
        assert status == 200 and json.loads(body)['content'] == '# Written by release A\n', body
        alice = Client()
        assert alice.request(b, '/auth/password/login', {'username': 'alice', 'password': alice_password}, form=True)[0] == 303
        status, _, body = alice.request(a, '/p/private-alice/api/note?path=mine.md')
        assert status == 200 and json.loads(body)['content'] == '# Alice before the cutover\n', body
        assert alice.request(a, '/p/default/api/notes')[0] == 403, 'the cutover widened access'
        status, _, body = alice.request(b, '/p/private-alice/api/note', {'path': 'after.md', 'content': '# Alice after the cutover\n', 'revision': ''})
        assert status == 200, body
        run('docker', 'exec', f'{name}-owner', 'test', '-f', '/data/projects/private-alice/workspace/after.md')
        for label in ('gateway-a', 'gateway-b'):
            run('docker', 'exec', f'{name}-{label}', 'sh', '-c', 'test ! -e /data/projects')
        print('PASS: two gateways serve the imported accounts, permissions and notes from the owner, and accept writes', flush=True)

        assert volume_digest(image, cloud.source) == before, 'the cutover changed the stopped volume'
        cutover.rollback(cloud, {**receipt, 'rehearsal': False, 'writes_confirmed_absent': True})
        port = cloud.ports['release-a']
        back = Client()
        assert back.request(port, '/auth/password/login', {'username': 'first-admin', 'password': password}, form=True)[0] == 303
        status, _, body = back.request(port, '/p/default/api/note?path=notes/before.md')
        assert status == 200 and json.loads(body)['content'] == '# Written by release A\n', body
        print('PASS: the stopped volume is unchanged and release A serves it again after the rollback', flush=True)
    except BaseException:
        if cloud is not None:
            for container in cloud.started:
                logs = subprocess.run(['docker', 'logs', container], text=True, capture_output=True)
                print(f'--- last lines from {container}:\n' + '\n'.join((logs.stdout + logs.stderr).splitlines()[-30:]), flush=True)
        raise
    finally:
        if cloud is not None:
            cloud.close()
        db.close()
        shutil.rmtree(files, ignore_errors=True)


if __name__ == '__main__':
    main()
