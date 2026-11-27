import {test, expect} from '@playwright/test';
import http from 'node:http';
import {readFile} from 'node:fs/promises';
import {startApp} from './app-fixture.mjs';

// The instance's chat reads an image archived in the browser. A synthetic
// provider stands in for the chat model and counts what it is sent.
let app, provider;
const requests = [];
test.beforeAll(async () => {
  provider = http.createServer(async (request, response) => {
    let body = '';
    for await (const chunk of request) body += chunk;
    requests.push(body);
    response.writeHead(200, {'Content-Type': 'application/json'});
    response.end(
      JSON.stringify({
        choices: [
          {
            message: {
              role: 'assistant',
              content: 'HARBOUR NOTICE\nNo whaling within the harbour.\n\nImage description: A printed notice.'
            },
            finish_reason: 'stop'
          }
        ],
        usage: {prompt_tokens: 812, completion_tokens: 20}
      })
    );
  });
  await new Promise(resolve => provider.listen(0, '127.0.0.1', resolve));
  app = await startApp({
    name: 'forgetmenote-source-chat-text-',
    account: {username: 'source-chat-admin', password: 'DisposableSourceChat123!'},
    chatConfig: {
      connections: [
        {
          id: 'vision',
          label: 'Synthetic vision model',
          format: 'chat-completions',
          baseURL: 'http://127.0.0.1:' + provider.address().port,
          models: ['synthetic-vision'],
          local: true,
          capabilities: {images: true},
          limits: {contextTokens: 65536, runSeconds: 60}
        }
      ]
    }
  });
});
test.afterAll(async () => {
  await app?.close();
  provider?.close();
});

test('the chat reads an uploaded image, and the archive dialog says why it does not', async ({page}) => {
  const png = await readFile(new URL('../../internal/sources/testdata/reference.png', import.meta.url));
  await app.login(page, 'password');
  await page.goto(app.origin + '/?project=default');
  await expect(page.locator('#new-note')).toBeEnabled();
  async function archive(name) {
    await page.locator('#create-toggle').click();
    await page.locator('#create-source').click();
    await page.locator('#archive-file').setInputFiles({name, mimeType: 'image/png', buffer: png});
    await page.locator('#archive-preview-button').click();
    await expect(page.locator('#archive-save')).toBeVisible();
    await page.locator('#archive-save').click();
    await expect(page.locator('#archive-saved')).toBeVisible();
  }
  const awaiting = async () =>
    (await app.api('/p/default/api/sources/awaiting-text')).sources.map(source => source.title);

  // AI writing on does not let the chat read uploads: the upload waits for a
  // connected agent and the dialog says why. Nothing reaches the provider.
  await app.api('/api/organization/action/set_ai_settings', {config: {revision: 0, writing: true}});
  await archive('closed-notice.png');
  await expect(page.locator('#archive-chat-status')).toHaveText(
    /^Stored without text\. The instance AI policy does not let the chat read uploaded images/
  );
  expect(requests).toHaveLength(0);
  expect(await awaiting()).toEqual(['closed-notice.png']);

  // An admin turns on the switch of its own in Administration → AI features.
  await page.goto(app.origin + '/?project=default&page=administration&category=ai');
  const uploads = page.locator('#instance-ai-uploadReading');
  await expect(uploads).toBeEnabled();
  await expect(uploads).not.toBeChecked();
  await expect(page.locator('#instance-ai-writing')).toBeChecked();
  await uploads.check();
  await page.getByRole('button', {name: 'Save AI settings', exact: true}).click();
  await expect(page.locator('#instance-ai-status')).toHaveText('AI settings saved for all notebooks.');
  expect((await app.api('/api/organization/action/ai_settings', {})).config.uploadReading).toBe(true);
  await page.goto(app.origin + '/?project=default');
  await expect(page.locator('#new-note')).toBeEnabled();
  await archive('notice.png');
  await expect(page.locator('#archive-chat-status')).toHaveText(/^The chat read this upload/);
  expect(requests).toHaveLength(1);
  expect(requests[0]).toContain('data:image/png;base64,');
  expect(await awaiting()).toEqual(['closed-notice.png']);
  const citation = await page.locator('#archive-result').inputValue();
  const path = decodeURIComponent(citation.match(/\]\(\/([^?]+)\?/)[1]);
  const note = await app.api('/p/default/api/note?path=' + encodeURIComponent(path));
  expect(note.content).toContain('No whaling within the harbour.');
  expect(note.metadata.source.extractor).toBe('ForgetMeNote chat; synthetic-vision');
  expect(citation).toContain('fmn-rev=' + note.revision);
  await page.locator('#archive-view').click();
  await expect(page.locator('#new-dialog')).toBeHidden();
  await expect(page.locator('#archive-details')).toContainText("by the instance's chat model");
});
