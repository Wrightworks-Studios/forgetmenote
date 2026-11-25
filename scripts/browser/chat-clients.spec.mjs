import {test, expect} from '@playwright/test';
import {mkdtemp, writeFile, rm} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {startApp} from './app-fixture.mjs';

test('chat client administration persists scoped channels and linked accounts on desktop and mobile', async ({
  page
}, testInfo) => {
  const dir = await mkdtemp(join(tmpdir(), 'fmn-chat-client-'));
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
    const directory = await app.api('/api/admin'),
      admin = directory.users.find(u => u.admin && u.id !== '@local-admin');
    await app.login(page, 'password');
    await page.goto(app.origin);
    await page.locator('#account-actions-toggle').click();
    await page.locator('#admin-open').click();
    await page.locator('#admin-dialog [data-settings-category=chat-clients]').click();
    const panel = page.locator('#chat-clients-panel');
    await expect(panel.getByLabel('Workspace ID', {exact: true})).toHaveValue('T123');
    await expect(panel.getByLabel('Slack event URL')).toHaveValue(app.origin + '/integrations/slack/events');
    await expect(panel.getByLabel('Slack interactivity URL')).toHaveValue(
      app.origin + '/integrations/slack/interactions'
    );
    await expect(panel.getByLabel('Slack /task command URL')).toHaveValue(app.origin + '/integrations/slack/commands');
    await panel.getByRole('button', {name: 'Link account'}).click();
    await panel.getByLabel('Chat user ID', {exact: true}).fill('U123');
    await panel.getByLabel('ForgetMeNote user ID', {exact: true}).fill(admin.id);
    await panel.getByRole('button', {name: 'Add channel', exact: true}).click();
    const channel = panel.getByRole('group', {name: 'Channel', exact: true});
    await channel.getByLabel('Channel ID', {exact: true}).fill('C123');
    await channel.getByLabel('Primary notebook', {exact: true}).fill('default');
    await channel.getByLabel('Allowed members', {exact: true}).fill(admin.id);
    await channel.getByLabel('Shared replies', {exact: true}).check();
    await channel.getByLabel('Approve this channel audience').check();
    await channel.getByLabel('Include thread context').check();
    await channel.getByLabel('Process attached files').check();
    await panel.getByRole('button', {name: 'Save chat client'}).click();
    await expect(panel.getByRole('status')).toHaveText('Chat client saved.');
    const saved = await app.api('/api/organization/action/chat_clients', {});
    expect(saved.config.clients.slack.users.U123).toBe(admin.id);
    expect(saved.config.clients.slack.channels.C123.shared).toBe(true);
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
