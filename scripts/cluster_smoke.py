#!/usr/bin/env python3
"""Two gateways serve one instance from Postgres and S3, with no owner.

Both gateways share the database, the recovery key and an S3 blob store
(smoke_s3.py, run from the app image). Notebooks live in those two; each gateway
keeps only its Git exports on its own volume. The smoke signs in on one gateway
and continues on the other, writes notes through each and reads them, their
content blocks and their exports on both, moves an OAuth consent and a legacy
MCP session between them, reads an export that one gateway began in chunks from
both, and replaces a gateway with one on an empty volume without losing
anything.
"""
import base64
import hashlib
import html
import http.cookiejar
import json
import os
import re
import secrets
import shutil
import socket
import subprocess
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
from pathlib import Path

from smoke_database import Database
from smoke_export import wait_exported

ORIGIN = 'http://localhost:8080'
BUCKET, REGION = 'forgetmenote-smoke', 'us-east-1'


def run(*args):
    return subprocess.check_output(args, text=True, stderr=subprocess.STDOUT).strip()


def free_port():
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0))
        return sock.getsockname()[1]


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None


class Client:
    """One browser or API client. Every gateway answers for the same origin, so
    a request names its gateway by port and the origin by its Host header; the
    cookie jar ignores ports, so a session follows the client between them."""

    def __init__(self):
        self.opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()), NoRedirect())

    def request(self, port, path, body=None, form=False, headers=None):
        sent = {'Host': 'localhost:8080', 'Origin': ORIGIN, 'Content-Type': 'application/x-www-form-urlencoded' if form else 'application/json', **(headers or {})}
        data = None if body is None else (urllib.parse.urlencode(body) if form else json.dumps(body)).encode()
        try:
            response = self.opener.open(urllib.request.Request(f'http://127.0.0.1:{port}{path}', data, sent), timeout=60)
        except urllib.error.HTTPError as error:
            response = error
        return response.code, dict(response.headers), response.read().decode()


def eventually(check, description, timeout=10):
    """A gateway learns of another's revision by NOTIFY, or by its poll within
    five seconds, so a read there may trail the write by that much."""
    deadline = time.monotonic() + timeout
    while not check():
        if time.monotonic() > deadline:
            raise AssertionError(description)
        time.sleep(0.1)


def rpc_result(body):
    """The JSON-RPC response from a JSON or event-stream reply."""
    if body.lstrip().startswith('{'):
        return json.loads(body)
    return json.loads(next(line[5:] for line in body.splitlines() if line.startswith('data:')))


