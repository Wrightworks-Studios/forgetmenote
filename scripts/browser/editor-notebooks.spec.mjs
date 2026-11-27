import {expectEditorText, fillSource} from './editor-text.mjs';
import {test, expect} from '@playwright/test';
// Each worker owns an app; baseline reset makes cases independent within it.
test.describe.configure({mode: 'parallel'});
import {watchConfirmations} from './app-fixture.mjs';
import {
  origin,
  api,
  login,
  edit,
  privateDraft,
  clickNoteAction,
  clickNewNote,
  ensureSearch,
  useEditorApp
} from './editor-fixture.mjs';
useEditorApp();
test('Reader sidebar preserves access restrictions and touch menu dismissal', async ({browser}) => {
  const password = 'DisposableSidebarPassword123!';
  const reader = await api('/api/local-users', {username: 'sidebar-reader', password});
  await api('/api/members', {project: 'default', user: reader.id, role: 'read'});
  const context = await browser.newContext({viewport: {width: 390, height: 844}, hasTouch: true, isMobile: true});
  const page = await context.newPage();
  page.setDefaultTimeout(10000);
  try {
    const signedIn = await page.request.post(origin + '/auth/password/login', {
      form: {username: 'sidebar-reader', password},
      headers: {Origin: origin}
    });
    expect(signedIn.ok()).toBe(true);
    await page.goto(origin + '/?project=default');
    await page.locator('#menu').tap();
    await expect(page.locator('#new-note')).toBeDisabled();
    await expect(page.locator('#create-actions-toggle')).toHaveCount(0);
    await expect(page.locator('#project-select option[value="@import"]')).toHaveCount(0);
    await page.locator('#account-actions-toggle').tap();
    await expect(page.locator('#account-open')).toBeVisible();
    await expect(page.locator('#admin-open')).toBeHidden();
    await page.keyboard.press('Escape');
    await expect(page.locator('#sidebar')).toBeVisible();
    await expect(page.locator('#account-open')).toBeHidden();

    await expect(page.locator('#review-open')).toBeVisible();
    await page.locator('#sidebar').tap({position: {x: 4, y: 300}});
    await expect(page.locator('#review-open')).toBeVisible();
    await ensureSearch(page);
    await page.locator('#search-options-toggle').tap();
    if (!(await page.locator('#search-scope').isVisible())) await page.locator('#advanced-search summary').click();
    await page.locator('#search-scope').check();
    await page.locator('#search-scope').press('Escape');
    await expect(page.locator('#note-filters')).toBeHidden();
    await expect(page.locator('#sidebar')).toBeHidden();
    await expect(page.locator('#active-search-options')).toContainText('Previous versions');
    await page.screenshot({path: test.info().outputPath('sidebar-reader-mobile.png')});
  } finally {
    await context.close().catch(() => {});
  }
});
test('private recovered drafts can reconcile, reload safely, and report committed saves after refresh failure', async ({
  browser
}) => {
  const initial = await api('/p/default/api/note', {path: 'notes/conflict.md', content: '# Initial\n', revision: ''});
  const a = await browser.newContext(),
    b = await browser.newContext();
  const first = await a.newPage(),
    second = await b.newPage();
  await watchConfirmations(first, d => d.accept());
  await watchConfirmations(second, d => d.accept());
  try {
    await login(first);
    await login(second);
    await privateDraft(first, initial, '# First writer\n');
    await privateDraft(second, initial, '# My draft\n');
    const url = origin + '/?project=default&note=notes%2Fconflict.md';
    await first.goto(url);
    await second.goto(url);
    await edit(first);
    await edit(second);

    await fillSource(first, '# First writer\n');
    await fillSource(second, '# My draft\n');
    await first.locator('#save').click();
    await expect(first.locator('#save-state')).toContainText('Saved');
    await second.locator('#save').click();
    await expect(second.locator('#draft-compare')).toBeVisible();
    await second.reload();
    await expectEditorText(second, '# My draft\n');
    await second.locator('#draft-compare').click();
    await expect(second.locator('#reconcile-latest')).toHaveValue('# First writer\n');
    await second.locator('#reconcile-content').fill('# Both writers\n');
    await second.locator('#reconcile-close').click();
    await second.reload();
    await second.locator('#draft-compare').click();
    await expect(second.locator('#reconcile-content')).toHaveValue('# Both writers\n');
    await second.locator('#reconcile-apply').click();
    await expect(second.locator('#reconcile-dialog')).not.toBeVisible();
    await second.route('**/api/locks?*', route => route.abort());
    await second.locator('#save').click();
    await expect(second.locator('#save-state')).toContainText('Saved version · view refresh failed');
    expect((await api('/p/default/api/note?path=notes/conflict.md')).content).toBe('# Both writers\n');
    await second.unroute('**/api/locks?*');
    await second.reload();
    await expect(second.locator('#preview')).toContainText('Both writers');
  } finally {
    await a.close().catch(() => {});
    await b.close().catch(() => {});
  }
});

