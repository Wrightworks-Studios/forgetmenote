import {test, expect} from '@playwright/test';
import {startApp} from './app-fixture.mjs';
let app;
const account = {username: 'modules-admin', password: 'DisposableModules123!'};
test.beforeAll(async () => {
  app = await startApp({
    name: 'forgetmenote-modules-off-',
    account,
    // Every module but agent memory, so the assistant page keeps one section.
    modulesOff: [
      'chat',
      'automations',
      'judgments',
      'data-connections',
      'sources',
      'repositories',
      'tasks',
      'messaging',
      'personal',
      'managed-backups'
    ]
  });
  await app.api('/p/default/api/note', {path: 'Welcome.md', content: '# Team notebook', revision: ''});
});
test.afterAll(async () => app?.close());

test('an instance that turns modules off leaves out their entry points and says they are off', async ({page}) => {
  const requests = [];
  page.on('request', request => requests.push(new URL(request.url()).pathname));
  await app.login(page, 'password');
  await page.goto(app.origin + '/?project=default');
  await expect(page.locator('#new-note')).toBeEnabled();
  // Chat: nothing opens a conversation.
  await expect(page.locator('#chat-open')).toBeHidden();
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
  for (const category of ['connections', 'repositories', 'chat-clients', 'tasks'])
    await expect(page.locator(`#admin-dialog [data-settings-category=${category}]`)).toBeHidden();
  // Repositories: the import from the App's installation goes with its category.
  await expect(page.locator('#admin-dialog [data-settings-category=repositories]')).toHaveCount(1);
  // Automations: service accounts stay, the automations list goes.
  await page.locator('#admin-dialog [data-settings-category=automation]').click();
  await expect(page.locator('#service-create-form')).toBeVisible();
  await expect(page.locator('#automation-jobs')).toBeHidden();
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
    'Notebook chat; Automations; Judgments; Data connections; Archived sources; Repository notebooks; Task queues; Slack messaging; Personal assistants; Managed backups'
  );
  expect(rows['Modules on']).toBe('Agent memory');
  expect(rows['Gateways disagree']).toBeUndefined();
  for (const row of [
    'AI conversations',
    'Slack',
    'Personal connections',
    'Automation definitions',
    'Automation runtime'
  ])
    expect(rows[row]).toBe('Turned off');
  expect(rows['AI checks']).toBe('Turned off');
  expect(rows['Data connections']).toBe('Turned off');
  expect(rows['Semantic search']).toBe('Not configured');
  expect(rows['Repository sources']).toBe('Turned off');
  expect(rows['Managed backups']).toBe('Turned off');

  await page.locator('#admin-dialog [data-settings-category=ai]').click();
  // Sources have no AI features: connected agents extract what the server does not.
  await expect(page.locator('#instance-ai-sourceOCR')).toHaveCount(0);
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
  expect(requests.filter(path => path.includes('/api/chat') || path.endsWith('/automation_status'))).toEqual([]);
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

test('Inbox sorting and search ask nothing of judgments while it is off', async ({page}) => {
  const requests = [];
  page.on('request', request => requests.push(request));
  await app.login(page, 'password');
  await page.goto(app.origin + '/?project=default&folder=');
  await page.locator('#inbox-open').click();
  const dialog = page.locator('#inbox-view');
  await dialog.getByRole('button', {name: 'Preferences', exact: true}).click();
  await expect(dialog).toContainText(
    'Remote sorting needs the judgments module, which is turned off on this instance.'
  );
  const modes = await dialog
    .getByLabel('Sorting mode')
    .evaluate(select => [...select.options].filter(option => !option.disabled).map(option => option.value));
  expect(modes).toEqual(['manual']);
  await dialog.getByRole('button', {name: 'Save Quicknotes preferences'}).click();
  await expect(dialog).toContainText('Quicknotes preferences saved.');
  await page.keyboard.press('Escape');

  // Enter asks for no judgment ranking.
  await page.locator('#search-open').click();
  await page.locator('#search').fill('team notebook');
  await expect(page.locator('#search-results .note-row a')).toHaveCount(1);
  const searched = page.waitForRequest(
    request => request.url().endsWith('/api/knowledge/hybrid-search') && request.postDataJSON().text === 'team notebook'
  );
  await page.locator('#search').press('Enter');
  expect((await searched).postDataJSON().rerank).toBeUndefined();
  await expect(page.locator('#search-status')).not.toContainText('judgments');
  expect(requests.filter(request => new URL(request.url()).pathname.includes('/api/judgments'))).toEqual([]);
});

test('the assistant page keeps agent memory while personal assistants are off', async ({page}) => {
  const requests = [];
  page.on('request', request => requests.push(new URL(request.url()).pathname));
  await app.login(page, 'password');
  await page.goto(app.origin + '/?project=default');
  await page.locator('#account-actions-toggle').click();
  await page.locator('#account-actions a[href="/account/assistant"]').click();
  await expect(page.locator('#assistant-message')).toHaveText('Personal assistants are turned off on this instance.');
  await expect(page.locator('#memory-setup')).toBeVisible();
  for (const id of ['#assistant-setup', '#reply-setup', '#assistant-admin'])
    await expect(page.locator(id)).toBeHidden();
  await expect(page.getByRole('heading', {name: 'Connected accounts'})).toBeHidden();
  await expect(page.getByRole('heading', {name: 'Suggestions'})).toBeHidden();
  await page.locator('#assistant-memory').check();
  await page.getByRole('button', {name: 'Save memory setting'}).click();
  await expect(page.locator('#assistant-message')).toHaveText('Memory setting saved.');
  expect(requests.filter(path => /assistant_(context|activity|policy)$/.test(path))).toEqual([]);
});
