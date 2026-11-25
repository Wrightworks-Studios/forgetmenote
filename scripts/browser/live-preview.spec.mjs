// The live preview (frontend/live-preview.mjs) in the app's Visual mode, on
// the stress note (fixtures/stress-note.md): hidden markers, task boxes,
// tables and front matter show formatted, and looking at them never changes
// text the user did not edit. CodeMirror draws only the lines near the screen,
// so every case brings what it looks at into view and finds it by its text,
// never by its place among the lines drawn. Typing next to hidden markers,
// IME compositions and mode switches are covered on the app by
// editor-corpus, editor-manuscript and side-by-side.
import {test, expect} from '@playwright/test';
import {readFileSync} from 'node:fs';
import {startApp} from './app-fixture.mjs';
import {editorText, expectVisual} from './editor-text.mjs';

const stress = readFileSync(new URL('./fixtures/stress-note.md', import.meta.url), 'utf8');

let app;
test.beforeAll(async () => {
  app = await startApp({
    name: 'forgetmenote-live-preview-',
    account: {username: 'preview-admin', password: 'DisposablePreview123!'}
  });
});
test.afterAll(async () => {
  await app?.close();
});
test.beforeEach(async ({page}) => {
  page.on('pageerror', error => {
    throw error;
  });
  await app.login(page, 'recovery');
});

// Opens content as a note of its own in Visual.
async function open(page, content, testInfo) {
  const path = 'preview-' + testInfo.testId + '-' + testInfo.repeatEachIndex + '.md';
  await app.api('/p/default/api/note', {path, content, revision: ''});
  await page.goto(app.origin + '/?project=default&note=' + encodeURIComponent(path));
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await page.locator('#mode').click();
  await expectVisual(page);
  await expect.poll(() => editorText(page)).toBe(content);
}

const head = page => page.evaluate(() => document.querySelector('#editor').cmTile.root.view.state.selection.main.head);
// Puts the caret at a text offset, focused, and on screen.
const caretAt = (page, at) =>
  page.evaluate(at => {
    const view = document.querySelector('#editor').cmTile.root.view;
    view.focus();
    view.dispatch({selection: {anchor: at}, scrollIntoView: true});
  }, at);
// Scrolls the line holding needle to the top of the editor.
const show = (page, needle) =>
  page.evaluate(needle => {
    const view = document.querySelector('#editor').cmTile.root.view,
      at = view.state.doc.toString().indexOf(needle);
    view.dispatch({effects: view.constructor.scrollIntoView(at, {y: 'start'})});
  }, needle);
// The drawn line holding text.
const lineWith = (page, text) => page.locator('#editor .cm-line', {hasText: text}).first();
// The only difference between two texts, as {at, removed, inserted}.
function difference(before, after) {
  let start = 0;
  while (start < before.length && before[start] === after[start]) start++;
  let end = 0;
  while (end < before.length - start && end < after.length - start && before.at(-1 - end) === after.at(-1 - end)) end++;
  return {
    at: start,
    removed: before.slice(start, before.length - end),
    inserted: after.slice(start, after.length - end)
  };
}

test('markers hide away from the caret and show around it', async ({page}, testInfo) => {
  await open(page, stress, testInfo);
  await show(page, '## Inline formatting');
  await expect(lineWith(page, 'Plain text with')).toContainText(
    'Plain text with emphasis, underscore emphasis, strong, underscore strong'
  );
  // A click in "strong" shows its markers and only its markers.
  await lineWith(page, 'Plain text with')
    .locator('.cm-lp-strong', {hasText: /^strong$/})
    .click();
  await expect(lineWith(page, 'Plain text with')).toContainText(
    'Plain text with emphasis, underscore emphasis, **strong**, underscore strong'
  );
  // Headings show their # on the caret line only.
  await expect(lineWith(page, 'Inline formatting')).toHaveText('Inline formatting');
  await lineWith(page, 'Inline formatting').click();
  await expect(lineWith(page, 'Inline formatting')).toHaveText('## Inline formatting');
  // Leaving the editor hides them all again.
  await page.locator('#format-visual').focus();
  await expect(lineWith(page, 'Plain text with')).toContainText('emphasis, strong, underscore');
  await expect(lineWith(page, 'Inline formatting')).toHaveText('Inline formatting');
  expect(await editorText(page)).toBe(stress);
});

