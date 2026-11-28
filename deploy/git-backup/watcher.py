#!/usr/bin/env python3
"""Host-managed, one-way Git backup and snapshot worker."""
import argparse
from collections import namedtuple
from contextlib import contextmanager, suppress
from datetime import datetime, timedelta, timezone
import fcntl
import hashlib
import json
import logging
import os
import re
from pathlib import Path
import selectors
import shlex
import shutil
import signal
import subprocess
import sys
import tarfile
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
        write_json(self.state_dir / 'status.json', dict(
            status=status, observed_commit=self.observed,
            last_pushed_commit=self.last_pushed, last_success_at=self.last_success,
            last_attempt_at=self.last_attempt, error=self.error,
            updated_at=datetime.now(timezone.utc).isoformat(), blocked=self.blocked,
            pushed_purges=self.pushed_purges))

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

    def once(self):
        self.observe(force=True)
        return self.push()

    def push(self, force=False):
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


# Snapshots are versioned copies in a folder or an rclone remote, in place of a
# pushed branch. Each is a data file and its manifest, uploaded in that order
# and each verified; a snapshot exists only once its manifest does.
SNAPSHOT_WORKER = 'forgetmenote-backup snapshot 1'
STAMP = '%Y%m%dT%H%M%SZ'
SHA256 = re.compile(r'[a-f0-9]{64}')
NAME_ID = re.compile(r'[A-Za-z0-9][A-Za-z0-9._-]{0,127}')
REMOTE = re.compile(r'[A-Za-z0-9_][A-Za-z0-9_.+@-]{0,63}:')
SEGMENT = r'[A-Za-z0-9_](?:[A-Za-z0-9_. -]{0,126}[A-Za-z0-9_])?'
REMOTE_PATH = re.compile(f'(?:{SEGMENT}(?:/{SEGMENT}){{0,15}})?')
MANIFEST_BYTES = 1024 * 1024
DRIVES = {'gdrive': 'drive', 'dropbox': 'dropbox'}
Snapshot = namedtuple('Snapshot', 'name manifest time')


def utc_now():
    return datetime.now(timezone.utc)


def iso(moment):
    # Microseconds order two snapshots taken within one second of each other.
    return moment.strftime('%Y-%m-%dT%H:%M:%S.%fZ')


def created_time(manifest):
    try:
        moment = datetime.fromisoformat(manifest['created'])
    except (KeyError, TypeError, ValueError):
        return None
    return moment if moment.tzinfo is not None else None


def stamp_time(name):
    try:
        return datetime.strptime(name[:16], STAMP).replace(tzinfo=timezone.utc)
    except ValueError:
        return None


def private_file(path, flags):
    return os.open(path, flags | os.O_NOFOLLOW | os.O_CLOEXEC, 0o600)


def write_json(path, data):
    temporary = path.with_name(path.name + '.tmp')
    with temporary.open('w') as output:
        json.dump(data, output, indent=2)
        output.write('\n')
        output.flush()
        os.fsync(output.fileno())
    temporary.replace(path)


def sha256_file(path):
    digest = hashlib.sha256()
    with open(path, 'rb') as source:
        while chunk := source.read(1 << 20):
            digest.update(chunk)
    return digest.hexdigest()


def end_group(process):
    """Stop a child started in its own session, and everything it started."""
    if process.poll() is None:
        os.killpg(process.pid, signal.SIGTERM)
        try:
            process.wait(timeout=3)
        except subprocess.TimeoutExpired:
            os.killpg(process.pid, signal.SIGKILL)
            process.wait()


def git_in(directory, *args, env, timeout=60):
    result = subprocess.run(['git', '-C', str(directory), '-c', 'core.hooksPath=/dev/null', '-c', 'gc.auto=0',
                             '-c', 'maintenance.auto=false', '-c', 'protocol.allow=never', *args],
                            env=env, capture_output=True, text=True, timeout=timeout)
    if result.returncode:
        raise RuntimeError((result.stderr or result.stdout).strip()[-2000:] or 'git ' + args[0] + ' failed')
    return result.stdout.strip()


def bundle_commit(directory, bundle, branch, env, timeout=60):
    """Verify a complete bundle in an empty repository and return the commit it
    holds, which its HEAD and branch must both name."""
    repository = Path(directory) / 'verify.git'
    git_in(directory, 'init', '--quiet', '--bare', '--template=', str(repository), env=env)
    git_in(repository, 'bundle', 'verify', '--quiet', str(bundle), env=env, timeout=timeout)
    heads = dict(reversed(line.split()) for line in git_in(repository, 'bundle', 'list-heads', str(bundle), env=env).splitlines()
                 if len(line.split()) == 2)
    commit = heads.get('refs/heads/' + branch, '')
    if set(heads) != {'HEAD', 'refs/heads/' + branch} or heads['HEAD'] != commit or not COMMIT.fullmatch(commit):
        raise RuntimeError('The bundle does not hold one consistent branch and HEAD')
    return commit


def retention_rules(value, **defaults):
    rules = {**defaults, **(value or {})}
    if set(rules) != {'newest', 'daily', 'weekly'} or not all(
            isinstance(v, int) and not isinstance(v, bool) and 0 <= v <= 100000 for v in rules.values()):
        raise ValueError('Retention takes whole numbers for "newest", "daily" and "weekly"')
    return rules


def retained(snapshots, now, newest=1, daily=0, weekly=0):
    """The names retention keeps of snapshots, newest first: the newest few,
    the newest of each recent UTC day and of each recent ISO week, and always
    the newest of all."""
    keep = {s.name for s in snapshots[:max(1, newest)]}
    days, weeks = {}, {}
    today = now.date()
    monday = today - timedelta(days=today.weekday())
    for snapshot in snapshots:
        day = snapshot.time.date()
        if (today - day).days < daily:
            days.setdefault(day, snapshot.name)
        week = day - timedelta(days=day.weekday())
        if (monday - week).days // 7 < weekly:
            weeks.setdefault(week, snapshot.name)
    return keep | set(days.values()) | set(weeks.values())


