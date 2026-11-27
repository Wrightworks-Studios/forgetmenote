// The browser's storage budgets (docs/architecture.md) on the real app and the
// whole F1 manuscript: opening it, keystroke to paint in the middle of the
// book in Visual (the live preview over the whole note) and Source,
// unthrottled and at 4× CPU throttle (and in Visual on a 53 KB note, for
// comparison), how long another client takes to show an edit or an agent's
// patch, long tasks while remote updates arrive during typing, and typing
// with the note context panel open. CodeMirror draws only the lines near the
// screen, so the other client scrolls to the edited paragraph first, as its
// reader would. It is a measurement, not a regression test (only the context
// panel case asserts its budget), so it runs only when
// FMN_BROWSER_BUDGETS names the JSON file the numbers go to. F1 is read from
// FMN_FIXTURE_CACHE, as internal/benchfixture reads it.
import {test, expect} from '@playwright/test';
import {createHash} from 'node:crypto';
import {readFileSync, writeFileSync} from 'node:fs';
import {homedir} from 'node:os';
import {join} from 'node:path';
import {setTimeout as delay} from 'node:timers/promises';
import {startApp} from './app-fixture.mjs';
import {documentEnd} from './editor-text.mjs';

const output = process.env.FMN_BROWSER_BUDGETS;
test.skip(!output, 'Set FMN_BROWSER_BUDGETS to a results file to measure the browser budgets');
test.describe.configure({mode: 'serial', timeout: 20 * 60000});
// Playwright's trace, which the suite keeps on failure, snapshots the whole
// page in the page's own thread around every key press: with F1 open it took
// half of a typing profile's time at 4x, and lengthened what it measured.
test.use({trace: 'off'});

const path = 'Manuscripts/Moby-Dick.md';
// Chapters 1–3 of F1 (53 KB), as editor-manuscript.spec.mjs uses them: the
// same editor on a note a twentieth the size.
const slicePath = 'Manuscripts/Slice.md';
const TEXT = 'the whale rose slowly out of the grey sea and ishmael watched it from the deck ';
const results = {started: new Date().toISOString()};
let app, novel;

function readNovel() {
  const dir = process.env.FMN_FIXTURE_CACHE || join(homedir(), '.cache', 'forgetmenote-fixtures');
  const text = readFileSync(join(dir, 'f1-moby-dick.md'));
  const sha = createHash('sha256').update(text).digest('hex');
  if (sha !== '15bf4dd69bccc3785d705aee3f26e80726a0a4f41baf0f26a9c7053d4d1077dd') throw Error('F1 has SHA-256 ' + sha);
  return text.toString('utf8');
}

const percentile = (values, p) => {
  if (!values.length) return null;
  const sorted = [...values].sort((a, b) => a - b);
  return Math.round(sorted[Math.min(sorted.length - 1, Math.ceil((p / 100) * sorted.length) - 1)] * 10) / 10;
};
const summary = values => ({
  n: values.length,
  p50: percentile(values, 50),
  p95: percentile(values, 95),
  max: values.length ? Math.round(Math.max(...values) * 10) / 10 : null
});

// Keystroke to paint: keydown to the first task after the next animation
// frame. Work: keydown until the last input listener ran. Long tasks over
// 50 ms, as the browser reports them.
function instrument() {
  const keys = [],
    works = [],
    longtasks = [],
    waiters = [];
  let pressed = null;
  addEventListener('input', () => {
    const start = pressed;
    pressed = null;
    if (start != null) queueMicrotask(() => works.push(performance.now() - start));
  });
  addEventListener(
    'keydown',
    event => {
      const start = event.timeStamp;
      pressed = start;
      requestAnimationFrame(() => {
        const channel = new MessageChannel();
        channel.port1.onmessage = () => {
          keys.push(performance.now() - start);
          while (waiters.length && waiters[0].count <= keys.length) waiters.shift().resolve();
        };
        channel.port2.postMessage(0);
      });
    },
    true
  );
  new PerformanceObserver(list => {
    for (const entry of list.getEntries()) longtasks.push({start: entry.startTime, duration: entry.duration});
  }).observe({type: 'longtask', buffered: true});
  window.budget = {
    keys,
    works,
    longtasks,
    keysAtLeast: count =>
      keys.length >= count ? Promise.resolve() : new Promise(resolve => waiters.push({count, resolve}))
  };
}

async function newPage(browser, throttle = 1) {
  const context = await browser.newContext({viewport: {width: 1280, height: 900}});
  const page = await context.newPage();
  page.on('pageerror', error => {
    throw error;
  });
  await page.addInitScript(instrument);
  // What each live request answers, in bytes.
  page.liveBytes = [];
  page.on('response', async response => {
    if (!new URL(response.url()).pathname.endsWith('/api/live')) return;
    try {
      page.liveBytes.push((await response.body()).length);
    } catch {
      // A page closed before the body arrived.
    }
  });
  if (throttle > 1) {
    const cdp = await context.newCDPSession(page);
    await cdp.send('Emulation.setCPUThrottlingRate', {rate: throttle});
  }
  return page;
}

