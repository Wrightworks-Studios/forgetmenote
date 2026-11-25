import {watchConfirmations, removeConfirmations} from './app-fixture.mjs';
import {test, expect} from '@playwright/test';
import {startApp} from './app-fixture.mjs';
import {expectEditorText, fillSource} from './editor-text.mjs';
let app, origin;
async function api(path, body) {
  return app.api(path, body);
}
async function login(page) {
  await app.login(page, 'recovery');
}
test.beforeAll(async () => {
  app = await startApp({
    name: 'forgetmenote-containers-',
    account: {username: 'containers-admin', password: 'DisposableContainerPassword123!'}
  });
  ({origin} = app);
});
test.afterAll(async () => {
  await app?.close();
});
test.beforeEach(async ({page}) => {
  page.on('pageerror', error => {
    throw error;
  });
  await watchConfirmations(page, dialog => dialog.accept());
});
for (const width of [1280, 390])
  test(`collections separate Trash at ${width}px`, async ({page}) => {
    await page.setViewportSize({width, height: 900});
    await login(page);
    await page.goto(origin + '/?project=default&folder=');
    await expect(page.locator('#folder-title')).toHaveText('My notes');
    if (width < 760) await page.locator('#menu').click();
    if (width <= 720) {
      await expect(page.locator('#sidebar .sidebar-bottom #trash-open')).toBeVisible();
    } else {
      const collections = await page.locator('.collection-navigation').boundingBox(),
        trash = await page.locator('#trash-open').boundingBox();
      expect(trash.y).toBeGreaterThan(collections.y + collections.height);
    }
    expect(await page.locator('#sidebar').evaluate(el => el.scrollWidth <= el.clientWidth)).toBe(true);
    await page.screenshot({path: test.info().outputPath(`collections-${width}.png`)});
  });

test('collection footer toggles and switches one selection and Trash returns to Notes', async ({page}) => {
  await login(page);
  await page.goto(origin + '/?project=default&folder=');
  const selected = async name => {
    await expect(page.locator('#folder-title')).toHaveText(
      {notes: 'My notes', drafts: 'Drafts', archives: 'Archives'}[name]
    );
    for (const collection of ['notes', 'drafts', 'archives'])
      await expect(page.locator(`[data-collection="${collection}"]`)).toHaveAttribute(
        'aria-pressed',
        String(collection === name)
      );
    expect(new URL(page.url()).searchParams.get('collection')).toBe(name);
  };
  await selected('notes');
  for (const name of ['archives', 'archives', 'drafts', 'drafts', 'archives', 'drafts', 'archives']) {
    const previous = new URL(page.url()).searchParams.get('collection');
    await page.locator('#collection-' + name).click();
    await selected(previous === name ? 'notes' : name);
  }
  await page.goBack();
  await selected('drafts');
  await page.goForward();
  await selected('archives');
  for (const name of ['archives', 'drafts']) {
    if (name === 'drafts') {
      await clickNotebookAction(page, 'collection-drafts');
      await selected('drafts');
    }
    await clickNotebookAction(page, 'trash-open');
    await expect(page.locator('#trash-dialog')).toBeVisible();
    await selected('notes');
    await page.locator('#trash-close').click();
    await expect(page.locator('#trash-dialog')).toBeHidden();
    await selected('notes');
  }
  await clickNotebookAction(page, 'collection-archives');
  await selected('archives');
  await expect(page.locator('#trash-dialog')).toBeHidden();
});

test('history navigation closes Trash and its pending empty confirmation before restoring a collection', async ({
  page
}) => {
  const path = 'footer-history-trash.md',
    note = await api('/p/default/api/note', {path, content: '# Footer history Trash\n', revision: ''});
  await api('/p/default/api/trash', {paths: [path], revision: note.revision});
  await login(page);
  await page.goto(origin + '/?project=default&collection=archives&folder=');
  await expect(page.locator('#folder-title')).toHaveText('Archives');
  await clickNotebookAction(page, 'trash-open');
  await expect(page.locator('#trash-dialog')).toBeVisible();
  await expect(page.locator('#folder-title')).toHaveText('My notes');
  await page.locator('#trash-empty').click();
  await expect(page.locator('#trash-empty-dialog')).toBeVisible();
  await page.goBack();
  await expect(page.locator('#folder-title')).toHaveText('Archives');
  await expect(page.locator('#collection-archives')).toHaveAttribute('aria-pressed', 'true');
  await expect(page.locator('#trash-dialog')).toBeHidden();
  await expect(page.locator('#trash-empty-dialog')).toBeHidden();
  await page.goForward();
  await expect(page.locator('#folder-title')).toHaveText('My notes');
  await expect(page.locator('#collection-archives')).toHaveAttribute('aria-pressed', 'false');
  await expect(page.locator('#trash-dialog')).toBeHidden();
  await expect(page.locator('#trash-empty-dialog')).toBeHidden();
  expect((await api('/p/default/api/trash')).entries.map(entry => entry.originalPath)).toContain(path);
});