def valid_manifest(manifest):
    return (isinstance(manifest, dict) and manifest.get('format') == 1 and
            str(manifest.get('worker', '')).startswith('forgetmenote-backup ') and
            isinstance(manifest.get('sha256'), str) and SHA256.fullmatch(manifest['sha256']) and
            type(manifest.get('size')) is int and manifest['size'] >= 0 and created_time(manifest) is not None and
            isinstance(manifest.get('purges'), list) and
            all(isinstance(p, str) and PURGE_ID.fullmatch(p) for p in manifest['purges']))


class Folder:
    """A snapshot destination in a local or mounted folder. The folder must
    exist: the worker never creates it, so an unmounted share fails instead of
    filling the disk beneath its mount point."""
    def __init__(self, path):
        self.path = Path(path)

    def __enter__(self):
        if self.path.is_symlink() or not self.path.is_dir():
            raise RuntimeError('Snapshot folder is missing; mount or create it first')
        return self

    def __exit__(self, *exc):
        return False

    def list(self):
        with os.scandir(self.path) as entries:
            return {e.name: e.stat(follow_symlinks=False).st_size for e in entries if e.is_file(follow_symlinks=False)}

    def read_manifests(self, names):
        found = {}
        for name in names:
            with suppress(FileNotFoundError), os.fdopen(private_file(self.path / name, os.O_RDONLY), 'rb') as source:
                found[name] = source.read(MANIFEST_BYTES + 1)
        return found

    def download(self, name, local, limit=-1):
        with os.fdopen(private_file(self.path / name, os.O_RDONLY), 'rb') as source, \
                open(local, 'xb', opener=private_file) as output:
            if limit < 0:
                shutil.copyfileobj(source, output, 1 << 20)
            else:
                output.write(source.read(limit))

    def upload(self, local, name, sha256):
        partial = self.path / (name + '.partial')
        with suppress(FileNotFoundError):
            partial.unlink()
        with open(local, 'rb') as source, open(partial, 'xb', opener=private_file) as output:
            shutil.copyfileobj(source, output, 1 << 20)
            output.flush()
            os.fsync(output.fileno())
        if sha256_file(partial) != sha256:
            raise RuntimeError('Snapshot copy does not match its checksum')
        os.rename(partial, self.path / name)
        directory = os.open(self.path, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)

    def delete(self, name):
        with suppress(FileNotFoundError):
            (self.path / name).unlink()


class Rclone:
    """A snapshot destination reached through the rclone executable. Every call
    is bounded and runs in its own process group, without a shell, ambient
    RCLONE_* settings or proxies, and with no secret in its arguments."""
    def __init__(self, binary, remote, path, *, state_dir, config=None, env=None, timeout=120,
                 deadline=None, check=None, verbose=True):
        if not REMOTE.fullmatch(remote) or not REMOTE_PATH.fullmatch(path):
            raise ValueError('Invalid rclone destination')
        self.binary, self.remote, self.path, self.state_dir = str(binary), remote, path, Path(state_dir)
        self.config, self.timeout, self.deadline, self.check, self.verbose = config, timeout, deadline, check, verbose
        self.env = {'PATH': '/usr/bin:/bin', 'HOME': str(self.state_dir), **(env or {})}
        self.work = None

    def __enter__(self):
        self.work = Path(tempfile.mkdtemp(prefix='rclone-', dir=self.state_dir))
        if self.config is None:
            # An empty configuration: never the service account's own remotes.
            self.config = self.work / 'rclone.conf'
            self.config.touch(mode=0o600)
        return self

    def __exit__(self, *exc):
        shutil.rmtree(self.work, ignore_errors=True)
        return False

    @property
    def root(self):
        return self.remote + self.path

    def file(self, name):
        return self.remote + (self.path + '/' if self.path else '') + name

    def run(self, *args, data=None, output=None, ok=(0,)):
        timeout = self.timeout if self.deadline is None else min(self.timeout, self.deadline - time.monotonic())
        if timeout <= 0:
            raise RuntimeError('Snapshot transfer ran out of time')
        process = subprocess.Popen([self.binary, '--config', str(self.config), '--ask-password=false', *args],
                                   env=self.env, stdin=subprocess.DEVNULL if data is None else subprocess.PIPE,
                                   stdout=output or subprocess.PIPE, stderr=subprocess.PIPE, start_new_session=True)
        deadline = time.monotonic() + timeout
        try:
            while True:
                try:
                    stdout, stderr = process.communicate(data, timeout=min(2, max(0.01, deadline - time.monotonic())))
                    break
                except subprocess.TimeoutExpired:
                    data = None
                    if time.monotonic() >= deadline:
                        raise RuntimeError('rclone ' + args[0] + ' timed out') from None
                    if self.check:
                        self.check()
            if process.returncode not in ok:
                detail = (stderr or b'').decode(errors='replace').strip()[-1000:] if self.verbose else ''
                raise RuntimeError('rclone ' + args[0] + ' failed' + (': ' + detail if detail else ''))
            return process.returncode, (stdout or b'').decode(errors='replace')
        finally:
            end_group(process)
            for stream in (process.stdin, process.stdout, process.stderr):
                if stream:
                    stream.close()

    def list(self):
        code, listing = self.run('lsjson', '--files-only', '--no-mimetype', self.root, ok=(0, 3))
        if code == 3:  # rclone: directory not found
            return {}
        return {e['Name']: e['Size'] for e in json.loads(listing)
                if isinstance(e, dict) and isinstance(e.get('Name'), str) and type(e.get('Size')) is int}

    def read_manifests(self, names):
        cache = self.state_dir / 'manifests'
        cache.mkdir(mode=0o700, exist_ok=True)
        for stale in cache.iterdir():
            if stale.name not in names:
                stale.unlink()
        if names:
            # One transfer for every manifest not already cached unchanged.
            listing = self.work / 'manifests.txt'
            listing.write_text(''.join(name + '\n' for name in names))
            self.run('copy', '--files-from', str(listing), '--max-size', '1M', self.root, str(cache))
        found = {}
        for name in names:
            with suppress(FileNotFoundError), os.fdopen(private_file(cache / name, os.O_RDONLY), 'rb') as source:
                found[name] = source.read(MANIFEST_BYTES + 1)
        return found

    def download(self, name, local, limit=-1):
        with open(local, 'xb', opener=private_file) as output:
            self.run('cat', *(['--count', str(limit)] if limit >= 0 else []), self.file(name), output=output)

    def upload(self, local, name, sha256):
        partial = name + '.partial'
        self.run('copyto', str(local), self.file(partial))
        self.confirm(local, partial, sha256)
        self.run('moveto', self.file(partial), self.file(name))

    def confirm(self, local, name, sha256):
        info = json.loads(self.run('lsjson', '--stat', '--hash', '--no-mimetype', self.file(name))[1])
        if not isinstance(info, dict) or info.get('Size') != local.stat().st_size:
            raise RuntimeError('Uploaded snapshot has the wrong size')
        hashes = {str(k).lower(): v.lower() for k, v in (info.get('Hashes') or {}).items() if isinstance(v, str) and v}
        if 'sha256' in hashes:
            matched = hashes['sha256'] == sha256
        elif hashes:
            # The remote's own hash type, computed locally by the same rclone.
            kind = min(hashes)
            fields = self.run('hashsum', kind, str(local))[1].split()
            matched = bool(fields) and fields[0].lower() == hashes[kind]
        else:
            # A remote without hashes, such as an encrypted one: compare the bytes.
            with tempfile.TemporaryDirectory(dir=self.work) as directory:
                try:
                    os.link(local, Path(directory) / name)
                except OSError:
                    shutil.copyfile(local, Path(directory) / name)
                matched = self.run('check', '--download', '--one-way', '--include', '/' + name,
                                   directory, self.root, ok=(0, 1))[0] == 0
        if not matched:
            raise RuntimeError('Uploaded snapshot does not match its checksum')

    def delete(self, name):
        self.run('deletefile', self.file(name))

    def obscure(self, secret):
        if not secret or any(c in secret for c in '\r\n\x00') or len(secret) > 1024:
            raise ValueError('Invalid encryption password')
        obscured = self.run('obscure', '-', data=(secret + '\n').encode())[1].strip()
        if not re.fullmatch(r'[A-Za-z0-9_-]+', obscured):
            raise RuntimeError('rclone obscure failed')
        return obscured

    def remote_types(self):
        types = {}
        for line in self.run('listremotes', '--long')[1].splitlines():
            fields = line.split()
            if len(fields) >= 2 and fields[0].endswith(':'):
                types[fields[0][:-1]] = fields[1]
        return types


