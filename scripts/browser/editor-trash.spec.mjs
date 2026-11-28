import {test, expect} from '@playwright/test';
// Each worker owns an app; baseline reset makes cases independent within it.
test.describe.configure({mode: 'parallel'});
import {watchConfirmations} from './app-fixture.mjs';
import {
  origin,
  chooseHistory,
  api,
  login,
  edit,
  clickNoteAction,
  ensureSearch,
  clickNotebookAction,
  useEditorApp
} from './editor-fixture.mjs';
import {expectEditorText, fillSource} from './editor-text.mjs';
useEditorApp();
test('Trash supports Undo restore search opt-in and confirmed empty without erasing history', async ({page}) => {
  const path = 'trash-ui.md',
    content = '# Trash UI proof\nUnmistakable trash phrase\n';
  const original = await api('/p/default/api/note', {path, content, revision: ''});
  await login(page);
  await page.goto(origin + '/?project=default&note=' + path);
  await expect(page.locator('#trash-open')).toBeVisible({timeout: 2000});
  let release, seen;
  const held = new Promise(resolve => (release = resolve)),
    started = new Promise(resolve => (seen = resolve));
  await page.route('**/api/browse?*', async route => {
    seen();
    await held;
    await route.continue();
  });
  try {
    await clickNoteAction(page, 'properties-open');
    await page.locator('#note-trash').click();
    await started;
    // Undo is offered once the notebook shows the move.
    await expect(page.locator('#feedback-tray').getByRole('button', {name: 'Undo move to Trash'})).toHaveCount(0);
  } finally {
    release();
    await page.unrouteAll({behavior: 'wait'});
  }
  await expect(page.locator('#notice')).toHaveText('Moved to Trash.');
  await expect(page.locator('#feedback-tray').getByRole('button', {name: 'Undo move to Trash'})).toBeVisible();
  await expect(page.locator('#sidebar a[data-note="' + path + '"]').first()).toHaveCount(0);
  await page.locator('#feedback-tray').getByRole('button', {name: 'Undo move to Trash'}).click();
  await expect(page.locator('#notice')).toHaveText('Restored ' + path + '.');
  await page.locator('#browse-root').click();
  await expect(page.locator('#folder-items a[data-note="' + path + '"]').first()).toBeVisible();
  await page.locator('#folder-items a[data-note="' + path + '"]').click();
  await clickNoteAction(page, 'properties-open');
  await page.locator('#note-trash').click();
  await expect(page.locator('#notice')).toHaveText('Moved to Trash.');
  await ensureSearch(page);
  await page.locator('#search').fill('Unmistakable trash phrase');
  await expect(page.locator('#search-results')).toContainText('No matching notes');
  await ensureSearch(page);
  await page.locator('#search-options-toggle').click();
  await expect(page.locator('#history-search-hint')).toHaveCount(0);
  if (!(await page.locator('#include-trash').isVisible())) await page.locator('#advanced-search summary').click();
  await page.locator('#include-trash').check();
  await expect(page.locator('#search-results')).toContainText('Trash UI proof');
  await page.locator('#search-results a').filter({hasText: 'Trash UI proof'}).click();
  await expect(page.locator('#trash-dialog')).toBeVisible();
  await expect(page.locator('#trash-preview')).toContainText('Unmistakable trash phrase');
  await page.locator('#trash-empty').click();
  await expect(page.locator('#trash-empty-dialog')).toBeVisible();
  await expect(page.locator('#trash-empty-question')).toContainText('1 note');
  await page.locator('#trash-empty-cancel').click();
  expect((await api('/p/default/api/trash')).entries).toHaveLength(1);
  await page.locator('#trash-empty').click();
  await page.locator('#trash-empty-confirm').click();
  await expect(page.locator('#trash-list')).toContainText('Trash is empty');
  expect((await api('/p/default/api/note?path=' + path + '&revision=' + original.revision)).content).toBe(content);
});