test('Trash supersedes a delayed collection navigation', async ({page}) => {
  await login(page);
  await page.goto(origin + '/?project=default&folder=');
  await expect(page.locator('#folder-title')).toHaveText('My notes');
  let release,
    delivered,
    waiting = false;
  const gate = new Promise(resolve => (release = resolve)),
    delivery = new Promise(resolve => (delivered = resolve));
  await page.route(
    url => url.pathname.endsWith('/api/browse') && url.searchParams.get('collection') === 'archives',
    async route => {
      const response = await route.fetch();
      waiting = true;
      await gate;
      await route.fulfill({response});
      delivered();
    }
  );
  try {
    await clickNotebookAction(page, 'collection-archives');
    await expect.poll(() => waiting).toBe(true);
    await clickNotebookAction(page, 'trash-open');
    await expect(page.locator('#trash-dialog')).toBeVisible();
    release();
    await delivery;
    await page.evaluate(() => new Promise(requestAnimationFrame));
    await expect(page.locator('#folder-title')).toHaveText('My notes');
    await expect(page.locator('#collection-archives')).toHaveAttribute('aria-pressed', 'false');
    await expect(page.locator('#collection-drafts')).toHaveAttribute('aria-pressed', 'false');
    expect(new URL(page.url()).searchParams.get('collection')).toBe('notes');
    await page.locator('#trash-close').click();
    await expect(page.locator('#folder-title')).toHaveText('My notes');
  } finally {
    release();
  }
});

for (const target of ['collection-drafts', 'trash-open'])
  test(`collection footer ${target} respects declined navigation and retains private text`, async ({page}) => {
    const name = 'footer-private-' + target,
      text = '# Private footer retention ' + target + '\n';
    await login(page);
    await page.goto(origin + '/?project=default&collection=drafts&folder=');
    await expect(page.locator('#folder-title')).toHaveText('Drafts');
    await clickNewNote(page);
    await page.locator('#new-name').fill(name);
    await page.locator('#new-container').selectOption('draft');
    await page.locator('#new-submit').click();
    await page.locator('#format-source').click();
    await fillSource(page, text);
    const url = page.url(),
      requests = [];
    page.on('request', request => {
      if (request.method() === 'POST') requests.push(request.postData() || '');
    });
    removeConfirmations(page);
    await watchConfirmations(page, dialog => dialog.dismiss(), true);
    await page.locator('#' + target).click();
    await expectEditorText(page, text);
    expect(page.url()).toBe(url);
    await expect(page.locator('#collection-drafts')).toHaveAttribute('aria-pressed', 'true');
    await expect(page.locator('#trash-dialog')).toBeHidden();
    await watchConfirmations(page, dialog => dialog.accept());
    await page.locator('#' + target).click();
    await expect(page.locator('#folder-title')).toHaveText('My notes');
    if (target === 'trash-open') {
      await expect(page.locator('#trash-dialog')).toBeVisible();
      await page.locator('#trash-close').click();
    }
    expect(JSON.stringify(await api('/p/default/api/notes'))).not.toContain(name);
    expect(requests.join('')).not.toContain('Private footer retention');
    await clickNewNote(page);
    await page.locator('#new-name').fill(name);
    await page.locator('#new-container').selectOption('draft');
    await page.locator('#new-submit').click();
    await page.locator('#format-source').click();
    await expectEditorText(page, text);
  });

