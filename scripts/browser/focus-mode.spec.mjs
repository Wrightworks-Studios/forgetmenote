import {test, expect} from '@playwright/test';
// Each worker owns an app; baseline reset makes cases independent within it.
test.describe.configure({mode: 'parallel'});
import {origin, api, login, edit, documentEnd, retainedTexts, useEditorApp} from './editor-fixture.mjs';
import {countLiveUpdates, editorText, expectEditorText} from './editor-text.mjs';
useEditorApp();

const shortcut = 'Control+Shift+Period';
const stored = (page, key = 'gitnotes:focus-mode') => page.evaluate(key => localStorage.getItem(key), key);
const toolbar = page => page.locator('#main > header');
const fullscreen = page => page.evaluate(() => document.fullscreenElement === document.documentElement);
async function openNote(page, path, content = '# Focus\n\nA quiet paragraph.\n') {
  const note = await api('/p/default/api/note', {path, content, revision: ''});
  await login(page);
  await page.goto(origin + '/?project=default&note=' + path);
  await expect(page.locator('#focus-toggle')).toBeVisible();
  return note;
}
async function expectFocus(page, on) {
  await expect(page.locator('body')).toHaveClass(on ? /\bfocus-mode\b/ : /^(?!.*\bfocus-mode\b)/);
  await expect(page.locator('#focus-toggle')).toHaveText(on ? 'Exit focus' : 'Focus');
  await expect(page.locator('#mode')).toBeVisible();
  if (on)
    for (const id of ['sidebar', 'note-back', 'chat-open', 'note-context-toggle', 'note-details-toggle'])
      await expect(page.locator('#' + id)).toBeHidden();
  else {
    await expect(page.locator('#note-back')).toBeVisible();
    await expect(page.locator('#note-details-toggle')).toBeVisible();
    if (page.viewportSize().width > 720) await expect(page.locator('#sidebar')).toBeVisible();
  }
}
// With a mouse, the toolbar waits below the bottom edge until the pointer reaches it.
async function revealToolbar(page) {
  const {width, height} = page.viewportSize();
  await page.mouse.move(width / 2, height - 2);
  await expect(toolbar(page)).toBeInViewport({ratio: 1});
}
async function leaveToolbar(page) {
  const {width, height} = page.viewportSize();
  await page.mouse.move(width / 2, height / 3);
}
const caretOffset = page =>
  page.evaluate(() => {
    const selection = getSelection(),
      range = document.createRange();
    range.setStart(selection.focusNode, selection.focusOffset);
    const rect =
      range.getClientRects()[0] ||
      (selection.focusNode.nodeType === 1
        ? selection.focusNode
        : selection.focusNode.parentElement
      ).getBoundingClientRect();
    return Math.abs(rect.top + rect.height / 2 - innerHeight / 2);
  });

test('focus mode toggles by button, shortcut and Esc, survives a reload and keeps the sidebar choice', async ({
  page
}) => {
  await openNote(page, 'focus-toggle.md');
  await expectFocus(page, false);
  await page.locator('#focus-toggle').click();
  await expectFocus(page, true);
  expect(await stored(page)).toBe('true');
  await expect(page.locator('#focus-toggle')).toBeFocused();

  // A reload keeps the page but cannot take the screen without a gesture.
  await page.reload();
  await expect(page.locator('#document')).toBeVisible();
  await expectFocus(page, true);
  expect(await fullscreen(page)).toBe(false);

  await page.keyboard.press('Escape');
  await expectFocus(page, false);
  expect(await stored(page)).toBe('false');
  expect(await page.evaluate(() => localStorage.getItem('gitnotes:sidebar-collapsed'))).not.toBe('true');

  await page.keyboard.press(shortcut);
  await expectFocus(page, true);
  await page.keyboard.press(shortcut);
  await expectFocus(page, false);

  // The choice applies to notes only; other views keep their navigation.
  await page.keyboard.press(shortcut);
  await page.goto(origin + '/?project=default&folder=');
  await expect(page.locator('#folder-browser')).toBeVisible();
  await expect(page.locator('#sidebar')).toBeVisible();
  await expect(page.locator('#focus-toggle')).toBeHidden();
  await page.keyboard.press(shortcut);
  expect(await stored(page)).toBe('true');
  await page.locator('#folder-items a[data-note="focus-toggle.md"]').click();
  await expectFocus(page, true);
});

