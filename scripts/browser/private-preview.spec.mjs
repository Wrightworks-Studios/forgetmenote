import {test, expect} from '@playwright/test';
import {startApp, watchConfirmations} from './app-fixture.mjs';
import {fillSource} from './editor-text.mjs';
let app;
test.beforeAll(async () => {
  app = await startApp({
    name: 'forgetmenote-private-preview-',
    account: {username: 'private-preview-admin', password: 'DisposablePrivatePreview123!'}
  });
});
test.afterAll(async () => {
  await app?.close();
});

test('unsaved private preview renders locally and keeps active content inert', async ({page}) => {
  await app.login(page, 'recovery');
  await page.goto(app.origin + '/?project=default');
  await page.locator('#new-note').click();
  await page.locator('#new-name').fill('local-preview');
  await page.locator('#new-container').selectOption('draft');
  await page.locator('#new-submit').click();
  await page.locator('#format-source').click();
  const posts = [],
    remote = [];
  page.on('request', request => {
    if (request.method() === 'POST') posts.push(request.postData() || '');
  });
  await page.route('https://example.invalid/**', route => {
    remote.push(route.request().url());
    return route.abort();
  });
  const text =
    '---\nfmn-container: draft\n---\n# Private preview AZALEA-427\n\n[Unsafe](javascript:window.privateExecuted=true)\n\n![Remote](https://example.invalid/private-image.png)\n\n<script>window.privateExecuted=true</script>\n\n<img src="https://example.invalid/raw.png" onerror="window.privateExecuted=true">\n';
  await fillSource(page, text);
  await page.locator('#mode').click();
  await expect(page.locator('#preview')).toContainText('Private preview AZALEA-427');
  expect(posts.join('')).not.toContain('AZALEA-427');
  await expect(page.locator('#preview h1')).toHaveText('Private preview AZALEA-427');
  await expect(page.locator('#preview')).not.toContainText('fmn-container');
  expect(await page.evaluate(() => !!window.privateExecuted)).toBe(false);
  await expect(page.locator('#preview script')).toHaveCount(0);
  await expect(page.locator('#preview a[href^="javascript:"]')).toHaveCount(0);
  await expect(page.locator('#preview img[src^="https:"]')).toHaveCount(0);
  await expect(page.locator('#preview [onerror], #preview [onclick]')).toHaveCount(0);
  expect(remote).toEqual([]);
  expect(JSON.stringify(await app.api('/p/default/api/notes'))).not.toContain('local-preview');
});

async function newPrivateNote(page, name) {
  await app.login(page, 'recovery');
  await page.goto(app.origin + '/?project=default');
  await page.locator('#new-note').click();
  await page.locator('#new-name').fill(name);
  await page.locator('#new-container').selectOption('draft');
  await page.locator('#new-submit').click();
  await page.locator('#format-source').click();
}

test('private Source preview preserves typing focus and does not mount video players', async ({page}) => {
  await newPrivateNote(page, 'source-preview');
  const posts = [];
  page.on('request', request => {
    if (request.method() === 'POST') posts.push(request.postData() || '');
  });
  await fillSource(
    page,
    '# Private source focus AZALEA-428\n\n[Video](https://www.youtube.com/watch?v=dQw4w9WgXcQ "video")\n'
  );
  await page.keyboard.type(' typed on');
  await expect(page.locator('#editor')).toBeFocused();
  await expect(page.locator('#editor')).toContainText('typed on');
  // Typing never renders the hidden preview; leaving edit mode renders it once, locally.
  await page.locator('#mode').click();
  await expect(page.locator('#preview h1')).toHaveText('Private source focus AZALEA-428');
  await expect(page.locator('#preview iframe,#preview video')).toHaveCount(0);
  expect(posts.join('')).not.toContain('AZALEA-428');
});

test('private preview remains local and readable if the lazy parser cannot load', async ({page}) => {
  await page.route('**/static/dist/rendered-preview-*.js', route => route.abort());
  await newPrivateNote(page, 'offline-preview');
  const posts = [];
  page.on('request', request => {
    if (request.method() === 'POST') posts.push(request.postData() || '');
  });
  await fillSource(page, '# Offline private preview AZALEA-429\n');
  await page.locator('#mode').click();
  await expect(page.locator('#preview pre')).toHaveText('# Offline private preview AZALEA-429');
  expect(posts.join('')).not.toContain('AZALEA-429');
});

// Drafts render richly up to 4 MiB of UTF-8; a draft under 4 Mi characters
// but over 4 MiB in UTF-8 stays plain text.
test('large UTF-8 private previews stay local without rich parsing', async ({page}) => {
  await newPrivateNote(page, 'large-preview');
  const posts = [];
  page.on('request', request => {
    if (request.method() === 'POST') posts.push(request.postData() || '');
  });
  const text = '# Large private preview AZALEA-430\n' + 'ユ'.repeat(1400000);
  await fillSource(page, text);
  await page.locator('#mode').click();
  await expect(page.locator('#preview pre')).toContainText('AZALEA-430');
  expect(await page.locator('#preview pre').textContent()).toBe(text);
  expect(posts.join('')).not.toContain('AZALEA-430');
});