test('collections preserve paths and navigation while reversible actions commit membership', async ({page}) => {
  const path = 'isolated/archive.md';
  await api('/p/default/api/note', {
    path,
    content: '---\nfmn-container: archive\nstatus: draft\n---\n# Archive body\n',
    revision: ''
  });
  await api('/p/default/api/note', {path: 'ordinary.md', content: '# Ordinary\n', revision: ''});
  await login(page);
  await page.goto(origin + '/?project=default&folder=');
  await expect(page.locator('#collection-archives')).toBeVisible();
  await expect(page.locator('#sidebar [data-note="' + path + '"]')).toHaveCount(0);
  await expect(page.locator('#folder-items')).not.toContainText('isolated');
  await clickNotebookAction(page, 'collection-archives');
  await expect(page).toHaveURL(/collection=archives/);
  await expect(page.locator('#folder-title')).toHaveText('Archives');
  await page.locator('#folder-groups a').filter({hasText: 'isolated'}).click();
  await expect(page.locator('#folder-crumbs')).toContainText('Archives');
  await page.screenshot({path: 'test-results/containers-desktop.png', fullPage: true});
  await page.locator('#folder-items a').filter({hasText: 'Archive body'}).click();
  await expect(page.locator('#metadata-bar')).toContainText('Archive');
  await expect(page.locator('#note-path')).toContainText('isolated/archive');
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await clickNoteAction(page, 'properties-open');
  await page.locator('#note-container-draft').click();
  await expect(page.locator('#properties-dialog')).toBeHidden();
  expect((await api('/p/default/api/note?path=' + path)).metadata.container).toBe('draft');
  await expect(page.locator('#metadata-bar')).toContainText('Shared draft');
  await clickNotebookAction(page, 'collection-drafts');
  await expect(page.locator('#folder-title')).toHaveText('Drafts');
  await page.goBack();
  await expect(page.locator('#document')).toBeVisible();
  await page.goForward();
  await expect(page.locator('#folder-title')).toHaveText('Drafts');
  await page.reload();
  await expect(page.locator('#folder-title')).toHaveText('Drafts');
});
test('new shared Draft stays private until explicit save and survives collection navigation', async ({page}) => {
  await login(page);
  await page.goto(origin + '/?project=default&collection=drafts&folder=');
  await expect(page.locator('#folder-title')).toHaveText('Drafts');
  await clickNewNote(page);
  await page.locator('#new-name').fill('private-before-save');
  await page.locator('#new-container').selectOption('draft');
  await page.locator('#new-submit').click();
  await page.locator('#format-source').click();
  const text = '---\nfmn-container: draft\n---\n# Private pre-save text\n';
  await fillSource(page, text);
  const requests = [];
  page.on('request', request => {
    if (request.method() === 'POST') requests.push(request.postData() || '');
  });
  await clickNotebookAction(page, 'collection-archives');
  await expect(page.locator('#folder-title')).toHaveText('Archives');
  expect(JSON.stringify(await api('/p/default/api/notes'))).not.toContain('private-before-save');
  expect(requests.join('')).not.toContain('Private pre-save text');
  await clickNewNote(page);
  await page.locator('#new-name').fill('private-before-save');
  await page.locator('#new-container').selectOption('draft');
  await page.locator('#new-submit').click();
  await page.locator('#format-source').click();
  await expectEditorText(page, text);
  await expect(page.locator('#save')).toHaveText('Save to Drafts');
  const response = page.waitForResponse(r => r.request().method() === 'POST' && r.url().endsWith('/api/note'));
  await page.locator('#save').click();
  const result = await response;
  expect(result.status(), await result.text()).toBe(200);
  await expect(page.locator('#save-state'), await page.locator('#notice').textContent()).toContainText('Saved');
  const saved = await api('/p/default/api/note?path=private-before-save.md');
  expect(saved.metadata.container).toBe('draft');
  expect(saved.content).toBe(text);
});
test('failed classification and failed private retention leave the current text intact', async ({page}) => {
  const path = 'failed-transition.md',
    text = '# Preserve original\n';
  await api('/p/default/api/note', {path, content: text, revision: ''});
  await login(page);
  await page.goto(origin + '/?project=default&note=' + path);
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await page.route('**/api/container', route => route.abort());
  await clickNoteAction(page, 'properties-open');
  await page.locator('#note-container-archive').click();
  await expect(page.locator('#properties-error')).toBeVisible();
  expect((await api('/p/default/api/note?path=' + path)).content).toBe(text);
  await expect(page.locator('#note-path')).toContainText('failed-transition');
  await page.locator('[data-close="properties-dialog"]').click();
  await page.unroute('**/api/container');
  await clickNewNote(page);
  await page.locator('#new-name').fill('retention-failure');
  await page.locator('#new-submit').click();
  await page.locator('#format-source').click();
  await fillSource(page, '# Private retained intent\n');
  const url = page.url();
  await page.evaluate(() => {
    const set = Storage.prototype.setItem;
    window.restoreStorage = () => (Storage.prototype.setItem = set);
    Storage.prototype.setItem = function (key, value) {
      if (key.startsWith('gitnotes:draft:')) throw new DOMException('Synthetic draft failure', 'QuotaExceededError');
      return set.call(this, key, value);
    };
  });
  for (const target of ['collection-archives', 'trash-open']) {
    await page.locator('#' + target).click();
    await expect(page.locator('#document')).toBeVisible();
    await expectEditorText(page, '# Private retained intent\n');
    await expect(page.locator('#trash-dialog')).toBeHidden();
    expect(page.url()).toBe(url);
  }
  expect(JSON.stringify(await api('/p/default/api/notes'))).not.toContain('retention-failure');
  await page.evaluate(() => window.restoreStorage());
});
test('mobile collection links keep breadcrumbs and original paths', async ({page}) => {
  await page.setViewportSize({width: 390, height: 844});
  const path = 'mobile/location.md';
  await api('/p/default/api/note', {
    path,
    content: '---\nfmn-container: archive\n---\n# Mobile archive\n',
    revision: ''
  });
  await login(page);
  await page.goto(origin + '/?project=default&collection=archives&folder=mobile');
  await expect(page.locator('#folder-crumbs')).toContainText('Archives');
  await page.screenshot({path: 'test-results/containers-mobile.png', fullPage: true});
  await page.locator('#folder-items a').filter({hasText: 'Mobile archive'}).click();
  await expect(page.locator('#note-path')).toContainText('mobile/location');
  await expect(page).toHaveURL(/collection=archives/);
  await page.locator('#menu').click();
  await clickNotebookAction(page, 'collection-drafts');
  await expect(page.locator('#folder-title')).toHaveText('Drafts');
  await expect(page.locator('#menu')).toHaveAttribute('aria-expanded', 'false');
  await page.goBack();
  await expect(page.locator('#note-path')).toContainText('mobile/location');
  await page.reload();
  await expect(page.locator('#metadata-bar')).toContainText('Archive');
});
test('regression: historical collection route survives back navigation', async ({page}) => {
  const path = 'review-history.md';
  const old = await api('/p/default/api/note', {path, content: '# Earlier exact bytes\n', revision: ''});
  await api('/p/default/api/note', {
    path,
    content: '---\nfmn-container: archive\n---\n# Current archive bytes\n',
    revision: old.revision
  });
  await login(page);
  await page.goto(origin + '/?project=default&collection=archives&note=' + path + '&revision=' + old.revision);
  await expect(page.locator('.citation-content')).toContainText('Earlier exact bytes');
  await expect(page).toHaveURL(new RegExp('collection=archives.*revision=' + old.revision));
  await page.locator('[data-close="knowledge-dialog"]').click();
  await clickNotebookAction(page, 'collection-drafts');
  await expect(page.locator('#folder-title')).toHaveText('Drafts');
  await page.goBack();
  await expect(page.locator('#knowledge-dialog')).toBeVisible();
  await expect(page.locator('.citation-content')).toContainText('Earlier exact bytes');
  await expect(page).toHaveURL(new RegExp('collection=archives.*revision=' + old.revision));
  await page.goForward();
  await expect(page.locator('#folder-title')).toHaveText('Drafts');
  await expect(page.locator('#knowledge-dialog')).toBeHidden();
  await page.goBack();
  await expect(page.locator('.citation-content')).toContainText('Earlier exact bytes');
  await page.reload();
  await expect(page.locator('.citation-content')).toContainText('Earlier exact bytes');
  await expect(page).toHaveURL(new RegExp('collection=archives.*revision=' + old.revision));
});
test('regression: incoming committed classification is shown as saved', async ({page}) => {
  const path = 'review-remote.md';
  const old = await api('/p/default/api/note', {path, content: '# Remote classification\n', revision: ''});
  await login(page);
  await page.goto(origin + '/?project=default&note=' + path);
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  const live = await api('/p/default/api/live?path=' + path);
  await api('/p/default/api/live/patch', {
    path,
    base: live.liveRevision,
    operationId: 'review-classification',
    edits: [{start: 0, end: 0, old: '', text: '---\nfmn-container: archive\n---\n'}]
  });
  await api('/p/default/api/live/checkpoint', {path});
  await expectEditorText(page, /fmn-container: archive/);
  await expect(page.locator('#save')).toBeDisabled();

  await expect(page.locator('#container-badge')).toHaveText('Archive');
  await clickNoteAction(page, 'properties-open');
  await expect(page.locator('#note-container-archive')).toBeHidden();
  await expect(page.locator('#note-container-notes')).toBeEnabled();
});
test('regression: escaped valid classification is shown correctly', async ({page}) => {
  const path = 'review-quoted.md';
  await api('/p/default/api/note', {
    path,
    content: '---\nfmn-container: "\\u0061rchive"\n---\n# Valid escaped classification\n',
    revision: ''
  });
  await login(page);
  await page.goto(origin + '/?project=default&collection=archives&note=' + path);
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await expect(page.locator('#container-badge')).toHaveText('Archive');
  await clickNoteAction(page, 'properties-open');
  await expect(page.locator('#note-container-archive')).toBeHidden();
  await expect(page.locator('#note-container-notes')).toBeEnabled();
});

