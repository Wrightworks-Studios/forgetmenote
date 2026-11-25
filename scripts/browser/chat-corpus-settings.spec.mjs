import {test, expect} from '@playwright/test';
import http from 'node:http';
import {startApp} from './app-fixture.mjs';

test('embedded notebook AI defaults save with CAS and start new chats without changing saved selections', async ({
  page
}) => {
  test.setTimeout(120000);
  let app;
  const provider = http.createServer(async (request, response) => {
    for await (const chunk of request) {
    }
    response.setHeader('Content-Type', 'application/json');
    response.end(
      JSON.stringify({
        choices: [{message: {role: 'assistant', content: 'Synthetic scoped answer.'}, finish_reason: 'stop'}],
        usage: {prompt_tokens: 20, completion_tokens: 5}
      })
    );
  });
  await new Promise(resolve => provider.listen(0, '127.0.0.1', resolve));
  try {
    app = await startApp({
      name: 'forgetmenote-chat-corpus-',
      account: {username: 'corpus-admin', password: 'DisposableCorpusPassword123!'},
      chatConfig: {
        connections: [
          {
            id: 'local',
            label: 'Synthetic',
            format: 'chat-completions',
            baseURL: 'http://127.0.0.1:' + provider.address().port,
            models: ['synthetic'],
            local: true,
            capabilities: {tools: true},
            limits: {runSeconds: 10}
          }
        ]
      }
    });
    const {origin, api} = app;
    await app.login(page, 'password');
    page.on('pageerror', error => {
      throw error;
    });
    await page.goto(origin + '/?project=default&folder=');
    await expect(page.locator('#folder-title')).toHaveText('My notes');
    const before = await api('/p/default/api/knowledge/retrieval-settings');
    await page.locator('#settings-open').click();
    await page.locator('[data-settings-category=search]').click();
    await expect(page.locator('#default-ai-archives')).toHaveValue('exclude');
    await expect(page.locator('#default-ai-drafts')).toHaveValue('exclude');
    await page.locator('#default-ai-archives').selectOption('lower');
    await page.locator('#default-ai-drafts').selectOption('include');
    await page.locator('#retrieval-defaults-save').click();
    await expect(page.locator('#retrieval-defaults-status')).toHaveText('Defaults saved.');
    const saved = await api('/p/default/api/knowledge/retrieval-settings');
    expect(saved.settings.search).toEqual(before.settings.search);
    expect(saved.settings.ai).toEqual({version: 1, archives: 'lower', drafts: 'include'});
    expect(saved.id).not.toBe(before.id);
    // A stale form must preserve newer settings and tell the user to reload.
    await api('/p/default/api/knowledge/retrieval-settings', {
      settings: {...saved.settings, ai: {version: 1, archives: 'include', drafts: 'lower'}},
      expectedId: saved.id
    });
    await page.locator('#default-ai-drafts').selectOption('exclude');
    await page.locator('#retrieval-defaults-save').click();
    await expect(page.locator('#retrieval-defaults-status')).toContainText('Reopen notebook settings');
    expect((await api('/p/default/api/knowledge/retrieval-settings')).settings.ai).toEqual({
      version: 1,
      archives: 'include',
      drafts: 'lower'
    });
    await page.locator('[data-close="settings-dialog"]').click();
    await page.locator('#chat-open').click();
    await expect(page.locator('#chat-model')).toHaveValue('synthetic');
    await page.getByRole('button', {name: 'Chat settings', exact: true}).click();
    await expect(page.locator('#chat-archives')).toHaveValue('include');
    await expect(page.locator('#chat-drafts')).toHaveValue('lower');
    await expect(page.locator('#chat-history')).not.toBeChecked();
    await page.locator('#chat-settings').getByRole('button', {name: 'Close', exact: true}).click();
    await page.locator('#chat-input').fill('Explain the selected notebook context.');
    await page.locator('#chat-send').click();
    await expect(page.locator('#chat-turns')).toContainText('Synthetic scoped answer.');
    const list = await (await page.request.get(origin + '/p/default/api/chat')).json(),
      id = list.conversations[0].id,
      chat = await (await page.request.get(origin + '/p/default/api/chat/' + id)).json(),
      corpus = {version: 1, archives: 'include', drafts: 'lower', history: false};
    expect(chat.options.corpus).toEqual(corpus);
    expect(chat.runs[0].options.corpus).toEqual(corpus);
    const latest = await api('/p/default/api/knowledge/retrieval-settings');
    await api('/p/default/api/knowledge/retrieval-settings', {
      settings: {...latest.settings, ai: {version: 1, archives: 'exclude', drafts: 'exclude'}},
      expectedId: latest.id
    });
    await page.reload();
    await expect(page.locator('#chat-panel')).toBeVisible();
    await page.getByRole('button', {name: 'Chat settings', exact: true}).click();
    await expect(page.locator('#chat-archives')).toHaveValue('include');
    await expect(page.locator('#chat-drafts')).toHaveValue('lower');
    await page.screenshot({path: 'test-results/chat-corpus-settings-desktop.png', fullPage: true});
    await page.locator('#chat-settings').getByRole('button', {name: 'Close', exact: true}).click();
    await page.locator('#chat-new').click();
    await page.getByRole('button', {name: 'Chat settings', exact: true}).click();
    await expect(page.locator('#chat-archives')).toHaveValue('exclude');
    await expect(page.locator('#chat-drafts')).toHaveValue('exclude');
    await page.setViewportSize({width: 390, height: 667});
    await expect(page.locator('#chat-history')).toBeInViewport();
    await page.screenshot({path: 'test-results/chat-corpus-settings-mobile.png', fullPage: true});
    await page.keyboard.press('Escape');
    await expect(page.locator('#chat-settings')).toBeHidden();
    await expect(page.locator('#chat-settings-open')).toBeFocused();
  } finally {
    try {
      await app?.close();
    } finally {
      provider.closeAllConnections();
      await new Promise(resolve => provider.close(resolve));
    }
  }
});
