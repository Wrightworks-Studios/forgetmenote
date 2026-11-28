import base64
from contextlib import contextmanager
import fcntl
import json
import io
import os
from pathlib import Path
import re
import stat
import tarfile
import signal
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from unittest import mock
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import watcher

# The worker image names its rclone here; these tests choose their own.
os.environ.pop('FMN_BACKUP_RCLONE', None)


class ManagedContractTest(unittest.TestCase):
    def test_worker_rejects_gateway_without_transport_protection(self):
        with self.assertRaises(ValueError):
            watcher.Gateway({'gateway':'http://example.com','worker':'local','token_file':'/unreadable'})

    def test_managed_lease_rejects_arbitrary_destination(self):
        for remote in ['/tmp/remote.git','https://attacker.example/repo.git','https://token@github.com/example/backup.git']:
            with self.assertRaises(ValueError):
                watcher.validate_lease({'id':'lease','remote':remote,'branch':'main','revision':'a'*40,'project':'notes','generation':1},
                                       {'project':'notes','generation':1,'branch':'main','repository':{'full_name':'example/backup'}},'a'*40)

    def test_a_worker_beside_the_gateway_connects_over_loopback_with_its_host(self):
        seen = []

        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                seen.append((self.path, self.headers['Host'], self.headers['Authorization']))
                body = b'[]'
                self.send_response(200)
                self.send_header('Content-Length', str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def log_message(self, *args):
                pass

        server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        with tempfile.TemporaryDirectory() as tmp:
            token = Path(tmp) / 'token'
            token.write_text('t' * 40)
            settings = {'gateway': 'https://notes.example', 'worker': 'local', 'token_file': str(token)}
            gateway = watcher.Gateway({**settings, 'gateway_address': f'http://127.0.0.1:{server.server_port}'})
            self.assertEqual(gateway.call('bindings'), [])
            self.assertEqual(seen, [('/internal/backups/v1/bindings', 'notes.example', 'Bearer ' + 't' * 40)])
            for address in ['https://127.0.0.1:8080', 'http://10.0.0.5:8080', 'http://notes.example',
                            'http://127.0.0.1:8080/api', 'http://user@127.0.0.1:8080']:
                with self.assertRaises(ValueError, msg=address):
                    watcher.Gateway({**settings, 'gateway_address': address})

SCRIPT = Path(__file__).with_name('watcher.py')
# The source checkout; the release gate runs these tests with only this directory.
REPO = Path(__file__).resolve().parent.parent.parent


class RcloneSetupTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.rclone = Path(self.tmp.name) / 'rclone'
        self.rclone.write_text('#!/bin/sh\n')
        self.rclone.chmod(0o755)

    def test_the_worker_image_rclone_serves_when_none_is_configured(self):
        other = Path(self.tmp.name) / 'pinned-rclone'
        other.write_text('#!/bin/sh\n')
        with mock.patch.dict(os.environ, {'FMN_BACKUP_RCLONE': str(self.rclone)}):
            self.assertEqual(watcher.rclone_binary({}), self.rclone)
            self.assertEqual(watcher.rclone_binary({'rclone': str(other)}), other)

    def test_a_worker_without_rclone_needs_it_configured(self):
        env = {k: v for k, v in os.environ.items() if k != 'FMN_BACKUP_RCLONE'}
        with mock.patch.dict(os.environ, env, clear=True):
            with self.assertRaisesRegex(ValueError, 'Set "rclone"'):
                watcher.rclone_binary({})
        with mock.patch.dict(os.environ, {'FMN_BACKUP_RCLONE': str(Path(self.tmp.name) / 'missing')}):
            with self.assertRaisesRegex(ValueError, 'Set "rclone"'):
                watcher.rclone_binary({})

    def test_the_worker_image_ci_and_readme_pin_one_rclone_release(self):
        if not (REPO / 'Dockerfile').is_file():
            self.skipTest('needs the source checkout')
        docker = (REPO / 'Dockerfile').read_text()
        version = re.search(r'^ARG RCLONE_VERSION=([0-9.]+)$', docker, re.M)[1]
        sums = {arch: re.search(rf'^ARG RCLONE_SHA256_{arch.upper()}=([0-9a-f]{{64}})$', docker, re.M)[1]
                for arch in ('amd64', 'arm64')}
        ci = (REPO / '.github/workflows/check.yml').read_text()
        self.assertEqual(set(re.findall(r'rclone-v([0-9.]+)-linux', ci)), {version})
        self.assertIn(f'https://downloads.rclone.org/v{version}/rclone-v{version}-linux-amd64.zip', ci)
        self.assertIn(f'echo "{sums["amd64"]}  ', ci)
        readme = (REPO / 'deploy/git-backup/README.md').read_text()
        self.assertEqual(set(re.findall(r'pinned at v([0-9]+(?:\.[0-9]+)*)', readme)), {version})
        self.assertEqual(set(re.findall(r'rclone-v([0-9.]+)-linux', readme)), {version})
        for arch, digest in sums.items():
            self.assertIn(f'| `rclone-v{version}-linux-{arch}.zip` | `{digest}` |', readme)



def git(repo, *args, check=True):
    return subprocess.run(['git', '-C', str(repo), *args], check=check,
                          capture_output=True, text=True)


class BackupTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix='notebook-backup-test-')
        self.root = Path(self.tmp.name)
        self.repo = self.root / 'notebook'
        self.remote = self.root / 'remote.git'
        self.repo.mkdir()
        self.remote.mkdir()
        git(self.repo, 'init', '-b', 'main')
        git(self.repo, 'config', 'user.name', 'Synthetic test')
        git(self.repo, 'config', 'user.email', 'test@localhost')
        git(self.remote, 'init', '--bare', '-b', 'main')
        self.config = self.root / 'config.json'
        self.settings = dict(repository=str(self.repo), remote=str(self.remote),
                             branch='main', state_dir=str(self.root / 'state'),
                             debounce_seconds=0.3, max_delay_seconds=0.7,
                             reconcile_seconds=1.2, retry_seconds=0.1,
                             retry_max_seconds=0.4, push_timeout_seconds=5)
        self.config.write_text(json.dumps(self.settings))
        self.processes = []
        self.logs = []
        self.commit('initial')

    def tearDown(self):
        for process in self.processes:
            if process.poll() is None:
                os.killpg(process.pid, signal.SIGTERM)
                try:
                    process.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    os.killpg(process.pid, signal.SIGKILL)
                    process.wait()
        for log in self.logs:
            log.close()
        self.tmp.cleanup()

    def commit(self, text):
        (self.repo / 'note.md').write_text(text)
        git(self.repo, 'add', 'note.md')
        git(self.repo, 'commit', '-m', text)
        return git(self.repo, 'rev-parse', 'HEAD').stdout.strip()

    def head(self, repo=None):
        result = git(repo or self.remote, 'rev-parse', '--verify', 'refs/heads/main', check=False)
        return result.stdout.strip() if result.returncode == 0 else None

    def once(self, resume=False):
        return subprocess.run([sys.executable, str(SCRIPT), '--config', str(self.config), '--once',
                               *(['--resume'] if resume else [])],
                              capture_output=True, text=True, timeout=15)

    def bare_export(self):
        """A bare repository like ForgetMeNote's export of the notebook, which
        the worker then backs up instead of the working tree."""
        export = self.root / 'export.git'
        git(self.root, 'init', '--quiet', '--bare', '-b', 'main', str(export))
        git(self.repo, 'push', '--quiet', str(export), 'main')
        self.config.write_text(json.dumps({**self.settings, 'repository': str(export)}))
        return export

    def publish(self, export, commit):
        # The exporter moves the branch itself, also to rewritten history.
        git(self.repo, 'push', '--quiet', '--force', str(export), commit + ':refs/heads/main')

    def rewrite(self, base, text, purge):
        """A commit on base as a purge rewrites history, carrying its trailer."""
        git(self.repo, 'checkout', '--quiet', '--detach', base)
        (self.repo / 'note.md').write_text(text)
        git(self.repo, 'commit', '--quiet', '-am', 'rewritten', '-m', 'Gitnotes-Purge: ' + purge)
        return git(self.repo, 'rev-parse', 'HEAD').stdout.strip()

    def flag(self, export, purge, head, replaces):
        temporary = export / (watcher.PURGE_FLAG + '.tmp')
        temporary.write_text(json.dumps(dict(purge=purge, head=head, replaces=replaces)))
        temporary.replace(export / watcher.PURGE_FLAG)

    def start(self, clock=None):
        if not sys.platform.startswith('linux'):
            self.skipTest('live watcher requires Linux inotify; portable backup tests still run')
        if not Path('/usr/bin/inotifywait').is_file():
            self.fail('Linux watcher tests require inotify-tools')
        log = (self.root / f'worker-{len(self.logs)}.log').open('w+')
        self.logs.append(log)
        command = [sys.executable, str(SCRIPT)]
        if clock is not None:
            # Control only the worker's scheduling clock. Git, inotify, the
            # selector, remote transfer and the parent's deadlines remain real.
            command = [sys.executable, '-c', """
import sys, time
from pathlib import Path
from types import SimpleNamespace
sys.path.insert(0, sys.argv.pop(1))
clock = Path(sys.argv.pop(1))
import watcher
watcher.time = SimpleNamespace(monotonic=lambda: float(clock.read_text()), sleep=time.sleep)
sys.exit(watcher.main())
""", str(SCRIPT.parent), str(clock)]
        process = subprocess.Popen([*command, '--config', str(self.config)],
                                   stdout=log, stderr=log, start_new_session=True)
        self.processes.append(process)
        return process

    def until(self, condition, timeout=8):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if condition():
                return
            time.sleep(0.025)
        for log in self.logs:
            log.flush()
            log.seek(0)
            print(log.read())
        self.fail('backup condition was not reached')

    def state(self):
        try:
            return json.loads((self.root / 'state/status.json').read_text())
        except (FileNotFoundError, json.JSONDecodeError):
            return {}

    def test_once_preserves_history_and_does_not_touch_dirty_files_or_run_hooks(self):
        expected = self.commit('second revision')
        (self.repo / 'note.md').write_text('unsaved external draft')
        hook = self.repo / '.git/hooks/pre-push'
        hook.write_text('#!/bin/sh\nexit 77\n')
        hook.chmod(0o700)
        before = git(self.repo, 'status', '--porcelain').stdout
        result = self.once()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.head(), expected, 'the backup must publish committed HEAD')
        self.assertEqual(git(self.remote, 'show', 'main:note.md').stdout, 'second revision')
        self.assertEqual(git(self.remote, 'rev-list', '--count', 'main').stdout.strip(), '2')
        self.assertEqual((self.repo / 'note.md').read_text(), 'unsaved external draft')
        self.assertEqual(git(self.repo, 'status', '--porcelain').stdout, before)
        self.assertEqual(self.state()['last_pushed_commit'], expected)

    def test_existing_github_token_backs_up_private_repo_and_restores(self):
        token = 'existing-synthetic-wiki-token'
        key = self.root / 'github-token'; key.write_text(token)
        remote = 'https://github.com/example/backup.git'
        self.config.write_text(json.dumps({**self.settings, 'mode':'github', 'remote':remote, 'token_file':str(key)}))
        expected = self.head(self.repo)
        real_popen = subprocess.Popen
        seen = []
        def transport(command, *args, **kwargs):
            if isinstance(command, list) and command[0] == 'git' and remote in command:
                self.assertNotIn(token, str(command) + str(kwargs.get('env')))
                helper = next(v.split('=',1)[1][1:] for v in command if v.startswith('credential.helper=!'))
                cmd = watcher.shlex.split(helper) + ['get']
                for path, allowed in [('example/backup.git', True), ('example/other.git', False)]:
                    child = real_popen(cmd, stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True)
                    out, _ = child.communicate(f'protocol=https\nhost=github.com\npath={path}\n\n')
                    self.assertEqual(token in out, allowed)
                seen.append(cmd[1])
                command = [str(self.remote) if v == remote else v for v in command]
                command[1:1] = ['-c','protocol.file.allow=always']
            return real_popen(command,*args,**kwargs)
        private = True
        def metadata(request, timeout):
            self.assertEqual(request.full_url, 'https://api.github.com/repos/example/backup')
            self.assertEqual(request.get_header('Authorization'), 'Bearer '+token)
            return io.BytesIO(json.dumps({'private':private,'archived':False,'full_name':'example/backup','permissions':{'push':True}}).encode())
        opener = mock.Mock(); opener.open.side_effect = metadata
        with mock.patch.object(sys, 'argv', [str(SCRIPT),'--config',str(self.config),'--once']), mock.patch.object(watcher.urllib.request, 'build_opener', return_value=opener), mock.patch.object(watcher.subprocess,'Popen',side_effect=transport):
            self.assertEqual(watcher.main(), 0)
            self.assertEqual(self.head(),expected)
            private = False
            self.commit('new private material')
            before=len(seen)
            self.assertEqual(watcher.main(),1)
            self.assertEqual(len(seen),before,'public destination reached Git transport')
        clone=self.root/'restored'
        subprocess.run(['git','clone',str(self.remote),str(clone)],check=True,capture_output=True)
        self.assertEqual((clone/'note.md').read_text(),'initial')
        self.assertTrue(all(not Path(p).exists() for p in seen))
        self.assertNotIn(token,(Path(self.settings['state_dir'])/'status.json').read_text())

    def test_managed_gateway_push_restores_independently_and_keeps_credentials_private(self):
        token = 'private-installation-fixture-token'
        worker_token = 'private-worker-fixture-token-0123456789'
        binding = dict(project='notes', generation=1, branch='main', request=1, repository={'full_name':'example/backup'})
        receipts, checks = [], []
        expected = self.commit('saved markdown')
        (self.repo / 'image.bin').write_bytes(bytes(range(256)))
        git(self.repo, 'add', 'image.bin')
        git(self.repo, 'commit', '-m', 'attachment')
        expected = self.head(self.repo)
        parent = self
        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass
            def do_GET(self):
                parent.assertEqual(self.headers.get('Authorization'), 'Bearer '+worker_token)
                self.send_response(200); self.end_headers(); self.wfile.write(json.dumps([binding]).encode())
            def do_POST(self):
                parent.assertEqual(self.headers.get('Authorization'), 'Bearer '+worker_token)
                value = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
                if self.path.endswith('/lease'):
                    output = dict(id='lease', token=token, project='notes', generation=1, revision=value['revision'], branch='main', remote='https://github.com/example/backup.git')
                elif self.path.endswith('/check'):
                    checks.append(value); output = {'valid':True}
                else:
                    receipts.append(value); output = binding
                self.send_response(200); self.end_headers(); self.wfile.write(json.dumps(output).encode())
        server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        thread = threading.Thread(target=server.serve_forever); thread.start()
        self.addCleanup(lambda: (server.shutdown(), thread.join(), server.server_close()))
        key = self.root / 'worker-secret'; key.write_text(worker_token)
        config = {**self.settings, 'mode':'managed', 'gateway':f'http://127.0.0.1:{server.server_port}', 'worker':'local', 'token_file':str(key), 'notebooks':{'notes':str(self.repo)}}
        worker = watcher.ManagedWorker(config)
        worker.state_dir.mkdir(mode=0o700)
        real_popen = subprocess.Popen
        checked_helper = []
        def local_git(command, *args, **kwargs):
            if isinstance(command, list) and command[0]=='git' and 'https://github.com/example/backup.git' in command:
                parent.assertNotIn(token, str(command)+str(kwargs.get('env')))
                helper = next(part.split('=',1)[1] for part in command if part.startswith('credential.helper=!'))[1:]
                # Exercise the real helper protocol and its host/path boundary.
                helper_command = watcher.shlex.split(helper) + ['get']
                for host, allowed in [('github.com', True), ('attacker.example', False)]:
                    p=real_popen(helper_command,stdin=subprocess.PIPE,stdout=subprocess.PIPE,text=True)
                    output,_=p.communicate(f'protocol=https\nhost={host}\npath=example/backup.git\n\n')
                    parent.assertEqual(token in output, allowed)
                checked_helper.append(helper_command[1])
                # Replace only the remote transport with a real local bare Git
                # repository. The actual shared push/verification path still runs.
                command=[(''+str(parent.remote)) if v=='https://github.com/example/backup.git' else v for v in command]
                command[1:1]=['-c','protocol.file.allow=always']
            return real_popen(command,*args,**kwargs)
        with mock.patch.object(watcher.subprocess,'Popen',side_effect=local_git):
            self.assertTrue(worker.tick(once=True))
        self.assertTrue(checks)
        self.assertEqual(receipts[-1]['revision'],expected)
        self.assertNotIn('error',receipts[-1])
        self.assertEqual(self.head(),expected)
        self.assertTrue(all(not Path(p).exists() for p in checked_helper))
        for p in worker.state_dir.rglob('*'):
            if p.is_file():self.assertNotIn(token,p.read_text())
        clone=self.root/'restored'
        subprocess.run(['git','clone',str(self.remote),str(clone)],check=True,capture_output=True)
        self.assertEqual((clone/'note.md').read_text(),'saved markdown')
        self.assertEqual((clone/'image.bin').read_bytes(),bytes(range(256)))
        self.assertEqual(git(clone,'rev-list','--count','HEAD').stdout.strip(),'3')
        # Restart obtains a new lease and reconciles the exact already-pushed HEAD.
        with mock.patch.object(watcher.subprocess,'Popen',side_effect=local_git):
            self.assertTrue(watcher.ManagedWorker(config).tick(once=True))
        # A drained handoff cannot lease or push another locally committed note.
        binding.update(generation=2, handoff='a'*32, enabled=False)
        self.commit('after authority preparation')
        previous_checks=len(checks)
        self.assertTrue(worker.tick(once=True))
        self.assertEqual(receipts[-1],dict(project='notes',generation=2,handoff='a'*32))
        self.assertEqual(len(checks),previous_checks)
        self.assertEqual(self.head(),expected)
        self.assertNotIn('notes',worker.active)

    def test_managed_revocation_stops_an_active_transfer_and_removes_credentials(self):
        binding = dict(project='notes', generation=1, branch='main', repository={'full_name':'example/backup'})
        class RevokedGateway:
            checks = 0
            def call(self, path, data):
                if path == 'lease':
                    return dict(id='lease', token='private-fixture-token', project='notes', generation=1,
                                revision=data['revision'], branch='main', remote='https://github.com/example/backup.git')
                if path == 'check':
                    self.checks += 1
                    if self.checks == 1:
                        return {'valid':True}
                raise RuntimeError('Lease revoked')
        gateway = RevokedGateway()
        backup = watcher.ManagedBackup(self.settings, gateway, binding)
        backup.state_dir.mkdir()
        real_popen, transfers = subprocess.Popen, []
        def slow_transfer(command, *args, **kwargs):
            if isinstance(command, list) and command[0] == 'git' and 'push' in command:
                # A real child process exercises cancellation at the transport
                # boundary without sending notebook content to a remote service.
                process = real_popen([sys.executable, '-c', 'import time; time.sleep(60)'], *args, **kwargs)
                transfers.append(process)
                return process
            return real_popen(command, *args, **kwargs)
        with mock.patch.object(watcher.subprocess, 'Popen', side_effect=slow_transfer):
            self.assertFalse(backup.push())
        self.assertGreaterEqual(gateway.checks, 2)
        self.assertEqual(len(transfers), 1)
        self.assertIsNotNone(transfers[0].poll(), 'revoked upload process remained active')
        self.assertIsNone(self.head(), 'revoked transfer published content')
        self.assertFalse(list(backup.state_dir.glob('credential-*')))
        self.assertIsNone(self.state()['last_pushed_commit'])
        self.assertNotIn('private-fixture-token', json.dumps(self.state()))

    def test_worker_requires_the_exact_notebook_root(self):
        nested = self.repo / 'nested'
        nested.mkdir()
        with self.assertRaisesRegex(ValueError, 'exact notebook Git root'):
            watcher.Backup({**self.settings, 'repository':str(nested)})
        export = self.bare_export()
        with self.assertRaisesRegex(ValueError, 'exact notebook Git root'):
            watcher.Backup({**self.settings, 'repository':str(export / 'refs')})

    def test_bare_export_repository_backs_up_by_fast_forward(self):
        export = self.bare_export()
        initial = self.head(self.repo)
        self.assertEqual(self.once().returncode, 0)
        self.assertEqual(self.head(), initial)
        second = self.commit('second revision')
        self.publish(export, second)
        result = self.once()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.head(), second)
        self.assertEqual(git(self.remote, 'show', 'main:note.md').stdout, 'second revision')
        self.assertEqual(self.state()['last_pushed_commit'], second)

    def test_live_watcher_follows_a_bare_export(self):
        export = self.bare_export()
        self.start()
        self.until(lambda: self.head() == self.head(self.repo))
        expected = self.commit('exported later')
        self.publish(export, expected)
        self.until(lambda: self.head() == expected)

    def test_purge_flag_replaces_the_dropped_history_once(self):
        export = self.bare_export()
        initial = self.head(self.repo)
        leaked = self.commit('the key is sk-live-0123')
        self.publish(export, leaked)
        self.assertEqual(self.once().returncode, 0)
        self.assertEqual(self.head(), leaked)
        purged = self.rewrite(initial, 'the key is [purged]', 'purge-1')
        # Without a flag, rewritten history is divergence, as always.
        self.publish(export, purged)
        self.assertNotEqual(self.once().returncode, 0)
        self.assertEqual(self.head(), leaked)
        self.assertTrue(self.state()['blocked'])
        # The purge's flag authorizes replacing the commits it dropped.
        self.flag(export, 'purge-1', purged, [leaked])
        result = self.once(resume=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.head(), purged)
        self.assertEqual(git(self.remote, 'show', 'main:note.md').stdout, 'the key is [purged]')
        self.assertFalse((export / watcher.PURGE_FLAG).exists())
        self.assertEqual(self.state()['pushed_purges'], ['purge-1'])
        self.assertFalse(self.state()['blocked'])
        # Fast-forward pushes carry on.
        later = self.commit('after the purge')
        self.publish(export, later)
        self.assertEqual(self.once().returncode, 0)
        self.assertEqual(self.head(), later)
        # A pushed purge authorizes nothing more, even when its flag remains.
        again = self.rewrite(initial, 'rewritten once more', 'purge-1')
        self.publish(export, again)
        self.flag(export, 'purge-1', again, [later])
        self.assertNotEqual(self.once(resume=True).returncode, 0)
        self.assertEqual(self.head(), later)

    def test_purge_flag_never_replaces_other_history(self):
        export = self.bare_export()
        initial = self.head(self.repo)
        leaked = self.commit('the key is sk-live-0123')
        self.publish(export, leaked)
        self.assertEqual(self.once().returncode, 0)
        stray = self.rewrite(initial, 'another rewrite', 'purge-1')
        purged = self.rewrite(initial, 'the key is [purged]', 'purge-1')
        self.publish(export, purged)
        for purge, head, replaces in [
                ('purge-1', purged, [initial]),   # the remote is not at a commit the purge dropped
                ('purge-2', purged, [leaked]),    # the pushed history does not record this purge
                ('purge-1', stray, [leaked]),     # the pushed revision lacks the flagged commit
                ('../purge-1', purged, [leaked]), # not a purge ID
        ]:
            self.flag(export, purge, head, replaces)
            self.assertNotEqual(self.once(resume=True).returncode, 0, (purge, head, replaces))
            self.assertEqual(self.head(), leaked)
            self.assertTrue((export / watcher.PURGE_FLAG).exists())
        (export / watcher.PURGE_FLAG).write_text('{not json')
        self.assertNotEqual(self.once(resume=True).returncode, 0)
        # A commit someone else pushed is never replaced, even by a valid flag.
        other = self.root / 'other'
        subprocess.run(['git', 'clone', '--quiet', str(self.remote), str(other)], check=True, capture_output=True)
        git(other, 'config', 'user.name', 'Other writer')
        git(other, 'config', 'user.email', 'other@localhost')
        (other / 'other.md').write_text('remote-only change')
        git(other, 'add', '.')
        git(other, 'commit', '--quiet', '-m', 'remote change')
        git(other, 'push', '--quiet', 'origin', 'main')
        foreign = self.head()
        self.flag(export, 'purge-1', purged, [leaked])
        self.assertNotEqual(self.once(resume=True).returncode, 0)
        self.assertEqual(self.head(), foreign)
        self.assertEqual(self.state()['pushed_purges'], [])

    def test_purge_flag_is_spent_by_a_fast_forward_that_pushes_the_purge(self):
        export = self.bare_export()
        initial = self.head(self.repo)
        self.assertEqual(self.once().returncode, 0)
        # A flag stays while the pushed revision lacks its purge's commit.
        elsewhere = self.rewrite(initial, 'elsewhere', 'purge-0')
        git(self.repo, 'checkout', '--quiet', 'main')
        second = self.commit('second revision')
        self.publish(export, second)
        self.flag(export, 'purge-0', elsewhere, [initial])
        self.assertEqual(self.once().returncode, 0)
        self.assertEqual(self.head(), second)
        self.assertTrue((export / watcher.PURGE_FLAG).exists())
        self.assertEqual(self.state()['pushed_purges'], [])
        # The remote never received the leak, so the purge arrives as a fast
        # forward; it is spent all the same.
        leaked = self.commit('the key is sk-live-0123')
        self.publish(export, leaked)
        purged = self.rewrite(second, 'the key is [purged]', 'purge-1')
        self.flag(export, 'purge-1', purged, [leaked])
        self.publish(export, purged)
        result = self.once()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.head(), purged)
        self.assertFalse((export / watcher.PURGE_FLAG).exists())
        self.assertEqual(self.state()['pushed_purges'], ['purge-1'])

    def test_unreadable_purge_flag_never_fails_a_fast_forward(self):
        export = self.bare_export()
        (export / watcher.PURGE_FLAG).mkdir()
        result = self.once()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.head(), self.head(self.repo))

    def test_spent_purge_flag_is_removed_under_the_exporters_lock(self):
        export = self.bare_export()
        head = self.head(self.repo)
        backup = watcher.Backup({**self.settings, 'repository': str(export)})
        self.flag(export, 'purge-1', head, [])
        with open(export / watcher.PURGE_LOCK, 'w') as lock:
            fcntl.flock(lock, fcntl.LOCK_EX)
            remover = threading.Thread(target=backup.remove_purge_flag, args=['purge-1'])
            remover.start()
            time.sleep(0.3)
            self.assertTrue(remover.is_alive(), 'the worker removed the flag while the exporter held its lock')
            # The exporter writes a newer purge's flag under its lock.
            self.flag(export, 'purge-2', head, [])
        remover.join(10)
        self.assertFalse(remover.is_alive())
        self.assertEqual(json.loads((export / watcher.PURGE_FLAG).read_text())['purge'], 'purge-2')
        backup.remove_purge_flag('purge-2')
        self.assertFalse((export / watcher.PURGE_FLAG).exists())

    def test_debounces_commits_and_ignores_uncommitted_edits(self):
        clock = self.root / 'clock'
        def advance(value):
            temporary = clock.with_suffix('.tmp')
            temporary.write_text(str(value))
            temporary.replace(clock)
        advance(0)
        initial = self.head(self.repo)
        self.start(clock=clock)
        self.until(lambda: self.state().get('observed_commit') == initial)
        advance(0.4)
        self.until(lambda: self.head() == initial)
        for number, text in enumerate(['one', 'two', 'three'], start=1):
            advance(0.4 + number * 0.06)
            expected = self.commit(text)
            self.until(lambda: self.state().get('observed_commit') == expected)
        self.assertEqual(self.head(), initial, self.state())
        advance(0.9)
        self.until(lambda: self.head() == expected)
        self.until(lambda: self.state().get('status') == 'synced')
        last_success = self.state()['last_success_at']
        (self.repo / 'note.md').write_text('not committed')
        advance(2)
        self.until(lambda: self.state().get('last_success_at') != last_success)
        self.assertEqual(self.head(), expected)
        self.assertEqual(git(self.remote, 'show', 'main:note.md').stdout, 'three')
        self.assertEqual((self.repo / 'note.md').read_text(), 'not committed')

    def test_retries_failure_without_another_commit(self):
        initial = self.head(self.repo)
        self.start()
        self.until(lambda: self.head() == initial)
        offline = self.root / 'offline.git'
        self.remote.rename(offline)
        expected = self.commit('while offline')
        self.until(lambda: self.state().get('status') == 'error')
        offline.rename(self.remote)
        self.until(lambda: self.head() == expected)
        self.until(lambda: self.state().get('status') == 'synced')

    def test_reconciliation_detects_remote_divergence_without_a_local_event(self):
        initial = self.head(self.repo)
        self.start()
        self.until(lambda: self.head() == initial)
        other = self.root / 'other'
        subprocess.run(['git', 'clone', str(self.remote), str(other)], check=True, capture_output=True)
        git(other, 'config', 'user.name', 'Other writer')
        git(other, 'config', 'user.email', 'other@localhost')
        (other / 'other.md').write_text('remote-only change')
        git(other, 'add', '.')
        git(other, 'commit', '-m', 'remote change')
        git(other, 'push', 'origin', 'main')
        remote_head = self.head()
        self.until(lambda: self.state().get('status') == 'error')
        self.assertEqual(self.head(), remote_head)
        self.assertEqual(self.head(self.repo), initial)

    def test_maximum_delay_does_not_starve_continuous_commits(self):
        clock = self.root / 'clock'
        def advance(value):
            temporary = clock.with_suffix('.tmp')
            temporary.write_text(str(value))
            temporary.replace(clock)
        advance(0)
        initial = self.head(self.repo)
        self.start(clock=clock)
        self.until(lambda: self.state().get('observed_commit') == initial)
        advance(0.4)
        self.until(lambda: self.head() == initial)
        self.until(lambda: self.state().get('status') == 'synced')
        for number, now in enumerate([0.5, 0.7, 0.9, 1.1], start=1):
            advance(now)
            expected = self.commit(f'continuous {number}')
            self.until(lambda: self.state().get('observed_commit') == expected)
        self.assertEqual(self.head(), initial, self.state())
        # Max delay expires at 1.2; the latest commit's debounce expires at 1.4.
        # Keep Git, inotify and transfer real without imposing a host-speed race.
        advance(1.21)
        self.until(lambda: self.head() == expected)
        self.until(lambda: self.state().get('status') == 'synced')
        advance(1.25)
        expected = self.commit('continuous after maximum delay')
        self.until(lambda: self.state().get('observed_commit') == expected)
        advance(1.6)
        self.until(lambda: self.head() == expected)

    def test_restart_catches_up_and_second_worker_cannot_overlap(self):
        process = self.start()
        self.until(lambda: self.head() == self.head(self.repo))
        second = self.once()
        self.assertNotEqual(second.returncode, 0)
        os.killpg(process.pid, signal.SIGTERM)
        process.wait(timeout=5)
        expected = self.commit('while stopped')
        self.start()
        self.until(lambda: self.head() == expected)

    def test_commit_during_push_is_uploaded_by_a_followup_push(self):
        initial = self.head(self.repo)
        self.assertEqual(self.once().returncode, 0)
        backup = watcher.Backup(self.settings)
        backup.last_pushed = initial
        first = self.commit('first pending revision')
        backup.observe()
        started, proceed = threading.Event(), threading.Event()
        real_git = backup.git
        def delayed_git(*args, **kwargs):
            # Pause after the target SHA is selected, but perform the actual Git push.
            if args[0] == 'push':
                started.set()
                if not proceed.wait(timeout=5):
                    raise RuntimeError('test push gate timed out')
            return real_git(*args, **kwargs)
        backup.git = delayed_git
        pushing = threading.Thread(target=backup.push)
        pushing.start()
        try:
            self.assertTrue(started.wait(timeout=3))
            expected = self.commit('committed during upload')
            self.assertEqual(self.state()['last_pushed_commit'], initial)
        finally:
            proceed.set()
            pushing.join(timeout=5)
        self.assertFalse(pushing.is_alive())
        self.assertEqual(self.head(), first)
        self.assertEqual(backup.last_pushed, first)
        self.assertIsNotNone(backup.pending_since)
        self.assertTrue(backup.push())
        self.assertEqual(self.head(), expected)

    def test_packed_refs_still_trigger_backup(self):
        initial = self.head(self.repo)
        git(self.repo, 'pack-refs', '--all')
        self.start()
        self.until(lambda: self.head() == initial)
        expected = self.commit('after packing references')
        self.until(lambda: self.head() == expected)


