// Side by side: the note's Markdown in the editor next to the note rendered
// as Read shows it, read-only, the two sides scrolling together. The last
// case measures typing and scrolling on the whole F1 manuscript with the
// pane open; like editor-budgets.spec.mjs it runs only when
// FMN_BROWSER_BUDGETS names a results file, and it writes its numbers next to
// it (…-side-by-side.json). F1 is read from FMN_FIXTURE_CACHE.
import {test, expect} from '@playwright/test';
import {createHash} from 'node:crypto';
import {readFileSync, writeFileSync} from 'node:fs';
import {homedir} from 'node:os';
import {join} from 'node:path';
import {setTimeout as delay} from 'node:timers/promises';
import {startApp} from './app-fixture.mjs';
import {caretAtLineEnd, countLiveUpdates, expectEditorText} from './editor-text.mjs';

let app;
test.beforeAll(async () => {
  app = await startApp({
    name: 'forgetmenote-side-by-side-',
    account: {username: 'side-admin', password: 'DisposableSideBySide123!'}
  });
});
test.afterAll(async () => {
  await app?.close();
});
test.use({viewport: {width: 1440, height: 900}});

async function signIn(page) {
  page.on('pageerror', error => {
    throw error;
  });
  await app.login(page, 'password');
}
async function openNote(page, path, content) {
  await app.api('/p/default/api/note', {path, content, revision: ''});
  await page.goto(app.origin + '/?project=default&note=' + encodeURIComponent(path));
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected', {timeout: 60000});
  await page.locator('#mode').click();
}
async function openSplit(page, path, content) {
  await openNote(page, path, content);
  await page.locator('#format-split').click();
  await expect(page.locator('#editing')).toHaveAttribute('data-format', 'split');
  await expect(page.locator('#editor')).toHaveAttribute('aria-label', 'Markdown content');
}
const pane = page => page.locator('#rendered-pane .rendered-preview');
// The number of the "Section N" heading at the top of the editor and of the
// pane: the first one whose top is in view, less one when the reader is
// still within the section before it.
const topSections = page =>
  page.evaluate(() => {
    const first = (elements, top) => {
      const list = [...elements].map(element => ({element, rect: element.getBoundingClientRect()}));
      const below = list.find(item => item.rect.top >= top - 1);
      const number = item => Number(item.element.textContent.match(/Section (\d+)/)[1]);
      return below ? number(below) - (below.rect.top - top > 40 ? 1 : 0) : null;
    };
    const editor = document.querySelector('#note-editor .cm-scroller'),
      rendered = document.querySelector('#rendered-pane .rendered-preview');
    return {
      editor: first(
        [...document.querySelectorAll('#editor .cm-line')].filter(line => /^## Section \d+$/.test(line.textContent)),
        editor.getBoundingClientRect().top
      ),
      pane: first(rendered.querySelectorAll('h2'), rendered.getBoundingClientRect().top)
    };
  });

test('side by side renders the note next to its Markdown and follows edits', async ({page}) => {
  await signIn(page);
  const content =
    '---\ntitle: Pane fixture\n---\n# Pane heading\n\nSome *emphasis* and a [reference][ref].\n\n| Item | Count |\n|---|---|\n| Pears | 12 |\n\nLast paragraph.\n\n[ref]: https://example.com/ref\n';
  await openNote(page, 'pane.md', content);
  const sent = countLiveUpdates(page);
  await page.locator('#format-split').click();
  await expect(pane(page)).toBeVisible();
  await expect(pane(page)).toHaveAttribute('role', 'region');
  await expect(pane(page)).toHaveAttribute('aria-label', 'Rendered note');
  await expect(pane(page).locator('h1')).toHaveText('Pane heading');
  await expect(pane(page).locator('em')).toHaveText('emphasis');
  // A reference resolves against a definition in another block.
  await expect(pane(page).locator('a[href="https://example.com/ref"]')).toHaveText('reference');
  await expect(pane(page).locator('td.numeric')).toHaveText('12');
  // Front matter is not part of the rendered note, as in Read.
  await expect(pane(page)).not.toContainText('Pane fixture');
  // Both sides fill the window side by side, each scrolling on its own.
  const [editorBox, paneBox] = [
    await page.locator('#note-editor').boundingBox(),
    await page.locator('#rendered-pane').boundingBox()
  ];
  expect(Math.abs(editorBox.width - paneBox.width)).toBeLessThan(4);
  expect(paneBox.x).toBeGreaterThan(editorBox.x + editorBox.width - 2);
  expect(paneBox.y + paneBox.height).toBeLessThanOrEqual(900);
  // Changing modes changes no text.
  for (const mode of ['visual', 'source', 'split']) {
    await page.locator('#format-' + mode).click();
    await expect(page.locator('#editing')).toHaveAttribute('data-format', mode);
  }
  await expect(pane(page).locator('h1')).toHaveText('Pane heading');
  // Showing the pane again leaves the editor's configuration as it was (a
  // pane's listener sits in one compartment, reused by the next pane).
  const compartments = () =>
    page.evaluate(() => document.querySelector('#editor').cmTile.root.view.state.config.compartments.size);
  const configured = await compartments();
  for (const mode of ['source', 'split', 'visual', 'split']) {
    await page.locator('#format-' + mode).click();
    await expect(page.locator('#editing')).toHaveAttribute('data-format', mode);
  }
  await expect(pane(page).locator('h1')).toHaveText('Pane heading');
  expect(await compartments()).toBe(configured);
  await page.waitForTimeout(500);
  expect(sent).toEqual([]);
  await expectEditorText(page, content);
  // Typing re-renders only the edited block once typing stops.
  await page.evaluate(() => {
    document.querySelector('#rendered-pane h1').kept = true;
  });
  await caretAtLineEnd(page, 'Last paragraph.');
  await page.keyboard.type(' Typed AZALEA-501.');
  await expect(pane(page).locator('p').last()).toHaveText('Last paragraph. Typed AZALEA-501.');
  expect(await page.evaluate(() => document.querySelector('#rendered-pane h1').kept)).toBe(true);
  await expectEditorText(page, content.replace('Last paragraph.', 'Last paragraph. Typed AZALEA-501.'));
  // A change from another client shows too.
  const base = await app.api('/p/default/api/live?path=pane.md'),
    start = base.content.indexOf('Pane heading');
  await app.api('/p/default/api/live/patch', {
    path: 'pane.md',
    base: base.liveRevision,
    operationId: 'pane-remote',
    edits: [{start, end: start + 4, old: 'Pane', text: 'Remote'}]
  });
  await expect(pane(page).locator('h1')).toHaveText('Remote heading');
});

test('the two sides scroll together, driven by the side last scrolled', async ({page}) => {
  await signIn(page);
  const sections = Array.from(
    {length: 80},
    (_, i) =>
      `## Section ${i + 1}\n\n` +
      `Paragraph ${i + 1} ${'with words that wrap across the column '.repeat(i % 3 === 0 ? 6 : 2)}end.\n`
  );
  await openSplit(page, 'scroll.md', '# Scrolling\n\n' + sections.join('\n'));
  await expect(pane(page).locator('h2').first()).toHaveText('Section 1');
  const editorBox = await page.locator('#note-editor').boundingBox(),
    paneBox = await page.locator('#rendered-pane').boundingBox();
  // Scrolling the editor takes the pane along.
  await page.mouse.move(editorBox.x + editorBox.width / 2, editorBox.y + 300);
  for (let i = 0; i < 6; i++) await page.mouse.wheel(0, 700);
  await expect.poll(async () => (await topSections(page)).editor).toBeGreaterThan(10);
  await expect
    .poll(async () => {
      const {editor, pane} = await topSections(page);
      return Math.abs(editor - pane);
    })
    .toBeLessThanOrEqual(1);
  // Scrolling the pane takes the editor along.
  const before = (await topSections(page)).editor;
  await page.mouse.move(paneBox.x + paneBox.width / 2, paneBox.y + 300);
  for (let i = 0; i < 8; i++) await page.mouse.wheel(0, 700);
  await expect.poll(async () => (await topSections(page)).pane).toBeGreaterThan(before + 5);
  await expect
    .poll(async () => {
      const {editor, pane} = await topSections(page);
      return Math.abs(editor - pane);
    })
    .toBeLessThanOrEqual(1);
  // Back to the top in the editor: the pane is at its top too.
  await page.mouse.move(editorBox.x + editorBox.width / 2, editorBox.y + 300);
  for (let i = 0; i < 40; i++) await page.mouse.wheel(0, -1000);
  await expect.poll(() => pane(page).evaluate(element => element.scrollTop)).toBe(0);
  // Moving the caret far away takes the pane along too.
  await page.locator('#editor').focus();
  await page.keyboard.press('Control+End');
  await expect.poll(async () => (await topSections(page)).pane).toBeGreaterThan(70);
  // Only the blocks around the pane's viewport are rendered.
  expect(await pane(page).locator('.rendered-block.rendered').count()).toBeLessThan(60);
});

test('a long note’s pane takes the whole note and then rests, with no editor input', async ({page}) => {
  await signIn(page);
  // Longer than the editor's viewport and the 100,000 characters CodeMirror
  // parses past it, so the pane parses the rest of the note itself.
  const sections = Array.from(
    {length: 1200},
    (_, i) => `## Section ${i + 1}\n\nParagraph ${i + 1} ${'with words that make the note long '.repeat(12)}end.\n`
  );
  const content =
    '# Long note\n\nA [reference][late] defined at the very end.\n\n' +
    sections.join('\n') +
    '\n## Last section\n\nThe end.\n\n[late]: https://example.com/late\n';
  expect(content.length).toBeGreaterThan(500000);
  await openNote(page, 'long-pane.md', content);
  const sent = countLiveUpdates(page);
  await page.locator('#format-split').click();
  await expect(pane(page).locator('h1')).toHaveText('Long note');
  // The definition at the end of the note resolves the reference at its start.
  await expect(pane(page).locator('a[href="https://example.com/late"]')).toHaveText('reference');
  // Then the pane does no work while the reader is idle: no rendering pass
  // and no scroll sync.
  await page.waitForTimeout(1000);
  const idle = await pane(page).evaluate(async element => {
    const {get, set} = Object.getOwnPropertyDescriptor(Element.prototype, 'scrollTop'),
      request = window.requestIdleCallback,
      counts = {scrollWrites: 0, idleCallbacks: 0};
    Object.defineProperty(element, 'scrollTop', {
      configurable: true,
      get() {
        return get.call(this);
      },
      set(value) {
        counts.scrollWrites++;
        set.call(this, value);
      }
    });
    window.requestIdleCallback = (...args) => {
      counts.idleCallbacks++;
      return request.apply(window, args);
    };
    await new Promise(resolve => setTimeout(resolve, 2000));
    delete element.scrollTop;
    window.requestIdleCallback = request;
    return counts;
  });
  expect(idle).toEqual({scrollWrites: 0, idleCallbacks: 0});
  // The note's last block is a block of its own, not part of a placeholder
  // for text the pane has not parsed.
  await expect(pane(page).locator(`.rendered-block[data-from="${content.indexOf('## Last section')}"]`)).toHaveCount(1);
  // End in the pane reaches it, and the pane stays at its end while the
  // blocks there render taller than their placeholders.
  await pane(page).focus();
  await page.keyboard.press('End');
  await expect(pane(page).locator('h2', {hasText: 'Last section'})).toBeInViewport();
  await expect(pane(page).locator('p').last()).toHaveText('The end.');
  await expect(pane(page).locator('p').last()).toBeInViewport();
  // Scrolling up from there is not held at the end.
  const box = await page.locator('#rendered-pane').boundingBox();
  await page.mouse.move(box.x + box.width / 2, box.y + 300);
  await page.mouse.wheel(0, -3000);
  await expect(pane(page).locator('h2', {hasText: 'Last section'})).not.toBeInViewport();
  await page.waitForTimeout(500);
  await expect(pane(page).locator('h2', {hasText: 'Last section'})).not.toBeInViewport();
  expect(sent).toEqual([]);
  await expectEditorText(page, content);
});

test('a pane taller than its Markdown starts at its top and scrolls on its own', async ({page}) => {
  await signIn(page);
  // Twenty headings on twenty lines fit the editor; rendered they do not.
  const content = Array.from({length: 20}, (_, i) => `# Heading ${i + 1}`).join('\n') + '\n';
  await openSplit(page, 'tall.md', content);
  await expect(pane(page).locator('h1')).toHaveCount(20);
  const sizes = await page.evaluate(() => {
    const editor = document.querySelector('#note-editor .cm-scroller'),
      rendered = document.querySelector('#rendered-pane .rendered-preview');
    return {editor: editor.scrollHeight - editor.clientHeight, pane: rendered.scrollHeight - rendered.clientHeight};
  });
  expect(sizes.editor).toBeLessThanOrEqual(2);
  expect(sizes.pane).toBeGreaterThan(200);
  await page.waitForTimeout(500);
  expect(await pane(page).evaluate(element => element.scrollTop)).toBe(0);
  await expect(pane(page).locator('h1').first()).toBeInViewport();
  const box = await page.locator('#rendered-pane').boundingBox();
  await page.mouse.move(box.x + box.width / 2, box.y + 200);
  await page.mouse.wheel(0, 2000);
  await expect(pane(page).locator('h1').last()).toBeInViewport();
});

test('side by side is offered only on wide screens', async ({page}) => {
  await page.addInitScript(() => localStorage.setItem('fmn:editor-format', 'split'));
  await page.setViewportSize({width: 800, height: 900});
  await signIn(page);
  await openNote(page, 'narrow.md', '# Narrow heading\n\nText.\n');
  await expect(page.locator('#format-split')).toBeHidden();
  await expect(page.locator('#editing')).toHaveAttribute('data-format', 'source');
  await expect(page.locator('#rendered-pane')).toBeHidden();
  await page.setViewportSize({width: 1440, height: 900});
  await expect(page.locator('#editing')).toHaveAttribute('data-format', 'split');
  await expect(pane(page).locator('h1')).toHaveText('Narrow heading');
  await page.setViewportSize({width: 800, height: 900});
  await expect(page.locator('#editing')).toHaveAttribute('data-format', 'source');
  await expect(page.locator('#rendered-pane')).toBeHidden();
  await expectEditorText(page, '# Narrow heading\n\nText.\n');
});

test('the rendered pane stays inert and requests nothing from outside', async ({page}) => {
  await signIn(page);
  const remote = [];
  await page.route('https://example.invalid/**', route => {
    remote.push(route.request().url());
    return route.abort();
  });
  const content = [
    '# Inert pane',
    '<script>window.paneExecuted = true</script>',
    '<img src="https://example.invalid/raw.png" onerror="window.paneExecuted = true">',
    'Inline <b onclick="window.paneExecuted = true">bold</b> and [script](javascript:window.paneExecuted=true) link.',
    '![Remote](https://example.invalid/remote.png) and ![Local](images/pic.png)',
    '```view\n<script>window.paneExecuted = true</script>\n```',
    '<iframe src="https://example.invalid/frame"></iframe>'
  ].join('\n\n');
  await openSplit(page, 'inert.md', content);
  await expect(pane(page).locator('h1')).toHaveText('Inert pane');
  await expect(pane(page).locator('pre code.language-view')).toHaveText(
    '<script>window.paneExecuted = true</script>\n'
  );
  await expect(pane(page).locator('script, iframe, b, [onerror], [onclick]')).toHaveCount(0);
  await expect(pane(page).locator('a[href^="javascript:"]')).toHaveCount(0);
  await expect(pane(page)).toContainText('Inline bold and script link.');
  await expect(pane(page).locator('img[alt="Remote"]')).not.toHaveAttribute('src', /./);
  await expect(pane(page).locator('img[alt="Local"]')).toHaveAttribute('src', '/p/default/assets/images/pic.png');
  await page.waitForTimeout(500);
  expect(await page.evaluate(() => !!window.paneExecuted)).toBe(false);
  expect(remote).toEqual([]);
});

test('links in the pane follow the reader’s rules', async ({page}) => {
  await signIn(page);
  const filler = Array.from({length: 60}, (_, i) => `Filler paragraph ${i + 1}.`).join('\n\n');
  const content = `# Links\n\n[External](https://example.com/page) and [Down](#far-heading) and [[Some Page|a page]] and [Unsafe](javascript:alert(1)).\n\n${filler}\n\n## Far heading\n\nThe end.\n`;
  await openSplit(page, 'links.md', content);
  const external = pane(page).locator('a', {hasText: 'External'});
  await expect(external).toHaveAttribute('href', 'https://example.com/page');
  await expect(external).toHaveAttribute('target', '_blank');
  await expect(external).toHaveAttribute('rel', 'noopener noreferrer');
  await expect(pane(page).locator('a.page-link[data-wiki="Some Page"]')).toHaveText('a page');
  await expect(pane(page).locator('a', {hasText: 'Unsafe'})).toHaveCount(0);
  await expect(pane(page)).toContainText('and Unsafe.');
  // A link within the note scrolls both sides to its heading.
  await pane(page).locator('a', {hasText: 'Down'}).click();
  await expect(page.locator('#editor .cm-line', {hasText: '## Far heading'})).toBeInViewport();
  await expect(pane(page).locator('h2', {hasText: 'Far heading'})).toBeInViewport();
  expect(new URL(page.url()).searchParams.get('note')).toBe('links.md');
});

// ---------------------------------------------------------------------------
// The opt-in F1 budget case.

const output = process.env.FMN_BROWSER_BUDGETS;
// Playwright's trace snapshots the page around every key press (see
// editor-budgets.spec.mjs), so a measuring run keeps none.
if (output) test.use({trace: 'off'});
const TEXT = 'the whale rose slowly out of the grey sea and ishmael watched it from the deck ';
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
// Keystroke to paint (keydown to the first task after the next frame), and
// long tasks over 50 ms, as editor-budgets.spec.mjs measures them.
function instrument() {
  const keys = [],
    longtasks = [],
    waiters = [];
  addEventListener(
    'keydown',
    event => {
      const start = event.timeStamp;
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
  const frames = [];
  try {
    new PerformanceObserver(list => {
      for (const entry of list.getEntries())
        frames.push({
          start: entry.startTime,
          duration: Math.round(entry.duration),
          layout: Math.round(entry.startTime + entry.duration - entry.styleAndLayoutStart),
          render: entry.renderStart ? Math.round(entry.styleAndLayoutStart - entry.renderStart) : 0,
          scripts: entry.scripts.map(
            script =>
              `${script.invoker}:${Math.round(script.duration)}:${Math.round(script.forcedStyleAndLayoutDuration)}`
          )
        });
    }).observe({type: 'long-animation-frame', buffered: true});
  } catch {}
  window.budget = {
    frames,
    keys,
    longtasks,
    keysAtLeast: count =>
      keys.length >= count ? Promise.resolve() : new Promise(resolve => waiters.push({count, resolve}))
  };
}
// Where the slow frames' time went (Long Animation Frames: each script by
// caller, with its duration and forced layout, then style, layout and render).
const slowFrames = (page, start) =>
  page.evaluate(
    start => budget.frames.filter(frame => frame.start >= start && frame.duration > 50).slice(0, 12),
    start
  );
const longtasksSince = (page, start) =>
  page.evaluate(
    start => budget.longtasks.filter(task => task.start >= start).map(task => Math.round(task.duration)),
    start
  );

test.describe('F1 with the pane open', () => {
  test.skip(!output, 'Set FMN_BROWSER_BUDGETS to a results file to measure side by side on F1');
  test.describe.configure({mode: 'serial', timeout: 20 * 60000});
  const results = {started: new Date().toISOString()},
    path = 'Manuscripts/Moby-Dick.md';
  test.beforeAll(async () => {
    if (!output) return;
    const dir = process.env.FMN_FIXTURE_CACHE || join(homedir(), '.cache', 'forgetmenote-fixtures');
    const novel = readFileSync(join(dir, 'f1-moby-dick.md'));
    expect(createHash('sha256').update(novel).digest('hex')).toBe(
      '15bf4dd69bccc3785d705aee3f26e80726a0a4f41baf0f26a9c7053d4d1077dd'
    );
    // F1 carries an fmn-id, so the notebook holds it once.
    await app.api('/p/default/api/note', {path, content: novel.toString('utf8'), revision: ''});
  });
  test.afterAll(() => {
    if (output)
      writeFileSync(output.replace(/\.json$/, '') + '-side-by-side.json', JSON.stringify(results, null, 2) + '\n');
  });

  for (const throttle of [1, 4]) {
    test(`typing and scrolling F1 at ${throttle}x keeps every task short`, async ({browser}) => {
      const context = await browser.newContext({viewport: {width: 1440, height: 900}});
      const page = await context.newPage();
      await page.addInitScript(instrument);
      if (throttle > 1)
        await (await context.newCDPSession(page)).send('Emulation.setCPUThrottlingRate', {rate: throttle});
      await signIn(page);
      const opened = Date.now();
      await page.goto(app.origin + '/?project=default&note=' + encodeURIComponent(path));
      await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected', {timeout: 120000});
      await page.locator('#mode').click();
      await page.locator('#format-split').click();
      await expect(pane(page).locator('h1, h2').first()).toBeVisible({timeout: 120000});
      const row = {throttle, openMs: Date.now() - opened};
      // The caret in the middle of the book; the pane follows it there.
      await page.evaluate(() => {
        const view = document.querySelector('#editor').cmTile.root.view;
        const line = view.state.doc.lineAt(Math.floor(view.state.doc.length / 2));
        view.dispatch({selection: {anchor: line.to}, scrollIntoView: true});
        view.focus();
      });
      await delay(3000);
      // 300 keys at the caret (every 20th a Backspace), each after the last
      // one's frame and at least 30 ms apart.
      const type = async () => {
        const start = await page.evaluate(() => performance.now()),
          k0 = await page.evaluate(() => budget.keys.length);
        for (let typed = 0; typed < 300; typed++) {
          const at = Date.now(),
            char = TEXT[typed % TEXT.length],
            key = typed % 20 === 19 ? 'Backspace' : char === ' ' ? 'Space' : char;
          await page.keyboard.press(key);
          await page.evaluate(n => budget.keysAtLeast(n), k0 + typed + 1);
          const spent = Date.now() - at;
          if (spent < 30) await delay(30 - spent);
        }
        await delay(1000);
        return {
          paint: summary(await page.evaluate(k0 => budget.keys.slice(k0), k0)),
          longtasks: await longtasksSince(page, start),
          frames: await slowFrames(page, start)
        };
      };
      row.typing = await type();
      // Scrolling the pane through the whole book, then the editor back up.
      let start = await page.evaluate(() => performance.now());
      const box = await page.locator('#rendered-pane').boundingBox();
      await page.mouse.move(box.x + box.width / 2, box.y + 300);
      const scrolled = Date.now();
      for (let step = 0; step < 400; step++) {
        await page.mouse.wheel(0, 1500);
        await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
        if (await pane(page).evaluate(element => element.scrollTop + element.clientHeight >= element.scrollHeight - 2))
          break;
      }
      row.paneScroll = {ms: Date.now() - scrolled, longtasks: await longtasksSince(page, start)};
      start = await page.evaluate(() => performance.now());
      const editor = await page.locator('#note-editor').boundingBox();
      await page.mouse.move(editor.x + editor.width / 2, editor.y + 300);
      for (let step = 0; step < 400; step++) {
        await page.mouse.wheel(0, -1500);
        await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
        if (await pane(page).evaluate(element => element.scrollTop === 0)) break;
      }
      row.editorScroll = {longtasks: await longtasksSince(page, start)};
      row.editorScroll.frames = await slowFrames(page, start);
      await delay(1000);
      row.renderedBlocks = await pane(page).locator('.rendered-block.rendered').count();
      row.blocks = await pane(page).locator('.rendered-block').count();
      // For comparison: the same scroll down and back up in Source, without
      // the pane (Source scrolls the page, not its own scroller).
      await page.locator('#format-source').click();
      await expect(page.locator('#rendered-pane')).toBeHidden();
      start = await page.evaluate(() => performance.now());
      for (const direction of [1, -1])
        for (let step = 0; step < 400; step++) {
          await page.mouse.wheel(0, 1500 * direction);
          await page.evaluate(
            () => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve)))
          );
          const end = await page.evaluate(() => {
            const area = document.scrollingElement;
            return area.scrollTop === 0 || area.scrollTop + innerHeight >= area.scrollHeight - 2;
          });
          if (end && step > 2) break;
        }
      row.sourceScroll = {longtasks: await longtasksSince(page, start)};
      // And typing in Source at the same place, for the same host load.
      await page.evaluate(() => {
        const view = document.querySelector('#editor').cmTile.root.view;
        const line = view.state.doc.lineAt(Math.floor(view.state.doc.length / 2));
        view.dispatch({selection: {anchor: line.to}, scrollIntoView: true});
        view.focus();
      });
      await delay(1000);
      row.sourceTyping = await type();
      results['x' + throttle] = row;
      console.log(JSON.stringify(row));
      await context.close();
      expect(row.typing.paint.n).toBe(300);
      expect(row.renderedBlocks).toBeLessThan(200);
      expect(row.typing.longtasks).toEqual([]);
      expect(row.paneScroll.longtasks).toEqual([]);
      expect(row.editorScroll.longtasks).toEqual([]);
    });
  }
});
