// A book-length note: one editor for the whole text in every mode, remote
// changes applied as changes, and edits that change only the typed bytes.
import {test, expect} from '@playwright/test';
import {readFileSync} from 'node:fs';
import {startApp} from './app-fixture.mjs';
import {
  caretAtLineEnd,
  countLiveUpdates,
  editorText,
  expectEditorText,
  expectVisual,
  revealLine
} from './editor-text.mjs';

// Chapters 1–3 of the F1 manuscript (~/forgetmenote-design/fixtures/f1-moby-dick.md,
// SHA-256 15bf4dd6…), cut before "## Chapter 4." with its fmn-id line and the
// cut's trailing blank line removed: 53,034 bytes, SHA-256
// 2ec600d6f9840f2b18d4fcbd1af42fc21069472c753dfc39f45b17a54390ddc8.
const manuscript = readFileSync(new URL('./fixtures/f1-slice.md', import.meta.url), 'utf8');
// A novel-size note (about 1.2 MB, F1's size): the three chapters again and
// again, numbered on.
const body = manuscript.slice(manuscript.indexOf('## Chapter 1.'));
const novel =
  manuscript.slice(0, manuscript.indexOf('## Chapter 1.')) +
  Array.from({length: 23}, (_, round) =>
    body.replace(/^## Chapter (\d+)\./gm, (_, number) => '## Chapter ' + (round * 3 + Number(number)) + '.')
  ).join('\n') +
  '\n**The end.**\n';
const lastSentence = 'I turned in, and never slept better in my life.';

let app;
test.beforeAll(async () => {
  app = await startApp({
    name: 'forgetmenote-manuscript-',
    account: {username: 'manuscript-admin', password: 'DisposableManuscript123!'}
  });
});
test.afterAll(async () => {
  await app?.close();
});
test.beforeEach(async ({page}) => {
  page.on('pageerror', error => {
    throw error;
  });
  // Token sign-in: password sign-in allows ten attempts a minute per account.
  await app.login(page, 'recovery');
});

const live = async path => app.api('/p/default/api/live?path=' + encodeURIComponent(path));
async function patch(path, old, text, operationId) {
  const base = await live(path),
    start = base.content.indexOf(old);
  expect(start).toBeGreaterThanOrEqual(0);
  return app.api('/p/default/api/live/patch', {
    path,
    base: base.liveRevision,
    operationId,
    edits: [{start, end: start + old.length, old, text}]
  });
}
// Creates the note unless `content` is null (it exists already), then edits
// it in `format`.
async function open(page, path, content = manuscript, format = 'visual') {
  if (content !== null) await app.api('/p/default/api/note', {path, content, revision: ''});
  await page.goto(app.origin + '/?project=default&note=' + encodeURIComponent(path));
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await page.locator('#mode').click();
  await page.locator('#format-' + format).click();
  await expect(page.locator('#editing')).toHaveAttribute('data-format', format);
}
// Selects `length` characters from `offset` into the editor line starting
// with `text`, through the DOM selection CodeMirror follows. The line must
// have no hidden markup before the selection.
async function selectIn(page, text, offset, length = 0) {
  const line = await revealLine(page, text);
  await page.locator('#editor').focus();
  await page.evaluate(() => new Promise(resolve => setTimeout(resolve, 50)));
  await line.evaluate(
    (line, [offset, length]) => {
      const walker = document.createTreeWalker(line, NodeFilter.SHOW_TEXT);
      const point = at => {
        for (let node = walker.nextNode(); node; node = walker.nextNode()) {
          if (at <= node.data.length) return [node, at];
          at -= node.data.length;
        }
      };
      const [node, start] = point(offset);
      getSelection().setBaseAndExtent(node, start, node, start + length);
    },
    [offset, length]
  );
}
const selected = page => page.evaluate(() => getSelection().toString());

test('a novel-size note opens whole in Visual and stays one note', async ({page}) => {
  const path = 'manuscript/novel.md',
    sent = countLiveUpdates(page);
  await open(page, path, novel);
  await expectVisual(page);
  await expect(page.locator('#visual-sections')).toHaveCount(0);
  await expect(page.locator('#editor .cm-lp-h1')).toHaveText('Moby-Dick; or, The Whale');
  await expect(page.locator('#editor .cm-lp-frontmatter-summary')).toContainText('manuscript');
  await expectEditorText(page, novel);
  // CodeMirror draws only what is near the screen; the end is one key away.
  await expect(page.locator('#editor')).not.toContainText('Chapter 69.');
  await page.locator('#editor').press('Control+End');
  await expect(page.locator('#editor')).toContainText(lastSentence);
  // The end is formatted as soon as it shows.
  await expect(page.locator('#editor .cm-lp-strong').filter({hasText: 'The end.'})).toBeInViewport();
  await page.locator('#editor').press('Control+Home');
  await expect(page.locator('#editor .cm-lp-h1')).toBeInViewport();
  // Looking around sends nothing and changes nothing.
  await page.locator('#format-source').click();
  await page.locator('#format-visual').click();
  await expectEditorText(page, novel);
  expect(sent).toEqual([]);
  expect((await live(path)).content).toBe(novel);
});

test('an edit changes only the typed bytes and every other byte stays', async ({page}) => {
  const path = 'manuscript/bytes.md';
  // Blocks a serializer would respell (bullets, a setext heading, a ")" list,
  // the blank line after the front matter) share the chapter.
  const respelled = '- dash item\n- another\n\nSetext title\n============\n\n1) paren list\n\n',
    content = manuscript.replace('## Chapter 2.', respelled + '## Chapter 2.');
  // Updates carry only what the server lacks; it accepts them as they are.
  const updates = [];
  page.on('request', request => {
    if (request.method() === 'POST' && new URL(request.url()).pathname === '/p/default/api/live')
      updates.push(Buffer.from(request.postDataJSON().update, 'base64').length);
  });
  await open(page, path, content);
  const paragraph = 'Call me Ishmael.',
    end = content.indexOf('\n', content.indexOf(paragraph));
  await caretAtLineEnd(page, paragraph);
  await page.keyboard.type(' Typed here.');
  const expected = content.slice(0, end) + ' Typed here.' + content.slice(end);
  await expect.poll(async () => (await live(path)).content).toBe(expected);
  await expectEditorText(page, expected);
  expect(updates.length).toBeGreaterThan(0);
  for (const bytes of updates) expect(bytes).toBeLessThan(2000);
  // Chapter 3 has emphasis, quotes and dashes a serializer would rewrite.
  await selectIn(page, 'Entering that gable-ended Spouter-Inn', 0);
  await page.keyboard.type('Then. ');
  const next = expected.replace('Entering that gable-ended Spouter-Inn', 'Then. Entering that gable-ended Spouter-Inn');
  await expect.poll(async () => (await live(path)).content).toBe(next);
  await page.locator('#save').click();
  await expect(page.locator('#save-state')).toContainText('Saved');
  expect((await app.api('/p/default/api/note?path=' + encodeURIComponent(path))).content).toBe(next);
});

for (const format of ['visual', 'source'])
  test('remote and agent changes keep the selection in ' + format, async ({page}) => {
    const path = 'manuscript/remote-' + format + '.md';
    await open(page, path, manuscript, format);
    // "your" in "There now is your insular city"; the line has no markup.
    const target = 'There now is your insular city';
    await selectIn(page, target, 13, 4);
    await expect.poll(() => selected(page)).toBe('your');
    await patch(path, 'Call me Ishmael.', 'Call me Ishmael, wrote the agent.', 'manuscript-remote-1-' + format);
    await expect(page.locator('#editor')).toContainText('wrote the agent');
    expect(await selected(page)).toBe('your');
    // A change after the selection, out of sight, leaves it alone too.
    await patch(path, 'Chapter 3. The Spouter-Inn.', 'Chapter 3. The Spouter Inn.', 'manuscript-remote-2-' + format);
    await expect.poll(async () => (await editorText(page)).includes('Chapter 3. The Spouter Inn.')).toBe(true);
    expect(await selected(page)).toBe('your');
    await page.keyboard.type('our');
    await expect.poll(async () => (await live(path)).content).toContain('There now is our insular city');
    await expectEditorText(page, (await live(path)).content);
  });

for (const format of ['visual', 'source'])
  test('Undo of an edit off screen in ' + format + ' scrolls to it with the caret after the change', async ({page}) => {
    const path = 'manuscript/undo-' + format + '.md';
    await open(page, path, manuscript, format);
    await page.locator('#editor').press('Control+End');
    await page.keyboard.type('The end.');
    await expect.poll(async () => (await live(path)).content).toBe(manuscript + 'The end.');
    await page.locator('#editor').press('Control+Home');
    // CodeMirror draws only what is near the screen.
    await expect(page.locator('#editor')).not.toContainText(lastSentence);
    await page.locator('#editor').press('Control+z');
    await expect.poll(async () => (await live(path)).content).toBe(manuscript);
    await expect(page.locator('#editor').getByText(lastSentence)).toBeInViewport();
    await page.keyboard.type('Typed at the end.');
    await expect.poll(async () => (await live(path)).content).toBe(manuscript + 'Typed at the end.');
  });

test('an IME composition next to hidden formatting in Visual commits exactly once', async ({page, context}) => {
  const path = 'manuscript/ime.md',
    content = '# IME\n\nSome **bold** text.\n\nLast line.\n';
  await open(page, path, content);
  await expectVisual(page);
  await expect(page.locator('#editor .cm-lp-strong')).toHaveText('bold');
  // The caret right after "bold": its markers show while it touches them.
  await caretAtLineEnd(page, 'Some');
  for (let i = 0; i < ' text.'.length; i++) await page.keyboard.press('ArrowLeft');
  await expect(page.locator('#editor')).toContainText('Some **bold** text.');
  const cdp = await context.newCDPSession(page);
  await cdp.send('Input.imeSetComposition', {text: 'に', selectionStart: 1, selectionEnd: 1});
  await cdp.send('Input.imeSetComposition', {text: '日本', selectionStart: 2, selectionEnd: 2});
  await cdp.send('Input.insertText', {text: '日本語'});
  const expected = content.replace('**bold**', '**bold**日本語');
  await expectEditorText(page, expected);
  await expect.poll(async () => (await live(path)).content).toBe(expected);
  // Decorations are rebuilt after the composition, around the new text.
  await page.keyboard.press('End');
  await page.keyboard.press('ArrowDown');
  await expect(page.locator('#editor .cm-lp-strong')).toHaveText('bold');
  await expect(page.locator('#editor')).toContainText('Some bold日本語 text.');
  expect(await editorText(page)).toBe(expected);
});

// CodeMirror draws only the lines near the screen, so the browser's find
// cannot see the rest: Ctrl+F opens the editor's own search of the whole note.
for (const format of ['source', 'visual'])
  test('Find in ' + format + ' searches the whole manuscript, not only the lines on screen', async ({page}) => {
    const path = 'manuscript/find-' + format + '.md',
      term = 'never slept better',
      replacement = 'always slept well';
    await open(page, path, manuscript, format);
    await page.locator('#editor').press('Control+Home');
    await expect(page.locator('#editor')).not.toContainText(term);
    await page.locator('#editor').press('Control+f');
    const field = page.locator('.cm-search input[name=search]');
    await expect(field).toBeFocused();
    await field.pressSequentially(term);
    await field.press('Enter');
    const match = page.locator('.cm-searchMatch-selected');
    await expect(match).toHaveText(term);
    await expect(match).toBeInViewport();
    // The panel wears the app's controls and colours.
    const look = await page.evaluate(() => {
      const style = selector => getComputedStyle(document.querySelector(selector));
      return {
        panel: style('.cm-panels').backgroundColor,
        surface: style('#note-editor').backgroundColor,
        button: style('.cm-search .cm-button').backgroundImage,
        radius: style('.cm-search .cm-button').borderRadius,
        toolbar: style('#format-source').borderRadius,
        font: style('.cm-search').fontFamily,
        interface: style('#format-source').fontFamily
      };
    });
    expect(look.panel).toBe(look.surface);
    expect(look.button).toBe('none');
    expect(look.radius).toBe(look.toolbar);
    expect(look.font).toBe(look.interface);
    // Escape returns to the text with the match selected; typing replaces it.
    await field.press('Escape');
    await expect(page.locator('.cm-search')).toHaveCount(0);
    await expect(page.locator('#editor')).toBeFocused();
    await page.keyboard.type(replacement);
    const replaced = manuscript.replace(term, replacement);
    await expect.poll(async () => (await live(path)).content).toBe(replaced);
    // Undo in the search field does not reach the note, nor does composing there hold it.
    await page.locator('#editor').press('Control+f');
    await expect(field).toBeFocused();
    await field.press('Control+z');
    await expectEditorText(page, replaced);
    await field.dispatchEvent('compositionstart');
    await patch(path, 'Call me Ishmael.', 'Call me Ishmael, said the agent.', 'manuscript-find-remote-' + format);
    await expectEditorText(page, replaced.replace('Call me Ishmael.', 'Call me Ishmael, said the agent.'));
    await field.dispatchEvent('compositionend');
  });
