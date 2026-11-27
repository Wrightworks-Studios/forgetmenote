import {test, expect} from '@playwright/test';
import {startApp} from './app-fixture.mjs';
let app;
const account = {username: 'modules-admin', password: 'DisposableModules123!'};
test.beforeAll(async () => {
  app = await startApp({
    name: 'forgetmenote-modules-off-',
    account,
    modulesOff: ['judgments', 'data-connections', 'sources', 'repositories', 'managed-backups']
  });
  await app.api('/p/default/api/note', {path: 'Welcome.md', content: '# Team notebook', revision: ''});
});
test.afterAll(async () => app?.close());

test('an instance that turns modules off leaves out their entry points and says they are off', async ({page}) => {
  await app.login(page, 'password');
  await page.goto(app.origin + '/?project=default');
  await expect(page.locator('#new-note')).toBeEnabled();
  // Sources: nothing offers an import.
  await expect(page.locator('#sidebar-import-source')).toBeHidden();
  await page.locator('#create-toggle').click();
  await expect(page.locator('#new-note')).toBeVisible();
  await expect(page.locator('#create-source')).toBeHidden();
  await page.keyboard.press('Escape');
  expect(await page.locator('#create-source-tab').evaluate(node => node.hidden)).toBe(true);
  // Judgments: no checks and no notifications.
  await expect(page.locator('#notifications-open')).toBeHidden();
  for (const id of ['#judgments-settings-open', '#judgments-review-open'])
    expect(await page.locator(id).evaluate(node => node.hidden)).toBe(true);

  await page.locator('#account-actions-toggle').click();
  await page.locator('#admin-open').click();
  await expect(page.locator('#admin-dialog')).toBeVisible();
  await expect(page.locator('#admin-dialog [data-settings-category=people]')).toBeVisible();
  await expect(page.locator('#admin-dialog [data-settings-category=connections]')).toBeHidden();
  await page.locator('#admin-dialog [data-settings-category=instance]').click();
  await expect(page.locator('#instance-status')).toContainText('Modules off');
  const rows = await page
    .locator('#instance-status')
    .evaluate(node =>
      Object.fromEntries(
        [...node.querySelectorAll('dt')].map(term => [term.textContent, term.nextElementSibling.textContent])
      )
    );
  expect(rows['Modules off']).toBe(
    'Judgments; Data connections; Archived sources; Repository notebooks; Managed backups'
  );
  expect(rows['AI checks']).toBe('Turned off');
  expect(rows['Data connections']).toBe('Turned off');
  expect(rows['Semantic search']).toBe('Not configured');
  expect(rows['Repository sources']).toBe('Turned off');
  expect(rows['Managed backups']).toBe('Turned off');

  await page.locator('#admin-dialog [data-settings-category=ai]').click();
  // The chat's reading of uploads, the sources module's one AI feature, says the module is off.
  await expect(page.locator('#instance-ai-sourceOCR')).toHaveCount(0);
  await expect(page.locator('#instance-ai-uploadReading-hint')).toHaveText(
    'The sources module is turned off on this instance.'
  );
  await expect(page.locator('#instance-ai-rerank-hint')).toHaveText(
    'The judgments module is turned off on this instance.'
  );
  await expect(page.locator('#instance-ai-embeddings-hint')).toHaveText('Requires a configured embeddings provider.');

  // Repositories: a new notebook cannot connect a GitHub repository.
  await page.locator('#admin-dialog [data-settings-category=instance]').click();
  await page.locator('#workspace-create-open').click();
  await expect(page.locator('#workspace-create-dialog')).toBeVisible();
  await expect(page.locator('#notebook-create-mode')).toBeVisible();
  await expect(page.locator('#notebook-repository-mode')).toBeHidden();
});

test('notebook settings leave out repository sources, authority handoffs and managed backups', async ({page}) => {
  const requests = [];
  page.on('request', request => requests.push(new URL(request.url()).pathname));
  await app.login(page, 'password');
  await page.goto(app.origin + '/?project=default');
  await page.locator('#settings-open').click();
  await expect(page.locator('#settings-dialog [data-settings-category=sharing]')).toBeVisible();
  await expect(page.locator('#settings-dialog [data-settings-category=sources]')).toBeHidden();
  await page.locator('#settings-dialog [data-settings-category=authority]').click();
  await expect(page.locator('#authority-current')).toHaveText('ForgetMeNote · Editable');
  await expect(page.locator('#authority-form')).toBeHidden();
  await page.locator('#settings-dialog [data-settings-category=history]').click();
  await expect(page.locator('#managed-backup')).toBeHidden();
  expect(requests.filter(path => path.includes('/api/repository') || path.includes('/api/managed-backup'))).toEqual([]);
});

test('the assistant page leaves out data watches while data connections are off', async ({page}) => {
  await app.login(page, 'password');
  await page.goto(app.origin + '/account/assistant');
  await expect(page.locator('#assistant-preferences')).toBeAttached();
  expect(await page.locator('#assistant-watch-section').evaluate(node => node.hidden)).toBe(true);
});