// Opens the manuscript, or the note at note, for editing in mode, and returns
// the milliseconds from navigation until it is editable.
async function open(page, mode, note = path) {
  await app.login(page, 'recovery');
  const started = Date.now();
  await page.goto(app.origin + '/?project=default&note=' + encodeURIComponent(note));
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected', {timeout: 120000});
  await page.locator('#mode').click();
  await page.locator('#format-' + mode).click();
  await expect(page.locator('#editor')).toHaveAttribute(
    'aria-label',
    mode === 'visual' ? 'Note content' : 'Markdown content',
    {
      timeout: 120000
    }
  );
  return Date.now() - started;
}

// Puts the caret at the end of the line in the middle of the note, in any
// mode, through the editor's view (CodeMirror keeps it on its content
// element), and returns that line's start.
async function caretInMiddle(page) {
  const from = await page.evaluate(() => {
    const view = document.querySelector('#editor').cmTile.root.view;
    const line = view.state.doc.lineAt(Math.floor(view.state.doc.length / 2));
    view.dispatch({selection: {anchor: line.to}, scrollIntoView: true});
    view.focus();
    return line.from;
  });
  await delay(300);
  return from;
}
// Scrolls the line holding pos into view without moving the caret there.
async function showLine(page, pos) {
  await page.evaluate(pos => {
    const view = document.querySelector('#editor').cmTile.root.view;
    view.dispatch({effects: view.constructor.scrollIntoView(pos, {y: 'center'})});
  }, pos);
  await delay(300);
}
// The start of the line the caret is on.
const caretLine = page =>
  page.evaluate(() => {
    const view = document.querySelector('#editor').cmTile.root.view;
    return view.state.doc.lineAt(view.state.selection.main.head).from;
  });

// Types count keys at the caret, one at a time (every 20th a Backspace): each
// waits for its frame, then at least 30 ms, so a slow editor is measured
// without a growing backlog.
async function type(page, count) {
  const [k0, w0, t0] = await page.evaluate(() => [budget.keys.length, budget.works.length, performance.now()]);
  for (let typed = 0; typed < count; typed++) {
    const at = Date.now(),
      key = typed % 20 === 19 ? 'Backspace' : TEXT[typed % TEXT.length] === ' ' ? 'Space' : TEXT[typed % TEXT.length];
    await page.keyboard.press(key);
    await page.evaluate(n => budget.keysAtLeast(n), k0 + typed + 1);
    const spent = Date.now() - at;
    if (spent < 30) await delay(30 - spent);
  }
  const raw = await page.evaluate(
    ([k0, w0, t0]) => ({
      keys: budget.keys.slice(k0),
      works: budget.works.slice(w0),
      longtasks: budget.longtasks.filter(task => task.start >= t0).map(task => task.duration)
    }),
    [k0, w0, t0]
  );
  return {
    paint: summary(raw.keys),
    work: summary(raw.works),
    longtasks: {count: raw.longtasks.length, max: raw.longtasks.length ? Math.round(Math.max(...raw.longtasks)) : 0}
  };
}

const live = () => app.api('/p/default/api/live?path=' + encodeURIComponent(path));

// Resolves with Date.now() when the editor shows marker.
function seen(page, marker) {
  return page.evaluate(
    marker =>
      new Promise(resolve => {
        const root = document.querySelector('#editor');
        const check = () => {
          if (!root.textContent.includes(marker)) return false;
          observer.disconnect();
          resolve(Date.now());
          return true;
        };
        const observer = new MutationObserver(check);
        if (!check()) observer.observe(root, {subtree: true, characterData: true, childList: true});
      }),
    marker
  );
}

// Waits up to 15 s for shown, then says where the marker got to.
async function within(shown, marker, writer, reader) {
  const late = Symbol('late');
  const value = await Promise.race([shown, delay(15000, late)]);
  if (value !== late) return value;
  const has = page => page.evaluate(m => document.getElementById('editing').markdown?.includes(m), marker);
  throw Error(
    `${marker.trim()} not shown after 15 s: writer text ${await has(writer)}, server ${(await live()).content.includes(marker)}, reader text ${await has(reader)}`
  );
}

test.beforeAll(async () => {
  novel = readNovel();
  app = await startApp({
    name: 'forgetmenote-budgets-',
    account: {username: 'budget-admin', password: 'DisposableBudget123!'}
  });
  await app.api('/p/default/api/note', {path, content: novel, revision: ''});
  await app.api('/p/default/api/note', {
    path: slicePath,
    content: readFileSync(new URL('./fixtures/f1-slice.md', import.meta.url), 'utf8'),
    revision: ''
  });
});
test.afterAll(async () => {
  await app?.close();
  if (output) writeFileSync(output, JSON.stringify(results, null, 2) + '\n');
});

