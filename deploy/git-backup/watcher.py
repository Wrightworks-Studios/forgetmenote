#!/usr/bin/env python3
"""Host-managed, one-way Git backup worker."""
import argparse
from contextlib import contextmanager
from datetime import datetime, timezone
import fcntl
import json
import logging
import os
import re
from pathlib import Path
import selectors
import shlex
import signal
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request


class Diverged(RuntimeError):
    pass


# ForgetMeNote writes this file into an export repository when a purge has
# rewritten the notebook's history (internal/gitexport). It is the only thing
# that permits replacing the remote branch instead of fast-forwarding it.
PURGE_FLAG = 'forgetmenote-purge.json'
# ForgetMeNote replaces the flag only while it holds this file's lock (flock),
# and the worker removes a spent flag only while it holds it.
PURGE_LOCK = 'forgetmenote-purge.lock'
PURGE_ID = re.compile(r'[A-Za-z0-9][A-Za-z0-9._-]{0,127}')
COMMIT = re.compile(r'[a-f0-9]{40}')


class Backup:
    def __init__(self, config, *, managed=False):
        self.repo = Path(config['repository']).resolve(strict=True)
        self.remote = config['remote']
        self.branch = config.get('branch', 'main')
        self.state_dir = Path(config['state_dir']).resolve()
        if self.state_dir.is_relative_to(self.repo):
            raise ValueError('Backup state must be outside the notebook repository')
        self.debounce = float(config.get('debounce_seconds', 10))
        self.max_delay = float(config.get('max_delay_seconds', 60))
        self.reconcile = float(config.get('reconcile_seconds', 300))
        self.retry = float(config.get('retry_seconds', 30))
        self.retry_max = float(config.get('retry_max_seconds', 300))
        self.timeout = float(config.get('push_timeout_seconds', 120))
        if min(self.debounce, self.max_delay, self.reconcile, self.retry, self.retry_max, self.timeout) <= 0:
            raise ValueError('Intervals must be positive')
        self.env = {'PATH': '/usr/bin:/bin', 'GIT_CONFIG_NOSYSTEM': '1',
                    'GIT_CONFIG_GLOBAL': '/dev/null', 'GIT_TERMINAL_PROMPT': '0',
                    'GIT_OPTIONAL_LOCKS': '0', 'GIT_NO_REPLACE_OBJECTS': '1'}
        if config.get('ssh_key'):
            self.env['GIT_SSH_COMMAND'] = shlex.join([
                '/usr/bin/ssh', '-F', '/dev/null', '-i', config['ssh_key'],
                '-o', 'IdentitiesOnly=yes', '-o', 'IdentityAgent=none',
                '-o', 'BatchMode=yes', '-o', 'StrictHostKeyChecking=yes',
                '-o', 'UserKnownHostsFile=' + config['known_hosts'],
                '-o', 'ConnectTimeout=10', '-o', 'ServerAliveInterval=15',
                '-o', 'ServerAliveCountMax=2'])
        elif not managed and not Path(self.remote).is_absolute():
            raise ValueError('Network backups require a dedicated SSH key and known_hosts')
        self.git('check-ref-format', 'refs/heads/' + self.branch)
        # A ForgetMeNote export is a bare repository, whose root is its Git
        # directory; an older notebook repository is a working tree.
        if self.git('rev-parse', '--is-bare-repository') == 'true':
            root = self.git('rev-parse', '--absolute-git-dir')
        else:
            root = self.git('rev-parse', '--show-toplevel')
        if Path(root).resolve() != self.repo:
            raise ValueError('Configure the exact notebook Git root, not a parent or subdirectory')
        self.git_dir = Path(self.git('rev-parse', '--absolute-git-dir'))
        self.git_identity = self.identity()
        self.observed = None
        self.pending_since = None
        self.last_change = 0
        self.retry_at = 0
        self.retry_delay = self.retry
        self.last_pushed = None
        self.last_success = None
        self.last_attempt = None
        self.error = None
        self.blocked = False
        self.pushed_purges = []
        try:
            previous = json.loads((self.state_dir / 'status.json').read_text())
            self.blocked = bool(previous.get('blocked'))
            # A purge authorizes one replacement; its flag may outlive the
            # push when the repository is mounted read-only.
            self.pushed_purges = [p for p in previous.get('pushed_purges', []) if isinstance(p, str)]
            if self.blocked:
                self.error = 'Backup branch diverged; resolve the destination and explicitly resume'
        except (FileNotFoundError, ValueError):
            pass

    def git(self, *args, timeout=15):
        result = subprocess.run([
            'git', '-C', str(self.repo), '-c', 'core.hooksPath=/dev/null',
            '-c', 'gc.auto=0', '-c', 'maintenance.auto=false',
            '-c', 'protocol.allow=never', '-c', 'protocol.file.allow=always',
            '-c', 'protocol.ssh.allow=always', *args], env=self.env,
            capture_output=True, text=True, timeout=timeout)
        if result.returncode:
            if any(text in result.stdout + result.stderr for text in ['non-fast-forward', 'fetch first', 'stale info']):
                raise Diverged('Backup branch diverged; resolve the destination and explicitly resume')
            raise RuntimeError((result.stderr or result.stdout).strip()[-2000:])
        return result.stdout.strip()

    def identity(self):
        stat = self.git_dir.stat()
        return stat.st_dev, stat.st_ino

    def head(self):
        if self.git('symbolic-ref', '-q', 'HEAD') != 'refs/heads/' + self.branch:
            raise RuntimeError('Notebook branch changed; refusing to back up a different branch')
        return self.git('rev-parse', '--verify', 'HEAD^{commit}')

    def status(self, status):
        data = dict(status=status, observed_commit=self.observed,
                    last_pushed_commit=self.last_pushed, last_success_at=self.last_success,
                    last_attempt_at=self.last_attempt, error=self.error,
                    updated_at=datetime.now(timezone.utc).isoformat(), blocked=self.blocked,
                    pushed_purges=self.pushed_purges)
        temporary = self.state_dir / 'status.json.tmp'
        with temporary.open('w') as output:
            json.dump(data, output, indent=2)
            output.write('\n')
            output.flush()
            os.fsync(output.fileno())
        temporary.replace(self.state_dir / 'status.json')

    def observe(self, force=False):
        revision = self.head()
        now = time.monotonic()
        if revision != self.observed:
            self.observed = revision
            self.last_change = now
            if self.pending_since is None:
                self.pending_since = now
            self.status('error' if self.error else 'pending')
        if force and self.pending_since is None:
            self.pending_since = now
            self.last_change = now - self.debounce

    def push(self):
        if self.blocked:
            return False
        # Pin exactly the revision sent, then recheck HEAD after completion.
        # A save during the push must never be mistaken for an uploaded commit.
        target = self.head()
        self.last_attempt = datetime.now(timezone.utc).isoformat()
        self.status('pushing')
        branch = 'refs/heads/' + self.branch
        purge = None
        try:
            with self.credentials(target):
                try:
                    self.git('push', '--porcelain', '--no-follow-tags', '--recurse-submodules=no',
                             '--', self.remote, target + ':' + branch, timeout=self.timeout)
                except Diverged:
                    # The one exception to fast-forward only: history an
                    # authorized purge rewrote replaces the commits it dropped.
                    replaced = self.remote_head()
                    purge = self.authorized_purge(target, replaced)
                    if purge is None:
                        raise
                    logging.warning('Replacing %s with %s for purge %s', replaced, target, purge)
                    self.git('push', '--porcelain', '--no-follow-tags', '--recurse-submodules=no',
                             '--force-with-lease=' + branch + ':' + replaced,
                             '--', self.remote, target + ':' + branch, timeout=self.timeout)
                remote = self.git('ls-remote', '--refs', '--', self.remote, branch)
                if remote.split() != [target, branch]:
                    raise Diverged('Remote branch does not match the pinned backup commit')
        except (RuntimeError, subprocess.TimeoutExpired) as error:
            self.error = str(error) if isinstance(error, RuntimeError) else 'Git push timed out'
            self.blocked = isinstance(error, Diverged)
            self.retry_at = time.monotonic() + self.retry_delay
            self.retry_delay = min(self.retry_max, self.retry_delay * 2)
            if self.pending_since is None:
                self.pending_since = time.monotonic()
            self.status('error')
            logging.error('Backup failed; no force-push or local changes made: %s', self.error)
            return False
        self.last_pushed = target
        self.last_success = datetime.now(timezone.utc).isoformat()
        self.pending_since = None
        self.retry_at = 0
        self.retry_delay = self.retry
        self.error = None
        if purge is None:
            # A remote that never received the dropped commits takes the
            # purge as a fast-forward, which spends its flag all the same.
            purge = self.flagged_purge(target)[0]
            if purge in self.pushed_purges:
                purge = None
        if purge is not None:
            self.pushed_purges = (self.pushed_purges + [purge])[-100:]
        self.status('synced')
        if purge is not None:
            self.remove_purge_flag(purge)
        logging.info('Pushed %s to %s', target, self.branch)
        self.observe()
        return True

    def remote_head(self):
        fields = self.git('ls-remote', '--refs', '--', self.remote, 'refs/heads/' + self.branch).split()
        return fields[0] if len(fields) == 2 and COMMIT.fullmatch(fields[0]) else None

    def read_purge_flag(self):
        try:
            with (self.git_dir / PURGE_FLAG).open('rb') as source:
                raw = source.read(64 * 1024 * 1024 + 1)
        except FileNotFoundError:
            return None
        except OSError as error:
            # A flag that cannot be read authorizes nothing.
            logging.warning('Ignoring the unreadable purge flag: %s', error.strerror)
            return None
        try:
            flag = json.loads(raw)
            purge, head, replaces = flag['purge'], flag['head'], flag['replaces']
        except (ValueError, TypeError, KeyError):
            return None
        if (len(raw) > 64 * 1024 * 1024 or not isinstance(purge, str) or not PURGE_ID.fullmatch(purge) or
                not isinstance(head, str) or not COMMIT.fullmatch(head) or not isinstance(replaces, list)):
            return None
        return purge, head, set(r for r in replaces if isinstance(r, str))

    def flagged_purge(self, target):
        """Return the purge the flag names and the commits it dropped, when
        target contains the purge's newest rewritten commit and that commit
        carries the trailer Gitnotes-Purge naming it; else (None, set())."""
        flag = self.read_purge_flag()
        if flag is None:
            return None, set()
        purge, head, replaces = flag
        try:
            marks = self.git('log', '-1', '--format=%(trailers:key=Gitnotes-Purge,valueonly)', head, '--')
            self.git('merge-base', '--is-ancestor', head, target)
        except (RuntimeError, subprocess.TimeoutExpired):
            return None, set()
        if purge not in [line.strip() for line in marks.splitlines()]:
            return None, set()
        return purge, replaces

    def authorized_purge(self, target, replaced):
        """Return the purge that authorizes replacing remote commit replaced
        with target, or None. ForgetMeNote's flag names the purge, its newest
        rewritten commit, which carries the trailer Gitnotes-Purge naming it,
        and every commit the rewrite dropped. Only a remote at one of those
        commits is replaced, only by a revision containing the rewritten
        commit, and only once per purge."""
        if replaced is None:
            return None
        purge, replaces = self.flagged_purge(target)
        if purge is None or purge in self.pushed_purges or replaced not in replaces:
            return None
        return purge

    def remove_purge_flag(self, purge, wait=30):
        # Under the exporter's lock, so a newer purge's flag written since
        # is never mistaken for this one: that flag stays.
        try:
            lock = os.open(self.git_dir / PURGE_LOCK, os.O_RDONLY | os.O_CLOEXEC | os.O_NONBLOCK)
        except FileNotFoundError:
            lock = None
        except OSError as error:
            logging.warning('Purge %s is pushed and spent; its flag remains: %s', purge, error.strerror)
            return
        try:
            deadline = time.monotonic() + wait
            while lock is not None:
                try:
                    fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
                    break
                except BlockingIOError:
                    if time.monotonic() >= deadline:
                        logging.warning('Purge %s is pushed and spent; its flag remains: the exporter holds its lock', purge)
                        return
                    time.sleep(0.05)
            flag = self.read_purge_flag()
            if flag is None or flag[0] != purge:
                return
            try:
                (self.git_dir / PURGE_FLAG).unlink()
            except OSError as error:
                logging.warning('Purge %s is pushed and spent; its flag remains: %s', purge, error.strerror)
        finally:
            if lock is not None:
                os.close(lock)

    @contextmanager
    def credentials(self, target):
        yield

    def watch(self):
        # Watch parent directories too: Git replaces loose refs by rename, and
        # HEAD usually remains a symbolic ref. Events are hints; Git is authoritative.
        watcher = subprocess.Popen([
            '/usr/bin/inotifywait', '--monitor', '--recursive', '--quiet',
            '--event', 'create,delete,close_write,moved_to,moved_from,move_self,delete_self',
            '--exclude', '/(objects|logs|hooks)(/|$)', '--format', '%w%f', str(self.git_dir)],
            stdout=subprocess.PIPE)
        selector = selectors.DefaultSelector()
        os.set_blocking(watcher.stdout.fileno(), False)
        selector.register(watcher.stdout, selectors.EVENT_READ)
        next_reconcile = time.monotonic()
        try:
            logging.info('Watching committed revisions; debounce=%ss max-delay=%ss reconciliation=%ss',
                         self.debounce, self.max_delay, self.reconcile)
            while True:
                now = time.monotonic()
                if watcher.poll() is not None:
                    raise RuntimeError('Filesystem watcher exited; restart is required')
                if now >= next_reconcile:
                    if self.identity() != self.git_identity:
                        raise RuntimeError('Git directory replaced; restart to reattach watches')
                    self.observe(force=True)
                    next_reconcile = now + self.reconcile
                deadline = next_reconcile
                if self.pending_since is not None and not self.blocked:
                    due = max(min(self.last_change + self.debounce,
                                  self.pending_since + self.max_delay), self.retry_at)
                    if time.monotonic() >= due:
                        self.push()
                        continue
                    deadline = min(deadline, due)
                events = selector.select(max(0, deadline - time.monotonic()))
                if events:
                    if not os.read(watcher.stdout.fileno(), 65536):
                        raise RuntimeError('Filesystem watcher closed its event stream')
                    self.observe()
        except Exception as error:
            self.error = str(error)
            self.status('error')
            raise
        finally:
            selector.close()
            if watcher.poll() is None:
                watcher.terminate()
            try:
                watcher.wait(timeout=5)
            except subprocess.TimeoutExpired:
                watcher.kill()
                watcher.wait()
            watcher.stdout.close()


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


