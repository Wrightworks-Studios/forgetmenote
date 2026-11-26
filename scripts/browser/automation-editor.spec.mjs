import {test, expect} from '@playwright/test';
import {mkdtemp, rm, writeFile} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {startApp} from './app-fixture.mjs';

// Operations are configured but never called: these cases check only what the
// automation editor saves.
let app, dir, service;
test.beforeAll(async () => {
  dir = await mkdtemp(join(tmpdir(), 'fmn-automation-editor-config-'));
  const connections = join(dir, 'connections.json');
  await writeFile(
    connections,
    JSON.stringify({
      version: 1,
      connections: [
        {
          id: 'metrics',
          label: 'Fixture metrics',
          kind: 'http',
          endpoint: 'http://127.0.0.1:9',
          externalProcessing: true,
          operations: [{name: 'status', description: 'Read metrics', readOnly: true, method: 'GET', path: '/status'}]
        },
        {
          id: 'warehouse',
          label: 'Fixture warehouse',
          kind: 'http',
          endpoint: 'http://127.0.0.1:9',
          operations: [{name: 'rows', description: 'Read rows', readOnly: true, method: 'GET', path: '/rows'}]
        }
      ]
    })
  );
  app = await startApp({
    name: 'fmn-automation-editor-',
    account: {username: 'automation-admin', password: 'DisposableAutomation123!'},
    env: {DATA_CONNECTIONS_CONFIG_FILE: connections}
  });
  await app.api('/api/projects', {id: 'reports', name: 'Reports'});
  await app.api('/api/projects', {id: 'archive', name: 'Archive'});
  const directory = await app.api('/api/admin'),
    admin = directory.users.find(user => user.admin && user.id !== '@local-admin');
  service = await app.api('/api/organization/action/create_service_account', {
    name: 'Editor test worker',
    steward: admin.id
  });
  for (const project of ['default', 'reports', 'archive'])
    await app.api('/api/members', {project, user: service.id, role: 'edit'});
});
test.afterAll(async () => {
  try {
    await app?.close();
  } finally {
    if (dir) await rm(dir, {recursive: true, force: true});
  }
});
test.afterEach(async ({}, info) => {
  if (info.status !== info.expectedStatus) await info.attach('app-log', {body: app.output, contentType: 'text/plain'});
});

async function openAutomations(page) {
  await app.login(page, 'password');
  await page.goto(app.origin + '/?project=default&page=administration&category=automation');
  const root = page.locator('#integration-jobs');
  await expect(root.getByRole('button', {name: 'New automation', exact: true})).toBeVisible();
  return root;
}
async function savedSpec(id) {
  const status = await app.api('/api/organization/action/automation_status', {});
  const job = status.jobs.find(job => job.id === id);
  return {revision: job.revision, spec: job.spec};
}

test('automation pickers save the calendar, notebooks and connections they show', async ({page}) => {
  const root = await openAutomations(page);
  await root.getByRole('button', {name: 'New automation', exact: true}).click();
  await root.getByLabel('ID', {exact: true}).fill('picker-health');
  await root.getByLabel('Service account', {exact: true}).selectOption(service.id);
  const notebook = root.getByLabel('Notebook', {exact: true});
  await expect(notebook.locator('option')).toHaveText([
    'Choose a notebook',
    'Archive (archive)',
    'My notes (default)',
    'Reports (reports)'
  ]);
  await notebook.selectOption('default');
  await root.getByLabel('Trigger', {exact: true}).selectOption('calendar');
  await root.getByLabel('Timezone', {exact: true}).selectOption('America/Denver');
  await root.getByLabel('Hours (0–23, comma separated)').fill('9, 24');
  await root.getByLabel('Hours (0–23, comma separated)').press('Tab');
  await expect(page.locator('#admin-status')).toHaveText(
    'Hours must be whole numbers from 0 to 23, separated by commas.'
  );
  await root.getByLabel('Hours (0–23, comma separated)').fill('17, 9');
  await root.getByLabel('Minutes (0–59, comma separated)').fill('15');
  const weekdays = root.getByRole('group', {name: 'Weekdays (none for every day)', exact: true});
  await weekdays.getByLabel('Friday', {exact: true}).check();
  await weekdays.getByLabel('Monday', {exact: true}).check();
  const inputs = root.getByRole('group', {name: 'Input notebooks', exact: true});
  await inputs.getByLabel('Reports (reports)', {exact: true}).check();
  const connections = root.getByRole('group', {name: 'Data connections', exact: true});
  await expect(connections.locator('label')).toHaveText(['Fixture metrics (metrics)', 'Fixture warehouse (warehouse)']);
  await connections.getByLabel('Fixture metrics (metrics)', {exact: true}).check();
  await root.getByRole('button', {name: 'Save automation', exact: true}).click();
  const card = root.locator('[data-automation="picker-health"]');
  await expect(card).toContainText('Paused');
  const first = await savedSpec('picker-health');
  expect(first.spec).toMatchObject({
    project: 'default',
    readProjects: ['reports'],
    connections: ['metrics'],
    schedule: {timezone: 'America/Denver', hours: [17, 9], minutes: [15], weekdays: [5, 1], catchUpSeconds: 3600}
  });

  // The same definition written as JSON, in an order the form does not offer
  // and naming a connection it does not list, survives an unchanged form save.
  await card.getByRole('button', {name: 'Edit', exact: true}).click();
  await expect(root.getByLabel('Timezone', {exact: true})).toHaveValue('America/Denver');
  await expect(weekdays.getByLabel('Monday', {exact: true})).toBeChecked();
  await root.getByText('JSON definition', {exact: true}).click();
  const json = root.getByLabel('Automation JSON');
  const written = JSON.parse(await json.inputValue());
  written.readProjects = ['reports', 'archive'];
  written.connections = ['warehouse', 'metrics'];
  written.schedule.weekdays = [5, 3, 1];
  await json.fill(JSON.stringify(written));
  await root.getByRole('button', {name: 'Apply JSON', exact: true}).click();
  await expect(inputs.getByLabel('Archive (archive)', {exact: true})).toBeChecked();
  await expect(weekdays.getByLabel('Wednesday', {exact: true})).toBeChecked();
  await root.getByRole('button', {name: 'Save automation', exact: true}).click();
  await expect(card).toContainText('Paused');
  const fromJSON = await savedSpec('picker-health');
  expect(fromJSON.spec).toMatchObject({
    readProjects: ['reports', 'archive'],
    connections: ['warehouse', 'metrics'],
    schedule: {weekdays: [5, 3, 1]}
  });
  await card.getByRole('button', {name: 'Edit', exact: true}).click();
  await expect(inputs.getByLabel('Archive (archive)', {exact: true})).toBeChecked();
  await root.getByRole('button', {name: 'Save automation', exact: true}).click();
  await expect(card).toContainText('Paused');
  const fromForm = await savedSpec('picker-health');
  // Each save advances the generation; nothing else may change.
  const {generation: savedAgain, ...formSpec} = fromForm.spec,
    {generation: savedFromJSON, ...jsonSpec} = fromJSON.spec;
  expect(savedAgain).toBe(savedFromJSON + 1);
  expect(formSpec).toEqual(jsonSpec);
});

