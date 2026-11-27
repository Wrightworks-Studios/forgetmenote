import {spawn, execFileSync} from 'node:child_process';
import {randomInt} from 'node:crypto';
import {mkdtemp, readFile, readdir, writeFile, rm, cp, open} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import net from 'node:net';
import {setTimeout as delay} from 'node:timers/promises';
import {startPostgres, sql, databaseURL} from './postgres.mjs';

let apps = 0;

// An app's port is unbound while the app starts, on every reset and restart
// too, and another process given it then answers in the app's place, as a Go
// test's httptest server once did. The kernel assigns ports to listen(0) and
// outgoing connections only from its ephemeral range, so take one below it.
async function appPort() {
  const ephemeral =
    process.platform === 'darwin'
      ? Number(execFileSync('sysctl', ['-n', 'net.inet.ip.portrange.first'], {encoding: 'utf8'}))
      : Number((await readFile('/proc/sys/net/ipv4/ip_local_port_range', 'utf8')).split(/\s+/)[0]);
  if (!(ephemeral > 11000)) throw Error(`The ephemeral port range starts at ${ephemeral}: no port below it is free`);
  for (let attempt = 0; attempt < 100; attempt++) {
    const port = randomInt(10000, ephemeral),
      listener = net.createServer();
    const free = await new Promise(resolve => {
      listener.once('error', () => resolve(false));
      listener.listen(port, '127.0.0.1', () => resolve(true));
    });
    if (!free) continue;
    await new Promise((resolve, reject) => listener.close(error => (error ? reject(error) : resolve())));
    return port;
  }
  throw Error(`No free port from 10000 to ${ephemeral}`);
}

// Native clipboard contents are shared across browser workers and contexts.
// The invocation owns this path, so interrupted runs cannot leave stale locks.
export async function withClipboard(action) {
  const path = process.env.FMN_BROWSER_CLIPBOARD_LOCK;
  if (!path) throw Error('Browser setup did not configure clipboard isolation');
  const deadline = Date.now() + 15000;
  let lock;
  while (!lock) {
    try {
      lock = await open(path, 'wx');
    } catch (error) {
      if (error.code !== 'EEXIST' || Date.now() >= deadline) throw error;
      await delay(25);
    }
  }
  try {
    return await action();
  } finally {
    await lock.close();
    await rm(path);
  }
}

