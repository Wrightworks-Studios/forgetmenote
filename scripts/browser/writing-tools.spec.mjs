import {test, expect} from '@playwright/test';
import {startApp} from './app-fixture.mjs';
import {readFileSync} from 'node:fs';
import {expectEditorText, expectVisual, fillSource} from './editor-text.mjs';

let app, cookies;
test.beforeAll(async ({browser}) => {
  app = await startApp({
    name: 'forgetmenote-writing-',
    account: {username: 'writing-admin', password: 'DisposableWriting123!'}
  });
  const context = await browser.newContext();
  try {
    await app.login(await context.newPage(), 'password');
    cookies = await context.cookies();
  } finally {
    await context.close();
  }
});
test.afterAll(async () => app?.close());
test.beforeEach(async ({page, context}) => {
  page.on('pageerror', error => {
    throw error;
  });
  await context.addCookies(cookies);
});

async function open(page, path, content) {
  await app.api('/p/default/api/note', {path, content, revision: ''});
  await page.goto(app.origin + '/?project=default&note=' + encodeURIComponent(path));
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await page.locator('#mode').click();
}

test('word counts measure prose in Visual and Source without changing Markdown', async ({page}) => {
  const content =
    '---\ntitle: Excluded metadata words\n---\n# Chapter one\n\nRead [the guide](https://example.com/a-long-url) and **enjoy** it.\n\n```text\nThis code is excluded.\n```\n\n<!-- Hidden comment -->\n';
  await open(page, 'prose-count.md', content);
  await expect(page.locator('#word-count')).toHaveText('8 words');
  await page.locator('#format-source').click();
  await expect(page.locator('#word-count')).toHaveText('8 words');
  await expectEditorText(page, content.replace(/fmn-id: [^\n]+\n/, ''));
});

for (const format of ['visual', 'source', 'split'])
  test(`${format} fixes preserve Markdown and undo exactly once`, async ({page}) => {
    const content =
      '---\ntitle: Preserved\ncustom: retained\n---\n# The chapter\n\nA **very very** useful sentence.\n\n[Guide](https://example.com) and `code code`.\n';
    await open(page, 'fix-' + format + '.md', content);
    await expectVisual(page);
    await page.locator('#format-' + format).click();
    await expect(page.locator('#editing')).toHaveAttribute('data-format', format);
    await page.locator('#word-count').click();
    await page.locator('#writing-tab-checks').click();
    const finding = page.locator('#writing-findings li').filter({hasText: 'Adjacent repeated word'});
    await expect(finding).toHaveCount(1);
    await finding.getByRole('button', {name: 'Fix', exact: true}).click();
    await expectEditorText(page, content.replace('very very', 'very'));
    await page.locator('#writing-close').click();
    await page.locator('#editor').focus();
    await page.keyboard.press('ControlOrMeta+z');
    await expectEditorText(page, content);
    await page.keyboard.press('ControlOrMeta+Shift+z');
    await expectEditorText(page, content.replace('very very', 'very'));
    await page.locator('#save').click();
    await expect(page.locator('#save-state')).toContainText('Saved');
    expect((await app.api('/p/default/api/note?path=fix-' + format + '.md')).content).toBe(
      content.replace('very very', 'very')
    );
  });

test('term aliases work locally and committed notebook results carry citations; glossary is a regular note', async ({
  page
}) => {
  await open(page, 'terms.md', '# Elizabeth\n\nLizzy returns.\n');
  await page.locator('#writing-open').click();
  await page.locator('#writing-tab-terms').click();
  await page.locator('#writing-term-input').fill('Elizabeth | Lizzy');
  await expect(page.locator('#writing-mentions li')).toHaveCount(2);
  await page.locator('#writing-terms summary').click();
  await page.locator('#writing-glossary-path').fill('Cast glossary.md');
  await page.locator('#writing-glossary-save').click();
  await expect(page.locator('#writing-term-status')).toHaveText('Glossary saved.');
  const glossary = await app.api('/p/default/api/note?path=Cast%20glossary.md');
  expect(glossary.content).toBe('# Writing glossary\n\n- Elizabeth | Lizzy\n');
  await page.locator('#writing-term-input').fill('Unrelated');
  await page.locator('#writing-glossary-load').click();
  await expect(page.locator('#writing-term-input')).toHaveValue('Elizabeth | Lizzy');
  await page.locator('#writing-term-scope').selectOption('notebook');
  await page.locator('#writing-scan').click();
  await expect(page.locator('#writing-term-status')).toContainText('Search complete');
  const links = page.locator('#writing-mentions a');
  expect(await links.count()).toBeGreaterThanOrEqual(2);
  await expect(links.first()).toHaveAttribute('href', /revision=[a-f0-9]{40}/);
});