def rclone_binary(config, outside=()):
    """The configured rclone, or else the worker image's, which it names in
    FMN_BACKUP_RCLONE."""
    binary = Path(config.get('rclone') or os.environ.get('FMN_BACKUP_RCLONE', ''))
    if not binary.is_absolute() or not binary.is_file():
        raise ValueError('Set "rclone" to the absolute path of the pinned rclone executable, or run the worker image')
    if any(binary.resolve().is_relative_to(root) for root in outside):
        raise ValueError('The rclone executable must be outside notebook data')
    return binary


def configured_destination(config, state_dir, timeout, outside):
    """A factory for the operator's destination: an existing folder, or a
    named rclone remote and a path inside it."""
    target = config['destination']
    if isinstance(target, dict) and set(target) == {'folder'}:
        folder = Path(target['folder'])
        if not folder.is_absolute():
            raise ValueError('Snapshot folder must be an absolute path')
        if any(folder.resolve().is_relative_to(root) for root in [*outside, state_dir]):
            raise ValueError('Snapshot folder must be outside notebook data and worker state')
        return lambda: Folder(folder)
    if not isinstance(target, dict) or 'rclone' not in target or set(target) - {'rclone', 'path'}:
        raise ValueError('Destination takes "folder", or "rclone" and an optional "path"')
    binary = rclone_binary(config, outside)
    settings = config.get('rclone_config')
    if settings:
        settings = Path(settings).resolve(strict=True)
        if any(settings.is_relative_to(root) for root in outside):
            raise ValueError('The rclone configuration must be outside notebook data')
    Rclone(binary, target['rclone'], target.get('path', ''), state_dir=state_dir)  # validates the names
    return lambda: Rclone(binary, target['rclone'], target.get('path', ''), state_dir=state_dir,
                          config=settings, timeout=timeout)


class Store:
    """The snapshots of one notebook or instance at one destination."""
    def __init__(self, transport, suffix, label, accept):
        self.transport, self.suffix, self.accept = transport, suffix, accept
        self.data = re.compile(r'\d{8}T\d{6}Z-(' + label + ')' + re.escape(suffix))
        self.meta = re.compile(r'\d{8}T\d{6}Z-(' + label + r')\.json')

    def manifest_name(self, name):
        return name[:-len(self.suffix)] + '.json'

    def scan(self):
        """Files at the destination, this worker's valid manifests by name, and
        the complete snapshots, newest first. Partial uploads, data without a
        manifest and anything else never count."""
        files = self.transport.list()
        ours = {}
        for name, raw in self.transport.read_manifests(sorted(n for n in files if self.meta.fullmatch(n))).items():
            try:
                manifest = json.loads(raw) if len(raw) <= MANIFEST_BYTES else None
            except ValueError:
                manifest = None
            if valid_manifest(manifest) and stamp_time(name) and self.accept(self.meta.fullmatch(name)[1], manifest):
                ours[name] = manifest
        snapshots = [Snapshot(n, ours[self.manifest_name(n)], created_time(ours[self.manifest_name(n)])) for n in files
                     if self.data.fullmatch(n) and self.manifest_name(n) in ours and
                     files[n] == ours[self.manifest_name(n)]['size']]
        return files, ours, sorted(snapshots, key=lambda s: (s.time, s.name), reverse=True)

    def publish(self, local, name, manifest):
        self.transport.upload(local, name, manifest['sha256'])
        meta = local.with_name(self.manifest_name(name))
        meta.write_text(json.dumps(manifest, indent=2) + '\n')
        self.transport.upload(meta, self.manifest_name(name), sha256_file(meta))

    def remove(self, name):
        self.transport.delete(name)
        self.transport.delete(self.manifest_name(name))

    def prune(self, new, files, ours, snapshots, rules):
        """After snapshot new verified: delete the older snapshots whose
        history a purge new records has since rewritten, those retention no
        longer keeps, and day-old leftovers of interrupted uploads and
        deletions. Returns the names of the snapshots deleted."""
        deleted = []
        purges = set(new.manifest['purges'])
        for snapshot in snapshots[snapshots.index(new) + 1:]:
            if purges - set(snapshot.manifest['purges']):
                self.remove(snapshot.name)
                deleted.append(snapshot.name)
        remaining = [s for s in snapshots if s.name not in deleted]
        keep = retained(remaining, utc_now(), **rules)
        for snapshot in remaining:
            if snapshot.name not in keep:
                self.remove(snapshot.name)
                deleted.append(snapshot.name)
        cutoff = utc_now() - timedelta(days=1)
        for name in files:
            base = name.removesuffix('.partial')
            if (stamp_time(base) or cutoff) >= cutoff:
                continue
            if (base != name and (self.data.fullmatch(base) or self.meta.fullmatch(base)) or
                    self.data.fullmatch(name) and self.manifest_name(name) not in files or
                    name in ours and name[:-len('.json')] + self.suffix not in files):
                self.transport.delete(name)
        if deleted:
            logging.info('Deleted snapshots %s', ', '.join(deleted))
        return deleted


