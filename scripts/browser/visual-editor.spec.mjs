import {test, expect} from '@playwright/test';
import {startApp} from './app-fixture.mjs';
import {
  caretAtLineEnd,
  countLiveUpdates,
  editorLine,
  editorText,
  expectEditorText,
  expectVisual
} from './editor-text.mjs';

// One password login for the file: the server allows ten a minute for an
// account, fewer than this file has cases.
let app, cookies;
test.beforeAll(async ({browser}) => {
  app = await startApp({
    name: 'forgetmenote-visual-',
    account: {username: 'visual-admin', password: 'DisposableVisual123!'}
  });
  const context = await browser.newContext(),
    page = await context.newPage();
  try {
    await app.login(page, 'password');
    cookies = await context.cookies();
  } finally {
    await context.close();
  }
});
test.afterAll(async () => {
  await app?.close();
});
test.beforeEach(async ({page, context}) => {
  page.on('pageerror', error => {
    throw error;
  });
  await context.addCookies(cookies);
});

async function openEditor(page, path, content) {
  await app.api('/p/default/api/note', {path, content, revision: ''});
  await page.goto(app.origin + '/?project=default&note=' + encodeURIComponent(path));
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await page.locator('#mode').click();
}

test('visual edits preserve named anchors and inert HTML through toggles save reload and undo', async ({page}) => {
  const path = 'anchors.md',
    prefix = '---\ntitle: Anchor fixture\ncustom: retained\n---\n';
  const markup = ['<a id="source-001"></a>', "<a name='legacy-target'></a>", '<!-- retained marker -->', '<br>'];
  const content =
    prefix +
    '# Anchor fixture\n\n' +
    markup.slice(0, 3).join('\n\n') +
    '\n\nParagraph to edit.\n\nA line<br>Another line.\n';
  const sent = countLiveUpdates(page);
  await openEditor(page, path, content);
  await expectVisual(page);
  // Front matter is a Properties summary; HTML is literal text, never markup.
  await expect(page.locator('#editor .cm-lp-frontmatter-summary')).toContainText('Properties');
  for (const raw of markup) await expect(page.locator('#editor')).toContainText(raw);
  expect(await page.locator('#editor a').count()).toBe(0);
  await expect(editorLine(page, 'A line<br>Another line.')).toBeVisible();
  // Switching modes only reconfigures the editor: nothing is sent or changed.
  for (const format of ['source', 'split', 'visual', 'source', 'visual']) {
    await page.locator('#format-' + format).click();
    await expect(page.locator('#editing')).toHaveAttribute('data-format', format);
    await expectEditorText(page, content);
  }
  await expectVisual(page);
  expect(sent).toEqual([]);
  await caretAtLineEnd(page, 'Paragraph to edit.');
  await page.keyboard.type(' Added.');
  await expectEditorText(page, content.replace('Paragraph to edit.', 'Paragraph to edit. Added.'));
  await page.getByRole('button', {name: 'Undo', exact: true}).click();
  await expectEditorText(page, content);
  await page.getByRole('button', {name: 'Redo', exact: true}).click();
  await expectEditorText(page, content.replace('Paragraph to edit.', 'Paragraph to edit. Added.'));
  await page.locator('#save').click();
  await expect(page.locator('#save-state')).toContainText('Saved');
  await expect(page.locator('#save')).toBeDisabled();
  const saved = await app.api('/p/default/api/note?path=' + path);
  expect(saved.content).toBe(content.replace('Paragraph to edit.', 'Paragraph to edit. Added.'));
  expect(saved.content.startsWith(prefix)).toBe(true);
  for (const raw of markup) expect(saved.content).toContain(raw);
  expect((await app.api('/p/default/api/note?path=' + path + '&revision=' + saved.revision)).content).toBe(
    saved.content
  );
  await page.reload();
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await page.locator('#mode').click();
  await expectVisual(page);
  await page.locator('#format-source').click();
  await expectEditorText(page, saved.content);
});