for (const change of ['typing', 'navigation'])
  test('checkpoint metadata refresh preserves newer ' + change, async ({page}) => {
    const path = 'late-metadata-' + change + '.md',
      body = '# Before checkpoint\n';
    await api('/p/default/api/note', {path, content: body, revision: ''});
    await login(page);
    await page.goto(origin + '/?project=default&note=' + path);
    await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
    let release,
      delivered,
      waiting = false;
    const gate = new Promise(resolve => (release = resolve)),
      delivery = new Promise(resolve => (delivered = resolve));
    await page.route(
      url =>
        url.pathname === '/p/default/api/note' &&
        url.searchParams.get('path') === path &&
        url.searchParams.has('revision'),
      async route => {
        const response = await route.fetch();
        waiting = true;
        await gate;
        await route.fulfill({response});
        delivered();
      }
    );
    try {
      const live = await api('/p/default/api/live?path=' + path);
      await api('/p/default/api/live/patch', {
        path,
        base: live.liveRevision,
        operationId: 'late-checkpoint-' + change,
        edits: [{start: 0, end: 0, old: '', text: '---\nfmn-container: archive\n---\n'}]
      });
      await api('/p/default/api/live/checkpoint', {path});
      await expect.poll(() => waiting).toBe(true);
      if (change === 'typing') {
        await page.locator('#mode').click();
        await page.locator('#format-source').click();
        await fillSource(page, '---\nfmn-container: draft\n---\n# Newer uncommitted input\n');
        release();
        await delivery;
        await page.evaluate(() => new Promise(requestAnimationFrame));
        await expectEditorText(page, /Newer uncommitted input/);
        await expect(page.locator('#save')).toBeEnabled();
        await expect(page.locator('#save-state')).toContainText('Live changes synced');
        await expect(page.locator('#container-badge')).toHaveText('Archive → Shared draft · pending Save');
        await expect(page.locator('#sidebar a[data-note="' + path + '"]')).toHaveCount(0);
        await clickNotebookAction(page, 'collection-drafts');
        await expect(page.locator('#folder-title')).toHaveText('Drafts');
        const retained = await page.evaluate(
          path =>
            Object.entries(localStorage)
              .filter(([key]) => key.includes(path))
              .map(([, value]) => value)
              .join('\n'),
          path
        );
        expect(retained).toContain('Newer uncommitted input');
        expect((await api('/p/default/api/note?path=' + path)).metadata.container).toBe('archive');
      } else {
        await clickNotebookAction(page, 'collection-drafts');
        await expect(page.locator('#folder-title')).toHaveText('Drafts');
        release();
        await delivery;
        await page.evaluate(() => new Promise(requestAnimationFrame));
        await expect(page.locator('#document')).toBeHidden();
        await expect(page.locator('#container-badge')).toHaveCount(0);
        await expect(page).toHaveURL(/collection=drafts/);
      }
    } finally {
      release();
    }
  });
