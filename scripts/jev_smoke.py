#!/usr/bin/env python3
"""Synthetic Docker Jev/Quicknotes HTTP and independent full-volume restore.

All credentials, TLS material, accounts and notes are generated test data. The
only inference endpoint is a disposable HTTPS fixture on an isolated bridge.
"""
import json
import os
from pathlib import Path
import secrets
import subprocess
import tempfile
import time
import urllib.error
import urllib.parse
import uuid

from chat_smoke import Browser, run
from smoke_app import free_port, wait_ready
from smoke_database import Database
from smoke_export import wait_exported

PROVIDER = r'''
import https from 'node:https';
import http from 'node:http';
import fs from 'node:fs';
import assert from 'node:assert/strict';
let calls = 0;
const errors = [];
const json = (res, value) => { res.setHeader('Content-Type', 'application/json'); res.end(JSON.stringify(value)); };
https.createServer({key:fs.readFileSync('/fixture/tls.key'),cert:fs.readFileSync('/fixture/tls.crt')}, async(req,res)=>{
  try {
    assert.equal(req.url,'/v1/systemone'); assert.equal(req.method,'POST');
    assert.equal(req.headers.authorization,'Bearer synthetic-jev-secret');
    let raw=''; for await(const part of req) { raw+=part; assert(Buffer.byteLength(raw)<=65536); }
    const body=JSON.parse(raw); assert.equal(body.model,'jev-1.13.0');
    assert(!raw.includes('EXCLUDED_ARCHIVE_MARKER'));
    assert(!raw.includes('EXCLUDED_DRAFT_MARKER'));
    assert(!raw.includes('PRIVATE_CAPTURE_MARKER'));
    calls++;
    const answers={};
    for(const [id,q] of Object.entries(body.questions)) {
      if(q.type==='noul') answers[id]={type:'noul',noul:id==='none'?.01:.99};
      else if(q.type==='score') {
        const top=q.criteria.length-1;
        answers[id]={type:'score',score:top,confidence:1,
          probabilities:Object.fromEntries(q.criteria.map((_,i)=>[String(i),i===top?1:0])),
          legend:Object.fromEntries(q.criteria.map((v,i)=>[String(i),v]))};
      } else {
        assert.equal(q.type,'choice'); const keys=Object.keys(q.criteria);
        const selected=keys.includes('redundant') ? (body.state.duplicate.text===body.state.retained.text?'redundant':'unique') : keys.includes('folder0') ? 'folder0' : keys.includes('related') ? 'related' : keys.includes('contradiction')
          ? (body.state.evidence.length>1?'contradiction':'compatible')
          : keys.find(k=>k==='contradicted') || keys.find(k=>k==='compatible') || keys[0];
        answers[id]={type:'choice',choice:selected,confidence:1,
          probabilities:Object.fromEntries(keys.map(k=>[k,k===selected?1:0]))};
      }
    }
    json(res,{model:'jev-1.13.0',answers,usage:{input_tokens:140,output_tokens:20}});
  } catch(e) { errors.push(String(e)); res.statusCode=400; json(res,{error:'fixture assertion'}); }
}).listen(8443,'0.0.0.0');
http.createServer((req,res)=>{
 if(req.url==='/control') return json(res,{calls,errors});
 res.statusCode=404; res.end();
}).listen(8888,'0.0.0.0');
'''


def finished(browser, path):
    deadline = time.monotonic() + 30
    while time.monotonic() < deadline:
        job = browser.request(path)
        if job['state'] == 'paused':
            browser.request(path + '/continue', {})
        elif job['state'] not in ('queued', 'running'):
            assert job['state'] == 'completed', 'Synthetic review did not complete'
            return job
        time.sleep(.1)
    raise AssertionError('Synthetic review exceeded test deadline')


