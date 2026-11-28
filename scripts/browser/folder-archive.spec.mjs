import {test, expect} from '@playwright/test';
import {startApp} from './app-fixture.mjs';

let app;
const account = {username: 'folder-archive-admin', password: 'DisposableFolderArchive123!'};
test.beforeAll(async () => {
  app = await startApp({name: 'forgetmenote-folder-archive-', account});
  for (const path of [
    'Season/Plan.md',
    'Season/Budget.md',
    'Kept.md',
    'Retired/One.md',
    'Retired/Two.md',
    'Shelf/Old.md',
    'Vault/Kept.md'
  ])
    await app.api('/p/default/api/note', {path, content: '# ' + path + '\n\nSynthetic note.\n', revision: ''});
});
test.afterAll(async () => {
  await app?.close();
});

test('a folder moves to Archives and back from its actions menu', async ({page}) => {
  page.on('pageerror', error => {
    throw error;
  });
  await app.login(page, 'password');
  await page.goto(app.origin + '/?project=default&folder=');
  const folder = page.locator('#folder-groups .browse-item').filter({hasText: 'Season'});
  await expect(folder).toBeVisible();
  const row = await folder.boundingBox();
  await folder.hover();
  const menu = folder.locator('.tree-menu');
  const button = await menu.boundingBox();
  // The actions button is centred on the row, level with its chevron.
  expect(Math.abs(button.y + button.height / 2 - (row.y + row.height / 2))).toBeLessThanOrEqual(1);
  await menu.click();
  await expect(page.getByRole('menuitem', {name: 'Return to Notes'})).toBeHidden();
  await page.getByRole('menuitem', {name: 'Move to Archives'}).click();
  await expect(page.locator('[data-confirm-action]')).toContainText('Move 2 notes in “Season” to Archives?');
  await page.getByRole('button', {name: 'Continue'}).click();
  await expect(page.getByText('Moved 2 notes to Archives.')).toBeVisible();
  // A completed move is a success notice that dismisses itself.
  await expect(page.locator('#feedback-tray .toast')).toHaveAttribute('data-level', 'success');
  await expect(page.locator('#notice')).toBeHidden({timeout: 8000});
  await expect(folder).toBeHidden();
  await expect(page.locator('#folder-items')).toContainText('Kept');

  await page.locator('#collection-archives').click();
  await expect(folder).toBeVisible();
  await folder.hover();
  await folder.locator('.tree-menu').click();
  await expect(page.getByRole('menuitem', {name: 'Move to Archives'})).toBeHidden();
  await page.getByRole('menuitem', {name: 'Return to Notes'}).click();
  await page.getByRole('button', {name: 'Continue'}).click();
  await expect(page.getByText('Moved 2 notes to Notes.')).toBeVisible();
  await expect(folder).toBeHidden();
});

test('folder actions are grouped with icons and a folder moves to Trash with its note count', async ({page}) => {
  page.on('pageerror', error => {
    throw error;
  });
  await app.login(page, 'password');
  await page.goto(app.origin + '/?project=default&folder=');
  const folder = page.locator('#folder-groups .browse-item').filter({hasText: 'Retired'});
  await folder.hover();
  // The dots draw at the chevron's heavier weight.
  expect(await folder.locator('.tree-menu svg').evaluate(icon => getComputedStyle(icon).strokeWidth)).toBe('3px');
  await folder.locator('.tree-menu').click();
  const items = page.locator('#file-menu [role=menuitem]:visible');
  await expect(items).toHaveText([
    'Rename…',
    'Move to folder…',
    'Duplicate…',
    'Move to Drafts',
    'Move to Archives',
    'Lock…',
    'Move to Trash…'
  ]);
  for (const item of await items.all()) await expect(item.locator('svg')).toHaveCount(1);
  const trash = page.getByRole('menuitem', {name: 'Move to Trash…'});
  await expect(trash).toBeEnabled();
  // Trash sits in its own group, set apart by a rule.
  expect(await trash.evaluate(item => getComputedStyle(item.parentElement).borderTopWidth)).toBe('1px');
  await trash.click();
  await expect(page.locator('[data-confirm-action]')).toContainText('Move the 2 notes in “Retired” to Trash?');
  await page.getByRole('button', {name: 'Continue'}).click();
  await expect(page.getByText('Moved 2 notes to Trash.')).toBeVisible();
  await expect(folder).toBeHidden();
  const paths = async () => (await app.api('/p/default/api/notes')).map(entry => entry.path);
  expect(await paths()).not.toContain('Retired/One.md');
  await page.locator('#feedback-tray').getByRole('button', {name: 'Undo move to Trash'}).click();
  await expect.poll(paths).toEqual(expect.arrayContaining(['Retired/One.md', 'Retired/Two.md']));
});

test('Archives with only folders shows no empty Notes section and offers no New note', async ({page}) => {
  const status = await app.api('/p/default/api/status');
  await app.api('/p/default/api/folder-container', {folder: 'Shelf', container: 'archive', revision: status.revision});
  await app.login(page, 'password');
  await page.goto(app.origin + '/?project=default&folder=');
  await expect(page.locator('#new-note')).toBeVisible();
  await page.locator('#collection-archives').click();
  await expect(page.locator('#folder-groups')).toContainText('Shelf');
  await expect(page.locator('#library-notes-label')).toBeHidden();
  await expect(page.locator('#folder-empty')).toBeHidden();
  await expect(page.locator('#new-note')).toBeHidden();
  // The selected collection keeps its tint while hovered.
  await page.locator('#collection-archives').hover();
  const selected = await page.locator('#collection-archives').evaluate(n => getComputedStyle(n).backgroundColor);
  await page.mouse.move(900, 900);
  await expect
    .poll(() => page.locator('#collection-archives').evaluate(n => getComputedStyle(n).backgroundColor))
    .toBe(selected);
});

test('a locked folder explains why its notes cannot change collection', async ({page}) => {
  const status = await app.api('/p/default/api/status');
  await app.api('/p/default/api/locks', {path: 'Vault', locked: true, revision: status.revision});
  await app.login(page, 'password');
  await page.goto(app.origin + '/?project=default&folder=');
  const folder = page.locator('#folder-groups .browse-item').filter({hasText: 'Vault'});
  await folder.hover();
  await folder.locator('.tree-menu').click();
  await expect(page.getByRole('menuitem', {name: 'Move to Archives'})).toBeDisabled();
  await expect(page.locator('#file-menu-collection-hint')).toHaveText('Unlock this folder to move its notes.');
  await expect(page.getByRole('menuitem', {name: 'Unlock…'})).toBeVisible();
});
