import {test, expect} from '@playwright/test';
import {startApp} from './app-fixture.mjs';
import {
  caretAtLineEnd,
  countLiveUpdates,
  editorLine,
  editorText,
  expectEditorText,
  selectSource
} from './editor-text.mjs';

// The formatting toolbar, its commands and keys, Enter and Backspace on list
// markup and the task boxes: every one changes exactly the text it names, as
// one undo step, in every mode, and nothing in a read-only note.

let app, cookies;
test.beforeAll(async ({browser}) => {
  app = await startApp({
    name: 'forgetmenote-commands-',
    account: {username: 'commands-admin', password: 'DisposableCommands123!'}
  });
  // One sign-in for every case: password sign-ins are rate limited.
  const context = await browser.newContext();
  try {
    await app.login(await context.newPage(), 'password');
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
test.afterEach(async ({}, info) => {
  if (info.status !== info.expectedStatus) await info.attach('app-log', {body: app.output, contentType: 'text/plain'});
});

async function openEditor(page, path, content, format = 'visual') {
  await app.api('/p/default/api/note', {path, content, revision: ''});
  await page.goto(app.origin + '/?project=default&note=' + encodeURIComponent(path));
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await page.locator('#mode').click();
  await page.locator('#format-' + format).click();
  await expect(page.locator('#editing')).toHaveAttribute('data-format', format);
  await expect(page.locator('#editor')).toBeVisible();
}
const control = (page, name) => page.getByRole('button', {name, exact: true});
const undo = page => page.keyboard.press('Control+z');
const noBareLF = text => !/(^|[^\r])\n/.test(text);

test('the formatting toolbar and its Insert menu are in every mode, and looking around sends nothing', async ({
  page
}) => {
  await page.setViewportSize({width: 1440, height: 900});
  const content = '# Toolbar\n\nSome **bold** words.\n\n- item\n';
  const sent = countLiveUpdates(page);
  await openEditor(page, 'commands/toolbar.md', content);
  await expect(page.locator('#source-insert')).toHaveCount(0);
  for (const format of ['visual', 'source', 'split']) {
    await page.locator('#format-' + format).click();
    await expect(page.locator('#editing')).toHaveAttribute('data-format', format);
    const bar = page.getByRole('group', {name: 'Formatting toolbar', exact: true});
    for (const name of [
      'Undo',
      'Redo',
      'Bold',
      'Italic',
      'Strikethrough',
      'Inline code',
      'Link',
      'Bulleted list',
      'Numbered list',
      'Task list'
    ])
      await expect(bar.getByRole('button', {name, exact: true})).toBeVisible();
    await expect(bar.getByRole('combobox', {name: 'Text style', exact: true})).toBeVisible();
    const insert = bar.getByRole('combobox', {name: 'Insert block', exact: true});
    await expect(insert).toBeVisible();
    expect(await insert.locator('option').allTextContents()).toEqual([
      'Insert',
      'Image or file…',
      'Video embed…',
      'Table',
      'Quote',
      'Code block',
      'Divider'
    ]);
    await expect(control(page, 'Bold')).toHaveAttribute('aria-keyshortcuts', 'Control+B');
    await expect(control(page, 'Strikethrough')).toHaveAttribute('aria-keyshortcuts', 'Alt+Shift+5');
    await expect(control(page, 'Inline code')).toHaveAttribute('aria-keyshortcuts', 'Control+`');
  }
  // Moving the caret only updates what the toolbar shows.
  await page.locator('#format-visual').click();
  await caretAtLineEnd(page, 'Some');
  await page.keyboard.press('Home');
  for (let i = 0; i < 7; i++) await page.keyboard.press('ArrowRight');
  await expect(control(page, 'Bold')).toHaveAttribute('aria-pressed', 'true');
  await page.keyboard.press('ArrowDown');
  await page.keyboard.press('ArrowDown');
  await expect(control(page, 'Bulleted list')).toHaveAttribute('aria-pressed', 'true');
  await expect(control(page, 'Bold')).toHaveAttribute('aria-pressed', 'false');
  await page.waitForTimeout(500);
  await expectEditorText(page, content);
  expect(sent).toEqual([]);
});

test('formatting keys and buttons wrap the selection as one undo step that restores the bytes', async ({page}) => {
  const content = 'Alpha beta gamma delta.\n';
  await openEditor(page, 'commands/keys.md', content, 'source');
  // A command right after typing is an undo step of its own.
  await selectSource(page, 23);
  await page.keyboard.type(' Z');
  const typed = 'Alpha beta gamma delta. Z\n';
  await expectEditorText(page, typed);
  await selectSource(page, 6, 10);
  await page.keyboard.press('Control+b');
  await expectEditorText(page, 'Alpha **beta** gamma delta. Z\n');
  await expect(control(page, 'Bold')).toHaveAttribute('aria-pressed', 'true');
  await undo(page);
  await expectEditorText(page, typed);
  await undo(page);
  await expectEditorText(page, content);
  // Each kind of inline formatting, by key or button.
  await selectSource(page, 11, 16);
  await control(page, 'Italic').click();
  await expectEditorText(page, 'Alpha beta *gamma* delta.\n');
  await selectSource(page, 19, 24);
  await page.keyboard.press('Alt+Shift+5');
  await expectEditorText(page, 'Alpha beta *gamma* ~~delta~~.\n');
  await selectSource(page, 0, 5);
  await page.keyboard.press('Control+Backquote');
  await expectEditorText(page, '`Alpha` beta *gamma* ~~delta~~.\n');
  await selectSource(page, 8, 12);
  await page.keyboard.press('Control+i');
  await expectEditorText(page, '`Alpha` *beta* *gamma* ~~delta~~.\n');
  // Unwrapping restores the text exactly.
  await page.keyboard.press('Control+i');
  await expectEditorText(page, '`Alpha` beta *gamma* ~~delta~~.\n');
  // Link puts the caret in the address; Mod-K never opens search from the note.
  await selectSource(page, 8, 12);
  await page.keyboard.press('Control+k');
  await expect(page.locator('body')).not.toHaveClass(/search-active/);
  await page.keyboard.type('https://e.com');
  await expectEditorText(page, '`Alpha` [beta](https://e.com) *gamma* ~~delta~~.\n');
  await undo(page);
  await expectEditorText(page, '`Alpha` [beta]() *gamma* ~~delta~~.\n');
  // Six commands, six undo steps.
  for (let i = 0; i < 6; i++) await undo(page);
  await expectEditorText(page, content);
  // Ctrl+F still opens the note's own search.
  await page.keyboard.press('Control+f');
  await expect(page.locator('#editing .cm-search')).toBeVisible();
  await page.keyboard.press('Escape');
  await expect(page.locator('#editing .cm-search')).toHaveCount(0);
  await page.locator('#editor').focus();
  // Where formatting would be literal text, nothing changes.
  await page.keyboard.press('Control+a');
  await page.keyboard.type('Run `ls -l` now.');
  await selectSource(page, 6, 8);
  await page.keyboard.press('Control+b');
  await control(page, 'Italic').click();
  await expectEditorText(page, 'Run `ls -l` now.');
});

test('in Visual the toolbar shows the caret’s heading, marks and list, and unwraps where the caret is', async ({
  page
}) => {
  const content = '## Heading\n\nSome **bold** text.\n\n- item one\n- [ ] a task\n';
  await openEditor(page, 'commands/visual.md', content);
  await caretAtLineEnd(page, 'Heading');
  await expect(page.getByRole('combobox', {name: 'Text style', exact: true})).toHaveValue('2');
  await page.getByRole('combobox', {name: 'Text style', exact: true}).selectOption({label: 'Heading 3'});
  await expectEditorText(page, content.replace('## Heading', '### Heading'));
  await undo(page);
  await expectEditorText(page, content);
  await page.locator('#editor .cm-lp-strong').click();
  await expect(control(page, 'Bold')).toHaveAttribute('aria-pressed', 'true');
  await expect(page.getByRole('combobox', {name: 'Text style', exact: true})).toHaveValue('0');
  await control(page, 'Bold').click();
  await expectEditorText(page, content.replace('**bold**', 'bold'));
  await expect(control(page, 'Bold')).toHaveAttribute('aria-pressed', 'false');
  await undo(page);
  await expectEditorText(page, content);
  await caretAtLineEnd(page, 'item one');
  await expect(control(page, 'Bulleted list')).toHaveAttribute('aria-pressed', 'true');
  await control(page, 'Numbered list').click();
  await expectEditorText(page, content.replace('- item one', '1. item one'));
  await caretAtLineEnd(page, 'a task');
  await expect(control(page, 'Task list')).toHaveAttribute('aria-pressed', 'true');
  await control(page, 'Task list').click();
  await expectEditorText(page, content.replace('- item one', '1. item one').replace('- [ ] a task', 'a task'));
});

test('formatting a whole heading or task line wraps its text and keeps its # and its box', async ({page}) => {
  const content = '# Title here\n\n- [ ] a task\n';
  await openEditor(page, 'commands/whole-lines.md', content);
  // Visual: the caret at the heading's end, Shift+Home, then Bold.
  await caretAtLineEnd(page, 'Title here');
  await page.keyboard.press('Shift+Home');
  await page.keyboard.press('Control+b');
  await expectEditorText(page, '# **Title here**\n\n- [ ] a task\n');
  await expect(editorLine(page, 'Title here')).toHaveClass(/cm-lp-h1/);
  await undo(page);
  await expectEditorText(page, content);
  // Select all: each block's text, not its markup.
  await page.keyboard.press('Control+a');
  await page.keyboard.press('Control+b');
  await expectEditorText(page, '# **Title here**\n\n- [ ] **a task**\n');
  // With the caret off the task line, its box shows again.
  await page.keyboard.press('Control+Home');
  await expect(page.locator('#editor input.cm-lp-task')).toHaveCount(1);
  await undo(page);
  await expectEditorText(page, content);
  // Source: a whole task line, then Code and Link over the heading line.
  await page.locator('#format-source').click();
  const task = content.indexOf('- [ ]');
  await selectSource(page, task, task + '- [ ] a task'.length);
  await page.keyboard.press('Control+b');
  await expectEditorText(page, '# Title here\n\n- [ ] **a task**\n');
  await undo(page);
  await selectSource(page, 0, '# Title here'.length);
  await page.keyboard.press('Control+Backquote');
  await expectEditorText(page, '# `Title here`\n\n- [ ] a task\n');
  await undo(page);
  await selectSource(page, 0, '# Title here'.length);
  await page.keyboard.press('Control+k');
  await expectEditorText(page, '# [Title here]()\n\n- [ ] a task\n');
  await undo(page);
  await expectEditorText(page, content);
});

test('Text style, lists and Insert change only the lines the selection touches', async ({page}) => {
  const content = 'Intro.\n\nFirst line\nSecond line\n\nThird\n\nOutro.\n';
  await openEditor(page, 'commands/lines.md', content, 'source');
  const style = page.getByRole('combobox', {name: 'Text style', exact: true}),
    insert = page.getByRole('combobox', {name: 'Insert block', exact: true});
  const lines = [8, 30];
  const cases = [
    [() => style.selectOption({label: 'Heading 2'}), lines, '## First line\n## Second line'],
    [() => control(page, 'Bulleted list').click(), lines, '- First line\n- Second line'],
    [() => control(page, 'Numbered list').click(), lines, '1. First line\n2. Second line'],
    [() => control(page, 'Task list').click(), lines, '- [ ] First line\n- [ ] Second line'],
    [() => insert.selectOption({label: 'Quote'}), lines, '> First line\n> Second line'],
    [() => insert.selectOption({label: 'Code block'}), lines, '```\nFirst line\nSecond line\n```'],
    [
      () => insert.selectOption({label: 'Table'}),
      [30, 30],
      'First line\nSecond line\n\n| Column 1 | Column 2 | Column 3 |\n| --- | --- | --- |\n|  |  |  |\n|  |  |  |'
    ],
    [() => insert.selectOption({label: 'Divider'}), [30, 30], 'First line\nSecond line\n\n---']
  ];
  for (const [command, [from, to], replaced] of cases) {
    await selectSource(page, from, to);
    await command();
    await expectEditorText(page, content.replace('First line\nSecond line', replaced));
    await expect(insert).toHaveValue('');
    await page.locator('#editor').focus();
    await undo(page);
    await expectEditorText(page, content);
  }
  // A heading prefix cannot go inside code.
  await page.keyboard.press('Control+a');
  await page.keyboard.type('```\ncode\n```');
  await selectSource(page, 5);
  await expect(style).toBeDisabled();
  await control(page, 'Bulleted list').click();
  await expectEditorText(page, '```\ncode\n```');
});

for (const format of ['visual', 'source'])
  test(`Enter and Backspace continue and remove list, task and quote markup on the caret line only in ${format}`, async ({
    page
  }) => {
    const content = '1. one\n2. two\n3. three\n\n- [x] done\n\n> quoted\n';
    await openEditor(page, 'commands/enter-' + format + '.md', content, format);
    await caretAtLineEnd(page, 'one');
    await page.keyboard.press('Enter');
    await page.keyboard.type('new');
    // The new item takes the next number; the items after it keep theirs.
    let expected = '1. one\n2. new\n2. two\n3. three\n\n- [x] done\n\n> quoted\n';
    await expectEditorText(page, expected);
    await caretAtLineEnd(page, 'done');
    await page.keyboard.press('Enter');
    expected = expected.replace('- [x] done\n', '- [x] done\n- [ ] \n');
    await expectEditorText(page, expected);
    // Enter on the empty item ends the list.
    await page.keyboard.press('Enter');
    await page.keyboard.type('after');
    expected = expected.replace('- [x] done\n- [ ] \n', '- [x] done\nafter\n');
    await expectEditorText(page, expected);
    await caretAtLineEnd(page, 'quoted');
    await page.keyboard.press('Enter');
    await expectEditorText(page, expected.replace('> quoted\n', '> quoted\n> \n'));
    // Backspace right after the markup removes it, not one invisible byte.
    await page.keyboard.press('Backspace');
    await page.keyboard.type('plain');
    await expectEditorText(page, expected.replace('> quoted\n', '> quoted\nplain\n'));
    // Elsewhere Enter is a plain line break.
    await caretAtLineEnd(page, 'plain');
    await page.keyboard.press('Enter');
    await expectEditorText(page, expected.replace('> quoted\n', '> quoted\nplain\n\n'));
  });

test('Backspace in Visual right after a task box or a list marker removes only that markup', async ({page}) => {
  const content = '- [ ] alpha\n- beta\n';
  await openEditor(page, 'commands/backspace-visual.md', content);
  await caretAtLineEnd(page, 'alpha');
  for (let i = 0; i < 'alpha'.length; i++) await page.keyboard.press('ArrowLeft');
  await page.keyboard.press('Backspace');
  await expectEditorText(page, '- alpha\n- beta\n');
  await caretAtLineEnd(page, 'beta');
  for (let i = 0; i < 'beta'.length; i++) await page.keyboard.press('ArrowLeft');
  await page.keyboard.press('Backspace');
  await expectEditorText(page, '- alpha\nbeta\n');
});

test('a CRLF note keeps CRLF line ends through Enter, formatting and Insert, saved as typed', async ({page}) => {
  const path = 'commands/crlf.md',
    content = '# Title\r\n\r\n- one\r\n- two\r\n\r\nWords here.\r\n';
  await openEditor(page, path, content, 'source');
  // The caret goes before each line's \r, as a click there puts it.
  await selectSource(page, content.indexOf('- one') + 5);
  await page.keyboard.press('Enter');
  await page.keyboard.type('mid');
  let expected = content.replace('- one\r\n', '- one\r\n- mid\r\n');
  await expectEditorText(page, expected);
  await selectSource(page, expected.indexOf('Words here.') + 11);
  await page.getByRole('combobox', {name: 'Insert block', exact: true}).selectOption({label: 'Divider'});
  expected = expected.replace('Words here.\r\n', 'Words here.\r\n\r\n---\r\n');
  await expectEditorText(page, expected);
  const start = expected.indexOf('Words');
  await selectSource(page, start, start + 5);
  await page.keyboard.press('Control+b');
  expected = expected.replace('Words', '**Words**');
  await expectEditorText(page, expected);
  await selectSource(page, 7);
  await page.getByRole('combobox', {name: 'Text style', exact: true}).selectOption({label: 'Paragraph'});
  expected = expected.replace('# Title', 'Title');
  await expectEditorText(page, expected);
  expect(noBareLF(await editorText(page))).toBe(true);
  // Ctrl+S still saves from inside the note.
  await page.keyboard.press('Control+s');
  await expect(page.locator('#save-state')).toContainText('Saved');
  expect((await app.api('/p/default/api/note?path=' + path)).content).toBe(expected);
});

test('a task box changes one character where the task is now, as one undo step, and is disabled while read-only', async ({
  page
}) => {
  const path = 'commands/tasks.md',
    content = '# Tasks\n\n- [ ] first\n- [x] second\n';
  await openEditor(page, path, content);
  const boxes = page.locator('#editor input.cm-lp-task');
  await expect(boxes).toHaveCount(2);
  await boxes.nth(0).click();
  await expectEditorText(page, content.replace('- [ ] first', '- [x] first'));
  // An agent's edit moves the tasks; the box changes the task where it is now.
  const base = await app.api('/p/default/api/live?path=' + encodeURIComponent(path));
  const at = base.content.indexOf('Tasks');
  await app.api('/p/default/api/live/patch', {
    path,
    base: base.liveRevision,
    operationId: 'commands-agent',
    edits: [{start: at, end: at + 5, old: 'Tasks', text: 'Tasks for today'}]
  });
  const moved = content.replace('# Tasks', '# Tasks for today').replace('- [ ] first', '- [x] first');
  await expectEditorText(page, moved);
  await boxes.nth(1).click();
  await expectEditorText(page, moved.replace('- [x] second', '- [ ] second'));
  // The click is an undo step of its own; the agent's edit is not this user's.
  await page.locator('#editor').focus();
  await undo(page);
  await expectEditorText(page, moved);
  await undo(page);
  await expectEditorText(page, content.replace('# Tasks', '# Tasks for today'));
  // While an upload holds the note read-only, nothing changes it.
  await page.route('**/api/upload', () => {});
  await page.locator('#upload').setInputFiles({name: 'held.txt', mimeType: 'text/plain', buffer: Buffer.from('held')});
  await expect(boxes.nth(0)).toBeDisabled();
  await expect(control(page, 'Bold')).toBeDisabled();
  await expect(page.getByRole('combobox', {name: 'Insert block', exact: true})).toBeDisabled();
  await boxes.nth(0).click({force: true});
  await page.locator('#editor').focus();
  await page.keyboard.press('Control+a');
  await page.keyboard.press('Control+b');
  await page.keyboard.press('Enter');
  await page.waitForTimeout(300);
  await expectEditorText(page, content.replace('# Tasks', '# Tasks for today'));
});

for (const format of ['visual', 'source'])
  test(`a pasted URL and typed HTML go in exactly as given in ${format}`, async ({page}) => {
    const content = 'Read the site today.\n';
    await openEditor(page, 'commands/paste-' + format + '.md', content, format);
    await caretAtLineEnd(page, 'today.');
    // Select "site" (offsets 9–13) with the keyboard, which works in every mode.
    await page.keyboard.press('Home');
    for (let i = 0; i < 9; i++) await page.keyboard.press('ArrowRight');
    for (let i = 0; i < 4; i++) await page.keyboard.press('Shift+ArrowRight');
    await page.locator('#editor').evaluate(editor => {
      const data = new DataTransfer();
      data.setData('text/plain', 'https://example.com');
      editor.dispatchEvent(new ClipboardEvent('paste', {clipboardData: data, bubbles: true, cancelable: true}));
    });
    await expectEditorText(page, 'Read the https://example.com today.\n');
    // In an HTML block, ">" and "</" close no tag.
    await page.keyboard.press('End');
    await page.keyboard.press('Enter');
    await page.keyboard.press('Enter');
    await page.keyboard.type('<div>');
    await expectEditorText(page, 'Read the https://example.com today.\n\n<div>\n');
    await page.keyboard.type('x</');
    await expectEditorText(page, 'Read the https://example.com today.\n\n<div>x</\n');
    await expect(editorLine(page, 'today')).toBeVisible();
  });

// Paste inserts only the clipboard's plain text, never its HTML, and a
// clipboard holding text pastes it even when it also holds an image, as
// spreadsheet and Office copies do. Files go through Insert, and dropping
// never inserts or moves anything: refused, they change nothing and send
// nothing.
for (const format of ['visual', 'source'])
  test(`paste inserts only plain text, and drops and pasted files change nothing, in ${format}`, async ({page}) => {
    const content = 'One two three.\n';
    const path = 'commands/transfer-' + format + '.md';
    await openEditor(page, path, content, format);
    const sent = countLiveUpdates(page);
    const hint = page.locator('#editor-hint');
    const transfer = (type, kinds) =>
      editorLine(page, 'One two three.').evaluate(
        (line, {type, kinds}) => {
          const data = new DataTransfer();
          if (kinds.includes('text')) data.setData('text/plain', kinds.includes('html') ? 'a\tb\n1\t2' : 'DROPPED');
          if (kinds.includes('html')) data.setData('text/html', '<table><tr><td><b>a</b></td><td>b</td></tr></table>');
          if (kinds.includes('image'))
            data.items.add(
              new File([new Uint8Array([137, 80, 78, 71, 13, 10, 26, 10])], 'image.png', {type: 'image/png'})
            );
          if (kinds.includes('file')) data.items.add(new File(['bytes'], 'dropped.txt', {type: 'text/plain'}));
          const box = line.getBoundingClientRect();
          const options = {bubbles: true, cancelable: true};
          if (type === 'paste')
            document
              .getElementById('editor')
              .dispatchEvent(new ClipboardEvent('paste', {...options, clipboardData: data}));
          // On the text, where an accepted drop would insert.
          else
            line.dispatchEvent(
              new DragEvent('drop', {
                ...options,
                dataTransfer: data,
                clientX: box.left + 20,
                clientY: box.top + box.height / 2
              })
            );
        },
        {type, kinds}
      );
    await caretAtLineEnd(page, 'One two three.');
    await transfer('paste', ['text', 'html', 'image']);
    let text = 'One two three.a\tb\n1\t2\n';
    await expectEditorText(page, text);
    await expect(hint).toBeHidden();
    await page.locator('#editor').evaluate(editor => {
      const data = new DataTransfer();
      data.setData('text/plain', 'plain');
      data.setData('text/html', '<b>rich</b>');
      editor.dispatchEvent(new ClipboardEvent('paste', {clipboardData: data, bubbles: true, cancelable: true}));
    });
    text = 'One two three.a\tb\n1\t2plain\n';
    await expectEditorText(page, text);
    await expect
      .poll(async () => (await app.api('/p/default/api/live?path=' + encodeURIComponent(path))).content)
      .toBe(text);
    const shared = sent.length;
    for (const [type, kinds, message] of [
      ['drop', ['text'], 'Use Insert → Image or file to add files, or cut and paste to move text.'],
      ['paste', ['image'], 'Use Insert → Image or file to add files to this project.'],
      ['drop', ['file'], 'Use Insert → Image or file to add files, or cut and paste to move text.']
    ]) {
      await transfer(type, kinds);
      await expect(hint).toHaveText(message);
      await expectEditorText(page, text);
    }
    // A refused drop of a file would read it asynchronously.
    await page.waitForTimeout(500);
    await expectEditorText(page, text);
    expect(sent.length).toBe(shared);
  });

test('Insert adds an image or a video as a block of its own after the caret’s line, as one undo step', async ({
  page
}) => {
  const content = 'Intro.\n\nOutro.\n';
  await openEditor(page, 'commands/media.md', content, 'source');
  await selectSource(page, 6);
  await page.getByRole('combobox', {name: 'Insert block', exact: true}).selectOption({label: 'Video embed…'});
  await expect(page.locator('#video-dialog')).toBeVisible();
  await page.locator('#video-url').fill('https://www.youtube.com/watch?v=dQw4w9WgXcQ');
  await page.locator('#video-url').press('Enter');
  const video = '[Video](<https://www.youtube.com/watch?v=dQw4w9WgXcQ> "video")';
  await expectEditorText(page, 'Intro.\n\n' + video + '\n\nOutro.\n');
  await page.locator('#editor').focus();
  await undo(page);
  await expectEditorText(page, content);
  await selectSource(page, 6);
  const png = Buffer.from(
    'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==',
    'base64'
  );
  await page.locator('#upload').setInputFiles({name: 'dot.png', mimeType: 'image/png', buffer: png});
  // The live session may report its sync before this check reads the state.
  await expectEditorText(page, /^Intro\.\n\n!\[dot\.png\]\(<\/[^>]*dot\.png>\)\n\nOutro\.\n$/);
  await page.locator('#editor').focus();
  await undo(page);
  await expectEditorText(page, content);
  // In code (or a table or front matter) Insert offers no block, and a
  // chosen one changes nothing: its Markdown would be code there.
  await page.keyboard.press('Control+a');
  await page.keyboard.type('```\ncode\n```\n\nafter');
  await selectSource(page, 5);
  const insert = page.getByRole('combobox', {name: 'Insert block', exact: true});
  for (const label of ['Image or file…', 'Video embed…', 'Table', 'Divider'])
    await expect(insert.locator('option', {hasText: label})).toBeDisabled();
  await expect(insert.locator('option', {hasText: 'Quote'})).toBeEnabled();
  for (const value of ['video', 'attachment', 'table', 'divider'])
    await insert.evaluate((element, value) => {
      element.value = value;
      element.dispatchEvent(new Event('change'));
    }, value);
  await expect(page.locator('#video-dialog')).toBeHidden();
  await expectEditorText(page, '```\ncode\n```\n\nafter');
  // Out of the code block, they are offered again.
  await page.keyboard.press('Control+End');
  await expect(insert.locator('option', {hasText: 'Video embed…'})).toBeEnabled();
});

test('no command or task box runs inside an open IME composition', async ({page, context}) => {
  const content = 'Some text.\n\n- [ ] task\n';
  await openEditor(page, 'commands/ime.md', content);
  await caretAtLineEnd(page, 'Some');
  const cdp = await context.newCDPSession(page);
  await cdp.send('Input.imeSetComposition', {text: 'に', selectionStart: 1, selectionEnd: 1});
  await control(page, 'Bold').click();
  await control(page, 'Bulleted list').click();
  await page.locator('#editor input.cm-lp-task').click();
  await cdp.send('Input.insertText', {text: '日本'});
  await expectEditorText(page, 'Some text.日本\n\n- [ ] task\n');
});

// An undo step's offsets are the live note's, which does not hold an open
// composition's text yet: run inside it, Undo and Redo changed other
// characters in the editor than in the note, and the composition never
// ended. They wait for it, as every command does.
for (const format of ['visual', 'source'])
  for (const how of ['toolbar', 'keyboard'])
    test(`Undo and Redo do nothing inside an open IME composition in a live note (${format}, ${how})`, async ({
      page,
      context
    }) => {
      const path = 'commands/ime-undo-' + format + '-' + how + '.md';
      const warnings = [];
      page.on('console', message => {
        if (message.type() === 'warning') warnings.push(message.text());
      });
      const expectShared = async text => {
        await expectEditorText(page, text);
        await expect
          .poll(async () => (await app.api('/p/default/api/live?path=' + encodeURIComponent(path))).content)
          .toBe(text);
      };
      const history = async redo => {
        if (how === 'toolbar') await control(page, redo ? 'Redo' : 'Undo').click();
        else await page.keyboard.press(redo ? 'Control+y' : 'Control+z');
      };
      await openEditor(page, path, 'Hello world.\n', format);
      await caretAtLineEnd(page, 'Hello world.');
      await page.keyboard.type(' Added');
      await expectShared('Hello world. Added\n');
      const cdp = await context.newCDPSession(page);
      await page.keyboard.press('Control+Home');
      await cdp.send('Input.imeSetComposition', {text: 'に', selectionStart: 1, selectionEnd: 1});
      await history(false);
      await cdp.send('Input.insertText', {text: '日本'});
      await expectShared('日本Hello world. Added\n');
      // Outside a composition Undo works, and leaves a step to redo.
      await undo(page);
      const undone = await editorText(page);
      expect(undone).not.toBe('日本Hello world. Added\n');
      await expectShared(undone);
      await page.keyboard.press('Control+Home');
      await cdp.send('Input.imeSetComposition', {text: 'ご', selectionStart: 1, selectionEnd: 1});
      await history(true);
      await cdp.send('Input.insertText', {text: '語'});
      await expectShared('語' + undone);
      // The composition ended: the next key reaches the shared note too.
      await page.keyboard.press('Control+End');
      await page.keyboard.type('X');
      await expectShared('語' + undone + 'X');
      expect(warnings.filter(text => /typing may be lost/.test(text))).toEqual([]);
    });

// The browser never changes what is typed: no mode has its autocorrect,
// auto-capitalization or writing suggestions. Spellcheck only underlines.
// typed-text.spec.mjs checks the other fields where note or comment text is
// typed.
test('no mode lets the browser autocorrect, capitalize or suggest, and spellcheck stays on', async ({page}) => {
  await page.setViewportSize({width: 1440, height: 900});
  await openEditor(page, 'commands/autocorrect.md', 'Some prose.\n');
  const attributes = {autocorrect: 'off', autocapitalize: 'off', writingsuggestions: 'false', spellcheck: 'true'};
  for (const format of ['visual', 'source', 'split', 'visual']) {
    await page.locator('#format-' + format).click();
    await expect(page.locator('#editing')).toHaveAttribute('data-format', format);
    for (const [name, value] of Object.entries(attributes))
      await expect(page.locator('#editor')).toHaveAttribute(name, value);
  }
});

test.describe('on Android', () => {
  test.use({
    userAgent:
      'Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Mobile Safari/537.36'
  });
  // With autocorrect off, CodeMirror turns the keyboard's period back into
  // the space typed.
  for (const format of ['visual', 'source'])
    test(`a double space stays two spaces in ${format}`, async ({page}) => {
      await openEditor(page, `commands/android-period-${format}.md`, 'Hello world\n', format);
      await caretAtLineEnd(page, 'Hello world');
      await page.keyboard.type(' ');
      await expectEditorText(page, 'Hello world \n');
      // The keyboard replaces the space it typed with a period and a space.
      await editorLine(page, 'Hello world').evaluate(line => {
        const walker = document.createTreeWalker(line, NodeFilter.SHOW_TEXT);
        let last = null;
        for (let node = walker.nextNode(); node; node = walker.nextNode()) if (node.data.length) last = node;
        last.data = last.data.replace(/ $/, '. ');
        getSelection().collapse(last, last.data.length);
      });
      await expectEditorText(page, 'Hello world  \n');
    });
});
