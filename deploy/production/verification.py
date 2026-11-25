"""Private release receipts and a bounded scheduler for isolated verification processes."""
from concurrent.futures import ThreadPoolExecutor, wait, FIRST_COMPLETED
from contextlib import contextmanager
import fcntl
import hashlib
import json
import math
import os
import re
from pathlib import Path
import signal
import subprocess
import threading
import time

# Nothing the gate runs should take an hour. Go tests have their own 45-minute
# limit, and the slowest image build seen spent about 50 minutes in apt.
CHECK_LIMIT = 3600
# How long a stopped check has to remove its containers.
STOP_GRACE = 30
LIMIT_EXIT = 124  # as timeout(1) reports it


def fingerprint(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True).encode()).hexdigest()


def source_inputs(source, patterns):
    return {str(path.relative_to(source)): hashlib.sha256(path.read_bytes()).hexdigest()
            for path in sorted({path for pattern in patterns for path in source.glob(pattern + "/*" if pattern.endswith("/**") else pattern)
                                if path.is_file() and "__pycache__" not in path.parts})}


def go_package_inputs(source, packages, inventory):
    """Hash each race-test package's real build and test dependency closure."""
    decoder, records = json.JSONDecoder(), {}
    while inventory.strip():
        record, end = decoder.raw_decode(inventory.lstrip())
        inventory = inventory.lstrip()[end:]
        records[record['ImportPath']] = record
    source = source.resolve()
    common = source_inputs(source, ['go.mod', 'go.sum', 'go.work', 'go.work.sum', 'vendor/**'])
    file_fields = ('GoFiles', 'CgoFiles', 'CFiles', 'CXXFiles', 'MFiles', 'HFiles', 'FFiles',
                   'SFiles', 'SwigFiles', 'SwigCXXFiles', 'SysoFiles', 'EmbedFiles')
    test_fields = ('TestGoFiles', 'XTestGoFiles', 'TestEmbedFiles', 'XTestEmbedFiles')
    result = {}
    for package in packages:
        inputs, visited = dict(common), set()

        def visit(name, test=False):
            if name == 'C' or name in visited:
                return
            visited.add(name)
            if name not in records:
                raise ValueError('Incomplete Go dependency inventory: ' + name)
            record = records[name]
            if record.get('Standard'):
                return
            imports = record.get('Imports', [])
            if test:
                imports = imports + record.get('TestImports', []) + record.get('XTestImports', [])
            for dependency in imports:
                visit(dependency)
            module = record.get('Module') or {}
            if not module.get('Main') and (not module.get('Replace') or module['Replace'].get('Version')):
                # Downloaded dependencies are pinned by go.mod/go.sum. Local
                # replacements must be inside the archived source to be reusable.
                return
            directory = Path(record['Dir']).resolve()
            if not directory.is_relative_to(source):
                raise ValueError('Go dependency is outside the committed snapshot: ' + name)
            files = [directory / file for field in file_fields + (test_fields if test else ())
                     for file in record.get(field, [])]
            for path in files:
                if not path.resolve().is_relative_to(source):
                    raise ValueError('Go input is outside the committed snapshot: ' + str(path))
                inputs[str(path.relative_to(source))] = hashlib.sha256(path.read_bytes()).hexdigest()
            # Runtime file reads are not in go list. Keep package fixtures in the
            # closure, including fixtures read by a dependent package's tests.
            inputs.update(source_inputs(source, [str(directory.relative_to(source) / 'testdata') + '/**']))
        visit(package, test=True)
        if package in ('forgetmenote/internal/connectors', 'forgetmenote/internal/automation', 'forgetmenote/internal/web'):
            inputs.update(source_inputs(source, ['deploy/integrations/**']))
        result[package] = inputs
    return result


def stop_group(process):
    """Stop a check's processes, giving each signal STOP_GRACE to work.

    SIGINT comes first, as Ctrl-C would, because smokes then remove their
    containers. SIGTERM follows, because a container can ignore SIGINT and only
    a signal it acts on stops it: killing docker run leaves the container
    running. make exits first and the smoke below it keeps working, so this
    waits for the whole group rather than the process that started it."""
    try:
        for stop in (signal.SIGINT, signal.SIGTERM):
            os.killpg(process.pid, stop)
            deadline = time.monotonic() + STOP_GRACE
            while time.monotonic() < deadline:
                process.poll()
                os.killpg(process.pid, 0)
                time.sleep(.05)
        os.killpg(process.pid, signal.SIGKILL)
    except (ProcessLookupError, PermissionError):
        pass  # macOS reports a group with only exited members as PermissionError.
    process.wait()