class SnapshotBackup(Backup):
    """Operator snapshots of one notebook: verified Git bundles of its export
    in a folder or an rclone remote, at most one per interval."""
    def __init__(self, config, *, destination=True):
        super().__init__({**config, 'remote': None}, managed=True)
        self.notebook = config.get('notebook') or self.repo.name.removesuffix('.git')
        if not NAME_ID.fullmatch(self.notebook):
            raise ValueError('Invalid notebook identifier; set "notebook" in the configuration')
        self.min_interval = float(config.get('min_interval_seconds', 3600))
        if self.min_interval < 0:
            raise ValueError('Intervals must be positive')
        self.retention = retention_rules(config.get('retention'), newest=24, daily=30, weekly=52)
        self.staging = self.state_dir / 'staging'
        self.destination = configured_destination(config, self.state_dir, self.timeout, [self.repo]) if destination else None
        self.last_snapshot = self.last_snapshot_at = self.counts = self.result = None
        self.not_before = 0
        try:
            previous = json.loads((self.state_dir / 'status.json').read_text())
            elapsed = (utc_now() - datetime.fromisoformat(previous['last_snapshot_at'])).total_seconds()
            self.last_snapshot_at = previous['last_snapshot_at']
            self.not_before = self.retry_at = time.monotonic() + max(0, min(self.min_interval, self.min_interval - elapsed))
        except (OSError, ValueError, KeyError, TypeError):
            pass

    def status(self, status):
        write_json(self.state_dir / 'status.json', dict(
            status=status, observed_commit=self.observed, last_pushed_commit=self.last_pushed,
            last_snapshot=self.last_snapshot, last_snapshot_at=self.last_snapshot_at,
            last_success_at=self.last_success, last_attempt_at=self.last_attempt, error=self.error,
            counts=self.counts, updated_at=datetime.now(timezone.utc).isoformat(), blocked=self.blocked))

    def once(self):
        self.observe(force=True)
        return self.push(force=True)

    # Poll committed HEAD like token mode: no inotify tools needed.
    watch = GitHubBackup.watch

    def transport(self, commit):
        return self.destination()

    def accept(self, label, manifest):
        commit = manifest.get('commit')
        return (manifest.get('kind') == 'notebook' and manifest.get('notebook') == self.notebook and
                isinstance(commit, str) and COMMIT.fullmatch(commit) is not None and commit[:12] == label)

    def push(self, force=False):
        """Take a snapshot, unless the last one this worker verified holds
        committed HEAD. An unforced snapshot waits for the minimum interval."""
        if self.blocked:
            return False
        if not force and self.head() == self.last_pushed:
            self.pending_since = None
            self.status('synced')
            return True
        self.last_attempt = datetime.now(timezone.utc).isoformat()
        self.result = None
        self.status('pushing')
        try:
            shutil.rmtree(self.staging, ignore_errors=True)
            self.staging.mkdir(mode=0o700)
            bundle = self.staging / 'notebook.bundle'
            self.git('bundle', 'create', '--quiet', str(bundle), 'HEAD', 'refs/heads/' + self.branch, timeout=self.timeout)
            # The export may have moved meanwhile: the bundle says what it holds.
            commit = bundle_commit(self.staging, bundle, self.branch, self.env, self.timeout)
            with self.transport(commit) as transport:
                self.result = self.publish(transport, bundle, commit)
        except Exception as error:
            self.error = str(error) or 'Snapshot failed'
            self.retry_at = max(self.not_before, time.monotonic() + self.retry_delay)
            self.retry_delay = min(self.retry_max, self.retry_delay * 2)
            if self.pending_since is None:
                self.pending_since = time.monotonic()
            self.status('error')
            logging.error('Snapshot failed; earlier snapshots are unchanged: %s', self.error)
            return False
        finally:
            shutil.rmtree(self.staging, ignore_errors=True)
        result = self.result
        self.last_pushed = result['commit']
        self.last_snapshot = {k: result[k] for k in ('name', 'commit', 'created', 'sha256', 'size')}
        if result['uploaded']:
            self.last_snapshot_at = result['created']
            self.not_before = time.monotonic() + self.min_interval
        self.counts = dict(snapshots=result['kept'], deleted=len(result['deleted']))
        self.last_success = datetime.now(timezone.utc).isoformat()
        self.pending_since = None
        self.retry_at = self.not_before
        self.retry_delay = self.retry
        self.error = None
        self.status('synced')
        logging.info('Snapshot %s holds %s', result['name'], result['commit'])
        self.observe()
        return True

    def publish(self, transport, bundle, commit):
        store = Store(transport, '.bundle', '[a-f0-9]{12}', self.accept)
        files, ours, snapshots = store.scan()
        uploaded = not snapshots or snapshots[0].manifest['commit'] != commit
        if uploaded:
            now = utc_now()
            name = now.strftime(STAMP) + '-' + commit[:12] + '.bundle'
            store.publish(bundle, name, dict(
                format=1, kind='notebook', notebook=self.notebook, commit=commit, created=iso(now),
                size=bundle.stat().st_size, sha256=sha256_file(bundle), purges=self.purges(commit),
                worker=SNAPSHOT_WORKER))
            files, ours, snapshots = store.scan()
        new = next((s for s in snapshots if s.name == name), None) if uploaded else snapshots[0]
        if new is None:
            raise RuntimeError('The uploaded snapshot is missing from the destination listing')
        deleted = store.prune(new, files, ours, snapshots, self.retention)
        return dict(name=new.name, commit=new.manifest['commit'], created=new.manifest['created'],
                    sha256=new.manifest['sha256'], size=new.manifest['size'], uploaded=uploaded,
                    kept=len(snapshots) - len(deleted), deleted=deleted)

    def purges(self, commit):
        """Every purge recorded in commit's history. The Git push worker owns
        the purge flag; snapshots rely on these trailers alone."""
        found = set()
        for line in self.git('log', '--format=%(trailers:key=Gitnotes-Purge,valueonly)', commit, '--',
                             timeout=self.timeout).splitlines():
            if PURGE_ID.fullmatch(line.strip()):
                found.add(line.strip())
        return sorted(found)

    def verify(self):
        """Download the newest snapshot and check it against its manifest."""
        with self.transport(None) as transport, \
                tempfile.TemporaryDirectory(prefix='verify-', dir=self.state_dir) as directory:
            store = Store(transport, '.bundle', '[a-f0-9]{12}', self.accept)
            found = newest_snapshot(store, transport, Path(directory))
            if found is None:
                logging.error('No snapshot at the destination')
                return False
            name, manifest = found
            bundle = Path(directory) / name
            transport.download(name, bundle)
            if bundle.stat().st_size != manifest['size'] or sha256_file(bundle) != manifest['sha256']:
                logging.error('Snapshot %s does not match its manifest', name)
                return False
            try:
                commit = bundle_commit(directory, bundle, self.branch, self.env, self.timeout)
            except RuntimeError as error:
                logging.error('Snapshot %s is not a complete bundle: %s', name, error)
                return False
            if commit != manifest['commit']:
                logging.error('Snapshot %s holds %s, not its manifest commit', name, commit)
                return False
        logging.info('Verified snapshot %s: commit %s', name, commit)
        return True