# A stand-in for the rclone executable: the subset of commands the worker runs,
# against directories beside the script. It records every call's arguments and
# environment. Remotes come from --config and RCLONE_CONFIG_* like rclone's;
# a crypt remote seals names and contents and reports no hashes.
FAKE_RCLONE = r'''
import base64, configparser, hashlib, json, os, sys, time
from pathlib import Path

HERE = Path(__file__).resolve().parent
argv = sys.argv[1:]
with open(HERE / 'calls.jsonl', 'a') as log:
    log.write(json.dumps({'argv': argv, 'env': dict(os.environ)}) + '\n')
VALUED = {'--config', '--include', '--include-from', '--files-from', '--max-size', '--count'}
options, positional, index = {}, [], 0
while index < len(argv):
    if argv[index] in VALUED:
        options[argv[index]] = argv[index + 1]
        index += 2
    elif argv[index].startswith('--'):
        key, _, value = argv[index].partition('=')
        options[key] = value or True
        index += 1
    else:
        positional.append(argv[index])
        index += 1
command, args = positional[0], positional[1:]
HASHES = {'local': ('md5', 'sha256'), 'drive': ('md5', 'sha1', 'sha256'), 'dropbox': ('dropbox',)}

def fail(message, code=1):
    print(message, file=sys.stderr)
    sys.exit(code)

def control(name, target):
    path = HERE / name
    return path.exists() and path.read_text() in target

def digest(kind, data):
    return hashlib.new({'dropbox': 'sha1'}.get(kind, kind), data).hexdigest()

def seal(name):
    return 'enc-' + base64.urlsafe_b64encode(name.encode()).decode().rstrip('=')

def unseal(name):
    return base64.urlsafe_b64decode(name[4:] + '=' * (-len(name[4:]) % 4)).decode()

def remotes():
    found = {}
    parser = configparser.ConfigParser(interpolation=None)
    if options.get('--config'):
        parser.read(options['--config'])
    for section in parser.sections():
        found[section] = dict(parser[section])
    for key, value in os.environ.items():
        if key.startswith('RCLONE_CONFIG_'):
            name, _, option = key[len('RCLONE_CONFIG_'):].partition('_')
            found.setdefault(name.lower(), {})[option.lower()] = value
    return found

def locate(spec):
    """(storage path, crypt, backend type) of a remote or local path."""
    if ':' not in spec or spec.startswith('/'):
        return Path(spec), False, 'local'
    name, _, path = spec.partition(':')
    remote = remotes().get(name) or fail("didn't find section in config file")
    if remote.get('type') == 'crypt':
        if not remote.get('password', '').startswith('fakeobscured'):
            fail('crypt password is not obscured')
        base = locate(remote['remote'])[0]
        return base.joinpath(*[seal(p) for p in path.split('/') if p]), True, 'crypt'
    if remote.get('type') == 'local':
        return Path(path), False, 'local'
    return HERE / 'remotes' / name / path, False, remote.get('type')

def read(spec):
    path, crypt, _ = locate(spec)
    if not path.is_file():
        fail('object not found', 3)
    data = path.read_bytes()
    return data[len(b'FAKECRYPT'):][::-1] if crypt else data

def write(spec, data):
    path, crypt, _ = locate(spec)
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(b'FAKECRYPT' + data[::-1] if crypt else data)

def join(root, name):
    return root + name if root.endswith(':') else root + '/' + name

def entry(spec, name):
    data, kind = read(spec), locate(spec)[2]
    item = {'Path': name, 'Name': name, 'Size': len(data), 'IsDir': False}
    if options.get('--hash') and HASHES.get(kind):
        item['Hashes'] = {k: digest(k, data) for k in HASHES[kind]}
    return item

if command == 'lsjson':
    if options.get('--stat'):
        print(json.dumps(entry(args[0], args[0].rpartition(':')[2].rpartition('/')[2])))
        sys.exit(0)
    path, crypt, _ = locate(args[0])
    if not path.is_dir():
        fail('directory not found', 3)
    names = [unseal(p.name) if crypt else p.name for p in sorted(path.iterdir()) if p.is_file()]
    print(json.dumps([entry(join(args[0], name), name) for name in names]))
elif command == 'copyto':
    data = read(args[0])
    if control('slow-copyto', args[1]):
        (HERE / 'copyto.pid').write_text(str(os.getpid()))
        time.sleep(60)
    if control('fail-copyto', args[1]):
        write(args[1], data[:len(data) // 2])
        fail('upload interrupted')
    if control('corrupt-copyto', args[1]):
        data = bytes([data[0] ^ 1]) + data[1:]
    write(args[1], data)
elif command == 'moveto':
    source, target = locate(args[0])[0], locate(args[1])[0]
    if not source.is_file():
        fail('object not found', 3)
    source.replace(target)
elif command == 'deletefile':
    path = locate(args[0])[0]
    if not path.is_file():
        fail('object not found', 3)
    path.unlink()
elif command == 'cat':
    data = read(args[0])
    sys.stdout.buffer.write(data[:int(options['--count'])] if '--count' in options else data)
elif command == 'copy':
    # As rclone 1.74 does, refuse --files-from beside any other filter.
    if '--files-from' in options and len({'--include', '--include-from', '--max-size'} & set(options)):
        fail('the usage of --files-from overrides all other filters', 1)
    limit = int(options['--max-size'].rstrip('M')) * 1024 * 1024
    target = Path(args[1])
    target.mkdir(parents=True, exist_ok=True)
    rules = Path(options['--include-from']).read_text().split()
    assert all(rule.startswith('/') for rule in rules)
    for name in (rule[1:] for rule in rules):
        if locate(join(args[0], name))[0].is_file() and len(read(join(args[0], name))) <= limit:
            (target / name).write_bytes(read(join(args[0], name)))
elif command == 'hashsum':
    print(digest(args[0], Path(args[1]).read_bytes()) + '  ' + Path(args[1]).name)
elif command == 'check':
    assert options.get('--download') and options.get('--one-way')
    name = options['--include'].lstrip('/')
    if (Path(args[0]) / name).read_bytes() != read(join(args[1], name)):
        fail('1 differences found', 1)
elif command == 'obscure':
    secret = sys.stdin.readline().rstrip('\n')
    print('fakeobscured' + base64.urlsafe_b64encode(secret[::-1].encode()).decode().rstrip('='))
elif command == 'listremotes':
    for name, remote in sorted(remotes().items()):
        print(f'{name + ":":<16} {remote.get("type", "")}')
else:
    fail('unknown command ' + command, 2)
'''