test('a bare contenteditable sets the frame floor', async ({browser}) => {
  results.floor = {};
  for (const throttle of [1, 4]) {
    const page = await newPage(browser, throttle);
    await page.setContent('<div contenteditable style="font: 16px serif; padding: 2em">Call me Ishmael.</div>');
    // Set content is no navigation, so the init script did not run.
    await page.evaluate(instrument);
    await page.locator('div').click();
    await page.keyboard.press('End');
    results.floor[throttle + 'x'] = await type(page, 500);
    await page.context().close();
  }
});

for (const [name, mode, note] of [
  ['visual', 'visual', path],
  ['source', 'source', path],
  ['visualSlice', 'visual', slicePath]
])
  test(`keystroke to paint in ${name}`, async ({browser}) => {
    results[name] = {};
    for (const throttle of [1, 4]) {
      const page = await newPage(browser, throttle);
      const opened = await open(page, mode, note);
      await caretInMiddle(page);
      const typed = await type(page, 500);
      results[name][throttle + 'x'] = {opened, ...typed, liveBytes: summary(page.liveBytes)};
      await page.context().close();
    }
  });

test('another client shows an edit and an agent patch', async ({browser}) => {
  const writer = await newPage(browser),
    reader = await newPage(browser);
  await open(writer, 'visual');
  await open(reader, 'visual');
  // The reader looks at the paragraph the writer edits.
  const middle = await caretInMiddle(writer);
  await showLine(reader, middle);
  // Isolated edits: one inserted word, then a pause.
  const edits = [];
  for (let i = 0; i < 30; i++) {
    const marker = ` zqedit${i}x`,
      shown = seen(reader, marker);
    const sent = Date.now();
    await writer.keyboard.insertText(marker);
    edits.push((await within(shown, marker, writer, reader)) - sent);
    await delay(1000);
  }
  // An agent's patch to the paragraph on screen.
  await delay(1000);
  const patches = [];
  for (let i = 0; i < 30; i++) {
    const current = await live(),
      start = await caretLine(writer);
    const marker = `zqagent${i}x`,
      shown = seen(reader, marker);
    const sent = Date.now();
    await app.api('/p/default/api/live/patch', {
      path,
      base: current.liveRevision,
      operationId: `budget-agent-${Date.now()}-${i}`,
      edits: [{start, end: start, old: '', text: marker + ' '}]
    });
    patches.push((await within(shown, marker, writer, reader)) - sent);
    await delay(1000);
  }
  results.remote = {edit: summary(edits), agent: summary(patches), readerLiveBytes: summary(reader.liveBytes)};
  await writer.context().close();
  await reader.context().close();
});

// Typing at the end of the book with the note context panel open (plan risk
// 18): the panel reads the editor's text, so no typing pause sends the whole
// note to /api/preview or renders it, and none adds a long task.
test('typing with the context panel open', async ({browser}) => {
  const page = await newPage(browser);
  const previews = [];
  page.on('request', request => {
    if (request.method() === 'POST' && new URL(request.url()).pathname.endsWith('/api/preview'))
      previews.push(request.postDataBuffer()?.length ?? 0);
  });
  await open(page, 'visual');
  await page.locator('#note-context-toggle').click();
  await expect(page.locator('#note-context-contents a')).toHaveCount(136);
  await page.locator('#editor').press(documentEnd);
  await delay(1500);
  const t0 = await page.evaluate(() => performance.now());
  for (let i = 1; i <= 5; i++) {
    await page.keyboard.type(' word' + i);
    await delay(1500);
  }
  const longtasks = await page.evaluate(
    t0 => budget.longtasks.filter(task => task.start >= t0).map(task => Math.round(task.duration)),
    t0
  );
  results.typingWithContext = {previews, longtasks};
  await page.context().close();
  expect(previews).toEqual([]);
  expect(longtasks).toEqual([]);
});

test('remote updates while typing in the middle of the note', async ({browser}) => {
  results.typingWithUpdates = {};
  for (const throttle of [1, 4]) {
    const page = await newPage(browser, throttle);
    await open(page, 'visual');
    await caretInMiddle(page);
    // Every 500 ms an agent adds a word to the paragraph being typed in.
    let patching = true,
      applied = 0;
    const agent = (async () => {
      for (let i = 0; patching; i++) {
        const current = await live(),
          at = await caretLine(page);
        try {
          await app.api('/p/default/api/live/patch', {
            path,
            base: current.liveRevision,
            operationId: `budget-typing-${throttle}-${i}`,
            edits: [{start: at, end: at, old: '', text: 'Zq '}]
          });
          applied++;
        } catch {
          // A keystroke the server accepted in between moved the base.
        }
        await delay(500);
      }
    })();
    const typed = await type(page, 200);
    patching = false;
    await agent;
    results.typingWithUpdates[throttle + 'x'] = {patches: applied, ...typed, liveBytes: summary(page.liveBytes)};
    await page.context().close();
  }
});
