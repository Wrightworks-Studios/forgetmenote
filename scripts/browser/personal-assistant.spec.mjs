import {test, expect} from '@playwright/test';
import {mkdtemp, rm, writeFile} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {startApp} from './app-fixture.mjs';

let app;
test.beforeAll(async () => {
  app = await startApp({name: 'fmn-assistant-', account: {username: 'employee', password: 'DisposableAssistant123!'}});
  await app.api('/api/organization/action/set_assistant_policy', {
    enabled: true,
    aiConnection: 'luna',
    model: 'gpt-6-luna',
    reasoningEffort: 'low'
  });
});
test.afterAll(async () => {
  await app?.close();
});
test.afterEach(async ({}, info) => {
  if (info.status !== info.expectedStatus) await info.attach('app-log', {body: app.output, contentType: 'text/plain'});
});

// The assistant's profile files and the responsibilities records are notebook
// files: the browser keeps them as typed, as in the editor.
async function expectAsTyped(field) {
  for (const [name, value] of Object.entries({
    autocorrect: 'off',
    autocapitalize: 'off',
    writingsuggestions: 'false',
    spellcheck: 'true'
  }))
    await expect(field).toHaveAttribute(name, value);
}

test('employee can opt in, edit memory, exclude and restore a notebook', async ({page}) => {
  await app.login(page, 'password');
  await page.goto(app.origin + '/account/assistant');
  await expect(page.locator('#assistant-enabled')).toBeEnabled();
  await page.locator('#assistant-enabled').check();
  await page.locator('#assistant-consent').check();
  await page.locator('#assistant-features input[value=email]').check();
  await page.getByRole('button', {name: 'Save preferences', exact: true}).click();
  await expect(page.locator('#assistant-message')).toHaveText('Assistant enabled.');
  await page.locator('summary').filter({hasText: 'Memory and preferences'}).click();
  await expect(page.getByRole('textbox', {name: 'SOUL.md', exact: true})).toBeVisible();
  await expectAsTyped(page.getByRole('textbox', {name: 'SOUL.md', exact: true}));
  await page.getByRole('textbox', {name: 'SOUL.md', exact: true}).fill('# Preferences\n\nKeep replies brief.');
  await page.getByRole('button', {name: 'Save SOUL.md', exact: true}).click();
  await expect(page.locator('#assistant-message')).toHaveText('Memory saved.');
  await page.locator('#assistant-exclusions').selectOption('default');
  await page.getByRole('button', {name: 'Save preferences', exact: true}).click();
  await expect(page.locator('#assistant-exclusions option:checked')).toHaveAttribute('value', 'default');
  await page.locator('#assistant-exclusions').selectOption([]);
  await page.getByRole('button', {name: 'Save preferences', exact: true}).click();
  await expect(page.locator('#assistant-exclusions option:checked')).toHaveCount(0);
  await page.reload();
  await expect(page.locator('#assistant-features input[value=email]')).toBeChecked();
  await page.locator('summary').filter({hasText: 'Memory and preferences'}).click();
  await expect(page.getByRole('textbox', {name: 'SOUL.md', exact: true})).toHaveValue(
    '# Preferences\n\nKeep replies brief.'
  );
});

test('assistant action failures show the server message instead of raw JSON', async ({page}) => {
  await app.login(page, 'password');
  await page.goto(app.origin + '/account/assistant');
  await expect(page.locator('#assistant-enabled')).toBeEnabled();
  await page.route('**/api/assistant/action/save_assistant_settings', route =>
    route.fulfill({
      status: 403,
      contentType: 'application/json',
      body: JSON.stringify({error: 'Assistant settings are locked by policy.', code: 'forbidden'})
    })
  );
  await page.getByRole('button', {name: 'Save preferences', exact: true}).click();
  await expect(page.locator('#assistant-message')).toHaveText('Assistant settings are locked by policy.');
  await page.unroute('**/api/assistant/action/save_assistant_settings');
});