test('a task box click changes exactly its one character', async ({page}, testInfo) => {
  await open(page, stress, testInfo);
  await show(page, '## Tasks');
  const box = lineWith(page, 'An open task').locator('.cm-lp-task');
  await expect(box).not.toBeChecked();
  const caret = await head(page);
  await box.click();
  await expect(box).toBeChecked();
  expect(difference(stress, await editorText(page))).toEqual({
    at: stress.indexOf('- [ ] An open task') + 3,
    removed: ' ',
    inserted: 'x'
  });
  expect(await head(page)).toBe(caret);
  await box.click();
  await expect(box).not.toBeChecked();
  expect(await editorText(page)).toBe(stress);
});

test('a table is a widget until a click on a cell edits its source', async ({page}, testInfo) => {
  await open(page, stress, testInfo);
  await show(page, '## Tables');
  const table = page.locator('#editor .cm-lp-table').filter({hasText: 'long cell text'});
  await expect(table.locator('td').filter({hasText: 'code with | pipe'})).toHaveCount(1);
  await table.locator('td', {hasText: 'long cell text'}).click();
  // Its pipe Markdown shows, and the caret is in the cell clicked.
  await expect(table).toHaveCount(0);
  await expect(lineWith(page, '| long cell text')).toBeVisible();
  const at = await head(page);
  expect(stress.slice(at, at + 9)).toBe('long cell');
  await page.keyboard.type('X');
  expect(difference(stress, await editorText(page))).toEqual({at, removed: '', inserted: 'X'});
  // The caret leaving the table renders it again.
  await lineWith(page, 'Without outer pipes').click();
  await expect(table).toHaveCount(1);
});

test('up and down arrows enter a table and front matter instead of passing them', async ({page}, testInfo) => {
  await open(page, stress, testInfo);
  const start = stress.indexOf('| Left'),
    end = stress.indexOf('\n\nWithout outer pipes'),
    table = page.locator('#editor .cm-lp-table').filter({hasText: 'long cell text'}),
    source = lineWith(page, '| Left | Centre');
  await caretAt(page, start - 1);
  await expect(table).toBeVisible();
  await page.keyboard.press('ArrowDown');
  expect(await head(page)).toBe(start);
  await expect(source).toBeVisible();
  await expect(table).toHaveCount(0);
  // From the blank line below it.
  await caretAt(page, end + 1);
  await expect(table).toBeVisible();
  await page.keyboard.press('ArrowUp');
  expect(await head(page)).toBe(end);
  await expect(source).toBeVisible();
  await caretAt(page, stress.indexOf('# Live preview stress note'));
  await expect(page.locator('#editor .cm-lp-frontmatter-summary')).toBeVisible();
  await page.keyboard.press('ArrowUp');
  await page.keyboard.press('ArrowUp');
  expect(await head(page)).toBe(stress.indexOf('\n\n# Live preview stress note'));
  await expect(page.locator('#editor .cm-lp-frontmatter-summary')).toHaveCount(0);
  expect(await editorText(page)).toBe(stress);
});

test('front matter is collapsed until it is clicked', async ({page}, testInfo) => {
  await open(page, stress, testInfo);
  const summary = page.locator('#editor .cm-lp-frontmatter-summary');
  await expect(summary).toContainText('title Live preview stress note');
  await expect(lineWith(page, 'title: Live preview')).toHaveCount(0);
  await summary.click();
  await expect(summary).toHaveCount(0);
  await expect(lineWith(page, 'title: Live preview')).toHaveText('title: Live preview stress note');
  expect(await editorText(page)).toBe(stress);
});

test('a table far into a long note is a widget once scrolled to', async ({page}, testInfo) => {
  const filler = Array.from({length: 3000}, (_, i) => `Paragraph ${i} with *some* text.`).join('\n\n'),
    content = filler + '\n\n| a | b |\n|---|---|\n| 1 | 2 |\n\nThe end.\n';
  await open(page, content, testInfo);
  await expect(page.locator('#editor .cm-lp-table')).toHaveCount(0);
  await page.evaluate(() => {
    const view = document.querySelector('#editor').cmTile.root.view;
    view.dispatch({effects: view.constructor.scrollIntoView(view.state.doc.length)});
  });
  await expect(page.locator('#editor .cm-lp-table')).toHaveCount(1);
  await expect(page.locator('#editor .cm-lp-table td').first()).toHaveText('1');
  expect(await editorText(page)).toBe(content);
});
