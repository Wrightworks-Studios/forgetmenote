import {test, expect} from '@playwright/test';
// Each worker owns an app; baseline reset makes cases independent within it.
test.describe.configure({mode: 'parallel'});
import {withClipboard} from './app-fixture.mjs';
import {sourceCitation} from '../../internal/web/ui/sources.mjs';
import {
  origin,
  retainedTexts,
  api,
  login,
  edit,
  privateDraft,
  expectCreationPending,
  clickNewNote,
  ensureSearch,
  useEditorApp
} from './editor-fixture.mjs';
import {expectEditorText, fillSource} from './editor-text.mjs';
useEditorApp();
for (const routeKind of ['folder', 'note'])
  test('creation waits for initial ' + routeKind + ' routing before choosing a destination', async ({page}) => {
    const folder = 'creation-routing-' + routeKind,
      path = folder + '/reference.md';
    const note = await api('/p/default/api/note', {path, content: '# Routing reference\n', revision: ''});
    if (routeKind === 'folder')
      await api('/p/default/api/locks', {path: folder, locked: true, revision: note.revision});
    await login(page);
    if (routeKind === 'note') await privateDraft(page, note, '# Recovered routing draft\n');
    let releaseStatus,
      releaseDestination,
      statusWaiting = false,
      destinationWaiting = false;
    const statusGate = new Promise(resolve => (releaseStatus = resolve)),
      destinationGate = new Promise(resolve => (releaseDestination = resolve));
    await page.route('**/api/status', async route => {
      statusWaiting = true;
      await statusGate;
      await route.continue();
    });
    await page.route(
      url =>
        url.pathname.endsWith('/api/' + (routeKind === 'folder' ? 'browse' : 'note')) &&
        url.searchParams.get('path') === (routeKind === 'folder' ? folder : path) &&
        (routeKind !== 'folder' || url.searchParams.get('collection') === 'notes'),
      async route => {
        destinationWaiting = true;
        await destinationGate;
        await route.continue();
      }
    );
    try {
      await page.goto(
        origin + '/?project=default&' + routeKind + '=' + encodeURIComponent(routeKind === 'folder' ? folder : path)
      );
      await expect.poll(() => statusWaiting).toBe(true);
      await expectCreationPending(page);
      releaseStatus();
      await expect.poll(() => destinationWaiting).toBe(true);
      await expectCreationPending(page);
    } finally {
      releaseStatus();
      releaseDestination();
    }
    if (routeKind === 'folder') await expect(page.locator('#path')).toHaveText(folder);
    else await expect(page.locator('#note-path')).toHaveText(path);
    await expect(page.locator(routeKind === 'folder' ? '#folder-browser' : '#document')).toBeVisible();
    await clickNewNote(page);
    await page.locator('#new-name').fill('Another note');
    await expect(page.locator('#new-folder')).toHaveValue(folder);
    await page.getByRole('tab', {name: 'Import source', exact: true}).click();
    await expect(page.locator('#archive-folder')).toHaveValue(folder);
    await page.getByRole('tab', {name: 'Note', exact: true}).click();
    await page.locator('#new-submit').click();
    if (routeKind === 'folder') {
      await expect(page.locator('#new-error')).toContainText('read-only');
      await expect(page.locator('#new-dialog')).toBeVisible();
      await expect(page.locator('#path')).toHaveText(folder.replace(/\.md$/i, ''));
    } else {
      await expect(page.locator('#new-dialog')).toBeHidden();
      await expect(page.locator('#path')).toHaveText(
        (folder + '/Another note.md').split('/').pop().replace(/\.md$/i, '')
      );
      const drafts = await page.evaluate(() =>
        Object.entries(localStorage)
          .filter(([key]) => key.endsWith('creation-routing-note/reference.md'))
          .map(([, value]) => JSON.parse(value).content)
      );
      expect(drafts).toContain('# Recovered routing draft\n');
    }
  });