test('historical private drafts keep previews local and committed content unchanged', async ({page}) => {
  const path = 'historical-preview.md',
    first = await app.api('/p/default/api/note', {path, content: '# Old checkpoint\n', revision: ''});
  const saved = await app.api('/p/default/api/note', {
    path,
    content: '# Current checkpoint\n',
    revision: first.revision
  });
  await app.login(page, 'recovery');
  await page.goto(app.origin + '/?project=default&note=' + path);
  await watchConfirmations(page, d => d.accept());
  await page.locator('#note-details-toggle').click();
  await page.locator('#history-open').click();
  await page
    .locator('#history-list button')
    .filter({hasText: first.revision.slice(0, 7)})
    .click();
  await expect(page.locator('#history-dialog')).toBeHidden();
  await page.locator('#format-source').click();
  const posts = [];
  page.on('request', request => {
    if (request.method() === 'POST') posts.push(request.postData() || '');
  });
  await fillSource(page, '# Historical private preview AZALEA-431\n');
  await page.locator('#mode').click();
  await expect(page.locator('#preview h1')).toHaveText('Historical private preview AZALEA-431');
  expect(posts.join('')).not.toContain('AZALEA-431');
  expect((await app.api('/p/default/api/note?path=' + path)).content).toBe(saved.content);
});

test('private table previews avoid amplified Markdown padding', async ({page}) => {
  await newPrivateNote(page, 'table-preview');
  const text = '|' + 'h'.repeat(8192) + '|\n|---|\n' + '|a|\n'.repeat(256);
  await page.evaluate(() => {
    window.previewSpaceAllocation = 0;
    const repeat = String.prototype.repeat;
    String.prototype.repeat = function (n) {
      if (String(this) === ' ') window.previewSpaceAllocation += n;
      return Reflect.apply(repeat, this, [n]);
    };
  });
  await fillSource(page, text);
  await page.locator('#mode').click();
  await expect(page.locator('#preview table')).toBeVisible();
  const allocated = await page.evaluate(() => window.previewSpaceAllocation);
  expect(allocated).toBeLessThan(text.length * 10);
});

test('private preview bounds repeated reference destinations before rendering', async ({page}) => {
  await newPrivateNote(page, 'reference-preview');
  const text =
    '# Repeated private references\n\n' +
    '[example][r] '.repeat(64) +
    '\n\n[r]: https://example.invalid/' +
    'a'.repeat(16384);
  await fillSource(page, text);
  await page.locator('#mode').click();
  await expect(page.locator('#preview pre')).toHaveText(text);
  await expect(page.locator('#preview a')).toHaveCount(0);
});

test('private preview recovers after a draft it cannot render richly', async ({page}) => {
  await newPrivateNote(page, 'failed-dom-preview');
  const text =
    '# Private failed preview\n\n' +
    '[example][r] '.repeat(64) +
    '\n\n[r]: https://example.invalid/' +
    'a'.repeat(16384);
  await fillSource(page, text);
  await page.locator('#mode').click();
  await expect(page.locator('#preview pre')).toHaveText(text);
  await page.locator('#mode').click();
  await fillSource(page, '# Recovered private preview\n');
  await page.locator('#mode').click();
  await expect(page.locator('#preview h1')).toHaveText('Recovered private preview');
});

test('a private draft as long as a novel renders richly without leaving the browser', async ({page}) => {
  await newPrivateNote(page, 'novel-preview');
  const posts = [];
  page.on('request', request => {
    if (request.method() === 'POST') posts.push(request.postData() || '');
  });
  const chapter = n =>
    `## Chapter ${n}\n\n` +
    Array.from(
      {length: 12},
      () => 'Call me Ishmael. Some years ago, never mind how long precisely, I went to sea. '
    ).join('') +
    '\n';
  const text = '# Private novel AZALEA-432\n\n' + Array.from({length: 1400}, (_, i) => chapter(i + 1)).join('\n');
  expect(text.length).toBeGreaterThan(1 << 20);
  // Pasted through the editor view: typing a megabyte would take minutes.
  await page.locator('#editor').focus();
  await page.evaluate(text => {
    const view = document.querySelector('#editor').cmTile.root.view;
    view.dispatch({changes: {from: 0, to: view.state.doc.length, insert: text}, userEvent: 'input.paste'});
  }, text);
  await expect.poll(() => page.evaluate(() => document.getElementById('editing').markdown.length)).toBe(text.length);
  await page.locator('#mode').click();
  await expect(page.locator('#preview h1')).toHaveText('Private novel AZALEA-432', {timeout: 30000});
  await expect(page.locator('#preview h2')).toHaveCount(1400);
  await expect(page.locator('#preview h2').last()).toHaveAttribute('id', 'h-chapter-1400');
  expect(posts.join('')).not.toContain('AZALEA-432');
});