test('focus mode takes the screen and shows its toolbar only at the bottom edge or from the keyboard', async ({
  page
}) => {
  await openNote(page, 'focus-screen.md');
  await page.locator('#focus-toggle').click();
  await expectFocus(page, true);
  await expect.poll(() => fullscreen(page)).toBe(true);
  // The toolbar shows where it lives, then leaves only the page.
  await expect(toolbar(page)).not.toBeInViewport();
  await revealToolbar(page);
  await expect(page.locator('#focus-controls')).toBeVisible();
  await leaveToolbar(page);
  await expect(toolbar(page)).not.toBeInViewport();

  await revealToolbar(page);
  await page.locator('#mode').click();
  const prose = page.locator('#editor');
  await expect(prose).toBeFocused();
  await expect(page.locator('#editor-tools')).toBeHidden();
  await expect(page.locator('.editor-footer')).toBeHidden();
  // Typing hides the toolbar and the pointer, even with the pointer at the edge.
  await page.keyboard.type('Typed ');
  await expect(toolbar(page)).not.toBeInViewport();
  await expect(prose).toHaveCSS('cursor', 'none');
  await leaveToolbar(page);
  await expect(prose).not.toHaveCSS('cursor', 'none');

  // Keyboard focus brings the toolbar back.
  await prose.focus();
  await page.keyboard.press('Shift+Tab');
  await expect(page.locator('#focus-toggle')).toBeFocused();
  await expect(toolbar(page)).toBeInViewport({ratio: 1});
  await page.keyboard.press('Enter');
  await expectFocus(page, false);
  await expect.poll(() => fullscreen(page)).toBe(false);
  await expect(toolbar(page)).toBeInViewport();
});

test('leaving full screen in the browser leaves focus mode; Full screen off keeps it in the window', async ({page}) => {
  await openNote(page, 'focus-fullscreen.md');
  await page.locator('#focus-toggle').click();
  await expect.poll(() => fullscreen(page)).toBe(true);
  await page.evaluate(() => document.exitFullscreen());
  await expectFocus(page, false);

  await page.locator('#focus-toggle').click();
  await expect.poll(() => fullscreen(page)).toBe(true);
  await revealToolbar(page);
  await expect(page.locator('#focus-fullscreen')).toHaveAttribute('aria-pressed', 'true');
  await page.locator('#focus-fullscreen').click();
  await expect.poll(() => fullscreen(page)).toBe(false);
  await expectFocus(page, true);
  await expect(page.locator('#focus-fullscreen')).toHaveAttribute('aria-pressed', 'false');

  await page.keyboard.press('Escape');
  await expectFocus(page, false);
  await page.keyboard.press(shortcut);
  await expectFocus(page, true);
  expect(await fullscreen(page)).toBe(false);
});

// Two presses inside one task leave the screen change of the first still on its way.
const toggleTwice = page =>
  page.evaluate(() => {
    const toggle = document.getElementById('focus-toggle');
    const changed = new Promise(resolve => document.addEventListener('fullscreenchange', resolve, {once: true}));
    toggle.click();
    toggle.click();
    return changed;
  });

test('quick toggles end with full screen matching focus mode', async ({page}) => {
  await openNote(page, 'focus-quick.md');
  // On, then off before the screen is taken: the page gives the screen back.
  await toggleTwice(page);
  await expectFocus(page, false);
  await expect.poll(() => fullscreen(page)).toBe(false);

  // Off, then on before the screen is returned: focus mode stays and takes it again.
  await page.locator('#focus-toggle').click();
  await expect.poll(() => fullscreen(page)).toBe(true);
  await toggleTwice(page);
  await expectFocus(page, true);
  await expect.poll(() => fullscreen(page)).toBe(true);
  expect(await stored(page)).toBe('true');
});