test('source import retains original and searchable text through the browser', async ({page}) => {
  await login(page);
  await page.goto(origin + '/?project=default');
  await clickNewNote(page);
  await page.getByRole('tab', {name: 'Import source', exact: true}).click();
  await page
    .locator('#archive-file')
    .setInputFiles({name: 'figure.png', mimeType: 'image/png', buffer: Buffer.from('selection fixture')});
  await expect(page.locator('#archive-formats')).toContainText('64 MiB');
  await page.screenshot({path: test.info().outputPath('source-options-desktop.png')});
  const viewport = page.viewportSize();
  await page.setViewportSize({width: 390, height: 844});
  await page.screenshot({path: test.info().outputPath('source-options-mobile.png')});
  await page.setViewportSize(viewport);
  await page
    .locator('#archive-file')
    .setInputFiles({name: 'proof.csv', mimeType: 'text/csv', buffer: Buffer.from('name,value\nmeasurement,42\n')});
  const submissions = [];
  page.on('request', request => {
    if (request.method() === 'POST' && request.url().endsWith('/api/sources/jobs'))
      submissions.push(request.postDataJSON());
  });
  const queued = page.waitForResponse(
    response => response.request().method() === 'POST' && response.url().endsWith('/api/sources/jobs')
  );
  await page.locator('#archive-preview-button').click();
  const job = (await (await queued).json()).job;
  await expect(page.locator('#archive-preview-text')).toContainText('measurement');
  await expect(page.locator('#archive-job-status')).toContainText('Ready');
  // A browser reload reuses the durable preview without a second upload.
  await page.reload();
  await clickNewNote(page);
  await page.getByRole('tab', {name: 'Import source', exact: true}).click();
  await expect(page.locator('#archive-preview-text')).toContainText('measurement');
  await expect(page.locator('#archive-save')).toBeVisible();
  expect(submissions).toHaveLength(1);
  const saved = page.waitForRequest(
    request => request.method() === 'POST' && request.url().endsWith('/api/sources/import')
  );
  await page.locator('#archive-title').fill('Browser source proof');
  await page.locator('#archive-save').click();
  const saveInput = (await saved).postDataJSON();
  expect(saveInput.job).toBe(job.id);
  expect(saveInput.data).toBeUndefined();
  expect(saveInput.prepared).toBeUndefined();
  await expect(page.locator('#archive-saved')).toBeVisible();
  await page.locator('#archive-view').click();
  await expect(page.locator('#preview')).toContainText('measurement');
  const entries = await api('/p/default/api/notes');
  const record = entries.find(n => n.metadata?.title === 'Browser source proof');
  expect(record.metadata.source).toBeTruthy();
  const note = await api('/p/default/api/note?path=' + encodeURIComponent(record.path));
  const response = await page.request.get(
    origin + `/p/default/assets/${record.metadata.source.original}?revision=${note.revision}`
  );
  expect(response.status()).toBe(200);
  expect((await response.body()).toString()).toBe('name,value\nmeasurement,42\n');
  await page.locator('#archive-details > summary').click();
  await page.getByRole('button', {name: 'Update source', exact: true}).click();
  await expect(page.locator('#creation-tabs')).toBeHidden();
  await expect(page.locator('#creation-title')).toHaveText('Capture a new version');
  await expect(page.locator('#archive-folder-field')).toBeHidden();
  await page.locator('#archive-file').setInputFiles({
    name: 'proof.csv',
    mimeType: 'text/csv',
    buffer: Buffer.from('name,value\nupdated-measurement,43\n')
  });
  await page.locator('#archive-preview-button').click();
  await expect(page.locator('#archive-preview-text')).toContainText('updated-measurement');
  await page.locator('#archive-save').click();
  await expect(page.locator('#archive-saved')).toBeVisible();
  await page.locator('#archive-view').click();
  await expect(page.locator('#preview')).toContainText('updated-measurement');
  const updated = await api('/p/default/api/note?path=' + encodeURIComponent(record.path));
  expect(updated.revision).not.toBe(note.revision);
});