def newest_snapshot(store, transport, directory):
    """The newest snapshot whose freshly downloaded manifest is valid, or None."""
    files = transport.list()
    for name in sorted((n for n in files if store.data.fullmatch(n) and store.manifest_name(n) in files), reverse=True):
        meta = directory / store.manifest_name(name)
        transport.download(store.manifest_name(name), meta, MANIFEST_BYTES + 1)
        try:
            manifest = json.loads(meta.read_bytes())
        except ValueError:
            continue
        if valid_manifest(manifest) and store.accept(store.data.fullmatch(name)[1], manifest):
            return name, manifest
    return None


def validate_drive_lease(lease, binding, target):
    try:
        encryption = lease['encryption']
        ok = (isinstance(lease['id'], str) and 0 < len(lease['id']) <= 256 and
              lease['project'] == binding['project'] and lease['kind'] == binding['kind'] and
              lease['generation'] == binding['generation'] and
              lease.get('notebookGeneration', 0) == binding.get('notebookGeneration', 0) and
              lease['revision'] == target and COMMIT.fullmatch(target) is not None and
              lease['path'] == binding['path'] and REMOTE_PATH.fullmatch(binding['path']) is not None and binding['path'] != '' and
              isinstance(lease['token'], str) and 0 < len(lease['token']) <= 16384 and
              not any(c in lease['token'] for c in '\r\n\x00') and
              datetime.fromisoformat(lease['tokenExpires']).tzinfo is not None and
              # An encrypted destination never receives an unencrypted upload.
              (encryption is None if binding.get('encrypted') is not True else
               isinstance(encryption, dict) and isinstance(encryption.get('password'), str) and encryption['password'] != ''))
    except (KeyError, TypeError, ValueError):
        ok = False
    if not ok:
        raise ValueError('Snapshot lease does not match the selected notebook, destination and revision')


def drive_environment(kind, token, expires, path, obscured):
    """The one-run rclone remotes of a managed drive snapshot, as environment
    variables: neither a configuration file nor an argument holds the token."""
    env = {'RCLONE_CONFIG_FMNSNAP_TYPE': DRIVES[kind],
           'RCLONE_CONFIG_FMNSNAP_TOKEN': json.dumps({'access_token': token, 'token_type': 'Bearer', 'expiry': expires})}
    if kind == 'gdrive':
        # drive.file reaches only the files this app created. A deleted
        # snapshot is deleted, not kept in the Drive's trash.
        env.update(RCLONE_CONFIG_FMNSNAP_SCOPE='drive.file', RCLONE_CONFIG_FMNSNAP_USE_TRASH='false')
    if obscured is not None:
        env.update(RCLONE_CONFIG_FMNCRYPT_TYPE='crypt', RCLONE_CONFIG_FMNCRYPT_REMOTE='fmnsnap:' + path,
                   RCLONE_CONFIG_FMNCRYPT_PASSWORD=obscured, RCLONE_CONFIG_FMNCRYPT_FILENAME_ENCRYPTION='standard',
                   RCLONE_CONFIG_FMNCRYPT_DIRECTORY_NAME_ENCRYPTION='true')
    return env


class ManagedSnapshot(SnapshotBackup):
    """A drive snapshot the app leases: a short-lived access token for one
    folder, optionally encrypted, used for one run and then dropped."""
    def __init__(self, config, gateway, binding):
        self.gateway, self.binding, self.lease = gateway, binding, None
        super().__init__({**config, 'notebook': binding['project']}, destination=False)
        if self.timeout > 120:
            raise ValueError('Managed snapshots must fit the three-minute credential lease')
        if binding.get('kind') not in DRIVES or not binding.get('path') or not REMOTE_PATH.fullmatch(binding['path']):
            raise ValueError('Invalid drive destination')
        self.rclone = rclone_binary(config)
        self.blocked = binding.get('blocked', False)

    @contextmanager
    def transport(self, commit):
        binding = self.binding
        self.lease = self.gateway.call('lease', dict(project=binding['project'], kind=binding['kind'],
                                                    generation=binding['generation'], revision=commit))
        try:
            validate_drive_lease(self.lease, binding, commit)
            encrypted = self.lease['encryption'] is not None
            with Rclone(self.rclone, 'fmncrypt:' if encrypted else 'fmnsnap:', '' if encrypted else binding['path'],
                        state_dir=self.state_dir, timeout=self.timeout, deadline=time.monotonic() + self.timeout,
                        check=self.check_lease, verbose=False) as transport:
                obscured = transport.obscure(self.lease['encryption']['password']) if encrypted else None
                transport.env.update(drive_environment(binding['kind'], self.lease['token'], self.lease['tokenExpires'],
                                                       binding['path'], obscured))
                self.check_lease()
                yield transport
            self.gateway.call('receipt', dict(self.receipt(commit), snapshot={
                k: self.result[k] for k in ('name', 'sha256', 'size', 'kept', 'deleted')}))
        except Exception:
            with suppress(Exception):
                self.gateway.call('receipt', dict(self.receipt(commit), error='upload failed'))
            raise RuntimeError('Managed snapshot failed; check gateway, drive access, and destination') from None
        finally:
            self.lease = None

    def check_lease(self):
        if not self.lease:
            raise RuntimeError('Managed snapshot requires a current lease')
        self.gateway.call('check', {'lease': self.lease['id']})

    def receipt(self, target):
        return dict(lease=self.lease.get('id'), project=self.binding['project'], kind=self.binding['kind'],
                    generation=self.binding['generation'], revision=target)


