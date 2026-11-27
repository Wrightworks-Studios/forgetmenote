// Comments while editing (decision 9): highlights, Comment on selection and
// Show passage work in Visual, Source and Side by side, on the note's
// rendered text as the server and the reader read it, without ever changing
// the note's text.
import {test, expect} from '@playwright/test';
import {readFileSync} from 'node:fs';
import {startApp} from './app-fixture.mjs';
import {
  caretAtLineEnd,
  countLiveUpdates,
  documentEnd,
  editorText,
  expectEditorText,
  revealLine
} from './editor-text.mjs';

// Chapters 1–3 of F1 (see editor-manuscript.spec.mjs).
const manuscript = readFileSync(new URL('./fixtures/f1-slice.md', import.meta.url), 'utf8');
const titles = ['Chapter 1. Loomings.', 'Chapter 2. The Carpet-Bag.', 'Chapter 3. The Spouter-Inn.'];
const formats = ['visual', 'source', 'split'];

let app;
test.beforeAll(async () => {
  app = await startApp({
    name: 'forgetmenote-comments-',
    account: {username: 'comments-admin', password: 'DisposableComments123!'}
  });
});
test.afterAll(async () => {
  await app?.close();
});
test.beforeEach(async ({page}) => {
  page.on('pageerror', error => {
    throw error;
  });
  await page.setViewportSize({width: 1280, height: 900});
  await app.login(page, 'recovery');
});

const note = path => app.api('/p/default/api/note?path=' + encodeURIComponent(path));
async function addComment(path, anchor, text = 'About this passage.') {
  const {revision} = await note(path);
  return app.api('/p/default/api/comments', {path, revision, text, anchor});
}
// Creates the note unless `content` is null, then edits it in `format`.
async function open(page, path, content, format = 'visual') {
  if (content !== null) await app.api('/p/default/api/note', {path, content, revision: ''});
  await page.goto(app.origin + '/?project=default&note=' + encodeURIComponent(path));
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await page.locator('#mode').click();
  await page.locator('#format-' + format).click();
  await expect(page.locator('#editing')).toHaveAttribute('data-format', format);
}
const highlights = page => page.locator('#editor .comment-highlight');
// The highlighted text, which decorations inside a passage split into parts.
const highlighted = page =>
  highlights(page)
    .allTextContents()
    .then(texts => texts.join(''));
// Selects from the start of `from` to the end of `to` in the editor's drawn
// text, through the DOM selection CodeMirror follows.
async function selectText(page, from, to) {
  await page.locator('#editor').focus();
  await page.evaluate(() => new Promise(resolve => setTimeout(resolve, 50)));
  await page.evaluate(
    ([from, to]) => {
      const walker = document.createTreeWalker(document.getElementById('editor'), NodeFilter.SHOW_TEXT),
        nodes = [];
      let text = '';
      for (let node = walker.nextNode(); node; node = walker.nextNode()) {
        nodes.push({node, at: text.length});
        text += node.data;
      }
      const start = text.indexOf(from),
        end = text.indexOf(to, start) + to.length;
      const point = (offset, last) => {
        const item = nodes.findLast(item => (last ? item.at < offset : item.at <= offset));
        return [item.node, offset - item.at];
      };
      getSelection().setBaseAndExtent(...point(start, false), ...point(end, true));
    },
    [from, to]
  );
}
const selected = page => page.evaluate(() => getSelection().toString());
// Counts the worker's answers in window.placements (an init script), so a
// test knows the editor has placed the passages.
function countPlacements() {
  window.placements = 0;
  const Original = Worker;
  window.Worker = class extends Original {
    constructor(...args) {
      super(...args);
      this.addEventListener('message', event => {
        if (event.data?.ranges) window.placements++;
      });
    }
  };
}
// The length of each note text sent to /api/preview.
function countPreviews(page) {
  const previews = [];
  page.on('request', request => {
    if (request.method() === 'POST' && new URL(request.url()).pathname.endsWith('/api/preview'))
      previews.push(request.postDataJSON().content.length);
  });
  return previews;
}
// The caret's offset in the note and the text of its line, from the editor's view.
const caret = page =>
  page.evaluate(() => {
    const view = document.querySelector('#editor').cmTile.root.view,
      head = view.state.selection.main.head;
    return {head, line: view.state.doc.lineAt(head).text};
  });
