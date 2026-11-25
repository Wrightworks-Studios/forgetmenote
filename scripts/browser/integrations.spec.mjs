import {test, expect} from '@playwright/test';
import {createServer} from 'node:http';
import {mkdtemp, writeFile, rm} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {startApp} from './app-fixture.mjs';

test('connection grants and a durable digest work through administration', async ({page}) => {
  const dir = await mkdtemp(join(tmpdir(), 'fmn-integration-config-'));
  const upstream = createServer((req, res) => {
    res.setHeader('Content-Type', 'application/json');
    res.end('{"count":7}');
  });
  await new Promise(resolve => upstream.listen(0, '127.0.0.1', resolve));
  let app;
  try {
    const connections = join(dir, 'connections.json'),
      jobs = join(dir, 'jobs.json');
    await writeFile(
      connections,
      JSON.stringify({
        version: 1,
        connections: [
          {
            id: 'fixture',
            label: 'Fixture metrics',
            kind: 'http',
            endpoint: `http://127.0.0.1:${upstream.address().port}`,
            operations: [
              {
                name: 'status',
                description: 'Read current fixture metrics',
                readOnly: true,
                method: 'GET',
                path: '/metrics'
              }
            ]
          }
        ]
      })
    );
    await writeFile(jobs, JSON.stringify({version: 1, jobs: []}));
    const account = {username: 'integration-admin', password: 'DisposableIntegration123!'};
    app = await startApp({
      name: 'fmn-integration-app-',
      account,
      env: {DATA_CONNECTIONS_CONFIG_FILE: connections, AUTOMATIONS_CONFIG_FILE: jobs}
    });
    const directory = await app.api('/api/admin');
    const admin = directory.users.find(u => u.admin && u.id !== '@local-admin');
    const service = await app.api('/api/organization/action/create_service_account', {
      name: 'Digest worker',
      steward: admin.id
    });
    await app.api('/api/members', {project: 'default', user: service.id, role: 'edit'});
    await writeFile(
      jobs,
      JSON.stringify({
        version: 1,
        jobs: [
          {
            id: 'metrics-digest',
            kind: 'digest',
            actor: service.id,
            project: 'default',
            output: 'metrics.md',
            connections: ['fixture'],
            calls: [{connection: 'fixture', operation: 'status', arguments: {}}]
          }
        ]
      })
    );
    await app.restart();
    await app.login(page, 'password');
    await page.goto(app.origin + '/?project=default');
    await page.locator('#account-actions-toggle').click();
    await page.locator('#admin-open').click();
    await page.locator('#admin-dialog [data-settings-category=connections]').click();
    await page.locator('#integration-connections').getByRole('button', {name: 'Fixture metrics · http'}).click();
    await page.getByLabel('Connection account or group').selectOption('user:' + service.id);
    await page.locator('#integration-connections').getByLabel('status · read').check();
    await page.locator('#integration-connections').getByRole('button', {name: 'Save access'}).click();
    await expect(page.locator('#integration-connections').getByRole('button', {name: 'Save access'})).toBeEnabled();
    await expect(page.getByLabel('Connection account or group')).toHaveValue('user:' + service.id);
    await expect(page.locator('#integration-connections').getByLabel('status · read')).toBeChecked();
    await expect
      .poll(async () => {
        const g = await app.api('/api/organization/action/data_connection_access', {connection: 'fixture'});
        return g.users[service.id];
      })
      .toEqual(['status']);
    await page.locator('#admin-dialog [data-settings-category=automation]').click();
    await page.locator('#service-list').getByRole('button', {name: 'Digest worker', exact: true}).click();
    const credentials = page.locator('#service-details form').filter({hasText: 'New credential'});
    await expect(credentials.getByLabel('Data access', {exact: true})).toBeVisible();
    await credentials.getByLabel('Name', {exact: true}).fill('Metrics client');
    await credentials.getByLabel('Notebooks', {exact: true}).selectOption('default');
    await credentials.getByLabel('Data access', {exact: true}).selectOption('data:read');
    await credentials.getByLabel('Connections', {exact: true}).selectOption('fixture');
    await credentials.getByRole('button', {name: 'Create credential'}).click();
    await expect(page.locator('#organization-secret-value')).toHaveValue(/^fmn_/);
    const token = await page.locator('#organization-secret-value').inputValue();
    const result = await page.request.post(app.origin + '/api/v1/actions/call_data_operation', {
      headers: {Authorization: 'Bearer ' + token},
      data: {connection: 'fixture', operation: 'status', arguments: {}}
    });
    expect(result.status(), await result.text()).toBe(200);
    expect((await result.json()).data.count).toBe(7);
    await page.locator('#organization-secret-close').click();
    await page.locator('#integration-jobs').getByRole('button', {name: 'Run now'}).click();
    await expect
      .poll(async () => {
        const s = await app.api('/api/organization/action/automation_status', {});
        return s.runs[0]?.state;
      })
      .toBe('completed');
    const note = await app.api('/p/default/api/note?path=metrics.md');
    expect(note.content).toContain('"count": 7');
    expect(note.content).toContain('"verification": "executed"');
    await page.locator('#integration-jobs').getByRole('button', {name: 'Refresh runs'}).click();
    await expect(page.locator('#integration-jobs')).toContainText('completed');
    const learningStatus = await app.api('/api/organization/action/automation_status', {});
    learningStatus.runs.unshift(
      ...[
        {id: 'created', state: 'completed', learning: {state: 'published', summary: 'Created: profiles/revenue.md.'}},
        {
          id: 'deferred',
          state: 'completed',
          learning: {state: 'deferred', summary: 'Deferred: 1 successful related calls; need 2.'}
        },
        {id: 'failed', state: 'failed', error: 'Source unavailable', learning: {state: 'failed', summary: ''}}
      ].map(run => ({
        job: 'profile-learning',
        updated: new Date().toISOString(),
        receipts: 0,
        inputTokens: 0,
        outputTokens: 0,
        ...run
      }))
    );
    const statusRoute = '**/api/organization/action/automation_status';
    await page.route(statusRoute, route => route.fulfill({json: learningStatus}));
    await page.locator('#integration-jobs').getByRole('button', {name: 'Refresh runs'}).click();
    await expect(page.locator('#integration-jobs')).toContainText('Profiles published');
    await expect(page.locator('#integration-jobs')).toContainText('Created: profiles/revenue.md.');
    await expect(page.locator('#integration-jobs')).toContainText('Deferred: 1 successful related calls; need 2.');
    await expect(page.locator('#integration-jobs')).toContainText('Learning failed');
    await expect(page.locator('#integration-jobs')).toContainText('Source unavailable');
    await page.unroute(statusRoute);
    const automationRoot = page.locator('#integration-jobs');
    await automationRoot.getByRole('button', {name: 'New automation', exact: true}).click();
    await automationRoot.getByLabel('ID', {exact: true}).fill('notebook-health');
    await automationRoot.getByLabel('Name', {exact: true}).fill('Notebook health');
    await automationRoot.getByLabel('Service account', {exact: true}).selectOption(service.id);
    await automationRoot.getByLabel('Notebook ID', {exact: true}).fill('default');
    await automationRoot.getByLabel('Trigger', {exact: true}).selectOption('calendar');
    await automationRoot.getByLabel('Timezone', {exact: true}).fill('America/Denver');
    await automationRoot.getByLabel('Hours (0–23, comma separated)').fill('9');
    await automationRoot.getByLabel('Minutes (0–59, comma separated)').fill('15');
    await automationRoot.getByRole('button', {name: 'Validate', exact: true}).click();
    await expect(automationRoot.getByRole('status')).toHaveText('Definition is valid.');
    await page.setViewportSize({width: 390, height: 844});
    {
      const layout = await page.locator('#admin-dialog').evaluate(e => ({
        width: e.clientWidth,
        scroll: e.scrollWidth,
        offenders: [...e.querySelectorAll('*')]
          .filter(
            n =>
              n.getBoundingClientRect().right >
              e.getBoundingClientRect().right - parseFloat(getComputedStyle(e).paddingRight) - 1
          )
          .map(n => ({
            tag: n.tagName,
            id: n.id,
            class: n.className,
            right: n.getBoundingClientRect().right,
            width: n.getBoundingClientRect().width
          }))
          .slice(0, 12)
      }));
      expect(layout.scroll, JSON.stringify(layout)).toBeLessThanOrEqual(layout.width + 1);
    }
    await page.screenshot({path: '/tmp/fmn-automation-editor-mobile.png'});
    await automationRoot.getByRole('button', {name: 'Save automation', exact: true}).click();
    const card = automationRoot.locator('[data-automation="notebook-health"]');
    await expect(card).toContainText('Paused');
    await card.getByRole('button', {name: 'Test', exact: true}).click();
    await expect
      .poll(async () => {
        const s = await app.api('/api/organization/action/automation_status', {});
        return s.runs.find(r => r.job === 'notebook-health')?.state;
      })
      .toBe('completed');
    await card.getByRole('button', {name: 'Edit', exact: true}).click();
    await automationRoot.getByText('JSON definition', {exact: true}).click();
    const definition = JSON.parse(await automationRoot.getByLabel('Automation JSON').inputValue());
    definition.name = 'Notebook health updated';
    await automationRoot.getByLabel('Automation JSON').fill(JSON.stringify(definition));
    await expect(automationRoot.getByLabel('Name', {exact: true})).toBeDisabled();
    await automationRoot.getByRole('button', {name: 'Apply JSON', exact: true}).click();
    await expect(automationRoot.getByLabel('Name', {exact: true})).toHaveValue('Notebook health updated');
    await automationRoot.getByRole('button', {name: 'Save automation', exact: true}).click();
    await expect(card).toContainText('Notebook health updated');
    await page.setViewportSize({width: 1280, height: 900});
    await page.screenshot({path: '/tmp/fmn-consolidation-admin-desktop.png'});
    await automationRoot.getByRole('button', {name: 'New automation', exact: true}).click();
    await automationRoot.getByLabel('Task', {exact: true}).selectOption('checks');
    await automationRoot.getByLabel('Automatically diagnose and repair checks', {exact: true}).check();
    await expect(automationRoot.getByLabel('Recipe note', {exact: true})).toBeVisible();
    await expect(automationRoot.getByLabel('Repair cooldown (seconds)', {exact: true})).toHaveValue('3600');
    await expect(automationRoot.getByLabel('Output note', {exact: true})).toBeHidden();
    const repairDefinition = JSON.parse(await automationRoot.getByLabel('Automation JSON').inputValue());
    expect(repairDefinition.repair).toEqual({cooldownSeconds: 3600});
    await page.setViewportSize({width: 390, height: 844});
    const repairLayout = await page
      .locator('#admin-dialog')
      .evaluate(e => ({width: e.clientWidth, scroll: e.scrollWidth}));
    expect(repairLayout.scroll).toBeLessThanOrEqual(repairLayout.width + 1);
    await automationRoot.getByRole('button', {name: 'Back', exact: true}).click();
    await page.setViewportSize({width: 390, height: 844});
    await page.locator('#admin-dialog [data-settings-category=connections]').click();
    {
      const layout = await page.locator('#admin-dialog').evaluate(e => ({
        width: e.clientWidth,
        scroll: e.scrollWidth,
        offenders: [...e.querySelectorAll('*')]
          .filter(
            n =>
              n.getBoundingClientRect().right >
              e.getBoundingClientRect().right - parseFloat(getComputedStyle(e).paddingRight) - 1
          )
          .map(n => ({
            tag: n.tagName,
            id: n.id,
            class: n.className,
            right: n.getBoundingClientRect().right,
            width: n.getBoundingClientRect().width
          }))
          .slice(0, 12)
      }));
      expect(layout.scroll, JSON.stringify(layout)).toBeLessThanOrEqual(layout.width + 1);
    }
    await page.screenshot({path: '/tmp/fmn-consolidation-admin-mobile.png'});
  } finally {
    await app?.close();
    await new Promise(resolve => upstream.close(resolve));
    await rm(dir, {recursive: true, force: true});
  }
});