test('visual markup stays inert and survives live projection', async ({page}) => {
  const path = 'inert.md',
    markup = '<img src="https://invalid.example/tracker" onerror="window.htmlExecuted=true">';
  const script = '<script>window.htmlExecuted=true</script>';
  const content =
    '# Inert fixture\n\n<a id="live-target"></a>\n\n' +
    markup +
    '\n\n' +
    script +
    '\n\n![External](https://invalid.example/image.png) and [link](javascript:window.htmlExecuted=true)\n\n' +
    'Before remote change.\n';
  const requests = [];
  page.on('request', request => {
    if (request.url().includes('invalid.example')) requests.push(request.url());
  });
  await openEditor(page, path, content);
  await expectVisual(page);
  await expect(page.locator('#editor')).toContainText(markup);
  await expect(page.locator('#editor .cm-lp-image-missing')).toHaveText('External');
  await page
    .locator('#editor .cm-lp-link')
    .filter({hasText: 'link'})
    .click({modifiers: ['ControlOrMeta']});
  expect(await page.evaluate(() => window.htmlExecuted)).toBeUndefined();
  // CodeMirror's own cursor buffers around widgets are the only images.
  expect(await page.locator('#editor img:not(.cm-widgetBuffer), #editor script, #editor iframe').count()).toBe(0);
  expect(requests).toEqual([]);
  const base = await app.api('/p/default/api/live?path=' + path),
    start = base.content.indexOf('Before remote change.');
  await app.api('/p/default/api/live/patch', {
    path,
    base: base.liveRevision,
    operationId: 'visual-markup-remote',
    edits: [{start, end: start + 21, old: 'Before remote change.', text: 'After remote change.'}]
  });
  await expect(page.locator('#editor')).toContainText('After remote change.');
  await expectVisual(page);
  await caretAtLineEnd(page, 'After remote change.');
  await page.keyboard.type(' Local.');
  await page.locator('#save').click();
  await expect(page.locator('#save-state')).toContainText('Saved');
  await expect(page.locator('#save')).toBeDisabled();
  const saved = await app.api('/p/default/api/note?path=' + path);
  expect(saved.content).toBe(content.replace('Before remote change.', 'After remote change. Local.'));
  expect(requests).toEqual([]);
  expect(await page.evaluate(() => window.htmlExecuted)).toBeUndefined();
});

test('typing in Visual adds exactly the typed bytes and blank lines stay blank across modes', async ({page}) => {
  const content = '# Blank lines\n\nFirst paragraph.\n\nLast paragraph.\n';
  await openEditor(page, 'blank-lines.md', content);
  await expectVisual(page);
  await caretAtLineEnd(page, 'First paragraph.');
  await page.keyboard.press('Enter');
  await page.keyboard.press('Enter');
  await page.keyboard.type('New paragraph.');
  const typed = content.replace('First paragraph.', 'First paragraph.\n\nNew paragraph.');
  await expectEditorText(page, typed);
  await page.locator('#format-source').click();
  await expectEditorText(page, typed);
  await page.locator('#format-visual').click();
  await expectVisual(page);
  await expect(page.locator('#editor')).not.toContainText('<br');
  await caretAtLineEnd(page, 'New paragraph.');
  await page.keyboard.type(' More.');
  await expectEditorText(page, typed.replace('New paragraph.', 'New paragraph. More.'));
});