test('regression: live committed classification refreshes the selected collection tree and counts', async ({page}) => {
  const path = 'peer-membership.md';
  const note = await api('/p/default/api/note', {path, content: '# Peer classification\n', revision: ''});
  await login(page);
  await page.goto(origin + '/?project=default&collection=notes&note=' + path);
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await expect(page.locator('#container-badge')).toHaveCount(0);
  const live = await api('/p/default/api/live?path=' + path);
  await api('/p/default/api/live/patch', {
    path,
    base: live.liveRevision,
    operationId: 'review-peer-archive',
    edits: [{start: 0, end: 0, old: '', text: '---\nfmn-container: archive\n---\n'}]
  });
  await api('/p/default/api/live/checkpoint', {path});
  await expect(page.locator('#container-badge')).toHaveText('Archive');
  await expect(page.locator('#sidebar a[data-note="' + path + '"]')).toHaveCount(0);
  const counts = await api('/p/default/api/collections');
  await expect(page.locator('#count-notes')).toHaveText(String(counts.notes));
  await expect(page.locator('#count-archives')).toHaveText(String(counts.archives));
  await page.locator('#browse-root').click();
  await expect(page.locator('#folder-title')).toHaveText('My notes');
  await expect(page.locator('#sidebar a[data-note="' + path + '"]')).toHaveCount(0);
  await expect(page.locator('#count-notes')).toHaveText(String(counts.notes));
  await expect(page.locator('#count-archives')).toHaveText(String(counts.archives));
});

test('regression: failed switched collection load retries after toggling through Notes', async ({page}) => {
  await api('/p/default/api/note', {
    path: 'retry-archive.md',
    content: '---\nfmn-container: archive\n---\n# Retry archive\n',
    revision: ''
  });
  await login(page);
  await page.goto(origin + '/?project=default&collection=notes&folder=');
  await expect(page.locator('#folder-title')).toHaveText('My notes');
  await page.route(
    url => url.pathname.endsWith('/api/notes') && url.searchParams.get('collection') === 'archives',
    route => route.abort()
  );
  await clickNotebookAction(page, 'collection-archives');
  await expect(page.locator('#folder-title')).toHaveText('Archives');
  await expect(page.locator('#search-status')).not.toHaveText('');
  await page.unrouteAll();
  await clickNotebookAction(page, 'collection-archives');
  await expect(page.locator('#folder-title')).toHaveText('My notes');
  await clickNotebookAction(page, 'collection-archives');
  await expect(page.locator('#folder-title')).toHaveText('Archives');
  await expect(page.locator('#folder-items a[data-note="retry-archive.md"]')).toHaveCount(1);
});