test('run at is chosen in local time and keeps the saved time when unchanged', async ({page}) => {
  const root = await openAutomations(page);
  await root.getByRole('button', {name: 'New automation', exact: true}).click();
  await root.getByLabel('ID', {exact: true}).fill('picker-once');
  await root.getByLabel('Service account', {exact: true}).selectOption(service.id);
  await root.getByLabel('Notebook', {exact: true}).selectOption('reports');
  await root.getByLabel('Trigger', {exact: true}).selectOption('once');
  await expect(root.getByLabel('Run at', {exact: true})).toHaveAttribute('type', 'datetime-local');
  await root.getByLabel('Run at', {exact: true}).fill('2099-03-04T08:30');
  await root.getByRole('button', {name: 'Save automation', exact: true}).click();
  const card = root.locator('[data-automation="picker-once"]');
  await expect(card).toContainText('Paused');
  const {spec} = await savedSpec('picker-once');
  expect(spec.schedule.once).toMatch(/^2099-03-04T08:30:00(Z|[+-]\d\d:\d\d)$/);
  expect(new Date(spec.schedule.once).getTime()).toBe(
    await page.evaluate(() => new Date('2099-03-04T08:30').getTime())
  );
  // A time written with seconds in another offset shows in local time and is
  // kept exactly while the field is not changed.
  await card.getByRole('button', {name: 'Edit', exact: true}).click();
  await root.getByText('JSON definition', {exact: true}).click();
  const json = root.getByLabel('Automation JSON');
  const written = JSON.parse(await json.inputValue());
  written.schedule.once = '2099-05-06T07:08:09+05:30';
  await json.fill(JSON.stringify(written));
  await root.getByRole('button', {name: 'Apply JSON', exact: true}).click();
  await expect(root.getByLabel('Run at', {exact: true})).toHaveValue(
    await page.evaluate(() => {
      const d = new Date('2099-05-06T07:08:09+05:30'),
        pad = n => String(n).padStart(2, '0');
      return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
    })
  );
  await root.getByLabel('Name', {exact: true}).fill('One-off check');
  await root.getByRole('button', {name: 'Save automation', exact: true}).click();
  await expect(card).toContainText('One-off check');
  expect((await savedSpec('picker-once')).spec.schedule.once).toBe('2099-05-06T07:08:09+05:30');
});

test('a saved account or notebook the pickers do not offer stays selected', async ({page}) => {
  const root = await openAutomations(page);
  await root.getByRole('button', {name: 'New automation', exact: true}).click();
  await root.getByLabel('ID', {exact: true}).fill('picker-missing');
  await root.getByText('JSON definition', {exact: true}).click();
  const json = root.getByLabel('Automation JSON');
  const written = JSON.parse(await json.inputValue());
  Object.assign(written, {actor: 'retired-worker', project: 'retired-notebook'});
  await json.fill(JSON.stringify(written));
  await root.getByRole('button', {name: 'Apply JSON', exact: true}).click();
  await expect(root.getByLabel('Service account', {exact: true}).locator('option:checked')).toHaveText(
    'retired-worker (not found)'
  );
  await expect(root.getByLabel('Notebook', {exact: true}).locator('option:checked')).toHaveText(
    'retired-notebook (not found)'
  );
  await root.getByLabel('Name', {exact: true}).fill('Missing references');
  await expect(JSON.parse(await json.inputValue())).toMatchObject({
    actor: 'retired-worker',
    project: 'retired-notebook'
  });
});