# A stand-in for the application's capture command (capture protocol v1).
FAKE_CAPTURE = r'''
import argparse, json, os, sys
from pathlib import Path
parser = argparse.ArgumentParser()
for option in ('--out', '--log', '--mode', '--purge'):
    parser.add_argument(option)
args = parser.parse_args()
def log(event):
    with open(args.log, 'a') as output:
        output.write(event + '\n')
log('started')
if args.mode == 'eof':
    sys.exit(1)
out = Path(args.out)
(out / 'blocks').mkdir(parents=True)
(out / 'blocks' / 'ab').write_bytes(bytes(range(256)))
(out / 'admin.token').write_text('synthetic-recovery-key')
os.chmod(out / 'admin.token', 0o600)
(out / 'capture.json').write_text(json.dumps({'version': 1}))
print('snapshot 00000003-0000001B-1 purge ' + args.purge, flush=True)
line = sys.stdin.readline()
if line != 'done\n':
    log('aborted')
    sys.exit(3)
log('done')
print('nope' if args.mode == 'no-ok' else 'ok', flush=True)
'''

FAKE_DUMP = r'''
import sys
if '--fail' in sys.argv:
    sys.exit(2)
print('-- synthetic dump of ' + sys.argv[1])
print('CREATE TABLE notes (id text);')
'''