test('administrators can remove existing grants when operation discovery is unavailable', async ({page}) => {
  const dir = await mkdtemp(join(tmpdir(), 'fmn-offline-grants-'));
  let app;
  try {
    const connections = join(dir, 'connections.json');
    await writeFile(
      connections,
      JSON.stringify({
        version: 1,
        connections: [
          {
            id: 'fixture',
            label: 'Fixture metrics',
            kind: 'http',
            endpoint: 'http://127.0.0.1:1',
            operations: [{name: 'status', readOnly: true, method: 'GET', path: '/metrics'}]
          }
        ]
      })
    );
    const account = {username: 'offline-admin', password: 'DisposableIntegration123!'};
    app = await startApp({name: 'fmn-offline-grants-app-', account, env: {DATA_CONNECTIONS_CONFIG_FILE: connections}});
    const directory = await app.api('/api/admin');
    const admin = directory.users.find(u => u.admin && u.id !== '@local-admin');
    const service = await app.api('/api/organization/action/create_service_account', {
      name: 'Offline worker',
      steward: admin.id
    });
    await app.api('/api/organization/action/set_data_connection_access', {
      connection: 'fixture',
      users: {[service.id]: ['status']}
    });
    await page.route('**/integrations/actions/list_data_operations', route =>
      route.fulfill({
        status: 503,
        contentType: 'application/json',
        body: JSON.stringify({error: {code: 'unavailable', message: 'data connection unavailable'}})
      })
    );
    await app.login(page, 'password');
    await page.goto(app.origin + '/?project=default');
    await page.locator('#account-actions-toggle').click();
    await page.locator('#admin-open').click();
    await page.locator('#admin-dialog [data-settings-category=connections]').click();
    const box = page.locator('#integration-connections');
    await box.getByRole('button', {name: 'Fixture metrics · http'}).click();
    await page.getByLabel('Connection account or group').selectOption('user:' + service.id);
    await expect(box.getByLabel('status', {exact: true})).toBeChecked();
    await box.getByLabel('status', {exact: true}).uncheck();
    await box.getByRole('button', {name: 'Save access'}).click();
    await expect
      .poll(async () => {
        const g = await app.api('/api/organization/action/data_connection_access', {connection: 'fixture'});
        return g.users[service.id];
      })
      .toEqual([]);
  } finally {
    await app?.close();
    await rm(dir, {recursive: true, force: true});
  }
});