// The browser takes the screen back to ask about unsaved text; that is not the
// reader leaving focus mode. The page keeps running after the answer, until
// the next page arrives.
test('asking about unsaved text keeps focus mode whether the reader stays or reloads', async ({page}) => {
  await openNote(page, 'focus-leave.md');
  await edit(page);
  await page.keyboard.press(shortcut);
  await expectFocus(page, true);
  await expect.poll(() => fullscreen(page)).toBe(true);
  await page.locator('#editor').press(documentEnd);
  await page.keyboard.type(' Unsaved.');
  const asked = [];
  let answer = 'dismiss';
  page.on('dialog', dialog => {
    asked.push(dialog.type());
    dialog[answer]();
  });

  // Staying keeps focus mode in the window.
  await page.evaluate(() => location.reload());
  expect(asked).toEqual(['beforeunload']);
  await expect.poll(() => fullscreen(page)).toBe(false);
  await expectFocus(page, true);
  expect(await stored(page)).toBe('true');
  // Once the reader is back on the page, leaving full screen in the browser leaves focus mode again.
  await revealToolbar(page);
  await page.locator('#focus-fullscreen').click();
  await expect.poll(() => fullscreen(page)).toBe(true);
  await page.evaluate(() => document.exitFullscreen());
  await expectFocus(page, false);

  // A slow server leaves the page time to see the screen go before the next one arrives.
  await page.keyboard.press(shortcut);
  await expectFocus(page, true);
  await expect.poll(() => fullscreen(page)).toBe(true);
  answer = 'accept';
  await page.route(
    url => url.pathname === '/',
    async route => {
      await new Promise(resolve => setTimeout(resolve, 300));
      await route.continue();
    }
  );
  await page.reload();
  expect(asked).toEqual(['beforeunload', 'beforeunload']);
  await expectFocus(page, true);
  expect(await stored(page)).toBe('true');
  expect(await fullscreen(page)).toBe(false);
});

test('text size, page width, position and background change the page and persist', async ({page}) => {
  await openNote(page, 'focus-page.md');
  const preview = page.locator('#preview'),
    paper = page.locator('#document');
  const fontSize = () => preview.evaluate(element => parseFloat(getComputedStyle(element).fontSize));
  const normal = await fontSize();
  await page.keyboard.press(shortcut);
  await expectFocus(page, true);
  expect(await fontSize()).toBe(normal);
  const centred = await paper.boundingBox();
  expect(centred.x).toBeGreaterThan(0);

  await revealToolbar(page);
  await page.locator('#focus-text-size').fill('150');
  await expect.poll(fontSize).toBeCloseTo(normal * 1.5, 1);
  const wide = (await paper.boundingBox()).width;
  await page.locator('#focus-page-width').fill('60');
  await expect.poll(async () => (await paper.boundingBox()).width).toBeLessThan(wide * 0.7);
  await page.locator('#focus-page-position').fill('0');
  await expect.poll(async () => (await paper.boundingBox()).x).toBe(0);

  // color-mix() and rgb() serialize differently; compare the painted pixels.
  const colours = () =>
    page.evaluate(() => {
      const canvas = document.createElement('canvas').getContext('2d');
      return [document.body, document.getElementById('document')].map(element => {
        canvas.fillStyle = getComputedStyle(element).backgroundColor;
        canvas.fillRect(0, 0, 1, 1);
        return canvas.getImageData(0, 0, 1, 1).data.join(',');
      });
    });
  await page.locator('#focus-background').fill('0');
  await expect.poll(async () => new Set(await colours()).size).toBe(1);
  await page.locator('#focus-background').fill('80');
  await expect.poll(async () => new Set(await colours()).size).toBe(2);

  await page.reload();
  await expectFocus(page, true);
  await expect(page.locator('#focus-text-size')).toHaveValue('150');
  await expect(page.locator('#focus-page-width')).toHaveValue('60');
  await expect(page.locator('#focus-page-position')).toHaveValue('0');
  await expect(page.locator('#focus-background')).toHaveValue('80');
  await expect.poll(fontSize).toBeCloseTo(normal * 1.5, 1);
  expect((await paper.boundingBox()).x).toBe(0);

  // Leaving focus mode restores the reader's own size.
  await page.keyboard.press('Escape');
  await expectFocus(page, false);
  expect(await fontSize()).toBe(normal);
});