class Gateway:
    def __init__(self, config):
        self.origin = config['gateway'].rstrip('/')
        url = urllib.parse.urlsplit(self.origin)
        if (url.scheme != 'https' and not (url.scheme == 'http' and url.hostname in ['localhost', '127.0.0.1'])) or not url.hostname or url.username or url.path or url.query or url.fragment:
            raise ValueError('Managed gateway must be an HTTPS origin (HTTP only on localhost)')
        self.worker = config['worker']
        self.token = Path(config['token_file']).read_text().strip()
        if not 32 <= len(self.token) <= 4096 or any(c in self.token for c in '\r\n\x00'):
            raise ValueError('Invalid worker credential')
        self.http = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())

    def call(self, path, data=None):
        req = urllib.request.Request(self.origin + '/internal/backups/v1/' + path,
                                     data=json.dumps(data).encode() if data is not None else None,
                                     headers={'Authorization': 'Bearer ' + self.token,
                                              'X-Backup-Worker': self.worker, 'Content-Type': 'application/json'})
        try:
            with self.http.open(req, timeout=15) as response:
                raw = response.read(4 * 1024 * 1024 + 1)
                if len(raw) > 4 * 1024 * 1024:
                    raise ValueError('Worker response too large')
                return json.loads(raw)
        except (OSError, ValueError, urllib.error.HTTPError):
            # Never reflect an HTTP body, request, or credential in logs/status.
            raise RuntimeError('Backup gateway unavailable or authorization changed') from None


