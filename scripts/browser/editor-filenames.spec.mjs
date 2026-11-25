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
  navigateNote,
  useEditorApp
} from './editor-fixture.mjs';
import {expectEditorText, fillSource} from './editor-text.mjs';
useEditorApp();
test('inline filename keeps its folder and display title and updates links', async ({page}) => {
  const path = 'filename-inline/Original.md',
    to = 'filename-inline/Renamed.md';
  await api('/p/default/api/note', {path, content: '---\ntitle: Display name\n---\n# Body\n', revision: ''});
  await api('/p/default/api/note', {path: 'filename-link.md', content: '[Reference](' + path + ')', revision: ''});
  await login(page);
  await page.goto(origin + '/?project=default&note=' + path);
  await expect(page.locator('#save-state')).toContainText('Saved');
  await expect(page.locator('#path')).toHaveText('Display name');
  await clickNoteAction(page, 'filename-open');
  await page.getByRole('textbox', {name: 'Filename', exact: true}).fill('Renamed.md');
  await expect(page.locator('#filename-save')).toBeEnabled();
  const moved = page.waitForResponse(
    response => response.request().method() === 'POST' && new URL(response.url()).pathname === '/p/default/api/move'
  );
  await page.getByRole('textbox', {name: 'Filename', exact: true}).press('Enter');
  const response = await moved;
  expect(response.ok(), await response.text()).toBe(true);
  await expect(page).toHaveURL(new RegExp('note=filename-inline%2FRenamed.md'));
  await expect(page.locator('#path')).toHaveText('Display name');
  expect((await api('/p/default/api/note?path=' + to)).metadata.title).toBe('Display name');
  expect((await api('/p/default/api/note?path=filename-link.md')).content).toContain(to);
  await clickNoteAction(page, 'properties-open');
  await expect(page.getByLabel('Display title (optional)', {exact: true})).toHaveValue('Display name');
});

test('inline filename validates names and cancels without saving on mobile', async ({page}) => {
  const path = 'filename-validate/Original.md';
  await api('/p/default/api/note', {path, content: '# Original', revision: ''});
  await api('/p/default/api/note', {path: 'filename-validate/Taken.md', content: '# Existing', revision: ''});
  await page.setViewportSize({width: 320, height: 900});
  await login(page);
  await page.goto(origin + '/?project=default&note=' + path);
  const open = page.getByRole('button', {name: 'Rename…', exact: true}),
    input = page.getByRole('textbox', {name: 'Filename', exact: true});
  await clickNoteAction(page, 'filename-open');
  await expect(page.locator('#filename-save')).toBeEnabled();
  for (const name of ['../escape', 'other/new', 'Taken']) {
    await input.fill(name);
    await input.press('Enter');
    await expect(page.locator('#filename-error')).not.toBeEmpty();
    expect((await api('/p/default/api/note?path=' + path)).content).toBe('# Original');
  }
  await page.screenshot({path: test.info().outputPath('filename-mobile.png')});
  expect(await page.locator('header').evaluate(el => el.scrollWidth <= el.clientWidth)).toBe(true);
  await input.fill('Canceled');
  await input.press('Escape');
  await expect(input).toBeHidden();
  await expect(page.locator('#note-details-toggle')).toBeFocused();
  await clickNoteAction(page, 'filename-open');
  await input.fill('Also canceled');
  await page.locator('#preview').click({position: {x: 2, y: 2}});
  await expect(input).toBeHidden();
  expect(
    (await api('/p/default/api/notes'))
      .filter(n => n.path.startsWith('filename-validate/'))
      .map(n => n.path)
      .sort()
  ).toEqual(['filename-validate/Original.md', 'filename-validate/Taken.md']);
  await clickNoteAction(page, 'filename-open');
  await input.fill('New name');
  const moved = page.waitForResponse(
    response => response.request().method() === 'POST' && new URL(response.url()).pathname === '/p/default/api/move'
  );
  await page.locator('#filename-save').click();
  const response = await moved;
  expect(response.ok(), await response.text()).toBe(true);
  await expect(page.locator('#note-path')).toHaveText('filename-validate/New name.md');
  await expect(page.locator('#path')).toHaveText('Original');
  await expect(page).toHaveTitle('Original · ForgetMeNote');
  await page.setViewportSize({width: 1280, height: 900});
  await page.screenshot({path: test.info().outputPath('filename-desktop.png')});
});

test('inline filename preserves private drafts and refuses stale revisions', async ({page}) => {
  const path = 'filename-draft.md',
    first = await api('/p/default/api/note', {path, content: '# Earlier', revision: ''});
  await api('/p/default/api/note', {path, content: '# Current', revision: first.revision});
  await login(page);
  await watchConfirmations(page, dialog => dialog.accept());
  await page.goto(origin + '/?project=default&note=' + path);
  await edit(page);
  await chooseHistory(page, first.revision);
  await fillSource(page, '# Private changes');
  await clickNoteAction(page, 'filename-open');
  await page.locator('#filename-input').fill('Lost draft');
  await page.locator('#filename-save').click();
  await expect(page.locator('#filename-error')).toContainText('Save or reconcile');
  await expectEditorText(page, '# Private changes');
  await page.locator('#filename-cancel').click();
  await page.reload();
  await edit(page);
  await expectEditorText(page, '# Private changes');
  await page.locator('#draft-compare').click();
  await page.locator('#reconcile-discard').click();
  await clickNoteAction(page, 'filename-open');
  await expect(page.locator('#filename-save')).toBeEnabled();
  await api('/p/default/api/note', {path: 'filename-concurrent.md', content: '# Concurrent write', revision: ''});
  await page.locator('#filename-input').fill('Stale');
  await page.locator('#filename-save').click();
  await expect(page.locator('#filename-error')).not.toBeEmpty();
  await expect(page.locator('#filename-input')).toHaveValue('Stale');
  expect((await api('/p/default/api/note?path=' + path)).content).toBe('# Current');
});

test('inline filename ignores a pending form after navigation and search-result rename still works', async ({page}) => {
  const path = 'filename-navigation.md',
    other = 'filename-other.md';
  for (const p of [path, other]) await api('/p/default/api/note', {path: p, content: '# ' + p, revision: ''});
  await login(page);
  await page.goto(origin + '/?project=default&note=' + path);
  await page.locator('#note-details-toggle').click();
  await expect(page.locator('#filename-open')).toBeVisible();
  let release;
  const held = new Promise(resolve => (release = resolve));
  await page.route('**/api/status', async route => {
    await held;
    await route.continue();
  });
  await clickNoteAction(page, 'filename-open');
  await expect(page.locator('#filename-save')).toBeDisabled();
  await navigateNote(page, other);
  await expect(page.locator('#note-path')).toHaveText('filename-other.md');
  release();
  await page.unrouteAll({behavior: 'wait'});
  await expect(page.locator('#filename-form')).toBeHidden();
  await ensureSearch(page);
  await page.getByRole('button', {name: 'Actions for ' + other, exact: true}).click();
  await page.locator('[data-file-action=rename]').click();
  await page.locator('#file-name').fill('Sidebar renamed');
  const moved = page.waitForResponse(
    response => response.request().method() === 'POST' && new URL(response.url()).pathname === '/p/default/api/move'
  );
  await page.locator('#file-action-submit').click();
  const response = await moved;
  expect(response.ok(), await response.text()).toBe(true);
  await expect(page.locator('#note-path')).toHaveText('Sidebar renamed.md');
  await expect(page.locator('#path')).toHaveText('filename-other.md');
  expect((await api('/p/default/api/note?path=Sidebar%20renamed.md')).content).toBe('# ' + other);
});