# Capture protocol v1: the application's capture command prints this line once
# it holds the block-sweep lock and an exported database snapshot.
CAPTURE = re.compile(r'snapshot ([0-9A-Fa-f]{1,32}(?:-[0-9A-Fa-f]{1,32}){1,4}) purge (-|' + PURGE_ID.pattern + ')')


def read_line(stream, pending, deadline, what):
    """One line of a child's output before the deadline; pending keeps what
    was read past it."""
    selector = selectors.DefaultSelector()
    selector.register(stream, selectors.EVENT_READ)
    try:
        while b'\n' not in pending:
            if len(pending) > 4096:
                raise RuntimeError(what + ' printed an overlong line')
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise RuntimeError(what + ' timed out')
            if selector.select(remaining):
                chunk = os.read(stream.fileno(), 4096)
                if not chunk:
                    raise RuntimeError(what + ' ended without reporting')
                pending.extend(chunk)
    finally:
        selector.close()
    line, _, rest = bytes(pending).partition(b'\n')
    pending[:] = rest
    return line.decode('utf-8', 'replace').strip()


def add_tree(tar, root, arcname):
    """Add a file or directory, keeping modes, and refuse symlinks that leave it."""
    root = Path(root)
    if root.is_symlink():
        raise RuntimeError(f'Refusing to archive the symlink {root}; list its target')
    real = os.path.realpath(root)
    tar.add(root, arcname, recursive=False)
    if not root.is_dir():
        return
    for directory, dirnames, filenames in os.walk(root):
        dirnames.sort()
        for name in dirnames + sorted(filenames):
            path = os.path.join(directory, name)
            if os.path.islink(path) and os.path.commonpath([real, os.path.realpath(path)]) != real:
                raise RuntimeError(f'Refusing a symlink that leaves {root}: {path}')
            tar.add(path, arcname + '/' + os.path.relpath(path, root), recursive=False)


def command(value, name):
    if not isinstance(value, list) or not value or not all(isinstance(part, str) and part for part in value):
        raise ValueError(f'"{name}" must be a list of command arguments')
    return value