test('Trash refuses stale empty confirmation and restore collisions', async ({page}) => {
  const paths = ['trash-stale-a.md', 'trash-stale-b.md'];
  for (const path of paths) await api('/p/default/api/note', {path, content: '# ' + path, revision: ''});
  let state = await api('/p/default/api/status');
  await api('/p/default/api/trash', {paths: [paths[0]], revision: state.revision});
  await login(page);
  await page.goto(origin + '/?project=default');
  await clickNotebookAction(page, 'trash-open');
  await page.locator('#trash-empty').click();
  await expect(page.locator('#trash-empty-dialog')).toBeVisible();
  state = await api('/p/default/api/status');
  await api('/p/default/api/trash', {paths: [paths[1]], revision: state.revision});
  await page.locator('#trash-empty-confirm').click();
  await expect(page.locator('#trash-empty-error')).toContainText('Cancel and review');
  expect((await api('/p/default/api/trash')).entries).toHaveLength(2);
  await page.locator('#trash-empty-cancel').click();
  await api('/p/default/api/note', {path: paths[0], content: '# New occupant', revision: ''});
  await page.locator('#trash-reload').click();
  await page.locator('#trash-list').getByRole('button', {name: paths[0], exact: true}).click();
  await page.locator('#trash-restore').click();
  await expect(page.locator('#trash-error')).toContainText('occupied');
  expect((await api('/p/default/api/note?path=' + paths[0])).content).toBe('# New occupant');
  await page.locator('#trash-destination').fill('trash-restored-alternate.md');
  await page.locator('#trash-restore').click();
  await expect(page.locator('#trash-list')).not.toContainText(paths[0]);
  expect((await api('/p/default/api/note?path=trash-restored-alternate.md')).content).toBe('# ' + paths[0]);
  await page.locator('#trash-empty').click();
  await page.locator('#trash-empty-confirm').click();
  await expect(page.locator('#trash-list')).toContainText('Trash is empty');
});

test('Trash blocks deletion of private drafts and readers can only inspect', async ({page, browser}) => {
  const path = 'trash-private.md';
  const doc = await api('/p/default/api/note', {path, content: '# Original private test', revision: ''});
  await api('/p/default/api/note', {path, content: '# Current private test', revision: doc.revision});
  await login(page);
  await watchConfirmations(page, d => d.accept());
  await page.goto(origin + '/?project=default&note=' + path);
  await edit(page);
  await chooseHistory(page, doc.revision);
  await fillSource(page, '# Unsaved private text');
  await clickNoteAction(page, 'properties-open');
  await page.locator('#note-trash').click();
  await expect(page.locator('#properties-error')).toContainText('Save or reconcile');
  await page.locator('[data-close=properties-dialog]').click();
  await expectEditorText(page, '# Unsaved private text');
  expect((await api('/p/default/api/trash')).entries).toHaveLength(0);
  const state = await api('/p/default/api/status');
  await api('/p/default/api/trash', {paths: [path], revision: state.revision});
  const password = 'DisposableTrashReader123!',
    reader = await api('/api/local-users', {username: 'trash-reader', password});
  await api('/api/members', {project: 'default', user: reader.id, role: 'read'});
  const context = await browser.newContext({viewport: {width: 390, height: 844}, hasTouch: true, isMobile: true});
  const readPage = await context.newPage();
  try {
    await readPage.request.post(origin + '/auth/password/login', {
      form: {username: 'trash-reader', password},
      headers: {Origin: origin}
    });
    await readPage.goto(origin + '/?project=default');
    await readPage.locator('#menu').tap();
    await readPage.locator('#trash-open').tap();
    await readPage.locator('#trash-list button').first().tap();
    await expect(readPage.locator('#trash-preview')).toContainText('Current private test');
    await expect(readPage.locator('#trash-empty')).toBeHidden();
    await expect(readPage.locator('#trash-restore-fields')).toBeHidden();
    await readPage.screenshot({path: test.info().outputPath('trash-reader-mobile.png')});
  } finally {
    await context.close();
  }
});

