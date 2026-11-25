import {test, expect} from '@playwright/test';
import http from 'node:http';
import {startApp} from './app-fixture.mjs';

// Use the embedded app, real chat persistence and safe Markdown preview. The
// synthetic provider deliberately produces the noisy multi-round conversation.
test('embedded chat prioritizes answers over many tools and sources on desktop and mobile', async ({
  page
}, testInfo) => {
  test.setTimeout(120000);
  page.setDefaultTimeout(10000);
  let app,
    round = 0;
  const provider = http.createServer(async (request, response) => {
    for await (const chunk of request) {
    }
    round++;
    const message =
      round <= 2
        ? {
            role: 'assistant',
            content: `I will search the next evidence batch ${round}.`,
            tool_calls: Array.from({length: 6}, (_, i) => ({
              id: `read-${round}-${i}`,
              type: 'function',
              function: {name: 'read_note', arguments: JSON.stringify({path: `Evidence-${(round - 1) * 6 + i + 1}.md`})}
            }))
          }
        : {
            role: 'assistant',
            content:
              '# Clear answer\n\nThe notebook supports **three useful conclusions**. These conclusions stay readable after a long tool run.\n\n- First finding with supporting evidence.\n- Second finding with supporting evidence.\n- Third finding with supporting evidence.\n\n| Topic | Result |\n| --- | --- |\n| Evidence | Reviewed |\n\n```text\nA small, readable example\n```\n\n<script>window.untrustedChatExecuted=true</script>'
          };
    response.writeHead(200, {'Content-Type': 'application/json'});
    response.end(
      JSON.stringify({
        choices: [{message, finish_reason: round <= 2 ? 'tool_calls' : 'stop'}],
        usage: {prompt_tokens: 80, completion_tokens: 30}
      })
    );
  });
  await new Promise(resolve => provider.listen(0, '127.0.0.1', resolve));
  try {
    app = await startApp({
      name: 'forgetmenote-chat-readable-',
      account: {username: 'chat-readable-admin', password: 'DisposableReadablePassword123!'},
      chatConfig: {
        connections: [
          {
            id: 'local',
            label: 'Synthetic readable model',
            format: 'chat-completions',
            baseURL: 'http://127.0.0.1:' + provider.address().port,
            models: ['synthetic'],
            local: true,
            capabilities: {tools: true},
            limits: {contextTokens: 65536, runTokens: 250000, runSeconds: 60}
          }
        ]
      }
    });
    const {origin, api} = app;
    for (let i = 1; i <= 12; i++)
      await api('/p/default/api/note', {
        path: `Evidence-${i}.md`,
        content: `# Synthetic evidence ${i}\n\nReadable notebook evidence record ${i}. This fixture contains no personal content.\n`,
        revision: ''
      });
    await app.login(page, 'password');
    await page.setViewportSize({width: 1440, height: 1000});
    await page.goto(origin + '/?project=default&folder=');
    await page.locator('#chat-open').click();
    await expect(page.locator('#chat-model')).toHaveValue('synthetic');
    await page.locator('#chat-input').fill('Summarize the twelve synthetic evidence notes.');
    await page.locator('#chat-send').click();
    await expect(page.locator('.chat-markdown h1')).toHaveText('Clear answer', {timeout: 65000});
    await expect(page.locator('#chat-send')).toBeEnabled();
    expect(round).toBe(3);
    const conversation = await page.locator('#chat-conversations').inputValue();
    const saved = await page.request.get(origin + '/p/default/api/chat/' + conversation);
    const transcript = await saved.json();
    expect(transcript.runs[0].operations).toHaveLength(12);
    expect(transcript.turns.filter(turn => turn.role === 'assistant')).toHaveLength(3);
    await page.screenshot({path: testInfo.outputPath('desktop-chat.png')});
    await expect(page.getByRole('button', {name: 'Chat settings', exact: true})).toBeVisible();
    await expect(page.locator('#chat-settings')).not.toHaveAttribute('open');
    await expect(page.locator('#chat-mode')).toHaveValue('ask');
    await expect(page.locator('#chat-connection')).toBeHidden();
    await expect(page.locator('.chat-activity')).not.toHaveAttribute('open');
    await expect(page.locator('.chat-sources')).not.toHaveAttribute('open');
    await expect(page.getByText('I will search the next evidence batch 1.', {exact: true})).toBeHidden();
    await expect(page.locator('.chat-operation')).toHaveCount(12);
    await expect(page.locator('.chat-operation').first()).toBeHidden();
    await expect(page.locator('.chat-markdown strong')).toHaveText('three useful conclusions');
    await expect(page.locator('.chat-markdown li')).toHaveCount(3);
    await expect(page.locator('.chat-markdown table')).toBeVisible();
    await expect(page.locator('.chat-markdown pre code')).toHaveText('A small, readable example\n');
    expect(await page.evaluate(() => window.untrustedChatExecuted)).toBeUndefined();
    const prose = await page
      .locator('.chat-content')
      .evaluateAll(nodes => nodes.map(node => getComputedStyle(node).fontFamily));
    expect(new Set(prose).size).toBe(1);
    expect(prose[0]).toContain('Source Serif 4');
    expect(await page.locator('.chat-markdown h1').evaluate(node => getComputedStyle(node).fontFamily)).toBe(prose[0]);
    const userStyle = await page.locator('.chat-turn-user').evaluate(node => ({
      background: getComputedStyle(node).backgroundColor,
      radius: getComputedStyle(node).borderRadius
    }));
    expect(userStyle).toEqual({background: 'rgba(0, 0, 0, 0)', radius: '0px'});
    const geometry = () =>
      page.evaluate(() =>
        Object.fromEntries(
          ['chat-panel', 'chat-transcript', 'chat-form', 'chat-send'].map(id => {
            const box = document.getElementById(id).getBoundingClientRect();
            return [id, {top: box.top, bottom: box.bottom, height: box.height, width: box.width}];
          })
        )
      );
    const desktop = await geometry();
    expect(desktop['chat-transcript'].height).toBeGreaterThan(desktop['chat-panel'].height * 0.55);
    await page.locator('.chat-activity > summary').click();
    await expect(page.getByText('I will search the next evidence batch 1.', {exact: true})).toBeVisible();
    await expect(page.locator('.chat-operation').first()).toBeVisible();
    await expect(page.locator('.chat-operation pre').first()).toBeHidden();
    await page.locator('.chat-operation details > summary').first().click();
    await expect(page.locator('.chat-operation pre').first()).toContainText('Evidence-1.md');
    await page.locator('.chat-activity > summary').click();
    await page.locator('.chat-sources > summary').click();
    const citations = page.locator('.chat-sources .chat-citation');
    expect(await citations.count()).toBeGreaterThanOrEqual(12);
    const labels = await citations.allTextContents();
    expect(new Set(labels).size).toBe(labels.length);
    await citations.first().click();
    await expect(page.locator('#knowledge-dialog')).toBeVisible();
    await page.locator('#knowledge-dialog').getByRole('button', {name: 'Close', exact: true}).click();
    await page.locator('.chat-sources > summary').click();
    await page.getByRole('button', {name: 'Chat settings', exact: true}).click();
    await expect(page.locator('#chat-settings')).toHaveAttribute('open', '');
    await page.screenshot({path: testInfo.outputPath('desktop-chat-settings.png')});
    await page.locator('#chat-settings').getByRole('button', {name: 'Close', exact: true}).click();
    await page.locator('#chat-mode').selectOption('suggest');
    await expect(page.locator('#chat-mode')).toHaveValue('suggest');
    await page.locator('#chat-input').fill('Unsent readable follow-up');
    await page.setViewportSize({width: 390, height: 844});
    const mobile = await geometry();
    expect(mobile['chat-panel'].width).toBe(390);
    expect(mobile['chat-transcript'].height).toBeGreaterThan(mobile['chat-panel'].height * 0.5);
    expect(mobile['chat-send'].bottom).toBeLessThanOrEqual(844);
    await page.locator('.chat-markdown h1').scrollIntoViewIfNeeded();
    await expect(page.locator('.chat-markdown h1')).toBeInViewport();
    await page.screenshot({path: testInfo.outputPath('mobile-chat.png')});
    await page.getByRole('button', {name: 'Chat settings', exact: true}).click();
    await page.locator('#chat-options > summary').click();
    await page.locator('#chat-review').check();
    const expanded = await geometry();
    expect(expanded['chat-send'].bottom).toBeLessThanOrEqual(844);
    await page.locator('#chat-settings').getByRole('button', {name: 'Close', exact: true}).click();
    await page.locator('#chat-mode').selectOption('suggest');
    await expect(page.locator('#chat-mode')).toHaveValue('suggest');
    await page.locator('#chat-open').click();
    await expect(page.locator('#folder-browser')).toBeVisible();
    await page.locator('#chat-open').click();
    await expect(page.locator('#chat-conversations')).toHaveValue(conversation);
    await expect(page.locator('#chat-input')).toHaveValue('Unsent readable follow-up');
  } finally {
    try {
      await app?.close();
    } finally {
      provider.closeAllConnections();
      await new Promise(resolve => provider.close(resolve));
    }
  }
});