for (const format of ['visual', 'source'])
  test(`typewriter keeps the typed line mid-screen and dimming leaves only the current paragraph bright in ${format}`, async ({
    page
  }) => {
    const chapter = Array.from({length: 40}, (_, i) => `Paragraph ${i + 1} of a long chapter.`).join('\n\n');
    const content = '# Chapter\n\n' + chapter + '\n';
    await openNote(page, 'focus-typewriter-' + format + '.md', content);
    await page.locator('#mode').click();
    await page.locator('#format-' + format).click();
    const prose = page.locator('#editor');
    await prose.focus();
    const sent = countLiveUpdates(page);
    await page.keyboard.press(shortcut);
    await expectFocus(page, true);
    await revealToolbar(page);
    await page.locator('#focus-typewriter').click();
    await page.locator('#focus-dim').click();
    await expect(page.locator('#focus-typewriter')).toHaveAttribute('aria-pressed', 'true');
    await expect(page.locator('#focus-dim')).toHaveAttribute('aria-pressed', 'true');
    // The controls leave the caret in the text.
    await expect(prose).toBeFocused();
    expect(sent).toEqual([]);

    // The note ends with a line break: the last paragraph is the line before.
    await page.keyboard.press(documentEnd);
    await page.keyboard.press('ArrowLeft');
    await page.keyboard.type(' More.');
    await expect.poll(() => caretOffset(page)).toBeLessThan(24);
    const line = n => prose.locator('.cm-line').filter({hasText: new RegExp('^Paragraph ' + n + ' of')}),
      last = line(40);
    await expect(last).toHaveText('Paragraph 40 of a long chapter. More.');
    await expect(last).toHaveClass(/\bfmn-current-block\b/);
    await expect(prose.locator(':scope > .fmn-current-block')).toHaveCount(1);
    await expect(last).toHaveCSS('opacity', '1');
    await expect(line(38)).not.toHaveCSS('opacity', '1');

    // Moving up a paragraph (past the blank line between) moves the light
    // and keeps that line centred.
    await page.keyboard.press('ArrowUp');
    await page.keyboard.press('ArrowUp');
    await expect(line(39)).toHaveClass(/\bfmn-current-block\b/);
    await expect(last).not.toHaveCSS('opacity', '1');
    await expect.poll(() => caretOffset(page)).toBeLessThan(24);
    await expectEditorText(page, content.replace('40 of a long chapter.', '40 of a long chapter. More.'));

    await revealToolbar(page);
    await page.locator('#focus-dim').click();
    await expect(line(38)).toHaveCSS('opacity', '1');
    await page.reload();
    await expectFocus(page, true);
    await expect(page.locator('#focus-typewriter')).toHaveAttribute('aria-pressed', 'true');
    await expect(page.locator('#focus-dim')).toHaveAttribute('aria-pressed', 'false');
  });

test("text size sets the editor's text in Visual and Source", async ({page}) => {
  await openNote(page, 'focus-editor-size.md', '# Size\n\nA paragraph to read.\n');
  await page.locator('#mode').click();
  const prose = page.locator('#editor'),
    size = () => prose.evaluate(element => parseFloat(getComputedStyle(element).fontSize));
  const sizes = {};
  for (const format of ['visual', 'source']) {
    await page.locator('#format-' + format).click();
    sizes[format] = await size();
  }
  await page.keyboard.press(shortcut);
  await expectFocus(page, true);
  await revealToolbar(page);
  await page.locator('#focus-text-size').fill('150');
  for (const format of ['source', 'visual']) {
    await page.evaluate(format => document.getElementById('format-' + format).click(), format);
    await expect.poll(size).toBeCloseTo(sizes[format] * 1.5, 1);
    // CodeMirror measures the larger lines: the caret goes where clicked.
    await prose
      .locator('.cm-line')
      .filter({hasText: 'A paragraph'})
      .click({position: {x: 2, y: 4}});
    await page.keyboard.type('X');
    await expectEditorText(page, /\nX+A paragraph to read\.\n$/);
  }
  await page.keyboard.press('Escape');
  await expectFocus(page, false);
  expect(await size()).toBeCloseTo(sizes.visual, 1);
});