test('references and footnotes stay in Visual, bytes exact, on desktop tablet and phone', async ({page}) => {
  const content =
    '# References\n\nRead [the guide][guide] and [Guide].[^1]\n\n[guide]: https://example.com/guide\n\n[^1]: A footnote.\n';
  const sent = countLiveUpdates(page);
  await openEditor(page, 'references.md', content);
  for (const width of [1440, 768, 390]) {
    await page.setViewportSize({width, height: 900});
    await expectVisual(page);
    await expect(page.locator('#format-visual')).toBeVisible();
    // Reference links resolve through the note's definitions.
    await expect(page.locator('#editor .cm-lp-link').filter({hasText: 'the guide'})).toHaveAttribute(
      'data-lp-href',
      'https://example.com/guide'
    );
    await expect(page.locator('#editor .cm-lp-link').filter({hasText: /^Guide$/})).toBeVisible();
    await expectEditorText(page, content);
  }
  // Side by side is offered only on wide screens.
  await expect(page.locator('#format-split')).toBeHidden();
  await page.setViewportSize({width: 1440, height: 900});
  await expect(page.locator('#format-split')).toBeVisible();
  expect(sent).toEqual([]);
  const base = await app.api('/p/default/api/live?path=references.md'),
    start = base.content.indexOf('Read');
  await app.api('/p/default/api/live/patch', {
    path: 'references.md',
    base: base.liveRevision,
    operationId: 'references-remote',
    edits: [{start, end: start + 4, old: 'Read', text: 'Consult'}]
  });
  await expectEditorText(page, content.replace('Read', 'Consult'));
  await expectVisual(page);
  await caretAtLineEnd(page, 'A footnote.');
  await page.keyboard.type(' Typed.');
  await expectEditorText(page, content.replace('Read', 'Consult').replace('A footnote.', 'A footnote. Typed.'));
});

test('the chosen mode applies to every note and survives a reload', async ({page}) => {
  const plain = '# Supported\n\nAn ordinary paragraph.\n';
  await app.api('/p/default/api/note', {path: 'supported.md', content: plain, revision: ''});
  await openEditor(page, 'unsupported.md', '# Unsupported\n\nText.[^1]\n\n[^1]: Footnote.\n');
  await expectVisual(page);
  await page.locator('#format-source').click();
  await page.locator('#browse-root').click();
  await page.locator('#folder-items [data-note="supported.md"]').click();
  await page.locator('#mode').click();
  await expect(page.locator('#editor')).toHaveAttribute('aria-label', 'Markdown content');
  await expect(page.locator('#format-source')).toHaveAttribute('aria-pressed', 'true');
  await page.reload();
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await page.locator('#mode').click();
  await expect(page.locator('#format-source')).toHaveAttribute('aria-pressed', 'true');
  await page.locator('#format-visual').click();
  await expectVisual(page);
  await expectEditorText(page, plain);
});

// The context panel reads the editor's text while editing, not the hidden
// preview (plan risk 18).
test('typing renders no hidden preview, with or without the context panel, until editing ends', async ({page}) => {
  const previews = [];
  page.on('request', request => {
    if (request.method() === 'POST' && request.url().endsWith('/api/preview'))
      previews.push(request.postDataJSON().content);
  });
  const content = '# Preview cost\n\nFirst paragraph.\n';
  await openEditor(page, 'preview-cost.md', content);
  await page.locator('#format-source').click();
  await page.locator('#editor').press('Control+End');
  await page.keyboard.type('Typed without a preview.');
  await expectEditorText(page, content + 'Typed without a preview.');
  // Past the preview delay: nothing rendered the hidden preview.
  await page.waitForTimeout(800);
  expect(previews).toEqual([]);
  await page.locator('#note-context-toggle').click();
  await expect(page.locator('#comments-panel')).toBeVisible();
  await page.locator('#editor').press('Control+End');
  await page.keyboard.type(' Panel open.');
  await expectEditorText(page, content + 'Typed without a preview. Panel open.');
  await page.waitForTimeout(800);
  expect(previews).toEqual([]);
  await page.locator('#note-context-toggle').click();
  await expect(page.locator('#comments-panel')).toBeHidden();
  await page.locator('#editor').press('Control+End');
  await page.keyboard.type(' Closed again.');
  await page.waitForTimeout(800);
  expect(previews).toEqual([]);
  // Leaving edit mode renders it once.
  await page.locator('#mode').click();
  await expect(page.locator('#preview')).toContainText('Panel open. Closed again.');
  expect(previews).toEqual([content + 'Typed without a preview. Panel open. Closed again.']);
});