for (const failed of [false, true])
  test(
    'creation recovers after ' + (failed ? 'a failed initial folder load' : 'loading an empty notebook'),
    async ({page}) => {
      const id = 'creation-empty-' + failed,
        name = 'Empty creation ' + failed;
      await api('/api/projects', {id, name});
      await login(page);
      let release,
        waiting = false;
      const gate = new Promise(resolve => (release = resolve));
      await page.route(
        url =>
          url.pathname === '/p/' + id + '/api/browse' &&
          url.searchParams.get('path') === (failed ? 'missing' : '') &&
          url.searchParams.get('collection') === 'notes',
        async route => {
          waiting = true;
          await gate;
          if (failed) await route.fulfill({status: 404, json: {error: 'Synthetic missing initial folder'}});
          else await route.continue();
        }
      );
      try {
        await page.goto(origin + '/?project=' + id + (failed ? '&folder=missing' : ''));
        await expect.poll(() => waiting).toBe(true);
        await expectCreationPending(page);
      } finally {
        release();
      }
      if (failed) {
        await expect(page.locator('#notice')).toContainText('Synthetic missing initial folder');
        await expectCreationPending(page);
        await page.locator('#browse-root').click();
      }
      await expect(page.locator('#folder-browser')).toBeVisible();
      await expect(page.locator('#path')).toHaveText(name.replace(/\.md$/i, ''));
      await page.locator('#create-toggle').click();
      const source = page.locator('#create-source');
      await source.click();
      await expect(page.locator('#archive-folder')).toHaveValue('');
      await page.locator('#creation-close').click();
      await clickNewNote(page);
      await page.locator('#new-name').fill('First note');
      await expect(page.locator('#new-folder')).toHaveValue('Top level');
      await page.locator('#new-submit').click();
      await expect(page.locator('#note-path')).toHaveText('First note.md');
      await page.locator('#format-source').click();
      await fillSource(page, '# First note\n');
      await page.locator('#save').click();
      await expect(page.locator('#save-state')).toContainText('Saved');
      expect((await api('/p/' + id + '/api/note?path=First%20note.md')).content).toBe('# First note\n');
    }
  );