def script(path, text):
    path.write_text('#!' + sys.executable + '\n' + text)
    path.chmod(0o700)
    return path


def fake_rclone(directory):
    directory.mkdir()
    return script(directory / 'rclone', FAKE_RCLONE)


def rclone_calls(directory):
    try:
        calls = [json.loads(line) for line in (directory / 'calls.jsonl').read_text().splitlines()]
    except FileNotFoundError:
        return []
    # What the fake's own interpreter adds: macOS's __CF_USER_TEXT_ENCODING and
    # Python's C locale coercion (PEP 538).
    return [dict(call, env={k: v for k, v in call['env'].items() if not k.startswith('__CF') and k != 'LC_CTYPE'})
            for call in calls]


def fake_obscure(secret):
    return 'fakeobscured' + base64.urlsafe_b64encode(secret[::-1].encode()).decode().rstrip('=')


def unseal(name):
    return base64.urlsafe_b64decode(name[4:] + '=' * (-len(name[4:]) % 4)).decode()


class ExportTest(unittest.TestCase):
    """A notebook's bare Git export, which ForgetMeNote writes and the worker
    only reads, built from a working repository standing in for the app."""
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix='notebook-snapshot-test-')
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name).resolve()
        self.repo = self.root / 'notebook'
        self.repo.mkdir()
        git(self.repo, 'init', '--quiet', '-b', 'main')
        git(self.repo, 'config', 'user.name', 'Synthetic test')
        git(self.repo, 'config', 'user.email', 'test@localhost')
        self.initial = self.commit('initial')
        self.export = self.new_export('notes')

    def new_export(self, name):
        export = self.root / 'exports' / (name + '.git')
        export.parent.mkdir(exist_ok=True)
        git(self.root, 'init', '--quiet', '--bare', '-b', 'main', str(export))
        git(self.repo, 'push', '--quiet', str(export), 'HEAD:refs/heads/main')
        return export

    def commit(self, text):
        (self.repo / 'note.md').write_text(text)
        git(self.repo, 'add', 'note.md')
        git(self.repo, 'commit', '--quiet', '-m', text)
        return git(self.repo, 'rev-parse', 'HEAD').stdout.strip()

    def publish(self, commit, export=None):
        git(self.repo, 'push', '--quiet', '--force', str(export or self.export), commit + ':refs/heads/main')

    def rewrite(self, base, text, purge=None):
        """A commit on base, as a purge's rewrite (with its trailer) or a
        database restored from before it (without one)."""
        git(self.repo, 'checkout', '--quiet', '--detach', base)
        (self.repo / 'note.md').write_text(text)
        git(self.repo, 'commit', '--quiet', '-am', 'rewritten', *(['-m', 'Gitnotes-Purge: ' + purge] if purge else []))
        return git(self.repo, 'rev-parse', 'HEAD').stdout.strip()

    def tree(self, path):
        return {str(p): (p.stat().st_size, p.stat().st_mtime_ns) for p in sorted(path.rglob('*'))}


