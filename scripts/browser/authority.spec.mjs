import {test, expect} from '@playwright/test';
import {startApp} from './app-fixture.mjs';

let app;
const revision = 'a'.repeat(40),
  source = 'b'.repeat(32);
test.beforeAll(async () => {
  app = await startApp({
    name: 'forgetmenote-authority-',
    account: {username: 'authority-admin', password: 'DisposableAuthority123!'}
  });
  await app.api('/api/projects', {id: 'source-book', name: 'Source book', kind: 'repository'});
});
test.afterAll(async () => app?.close());

test('GitHub notebook layout browses containers without showing the retained local tree', async ({page}) => {
  await app.login(page, 'password');
  const localReads = [];
  page.on('request', request => {
    if (/\/p\/source-book\/api\/(notes|collections)(\?|$)/.test(request.url())) localReads.push(request.url());
  });
  await page.route('**/p/source-book/api/repository', route =>
    route.fulfill({
      json: {
        connected: true,
        downloaded: revision,
        repository: {full_name: 'example/notebook', default_branch: 'main'},
        index: {}
      }
    })
  );
  const collections = [];
  await page.route('**/p/source-book/api/documents?*', route => {
    const collection = new URL(route.request().url()).searchParams.get('collection');
    collections.push(collection);
    const path = collection === 'drafts' ? 'Unfinished.md' : collection === 'archives' ? 'Old.md' : 'Guide.md';
    return route.fulfill({
      json: {
        project: 'source-book',
        origin: 'repository',
        notebookLayout: true,
        source,
        revision,
        total: 1,
        entries: [{project: 'source-book', origin: 'repository', source, revision, path, supported: true}]
      }
    });
  });
  await page.goto(app.origin + '/?project=source-book');
  await expect(page.locator('#source-files')).toContainText('Guide.md');
  await expect(page.locator('#collection-drafts')).toBeVisible();
  await page.locator('#collection-drafts').click();
  await expect(page.locator('#source-files')).toContainText('Unfinished.md');
  await expect(page).toHaveURL(/collection=drafts/);
  await page.locator('#source-file-query').fill('Unfinished');
  await expect.poll(() => collections.filter(value => value === 'drafts').length).toBeGreaterThan(1);
  await page.locator('#collection-archives').click();
  await expect(page.locator('#source-files')).toContainText('Old.md');
  await page.reload();
  await expect(page.locator('#source-files')).toContainText('Old.md');
  expect(localReads).toEqual([]);
  await expect(page.locator('#trash-open')).toBeHidden();
});

test('authority settings preview, wait for drain, and cancel without losing access', async ({page}) => {
  await app.login(page, 'password');
  let transfer = null;
  const preview = {
    id: 'c'.repeat(32),
    project: 'default',
    from: 'forgetmenote',
    to: 'github',
    repository: 'example/notebook',
    sourceBranch: 'main',
    sourceRevision: revision,
    files: 5,
    notes: 3,
    attachments: 1,
    phase: 'preview'
  };
  await page.route('**/api/authority/status?*', route =>
    route.fulfill({
      json: {
        authority: {type: 'forgetmenote', generation: 1},
        changing: transfer?.phase === 'preparing',
        revision,
        transfer
      }
    })
  );
  await page.route('**/p/default/api/authority/preview', route => {
    expect(route.request().postDataJSON().operatorWritersStopped).toBe(true);
    transfer = {...preview};
    return route.fulfill({json: transfer});
  });
  await page.route('**/api/authority/apply', route => {
    transfer = {...preview, phase: 'preparing'};
    return route.fulfill({json: transfer});
  });
  await page.route('**/api/authority/cancel', route => {
    transfer = null;
    return route.fulfill({json: {}});
  });
  await page.goto(app.origin + '/?project=default');
  await page.locator('#settings-open').click();
  await page.locator('[data-settings-category=authority]').click();
  await expect(page.locator('#authority-current')).toHaveText('ForgetMeNote · Editable');
  await page.locator('#authority-writers').check();
  await page.locator('#authority-preview').click();
  await expect(page.locator('#authority-preview-detail')).toContainText('5 · 3 notes · 1 attachments');
  await page.locator('#authority-apply').click();
  await expect(page.locator('#authority-apply')).toHaveText('Continue handoff');
  await expect(page.locator('#authority-status')).toContainText('Waiting for backup worker');
  await page.locator('#authority-cancel').click();
  await expect(page.locator('#authority-current')).toHaveText('ForgetMeNote · Editable');
  await expect(page.locator('#authority-preview-panel')).toBeHidden();
});