test('Trash folder in unfiltered opt-in results opens the managed view with readable titles', async ({page}) => {
  const path = 'trash-folder-entry.md',
    doc = await api('/p/default/api/note', {path, content: '# Friendly Trash title\nFolder entry proof', revision: ''});
  await api('/p/default/api/trash', {paths: [path], revision: doc.revision});
  await login(page);
  await page.goto(origin + '/?project=default');
  await ensureSearch(page);
  await page.locator('#search-options-toggle').click();
  if (!(await page.locator('#include-trash').isVisible())) await page.locator('#advanced-search summary').click();
  await page.locator('#include-trash').check();
  const result = page.locator('#search-results a').filter({hasText: 'Friendly Trash title'});
  await expect(result).toBeVisible({timeout: 2000});
  await result.click();
  await expect(page.locator('#trash-dialog')).toBeVisible({timeout: 2000});
  await expect(page.locator('#trash-list')).toContainText('Friendly Trash title');
});

test('mobile sidebar opened during initial folder loading stays open', async ({browser}) => {
  const context = await browser.newContext({viewport: {width: 390, height: 844}, hasTouch: true, isMobile: true}),
    page = await context.newPage();
  let release;
  const gate = new Promise(r => (release = r));
  let entered;
  const requested = new Promise(r => (entered = r));
  try {
    await login(page);
    await page.route(
      url =>
        url.pathname.endsWith('/api/browse') &&
        url.searchParams.get('path') === '' &&
        url.searchParams.get('collection') === 'notes',
      async route => {
        entered();
        await gate;
        await route.continue();
      }
    );
    await page.goto(origin + '/?project=default');
    await requested;
    await page.locator('#menu').tap();
    await expect(page.locator('#sidebar')).toBeVisible();
    release();
    await expect(page.locator('#folder-browser')).toBeVisible();
    await expect(page.locator('#sidebar')).toBeVisible();
    await clickNotebookAction(page, 'trash-open');
    await expect(page.locator('#trash-dialog')).toBeVisible();
  } finally {
    release?.();
    await context.close();
  }
});

test('deletion proposals require Owner permission in the review UI', async ({page, browser}) => {
  const password = 'DisposableProposalEditor123!',
    editor = await api('/api/local-users', {username: 'proposal-editor', password});
  await api('/api/members', {project: 'default', user: editor.id, role: 'edit'});
  const proposals = [false, true].map(deleting => ({
    id: deleting ? 'remove' : 'edit',
    status: 'proposed',
    author: 'another-user',
    provenance: {name: 'Reviewer'},
    group: {
      summary: deleting ? 'Remove note' : 'Revise note',
      edits: [{path: 'proposal.md', delete: deleting, content: 'New content'}]
    },
    before: {'proposal.md': 'Old content'}
  }));
  const review = {findings: [], proposals, checkpoints: []},
    context = await browser.newContext(),
    person = await context.newPage();
  try {
    await person.route('**/api/knowledge/review', route => route.fulfill({json: review}));
    await person.request.post(origin + '/auth/password/login', {
      form: {username: 'proposal-editor', password},
      headers: {Origin: origin}
    });
    await person.goto(origin + '/?project=default');
    await person.locator('#review-open').click();
    await person.locator('[data-review-section=proposals]').click();
    const cards = person.locator('#knowledge-dialog .knowledge-card');
    await expect(
      cards.filter({hasText: 'Revise note'}).getByRole('button', {name: 'Accept group', exact: true})
    ).toBeVisible();
    await expect(
      cards.filter({hasText: 'Remove note'}).getByRole('button', {name: 'Accept group', exact: true})
    ).toHaveCount(0);
    await expect(cards.filter({hasText: 'Remove note'})).toContainText('Owner or Admin');
    await page.route('**/api/knowledge/review', route => route.fulfill({json: review}));
    await login(page);
    await page.goto(origin + '/?project=default');
    await page.locator('#review-open').click();
    await page.locator('[data-review-section=proposals]').click();
    await expect(
      page.locator('#knowledge-dialog').getByRole('button', {name: 'Accept group', exact: true})
    ).toHaveCount(2);
  } finally {
    await context.close();
  }
});