def validate_lease(lease, binding, target):
    name = binding['repository']['full_name']
    if not re.fullmatch(r'[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+', name) or '..' in name:
        raise ValueError('Invalid GitHub destination')
    if (lease['remote'] != 'https://github.com/' + name + '.git' or
        lease['branch'] != binding['branch'] or lease['revision'] != target or
        lease['project'] != binding['project'] or lease['generation'] != binding['generation'] or
        lease.get('notebookGeneration', 0) != binding.get('notebookGeneration', 0) or
        not re.fullmatch('[a-f0-9]{40}', target)):
        raise ValueError('Backup lease does not match the selected notebook and revision')


# This helper is private to one push and only answers for its exact destination.
HELPER = '''import json, pathlib, sys
if sys.argv[-1] != "get": sys.exit(0)
request = dict(line.rstrip("\\n").split("=",1) for line in sys.stdin if "=" in line)
secret = json.loads(pathlib.Path(__file__).with_name("credential.json").read_text())
if request.get("protocol") == "https" and request.get("host") == "github.com" and request.get("path") == secret["path"]:
    print("username=x-access-token\\npassword=" + secret["token"] + "\\n")
'''


class GitHubBackup(Backup):
    """Operator-owned GitHub backup using an existing runtime credential."""
    def __init__(self, config):
        remote = config['remote']
        match = re.fullmatch(r'https://github\.com/([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)\.git', remote)
        if not match or '..' in match[1]:
            raise ValueError('GitHub backup requires an exact HTTPS repository URL')
        self.repository_name, self.helper = match[1], None
        self.token_file = Path(config['token_file']).resolve(strict=True)
        root = Path(config['repository']).resolve(strict=True)
        if self.token_file.is_relative_to(root):
            raise ValueError('Backup credentials must be outside the notebook')
        self.http = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
        self.token = None
        super().__init__(config, managed=True)

    def check_transfer(self):
        req = urllib.request.Request('https://api.github.com/repos/' + self.repository_name,
            headers={'Authorization':'Bearer '+self.token, 'Accept':'application/vnd.github+json'})
        try:
            with self.http.open(req, timeout=15) as response:
                raw = response.read(256 * 1024 + 1)
            if len(raw) > 256 * 1024:
                raise ValueError('metadata too large')
            data = json.loads(raw)
            if (data.get('full_name', '').lower() != self.repository_name.lower() or
                data.get('private') is not True or data.get('archived') is True or
                data.get('permissions', {}).get('push') is not True):
                raise ValueError('destination access changed')
        except (OSError, ValueError, urllib.error.HTTPError):
            raise RuntimeError('GitHub backup requires the selected private, writable repository') from None

    @contextmanager
    def credential_helper(self, token, path):
        if not token or any(c in token for c in '\r\n\x00') or len(token) > 16384:
            raise RuntimeError('Invalid GitHub credential')
        with tempfile.TemporaryDirectory(prefix='credential-', dir=self.state_dir) as directory:
            helper = Path(directory) / 'helper.py'
            helper.write_text(HELPER)
            secret = Path(directory) / 'credential.json'
            with open(secret, 'x', opener=lambda path, flags: os.open(path, flags, 0o600)) as output:
                json.dump(dict(token=token, path=path), output)
            self.helper = '!' + shlex.join([sys.executable, str(helper)])
            try:
                yield
            finally:
                self.helper = None

    @contextmanager
    def credentials(self, target):
        try:
            with self.token_file.open() as source:
                self.token = source.read(16385).strip()
            with self.credential_helper(self.token, self.repository_name + '.git'):
                self.check_transfer()
                yield
        except (OSError, ValueError):
            raise RuntimeError('Cannot read GitHub backup credential') from None
        finally:
            self.token = None

    def watch(self):
        # Poll committed HEAD; this also works in a minimal container without
        # inotify tools. Git objects, not filesystem events, define the backup.
        reconcile_at = 0
        while True:
            if self.identity() != self.git_identity:
                raise RuntimeError('Git directory replaced; restart the backup worker')
            now = time.monotonic()
            force = now >= reconcile_at
            self.observe(force=force)
            if force:
                reconcile_at = now + self.reconcile
            if self.pending_since is not None and not self.blocked:
                due = max(min(self.last_change + self.debounce, self.pending_since + self.max_delay), self.retry_at)
                if now >= due:
                    self.push()
            time.sleep(2)

    def git(self, *args, timeout=15):
        if args[0] not in ['push', 'ls-remote']:
            return super().git(*args, timeout=timeout)
        if not self.helper:
            raise RuntimeError('GitHub push requires scoped credentials')
        command = ['git', '-C', str(self.repo), '-c', 'core.hooksPath=/dev/null', '-c', 'gc.auto=0',
                   '-c', 'maintenance.auto=false', '-c', 'protocol.allow=never', '-c', 'protocol.https.allow=always',
                   '-c', 'credential.helper=', '-c', 'credential.helper=' + self.helper,
                   '-c', 'credential.useHttpPath=true', '-c', 'http.followRedirects=false',
                   '-c', 'http.sslVerify=true', *args]
        process = subprocess.Popen(command, env=self.env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, start_new_session=True)
        deadline = time.monotonic() + timeout
        try:
            while True:
                try:
                    stdout, stderr = process.communicate(timeout=min(2, max(0.01, deadline - time.monotonic())))
                    break
                except subprocess.TimeoutExpired:
                    if time.monotonic() >= deadline:
                        raise RuntimeError('Git push timed out') from None
                    self.check_transfer()
            if process.returncode:
                if any(text in stdout + stderr for text in ['non-fast-forward', 'fetch first', 'stale info']):
                    raise Diverged('Backup branch diverged; resolve the destination and explicitly resume')
                raise RuntimeError('GitHub backup upload failed')
            return stdout.strip()
        finally:
            if process.poll() is None:
                os.killpg(process.pid, signal.SIGTERM)
                try:
                    process.communicate(timeout=3)
                except subprocess.TimeoutExpired:
                    os.killpg(process.pid, signal.SIGKILL)
                    process.communicate()


