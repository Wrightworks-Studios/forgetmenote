import {test, expect} from '@playwright/test';
import {startApp} from './app-fixture.mjs';

// Automations and task queues run while chat and messaging (and the personal
// assistants that need it) are off, so their pages offer only what still runs.
let app;
const account = {username: 'modules-some-admin', password: 'DisposableSomeModules123!'};
test.beforeAll(async () => {
  app = await startApp({
    name: 'forgetmenote-modules-some-off-',
    account,
    modulesOff: ['chat', 'messaging', 'personal']
  });
});
test.afterAll(async () => app?.close());
test.afterEach(async ({}, info) => {
  if (info.status !== info.expectedStatus) await info.attach('app-log', {body: app.output, contentType: 'text/plain'});
});

const offered = select =>
  select.evaluate(node => [...node.options].filter(option => !option.disabled).map(option => option.value));

test('task queues work without a messaging client while messaging is off', async ({page}) => {
  const requests = [];
  page.on('request', request => requests.push(new URL(request.url()).pathname));
  await app.login(page, 'password');
  await page.goto(app.origin + '/?project=default&page=administration&category=tasks');
  const panel = page.locator('#task-queues-panel');
  await expect(panel).toContainText(
    'Slack messaging is turned off on this instance, so queues work without a client: nobody gets reminders or notices.'
  );
  await expect(page.locator('#admin-dialog [data-settings-category=chat-clients]')).toBeHidden();
  expect(requests.filter(path => path.endsWith('/chat_clients'))).toEqual([]);
});

test('the automation editor offers only what the modules that run can do', async ({page}) => {
  const requests = [];
  page.on('request', request => requests.push(new URL(request.url()).pathname));
  await app.login(page, 'password');
  await page.goto(app.origin + '/?project=default&page=administration&category=automation');
  const root = page.locator('#integration-jobs');
  await root.getByRole('button', {name: 'New automation', exact: true}).click();
  // AI tasks need chat; Slack email pairing needs messaging.
  const kinds = await offered(root.getByLabel('Task', {exact: true}));
  expect(kinds).toContain('digest');
  expect(kinds).not.toContain('task');
  expect(kinds).not.toContain('learning');
  const maintenance = await offered(root.getByLabel('Maintenance task', {exact: true}));
  expect(maintenance).toContain('task-reminder');
  expect(maintenance).not.toContain('email-pairing');
  await expect(root.getByLabel('AI connection (blank uses instance default)', {exact: true})).toBeHidden();

  await root.getByLabel('Task', {exact: true}).selectOption('checks');
  const legends = () => root.locator('.automation-part > legend').allTextContents();
  await expect.poll(legends).toContain('Checks');
  await expect(root.getByText('Automatically diagnose and repair checks')).toBeHidden();

  // Slack notices and the Slack knowledge event need messaging.
  await root.getByLabel('Task', {exact: true}).selectOption('digest');
  await expect.poll(legends).toContain('Data calls');
  expect(await legends()).not.toContain('Slack notices');
  await root.getByLabel('Trigger', {exact: true}).selectOption('event');
  expect(await offered(root.getByLabel('Event', {exact: true}))).toEqual(['repository.updated']);
  expect(requests.filter(path => path.endsWith('/chat_clients'))).toEqual([]);
});