test('page links typed in Visual are exactly the typed bytes', async ({page}) => {
  const path = 'page-links.md',
    content = '# Page links\n\nSee [[Existing page]] for more.\n\nParagraph to edit.\n';
  await openEditor(page, path, content);
  await expectVisual(page);
  await expect(page.locator('#editor .cm-lp-wikilink')).toHaveText('Existing page');
  await caretAtLineEnd(page, 'Paragraph to edit.');
  await page.keyboard.type(' Typed [[New page]] and [[Target|label]].');
  const typed = content.replace('to edit.', 'to edit. Typed [[New page]] and [[Target|label]].');
  await expectEditorText(page, typed);
  await caretAtLineEnd(page, 'for more.');
  await page.keyboard.type(' Still a link.');
  const edited = typed.replace('for more.', 'for more. Still a link.');
  await expectEditorText(page, edited);
  await page.locator('#save').click();
  await expect(page.locator('#save-state')).toContainText('Saved');
  expect((await app.api('/p/default/api/note?path=' + path)).content).toBe(edited);
});

test('text before a page link keeps its escapes and a table cell keeps its padding when Visual edits them', async ({
  page
}) => {
  const path = 'page-link-escapes.md',
    content =
      '# Escapes\n\n1984\\. That year, see [[Orwell]]\n\nUse \\`npm\\` then see [[Setup]]\n\nThe \\<b> tag, see [[HTML]]\n\n' +
      '| Cell            | Other |\n| --------------- | ----- |\n| A \\| B [[Note]] | x     |\n';
  await openEditor(page, path, content);
  await expectVisual(page);
  for (const text of ['see Orwell', 'see Setup', 'see HTML']) {
    await caretAtLineEnd(page, text);
    await page.keyboard.type('Z');
  }
  // A click on a table cell edits the table's source at that cell.
  await page.locator('#editor .cm-lp-table td').filter({hasText: /^x$/}).click();
  await expect(editorLine(page, '| x     |')).toBeVisible();
  await page.keyboard.press('ArrowRight');
  await page.keyboard.type('Z');
  const edited = content
    .replace('[[Orwell]]', '[[Orwell]]Z')
    .replace('[[Setup]]', '[[Setup]]Z')
    .replace('[[HTML]]', '[[HTML]]Z')
    .replace('| x     |', '| xZ     |');
  await expectEditorText(page, edited);
  await page.locator('#save').click();
  await expect(page.locator('#save-state')).toContainText('Saved');
  expect((await app.api('/p/default/api/note?path=' + path)).content).toBe(edited);
  expect(await editorText(page)).toBe(edited);
});

test('Visual tells screen readers which lines are headings and at what level', async ({page}) => {
  const content =
    '# Big title\n\nA paragraph with [a link](https://example.com) and **bold**.\n\n## Section two\n\n- one\n- [ ] task\n\n' +
    'Setext title\n============\n\nTwo line\nsetext\n---\n\n> ### Quoted\n';
  await openEditor(page, 'heading-roles.md', content);
  await expectVisual(page);
  await caretAtLineEnd(page, 'A paragraph with');
  // A setext heading over two lines is one heading, named by its first line.
  await expect(page.locator('#editor')).toMatchAriaSnapshot(`
    - textbox "Note content":
      - heading "Big title" [level=1]
      - heading "Section two" [level=2]
      - checkbox "Open task"
      - heading "Setext title" [level=1]
      - heading "Two line" [level=2]
      - heading "Quoted" [level=3]
  `);
  await expect(page.locator('#editor').getByRole('heading')).toHaveCount(5);
  await expectEditorText(page, content);
});