test('creation follows the selected notebook when an earlier initial route finishes late', async ({page}) => {
  const first = 'creation-switch-first',
    second = 'creation-switch-second';
  for (const id of [first, second]) {
    await api('/api/projects', {id, name: id});
    await api('/p/' + id + '/api/note', {
      path: 'old-folder/reference.md',
      content: '# Switch reference\n',
      revision: ''
    });
  }
  let releaseFirst,
    releaseSecond,
    firstWaiting = false,
    secondWaiting = false;
  const firstGate = new Promise(resolve => (releaseFirst = resolve)),
    secondGate = new Promise(resolve => (releaseSecond = resolve));
  await page.route('**/p/' + first + '/api/status', async route => {
    firstWaiting = true;
    await firstGate;
    await route.continue();
  });
  await page.route(
    url =>
      url.pathname === '/p/' + second + '/api/browse' &&
      url.searchParams.get('path') === '' &&
      url.searchParams.get('collection') === 'notes',
    async route => {
      secondWaiting = true;
      await secondGate;
      await route.continue();
    }
  );
  await login(page);
  try {
    await page.goto(origin + '/?project=' + first + '&folder=old-folder');
    await expect.poll(() => firstWaiting).toBe(true);
    await expectCreationPending(page);
    await page.locator('#project-select').selectOption(second);
    await expect.poll(() => secondWaiting).toBe(true);
    await expectCreationPending(page);
    const response = page.waitForResponse('**/p/' + first + '/api/status');
    releaseFirst();
    await response;
    await expectCreationPending(page);
  } finally {
    releaseFirst();
    releaseSecond();
  }
  await expect(page.locator('#folder-browser')).toBeVisible();
  await expect(page.locator('#path')).toHaveText(second.replace(/\.md$/i, ''));
  await clickNewNote(page);
  await page.locator('#new-name').fill('Selected notebook');
  await expect(page.locator('#new-folder')).toHaveValue('Top level');
  await page.locator('#new-submit').click();
  await expect(page.locator('#note-path')).toHaveText('Selected notebook.md');
  await expect(page.locator('#project-select')).toHaveValue(second);
});
test('creation tabs retain inputs and create a named note in a searched folder', async ({page}) => {
  await api('/p/default/api/note', {path: 'creation/research/reference.md', content: '# Reference\n', revision: ''});
  await login(page);
  await page.goto(origin + '/?project=default&folder=creation');
  await clickNewNote(page);
  await expect(page.getByRole('tab', {name: 'Note', exact: true})).toHaveAttribute('aria-selected', 'true');
  await page.getByLabel('Note name', {exact: true}).fill('My idea');
  const folder = page.getByRole('combobox', {name: 'Folder', exact: true});
  await expect(folder).toHaveValue('creation');
  await folder.fill('research');
  await page.getByRole('option', {name: 'creation/research', exact: true}).click();
  await page.getByRole('tab', {name: 'Import source', exact: true}).click();
  await page
    .locator('#archive-file')
    .setInputFiles({name: 'tab-proof.csv', mimeType: 'text/csv', buffer: Buffer.from('idea,value\nsource-tab,42\n')});
  await page.getByRole('tab', {name: 'Note', exact: true}).click();
  await expect(page.getByLabel('Note name', {exact: true})).toHaveValue('My idea');
  await expect(folder).toHaveValue('creation/research');
  await page.getByRole('tab', {name: 'Import source', exact: true}).click();
  expect(await page.locator('#archive-file').evaluate(input => input.files[0].name)).toBe('tab-proof.csv');
  await page.locator('#archive-preview-button').click();
  await expect(page.locator('#archive-preview-text')).toContainText('source-tab');
  await page.getByRole('tab', {name: 'Note', exact: true}).click();
  await page.getByRole('button', {name: 'Start writing', exact: true}).click();
  await expect(page.locator('#new-dialog')).toBeHidden();
  await expect(page.locator('#note-path')).toHaveText('creation/research/My idea.md');
  await page.locator('#format-source').click();
  await fillSource(page, '# My new idea\n');
  await page.locator('#save').click();
  await expect(page.locator('#save-state')).toContainText('Saved');
  expect((await api('/p/default/api/note?path=creation%2Fresearch%2FMy%20idea.md')).content).toBe('# My new idea\n');
  await ensureSearch(page);
  await expect(page.locator('#search-options-toggle')).toContainText('Filters');
  await expect(page.locator('#search-options-toggle svg')).toBeVisible();
  await expect(page.locator('#create-actions-toggle')).toHaveCount(0);
  await expect(page.locator('#connection')).toBeHidden();
  const layout = await page.locator('#account-actions-toggle').evaluate(button => {
    const buttonBox = button.getBoundingClientRect(),
      name = button.querySelector('span').getBoundingClientRect(),
      arrow = button.querySelector('svg:last-child').getBoundingClientRect();
    return {
      vertical: Math.abs(name.y + name.height / 2 - arrow.y - arrow.height / 2),
      right: buttonBox.right - arrow.right
    };
  });
  expect(layout.vertical).toBeLessThan(2);
  expect(layout.right).toBeLessThan(20);
});
test('sidebar discloses search options and menus without losing search state or drafts', async ({page}) => {
  await api('/p/default/api/note', {
    path: 'sidebar/proof.md',
    content: '---\ntags: [sidebar-proof]\n---\n# Sidebar proof\nA quiet navigation fixture.\n',
    revision: ''
  });
  await login(page);
  await page.goto(origin + '/?project=default&note=sidebar%2Fproof.md');
  await edit(page);
  await fillSource(page, '# Keep this sidebar draft\n');
  const options = page.getByRole('button', {name: 'Search filters', exact: true});
  await ensureSearch(page);
  await expect(options).toBeVisible();
  await expect(page.locator('#note-filters')).toBeHidden();
  await expect(page.locator('#settings-open')).toBeVisible();
  await expect(page.locator('#account-open')).toBeHidden();
  await expect(page.locator('#new-dialog')).toBeHidden();
  await options.click();
  await page.locator('#advanced-search summary').click();
  await page.locator('#note-filters').getByLabel('Include previous versions', {exact: true}).check();
  if (!(await page.locator('input[name=search-mode][value=text]').isVisible()))
    await page.locator('#advanced-search summary').click();
  await page.locator('input[name=search-mode][value=text]').check();
  await page.locator('#tag-filter').selectOption('sidebar-proof');
  await options.click();
  await expect(page.locator('#active-search-options')).toContainText('Previous versions');
  await expect(page.locator('#active-search-options')).toContainText('Text');
  await expect(page.locator('#active-search-options')).toContainText('sidebar-proof');
  await page.getByRole('button', {name: 'Remove Previous versions', exact: true}).click();
  await expect(page.locator('#search-scope')).not.toBeChecked();
  await page.getByRole('button', {name: 'Remove Tag: sidebar-proof', exact: true}).click();
  await ensureSearch(page);
  await page.locator('#search').fill('never-present-sidebar-phrase');
  await page.getByRole('button', {name: 'Search previous versions', exact: true}).click();
  await expect(page.locator('#search-scope')).toBeChecked();
  await expect(page.locator('#active-search-options')).toContainText('Previous versions');

  await page.locator('#settings-open').click();
  await expect(page.locator('#settings-dialog')).toBeVisible();
  await page.keyboard.press('Escape');
  await expect(page.locator('#settings-open')).toBeFocused();
  await page.locator('#account-actions-toggle').click();
  await expect(page.locator('#account-open')).toBeVisible();
  await expect(page.locator('#admin-open')).toBeVisible();
  await page.locator('#search').click();
  await expect(page.locator('#account-open')).toBeHidden();
  await clickNewNote(page);
  await expect(page.locator('#new-dialog')).toBeVisible();
  await page.keyboard.press('Escape');
  await expectEditorText(page, '# Keep this sidebar draft\n');
  await page.setViewportSize({width: 390, height: 844});
  await page.locator('#menu').click();
  await clickNewNote(page);
  await page.getByRole('tab', {name: 'Import source', exact: true}).click();
  await expect(page.locator('#new-source-panel')).toBeVisible();
  await page.locator('#creation-close').click();
  await expect(page.locator('#new-dialog')).toBeHidden();
  await expectEditorText(page, '# Keep this sidebar draft\n');
  await ensureSearch(page);
  await options.click();
  await expect
    .poll(async () => {
      const box = await page.locator('#note-filters').boundingBox();
      return box.x + box.width;
    })
    .toBeLessThanOrEqual(390);
  await page.screenshot({path: test.info().outputPath('sidebar-options-mobile.png')});
});
test('creation errors keep the current draft and mobile folder selection is keyboard accessible', async ({page}) => {
  await api('/p/default/api/note', {path: 'creation-safety/existing.md', content: '# Existing\n', revision: ''});
  await login(page);
  await page.goto(origin + '/?project=default&note=creation-safety%2Fexisting.md');
  await edit(page);
  await fillSource(page, '# Keep my existing draft\n');
  await clickNewNote(page);
  await page.keyboard.press('Control+s');
  expect((await api('/p/default/api/note?path=creation-safety%2Fexisting.md')).content).toBe('# Existing\n');
  await page.locator('#new-name').fill('existing.md');
  await page.locator('#new-submit').click();
  await expect(page.locator('#new-error')).toContainText('already exists');
  await expectEditorText(page, '# Keep my existing draft\n');
  await page.locator('#new-name').fill('../not-a-name');
  await page.locator('#new-submit').click();
  await expect(page.locator('#new-error')).toContainText('without slashes');
  await page.locator('#new-name').fill('A root idea.md');
  await page.locator('#new-folder').fill('no-such-folder');
  await expect(page.locator('#new-folder-options')).toContainText('No matching folders');
  await page.locator('#new-submit').click();
  await expect(page.locator('#new-error')).toContainText('Choose a destination folder');
  await page.setViewportSize({width: 390, height: 844});
  await page.locator('#new-folder').fill('top');
  await page.locator('#new-folder').press('ArrowDown');
  await page.locator('#new-folder').press('Enter');
  await expect(page.locator('#new-folder')).toHaveValue('Top level');
  await expect(page.locator('#new-folder-options')).toBeHidden();
  await page.locator('#new-folder-toggle').click();
  await page.locator('#new-folder').press('Escape');
  await expect(page.locator('#new-dialog')).toBeVisible();
  await expect(page.locator('#new-folder-options')).toBeHidden();
  await page.locator('#create-note-tab').focus();
  await page.keyboard.press('ArrowRight');
  await expect(page.locator('#create-source-tab')).toHaveAttribute('aria-selected', 'true');
  await page.screenshot({path: test.info().outputPath('create-source-mobile.png')});
  await page.keyboard.press('ArrowLeft');
  await expect(page.locator('#new-name')).toHaveValue('A root idea.md');
  await page.screenshot({path: test.info().outputPath('create-note-mobile.png')});
  await page.locator('#new-submit').click();
  await expect(page.locator('#note-path')).toHaveText('A root idea.md');
  await expect(page.locator('#new-dialog')).toBeHidden();
  expect(await page.locator('#main').evaluate(main => main.inert)).toBe(false);
  const drafts = await page.evaluate(() =>
    Object.entries(localStorage)
      .filter(([key]) => key.endsWith('creation-safety/existing.md'))
      .map(([, value]) => JSON.parse(value).content)
  );
  expect(drafts).toContain('# Keep my existing draft\n');
});
test('creation refuses locked destinations and reports a failed folder load', async ({page}) => {
  const note = await api('/p/default/api/note', {
    path: 'creation-locked/existing.md',
    content: '# Protected\n',
    revision: ''
  });
  await api('/p/default/api/locks', {path: 'creation-locked', locked: true, revision: note.revision});
  await login(page);
  await page.goto(origin + '/?project=default&folder=creation-locked');
  await expect(page.locator('#folder-browser')).toBeVisible();
  await expect(page.locator('#path')).toHaveText('creation-locked'.replace(/\.md$/i, ''));
  await clickNewNote(page);
  await page.locator('#new-name').fill('Another note');
  await page.locator('#new-submit').click();
  await expect(page.locator('#new-error')).toContainText('read-only');
  await expect(page.locator('#new-dialog')).toBeVisible();
  await page.locator('#creation-close').click();
  await page.route('**/api/notes', route => route.abort());
  await clickNewNote(page);
  await expect(page.locator('#new-folder-status')).toContainText('Could not load folders');
  await expect(page.locator('#new-submit')).toBeDisabled();
  await page.locator('#creation-close').click();
  await page.unroute('**/api/notes');
});
test('pinned Markdown source links retain their page revision collection and private draft in new tabs', async ({
  page,
  context
}) => {
  const source = 'pinned-source.md';
  const html =
    '<html><body><h1>Reference pages</h1><h2>Page 1</h2>' +
    '<p>Earlier page context.</p>'.repeat(30) +
    '<h2>Page 3</h2><p>Earlier pinned bytes.</p></body></html>';
  const prepared = await api('/p/default/api/sources/preview', {
    filename: 'reference.html',
    data: Buffer.from(html).toString('base64')
  });
  const old = await api('/p/default/api/sources/import', {prepared, title: 'Earlier version'}),
    target = old.path;
  expect(old.metadata.source).toBeTruthy();
  await api('/p/default/api/note', {
    path: target,
    content: old.content.replace('Earlier pinned bytes', 'Current target bytes'),
    revision: old.revision
  });
  async function expectPageTarget(tab) {
    await expect(tab.locator('.citation-source')).toContainText('Earlier pinned bytes');
    await expect(tab.locator('.citation-source #h-page-3')).toBeInViewport();
    await expect(tab.locator('.citation-source #h-page-1')).not.toBeInViewport();
  }
  const archived = '---\nfmn-container: archive\n---\n';
  const base = await api('/p/default/api/note', {
    path: source,
    content: archived + '# Original source\n',
    revision: ''
  });
  await api('/p/default/api/note', {path: source, content: archived + '# Current source\n', revision: base.revision});
  const draft = archived + '# Private source draft\n' + sourceCitation(old, 'page-3') + '\n';
  await login(page);
  await privateDraft(page, base, draft);
  await page.goto(origin + '/?project=default&collection=archives&note=' + source);
  await edit(page);
  await expectEditorText(page, draft);
  await page.locator('#mode').click();
  const link = page.locator('#preview').getByRole('link', {name: 'Earlier version'});
  await expect(link).toBeVisible();
  const href = await link.getAttribute('href'),
    url = new URL(href, origin);
  expect(url.searchParams.get('note')).toBe(target);
  expect(url.searchParams.get('revision')).toBe(old.revision);
  expect(url.searchParams.get('collection')).toBe('archives');
  expect(url.hash).toBe('#page-3');
  const copied = await context.newPage();
  try {
    await copied.goto(url.href);
    await expectPageTarget(copied);
    expect(new URL(copied.url()).hash).toBe('#page-3');
    await expect(copied.locator('#document')).toBeHidden();
    await expect(copied.locator('#collection-archives')).toHaveAttribute('aria-pressed', 'true');
  } finally {
    await copied.close();
  }
  // A modified click belongs to the browser, which opens the link in a
  // background tab without an opener. When Chromium is slow to create that
  // tab's first document, Playwright reads an empty frame tree, waits for a
  // navigation event Chromium never sends for such tabs, and never reports the
  // page. Chromium's own target list shows the tab and its URL either way; the
  // copied tab above checks what that URL shows.
  const chromium = await context.browser().newBrowserCDPSession();
  try {
    await chromium.send('Target.setDiscoverTargets', {discover: true});
    const existing = new Set((await chromium.send('Target.getTargets')).targetInfos.map(target => target.targetId));
    const opened = new Map();
    const record = ({targetInfo}) => {
      if (targetInfo.type === 'page' && !existing.has(targetInfo.targetId))
        opened.set(targetInfo.targetId, targetInfo.url);
    };
    chromium.on('Target.targetCreated', record);
    chromium.on('Target.targetInfoChanged', record);
    await link.click({modifiers: ['ControlOrMeta']});
    await expect.poll(() => [...opened.values()]).toContain(url.href);
    await expect(page.locator('#knowledge-dialog')).toBeHidden();
    for (const targetId of opened.keys()) await chromium.send('Target.closeTarget', {targetId});
  } finally {
    await chromium.detach();
  }
  await link.click();
  await expectPageTarget(page);
  await page.locator('[data-close="knowledge-dialog"]').click();
  await edit(page);
  await expectEditorText(page, draft);
  expect(await retainedTexts(page, source)).toContain(draft);
  expect((await api('/p/default/api/note?path=' + source)).content).toBe(archived + '# Current source\n');
});
test('current note heading links retain their destination on load and reload', async ({page}) => {
  const path = 'heading-target.md';
  await api('/p/default/api/note', {
    path,
    content:
      '# Heading target\n\n' + 'Context paragraph.\n\n'.repeat(30) + '## Selected heading\n\nDestination text.\n',
    revision: ''
  });
  await login(page);
  await page.goto(origin + '/?project=default&collection=archives&note=' + path + '#selected-heading');
  for (const reload of [false, true]) {
    if (reload) await page.reload();
    await expect(page.locator('#preview #h-selected-heading')).toBeInViewport();
    await expect(page.locator('#preview #h-heading-target')).not.toBeInViewport();
    expect(new URL(page.url()).hash).toBe('#selected-heading');
    await expect(page.locator('#collection-archives')).toHaveAttribute('aria-pressed', 'true');
  }
});
test('historical search previews, copies and compares removed passages without changing a draft', async ({
  page,
  context
}) => {
  // A history scan started by earlier tests may still target older commits.
  // This preview fixture uses its own notebook with two complete versions.
  const project = 'history-preview',
    base = '/p/' + project + '/api/';
  await api('/api/projects', {id: project, name: 'History preview'});
  const old = await api(base + 'note', {
    path: 'notes/history-search.md',
    content: '# Historical search\nAn extinct cobalt lantern.\n',
    revision: ''
  });
  const saved = await api(base + 'note', {
    path: old.path,
    content: '# Historical search\nToday has a copper lantern.\n',
    revision: old.revision
  });
  await login(page);
  await privateDraft(page, saved, '# My unsaved historical search draft\n', project);
  await page.goto(origin + '/?project=' + project + '&note=notes%2Fhistory-search.md');
  await edit(page);
  await fillSource(page, '# My unsaved historical search draft\n');
  await ensureSearch(page);
  await page.getByRole('button', {name: 'Search filters', exact: true}).click();
  await expect(page.locator('#search-scope')).not.toBeChecked();
  if (!(await page.locator('input[name=search-mode][value=text]').isVisible()))
    await page.locator('#advanced-search summary').click();
  await page.locator('input[name=search-mode][value=text]').check();
  await ensureSearch(page);
  await page.locator('#search').fill('extinct cobalt');
  await expect(page.locator('#search-status')).toContainText('0 text matches');
  if (!(await page.locator('#search-scope').isVisible())) await page.locator('#advanced-search summary').click();
  await page.locator('#search-scope').check();
  const result = page.locator('#search-results a[data-revision]');
  await expect(result).toHaveCount(1);
  await expect(result).toHaveAttribute('data-revision', old.revision);
  const href = await result.getAttribute('href');
  await result.click();
  await expect(page.locator('#knowledge-dialog')).toBeVisible();
  await expect(page.locator('#historical-passage')).toContainText('extinct cobalt');
  await context.grantPermissions(['clipboard-read', 'clipboard-write']);
  await withClipboard(async () => {
    await page.locator('#historical-copy').click();
    await expect(page.locator('#knowledge-dialog [role=status]')).toHaveText('Copied.');
    expect(await page.evaluate(() => navigator.clipboard.readText())).toContain('extinct cobalt');
  });
  await page.locator('#historical-compare').click();
  await expect(page.locator('#historical-comparison')).toContainText('copper lantern');
  await expectEditorText(page, '# My unsaved historical search draft\n');
  expect((await api(base + 'note?path=' + encodeURIComponent(old.path))).content).toContain('copper lantern');
  await page.locator('[data-close="knowledge-dialog"]').click();
  await expectEditorText(page, '# My unsaved historical search draft\n');
  const tab = await context.newPage();
  await tab.goto(origin + href);
  await expect(tab.locator('#knowledge-dialog')).toBeVisible();
  await expect(tab.locator('.citation-content')).toContainText('extinct cobalt');
  await expect(tab.locator('#document')).not.toBeVisible();
  const current = await api(base + 'note?path=' + encodeURIComponent(old.path));
  await api(base + 'move', {from: old.path, to: 'notes/history-moved.md', revision: current.revision});
  await tab.reload();
  await expect(tab.locator('.citation-content')).toContainText('extinct cobalt');
  await expect(tab.locator('#knowledge-body')).toContainText('No current note is available');
  await expect(tab.locator('#historical-compare')).toBeDisabled();
  await tab.close();
});
test('sidebar filters work without a query and reset on notebook switch', async ({page}) => {
  await api('/p/default/api/note', {path: 'sidebar-filter/proof.md', content: '# Filter fixture', revision: ''});
  await api('/api/projects', {id: 'sidebar-other', name: 'Sidebar other'});
  await api('/p/sidebar-other/api/note', {path: 'other.md', content: '# Other notebook', revision: ''});
  await login(page);
  await page.goto(origin + '/?project=default');
  await ensureSearch(page);
  await page.locator('#search-options-toggle').click();
  await page.locator('#folder-filter').selectOption('sidebar-filter/');
  await expect(page.locator('#search-results a[data-note]')).toHaveCount(1);
  await expect(page.locator('#search-results a[data-note]')).toHaveAttribute('data-note', 'sidebar-filter/proof.md');
  await ensureSearch(page);
  await page.locator('#search-options-toggle').click();
  await expect(page.locator('#active-search-options')).toContainText('Folder: sidebar-filter');
  await page.screenshot({path: test.info().outputPath('sidebar-filtered-desktop.png')});
  await page.locator('#project-select').selectOption('sidebar-other');
  await expect(page.locator('#folder-browser')).toBeVisible();
  await expect(page.locator('#folder-items a[data-note]')).toHaveCount(1);
  await expect(page.locator('#folder-items a[data-note]')).toHaveAttribute('data-note', 'other.md');
  await expect(page.locator('#active-search-options')).toBeHidden();
  await expect(page.locator('#note-filters')).toBeHidden();
  await expect(page.locator('#search-scope')).not.toBeChecked();
  await expect(page.locator('#search-all')).toBeChecked();
  await expect(page.locator('#folder-filter')).toHaveValue('');
  await expect(page.locator('#connection')).toBeHidden();
  await expect(page.locator('#connection')).toHaveAttribute('aria-label', /Local repository/);
  await page.screenshot({path: test.info().outputPath('sidebar-resting-desktop.png')});
});
test('sidebar review shows open items and failures without inventing a healthy count', async ({page}) => {
  let review = {
    findings: [{comment: {resolved: false}}, {comment: {resolved: true}}],
    proposals: [{status: 'proposed'}, {status: 'dismissed'}, {status: 'applied'}]
  };
  await page.route('**/api/knowledge/review', route => route.fulfill({json: review}));
  await login(page);
  await page.goto(origin + '/?project=default');
  await expect(page.locator('#review-count')).toHaveText('2');
  review = {findings: [], proposals: [], checkpoints: []};
  await page.locator('#review-open').click();
  await expect(page.locator('#knowledge-dialog')).toBeVisible();
  await expect(page.locator('#review-count')).toBeHidden();
  await page.locator('[data-close="knowledge-dialog"]').click();
  await expect(page.locator('#knowledge-dialog')).toBeHidden();
  await expect(page.locator('#settings-open')).toBeEnabled();
  await page.locator('#settings-open').focus();
  await expect(page.locator('#settings-open')).toBeFocused();
  await page.keyboard.press('Enter');
  await expect(page.locator('#settings-dialog')).toBeVisible();
  await page.keyboard.press('Escape');
  await expect(page.locator('#settings-open')).toBeFocused();
  await page.unroute('**/api/knowledge/review');
  await page.route('**/api/knowledge/review', route => route.fulfill({status: 503, body: 'temporarily unavailable'}));
  await page.reload();
  await expect(page.locator('#review-count')).toHaveText('unavailable');
  await page.route('**/api/status', route => route.abort());
  await page.reload();
  await expect(page.locator('#connection')).toHaveText('Could not check notebook connection');
  await expect(page.locator('#connection')).not.toHaveClass(/healthy/);
});
test('the connection indicator reports going offline and checks again when back online', async ({page, context}) => {
  await login(page);
  await page.goto(origin + '/?project=default');
  await expect(page.locator('#connection')).toHaveAttribute('aria-label', /Local repository/);
  await expect(page.locator('#connection')).toBeHidden();
  await context.setOffline(true);
  await expect(page.locator('#connection')).toHaveText('Offline · drafts remain on this device');
  await expect(page.locator('#connection')).toHaveClass(/problem/);
  const checked = page.waitForResponse(response => new URL(response.url()).pathname === '/p/default/api/status');
  await context.setOffline(false);
  await checked;
  await expect(page.locator('#connection')).toBeHidden();
  await expect(page.locator('#connection')).toHaveAttribute('aria-label', /Local repository/);
});
