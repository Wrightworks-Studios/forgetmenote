import {test, expect} from '@playwright/test';
import http from 'node:http';
import {readFile} from 'node:fs/promises';
import {startApp} from './app-fixture.mjs';
import {expectEditorText, fillSource} from './editor-text.mjs';

let server,
  origin,
  conversations,
  requests,
  streams,
  sequence,
  denied,
  configured,
  loseAck,
  previewCalls,
  publicConnections;
const revision = 'a'.repeat(40),
  connection = {
    id: 'local',
    label: 'Synthetic local',
    models: ['small', 'large'],
    defaultModel: 'small',
    local: true,
    verified: false,
    capabilities: {tools: true},
    limits: {contextTokens: 16384, outputTokens: 2048}
  };
const shell = `<!doctype html><html><head><meta name="viewport" content="width=device-width,initial-scale=1"><link rel="stylesheet" href="/static/style.css"></head><body><main id="main"><button id="chat-open" aria-expanded="false" aria-controls="chat-panel">Chat</button><textarea id="editor">PRIVATE TEXT NEVER IMPLICITLY ATTACHED</textarea><button id="switch-project">Switch notebook</button><button id="switch-account">Switch account</button><p id="citation-preview"></p></main><aside id="chat-panel" aria-labelledby="chat-title" hidden></aside><script type="module">
import {initChat} from '/static/chat.mjs';
let project={id:'default',name:'Synthetic notebook',role:'owner'},account={id:'synthetic-user'};
const api=async(path,options={})=>{const r=await fetch(path,options);if(!r.ok){const error=new Error(await r.text());error.status=r.status;throw error}return r.json()};
const post=(path,body,options={})=>api(path,{...options,method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)});
window.draftReads=0;window.citations=[];window.documentTargets=[];
const chat=initChat({api,post,getProject:()=>project,getUser:()=>account,getNote:()=>({path:'note.md',revision:'${revision}',liveRevision:'live:1'}),getDraftAttachment:()=>{window.draftReads++;return {path:'note.md',revision:'${revision}',content:document.getElementById('editor').value}},canEdit:()=>true,canManage:()=>true,openDocument:ref=>window.documentTargets.push(ref),openCitation:(...args)=>{window.citations.push(args);document.getElementById('citation-preview').textContent=args.join(' ')},openReview:()=>{window.reviewOpened=true},openSource:url=>{window.sourceOpened=url}});chat.setProject();window.chat=chat;
document.getElementById('switch-account').onclick=()=>{account={id:'other-user'};chat.setProject()};
document.getElementById('switch-project').onclick=()=>{project={id:'other',name:'Other notebook',role:'owner'};chat.setProject()};
</script></body></html>`;
function json(response, value, status = 200) {
  response.writeHead(status, {'Content-Type': 'application/json'});
  response.end(JSON.stringify(value));
}
function emit(chat, run, type, data) {
  const event = {sequence: ++sequence, run, type, data};
  chat.eventCursor = sequence;
  chat.events.push(event);
  for (const response of streams.get(chat.id) || [])
    response.write(`event: chat\nid: ${event.sequence}\ndata: ${JSON.stringify(event)}\n\n`);
}
function newConversation(id = '1'.repeat(32)) {
  return {
    id,
    version: 2,
    title: 'Synthetic conversation',
    project: 'default',
    options: {
      connection: 'local',
      model: 'small',
      mode: 'ask',
      corpus: {version: 1, archives: 'exclude', drafts: 'exclude', history: false}
    },
    turns: [],
    runs: [],
    eventCursor: 0,
    events: []
  };
}
function complete(chat, run, text = 'Saved answer with <script>unsafe()</script> text') {
  const turn = {
    id: 'answer-' + run.id,
    runId: run.id,
    role: 'assistant',
    content: text,
    citations: [{id: 'citation-1', path: 'note.md', revision, title: 'Pinned note', url: 'note.md?fmn-rev=' + revision}]
  };
  chat.turns.push(turn);
  emit(chat, run.id, 'assistant', turn);
  run.state = 'completed';
  run.usage = {inputTokens: 18, outputTokens: 8, estimated: true};
  emit(chat, run.id, 'state', {state: run.state, usage: run.usage});
}
test.beforeAll(async () => {
  server = http.createServer(async (request, response) => {
    const url = new URL(request.url, 'http://localhost');
    if (url.pathname === '/') {
      response.writeHead(200, {'Content-Type': 'text/html'});
      response.end(shell);
      return;
    }
    if (url.pathname.startsWith('/static/')) {
      try {
        const file = url.pathname.slice(8);
        if (!/^[a-z][a-z-]*\.mjs$/.test(file) && !/^(styles\/[a-z-]+|style)\.css$/.test(file)) throw Error();
        response.setHeader('Content-Type', file.endsWith('mjs') ? 'text/javascript' : 'text/css');
        response.end(await readFile('internal/web/ui/' + file));
      } catch {
        response.writeHead(404);
        response.end();
      }
      return;
    }
    if (denied) {
      json(response, 'Notebook access revoked', 403);
      return;
    }
    if (url.pathname === '/api/projects') {
      json(response, [
        {id: 'default', name: 'Synthetic notebook', role: 'owner'},
        {id: 'other', name: 'Other notebook', role: 'owner'}
      ]);
      return;
    }
    if (url.pathname === '/api/chat/connections') {
      json(response, {
        enabled: configured,
        connections: configured ? publicConnections : [],
        defaultConnection: publicConnections[0]?.id,
        research: true,
        actions: [{name: 'empty_trash', description: 'Empty the exact Trash manifest', read: false, control: true}]
      });
      return;
    }
    if (url.pathname.endsWith('/api/knowledge/retrieval-settings')) {
      json(response, {
        settings: {
          version: 1,
          search: {version: 1, archives: 'lower', drafts: 'include'},
          ai: {version: 1, archives: 'exclude', drafts: 'exclude'}
        },
        id: 'synthetic-defaults'
      });
      return;
    }
    let body = '';
    for await (const chunk of request) body += chunk;
    body = body ? JSON.parse(body) : undefined;
    if (request.method === 'POST') requests.push({path: url.pathname, body});
    if (url.pathname.endsWith('/api/preview')) {
      previewCalls++;
      json(response, {
        html: '<p>' + body.content.replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('>', '&gt;') + '</p>'
      });
      return;
    }
    const match = /^\/p\/([^/]+)\/api\/chat(?:\/([^/]+))?(.*)$/.exec(url.pathname);
    if (!match) {
      json(response, 'Not found', 404);
      return;
    }
    const [, project, id, suffix] = match;
    if (!id) {
      if (request.method === 'POST') {
        const chat = newConversation((conversations.size + 1).toString(16).padStart(32, '0'));
        chat.options = body;
        conversations.set(chat.id, chat);
        json(response, chat);
      } else json(response, {conversations: [...conversations.values()].filter(chat => chat.project === project)});
      return;
    }
    const chat = conversations.get(id);
    if (!chat || chat.project !== project) {
      json(response, 'Unavailable', 404);
      return;
    }
    if (suffix === '/events') {
      response.writeHead(200, {'Content-Type': 'text/event-stream', 'Cache-Control': 'no-cache'});
      response.write(': connected\n\n');
      for (const event of chat.events.filter(event => event.sequence > Number(url.searchParams.get('after') || 0)))
        response.write(`event: chat\nid: ${event.sequence}\ndata: ${JSON.stringify(event)}\n\n`);
      if (!streams.has(id)) streams.set(id, new Set());
      streams.get(id).add(response);
      request.on('close', () => streams.get(id)?.delete(response));
      return;
    }
    if (!suffix && request.method === 'DELETE') {
      if (chat.runs.some(run => ['queued', 'running', 'awaiting_confirmation'].includes(run.state))) {
        json(response, 'Stop the conversation before deleting it', 409);
        return;
      }
      conversations.delete(id);
      for (const response of streams.get(id) || []) response.end();
      json(response, {deleted: true});
      return;
    }
    if (!suffix) {
      const before = Number(url.searchParams.get('before') || chat.turns.length),
        start = Math.max(0, before - 100);
      json(response, {
        ...chat,
        events: undefined,
        turns: chat.turns.slice(start, before),
        totalTurns: chat.turns.length,
        nextBefore: start
      });
      return;
    }
    if (suffix === '/turns') {
      const previous = chat.runs.find(run => run.submissionId === body.submissionId);
      if (previous) {
        json(response, {chat: id, run: previous.id, state: previous.state, replayed: true});
        return;
      }
      const run = {
        id: 'run-' + (chat.runs.length + 1),
        submissionId: body.submissionId,
        state: 'running',
        options: body.options,
        operations: []
      };
      chat.runs.push(run);
      chat.options = body.options;
      chat.turns.push({
        id: 'user-' + run.id,
        runId: run.id,
        role: 'user',
        content: body.text,
        attachments: body.attachments
      });
      if (body.text.includes('approval')) {
        run.state = 'awaiting_confirmation';
        run.operations = [
          {
            id: 'prepared-action',
            name: 'empty_trash',
            state: 'awaiting_confirmation',
            arguments: {revision: 'manifest-revision', confirmation: 'trash-manifest-token'},
            confirmation: 'server-opaque-token',
            result: {count: 3}
          }
        ];
      } else if (body.text.includes('stream')) {
        setTimeout(() => {
          const text = 'Streaming answer';
          const turn = {id: 'answer-' + run.id, runId: run.id, role: 'assistant', content: text, streaming: true};
          chat.turns.push(turn);
          emit(chat, run.id, 'text_delta', {turnId: turn.id, text});
        }, 80);
      } else complete(chat, run);
      if (loseAck) {
        loseAck = false;
        json(response, 'Proxy lost the acknowledgement', 502);
        return;
      }
      json(response, {chat: id, run: run.id, state: run.state});
      return;
    }
    if (suffix.endsWith('/cancel')) {
      const run = chat.runs.find(run => suffix === '/runs/' + run.id + '/cancel');
      run.state = 'cancelled';
      emit(chat, run.id, 'state', {state: run.state});
      json(response, {run: run.id, cancelRequested: true});
      return;
    }
    if (suffix.endsWith('/resume')) {
      const run = chat.runs.find(run => suffix === '/runs/' + run.id + '/resume');
      run.state = 'queued';
      json(response, {run: run.id, state: run.state});
      setTimeout(() => complete(chat, run, 'Continued answer'), 80);
      return;
    }
    if (suffix.startsWith('/actions/')) {
      json(response, 'Trash changed; confirmation is stale', 409);
      return;
    }
    if (suffix === '/save-note') {
      json(response, {path: body.path, revision: 'b'.repeat(40), replayed: false});
      return;
    }
    json(response, 'Not found', 404);
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  origin = 'http://127.0.0.1:' + server.address().port;
});
test.afterAll(async () => {
  for (const list of streams.values()) for (const response of list) response.end();
  await new Promise(resolve => server.close(resolve));
});
test.beforeEach(async ({page}) => {
  conversations = new Map();
  requests = [];
  streams = new Map();
  sequence = 0;
  denied = false;
  configured = true;
  loseAck = false;
  previewCalls = 0;
  publicConnections = [structuredClone(connection)];
  page.on('pageerror', error => {
    throw error;
  });
  await page.goto(origin);
});
async function open(page) {
  if (!(await page.locator('#chat-panel').isVisible())) await page.locator('#chat-open').click();
  await expect(page.locator('#chat-model')).toHaveValue('small');
}
async function settlePanel(page) {
  await page.locator('#chat-panel').evaluate(async panel => {
    await Promise.all(panel.getAnimations().map(animation => animation.finished));
  });
}
async function send(page, text) {
  await page.locator('#chat-input').fill(text);
  await page.locator('#chat-send').click();
}

test('unchanged transcript refresh preserves focused message controls without moving rows', async ({page}) => {
  const chat = newConversation();
  chat.turns = [
    {id: 'question', role: 'user', content: 'A saved question'},
    {id: 'answer', role: 'assistant', content: 'A saved answer'}
  ];
  conversations.set(chat.id, chat);
  await open(page);
  await expect(page.locator('.chat-markdown')).toContainText('A saved answer');
  const control = page.locator('[data-turn="question"]').getByRole('button', {name: 'Message actions', exact: true});
  await control.focus();
  await page.evaluate(async () => {
    window.transcriptMoves = 0;
    const observer = new MutationObserver(records => {
      window.transcriptMoves += records.length;
    });
    observer.observe(document.getElementById('chat-turns'), {childList: true});
    await window.chat.refresh();
    await new Promise(resolve => queueMicrotask(resolve));
    observer.disconnect();
  });
  await expect(control).toBeFocused();
  expect(await page.evaluate(() => window.transcriptMoves)).toBe(0);
  expect(previewCalls).toBe(1);
});

test('a superseded configuration response cannot disable the current chat or discard its draft', async ({page}) => {
  const chat = newConversation();
  conversations.set(chat.id, chat);
  await open(page);
  await page.locator('#chat-input').fill('Keep this private draft');
  let release, arrived;
  const waiting = new Promise(resolve => {
      release = resolve;
    }),
    started = new Promise(resolve => {
      arrived = resolve;
    });
  let calls = 0;
  await page.route('**/api/chat/connections', async route => {
    if (++calls !== 1) return route.continue();
    arrived();
    await waiting;
    await route.fulfill({json: {enabled: false, connections: []}});
  });
  try {
    await page.evaluate(() => {
      window.olderChatRefresh = window.chat.refresh();
    });
    await started;
    await page.evaluate(() => window.chat.refresh());
    release();
    await page.evaluate(() => window.olderChatRefresh);
    await expect(page.locator('#chat-input')).toBeEnabled();
    await expect(page.locator('#chat-input')).toHaveValue('Keep this private draft');
    await expect(page.locator('#chat-model')).toHaveValue('small');
  } finally {
    release();
  }
});

test('chat source notebooks are an explicit saved selection', async ({page}) => {
  await open(page);
  await page.getByRole('button', {name: 'Chat settings', exact: true}).click();
  await page.getByRole('listbox', {name: 'Repository notebooks for chat'}).selectOption(['default', 'other']);
  await page.locator('#chat-settings').getByRole('button', {name: 'Close', exact: true}).click();
  await send(page, 'Compare selected source evidence');
  await expect(page.locator('#chat-input')).toHaveValue('');
  const submitted = requests.filter(request => request.path.endsWith('/turns')).at(-1);
  expect(submitted.body.options.corpus.sourceProjects).toEqual(['default', 'other']);
  await page.getByRole('button', {name: 'Chat settings', exact: true}).click();
  await expect(page.getByRole('listbox', {name: 'Repository notebooks for chat'})).toHaveValues(['default', 'other']);
});

test('desktop chat sends no editor text without explicit attachment and retains a lost acknowledgement', async ({
  page
}) => {
  await open(page);
  loseAck = true;
  await send(page, 'ordinary question');
  await expect(page.locator('#chat-send')).toHaveText('Retry send');
  await expect(page.locator('#chat-input')).toHaveValue('ordinary question');
  expect(await page.evaluate(() => window.draftReads)).toBe(0);
  const first = requests.find(request => request.path.endsWith('/turns'));
  expect(first.body.options).not.toHaveProperty('budgetTokens');
  expect(first.body.attachments).toEqual([]);
  expect(JSON.stringify(first)).not.toContain('PRIVATE TEXT');
  await page.locator('#chat-send').click();
  await expect(page.locator('#chat-input')).toHaveValue('');
  const sends = requests.filter(request => request.path.endsWith('/turns'));
  expect(sends).toHaveLength(2);
  expect(sends[1].body).toEqual(sends[0].body);
  await expect(page.locator('#chat-turns')).toContainText('Saved answer');
  await expect(page.locator('#chat-turns script')).toHaveCount(0);
  await page.locator('#chat-attach').click();
  await page.locator('[data-attachment-kind="private"]').click();
  expect(await page.evaluate(() => window.draftReads)).toBe(1);
  await send(page, 'with deliberate attachment');
  await expect(page.locator('#chat-input')).toHaveValue('');
  expect(requests.filter(request => request.path.endsWith('/turns')).at(-1).body.attachments).toEqual([
    {kind: 'private', path: 'note.md', revision, content: 'PRIVATE TEXT NEVER IMPLICITLY ATTACHED'}
  ]);
  await page.locator('.chat-sources > summary').first().click();
  await page.locator('.chat-citation').first().click();
  await expect(page.locator('#editor')).toHaveValue('PRIVATE TEXT NEVER IMPLICITLY ATTACHED');
  expect(await page.evaluate(() => window.citations[0])).toEqual(['note.md', revision, '']);
  expect(await page.evaluate(() => Object.keys(localStorage))).toEqual([]);
});
test('reopening after reload restores the latest saved conversation and pages complete history', async ({page}) => {
  const chat = newConversation();
  for (let i = 0; i < 102; i++)
    chat.turns.push({id: 'old-' + i, role: i % 2 ? 'assistant' : 'user', content: 'Older message ' + i});
  conversations.set(chat.id, chat);
  await open(page);
  await expect(page.locator('#chat-turns')).toContainText('Older message 101');
  await expect(page.locator('#chat-older')).toBeVisible();
  await page.locator('#chat-older').click();
  await expect(page.locator('#chat-turns')).toContainText('Older message 0');
  await page.reload();
  await open(page);
  await expect(page.locator('#chat-turns')).toContainText('Older message 101');
});
test('stream reconnect never resubmits, Stop and Continue are explicit, and usage stays in the saved record', async ({
  page
}) => {
  await open(page);
  await send(page, 'stream please');
  await expect(page.locator('#chat-turns')).toContainText('Streaming answer');
  for (const list of streams.values()) for (const response of list) response.end();
  await expect(page.locator('#chat-message')).toContainText('Connection interrupted');
  expect(requests.filter(request => request.path.endsWith('/turns'))).toHaveLength(1);
  await page.locator('#chat-stop').click();
  await expect(page.locator('#chat-continue')).toBeVisible();
  await page.locator('#chat-continue').click();
  await expect(page.locator('#chat-turns')).toContainText('Continued answer');
  expect(requests.find(request => request.path.endsWith('/resume')).body).toEqual({});
  expect([...conversations.values()][0].runs[0].usage.estimated).toBe(true);
  await expect(page.locator('#chat-panel')).not.toContainText('output tokens');
});
test('exact destructive action confirmation and selected transcript export remain reviewable', async ({page}) => {
  await open(page);
  await send(page, 'approval please');
  const card = page.locator('[data-action="prepared-action"]');
  await expect(card).toContainText('manifest-revision');
  await expect(card).toContainText('3 items');
  await expect(card).toContainText('Git history and backups remain');
  expect(requests.filter(request => request.path.includes('/actions/'))).toHaveLength(0);
  await card.getByRole('button', {name: 'Approve action'}).click();
  await expect(page.locator('#chat-error')).toContainText('confirmation is stale');
  expect(requests.find(request => request.path.includes('/actions/')).body).toEqual({
    decision: 'approve',
    confirmation: 'server-opaque-token'
  });
  expect(requests.filter(request => request.path.includes('/actions/'))).toHaveLength(1);
  await page.locator('.chat-turn-user').getByRole('button', {name: 'Message actions', exact: true}).click();
  await page.getByRole('button', {name: 'Save to note', exact: true}).click();
  await expect(page.locator('.chat-save-turns input').first()).toBeChecked();
  await expect(page.locator('#chat-save-preview')).toContainText('approval please');
  await page.locator('#chat-save-path').fill('Chat export.md');
  await page.getByRole('button', {name: 'Save selected messages'}).click();
  await expect(page.locator('#chat-message')).toContainText('Saved chat to Chat export.md');
  const exported = requests.find(request => request.path.endsWith('/save-note')).body;
  expect(exported.path).toBe('Chat export.md');
  expect(exported.revision).toBe('');
  expect(exported.turnIds).toEqual(['user-run-1']);
  expect(exported.submissionId).toBeTruthy();
});
test('revocation clears transcript, composer, attachments, and conversation titles', async ({page}) => {
  await open(page);
  await send(page, 'sensitive conversation');
  await expect(page.locator('#chat-turns')).toContainText('Saved answer');
  await page.locator('#chat-input').fill('Unsent secret');
  await page.locator('#chat-attach').click();
  await page.locator('[data-attachment-kind="private"]').click();
  denied = true;
  await page.locator('#chat-refresh').click();
  await expect(page.locator('#chat-error')).toContainText('revoked');
  await expect(page.locator('#chat-turns')).toBeEmpty();
  await expect(page.locator('#chat-input')).toHaveValue('');
  await expect(page.locator('#chat-attachments')).toBeEmpty();
  await expect(page.locator('#chat-conversations')).toBeEmpty();
});
test('missing configuration is honest and notebook changes clear scoped chat state', async ({page}) => {
  configured = false;
  await page.locator('#chat-open').click();
  await expect(page.locator('#chat-message')).toContainText('not configured');
  await expect(page.locator('#chat-send')).toBeDisabled();
  configured = true;
  await page.locator('#chat-refresh').click();
  await send(page, 'original notebook');
  await expect(page.locator('#chat-turns')).toContainText('Saved answer');
  await page.locator('#chat-open').click();
  await page.locator('#switch-project').click();
  await open(page);
  await expect(page.locator('#chat-scope')).toContainText('Other notebook');
  await expect(page.locator('#chat-turns')).toBeEmpty();
});
test('mobile chat is a full height keyboard accessible view with a toggle and preserved editor', async ({page}) => {
  await page.setViewportSize({width: 390, height: 844});
  await open(page);
  await settlePanel(page);
  const bounds = await page.locator('#chat-panel').boundingBox();
  expect(bounds.width).toBe(390);
  expect(bounds.height).toBe(844);
  await expect(page.locator('#chat-open')).toBeFocused();
  await page.keyboard.press('Tab');
  await expect(page.locator('#chat-conversations')).toBeFocused();
  await page.keyboard.press('Shift+Tab');
  await expect(page.locator('#chat-open')).toBeFocused();
  await send(page, 'mobile question');
  await expect(page.locator('#chat-turns')).toContainText('Saved answer');
  await page.keyboard.press('Escape');
  await expect(page.locator('#chat-panel')).toBeHidden();
  await expect(page.locator('#chat-open')).toBeFocused();
  await expect(page.locator('#editor')).toHaveValue('PRIVATE TEXT NEVER IMPLICITLY ATTACHED');
});

test('embedded app keeps a real private draft through send pinned preview explicit attach and reload', async ({
  page
}) => {
  test.setTimeout(90000);
  const modelRequests = [];
  let app, pinnedRevision;
  const provider = http.createServer(async (request, response) => {
    let body = '';
    for await (const chunk of request) body += chunk;
    modelRequests.push(JSON.parse(body));
    json(response, {
      choices: [
        {
          message: {
            role: 'assistant',
            content: '# Fixture answer\n\n[pinned evidence](Evidence.md?fmn-rev=' + pinnedRevision + ')'
          },
          finish_reason: 'stop'
        }
      ],
      usage: {prompt_tokens: 45, completion_tokens: 12}
    });
  });
  await new Promise(resolve => provider.listen(0, '127.0.0.1', resolve));
  try {
    app = await startApp({
      name: 'forgetmenote-chat-browser-',
      account: {username: 'chat-browser-admin', password: 'DisposableChatBrowserPassword123!'},
      chatConfig: {
        connections: [
          {
            id: 'local',
            label: 'Synthetic model',
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
    const {origin: appOrigin, api: realAPI} = app;
    const saved = await realAPI('/p/default/api/note', {
      path: 'Evidence.md',
      content: '# Evidence\n\nNotebook evidence about durable memories.\n',
      revision: ''
    });
    pinnedRevision = saved.revision;
    await app.login(page, 'password');
    await page.goto(appOrigin + '/?project=default&note=Evidence.md');
    await expect(page.locator('#save-state')).toContainText('Saved');
    await clickNewNote(page);
    await page.locator('#new-name').fill('Private working note');
    await page.locator('#new-submit').click();
    await expect(page.locator('#new-dialog')).toBeHidden();
    await page.locator('#format-source').click();
    const privateText = '# Private\n\nUnpublished editor content for the explicit attachment test.';
    await fillSource(page, privateText);
    const sends = [];
    page.on('request', request => {
      if (request.method() === 'POST' && request.url().endsWith('/turns')) sends.push(request.postDataJSON());
    });
    await page.locator('#chat-open').click();
    await expect(page.locator('#chat-model')).toHaveValue('synthetic');
    await send(page, 'Find notebook evidence about durable memories.');
    await expect(page.locator('#chat-turns')).toContainText('Fixture answer');
    await expect(page.locator('#chat-send')).toHaveText('Send');
    expect(sends[0].attachments).toEqual([]);
    expect(JSON.stringify(modelRequests[0])).not.toContain('Unpublished editor content');
    await page.locator('.chat-markdown a').filter({hasText: 'pinned evidence'}).first().click();
    await expect(page.locator('#knowledge-dialog')).toBeVisible();
    await expectEditorText(page, privateText);
    await page.locator('#knowledge-dialog').getByRole('button', {name: 'Close', exact: true}).click();
    await page.locator('#chat-attach').click();
    await page.locator('[data-attachment-kind="private"]').click();
    await send(page, 'Use only the explicitly attached private text.');
    await expect(page.locator('#chat-send')).toHaveText('Send');
    expect(sends[1].attachments).toEqual([
      {kind: 'private', path: 'Private working note.md', revision: '', content: privateText}
    ]);
    await expect.poll(() => modelRequests.length, {timeout: 15000}).toBeGreaterThanOrEqual(2);
    expect(JSON.stringify(modelRequests.at(-1))).toContain('Unpublished editor content');
    await expectEditorText(page, privateText);
    await page.reload();
    await expect(page.locator('#chat-panel')).toBeVisible();
    await expect(page.locator('#chat-turns')).toContainText('Use only the explicitly attached private text.');
    expect(
      await page.evaluate(() =>
        Object.entries(localStorage)
          .filter(([key]) => key.startsWith('gitnotes:draft:') && key.endsWith('/Private working note.md'))
          .some(([, raw]) => JSON.parse(raw).content?.includes('Unpublished editor content'))
      )
    ).toBe(true);
    const missing = await page.request.get(appOrigin + '/p/default/api/note?path=Private%20working%20note.md');
    expect(missing.status()).toBe(404);
    expect((await realAPI('/p/default/api/note?path=Evidence.md')).revision).toBe(pinnedRevision);
  } finally {
    try {
      await app?.close();
    } finally {
      provider.closeAllConnections();
      await new Promise(resolve => provider.close(resolve));
    }
  }
});

for (const removed of ['connection', 'model'])
  test('removed configured ' + removed + ' requires an explicit replacement choice', async ({page}) => {
    const chat = newConversation();
    chat.turns = [{id: 'question', role: 'user', content: 'Previous configured model'}];
    conversations.set(chat.id, chat);
    await open(page);
    await expect(page.locator('#chat-turns')).toContainText('Previous configured model');
    if (removed === 'connection')
      publicConnections = [{...connection, id: 'remote', label: 'Remote replacement', local: false}];
    else publicConnections = [{...connection, models: ['new-model'], defaultModel: 'new-model'}];
    await page.locator('#chat-refresh').click();
    await expect(page.locator('#chat-send')).toBeDisabled();
    await page.getByRole('button', {name: 'Chat settings', exact: true}).click();
    await expect(page.locator('#chat-model')).toBeVisible();
    await expect(page.locator('#chat-model option:checked')).toContainText('unavailable');
    expect(requests.filter(request => request.path.endsWith('/turns'))).toHaveLength(0);
  });

test('review probe compact mobile expanded options keeps composer reachable', async ({page}) => {
  await page.setViewportSize({width: 390, height: 667});
  await open(page);
  await page.getByRole('button', {name: 'Chat settings', exact: true}).click();
  await page.locator('#chat-options > summary').click();
  if (await page.locator('#chat-settings').isVisible())
    await page.locator('#chat-settings').getByRole('button', {name: 'Close', exact: true}).click();
  await page.locator('#chat-mode').selectOption('edit');
  await page.getByRole('button', {name: 'Chat settings', exact: true}).click();
  await page.locator('#chat-review').check();
  const boxes = await page.evaluate(() =>
    Object.fromEntries(
      ['chat-panel', 'chat-options', 'chat-transcript', 'chat-form', 'chat-send'].map(id => {
        const b = document.getElementById(id).getBoundingClientRect();
        return [id, {top: b.top, bottom: b.bottom, height: b.height}];
      })
    )
  );

  expect(boxes['chat-send'].bottom).toBeLessThanOrEqual(667);
});
test('review probe refresh keeps middle transcript pageable after another device adds over 100 turns', async ({
  page
}) => {
  const chat = newConversation();
  chat.turns = [
    {id: 'old-0', role: 'user', content: 'Original question'},
    {id: 'old-1', role: 'assistant', content: 'Original answer'}
  ];
  conversations.set(chat.id, chat);
  await open(page);
  await expect(page.locator('#chat-turns')).toContainText('Original answer');
  await page.locator('#chat-open').click();
  for (let i = 2; i < 205; i++)
    chat.turns.push({id: 'old-' + i, role: i % 2 ? 'assistant' : 'user', content: 'Device two message ' + i});
  await open(page);
  await expect(page.locator('#chat-turns')).toContainText('Device two message 204');

  await expect(page.locator('#chat-older')).toBeVisible();
  await page.locator('#chat-older').click();
  await expect(page.locator('#chat-turns')).toContainText('Device two message 50');
});
test('review probe temporary approval rejection can be retried after explicit refresh', async ({page}) => {
  await open(page);
  await send(page, 'approval please');
  const card = page.locator('[data-action="prepared-action"]');
  await page.route('**/actions/prepared-action', route =>
    route.fulfill({status: 409, body: 'Run is still pausing; retry shortly'})
  );
  await card.getByRole('button', {name: 'Approve action'}).click();
  await expect(page.locator('#chat-error')).toContainText('still pausing');
  await page.unroute('**/actions/prepared-action');
  await page.locator('#chat-refresh').click();
  await expect(card.getByRole('button', {name: 'Approve action'})).toBeEnabled();
});

test('embedded app keeps chat across root folder note reload and mobile navigation', async ({page}) => {
  test.setTimeout(120000);
  page.setDefaultTimeout(15000);
  const modelRequests = [],
    providerStreams = [];
  let app;
  const provider = http.createServer(async (request, response) => {
    let body = '';
    for await (const chunk of request) body += chunk;
    modelRequests.push(JSON.parse(body));
    response.writeHead(200, {'Content-Type': 'text/event-stream'});
    response.write(
      'data: ' +
        JSON.stringify({
          choices: [{index: 0, delta: {role: 'assistant', content: 'Streaming notebook '}, finish_reason: null}]
        }) +
        '\n\n'
    );
    const nextFragment = setTimeout(
      () =>
        response.write(
          'data: ' + JSON.stringify({choices: [{index: 0, delta: {content: 'answer'}, finish_reason: null}]}) + '\n\n'
        ),
      200
    );
    response.on('close', () => clearTimeout(nextFragment));
    providerStreams.push(response);
  });
  const finish = () => {
    for (const response of providerStreams.splice(0)) {
      response.write(
        'data: ' +
          JSON.stringify({
            choices: [{index: 0, delta: {content: ' completed'}, finish_reason: 'stop'}],
            usage: {prompt_tokens: 45, completion_tokens: 12}
          }) +
          '\n\ndata: [DONE]\n\n'
      );
      response.end();
    }
  };
  await new Promise(resolve => provider.listen(0, '127.0.0.1', resolve));
  try {
    app = await startApp({
      name: 'forgetmenote-chat-navigation-',
      account: {username: 'chat-navigation-admin', password: 'DisposableChatNavigationPassword123!'},
      chatConfig: {
        connections: [
          {
            id: 'local',
            label: 'Synthetic streaming model',
            format: 'chat-completions',
            baseURL: 'http://127.0.0.1:' + provider.address().port,
            models: ['synthetic'],
            local: true,
            capabilities: {tools: true, stream: true},
            limits: {runSeconds: 90}
          }
        ]
      }
    });
    const {origin: appOrigin, api: realAPI} = app;
    await realAPI('/p/default/api/note', {
      path: 'Research/Evidence.md',
      content: '# Evidence\n\nSynthetic navigation evidence.\n',
      revision: ''
    });
    await realAPI('/api/projects', {id: 'other', name: 'Other synthetic notebook'});
    await app.login(page, 'password');
    const sends = [],
      eventConnections = [];
    page.on('request', request => {
      if (request.method() === 'POST' && request.url().endsWith('/turns'))
        sends.push({url: request.url(), body: request.postDataJSON()});
      if (request.url().includes('/api/chat/') && request.url().includes('/events?'))
        eventConnections.push(request.url());
    });
    await page.goto(appOrigin + '/?project=default&folder=');
    await expect(page.locator('#folder-browser')).toBeVisible();
    await assertLauncherGeometry(page);
    await expect(page.locator('#chat-open')).toBeVisible();
    await page.locator('#chat-open').click();
    await expect(page.locator('#chat-model')).toHaveValue('synthetic');
    await expect(page.locator('#chat-attach')).toBeDisabled();
    await assertLauncherGeometry(page);
    const sidebarWasHidden = !(await page.locator('#sidebar').isVisible());
    if (sidebarWasHidden) await page.locator('#menu').click();
    const chevrons = await page.evaluate(() => {
      const style = id => {
        const css = getComputedStyle(document.getElementById(id).parentElement, '::after');
        return ['width', 'height', 'right', 'borderRight', 'transform'].map(key => css[key]);
      };
      return {notebook: style('project-select'), chat: style('chat-conversations')};
    });
    expect(chevrons.chat).toEqual(chevrons.notebook);
    if (sidebarWasHidden) await page.locator('#menu').click();
    await send(page, 'Start a notebook question from the root.');
    await expect(page.locator('#chat-turns')).toContainText('Streaming notebook answer');
    await page.locator('#chat-input').fill('Private follow-up kept while browsing');
    const conversation = await page.locator('#chat-conversations').inputValue(),
      connected = eventConnections.length;
    await page.locator('#folder-groups [data-kind="folder"] .browse-item-link').click();
    await expect(page.locator('#folder-title')).toHaveText('Research');
    await expect(page.locator('#chat-panel')).toBeVisible();
    await expect(page.locator('#chat-open')).toBeVisible();
    await expect(page.locator('#chat-input')).toHaveValue('Private follow-up kept while browsing');
    await page.locator('#folder-items [data-kind="note"] .browse-item-link').click();
    await expect(page.locator('#document')).toBeVisible();
    await expect(page.locator('#chat-conversations')).toHaveValue(conversation);
    await expect(page.locator('#chat-stop')).toBeVisible();
    finish();
    await expect(page.locator('#chat-turns')).toContainText('Streaming notebook answer completed');
    await expect(page.locator('#chat-attach')).toBeEnabled();
    await page.locator('#chat-attach').click();
    await expect(page.getByRole('dialog', {name: 'Attach a note version', exact: true})).toContainText(
      'Research/Evidence.md'
    );
    await page
      .getByRole('dialog', {name: 'Attach a note version', exact: true})
      .getByRole('button', {name: 'Close', exact: true})
      .click();
    // Root navigation uses the actual sidebar; the chat opener must also work when it is collapsed.
    if (await page.locator('#sidebar').isHidden()) await page.locator('#menu').click();
    await page.locator('#browse-root').click();
    await expect(page.locator('#folder-browser')).toBeVisible();
    await expect(page.locator('#chat-attach')).toBeDisabled();
    await expect(page.locator('#chat-input')).toHaveValue('Private follow-up kept while browsing');
    expect(sends).toHaveLength(1);
    expect(sends[0].body.attachments).toEqual([]);
    expect(modelRequests).toHaveLength(1);
    expect(eventConnections.length).toBeLessThanOrEqual(connected + 1);
    await page.reload();
    await expect(page.locator('#chat-panel')).toBeVisible();
    await expect(page.locator('#chat-conversations')).toHaveValue(conversation);
    await expect(page.locator('#chat-turns')).toContainText('Streaming notebook answer completed');
    expect(sends).toHaveLength(1);
    await page.locator('#chat-new').click();
    await send(page, 'A second saved conversation');
    await expect.poll(() => modelRequests.length).toBe(2);
    finish();
    await expect(page.locator('#chat-turns')).toContainText('completed');
    await page.locator('#chat-conversations').selectOption(conversation);
    await expect(page.locator('#chat-turns')).toContainText('Start a notebook question from the root.');
    await page.reload();
    await expect(page.locator('#chat-panel')).toBeVisible();
    await expect(page.locator('#chat-conversations')).toHaveValue(conversation);
    await page.setViewportSize({width: 390, height: 667});
    await expect(page.locator('#chat-panel')).toHaveAttribute('aria-modal', 'true');
    await assertLauncherGeometry(page);
    await page.locator('#chat-input').fill('Mobile unsent follow-up');
    await page.locator('#chat-open').click();
    await expect(page.locator('#chat-open')).toBeFocused();
    await expect(page.locator('#sidebar')).toBeHidden();
    await expect(page.locator('#chat-open')).toBeVisible();
    await page.locator('#folder-groups [data-kind="folder"] .browse-item-link').click();
    await page.locator('#chat-open').click();
    await expect(page.locator('#chat-input')).toHaveValue('Mobile unsent follow-up');
    await page.locator('#chat-open').click();
    await page.locator('#folder-items [data-kind="note"] .browse-item-link').click();
    await page.locator('#chat-open').click();
    await expect(page.locator('#chat-conversations')).toHaveValue(conversation);
    await expect(page.locator('#chat-input')).toHaveValue('Mobile unsent follow-up');
    await page.locator('#chat-open').click();
    await page.locator('#menu').click();
    await page.locator('#project-select').selectOption('other');
    await page.keyboard.press('Escape');
    await expect(page.locator('#sidebar')).toBeHidden();
    await page.locator('#chat-open').click();
    await expect(page.locator('#chat-scope')).toContainText('Other synthetic notebook');
    await expect(page.locator('#chat-turns')).toBeEmpty();
    await expect(page.locator('#chat-input')).toHaveValue('');
    await expect(page.locator('#chat-attachments')).toBeEmpty();
    await expect(page.locator('#chat-attach')).toBeDisabled();
    await page.reload();
    await expect(page.locator('#chat-panel')).toBeVisible();
    await expect(page.locator('#chat-turns')).toBeEmpty();
    await expect(page.locator('#chat-scope')).toContainText('Other synthetic notebook');
    expect(sends).toHaveLength(2);
    expect(sends.every(request => request.url.includes('/p/default/'))).toBe(true);
    expect(
      await page.evaluate(() =>
        JSON.stringify([
          Object.entries(localStorage).filter(([key]) => key.includes('chat')),
          sessionStorage.getItem('gitnotes:chat-view')
        ])
      )
    ).not.toContain('Mobile unsent');
    await page.locator('#chat-open').click();
    await page.reload();
    await expect(page.locator('#chat-panel')).toBeHidden();
    // Exercise the real empty app shell while the synthetic account has no available notebooks.
    await page.route('**/api/projects', route => route.fulfill({json: []}));
    await page.reload();
    await expect(page.locator('#empty')).toBeVisible();
    await expect(page.locator('#chat-open')).toBeVisible();
    await page.locator('#chat-open').click();
    await expect(page.locator('#chat-message')).toContainText('Choose a notebook');
    await expect(page.locator('#chat-send')).toBeDisabled();
  } finally {
    finish();
    try {
      await app?.close();
    } finally {
      provider.closeAllConnections();
      await new Promise(resolve => provider.close(resolve));
    }
  }
});

test('account changes clear chat and reload markers contain only scoped presentation state', async ({page}) => {
  await open(page);
  await send(page, 'Saved personal question');
  await expect(page.locator('#chat-turns')).toContainText('Saved answer');
  await page.locator('#chat-input').fill('Unsent personal question');
  await page.locator('#chat-attach').click();
  await page.locator('[data-attachment-kind="private"]').click();
  const saved = await page.evaluate(() => JSON.parse(sessionStorage.getItem('gitnotes:chat-view')));
  expect(saved).toEqual({
    user: 'synthetic-user',
    project: 'default',
    open: true,
    conversation: await page.locator('#chat-conversations').inputValue(),
    newChat: false
  });
  expect(await page.evaluate(() => Object.keys(localStorage))).toEqual([]);
  conversations.clear();
  await page.locator('#switch-account').click();
  await expect(page.locator('#chat-turns')).toBeEmpty();
  await expect(page.locator('#chat-input')).toHaveValue('');
  await expect(page.locator('#chat-attachments')).toBeEmpty();
  await expect(page.locator('#chat-conversations')).not.toContainText('Synthetic conversation');
  expect(await page.evaluate(() => JSON.parse(sessionStorage.getItem('gitnotes:chat-view')).user)).toBe('other-user');
  // Reloading this fixture signs back into the original account; the other account's open marker cannot reopen it.
  await page.reload();
  await expect(page.locator('#chat-panel')).toBeHidden();
  await expect(page.locator('#chat-turns')).toBeEmpty();
});

test('streamed tool updates move into Activity and remain collapsed after replay', async ({page}) => {
  const chat = newConversation(),
    run = {id: 'readable-run', state: 'running', options: chat.options, operations: []};
  chat.runs = [run];
  chat.turns = [
    {id: 'readable-question', runId: run.id, role: 'user', content: 'Read the notebook'},
    {id: 'old-update', runId: run.id, role: 'assistant', content: 'Earlier tool update', intermediate: true}
  ];
  conversations.set(chat.id, chat);
  await open(page);
  await expect(page.getByText('Earlier tool update', {exact: true})).toBeHidden();
  await expect(page.locator('#chat-stop')).toBeVisible();
  const update = {id: 'live-update', runId: run.id, role: 'assistant', content: 'Streaming tool update'};
  chat.turns.push(update);
  emit(chat, run.id, 'text_delta', {turnId: update.id, text: update.content});
  await expect(page.locator('#chat-turns > [data-turn="live-update"]')).toBeVisible();
  update.intermediate = true;
  emit(chat, run.id, 'assistant', update);
  await expect(page.locator('#chat-turns > [data-turn="live-update"]')).toHaveCount(0);
  await expect(page.getByText(update.content, {exact: true})).toBeHidden();
  await page.locator('.chat-activity > summary').click();
  await expect(page.getByText(update.content, {exact: true})).toBeVisible();
  emit(chat, run.id, 'progress', {message: 'Reading evidence'});
  await expect(page.locator('.chat-activity')).toHaveAttribute('open', '');
  complete(chat, run, 'The final readable answer.');
  await expect(page.locator('.chat-turn-assistant')).toHaveCount(1);
  await page.reload();
  await expect(page.locator('#chat-panel')).toBeVisible();
  await expect(page.locator('.chat-activity')).not.toHaveAttribute('open');
  await expect(page.locator('.chat-turn-assistant')).toHaveCount(1);
  await expect(page.getByText(update.content, {exact: true})).toBeHidden();
});

test('mobile required approval keeps decision buttons visible without opening Activity', async ({page}) => {
  await page.setViewportSize({width: 390, height: 844});
  await open(page);
  await send(page, 'approval please');
  await expect(page.locator('.chat-activity')).not.toHaveAttribute('open');
  await expect(page.locator('#chat-runs')).toContainText('Approval required');
  const card = page.locator('[data-action="prepared-action"]');
  await expect(card).toBeVisible();
  await expect(card.getByRole('button', {name: 'Approve action'})).toBeInViewport();
  await expect(card.getByRole('button', {name: 'Decline', exact: true})).toBeInViewport();
  await expect(card.locator('details')).toHaveAttribute('open', '');
  expect(requests.filter(request => request.path.includes('/actions/'))).toHaveLength(0);
});

function olderRunConversation(state = 'interrupted') {
  const chat = newConversation(),
    older = {
      id: 'older-run',
      state,
      options: chat.options,
      operations: [],
      message: 'Saved earlier run budget reached'
    },
    latest = {id: 'latest-run', state: 'completed', options: chat.options, operations: []};
  chat.runs = [older, latest];
  chat.turns = [
    {id: 'older-question', runId: older.id, role: 'user', content: 'Earlier question'},
    {id: 'latest-question', runId: latest.id, role: 'user', content: 'Newer question'},
    {id: 'latest-answer', runId: latest.id, role: 'assistant', content: 'The newer completed answer stays readable.'}
  ];
  conversations.set(chat.id, chat);
  return {chat, older, latest};
}
for (const incompleteReview of [false, true])
  test(
    'older ' +
      (incompleteReview ? 'incomplete review' : 'interrupted run') +
      ' remains explicitly resumable after a newer completed answer',
    async ({page}) => {
      const {chat, older, latest} = olderRunConversation(incompleteReview ? 'completed' : 'interrupted');
      if (incompleteReview) {
        older.review = {notes: [], complete: false};
        chat.turns = chat.turns.filter(turn => turn.runId !== older.id);
      }
      await open(page);
      await page.locator('#chat-input').fill('Unsent follow-up while resuming earlier work');
      const activity = page.locator('[data-run="older-run"] .chat-activity');
      await expect(activity).not.toHaveAttribute('open');
      await expect(page.locator('#chat-continue')).toHaveCount(0);
      await activity.locator('summary').first().click();
      await expect(activity.getByRole('button', {name: 'Continue', exact: true})).toBeVisible();
      await activity.getByRole('button', {name: 'Continue', exact: true}).click();
      await expect(page.locator('#chat-turns')).toContainText('Continued answer');
      expect(requests.filter(request => request.path.endsWith('/resume')).map(request => request.path)).toEqual([
        '/p/default/api/chat/' + chat.id + '/runs/older-run/resume'
      ]);
      expect(latest.state).toBe('completed');
      await expect(page.locator('#chat-input')).toHaveValue('Unsent follow-up while resuming earlier work');
      await expect(page.locator('#editor')).toHaveValue('PRIVATE TEXT NEVER IMPLICITLY ATTACHED');
    }
  );
for (const arrival of ['snapshot', 'sse', 'reconnect'])
  test(
    'older active run from ' + arrival + ' owns visible Stop and blocks a second Send without losing draft',
    async ({page}) => {
      const {chat, older, latest} = olderRunConversation();
      await open(page);
      await page.locator('#chat-input').fill('Keep this unsent message');
      older.state = 'running';
      older.progress = 'Continuing the earlier run';
      older.message = '';
      if (arrival === 'snapshot') await page.locator('#chat-refresh').click();
      else if (arrival === 'sse')
        emit(chat, older.id, 'state', {state: older.state, progress: older.progress, message: ''});
      else {
        await expect.poll(() => streams.get(chat.id)?.size || 0).toBe(1);
        for (const response of streams.get(chat.id)) response.end();
      }
      await expect(page.locator('#chat-stop')).toBeVisible();
      await expect(page.locator('#chat-runs')).toContainText('Working…');
      await expect(page.locator('#chat-send')).toBeDisabled();
      await expect(page.locator('#chat-attach')).toBeDisabled();
      await expect(page.locator('#chat-input')).toHaveValue('Keep this unsent message');
      await page.locator('#chat-stop').click();
      expect(requests.filter(request => request.path.endsWith('/cancel')).map(request => request.path)).toEqual([
        '/p/default/api/chat/' + chat.id + '/runs/older-run/cancel'
      ]);
      expect(latest.state).toBe('completed');
      await expect(page.locator('#chat-send')).toBeEnabled();
      await expect(page.locator('#chat-attach')).toBeEnabled();
      await expect(page.locator('#chat-input')).toHaveValue('Keep this unsent message');
      expect(requests.filter(request => request.path.endsWith('/turns'))).toHaveLength(0);
      await expect(page.locator('#editor')).toHaveValue('PRIVATE TEXT NEVER IMPLICITLY ATTACHED');
    }
  );

test('native settings modal returns keyboard focus and survives notebook and account changes', async ({page}) => {
  await open(page);
  await page.locator('#chat-input').fill('Unsent settings draft');
  const gear = page.getByRole('button', {name: 'Chat settings', exact: true}),
    dialog = page.getByRole('dialog', {name: 'Chat settings', exact: true});
  await gear.click();
  expect(await dialog.evaluate(node => node.matches(':modal'))).toBe(true);
  await page.keyboard.press('Escape');
  await page.locator('#chat-mode').selectOption('suggest');
  await gear.click();
  await page.keyboard.press('Escape');
  await expect(dialog).toBeHidden();
  await expect(gear).toBeFocused();
  await expect(page.locator('#chat-panel')).toBeVisible();
  await expect(page.locator('#chat-input')).toHaveValue('Unsent settings draft');
  await page.setViewportSize({width: 390, height: 667});
  await gear.click();
  await page.keyboard.press('Escape');
  await expect(gear).toBeFocused();
  expect(await page.locator('#main').evaluate(node => node.inert)).toBe(true);
  expect(await page.locator('#chat-open').evaluate(node => !!node.closest('[inert]'))).toBe(false);
  await page.locator('#chat-open').click();
  expect(await page.locator('#main').evaluate(node => node.inert)).toBe(false);
  await page.locator('#switch-project').click();
  await open(page);
  await gear.click();
  await expect(dialog).toBeVisible();
  await expect(page.locator('#chat-mode')).toHaveValue('ask');
  await expect(page.locator('#chat-input')).toHaveValue('');
  await page.keyboard.press('Escape');
  await page.locator('#chat-open').click();
  await page.locator('#switch-account').click();
  await open(page);
  await gear.click();
  await expect(dialog).toBeVisible();
  expect(await page.locator('#chat-settings').count()).toBe(1);
  await expect(page.locator('#chat-mode')).toHaveValue('ask');
});

test('saving an older message selects that message and restores its actions focus', async ({page}) => {
  await open(page);
  await send(page, 'First requested message');
  await expect(page.locator('#chat-send')).toBeEnabled();
  await send(page, 'Second requested message');
  await expect(page.locator('#chat-send')).toBeEnabled();
  const actions = page.locator('[data-turn="user-run-1"]').getByRole('button', {name: 'Message actions', exact: true});
  await actions.click();
  await page.getByRole('button', {name: 'Save to note', exact: true}).click();
  const dialog = page.getByRole('dialog', {name: 'Save chat to a note', exact: true});
  await expect(dialog).toBeVisible();
  expect(
    await page.locator('.chat-save-turns input:checked').evaluateAll(nodes => nodes.map(node => node.value))
  ).toEqual(['user-run-1']);
  await expect(page.locator('#chat-save-preview')).toContainText('First requested message');
  await expect(page.locator('#chat-save-preview')).not.toContainText('Second requested message');
  await dialog.getByRole('button', {name: 'Close', exact: true}).click();
  await expect(actions).toBeFocused();
});

test('chat bubble is top right and the selector shares the notebook chevron', async ({page}) => {
  await open(page);
  await settlePanel(page);
  await expect(page.locator('#chat-close')).toHaveCount(0);
  for (const width of [1280, 390]) {
    await page.setViewportSize({width, height: 844});
    const geometry = await page.evaluate(() => {
      const bubble = document.getElementById('chat-open'),
        b = bubble.getBoundingClientRect();
      return {
        top: b.top,
        right: b.right,
        hit: document.elementFromPoint(b.x + b.width / 2, b.y + b.height / 2)?.closest('button')?.id,
        overlap: ['chat-title', 'chat-new', 'chat-settings-open'].some(id => {
          const r = document.getElementById(id).getBoundingClientRect();
          return r.left < b.right && r.right > b.left && r.top < b.bottom && r.bottom > b.top;
        }),
        picker: document.getElementById('chat-conversations').parentElement.classList.contains('select-picker')
      };
    });
    expect(geometry.top).toBeLessThanOrEqual(24);
    expect(geometry.right).toBeGreaterThan(width - 40);
    expect(geometry.hit).toBe('chat-open');
    expect(geometry.overlap).toBe(false);
    expect(geometry.picker).toBe(true);
  }
  await page.locator('#chat-open').click();
  await expect(page.locator('#chat-panel')).toBeHidden();
  await page.locator('#chat-open').click();
  await expect(page.locator('#chat-panel')).toBeVisible();
});

test('delete from the conversation selector confirms and keeps unrelated unsent composers', async ({page}) => {
  const first = newConversation('1'.repeat(32)),
    second = newConversation('2'.repeat(32));
  first.title = 'Delete this conversation';
  first.turns = [{id: 'sensitive', role: 'user', content: 'Saved secret to delete'}];
  second.title = 'Keep this conversation';
  conversations.set(first.id, first);
  conversations.set(second.id, second);
  await open(page);
  await expect(page.locator('#chat-turns')).toContainText('Saved secret to delete');
  await page.locator('#chat-conversations').selectOption(second.id);
  await page.locator('#chat-input').fill('Keep this unsent composer');
  await page.locator('#chat-conversations').selectOption(first.id);
  await page.getByRole('button', {name: 'Delete conversation', exact: true}).click();
  const dialog = page.getByRole('dialog', {name: 'Delete conversation?', exact: true});
  await expect(dialog).toContainText(first.title);
  await expect(dialog.getByRole('button', {name: 'Cancel', exact: true})).toBeFocused();
  await page.keyboard.press('Escape');
  await expect(dialog).toHaveCount(0);
  expect(conversations.has(first.id)).toBe(true);
  await page.getByRole('button', {name: 'Delete conversation', exact: true}).click();
  await page
    .getByRole('dialog', {name: 'Delete conversation?', exact: true})
    .getByRole('button', {name: 'Delete conversation', exact: true})
    .click();
  await expect(page.locator('#chat-conversations option[value="' + first.id + '"]')).toHaveCount(0);
  await expect(page.locator('#chat-turns')).not.toContainText('Saved secret to delete');
  await page.locator('#chat-conversations').selectOption(second.id);
  await expect(page.locator('#chat-input')).toHaveValue('Keep this unsent composer');
  await expect(page.locator('#editor')).toHaveValue('PRIVATE TEXT NEVER IMPLICITLY ATTACHED');
});

async function assertLauncherGeometry(page) {
  await settlePanel(page);
  const result = await page.evaluate(() => {
    const bubble = document.getElementById('chat-open'),
      b = bubble.getBoundingClientRect();
    const overlaps = [
      ...document.querySelectorAll('#main > header button,#chat-panel > .chat-heading button,#chat-title')
    ]
      .filter(node => node !== bubble && !node.closest('[inert]') && !node.hidden && node.getClientRects().length)
      .filter(node => {
        const r = node.getBoundingClientRect();
        return r.left < b.right && r.right > b.left && r.top < b.bottom && r.bottom > b.top;
      })
      .map(node => node.id);
    return {
      top: b.top,
      hit: document.elementFromPoint(b.x + b.width / 2, b.y + b.height / 2)?.closest('button')?.id,
      overlaps
    };
  });
  expect(result.top).toBeLessThanOrEqual(24);
  expect(result.hit).toBe('chat-open');
  expect(result.overlaps).toEqual([]);
}

test('another device deleting chat clears saved content while preserving each unsent draft', async ({page}) => {
  const first = newConversation();
  first.turns = [{id: 'saved-secret', role: 'user', content: 'Saved private conversation'}];
  conversations.set(first.id, first);
  await open(page);
  const other = await page.context().newPage();
  await other.goto(origin);
  await open(other);
  await other.locator('#chat-new').click();
  await other.locator('#chat-input').fill('Existing new composer');
  await other.locator('#chat-conversations').selectOption(first.id);
  await other.locator('#chat-input').fill('Other-device unsent text');
  await page.getByRole('button', {name: 'Delete conversation', exact: true}).click();
  await page
    .getByRole('dialog', {name: 'Delete conversation?', exact: true})
    .getByRole('button', {name: 'Delete conversation', exact: true})
    .click();
  await expect(other.locator('#chat-turns')).toBeEmpty();
  await expect(other.locator('#chat-input')).toHaveValue('Other-device unsent text');
  await expect(other.locator('#chat-conversations')).toHaveValue('draft:' + first.id);
  await other.locator('#chat-conversations').selectOption('');
  await expect(other.locator('#chat-input')).toHaveValue('Existing new composer');
  await other.locator('#chat-conversations').selectOption('draft:' + first.id);
  await expect(other.locator('#chat-input')).toHaveValue('Other-device unsent text');
  await other.close();
});

test('active chat deletion asks the user to stop and can be retried after stopping', async ({page}) => {
  await open(page);
  await send(page, 'stream before delete');
  await expect(page.locator('#chat-turns')).toContainText('Streaming answer');
  await page.getByRole('button', {name: 'Delete conversation', exact: true}).click();
  const dialog = page.getByRole('dialog', {name: 'Delete conversation?', exact: true});
  await dialog.getByRole('button', {name: 'Delete conversation', exact: true}).click();
  await expect(dialog.getByRole('alert')).toContainText('Stop');
  await dialog.getByRole('button', {name: 'Cancel', exact: true}).click();
  await page.locator('#chat-stop').click();
  await expect(page.locator('#chat-continue')).toBeVisible();
  await page.getByRole('button', {name: 'Delete conversation', exact: true}).click();
  await dialog.getByRole('button', {name: 'Delete conversation', exact: true}).click();
  await expect(page.locator('#chat-message')).toHaveText('Conversation deleted.');
  await expect(page.locator('#chat-turns')).toBeEmpty();
});

test('unselected remotely deleted chat keeps its text and attachment as a reachable draft', async ({page}) => {
  const first = newConversation('1'.repeat(32)),
    second = newConversation('2'.repeat(32));
  conversations.set(first.id, first);
  conversations.set(second.id, second);
  await open(page);
  await page.locator('#chat-new').click();
  await page.locator('#chat-input').fill('Separate new-message draft');
  await page.locator('#chat-conversations').selectOption(first.id);
  await page.locator('#chat-input').fill('Recover my unsent text');
  await page.locator('#chat-attach').click();
  await page.locator('[data-attachment-kind="private"]').click();
  await page.locator('#chat-conversations').selectOption(second.id);
  await page.locator('#chat-input').fill('Keep the selected composer');
  expect((await page.request.delete(origin + '/p/default/api/chat/' + first.id)).status()).toBe(200);
  await page.evaluate(() => window.chat.refresh());
  await expect(page.locator('#chat-conversations')).toHaveValue(second.id);
  await expect(page.locator('#chat-input')).toHaveValue('Keep the selected composer');
  await expect(page.locator('#chat-conversations option[value="draft:' + first.id + '"]')).toHaveCount(1);
  await page.locator('#chat-conversations').selectOption('');
  await expect(page.locator('#chat-input')).toHaveValue('Separate new-message draft');
  await page.locator('#chat-conversations').selectOption('draft:' + first.id);
  await expect(page.locator('#chat-input')).toHaveValue('Recover my unsent text');
  await expect(page.locator('#chat-attachments')).toContainText('Private draft');
});

test('attachment-only orphan stays selected after visiting saved chat and awaiting refresh', async ({page}) => {
  const first = newConversation('1'.repeat(32)),
    second = newConversation('2'.repeat(32));
  conversations.set(first.id, first);
  conversations.set(second.id, second);
  await open(page);
  await page.locator('#chat-attach').click();
  await page.locator('[data-attachment-kind="private"]').click();
  await page.locator('#chat-delete').click();
  await page
    .getByRole('dialog', {name: 'Delete conversation?', exact: true})
    .getByRole('button', {name: 'Delete conversation', exact: true})
    .click();
  await expect(page.locator('#chat-conversations')).toHaveValue('draft:' + first.id);
  await page.locator('#chat-conversations').selectOption(second.id);
  await page.locator('#chat-conversations').selectOption('draft:' + first.id);
  await page.evaluate(() => window.chat.refresh());
  await expect(page.locator('#chat-conversations')).toHaveValue('draft:' + first.id);
  await expect(page.locator('#chat-input')).toHaveValue('');
  await expect(page.locator('#chat-attachments')).toContainText('Private draft');
});

for (const ordering of [
  'cleanup error before retirement',
  'retirement before cleanup error',
  'retirement before lost acknowledgement',
  'retirement before success'
]) {
  test('pending deletion survives ' + ordering, async ({page}) => {
    const first = newConversation('1'.repeat(32)),
      second = newConversation('2'.repeat(32));
    first.turns = [{id: 'saved', role: 'user', content: 'Private saved transcript'}];
    conversations.set(first.id, first);
    conversations.set(second.id, second);
    await open(page);
    await expect(page.locator('#chat-turns')).toContainText('Private saved transcript');
    await page.locator('#chat-new').click();
    await page.locator('#chat-input').fill('Separate new-message composer');
    await page.locator('#chat-conversations').selectOption(second.id);
    await page.locator('#chat-input').fill('Unrelated saved-chat composer');
    await page.locator('#chat-conversations').selectOption(first.id);
    await page.locator('#chat-input').fill('Private unsent follow-up');
    await page.locator('#chat-attach').click();
    await page.locator('[data-attachment-kind="private"]').click();
    const endpoint = '/p/default/api/chat/' + first.id,
      attempts = [];
    let release, startedResolve;
    const started = new Promise(resolve => (startedResolve = resolve)),
      held = new Promise(resolve => (release = resolve));
    await page.route('**/api/chat/' + first.id, async route => {
      if (route.request().method() !== 'DELETE') {
        await route.continue();
        return;
      }
      attempts.push(new URL(route.request().url()).pathname);
      if (attempts.length > 1) {
        await route.fulfill({status: 200, json: {deleted: true}});
        return;
      }
      conversations.delete(first.id);
      startedResolve();
      await held;
      if (ordering.includes('lost acknowledgement')) await route.abort('failed');
      else if (ordering.endsWith('success')) await route.fulfill({status: 200, json: {deleted: true}});
      else await route.fulfill({status: 500, contentType: 'text/plain', body: 'Cleanup failed; retry deletion'});
    });
    try {
      await page.locator('#chat-delete').click();
      const dialog = page.getByRole('dialog', {name: 'Delete conversation?', exact: true});
      await dialog.getByRole('button', {name: 'Delete conversation', exact: true}).click();
      await started;
      if (ordering.startsWith('cleanup error')) {
        release();
        await expect(dialog.getByRole('alert')).toContainText('Cleanup failed');
      }
      for (const response of streams.get(first.id) || []) response.end();
      await page.evaluate(() => window.chat.refresh());
      await expect(page.locator('#chat-turns')).toBeEmpty();
      await expect(page.locator('#chat-conversations')).toHaveValue('draft:' + first.id);
      await expect(page.locator('#chat-input')).toHaveValue('Private unsent follow-up');
      await expect(page.locator('#chat-attachments')).toContainText('Private draft');
      await expect(dialog).toBeVisible();
      if (!ordering.startsWith('cleanup error')) {
        await expect(dialog.getByRole('button', {name: 'Delete conversation', exact: true})).toBeDisabled();
        release();
      }
      if (!ordering.endsWith('success')) {
        await expect(dialog.getByRole('alert')).toContainText(
          ordering.includes('lost acknowledgement') ? 'Failed to fetch' : 'Cleanup failed'
        );
        const retry = dialog.getByRole('button', {name: 'Retry deletion', exact: true});
        await expect(retry).toBeEnabled();
        await page.evaluate(() => window.chat.refresh());
        await expect(dialog.getByRole('alert')).not.toBeEmpty();
        expect(attempts).toEqual([endpoint]);
        await retry.click();
      }
      await expect(dialog).toHaveCount(0);
      await expect(page.locator('#chat-message')).toHaveText('Conversation deleted.');
      expect(attempts).toEqual(ordering.endsWith('success') ? [endpoint] : [endpoint, endpoint]);
      await expect(page.locator('#chat-conversations')).toHaveValue('draft:' + first.id);
      await expect(page.locator('#chat-input')).toHaveValue('Private unsent follow-up');
      await expect(page.locator('#chat-attachments')).toContainText('Private draft');
      await page.locator('#chat-conversations').selectOption(second.id);
      await expect(page.locator('#chat-input')).toHaveValue('Unrelated saved-chat composer');
      await page.locator('#chat-conversations').selectOption('');
      await expect(page.locator('#chat-input')).toHaveValue('Separate new-message composer');
    } finally {
      release();
    }
  });
}

for (const target of ['saved conversation', 'New chat', 'newer edit'])
  test('delayed correction preserves private text after ' + target, async ({page}) => {
    const saved = newConversation();
    conversations.set(saved.id, saved);
    await open(page);
    if (target === 'New chat') await page.locator('#chat-new').click();
    await page.locator('#chat-input').fill('Existing private unsent text');
    let release, entered;
    const gate = new Promise(resolve => (release = resolve)),
      started = new Promise(resolve => (entered = resolve));
    let delayed = false;
    await page.route('**/api/knowledge/retrieval-settings', async route => {
      if (!delayed) {
        delayed = true;
        entered();
        await gate;
      }
      try {
        await route.continue();
      } catch {}
    });
    await page.evaluate(() => {
      window.pendingCorrection = window.chat.prepareMessage('STALE CORRECTION MUST NOT REPLACE PRIVATE TEXT');
    });
    await started;
    if (target === 'saved conversation') await page.locator('#chat-conversations').selectOption(saved.id);
    else if (target === 'New chat') await page.locator('#chat-new').click();
    else await page.locator('#chat-input').fill('Newer private edit while defaults load');
    release();
    await page.evaluate(() => window.pendingCorrection);
    const expected =
      target === 'newer edit' ? 'Newer private edit while defaults load' : 'Existing private unsent text';
    await expect(page.locator('#chat-input')).toHaveValue(expected);
    // Leave and return through the public selector, checking the retained draft too.
    if (target === 'saved conversation') {
      await page.locator('#chat-new').click();
      await page.locator('#chat-conversations').selectOption(saved.id);
    } else if (target === 'New chat') {
      await page.locator('#chat-conversations').selectOption(saved.id);
      await page.locator('#chat-new').click();
    } else {
      await page.locator('#chat-conversations').selectOption(saved.id);
      const draft = page.locator('#chat-conversations option').filter({hasText: 'Unsent draft'});
      await page.locator('#chat-conversations').selectOption(await draft.last().getAttribute('value'));
    }
    await expect(page.locator('#chat-input')).toHaveValue(expected);
  });

async function clickNewNote(page) {
  await expect(page.locator('#new-note')).toBeEnabled();
  if (await page.locator('#new-note').isHidden()) {
    if (await page.locator('#sidebar').isHidden()) await page.locator('#menu').click();
    await page.locator('#sidebar-new-note').click();
  } else await page.locator('#new-note').click();
}

test('cross-notebook and code citations use the document navigation callback', async ({page}) => {
  const chat = newConversation(),
    source = 'b'.repeat(32);
  chat.turns = [
    {
      id: 'answer',
      role: 'assistant',
      content: 'Pinned evidence across notebooks.',
      citations: [
        {
          id: 'note',
          kind: 'notebook',
          title: 'Other notebook page',
          project: 'other',
          path: 'Guide.md',
          revision,
          url: '/?project=other&note=Guide.md&revision=' + revision
        },
        {
          id: 'source',
          kind: 'repository',
          title: 'Source file',
          project: 'code',
          source,
          path: 'server.py',
          revision,
          startLine: 5,
          endLine: 9,
          url: 'https://github.com/example/code/blob/' + revision + '/server.py'
        }
      ]
    }
  ];
  conversations.set(chat.id, chat);
  await open(page);
  await page.locator('.chat-sources > summary').click();
  await page.getByRole('button', {name: 'Other notebook page', exact: true}).click();
  await page.getByRole('button', {name: 'Source file', exact: true}).click();
  expect(await page.evaluate(() => window.documentTargets)).toEqual([
    {project: 'other', origin: 'notes', path: 'Guide.md', revision},
    {project: 'code', origin: 'repository', source, path: 'server.py', revision, startLine: 5, endLine: 9}
  ]);
  await expect(page.locator('#editor')).toHaveValue('PRIVATE TEXT NEVER IMPLICITLY ATTACHED');
});