test('task boxes stay out of the Tab order and toggling one keeps the keyboard in the note', async ({page}) => {
  const content = '# Tasks\n\nIntro line.\n\n- [ ] one\n- [ ] two\n\nEnd line.\n';
  await openEditor(page, 'task-focus.md', content);
  await expectVisual(page);
  const boxes = page.locator('#editor input.cm-lp-task');
  await expect(boxes).toHaveCount(2);
  // Tab leaves the editor instead of stopping at each box.
  await caretAtLineEnd(page, 'Intro line.');
  await page.keyboard.press('Tab');
  const outside = () => page.evaluate(() => document.activeElement.closest('#editor') === null);
  expect(await outside()).toBe(true);
  // A click while the editor has no focus toggles the task and leaves the
  // focus where it was, so a tap opens no on-screen keyboard.
  await boxes.nth(0).click();
  const first = content.replace('- [ ] one', '- [x] one');
  await expectEditorText(page, first);
  expect(await outside()).toBe(true);
  // Space on a box that has the focus (as assistive technology activates it)
  // toggles exactly that task, and the next key reaches the note.
  await caretAtLineEnd(page, 'End line.');
  await boxes.nth(1).focus();
  await page.keyboard.press('Space');
  const second = first.replace('- [ ] two', '- [x] two');
  await expectEditorText(page, second);
  await expect(page.locator('#editor')).toBeFocused();
  await page.keyboard.type('Q');
  const typed = second.replace('End line.', 'End line.Q');
  await expectEditorText(page, typed);
  // A click while writing toggles its task and leaves the caret where it was.
  await boxes.nth(0).click();
  await expectEditorText(page, typed.replace('- [x] one', '- [ ] one'));
  await expect(page.locator('#editor')).toBeFocused();
  await page.keyboard.type('M');
  await expectEditorText(page, typed.replace('- [x] one', '- [ ] one').replace('End line.Q', 'End line.QM'));
});

test('on a phone a wide table in Visual scrolls on its own and the text wraps at the screen', async ({page}) => {
  await page.setViewportSize({width: 390, height: 844});
  const prose =
    'Ordinary prose that wants to read on a phone without scrolling sideways, wrapping at the edge of the screen like any other paragraph.';
  const table =
    '| Character | Role | First appears | Ship | Fate |\n| --- | --- | --- | --- | --- |\n' +
    '| Ishmael | Narrator | Chapter 1 | Pequod | Survives |\n| Ahab | Captain | Chapter 28 | Pequod | Lost |\n';
  const content = '# Crew\n\n' + prose + '\n\n' + table + '\n' + prose + '\n';
  await openEditor(page, 'wide-table.md', content);
  for (const format of ['visual', 'source', 'visual']) {
    await page.locator('#format-' + format).click();
    await expect(page.locator('#editing')).toHaveAttribute('data-format', format);
  }
  await expectVisual(page);
  await expect(page.locator('#editor .cm-lp-table-wrap')).toBeVisible();
  const layout = await page.evaluate(() => {
    const scroller = document.getElementById('editor').closest('.cm-scroller'),
      wrap = document.querySelector('#editor .cm-lp-table-wrap');
    const prose = [...document.querySelectorAll('#editor .cm-line')].filter(line =>
      line.textContent.startsWith('Ordinary prose')
    );
    const before = wrap.scrollLeft;
    wrap.scrollLeft = 100;
    return {
      width: innerWidth,
      scroller: [scroller.scrollWidth, scroller.clientWidth],
      proseRight: Math.max(...prose.map(line => line.getBoundingClientRect().right)),
      proseLines: prose.length,
      wrap: [wrap.scrollWidth, wrap.clientWidth],
      scrolled: wrap.scrollLeft - before
    };
  });
  // The note is no wider than the screen, its prose wraps inside it, and the
  // table scrolls inside its own box.
  expect(layout.proseLines).toBe(2);
  expect(layout.scroller[0]).toBeLessThanOrEqual(layout.scroller[1]);
  expect(layout.proseRight).toBeLessThanOrEqual(layout.width);
  expect(layout.wrap[0]).toBeGreaterThan(layout.wrap[1]);
  expect(layout.scrolled).toBeGreaterThan(0);
  await expectEditorText(page, content);
});