// A real Go app with a private workspace and database; provider HTTP fixtures
// stay in the specs. Outside Playwright the app starts a cluster of its own.
export async function startApp({name, account, chatConfig, env = {}}) {
  const dir = await mkdtemp(join(tmpdir(), name));
  const database = `app_${process.pid}_${++apps}`;
  let cluster,
    host = process.env.FMN_BROWSER_DATABASE_HOST;
  const appEnv = {
    ...process.env,
    INSTANCE_CONFIG_FILE: '',
    CHAT_CONFIG_FILE: '',
    DECISION_CONFIG_FILE: '',
    DATA_CONNECTIONS_CONFIG_FILE: '',
    PERSONAL_CONNECTIONS_CONFIG_FILE: '',
    AUTOMATIONS_CONFIG_FILE: '',
    AUTOMATION_PYTHON: '',
    AUTOMATION_PYTHON_WHEELHOUSE: '',
    MCP_ACCESS_CONFIG_FILE: '',
    SLACK_CONFIG_FILE: '',
    ...env
  };
  const binary = process.env.FMN_BROWSER_BINARY || join(dir, 'app');
  let origin,
    token,
    child,
    exited,
    outcome,
    output = '',
    closing,
    baseline = false;

  // On SIGTERM the app ends at once the requests that wait for changes,
  // such as an open page's live poll or chat events, lets every other
  // request run for up to 15 s, and waits for any handler left before it
  // closes its stores (internal/app/http.go). The harness waits for all of
  // that rather than killing an app that is still stopping: a kill lands
  // mid-shutdown, and a process killed inside a disk write exits only once
  // the write completes, which on a loaded host can take seconds. An app
  // that outlives its own bounds is killed and fails the stop, so a hang
  // shows.
  const drainBound = 20000,
    reapBound = 15000;
  async function stop() {
    if (!child || outcome) return;
    child.kill('SIGTERM');
    if (await exitWithin(drainBound)) return;
    const threads = await threadStates(child.pid);
    child.kill('SIGKILL');
    const killed = await exitWithin(reapBound);
    throw Error(
      `Synthetic app did not stop within ${drainBound / 1000} s of SIGTERM` +
        (killed ? ' and was killed' : ` nor within ${reapBound / 1000} s of SIGKILL`) +
        `: ${origin}\nIts threads (state:kernel wait): ${threads}` +
        (output ? '\n' + output : '')
    );
  }
  function exitWithin(ms) {
    return Promise.race([exited.then(() => true), delay(ms, false, {ref: false})]);
  }
  function close() {
    return (closing ||= (async () => {
      try {
        await stop();
      } finally {
        try {
          if (cluster) await cluster.stop();
          else if (host) {
            sql(host, `DROP DATABASE IF EXISTS ${database} WITH (FORCE)`);
            sql(host, `DROP DATABASE IF EXISTS ${database}_baseline`);
          }
        } finally {
          await rm(dir, {recursive: true, force: true});
        }
      }
    })());
  }
  async function launch() {
    output = '';
    outcome = null;
    child = spawn(
      binary,
      ['-data', join(dir, 'data'), '-listen', '127.0.0.1:' + new URL(origin).port, '-url', origin],
      {env: appEnv, stdio: ['ignore', 'pipe', 'pipe']}
    );
    exited = new Promise(resolve => {
      const finish = result => {
        outcome = result;
        resolve(result);
      };
      child.once('error', error => finish({error}));
      child.once('exit', (code, signal) => finish({code, signal}));
    });
    for (const stream of [child.stdout, child.stderr]) {
      stream.on('data', data => {
        output = (output + data.toString()).slice(-8192);
      });
    }
    const started = Date.now(),
      deadline = started + 15000;
    function startupError(message) {
      const error = new Error(message + (output ? '\n' + output : ''));
      error.readinessMs = Date.now() - started;
      return error;
    }
    while (true) {
      if (outcome) {
        const reason = outcome.error?.message || (outcome.signal ? 'signal ' + outcome.signal : 'code ' + outcome.code);
        throw startupError('Synthetic app exited before readiness: ' + reason);
      }
      if (Date.now() >= deadline) throw startupError('Synthetic app readiness timed out after 15000 ms');
      // Ready is this app answering, not just something on its port: only
      // this app accepts the recovery token it wrote to its data directory.
      try {
        token = (await readFile(join(dir, 'data', 'admin.token'), 'utf8')).trim();
        const response = await fetch(origin + '/api/me', {
          headers: {Authorization: 'Bearer ' + token},
          signal: AbortSignal.timeout(Math.min(1000, deadline - Date.now()))
        });
        if (response.ok && (await response.json()).id === '@local-admin') break;
      } catch {}
      await Promise.race([delay(50), exited]);
    }
  }
  async function api(path, body) {
    const response = await fetch(origin + path, {
      method: body ? 'POST' : 'GET',
      headers: {Authorization: 'Bearer ' + token, 'Content-Type': 'application/json'},
      body: body ? JSON.stringify(body) : undefined
    });
    if (!response.ok)
      throw Error(
        `${body ? 'POST' : 'GET'} ${path}: HTTP ${response.status}\n${(await response.text()).slice(0, 8192)}`
      );
    return response.json();
  }
  async function login(page, mode) {
    if (!['recovery', 'password'].includes(mode)) throw Error('Choose recovery or password login');
    const response = await page.request.post(origin + (mode === 'recovery' ? '/login' : '/auth/password/login'), {
      form: mode === 'recovery' ? {token} : account,
      headers: {Origin: origin}
    });
    if (!response.ok())
      throw Error(`Synthetic ${mode} login: HTTP ${response.status()}\n${(await response.text()).slice(0, 8192)}`);
  }
  // `env` changes the app's environment from this start on.
  async function restart({env} = {}) {
    if (closing) throw Error('Synthetic app is closed');
    await stop();
    Object.assign(appEnv, env);
    await launch();
  }
  async function saveBaseline() {
    if (closing || baseline) throw Error('Synthetic baseline is already saved or the app is closed');
    // Copy only a stopped disposable instance, including its real Git history and
    // synthetic login state. No running store observes a replacement.
    await stop();
    await cp(join(dir, 'data'), join(dir, 'baseline'), {recursive: true, errorOnExist: true, force: false});
    sql(host, `CREATE DATABASE ${database}_baseline TEMPLATE ${database}`);
    baseline = true;
    await launch();
  }
  async function reset() {
    if (closing || !baseline) throw Error('Save a synthetic baseline before resetting the app');
    await stop();
    await rm(join(dir, 'data'), {recursive: true, force: true});
    await cp(join(dir, 'baseline'), join(dir, 'data'), {recursive: true, errorOnExist: true, force: false});
    sql(host, `DROP DATABASE ${database} WITH (FORCE)`);
    sql(host, `CREATE DATABASE ${database} TEMPLATE ${database}_baseline`);
    await launch();
  }
  try {
    if (chatConfig) {
      appEnv.CHAT_CONFIG_FILE = join(dir, 'chat.json');
      await writeFile(appEnv.CHAT_CONFIG_FILE, JSON.stringify(chatConfig));
    }
    if (!process.env.FMN_BROWSER_BINARY)
      execFileSync('go', ['build', '-trimpath', '-o', binary, './cmd/forgetmenote'], {timeout: 60000});
    if (!host) host = (cluster = await startPostgres()).host;
    sql(host, `CREATE DATABASE ${database}`);
    appEnv.DATABASE_URL_FILE = join(dir, 'database.url');
    await writeFile(appEnv.DATABASE_URL_FILE, databaseURL(host, database) + '\n', {mode: 0o600});
    execFileSync(binary, ['migrate'], {
      env: {PATH: process.env.PATH, DATABASE_URL_FILE: appEnv.DATABASE_URL_FILE},
      stdio: ['ignore', 'ignore', 'pipe'],
      timeout: 30000
    });
    origin = 'http://localhost:' + (await appPort());
    await launch();
    await api('/account/setup', account);
    return {
      dir,
      origin,
      api,
      login,
      restart,
      saveBaseline,
      reset,
      close,
      get output() {
        return output;
      },
      get child() {
        return child;
      },
      get token() {
        return token;
      }
    };
  } catch (error) {
    try {
      await close();
    } catch (cleanup) {
      throw new AggregateError([error, cleanup], 'Synthetic app startup and cleanup failed');
    }
    throw error;
  }
}

