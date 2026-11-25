import {watchConfirmations, removeConfirmations} from './app-fixture.mjs';
import {test, expect} from '@playwright/test';
import http from 'node:http';
import {execFileSync} from 'node:child_process';
import {startApp} from './app-fixture.mjs';
import {join} from 'node:path';
import {expectEditorText, fillSource} from './editor-text.mjs';

let provider, app, dir, origin;
const password = 'DisposableChatControls123!';
async function api(path, body) {
  return app.api(path, body);
}
async function request(page, path, body) {
  const response = await page.request.post(origin + path, {data: body, headers: {Origin: origin}});
  expect(response.ok(), await response.text()).toBe(true);
  return response.json();
}
async function openChat(page) {
  await page.locator('#chat-open').click();
  await expect(page.locator('#chat-panel')).toBeVisible();
  await expect(page.locator('#chat-send')).toBeEnabled();
}

test.beforeAll(async () => {
  provider = http.createServer(async (request, response) => {
    for await (const chunk of request) {
    }
    response.writeHead(200, {'Content-Type': 'application/json'});
    response.end(
      JSON.stringify({
        choices: [
          {
            message: {role: 'assistant', content: '# Clear answer\n\nThe generated notebook contains a small example.'},
            finish_reason: 'stop'
          }
        ],
        usage: {prompt_tokens: 40, completion_tokens: 20}
      })
    );
  });
  await new Promise(resolve => provider.listen(0, '127.0.0.1', resolve));
  app = await startApp({
    name: 'forgetmenote-chat-controls-',
    account: {username: 'chat-controls-admin', password},
    chatConfig: {
      connections: [
        {
          id: 'local',
          label: 'Synthetic model',
          format: 'chat-completions',
          baseURL: 'http://127.0.0.1:' + provider.address().port,
          models: ['small', 'large'],
          defaultModel: 'small',
          local: true,
          capabilities: {tools: true},
          limits: {contextTokens: 16384, runSeconds: 30}
        }
      ]
    }
  });
  ({dir, origin} = app);
  for (const path of ['Controls.md', 'Delete.md'])
    await api('/p/default/api/note', {
      path,
      content: '# Synthetic example\n\nA note for UI verification.\n',
      revision: ''
    });
});
test.afterAll(async () => {
  try {
    await app?.close();
  } finally {
    if (provider) {
      provider.closeAllConnections();
      await new Promise(resolve => provider.close(resolve));
    }
  }
});
test.beforeEach(async ({page}) => {
  page.on('pageerror', error => {
    throw error;
  });
  await watchConfirmations(page, dialog => dialog.accept());
  await app.login(page, 'password');
  await page.setViewportSize({width: 1440, height: 1000});
});

test('labeled notebook chat is available from folders and highlights without covering Send', async ({page}) => {
  await page.goto(origin + '/?project=default&folder=');
  const bubble = page.locator('#chat-open');
  await expect(bubble).toHaveText('Ask notebook');
  expect(await bubble.evaluate(node => node.parentElement.tagName)).toBe('HEADER');
  for (const size of [
    {width: 1440, height: 1000},
    {width: 390, height: 844}
  ]) {
    await page.setViewportSize(size);
    await expect(bubble).toBeVisible();
    const box = await bubble.boundingBox();
    expect(box.x + box.width).toBeLessThanOrEqual(size.width);
    expect(box.x).toBeGreaterThan(0);
    expect(box.y).toBeGreaterThanOrEqual(0);
    expect(box.y).toBeLessThanOrEqual(24);
    await openChat(page);
    await expect(bubble).toHaveAttribute('aria-expanded', 'true');
    // Open, the launcher becomes the panel's quiet icon-only close control.
    await expect(bubble).toHaveAccessibleName('Close notebook chat');
    await expect(bubble).toHaveText('');
    await page.locator('#chat-input').fill('Unsent follow-up');
    const launch = await bubble.boundingBox(),
      send = await page.locator('#chat-send').boundingBox();
    expect(
      launch.x >= send.x + send.width ||
        launch.x + launch.width <= send.x ||
        launch.y >= send.y + send.height ||
        launch.y + launch.height <= send.y
    ).toBe(true);
    await bubble.click();
    await expect(page.locator('#chat-panel')).toBeHidden();
    await expect(bubble).toHaveAttribute('aria-expanded', 'false');
    await openChat(page);
    await expect(page.locator('#chat-input')).toHaveValue('Unsent follow-up');
    await bubble.click();
  }
});

