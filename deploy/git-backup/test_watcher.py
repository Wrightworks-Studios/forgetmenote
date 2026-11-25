import fcntl
import json
import io
import os
from pathlib import Path
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


class ManagedContractTest(unittest.TestCase):
    def test_worker_rejects_gateway_without_transport_protection(self):
        with self.assertRaises(ValueError):
            watcher.Gateway({'gateway':'http://example.com','worker':'local','token_file':'/unreadable'})

    def test_managed_lease_rejects_arbitrary_destination(self):
        for remote in ['/tmp/remote.git','https://attacker.example/repo.git','https://token@github.com/example/backup.git']:
            with self.assertRaises(ValueError):
                watcher.validate_lease({'id':'lease','remote':remote,'branch':'main','revision':'a'*40,'project':'notes','generation':1},
                                       {'project':'notes','generation':1,'branch':'main','repository':{'full_name':'example/backup'}},'a'*40)

SCRIPT = Path(__file__).with_name('watcher.py')


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


if __name__ == '__main__':
    unittest.main()
