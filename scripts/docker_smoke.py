#!/usr/bin/env python3
"""Exercise a disposable container, its persisted volume, and an independent restore."""
import json
import http.cookiejar
import urllib.parse
import secrets
import os
from pathlib import Path
import subprocess
import tempfile
import urllib.error
import urllib.request
import uuid

from smoke_app import free_port, wait_ready
from smoke_database import Database
from smoke_export import wait_exported


def run(*args):
    return subprocess.check_output(args, text=True, stderr=subprocess.STDOUT).strip()


def main():
    image = os.environ.get("SMOKE_IMAGE", "forgetmenote:poc")
    name = "forgetmenote-smoke-" + uuid.uuid4().hex[:10]
    volume = name + "-data"
    port = free_port()
    origin = f"http://localhost:{port}"
    run("docker", "volume", "create", volume)
    db = Database()
    try:
        db.migrate(image)
        run("docker", "run", "--init", "-d", "--name", name, "--read-only", "--tmpfs", "/tmp:size=64m", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true", "-p", f"127.0.0.1:{port}:8080", "-e", f"PUBLIC_URL={origin}", "-v", f"{volume}:/data", *db.args(), image)
        def ready():
            wait_ready(origin, name, 10)
        ready()
        token = run("docker", "exec", name, "forgetmenote", "token")
        def request(path, body=None, authenticated=True):
            data = None if body is None else json.dumps(body).encode()
            headers = {"Content-Type": "application/json"}
            if authenticated:
                headers["Authorization"] = "Bearer " + token
            with urllib.request.urlopen(urllib.request.Request(origin + ("/p/default"+path if path.startswith("/api/") else path), data=data, headers=headers), timeout=20) as response:
                return response.read()
        setup_jar = http.cookiejar.CookieJar()
        setup_browser = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(setup_jar))
        login = urllib.request.Request(origin + "/login", urllib.parse.urlencode({"token": token}).encode(), {"Origin": origin})
        setup_browser.open(login, timeout=20).close()
        setup_password = secrets.token_urlsafe(24)
        setup = urllib.request.Request(origin + "/account/setup", json.dumps({"username": "first-admin", "password": setup_password}).encode(), {"Origin": origin, "Content-Type": "application/json"})
        with setup_browser.open(setup, timeout=20) as response:
            first_admin = json.load(response)["id"]
        with setup_browser.open(origin + "/api/me", timeout=20) as response:
            me = json.load(response)
            assert me["id"] == first_admin and me["admin"] and not me["setupAvailable"]
        try:
            request("/api/notes", authenticated=False)
            raise AssertionError("unauthenticated private read succeeded")
        except urllib.error.HTTPError as error:
            assert error.code == 401
        first = json.loads(request("/api/note", {"path": "notes/smoke.md", "content": "# Synthetic smoke test\n", "revision": ""}))
        request("/api/note", {"path": first["path"], "content": "# Updated smoke test\n", "revision": first["revision"]})
        try:
            request("/api/note", {"path": first["path"], "content": "stale", "revision": first["revision"]})
            raise AssertionError("stale edit succeeded")
        except urllib.error.HTTPError as error:
            assert error.code == 409
        run("docker", "restart", name)
        ready()
        with setup_browser.open(origin + "/api/me", timeout=20) as response:
            me = json.load(response)
            assert me["id"] == first_admin and me["admin"] and not me["setupAvailable"]
        assert json.loads(request("/api/note?path=notes/smoke.md"))["content"] == "# Updated smoke test\n"
        metadata = json.loads(request("/.well-known/oauth-protected-resource/mcp", authenticated=False))
        assert metadata["resource"] == origin + "/mcp"
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            bundle = root / "backup.bundle"
            bundle.write_bytes(request("/api/backup"))
            run("git", "clone", str(bundle), str(root / "restored"))
            assert (root / "restored/notes/smoke.md").read_text() == "# Updated smoke test\n"
            assert len(run("git", "-C", str(root / "restored"), "log", "--oneline").splitlines()) == 3
            boundary = 'notebook-' + uuid.uuid4().hex
            upload = (
                f'--{boundary}\r\nContent-Disposition: form-data; name="id"\r\n\r\nimported-smoke\r\n'
                f'--{boundary}\r\nContent-Disposition: form-data; name="name"\r\n\r\nImported smoke notebook\r\n'
                f'--{boundary}\r\nContent-Disposition: form-data; name="bundle"; filename="notes.bundle"\r\nContent-Type: application/octet-stream\r\n\r\n'
            ).encode() + bundle.read_bytes() + f'\r\n--{boundary}--\r\n'.encode()
            imported = urllib.request.Request(origin + '/api/projects/import', upload, {'Origin': origin, 'Content-Type': 'multipart/form-data; boundary=' + boundary})
            with setup_browser.open(imported, timeout=30) as response:
                assert json.load(response)['id'] == 'imported-smoke'
            with setup_browser.open(origin + '/p/imported-smoke/api/note?path=notes/smoke.md', timeout=20) as response:
                assert json.load(response)['content'] == '# Updated smoke test\n'
            # The import keeps the bundle's commits: the notebook's revision is
            # the bundle's HEAD, and its Git export holds that same history.
            head = run('git', '-C', str(root / 'restored'), 'rev-parse', 'HEAD')
            with setup_browser.open(origin + '/p/imported-smoke/api/status', timeout=20) as response:
                assert json.load(response)['revision'] == head
            wait_exported(name, 'imported-smoke', head)
        # Personal local accounts use the same storage, permissions and Git authorship.
        class NoRedirect(urllib.request.HTTPRedirectHandler):
            def redirect_request(self, *args, **kwargs):
                return None
        jar = http.cookiejar.CookieJar()
        browser = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(jar), NoRedirect())
        def local(path, body=None, admin=False, form=False):
            headers = {'Origin': origin, 'Content-Type': 'application/x-www-form-urlencoded' if form else 'application/json'}
            if admin:
                headers['Authorization'] = 'Bearer ' + token
            data = None if body is None else (urllib.parse.urlencode(body) if form else json.dumps(body)).encode()
            try:
                r = browser.open(urllib.request.Request(origin + path, data, headers), timeout=20)
            except urllib.error.HTTPError as error:
                r = error
            return r.code, r.read().decode()
        password = secrets.token_urlsafe(24)
        status, body = local('/api/local-users', {'username': 'alice', 'password': password}, admin=True)
        assert status == 200, body
        user = json.loads(body)['id']
        assert local('/auth/password/login', {'username': 'alice', 'password': password}, form=True)[0] == 303
        assert local('/p/default/api/notes')[0] == 403
        assert local('/api/projects', {'id':'private-alice','name':'Private Alice'})[0] == 403
        assert local('/api/users', {'id':user,'canCreateProjects':True,'displayName':'Alice Example'}, admin=True)[0] == 200
        assert local('/api/projects', {'id':'private-alice','name':'Private Alice'})[0] == 200
        assert local('/p/default/api/notes')[0] == 403, 'creation leaked unrelated workspace access'
        assert local('/api/members', {'project':'private-alice','user':user,'role':'edit'})[0] == 409
        assert local('/p/private-alice/api/note', {'path':'private.md','content':'# Private fixture','revision':''})[0] == 200
        assert local('/api/members', {'project': 'default', 'user': user, 'role': 'edit'}, admin=True)[0] == 200
        status, body = local('/account')
        identity = json.loads(body)['author']
        assert local('/p/default/api/note', {'path': 'notes/alice.md', 'content': '# Written by Alice\n', 'revision': ''})[0] == 200
        _, body = local('/p/default/api/history?path=notes/alice.md')
        assert identity['email'] in body and 'forgetmenote@localhost' in body
        run('docker', 'restart', name)
        ready()
        assert local('/api/me')[0] == 200, 'local browser session lost on restart'
        assert local('/logout', {})[0] == 303
        assert local('/auth/password/login', {'username': 'alice', 'password': password}, form=True)[0] == 303
        assert local('/account/password', {'user': user, 'password': secrets.token_urlsafe(24)}, admin=True)[0] == 200
        assert local('/api/me')[0] == 401, 'reset did not revoke session'
        assert local('/auth/password/login', {'username': 'alice', 'password': password}, form=True)[0] == 401
        # Knowledge state lives outside content Git and survives a full /data restore.
        config = {"enabled": True, "model": "fixture-v1", "dimensions": 3, "passageRunes": 256, "splitter": "markdown-heading-v1"}
        request('/api/knowledge/config', {'config': config})
        pending = json.loads(request('/api/knowledge/passages'))
        request('/api/knowledge/embeddings', {'revision': pending['status']['revision'], 'configId': pending['status']['configId'], 'vectors': [{**{k: p[k] for k in ('id', 'hash', 'noteHash')}, 'vector': [1, 0, 0]} for p in pending['passages']]})
        matches = json.loads(request('/api/knowledge/search', {'vector': [1, 0, 0], 'configId': pending['status']['configId']}))
        assert matches and all('revision' in row and 'citation' in row for row in matches)
        hybrid = json.loads(request('/api/knowledge/hybrid-search', {'text': 'smoke', 'vector': [1, 0, 0], 'configId': pending['status']['configId']}))
        assert hybrid['mode'] == 'all' and hybrid['matches']
        assert len({row['path'] for row in hybrid['matches']}) == len(hybrid['matches'])
        fallback = json.loads(request('/api/knowledge/hybrid-search', {'text': 'smoke'}))
        assert fallback['mode'] == 'text' and fallback['message'] and fallback['matches']
        request('/api/knowledge/complete', {'consumer': 'smoke.review', 'previous': '', 'revision': pending['status']['revision'], 'summary': 'Synthetic verification'})
        request('/api/knowledge/proposals', {'id': 'smoke-proposal', 'summary': 'Synthetic pending change', 'edits': [{'path': 'notes/proposal.md', 'content': '# Proposed note'}]})
        archived_input = {'id':'archived-smoke','summary':'Archived audit record','edits':[{'path':'notes/archived.md','content':'# Archived proposal'}]}
        archived = json.loads(request('/api/knowledge/proposals', archived_input))
        request('/api/knowledge/proposals/decide', {'id':archived['id'],'decision':'dismiss'})
        request('/api/knowledge/proposals/decide', {'id':archived['id'],'decision':'archive'})
        trash_note = json.loads(request('/api/note', {'path': 'notes/trash-smoke.md', 'content': '# Trash restore fixture\n', 'revision': ''}))
        request('/api/trash', {'paths': [trash_note['path']], 'revision': trash_note['revision']})
        trash_before = json.loads(request('/api/trash'))
        assert len(trash_before['entries']) == 1
        assert not any(row['path'].startswith('Trash/') for row in json.loads(request('/api/notes')))
        assert json.loads(request('/api/knowledge/hybrid-search', {'text': 'Trash restore fixture', 'mode': 'text'}))['matches'] == []
        included = json.loads(request('/api/knowledge/hybrid-search', {'text': 'Trash restore fixture', 'mode': 'text', 'includeTrash': True}))
        assert included['matches'] and included['matches'][0]['trashId'] == trash_before['entries'][0]['id']
        before_restore = json.loads(request('/api/knowledge/review'))
        assert len(before_restore['proposals']) == 1
        def organization(action, body):
            status, result = local('/api/organization/action/' + action, body, admin=True)
            assert status == 200, result
            return json.loads(result)
        organization('set_group', {'id':'operations','name':'Operations','members':[user],'revision':0})
        organization('set_project_group', {'project':'default','group':'operations','role':'read'})
        invite = organization('create_invitation', {'name':'Restored colleague','groups':['operations'],'projects':{}})
        bot = organization('create_service_account', {'name':'Restore bot','steward':user})
        assert local('/api/members', {'project':'default','user':bot['id'],'role':'read'}, admin=True)[0] == 200
        credential = organization('issue_service_credential', {'user':bot['id'],'name':'Restore credential','projects':['default'],'scopes':['notes:read'],'audiences':['api'],'days':1})
        restored_volume = name + '-restored'
        run('docker', 'stop', name)
        run('docker', 'volume', 'create', restored_volume)
        try:
            # Copy a stopped complete volume using the existing application image.
            run('docker', 'run', '--init', '--rm', '--user', '0', '--entrypoint', 'sh', '-v', volume+':/source:ro', '-v', restored_volume+':/target', image, '-c', 'cp -a /source/. /target/ && rm -rf /target/projects/default/cache')
            # Restore the database from its own dump, as an operator's backup would.
            restored_db = db.restore_copy()
            db.migrate(image, restored_db)
            run('docker', 'rm', name)
            run('docker', 'run', '--init', '-d', '--name', name, '--read-only', '--tmpfs', '/tmp:size=64m', '-p', f'127.0.0.1:{port}:8080', '-e', f'PUBLIC_URL={origin}', '-v', restored_volume+':/data', *db.args(restored_db), image)
            ready()
            _, account_body = local('/api/admin', admin=True)
            restored_admin = json.loads(account_body)
            restored_user = next(u for u in restored_admin['users'] if u['id'] == user)
            assert restored_user['canCreateProjects'] and restored_user['displayName'] == 'Alice Example'
            restored_project = next(p for p in restored_admin['projects'] if p['id'] == 'private-alice')
            assert restored_project['members'] == {user:'owner'}
            org = organization('organization_status', {})['organization']
            assert next(g for g in org['groups'] if g['id'] == 'operations')['members'] == [user]
            assert any(i['id'] == invite['id'] and i['status'] == 'pending' for i in org['invitations'])
            assert any(e['action'] == 'service.create' and e['target'] == bot['id'] for e in org['activity'])
            assert any(u['id'] == bot['id'] and u['kind'] == 'service' and u['steward'] == user for u in restored_admin['users'])
            req = urllib.request.Request(origin + '/api/v1/actions/list_projects', b'{}', {'Authorization':'Bearer ' + credential['token'], 'Content-Type':'application/json'})
            with urllib.request.urlopen(req, timeout=20) as response:
                assert [p['id'] for p in json.load(response)] == ['default']
            assert local('/auth/invitation/accept', {'token':invite['token'],'username':'restored-colleague','password':secrets.token_urlsafe(24)})[0] == 200
            assert local('/auth/invitation/accept', {'token':invite['token'],'username':'restored-replay','password':secrets.token_urlsafe(24)})[0] == 403
            print('PASS: groups, invitations, service credentials and activity survive independent full-volume and database restore')
            assert '# Private fixture' in local('/p/private-alice/api/note?path=private.md', admin=True)[1]
            restored_review = json.loads(request('/api/knowledge/review'))
            assert restored_review == before_restore
            replay = json.loads(request('/api/knowledge/proposals', archived_input))
            assert replay['id'] == archived['id'] and replay['status'] == 'dismissed'

            trash_after = json.loads(request('/api/trash'))
            assert trash_after['entries'] == trash_before['entries']
            request('/api/trash/restore', {'id': trash_after['entries'][0]['id'], 'revision': trash_after['revision']})
            restored_note = json.loads(request('/api/note?path=notes/trash-smoke.md'))
            assert restored_note['content'] == '# Trash restore fixture\n'
            request('/api/trash', {'paths': [restored_note['path']], 'revision': restored_note['revision']})
            confirmation = json.loads(request('/api/trash'))
            request('/api/trash/empty', {'revision': confirmation['revision'], 'confirmation': confirmation['confirmation']})
            assert json.loads(request('/api/trash'))['entries'] == []
            assert json.loads(request('/api/note?path=notes/trash-smoke.md&revision=' + trash_note['revision']))['content'] == '# Trash restore fixture\n'
            print('PASS: Trash search filtering, full-volume restoration, restore, confirmed empty and retained Git history')
            # Vectors live in the database, so the restore keeps them without
            # paying for embeddings again while the removed cache rebuilds.
            restored_index = json.loads(request('/api/knowledge/passages'))
            assert restored_index['status']['indexed'] == restored_index['status']['total'] == len(pending['passages']) and restored_index['passages'] == []
            restored_matches = json.loads(request('/api/knowledge/search', {'vector': [1, 0, 0], 'configId': pending['status']['configId']}))
            assert sorted(row['path'] for row in restored_matches) == sorted(row['path'] for row in matches)
            assert json.loads(request('/api/note?path=notes/smoke.md'))['content'] == '# Updated smoke test\n'
            with setup_browser.open(origin + '/api/me', timeout=20) as response:
                assert json.load(response)['id'] == first_admin
            with setup_browser.open(origin + '/p/imported-smoke/api/note?path=notes/smoke.md', timeout=20) as response:
                assert json.load(response)['content'] == '# Updated smoke test\n'
        finally:
            subprocess.run(['docker', 'rm', '-f', name], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            subprocess.run(['docker', 'volume', 'rm', restored_volume], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        print('PASS: knowledge vectors, checkpoints, proposals, full-volume restore and derived rebuild')
        print('PASS: delegated workspace creation, isolation, ownership, display names and grants after full restoration')
        print('PASS: Docker local login, user permissions/authorship, restart persistence and password reset revocation')
        print("PASS: Docker authentication, save, conflict, restart persistence, OAuth discovery, independent Git restore")
        print("PASS: notebook bundle upload, original Git revision, and full-instance restoration")
    finally:
        subprocess.run(["docker", "rm", "-f", name], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        subprocess.run(["docker", "volume", "rm", volume], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        db.close()


if __name__ == "__main__":
    main()