test('the toolbar counts words written this session and opens writing tools', async ({page}) => {
  await openNote(page, 'focus-words.md', '# Focus\n\nOne two three four.\n');
  await edit(page);
  await page.keyboard.press(shortcut);
  await expectFocus(page, true);
  await expect(page.locator('#focus-word-total')).toHaveText(/^\d+ words$/);
  await expect(page.locator('#focus-word-session')).toBeHidden();
  const before = Number((await page.locator('#focus-word-total').textContent()).split(' ')[0]);
  await page.locator('#editor').press(documentEnd);
  await page.locator('#editor').pressSequentially(' Five six seven.');
  await expect(page.locator('#focus-word-total')).toHaveText(before + 3 + ' words');
  await expect(page.locator('#focus-word-session')).toHaveText('+3');
  await expect(page.locator('#focus-words')).toHaveAccessibleName(
    before + 3 + ' words, 3 added this session · open writing tools'
  );

  await revealToolbar(page);
  await page.locator('#focus-words').click();
  await expect(page.locator('#writing-panel')).toBeVisible();
});

test('focus mode closes chat, keeps typing intact and leaves only after open panels close', async ({page}) => {
  await openNote(page, 'focus-keys.md');
  await page.locator('#chat-open').click();
  await expect(page.locator('#chat-panel')).toBeVisible();
  await page.keyboard.press(shortcut);
  await expectFocus(page, true);
  await expect(page.locator('#chat-panel')).toBeHidden();
  await expect(page.locator('#focus-toggle')).toBeFocused();
  await page.keyboard.press(shortcut);
  await expectFocus(page, false);

  await edit(page);
  const before = await editorText(page);
  await page.locator('#editor').press(shortcut);
  await expectFocus(page, true);
  await expect(page.locator('#editor')).toBeFocused();
  await page.locator('#editor').press(shortcut);
  await expectFocus(page, false);
  await page.locator('#editor').press(shortcut);
  await expectFocus(page, true);
  await expectEditorText(page, before);
  await expect(page.locator('#editor')).toBeFocused();

  // Escape closes the writing panel and note context first; focus mode stays until the next one.
  await revealToolbar(page);
  await page.locator('#focus-words').click();
  await expect(page.locator('#writing-panel')).toBeVisible();
  await page.keyboard.press('Escape');
  await expect(page.locator('#writing-panel')).toBeHidden();
  await expectFocus(page, true);
  await revealToolbar(page);
  await page.locator('#focus-context').click();
  await expect(page.locator('#comments-panel')).toBeVisible();
  await page.keyboard.press('Escape');
  await expect(page.locator('#comments-panel')).toBeHidden();
  await expectFocus(page, true);
  await page.locator('#editor').press('Escape');
  await expectFocus(page, false);
  await expect(page.locator('#editor')).toBeFocused();
});

test('Go to searches without leaving focus mode for good', async ({page}) => {
  await openNote(page, 'focus-goto.md');
  await page.keyboard.press(shortcut);
  await expectFocus(page, true);
  await revealToolbar(page);
  await page.locator('#focus-goto').click();
  await expect(page.locator('#search-view')).toBeVisible();
  await expect(page.locator('#search')).toBeFocused();
  await expect(page.locator('body')).not.toHaveClass(/\bfocus-mode\b/);
  await page.locator('#search-close').click();
  await expectFocus(page, true);
});