test('same collection refresh keeps search text, filters and history options', async ({page}) => {
  await api('/p/default/api/note', {
    path: 'retained-options/note.md',
    content: '---\ntags: [retained]\nstatus: canon\n---\n# Retained search\n',
    revision: ''
  });
  await login(page);
  await page.goto(origin + '/?project=default&collection=notes&folder=');
  await expect(page.locator('#folder-title')).toHaveText('My notes');
  await ensureSearch(page);
  await page.locator('#search-options-toggle').click();
  await page.locator('#advanced-search summary').click();
  await page.locator('input[name="search-mode"][value="text"]').check();
  if (!(await page.locator('#search-scope').isVisible())) await page.locator('#advanced-search summary').click();
  await page.locator('#search-scope').check();
  await page.locator('#include-trash').check();
  await page.locator('#folder-filter').selectOption('retained-options/');
  await page.locator('#tag-filter').selectOption('retained');
  await page.locator('#status-filter').selectOption('canon');
  await ensureSearch(page);
  await page.locator('#search').fill('retained');
  await expect(page.locator('#search-status')).toContainText('match');
  const searched = page.waitForResponse(
    response =>
      response.url().endsWith('/api/knowledge/hybrid-search') && response.request().postDataJSON()?.text === 'retained'
  );
  await page.locator('#browse-root').click();
  const response = await searched;
  expect(response.status()).toBe(200);
  expect(response.request().postDataJSON()).toMatchObject({
    text: 'retained',
    mode: 'text',
    scope: 'history',
    includeTrash: true,
    path: 'retained-options/',
    tag: 'retained',
    status: 'canon'
  });
  await expect(page.locator('#search')).toHaveValue('retained');
  await expect(page.locator('#folder-filter')).toHaveValue('retained-options/');
  await expect(page.locator('#tag-filter')).toHaveValue('retained');
  await expect(page.locator('#status-filter')).toHaveValue('canon');
  await expect(page.locator('#search-scope')).toBeChecked();
  await expect(page.locator('#include-trash')).toBeChecked();
  await expect(page.locator('input[name="search-mode"][value="text"]')).toBeChecked();
});
test('late metadata cannot replace a superseding committed membership', async ({page}) => {
  const path = 'superseded-metadata.md';
  await api('/p/default/api/note', {path, content: '# Superseded base\n', revision: ''});
  await login(page);
  await page.goto(origin + '/?project=default&note=' + path);
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  let release,
    delivered,
    waiting = false;
  const gate = new Promise(resolve => (release = resolve)),
    delivery = new Promise(resolve => (delivered = resolve));
  await page.route(
    url =>
      url.pathname === '/p/default/api/note' &&
      url.searchParams.get('path') === path &&
      url.searchParams.has('revision'),
    async route => {
      if (waiting) {
        await route.continue();
        return;
      }
      waiting = true;
      const response = await route.fetch();
      await gate;
      await route.fulfill({response});
      delivered();
    }
  );
  try {
    let live = await api('/p/default/api/live?path=' + path);
    await api('/p/default/api/live/patch', {
      path,
      base: live.liveRevision,
      operationId: 'superseded-archive',
      edits: [{start: 0, end: 0, old: '', text: '---\nfmn-container: archive\n---\n'}]
    });
    await api('/p/default/api/live/checkpoint', {path});
    await expect.poll(() => waiting).toBe(true);
    live = await api('/p/default/api/live?path=' + path);
    const start = live.content.indexOf('archive');
    await api('/p/default/api/live/patch', {
      path,
      base: live.liveRevision,
      operationId: 'superseding-draft',
      edits: [{start, end: start + 7, old: 'archive', text: 'draft'}]
    });
    const saved = await api('/p/default/api/live/checkpoint', {path});
    await expect(page.locator('#container-badge')).toHaveText('Shared draft');
    release();
    await delivery;
    await page.evaluate(() => new Promise(requestAnimationFrame));
    await expect(page.locator('#container-badge')).toHaveText('Shared draft');
    await expectEditorText(page, saved.content);
    await expect(page.locator('#save')).toBeDisabled();
    expect((await api('/p/default/api/note?path=' + path)).content).toBe(saved.content);
  } finally {
    release();
  }
});