class ManagedBackup(GitHubBackup):
    def __init__(self, config, gateway, binding):
        self.gateway, self.binding, self.lease, self.helper = gateway, binding, None, None
        Backup.__init__(self, {**config, 'remote': 'https://github.com/' + binding['repository']['full_name'] + '.git',
                          'branch': binding['branch']}, managed=True)
        if self.timeout > 120:
            raise ValueError('Managed pushes must fit the three-minute credential lease')
        # The server generation is the authority for an explicit resume.
        self.blocked = binding.get('blocked', False)

    @contextmanager
    def credentials(self, target):
        self.lease = self.gateway.call('lease', dict(project=self.binding['project'], generation=self.binding['generation'], revision=target))
        try:
            validate_lease(self.lease, self.binding, target)
            token = self.lease['token']
            with self.credential_helper(token, self.binding['repository']['full_name'] + '.git'):
                self.check_transfer()
                yield
                self.gateway.call('receipt', self.receipt(target))
        except Exception as error:
            try:
                self.gateway.call('receipt', {**self.receipt(target), 'error': 'upload failed', 'diverged': isinstance(error, Diverged)})
            except RuntimeError:
                pass
            if isinstance(error, Diverged):
                raise
            raise RuntimeError('Managed backup failed; check gateway, installation access, and destination') from None
        finally:
            self.lease = self.helper = None

    def check_transfer(self):
        if not self.lease:
            raise RuntimeError('Managed push requires a current lease')
        self.gateway.call('check', {'lease': self.lease['id']})

    def receipt(self, target):
        return dict(lease=self.lease['id'], project=self.binding['project'], generation=self.binding['generation'], revision=target)



