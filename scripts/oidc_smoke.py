#!/usr/bin/env python3
"""Disposable HTTPS Authelia/SQLite/TOTP + ForgetMeNote integration and full restore.
Requires deploy/requirements.txt, Docker, and forgetmenote:oidc-test.
No real DNS, SMTP, accounts or application data are used.
"""
import base64
import hashlib
import hmac
import html
import http.cookiejar
import importlib.util
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import socket
import ssl
import struct
import subprocess
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request

import yaml

REPO = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location('standalone_operator', REPO / 'deploy/standalone/operator.py')
op = importlib.util.module_from_spec(spec)
spec.loader.exec_module(op)
APP = 'https://notes.forgetmenote.test'
AUTH = 'https://auth.forgetmenote.test'
PORT = 19443
REQUIRE_TOTP = os.environ.get('REQUIRE_TOTP') == '1'


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None


class Browser:
    def __init__(self, root):
        self.jar = http.cookiejar.CookieJar()
        ctx = ssl.create_default_context(cafile=str(root / 'caddy-data/caddy/pki/authorities/local/root.crt'))
        self.client = urllib.request.build_opener(urllib.request.ProxyHandler({}), urllib.request.HTTPCookieProcessor(self.jar), urllib.request.HTTPSHandler(context=ctx), NoRedirect())

    def request(self, url, data=None, form=False, headers=None):
        h = dict(headers or {})
        body = None
        if data is not None:
            h['Content-Type'] = 'application/x-www-form-urlencoded' if form else 'application/json'
            u = urllib.parse.urlsplit(url)
            h['Origin'] = u.scheme + '://' + u.netloc
            body = (urllib.parse.urlencode(data) if form else json.dumps(data)).encode()
        try:
            r = self.client.open(urllib.request.Request(url, body, h), timeout=15)
        except urllib.error.HTTPError as e:
            r = e
        raw = r.read().decode()
        return r.code, r.headers, raw

    def follow(self, url):
        for _ in range(10):
            status, headers, body = self.request(url)
            if status not in [302, 303]:
                return status, headers, body
            url = urllib.parse.urljoin(url, headers['Location'])
        raise AssertionError('redirect loop')


def compose(root, *args):
    return op.compose(root, *args, capture=True)


def wait_for(check):
    for _ in range(40):
        try:
            if check():
                return
        except (OSError, urllib.error.URLError):
            pass
        time.sleep(1)
    raise AssertionError('service did not become ready')