test('search scope controls separate global queries from collection browsing', async ({page}) => {
  for (const [path, container, title] of [
    ['scope-normal.md', '', 'Scope current'],
    ['scope-draft.md', 'draft', 'Scope draft'],
    ['scope-archive.md', 'archive', 'Scope archived']
  ])
    await api('/p/default/api/note', {
      path,
      content:
        (container ? '---\nfmn-container: ' + container + '\n---\n' : '') + '# ' + title + '\nsearchscopeneedle\n',
      revision: ''
    });
  await login(page);
  await page.goto(origin + '/?project=default&collection=notes&folder=');
  await expect(page.locator('#folder-title')).toHaveText('My notes');
  await ensureSearch(page);
  await page.locator('#search-options-toggle').click();
  if (!(await page.locator('#search-archives').isVisible())) await page.locator('#advanced-search summary').click();
  await expect(page.locator('#search-archives')).toBeVisible();
  await expect(page.locator('#search-drafts')).toBeVisible();
  const global = page.waitForResponse(
    r => r.url().endsWith('/api/knowledge/hybrid-search') && r.request().postDataJSON()?.text === 'searchscopeneedle'
  );
  await ensureSearch(page);
  await page.locator('#search').fill('searchscopeneedle');
  const globalResponse = await global;
  expect(globalResponse.request().postDataJSON().collection || 'all').toBe('all');
  // Archives stay out of ordinary search unless a search asks for them.
  await expect(page.locator('#search-results [data-note="scope-draft.md"]')).toHaveCount(1);
  await expect(page.locator('#search-results [data-note="scope-archive.md"]')).toHaveCount(0);
  await page.locator('#search-archives').selectOption('include');
  await expect(page.locator('#search-results [data-note="scope-archive.md"]')).toHaveCount(1);
  await page.screenshot({path: 'test-results/search-containers-desktop.png', fullPage: true});
  await page.locator('#search-archives').selectOption('exclude');
  await expect(page.locator('#search-results [data-note="scope-archive.md"]')).toHaveCount(0);
  await page.locator('#search-drafts').selectOption('exclude');
  await expect(page.locator('#search-results [data-note="scope-draft.md"]')).toHaveCount(0);
  await page.locator('#search-archives').selectOption('include');
  if (!(await page.locator('#search-drafts').isVisible())) await page.locator('#advanced-search summary').click();
  await page.locator('#search-drafts').selectOption('include');
  await clickNotebookAction(page, 'collection-archives');
  await expect(page.locator('#search')).toHaveValue('');
  await ensureSearch(page);
  await page.locator('#search').fill('searchscopeneedle');
  await expect(page.getByRole('button', {name: 'Remove Collection: Archives', exact: true})).toBeVisible();
  await expect(page.locator('#search-results [data-note="scope-normal.md"]')).toHaveCount(0);
  await expect(page.locator('#search-results [data-note="scope-archive.md"]')).toHaveCount(1);
  await page.getByRole('button', {name: 'Remove Collection: Archives', exact: true}).click();
  await expect(page.locator('#search-results [data-note="scope-normal.md"]')).toHaveCount(1);
  await ensureSearch(page);
  await page.locator('#search').fill('');
  await page.locator('#search-close').click();
  await expect(page.locator('#search-view')).toBeHidden();
  await expect(page.locator('#sidebar [data-note="scope-normal.md"]')).toHaveCount(0);
  await expect(page.locator('#folder-items [data-note="scope-archive.md"]')).toHaveCount(1);
  await clickNotebookAction(page, 'collection-drafts');
  await expect(page.locator('#folder-title')).toHaveText('Drafts');
  await page.goBack();
  await expect(page.locator('#folder-title')).toHaveText('Archives');
  await page.goForward();
  await expect(page.locator('#folder-title')).toHaveText('Drafts');
});

test('notebook defaults save with CAS and explicit search choices survive reload', async ({page}) => {
  await login(page);
  await page.goto(origin + '/?project=default&folder=');
  await expect(page.locator('#folder-title')).toHaveText('My notes');
  await ensureSearch(page);
  await page.locator('#search-options-toggle').click();
  if (!(await page.locator('#search-drafts').isVisible())) await page.locator('#advanced-search summary').click();
  await page.locator('#search-drafts').selectOption('include');
  await page.locator('#settings-open').click();
  await page.locator('[data-settings-category=search]').click();
  await page.locator('#default-search-drafts').selectOption('exclude');
  await page.locator('#retrieval-defaults-save').click();
  await expect(page.locator('#retrieval-defaults-status')).toHaveText('Defaults saved.');
  expect((await api('/p/default/api/knowledge/retrieval-settings')).settings.search.drafts).toBe('exclude');
  await page.locator('[data-close="settings-dialog"]').click();
  await page.reload();
  await ensureSearch(page);
  await page.locator('#search-options-toggle').click();
  await expect(page.locator('#search-drafts')).toHaveValue('include');
  await page.locator('#advanced-search summary').click();
  await page.locator('#search-drafts').selectOption('');
  await expect(page.locator('#search-drafts option[value=""]')).toHaveText('Default: Exclude');
  const settings = await api('/p/default/api/knowledge/retrieval-settings');
  settings.settings.search.drafts = 'include';
  await api('/p/default/api/knowledge/retrieval-settings', {settings: settings.settings, expectedId: settings.id});
});