class InstanceBackup:
    """Daily encrypted snapshots of the whole instance, taken without downtime
    through the application's capture command (capture protocol v1)."""
    def __init__(self, config):
        self.state_dir = Path(config['state_dir']).resolve()
        self.at = datetime.strptime(config.get('at', '04:00'), '%H:%M').time()
        self.capture_command = command(config['capture_command'], 'capture_command')
        self.dump_command = command(config['dump_command'], 'dump_command')
        if not any('{snapshot}' in part for part in self.dump_command):
            raise ValueError('"dump_command" must pass the database snapshot as {snapshot}')
        self.capture_dir = Path(config['capture_dir'])
        # The worker deletes this directory after every capture.
        if not self.capture_dir.is_absolute() or self.capture_dir.parent.name != '.capture':
            raise ValueError('"capture_dir" must be an absolute directory inside a ".capture" folder')
        if self.state_dir.is_relative_to(self.capture_dir.parent):
            raise ValueError('Backup state must be outside the capture folder')
        self.include = [Path(p) for p in config.get('include', [])]
        if not all(p.is_absolute() for p in self.include):
            raise ValueError('"include" takes absolute paths')
        target = config['destination']
        if not isinstance(target, dict) or 'rclone' not in target:
            raise ValueError('Instance snapshots need an rclone crypt remote; folder destinations are refused')
        self.remote = target['rclone']
        self.timeout = float(config.get('upload_timeout_seconds', 3600))
        self.capture_timeout = float(config.get('capture_timeout_seconds', 3600))
        self.retry = float(config.get('retry_seconds', 300))
        self.retry_max = float(config.get('retry_max_seconds', 3600))
        if min(self.timeout, self.capture_timeout, self.retry, self.retry_max) <= 0:
            raise ValueError('Intervals must be positive')
        self.retention = retention_rules(config.get('retention'), newest=1, daily=14, weekly=8)
        self.destination = configured_destination(config, self.state_dir, self.timeout, [self.capture_dir.parent])
        self.staging = self.state_dir / 'staging'
        # The commands are the operator's own; the worker adds no secrets to them.
        self.command_env = {k: v for k, v in os.environ.items() if not k.startswith('RCLONE_')}
        self.last_snapshot = self.last_success = self.last_attempt = self.error = self.counts = None
        self.retry_at, self.retry_delay = None, self.retry
        with suppress(OSError, ValueError, AttributeError):
            previous = json.loads((self.state_dir / 'status.json').read_text())
            self.last_snapshot, self.last_success = previous.get('last_snapshot'), previous.get('last_success_at')

    def status(self, status):
        write_json(self.state_dir / 'status.json', dict(
            status=status, last_snapshot=self.last_snapshot, last_success_at=self.last_success,
            last_attempt_at=self.last_attempt, error=self.error, counts=self.counts,
            updated_at=datetime.now(timezone.utc).isoformat()))

    def accept(self, label, manifest):
        return (manifest.get('kind') == 'instance' and label == 'instance-' + manifest['sha256'][:12] and
                (manifest.get('purge') is None or isinstance(manifest['purge'], str) and PURGE_ID.fullmatch(manifest['purge'])))

    def store(self, transport):
        return Store(transport, '.tar.gz', 'instance-[a-f0-9]{12}', self.accept)

    def transport(self):
        transport = self.destination()
        if not isinstance(transport, Rclone):
            raise ValueError('Instance snapshots need an rclone crypt remote')
        return transport

    def once(self):
        return self.push(force=True)

    def push(self, force=False):
        self.last_attempt = datetime.now(timezone.utc).isoformat()
        self.status('capturing')
        try:
            with self.transport() as transport:
                # The archive holds the instance's secrets: refuse any
                # destination that would store it unencrypted.
                if transport.remote_types().get(self.remote[:-1]) != 'crypt':
                    raise RuntimeError(f'Instance snapshots need an rclone crypt remote; {self.remote} is not one')
                store = self.store(transport)
                _, _, snapshots = store.scan()
                shutil.rmtree(self.staging, ignore_errors=True)
                self.staging.mkdir(mode=0o700)
                purge, dump = self.capture()
                archive = self.staging / 'instance.tar.gz'
                with open(archive, 'xb', opener=private_file) as output:
                    with tarfile.open(fileobj=output, mode='w:gz', compresslevel=6) as tar:
                        add_tree(tar, self.capture_dir, 'capture')
                        tar.add(dump, 'postgres.sql', recursive=False)
                        for path in self.include:
                            add_tree(tar, path, 'include' + str(path))
                    output.flush()
                    os.fsync(output.fileno())
                self.remove_capture()
                # Purges seen so far: a capture deletes older ones only for a
                # purge none of them has seen, never after a database rollback.
                purges = list(snapshots[0].manifest['purges']) if snapshots else []
                if purge and purge not in purges:
                    purges.append(purge)
                now, digest = utc_now(), sha256_file(archive)
                name = now.strftime(STAMP) + '-instance-' + digest[:12] + '.tar.gz'
                store.publish(archive, name, dict(
                    format=1, kind='instance', created=iso(now), size=archive.stat().st_size, sha256=digest,
                    purge=purge, purges=purges[-100:], worker=SNAPSHOT_WORKER))
                files, ours, snapshots = store.scan()
                new = next((s for s in snapshots if s.name == name), None)
                if new is None:
                    raise RuntimeError('The uploaded snapshot is missing from the destination listing')
                deleted = store.prune(new, files, ours, snapshots, self.retention)
        except Exception as error:
            self.error = str(error) or 'Instance snapshot failed'
            self.status('error')
            logging.error('Instance snapshot failed; earlier snapshots are unchanged: %s', self.error)
            return False
        finally:
            self.remove_capture(strict=False)
            shutil.rmtree(self.staging, ignore_errors=True)
        self.last_snapshot = dict(name=name, created=iso(now), sha256=digest, purge=purge)
        self.last_success = datetime.now(timezone.utc).isoformat()
        self.counts = dict(snapshots=len(snapshots) - len(deleted), deleted=len(deleted))
        self.error = None
        self.status('synced')
        logging.info('Instance snapshot %s uploaded', name)
        return True

    def capture(self):
        """Run capture protocol v1. Returns the latest purge ID, or None, and
        the database dump taken from the capture's own database snapshot."""
        self.remove_capture()
        dump = self.staging / 'postgres.sql'
        process = subprocess.Popen(self.capture_command, stdin=subprocess.PIPE, stdout=subprocess.PIPE, bufsize=0,
                                   env=self.command_env, start_new_session=True)
        pending, confirmed = bytearray(), False
        deadline = time.monotonic() + self.capture_timeout
        try:
            found = CAPTURE.fullmatch(read_line(process.stdout, pending, deadline, 'Capture command'))
            if not found:
                raise RuntimeError('Capture command did not report a database snapshot')
            snapshot, purge = found[1], None if found[2] == '-' else found[2]
            with open(dump, 'xb', opener=private_file) as output:
                dumper = subprocess.Popen([part.replace('{snapshot}', snapshot) for part in self.dump_command],
                                          stdin=subprocess.DEVNULL, stdout=output, env=self.command_env,
                                          start_new_session=True)
                try:
                    code = dumper.wait(timeout=max(1, deadline - time.monotonic()))
                except subprocess.TimeoutExpired:
                    raise RuntimeError('Database dump timed out') from None
                finally:
                    end_group(dumper)
                if code:
                    raise RuntimeError(f'Database dump failed (exit {code})')
                output.flush()
                os.fsync(output.fileno())
            try:
                process.stdin.write(b'done\n')
                process.stdin.flush()
            except BrokenPipeError:
                raise RuntimeError('Capture command ended before the dump finished') from None
            if read_line(process.stdout, pending, deadline, 'Capture command') != 'ok':
                raise RuntimeError('Capture command did not confirm the capture')
            process.stdin.close()
            try:
                code = process.wait(timeout=max(1, deadline - time.monotonic()))
            except subprocess.TimeoutExpired:
                raise RuntimeError('Capture command did not exit') from None
            if code:
                raise RuntimeError(f'Capture command failed (exit {code})')
            confirmed = True
        finally:
            if not confirmed:
                # End of input aborts a capture: the command ends its database
                # snapshot and releases the lock.
                with suppress(OSError):
                    process.stdin.close()
                try:
                    process.wait(timeout=30)
                except subprocess.TimeoutExpired:
                    end_group(process)
            process.stdout.close()
        if self.capture_dir.is_symlink() or not (self.capture_dir / 'capture.json').is_file():
            raise RuntimeError('The capture holds no capture.json')
        return purge, dump

    def remove_capture(self, strict=True):
        try:
            if self.capture_dir.is_symlink():
                self.capture_dir.unlink()
            elif self.capture_dir.exists():
                shutil.rmtree(self.capture_dir)
        except OSError as error:
            if strict:
                raise RuntimeError(f'Cannot remove the previous capture: {error.strerror}') from None
            logging.warning('Cannot remove the capture at %s: %s', self.capture_dir, error.strerror)

    def next_capture(self):
        """Now, when no capture succeeded since the most recent scheduled time;
        otherwise the next scheduled time, in local time."""
        now = datetime.now()
        slot = datetime.combine(now.date(), self.at)
        if slot > now:
            slot -= timedelta(days=1)
        try:
            last = datetime.fromisoformat(self.last_success).astimezone().replace(tzinfo=None)
        except (TypeError, ValueError):
            last = None
        return now if last is None or last < slot else slot + timedelta(days=1)

    def watch(self):
        logging.info('Instance snapshots daily at %s', self.at.strftime('%H:%M'))
        while True:
            due = self.retry_at or self.next_capture()
            while (remaining := (due - datetime.now()).total_seconds()) > 0:
                time.sleep(min(60, remaining))
            if self.push():
                self.retry_at, self.retry_delay = None, self.retry
            else:
                self.retry_at = datetime.now() + timedelta(seconds=self.retry_delay)
                self.retry_delay = min(self.retry_max, self.retry_delay * 2)

    def verify(self):
        """Download the newest instance snapshot and check it against its manifest."""
        with self.transport() as transport, \
                tempfile.TemporaryDirectory(prefix='verify-', dir=self.state_dir) as directory:
            found = newest_snapshot(self.store(transport), transport, Path(directory))
            if found is None:
                logging.error('No instance snapshot at the destination')
                return False
            name, manifest = found
            archive = Path(directory) / name
            transport.download(name, archive)
            if archive.stat().st_size != manifest['size'] or sha256_file(archive) != manifest['sha256']:
                logging.error('Instance snapshot %s does not match its manifest', name)
                return False
            try:
                with tarfile.open(archive, 'r:gz') as tar:
                    members = set(tar.getnames())
            except (tarfile.TarError, OSError, EOFError):
                members = set()
            if not {'capture/capture.json', 'postgres.sql'} <= members:
                logging.error('Instance snapshot %s is not a complete capture', name)
                return False
        logging.info('Verified instance snapshot %s', name)
        return True


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
        if config.get('rclone'):
            rclone_binary(config, self.roots.values())
        # Active backups by (notebook, kind): a GitHub push and a drive
        # snapshot of one notebook are independent.
        self.active, self.reconcile_at, self.refused = {}, 0, set()

    def tick(self, once=False):
        # Kinds this worker does not know are left to a worker that does.
        bindings = [b for b in self.gateway.call('bindings') if b.get('kind', 'github') in ('github', *DRIVES)]
        # tick owns every push synchronously. A drain acknowledgement is sent only
        # after a prior push (including process termination) has returned.
        for binding in bindings:
            kind = binding.get('kind', 'github')
            if binding['project'] in self.roots and binding.get('handoff'):
                self.active.pop((binding['project'], kind), None)
                self.gateway.call('drain', dict(project=binding['project'], **({} if kind == 'github' else {'kind': kind}),
                                              generation=binding['generation'],
                                              handoff=binding['handoff']))
        allowed = {(b['project'], b.get('kind', 'github')): b for b in bindings
                   if b['project'] in self.roots and not b.get('handoff') and
                   (b.get('kind', 'github') == 'github' or b.get('enabled', True) is not False)}
        for key in list(self.active):
            if key not in allowed or self.active[key].binding['generation'] != allowed[key]['generation']:
                del self.active[key]
        now = time.monotonic()
        force = once or now >= self.reconcile_at
        success = True
        for key, binding in allowed.items():
            project, kind = key
            if not re.fullmatch('[a-z0-9][a-z0-9-]{0,63}', project):
                raise ValueError('Invalid notebook identifier')
            if key not in self.active:
                if kind == 'github':
                    settings = {**self.config, 'repository': self.roots[project], 'state_dir': str(self.state_dir / project)}
                    self.active[key] = ManagedBackup(settings, self.gateway, binding)
                else:
                    settings = {**self.config, 'repository': self.roots[project],
                                'state_dir': str(self.state_dir / (project + '.' + kind))}
                    try:
                        self.active[key] = ManagedSnapshot(settings, self.gateway, binding)
                    except ValueError as error:
                        # One unusable drive binding must not stop the others.
                        if key not in self.refused:
                            logging.error('Skipping the %s snapshot of %s: %s', kind, project, error)
                            self.refused.add(key)
                        success = False
                        continue
                self.active[key].state_dir.mkdir(mode=0o700, parents=True, exist_ok=True)
            backup = self.active[key]
            if backup.identity() != backup.git_identity:
                raise RuntimeError('Notebook storage replaced; restart to reattach')
            requested = backup.binding.get('request') != binding.get('request')
            backup.binding = binding
            backup.observe(force=force or requested)
            if backup.pending_since is not None and not backup.blocked:
                due = max(min(backup.last_change + backup.debounce, backup.pending_since + backup.max_delay), backup.retry_at)
                if once or requested or time.monotonic() >= due:
                    success = backup.push(force=once or requested) and success
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
    parser.add_argument('--verify', action='store_true', help='Snapshot and instance modes: download the newest snapshot and check it')
    args = parser.parse_args()
    os.umask(0o077)
    logging.basicConfig(level=logging.INFO, format='%(asctime)s %(levelname)s %(message)s')
    def stop(signum, frame):
        raise KeyboardInterrupt
    signal.signal(signal.SIGTERM, stop)
    try:
        with open(args.config) as source:
            config = json.load(source)
        modes = {None: Backup, 'github': GitHubBackup, 'managed': ManagedWorker,
                 'snapshot': SnapshotBackup, 'instance': InstanceBackup}
        if config.get('mode') not in modes:
            raise ValueError('Unknown backup mode')
        managed = config.get('mode') == 'managed'
        backup = modes[config.get('mode')](config)
        if args.resume:
            if managed:
                raise ValueError('Resume managed backups through Notebook settings')
            backup.blocked = False
            backup.error = None
        backup.state_dir.mkdir(mode=0o700, parents=True, exist_ok=True)
        if args.verify:
            # Read-only, so it runs beside the worker that holds the state lock.
            if not hasattr(backup, 'verify'):
                raise ValueError('--verify needs snapshot or instance mode')
            return 0 if backup.verify() else 1
        with (backup.state_dir / 'worker.lock').open('a') as lock:
            try:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError:
                raise RuntimeError('Another backup worker holds the state lock') from None
            if args.once:
                if managed:
                    return 0 if backup.tick(once=True) else 1
                return 0 if backup.once() else 1
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