@contextmanager
def release_lock(directory):
    directory.mkdir(parents=True, exist_ok=True, mode=0o700)
    directory.chmod(0o700)
    with (directory / 'lock').open('a') as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            raise ValueError('Another release owns this state directory; resume after it finishes.') from None
        yield lock.fileno()


class Verification:
    def __init__(self, directory, revision, tools, *, retry_failed=False, lock_fd=None):
        self.directory = directory
        self.candidate = directory / revision
        self.candidate.mkdir(exist_ok=True)
        self.revision = revision
        self.tools = tools
        self.retry_failed = retry_failed
        self.image_id = None
        self.lock_fd = lock_fd
        self.started = time.monotonic()
        self.mutex = threading.Lock()
        self.running = set()
        self.results = {}
        self.complete = False
        self.expected_checks = []
        self.receipts = directory / 'checks'
        self.receipts.mkdir(exist_ok=True)
        previous = self.candidate / 'verification.json'
        prior = json.loads(previous.read_text()) if previous.exists() else {}
        self.plan = prior.get('plan', {})
        self.results = {name: value for name, value in prior.get('checks', {}).items()
                        if value.get('status') == 'failed' or value.get('unresolved_failure')}
        self.history = {}
        for path in directory.glob('*/verification.json'):
            for name, value in json.loads(path.read_text()).get('checks', {}).items():
                if (value.get('status') == 'passed' and value.get('exit_code') == 0
                        and value.get('started_at', 0) > self.history.get(name, {}).get('started_at', -1)):
                    self.history[name] = value

    def selected_tools(self, tool_keys):
        return self.tools if tool_keys is None else {key: self.tools.get(key) for key in tool_keys}

    def key(self, name, inputs, tool_keys=None):
        return fingerprint({'name': name, 'inputs': inputs, 'tools': self.selected_tools(tool_keys)})

    def saved(self, name, inputs, tool_keys=None):
        path = self.receipts / (self.key(name, inputs, tool_keys) + '.json')
        if not path.exists():
            return None
        value = json.loads(path.read_text())
        if (value.get('key') != path.stem or value.get('status') not in ('running', 'passed', 'failed')
                or (value.get('status') == 'passed' and value.get('exit_code') != 0)):
            raise ValueError('Invalid verification receipt: ' + str(path))
        return value

    def write(self, path, value):
        temporary = path.with_suffix('.tmp')
        temporary.write_text(json.dumps(value, indent=2) + '\n')
        temporary.replace(path)

    def record(self, name, value, *, reused=False):
        with self.mutex:
            if name is not None:
                previous = self.results.get(name, {})
                failure = previous.get('unresolved_failure') or (previous if previous.get('status') == 'failed' else None)
                current = {**value, 'reused': reused}
                if failure and not (value.get('status') == 'passed' and value.get('exit_code') == 0):
                    current['unresolved_failure'] = failure
                self.results[name] = current
            self.write(self.candidate / 'verification.json', {
                'source_revision': self.revision, 'image_id': self.image_id,
                'elapsed_seconds': round(time.monotonic() - self.started, 3),
                'complete': self.complete, 'expected_checks': self.expected_checks,
                'plan': self.plan, 'checks': self.results})

    def check(self, name, command, inputs, *, cwd, env=None, force=False, tool_keys=None, limit=CHECK_LIMIT):
        previous = self.saved(name, inputs, tool_keys)
        if previous and previous['status'] == 'passed' and not force:
            self.record(name, previous, reused=True)
            print('REUSE ' + name + ' (' + previous['source_revision'][:12] + ')', flush=True)
            return
        failure = self.results.get(name, {})
        if ((previous and previous['status'] == 'failed') or failure.get('status') == 'failed'
                or failure.get('unresolved_failure')) and not self.retry_failed:
            raise ValueError(f'{name} previously failed. Inspect {(previous or failure).get("log", "original receipt")}; use --retry-failed only after investigation.')
        key = self.key(name, inputs, tool_keys)
        attempt = time.time_ns()
        log = self.candidate / f'{name}-{attempt}.log'
        receipt = self.candidate / f'{name}-{attempt}.json'
        environment = {**os.environ, **(env or {})}
        if name == 'browser':
            environment['FMN_BROWSER_REPORT'] = str(self.candidate / f'browser-{attempt}.report.json')
            environment['FMN_BROWSER_RESULTS'] = str(self.candidate / f'browser-{attempt}-results')
        value = {'name': name, 'key': key, 'source_revision': self.revision,
                 'operator_revision': self.plan.get('operator_revision', self.revision),
                 'image_id': self.image_id, 'command': list(map(str, command)), 'inputs': inputs,
                 'tools': self.selected_tools(tool_keys), 'artifacts': {key: environment[key] for key in ('FMN_BROWSER_REPORT', 'FMN_BROWSER_RESULTS') if key in environment}, 'status': 'running', 'log': str(log), 'started_at': time.time()}
        self.write(receipt, value)
        self.write(self.receipts / (key + '.json'), value)
        self.record(name, value)
        print('RUN ' + name + '\n  log: ' + str(log), flush=True)
        started = time.monotonic()
        limited = False
        try:
            with log.open('w') as output:
                # Its own process group, so that the limit reaches what it starts.
                process = subprocess.Popen(command, cwd=cwd, env=environment, start_new_session=True,
                                           pass_fds=() if self.lock_fd is None else (self.lock_fd,),
                                           stdout=output, stderr=subprocess.STDOUT)
            with self.mutex:
                self.running.add(process)
            try:
                code = process.wait(timeout=limit)
            except subprocess.TimeoutExpired:
                stop_group(process)
                code, limited = LIMIT_EXIT, True
                with log.open('a') as output:
                    output.write(f'\nStopped at the {limit} s limit.\n')
            finally:
                with self.mutex:
                    self.running.discard(process)
        except OSError as error:
            log.write_text(str(error) + '\n')
            code = 127
        value.update(status='passed' if code == 0 else 'failed', exit_code=code,
                     elapsed_seconds=round(time.monotonic() - started, 3))
        self.write(receipt, value)
        self.write(self.receipts / (key + '.json'), value)
        self.record(name, value)
        print(f'{value["status"].upper()} {name}: {value["elapsed_seconds"]:.1f}s', flush=True)
        if code:
            # The first failure remains in its own immutable attempt log; never
            # retry it automatically or discard it after a later successful run.
            print(log.read_text(errors='replace')[-16000:], flush=True)
            reason = f'stopped at its {limit} s limit' if limited else f'exit {code}'
            raise ValueError(f'{name} failed ({reason}). Full original log: {log}')

    def go_timings(self):
        """Measured package times and top-level case times by package."""
        packages, cases = {}, {}
        for name, value in sorted(self.history.items(), key=lambda item: item[1].get('started_at', 0)):
            if not name.startswith('go-') or not Path(value['log']).is_file():
                continue
            with Path(value['log']).open() as log:
                for line in log:
                    try:
                        event = json.loads(line)
                    except ValueError:
                        continue
                    if not isinstance(event, dict):
                        continue
                    elapsed = event.get('Elapsed', 0)
                    if event.get('Action') != 'pass' or not isinstance(elapsed, (int, float)) or not math.isfinite(elapsed):
                        continue
                    package, test = event.get('Package', ''), event.get('Test', '')
                    if not test:
                        packages[package] = elapsed
                    elif '/' not in test:
                        cases.setdefault(package, {})[test] = elapsed
        # A batch reports only its own package time, so a batched package keeps
        # its whole measured cost through its cases instead of reverting to one check.
        for package in packages:
            packages[package] = max(packages[package], sum(cases.get(package, {}).values()))
        return packages, cases

    def package_batched(self, package, measured_long):
        """Whether a Go package runs in process batches, frozen for this candidate.

        The candidate's own runs update measured times, so deciding again on
        resume or publication would replace checks it already verified. Frozen
        batches are in package_shards; this records packages run as one check."""
        if package in self.plan.get('package_shards', {}):
            return True
        unbatched = self.plan.setdefault('unbatched_packages', [])
        if package not in unbatched and not measured_long:
            unbatched.append(package)
        return package not in unbatched

    def partition_web(self, names, durations):
        return self.partition(names, durations, lambda plan: plan, 'web_shards', 4, 16)

    def partition_package(self, package, names, durations):
        return self.partition(names, durations, lambda plan: plan.setdefault('package_shards', {}), package, 2, 8)

    def partition(self, names, durations, frozen, key, least, most):
        expected = sorted(name for shard in web_shards(names) for name in shard)
        shards = frozen(self.plan).get(key, [])
        if not shards or sorted(name for shard in shards for name in shard) != expected:
            # Shorter process batches avoid a ten-minute tail when browser and
            # Docker work occupy slots. Preserve existing membership on resume.
            count = min(most, max(least, math.ceil(sum(durations.get(name, 1) for name in expected) / 90)))
            shards = web_shards(expected, count=count, durations=durations)
            frozen(self.plan)[key] = shards
            self.record(None, None)
        return shards

    def run(self, checks, *, jobs):
        """A check costs host slots; Docker checks additionally share one lane."""
        if not 1 <= jobs <= 12:
            raise ValueError('--jobs must be between 1 and 12')
        # Keep the long serial Docker lane busy while host work fills other slots.
        def priority(check):
            if check.get('preflight'):
                return (-1, 0)
            if check.get('docker'):
                return (0, 0)
            elapsed = check.get('estimate', self.history.get(check['name'], {}).get('elapsed_seconds', 0))
            return (1, -elapsed)
        pending = sorted(checks, key=priority)
        self.expected_checks = [check["name"] for check in pending]
        if len(self.expected_checks) != len(set(self.expected_checks)):
            raise ValueError("Verification check names must be unique.")
        self.record(None, None)

        def admit(checks):
            for check in checks:
                saved = self.saved(check['name'], check['inputs'], check.get('tool_keys'))
                outstanding = self.results.get(check['name'], {})
                valid_saved = saved and saved['status'] == 'passed' and not check.get('force')
                if not valid_saved and not self.retry_failed and ((saved and saved['status'] == 'failed')
                        or outstanding.get('status') == 'failed' or outstanding.get('unresolved_failure')):
                    raise ValueError(f'{check["name"]} previously failed. Inspect {(saved or outstanding).get("log", "original receipt")}; use --retry-failed after investigation.')
        admit(pending)
        active, errors = {}, []
        with ThreadPoolExecutor(max_workers=jobs) as executor:
            while pending or active:
                used = sum(entry[0] for entry in active.values())
                docker_busy = any(entry[1] for entry in active.values())
                preflight = any(check.get('preflight') for check in pending) or any(entry[2] for entry in active.values())
                for check in pending[:]:
                    cost = min(check.get('slots', 1), jobs)
                    docker = check.get('docker', False)
                    # An eager check (the image build) only produces inputs; it
                    # overlaps preflight but never hides a preflight failure.
                    held = preflight and not check.get('preflight') and not check.get('eager')
                    if errors or held or used + cost > jobs or (docker and docker_busy):
                        continue
                    pending.remove(check)
                    args = {k: v for k, v in check.items() if k not in ('slots', 'docker', 'estimate', 'preflight', 'eager', 'then')}
                    future = executor.submit(self.check, **args)
                    active[future] = (cost, docker, check.get('preflight', False), check.get('then'))
                    used += cost
                    docker_busy |= docker
                if not active:
                    break
                try:
                    done, _ = wait(active, return_when=FIRST_COMPLETED)
                except KeyboardInterrupt:
                    # The terminal's signal misses checks in their own process groups.
                    with self.mutex:
                        for process in self.running:
                            try:
                                os.killpg(process.pid, signal.SIGINT)
                            except (ProcessLookupError, PermissionError):
                                pass
                    raise
                for future in done:
                    then = active.pop(future)[3]
                    try:
                        future.result()
                        if then and not errors:
                            # Checks that need this result, such as containers of the built image.
                            later = then()
                            names = [check['name'] for check in later]
                            if set(names) & set(self.expected_checks) or len(names) != len(set(names)):
                                raise ValueError("Verification check names must be unique.")
                            admit(later)
                            self.expected_checks += names
                            pending = sorted(pending + later, key=priority)
                            self.record(None, None)
                    except Exception as error:
                        errors.append(error)
            if errors:
                raise ValueError('\n'.join(map(str, errors)))
        self.complete = (all(self.results.get(name, {}).get('status') == 'passed' for name in self.expected_checks)
                         and not any(value.get('status') == 'failed' or value.get('unresolved_failure')
                                     for value in self.results.values()))
        self.record(None, None)
        if not self.complete:
            raise ValueError('Verification stopped before every required check passed.')


def web_shards(names, count=4, durations=None):
    """Separate processes isolate tests that mutate Go globals and the environment."""
    names = sorted(name for name in names if re.fullmatch(r'(Test|Example|Fuzz)\w*', name))
    if not names or len(names) != len(set(names)):
        raise ValueError('Cannot partition the web suite: missing or duplicate test names.')
    if not durations:
        return [names[index::count] for index in range(min(count, len(names)))]
    shards = [[] for _ in range(min(count, len(names)))]
    weights = [0.] * len(shards)
    for name in sorted(names, key=lambda name: (-durations.get(name, 1), name)):
        index = min(range(len(shards)), key=lambda index: (weights[index], len(shards[index]), index))
        shards[index].append(name)
        weights[index] += durations.get(name, 1)
    return [sorted(shard) for shard in shards]
