#!/usr/bin/env python3
"""Hand a Postgres instance's state back to the file-based release.

The current image serves a disposable volume against Postgres. After it stops,
export-files writes the database's state into the volume in the file-based
release's formats, and the file-based release image serves that volume: the
same accounts sign in, the same notebooks and notes are there, and the admin
token is unchanged. ROLLBACK_IMAGE names that release; it must already be
present locally, since this script never pulls an image.
"""
import http.cookiejar
import json
import os
import secrets
import socket
import subprocess
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid

from smoke_database import Database

RELEASE_A = 'ghcr.io/pleish/forgetmenote:main-0a043d7bd14e512d970f43a126c05b6ad1f822f4'


def run(*args):
    return subprocess.check_output(args, text=True, stderr=subprocess.STDOUT).strip()


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None


def main():
    image = os.environ.get('SMOKE_IMAGE', 'forgetmenote:poc')
    release_a = os.environ.get('ROLLBACK_IMAGE', RELEASE_A)
    if subprocess.run(['docker', 'image', 'inspect', release_a], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode != 0:
        raise SystemExit(f'{release_a} is not present locally; pull it once with operator approval, then rerun')
    name = 'forgetmenote-rollback-' + uuid.uuid4().hex[:10]
    volume = name + '-data'
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0))
        port = sock.getsockname()[1]
    origin = f'http://localhost:{port}'
    hardened = ['--init', '--read-only', '--tmpfs', '/tmp:size=64m', '--cap-drop', 'ALL', '--security-opt', 'no-new-privileges:true']
    run('docker', 'volume', 'create', volume)
    db = Database()

    def serve(container_image, *extra):
        run('docker', 'run', '-d', '--name', name, *hardened, '-p', f'127.0.0.1:{port}:8080', '-e', f'PUBLIC_URL={origin}', '-v', f'{volume}:/data', *extra, container_image)
        for _ in range(300):
            try:
                with urllib.request.urlopen(origin + '/healthz', timeout=1) as response:
                    if response.status == 200:
                        return
            except (OSError, urllib.error.URLError):
                time.sleep(0.1)
        raise RuntimeError(container_image + ' failed to become ready:\n' + run('docker', 'logs', name))

    def browser():
        return urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()), NoRedirect())

    def call(opener, path, body=None, token=None, form=False):
        headers = {'Origin': origin, 'Content-Type': 'application/x-www-form-urlencoded' if form else 'application/json'}
        if token:
            headers['Authorization'] = 'Bearer ' + token
        data = None if body is None else (urllib.parse.urlencode(body) if form else json.dumps(body)).encode()
        try:
            response = opener.open(urllib.request.Request(origin + path, data, headers), timeout=30)
        except urllib.error.HTTPError as error:
            response = error
        return response.code, response.read().decode()

    try:
        db.migrate(image)
        serve(image, *db.args())
        token = run('docker', 'exec', name, 'forgetmenote', 'token')
        admin = browser()
        assert call(admin, '/login', {'token': token}, form=True)[0] in (200, 303)
        admin_password = secrets.token_urlsafe(24)
        status, body = call(admin, '/account/setup', {'username': 'first-admin', 'password': admin_password})
        assert status == 200, body
        first_admin = json.loads(body)['id']
        status, body = call(admin, '/p/default/api/note', {'path': 'notes/rollback.md', 'content': '# Written before the rollback\n', 'revision': ''}, token)
        assert status == 200, body
        alice_password = secrets.token_urlsafe(24)
        status, body = call(admin, '/api/local-users', {'username': 'alice', 'password': alice_password}, token)
        assert status == 200, body
        alice_id = json.loads(body)['id']
        assert call(admin, '/api/users', {'id': alice_id, 'canCreateProjects': True, 'displayName': 'Alice Example'}, token)[0] == 200
        alice = browser()
        assert call(alice, '/auth/password/login', {'username': 'alice', 'password': alice_password}, form=True)[0] == 303
        assert call(alice, '/api/projects', {'id': 'private-alice', 'name': 'Private Alice'})[0] == 200
        assert call(alice, '/p/private-alice/api/note', {'path': 'private.md', 'content': '# Alice before the rollback\n', 'revision': ''})[0] == 200
        run('docker', 'stop', '-t', '30', name)
        run('docker', 'rm', name)

        exported = json.loads(run('docker', 'run', '--rm', '-v', f'{volume}:/data', *db.args(), image, 'export-files', '-output', '/data/rollback-export').splitlines()[-1])
        areas = {area['area']: area for area in exported['exported']}
        assert areas['catalog']['files'] > 0 and areas['sign-in']['files'] > 0, exported
        # No file-based state remains in a volume that only served Postgres, so
        # the export is copied over it without replacing anything.
        run('docker', 'run', '--rm', '-v', f'{volume}:/data', '--entrypoint', '/bin/sh', image, '-c',
            'cd /data/rollback-export && find . -type f | while read -r f; do [ ! -e "/data/$f" ] || { echo "$f exists"; exit 1; }; done && cp -a . /data/ && rm -rf /data/rollback-export')
        print('PASS: export-files wrote', ', '.join(f"{area} ({sum['files']})" for area, sum in areas.items() if sum['files']))

        serve(release_a)
        assert run('docker', 'exec', name, 'forgetmenote', 'token') == token, 'the admin token changed'
        returning = browser()
        assert call(returning, '/auth/password/login', {'username': 'first-admin', 'password': admin_password}, form=True)[0] == 303
        status, body = call(returning, '/api/me')
        assert status == 200 and json.loads(body)['id'] == first_admin and json.loads(body)['admin'], body
        status, body = call(returning, '/p/default/api/note?path=notes/rollback.md')
        assert status == 200 and json.loads(body)['content'] == '# Written before the rollback\n', body
        alice = browser()
        assert call(alice, '/auth/password/login', {'username': 'alice', 'password': alice_password}, form=True)[0] == 303
        status, body = call(alice, '/p/private-alice/api/note?path=private.md')
        assert status == 200 and json.loads(body)['content'] == '# Alice before the rollback\n', body
        assert call(alice, '/p/default/api/notes')[0] == 403, 'the rollback widened access'
        status, body = call(alice, '/p/private-alice/api/note', {'path': 'after.md', 'content': '# Written after the rollback\n', 'revision': ''})
        assert status == 200, body
        print('PASS: the file-based release serves the export: accounts, notebooks, notes, permissions and writes')
    finally:
        subprocess.run(['docker', 'rm', '-f', name], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        subprocess.run(['docker', 'volume', 'rm', volume], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        db.close()


if __name__ == '__main__':
    main()