test('assistant preferences remain disabled until the saved context refresh settles', async ({page}) => {
  await app.login(page, 'password');
  await page.goto(app.origin + '/account/assistant');
  await expect(page.locator('#assistant-enabled')).toBeEnabled();
  await page.locator('#assistant-enabled').check();
  await page.locator('#assistant-consent').check();
  const save = page.getByRole('button', {name: 'Save preferences', exact: true});
  await save.click();
  await expect(save).toBeEnabled();
  await expect(page.locator('#assistant-exclusions option[value=default]')).toHaveCount(1);
  await page.locator('#assistant-exclusions').selectOption('default');
  let release,
    waiting = false;
  const gate = new Promise(resolve => (release = resolve));
  await page.route('**/api/assistant/action/assistant_context', async route => {
    waiting = true;
    await gate;
    await route.continue();
  });
  try {
    await save.click();
    await expect.poll(() => waiting).toBe(true);
    await expect(page.locator('#assistant-preferences > fieldset')).toHaveJSProperty('disabled', true);
    await expect(page.locator('#assistant-exclusions')).toBeDisabled();
    release();
    await expect(page.locator('#assistant-preferences > fieldset')).toHaveJSProperty('disabled', false);
    await expect(page.locator('#assistant-message')).toHaveText('Assistant enabled.');
    await page.locator('#assistant-exclusions').selectOption([]);
    await save.click();
    await expect(save).toBeEnabled();
    await expect(page.locator('#assistant-exclusions option:checked')).toHaveCount(0);
    await page.reload();
    await expect(page.locator('#assistant-exclusions option[value=default]')).toHaveCount(1);
    await expect(page.locator('#assistant-exclusions option:checked')).toHaveCount(0);
  } finally {
    release();
  }
});

test('reply reminders explain that they need task queues', async ({page}) => {
  await app.login(page, 'password');
  await page.goto(app.origin + '/account/assistant');
  await expect(page.locator('#reply-setup')).toBeVisible();
  await expect(page.locator('#assistant-replies')).not.toBeChecked();
  await page.locator('#assistant-replies').check();
  await page.getByRole('button', {name: 'Save reply reminders', exact: true}).click();
  await expect(page.locator('#assistant-message')).toContainText('needs task queues');
  await page.reload();
  await expect(page.locator('#assistant-replies')).not.toBeChecked();
});

test('memory consent works while personal assistants are off', async ({page}) => {
  const plain = await startApp({name: 'fmn-memory-', account: {username: 'writer', password: 'DisposableMemory123!'}});
  try {
    await plain.login(page, 'password');
    await page.goto(plain.origin + '/account/assistant');
    await expect(page.locator('#assistant-message')).toContainText('Personal assistants are disabled');
    await expect(page.locator('#assistant-preferences')).toBeHidden();
    await expect(page.locator('#reply-setup')).toBeHidden();
    await expect(page.locator('#memory-notebook')).toBeHidden();
    await expect(page.locator('#assistant-memory')).toBeChecked();
    await page.locator('#assistant-memory').uncheck();
    await page.getByRole('button', {name: 'Save memory setting', exact: true}).click();
    await expect(page.locator('#assistant-message')).toHaveText('Memory setting saved.');
    await expect(page.locator('#memory-notebook')).toBeVisible();
    await page.reload();
    await expect(page.locator('#assistant-memory')).not.toBeChecked();
    await expect(page.locator('#assistant-enabled')).not.toBeChecked();
  } finally {
    await plain.close();
  }
});

test('changing the assistant policy updates the preferences without a reload', async ({page}) => {
  const plain = await startApp({
    name: 'fmn-assistant-policy-',
    account: {username: 'operator', password: 'DisposablePolicy123!'}
  });
  try {
    await plain.login(page, 'password');
    await page.goto(plain.origin + '/account/assistant');
    await expect(page.locator('#assistant-message')).toContainText('Personal assistants are disabled');
    await expect(page.locator('#assistant-preferences')).toBeHidden();
    await page.locator('#assistant-admin > summary').click();
    await expectAsTyped(page.getByRole('textbox', {name: 'Responsibilities and source links', exact: true}));
    await page.locator('#assistant-policy-enabled').check();
    await page.locator('#assistant-connection').fill('luna');
    await page.locator('#assistant-model').fill('gpt-6-luna');
    const savePolicy = page.getByRole('button', {name: 'Save assistant policy', exact: true});
    await savePolicy.click();
    await expect(page.locator('#assistant-preferences')).toBeVisible();
    await expect(savePolicy).toBeEnabled();
    await expect(page.locator('#assistant-preferences > fieldset')).toHaveJSProperty('disabled', false);
    await expect(page.locator('#assistant-enabled')).toBeEnabled();
    await page.locator('#assistant-policy-enabled').uncheck();
    await savePolicy.click();
    await expect(page.locator('#assistant-preferences')).toBeHidden();
    await expect(savePolicy).toBeEnabled();
    await expect(page.locator('#assistant-preferences > fieldset')).toHaveJSProperty('disabled', true);
  } finally {
    await plain.close();
  }
});