test('Google account test and write outcomes use the existing connection panel', async ({page}) => {
  const dir = await mkdtemp(join(tmpdir(), 'fmn-google-config-'));
  let app;
  let writes = 0;
  const upstream = createServer((req, res) => {
    res.setHeader('Content-Type', 'application/json');
    if (req.url === '/token') {
      res.end(JSON.stringify({access_token: 'synthetic', token_type: 'Bearer', expires_in: 3600}));
      return;
    }
    if (req.method !== 'GET') {
      writes++;
      res.statusCode = 500;
      res.end('{"error":"fixture lost acknowledgment"}');
      return;
    }
    res.end(JSON.stringify({emailAddress: 'operations@example.test', historyId: '9007199254740995'}));
  });
  await new Promise(resolve => upstream.listen(0, '127.0.0.1', resolve));
  try {
    const endpoint = `http://127.0.0.1:${upstream.address().port}`,
      file = join(dir, 'connections.json');
    await writeFile(
      file,
      JSON.stringify({
        version: 1,
        connections: [
          {
            id: 'mail',
            label: 'Mail fixture',
            kind: 'email',
            endpoint,
            oauthRefresh: {
              tokenURL: endpoint + '/token',
              clientID: 'fixture',
              clientSecretEnv: 'FMN_BROWSER_GOOGLE_SECRET',
              refreshTokenEnv: 'FMN_BROWSER_GOOGLE_REFRESH'
            },
            email: {
              provider: 'gmail',
              mailbox: 'operations@example.test',
              workflow: 'test',
              recipients: ['client@example.test']
            },
            operations: [
              {name: 'status', readOnly: true},
              {name: 'draft-create', readOnly: false}
            ]
          }
        ]
      })
    );
    app = await startApp({
      name: 'fmn-google-ui-',
      account: {username: 'google-admin', password: 'DisposableIntegration123!'},
      env: {
        DATA_CONNECTIONS_CONFIG_FILE: file,
        FMN_BROWSER_GOOGLE_SECRET: 'synthetic',
        FMN_BROWSER_GOOGLE_REFRESH: 'synthetic'
      }
    });
    await app.login(page, 'password');
    await page.goto(app.origin + '/?project=default');
    await page.locator('#account-actions-toggle').click();
    await page.locator('#admin-open').click();
    await page.locator('#admin-dialog [data-settings-category=connections]').click();
    const panel = page.locator('#integration-connections');
    await panel.getByRole('button', {name: 'Mail fixture · Email · Gmail'}).click();
    await panel.getByRole('button', {name: 'Test connection', exact: true}).click();
    await expect(panel.locator('pre[aria-live=polite]')).toContainText('operations@example.test');
    await expect(panel.locator('pre[aria-live=polite]')).toContainText('permittedOperations');
    const outcomesResponse = page.waitForResponse(r =>
      r.url().endsWith('/api/organization/action/data_connection_effects')
    );
    await panel.getByRole('button', {name: 'Inspect write outcomes'}).click();
    const initialOutcomes = await outcomesResponse;
    expect(initialOutcomes.status(), await initialOutcomes.text()).toBe(200);
    await expect(panel.getByLabel('Write outcomes', {exact: true}).locator('pre')).toHaveText('[]');
    expect(writes).toBe(0);
    await expect(
      app.api('/integrations/actions/call_data_operation', {
        connection: 'mail',
        operation: 'draft-create',
        arguments: {actionID: 'fixture-one', to: ['client@example.test'], subject: 'Fixture', text: 'Synthetic draft'}
      })
    ).rejects.toThrow(/Google HTTP 500/);
    await panel.getByRole('button', {name: 'Inspect write outcomes'}).click();
    await panel
      .getByRole('textbox', {name: /Reconciliation evidence/})
      .fill('Fixture provider confirmed this draft was not accepted.');
    await panel.getByRole('button', {name: 'Record rejected', exact: true}).click();
    await expect(panel.getByRole('button', {name: 'Record rejected', exact: true})).toHaveCount(0);
    const effects = await app.api('/api/organization/action/data_connection_effects', {connection: 'mail'});
    expect(effects).toHaveLength(1);
    expect(effects[0].state).toBe('rejected');
    expect(writes).toBe(1);
  } finally {
    await app?.close();
    await new Promise(resolve => upstream.close(resolve));
    await rm(dir, {recursive: true, force: true});
  }
});
