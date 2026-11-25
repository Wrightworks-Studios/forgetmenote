"""Exercise receipts and resource scheduling with real, disposable processes."""
import json
import os
from pathlib import Path
import signal
import sys
import subprocess
import tempfile
import time
import unittest
from unittest import mock

from verification import Verification, go_package_inputs, release_lock, source_inputs, web_shards


class VerificationTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.count = self.root / 'executions'
        self.command = [sys.executable, '-c',
                        "from pathlib import Path; p=Path('executions'); p.write_text(p.read_text()+'x' if p.exists() else 'x')"]

    def verifier(self, revision='a', **kwargs):
        return Verification(self.root, revision, {'python': sys.version}, **kwargs)

    def test_unchanged_inputs_reuse_success_with_revision_attribution(self):
        first = self.verifier()
        first.check('test', self.command, {'source': 'same'}, cwd=self.root)
        second = self.verifier('b')
        second.check('test', self.command, {'source': 'same'}, cwd=self.root)
        self.assertEqual(self.count.read_text(), 'x')
        record = json.loads((second.candidate / 'verification.json').read_text())
        self.assertEqual(record['source_revision'], 'b')
        self.assertEqual(record['checks']['test']['source_revision'], 'a')
        self.assertTrue(record['checks']['test']['reused'])
        second.check('test', self.command, {'source': 'changed'}, cwd=self.root)
        self.assertEqual(self.count.read_text(), 'xx')

    def test_attempt_records_executor_without_invalidating_unchanged_pass(self):
        runner = self.verifier()
        runner.plan['operator_revision'] = 'operator-one'
        runner.check('test', self.command, {}, cwd=self.root)
        resumed = self.verifier()
        resumed.plan['operator_revision'] = 'operator-two'
        resumed.check('test', self.command, {}, cwd=self.root)
        self.assertEqual(self.count.read_text(), 'x')
        self.assertEqual(resumed.results['test']['operator_revision'], 'operator-one')
        resumed.check('new', self.command, {}, cwd=self.root)
        attempt = next(resumed.candidate.glob('new-*.json'))
        self.assertEqual(json.loads(attempt.read_text())['operator_revision'], 'operator-two')

    def test_image_and_tool_changes_invalidate_receipts(self):
        first = self.verifier()
        first.check('smoke', self.command, {'image': 'one'}, cwd=self.root)
        first.check('smoke', self.command, {'image': 'two'}, cwd=self.root)
        changed = Verification(self.root, 'b', {'python': 'different'})
        changed.check('smoke', self.command, {'image': 'two'}, cwd=self.root)
        self.assertEqual(self.count.read_text(), 'xxx')

    def test_only_relevant_toolchain_changes_invalidate_a_scoped_check(self):
        first = Verification(self.root, 'a', {'go': 'same', 'docker': 'old'})
        first.check('go-package', self.command, {}, cwd=self.root, tool_keys=['go'])
        second = Verification(self.root, 'b', {'go': 'same', 'docker': 'new'})
        second.check('go-package', self.command, {}, cwd=self.root, tool_keys=['go'])
        self.assertEqual(self.count.read_text(), 'x')
        third = Verification(self.root, 'c', {'go': 'changed', 'docker': 'new'})
        third.check('go-package', self.command, {}, cwd=self.root, tool_keys=['go'])
        self.assertEqual(self.count.read_text(), 'xx')

    def test_interrupted_check_is_not_success_but_completed_check_is_reused(self):
        runner = self.verifier()
        runner.check('completed', self.command, {}, cwd=self.root)
        key = runner.key('interrupted', {})
        runner.write(runner.receipts / (key + '.json'), {'key': key, 'status': 'running'})
        resumed = self.verifier()
        resumed.check('completed', self.command, {}, cwd=self.root)
        resumed.check('interrupted', self.command, {}, cwd=self.root)
        self.assertEqual(self.count.read_text(), 'xx')

    def test_failure_retains_original_evidence_and_requires_explicit_retry(self):
        runner = self.verifier()
        with self.assertRaisesRegex(ValueError, 'exit 9'):
            runner.check('failed', [sys.executable, '-c', "print('original HTTP 409 conflict'); exit(9)"], {}, cwd=self.root)
        with self.assertRaisesRegex(ValueError, 'retry-failed'):
            self.verifier().check('failed', self.command, {}, cwd=self.root)
        self.assertFalse(self.count.exists())
        self.verifier(retry_failed=True).check('failed', self.command, {}, cwd=self.root)
        logs = list(runner.candidate.glob('failed-*.log'))
        self.assertEqual(len(logs), 2)
        self.assertTrue(any('original HTTP 409 conflict' in p.read_text() for p in logs))

    def test_interrupted_retry_retains_failure_until_valid_pass(self):
        runner = self.verifier()
        with self.assertRaises(ValueError):
            runner.check('test', [sys.executable, '-c', 'exit(9)'], {}, cwd=self.root)
        original = runner.results['test']
        retry = self.verifier(retry_failed=True)
        running = {**original, 'status': 'running'}
        running.pop('exit_code')
        retry.record('test', running)
        retry.write(retry.receipts / (running['key'] + '.json'), running)
        resumed = self.verifier()
        self.assertEqual(resumed.results['test']['unresolved_failure']['exit_code'], 9)
        with self.assertRaisesRegex(ValueError, 'retry-failed'):
            resumed.check('test', self.command, {}, cwd=self.root)
        with self.assertRaisesRegex(ValueError, 'every required check'):
            resumed.run([], jobs=1)
        final = self.verifier(retry_failed=True)
        final.check('test', self.command, {}, cwd=self.root)
        self.assertNotIn('unresolved_failure', final.results['test'])
        final.run([], jobs=1)
        self.assertTrue(final.complete)

    def test_check_past_its_limit_fails_and_stops_every_process(self):
        script = """
import subprocess,sys,time
child=subprocess.Popen([sys.executable,'-c','import time; time.sleep(60)'])
open('child','w').write(str(child.pid))
time.sleep(60)
"""
        runner = self.verifier()
        started = time.monotonic()
        with self.assertRaisesRegex(ValueError, r'stuck failed \(stopped at its 2 s limit\)'):
            runner.run([dict(name='stuck', command=[sys.executable, '-c', script], inputs={}, cwd=self.root, limit=2)], jobs=1)
        self.assertLess(time.monotonic() - started, 20)
        record = runner.results['stuck']
        self.assertEqual((record['status'], record['exit_code']), ('failed', 124))
        self.assertIn('2 s limit', Path(record['log']).read_text())
        with self.assertRaises(ProcessLookupError):
            os.kill(int((self.root / 'child').read_text()), 0)
        with self.assertRaisesRegex(ValueError, 'retry-failed'):
            self.verifier().check('stuck', self.command, {}, cwd=self.root)

    def test_stopped_check_finishes_cleaning_up_after_its_first_process_exits(self):
        # make leaves first, while the smoke below it removes its containers.
        cleanup = "from pathlib import Path\nimport time\ntry:\n time.sleep(60)\nfinally:\n time.sleep(.3)\n Path('cleaned').touch()\n"
        script = f"import subprocess,sys,time\nsubprocess.Popen([sys.executable,'-c',{cleanup!r}])\ntime.sleep(60)\n"
        with self.assertRaisesRegex(ValueError, 'limit'):
            self.verifier().check('stuck', [sys.executable, '-c', script], {}, cwd=self.root, limit=2)
        self.assertTrue((self.root / 'cleaned').exists())

    def test_check_that_ignores_interrupt_is_terminated(self):
        # A container can ignore SIGINT, and killing docker run would leave it running.
        script = """
from pathlib import Path
import signal,sys,time
def stop(*_):
 Path('terminated').touch()
 sys.exit(1)
signal.signal(signal.SIGINT,signal.SIG_IGN)
signal.signal(signal.SIGTERM,stop)
time.sleep(60)
"""
        with mock.patch('verification.STOP_GRACE', .2), self.assertRaisesRegex(ValueError, 'limit'):
            self.verifier().check('stuck', [sys.executable, '-c', script], {}, cwd=self.root, limit=1)
        self.assertTrue((self.root / 'terminated').exists())

    def test_check_that_ignores_every_stop_signal_is_killed(self):
        script = "import signal,time\nfor stop in (signal.SIGINT,signal.SIGTERM):\n signal.signal(stop,signal.SIG_IGN)\ntime.sleep(60)\n"
        started = time.monotonic()
        with mock.patch('verification.STOP_GRACE', .2), self.assertRaisesRegex(ValueError, 'limit'):
            self.verifier().check('stuck', [sys.executable, '-c', script], {}, cwd=self.root, limit=1)
        self.assertLess(time.monotonic() - started, 20)

    def test_interrupting_the_gate_stops_its_running_checks(self):
        check = "import os,time\nopen('started','w').write(str(os.getpid()))\ntime.sleep(60)\n"
        script = f"""
from pathlib import Path
import sys
from verification import Verification
root=Path(sys.argv[1])
Verification(root,'a',{{}}).run([dict(name='long',command=[sys.executable,'-c',{check!r}],inputs={{}},cwd=root)],jobs=1)
"""
        gate = subprocess.Popen([sys.executable, '-c', script, str(self.root)], cwd=Path(__file__).parent,
                                stderr=subprocess.DEVNULL)
        self.addCleanup(gate.wait)
        self.addCleanup(gate.kill)
        started = self.root / 'started'
        deadline = time.monotonic() + 15
        while not (started.exists() and started.read_text()):
            self.assertLess(time.monotonic(), deadline)
            time.sleep(.01)
        gate.send_signal(signal.SIGINT)
        self.assertNotEqual(gate.wait(timeout=20), 0)
        with self.assertRaises(ProcessLookupError):
            os.kill(int(started.read_text()), 0)

    def test_docker_lane_is_serial_while_independent_checks_overlap(self):
        # Independent processes rendezvous. If the scheduler serializes them, the
        # deadline fails; mkdir exclusively detects overlapping Docker scenarios.
        script = """
from pathlib import Path
import sys,time
name,docker=sys.argv[1:]
if docker=='yes': Path('docker-busy').mkdir()
Path(name).touch()
deadline=time.monotonic()+5
while not Path('docker-first').exists() or not Path('host').exists():
 if time.monotonic()>deadline: raise RuntimeError('independent checks did not overlap')
 time.sleep(.01)
time.sleep(.05)
if docker=='yes': Path('docker-busy').rmdir()
"""
        checks = [dict(name=name, command=[sys.executable, '-c', script, name, 'yes' if docker else 'no'],
                       inputs={}, cwd=self.root, docker=docker)
                  for name, docker in [('docker-first', True), ('docker-second', True), ('host', False)]]
        self.verifier().run(checks, jobs=2)

    def test_long_docker_lane_is_not_starved_by_queued_host_work(self):
        script = """
from pathlib import Path
import sys,time
name=sys.argv[1]
Path(name).touch()
if name=='docker-first':
 while not Path('host-first').exists(): time.sleep(.01)
if name=='host-first':
 deadline=time.monotonic()+5
 while not Path('docker-second').exists():
  if time.monotonic()>deadline: raise RuntimeError('Docker lane was starved')
  time.sleep(.01)
if name=='host-second' and not Path('docker-second').exists():
 raise RuntimeError('queued host work displaced the idle Docker lane')
"""
        checks = [dict(name=name, command=[sys.executable, '-c', script, name],
                       inputs={}, cwd=self.root, docker=name.startswith('docker'))
                  for name in ['docker-first', 'host-first', 'host-second', 'docker-second']]
        self.verifier().run(checks, jobs=2)

    def test_long_host_checks_start_first_using_previous_successful_timings(self):
        previous = self.verifier()
        previous.check('short', [sys.executable, '-c', 'pass'], {'version': 1}, cwd=self.root)
        previous.check('long', [sys.executable, '-c', 'import time; time.sleep(.15)'], {'version': 1}, cwd=self.root)
        script = "from pathlib import Path; import sys; p=Path('order'); p.write_text((p.read_text() if p.exists() else '')+sys.argv[1]+'\\n')"
        checks = [dict(name=name, command=[sys.executable, '-c', script, name], inputs={'version': 2}, cwd=self.root)
                  for name in ('short', 'long')]
        self.verifier('b').run(checks, jobs=1)
        self.assertEqual((self.root / 'order').read_text().splitlines(), ['long', 'short'])

    def test_image_build_overlaps_preflight_and_its_containers_follow_it(self):
        # Preflight and the build rendezvous; if the build waited behind
        # preflight, the deadline fails. Containers exist only after the build.
        script = """
from pathlib import Path
import sys,time
name=sys.argv[1]
Path(name+'-started').touch()
if name in ('build','contracts'):
 other={'build':'contracts','contracts':'build'}[name]
 deadline=time.monotonic()+5
 while not Path(other+'-started').exists():
  if time.monotonic()>deadline: raise RuntimeError(name+' did not overlap '+other)
  time.sleep(.01)
if name=='build': time.sleep(.1); Path('image').write_text('built')
if name=='smoke' and Path('image').read_text()!='built': raise RuntimeError('container started before its image')
"""
        command = lambda name: [sys.executable, '-c', script, name]
        runner = self.verifier()
        later = lambda: [dict(name='smoke', command=command('smoke'), inputs={'image': (self.root / 'image').read_text()},
                              cwd=self.root, docker=True)]
        checks = [dict(name='build', command=command('build'), inputs={}, cwd=self.root, docker=True, eager=True, then=later),
                  dict(name='contracts', command=command('contracts'), inputs={}, cwd=self.root, preflight=True),
                  dict(name='host', command=command('host'), inputs={}, cwd=self.root)]
        runner.run(checks, jobs=2)
        record = json.loads((self.root / 'a/verification.json').read_text())
        self.assertTrue(record['complete'])
        self.assertCountEqual(record['expected_checks'], ['build', 'contracts', 'host', 'smoke'])
        self.assertEqual(record['checks']['smoke']['status'], 'passed')

    def test_failed_build_never_starts_its_containers(self):
        later = lambda: [dict(name='smoke', command=self.command, inputs={}, cwd=self.root, docker=True)]
        checks = [dict(name='build', command=[sys.executable, '-c', 'exit(3)'], inputs={}, cwd=self.root,
                       docker=True, eager=True, then=later)]
        with self.assertRaisesRegex(ValueError, 'build failed'):
            self.verifier().run(checks, jobs=2)
        self.assertFalse(self.count.exists())

    def test_failed_preflight_never_starts_expensive_checks(self):
        checks = [dict(name='docker', command=self.command, inputs={}, cwd=self.root, docker=True),
                  dict(name='contracts', command=[sys.executable, '-c', 'exit(2)'], inputs={},
                       cwd=self.root, preflight=True)]
        with self.assertRaisesRegex(ValueError, 'contracts failed'):
            self.verifier().run(checks, jobs=2)
        self.assertFalse(self.count.exists())

    def test_measured_slow_web_suite_uses_short_process_batches(self):
        runner = self.verifier()
        names = ['TestCase' + str(n) for n in range(100)]
        shards = runner.partition_web(names, {name: 12 for name in names})
        self.assertGreater(len(shards), 4)
        self.assertLessEqual(len(shards), 16)
        self.assertCountEqual([name for shard in shards for name in shard], names)
        self.assertLessEqual(max(len(shard) * 12 for shard in shards), 96)
        self.assertEqual(self.verifier().partition_web(names, {name: 1 for name in names}), shards)

    def test_state_directory_cannot_have_two_owners(self):
        with release_lock(self.root):
            with self.assertRaisesRegex(ValueError, 'Another release'):
                with release_lock(self.root):
                    self.fail('must not enter')
        self.assertEqual(self.root.stat().st_mode & 0o777, 0o700)

    def test_browser_report_survives_writing_the_verification_receipt(self):
        runner = self.verifier()
        command = [sys.executable, '-c',
                   "import os; from pathlib import Path; Path(os.environ['FMN_BROWSER_REPORT']).write_text('{\"stats\":{\"expected\":1}}')"]
        runner.check('browser', command, {}, cwd=self.root)
        report = Path(runner.results['browser']['artifacts']['FMN_BROWSER_REPORT'])
        self.assertEqual(json.loads(report.read_text()), {'stats': {'expected': 1}})
        self.assertEqual(runner.saved('browser', {})['exit_code'], 0)

    def test_invalid_success_receipt_is_rejected(self):
        runner = self.verifier()
        key = runner.key('test', {})
        runner.write(runner.receipts / (key + '.json'), {'key': key, 'status': 'passed', 'exit_code': 3})
        with self.assertRaisesRegex(ValueError, 'Invalid verification receipt'):
            runner.check('test', self.command, {}, cwd=self.root)
        self.assertFalse(self.count.exists())

    def test_recursive_inputs_detect_edits_additions_and_deletions(self):
        directory = self.root / 'nested/deeper'
        directory.mkdir(parents=True)
        note = directory / 'test.txt'
        note.write_text('before')
        before = source_inputs(self.root, ['nested/**'])
        self.assertEqual(list(before), ['nested/deeper/test.txt'])
        note.write_text('after')
        self.assertNotEqual(before, source_inputs(self.root, ['nested/**']))
        note.unlink()
        self.assertEqual(source_inputs(self.root, ['nested/**']), {})

    def test_shards_cover_every_test_example_and_fuzz_seed_once(self):
        names = ['TestZ', 'TestA', 'TestB', 'ExampleAPI', 'FuzzParse', 'BenchmarkSpeed', 'ok module .1s']
        shards = web_shards(names)
        flattened = [name for shard in shards for name in shard]
        self.assertCountEqual(flattened, names[:5])
        self.assertEqual(len(flattened), len(set(flattened)))
        self.assertEqual(shards, web_shards(reversed(names)))
        with self.assertRaises(ValueError):
            web_shards([])

    def test_measured_shards_balance_long_cases_and_freeze_on_resume(self):
        names = ['Test' + str(i) for i in range(8)]
        timings = dict(zip(names, [100, 1, 1, 1, 100, 1, 1, 1]))
        runner = self.verifier()
        shards = runner.partition_web(names, timings)
        self.assertCountEqual([name for shard in shards for name in shard], names)
        self.assertLessEqual(max(sum(timings[name] for name in shard) for shard in shards), 101)
        resumed = self.verifier()
        self.assertEqual(resumed.partition_web(names, {name: 1 for name in names}), shards)
        changed = resumed.partition_web(names + ['ExampleNew'], timings)
        self.assertCountEqual([name for shard in changed for name in shard], names + ['ExampleNew'])

    def test_schedule_imports_package_and_top_level_case_timings_from_retained_go_logs(self):
        events = [{'Action': 'pass', 'Package': 'fixture/internal/web', 'Test': 'TestFirst', 'Elapsed': 8},
                  {'Action': 'pass', 'Package': 'fixture/internal/web', 'Test': 'TestFirst/child', 'Elapsed': 7},
                  {'Action': 'pass', 'Package': 'fixture/workspace', 'Test': 'TestStore', 'Elapsed': 11},
                  {'Action': 'pass', 'Package': 'fixture/workspace', 'Elapsed': 12}]
        command = [sys.executable, '-c', 'print(' + repr('\n'.join(json.dumps(row) for row in events) + '\n[]\ncompile output') + ')']
        self.verifier().check('go-other', command, {}, cwd=self.root)
        packages, cases = self.verifier('b').go_timings()
        self.assertEqual(packages, {'fixture/workspace': 12})
        self.assertEqual(cases, {'fixture/internal/web': {'TestFirst': 8}, 'fixture/workspace': {'TestStore': 11}})

    def test_long_package_is_split_into_frozen_process_batches(self):
        names = ['Test' + str(i) for i in range(30)] + ['ExampleUse']
        timings = {name: 20 for name in names}
        shards = self.verifier().partition_package('fixture/slow', names, timings)
        self.assertGreaterEqual(len(shards), 2)
        self.assertLessEqual(len(shards), 8)
        self.assertCountEqual([name for shard in shards for name in shard], names)
        self.assertEqual(self.verifier().partition_package('fixture/slow', names, {}), shards)
        self.assertEqual(self.verifier().partition_web(names, timings), self.verifier().partition_web(names, timings))


class GoInputsTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.write('go.mod', 'module fixture\n\ngo 1.27\n')
        self.write('leaf/leaf.go', 'package leaf\nimport _ "embed"\n//go:embed message.txt\nvar Message string\n')
        self.write('leaf/message.txt', 'original')
        self.write('leaf/race.go', '//go:build race\n\npackage leaf\nconst Race = true\n')
        self.write('dep/dep.go', 'package dep\nimport "fixture/leaf"\nvar Message = leaf.Message\n')
        self.write('helper/helper.go', 'package helper\nconst Value = 1\n')
        self.write('app/app.go', 'package app\nimport "fixture/dep"\nvar Message = dep.Message\n')
        self.write('app/app_test.go', 'package app_test\nimport ("testing"; "fixture/helper")\nfunc TestValue(t *testing.T) { if helper.Value == 0 { t.Fatal("zero") } }\n')
        self.write('other/other.go', 'package other\n')

    def write(self, path, content):
        target = self.root / path
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(content)

    def inputs(self):
        inventory = subprocess.run(['go', 'list', '-race', '-deps', '-test', '-json', './...'],
                                   cwd=self.root, capture_output=True, text=True, check=True).stdout
        return go_package_inputs(self.root, ['fixture/app', 'fixture/other'], inventory)

    def test_real_go_inventory_tracks_transitive_test_and_embedded_inputs(self):
        original = self.inputs()
        self.assertIn('leaf/race.go', original['fixture/app'])
        for path in ('leaf/message.txt', 'dep/dep.go', 'helper/helper.go', 'app/app_test.go'):
            with self.subTest(path=path):
                before = (self.root / path).read_text()
                self.write(path, before + ('\nchanged' if path.endswith('.txt') else '\n// changed\n'))
                changed = self.inputs()
                self.assertNotEqual(original['fixture/app'], changed['fixture/app'])
                self.assertEqual(original['fixture/other'], changed['fixture/other'])
                self.write(path, before)
        self.write('app/testdata/input.json', '{}')
        added = self.inputs()
        self.assertNotEqual(original['fixture/app'], added['fixture/app'])
        (self.root / 'app/testdata/input.json').unlink()
        self.assertEqual(original, self.inputs())

    def test_unrelated_tests_do_not_invalidate_consumers_but_dependency_additions_do(self):
        original = self.inputs()
        self.write('leaf/leaf_test.go', 'package leaf\n')
        self.write('other/other.go', 'package other\nconst Changed = true\n')
        changed = self.inputs()
        self.assertEqual(original['fixture/app'], changed['fixture/app'])
        self.assertNotEqual(original['fixture/other'], changed['fixture/other'])
        self.write('app/new.go', 'package app\nimport _ "fixture/other"\n')
        added = self.inputs()
        self.assertIn('other/other.go', added['fixture/app'])
        (self.root / 'app/new.go').unlink()
        self.assertEqual(changed, self.inputs())

    def test_missing_inventory_and_external_local_replacements_fail_closed(self):
        with self.assertRaisesRegex(ValueError, 'Incomplete Go dependency'):
            go_package_inputs(self.root, ['fixture/missing'], '')
        record = {'ImportPath': 'fixture/app', 'Dir': str(self.root.parent),
                  'Module': {'Replace': {'Dir': str(self.root.parent)}}}
        with self.assertRaisesRegex(ValueError, 'outside the committed snapshot'):
            go_package_inputs(self.root, ['fixture/app'], json.dumps(record))

    def test_runtime_deployment_examples_invalidate_their_consumer_packages(self):
        self.write('deploy/integrations/automations/example.json', '{}')
        packages = ['forgetmenote/internal/' + name for name in ('connectors', 'automation', 'web')]
        inventory = '\n'.join(json.dumps({'ImportPath': name, 'Dir': str(self.root), 'Module': {'Main': True}}) for name in packages)
        before = go_package_inputs(self.root, packages, inventory)
        self.write('deploy/integrations/automations/example.json', '{"changed":true}')
        after = go_package_inputs(self.root, packages, inventory)
        for name in packages:
            self.assertNotEqual(before[name], after[name])

    def test_local_replacement_below_a_downloaded_dependency_is_still_hashed(self):
        records = [{'ImportPath': 'fixture/app', 'Dir': str(self.root / 'app'), 'Module': {'Main': True},
                    'GoFiles': ['app.go'], 'Imports': ['external.example/wrapper']},
                   {'ImportPath': 'external.example/wrapper', 'Module': {'Version': 'v1.0.0'},
                    'Imports': ['external.example/patched']},
                   {'ImportPath': 'external.example/patched', 'Dir': str(self.root / 'leaf'),
                    'Module': {'Replace': {'Dir': str(self.root / 'leaf')}}, 'GoFiles': ['race.go']}]
        inventory = '\n'.join(json.dumps(record) for record in records)
        original = go_package_inputs(self.root, ['fixture/app'], inventory)
        self.write('leaf/race.go', '//go:build race\n\npackage leaf\nconst Race = false\n')
        self.assertNotEqual(original, go_package_inputs(self.root, ['fixture/app'], inventory))


if __name__ == '__main__':
    unittest.main()
