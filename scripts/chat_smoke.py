#!/usr/bin/env python3
"""Verify private chat through Docker HTTP, restart, and independent full restore.

Uses only generated accounts, notes, credentials, and a synthetic provider in
an isolated Docker bridge. Never points at an existing application instance.
SMOKE_PROVIDER_IMAGE may select an existing test image containing Node.
SMOKE_RESTORE_IMAGE selects the restored app image; it defaults to SMOKE_IMAGE.
"""
import http.cookiejar
import json
import os
from pathlib import Path
import secrets
import socket
import subprocess
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid

from smoke_database import Database
from smoke_export import wait_exported


def run(*args):
    return subprocess.check_output(args, text=True, stderr=subprocess.STDOUT).strip()


class Browser:
    def __init__(self, origin):
        self.origin = origin
        self.opener = urllib.request.build_opener(
            urllib.request.ProxyHandler({}),
            urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()),
        )

    def request(self, path, body=None, form=False):
        data = None
        headers = {'Origin': self.origin}
        if body is not None:
            data = (urllib.parse.urlencode(body) if form else json.dumps(body)).encode()
            headers['Content-Type'] = 'application/x-www-form-urlencoded' if form else 'application/json'
        request = urllib.request.Request(self.origin + path, data, headers)
        with self.opener.open(request, timeout=15) as response:
            raw = response.read()
            if 'application/json' in response.headers.get('Content-Type', ''):
                return json.loads(raw)
            return raw

    def denied(self, path, body=None):
        try:
            self.request(path, body)
        except urllib.error.HTTPError as error:
            assert error.code in (401, 403, 404), 'Unexpected denial status'
        else:
            raise AssertionError('Private chat access was accepted')


PROVIDER_SCRIPT = r'''
import http from 'node:http';
import assert from 'node:assert/strict';
let calls = 0, blocked = false, release;
const errors = [];
const server = http.createServer(async (req, res) => {
  const json = value => { res.setHeader('Content-Type', 'application/json'); res.end(JSON.stringify(value)); };
  if (req.url === '/control') return json({calls, blocked, errors});
  if (req.url === '/release' && req.method === 'POST') { release?.(); return json({ok: true}); }
  try {
    assert.equal(req.url, '/v1/chat/completions');
    assert.equal(req.headers.authorization, 'Bearer synthetic-chat-provider-secret');
    let raw = ''; for await (const chunk of req) { raw += chunk; assert(raw.length < 1e6); }
    const body = JSON.parse(raw);
    assert.equal(body.model, 'synthetic-chat');
    assert(!body.stream);
    for (const key of ['reasoning_effort', 'thinking', 'stream_options']) assert(!(key in body));
    calls++;
    const users = body.messages.filter(message => message.role === 'user');
    if (JSON.stringify(users.at(-1)).includes('stall-this-synthetic-run')) {
      blocked = true;
      await new Promise(resolve => { const timer = setTimeout(resolve, 15000); release = () => { clearTimeout(timer); resolve(); }; });
    }
    const hasResult = body.messages.some(message => message.role === 'tool');
    const message = hasResult
      ? {role: 'assistant', content: 'The synthetic observatory was calibrated on 2031-04-17.'}
      : {role: 'assistant', tool_calls: [{id: 'synthetic-read-1', type: 'function', function: {
          name: 'read_note', arguments: JSON.stringify({path: 'notes/chat-source.md'})}}]};
    json({choices: [{index: 0, message, finish_reason: hasResult ? 'stop' : 'tool_calls'}],
          usage: {prompt_tokens: 40, completion_tokens: 12, total_tokens: 52}});
  } catch (error) { errors.push(String(error)); res.statusCode = 400; json({error: 'Synthetic provider assertion failed'}); }
});
server.listen(8888, '0.0.0.0');
'''

def wait_ready(origin):
    for _ in range(150):
        try:
            with urllib.request.urlopen(origin + '/healthz', timeout=1) as response:
                if response.status == 200:
                    return
        except (OSError, urllib.error.URLError):
            time.sleep(.1)
    raise AssertionError('Synthetic application failed to become healthy')


def wait_run(browser, path, terminal=None):
    until = time.monotonic() + 15
    while time.monotonic() < until:
        conversation = browser.request(path)
        runs = conversation.get('runs', [])
        if runs and runs[-1]['state'] not in ('queued', 'running', 'awaiting_confirmation'):
            if terminal:
                assert runs[-1]['state'] == terminal, 'Unexpected terminal chat state: ' + json.dumps({key: runs[-1].get(key) for key in ('state', 'error', 'operations', 'usage')})
            return conversation
        time.sleep(.05)
    raise AssertionError('Synthetic chat exceeded test deadline')