// threadStates counts the threads of process pid by state and kernel wait,
// such as "D:cv_wait_common" for one in uninterruptible disk I/O, on Linux.
async function threadStates(pid) {
  try {
    const counts = {};
    for (const task of await readdir(`/proc/${pid}/task`)) {
      const stat = await readFile(`/proc/${pid}/task/${task}/stat`, 'utf8');
      const wchan = await readFile(`/proc/${pid}/task/${task}/wchan`, 'utf8').catch(() => '?');
      const key = stat.slice(stat.lastIndexOf(')') + 2).split(' ')[0] + ':' + (wchan || '-');
      counts[key] = (counts[key] || 0) + 1;
    }
    return Object.entries(counts)
      .map(([key, n]) => key + ' ×' + n)
      .join(', ');
  } catch (error) {
    return 'unavailable (' + error.message + ')';
  }
}

// Existing tests explicitly choose acceptance/refusal. Drive the real HTML
// confirmation through Playwright without injecting a production test hook.
const confirmations = new WeakMap();
export async function watchConfirmations(page, listener, once = false) {
  let state = confirmations.get(page);
  if (!state) {
    state = {};
    confirmations.set(page, state);
    await page.exposeBinding('__fmnBrowserConfirmation', async (_, message) => {
      const callback = state.listener;
      if (!callback) return;
      if (state.once) state.listener = null;
      const modal = page.getByRole('dialog', {name: 'Confirm action', exact: true});
      await callback({
        message: () => message,
        accept: () => modal.getByRole('button', {name: 'Continue', exact: true}).click(),
        dismiss: () => modal.getByRole('button', {name: 'Cancel', exact: true}).click()
      });
    });
    const observe = () => {
      if (window.__fmnConfirmationObserver) return;
      let current = null;
      const check = () => {
        const modal = document.querySelector('dialog[data-confirm-action][open]');
        if (modal && modal !== current) {
          current = modal;
          window.__fmnBrowserConfirmation(modal.querySelector('p').textContent);
        }
        if (!modal) current = null;
      };
      window.__fmnConfirmationObserver = new MutationObserver(check);
      window.__fmnConfirmationObserver.observe(document, {
        subtree: true,
        childList: true,
        attributes: true,
        attributeFilter: ['open']
      });
      check();
    };
    await page.addInitScript(observe);
    await page.evaluate(observe);
  }
  state.listener = listener;
  state.once = once;
  page.on('dialog', listener);
}
export function removeConfirmations(page) {
  const state = confirmations.get(page);
  if (state) state.listener = null;
  page.removeAllListeners('dialog');
}