for (const width of [320, 390, 1280])
  test(`Trash modal actions share one row and the close sits in the header at ${width}px`, async ({page}) => {
    await page.setViewportSize({width, height: 900});
    await login(page);
    await page.goto(origin + '/?project=default');
    if (width < 760) await page.locator('#menu').click();
    await clickNotebookAction(page, 'trash-open');
    await expect(page.locator('#trash-dialog')).toBeVisible();
    await expect(page.locator('#trash-dialog .dialog-title #trash-close')).toBeVisible();
    const boxes = await page.locator('#trash-close, .trash-actions button:visible').evaluateAll(buttons =>
      buttons.map(button => {
        const r = button.getBoundingClientRect();
        return {top: r.top, left: r.left, right: r.right};
      })
    );
    expect(boxes.length).toBeGreaterThanOrEqual(2);
    const actions = boxes.slice(1);
    expect(Math.max(...actions.map(b => b.top)) - Math.min(...actions.map(b => b.top))).toBeLessThan(2);
    for (const box of boxes) {
      expect(box.left).toBeGreaterThanOrEqual(0);
      expect(box.right).toBeLessThanOrEqual(width);
    }
    expect(await page.locator('#trash-dialog').evaluate(el => el.scrollWidth <= el.clientWidth)).toBe(true);
    await page.screenshot({path: test.info().outputPath(`trash-actions-${width}.png`)});
  });

test('Properties moves to Trash while preserving private drafts and Undo', async ({page}) => {
  const path = 'toolbar-trash.md',
    first = await api('/p/default/api/note', {path, content: '# Earlier version', revision: ''});
  await api('/p/default/api/note', {path, content: '# Current version', revision: first.revision});
  await login(page);
  await watchConfirmations(page, dialog => dialog.accept());
  await page.goto(origin + '/?project=default&note=' + path);
  await edit(page);
  await expect(page.locator('header #note-trash')).toHaveCount(0);
  await chooseHistory(page, first.revision);
  await fillSource(page, '# Private text');
  await clickNoteAction(page, 'properties-open');
  await page.locator('#properties-dialog #note-trash').click();
  await expect(page.locator('#properties-error')).toContainText('Save or reconcile');
  await expectEditorText(page, '# Private text');
  expect((await api('/p/default/api/trash')).entries.filter(entry => entry.originalPath === path)).toHaveLength(0);
  await expect(page.locator('#properties-dialog')).toBeVisible();
  await page.locator('[data-close=properties-dialog]').click();
  await page.reload();
  await edit(page);
  await page.locator('#draft-compare').click();
  await page.locator('#reconcile-discard').click();
  await expectEditorText(page, '# Current version');
  await clickNoteAction(page, 'properties-open');
  await page.locator('#properties-dialog #note-trash').click();
  await expect(page.locator('#properties-dialog')).toBeHidden();
  await expect(page.locator('#folder-browser')).toBeVisible();
  await expect(page.locator('#note-trash')).toBeHidden();
  expect((await api('/p/default/api/trash')).entries.map(e => e.originalPath)).toContain(path);
  await page.locator('#feedback-tray').getByRole('button', {name: 'Undo move to Trash'}).click();
  await expect
    .poll(async () => (await api('/p/default/api/trash')).entries.filter(entry => entry.originalPath === path).length)
    .toBe(0);
  expect((await api('/p/default/api/note?path=' + path)).content).toBe('# Current version');
});