def totp(uri):
    q = urllib.parse.parse_qs(urllib.parse.urlparse(uri).query)
    secret = q['secret'][0]
    key = base64.b32decode(secret + '=' * ((8 - len(secret) % 8) % 8))
    raw = hmac.new(key, struct.pack('>Q', int(time.time()) // 30), hashlib.sha1).digest()
    offset = raw[-1] & 15
    return str((struct.unpack('>I', raw[offset:offset + 4])[0] & 0x7fffffff) % 1000000).zfill(6)


def login(browser, password, uri=None, next_path='/'):
    for _ in range(15):
        s, h, login_body = browser.request(APP + '/auth/oidc/login', {'next': next_path}, True)
        if s != 502:
            break
        time.sleep(1)
    assert s == 303, 'OIDC login start: '+str(s)+' '+login_body
    authorize = h['Location']
    s, h, _ = browser.request(authorize)
    assert s in [302, 303], 'provider authorization'
    if uri is not None:
        s, _, body = browser.request(AUTH + '/api/firstfactor', {'username': 'alice', 'password': password, 'keepMeLoggedIn': False})
        assert s == 200 and json.loads(body)['status'] == 'OK', 'first factor'
        if REQUIRE_TOTP:
            s, h, _ = browser.request(authorize)
            assert h['Location'].startswith(AUTH + '/?'), 'password alone bypassed two-factor'
            s, _, _ = browser.request(APP + '/api/me')
            assert s == 401, 'ForgetMeNote logged in before second factor'
            s, _, _ = browser.request(AUTH + '/api/secondfactor/totp', {'token': 'invalid'})
            assert s != 200, 'invalid TOTP accepted'
            s, _, body = browser.request(AUTH + '/api/secondfactor/totp', {'token': totp(uri)})
            assert s == 200 and json.loads(body)['status'] == 'OK', 'TOTP failed'
    s, h, _ = browser.request(authorize)
    loc = h['Location']
    assert '/consent/' in loc, 'second-factor consent not reached'
    flow = urllib.parse.parse_qs(urllib.parse.urlparse(loc).query)['flow_id'][0]
    s, _, body = browser.request(AUTH + '/api/oidc/consent', {'client_id': 'forgetmenote', 'flow_id': flow, 'subflow': '', 'consent': True, 'pre_configure': False, 'claims': []})
    assert s == 200, 'provider consent'
    return browser.follow(json.loads(body)['data']['redirect_uri'])


def main():
    base = Path(tempfile.mkdtemp(prefix='forgetmenote-oidc-smoke-'))
    root = base / ('original-' + secrets.token_hex(4))
    restored = base / ('restored-' + secrets.token_hex(4))
    old_resolver = socket.getaddrinfo
    socket.getaddrinfo = lambda host, port, *a, **kw: old_resolver('127.0.0.1', PORT, *a, **kw) if host.endswith('.forgetmenote.test') else old_resolver(host, port, *a, **kw)
    password = secrets.token_urlsafe(24)
    try:
        settings = {'domain': 'notes.forgetmenote.test', 'auth_domain': 'auth.forgetmenote.test', 'admin': 'alice', 'admin_email': 'alice@example.com', 'notification_mode': 'operator', 'uid': os.getuid() or 1000, 'gid': os.getgid() or 1000}
        op.render(root, settings)
        if REQUIRE_TOTP:
            config = yaml.safe_load((root / 'authelia/configuration.yml').read_text())
            config['identity_providers']['oidc']['clients'][0]['authorization_policy'] = 'two_factor'
            op.atomic(root / 'authelia/configuration.yml', yaml.safe_dump(config), replace=True)
        op.user_change(root, 'add', 'alice', '', op.PasswordHasher().hash(password))
        op.atomic(root / 'Caddyfile', 'notes.forgetmenote.test {\n tls internal\n reverse_proxy forgetmenote:8080\n}\nauth.forgetmenote.test {\n tls internal\n reverse_proxy authelia:9091\n}\n', replace=True)
        c = yaml.safe_load((root / 'compose.yaml').read_text())
        c['services']['forgetmenote']['image'] = os.environ.get('SMOKE_IMAGE', 'forgetmenote:oidc-test')
        del c['services']['forgetmenote']['build']
        c['services']['forgetmenote']['environment']['SSL_CERT_FILE'] = '/certs/root.crt'
        c['services']['forgetmenote']['volumes'].append('./caddy-data/caddy/pki/authorities/local:/certs:ro')
        c['services']['caddy']['ports'] = [f'127.0.0.1:{PORT}:443']
        c['services']['caddy']['networks'] = {'default': {'aliases': ['notes.forgetmenote.test', 'auth.forgetmenote.test']}}
        for service in c['services'].values():
            service['init'] = True
        op.atomic(root / 'compose.yaml', yaml.safe_dump(c), replace=True)
        compose(root, 'run', '--rm', '--no-deps', 'authelia', 'authelia', 'config', 'validate', '--config', '/config/configuration.yml')
        compose(root, 'up', '-d', '--no-deps', 'caddy')
        ca = root / 'caddy-data/caddy/pki/authorities/local/root.crt'
        wait_for(ca.exists)
        # Caddy's internal CA directory is private by default. Trust its public certificate only.
        ca.parent.chmod(0o755)
        ca.chmod(0o644)
        # Mount the already-created test CA before creating the app container.
        compose(root, "up", "-d")
        browser = Browser(root)
        wait_for(lambda: browser.request(AUTH + '/api/health')[0] == 200)
        compose(root, 'stop', 'authelia')
        op.enroll_totp(root, 'alice')
        qr = root / 'authelia/enrollment/alice.png'
        assert qr.read_bytes().startswith(b'\x89PNG') and qr.stat().st_mode & 0o777 == 0o600
        before = qr.read_bytes()
        try:
            op.enroll_totp(root, 'alice')
            raise AssertionError('enrollment overwrote existing QR')
        except ValueError:
            assert qr.read_bytes() == before
        exported = compose(root, 'run', '--rm', '--no-deps', 'authelia', 'authelia', 'storage', 'user', 'totp', 'export', 'uri', '--config', '/config/configuration.yml').stdout
        uri = re.search(r'otpauth://\S+', exported).group(0)
        compose(root, 'start', 'authelia')
        wait_for(lambda: browser.request(AUTH + '/api/health')[0] == 200)
        assert login(browser, password, uri)[0] == 200
        notices = browser.request(AUTH + '/locales/en/settings.json')[2]
        assert 'No email was sent' in notices and 'SMTP' in notices
        assert not (root / 'authelia/notification.txt').exists() or not (root / 'authelia/notification.txt').read_text().strip()
        print('PASS: email-free enrollment, private QR, repeat enrollment protection and explicit SMTP notice', flush=True)
        user = json.loads(browser.request(APP + '/api/me')[2])
        assert not user['admin'] and user['id'].startswith('oidc-')
        assert json.loads(browser.request(APP + '/api/projects')[2]) == []
        assert browser.request(APP + '/p/default/api/notes')[0] == 403
        assert login(browser, password)[0] == 200
        assert json.loads(browser.request(APP + '/api/me')[2])['id'] == user['id']
        print('PASS: HTTPS OIDC, ' + ('required TOTP' if REQUIRE_TOTP else 'password-only provider login') + ', first/repeat login, no automatic access', flush=True)
        # Explicit operator grant; startup never grants OIDC users authority.
        compose(root, 'stop', 'forgetmenote')
        compose(root, 'run', '--rm', '--no-deps', 'forgetmenote', 'grant-admin', user['id'])
        compose(root, 'start', 'forgetmenote')
        wait_for(lambda: browser.request(APP + '/healthz')[0] == 200)
        assert json.loads(browser.request(APP + '/api/me')[2])['admin']
        s, _, saved = browser.request(APP + '/p/default/api/note', {'path': 'notes/proof.md', 'content': '# Disposable OIDC proof\n', 'revision': ''})
        assert s == 200, 'save'
        history = json.loads(browser.request(APP + '/p/default/api/history?path=notes/proof.md')[2])
        assert history and history[0]['authorEmail'].endswith('@users.gitnotes.invalid')
        # Fresh OIDC login followed by the existing user-bound MCP consent and PKCE exchange.
        s, _, body = browser.request(APP + '/oauth/register', {'client_name': 'Disposable MCP', 'redirect_uris': ['https://client.example/cb']})
        assert s == 201
        client = json.loads(body)['client_id']
        verifier = secrets.token_urlsafe(32)
        challenge = base64.urlsafe_b64encode(hashlib.sha256(verifier.encode()).digest()).decode().rstrip('=')
        query = urllib.parse.urlencode({'client_id': client, 'redirect_uri': 'https://client.example/cb', 'response_type': 'code', 'code_challenge_method': 'S256', 'code_challenge': challenge})
        s, _, consent = login(browser, password, next_path='/oauth/authorize?' + query)
        assert s == 200 and 'Connect Disposable MCP?' in consent
        fields = {key: html.unescape(re.search('name="' + key + '" value="([^"]+)"', consent).group(1)) for key in ['request', 'signature']}
        s, h, _ = browser.request(APP + '/oauth/authorize', fields, True)
        assert s == 303
        code = urllib.parse.parse_qs(urllib.parse.urlparse(h['Location']).query)['code'][0]
        s, _, body = browser.request(APP + '/oauth/token', {'grant_type': 'authorization_code', 'code': code, 'client_id': client, 'redirect_uri': 'https://client.example/cb', 'code_verifier': verifier}, True)
        assert s == 200
        tokens = json.loads(body)
        auth = {'Authorization': 'Bearer ' + tokens['access_token'], 'Accept': 'application/json, text/event-stream'}
        initialize = {'jsonrpc':'2.0','id':0,'method':'initialize','params':{'protocolVersion':'2025-06-18','capabilities':{},'clientInfo':{'name':'oidc-smoke','version':'1'}}}
        s, h, body = browser.request(APP + '/mcp', initialize, headers=auth)
        assert s == 200 and 'serverInfo' in body, body
        session = next((v for k,v in h.items() if k.lower() == 'mcp-session-id'), None)
        if session:
            auth['Mcp-Session-Id'] = session
        auth['MCP-Protocol-Version'] = '2025-06-18'
        browser.request(APP + '/mcp', {'jsonrpc':'2.0','method':'notifications/initialized'}, headers=auth)
        tool = {'jsonrpc': '2.0', 'id': 1, 'method': 'tools/call', 'params': {'name': 'read_note', 'arguments': {'project': 'default', 'path': 'notes/proof.md'}}}
        s, _, body = browser.request(APP + '/mcp', tool, headers=auth)
        assert s == 200 and 'Disposable OIDC proof' in body, f'MCP read status={s}: {body[:1000]}'
        print('PASS: explicit Admin, Git save, MCP consent through OIDC and authenticated tool call', flush=True)
        # Stop all writers, copy the COMPLETE instance into an independent deployment.
        compose(root, 'down')
        shutil.copytree(root, restored)
        compose(restored, 'up', '-d')
        wait_for(lambda: browser.request(APP + '/healthz')[0] == 200)
        wait_for(lambda: browser.request(AUTH + '/api/health')[0] == 200)
        assert json.loads(browser.request(APP + '/api/me')[2])['id'] == user['id']
        assert json.loads(browser.request(APP + '/api/me')[2])['admin']
        assert 'Disposable OIDC proof' in browser.request(APP + '/p/default/api/note?path=notes/proof.md')[2]
        assert json.loads(browser.request(APP + '/p/default/api/history?path=notes/proof.md')[2]) == history
        auth.pop('Mcp-Session-Id', None)
        s,h,body=browser.request(APP + '/mcp', initialize, headers=auth)
        assert s==200 and 'serverInfo' in body, body
        session=next((v for k,v in h.items() if k.lower()=='mcp-session-id'),None)
        if session:
            auth['Mcp-Session-Id']=session
        browser.request(APP + '/mcp', {'jsonrpc':'2.0','method':'notifications/initialized'}, headers=auth)
        s, _, body = browser.request(APP + '/mcp', tool, headers=auth)
        assert s == 200 and 'Disposable OIDC proof' in body, f'MCP read status={s}: {body[:1000]}'
        # In-memory IdP sessions end on restart; its encrypted TOTP database remains restorable.
        assert json.loads(browser.request(AUTH + '/api/state')[2])['data']['authentication_level'] == 0
        compose(restored, 'stop', 'authelia')
        exported2 = compose(restored, 'run', '--rm', '--no-deps', 'authelia', 'authelia', 'storage', 'user', 'totp', 'export', 'uri', '--config', '/config/configuration.yml').stdout
        assert uri in exported2
        # Disable in ForgetMeNote and prove that re-enabling does not resurrect tokens.
        recovery = {'Authorization': 'Bearer ' + (restored / 'data/admin.token').read_text().strip()}
        s, _, _ = browser.request(APP + '/api/users', {'id': user['id'], 'admin': False, 'disabled': True}, headers=recovery)
        assert s == 200
        assert browser.request(APP + '/api/me')[0] == 401
        assert browser.request(APP + '/mcp', tool, headers=auth)[0] == 401
        s, _, _ = browser.request(APP + '/oauth/token', {'grant_type': 'refresh_token', 'refresh_token': tokens['refresh_token'], 'client_id': client}, True)
        assert s == 400
        compose(restored, 'restart', 'forgetmenote')
        wait_for(lambda: browser.request(APP + '/healthz')[0] == 200)
        assert browser.request(APP + '/api/me')[0] == 401
        compose(restored, 'start', 'authelia')
        wait_for(lambda: browser.request(AUTH + '/api/health')[0] == 200)
        # A fresh TOTP counter avoids replaying the earlier code in the restored database.
        time.sleep(31 - time.time() % 30)
        denied = Browser(restored)
        assert login(denied, password, uri)[0] == 403, 'OIDC reactivated disabled user'
        print('PASS: complete restore, Git history/data, identity, permissions, browser/MCP persistence, TOTP persistence, disable/restart revocation', flush=True)
    finally:
        socket.getaddrinfo = old_resolver
        for directory in [restored, root]:
            if (directory / 'compose.yaml').exists():
                try:
                    if os.environ.get('KEEP_OIDC_SMOKE'):
                        for line in compose(directory, 'logs', '--tail=20', 'forgetmenote').stdout.splitlines():
                            if 'OIDC discovery failed:' in line:
                                print(line)
                    compose(directory, 'down', '--remove-orphans')
                except subprocess.CalledProcessError:
                    pass
        if os.environ.get("KEEP_OIDC_SMOKE"):
            print("Disposable diagnostics retained at", base)
        else:
            shutil.rmtree(base)

if __name__ == '__main__':
    main()
