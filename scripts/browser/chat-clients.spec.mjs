import {test, expect} from '@playwright/test';
import {mkdtemp, writeFile, rm} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {startApp} from './app-fixture.mjs';

// A Slack app with synthetic credentials. The pages here never reach Slack:
// each test answers the app's listing of Slack channels and people itself.
async function startSlackApp(dir) {
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
  return startApp({
    name: 'fmn-chat-client-app-',
    account: {username: 'chat-admin', password: 'DisposableChat123!'},
    env: {SLACK_CONFIG_FILE: config},
    chatConfig: {
      connections: [
        {
          id: 'local',
          label: 'Local fixture',
          format: 'chat-completions',
          baseURL: 'http://127.0.0.1:1',
          local: true,
          models: ['synthetic'],
          defaultModel: 'synthetic'
        }
      ],
      defaultConnection: 'local'
    }
  });
}

async function openChatClients(app, page) {
  await app.login(page, 'password');
  await page.goto(app.origin);
  await page.locator('#account-actions-toggle').click();
  await page.locator('#admin-open').click();
  await page.locator('#admin-dialog [data-settings-category=chat-clients]').click();
  return page.locator('#chat-clients-panel');
}

test('chat client administration persists scoped channels and linked accounts on desktop and mobile', async ({
  page
}, testInfo) => {
  const dir = await mkdtemp(join(tmpdir(), 'fmn-chat-client-'));
  let app;
  try {
    app = await startSlackApp(dir);
    // A Slack app without the listing scopes: every ID is typed.
    await page.route('**/api/organization/action/chat_client_directory', route =>
      route.fulfill({
        json: {
          channels: [],
          users: [],
          unlisted: {
            channels: 'the Slack app needs the channels:read and groups:read scopes to list these',
            users: 'the Slack app needs the users:read scope to list these'
          }
        }
      })
    );
    await app.api('/api/projects', {id: 'reports', name: 'Reports'});
    await app.api('/api/projects', {id: 'archive', name: 'Archive'});
    const directory = await app.api('/api/admin'),
      admin = directory.users.find(u => u.admin && u.id !== '@local-admin'),
      adminName = admin.displayName || directory.usernames?.[admin.id] || admin.id;
    const panel = await openChatClients(app, page);
    await expect(panel.getByLabel('Workspace ID', {exact: true})).toHaveValue('T123');
    await expect(panel.getByText('Channel IDs are typed: the Slack app needs the channels:read')).toBeVisible();
    await expect(panel.getByLabel('Slack event URL')).toHaveValue(app.origin + '/integrations/slack/events');
    await expect(panel.getByLabel('Slack interactivity URL')).toHaveValue(
      app.origin + '/integrations/slack/interactions'
    );
    await expect(panel.getByLabel('Slack /task command URL')).toHaveValue(app.origin + '/integrations/slack/commands');
    await panel.getByRole('button', {name: 'Link account'}).click();
    await panel.getByLabel('Chat user ID', {exact: true}).fill('U123');
    await panel.getByLabel('ForgetMeNote account', {exact: true}).selectOption(admin.id);
    await panel.getByRole('button', {name: 'Add channel', exact: true}).click();
    const channel = panel.getByRole('group', {name: 'Channel', exact: true});
    await channel.getByLabel('Channel ID', {exact: true}).fill('C123');
    await channel.getByLabel('Primary notebook', {exact: true}).selectOption('default');
    await channel
      .getByRole('group', {name: 'Allowed members', exact: true})
      .getByLabel(adminName, {exact: true})
      .check();
    await channel.getByLabel('Shared replies', {exact: true}).check();
    await channel.getByLabel('Approve this channel audience').check();
    await channel.getByLabel('Include thread context').check();
    await channel.getByLabel('Process attached files').check();
    await panel.getByRole('button', {name: 'Save chat client'}).click();
    await expect(panel.getByRole('status')).toHaveText('Chat client saved.');
    const saved = await app.api('/api/organization/action/chat_clients', {});
    expect(saved.config.clients.slack.users.U123).toBe(admin.id);
    expect(saved.config.clients.slack.channels.C123).toMatchObject({
      project: 'default',
      actors: [admin.id],
      shared: true,
      audienceApproved: true,
      threadHistory: true,
      attachments: true,
      externalProcessing: true
    });
    // A definition written as JSON keeps its order and its unlisted values
    // through an unchanged form save.
    await panel.getByText('JSON configuration', {exact: true}).click();
    const json = panel.getByLabel('Chat client JSON', {exact: true});
    const written = JSON.parse(await json.inputValue());
    written.channels.C123.readProjects = ['reports', 'archive'];
    written.channels.C123.connections = ['retired'];
    await json.fill(JSON.stringify(written));
    await panel.getByRole('button', {name: 'Save chat client'}).click();
    await expect(panel.getByLabel('Primary notebook', {exact: true})).toHaveValue('default');
    const notebooks = channel.getByRole('group', {name: 'Additional notebooks', exact: true});
    await expect(notebooks.getByLabel('Reports (reports)', {exact: true})).toBeChecked();
    await expect(notebooks.getByLabel('Archive (archive)', {exact: true})).toBeChecked();
    await expect(
      channel.getByRole('group', {name: 'Data connections', exact: true}).getByLabel('retired (not listed)')
    ).toBeChecked();
    const fromJSON = (await app.api('/api/organization/action/chat_clients', {})).config;
    await panel.getByRole('button', {name: 'Save chat client'}).click();
    await expect(panel.getByRole('status')).toHaveText('Chat client saved.');
    const fromForm = (await app.api('/api/organization/action/chat_clients', {})).config;
    expect(fromForm.revision).toBe(fromJSON.revision + 1);
    expect(JSON.stringify(fromForm.clients)).toBe(JSON.stringify(fromJSON.clients));
    expect(JSON.stringify(saved)).not.toContain('synthetic-bot');
    for (const width of [1280, 390]) {
      await page.setViewportSize({width, height: 900});
      const layout = await page.locator('#admin-dialog').evaluate(e => ({
        width: e.clientWidth,
        scroll: e.scrollWidth,
        checks: [...e.querySelectorAll('#chat-clients-panel input[type=checkbox]')].map(n => {
          const a = n.getBoundingClientRect(),
            b = n.nextElementSibling.getBoundingClientRect();
          return {width: a.width, top: a.top, labelTop: b.top, left: a.left, labelLeft: b.left};
        })
      }));
      expect(layout.scroll, JSON.stringify(layout)).toBeLessThanOrEqual(layout.width + 1);
      for (const c of layout.checks) {
        expect(c.width).toBeLessThanOrEqual(20);
        expect(c.labelLeft).toBeGreaterThan(c.left);
        expect(Math.abs(c.top - c.labelTop)).toBeLessThan(8);
      }
    }
    await page.screenshot({path: testInfo.outputPath('chat-clients-mobile.png')});
    await page.setViewportSize({width: 1280, height: 900});
    await app.restart();
    await app.login(page, 'password');
    await page.reload();
    await expect(page).toHaveURL(/page=administration/);
    await expect(page).toHaveURL(/category=chat-clients/);
    await expect(panel).toBeVisible();
    await expect(panel.getByLabel('Channel ID', {exact: true})).toHaveValue('C123');
    await expect(panel.getByLabel('Shared replies', {exact: true})).toBeChecked();
  } finally {
    await app?.close();
    await rm(dir, {recursive: true, force: true});
  }
});