class SnapshotTest(ExportTest):
    def setUp(self):
        super().setUp()
        self.nas = self.root / 'nas'
        self.nas.mkdir()
        self.settings = dict(mode='snapshot', repository=str(self.export), state_dir=str(self.root / 'state'),
                             destination={'folder': str(self.nas)}, min_interval_seconds=0, push_timeout_seconds=30)
        self.config = self.root / 'snapshot.json'
        self.configure()

    def configure(self, **changes):
        self.settings.update(changes)
        self.config.write_text(json.dumps(self.settings))

    def run_worker(self, *flags):
        return subprocess.run([sys.executable, str(SCRIPT), '--config', str(self.config), *flags],
                              capture_output=True, text=True, timeout=60)

    def once(self):
        result = self.run_worker('--once')
        self.assertEqual(result.returncode, 0, result.stderr)

    def backup(self):
        backup = watcher.SnapshotBackup(self.settings)
        backup.state_dir.mkdir(mode=0o700, exist_ok=True)
        return backup

    def snapshots(self, folder=None):
        """Complete snapshots, oldest first."""
        folder = folder or self.nas
        names = [p.name for p in folder.glob('*.bundle') if p.with_suffix('.json').exists()]
        return sorted(names, key=lambda name: (self.manifest(name, folder)['created'], name))

    def manifest(self, name, folder=None):
        return json.loads(((folder or self.nas) / name).with_suffix('.json').read_text())

    def status(self):
        return json.loads((self.root / 'state/status.json').read_text())

    def test_snapshot_after_a_commit_clones_and_matches_its_manifest(self):
        second = self.commit('second revision')
        self.publish(second)
        (self.export / watcher.PURGE_FLAG).write_text('{"purge": "p0"}')
        before = self.tree(self.export)
        self.once()
        name, = self.snapshots()
        self.assertRegex(name, r'^\d{8}T\d{6}Z-' + second[:12] + r'\.bundle$')
        manifest = self.manifest(name)
        bundle = self.nas / name
        self.assertEqual(manifest, dict(format=1, kind='notebook', notebook='notes', commit=second,
                                        created=manifest['created'], size=bundle.stat().st_size,
                                        sha256=watcher.sha256_file(bundle), purges=[], worker=watcher.SNAPSHOT_WORKER))
        self.assertEqual(watcher.created_time(manifest).replace(microsecond=0), watcher.stamp_time(name))
        # Restores without ForgetMeNote, and Import notebook needs the bundle's HEAD.
        clone = self.root / 'restored'
        subprocess.run(['git', 'clone', '--quiet', str(bundle), str(clone)], check=True, capture_output=True)
        self.assertEqual((clone / 'note.md').read_text(), 'second revision')
        self.assertEqual(git(clone, 'rev-parse', 'HEAD').stdout.strip(), second)
        self.assertEqual(git(clone, 'rev-list', '--count', 'HEAD').stdout.strip(), '2')
        self.assertIn(second + ' HEAD', git(self.root, 'bundle', 'list-heads', str(bundle)).stdout)
        # The export is read, never written; the purge flag belongs to the push worker.
        self.assertEqual(self.tree(self.export), before)
        self.assertFalse(list(self.nas.glob('*.partial')))
        self.assertFalse((self.root / 'state/staging').exists())
        status = self.status()
        self.assertEqual((status['status'], status['last_snapshot']['name'], status['last_pushed_commit']),
                         ('synced', name, second))

    def test_partial_uploads_never_count_and_unchanged_exports_are_skipped(self):
        self.once()
        first, = self.snapshots()
        second = self.commit('second revision')
        self.publish(second)
        # A complete manifest beside an unfinished upload is no snapshot of second.
        stamp = (watcher.utc_now() - watcher.timedelta(minutes=1)).strftime(watcher.STAMP)
        fresh = f'{stamp}-{second[:12]}'
        (self.nas / (fresh + '.bundle.partial')).write_bytes(b'unfinished')
        (self.nas / (fresh + '.json')).write_text(json.dumps({**self.manifest(first), 'commit': second}))
        old = (watcher.utc_now() - watcher.timedelta(days=3)).strftime(watcher.STAMP)
        leftovers = [f'{old}-{"a" * 12}.bundle.partial', f'{old}-{"b" * 12}.bundle', f'{old}-{"c" * 12}.json.partial']
        for leftover in leftovers:
            (self.nas / leftover).write_bytes(b'interrupted')
        (self.nas / 'README.txt').write_text('not a snapshot')
        self.once()
        self.assertEqual(len(self.snapshots()), 2)
        self.assertEqual(self.manifest(self.snapshots()[-1])['commit'], second)
        # Only day-old leftovers matching the snapshot names are removed.
        for leftover in leftovers:
            self.assertFalse((self.nas / leftover).exists(), leftover)
        self.assertTrue((self.nas / (fresh + '.bundle.partial')).exists())
        self.assertTrue((self.nas / 'README.txt').exists())
        listing = sorted(p.name for p in self.nas.iterdir())
        self.once()
        self.assertEqual(sorted(p.name for p in self.nas.iterdir()), listing, 'an unchanged export was uploaded again')

    def test_retention_keeps_the_newest_verified_snapshots(self):
        self.configure(retention={'newest': 2, 'daily': 0, 'weekly': 0})
        commits = [self.initial]
        self.once()
        for number in range(3):
            commits.append(self.commit(f'revision {number}'))
            self.publish(commits[-1])
            self.once()
        self.assertEqual([self.manifest(n)['commit'] for n in self.snapshots()], commits[-2:])
        self.configure(retention={'newest': 0, 'daily': 0, 'weekly': 0})
        commits.append(self.commit('latest'))
        self.publish(commits[-1])
        self.once()
        self.assertEqual([self.manifest(n)['commit'] for n in self.snapshots()], commits[-1:])

    def test_retention_selects_newest_daily_and_weekly(self):
        now = watcher.datetime(2026, 11, 27, 12, tzinfo=watcher.timezone.utc)  # a Friday
        def snapshot(days, hours=0):
            moment = now - watcher.timedelta(days=days, hours=hours)
            return watcher.Snapshot(moment.strftime(watcher.STAMP), {}, moment)
        snapshots = sorted([snapshot(0), snapshot(0, 2), snapshot(1), snapshot(1, 3), snapshot(3), snapshot(9), snapshot(40)],
                           key=lambda s: s.name, reverse=True)
        names = [s.name for s in snapshots]
        self.assertEqual(watcher.retained(snapshots, now, newest=0), {names[0]})
        self.assertEqual(watcher.retained(snapshots, now, newest=3), set(names[:3]))
        # Newest per UTC day for two days: today and yesterday.
        self.assertEqual(watcher.retained(snapshots, now, newest=1, daily=2), {names[0], names[2]})
        # Newest per ISO week for two weeks: this week (Monday on) and last.
        self.assertEqual(watcher.retained(snapshots, now, newest=1, weekly=2), {names[0], names[5]})
        with self.assertRaises(ValueError):
            watcher.retention_rules({'newest': -1}, newest=1, daily=0, weekly=0)
        with self.assertRaises(ValueError):
            watcher.retention_rules({'monthly': 3}, newest=1, daily=0, weekly=0)

    def test_failed_upload_keeps_earlier_snapshots(self):
        self.configure(retention={'newest': 1, 'daily': 0, 'weekly': 0})
        backup = self.backup()
        self.assertTrue(backup.once())
        first, = self.snapshots()
        second = self.commit('second revision')
        self.publish(second)
        real = watcher.Folder.upload
        def manifest_fails(folder, local, name, sha256):
            if name.endswith('.json'):
                raise RuntimeError('destination full')
            return real(folder, local, name, sha256)
        with mock.patch.object(watcher.Folder, 'upload', manifest_fails):
            self.assertFalse(backup.once())
        self.assertEqual(self.snapshots(), [first], 'a failed upload removed or replaced a snapshot')
        self.assertEqual((self.status()['status'], self.status()['error']), ('error', 'destination full'))
        self.assertFalse(backup.staging.exists())
        self.assertTrue(backup.once())
        name, = self.snapshots()
        self.assertEqual(self.manifest(name)['commit'], second)

    def test_purge_deletes_only_older_snapshots_without_it_once_the_new_one_verifies(self):
        backup = self.backup()
        self.assertTrue(backup.once())
        leaked = self.commit('the key is sk-live-0123')
        self.publish(leaked)
        self.assertTrue(backup.once())
        before_purge = self.snapshots()
        self.assertEqual(len(before_purge), 2)
        (self.export / watcher.PURGE_FLAG).write_text('{"purge": "p1"}')
        purged = self.rewrite(self.initial, 'the key is [purged]', 'p1')
        self.publish(purged)
        with mock.patch.object(watcher.Folder, 'upload', side_effect=RuntimeError('offline')):
            self.assertFalse(backup.once())
        self.assertEqual(self.snapshots(), before_purge, 'snapshots were deleted before the new one verified')
        self.assertTrue(backup.once())
        name, = self.snapshots()
        self.assertEqual((self.manifest(name)['commit'], self.manifest(name)['purges']), (purged, ['p1']))
        later = self.commit('after the purge')
        self.publish(later)
        self.assertTrue(backup.once())
        self.assertEqual([self.manifest(n)['commit'] for n in self.snapshots()], [purged, later])
        self.assertTrue((self.export / watcher.PURGE_FLAG).exists(), 'the snapshot worker removed the purge flag')

    def test_history_rollback_without_a_new_purge_deletes_nothing(self):
        backup = self.backup()
        purged = self.rewrite(self.initial, 'purged', 'p1')
        self.publish(purged)
        self.assertTrue(backup.once())
        later = self.commit('after the purge')
        self.publish(later)
        self.assertTrue(backup.once())
        # A database restored from before the purge: older history, no trailer.
        restored = self.rewrite(self.initial, 'restored from an older backup')
        self.publish(restored)
        self.assertTrue(backup.once())
        self.assertEqual([self.manifest(n)['commit'] for n in self.snapshots()], [purged, later, restored])
        self.assertEqual(self.manifest(self.snapshots()[-1])['purges'], [])

    def test_verify_downloads_the_newest_snapshot_and_checks_it(self):
        self.once()
        result = self.run_worker('--verify')
        self.assertEqual(result.returncode, 0, result.stderr)
        name, = self.snapshots()
        bundle = self.nas / name
        original = bundle.read_bytes()
        bundle.write_bytes(original[:-1] + bytes([original[-1] ^ 1]))
        self.assertEqual(self.run_worker('--verify').returncode, 1)
        bundle.write_bytes(original)
        manifest = self.manifest(name)
        bundle.with_suffix('.json').write_text(json.dumps({**manifest, 'sha256': '0' * 64}))
        self.assertEqual(self.run_worker('--verify').returncode, 1)
        bundle.with_suffix('.json').unlink()
        self.assertEqual(self.run_worker('--verify').returncode, 1)

    def test_snapshots_wait_for_the_minimum_interval_unless_forced(self):
        self.configure(min_interval_seconds=3600)
        backup = self.backup()
        self.assertTrue(backup.once())
        self.publish(self.commit('second revision'))
        backup.observe()
        self.assertGreater(backup.retry_at, time.monotonic() + 3000)
        # A restarted worker keeps waiting; a forced snapshot does not.
        again = self.backup()
        self.assertGreater(again.retry_at, time.monotonic() + 3000)
        self.assertTrue(again.once())
        self.assertEqual(len(self.snapshots()), 2)

    def test_destinations_are_checked(self):
        for destination in [{'folder': 'relative/path'}, {'folder': str(self.export / 'refs')},
                            {'folder': str(self.root / 'state' / 'inside')}, {'rclone': 'box:', 'path': '../escape'},
                            {'rclone': ':local:/tmp'}, {'rclone': 'box:'}, {'folder': str(self.nas), 'rclone': 'box:'}]:
            with self.subTest(destination=destination), self.assertRaises(ValueError):
                watcher.SnapshotBackup({**self.settings, 'destination': destination})
        rclone = fake_rclone(self.root / 'fake')
        with self.assertRaises(ValueError):
            watcher.SnapshotBackup({**self.settings, 'destination': {'rclone': 'box:'}, 'rclone': str(rclone),
                                    'rclone_config': str(self.export / 'config')})
        # An unmounted folder fails the snapshot instead of being created.
        self.configure(destination={'folder': str(self.root / 'unmounted')})
        result = self.run_worker('--once')
        self.assertEqual(result.returncode, 1)
        self.assertFalse((self.root / 'unmounted').exists())

    def test_rclone_destination_verifies_uploads_by_the_remotes_hash(self):
        fake = self.root / 'fake'
        rclone = fake_rclone(fake)
        settings = self.root / 'rclone.conf'
        settings.write_text('[box]\ntype = dropbox\n')
        self.configure(destination={'rclone': 'box:', 'path': 'ForgetMeNote/notes'}, rclone=str(rclone),
                       rclone_config=str(settings))
        remote = fake / 'remotes/box/ForgetMeNote/notes'
        # A corrupted upload never becomes a snapshot.
        (fake / 'corrupt-copyto').write_text('.bundle')
        self.assertEqual(self.run_worker('--once').returncode, 1)
        self.assertFalse(remote.exists() and self.snapshots(remote))
        self.assertIn('does not match its checksum', self.status()['error'])
        (fake / 'corrupt-copyto').unlink()
        self.once()
        name, = self.snapshots(remote)
        self.assertEqual(self.manifest(name, remote)['commit'], self.initial)
        calls = [c['argv'] for c in rclone_calls(fake)]
        self.assertIn(['--config', str(settings), '--ask-password=false', 'hashsum', 'dropbox'],
                      [c[:5] for c in calls])
        self.assertTrue(all(c['env'].keys() <= {'PATH', 'HOME'} for c in rclone_calls(fake)))
        result = self.run_worker('--verify')
        self.assertEqual(result.returncode, 0, result.stderr)