test('Properties Trash respects locks and Owner permission', async ({page, browser}) => {
  const path = 'toolbar-locked.md',
    note = await api('/p/default/api/note', {path, content: '# Protected note', revision: ''});
  await api('/p/default/api/locks', {path, locked: true, revision: note.revision});
  await login(page);
  await page.goto(origin + '/?project=default&note=' + path);
  await clickNoteAction(page, 'properties-open');
  await expect(page.locator('#properties-dialog #note-trash')).toBeVisible();
  await expect(page.locator('#note-trash')).toBeDisabled();
  await expect(page.locator('#filename-open')).toBeHidden();
  const state = await api('/p/default/api/status');
  await api('/p/default/api/locks', {path, locked: false, revision: state.revision});
  await page.setViewportSize({width: 390, height: 900});
  await page.reload();
  await edit(page);
  await expect(page.locator('#editor')).toBeVisible();
  await expectEditorText(page, '# Protected note');
  const firstLine = await page
    .locator('#editor')
    .evaluate(el => el.getBoundingClientRect().top + parseFloat(getComputedStyle(el).paddingTop));
  const toolbarBottom = await page.locator('#editor-tools').evaluate(el => el.getBoundingClientRect().bottom);
  expect(firstLine).toBeGreaterThanOrEqual(toolbarBottom + 8);
  await clickNoteAction(page, 'properties-open');
  await expect(page.locator('#properties-dialog #note-trash')).toBeEnabled();
  await page.screenshot({path: test.info().outputPath('properties-trash-mobile.png')});
  const password = 'DisposableToolbarEditor123!',
    editor = await api('/api/local-users', {username: 'toolbar-editor', password});
  await api('/api/members', {project: 'default', user: editor.id, role: 'edit'});
  const context = await browser.newContext(),
    person = await context.newPage();
  try {
    await person.request.post(origin + '/auth/password/login', {
      form: {username: 'toolbar-editor', password},
      headers: {Origin: origin}
    });
    await person.goto(origin + '/?project=default&note=' + path);
    await expect(person.locator('#note-path')).toHaveText(path);
    await clickNoteAction(person, 'properties-open');
    await expect(person.locator('#note-trash')).toBeHidden();
    await expect(person.locator('#filename-open')).toBeHidden();
  } finally {
    await context.close();
  }
});

test('Properties red Trash button keeps unsaved properties and does not submit the form', async ({page}) => {
  const path = 'properties-trash-draft.md';
  await api('/p/default/api/note', {path, content: '# Keep this note', revision: ''});
  await page.setViewportSize({width: 320, height: 900});
  await login(page);
  await page.goto(origin + '/?project=default&note=' + path);
  await clickNoteAction(page, 'properties-open');
  const button = page.locator('#properties-dialog #note-trash');
  await expect(button).toHaveText('Move to Trash');
  await expect(button).toHaveAttribute('type', 'button');
  const color = await button.evaluate(el =>
    getComputedStyle(el)
      .backgroundColor.match(/[\d.]+/g)
      .slice(0, 3)
      .map(Number)
  );
  expect(color[0]).toBeGreaterThan(color[1] * 1.4);
  expect(color[0]).toBeGreaterThan(color[2] * 1.4);
  await page.locator('#property-title').fill('Unsaved property title');
  await button.click();
  await expect(page.locator('#properties-error')).toContainText('Save your property changes');
  await expect(page.locator('#property-title')).toHaveValue('Unsaved property title');
  await expect(page.locator('#property-title')).toBeEnabled();
  expect((await api('/p/default/api/note?path=' + path)).content).toBe('# Keep this note');
  expect(await page.locator('#properties-dialog').evaluate(el => el.scrollWidth <= el.clientWidth)).toBe(true);
  await page.screenshot({path: test.info().outputPath('properties-trash-unsaved-mobile.png')});
  await page.locator('#property-title').fill('');
  await button.click();
  await expect(page.locator('#properties-dialog')).toBeHidden();
  const listing = await api('/p/default/api/trash'),
    entry = listing.entries.find(e => e.originalPath === path);
  expect(entry).toBeTruthy();
  expect((await api('/p/default/api/note?path=' + encodeURIComponent(entry.path))).content).toBe('# Keep this note');
});