const commentButton = page => page.locator('#formatting-tools [aria-label="Comment on selection"]');

for (const format of formats)
  test(`comment on a selection in ${format} quotes the rendered passage and highlights it`, async ({page}) => {
    const path = 'comment-on-' + format + '.md',
      content =
        '# Harbour\n\nA **bright** star\nshines over the [old](https://example.com) harbour.\n\nA bright star again.\n';
    await open(page, path, content, format);
    const sent = countLiveUpdates(page);
    await selectText(page, 'bright', 'old');
    await expect(commentButton(page)).toBeEnabled();
    await commentButton(page).click();
    await expect(page.locator('#comment-context')).toHaveText('bright star shines over the old');
    await page.locator('#comment-text').fill('Which star?');
    await page.locator('#comment-form').getByRole('button', {name: 'Post comment', exact: true}).click();
    await expect(page.locator('#comment-status')).toHaveText('Discussion saved.');
    // The server accepted the anchor: it is unique in its rendered text.
    const {comments} = await app.api('/p/default/api/comments?path=' + path);
    expect(comments.map(comment => comment.anchor)).toEqual([
      expect.objectContaining({
        quote: 'bright star shines over the old',
        prefix: 'Harbour A ',
        suffix: ' harbour. A bright star again.'
      })
    ]);
    await expect.poll(() => highlighted(page)).toMatch(/^bright(\*\*)? star ?shines over the \[?old$/);
    expect(await editorText(page)).toBe(content);
    expect(sent).toEqual([]);
  });

test('comment on a selection of markup that shows no text says why', async ({page}) => {
  const path = 'comment-on-markup.md',
    content = '# Harbour\n\nShines over the [old](https://example.com/harbour) harbour.\n';
  await open(page, path, content, 'source');
  await selectText(page, '(https', 'harbour)');
  expect(await selected(page)).toBe('(https://example.com/harbour)');
  await commentButton(page).click();
  await expect(page.locator('#notice')).toContainText('shows no text');
  await expect(page.locator('#comment-context')).toBeHidden();
  expect(await editorText(page)).toBe(content);
});

test('highlights show in every mode and a click on one opens its thread', async ({page}) => {
  const path = 'highlight-modes.md',
    content = '# Notes\n\nThe *quiet* harbour waits.\n\n| Ship | Port |\n|---|---|\n| Pequod | Nantucket |\n';
  await app.api('/p/default/api/note', {path, content, revision: ''});
  const {comments} = await addComment(path, {quote: 'quiet harbour', prefix: 'Notes The ', suffix: ' waits.'});
  await open(page, path, null);
  const sent = countLiveUpdates(page);
  for (const format of ['visual', 'source', 'split', 'visual']) {
    await page.locator('#format-' + format).click();
    await expect.poll(() => highlighted(page)).toMatch(/^quiet\*? harbour$/);
  }
  await highlights(page).last().click();
  await expect(page.locator('#comments-panel')).toBeVisible();
  await expect(page.locator('#thread-' + comments[0].id)).toBeFocused();
  expect(await editorText(page)).toBe(content);
  expect(sent).toEqual([]);
});

// A passage elsewhere in the note is found in the note's rendered text with
// its context: a quote across emphasis (the Markdown spells it "_That_
// once…"), or one whose words occur earlier in the note.
for (const [name, anchor] of [
  ['across emphasis', {quote: 'That once found out, and all the rest were plain.'}],
  [
    'whose words occur earlier',
    {quote: 'that it was a', prefix: 'I now took the measure of the bench, and found ', suffix: ' foot too short;'}
  ]
])
  for (const format of formats)
    test(`Show passage selects a comment's passage ${name} in ${format}`, async ({page}) => {
      const path = 'show-' + name.replaceAll(' ', '-') + '-' + format + '.md';
      expect(manuscript.indexOf(anchor.quote)).toBeLessThan(manuscript.indexOf('## ' + titles[2]));
      await app.api('/p/default/api/note', {path, content: manuscript, revision: ''});
      await addComment(path, anchor);
      // The worker's answers, so the test knows the passages are placed.
      await page.addInitScript(countPlacements);
      await open(page, path, null, format);
      await page.locator('#note-context-toggle').click();
      await page.locator('[data-context-tab="comments"]').click();
      const quote = page.locator('.comment-quote').filter({hasText: anchor.quote});
      await expect(quote).toBeEnabled();
      // A passage the editor has not placed yet would show in the reader.
      await expect.poll(() => page.evaluate(() => window.placements)).toBeGreaterThan(0);
      await quote.click();
      await expect.poll(async () => (await selected(page)).replace(/[_*]/g, '')).toBe(anchor.quote);
      await expect(page.locator('#editing')).toBeVisible();
      await expect(page.locator('#editing')).toHaveAttribute('data-format', format);
      // The selection is the commented passage on screen, not an earlier one.
      const line = page.locator('#editor .cm-line').filter({has: page.locator('.comment-highlight')});
      await expect(line.first()).toBeInViewport();
      await expect(line.first()).toContainText(anchor.prefix ? anchor.prefix.slice(-20) : 'That');
    });

// While editing, the context panel reads the editor's text, never the hidden
// preview (plan risk 18): typing with it open sends nothing to /api/preview,
// Contents follows the headings typed and moves the caret to one, and Show
// passage selects its passage in the editor. Leaving edit mode renders the
// preview once, and Contents reads it again.
test('the context panel follows the editor while editing and renders no preview', async ({page}) => {
  const path = 'context-while-editing.md',
    anchor = {quote: 'That once found out, and all the rest were plain.'},
    added = 'Chapter 4. The Counterpane.';
  await app.api('/p/default/api/note', {path, content: manuscript, revision: ''});
  await addComment(path, anchor);
  await page.addInitScript(countPlacements);
  const previews = countPreviews(page);
  await open(page, path, null);
  await page.locator('#note-context-toggle').click();
  const contents = page.locator('#note-context-contents');
  await expect(contents.locator('a')).toHaveText(titles);
  await page.locator('#editor').press(documentEnd);
  // Each pause is longer than the preview's delay.
  for (const word of [' one', ' two', ' three']) {
    await page.keyboard.type(word);
    await page.waitForTimeout(600);
  }
  await page.keyboard.press('Enter');
  await page.keyboard.press('Enter');
  await page.keyboard.type('## ' + added);
  const typed = manuscript + ' one two three\n\n## ' + added;
  await expectEditorText(page, typed);
  await expect(contents.locator('a')).toHaveText([...titles, added]);
  // A heading in Contents puts the caret on its line; the editor keeps focus.
  await contents.getByRole('link', {name: titles[1], exact: true}).click();
  await expect(page.locator('#editor')).toBeFocused();
  expect(await caret(page)).toEqual({head: typed.indexOf('## ' + titles[1]), line: '## ' + titles[1]});
  await expect(page.locator('#editor .cm-line').filter({hasText: titles[1]}).first()).toBeInViewport();
  expect(new URL(page.url()).hash).toBe('');
  await page.locator('[data-context-tab="comments"]').click();
  const quote = page.locator('.comment-quote').filter({hasText: anchor.quote});
  await expect(quote).toBeEnabled();
  await expect(page.locator('#comments-list')).not.toContainText('Passage changed or removed');
  await expect.poll(() => page.evaluate(() => window.placements)).toBeGreaterThan(0);
  await quote.click();
  await expect.poll(async () => (await selected(page)).replace(/[_*]/g, '')).toBe(anchor.quote);
  await expect(page.locator('#editing')).toBeVisible();
  expect(previews).toEqual([]);
  await page.locator('#mode').click();
  await expect(page.locator('#preview h2').last()).toHaveText(added);
  await page.locator('[data-context-tab="contents"]').click();
  await expect(contents.locator('a')).toHaveText([...titles, added]);
  await expect(contents.locator('a').first()).toHaveAttribute('href', /^#chapter-1/);
  expect(previews).toEqual([typed.length]);
});

// Context opens on Contents when the note has sections, counting the
// headings typed since the note was read.
test('Context opens Contents for headings typed while editing', async ({page}) => {
  const path = 'context-typed-headings.md',
    content = '# Log\n\nThe tide turned at noon.\n',
    previews = countPreviews(page);
  await open(page, path, content, 'source');
  await page.locator('#editor').press(documentEnd);
  await page.keyboard.type('\n## Morning\n\nGulls.\n\n## Evening');
  await expectEditorText(page, content + '\n## Morning\n\nGulls.\n\n## Evening');
  await page.locator('#note-context-toggle').click();
  await expect(page.locator('[data-context-tab="contents"]')).toHaveAttribute('aria-pressed', 'true');
  await expect(page.locator('#note-context-contents a')).toHaveText(['Morning', 'Evening']);
  await page.waitForTimeout(600);
  expect(previews).toEqual([]);
});

// In a compact note, a heading right after a list or a quote, with no blank
// line between them, is a section while editing as it is in Read.
test('Contents while editing lists the sections that follow lists and quotes', async ({page}) => {
  const path = 'context-compact.md',
    content = '# Notes\n- a\n## Groceries\n- milk\n## Todo\n> quoted\n## Ideas\n1. one\n2. two\n### Result\n',
    sections = ['Groceries', 'Todo', 'Ideas', 'Result'],
    previews = countPreviews(page);
  await open(page, path, content, 'source');
  await page.locator('#note-context-toggle').click();
  await expect(page.locator('[data-context-tab="contents"]')).toHaveAttribute('aria-pressed', 'true');
  const links = page.locator('#note-context-contents a');
  await expect(links).toHaveText(sections);
  await links.getByText('Ideas', {exact: true}).click();
  await expect(page.locator('#editor')).toBeFocused();
  expect(await caret(page)).toEqual({head: content.indexOf('## Ideas'), line: '## Ideas'});
  expect(previews).toEqual([]);
  // Read lists the same sections, from the server's rendering.
  await page.locator('#mode').click();
  await expect(page.locator('#preview h3')).toHaveText('Result');
  await expect(links).toHaveText(sections);
});

// Show passage on a passage the editor has not placed shows it in Read once
// Read has rendered the text edited. When that render fails, the request
// goes with it: a later render, here for another session's typing, does
// not scroll to the passage on its own.
test('a failed render drops the passage Show passage was waiting for', async ({page, browser}) => {
  const path = 'passage-render-fails.md',
    anchor = {quote: 'That once found out, and all the rest were plain.'};
  await app.api('/p/default/api/note', {path, content: manuscript, revision: ''});
  await addComment(path, anchor);
  // The editor never places the passage: its worker's answers are dropped.
  await page.addInitScript(() => {
    const Original = Worker;
    window.Worker = class extends Original {
      constructor(...args) {
        super(...args);
        this.addEventListener('message', event => {
          if (event.data?.ranges) event.stopImmediatePropagation();
        });
      }
    };
  });
  await open(page, path, null);
  await page.locator('#editor').press(documentEnd);
  await page.keyboard.type(' typed');
  await expectEditorText(page, manuscript + ' typed');
  await page.route('**/api/preview', route => route.abort());
  const failed = page.waitForEvent('requestfailed', request => request.url().endsWith('/api/preview'));
  await page.locator('#note-context-toggle').click();
  await page.locator('[data-context-tab="comments"]').click();
  await page.locator('.comment-quote').filter({hasText: anchor.quote}).click();
  await failed;
  await expect(page.locator('#preview')).toBeVisible();
  await expect(page.locator('#notice')).toBeVisible();
  await page.unroute('**/api/preview');
  await page.evaluate(() => scrollTo(0, 0));
  const other = await browser.newContext();
  try {
    const desk = await other.newPage();
    await app.login(desk, 'recovery');
    await desk.goto(app.origin + '/?project=default&note=' + encodeURIComponent(path));
    await expect(desk.locator('#document')).toHaveAttribute('data-live', 'connected');
    await desk.locator('#mode').click();
    await desk.locator('#editor').press(documentEnd);
    await desk.keyboard.type(' remote');
    await expect(page.locator('#preview')).toContainText('typed remote');
  } finally {
    await other.close();
  }
  // Longer than a smooth scroll to the passage.
  await page.waitForTimeout(1000);
  expect(await page.evaluate(() => scrollY)).toBe(0);
  await expect(page.locator('#preview').getByText('That once found out')).not.toBeInViewport();
});

test('highlights follow their passage through edits and leave when it changes', async ({page}) => {
  const path = 'highlight-edits.md',
    content = '# Log\n\nThe tide turned at noon.\n\nGulls circled the mast.\n';
  await app.api('/p/default/api/note', {path, content, revision: ''});
  await addComment(path, {quote: 'circled the mast', prefix: 'noon. Gulls ', suffix: '.'});
  await open(page, path, null, 'source');
  await expect.poll(() => highlighted(page)).toBe('circled the mast');
  // Typing before the passage moves it.
  const tide = await revealLine(page, 'The tide');
  await tide.click();
  await page.keyboard.press('End');
  await page.keyboard.type(' Rain followed.');
  await expect.poll(() => highlighted(page)).toBe('circled the mast');
  // An edit inside it removes it, and placing again after a pause does not
  // find the changed passage. (The pause makes it an undo step of its own.)
  await page.waitForTimeout(700);
  await selectText(page, 'mast', 'mast');
  await page.keyboard.type('masts');
  await expect(highlights(page)).toHaveCount(0);
  await page.waitForTimeout(1500);
  await expect(highlights(page)).toHaveCount(0);
  // Undo brings the passage back, and it is placed again.
  await page.keyboard.press('ControlOrMeta+z');
  await expectEditorText(page, content.replace('noon.', 'noon. Rain followed.'));
  await expect.poll(() => highlighted(page)).toBe('circled the mast');
  await page.keyboard.press('ControlOrMeta+z');
  await expectEditorText(page, content);
});

// A peer's or an agent's edit moves highlights like the user's own (plan
// risk 11). A composition inside a passage commits exactly once while its
// highlight only moves with it; placed again afterwards, the changed passage
// is no longer found (plan risk 5).
test('remote edits move a highlight and a composition inside it commits exactly once', async ({page, context}) => {
  const path = 'highlight-remote.md',
    content = '# Log\n\nThe tide turned at noon.\n\nGulls circled the mast.\n';
  await app.api('/p/default/api/note', {path, content, revision: ''});
  await addComment(path, {quote: 'circled the mast', prefix: 'noon. Gulls ', suffix: '.'});
  await open(page, path, null);
  await expect.poll(() => highlighted(page)).toBe('circled the mast');
  const base = await app.api('/p/default/api/live?path=' + path),
    start = base.content.indexOf('noon');
  await app.api('/p/default/api/live/patch', {
    path,
    base: base.liveRevision,
    operationId: 'comments-peer',
    edits: [{start, end: start + 4, old: 'noon', text: 'midday'}]
  });
  const moved = content.replace('noon', 'midday');
  await expectEditorText(page, moved);
  await expect.poll(() => highlighted(page)).toBe('circled the mast');
  // Placed again, the quote is still unique and its other context holds.
  await page.waitForTimeout(1500);
  await expect.poll(() => highlighted(page)).toBe('circled the mast');

  await caretAtLineEnd(page, 'Gulls');
  for (let i = 0; i < ' the mast.'.length; i++) await page.keyboard.press('ArrowLeft');
  const cdp = await context.newCDPSession(page);
  await cdp.send('Input.imeSetComposition', {text: 'ゆ', selectionStart: 1, selectionEnd: 1});
  await cdp.send('Input.imeSetComposition', {text: 'ゆっく', selectionStart: 3, selectionEnd: 3});
  await cdp.send('Input.insertText', {text: 'ゆっくり'});
  const composed = moved.replace('circled the', 'circledゆっくり the');
  await expectEditorText(page, composed);
  await expect.poll(async () => (await app.api('/p/default/api/live?path=' + path)).content).toBe(composed);
  await expect(highlights(page)).toHaveCount(0, {timeout: 5000});
  expect(await editorText(page)).toBe(composed);
});

// Placing runs in a worker over the whole note, so it reaches a passage at
// the end of a novel-size note on a CPU slowed four times without a long
// task in the page while the passage is placed.
test('a passage at the end of a novel-size note is highlighted on a slow CPU', async ({page}) => {
  test.setTimeout(240000);
  const path = 'novel-comment.md',
    body = manuscript.slice(manuscript.indexOf('## Chapter 1.')),
    content =
      manuscript.slice(0, manuscript.indexOf('## Chapter 1.')) +
      Array.from({length: 23}, (_, round) =>
        body.replace(/^## Chapter (\d+)\./gm, (_, number) => '## Chapter ' + (round * 3 + Number(number)) + '.')
      ).join('\n') +
      '\nThe very *last* line of the book.\n',
    anchor = {quote: 'very last line', prefix: '', suffix: ' of the book.'};
  expect(content.length).toBeGreaterThan(1100000);
  await app.api('/p/default/api/note', {path, content, revision: ''});
  await addComment(path, anchor);
  // Long tasks, and a time inside each task that does the page's part of
  // placing: reading the text and posting it to the worker, then decorating
  // what the worker placed.
  await page.addInitScript(() => {
    window.longTasks = [];
    window.placing = [];
    new PerformanceObserver(list =>
      window.longTasks.push(...list.getEntries().map(({startTime, duration}) => ({startTime, duration})))
    ).observe({type: 'longtask', buffered: false});
    const Native = window.Worker;
    window.Worker = class extends Native {
      constructor(...args) {
        super(...args);
        this.addEventListener('message', ({data}) => {
          if (data?.ranges) window.placing.push({kind: 'placed', at: performance.now()});
        });
      }
      postMessage(message, ...rest) {
        if (message?.kind === 'comments')
          window.placing.push({kind: 'read', at: performance.now(), length: message.source.length});
        return super.postMessage(message, ...rest);
      }
    };
  });
  await page.goto(app.origin + '/?project=default&note=' + path);
  // A novel-size note's live start and its editor's opening at 4x are slow
  // under load (as in editor-budgets and side-by-side).
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected', {timeout: 60000});
  const cdp = await page.context().newCDPSession(page);
  await cdp.send('Emulation.setCPUThrottlingRate', {rate: 4});
  await page.evaluate(() => (window.longTasks = []));
  await page.locator('#mode').click();
  await expect(page.locator('#editor')).toBeVisible({timeout: 60000});
  await page.locator('#editor').press(documentEnd);
  await expect.poll(() => highlighted(page), {timeout: 90000}).toMatch(/^very \*?last\*? line$/);
  const {tasks, placing} = await page.evaluate(() => ({tasks: window.longTasks, placing: window.placing}));
  await cdp.send('Emulation.setCPUThrottlingRate', {rate: 1});
  // The whole text was read once, at 4x, and placed once.
  expect(placing.map(({kind, length}) => [kind, length])).toEqual([
    ['read', content.length],
    ['placed', undefined]
  ]);
  // Placing adds no task over 200 ms at 4x: the 1.2 MB text is read once in
  // a timer and parsed in the worker, never in the page. Drawing the end of
  // the book is the editor's (live-preview-budgets.spec.mjs measures it).
  const placingTasks = tasks.filter(task =>
    placing.some(({at}) => at >= task.startTime && at <= task.startTime + task.duration)
  );
  expect(Math.max(0, ...placingTasks.map(task => task.duration))).toBeLessThan(200);
});