test('an edit in focus mode saves a new committed version', async ({page}) => {
  const path = 'focus-save.md',
    note = await openNote(page, path);
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await edit(page);
  await page.keyboard.press(shortcut);
  await expectFocus(page, true);
  await page.locator('#editor').press(documentEnd);
  await page.locator('#editor').pressSequentially('Written in focus.');
  await expect(page.locator('#save-state')).toBeVisible();
  await revealToolbar(page);
  await expect(page.locator('#save')).toBeEnabled();
  await page.locator('#save').click();
  await expect(page.locator('#save-state')).toHaveText('Saved');
  const saved = await api('/p/default/api/note?path=' + path);
  expect(saved.content).toBe(note.content + 'Written in focus.');
  expect(saved.revision).not.toBe(note.revision);
  await expectFocus(page, true);
});

test('a stale save in focus mode is refused visibly, keeps the draft and holds the toolbar open', async ({page}) => {
  const path = 'focus-conflict.md';
  // Without live editing, Save sends the revision it was based on.
  await page.route('**/api/live?*', route => route.fulfill({status: 503, json: {error: 'Synthetic live outage'}}));
  const note = await openNote(page, path);
  await edit(page);
  await page.locator('#focus-toggle').click();
  await expectFocus(page, true);
  await expect(page.locator('#editor')).toBeFocused();
  await page.locator('#editor').press(documentEnd);
  await page.locator('#editor').pressSequentially('Focused draft.');
  await expect(toolbar(page)).not.toHaveAttribute('data-attention');
  const peer = await api('/p/default/api/note', {path, content: '# Focus\n\nPeer version.\n', revision: note.revision});
  const request = page.waitForRequest(
    request => request.method() === 'POST' && new URL(request.url()).pathname.endsWith('/api/note')
  );
  await revealToolbar(page);
  await page.locator('#save').click();
  expect((await request).postDataJSON()).toMatchObject({path, revision: note.revision});
  await expect(page.locator('#save-state')).toContainText('Not saved');
  await expect(page.locator('#notice')).toBeVisible();
  await expect(page.locator('#notice')).toBeInViewport();
  await expect(page.locator('#draft-conflict')).toBeVisible();
  // The warning stays on screen while the person moves away or keeps typing.
  await expect(toolbar(page)).toHaveAttribute('data-attention', '');
  await leaveToolbar(page);
  await page.locator('#editor').pressSequentially(' More.');
  await expect(toolbar(page)).toHaveAttribute('data-attention', '');
  await expect(page.locator('#save-state')).toBeInViewport();
  await expect(page.locator('#save')).toBeInViewport();
  await expectFocus(page, true);
  expect((await api('/p/default/api/note?path=' + path)).content).toBe(peer.content);
  await expect.poll(() => retainedTexts(page, path)).toContain(note.content + 'Focused draft. More.');
});

test('focus mode on a phone keeps Edit, Save and exit at the top', async ({page}) => {
  await page.setViewportSize({width: 390, height: 844});
  await openNote(page, 'focus-phone.md');
  await expect(page.locator('#reader-actions')).toBeVisible();
  await page.locator('#focus-toggle').click();
  await expect(page.locator('#reader-actions')).toBeHidden();
  await expect(page.locator('#mobile-navigation')).toBeHidden();
  await expect(page.locator('#menu')).toBeHidden();
  await expect(page.locator('#focus-controls')).toBeHidden();
  await expectFocus(page, true);
  await expect(toolbar(page)).toBeInViewport();
  expect((await toolbar(page).boundingBox()).y).toBe(0);
  await page.locator('#focus-toggle').click();
  await expectFocus(page, false);

  await edit(page);
  await page.locator('#focus-toggle').click();
  await expectFocus(page, true);
  await page.locator('#editor').press(documentEnd);
  await page.locator('#editor').pressSequentially('Phone focus.');
  await expect(page.locator('#save')).toBeInViewport();
  await expect(page.locator('#save-state')).toBeInViewport();
  await expect(page.locator('#focus-toggle')).toBeInViewport();
  await page.locator('#focus-toggle').click();
  await expectFocus(page, false);
});
