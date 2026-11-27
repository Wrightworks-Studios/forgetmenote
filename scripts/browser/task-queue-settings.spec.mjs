import {test, expect} from '@playwright/test';
import {mkdtemp, writeFile, rm} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {startApp} from './app-fixture.mjs';

test('task queues are turned on and changed from Administration with the same settings as the API', async ({page}) => {
  const dir = await mkdtemp(join(tmpdir(), 'fmn-task-settings-'));
  let app;
  try {
    const signing = join(dir, 'signing'),
      bot = join(dir, 'bot'),
      config = join(dir, 'slack.json');
    await writeFile(signing, 'synthetic-signing', {mode: 0o600});
    await writeFile(bot, 'synthetic-bot', {mode: 0o600});
    await writeFile(
      config,
      JSON.stringify({team: 'T123', signingSecretFile: signing, botTokenFile: bot, users: {}, channels: {}}),
      {mode: 0o600}
    );
    app = await startApp({
      name: 'fmn-task-settings-app-',
      account: {username: 'task-admin', password: 'DisposableTasks123!'},
      env: {SLACK_CONFIG_FILE: config}
    });
    const settings = () => app.api('/api/v1/actions/task_queues', {});
    await app.login(page, 'password');
    await page.goto(app.origin);
    await page.locator('#account-actions-toggle').click();
    await page.locator('#admin-open').click();
    await page.locator('#admin-dialog [data-settings-category=tasks]').click();
    const panel = page.locator('#task-queues-panel');
    // Slack configured in the server's file is not a saved chat client yet.
    await expect(panel.getByLabel('Messaging client', {exact: true}).locator('option')).toHaveText(['None']);
    await expect(panel).toContainText('Save the client under Chat clients first');
    const chat = await app.api('/api/organization/action/chat_clients', {});
    await app.api('/api/organization/action/set_chat_clients', {config: chat.config});
    const group = await app.api('/api/organization/action/set_group', {id: 'planners', name: 'Planners', members: []});
    await panel.getByRole('button', {name: 'Refresh', exact: true}).click();
    await expect(panel).toContainText('Off. Turning them on creates the task-queues notebook');
    const client = panel.getByLabel('Messaging client', {exact: true});
    await expect(client.locator('option')).toHaveText(['None', 'Slack']);
    // A first setup starts from the only configured client.
    await expect(client).toHaveValue('slack');
    const accounts = panel.getByLabel('Accounts that may use tasks', {exact: true});
    await expect(accounts.locator('option')).toHaveText(['Everyone with an account', 'Planners']);
    await accounts.selectOption(group.id);
    await panel.getByLabel('Away statuses (one per line)', {exact: true}).fill('OOO\nParental leave\n');
    await expect(panel.getByLabel('Pause every task action', {exact: true})).toHaveCount(0);
    await expect(panel.locator('.settings-warning')).toHaveCount(0);
    await panel.getByRole('button', {name: 'Turn on task queues', exact: true}).click();
    await expect(panel.getByRole('status')).toHaveText('Task queues are on.');
    // Until a task-reminder automation exists, nobody is reminded, and the page says so.
    const noReminders =
      "No enabled task-reminder automation names the client slack, so nobody gets weekday task reminders. Add one that runs as the client's members actor every 15 minutes.";
    await expect(panel.locator('.settings-warning')).toHaveText([noReminders]);
    expect(await settings()).toEqual({
      project: 'task-queues',
      client: 'slack',
      workspace: 'T123',
      group: 'planners',
      awayStatuses: ['ooo', 'parental leave'],
      revision: 1,
      warnings: [noReminders]
    });

    // Settings saved through the API come back unchanged from an unchanged form.
    await app.api('/api/v1/actions/set_task_queues', {
      project: '',
      client: 'slack',
      holidayTitles: ['Thanksgiving', 'holiday'],
      awayStatuses: ['pto'],
      revision: 1
    });
    const fromAPI = await settings();
    expect(fromAPI.holidayTitles).toEqual(['holiday', 'thanksgiving']);
    await panel.getByRole('button', {name: 'Refresh', exact: true}).click();
    await expect(accounts).toHaveValue('');
    await expect(panel.getByLabel('Holiday titles (one per line)', {exact: true})).toHaveValue('holiday\nthanksgiving');
    await panel.getByRole('button', {name: 'Save task queues', exact: true}).click();
    await expect(panel.getByRole('status')).toHaveText('Task queues saved.');
    const fromForm = await settings();
    expect(fromForm.revision).toBe(fromAPI.revision + 1);
    expect({...fromForm, revision: 0}).toEqual({...fromAPI, revision: 0});

    // A change saved elsewhere since the form was drawn is refused, not overwritten.
    await app.api('/api/v1/actions/set_task_queues', {...fromForm, awayStatuses: ['away']});
    await panel.getByLabel('Pause every task action', {exact: true}).check();
    await panel.getByRole('button', {name: 'Save task queues', exact: true}).click();
    await expect(panel.getByRole('alert')).toHaveText(
      'Task queue settings changed since this page loaded. Refresh to see them, then save again.'
    );
    expect(await settings()).toMatchObject({awayStatuses: ['away'], revision: fromForm.revision + 1});
    expect((await settings()).disabled).toBeUndefined();
    await panel.getByRole('button', {name: 'Refresh', exact: true}).click();
    await panel.getByLabel('Pause every task action', {exact: true}).check();
    await panel.getByRole('button', {name: 'Save task queues', exact: true}).click();
    await expect(panel).toContainText('Paused: every task action is refused');
    expect(await settings()).toMatchObject({disabled: true, awayStatuses: ['away']});
    for (const width of [1280, 390]) {
      await page.setViewportSize({width, height: 900});
      const bounds = await page
        .locator('#admin-dialog')
        .evaluate(element => ({width: element.clientWidth, scroll: element.scrollWidth}));
      expect(bounds.scroll).toBeLessThanOrEqual(bounds.width + 1);
    }
    await page.screenshot({path: test.info().outputPath('task-queues-mobile.png'), fullPage: true});
  } finally {
    await app?.close();
    await rm(dir, {recursive: true, force: true});
  }
});