def main():
    image = os.environ.get('SMOKE_IMAGE', 'forgetmenote:poc')
    name = 'forgetmenote-cluster-' + uuid.uuid4().hex[:10]
    hardened = ['--init', '--read-only', '--tmpfs', '/tmp:size=64m', '--cap-drop', 'ALL', '--security-opt', 'no-new-privileges:true']
    secrets_dir = Path(tempfile.mkdtemp(prefix='fmn-cluster-'))
    # Containers run as another user; every value here is synthetic and disposable.
    secrets_dir.chmod(0o755)
    recovery = secrets.token_urlsafe(32)
    access_key, secret_key = 'AKIASMOKE' + secrets.token_hex(6).upper(), secrets.token_urlsafe(30)
    files = {
        'admin.token': recovery + '\n',
        'blobs.json': json.dumps({'s3': {'bucket': BUCKET, 'prefix': 'instance', 'region': REGION, 'endpoint': f'http://{name}-s3:9000', 'allowHTTP': True}}),
    }
    for file, content in files.items():
        (secrets_dir / file).write_text(content)
        (secrets_dir / file).chmod(0o644)
    volumes = []
    db = Database()
    ports = {}

    def volume(label):
        volumes.append(f'{name}-{label}')
        run('docker', 'volume', 'create', volumes[-1])
        return volumes[-1]

    def logs(container):
        result = subprocess.run(['docker', 'logs', container], text=True, capture_output=True)
        return result.stdout + result.stderr

    def start_gateway(label, data):
        ports[label] = free_port()
        run('docker', 'run', '-d', '--name', f'{name}-{label}', *hardened, *db.args(), '-p', f'127.0.0.1:{ports[label]}:8080',
            '-v', f'{data}:/data', '-v', f'{secrets_dir}/admin.token:/data/admin.token:ro', '-v', f'{secrets_dir}:/run/cluster:ro',
            '-e', 'PUBLIC_URL=' + ORIGIN, '-e', 'BLOBS_CONFIG_FILE=/run/cluster/blobs.json',
            '-e', 'AWS_ACCESS_KEY_ID=' + access_key, '-e', 'AWS_SECRET_ACCESS_KEY=' + secret_key, '-e', 'AWS_EC2_METADATA_DISABLED=true',
            image)
        for _ in range(300):
            try:
                if Client().request(ports[label], '/healthz')[0] == 200:
                    return
            except OSError:
                pass
            time.sleep(0.1)
        raise RuntimeError(f'gateway {label} failed to become ready:\n' + logs(f'{name}-{label}'))

    try:
        db.migrate(image)
        run('docker', 'run', '-d', '--name', f'{name}-s3', '--network', db.network, '--read-only', '--cap-drop', 'ALL',
            '-v', f'{Path(__file__).resolve().parent / "smoke_s3.py"}:/opt/smoke_s3.py:ro',
            '-e', 'SMOKE_S3_ACCESS_KEY=' + access_key, '-e', 'SMOKE_S3_SECRET_KEY=' + secret_key,
            '--entrypoint', 'python3', image, '-I', '/opt/smoke_s3.py', '9000', BUCKET, REGION)
        start_gateway('a', volume('a'))
        start_gateway('b', volume('b'))
        a, b = ports['a'], ports['b']
        bearer = {'Authorization': 'Bearer ' + recovery}
        api = Client()
        for port in (a, b):
            status, _, body = api.request(port, '/api/me', headers=bearer)
            assert status == 200 and json.loads(body)['id'] == '@local-admin', body

        # A recovery session begun on one gateway completes setup on the other.
        browser = Client()
        assert browser.request(a, '/login', {'token': recovery}, form=True)[0] in (200, 303)
        password = secrets.token_urlsafe(24)
        status, _, body = browser.request(b, '/account/setup', {'username': 'first-admin', 'password': password})
        assert status == 200, body
        admin = json.loads(body)['id']
        status, _, body = browser.request(a, '/api/me')
        assert status == 200 and json.loads(body)['id'] == admin, body
        def note(port, path):
            status, _, body = browser.request(port, '/p/default/api/note?path=' + urllib.parse.quote(path))
            return json.loads(body)['content'] if status == 200 else None

        def head(port):
            status, _, body = browser.request(port, '/p/default/api/status')
            assert status == 200, body
            return json.loads(body)['revision']

        status, _, body = browser.request(a, '/p/default/api/note', {'path': 'notes/cluster.md', 'content': '# Written through gateway A\n', 'revision': ''})
        assert status == 200, body
        written = json.loads(body)['revision']
        eventually(lambda: note(b, 'notes/cluster.md') == '# Written through gateway A\n', 'gateway B never read the note written through A')
        assert head(a) == head(b) == written
        # The notebook is in Postgres: neither gateway holds a Git working tree
        # (DATA/projects/ID keeps only derived state), and each keeps a Git
        # export that follows revisions made on either.
        for label in ('a', 'b'):
            run('docker', 'exec', f'{name}-{label}', 'sh', '-c', 'test ! -e /data/projects/default/workspace/.git')
            wait_exported(f'{name}-{label}', 'default', written)
        # A note of 8 KiB or more is kept as content blocks in the shared S3
        # store, so A reads exactly what B wrote though it never held the text.
        book = ''.join(f'Paragraph {number}: ' + 'a long note kept in blocks ' * 40 + '\n\n' for number in range(24))
        status, _, body = browser.request(b, '/p/default/api/note', {'path': 'notes/book.md', 'content': book, 'revision': ''})
        assert status == 200, body
        eventually(lambda: note(a, 'notes/book.md') == book, 'gateway A never read the long note written through B')
        requests = logs(f'{name}-s3')
        assert re.search(rf'"PUT /{BUCKET}/instance/blocks/[0-9a-f]{{64}} HTTP/1.1" 200', requests), requests[-2000:]
        print('PASS: shared recovery key, sessions across gateways, and notes in Postgres and S3 with an export on each gateway', flush=True)

        # A consent shown by B is submitted to A, the code is exchanged on B,
        # and a legacy MCP session initialized on A answers on both.
        status, _, body = browser.request(a, '/oauth/register', {'client_name': 'Cluster MCP', 'redirect_uris': ['https://client.example/cb']})
        assert status == 201, body
        client = json.loads(body)['client_id']
        verifier = secrets.token_urlsafe(32)
        challenge = base64.urlsafe_b64encode(hashlib.sha256(verifier.encode()).digest()).decode().rstrip('=')
        query = urllib.parse.urlencode({'client_id': client, 'redirect_uri': 'https://client.example/cb', 'response_type': 'code',
                                        'code_challenge_method': 'S256', 'code_challenge': challenge, 'resource': ORIGIN + '/mcp'})
        status, _, consent = browser.request(b, '/oauth/authorize?' + query)
        assert status == 200 and 'Connect Cluster MCP?' in consent, consent[:500]
        fields = {key: html.unescape(re.search('name="' + key + '" value="([^"]+)"', consent).group(1)) for key in ['request', 'signature']}
        status, headers, body = browser.request(a, '/oauth/authorize', fields, form=True)
        assert status == 303, body
        code = urllib.parse.parse_qs(urllib.parse.urlparse(headers['Location']).query)['code'][0]
        status, _, body = Client().request(b, '/oauth/token', {'grant_type': 'authorization_code', 'code': code, 'client_id': client,
                                                               'redirect_uri': 'https://client.example/cb', 'code_verifier': verifier}, form=True)
        assert status == 200, body
        mcp = {'Authorization': 'Bearer ' + json.loads(body)['access_token'], 'Accept': 'application/json, text/event-stream'}
        agent = Client()
        status, headers, body = agent.request(a, '/mcp', {'jsonrpc': '2.0', 'id': 0, 'method': 'initialize', 'params': {
            'protocolVersion': '2025-06-18', 'capabilities': {}, 'clientInfo': {'name': 'cluster-smoke', 'version': '1'}}}, headers=mcp)
        assert status == 200 and 'serverInfo' in body, body
        session = next(value for key, value in headers.items() if key.lower() == 'mcp-session-id')
        mcp |= {'Mcp-Session-Id': session, 'MCP-Protocol-Version': '2025-06-18'}
        assert agent.request(b, '/mcp', {'jsonrpc': '2.0', 'method': 'notifications/initialized'}, headers=mcp)[0] in (200, 202)

        def tool(port, call, arguments, number):
            status, _, body = agent.request(port, '/mcp', {'jsonrpc': '2.0', 'id': number, 'method': 'tools/call', 'params': {'name': call, 'arguments': arguments}}, headers=mcp)
            assert status == 200, f'{call} on {port}: {status} {body[:500]}'
            result = rpc_result(body)['result']
            assert not result.get('isError'), result
            return result.get('structuredContent') or json.loads(result['content'][0]['text'])

        for number, port in enumerate((b, a), 1):
            assert tool(port, 'read_note', {'project': 'default', 'path': 'notes/cluster.md'}, number)['content'] == '# Written through gateway A\n'
        print('PASS: OAuth consent, code exchange and a legacy MCP session across gateways', flush=True)

        # An export begun on A continues in chunks from both gateways.
        first = tool(a, 'export_project_backup', {'project': 'default', 'limit': 256}, 10)
        bundle, offset, number = base64.b64decode(first['data']), first['nextOffset'], 11
        assert first['total'] > 256, first['total']
        while offset < first['total']:
            part = tool((b, a)[number % 2], 'export_project_backup', {'project': 'default', 'offset': offset, 'limit': 256, 'sha256': first['sha256']}, number)
            bundle, offset, number = bundle + base64.b64decode(part['data']), part['nextOffset'], number + 1
        assert hashlib.sha256(bundle).hexdigest() == first['sha256'], 'the assembled export does not match its digest'
        requests = logs(f'{name}-s3')
        assert re.search(rf'"PUT /{BUCKET}/instance/exports/[^ ]+ HTTP/1.1" 200', requests), requests[-2000:]
        assert '403' not in re.findall(r'" (\d{3}) ', requests), 'a gateway sent an unsigned or mis-signed S3 request'
        print(f'PASS: an export of {first["total"]} bytes begun on A read in {number - 10} chunks from both gateways through S3', flush=True)

        # Replace A with a gateway on an empty volume, as a rollout does: B serves
        # meanwhile, and the new gateway holds the same sessions and notes.
        run('docker', 'stop', '-t', '30', f'{name}-a')
        run('docker', 'rm', f'{name}-a')
        status, _, body = browser.request(b, '/p/default/api/note', {'path': 'notes/rollout.md', 'content': '# Written during the rollout\n', 'revision': ''})
        assert status == 200, body
        written = json.loads(body)['revision']
        start_gateway('a', volume('a-replacement'))
        a = ports['a']
        assert note(a, 'notes/rollout.md') == '# Written during the rollout\n'
        assert note(a, 'notes/book.md') == book
        assert tool(a, 'read_note', {'project': 'default', 'path': 'notes/rollout.md'}, 99)['content'] == '# Written during the rollout\n'
        # The new gateway rebuilds its export of the whole history from the store.
        wait_exported(f'{name}-a', 'default', written)
        print('PASS: a replacement gateway on an empty volume keeps sessions, MCP sessions and notes, and rebuilds its export', flush=True)
    except BaseException:
        for label in ('a', 'b'):
            print(f'--- last lines from {label}:\n' + '\n'.join(logs(f'{name}-{label}').splitlines()[-40:]), flush=True)
        raise
    finally:
        for label in ('a', 'b', 's3'):
            subprocess.run(['docker', 'rm', '-f', f'{name}-{label}'], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        for item in volumes:
            subprocess.run(['docker', 'volume', 'rm', item], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        db.close()
        shutil.rmtree(secrets_dir, ignore_errors=True)


if __name__ == '__main__':
    main()