test('invalid term input cannot restore stale matches when another control rerenders', async ({page}) => {
  await open(page, 'invalid-terms.md', 'Elizabeth sees Lizzy.\n');
  await page.locator('#writing-open').click();
  await page.locator('#writing-tab-terms').click();
  await page.locator('#writing-term-input').fill('Elizabeth | Lizzy');
  await expect(page.locator('#writing-mentions li')).toHaveCount(2);
  const count = await page.evaluate(() => {
    const input = document.getElementById('writing-term-input');
    input.value = 'x'.repeat(101);
    input.dispatchEvent(new Event('input'));
    document.querySelector('[data-writing-rule="repetition,phrase"]').dispatchEvent(new Event('change'));
    return document.querySelectorAll('#writing-mentions li').length;
  });
  expect(count).toBe(0);
  await expect(page.locator('#writing-term-status')).toContainText('100 characters');
});

test('repository outline navigation reads the selected range at the same revision', async ({page}) => {
  await app.api('/api/projects', {id: 'writing-source', name: 'Writing source', kind: 'repository'});
  const ref = {
    project: 'writing-source',
    origin: 'repository',
    source: 'writing-test',
    revision: 'a'.repeat(40),
    path: 'Manual.md'
  };
  const content = '# Opening\n\n' + 'A line of prose.\n'.repeat(220) + '\n## Target\n\nThe final passage.\n',
    targetLine = content.slice(0, content.indexOf('## Target')).split('\n').length;
  await page.route('**/p/writing-source/api/document?*', route => {
    const q = new URL(route.request().url()).searchParams,
      start = Number(q.get('startLine') || 1),
      partial = q.has('startLine');
    return route.fulfill({
      json: {
        ...ref,
        hash: 'test',
        editable: false,
        citation: 'https://github.com/example/source/blob/' + ref.revision + '/Manual.md',
        content: partial
          ? content
              .split('\n')
              .slice(start - 1, start + 199)
              .join('\n')
          : content,
        html:
          start === targetLine ? '<h2>Target</h2><p>The final passage.</p>' : '<h1>Opening</h1><p>A line of prose.</p>',
        startLine: start,
        endLine: partial ? start + 199 : content.split('\n').length,
        more: partial && start === 1
      }
    });
  });
  await page.goto(app.origin + '/?' + new URLSearchParams({...ref, startLine: '1', endLine: '200'}));
  await page.locator('#source-writing-open').click();
  await page.locator('#writing-outline').getByRole('button', {name: 'Target', exact: true}).click();
  await expect(page).toHaveURL(new RegExp('startLine=' + targetLine));
  await expect(page).toHaveURL(new RegExp('revision=' + ref.revision));
  await expect(page.locator('#source-markdown h2')).toHaveText('Target');
});

