import {spawn, execFileSync} from 'node:child_process';
import {mkdtemp, rm} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {setTimeout as delay} from 'node:timers/promises';

// A disposable Postgres cluster from the host's initdb and postgres, listening
// only on a Unix socket in its own directory. Apps get databases of their own.
export async function startPostgres() {
  // A Unix socket path must stay under about 100 bytes.
  const dir = await mkdtemp(join(tmpdir().length > 60 ? '/tmp' : tmpdir(), 'fmnpg'));
  let server, exited, watchdog, watchdogExited;
  try {
    execFileSync(
      'initdb',
      ['-D', join(dir, 'data'), '-U', 'fmn', '-A', 'trust', '-E', 'UTF8', '--no-locale', '--no-sync'],
      {stdio: 'ignore', timeout: 60000}
    );
    server = spawn(
      'postgres',
      [
        '-D',
        join(dir, 'data'),
        '-k',
        dir,
        '-c',
        'listen_addresses=',
        '-c',
        'max_connections=300',
        '-c',
        'fsync=off',
        '-c',
        'synchronous_commit=off',
        '-c',
        'full_page_writes=off'
      ],
      {stdio: 'ignore'}
    );
    exited = new Promise(resolve => server.once('exit', resolve));
    // If this process is killed, its end of the watchdog's stdin closes and the
    // watchdog stops the cluster and removes its files anyway. Until then it
    // waits in a shell builtin, so stop can end it at once: it inherits this
    // process's descriptors, such as a release lock, and must not outlive stop.
    watchdog = spawn(
      '/bin/sh',
      ['-c', 'read -r line; kill -INT "$1" 2>/dev/null; sleep 5; rm -rf "$2"', 'fmnpg', String(server.pid), dir],
      {detached: true, stdio: ['pipe', 'ignore', 'ignore']}
    );
    watchdogExited = new Promise(resolve => watchdog.once('exit', resolve));
    watchdog.unref();
    const deadline = Date.now() + 30000;
    while (true) {
      try {
        sql(dir, 'SELECT 1');
        break;
      } catch (error) {
        if (Date.now() >= deadline) throw Error('Postgres did not accept connections: ' + error.message);
        await delay(50);
      }
    }
  } catch (error) {
    server?.kill('SIGKILL');
    watchdog?.kill('SIGKILL');
    await rm(dir, {recursive: true, force: true});
    throw error;
  }
  return {
    host: dir,
    async stop() {
      server.kill('SIGINT');
      if (!(await Promise.race([exited.then(() => true), delay(10000, false, {ref: false})]))) server.kill('SIGKILL');
      await exited;
      await rm(dir, {recursive: true, force: true});
      // Nothing is left for the watchdog to clean up. Referenced again, its
      // exit keeps this process alive until the watchdog is gone.
      watchdog.ref();
      watchdog.kill('SIGKILL');
      await watchdogExited;
    }
  };
}

// Runs one statement as the cluster's superuser.
export function sql(host, statement) {
  execFileSync(
    'psql',
    ['-X', '-q', '-v', 'ON_ERROR_STOP=1', '-h', host, '-U', 'fmn', '-d', 'postgres', '-c', statement],
    {
      stdio: ['ignore', 'ignore', 'pipe'],
      timeout: 30000
    }
  );
}

export function databaseURL(host, name) {
  return `postgres://fmn@/${name}?host=${encodeURIComponent(host)}&sslmode=disable`;
}