test('mobile search controls keep scoped choices inside options', async ({page}) => {
  for (const [path, container] of [
    ['mobile-scope-normal.md', ''],
    ['mobile-scope-archive.md', 'archive']
  ])
    await api('/p/default/api/note', {
      path,
      content:
        (container ? '---\nfmn-container: ' + container + '\n---\n' : '') + '# Mobile scope\nmobilequeryneedle\n',
      revision: ''
    });
  await page.setViewportSize({width: 390, height: 844});
  await login(page);
  await page.goto(origin + '/?project=default&collection=archives&folder=');
  await expect(page.locator('#folder-title')).toHaveText('Archives');
  await page.locator('#menu').click();
  await expect(page.locator('#search-archives')).toBeHidden();
  await ensureSearch(page);
  await page.locator('#search-options-toggle').click();
  if (!(await page.locator('#search-archives').isVisible())) await page.locator('#advanced-search summary').click();
  await expect(page.locator('#search-archives')).toBeVisible();
  expect((await page.locator('#include-trash').boundingBox()).width).toBeLessThanOrEqual(24);
  await page.locator('#search-archives').selectOption('include');
  await ensureSearch(page);
  await page.locator('#search').fill('mobilequeryneedle');
  await expect(page.getByRole('button', {name: 'Remove Collection: Archives', exact: true})).toBeVisible();
  await page.screenshot({path: 'test-results/search-containers-mobile.png', fullPage: true});
  await page.getByRole('button', {name: 'Remove Collection: Archives', exact: true}).click();
  await expect(page.locator('#search-results [data-note="mobile-scope-normal.md"]')).toHaveCount(1);
});

// A note version over the note size limit (FMN_LIMIT_FILE_BYTES) leaves the
// historical scan incomplete. The app stores no such note, so this one is
// stored under a larger limit and then removed (Trash emptied), and the app
// restarted with a smaller one before any historical search.
test('empty historical results retain partial coverage in the browser', async ({page}) => {
  const limited = await startApp({
    name: 'forgetmenote-history-limit-',
    account: {username: 'history-admin', password: 'DisposableHistoryPassword123!'},
    env: {FMN_LIMIT_FILE_BYTES: String(2 << 20)}
  });
  try {
    const path = 'oversized-history.md',
      note = await limited.api('/p/default/api/note', {
        path,
        content: '# Oversized\n\n' + 'Long words. '.repeat(90000) + '\n',
        revision: ''
      });
    await limited.api('/p/default/api/trash', {paths: [path], revision: note.revision});
    const trash = await limited.api('/p/default/api/trash');
    await limited.api('/p/default/api/trash/empty', {revision: trash.revision, confirmation: trash.confirmation});
    await limited.restart({env: {FMN_LIMIT_FILE_BYTES: String(1 << 20)}});
    await limited.login(page, 'recovery');
    await page.goto(limited.origin + '/?project=default&collection=notes&folder=');
    await expect(page.locator('#folder-title')).toHaveText('My notes');
    await ensureSearch(page);
    await page.locator('#search-options-toggle').click();
    if (!(await page.locator('#search-scope').isVisible())) await page.locator('#advanced-search summary').click();
    await page.locator('#search-scope').check();
    await ensureSearch(page);
    await page.locator('#search').fill('absentpartialneedle');
    await expect(page.locator('#search-status')).toContainText('Historical scan is incomplete');
    await expect(page.locator('#search-status')).toContainText('FMN_LIMIT_FILE_BYTES');
    await expect(page.locator('#search-results')).toContainText('No matches in the scanned versions');
    await expect(page.locator('#search-results')).not.toContainText('No matching notes');
  } finally {
    await limited.close();
  }
});

async function clickNoteAction(page, id) {
  if ((await page.locator('#note-details-toggle').getAttribute('aria-expanded')) !== 'true')
    await page.locator('#note-details-toggle').click();
  await page.locator('#' + id).click();
}

async function clickNewNote(page) {
  await expect(page.locator('#new-note')).toBeEnabled();
  if (await page.locator('#new-note').isHidden()) {
    if (await page.locator('#search-view').isVisible()) await page.locator('#search-close').click();
    await clickNotebookAction(page, 'sidebar-new-note');
  } else await page.locator('#new-note').click();
}

async function ensureSearch(page) {
  if (await page.locator('#search-view').isHidden()) {
    if (await page.locator('#sidebar').isHidden()) await page.locator('#menu').click();
    await page.locator('#search-open').click();
  } else if (page.viewportSize()?.width <= 720 && (await page.locator('#sidebar').isVisible()))
    await page.locator('#menu').click();
}

async function clickNotebookAction(page, id) {
  if (await page.locator('#' + id).isHidden()) {
    if (await page.locator('#sidebar').isHidden()) await page.locator('#menu').click();
  }
  await page.locator('#' + id).click();
}
