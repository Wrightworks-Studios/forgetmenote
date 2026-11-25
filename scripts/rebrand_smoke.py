#!/usr/bin/env python3
"""Upgrade disposable Gitnotes data to ForgetMeNote without rewriting history.
Requires OLD_IMAGE (defaults gitnotes:local) and NEW_IMAGE (forgetmenote:local).
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

from smoke_database import Database


def run(*args):
    return subprocess.check_output(args, stderr=subprocess.STDOUT, text=True).strip()


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None


def main():
    name = 'forgetmenote-upgrade-' + secrets.token_hex(5)
    volume = name + '-data'
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0))
        port = sock.getsockname()[1]
    origin = 'http://localhost:' + str(port)
    browser = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()), NoRedirect())
    run('docker', 'volume', 'create', volume)
    db = Database()
    try:
        def start(image, database=()):
            run('docker', 'run', '-d', '--name', name, '--read-only', '--tmpfs', '/tmp', '--cap-drop', 'ALL', '-p', f'127.0.0.1:{port}:8080', '-e', 'PUBLIC_URL=' + origin, '-v', volume + ':/data', *database, image)
            for _ in range(100):
                try:
                    if urllib.request.urlopen(origin + '/healthz', timeout=1).status == 200:
                        return
                except OSError:
                    time.sleep(.1)
            raise RuntimeError('container not ready')
        start(os.environ.get('OLD_IMAGE', 'gitnotes:local'))
        token = run('docker', 'exec', name, 'gitnotes', 'token')
        def request(path, data=None, admin=False, form=False):
            headers = {'Content-Type': 'application/x-www-form-urlencoded' if form else 'application/json', 'Origin': origin}
            if admin:
                headers['Authorization'] = 'Bearer ' + token
            body = None if data is None else (urllib.parse.urlencode(data) if form else json.dumps(data)).encode()
            try:
                response = browser.open(urllib.request.Request(origin + path, body, headers), timeout=20)
            except urllib.error.HTTPError as error:
                response = error
            return response.code, response.read().decode()
        password = secrets.token_urlsafe(24)
        status, raw = request('/api/local-users', {'username': 'alice', 'password': password}, admin=True)
        assert status == 200
        user = json.loads(raw)['id']
        assert request('/api/members', {'project': 'default', 'user': user, 'role': 'owner'}, admin=True)[0] == 200
        assert request('/auth/password/login', {'username': 'alice', 'password': password}, form=True)[0] == 303
        status, account = request('/account')
        author = json.loads(account)['author']
        status, note = request('/p/default/api/note', {'path': 'notes/legacy.md', 'content': '# Existing note\n', 'revision': ''})
        assert status == 200
        status, state = request('/p/default/api/status')
        assert request('/p/default/api/locks', {'path': 'notes/legacy.md', 'locked': True, 'revision': json.loads(state)['revision']})[0] == 200
        history = request('/p/default/api/history?path=notes/legacy.md')[1]
        run('docker', 'rm', '-f', name)
        # The Gitnotes-era image predates Postgres; only ForgetMeNote gets the database.
        new_image = os.environ.get('NEW_IMAGE', 'forgetmenote:local')
        db.migrate(new_image)
        # The upgrade imports the Gitnotes-era files once, before ForgetMeNote serves.
        run('docker', 'run', '--rm', '-v', volume + ':/data', *db.args(), new_image, 'import-files')
        start(new_image, db.args())
        # The gitnotes session cookie is retired, so a Gitnotes-era browser signs in again.
        assert request('/api/me')[0] == 401, 'retired gitnotes session accepted'
        assert request('/auth/password/login', {'username': 'alice', 'password': password}, form=True)[0] == 303
        assert request('/api/me')[0] == 200, 'sign-in after upgrade rejected'
        assert 'ForgetMeNote' in request('/')[1], 'old branding'
        assert json.loads(request('/account')[1])['author'] == author, 'local Git identity changed'
        assert request('/p/default/api/history?path=notes/legacy.md')[1] == history, 'history changed'
        status, note = request('/p/default/api/note?path=notes/legacy.md')
        old = json.loads(note)
        assert old['content'] == '# Existing note\n'
        assert request('/p/default/api/note', {'path': old['path'], 'content': 'should stay locked', 'revision': old['revision']})[0] == 403
        assert request('/p/default/api/note', {'path': 'notes/new.md', 'content': '# New note\n', 'revision': ''})[0] == 200
        new_history = json.loads(request('/p/default/api/history?path=notes/new.md')[1])
        assert new_history[0]['committerName'] == 'ForgetMeNote'
        assert new_history[0]['authorEmail'] == author['email']
        assert run('docker', 'exec', name, 'forgetmenote', 'token') == token
        assert run('docker', 'exec', name, 'gitnotes', 'token') == token
        print('PASS: Gitnotes upgrade preserves accounts, identity, notes, locks and history; new commits use ForgetMeNote')
    finally:
        subprocess.run(['docker', 'rm', '-f', name], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        subprocess.run(['docker', 'volume', 'rm', volume], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        db.close()


if __name__ == '__main__':
    main()