def main():
    profile = os.environ.get('SMOKE_PROFILE', 'full')
    if profile not in ('full', 'routine'):
        raise ValueError('SMOKE_PROFILE must be full or routine')
    full = profile == 'full'
    image = run('docker', 'image', 'inspect', os.environ.get('SMOKE_IMAGE', 'forgetmenote:poc'), '--format', '{{.Id}}')
    provider_image = os.environ.get('SMOKE_PROVIDER_IMAGE', 'node:24-alpine@sha256:e67514e5d0f6c46656005e1b693b2ec9d52e80b641307de684d4a015ba7a4eaf')
    name = 'forgetmenote-jev-smoke-' + uuid.uuid4().hex[:10]
    provider, restored, network = name + '-provider', name + '-restored', name + '-network'
    volume, restored_volume = name + '-data', name + '-restored-data'
    port = free_port()
    origin = f'http://localhost:{port}'
    with tempfile.TemporaryDirectory(prefix='forgetmenote-jev-fixture-') as temporary:
        fixture = Path(temporary)
        db = None
        try:
            run('docker', 'network', 'create', network)
            db = Database(network=network)
            run('openssl', 'req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-days', '1',
                '-subj', '/CN=' + provider, '-addext', 'subjectAltName=DNS:' + provider,
                '-keyout', str(fixture / 'tls.key'), '-out', str(fixture / 'tls.crt'))
            (fixture / 'provider.mjs').write_text(PROVIDER)
            (fixture / 'jev-key').write_text('synthetic-jev-secret')
            config = {'provider': 'typesafe', 'enabled': True, 'model': 'jev-1.13.0',
                      'endpoint': f'https://{provider}:8443/v1/systemone',
                      'secretFile': '/run/fixture/jev-key'}
            (fixture / 'decisions.json').write_text(json.dumps(config))
            fixture.chmod(0o755)
            for path in fixture.iterdir():
                path.chmod(0o444)
            run('docker', 'run', '-d', '--init', '--name', provider, '--network', network,
                '--read-only', '--cap-drop', 'ALL', '--security-opt', 'no-new-privileges:true',
                '-p', '127.0.0.1::8888', '-v', str(fixture) + ':/fixture:ro',
                '--entrypoint', 'node', provider_image, '/fixture/provider.mjs')
            provider_port = json.loads(run('docker', 'inspect', provider))[0]['NetworkSettings']['Ports']['8888/tcp'][0]['HostPort']
            control = Browser('http://localhost:' + provider_port)
            run('docker', 'volume', 'create', volume)
            run('docker', 'volume', 'create', restored_volume)

            def start(container, data, database='forgetmenote'):
                run('docker', 'run', '-d', '--init', '--name', container, *db.args(database),
                    '--read-only', '--tmpfs', '/tmp:size=64m', '--cap-drop', 'ALL',
                    '--security-opt', 'no-new-privileges:true', '-p', f'127.0.0.1:{port}:8080',
                    '-e', 'PUBLIC_URL=' + origin, '-e', 'DECISION_CONFIG_FILE=/run/fixture/decisions.json',
                    '-e', 'SSL_CERT_FILE=/run/fixture/tls.crt',
                    '-v', data + ':/data', '-v', str(fixture) + ':/run/fixture:ro', image)
                assert run('docker', 'inspect', container, '--format', '{{.Image}}') == image
                wait_ready(origin, container, 15)

            db.migrate(image)
            start(name, volume)
            token = run('docker', 'exec', name, 'forgetmenote', 'token')
            alice = Browser(origin)
            alice.request('/login', {'token': token}, form=True)
            alice.request('/account/setup', {'username': 'jev-alice', 'password': secrets.token_urlsafe(24)})
            alice_id = alice.request('/api/me')['id']
            Browser(origin).denied('/api/notifications')
            assert alice.request('/api/jev/settings')['available'], 'Standalone Jev is unavailable'
            assert not alice.request('/api/chat/connections')['enabled'], 'Jev enabled chat'
            assert not alice.request('/api/jev/settings')['settings']['consent']
            quick_settings = alice.request('/api/quicknotes/settings')
            assert quick_settings['mode'] == 'suggest' and not quick_settings['cloudConsent']
            quick_settings['mode'] = 'manual'
            quick_settings = alice.request('/api/quicknotes/settings', quick_settings)
            capture_request = {'operationId': 'smoke-capture-1', 'content': '# Private quicknote\nPRIVATE_CAPTURE_MARKER\n'}
            capture = alice.request('/api/quicknotes', capture_request)
            assert alice.request('/api/quicknotes', capture_request)['id'] == capture['id']
            assert control.request('/control')['calls'] == 0, 'Saving a capture invoked remote inference'
            documents = {}
            for path, content in {
                'notes/larch-a.md': '# Larch opening\nLarch Observatory opens at 09:00 on weekdays in 2030.\n',
                'notes/larch-b.md': '# Larch opening policy\nLarch Observatory opens at 11:00 on weekdays in 2030.\n',
                'notes/archive.md': '---\nfmn-container: archive\n---\nEXCLUDED_ARCHIVE_MARKER\nLarch Observatory opens at 13:00.\n',
                'notes/draft.md': '---\nfmn-container: draft\n---\nEXCLUDED_DRAFT_MARKER\nLarch Observatory opens at 15:00.\n',
            }.items():
                documents[path] = alice.request('/p/default/api/note', {'path': path, 'content': content, 'revision': ''})
            alice.request('/api/jev/settings', {'consent': True, 'rerank': True, 'citations': True})
            assert not alice.request('/p/default/api/jev/settings')['settings']['autoChecks']
            submission = {'kind': 'consistency', 'operationId': 'smoke-review-1'}
            job = alice.request('/p/default/api/jev/jobs', submission)
            job_path = '/p/default/api/jev/jobs/' + job['id']
            completed = finished(alice, job_path)
            assert completed['examined'] > 0 and completed['results'], 'Review has no examined evidence'
            assert control.request('/control')['calls'] > 0, 'Native HTTPS Jev was never called'
            notifications = alice.request('/api/notifications')
            assert notifications['items'], 'Review notification missing'
            alice.request('/api/notifications/read', {'ids': [notifications['items'][0]['id']]})
            filed = alice.request('/api/quicknotes/file', {'id': capture['id'], 'version': capture['version'], 'destination': {'project': 'default', 'folder': 'notes'}})
            assert filed['status'] == 'filed' and filed['canUndo']
            undone = alice.request('/api/quicknotes/undo', {'id': capture['id']})
            assert undone['project'] == capture['inboxProject'] and undone['status'] == 'inbox'
            password = secrets.token_urlsafe(24)
            bob_id = alice.request('/api/local-users', {'username': 'jev-bob', 'password': password})['id']
            bob = Browser(origin)
            bob.request('/auth/password/login', {'username': 'jev-bob', 'password': password}, form=True)
            bob.denied('/p/' + capture['inboxProject'] + '/api/notes')
            assert not bob.request('/api/notifications')['items']
            assert not bob.request('/api/quicknotes')['captures']
            alice.denied('/api/members', {'project': capture['inboxProject'], 'user': bob_id, 'role': 'read'})
            if full:
                def organize(operation, policy, mode='apply', scope='folder', folder='organization'):
                    request = {'kind': 'organize', 'operationId': operation, 'organization':
                               {'mode': mode, 'scope': scope, 'folder': folder, 'policy': policy}}
                    created = alice.request('/p/default/api/jev/jobs', request)
                    path = '/p/default/api/jev/jobs/' + created['id']
                    return path, finished(alice, path)

                shared = '# Synthetic duplicate\nJuniper clock calibration requires seven turns.\n'
                for path in ('organization/a.md', 'organization/b.md'):
                    alice.request('/p/default/api/note', {'path': path, 'content': shared, 'revision': ''})
                preview_path, preview = organize('organizer-preview', {'duplicates': True}, 'preview')
                assert any(r['status'] == 'suggested' for r in preview['results'])
                assert alice.request('/p/default/api/note?path=organization%2Fb.md')['content'] == shared
                cleanup_path, cleanup = organize('organizer-cleanup', {'duplicates': True})
                assert any(r['status'] == 'applied' and r['details']['result']['trash'] for r in cleanup['results'])
                assert alice.request('/p/default/api/note?path=organization%2Fa.md')['content'] == shared
                tag_path, tagged = organize('organizer-tags', {'tags': True, 'vocabulary': ['calibration']})
                assert 'calibration' in alice.request('/p/default/api/note?path=organization%2Fa.md')['content']
                undo = alice.request(tag_path + '/organization/undo', {})
                assert not any(o.get('error') for o in undo['outcomes'])
                auto = alice.request('/p/default/api/jev/organization', {'enabled': True, 'generation': 0,
                                     'policy': {'tags': True, 'vocabulary': ['automatic']}})
                assert not alice.request('/p/default/api/jev/settings')['settings']['autoChecks']
                auto_calls = control.request('/control')['calls']
                time.sleep(2)
                assert control.request('/control')['calls'] == auto_calls, 'Enablement backfilled Notes'
                alice.request('/p/default/api/note', {'path': 'organization/changed.md',
                              'content': '# Synthetic automatic change\nJuniper maintenance was committed.\n', 'revision': ''})
                deadline = time.monotonic() + 210
                automatic_path = None
                while time.monotonic() < deadline:
                    jobs = alice.request('/p/default/api/jev/jobs')['jobs']
                    match = next((j for j in jobs if j.get('automatic') and j['kind'] == 'organize'), None)
                    if match:
                        automatic_path = '/p/default/api/jev/jobs/' + match['id']
                        finished(alice, automatic_path)
                        break
                    time.sleep(2)
                assert automatic_path, 'Automatic organizer did not run after debounce'
                auto_content = alice.request('/p/default/api/note?path=organization%2Fchanged.md')['content']
                assert auto_content.startswith('---\n') and 'automatic' in auto_content.split('---')[1]
                alice.request('/p/default/api/jev/organization', {'enabled': False, 'generation': auto['settings']['generation'], 'policy': {}})
            before_calls = control.request('/control')['calls']
            before_captures = alice.request('/api/quicknotes')
            before_notifications = alice.request('/api/notifications')
            before_review = alice.request(job_path)
            run('docker', 'stop', '--timeout', '30', name)
            run('docker', 'run', '--rm', '--init', '--user', '0', '--entrypoint', 'sh',
                '-v', volume + ':/source:ro', '-v', restored_volume + ':/target', image,
                '-c', 'cp -a /source/. /target/')
            restored_db = db.restore_copy()
            db.migrate(image, restored_db)
            start(restored, restored_volume, restored_db)
            assert alice.request('/api/jev/settings')['available']
            assert not alice.request('/api/chat/connections')['enabled']
            assert alice.request('/api/me')['id'] == alice_id
            assert alice.request('/api/quicknotes') == before_captures
            assert alice.request('/api/notifications') == before_notifications
            assert alice.request(job_path) == before_review
            assert alice.request('/api/quicknotes', capture_request)['id'] == capture['id']
            assert alice.request('/p/default/api/jev/jobs', submission)['id'] == job['id']
            assert control.request('/control')['calls'] == before_calls, 'Restore replay repeated paid work'
            assert alice.request('/api/quicknotes/settings') == quick_settings
            bob.denied('/p/' + capture['inboxProject'] + '/api/notes')
            if full:
                assert alice.request(cleanup_path) == cleanup
                assert alice.request(automatic_path)['state'] == 'completed'
                restored_undo = alice.request(cleanup_path + '/organization/undo', {})
                assert not any(o.get('error') for o in restored_undo['outcomes']), restored_undo
                assert alice.request('/p/default/api/note?path=organization%2Fb.md')['content'] == shared
                assert alice.request(cleanup_path + '/organization/undo', {}) == {'outcomes': []}
                assert control.request('/control')['calls'] == before_calls, 'Undo invoked inference'
            assert not control.request('/control')['errors'], 'Synthetic provider rejected input or scope'
            # Each restored notebook's Git export reaches its head whole.
            for project in ('default', capture['inboxProject']):
                wait_exported(restored, project, alice.request('/p/' + project + '/api/status')['revision'])
            print('PASS: standalone HTTPS Jev without chat, consent, active-only review, notifications and replay')
            print('PASS: private capture, filing, Undo, protected history and independent full restore')
            if full:
                print('PASS: organizer preview/apply, duplicate Trash, debounce, activity and restored Undo')
            print('JEV_RESTORE_IMAGE ' + json.dumps({'image': image, 'syntheticOnly': True}), flush=True)
        finally:
            for container in (name, restored, provider):
                subprocess.run(['docker', 'rm', '-f', container], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            if db is not None:
                db.close()
            for data in (volume, restored_volume):
                subprocess.run(['docker', 'volume', 'rm', data], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            subprocess.run(['docker', 'network', 'rm', network], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


if __name__ == '__main__':
    main()