class FakeGateway:
    """The app's worker API: bindings, leases, lease checks, receipts and drains."""
    TOKEN = 'private-worker-fixture-token-0123456789'

    def __init__(self, test, bindings):
        self.bindings, self.calls, self.revoke_after = bindings, [], None
        gateway = self
        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass
            def reply(self, code, value):
                body = json.dumps(value).encode()
                self.send_response(code)
                self.send_header('Content-Length', str(len(body)))
                self.end_headers()
                self.wfile.write(body)
            def do_GET(self):
                test.assertEqual(self.headers.get('Authorization'), 'Bearer ' + gateway.TOKEN)
                self.reply(200, gateway.bindings)
            def do_POST(self):
                test.assertEqual(self.headers.get('Authorization'), 'Bearer ' + gateway.TOKEN)
                path = self.path.rsplit('/', 1)[-1]
                value = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
                gateway.calls.append((path, value))
                checks = sum(1 for p, _ in gateway.calls if p == 'check')
                if path == 'lease':
                    self.reply(200, gateway.lease(value))
                elif path == 'check' and gateway.revoke_after is not None and checks > gateway.revoke_after:
                    self.reply(403, {'error': 'revoked'})
                else:
                    self.reply(200, {'valid': True})
        self.server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        thread = threading.Thread(target=self.server.serve_forever)
        thread.start()
        test.addCleanup(lambda: (self.server.shutdown(), thread.join(), self.server.server_close()))
        self.origin = f'http://127.0.0.1:{self.server.server_port}'

    def lease(self, request):
        binding = next(b for b in self.bindings if (b['project'], b.get('kind', 'github')) == (request['project'], request['kind']))
        return dict(id='lease-' + binding['kind'], project=binding['project'], kind=binding['kind'],
                    generation=binding['generation'], notebookGeneration=0, revision=request['revision'],
                    path=binding['path'], token='synthetic-' + binding['kind'] + '-access-token',
                    tokenExpires='2026-11-27T12:00:00Z', expires='2026-11-27T11:03:00Z',
                    encryption={'password': 'synthetic crypt passphrase'} if binding['encrypted'] else None)

    def made(self, path):
        return [value for p, value in self.calls if p == path]