// Watch operations are configured but never called here: a personal run needs a
// model, and these cases check only what the form saves.
const watchConnections = {
  version: 1,
  connections: [
    {
      id: 'logs',
      label: 'Service logs',
      kind: 'http',
      endpoint: 'http://127.0.0.1:9',
      externalProcessing: true,
      operations: [
        {
          name: 'recent',
          description: 'Read recent log lines for one service',
          readOnly: true,
          method: 'GET',
          path: '/recent',
          inputSchema: {
            type: 'object',
            required: ['service'],
            properties: {
              service: {type: 'string', description: 'Service name'},
              limit: {type: 'integer', minimum: 1},
              errors: {type: 'boolean'},
              level: {enum: ['info', 'warn', 'error']},
              filter: {type: 'object'}
            },
            additionalProperties: false
          }
        },
        {name: 'summary', description: 'Read the error summary', readOnly: true, method: 'GET', path: '/summary'},
        {name: 'restart', description: 'Restart a service', readOnly: false, method: 'POST', path: '/restart'}
      ]
    },
    {
      id: 'local',
      label: 'Local metrics',
      kind: 'http',
      endpoint: 'http://127.0.0.1:9',
      operations: [{name: 'status', description: 'Read local status', readOnly: true, method: 'GET', path: '/status'}]
    }
  ]
};

async function startWatchApp(name) {
  const dir = await mkdtemp(join(tmpdir(), 'fmn-assistant-watch-config-'));
  const file = join(dir, 'connections.json');
  await writeFile(file, JSON.stringify(watchConnections));
  try {
    const watched = await startApp({
      name,
      account: {username: 'watcher', password: 'DisposableWatches123!'},
      env: {DATA_CONNECTIONS_CONFIG_FILE: file}
    });
    await watched.api('/api/organization/action/set_assistant_policy', {
      enabled: true,
      aiConnection: 'luna',
      model: 'gpt-6-luna',
      reasoningEffort: 'low'
    });
    const close = watched.close;
    watched.close = async () => {
      try {
        await close();
      } finally {
        await rm(dir, {recursive: true, force: true});
      }
    };
    return watched;
  } catch (error) {
    await rm(dir, {recursive: true, force: true});
    throw error;
  }
}

// Reads the saved settings as the page's own session does.
async function assistantAction(page, name, body = {}) {
  return page.evaluate(
    async ([name, body]) => {
      const response = await fetch('/api/assistant/action/' + name, {
        method: 'POST',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify(body)
      });
      if (!response.ok) throw Error(await response.text());
      return response.json();
    },
    [name, body]
  );
}

async function openWatches(page, origin) {
  await page.goto(origin + '/account/assistant');
  await expect(page.locator('#assistant-enabled')).toBeEnabled();
  await page.locator('#assistant-watch-section > summary').click();
}

test('data watches are built from connection, operation and argument controls', async ({page}) => {
  const watched = await startWatchApp('fmn-assistant-watches-');
  try {
    await watched.login(page, 'password');
    await openWatches(page, watched.origin);
    await page.getByRole('button', {name: 'Add data watch', exact: true}).click();
    const first = page.locator('.assistant-watch').nth(0);
    const connection = first.getByLabel('Connection', {exact: true});
    await expect(connection.locator('option[value=logs]')).toHaveJSProperty('disabled', false);
    await expect(connection.locator('option[value=local]')).toHaveJSProperty('disabled', true);
    await expect(connection.locator('option[value=local]')).toHaveText('Local metrics (no AI processing)');
    await connection.selectOption('logs');
    const operation = first.getByLabel('Operation', {exact: true});
    await expect(operation.locator('option')).toHaveText(['Choose an operation', 'recent', 'summary']);
    await operation.selectOption('recent');
    await expect(first.getByText('Read recent log lines for one service')).toBeVisible();
    await first.getByLabel('service', {exact: true}).fill('api');
    await expect(first.getByLabel('service', {exact: true})).toHaveAccessibleDescription('Service name');
    await first.getByLabel('limit', {exact: true}).fill('0');
    await first.getByLabel('errors', {exact: true}).check();
    await first.getByLabel('level', {exact: true}).selectOption('warn');
    await expect(first.getByText('Set here: filter')).toBeVisible();
    await first.getByLabel('Other arguments (JSON)', {exact: true}).fill('{"filter":{"status":500}}');
    await page.getByRole('button', {name: 'Add data watch', exact: true}).click();
    const second = page.locator('.assistant-watch').nth(1);
    await second.getByLabel('Connection', {exact: true}).selectOption('logs');
    await second.getByLabel('Operation', {exact: true}).selectOption('summary');
    await expect(second.locator('.operation-arguments')).toBeEmpty();
    const save = page.getByRole('button', {name: 'Save preferences', exact: true});
    await save.click();
    await expect(page.locator('#assistant-message')).toHaveText('Data watch recent: limit must be at least 1.');
    expect((await assistantAction(page, 'assistant_status')).settings.watches).toBeUndefined();
    await first.getByLabel('limit', {exact: true}).fill('25');
    await save.click();
    await expect(page.locator('#assistant-message')).toHaveText('Assistant paused.');
    const expected = [
      {
        connection: 'logs',
        operation: 'recent',
        arguments: {service: 'api', limit: 25, errors: true, level: 'warn', filter: {status: 500}}
      },
      {connection: 'logs', operation: 'summary', arguments: {}}
    ];
    expect((await assistantAction(page, 'assistant_status')).settings.watches).toEqual(expected);
    await openWatches(page, watched.origin);
    await expect(first.getByLabel('service', {exact: true})).toHaveValue('api');
    await expect(first.getByLabel('limit', {exact: true})).toHaveValue('25');
    await expect(first.getByLabel('errors', {exact: true})).toBeChecked();
    await expect(first.getByLabel('level', {exact: true}).locator('option:checked')).toHaveText('warn');
    expect(JSON.parse(await first.getByLabel('Other arguments (JSON)', {exact: true}).inputValue())).toEqual({
      filter: {status: 500}
    });
    await second.getByRole('button', {name: 'Remove data watch', exact: true}).click();
    await save.click();
    await expect(save).toBeEnabled();
    await expect(page.locator('.assistant-watch')).toHaveCount(1);
    expect((await assistantAction(page, 'assistant_status')).settings.watches).toEqual(expected.slice(0, 1));
  } finally {
    await watched.close();
  }
});