test('chat client settings choose Slack channels and people by name once the app lists them', async ({page}) => {
  const dir = await mkdtemp(join(tmpdir(), 'fmn-chat-client-'));
  let app;
  try {
    app = await startSlackApp(dir);
    const directory = await app.api('/api/admin'),
      admin = directory.users.find(u => u.admin && u.id !== '@local-admin');
    const chat = await app.api('/api/organization/action/chat_clients', {});
    await app.api('/api/organization/action/set_chat_clients', {
      config: {
        ...chat.config,
        clients: {
          slack: {
            ...chat.config.clients.slack,
            disabled: false,
            channels: {C999: {project: 'default', externalProcessing: true}}
          }
        }
      }
    });
    let release;
    const held = new Promise(resolve => (release = resolve));
    await page.route('**/api/organization/action/chat_client_directory', async route => {
      await held;
      await route.fulfill({
        json: {
          channels: [
            {id: 'C123', name: 'general'},
            {id: 'G456', name: 'planning'}
          ],
          users: [{id: 'U123', name: 'Ada Lovelace'}]
        }
      });
    });
    const panel = await openChatClients(app, page);
    await expect(panel.getByText('Listing Slack channels and people…')).toBeVisible();
    // A field typed before the listing arrives stays as typed.
    await panel.getByRole('button', {name: 'Add direct messages', exact: true}).click();
    const direct = panel.getByRole('group', {name: 'Direct messages', exact: true});
    await direct.getByRole('textbox', {name: 'DM user ID', exact: true}).fill('U123');
    await direct.getByLabel('Primary notebook', {exact: true}).selectOption('default');
    await panel.getByRole('button', {name: 'Link account'}).click();
    release();
    const saved = panel.getByRole('group', {name: 'Channel', exact: true}).first();
    await expect(saved.getByRole('combobox', {name: 'Channel ID', exact: true})).toHaveValue('C999');
    await expect(saved.getByRole('option', {name: 'C999 (not listed)'})).toHaveCount(1);
    await expect(direct.getByRole('textbox', {name: 'DM user ID', exact: true})).toHaveValue('U123');
    await expect(panel.getByText('Listing Slack channels and people…')).toHaveCount(0);
    const person = panel.getByRole('combobox', {name: 'Chat user ID', exact: true});
    await expect(person.getByRole('option', {name: 'Ada Lovelace (U123)'})).toHaveCount(1);
    await person.selectOption('U123');
    await panel.getByLabel('ForgetMeNote account', {exact: true}).selectOption(admin.id);
    await panel.getByRole('button', {name: 'Add channel', exact: true}).click();
    const added = panel.getByRole('group', {name: 'Channel', exact: true}).last();
    await added.getByRole('combobox', {name: 'Channel ID', exact: true}).selectOption({label: '#planning (G456)'});
    await added.getByLabel('Primary notebook', {exact: true}).selectOption('default');
    await panel.getByRole('button', {name: 'Save chat client'}).click();
    await expect(panel.getByRole('status')).toHaveText('Chat client saved.');
    const after = (await app.api('/api/organization/action/chat_clients', {})).config.clients.slack;
    expect(after.users).toEqual({U123: admin.id});
    expect(Object.keys(after.channels).sort()).toEqual(['C999', 'G456']);
    expect(Object.keys(after.directMessages)).toEqual(['U123']);
  } finally {
    await app?.close();
    await rm(dir, {recursive: true, force: true});
  }
});
