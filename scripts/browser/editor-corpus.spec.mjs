// No code path may change text the user did not edit: opening a note,
// clicking, moving the caret, scrolling, selecting and switching modes send
// nothing and leave every byte, in every mode, on notes that exercise each
// part of the live preview; an abandoned composition leaves every byte too.
import {test, expect} from '@playwright/test';
import {readFileSync} from 'node:fs';
import {startApp} from './app-fixture.mjs';
import {countLiveUpdates, editorText, expectEditorText, expectVisual} from './editor-text.mjs';

const slice = readFileSync(new URL('./fixtures/f1-slice.md', import.meta.url), 'utf8');
const body = slice.slice(slice.indexOf('## Chapter 1.'));
const corpus = {
  // About 1.2 MB, F1's size.
  'novel.md':
    slice.slice(0, slice.indexOf('## Chapter 1.')) +
    Array.from({length: 23}, (_, round) =>
      body.replace(/^## Chapter (\d+)\./gm, (_, number) => '## Chapter ' + (round * 3 + Number(number)) + '.')
    ).join('\n'),
  'stress.md': readFileSync(new URL('./fixtures/stress-note.md', import.meta.url), 'utf8'),
  'crlf.md':
    '---\r\ntitle: CRLF\r\n---\r\n# Heading\r\n\r\n- item **one**\r\n- [ ] task\r\n  - nested\r\n\r\n> quote *em*\r\n\r\n| a | b |\r\n|---|--:|\r\n| 1 | 2 |\r\n\r\n```js\r\ncode\r\n```\r\n',
  'lone-cr.md': 'First line\rSecond **bold** line\r\r# Not a heading?\r\r- item\r',
  'front-matter.md':
    '---\ntitle: Front matter\ntags: [a, b]\nstatus: "draft"\n---\n# Doc\n\n---\n\nText after a rule.\n',
  'unclosed-front-matter.md': '---\ntitle: Open\n\nText **bold**.\n',
  'html.md':
    '<div class="x">\n<b>bold</b>\n</div>\n\nText <span>inline</span> <br> end. <!-- comment -->\n\n<script>window.corpusRan=true</script>\n\n<img src="https://invalid.example/x.png" onerror="window.corpusRan=true">\n',
  'references.md':
    'See [a][ref], [b], [Ref][] and [missing][none].[^1]\n\n[ref]: http://x.example "Title"\n[b]: <http://y.example/a b>\n\n[^1]: A footnote with *emphasis*.\n',
  'tables.md':
    '# Tables\n\n| Site | Depth (m) | Notes |\n|:---|---:|:---:|\n| Mill Creek | 0.5 | `code` and [[Page]] |\n| North | 12 | a \\| pipe |\n\nAfter.\n\n- a list\n\n  | nested | table |\n  |---|---|\n  | x | y |\n',
  'lists.md':
    '- one\n  - two\n    1. three\n    2) four\n- [x] done\n- [ ] open with a long line that wraps across the width of the editor many times over, so the hanging indent shows on its wrapped rows.\n\n10. ten\n11. eleven\n\n* star\n+ plus\n'
};

let app;
test.beforeAll(async () => {
  app = await startApp({
    name: 'forgetmenote-corpus-',
    account: {username: 'corpus-admin', password: 'DisposableCorpus123!'}
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

// Clicks a few drawn lines, moves the caret, scrolls, selects all and
// collapses the selection again: everything a reader does without typing.
async function lookAround(page) {
  // Lines come and go as the caret moves (a table's source, front matter):
  // each click takes a line drawn at that moment.
  const lines = page.locator('#editor .cm-line >> visible=true');
  for (const where of [0, 0.5, 1]) {
    const count = Math.min(await lines.count(), 12);
    await lines.nth(Math.round(where * (count - 1))).click();
    for (const key of ['ArrowDown', 'ArrowDown', 'ArrowRight', 'ArrowUp', 'End', 'Home'])
      await page.keyboard.press(key);
  }
  // Rendered widgets put the caret into their source.
  for (const widget of ['.cm-lp-table td', '.cm-lp-frontmatter-summary']) {
    const target = page.locator('#editor ' + widget).first();
    if (await target.isVisible()) await target.click();
  }
  const box = await page.locator('#editor').boundingBox();
  await page.mouse.move(box.x + box.width / 2, Math.min(box.y + 200, 600));
  await page.mouse.wheel(0, 1500);
  await page.mouse.wheel(0, -1500);
  await page.keyboard.press('Control+a');
  await page.keyboard.press('ArrowLeft');
}

for (const [path, content] of Object.entries(corpus))
  test('looking around ' + path + ' in every mode sends nothing and keeps every byte', async ({page, context}) => {
    test.setTimeout(120000);
    await app.api('/p/default/api/note', {path, content, revision: ''});
    const sent = countLiveUpdates(page);
    await page.goto(app.origin + '/?project=default&note=' + encodeURIComponent(path));
    // novel.md's live start is slow under load (as in editor-budgets,
    // side-by-side and editor-comments).
    await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected', {timeout: 60000});
    await page.locator('#mode').click();
    await expectEditorText(page, content);
    for (const format of ['visual', 'source', 'split', 'visual']) {
      await page.locator('#format-' + format).click();
      await expect(page.locator('#editing')).toHaveAttribute('data-format', format);
      await lookAround(page);
      expect(await editorText(page), format).toBe(content);
    }
    // Nothing reached the server, and the note is not unsaved.
    await page.waitForTimeout(500);
    expect(sent).toEqual([]);
    expect((await app.api('/p/default/api/live?path=' + encodeURIComponent(path))).content).toBe(content);
    await expect(page.locator('#save')).toBeDisabled();
    // A composition abandoned without text leaves every byte: what it showed
    // while composing is typed input, and it is gone again.
    await page.locator('#editor').press('Control+End');
    const cdp = await context.newCDPSession(page);
    await cdp.send('Input.imeSetComposition', {text: 'x', selectionStart: 1, selectionEnd: 1});
    await cdp.send('Input.imeSetComposition', {text: '', selectionStart: 0, selectionEnd: 0});
    await cdp.send('Input.insertText', {text: ''});
    await expectEditorText(page, content);
    await expect
      .poll(async () => (await app.api('/p/default/api/live?path=' + encodeURIComponent(path))).content)
      .toBe(content);
    expect(await page.evaluate(() => window.corpusRan)).toBeUndefined();
  });

// One deleting key next to a table, rule or front matter widget, or next to
// hidden markers, deletes at most the one line break or the visible text
// Source would, never a block's hidden source (plan risks 4 and 7).
const table = '| a | b |\n|---|---|\n| 1 | 2 |',
  front = '---\ntitle: Keep me\nstatus: draft\n---';
const cut = (text, at, length = 1) => text.slice(0, at) + text.slice(at + length);
const deletions = [
  // [what, note, caret, key, the note after, the widget that shows before the key]
  ...[
    ['# T\n\n' + table + '\n# After\n', '# After'],
    ['# T\n\n' + table + '\n\nAfter table.\n', '\nAfter table.']
  ].flatMap(([note, needle]) =>
    ['Backspace', 'Control+Backspace'].map(key => [
      key + ' on the line after a table',
      note,
      note.indexOf(needle),
      key,
      cut(note, note.indexOf(needle) - 1),
      '.cm-lp-table'
    ])
  ),
  ...[
    ['Before.\n' + table + '\n', 7],
    ['Before.\n\n' + table + '\n', 8]
  ].flatMap(([note, at]) =>
    ['Delete', 'Control+Delete'].map(key => [
      key + ' on the line before a table',
      note,
      at,
      key,
      cut(note, at),
      '.cm-lp-table'
    ])
  ),
  [
    'Backspace on the line after a rule',
    'Above.\n\n---\nBelow rule.\n',
    12,
    'Backspace',
    'Above.\n\n---Below rule.\n',
    '.cm-lp-hr'
  ],
  [
    'Backspace on the blank line after a rule',
    'Above.\n\n---\n\nBelow.\n',
    12,
    'Backspace',
    'Above.\n\n---\nBelow.\n',
    '.cm-lp-hr'
  ],
  ['Delete on the line before a rule', '# Title\n---\nBelow.\n', 7, 'Delete', '# Title---\nBelow.\n', '.cm-lp-hr'],
  [
    'Delete on the blank line before a rule',
    'Above.\n\n---\n\nBelow.\n',
    7,
    'Delete',
    'Above.\n---\n\nBelow.\n',
    '.cm-lp-hr'
  ],
  [
    'Backspace on the first line after front matter',
    front + '\nFirst line.\n',
    front.length + 1,
    'Backspace',
    front + '\nFirst line.\n',
    '.cm-lp-frontmatter-summary'
  ],
  [
    'Backspace on the blank line after front matter',
    front + '\n\nFirst line.\n',
    front.length + 1,
    'Backspace',
    front + '\nFirst line.\n',
    '.cm-lp-frontmatter-summary'
  ],
  [
    'Control+Backspace after hidden strong markers',
    'Some **bold** text\n',
    14,
    'Control+Backspace',
    'Some **bold**text\n',
    null
  ],
  [
    'Control+Delete before hidden strong markers',
    'Some **bold** text\n',
    4,
    'Control+Delete',
    'Some**bold** text\n',
    null
  ],
  [
    'Backspace on the line after a code block, whose fence then shows',
    'Before.\n\n```js\ncode\n```\nAfter fence.\n',
    24,
    'Backspace',
    'Before.\n\n```js\ncode\n```After fence.\n',
    null
  ]
];
const liveText = path => app.api('/p/default/api/live?path=' + encodeURIComponent(path)).then(live => live.content);

test('deleting keys next to widgets and hidden markers delete only what Source would', async ({page}) => {
  test.setTimeout(240000);
  for (const [index, [what, content, at, key, expected, widget]] of deletions.entries()) {
    const path = 'deletion-' + index + '.md';
    await app.api('/p/default/api/note', {path, content, revision: ''});
    const sent = countLiveUpdates(page);
    await page.goto(app.origin + '/?project=default&note=' + encodeURIComponent(path));
    await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
    await page.locator('#mode').click();
    await expectVisual(page);
    await expectEditorText(page, content);
    await page.evaluate(at => {
      const view = document.querySelector('#editor').cmTile.root.view;
      view.focus();
      view.dispatch({selection: {anchor: at}});
    }, at);
    // The block is a widget before the key, so its source is hidden.
    if (widget) await expect(page.locator('#editor ' + widget), what).toHaveCount(1);
    await page.waitForTimeout(100);
    await page.keyboard.press(key);
    await expectEditorText(page, expected);
    if (expected === content) await page.waitForTimeout(600);
    else await expect.poll(() => liveText(path), {message: what}).toBe(expected);
    await page.waitForTimeout(300);
    expect(sent.length, what).toBe(expected === content ? 0 : 1);
    expect(await liveText(path), what).toBe(expected);
  }
});

// Plan risk 6: a spellcheck or autocorrect replacement acts on the DOM range
// of a word, next to hidden markers; it changes only that word's bytes.
test('a spellcheck replacement of a word inside hidden markers changes only that word', async ({page}) => {
  const path = 'spelling.md',
    content = 'Some **bodl** text and *mroe* here.\n\nEnd.\n';
  await app.api('/p/default/api/note', {path, content, revision: ''});
  const sent = countLiveUpdates(page);
  await page.goto(app.origin + '/?project=default&note=' + encodeURIComponent(path));
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await page.locator('#mode').click();
  await expectVisual(page);
  await page.locator('#editor .cm-line', {hasText: 'End.'}).click();
  await expect(page.locator('#editor .cm-line').first()).toHaveText('Some bodl text and mroe here.');
  // As the browser's replacement does: select the word's text, then insert.
  for (const [wrong, right] of [
    ['bodl', 'brave'],
    ['mroe', 'more']
  ]) {
    await page.evaluate(
      ([wrong, right]) => {
        const node = [...document.querySelectorAll('#editor .cm-lp-strong, #editor .cm-lp-em')]
          .flatMap(element => [...element.childNodes])
          .find(node => node.nodeType === 3 && node.data === wrong);
        const range = document.createRange();
        range.selectNodeContents(node);
        getSelection().removeAllRanges();
        getSelection().addRange(range);
        document.execCommand('insertText', false, right);
      },
      [wrong, right]
    );
    await page.waitForTimeout(100);
  }
  const expected = 'Some **brave** text and *more* here.\n\nEnd.\n';
  await expectEditorText(page, expected);
  await expect.poll(() => liveText(path)).toBe(expected);
  expect(sent.length).toBeLessThanOrEqual(2);
});
