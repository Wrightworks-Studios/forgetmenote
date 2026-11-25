import {test, expect} from '@playwright/test';
import http from 'node:http';
import {startApp} from './app-fixture.mjs';
import {fillSource} from './editor-text.mjs';

// Rendered notes and answers are untrusted: heading anchors must not replace
// the application's own elements, while in-document anchors keep working.
const filler = Array.from({length: 40}, (_, i) => `Filler paragraph ${i + 1} keeps headings apart.`).join('\n\n');
const note = `# Clobber\n\n[Jump to comment text](#comment-text)\n\n${filler}\n\n## Notice\n\nA heading named like the notice tray.\n\n${filler}\n\n## Search\n\nA heading named like the search field.\n\n${filler}\n\n## Comment text\n\nA heading named like the comment composer.\n\n[Back to notice](#notice)\n`;
const answer = '## Search\n\nAn answer heading named like the search field.\n\n## Notice\n\nAnother answer heading.\n';

function appElements() {
  return {
    notice: document.getElementById('notice')?.parentElement?.id,
    search: document.getElementById('search')?.tagName,
    comment: document.getElementById('comment-text')?.tagName
  };
}

test('note and chat heading anchors cannot take over application elements', async ({page}) => {
  test.setTimeout(120000);
  page.setDefaultTimeout(10000);
  let app;
  const provider = http.createServer(async (request, response) => {
    for await (const chunk of request) {
    }
    response.writeHead(200, {'Content-Type': 'application/json'});
    response.end(
      JSON.stringify({
        choices: [{message: {role: 'assistant', content: answer}, finish_reason: 'stop'}],
        usage: {prompt_tokens: 10, completion_tokens: 10}
      })
    );
  });
  await new Promise(resolve => provider.listen(0, '127.0.0.1', resolve));
  try {
    app = await startApp({
      name: 'forgetmenote-rendered-content-',
      account: {username: 'rendered-content-admin', password: 'DisposableRenderedPassword123!'},
      chatConfig: {
        connections: [
          {
            id: 'local',
            label: 'Synthetic heading model',
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
    await api('/p/default/api/note', {path: 'Clobber.md', content: note, revision: ''});
    await app.login(page, 'password');
    await page.setViewportSize({width: 1440, height: 900});
    await page.goto(origin + '/?project=default&note=Clobber.md#search');
    const preview = page.locator('#preview');
    await expect(preview.locator('h2')).toHaveText(['Notice', 'Search', 'Comment text']);
    expect(await page.evaluate(appElements)).toEqual({notice: 'feedback-tray', search: 'INPUT', comment: 'TEXTAREA'});
    expect(await preview.locator('h2').evaluateAll(nodes => nodes.map(node => node.id))).toEqual([
      'h-notice',
      'h-search',
      'h-comment-text'
    ]);
    await expect(preview.locator('h2').nth(1)).toBeInViewport();
    await preview.getByRole('link', {name: 'Jump to comment text'}).click();
    await expect(page).toHaveURL(/#comment-text$/);
    await expect(preview.locator('h2').nth(2)).toBeInViewport();
    await preview.getByRole('link', {name: 'Back to notice'}).click();
    await expect(page).toHaveURL(/#notice$/);
    await expect(preview.locator('h2').nth(0)).toBeInViewport();

    await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
    await page.locator('#note-context-toggle').click();
    const contents = page.locator('#note-context-contents');
    await expect(contents.getByRole('link', {name: 'Search', exact: true})).toHaveAttribute('href', '#search');
    await contents.getByRole('link', {name: 'Search', exact: true}).click();
    await expect(page).toHaveURL(/#search$/);
    await expect(preview.locator('h2').nth(1)).toBeInViewport();
    await page.locator('[data-context-tab="comments"]').click();
    await page.locator('#comment-text').fill('A comment beside clobbering headings.');
    await page.locator('#comment-form').getByRole('button', {name: 'Post comment', exact: true}).click();
    await expect(page.locator('#comments-list')).toContainText('A comment beside clobbering headings.');
    await expect(preview.locator('h2')).toHaveText(['Notice', 'Search', 'Comment text']);
    await page.keyboard.press('Escape');
    await page.locator('#mode').click();
    // The editor's heading lines carry no ids at all, at the top or the end.
    await expect(page.locator('#editor .cm-lp-h1')).toHaveText(/Clobber$/);
    expect(await page.locator('#editor [id]').count()).toBe(0);
    await page.locator('#editor').press('Control+End');
    await expect(page.locator('#editor .cm-lp-h2').last()).toHaveText(/Comment text$/);
    expect(await page.locator('#editor [id]').count()).toBe(0);
    expect(await page.evaluate(appElements)).toEqual({notice: 'feedback-tray', search: 'INPUT', comment: 'TEXTAREA'});
    await page.locator('#mode').click();

    await page.goto(origin + '/?project=default');
    await page.locator('#new-note').click();
    await page.locator('#new-name').fill('private-clobber');
    await page.locator('#new-container').selectOption('draft');
    await page.locator('#new-submit').click();
    await page.locator('#format-source').click();
    await fillSource(page, '---\nfmn-container: draft\n---\n# Private\n\n## Notice\n\n## Comment text\n');
    await page.locator('#mode').click();
    await expect(preview.locator('h2')).toHaveText(['Notice', 'Comment text']);
    expect(await preview.locator('h2').evaluateAll(nodes => nodes.map(node => node.id))).toEqual([
      'h-notice',
      'h-comment-text'
    ]);
    expect(await page.evaluate(appElements)).toEqual({notice: 'feedback-tray', search: 'INPUT', comment: 'TEXTAREA'});

    await page.goto(origin + '/?project=default&note=Clobber.md#comment-text');
    await expect(preview.locator('h2').nth(2)).toBeInViewport();
    await page.locator('#chat-open').click();
    await expect(page.locator('#chat-model')).toHaveValue('synthetic');
    await page.locator('#chat-input').fill('Answer with headings.');
    await page.locator('#chat-send').click();
    await expect(page.locator('.chat-markdown h2')).toHaveText(['Search', 'Notice'], {timeout: 65000});
    expect(await page.locator('.chat-markdown h2').evaluateAll(nodes => nodes.map(node => node.id))).toEqual([
      'h-search',
      'h-notice'
    ]);
    expect(await page.evaluate(appElements)).toEqual({notice: 'feedback-tray', search: 'INPUT', comment: 'TEXTAREA'});
  } finally {
    try {
      await app?.close();
    } finally {
      provider.closeAllConnections();
      await new Promise(resolve => provider.close(resolve));
    }
  }
});