test('notebook bundles import through the UI with history and useful errors', async ({page}) => {
  await api('/api/projects', {id: 'export-fixture', name: 'Export fixture'});
  const original = await api('/p/export-fixture/api/note', {
    path: 'notes/import-proof.md',
    content: '# Browser bundle proof\n',
    revision: ''
  });
  await login(page);
  await page.goto(origin + '/?project=default');
  await expect(page.locator('#project-select')).toHaveAttribute('aria-label', 'Notebook');
  await page.locator('#notebook-actions-toggle').click();
  await page.locator('#notebook-create').click();
  await page.locator('#notebook-import-mode').click();
  await expect(page.locator('#workspace-create-title')).toHaveText('Import notebook');
  await page.locator('#project-name').fill('Restored notebook');
  await page
    .locator('#notebook-bundle')
    .setInputFiles({name: 'bad.bundle', mimeType: 'application/octet-stream', buffer: Buffer.from('invalid bundle')});
  await page.locator('#notebook-submit').click();
  await expect(page.locator('#workspace-create-status')).toContainText('complete Git bundle');
  await expect(page.locator('#project-name')).toHaveValue('Restored notebook');
  const exported = await page.request.get(origin + '/p/export-fixture/api/backup');
  expect(exported.ok()).toBe(true);
  await page
    .locator('#notebook-bundle')
    .setInputFiles({name: 'notes.bundle', mimeType: 'application/octet-stream', buffer: await exported.body()});
  await expect(page.locator('#workspace-create-status')).toBeEmpty();
  await page.setViewportSize({width: 390, height: 844});
  await page.screenshot({path: test.info().outputPath('import-notebook-mobile.png')});
  const imported = page.waitForResponse(
    response => new URL(response.url()).pathname === '/api/projects/import' && response.request().method() === 'POST',
    {timeout: 30000}
  );
  await page.locator('#notebook-submit').click();
  const importResponse = await imported;
  expect(importResponse.status(), await importResponse.text()).toBe(200);
  await expect(page.locator('#workspace-create-dialog')).not.toBeVisible();
  await expect(page.locator('#project-select')).toHaveValue('restored-notebook');
  const toast = page.locator('#notice');
  await expect(toast).toHaveText('Notebook imported. Your notes and history are ready.');
  await expect(toast).toHaveAttribute('role', 'status');
  await expect(toast).toHaveClass(/\bsuccess\b/);
  for (const [theme, color, background] of [
    ['light', 'rgb(33, 110, 57)', 'rgb(237, 248, 240)'],
    ['dark', 'rgb(139, 223, 170)', 'rgb(32, 62, 48)'],
    ['parchment', 'rgb(71, 102, 56)', 'rgb(231, 239, 217)']
  ]) {
    await page.evaluate(theme => (document.documentElement.dataset.theme = theme), theme);
    await expect(toast).toHaveCSS('color', color);
    await expect(toast).toHaveCSS('border-top-color', color);
    await expect(toast).toHaveCSS('background-color', background);
    await page.screenshot({path: test.info().outputPath(`success-toast-${theme}.png`)});
  }
  await page.route('**/api/admin', route => route.fulfill({status: 500, body: 'Could not load notebook settings.'}));
  await page.locator('#menu').click();

  await page.locator('#settings-open').click();
  await expect(toast).toHaveText('Could not load notebook settings.');
  await expect(toast).toHaveAttribute('role', 'alert');
  await expect(toast).not.toHaveClass(/\bsuccess\b/);
  for (const [theme, color] of [
    ['light', 'rgb(196, 59, 62)'],
    ['dark', 'rgb(242, 146, 148)'],
    ['parchment', 'rgb(164, 67, 55)']
  ]) {
    await page.evaluate(theme => (document.documentElement.dataset.theme = theme), theme);
    await expect(toast).toHaveCSS('color', color);
    await expect(toast).toHaveCSS('border-top-color', color);
  }
  await page.unroute('**/api/admin');
  await page.goto(origin + '/?project=restored-notebook&note=notes%2Fimport-proof.md');
  await expect(page.locator('#preview')).toContainText('Browser bundle proof');
  const restored = await api('/p/restored-notebook/api/note?path=notes/import-proof.md');
  expect(restored.revision).toBe(original.revision);
  await page.locator('#menu').click();
  await page.locator('#notebook-actions-toggle').click();
  await page.locator('#notebook-import').click();
  await expect(page.locator('#notebook-bundle-fields')).toBeVisible();
  await page.locator('#project-name').fill('Restored notebook');
  await page
    .locator('#notebook-bundle')
    .setInputFiles({name: 'notes.bundle', mimeType: 'application/octet-stream', buffer: await exported.body()});
  await page.locator('#notebook-submit').click();
  await expect(page.locator('#workspace-create-status')).toContainText('already exists');
});

