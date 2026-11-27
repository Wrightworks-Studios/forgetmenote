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
          operations: [
            {
              name: 'status',
              description: 'Read metrics',
              readOnly: true,
              method: 'GET',
              path: '/status',
              inputSchema: {
                type: 'object',
                required: ['service', 'since'],
                properties: {
                  service: {type: 'string'},
                  since: {type: 'string'},
                  limit: {type: 'integer', minimum: 1},
                  filter: {type: 'object'}
                },
                additionalProperties: false
              }
            },
            {name: 'summary', description: 'Read the summary', readOnly: true, method: 'GET', path: '/summary'},
            {
              name: 'query',
              description: 'Run a read-only query',
              readOnly: true,
              method: 'POST',
              path: '/query',
              inputSchema: {type: 'object', properties: {sql: {type: 'string'}}}
            },
            {name: 'restart', description: 'Restart the service', readOnly: false, method: 'POST', path: '/restart'}
          ]
        },
        {
          id: 'warehouse',
          label: 'Fixture warehouse',
          kind: 'http',
          endpoint: 'http://127.0.0.1:9',
          operations: [{name: 'rows', description: 'Read rows', readOnly: true, method: 'GET', path: '/rows'}]
        },
        {
          id: 'drive',
          label: 'Fixture drive',
          kind: 'http',
          endpoint: 'http://127.0.0.1:9',
          operations: ['list', 'download', 'export'].map(name => ({
            name,
            description: 'Drive ' + name,
            readOnly: true,
            method: 'GET',
            path: '/' + name
          }))
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
async function newAutomation(root, id, kind, project = 'default') {
  await root.getByRole('button', {name: 'New automation', exact: true}).click();
  await root.getByLabel('ID', {exact: true}).fill(id);
  await root.getByLabel('Service account', {exact: true}).selectOption(service.id);
  await root.getByLabel('Notebook', {exact: true}).selectOption(project);
  await root.getByLabel('Task', {exact: true}).selectOption(kind);
}
async function save(root, id) {
  await root.getByRole('button', {name: 'Save automation', exact: true}).click();
  const card = root.locator(`[data-automation="${id}"]`);
  await expect(card).toContainText('Paused');
  return card;
}
const withoutGeneration = ({generation, ...spec}) => spec;
// The editor's JSON also shows empty top-level fields the server leaves out.
const stored = spec =>
  Object.fromEntries(
    Object.entries(withoutGeneration(spec)).filter(
      ([, value]) => value !== '' && value !== 0 && value !== false && !(Array.isArray(value) && !value.length)
    )
  );
// A definition written as JSON, then saved again from the unchanged form, must
// come back exactly as the JSON saved it.
async function roundTrip(root, id, change) {
  const card = root.locator(`[data-automation="${id}"]`);
  await card.getByRole('button', {name: 'Edit', exact: true}).click();
  await root.getByText('JSON definition', {exact: true}).click();
  const json = root.getByLabel('Automation JSON');
  const written = JSON.parse(await json.inputValue());
  change(written);
  await json.fill(JSON.stringify(written));
  await root.getByRole('button', {name: 'Apply JSON', exact: true}).click();
  await save(root, id);
  const fromJSON = await savedSpec(id);
  expect(withoutGeneration(fromJSON.spec)).toEqual(stored(written));
  await card.getByRole('button', {name: 'Edit', exact: true}).click();
  await expect(root.getByRole('button', {name: 'Save automation', exact: true})).toBeVisible();
  // Let operation discovery finish before the unchanged save.
  await expect(root.locator('.automation-sections select:disabled')).toHaveCount(0);
  await save(root, id);
  const fromForm = await savedSpec(id);
  expect(fromForm.spec.generation).toBe(fromJSON.spec.generation + 1);
  expect(withoutGeneration(fromForm.spec)).toEqual(withoutGeneration(fromJSON.spec));
  return fromForm.spec;
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
  await expect(connections.locator('label')).toHaveText([
    'Fixture drive (drive)',
    'Fixture metrics (metrics)',
    'Fixture warehouse (warehouse)'
  ]);
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

test('digest calls are built from connection, operation, argument and time controls', async ({page}) => {
  const root = await openAutomations(page);
  await newAutomation(root, 'form-digest', 'digest');
  await root.getByLabel('Output note', {exact: true}).fill('reports/digest.md');
  await root
    .getByRole('group', {name: 'Data connections', exact: true})
    .getByLabel('Fixture metrics (metrics)', {exact: true})
    .check();
  const calls = root.getByRole('group', {name: 'Data calls', exact: true});
  const first = calls.getByRole('group', {name: 'Call 1', exact: true});
  await first.getByLabel('Connection', {exact: true}).selectOption('metrics');
  // Only read-only operations are offered.
  await expect(first.getByLabel('Operation', {exact: true}).locator('option')).toHaveText([
    'Choose an operation',
    'status',
    'summary',
    'query'
  ]);
  await first.getByLabel('Operation', {exact: true}).selectOption('status');
  await first.getByLabel('service', {exact: true}).fill('api');
  await first.getByLabel('limit', {exact: true}).fill('5');
  // A time argument fills in a required argument at run time.
  await first.getByRole('button', {name: 'Add time argument', exact: true}).click();
  const time = first.getByRole('group', {name: 'Time argument 1', exact: true});
  await time.getByLabel('Argument', {exact: true}).fill('since');
  await time.getByLabel('Format', {exact: true}).selectOption('date');
  await time.getByLabel('Look back (seconds)', {exact: true}).fill('86400');
  await calls.getByRole('button', {name: 'Add call', exact: true}).click();
  const second = calls.getByRole('group', {name: 'Call 2', exact: true});
  await second.getByLabel('Connection', {exact: true}).selectOption('metrics');
  await root.getByRole('button', {name: 'Save automation', exact: true}).click();
  await expect(page.locator('#admin-status')).toHaveText('Choose an operation for call 2.');
  await second.getByLabel('Operation', {exact: true}).selectOption('query');
  await second.getByLabel('sql', {exact: true}).fill('select 1');
  await root
    .getByRole('group', {name: 'Slack notices', exact: true})
    .getByLabel('Slack user ID', {exact: true})
    .fill('U0FIXTURE');
  await save(root, 'form-digest');
  const {spec} = await savedSpec('form-digest');
  expect(spec).toMatchObject({kind: 'digest', output: 'reports/digest.md', notifySlackUser: 'U0FIXTURE'});
  expect(spec.calls).toEqual([
    {
      connection: 'metrics',
      operation: 'status',
      arguments: {service: 'api', limit: 5},
      time: {since: {format: 'date', offsetSeconds: -86400}}
    },
    {connection: 'metrics', operation: 'query', arguments: {sql: 'select 1'}}
  ]);
  await roundTrip(root, 'form-digest', written => {
    written.calls[0].arguments = {filter: {code: 500}, limit: 5, service: 'api'};
    written.calls[0].schemaHash = 'fixture-schema';
    written.calls[1] = {connection: 'warehouse', operation: 'retired', arguments: {window: '1h'}};
    written.connections = ['metrics', 'warehouse'];
  });
});

test('a monitor condition and incident alerts are set without JSON', async ({page}) => {
  const root = await openAutomations(page);
  await newAutomation(root, 'form-monitor', 'monitor');
  await root
    .getByRole('group', {name: 'Data connections', exact: true})
    .getByLabel('Fixture metrics (metrics)', {exact: true})
    .check();
  const call = root.getByRole('group', {name: 'Call', exact: true});
  await call.getByLabel('Connection', {exact: true}).selectOption('metrics');
  await call.getByLabel('Operation', {exact: true}).selectOption('summary');
  await expect(root.getByRole('button', {name: 'Add call', exact: true})).toBeHidden();
  const condition = root.getByRole('group', {name: 'Condition', exact: true});
  await condition.getByLabel('Result pointer', {exact: true}).fill('/errors');
  await condition.getByLabel('Test', {exact: true}).selectOption('greaterThan');
  await condition.getByLabel('Value', {exact: true}).fill('ten');
  await root.getByRole('button', {name: 'Save automation', exact: true}).click();
  await expect(page.locator('#admin-status')).toHaveText('The value for Greater than must be a number.');
  await condition.getByLabel('Value', {exact: true}).fill('10');
  const alerts = root.getByRole('group', {name: 'Incident alerts', exact: true});
  await alerts.getByLabel('Failing runs before an alert', {exact: true}).fill('2');
  await alerts.getByLabel('Notify when an incident resolves', {exact: true}).check();
  await save(root, 'form-monitor');
  const {spec} = await savedSpec('form-monitor');
  expect(spec.calls).toEqual([{connection: 'metrics', operation: 'summary', arguments: {}}]);
  expect(spec.condition).toEqual({pointer: '/errors', greaterThan: 10});
  expect(spec.alert).toEqual({activeAfter: 2, notifyResolved: true});
  await roundTrip(root, 'form-monitor', written => {
    written.condition = {pointer: '/state', equals: {status: 'down'}};
    written.alert = {repeatSeconds: 3600};
  });
  await root.locator('[data-automation="form-monitor"]').getByRole('button', {name: 'Edit', exact: true}).click();
  await expect(root.getByRole('group', {name: 'Condition', exact: true}).getByLabel('Test', {exact: true})).toHaveValue(
    'equals'
  );
});

test('checks and recipe checks are built from check, assertion and reference controls', async ({page}) => {
  const root = await openAutomations(page);
  await newAutomation(root, 'form-checks', 'checks');
  await root
    .getByRole('group', {name: 'Data connections', exact: true})
    .getByLabel('Fixture metrics (metrics)', {exact: true})
    .check();
  const checks = root.getByRole('group', {name: 'Checks', exact: true});
  const first = checks.getByRole('group', {name: 'Check 1', exact: true});
  await first.getByLabel('Check name', {exact: true}).fill('error-count');
  await expect(first.getByLabel('SQL block', {exact: true})).toBeHidden();
  const call = first.getByRole('group', {name: 'Call', exact: true});
  await call.getByLabel('Connection', {exact: true}).selectOption('metrics');
  await call.getByLabel('Operation', {exact: true}).selectOption('summary');
  await first.getByRole('button', {name: 'Add assertion', exact: true}).click();
  const assertion = first.getByRole('group', {name: 'Assertion 1', exact: true});
  await assertion.getByLabel('Result pointer', {exact: true}).fill('/errors');
  await assertion.getByLabel('Expected value', {exact: true}).fill('0');
  await checks.getByRole('button', {name: 'Add check', exact: true}).click();
  const second = checks.getByRole('group', {name: 'Check 2', exact: true});
  await second.getByLabel('Check name', {exact: true}).fill('matches-reference');
  const secondCall = second.getByRole('group', {name: 'Call', exact: true});
  await secondCall.getByLabel('Connection', {exact: true}).selectOption('metrics');
  await secondCall.getByLabel('Operation', {exact: true}).selectOption('query');
  await secondCall.getByLabel('sql', {exact: true}).fill('select 1');
  await second.getByLabel('Compare with a reference call', {exact: true}).check();
  const reference = second.getByRole('group', {name: 'Reference call', exact: true});
  await reference.getByLabel('Connection', {exact: true}).selectOption('metrics');
  await reference.getByLabel('Operation', {exact: true}).selectOption('query');
  await reference.getByLabel('sql', {exact: true}).fill('select 1 from reference');
  await second.getByLabel('Slowest allowed query (milliseconds)', {exact: true}).fill('1500');
  // Nested checks, calls and assertions still fit a phone screen.
  await page.setViewportSize({width: 390, height: 844});
  const bounds = await page.locator('#admin-dialog').evaluate(e => ({width: e.clientWidth, scroll: e.scrollWidth}));
  expect(bounds.scroll).toBeLessThanOrEqual(bounds.width + 1);
  await page.screenshot({path: test.info().outputPath('automation-checks-mobile.png'), fullPage: true});
  await page.setViewportSize({width: 1280, height: 900});
  await save(root, 'form-checks');
  const {spec} = await savedSpec('form-checks');
  expect(spec.checks).toEqual([
    {
      name: 'error-count',
      call: {connection: 'metrics', operation: 'summary', arguments: {}},
      assertions: [{pointer: '/errors', equals: 0}]
    },
    {
      name: 'matches-reference',
      call: {connection: 'metrics', operation: 'query', arguments: {sql: 'select 1'}},
      reference: {connection: 'metrics', operation: 'query', arguments: {sql: 'select 1 from reference'}},
      assertions: null,
      maxDurationMs: 1500
    }
  ]);
  await roundTrip(root, 'form-checks', written => {
    written.checks[0].assertions.push({pointer: '', equals: {errors: 0, warnings: [1, 2]}});
    written.checks[1].call.arguments = null;
  });

  await newAutomation(root, 'form-recipe', 'recipe');
  await root.getByLabel('Recipe note', {exact: true}).fill('recipes/errors.md');
  const recipe = root.getByRole('group', {name: 'Check 1', exact: true});
  await recipe.getByLabel('Check name', {exact: true}).fill('daily');
  await recipe.getByLabel('SQL block', {exact: true}).fill('daily');
  const query = recipe.getByRole('group', {name: 'Call', exact: true});
  await query.getByLabel('Connection', {exact: true}).selectOption('metrics');
  await query.getByLabel('Operation', {exact: true}).selectOption('query');
  await recipe.getByRole('button', {name: 'Add assertion', exact: true}).click();
  await recipe.getByLabel('Expected value', {exact: true}).fill('[]');
  const usage = root.getByRole('group', {name: 'Recheck after MCP use', exact: true});
  await usage.getByLabel('After a failed query', {exact: true}).check();
  await usage.getByLabel('Wait between rechecks (seconds, 60–86400)', {exact: true}).fill('600');
  await save(root, 'form-recipe');
  const saved = (await savedSpec('form-recipe')).spec;
  expect(saved).toMatchObject({
    kind: 'recipe',
    recipe: 'recipes/errors.md',
    usage: {errors: true, cooldownSeconds: 600}
  });
  expect(saved.checks).toEqual([
    {
      name: 'daily',
      block: 'daily',
      call: {connection: 'metrics', operation: 'query', arguments: {}},
      assertions: [{pointer: '', equals: []}]
    }
  ]);
  await roundTrip(root, 'form-recipe', written => {
    written.usage = {slowMs: 2500, cooldownSeconds: 900};
  });
});

test('drive imports and source events are set without JSON', async ({page}) => {
  // The chat-client policy names the Slack knowledge channels a trigger can use.
  await page.route('**/api/organization/action/chat_clients', route =>
    route.fulfill({
      json: {available: ['slack'], config: {revision: 1, clients: {slack: {knowledgeChannels: {C0KNOW: 'default'}}}}}
    })
  );
  const root = await openAutomations(page);
  await newAutomation(root, 'form-drive', 'drive-sync');
  await root
    .getByRole('group', {name: 'Data connections', exact: true})
    .getByLabel('Fixture drive (drive)', {exact: true})
    .check();
  const drive = root.getByRole('group', {name: 'Google Drive import', exact: true});
  await drive.getByLabel('Drive connection', {exact: true}).selectOption('drive');
  await drive.getByLabel('Folder ID 1', {exact: true}).fill('folder-a');
  await drive.getByRole('button', {name: 'Add folder', exact: true}).click();
  await drive.getByLabel('Folder ID 2', {exact: true}).fill('folder-b');
  await drive.getByLabel('Notebook folder', {exact: true}).fill('imports/drive');
  await drive.getByLabel('Files per run (0–128)', {exact: true}).fill('20');
  await save(root, 'form-drive');
  expect((await savedSpec('form-drive')).spec.drive).toEqual({
    connection: 'drive',
    folders: ['folder-a', 'folder-b'],
    destination: 'imports/drive',
    maxFiles: 20
  });
  await roundTrip(root, 'form-drive', written => {
    written.drive.folders.reverse();
    delete written.drive.maxFiles;
  });

  await newAutomation(root, 'form-event', 'task');
  await root.getByLabel('Output note', {exact: true}).fill('reports/changes.md');
  await root.getByLabel('Instructions', {exact: true}).fill('Summarize what changed.');
  await root.getByLabel('Allow configured AI processing', {exact: true}).check();
  await root.getByRole('group', {name: 'Input notebooks', exact: true}).getByLabel('Reports (reports)').check();
  await root.getByLabel('Trigger', {exact: true}).selectOption('event');
  const event = root.getByRole('group', {name: 'Source event', exact: true});
  await expect(event.getByLabel('Quiet period (seconds)', {exact: true})).toHaveValue('120');
  await event.getByLabel('Notebook to watch', {exact: true}).selectOption('reports');
  await event.getByLabel('Quiet period (seconds)', {exact: true}).fill('60');
  await event.getByLabel('Longest wait (seconds, up to 86400)', {exact: true}).fill('30');
  await root.getByRole('button', {name: 'Save automation', exact: true}).click();
  await expect(page.locator('#admin-status')).toHaveText('The longest wait must be at least the quiet period.');
  await event.getByLabel('Longest wait (seconds, up to 86400)', {exact: true}).fill('600');
  await save(root, 'form-event');
  expect((await savedSpec('form-event')).spec.trigger).toEqual({
    kind: 'repository.updated',
    project: 'reports',
    debounceSeconds: 60,
    maxWaitSeconds: 600
  });
  const card = root.locator('[data-automation="form-event"]');
  await card.getByRole('button', {name: 'Edit', exact: true}).click();
  await event.getByLabel('Event', {exact: true}).selectOption('slack.knowledge');
  await expect(event.getByLabel('Knowledge channel', {exact: true}).locator('option')).toHaveText([
    'Choose a channel',
    'C0KNOW · default'
  ]);
  await event.getByLabel('Knowledge channel', {exact: true}).selectOption('C0KNOW');
  await save(root, 'form-event');
  expect((await savedSpec('form-event')).spec.trigger).toEqual({
    kind: 'slack.knowledge',
    channel: 'C0KNOW',
    debounceSeconds: 60,
    maxWaitSeconds: 600
  });
  await roundTrip(root, 'form-event', written => {
    written.trigger = {kind: 'repository.updated', project: 'default', debounceSeconds: 5, maxWaitSeconds: 5};
  });
});

test('repository package bindings, capabilities and parameters come from its manifest', async ({page}) => {
  const manifest = {
    apiVersion: 'forgetmenote.automation/v1',
    id: 'bound',
    name: 'Bound package',
    entrypoint: 'main.py',
    runtime: 'python-v1',
    files: ['main.py'],
    capabilities: ['checkpoint', 'notebooks.read'],
    optionalCapabilities: ['web.search'],
    connections: {source: ['rows']},
    optionalConnections: {extra: ['status']},
    notebooks: {input: 'read'},
    parametersSchema: {
      type: 'object',
      required: ['window'],
      properties: {window: {type: 'integer', minimum: 1}, label: {type: 'string'}}
    }
  };
  const pkg = {
    project: 'reports',
    source: 'source',
    path: 'automations/bound',
    revision: 'e'.repeat(40),
    latest: 'e'.repeat(40),
    digest: 'f'.repeat(64),
    manifest
  };
  // Installing needs a prepared package runtime, so the server side is a
  // fixture here: it keeps exactly what the editor sends.
  const sent = [];
  let saved;
  await page.route('**/api/organization/action/automation_status', route =>
    route.fulfill({
      json: {
        jobs: saved
          ? [
              {
                id: saved.id,
                kind: 'script',
                project: saved.project,
                spec: saved,
                revision: 'r' + sent.length,
                managedBy: 'instance'
              }
            ]
          : [],
        runs: [],
        pythonConfigured: true
      }
    })
  );
  await page.route('**/api/organization/action/discover_automation_packages', route => route.fulfill({json: [pkg]}));
  await page.route('**/api/organization/action/install_automation_package', route => {
    saved = route.request().postDataJSON().spec;
    sent.push(saved);
    return route.fulfill({json: {spec: saved, revision: 'r' + sent.length, managedBy: 'instance'}});
  });
  const root = await openAutomations(page);
  await root.getByRole('button', {name: 'Repository packages', exact: true}).click();
  await root.getByLabel('Package notebook', {exact: true}).fill('reports');
  await root.getByRole('button', {name: 'Discover', exact: true}).click();
  await root.getByRole('button', {name: 'Prepare paused installation'}).click();
  await root.getByLabel('Service account', {exact: true}).selectOption(service.id);
  await root.getByLabel('Notebook', {exact: true}).selectOption('default');
  const connections = root.getByRole('group', {name: 'Package connections', exact: true});
  await expect(connections.getByLabel('extra', {exact: true}).locator('option:checked')).toHaveText('Not used');
  await root.getByRole('button', {name: 'Save automation', exact: true}).click();
  await expect(page.locator('#admin-status')).toHaveText('Choose a connection for the package connection source.');
  await connections.getByLabel('source', {exact: true}).selectOption('warehouse');
  await root
    .getByRole('group', {name: 'Package notebooks', exact: true})
    .getByLabel('input', {exact: true})
    .selectOption('reports');
  await root
    .getByRole('group', {name: 'Optional capabilities', exact: true})
    .getByLabel('web.search', {exact: true})
    .check();
  await root.getByRole('group', {name: 'Parameters', exact: true}).getByLabel('window', {exact: true}).fill('7');
  await root.getByRole('button', {name: 'Save automation', exact: true}).click();
  const card = root.locator('[data-automation="bound"]');
  await expect(card).toContainText('Paused');
  expect(saved.script).toMatchObject({
    connections: {source: 'warehouse'},
    notebooks: {input: 'reports'},
    capabilities: ['checkpoint', 'notebooks.read', 'web.search'],
    parameters: {window: 7}
  });
  expect(saved.script.connections).not.toHaveProperty('extra');

  // The same package written as JSON survives an unchanged form save.
  await card.getByRole('button', {name: 'Edit', exact: true}).click();
  await root.getByText('JSON definition', {exact: true}).click();
  const json = root.getByLabel('Automation JSON');
  const written = JSON.parse(await json.inputValue());
  written.script.connections.extra = 'metrics';
  written.script.parameters = {label: 'weekly', window: 3};
  written.script.capabilities = ['web.search', 'checkpoint', 'notebooks.read'];
  await json.fill(JSON.stringify(written));
  await root.getByRole('button', {name: 'Apply JSON', exact: true}).click();
  await root.getByRole('button', {name: 'Save automation', exact: true}).click();
  await expect(card).toContainText('Paused');
  const fromJSON = sent.at(-1);
  expect(fromJSON).toEqual(written);
  await card.getByRole('button', {name: 'Edit', exact: true}).click();
  await expect(connections.getByLabel('extra', {exact: true})).toHaveValue('metrics');
  await expect(
    root.getByRole('group', {name: 'Parameters', exact: true}).getByLabel('label', {exact: true})
  ).toHaveValue('weekly');
  await root.getByRole('button', {name: 'Save automation', exact: true}).click();
  await expect(card).toContainText('Paused');
  expect(sent.at(-1)).toEqual(fromJSON);
  expect(sent).toHaveLength(3);
});