test('mobile panel traps focus, retains the draft and closes with Escape', async ({page}) => {
  await page.setViewportSize({width: 390, height: 844});
  await open(page, 'mobile-writing.md', '# Small screen\n\nA clear sentence.\n');
  await page.locator('#word-count').click();
  await expect(page.locator('#writing-panel')).toHaveAttribute('aria-modal', 'true');
  expect(await page.locator('#main').evaluate(el => el.inert)).toBe(true);
  await page.locator('#writing-panel').evaluate(async panel => {
    await Promise.all(panel.getAnimations().map(animation => animation.finished));
  });
  const box = await page.locator('#writing-panel').boundingBox();
  expect(box.x).toBeGreaterThanOrEqual(0);
  expect(box.width).toBeLessThanOrEqual(390);
  await page.keyboard.press('Escape');
  await expect(page.locator('#writing-panel')).toBeHidden();
  expect(await page.locator('#main').evaluate(el => el.inert)).toBe(false);
  await expect(page.locator('#word-count')).toBeFocused();
});

test('a remote edit invalidates fixes without overwriting the peer change', async ({page}) => {
  await open(page, 'remote-writing.md', 'A very very clear sentence.\n');
  await page.locator('#word-count').click();
  await page.locator('#writing-tab-checks').click();
  await expect(page.locator('.writing-fix')).toHaveCount(1);
  const oldFix = await page.locator('.writing-fix').elementHandle();
  const base = await app.api('/p/default/api/live?path=remote-writing.md');
  await app.api('/p/default/api/live/patch', {
    path: 'remote-writing.md',
    base: base.liveRevision,
    operationId: 'writing-peer',
    edits: [{start: 2, end: 11, old: 'very very', text: 'wonderfully'}]
  });
  await expectEditorText(page, 'A wonderfully clear sentence.\n');
  await oldFix.evaluate(el => el.click());
  await expectEditorText(page, 'A wonderfully clear sentence.\n');
  await expect(page.locator('.writing-fix')).toHaveCount(0);
});

test('idle collaboration polls reuse writing analysis', async ({page}) => {
  await page.addInitScript(() => {
    window.writingJobs = 0;
    const original = Worker.prototype.postMessage;
    Worker.prototype.postMessage = function (data, ...args) {
      if (typeof data?.source === 'string' && data.options) window.writingJobs++;
      return original.call(this, data, ...args);
    };
  });
  await open(page, 'idle-writing.md', '# Quiet\n\nAn unchanged draft.\n');
  await expect(page.locator('#word-count')).toHaveText('4 words');
  const jobs = await page.evaluate(() => window.writingJobs);
  // Cross multiple real collaboration poll intervals; no new analysis is needed.
  await page.waitForTimeout(1800);
  expect(await page.evaluate(() => window.writingJobs)).toBe(jobs);
});

test('reading checks navigate to the exact occurrence without opening an editor', async ({page}) => {
  await open(page, 'reading-writing.md', 'A very very clear sentence.\n');
  await page.locator('#mode').click();
  await page.locator('#note-details-toggle').click();
  await page.locator('#writing-menu-open').click();
  await page.locator('#writing-tab-checks').click();
  await expect(page.locator('#writing-findings li')).toHaveCount(1);
  await expect(page.locator('.writing-fix')).toHaveCount(0);
  await page.locator('#writing-findings li').getByRole('button', {name: 'very', exact: true}).click();
  expect(await page.evaluate(() => ({text: getSelection().toString(), offset: getSelection().anchorOffset}))).toEqual({
    text: 'very',
    offset: 7
  });
  await expect(page.locator('#editing')).toBeHidden();
});