def main():
    image = run('docker', 'image', 'inspect', os.environ.get('SMOKE_IMAGE', 'forgetmenote:poc'), '--format', '{{.Id}}')
    restore_image = run('docker', 'image', 'inspect', os.environ.get('SMOKE_RESTORE_IMAGE', image), '--format', '{{.Id}}')
    name = 'forgetmenote-chat-smoke-' + uuid.uuid4().hex[:10]
    restored_name = name + '-restored'
    provider_name = name + '-provider'
    volume, restored_volume, network = name + '-data', name + '-restored-data', name + '-network'
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0))
        port = sock.getsockname()[1]
    origin = f'http://localhost:{port}'
    with tempfile.TemporaryDirectory(prefix='forgetmenote-chat-config-') as temporary:
        config_dir = Path(temporary)
        db = None
        try:
            run('docker', 'network', 'create', network)
            db = Database(network=network)
            (config_dir / 'provider.mjs').write_text(PROVIDER_SCRIPT)
            config = {
                'connections': [{
                    'id': 'synthetic', 'label': 'Synthetic provider', 'format': 'chat-completions',
                    'baseURL': f'http://{provider_name}:8888/v1',
                    'models': ['synthetic-chat'], 'defaultModel': 'synthetic-chat', 'local': True,
                    'secretFile': '/run/chat/provider-secret', 'capabilities': {'tools': True},
                    'limits': {'runSeconds': 15},
                }],
                'defaultConnection': 'synthetic', 'localOnly': True,
            }
            (config_dir / 'chat.json').write_text(json.dumps(config))
            (config_dir / 'provider-secret').write_text('synthetic-chat-provider-secret')
            # Only synthetic credentials; the image's nonroot user must read this mount.
            config_dir.chmod(0o755)
            for path in config_dir.iterdir():
                path.chmod(0o444)
            provider_image = os.environ.get('SMOKE_PROVIDER_IMAGE', 'node:24-alpine@sha256:e67514e5d0f6c46656005e1b693b2ec9d52e80b641307de684d4a015ba7a4eaf')
            run('docker', 'run', '-d', '--init', '--name', provider_name, '--network', network,
                '--read-only', '--cap-drop', 'ALL', '--security-opt', 'no-new-privileges:true',
                '-p', '127.0.0.1::8888', '-v', str(config_dir) + ':/fixture:ro',
                '--entrypoint', 'node', provider_image, '/fixture/provider.mjs')
            provider_port = json.loads(run('docker', 'inspect', provider_name))[0]['NetworkSettings']['Ports']['8888/tcp'][0]['HostPort']
            control = Browser('http://localhost:' + provider_port)
            until = time.monotonic() + 10
            while True:
                try:
                    control.request('/control')
                    break
                except (OSError, urllib.error.URLError):
                    assert time.monotonic() < until, 'Synthetic provider did not start'
                    time.sleep(.1)
            run('docker', 'volume', 'create', volume)
            run('docker', 'volume', 'create', restored_volume)

            def start(container, data, selected_image, database='forgetmenote'):
                run('docker', 'run', '-d', '--init', '--name', container, *db.args(database),
                    '--read-only', '--tmpfs', '/tmp:size=64m', '--cap-drop', 'ALL',
                    '--security-opt', 'no-new-privileges:true', '-p', f'127.0.0.1:{port}:8080',
                    '-e', 'PUBLIC_URL=' + origin, '-e', 'CHAT_CONFIG_FILE=/run/chat/chat.json',
                    '-v', data + ':/data', '-v', str(config_dir) + ':/run/chat:ro', selected_image)
                actual_image = run('docker', 'inspect', container, '--format', '{{.Image}}')
                assert actual_image == selected_image, 'Synthetic app started with an unexpected image ID'
                wait_ready(origin)
                return actual_image

            db.migrate(image)
            seed_actual = start(name, volume, image)
            token = run('docker', 'exec', name, 'forgetmenote', 'token')
            alice = Browser(origin)
            alice.request('/login', {'token': token}, form=True)
            alice.request('/account/setup', {'username': 'chat-alice', 'password': secrets.token_urlsafe(24)})
            alice_id = alice.request('/api/me')['id']
            source = alice.request('/p/default/api/note', {
                'path': 'notes/chat-source.md', 'content': '# Observatory\nCalibrated on 2031-04-17.\n', 'revision': '',
            })
            connections = alice.request('/api/chat/connections')
            public = json.dumps(connections)
            assert 'synthetic-chat-provider-secret' not in public and provider_name not in public and '/run/chat/' not in public
            conversation = alice.request('/p/default/api/chat', {'connection': 'synthetic', 'model': 'synthetic-chat', 'mode': 'ask'})
            path = '/p/default/api/chat/' + conversation['id']
            submission = {'submissionId': 'synthetic-turn-1', 'text': 'Read notes/chat-source.md and report its calibration date.', 'options': {'mode': 'ask'}}
            alice.request(path + '/turns', submission)
            completed = wait_run(alice, path, 'completed')
            assert '2031-04-17' in completed['turns'][-1]['content']
            assert completed['owner'] == alice_id
            assert completed['runs'][0]['operations'], 'Tool receipts/results were not retained'
            assert completed['runs'][0]['usage']['inputTokens'] > 0
            before_calls = control.request('/control')['calls']
            alice.request(path + '/turns', submission)
            assert control.request('/control')['calls'] == before_calls, 'Submission replay repeated model work'
            replay_request = urllib.request.Request(origin + path + '/events?after=0')
            with alice.opener.open(replay_request, timeout=10) as events:
                assert events.headers['Content-Type'].startswith('text/event-stream')
                for line in events:
                    if line.startswith(b'data:'):
                        assert json.loads(line[5:])['sequence'] > 0
                        break

            password = secrets.token_urlsafe(24)
            bob_id = alice.request('/api/local-users', {'username': 'chat-bob', 'password': password})['id']
            alice.request('/api/members', {'project': 'default', 'user': bob_id, 'role': 'read'})
            bob = Browser(origin)
            bob.request('/auth/password/login', {'username': 'chat-bob', 'password': password}, form=True)
            bob.request('/p/default/api/note?path=notes/chat-source.md')
            bob.denied(path)
            private_bob = bob.request('/p/default/api/chat', {'connection': 'synthetic', 'model': 'synthetic-chat', 'mode': 'ask'})
            bob_path = '/p/default/api/chat/' + private_bob['id']
            alice.request('/api/members', {'project': 'default', 'user': bob_id, 'role': ''})
            bob.denied(bob_path)
            alice.request('/api/members', {'project': 'default', 'user': bob_id, 'role': 'read'})

            cancel_input = {'submissionId': 'synthetic-cancel-1', 'text': 'stall-this-synthetic-run', 'options': {'mode': 'ask'}}
            alice.request(path + '/turns', cancel_input)
            until = time.monotonic() + 10
            while not control.request('/control')['blocked']:
                assert time.monotonic() < until, 'Provider never observed cancel fixture'
                time.sleep(.05)
            current = alice.request(path)
            run_id = current['runs'][-1]['id']
            alice.request(path + '/runs/' + run_id + '/cancel', {})
            control.request('/release', {})
            cancelled = wait_run(alice, path, 'cancelled')

            run('docker', 'stop', '--timeout', '30', name)
            run('docker', 'run', '--rm', '--init', '--user', '0', '--entrypoint', 'sh',
                '-v', volume + ':/source:ro', '-v', restored_volume + ':/target', image,
                '-c', 'cp -a /source/. /target/')
            restored_db = db.restore_copy()
            db.migrate(restore_image, restored_db)
            restored_actual = start(restored_name, restored_volume, restore_image, restored_db)
            restored = alice.request(path)
            assert restored['turns'] == cancelled['turns'] and restored['runs'] == cancelled['runs']
            assert restored['eventCursor'] == cancelled['eventCursor']
            assert alice.request('/api/me')['id'] == alice_id
            assert bob.request(bob_path)['owner'] == bob_id
            bob.denied(path)
            assert alice.request('/p/default/api/note?path=notes/chat-source.md')['content'] == source['content']
            before_calls = control.request('/control')['calls']
            alice.request(path + '/turns', submission)
            assert control.request('/control')['calls'] == before_calls, 'Restored submission replay repeated model work'
            assert not control.request('/control')['errors'], 'Synthetic provider assertions failed'
            updated = alice.request('/p/default/api/note', {
                'path': source['path'], 'content': source['content'] + 'Restore write verified.\n', 'revision': source['revision'],
            })
            assert updated['revision'] != source['revision']
            # The write after the restore is the notebook's head, and the restored
            # instance's Git export catches up to it whole.
            assert alice.request('/p/default/api/status')['revision'] == updated['revision']
            wait_exported(restored_name, 'default', updated['revision'])
            print('PASS: private chat HTTP, authorized tools, usage, SSE replay and submission idempotency')
            print('PASS: cross-user isolation, permission revocation and cancellation')
            print('PASS: independent stopped full-volume and database restore preserves chats, tools, usage, events, identities and note history')
            print('CHAT_RESTORE_IMAGES ' + json.dumps({'seedImage': seed_actual, 'restoredImage': restored_actual}, sort_keys=True), flush=True)
        finally:
            for container in (name, restored_name, provider_name):
                subprocess.run(['docker', 'rm', '-f', container], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            if db is not None:
                db.close()
            for data in (volume, restored_volume):
                subprocess.run(['docker', 'volume', 'rm', data], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            subprocess.run(['docker', 'network', 'rm', network], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


if __name__ == '__main__':
    main()
