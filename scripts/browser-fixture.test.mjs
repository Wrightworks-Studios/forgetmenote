import {test} from 'node:test';
import assert from 'node:assert/strict';
import {access, mkdtemp, readFile, readdir, rm} from 'node:fs/promises';
import {createServer} from 'node:http';
import {constants, tmpdir} from 'node:os';
import {execFileSync} from 'node:child_process';
import {join} from 'node:path';
import {randomUUID} from 'node:crypto';
import {once} from 'node:events';
import {startApp} from './browser/app-fixture.mjs';
import {buildNoNetlink, browserEnvironment} from './browser/no-netlink.mjs';
import {startPostgres} from './browser/postgres.mjs';

const account = {username: 'fixture-admin', password: 'DisposableFixturePassword123!'};

test('a child exiting before readiness reports its exit promptly and removes temporary data', {timeout: 30000}, async () => {
  const name = 'forgetmenote-fixture-failure-' + randomUUID() + '-';
  await assert.rejects(startApp({name, account, env: {CHAT_CONFIG_FILE: join(tmpdir(), name + 'missing.json')}}), error => {
    assert.match(error.message, /exited before readiness.*code 1/s);
    assert.ok(error.readinessMs < 5000, 'startup failure waited for the readiness deadline');
    return true;
  });
  assert.deepEqual((await readdir(tmpdir())).filter(entry => entry.startsWith(name)), []);
});

test('cleanup finishes when the real app already exited and remains safe to repeat', {timeout: 30000}, async () => {
  const app = await startApp({name: 'forgetmenote-fixture-exit-', account});
  try {
    const exited = once(app.child, 'exit');
    app.child.kill('SIGTERM');
    await exited;
    await app.close();
    await app.close();
    await assert.rejects(access(app.dir), {code: 'ENOENT'});
  } finally {await app.close();}
});

test('a stopped baseline isolates cases while ordinary restart preserves their writes', {timeout: 30000}, async () => {
  const app = await startApp({name: 'forgetmenote-fixture-baseline-', account});
  try {
    const baseline = await app.api('/p/default/api/note', {path: 'Baseline.md', content: '# Baseline\n', revision: ''});
    await app.saveBaseline();
    const changed = await app.api('/p/default/api/note', {path: 'Baseline.md', content: '# Changed\n', revision: baseline.revision});
    const latest = await app.api('/p/default/api/note', {path: 'Only-this-case.md', content: 'Temporary case\n', revision: ''});
    await app.restart();
    const restarted = await app.api('/p/default/api/note?path=Baseline.md');
    assert.equal(restarted.content, changed.content);
    assert.equal(restarted.revision, latest.revision);
    await app.reset();
    const restored = await app.api('/p/default/api/note?path=Baseline.md');
    assert.equal(restored.content, baseline.content);
    assert.equal(restored.revision, baseline.revision);
    await assert.rejects(app.api('/p/default/api/note?path=Only-this-case.md'), /404/);
    await app.reset();
    assert.equal((await app.api('/p/default/api/note?path=Baseline.md')).revision, baseline.revision);
  } finally {await app.close();}
});

// The app's port is unbound while it starts, including across every reset and
// restart. A Go test's httptest server once took it then and answered the
// readiness probe and the account setup, so a suite's baseline had no
// administrator and every page opened the first-run setup dialog.
test('the app port is one the kernel never hands to another process', {timeout: 30000}, async () => {
  const ephemeral = process.platform === 'darwin'
    ? Number(execFileSync('sysctl', ['-n', 'net.inet.ip.portrange.first'], {encoding: 'utf8'}))
    : Number((await readFile('/proc/sys/net/ipv4/ip_local_port_range', 'utf8')).split(/\s+/)[0]);
  const app = await startApp({name: 'forgetmenote-fixture-port-', account});
  try {
    assert.ok(Number(new URL(app.origin).port) < ephemeral, `${app.origin} is in the ephemeral range from ${ephemeral}`);
  } finally {await app.close();}
});

test('a server holding the app port is never taken for the app', {timeout: 30000}, async () => {
  const app = await startApp({name: 'forgetmenote-fixture-impostor-', account});
  const impostor = createServer((request, response) => {
    request.resume();
    response.setHeader('Content-Type', 'application/json');
    response.end('{"id":"someone-else"}');
  });
  try {
    const exited = once(app.child, 'exit');
    app.child.kill('SIGTERM');
    await exited;
    await new Promise(resolve => impostor.listen(Number(new URL(app.origin).port), '127.0.0.1', resolve));
    await assert.rejects(app.restart(), /exited before readiness.*address already in use/s);
  } finally {
    impostor.close();
    await app.close();
  }
});

test('a failed fixture request reports its method path HTTP status and response body', {timeout: 30000}, async () => {
  const app = await startApp({name: 'forgetmenote-fixture-diagnostic-', account});
  try {
    await assert.rejects(app.api('/p/default/api/note?path=Absent.md'), error => {
      assert.match(error.message, /GET \/p\/default\/api\/note\?path=Absent.md: HTTP 404/);
      assert.match(error.message, /not exist|not found/i);
      assert.ok(!error.message.includes(app.token));
      return true;
    });
  } finally {await app.close();}
});

// Chromium learns of host interface changes only through this socket family.
test('the Linux browser library refuses route netlink sockets and keeps others', {skip: process.platform !== 'linux', timeout: 60000}, async () => {
  const dir = await mkdtemp(join(tmpdir(), 'forgetmenote-no-netlink-'));
  try {
    const probe = ['import socket', 'for family, kind in ((socket.AF_NETLINK, socket.SOCK_RAW), (socket.AF_INET, socket.SOCK_STREAM)):',
      '    try: socket.socket(family, kind, 0).close(); print("open")', '    except OSError as error: print(error.errno)'].join('\n');
    assert.deepEqual(execFileSync('python3', ['-c', probe], {encoding: 'utf8'}).trim().split('\n'), ['open', 'open']);
    const env = browserEnvironment(buildNoNetlink(dir));
    assert.deepEqual(execFileSync('python3', ['-c', probe], {env, encoding: 'utf8'}).trim().split('\n'), [String(constants.errno.EAFNOSUPPORT), 'open']);
  } finally {await rm(dir, {recursive: true, force: true});}
});

// A release runs these tests holding its lock on an inherited descriptor, so a
// process a stopped cluster leaves behind keeps the release locked. Playwright's
// teardown stops the cluster with nothing else keeping its process alive.
test('a stopped Postgres cluster leaves no process behind', {timeout: 30000}, async () => {
  const script = `const {startPostgres} = await import(${JSON.stringify(new URL('./browser/postgres.mjs', import.meta.url).href)});
const cluster = await startPostgres();
await cluster.stop();
console.log(cluster.host);`;
  const host = execFileSync(process.execPath, ['--input-type=module', '-e', script], {encoding: 'utf8'}).trim();
  assert.notEqual(host, '', 'the process exited before stop finished');
  const left = execFileSync('ps', ['-A', '-o', 'args='], {encoding: 'utf8'}).split('\n').filter(line => line.includes(host));
  assert.deepEqual(left, []);
});
