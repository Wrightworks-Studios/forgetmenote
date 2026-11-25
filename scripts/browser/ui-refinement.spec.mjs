import {test, expect} from '@playwright/test';
import {startApp} from './app-fixture.mjs';
import {expectEditorText, fillSource} from './editor-text.mjs';
let app;
const account = {username: 'ui-refinement-admin', password: 'DisposableRefinement123!'};
test.beforeAll(async () => {
  app = await startApp({name: 'forgetmenote-ui-refinement-', account});
});
test.afterAll(async () => {
  await app?.close();
});
test.beforeEach(async ({page}) => {
  page.on('pageerror', error => {
    throw error;
  });
  await app.login(page, 'password');
});
test('Inbox is a persistent destination and creation separates private capture from notebook writing', async ({
  page
}) => {
  await page.goto(app.origin + '/?project=default&folder=');
  await expect(page.getByRole('button', {name: 'Inbox', exact: true})).toBeVisible();
  await expect(page.locator('#count-inbox')).toBeHidden();
  await page.getByRole('button', {name: 'More creation options', exact: true}).click();
  await page.getByRole('button', {name: 'Quick note', exact: true}).click();
  const dialog = page.getByRole('dialog', {name: 'Quick note', exact: true});
  await dialog
    .getByRole('textbox', {name: 'Quick note', exact: true})
    .fill('# Refined capture\nA synthetic Inbox note.');
  await dialog.getByRole('button', {name: 'Save to Inbox', exact: true}).click();
  await expect(dialog).toContainText('Saved privately');
  await dialog.getByRole('button', {name: 'Close', exact: true}).click();
  await page.getByRole('button', {name: 'Inbox 1', exact: true}).click();
  await expect(page.locator('#inbox-view')).toBeVisible();
  await expect(page.locator('#inbox-view')).toContainText('Refined capture');
  await expect(page.locator('#project-select option')).not.toContainText(['Quicknotes Inbox']);
  await page.reload();
  await expect(page.locator('#inbox-view')).toBeVisible();
  await page.setViewportSize({width: 390, height: 844});
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  await page.screenshot({path: '/tmp/ui-refinement-inbox-mobile.png'});
  await page.locator('#inbox-view').getByRole('link', {name: 'Open note', exact: true}).first().click();
  await expect(page.locator('#path')).toHaveText('Refined capture');
  await expect(page.locator('#note-path')).toContainText('.md');
  await expect(page.locator('#chat-scope')).toContainText('Quicknotes Inbox');
  await expect(page.locator('#inbox-open')).toHaveAttribute('aria-pressed', 'true');
  await page.locator('#menu').click();
  await page.locator('#inbox-open').click();
  await expect(page.locator('#sidebar [aria-current=page]')).toHaveCount(0);
});
test('Chat restores visible and hidden sidebar state and keeps mode beside composer', async ({page}) => {
  await page.goto(app.origin + '/?project=default&folder=');
  await expect(page.locator('#sidebar')).toBeVisible();
  await page.locator('#chat-open').click();
  await expect(page.locator('#chat-form #chat-mode')).toBeVisible();
  await expect(page.locator('#chat-form #chat-scope')).toContainText('Notebook');
  await page.locator('#chat-open').click();
  await expect(page.locator('#sidebar')).toBeVisible();
  await page.getByRole('button', {name: 'Hide sidebar', exact: true}).click();
  await page.locator('#chat-open').click();
  await page.locator('#chat-open').click();
  await expect(page.locator('#sidebar')).toBeHidden();
  await page.setViewportSize({width: 390, height: 844});
  await page.locator('#chat-open').click();
  await page.screenshot({path: '/tmp/ui-refinement-chat-mobile.png'});
  const chatBox = await page.locator('#chat-open').boundingBox();
  expect(chatBox.y).toBeLessThan(70);
});
test('display title, Details, creation and search keep the current private draft intact at phone width', async ({
  page
}) => {
  const path = 'refinement/filename.md';
  await app.api('/p/default/api/note', {
    path,
    content:
      '---\ntitle: Displayed idea\ntags: [refinement]\n---\n# Heading\nA **distinctive phrase** and [readable link](https://example.test).',
    revision: ''
  });
  await page.setViewportSize({width: 390, height: 844});
  await page.goto(app.origin + '/?project=default&note=' + encodeURIComponent(path));
  await expect(page.locator('#path')).toHaveText('Displayed idea');
  await expect(page.locator('#note-path')).toHaveText(path);
  await page.locator('#note-details-toggle').click();
  await expect(page.locator('#properties-open')).toBeVisible();
  await expect(page.locator('#filename-open')).toHaveText('Rename…');
  await page.keyboard.press('Escape');
  await expect(page.locator('#note-details-toggle')).toBeFocused();
  await page.locator('#mode').click();
  await page.locator('#format-source').click();
  // An unavailable live transport must leave this draft local while search runs.
  await page.route('**/api/live', route => route.abort());
  await fillSource(page, 'Private typing remains here');
  await ensureSearch(page);
  await page.locator('#search').fill('distinctive phrase');
  await expect(page.locator('#search-view')).toBeVisible();
  await ensureSearch(page);
  await expect(page.locator('#search-view')).toBeVisible();
  await expect(page.locator('#search-results')).toContainText('distinctive phrase');
  await expect(page.locator('#search-results .search-excerpt')).not.toContainText('**');
  await expect(page.locator('#search-results .search-excerpt')).not.toContainText('title:');
  await page.screenshot({path: '/tmp/ui-refinement-search-mobile.png'});
  await page.locator('#search-close').click();
  await expectEditorText(page, 'Private typing remains here');
  await page.screenshot({path: '/tmp/ui-refinement-header-mobile.png'});
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
});
test('Review uses one dialog with sections and settings keep their scopes', async ({page}) => {
  await page.goto(app.origin + '/?project=default&folder=');
  await page.locator('#review-open').click();
  const review = page.getByRole('dialog', {name: 'Review', exact: true});
  await expect(review).toBeVisible();
  await review.getByRole('button', {name: 'Proposed changes', exact: true}).click();
  await expect(review).toContainText('No proposed groups');
  await review.getByRole('button', {name: 'Notebook checks', exact: true}).click();
  await expect(review).toContainText('Consistency');
  await expect(page.locator('dialog[open]')).toHaveCount(1);
  await page.screenshot({path: '/tmp/ui-refinement-review-desktop.png'});
  await review.getByRole('button', {name: 'Findings', exact: true}).click();
  await expect(review).toContainText('No findings');
  await review.getByRole('button', {name: 'Close', exact: true}).click();
  await page.locator('#settings-open').click();
  await page.getByRole('button', {name: 'Search defaults', exact: true}).click();
  await expect(page.locator('#retrieval-defaults-form')).toBeVisible();
  await expect(page.locator('#project-members')).not.toBeVisible();
  await page.locator('[data-close="settings-dialog"]').click();
  await page.locator('#account-actions-toggle').click();
  await page.locator('#account-open').click();
  await expect(page.locator('#theme')).toBeVisible();
  await page.getByRole('button', {name: 'Account', exact: true}).click();
  await expect(page.locator('#account-summary')).toBeVisible();
  await expect(page.locator('#theme')).toBeHidden();
  await page.setViewportSize({width: 390, height: 844});
  await page.screenshot({path: '/tmp/ui-refinement-settings-mobile.png'});
  expect(await page.locator('#account-dialog').evaluate(el => el.scrollWidth <= el.clientWidth)).toBe(true);
});
test('Inbox remains available without notebook grants and does not provision a notebook on read', async ({browser}) => {
  const username = 'inbox-only-user',
    password = 'DisposableInboxOnly123!';
  await app.api('/api/local-users', {username, password});
  const context = await browser.newContext(),
    page = await context.newPage();
  try {
    await page.request.post(app.origin + '/auth/password/login', {
      form: {username, password},
      headers: {Origin: app.origin}
    });
    await page.goto(app.origin + '/?area=inbox');
    await expect(page.locator('#inbox-view')).toBeVisible();
    await expect(page.locator('#inbox-view')).toContainText('Your Inbox is empty');
    expect(await (await page.request.get(app.origin + '/api/projects')).json()).toEqual([]);
    await page.locator('#inbox-capture').click();
    await page.locator('#quicknotes-text').fill('No notebook grant needed for private capture');
    await page.getByRole('button', {name: 'Save to Inbox', exact: true}).click();
    await expect(page.locator('#quicknotes-dialog')).toContainText('Saved privately');
    await page.reload();
    await expect(page.locator('#inbox-view')).toContainText('No notebook grant needed');
    await expect(page.locator('#new-note')).toBeDisabled();
    await page.locator('#inbox-view').getByRole('link', {name: 'Open note', exact: true}).click();
    await expect(page.locator('#path')).toHaveText('No notebook grant needed for private capture');
    await expect(page.locator('#sidebar a[data-note]')).toHaveCount(0);
    await expect(page.locator('#preview')).toContainText('No notebook grant needed for private capture');
  } finally {
    await context.close();
  }
});

test('capture Escape restores the visible launch control on desktop and phone', async ({page}) => {
  await page.goto(app.origin + '/?project=default&folder=');
  await page.locator('#create-toggle').click();
  await page.locator('#quicknotes-open').click();
  await page.keyboard.press('Escape');
  await expect(page.locator('#create-toggle')).toBeFocused();
  await page.setViewportSize({width: 390, height: 844});
  await page.locator('#menu').click();
  await page.locator('#inbox-open').click();
  await page.locator('#inbox-capture').click();
  await page.keyboard.press('Escape');
  await expect(page.locator('#inbox-capture')).toBeFocused();
  await page.locator('#menu').click();
  await page.locator('#browse-root').click();
  await page.locator('#create-toggle').click();
  await page.locator('#quicknotes-open').click();
  await page.keyboard.press('Escape');
  await expect(page.locator('#create-toggle')).toBeFocused();
});

async function ensureSearch(page) {
  if (await page.locator('#search-view').isHidden()) {
    if (await page.locator('#sidebar').isHidden()) await page.locator('#menu').click();
    await page.locator('#search-open').click();
  } else if (page.viewportSize()?.width <= 720 && (await page.locator('#sidebar').isVisible()))
    await page.locator('#menu').click();
}