for (const mode of ['reading', 'visual', 'source'])
  test(`the ${mode} outline navigates repeated headings and scene breaks`, async ({page}) => {
    const passage = 'A quiet passage with enough words to fill a line.\n\n'.repeat(30);
    await open(
      page,
      'scene-' + mode + '.md',
      '# Chapter\n\n' + passage + '***\n\n## Later\n\n' + passage + '***\n\n## Later\n\nThe end.\n'
    );
    if (mode === 'reading') {
      await page.locator('#mode').click();
      await page.locator('#note-details-toggle').click();
      await page.locator('#writing-menu-open').click();
      const scenes = page.locator('#preview hr'),
        headings = page.locator('#preview h2');
      await expect(scenes.nth(1)).not.toBeInViewport();
      await page.locator('#writing-outline').getByRole('button', {name: 'Scene break', exact: true}).nth(1).click();
      await expect(scenes.nth(1)).toBeInViewport();
      await page.locator('#writing-outline').getByRole('button', {name: 'Later', exact: true}).first().click();
      await expect(headings.first()).toBeInViewport();
      await expect(page.locator('#editing')).toBeHidden();
      return;
    }
    await page.locator('#format-' + mode).click();
    await page.locator('#writing-open').click();
    // The editor selects the marker in the text, which then shows as typed,
    // and brings its line on screen; CodeMirror draws only lines near it.
    const line = text => page.locator('#editor .cm-line').filter({hasText: new RegExp('^' + text + '$')});
    await page.locator('#writing-outline').getByRole('button', {name: 'Scene break', exact: true}).nth(1).click();
    await expect.poll(() => page.evaluate(() => getSelection().toString())).toBe('***');
    await expect(line('\\*\\*\\*')).toBeInViewport();
    await expect(page.locator('#editor')).toBeFocused();
    await page.locator('#writing-outline').getByRole('button', {name: 'Later', exact: true}).first().click();
    await expect.poll(() => page.evaluate(() => getSelection().toString())).toBe('## Later');
    await expect(line('## Later').first()).toBeInViewport();
    await expect(page.locator('#editing')).toHaveAttribute('data-format', mode);
  });

test('a new unsaved Visual note keeps exact Markdown through fix and native Undo', async ({page}) => {
  await page.goto(app.origin + '/?project=default&collection=notes&folder=');
  await page.locator('#create-toggle').click();
  await page.locator('#new-note').click();
  await page.locator('#new-name').fill('Unsaved writing');
  await page.locator('#new-submit').click();
  await page.locator('#format-source').click();
  const content = '# Original\n\n\nA __very very__ clear sentence.\n';
  await fillSource(page, content);
  await page.locator('#format-visual').click();
  await expectVisual(page);
  await page.locator('#word-count').click();
  await page.locator('#writing-tab-checks').click();
  await page
    .locator('#writing-findings li')
    .filter({hasText: 'Adjacent repeated word'})
    .getByRole('button', {name: 'Fix', exact: true})
    .click();
  await expectEditorText(page, content.replace('very very', 'very'));
  await page.locator('#writing-close').click();
  await page.locator('#editor').focus();
  await page.keyboard.press('ControlOrMeta+z');
  await expectEditorText(page, content);
  await page.keyboard.press('ControlOrMeta+Shift+z');
  await expectEditorText(page, content.replace('very very', 'very'));
});

// Chapters 1–3 of F1 (see editor-manuscript.spec.mjs): the whole note is one
// editor, so a finding far from the caret is selected and fixed in place.
test('writing tools find, show and fix a passage far down a manuscript', async ({page}) => {
  const manuscript = readFileSync(new URL('./fixtures/f1-slice.md', import.meta.url), 'utf8'),
    path = 'manuscript-writing.md',
    content = manuscript.replace('I stuffed a shirt or two', 'I stuffed a very very old shirt or two');
  await open(page, path, content);
  await expectVisual(page);
  await page.locator('#word-count').click();
  await page.locator('#writing-tab-checks').click();
  const finding = page.locator('#writing-findings li').filter({hasText: 'Adjacent repeated word'});
  await expect(finding).toHaveCount(1);
  // Going to the finding selects it in the text, on screen.
  await finding.getByRole('button', {name: 'very', exact: true}).click();
  await expect.poll(() => page.evaluate(() => getSelection().toString())).toBe(' very');
  await expect(page.locator('#editor .cm-line').filter({hasText: 'I stuffed a very very old shirt'})).toBeInViewport();
  await finding.getByRole('button', {name: 'Fix', exact: true}).click();
  const fixed = content.replace('a very very old', 'a very old');
  await expectEditorText(page, fixed);
  await expect.poll(async () => (await app.api('/p/default/api/live?path=' + path)).content).toBe(fixed);
  await expect(page.locator('#editor .cm-line').filter({hasText: 'I stuffed a very old shirt'})).toBeVisible();
});