class ManagedSnapshotTest(ExportTest):
    def setUp(self):
        super().setUp()
        self.team = self.new_export('team')
        self.fake = self.root / 'fake'
        self.rclone = fake_rclone(self.fake)
        self.github = dict(project='notes', generation=1, branch='main', request=1, blocked=True,
                           repository={'full_name': 'example/backup'})
        self.gdrive = dict(kind='gdrive', project='notes', generation=3, notebookGeneration=0, path='ForgetMeNote/notes-k3x9q2',
                           encrypted=True, enabled=True, blocked=False, request=1)
        self.dropbox = dict(kind='dropbox', project='team', generation=1, notebookGeneration=0, path='ForgetMeNote/team',
                            encrypted=False, enabled=True, blocked=False, request=1)

    def worker(self, bindings):
        self.gateway = FakeGateway(self, bindings)
        key = self.root / 'worker-token'
        key.write_text(FakeGateway.TOKEN)
        worker = watcher.ManagedWorker(dict(mode='managed', gateway=self.gateway.origin, worker='local', token_file=str(key),
                                            state_dir=str(self.root / 'state'), rclone=str(self.rclone),
                                            notebooks={'notes': str(self.export), 'team': str(self.team)}))
        worker.state_dir.mkdir(mode=0o700)
        return worker

    def test_drive_snapshots_lease_one_run_remotes_and_report_receipts(self):
        worker = self.worker([self.github, self.gdrive, self.dropbox])
        with mock.patch.dict(os.environ, {'RCLONE_CONFIG_AMBIENT_TYPE': 'local', 'HTTPS_PROXY': 'http://proxy.example.com'}):
            self.assertTrue(worker.tick(once=True))
        self.assertEqual(set(worker.active), {('notes', 'github'), ('notes', 'gdrive'), ('team', 'dropbox')})
        self.assertEqual(self.gateway.made('lease'), [
            dict(project='notes', kind='gdrive', generation=3, revision=self.initial),
            dict(project='team', kind='dropbox', generation=1, revision=self.initial)])
        receipts = self.gateway.made('receipt')
        for receipt, kind, project, generation in zip(receipts, ['gdrive', 'dropbox'], ['notes', 'team'], [3, 1]):
            self.assertEqual(set(receipt), {'lease', 'project', 'kind', 'generation', 'revision', 'snapshot'})
            self.assertEqual((receipt['lease'], receipt['project'], receipt['kind'], receipt['generation'], receipt['revision']),
                             ('lease-' + kind, project, kind, generation, self.initial))
            snapshot = receipt['snapshot']
            self.assertEqual(set(snapshot), {'name', 'sha256', 'size', 'kept', 'deleted'})
            self.assertRegex(snapshot['name'], r'^\d{8}T\d{6}Z-' + self.initial[:12] + r'\.bundle$')
            self.assertEqual((snapshot['kept'], snapshot['deleted']), (1, []))
        self.assertTrue(self.gateway.made('check'))
        # Secrets travel only in the environment of each rclone run.
        secrets = ['synthetic-gdrive-access-token', 'synthetic-dropbox-access-token', 'synthetic crypt passphrase']
        calls = rclone_calls(self.fake)
        for call in calls:
            self.assertFalse(any(secret in ' '.join(call['argv']) for secret in secrets), call['argv'])
            self.assertNotIn('RCLONE_CONFIG_AMBIENT_TYPE', call['env'])
            self.assertNotIn('HTTPS_PROXY', call['env'])
            self.assertNotIn('synthetic crypt passphrase', json.dumps(call['env']))
        drive = next(c['env'] for c in calls if 'RCLONE_CONFIG_FMNCRYPT_TYPE' in c['env'])
        self.assertEqual({k: v for k, v in drive.items() if k != 'RCLONE_CONFIG_FMNSNAP_TOKEN'}, {
            'PATH': '/usr/bin:/bin', 'HOME': str(self.root / 'state/notes.gdrive'),
            'RCLONE_CONFIG_FMNSNAP_TYPE': 'drive', 'RCLONE_CONFIG_FMNSNAP_SCOPE': 'drive.file',
            'RCLONE_CONFIG_FMNSNAP_USE_TRASH': 'false', 'RCLONE_CONFIG_FMNCRYPT_TYPE': 'crypt',
            'RCLONE_CONFIG_FMNCRYPT_REMOTE': 'fmnsnap:ForgetMeNote/notes-k3x9q2',
            'RCLONE_CONFIG_FMNCRYPT_PASSWORD': fake_obscure('synthetic crypt passphrase'),
            'RCLONE_CONFIG_FMNCRYPT_FILENAME_ENCRYPTION': 'standard',
            'RCLONE_CONFIG_FMNCRYPT_DIRECTORY_NAME_ENCRYPTION': 'true'})
        self.assertEqual(json.loads(drive['RCLONE_CONFIG_FMNSNAP_TOKEN']), {
            'access_token': 'synthetic-gdrive-access-token', 'token_type': 'Bearer', 'expiry': '2026-11-27T12:00:00Z'})
        box = next(c['env'] for c in calls if c['env'].get('RCLONE_CONFIG_FMNSNAP_TYPE') == 'dropbox')
        self.assertEqual(set(box), {'PATH', 'HOME', 'RCLONE_CONFIG_FMNSNAP_TYPE', 'RCLONE_CONFIG_FMNSNAP_TOKEN'})
        # The encrypted drive holds only sealed names; the other restores directly.
        sealed = sorted(p.name for p in (self.fake / 'remotes/fmnsnap/ForgetMeNote/notes-k3x9q2').iterdir())
        self.assertTrue(sealed and all(n.startswith('enc-') for n in sealed), sealed)
        self.assertEqual(sorted(unseal(n).rsplit('.', 1)[1] for n in sealed), ['bundle', 'json'])
        plain = self.fake / 'remotes/fmnsnap/ForgetMeNote/team'
        bundle = plain / receipts[1]['snapshot']['name']
        subprocess.run(['git', 'clone', '--quiet', str(bundle), str(self.root / 'restored')], check=True, capture_output=True)
        self.assertEqual((self.root / 'restored/note.md').read_text(), 'initial')
        for path in (self.root / 'state').rglob('*'):
            self.assertFalse(path.name.startswith('rclone-'), path)
            if path.is_file():
                self.assertFalse(any(secret in path.read_text(errors='replace') for secret in secrets), path)
        # Nothing new is leased until something changes or someone asks.
        leases = len(self.gateway.made('lease'))
        self.assertTrue(worker.tick())
        self.assertEqual(len(self.gateway.made('lease')), leases)
        self.gdrive['request'] = 2
        self.assertTrue(worker.tick())
        self.assertEqual(self.gateway.made('receipt')[-1]['snapshot'], receipts[0]['snapshot'])
        # A handoff drains the drive snapshot alone; GitHub's drain has no kind.
        self.gdrive.update(generation=4, handoff='b' * 32)
        self.assertTrue(worker.tick())
        self.assertEqual(self.gateway.made('drain'), [dict(project='notes', kind='gdrive', generation=4, handoff='b' * 32)])
        self.assertEqual(set(worker.active), {('notes', 'github'), ('team', 'dropbox')})
        self.github.update(generation=2, handoff='a' * 32)
        drains = len(self.gateway.made('drain'))
        self.assertTrue(worker.tick())
        # Each tick acknowledges every pending handoff again.
        self.assertEqual(self.gateway.made('drain')[drains:], [
            dict(project='notes', generation=2, handoff='a' * 32),
            dict(project='notes', kind='gdrive', generation=4, handoff='b' * 32)])
        self.assertEqual(set(worker.active), {('team', 'dropbox')})

    def test_drive_leases_must_match_the_binding_and_revision(self):
        binding = dict(kind='gdrive', project='notes', generation=3, notebookGeneration=0, path='ForgetMeNote/notes', encrypted=True)
        lease = dict(id='lease', project='notes', kind='gdrive', generation=3, notebookGeneration=0, revision='a' * 40,
                     path='ForgetMeNote/notes', token='synthetic', tokenExpires='2026-11-27T12:00:00.123456789Z',
                     expires='2026-11-27T11:03:00Z', encryption={'password': 'synthetic'})
        watcher.validate_drive_lease(lease, binding, 'a' * 40)
        for change in [dict(kind='dropbox'), dict(project='other'), dict(generation=2), dict(notebookGeneration=1),
                       dict(revision='b' * 40), dict(path='ForgetMeNote/other'), dict(token=''), dict(token='a\nb'),
                       dict(tokenExpires='soon'), dict(tokenExpires='2026-11-27T12:00:00'), dict(encryption=None),
                       dict(encryption={'password': ''}), dict(id=None)]:
            with self.subTest(change=change), self.assertRaises(ValueError):
                watcher.validate_drive_lease({**lease, **change}, binding, 'a' * 40)
        with self.assertRaises(ValueError):
            watcher.validate_drive_lease({k: v for k, v in lease.items() if k != 'encryption'}, binding, 'a' * 40)
        # An unencrypted binding never takes a password, and a lease never redirects the path.
        with self.assertRaises(ValueError):
            watcher.validate_drive_lease(lease, {**binding, 'encrypted': False}, 'a' * 40)
        for path in ['../outside', '/absolute', '']:
            with self.subTest(path=path), self.assertRaises(ValueError):
                watcher.validate_drive_lease({**lease, 'path': path}, {**binding, 'path': path}, 'a' * 40)

    def test_lease_revocation_cancels_a_drive_upload(self):
        self.gdrive.update(encrypted=False)
        worker = self.worker([self.gdrive])
        self.gateway.revoke_after = 1
        (self.fake / 'slow-copyto').write_text('.bundle')
        started = time.monotonic()
        self.assertFalse(worker.tick(once=True))
        self.assertLess(time.monotonic() - started, 30)
        pid = int((self.fake / 'copyto.pid').read_text())
        with self.assertRaises(ProcessLookupError):
            os.kill(pid, 0)
        self.assertEqual(self.gateway.made('receipt')[-1], dict(
            lease='lease-gdrive', project='notes', kind='gdrive', generation=3, revision=self.initial, error='upload failed'))
        self.assertFalse(list((self.fake / 'remotes').rglob('*.bundle')))
        status = json.loads((self.root / 'state/notes.gdrive/status.json').read_text())
        self.assertEqual(status['status'], 'error')
        self.assertNotIn('synthetic-gdrive-access-token', json.dumps(status))


class InstanceTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix='instance-snapshot-test-')
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name).resolve()
        self.host = self.root / 'srv'
        (self.host / 'data/.capture').mkdir(parents=True)
        (self.host / '.env').write_text('SECRET=synthetic-environment-secret\n')
        (self.host / '.env').chmod(0o600)
        (self.host / 'secrets').mkdir()
        (self.host / 'secrets/app.key').write_text('synthetic-app-key')
        (self.host / 'secrets/app.key').chmod(0o640)
        (self.host / 'secrets/current').symlink_to('app.key')
        (self.host / 'instance.json').write_text('{}')
        self.capture_dir = self.host / 'data/.capture/current'
        self.log = self.root / 'capture.log'
        self.capture = script(self.root / 'capture.py', FAKE_CAPTURE)
        self.dump = script(self.root / 'dump.py', FAKE_DUMP)
        self.fake = self.root / 'fake'
        self.rclone = fake_rclone(self.fake)
        self.rclone_config = self.root / 'rclone.conf'
        self.rclone_config.write_text('[vault]\ntype = drive\n\n[plain]\ntype = drive\n\n'
                                      f'[secret]\ntype = crypt\nremote = vault:backups\npassword = {fake_obscure("synthetic")}\n')
        self.settings = dict(
            mode='instance', state_dir=str(self.root / 'state'), at='04:00', capture_dir=str(self.capture_dir),
            include=[str(self.host / '.env'), str(self.host / 'secrets'), str(self.host / 'instance.json')],
            destination={'rclone': 'secret:', 'path': 'ForgetMeNote/instance'}, rclone=str(self.rclone),
            rclone_config=str(self.rclone_config), retention={'newest': 10, 'daily': 14, 'weekly': 8})
        self.configure()

    def configure(self, mode='ok', purge='-', dump_fails=False, **changes):
        self.settings.update(changes, capture_command=[
            str(self.capture), '--out', str(self.capture_dir), '--log', str(self.log), '--mode', mode, '--purge', purge],
            dump_command=[str(self.dump), '--snapshot={snapshot}', *(['--fail'] if dump_fails else [])])
        (self.root / 'instance.json').write_text(json.dumps(self.settings))

    def backup(self):
        backup = watcher.InstanceBackup(self.settings)
        backup.state_dir.mkdir(mode=0o700, exist_ok=True)
        return backup

    def snapshots(self):
        backup = self.backup()
        with backup.transport() as transport:
            return [(s.name, s.manifest) for s in backup.store(transport).scan()[2]][::-1]

    def events(self):
        return self.log.read_text().split() if self.log.exists() else []

    def test_capture_uploads_an_encrypted_archive_of_the_whole_instance(self):
        backup = self.backup()
        self.assertTrue(backup.once(), backup.error)
        self.assertEqual(self.events(), ['started', 'done'])
        (name, manifest), = self.snapshots()
        self.assertRegex(name, r'^\d{8}T\d{6}Z-instance-[a-f0-9]{12}\.tar\.gz$')
        self.assertEqual(set(manifest), {'format', 'kind', 'created', 'size', 'sha256', 'purge', 'purges', 'worker'})
        self.assertEqual((manifest['kind'], manifest['purge'], manifest['purges']), ('instance', None, []))
        # Only sealed names and contents reach the remote under the crypt remote.
        stored = [p for p in (self.fake / 'remotes/vault/backups').rglob('*') if p.is_file()]
        self.assertTrue(stored and all(p.name.startswith('enc-') for p in stored))
        self.assertFalse(any(b'synthetic' in p.read_bytes() for p in stored))
        with backup.transport() as transport:
            transport.download(name, self.root / name)
        with tarfile.open(self.root / name) as tar:
            members = {m.name: m for m in tar.getmembers()}
            self.assertEqual(tar.extractfile('capture/admin.token').read(), b'synthetic-recovery-key')
            self.assertEqual(stat.S_IMODE(members['capture/admin.token'].mode), 0o600)
            self.assertEqual(tar.extractfile('capture/blocks/ab').read(), bytes(range(256)))
            self.assertIn(b'synthetic dump of --snapshot=00000003-0000001B-1', tar.extractfile('postgres.sql').read())
            include = 'include' + str(self.host)
            self.assertEqual(stat.S_IMODE(members[include + '/.env'].mode), 0o600)
            self.assertEqual(stat.S_IMODE(members[include + '/secrets/app.key'].mode), 0o640)
            self.assertTrue(members[include + '/secrets/current'].issym())
            self.assertEqual(members[include + '/secrets/current'].linkname, 'app.key')
            self.assertIn(include + '/instance.json', members)
        self.assertFalse(self.capture_dir.exists())
        self.assertFalse(backup.staging.exists())
        status = json.loads((self.root / 'state/status.json').read_text())
        self.assertEqual((status['status'], status['last_snapshot']['name']), ('synced', name))
        for text in [json.dumps(manifest), json.dumps(status)]:
            self.assertNotIn('synthetic-', text)
        result = subprocess.run([sys.executable, str(SCRIPT), '--config', str(self.root / 'instance.json'), '--verify'],
                                capture_output=True, text=True, timeout=60)
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_failed_captures_abort_and_leave_nothing(self):
        for mode, dump_fails, events, error in [
                ('no-ok', False, ['started', 'done'], 'did not confirm'),
                ('ok', True, ['started', 'aborted'], 'Database dump failed'),
                ('eof', False, ['started'], 'ended without reporting')]:
            with self.subTest(mode=mode, dump_fails=dump_fails):
                self.log.unlink(missing_ok=True)
                self.configure(mode=mode, dump_fails=dump_fails)
                backup = self.backup()
                self.assertFalse(backup.once())
                self.assertIn(error, backup.error)
                self.assertEqual(self.events(), events)
                self.assertFalse(self.capture_dir.exists())
                self.assertFalse(backup.staging.exists())
                self.assertEqual(self.snapshots(), [])
                self.assertEqual(json.loads((self.root / 'state/status.json').read_text())['status'], 'error')

    def test_unencrypted_destinations_are_refused(self):
        with self.assertRaises(ValueError):
            watcher.InstanceBackup({**self.settings, 'destination': {'folder': str(self.root)}})
        with self.assertRaises(ValueError):
            watcher.InstanceBackup({**self.settings, 'capture_dir': str(self.host / 'data')})
        self.configure(destination={'rclone': 'plain:', 'path': 'ForgetMeNote/instance'})
        backup = self.backup()
        self.assertFalse(backup.once())
        self.assertIn('crypt', backup.error)
        self.assertEqual(self.events(), [], 'captured for an unencrypted destination')
        self.assertFalse((self.fake / 'remotes/plain').exists())

    def test_symlinks_that_leave_an_included_tree_are_refused(self):
        (self.root / 'outside.txt').write_text('elsewhere')
        (self.host / 'secrets/escape').symlink_to(self.root / 'outside.txt')
        backup = self.backup()
        self.assertFalse(backup.once())
        self.assertIn('leaves', backup.error)
        self.assertFalse(self.capture_dir.exists())
        self.assertEqual(self.snapshots(), [])

    def test_a_new_purge_deletes_older_captures_but_a_rollback_does_not(self):
        for purge in ['p0', 'p0']:
            self.configure(purge=purge)
            self.assertTrue(self.backup().once())
        self.assertEqual([m['purge'] for _, m in self.snapshots()], ['p0', 'p0'])
        self.configure(purge='p1')
        self.assertTrue(self.backup().once())
        self.assertEqual([(m['purge'], m['purges']) for _, m in self.snapshots()], [('p1', ['p0', 'p1'])])
        # A database restored from between the purges reports the older one.
        self.configure(purge='p0')
        self.assertTrue(self.backup().once())
        self.assertEqual([m['purge'] for _, m in self.snapshots()], ['p1', 'p0'])

    def test_schedule_runs_daily_and_catches_up_a_missed_capture(self):
        backup = self.backup()
        self.assertLessEqual(backup.next_capture(), watcher.datetime.now())
        now = watcher.datetime.now()
        backup.last_success = watcher.datetime.now(watcher.timezone.utc).isoformat()
        due = backup.next_capture()
        self.assertGreater(due, now)
        self.assertEqual((due.hour, due.minute), (4, 0))
        self.assertLessEqual(due - now, watcher.timedelta(days=1))


RCLONE = os.environ.get('FMN_RCLONE')


@unittest.skipUnless(RCLONE or os.environ.get('FMN_REQUIRE_RCLONE') == '1',
                     'set FMN_RCLONE to the pinned rclone binary to run the real rclone tests')
class RealRcloneTest(ExportTest):
    """The real rclone, against local, alias and crypt remotes only: no network."""
    def setUp(self):
        if not RCLONE or not Path(RCLONE).is_file():
            self.fail('FMN_REQUIRE_RCLONE=1 needs FMN_RCLONE to name the rclone binary')
        super().setUp()
        self.storage = self.root / 'drive'
        self.storage.mkdir()
        self.state = self.root / 'state'
        self.state.mkdir()

    def test_environment_defined_crypt_remote_round_trips_snapshots(self):
        backup = watcher.SnapshotBackup(dict(repository=str(self.export), state_dir=str(self.state), min_interval_seconds=0),
                                        destination=False)
        @contextmanager
        def transport(commit):
            # The managed worker's own remotes, with a local folder for the drive.
            with watcher.Rclone(RCLONE, 'fmncrypt:', '', state_dir=self.state) as rclone:
                env = watcher.drive_environment('gdrive', 'unused', '2026-11-27T12:00:00Z', str(self.storage),
                                                rclone.obscure('synthetic crypt passphrase'))
                for unused in ['TOKEN', 'SCOPE', 'USE_TRASH']:
                    del env['RCLONE_CONFIG_FMNSNAP_' + unused]
                rclone.env.update(env, RCLONE_CONFIG_FMNSNAP_TYPE='local')
                yield rclone
        backup.transport = transport
        self.assertTrue(backup.once(), backup.error)
        second = self.commit('second revision')
        self.publish(second)
        self.assertTrue(backup.once(), backup.error)
        stored = sorted(self.storage.iterdir())
        self.assertEqual(len(stored), 4)
        for path in stored:
            self.assertNotIn('bundle', path.name)
            self.assertTrue(path.read_bytes().startswith(b'RCLONE\x00\x00'), path)
        self.assertTrue(backup.verify())
        backup.retention = dict(newest=1, daily=0, weekly=0)
        self.publish(self.commit('third revision'))
        self.assertTrue(backup.once(), backup.error)
        self.assertEqual(len(list(self.storage.iterdir())), 2)
        self.assertTrue(backup.verify())

    def test_uploads_are_verified_by_sha256(self):
        local = self.root / 'snapshot.bin'
        local.write_bytes(os.urandom(4096))
        env = {'RCLONE_CONFIG_FMNSNAP_TYPE': 'alias', 'RCLONE_CONFIG_FMNSNAP_REMOTE': str(self.storage)}
        with watcher.Rclone(RCLONE, 'fmnsnap:', 'nested/path', state_dir=self.state, env=env) as rclone:
            self.assertEqual(rclone.list(), {})
            rclone.upload(local, 'snapshot.bin', watcher.sha256_file(local))
            self.assertEqual(rclone.list(), {'snapshot.bin': 4096})
            self.assertEqual(rclone.read_manifests(['snapshot.bin']), {'snapshot.bin': local.read_bytes()})
            stored = self.storage / 'nested/path/snapshot.bin'
            stored.write_bytes(bytes([stored.read_bytes()[0] ^ 1]) + stored.read_bytes()[1:])
            with self.assertRaisesRegex(RuntimeError, 'checksum'):
                rclone.confirm(local, 'snapshot.bin', watcher.sha256_file(local))
            rclone.delete('snapshot.bin')
            self.assertEqual(rclone.list(), {})

    def test_instance_snapshots_reach_a_crypt_remote_and_refuse_a_plain_one(self):
        case = InstanceTest('test_capture_uploads_an_encrypted_archive_of_the_whole_instance')
        case.setUp()
        self.addCleanup(case.doCleanups)
        with watcher.Rclone(RCLONE, 'plain:', '', state_dir=self.state) as rclone:
            obscured = rclone.obscure('synthetic crypt passphrase')
        case.rclone_config.write_text(f'[plain]\ntype = alias\nremote = {self.storage}\n\n'
                                      f'[secret]\ntype = crypt\nremote = plain:vault\npassword = {obscured}\n')
        case.configure(rclone=RCLONE)
        backup = case.backup()
        with backup.transport() as rclone:
            self.assertEqual(rclone.remote_types(), {'plain': 'alias', 'secret': 'crypt'})
        self.assertTrue(backup.once(), backup.error)
        self.assertTrue(backup.verify())
        self.assertTrue(all(b'synthetic' not in p.read_bytes() for p in (self.storage / 'vault').rglob('*') if p.is_file()))
        case.configure(rclone=RCLONE, destination={'rclone': 'plain:', 'path': 'ForgetMeNote'})
        refused = case.backup()
        self.assertFalse(refused.once())
        self.assertIn('crypt', refused.error)


if __name__ == '__main__':
    unittest.main()
