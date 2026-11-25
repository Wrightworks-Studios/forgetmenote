import {editorText, fillSource} from './editor-text.mjs';
import {test, expect} from '@playwright/test';
import http from 'node:http';
// Each worker owns an app; baseline reset makes cases independent within it.
test.describe.configure({mode: 'parallel'});
import {origin, api, login, edit, useEditorApp} from './editor-fixture.mjs';
useEditorApp();

const viewNote = (title, source) => `# ${title}\n\nAn interactive view.\n\n\`\`\`view\n${source.trim()}\n\`\`\`\n`;
const frame = page => page.frameLocator('iframe.note-view-frame');
async function openNote(page, path, content) {
  const note = await api('/p/default/api/note', {path, content, revision: ''});
  await login(page);
  await page.goto(origin + '/?project=default&note=' + path);
  await expect(page.locator('.note-view')).toBeVisible();
  return note;
}
// A real server on another origin stands for the outside world: it records
// every request that leaves the browser. Chromium reports some attempts that
// the view's policy then blocks as requests, so only arrivals count.
async function outsideServer() {
  const hits = [];
  const server = http.createServer((request, response) => {
    hits.push(request.url);
    response.end('reached');
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  return {url: `http://127.0.0.1:${server.address().port}`, hits, close: () => server.close()};
}

test('a view reads its own notebook through the bridge and sizes its frame', async ({page}) => {
  await api('/p/default/api/note', {path: 'views/source.md', content: '# Wombat facts\n\nThey dig.\n', revision: ''});
  await api('/p/default/api/note', {path: 'elsewhere/other.md', content: '# Other\n', revision: ''});
  const note = await openNote(
    page,
    'views/bridge.md',
    viewNote(
      'Bridge',
      `
<pre id="out">waiting</pre>
<div style="height: 600px"></div>
<script>
const settle = promise => promise.then(value => value, error => 'error: ' + error.message);
async function found() {
  for (let attempt = 0; attempt < 60; attempt++) {
    const hits = await fmn.notes.search('Wombat');
    if (hits.length) return hits.map(hit => hit.path);
    await new Promise(resolve => setTimeout(resolve, 500));
  }
  return [];
}
Promise.all([
  fmn.notes.list('views'),
  fmn.notes.read('views/source.md'),
  fmn.theme(),
  settle(fmn.notes.read('../outside.md')),
  settle(fmn.notes.read('assets/picture.png')),
  found()
]).then(([list, source, theme, parentPath, asset, search]) => {
  document.getElementById('out').textContent = JSON.stringify({
    list: list.map(entry => entry.path).sort(),
    content: source.content,
    theme: theme.scheme,
    parentPath,
    asset,
    search
  });
}, error => {
  document.getElementById('out').textContent = 'failed: ' + error.message;
});
</script>
`
    )
  );
  await expect(page.locator('.note-view-label')).toHaveText('Interactive view');
  await expect(frame(page).locator('#out')).toContainText('"content"', {timeout: 40000});
  const result = JSON.parse(await frame(page).locator('#out').textContent());
  expect(result).toEqual({
    list: ['views/bridge.md', 'views/source.md'],
    content: '# Wombat facts\n\nThey dig.\n',
    theme: expect.stringMatching(/^(light|dark)$/),
    parentPath: 'error: A note path ending in .md is required',
    asset: 'error: A note path ending in .md is required',
    search: ['views/source.md']
  });
  await expect
    .poll(() => page.locator('iframe.note-view-frame').evaluate(element => element.getBoundingClientRect().height))
    .toBeGreaterThan(600);
  const open = page.locator('.note-view-bar a', {hasText: 'Open'});
  await expect(open).toHaveAttribute(
    'href',
    /\/p\/default\/api\/view\?path=views%2Fbridge\.md&revision=[0-9a-f]+&block=0$/
  );
  expect(await open.getAttribute('href')).toContain(note.revision);

  const code = page.locator('.note-view > pre');
  await expect(code).toBeHidden();
  await page.locator('.note-view-bar button', {hasText: 'Code'}).click();
  await expect(code).toBeVisible();
  await expect(code).toContainText('fmn.notes.list');

  await page.locator('.note-view-bar button', {hasText: 'Stop'}).click();
  await expect(page.locator('iframe.note-view-frame')).toHaveCount(0);
  await expect(page.locator('.note-view-status')).toHaveText('Stopped.');
  await page.locator('.note-view-bar button', {hasText: 'Run'}).click();
  await expect(frame(page).locator('#out')).toContainText('"content"', {timeout: 40000});
});

test('a view cannot reach the session, storage, page, network or forms', async ({page}) => {
  const outside = await outsideServer();
  const fromFrames = [];
  page.on('requestfinished', request => {
    if (
      request.frame() !== page.mainFrame() &&
      !request.url().includes('/api/view') &&
      !request.url().includes('/static/views/')
    )
      fromFrames.push(request.url());
  });
  await login(page);
  await page.goto(origin + '/');
  await page.evaluate(() => localStorage.setItem('fmn-view-secret', 'secret'));
  await openNote(
    page,
    'views/isolation.md',
    viewNote(
      'Isolation',
      `
<pre id="out">running</pre>
<script>
const results = {};
async function attempt(name, action) {
  try {
    results[name] = String(await action());
  } catch {
    results[name] = 'blocked';
  }
}
(async () => {
  await attempt('cookie', () => (document.cookie === '' ? 'empty' : document.cookie));
  await attempt('fetch', () => fetch('/p/default/api/notes').then(response => 'status ' + response.status));
  await attempt('storage', () => localStorage.getItem('fmn-view-secret'));
  await attempt('parent', () => parent.document.title);
  await attempt('top', () => {
    top.location.href = '/?leaked=1';
    return 'asked';
  });
  await attempt('popup', () => (window.open('OUTSIDE/popup') === null ? 'null' : 'opened'));
  await attempt('form', () => {
    const form = document.createElement('form');
    form.method = 'post';
    form.action = 'OUTSIDE/form';
    document.body.append(form);
    form.submit();
    return 'asked';
  });
  await attempt(
    'image',
    () =>
      new Promise(resolve => {
        const image = new Image();
        image.onload = () => resolve('loaded');
        image.onerror = () => resolve('failed');
        image.src = 'OUTSIDE/pixel.png';
      })
  );
  document.getElementById('out').textContent = JSON.stringify(results);
})();
</script>
`.replaceAll('OUTSIDE', outside.url)
    )
  );
  await expect(frame(page).locator('#out')).toContainText('"image"');
  const results = JSON.parse(await frame(page).locator('#out').textContent());
  expect(results).toMatchObject({
    cookie: 'blocked',
    fetch: 'blocked',
    storage: 'blocked',
    parent: 'blocked',
    popup: 'null',
    image: 'failed'
  });
  expect(page.url()).not.toContain('leaked');
  expect(page.context().pages()).toHaveLength(1);
  expect(outside.hits).toEqual([]);
  expect(fromFrames).toEqual([]);
  outside.close();
});

test('a view that navigates its frame gets no session and no bridge', async ({page}) => {
  const outside = await outsideServer();
  await api('/p/default/api/note', {
    path: 'views/second.md',
    content: viewNote('Second', `<p id="second">second</p>`),
    revision: ''
  });
  // Its own navigations carry no session, and the app refuses to be framed.
  const refused = page.waitForResponse(response => response.url().includes('views%2Fsecond.md'));
  await openNote(
    page,
    'views/leaves.md',
    viewNote('Leaves', `<script>location.href = '/p/default/api/view?path=views%2Fsecond.md&block=0';</script>`)
  );
  const response = await refused;
  expect(response.status()).toBe(401);
  expect(response.request().headers().cookie).toBeUndefined();
  await expect(frame(page).locator('#second')).toHaveCount(0);

  // The app page only frames its own origin, so leaving for another is refused.
  const blocked = page.waitForEvent(
    'console',
    message => message.text().includes(outside.url) && message.text().includes('frame-src')
  );
  await openNote(
    page,
    'views/outside.md',
    viewNote(
      'Outside',
      `<p id="outside">here</p><script>setTimeout(() => { location.href = '${outside.url}/leak?from=view'; }, 0);</script>`
    )
  );
  expect((await blocked).text()).toContain('has been blocked');
  expect(outside.hits).toEqual([]);
  outside.close();
});

test('a frame that loads a second document is stopped', async ({page}) => {
  await openNote(page, 'views/reloaded.md', viewNote('Reloaded', `<p id="first">first</p>`));
  await expect(frame(page).locator('#first')).toHaveText('first');
  await page.locator('iframe.note-view-frame').evaluate(element => {
    element.src = element.src;
  });
  await expect(page.locator('.note-view-status')).toHaveText('This view left its page and was stopped.');
  await expect(page.locator('iframe.note-view-frame')).toHaveCount(0);
});

test('a view stops answering a call storm after its budget', async ({page}) => {
  await openNote(
    page,
    'views/storm.md',
    viewNote(
      'Storm',
      `
<pre id="out">running</pre>
<script>
Promise.allSettled(Array.from({length: 125}, () => fmn.theme())).then(results => {
  const refused = results.filter(result => result.status === 'rejected');
  document.getElementById('out').textContent = refused.length + ' refused: ' + (refused[0]?.reason.message || '');
});
</script>
`
    )
  );
  await expect(frame(page).locator('#out')).toHaveText('5 refused: Too many calls; wait a minute and try again.');
});

test('an edited view runs its draft in the same sandbox', async ({page}) => {
  await openNote(page, 'views/draft.md', viewNote('Draft', `<p id="state">saved</p>`));
  await expect(frame(page).locator('#state')).toHaveText('saved');
  await edit(page);
  await fillSource(page, (await editorText(page)).replace('>saved<', '>drafted<'));
  const posted = page.waitForRequest(
    request => request.method() === 'POST' && request.url().endsWith('/api/view/preview')
  );
  await page.locator('#mode').click();
  await posted;
  await expect(frame(page).locator('#state')).toHaveText('drafted');
  await expect(page.locator('.note-view-bar a', {hasText: 'Open'})).toHaveCount(0);
});