test('chat and comments close each other while preserving both unsent composers', async ({page}) => {
  await page.goto(origin + '/?project=default&note=Controls.md');
  await clickNoteAction(page, 'comments-open');
  await expect(page.locator('#comments-panel')).toBeVisible();
  await page.locator('#comment-text').fill('Unposted comment');
  await openChat(page);
  await expect(page.locator('#comments-panel')).toBeHidden();
  await page.locator('#chat-input').fill('Unsent question');
  await clickNoteAction(page, 'comments-open');
  await expect(page.locator('#chat-panel')).toBeHidden();
  await expect(page.locator('#chat-open')).toHaveAttribute('aria-expanded', 'false');
  await expect(page.locator('#comment-text')).toHaveValue('Unposted comment');
  await openChat(page);
  await expect(page.locator('#comments-panel')).toBeHidden();
  await expect(page.locator('#chat-input')).toHaveValue('Unsent question');
});

test('gear opens a settings modal and finished messages keep technical counters out of the conversation', async ({
  page
}, testInfo) => {
  await page.goto(origin + '/?project=default&note=Controls.md');
  await openChat(page);
  const gear = page.getByRole('button', {name: 'Chat settings', exact: true});
  await expect(gear.locator('svg')).toHaveCount(1);
  await expect(page.locator('#chat-model')).toBeHidden();
  await gear.click();
  const dialog = page.getByRole('dialog', {name: 'Chat settings', exact: true});
  await expect(dialog).toBeVisible();
  await expect(page.locator('#chat-budget')).toHaveCount(0);
  expect(await dialog.evaluate(node => node.matches(':modal'))).toBe(true);
  await expect(page.locator('#chat-model')).toBeVisible();
  await expect(page.locator('#chat-connection')).toBeHidden();
  await page.locator('#chat-model').selectOption('large');
  await dialog.getByRole('button', {name: 'Close', exact: true}).click();
  await expect(gear).toBeFocused();
  await page.locator('#chat-mode').selectOption('suggest');
  await page.locator('#chat-input').fill('Give a short example.');
  await page.locator('#chat-send').click();
  await expect(page.locator('.chat-markdown h1')).toHaveText('Clear answer');
  const conversation = await page.locator('#chat-conversations').inputValue();
  const saved = await (await page.request.get(origin + '/p/default/api/chat/' + conversation)).json();
  expect(saved.runs[0].options.model).toBe('large');
  expect(saved.runs[0].options.mode).toBe('suggest');
  expect(saved.runs[0].options).not.toHaveProperty('budgetTokens');
  const visible = await page.locator('#chat-panel').innerText();
  expect(visible).not.toMatch(
    /input \/|output tokens|cached|requests|Model availability|Private to your account|Messages stay private|Only explicitly attached|Synthetic model|Connection/
  );
  await expect(page.locator('#chat-form').getByRole('button', {name: 'Save to note', exact: true})).toHaveCount(0);
  await page.locator('.chat-turn-assistant').getByRole('button', {name: 'Message actions', exact: true}).click();
  await page.getByRole('button', {name: 'Save to note', exact: true}).click();
  await expect(page.getByRole('dialog', {name: 'Save chat to a note'})).toBeVisible();
  await expect(page.locator('#chat-save-preview')).toContainText('Clear answer');
  await page
    .getByRole('dialog', {name: 'Save chat to a note'})
    .getByRole('button', {name: 'Close', exact: true})
    .click();
  await page.screenshot({path: testInfo.outputPath('desktop-clean-chat.png')});
  await page.setViewportSize({width: 390, height: 844});
  await gear.click();
  await expect(dialog).toBeInViewport();
  await dialog.getByRole('button', {name: 'Close', exact: true}).click();
  await expect(page.locator('#chat-send')).toBeInViewport();
  await page.screenshot({path: testInfo.outputPath('mobile-clean-chat.png')});
});