test('a form save keeps data watches written as JSON exactly as they were', async ({page}) => {
  const watched = await startWatchApp('fmn-assistant-watch-json-');
  try {
    await watched.login(page, 'password');
    await page.goto(watched.origin + '/account/assistant');
    await expect(page.locator('#assistant-enabled')).toBeEnabled();
    // Values the generated controls cannot show as they are: a limit that is
    // not an integer, an argument the schema does not declare, and a watch on
    // a connection that does not allow AI processing.
    const written = [
      {connection: 'logs', operation: 'recent', arguments: {limit: 'many', service: '', window: {days: 2}}},
      {connection: 'local', operation: 'status', arguments: {}}
    ];
    const {settings} = await assistantAction(page, 'assistant_status');
    await assistantAction(page, 'save_assistant_settings', {
      ...settings,
      timeZone: 'UTC',
      watches: written
    });
    const before = JSON.stringify((await assistantAction(page, 'assistant_status')).settings.watches);
    expect(JSON.parse(before)).toEqual(written);
    await openWatches(page, watched.origin);
    const first = page.locator('.assistant-watch').nth(0);
    await expect(first.getByLabel('limit', {exact: true})).toHaveCount(0);
    await expect(first.getByLabel('service', {exact: true})).toHaveValue('');
    await expect(first.getByText('Set here: limit, filter')).toBeVisible();
    expect(JSON.parse(await first.getByLabel('Other arguments (JSON)', {exact: true}).inputValue())).toEqual({
      limit: 'many',
      window: {days: 2}
    });
    const second = page.locator('.assistant-watch').nth(1);
    await expect(second.getByLabel('Connection', {exact: true}).locator('option:checked')).toHaveText(
      'Local metrics (no AI processing)'
    );
    await expect(second.getByLabel('Operation', {exact: true})).toHaveValue('status');
    await page.getByRole('button', {name: 'Save preferences', exact: true}).click();
    await expect(page.locator('#assistant-message')).toHaveText('Assistant paused.');
    expect(JSON.stringify((await assistantAction(page, 'assistant_status')).settings.watches)).toBe(before);
    await first.getByLabel('Other arguments (JSON)', {exact: true}).fill('{"service":"api"}');
    await page.getByRole('button', {name: 'Save preferences', exact: true}).click();
    await expect(page.locator('#assistant-message')).toHaveText(
      'Data watch recent: Set service in its own field, not in the JSON.'
    );
  } finally {
    await watched.close();
  }
});

for (const [width, theme] of [
  [1280, 'light'],
  [390, 'dark']
])
  test(`assistant layout ${width} ${theme}`, async ({page}, info) => {
    await page.setViewportSize({width, height: 900});
    await page.addInitScript(theme => localStorage.setItem('gitnotes:theme', theme), theme);
    await app.login(page, 'password');
    await page.goto(app.origin + '/account/assistant');
    await expect(page.locator('#assistant-features input')).toHaveCount(8);
    await expect(page.locator('html')).toHaveAttribute('data-theme', theme);
    const errors = await page.locator('main').evaluate(root => {
      const issues = [];
      if (document.documentElement.scrollWidth > innerWidth) issues.push('horizontal overflow');
      for (const input of root.querySelectorAll('input[type=checkbox]')) {
        if (!input.checkVisibility()) continue;
        const box = input.getBoundingClientRect(),
          label = input.closest('label').getBoundingClientRect();
        if (box.width < 14 || box.width > 24 || box.top > label.top + 12) issues.push('misaligned ' + input.id);
      }
      return issues;
    });
    expect(errors).toEqual([]);
    await page.screenshot({path: info.outputPath('assistant.png'), fullPage: true});
  });