test('Admins delegate private workspaces and Owners manage only their people', async ({browser}) => {
  const password = 'DisposableWorkspacePassword123!';
  const owner = await api('/api/local-users', {username: 'workspace-owner', password});
  const reader = await api('/api/local-users', {username: 'workspace-reader', password});
  const a = await browser.newContext(),
    b = await browser.newContext();
  const admin = await a.newPage(),
    person = await b.newPage();
  admin.setDefaultTimeout(10000);
  person.setDefaultTimeout(10000);
  try {
    await login(admin);
    await admin.goto(origin + '/?project=default');
    await admin.locator('#account-actions-toggle').click();
    await admin.locator('#admin-open').click();
    await admin.locator('#user-search').fill('workspace-owner');
    await admin.locator('#access-list button').filter({hasText: 'workspace-owner'}).click();
    await admin.locator('#user-display-name').fill('Workspace Owner');
    await admin.locator('#user-create-workspaces').check();
    await admin.locator('#user-form button[type=submit]').click();
    await expect(admin.locator('#admin-status')).toContainText('saved');
    await admin.screenshot({path: test.info().outputPath('admin-people.png')});
    const signedIn = await person.request.post(origin + '/auth/password/login', {
      form: {username: 'workspace-owner', password},
      headers: {Origin: origin}
    });
    expect(signedIn.ok()).toBe(true);
    await person.goto(origin + '/');
    await expect(person.locator('#admin-open')).not.toBeVisible();
    await person.locator('#notebook-actions-toggle').click();
    await person.locator('#notebook-create').click();
    await expect(person.locator('#instance-people')).not.toBeVisible();
    await person.locator('#project-name').fill('Private research');
    await person.locator('#project-form button[type=submit]').click();
    await expect(person.locator('#settings-dialog')).toBeVisible();
    await expect(person.locator('#member-list')).toContainText('Workspace Owner');
    await person.locator('#member-choose').click();
    await person.locator('#member-search').fill('workspace-reader');
    await expect(person.locator('#member-results')).toContainText('workspace-reader');
    await person.screenshot({path: test.info().outputPath('people-dropdown.png')});
    await person.setViewportSize({width: 390, height: 844});
    await expect
      .poll(async () => {
        const box = await person.locator('#member-picker').boundingBox();
        return box.x + box.width;
      })
      .toBeLessThanOrEqual(390);
    await expect(person.locator('#member-results')).toContainText('workspace-reader');
    await person.screenshot({path: test.info().outputPath('people-dropdown-mobile.png')});
    await person.locator('#member-search').press('ArrowDown');
    await person.keyboard.press('Enter');
    await expect(person.locator('#member-picker')).not.toBeVisible();
    await person.locator('#member-role').selectOption('read');
    await person.locator('#member-form button[type=submit]').click();
    await expect(person.locator('#member-list')).toContainText('workspace-reader');
    await person.evaluate(() => (document.documentElement.dataset.theme = 'dark'));
    await person.screenshot({path: test.info().outputPath('owner-people-dark.png')});
    await person.setViewportSize({width: 390, height: 844});
    await person.screenshot({path: test.info().outputPath('owner-people-mobile.png')});
    await person.setViewportSize({width: 1280, height: 720});
    const row = person.locator('#member-list .person-row').filter({hasText: 'Workspace Owner'});
    await row.locator('select').selectOption('edit');
    await row.getByRole('button', {name: 'Save', exact: true}).click();
    await expect(person.locator('#member-status')).toContainText('another active Owner');
    expect((await api('/api/projects')).some(p => p.id === 'private-research')).toBe(true);
    const response = await person.request.post(origin + '/api/users', {
      data: {id: owner.id, admin: true},
      headers: {Origin: origin}
    });
    expect(response.status()).toBe(403);
    await person.reload();
    await expect(person.locator('#project-select option')).toHaveCount(2);
    expect(
      await person.locator('#project-select option').evaluateAll(options => options.map(option => option.value))
    ).toEqual(['private-research', '']);
    await expect(person.locator('#project-select option[value=""]')).toHaveText('Choose notebook');
    expect(await (await person.request.get(origin + '/api/projects')).json()).toEqual([
      expect.objectContaining({id: 'private-research', name: 'Private research', role: 'owner'})
    ]);
    await admin.locator('#user-disabled').check();
    await admin.locator('#user-form button[type=submit]').click();
    await expect(admin.locator('#admin-status')).toContainText('saved');
    const revoked = await person.request.get(origin + '/api/projects');
    expect(revoked.status()).toBe(401);
  } finally {
    await a.close().catch(() => {});
    await b.close().catch(() => {});
  }
});

