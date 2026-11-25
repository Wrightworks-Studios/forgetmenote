import {test, expect} from '@playwright/test';
import http from 'node:http';
import {readFile} from 'node:fs/promises';

const defaults = {
  version: 1,
  search: {version: 1, archives: 'lower', drafts: 'include'},
  ai: {version: 1, archives: 'exclude', drafts: 'exclude'}
};
const excluded = {version: 1, archives: 'exclude', drafts: 'exclude', history: false};
const shell = `<!doctype html><html><head><meta name="viewport" content="width=device-width,initial-scale=1"><link rel="stylesheet" href="/static/style.css"></head><body><main><button id="chat-open">Chat</button></main><aside id="chat-panel" hidden></aside><script type="module">
import {initChat} from '/static/chat.mjs';
const api=async(path,options={})=>{const response=await fetch(path,options);if(!response.ok){const error=Error(await response.text());error.status=response.status;throw error}return response.json()};
const chat=initChat({api,post:(path,body,options)=>api(path,{...options,method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)}),getProject:()=>({id:'default',name:'Synthetic notebook',role:'owner'}),getUser:()=>({id:'user'}),getNote:()=>null,canEdit:()=>true,canManage:()=>true});chat.setProject();
</script></body></html>`;
let server, origin, settings, chats, requests, defaultFailure;
function json(response, value, status = 200) {
  response.writeHead(status, {'Content-Type': 'application/json'});
  response.end(JSON.stringify(value));
}
function conversation(id, corpus = excluded, version = 2) {
  return {
    id: id.repeat(32),
    version,
    title: 'Conversation ' + id,
    options: {connection: 'local', model: 'synthetic', mode: 'ask', ...(corpus === undefined ? {} : {corpus})},
    turns: [],
    runs: [],
    eventCursor: 0
  };
}
test.beforeAll(async () => {
  server = http.createServer(async (request, response) => {
    const path = new URL(request.url, 'http://localhost').pathname;
    if (path === '/') {
      response.setHeader('Content-Type', 'text/html');
      response.end(shell);
      return;
    }
    if (path.startsWith('/static/')) {
      const file = path.slice(8);
      if (!(/^[a-z][a-z-]*\.mjs$/.test(file) || /^(styles\/[a-z-]+|style)\.css$/.test(file))) {
        response.writeHead(404);
        response.end();
        return;
      }
      response.setHeader('Content-Type', file.endsWith('mjs') ? 'text/javascript' : 'text/css');
      response.end(await readFile('internal/web/ui/' + file));
      return;
    }
    if (path === '/api/chat/connections') {
      json(response, {
        enabled: true,
        defaultConnection: 'local',
        connections: [{id: 'local', label: 'Synthetic', models: ['synthetic'], defaultModel: 'synthetic', local: true}]
      });
      return;
    }
    if (path === '/api/projects') {
      json(response, [
        {id: 'default', name: 'Synthetic notebook', kind: 'notes', role: 'owner'},
        {id: 'other', name: 'Other notebook', kind: 'notes', role: 'owner'}
      ]);
      return;
    }
    if (path.endsWith('/retrieval-settings')) {
      json(
        response,
        defaultFailure ? 'Retrieval settings unavailable' : {settings, id: 'defaults-1'},
        defaultFailure ? 503 : 200
      );
      return;
    }
    let body = '';
    for await (const chunk of request) body += chunk;
    body = body ? JSON.parse(body) : undefined;
    if (request.method === 'POST') requests.push({path, body});
    if (path.endsWith('/api/preview')) {
      json(response, {html: '<p>Saved answer</p>'});
      return;
    }
    if (path.endsWith('/api/chat')) {
      if (request.method === 'POST') {
        const chat = conversation(String(chats.size + 1));
        chat.options = body;
        chats.set(chat.id, chat);
        json(response, chat);
      } else json(response, {conversations: [...chats.values()]});
      return;
    }
    const match = /\/api\/chat\/([^/]+)(.*)/.exec(path),
      chat = chats.get(match?.[1]),
      suffix = match?.[2];
    if (!chat) {
      json(response, 'Missing', 404);
      return;
    }
    if (suffix === '/events') {
      response.writeHead(200, {'Content-Type': 'text/event-stream'});
      response.write(': connected\n\n');
      return;
    }
    if (!suffix) {
      json(response, chat);
      return;
    }
    if (suffix === '/turns') {
      chat.options = structuredClone(body.options);
      const run = {id: 'run-' + (chat.runs.length + 1), state: 'completed', options: structuredClone(body.options)};
      chat.runs.push(run);
      chat.turns.push(
        {id: 'user-' + run.id, runId: run.id, role: 'user', content: body.text},
        {id: 'answer-' + run.id, runId: run.id, role: 'assistant', content: 'Saved answer'}
      );
      json(response, {run: run.id});
      return;
    }
    if (suffix.endsWith('/resume')) {
      json(response, {state: 'completed'});
      return;
    }
    json(response, 'Missing', 404);
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  origin = 'http://127.0.0.1:' + server.address().port;
});
test.afterAll(async () => {
  server.closeAllConnections();
  await new Promise(resolve => server.close(resolve));
});
test.beforeEach(async ({page}) => {
  settings = structuredClone(defaults);
  chats = new Map();
  requests = [];
  defaultFailure = false;
  page.on('pageerror', error => {
    throw error;
  });
  await page.goto(origin);
});
async function open(page) {
  await page.locator('#chat-open').click();
  await expect(page.locator('#chat-model')).toHaveValue('synthetic');
}
async function gear(page) {
  await page.getByRole('button', {name: 'Chat settings', exact: true}).click();
}
async function closeGear(page) {
  await page.locator('#chat-settings').getByRole('button', {name: 'Close', exact: true}).click();
}
async function send(page, text) {
  await page.locator('#chat-input').fill(text);
  await page.locator('#chat-send').click();
  await expect(page.locator('#chat-input')).toHaveValue('');
}

test('chat gear defaults exclude Archives and Drafts and sends explicit current-only scope', async ({page}) => {
  await open(page);
  await gear(page);
  await expect(page.getByRole('combobox', {name: 'Archives', exact: true})).toHaveValue('exclude');
  await expect(page.getByRole('combobox', {name: 'Drafts', exact: true})).toHaveValue('exclude');
  await expect(page.getByLabel('Include previous versions')).not.toBeChecked();
  await expect(page.locator('#chat-settings')).toContainText('Continue keeps its original settings.');
  await closeGear(page);
  await expect(page.locator('#chat-settings-open')).toBeFocused();
  await send(page, 'A fresh question');
  expect(requests.find(request => request.path.endsWith('/api/chat')).body.corpus).toEqual(excluded);
  expect(requests.find(request => request.path.endsWith('/turns')).body.options.corpus).toEqual(excluded);
  await expect(page.locator('#chat-turns')).not.toContainText('Archives');
});

test('conversation scope survives reload and notebook default changes while new chat uses the defaults', async ({
  page
}) => {
  const scope = {version: 1, archives: 'lower', drafts: 'include', history: true},
    chat = conversation('a', scope);
  chats.set(chat.id, chat);
  await open(page);
  await gear(page);
  await expect(page.getByRole('combobox', {name: 'Archives', exact: true})).toHaveValue('lower');
  await expect(page.getByLabel('Include previous versions')).toBeChecked();
  await closeGear(page);
  settings.ai = {version: 1, archives: 'include', drafts: 'lower'};
  await page.reload();
  await expect(page.locator('#chat-panel')).toBeVisible();
  await gear(page);
  await expect(page.getByRole('combobox', {name: 'Archives', exact: true})).toHaveValue('lower');
  await closeGear(page);
  await send(page, 'Same scope');
  expect(requests.find(request => request.path.endsWith('/turns')).body.options.corpus).toEqual(scope);
  await page.locator('#chat-new').click();
  await gear(page);
  await expect(page.getByRole('combobox', {name: 'Archives', exact: true})).toHaveValue('include');
  await expect(page.getByRole('combobox', {name: 'Drafts', exact: true})).toHaveValue('lower');
  await expect(page.getByLabel('Include previous versions')).not.toBeChecked();
});

test('scope edits survive conversation switching and Continue retains the saved run scope', async ({page}) => {
  const first = conversation('a'),
    second = conversation('b', {version: 1, archives: 'include', drafts: 'lower', history: true});
  first.runs = [{id: 'stopped', state: 'cancelled', options: {...first.options}}];
  chats.set(first.id, first);
  chats.set(second.id, second);
  await open(page);
  await gear(page);
  await page.getByRole('combobox', {name: 'Archives', exact: true}).selectOption('lower');
  await page.getByLabel('Include previous versions').check();
  await closeGear(page);
  await page.locator('#chat-conversations').selectOption(second.id);
  await gear(page);
  await expect(page.getByRole('combobox', {name: 'Archives', exact: true})).toHaveValue('include');
  await closeGear(page);
  await page.locator('#chat-conversations').selectOption(first.id);
  await gear(page);
  await expect(page.getByRole('combobox', {name: 'Archives', exact: true})).toHaveValue('lower');
  await expect(page.getByLabel('Include previous versions')).toBeChecked();
  await closeGear(page);
  await page.locator('#chat-continue').click();
  await expect.poll(() => requests.some(request => request.path.endsWith('/resume'))).toBe(true);
  expect(requests.find(request => request.path.endsWith('/resume')).body).toEqual({});
  expect(first.runs[0].options.corpus).toEqual(excluded);
  await send(page, 'New scope');
  expect(requests.find(request => request.path.endsWith('/turns')).body.options.corpus).toEqual({
    ...excluded,
    archives: 'lower',
    history: true
  });
});

for (const corpus of [
  {version: 2, archives: 'exclude', drafts: 'exclude', history: false},
  {version: 1, archives: 'exclude', drafts: 'exclude'},
  null
])
  test('unproven saved scope requires a deliberate selection ' + JSON.stringify(corpus), async ({page}) => {
    const chat = conversation('a', corpus);
    chat.turns = [{id: 'old', role: 'user', content: 'Original transcript stays readable'}];
    chat.runs = [{id: 'old-run', state: 'interrupted', options: {corpus}}];
    chats.set(chat.id, chat);
    await open(page);
    await expect(page.locator('#chat-turns')).toContainText('Original transcript stays readable');
    await expect(page.locator('#chat-send')).toBeDisabled();
    await expect(page.locator('#chat-continue')).toHaveCount(0);
    await gear(page);
    await expect(page.getByRole('combobox', {name: 'Archives', exact: true})).toHaveValue('');
    await page.getByRole('combobox', {name: 'Archives', exact: true}).selectOption('exclude');
    await page.getByRole('combobox', {name: 'Drafts', exact: true}).selectOption('exclude');
    await closeGear(page);
    await send(page, 'Deliberately use current notes');
    expect(requests.find(request => request.path.endsWith('/turns')).body.options.corpus).toEqual(excluded);
  });

test('legacy transcript takes current defaults for a new turn and never offers old continuation', async ({page}) => {
  const chat = conversation('a', null, 1);
  delete chat.options.corpus;
  chat.runs = [{id: 'legacy', state: 'interrupted', options: {}}];
  chats.set(chat.id, chat);
  settings.ai = {version: 1, archives: 'lower', drafts: 'exclude'};
  await open(page);
  await expect(page.locator('#chat-continue')).toHaveCount(0);
  await send(page, 'Fresh legacy turn');
  expect(requests.find(request => request.path.endsWith('/turns')).body.options.corpus).toEqual({
    ...excluded,
    archives: 'lower'
  });
});

test('unavailable notebook defaults cannot create an implicit scope but saved valid conversations remain usable', async ({
  page
}) => {
  defaultFailure = true;
  await open(page);
  await expect(page.locator('#chat-send')).toBeDisabled();
  const chat = conversation('a');
  chats.set(chat.id, chat);
  await page.locator('#chat-refresh').click();
  await expect(page.locator('#chat-send')).toBeEnabled();
  await send(page, 'Use the saved selection');
  expect(requests.find(request => request.path.endsWith('/turns')).body.options.corpus).toEqual(excluded);
});

test('historical review explains its limited coverage only inside Activity details', async ({page}) => {
  const chat = conversation('a', {...excluded, history: true});
  chat.runs = [
    {id: 'review', state: 'completed', options: chat.options, review: {notes: [], complete: true, historyLimited: true}}
  ];
  chats.set(chat.id, chat);
  await open(page);
  await expect(
    page.getByText('Historical comparisons are limited; completion covers eligible current notes only.', {exact: true})
  ).toBeHidden();
  await page.locator('.chat-activity > summary').click();
  await page.getByText('Review coverage', {exact: true}).click();
  await expect(
    page.getByText('Historical comparisons are limited; completion covers eligible current notes only.', {exact: true})
  ).toBeVisible();
});

test('New chat reads and submits current defaults without a page reload', async ({page}) => {
  const first = conversation('a');
  chats.set(first.id, first);
  await open(page);
  await gear(page);
  await expect(page.locator('#chat-archives')).toHaveValue('exclude');
  await closeGear(page);
  settings.ai = {version: 1, archives: 'include', drafts: 'lower'};
  await page.locator('#chat-new').click();
  await gear(page);
  await expect(page.locator('#chat-archives')).toHaveValue('include');
  await expect(page.locator('#chat-drafts')).toHaveValue('lower');
  await expect(page.locator('#chat-history')).not.toBeChecked();
  await closeGear(page);
  await send(page, 'Use the current notebook defaults');
  const scope = {...settings.ai, history: false};
  expect(requests.find(request => request.path.endsWith('/api/chat')).body.corpus).toEqual(scope);
  expect(requests.find(request => request.path.endsWith('/turns')).body.options.corpus).toEqual(scope);
});

test('New chat blocks fast sends until fresh defaults arrive', async ({page}) => {
  await open(page);
  settings.ai = {version: 1, archives: 'lower', drafts: 'include'};
  let release;
  const held = new Promise(resolve => (release = resolve));
  let requested = false;
  await page.route('**/retrieval-settings', async route => {
    requested = true;
    await held;
    await route.continue();
  });
  await page.locator('#chat-new').click();
  await expect.poll(() => requested).toBe(true);
  await page.locator('#chat-input').fill('Wait for the current scope');
  await page.locator('#chat-input').press('Control+Enter');
  await expect(page.locator('#chat-send')).toBeDisabled();
  expect(requests).toEqual([]);
  await gear(page);
  await expect(page.locator('#chat-settings')).toContainText('Loading notebook defaults');
  await expect(page.locator('#chat-archives')).toHaveValue('');
  await expect(page.locator('#chat-archives')).toBeDisabled();
  await closeGear(page);
  release();
  await expect(page.locator('#chat-send')).toBeEnabled();
  await page.locator('#chat-send').click();
  await expect(page.locator('#chat-input')).toHaveValue('');
  expect(requests.find(request => request.path.endsWith('/turns')).body.options.corpus).toEqual({
    ...settings.ai,
    history: false
  });
});

test('New chat clears stale defaults on failure and recovers through Refresh', async ({page}) => {
  await open(page);
  defaultFailure = true;
  await page.locator('#chat-new').click();
  await gear(page);
  await expect(page.locator('#chat-settings')).toContainText('Retrieval settings unavailable');
  await expect(page.locator('#chat-archives')).toHaveValue('');
  await expect(page.locator('#chat-drafts')).toHaveValue('');
  await expect(page.locator('#chat-send')).toBeDisabled();
  await closeGear(page);
  await page.locator('#chat-input').fill('Keep this message while defaults are unavailable');
  await page.locator('#chat-input').press('Control+Enter');
  expect(requests).toEqual([]);
  defaultFailure = false;
  settings.ai = {version: 1, archives: 'exclude', drafts: 'lower'};
  await page.locator('#chat-refresh').click();
  await expect(page.locator('#chat-send')).toBeEnabled();
  await page.locator('#chat-send').click();
  await expect(page.locator('#chat-input')).toHaveValue('');
  expect(requests.find(request => request.path.endsWith('/turns')).body.options.corpus).toEqual({
    ...settings.ai,
    history: false
  });
});

test('New chat preserves deliberate unsent scopes when defaults change', async ({page}) => {
  const first = conversation('a');
  chats.set(first.id, first);
  await open(page);
  await page.locator('#chat-new').click();
  await gear(page);
  await page.locator('#chat-archives').selectOption('lower');
  await page.locator('#chat-drafts').selectOption('include');
  await page.locator('#chat-history').check();
  await closeGear(page);
  await page.locator('#chat-input').fill('Deliberately selected scope');
  await page.locator('#chat-conversations').selectOption(first.id);
  settings.ai = {version: 1, archives: 'include', drafts: 'exclude'};
  await page.locator('#chat-new').click();
  await gear(page);
  await expect(page.locator('#chat-archives')).toHaveValue('lower');
  await expect(page.locator('#chat-drafts')).toHaveValue('include');
  await expect(page.locator('#chat-history')).toBeChecked();
  await closeGear(page);
  await expect(page.locator('#chat-input')).toHaveValue('Deliberately selected scope');
  await page.locator('#chat-send').click();
  await expect(page.locator('#chat-input')).toHaveValue('');
  expect(requests.find(request => request.path.endsWith('/turns')).body.options.corpus).toEqual({
    version: 1,
    archives: 'lower',
    drafts: 'include',
    history: true
  });
});

test('New chat preserves pending creation options while refreshing defaults', async ({page}) => {
  const first = conversation('a');
  chats.set(first.id, first);
  await open(page);
  await page.locator('#chat-new').click();
  const creations = [];
  await page.route('**/api/chat', async route => {
    if (route.request().method() !== 'POST') {
      await route.continue();
      return;
    }
    creations.push(route.request().postDataJSON());
    if (creations.length === 1) await route.fulfill({status: 502, body: 'Synthetic lost creation acknowledgement'});
    else await route.continue();
  });
  await page.locator('#chat-input').fill('Retry my original selection');
  await page.locator('#chat-send').click();
  await expect(page.locator('#chat-send')).toHaveText('Retry send');
  await page.locator('#chat-conversations').selectOption(first.id);
  settings.ai = {version: 1, archives: 'include', drafts: 'lower'};
  await page.locator('#chat-new').click();
  await gear(page);
  await expect(page.locator('#chat-archives')).toHaveValue('exclude');
  await expect(page.locator('#chat-drafts')).toHaveValue('exclude');
  await expect(page.locator('#chat-archives')).toBeDisabled();
  await closeGear(page);
  await page.locator('#chat-send').click();
  await expect(page.locator('#chat-input')).toHaveValue('');
  expect(creations).toHaveLength(2);
  expect(creations[1]).toEqual(creations[0]);
  const turn = requests.find(request => request.path.endsWith('/turns')).body;
  expect(turn.text).toBe('Retry my original selection');
  expect(turn.options.corpus).toEqual(excluded);
});

test('New chat defaults timeout permits explicit scope selection without a stale fallback', async ({page}) => {
  await open(page);
  let release;
  const held = new Promise(resolve => (release = resolve));
  await page.route('**/retrieval-settings', async route => {
    await held;
    await route.abort().catch(() => {});
  });
  await page.locator('#chat-new').click();
  await gear(page);
  await expect(page.locator('#chat-settings')).toContainText('Loading notebook defaults timed out.', {timeout: 15000});
  await expect(page.locator('#chat-send')).toBeDisabled();
  await expect(page.locator('#chat-archives')).toHaveValue('');
  await expect(page.locator('#chat-archives')).toBeEnabled();
  release();
  await page.locator('#chat-archives').selectOption('exclude');
  await page.locator('#chat-drafts').selectOption('lower');
  await closeGear(page);
  await send(page, 'Use my explicit scope after the timeout');
  expect(requests.find(request => request.path.endsWith('/turns')).body.options.corpus).toEqual({
    ...excluded,
    drafts: 'lower'
  });
});

test('New chat and default refresh preserve the exact pending turn retry body', async ({page}) => {
  const first = conversation('a');
  chats.set(first.id, first);
  await open(page);
  const attempts = [];
  await page.route('**/turns', async route => {
    attempts.push(route.request().postDataJSON());
    if (attempts.length === 1) await route.fulfill({status: 502, body: 'Synthetic lost turn acknowledgement'});
    else await route.continue();
  });
  await page.locator('#chat-input').fill('Retry the exact pending turn');
  await page.locator('#chat-send').click();
  await expect(page.locator('#chat-send')).toHaveText('Retry send');
  settings.ai = {version: 1, archives: 'include', drafts: 'lower'};
  await page.locator('#chat-new').click();
  await expect(page.locator('#chat-send')).toBeEnabled();
  await page.locator('#chat-conversations').selectOption(first.id);
  defaultFailure = true;
  await page.locator('#chat-refresh').click();
  await expect(page.locator('#chat-send')).toHaveText('Retry send');
  await gear(page);
  await expect(page.locator('#chat-archives')).toHaveValue('exclude');
  await expect(page.locator('#chat-drafts')).toHaveValue('exclude');
  await expect(page.locator('#chat-archives')).toBeDisabled();
  await closeGear(page);
  await page.locator('#chat-send').click();
  await expect(page.locator('#chat-input')).toHaveValue('');
  expect(attempts).toHaveLength(2);
  expect(attempts[1]).toEqual(attempts[0]);
  expect(attempts[0].submissionId).toBeTruthy();
});