test('deleting a parent comment preserves a different author reply and keeps Git history', async ({page}) => {
  removeConfirmations(page);
  const confirmations = [];
  let acceptDeletion = false,
    deletionRequests = 0;
  await watchConfirmations(page, async dialog => {
    confirmations.push(dialog.message());
    await (acceptDeletion ? dialog.accept() : dialog.dismiss());
  });
  page.on('request', request => {
    if (request.url().endsWith('/api/comments/delete')) deletionRequests++;
  });
  const path = 'Delete.md';
  await page.goto(origin + '/?project=default&note=' + path);
  const note = await api('/p/default/api/note?path=' + path);
  const parent = await request(page, '/p/default/api/comments', {
    path,
    text: 'Parent comment to delete',
    revision: note.revision
  });
  const comment = parent.comments.at(-1);
  const replied = await api('/p/default/api/comments', {
    path,
    replyTo: comment.id,
    text: 'A different author reply survives',
    revision: parent.revision
  });
  expect(replied.comments.at(-1).replies[0].author).not.toBe(comment.author);
  await clickNoteAction(page, 'comments-open');
  const thread = page.locator('#thread-' + comment.id);
  await thread.getByRole('button', {name: 'Delete comment', exact: true}).click();
  await expect.poll(() => confirmations.length).toBe(1);
  expect(confirmations[0]).toMatch(/delete this comment/i);
  expect(confirmations[0]).toMatch(/replies.*remain/i);
  expect(deletionRequests).toBe(0);
  expect(await api('/p/default/api/comments?path=' + path)).toEqual(replied);
  await expect(thread).toContainText('Parent comment to delete');
  acceptDeletion = true;
  await thread.getByRole('button', {name: 'Delete comment', exact: true}).click();
  expect(confirmations).toHaveLength(2);
  await expect(thread).toContainText('Comment deleted');
  expect(deletionRequests).toBe(1);
  await expect(thread).not.toContainText('Parent comment to delete');
  await expect(thread).toContainText('A different author reply survives');
  const discussion = await api('/p/default/api/comments?path=' + path);
  expect(discussion.comments.at(-1).deleted).toBe(true);
  expect(discussion.comments.at(-1).text).toBe('');
  expect(discussion.comments.at(-1).replies).toHaveLength(1);
  // The deleted text stays in the notebook's Git history, as its export
  // repository has it once the background export reaches that revision.
  const exported = join(dir, 'data/exports/default.git'),
    file = 'comments/' + (await import('node:crypto')).createHash('sha256').update(path).digest('hex') + '.json';
  await expect
    .poll(
      () => {
        try {
          return execFileSync('git', ['--git-dir', exported, 'show', replied.revision + ':' + file], {
            encoding: 'utf8',
            stdio: ['ignore', 'pipe', 'ignore']
          });
        } catch {
          return '';
        }
      },
      {timeout: 15000}
    )
    .toContain('Parent comment to delete');
  acceptDeletion = false;
  await thread.getByRole('button', {name: 'Delete reply', exact: true}).click();
  expect(confirmations).toHaveLength(3);
  expect(confirmations[2]).toMatch(/delete this reply/i);
  expect(deletionRequests).toBe(1);
  expect(await api('/p/default/api/comments?path=' + path)).toEqual(discussion);
  await expect(thread).toContainText('A different author reply survives');
  acceptDeletion = true;
  await thread.getByRole('button', {name: 'Delete reply', exact: true}).click();
  await expect(thread).toHaveCount(0);
  expect(confirmations).toHaveLength(4);
  expect(deletionRequests).toBe(2);
});

async function clickNoteAction(page, id) {
  if ((await page.locator('#note-details-toggle').getAttribute('aria-expanded')) !== 'true')
    await page.locator('#note-details-toggle').click();
  await page.locator('#' + id).click();
}

test('chat uses a modal overlay until reader space is reserved and preserves editing', async ({page}) => {
  await page.goto(origin + '/?project=default&note=Controls.md');
  await page.route('**/api/live*', r => r.abort());
  await page.locator('#mode').click();
  await page.locator('#format-source').click();
  await fillSource(page, 'Private responsive draft');
  for (const width of [1050, 1051, 1100, 1279, 1280]) {
    await page.setViewportSize({width, height: 900});
    await openChat(page);
    expect(await page.locator('#main').evaluate(n => n.inert)).toBe(width < 1280);
    if (width < 1280) {
      await expect(page.locator('#chat-panel')).toHaveAttribute('aria-modal', 'true');
      await page.keyboard.press('Tab');
      expect(await page.evaluate(() => !!document.activeElement.closest('#chat-panel'))).toBe(true);
    } else {
      const main = await page.locator('#main').boundingBox(),
        panel = await page.locator('#chat-panel').boundingBox();
      expect(main.x + main.width).toBeLessThanOrEqual(panel.x + 1);
    }
    await page.locator('#chat-open').click();
    await expectEditorText(page, 'Private responsive draft');
    expect(await page.locator('#main').evaluate(n => n.inert)).toBe(false);
  }
});
test('resizing an open compact chat keeps the reader inert across the phone breakpoint', async ({page}) => {
  await page.goto(origin + '/?project=default&note=Controls.md');
  await page.setViewportSize({width: 1100, height: 844});
  await openChat(page);
  await page.setViewportSize({width: 390, height: 844});
  await expect.poll(() => page.locator('#main').evaluate(n => n.inert)).toBe(true);
  await page.setViewportSize({width: 1100, height: 844});
  await expect.poll(() => page.locator('#main').evaluate(n => n.inert)).toBe(true);
  await page.locator('#chat-open').click();
  await expect.poll(() => page.locator('#main').evaluate(n => n.inert)).toBe(false);
});