class ManagedWorker:
    def __init__(self, config):
        self.config = config
        self.gateway = Gateway(config)
        self.state_dir = Path(config['state_dir']).resolve()
        self.roots = {k: str(Path(v).resolve(strict=True)) for k, v in config['notebooks'].items()}
        if len(set(self.roots.values())) != len(self.roots):
            raise ValueError('Each notebook requires a distinct local repository root')
        for root in self.roots.values():
            if self.state_dir.is_relative_to(root) or Path(config['token_file']).resolve().is_relative_to(root):
                raise ValueError('Worker state and credentials must be outside notebooks')
        self.active, self.reconcile_at = {}, 0

    def tick(self, once=False):
        bindings = self.gateway.call('bindings')
        # tick owns every push synchronously. A drain acknowledgement is sent only
        # after a prior push (including process termination) has returned.
        for binding in bindings:
            if binding['project'] in self.roots and binding.get('handoff'):
                self.active.pop(binding['project'], None)
                self.gateway.call('drain', dict(project=binding['project'],
                                              generation=binding['generation'],
                                              handoff=binding['handoff']))
        allowed = {b['project']: b for b in bindings
                   if b['project'] in self.roots and not b.get('handoff')}
        for project in list(self.active):
            if project not in allowed or self.active[project].binding['generation'] != allowed[project]['generation']:
                del self.active[project]
        now = time.monotonic()
        force = once or now >= self.reconcile_at
        success = True
        for project, binding in allowed.items():
            if not re.fullmatch('[a-z0-9][a-z0-9-]{0,63}', project):
                raise ValueError('Invalid notebook identifier')
            if project not in self.active:
                settings = {**self.config, 'repository': self.roots[project], 'state_dir': str(self.state_dir / project)}
                self.active[project] = ManagedBackup(settings, self.gateway, binding)
                self.active[project].state_dir.mkdir(mode=0o700, parents=True, exist_ok=True)
            backup = self.active[project]
            if backup.identity() != backup.git_identity:
                raise RuntimeError('Notebook storage replaced; restart to reattach')
            requested = backup.binding.get('request') != binding.get('request')
            backup.binding = binding
            backup.observe(force=force or requested)
            if backup.pending_since is not None and not backup.blocked:
                due = max(min(backup.last_change + backup.debounce, backup.pending_since + backup.max_delay), backup.retry_at)
                if once or requested or time.monotonic() >= due:
                    success = backup.push() and success
        if force:
            self.reconcile_at = time.monotonic() + float(self.config.get('reconcile_seconds', 300))
        return success

    def watch(self):
        # Fixed concurrency of one; committed HEAD is authoritative. No content
        # transfer or credentials are needed to detect a newly saved revision.
        retry = float(self.config.get('retry_seconds', 30))
        delay = retry
        while True:
            try:
                self.tick()
                delay = retry
            except RuntimeError as error:
                logging.error('%s', error)
                time.sleep(delay)
                delay = min(float(self.config.get('retry_max_seconds', 300)), delay * 2)
                continue
            time.sleep(2)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--config', required=True)
    parser.add_argument('--once', action='store_true')
    parser.add_argument('--resume', action='store_true', help='Operator SSH mode: resume after resolving a divergent destination')
    args = parser.parse_args()
    os.umask(0o077)
    logging.basicConfig(level=logging.INFO, format='%(asctime)s %(levelname)s %(message)s')
    def stop(signum, frame):
        raise KeyboardInterrupt
    signal.signal(signal.SIGTERM, stop)
    try:
        with open(args.config) as source:
            config = json.load(source)
        managed = config.get('mode') == 'managed'
        backup = ManagedWorker(config) if managed else GitHubBackup(config) if config.get('mode') == 'github' else Backup(config)
        if args.resume:
            if managed:
                raise ValueError('Resume managed backups through Notebook settings')
            backup.blocked = False
            backup.error = None
        backup.state_dir.mkdir(mode=0o700, parents=True, exist_ok=True)
        with (backup.state_dir / 'worker.lock').open('a') as lock:
            try:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError:
                raise RuntimeError('Another backup worker holds the state lock') from None
            if args.once:
                if managed:
                    return 0 if backup.tick(once=True) else 1
                backup.observe(force=True)
                return 0 if backup.push() else 1
            backup.watch()
    except KeyboardInterrupt:
        logging.info('Backup watcher stopped')
        return 0
    except Exception as error:
        logging.error('%s', error)
        return 1
    return 0


if __name__ == '__main__':
    sys.exit(main())