test('folder and note menus expose protection without a workspace locks shortcut', async ({page}) => {
  page.setDefaultTimeout(10000);
  for (let i = 0; i < 35; i++)
    await api('/p/default/api/note', {
      path: 'a-protection-scroll-' + String(i).padStart(2, '0') + '/note.md',
      content: '# Sidebar scroll fixture\n',
      revision: ''
    });
  await api('/p/default/api/note', {path: 'protected/example.md', content: '# Protection fixture', revision: ''});
  await login(page);
  await page.goto(origin + '/?project=default&folder=');
  await expect(page.locator('#folder-browser')).toBeVisible();
  await page.locator('#folder-groups').getByRole('button', {name: 'Actions for protected', exact: true}).click();
  await expect(page.locator('[data-file-action="protection"]')).toBeEnabled();
  await expect(page.locator('#file-menu')).toBeVisible();
  await page.evaluate(() => window.scrollBy(0, -40));
  await expect(page.locator('#file-menu')).toBeHidden();
  await page.locator('#folder-groups').getByRole('button', {name: 'Actions for protected', exact: true}).click();
  await page.locator('[data-file-action="protection"]').click();
  await expect(page.locator('#lock-scope')).toHaveValue('protected');
  await page.locator('#lock-change').click();
  await expect(page.locator('#protection-dialog')).not.toBeVisible();
  await page.goto(origin + '/?project=default&note=protected%2Fexample.md');
  await expect(page.locator('#document')).toBeVisible();
  await expect(page.locator('#note-path')).toHaveText('protected/example.md');
  await clickNoteAction(page, 'protection-open');
  await expect(page.locator('#lock-description')).toContainText('inherited');
  await page.locator('#lock-scope').selectOption('protected');
  await page.locator('#lock-change').click();
  await expect(page.locator('#protection-label')).toHaveText('Lock…');
  await page.locator('#settings-open').click();
  await expect(page.locator('#project-locks-open')).toHaveCount(0);
});
