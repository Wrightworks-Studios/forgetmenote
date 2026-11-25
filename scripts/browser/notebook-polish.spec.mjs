import {test, expect} from '@playwright/test';
import {startApp} from './app-fixture.mjs';
import {AxeBuilder} from '@axe-core/playwright';

let app;
test.beforeAll(async () => {
  app = await startApp({
    name: 'forgetmenote-visual-polish-',
    account: {username: 'polish-admin', password: 'DisposablePolish123!'}
  });
  await app.api('/p/default/api/note', {
    path: 'Reading.md',
    content:
      '# A place for thoughtful work\n\nA notebook keeps ideas close to their sources.\n\n> Read slowly. Keep what matters.\n',
    revision: ''
  });
});
test.afterAll(async () => app?.close());

test('notebook shell has aligned compact navigation and a serif reading default', async ({page}, info) => {
  await app.login(page, 'password');
  await page.setViewportSize({width: 1440, height: 960});
  await page.goto(app.origin + '/?project=default&note=Reading.md');
  await expect(page.locator('#preview h1')).toHaveText('A place for thoughtful work');
  await page.screenshot({path: info.outputPath('reading.png'), fullPage: true});
  await expect(page.locator('html')).toHaveAttribute('data-reading-font', 'serif');
  const labels = await page
    .locator(
      '#sidebar :is(#sidebar-new-note,#search-open,#browse-root,#collection-drafts,#collection-archives,#review-open,#settings-open,#trash-open) > span:first-of-type, #account-name'
    )
    .evaluateAll(nodes => nodes.map(node => node.getBoundingClientRect().x));
  expect(labels).toHaveLength(9);
  expect(Math.max(...labels) - Math.min(...labels)).toBeLessThanOrEqual(1);
  const row = await page.locator('#browse-root').boundingBox();
  expect(row.height).toBeGreaterThanOrEqual(32);
  expect(row.height).toBeLessThanOrEqual(36);
  expect((await page.locator('#main > header').boundingBox()).height).toBeLessThanOrEqual(64);
  await expect(page.locator('#count-drafts')).toBeHidden();
  expect(await page.locator('#browse-root').evaluate(n => getComputedStyle(n).fontWeight)).toBe(
    await page.locator('#collection-drafts').evaluate(n => getComputedStyle(n).fontWeight)
  );
  await page.locator('#sidebar-import-source').click();
  await expect(page.locator('#new-source-panel')).toBeVisible();
});

test('explicit reading preferences persist and touch navigation keeps generous targets', async ({browser}) => {
  const context = await browser.newContext({hasTouch: true, viewport: {width: 1440, height: 960}}),
    page = await context.newPage();
  try {
    await app.login(page, 'password');
    await page.goto(app.origin + '/?project=default');
    await page.evaluate(() => localStorage.setItem('gitnotes:reading-font', 'onest'));
    await page.reload();
    await expect(page.locator('html')).toHaveAttribute('data-reading-font', 'onest');
    await expect(page.locator('#browse-root')).toBeVisible();
    await expect(page.locator('#new-note')).toBeEnabled();
    expect((await page.locator('#browse-root').boundingBox()).height).toBeGreaterThanOrEqual(44);
    expect((await page.locator('#sidebar-import-source').boundingBox()).width).toBeGreaterThanOrEqual(44);
    await page.locator('#account-actions-toggle').click();
    // Measure after the menu's entry slide: a fractional transform can report 43.99997px.
    await page.locator('#account-actions').evaluate(async menu => {
      await Promise.all(menu.getAnimations().map(animation => animation.finished));
    });
    for (const height of await page
      .locator('#account-actions button,#account-actions a')
      .evaluateAll(rows => rows.map(row => row.getBoundingClientRect().height)))
      expect(height).toBeGreaterThanOrEqual(44);
  } finally {
    await context.close();
  }
});

test('Trash undo keeps its own readable surface after the notice expires', async ({page}, info) => {
  const saved = await app.api('/p/default/api/note', {
    path: 'Undo surface.md',
    content: '# Restorable content\n',
    revision: ''
  });
  await app.login(page, 'password');
  await page.goto(app.origin + '/?project=default&note=Undo%20surface.md');
  await page.locator('#note-details-toggle').click();
  await page.locator('#properties-open').click();
  await page.locator('#note-trash').click();
  await expect(page.locator('#trash-undo')).toBeVisible();
  await expect(page.locator('#notice')).toHaveText('Moved to Trash.');
  await expect(page.locator('#notice')).toBeHidden({timeout: 7000});
  expect(await page.locator('#trash-undo').evaluate(n => getComputedStyle(n).backgroundColor)).not.toBe(
    'rgba(0, 0, 0, 0)'
  );
  await page.screenshot({path: info.outputPath('undo-after-notice.png')});
  await page.locator('#trash-undo').click();
  await expect(page.locator('#notice')).toContainText('Restored Undo surface.md.');
  expect((await app.api('/p/default/api/note?path=Undo%20surface.md')).content).toBe(saved.content);
});

test('sign-in stays readable in every theme on desktop and phone', async ({page}, info) => {
  for (const width of [390, 1440])
    for (const theme of ['light', 'dark', 'parchment']) {
      await page.setViewportSize({width, height: 960});
      await page.goto(app.origin + '/login');
      await page.evaluate(theme => (document.documentElement.dataset.theme = theme), theme);
      await expect(page.getByRole('button', {name: 'Sign in', exact: true})).toBeVisible();
      await expect(page.locator('button.primary:visible')).toHaveCount(1);
      const result = await new AxeBuilder({page}).withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa']).analyze();
      expect(result.violations).toEqual([]);
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
      await page.screenshot({path: info.outputPath(theme + '-login-' + width + '.png'), fullPage: true});
    }
});
