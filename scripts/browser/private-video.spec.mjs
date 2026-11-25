// A private draft (text the server does not hold yet) never loads a video
// player from an outside host until the user asks: Visual shows a
// placeholder naming the host and the link, and Load video loads that one
// video. Saved notes show their players at once. Every request to a host
// other than the app's fails here and is counted.
import {test, expect} from '@playwright/test';
import * as Y from 'yjs';
import {startApp, watchConfirmations} from './app-fixture.mjs';
import {caretAtLineEnd, editorText, expectVisual, fillSource} from './editor-text.mjs';

let app;
test.beforeAll(async () => {
  app = await startApp({
    name: 'forgetmenote-private-video-',
    account: {username: 'private-video-admin', password: 'DisposablePrivateVideo123!'}
  });
});
test.afterAll(async () => {
  await app?.close();
});

const youtube = {
    link: 'https://www.youtube.com/watch?v=M7lc1UVf-VE',
    host: 'www.youtube-nocookie.com',
    src: 'https://www.youtube-nocookie.com/embed/M7lc1UVf-VE'
  },
  vimeo = {
    link: 'https://vimeo.com/76979871',
    host: 'player.vimeo.com',
    src: 'https://player.vimeo.com/video/76979871'
  };
const videos = `[Talk](${youtube.link} "video")\n\n[Film](${vimeo.link} "video")\n`;

// Fails every request to another host; returns the hosts asked, in order.
async function blockOutside(context) {
  const hosts = [];
  await context.route(
    url => url.origin !== app.origin,
    route => {
      hosts.push(new URL(route.request().url()).hostname);
      return route.abort();
    }
  );
  return hosts;
}
const asked = hosts => [...new Set(hosts)].sort();
const placeholder = (page, video) => page.locator('#editor .cm-lp-video-held').filter({hasText: video.link});
const player = (page, video) => page.locator(`#editor iframe[src="${video.src}"]`);
const load = (page, video) => placeholder(page, video).getByRole('button', {name: 'Load video', exact: true}).click();

// Visual holds both videos, and Side by side's rendered note shows only
// their links: nothing has asked an outside host.
async function expectHeld(page, hosts, text) {
  await expectVisual(page);
  // Both video widgets are drawn, and neither made a player.
  await expect(page.locator('#editor .cm-lp-video')).toHaveCount(2);
  await expect(page.locator('#editor iframe, #editor video')).toHaveCount(0);
  expect(hosts).toEqual([]);
  for (const video of [youtube, vimeo]) await expect(placeholder(page, video)).toContainText(video.host);
  await page.locator('#format-split').click();
  await expect(page.locator('#rendered-pane a', {hasText: 'Film'})).toBeVisible();
  await expect(page.locator('#rendered-pane iframe, #rendered-pane video')).toHaveCount(0);
  await page.locator('#format-visual').click();
  await expect(placeholder(page, youtube)).toBeVisible();
  expect(hosts).toEqual([]);
  expect(await editorText(page)).toBe(text);
}

// Load video loads that video only, and it stays loaded through a mode change.
async function loadYouTube(page, hosts, text) {
  await load(page, youtube);
  await expect(player(page, youtube)).toBeVisible();
  // The player takes the button's focus.
  await expect(player(page, youtube)).toBeFocused();
  await expect.poll(() => asked(hosts)).toEqual([youtube.host]);
  await expect(placeholder(page, vimeo)).toBeVisible();
  await page.locator('#format-source').click();
  await page.locator('#format-visual').click();
  await expect(player(page, youtube)).toBeVisible();
  await expect(placeholder(page, youtube)).toHaveCount(0);
  await expect(placeholder(page, vimeo)).toBeVisible();
  expect(asked(hosts)).toEqual([youtube.host]);
  expect(await editorText(page)).toBe(text);
}
// From the keyboard: Enter on the button loads the video and types nothing.
async function loadVimeo(page, hosts, text) {
  await placeholder(page, vimeo).getByRole('button', {name: 'Load video', exact: true}).focus();
  await page.keyboard.press('Enter');
  await expect(player(page, vimeo)).toBeVisible();
  await expect(player(page, vimeo)).toBeFocused();
  await expect.poll(() => asked(hosts)).toEqual([vimeo.host, youtube.host]);
  await expect(page.locator('#editor .cm-lp-video-held')).toHaveCount(0);
  expect(await editorText(page)).toBe(text);
}

test('a new note holds its videos until each is loaded, and plays the rest once saved', async ({page, context}) => {
  const hosts = await blockOutside(context);
  await app.login(page, 'recovery');
  await page.goto(app.origin + '/?project=default');
  await page.locator('#new-note').click();
  await page.locator('#new-name').fill('new-video');
  await page.locator('#new-submit').click();
  await page.locator('#format-source').click();
  await fillSource(page, videos);
  await page.locator('#format-visual').click();
  await expectHeld(page, hosts, videos);
  await loadYouTube(page, hosts, videos);
  // Saved, the note is no longer private: its other video plays at once.
  await page.locator('#save').click();
  await expect(page.locator('#save-state')).toHaveAttribute('title', /^Saved version/);
  await expect(player(page, vimeo)).toBeVisible();
  await expect(player(page, youtube)).toBeVisible();
  await expect(page.locator('#editor .cm-lp-video-held')).toHaveCount(0);
  expect((await app.api('/p/default/api/note?path=new-video.md')).content).toBe(videos);
});

test('Load video loads the one link chosen, not another link to the same video', async ({page, context}) => {
  const hosts = await blockOutside(context);
  const text = `[First](${youtube.link} "video")\n\n[Second](${youtube.link} "video")\n`;
  await app.login(page, 'recovery');
  await page.goto(app.origin + '/?project=default');
  await page.locator('#new-note').click();
  await page.locator('#new-name').fill('video-twice');
  await page.locator('#new-submit').click();
  await page.locator('#format-source').click();
  await fillSource(page, text);
  await page.locator('#format-visual').click();
  await expectVisual(page);
  const widgets = page.locator('#editor .cm-lp-video');
  await expect(widgets).toHaveCount(2);
  await expect(page.locator('#editor .cm-lp-video-held')).toHaveCount(2);
  await widgets.nth(1).getByRole('button', {name: 'Load video', exact: true}).click();
  await expect(widgets.nth(1).locator('iframe')).toBeVisible();
  await expect(widgets.nth(0).locator('.cm-lp-video-held')).toBeVisible();
  await expect(page.locator('#editor iframe')).toHaveCount(1);
  // It stays so through a mode change.
  await page.locator('#format-source').click();
  await page.locator('#format-visual').click();
  await expect(widgets.nth(1).locator('iframe')).toBeVisible();
  await expect(widgets.nth(0).locator('.cm-lp-video-held')).toBeVisible();
  await expect(page.locator('#editor iframe')).toHaveCount(1);
  expect(asked(hosts)).toEqual([youtube.host]);
  expect(await editorText(page)).toBe(text);
});

test('a historical version holds its videos until each is loaded', async ({page, context}) => {
  const hosts = await blockOutside(context);
  const path = 'video-history.md',
    first = await app.api('/p/default/api/note', {path, content: videos, revision: ''});
  await app.api('/p/default/api/note', {path, content: '# Current, without videos\n', revision: first.revision});
  await app.login(page, 'recovery');
  await page.goto(app.origin + '/?project=default&note=' + path);
  await watchConfirmations(page, dialog => dialog.accept());
  await page.locator('#note-details-toggle').click();
  await page.locator('#history-open').click();
  await page
    .locator('#history-list button')
    .filter({hasText: first.revision.slice(0, 7)})
    .click();
  await expect(page.locator('#history-dialog')).toBeHidden();
  await page.locator('#format-visual').click();
  await expectHeld(page, hosts, videos);
  await loadYouTube(page, hosts, videos);
  await loadVimeo(page, hosts, videos);
});

test('a draft retained as private on this device holds its videos until each is loaded', async ({page, context}) => {
  const hosts = await blockOutside(context);
  const path = 'video-retained.md',
    note = await app.api('/p/default/api/note', {path, content: '# Saved, without videos\n', revision: ''}),
    user = await app.api('/api/me');
  await app.login(page, 'recovery');
  await page.goto(app.origin + '/login');
  // What the tab kept after a crash or a refused live update.
  await page.evaluate(
    ({key, note, videos}) =>
      localStorage.setItem(
        key,
        JSON.stringify({content: videos, revision: note.revision, version: note.version, private: true})
      ),
    {key: 'gitnotes:draft:' + user.id + '/default/' + path, note, videos}
  );
  await page.goto(app.origin + '/?project=default&note=' + path);
  await expect(page.locator('#save-state')).toContainText('recovered');
  await expect(page.locator('#draft-conflict')).toBeVisible();
  await page.locator('#format-visual').click();
  await expectHeld(page, hosts, videos);
  await loadYouTube(page, hosts, videos);
  await loadVimeo(page, hosts, videos);
});

test('a crashed tab’s draft that its live record does not account for holds its videos from the start', async ({
  page,
  context
}) => {
  const hosts = await blockOutside(context);
  const path = 'video-crashed.md',
    saved = '# Saved, without videos\n',
    note = await app.api('/p/default/api/note', {path, content: saved, revision: ''}),
    snapshot = await app.api('/p/default/api/live?path=' + path),
    user = await app.api('/api/me');
  await app.login(page, 'recovery');
  await page.goto(app.origin + '/login');
  // The tab kept its typing, but its live record (of the same live document)
  // never had it: the note starts again as a private draft.
  await page.evaluate(
    ({scope, note, videos, live}) => {
      localStorage.setItem(
        'gitnotes:draft:' + scope,
        JSON.stringify({content: videos, revision: note.revision, version: note.version})
      );
      localStorage.setItem('gitnotes:live:' + scope, JSON.stringify(live));
    },
    {
      scope: user.id + '/default/' + path,
      note,
      videos,
      live: {generation: snapshot.liveRevision.split(':')[0], content: saved}
    }
  );
  await page.goto(app.origin + '/?project=default&note=' + path);
  await expect(page.locator('#notice')).toContainText('private changes');
  await expect(page.locator('#draft-conflict')).toBeVisible();
  await page.locator('#format-visual').click();
  await expectHeld(page, hosts, videos);
  // Read previews it again as a private draft: links, no players.
  await page.locator('#mode').click();
  await expect(page.locator('#preview a', {hasText: 'Film'})).toBeVisible();
  await expect(page.locator('#preview iframe, #preview video')).toHaveCount(0);
  expect(hosts).toEqual([]);
  await page.locator('#mode').click();
  await page.locator('#format-visual').click();
  await loadYouTube(page, hosts, videos);
});

// A crashed tab's draft that live editing never takes: the server does not
// hold it until it is saved.
for (const [kind, live] of [
  ['that has no live record', null],
  ['whose live record is of an older live document', {generation: 'an-older-live-document', content: videos}]
])
  test(`a recovered draft ${kind} holds its videos, in Visual and in Read, until it is saved`, async ({
    page,
    context
  }) => {
    const hosts = await blockOutside(context);
    const path = live ? 'video-older-live.md' : 'video-unjoined.md',
      saved = '# Saved, without videos\n',
      note = await app.api('/p/default/api/note', {path, content: saved, revision: ''}),
      user = await app.api('/api/me');
    await app.login(page, 'recovery');
    await page.goto(app.origin + '/login');
    await page.evaluate(
      ({scope, note, videos, live}) => {
        localStorage.setItem(
          'gitnotes:draft:' + scope,
          JSON.stringify({content: videos, revision: note.revision, version: note.version})
        );
        if (live) localStorage.setItem('gitnotes:live:' + scope, JSON.stringify(live));
      },
      {scope: user.id + '/default/' + path, note, videos, live}
    );
    // Live editing reads the live note, then leaves the draft as it is.
    const decided = page.waitForResponse(response => response.url().includes('/api/live?path='));
    await page.goto(app.origin + '/?project=default&note=' + path);
    await decided;
    await expect(page.locator('#save-state')).toContainText('recovered');
    await page.waitForTimeout(500);
    await expect(page.locator('#document')).not.toHaveAttribute('data-live');
    expect((await app.api('/p/default/api/live?path=' + path)).content).toBe(saved);
    await page.locator('#format-visual').click();
    await expectHeld(page, hosts, videos);
    // Read, rendered by the server, shows the links only.
    await page.locator('#mode').click();
    await expect(page.locator('#preview a', {hasText: 'Film'})).toBeVisible();
    await expect(page.locator('#preview iframe, #preview video')).toHaveCount(0);
    expect(hosts).toEqual([]);
    await page.locator('#mode').click();
    await page.locator('#format-visual').click();
    await loadYouTube(page, hosts, videos);
    // Saved, the server holds it: every player loads, in Visual and in Read.
    await page.locator('#save').click();
    await expect(page.locator('#save-state')).toHaveAttribute('title', /^Saved version/);
    await expect(player(page, vimeo)).toBeVisible();
    await expect(page.locator('#editor .cm-lp-video-held')).toHaveCount(0);
    await page.locator('#mode').click();
    await expect(page.locator(`#preview iframe[src="${vimeo.src}"]`)).toBeVisible();
    await expect.poll(() => asked(hosts)).toEqual([vimeo.host, youtube.host]);
    expect((await app.api('/p/default/api/note?path=' + path)).content).toBe(videos);
  });

test('a recovered draft that live editing takes plays its videos once it joins', async ({page, context}) => {
  const hosts = await blockOutside(context);
  const path = 'video-rejoined.md',
    saved = '# Saved, without videos\n',
    note = await app.api('/p/default/api/note', {path, content: saved, revision: ''}),
    snapshot = await app.api('/p/default/api/live?path=' + path),
    user = await app.api('/api/me');
  // The tab's live record holds its unsent typing, so the note joins live
  // editing with it.
  const doc = new Y.Doc();
  Y.applyUpdate(doc, Buffer.from(snapshot.update, 'base64'));
  doc.getText('markdown').insert(saved.length, '\n' + videos);
  const text = saved + '\n' + videos,
    live = {
      generation: snapshot.liveRevision.split(':')[0],
      content: text,
      update: Buffer.from(Y.encodeStateAsUpdate(doc)).toString('base64')
    };
  doc.destroy();
  await app.login(page, 'recovery');
  await page.goto(app.origin + '/login');
  await page.evaluate(
    ({scope, note, text, live}) => {
      localStorage.setItem(
        'gitnotes:draft:' + scope,
        JSON.stringify({content: text, revision: note.revision, version: note.version})
      );
      localStorage.setItem('gitnotes:live:' + scope, JSON.stringify(live));
    },
    {scope: user.id + '/default/' + path, note, text, live}
  );
  // Live editing decides once it has read the live note; until then Read
  // holds the videos too.
  let release,
    gated = false;
  const gate = new Promise(resolve => (release = resolve));
  await page.route(
    url => url.pathname.endsWith('/api/live') && url.searchParams.has('path') && !url.searchParams.has('since'),
    async route => {
      gated = true;
      await gate;
      await route.continue();
    }
  );
  try {
    await page.goto(app.origin + '/?project=default&note=' + path);
    await expect(page.locator('#save-state')).toContainText('recovered');
    await expect.poll(() => gated).toBe(true);
    await page.locator('#mode').click();
    await expect(page.locator('#preview a', {hasText: 'Film'})).toBeVisible();
    await expect(page.locator('#preview iframe, #preview video')).toHaveCount(0);
    expect(hosts).toEqual([]);
    await page.locator('#mode').click();
  } finally {
    release();
  }
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await expect.poll(async () => (await app.api('/p/default/api/live?path=' + path)).content).toBe(text);
  await page.locator('#format-visual').click();
  await expectVisual(page);
  await expect(player(page, youtube)).toBeVisible();
  await expect(player(page, vimeo)).toBeVisible();
  await expect(page.locator('#editor .cm-lp-video-held')).toHaveCount(0);
  await expect.poll(() => asked(hosts)).toEqual([vimeo.host, youtube.host]);
  expect(await editorText(page)).toBe(text);
  // Read shows them now as well.
  await page.locator('#mode').click();
  await expect(page.locator(`#preview iframe[src="${youtube.src}"]`)).toBeVisible();
  await expect(page.locator(`#preview iframe[src="${vimeo.src}"]`)).toBeVisible();
});

// The server refuses the note's live updates, as it refuses one over its limits.
const refuseLiveUpdates = page =>
  page.route('**/api/live', route =>
    route.request().method() === 'POST'
      ? route.fulfill({
          status: 422,
          contentType: 'application/json',
          body: JSON.stringify({error: 'Refused live update.', code: 'live_operation_rejected'})
        })
      : route.continue()
  );

test('a note kept private after the server refuses a live update holds videos added to it', async ({page, context}) => {
  const hosts = await blockOutside(context);
  const path = 'video-refused.md',
    saved = '# Shared\n';
  await app.api('/p/default/api/note', {path, content: saved, revision: ''});
  await app.login(page, 'recovery');
  await page.goto(app.origin + '/?project=default&note=' + path);
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await page.locator('#mode').click();
  await refuseLiveUpdates(page);
  await page.locator('#format-source').click();
  await fillSource(page, saved + '\nRefused.\n');
  await expect(page.locator('#draft-conflict')).toBeVisible();
  const text = saved + '\nRefused.\n\n' + videos;
  await fillSource(page, text);
  await page.locator('#format-visual').click();
  await expectHeld(page, hosts, text);
  await loadYouTube(page, hosts, text);
  await loadVimeo(page, hosts, text);
});

test('a player showing when the note becomes private keeps playing, and videos added then are held', async ({
  page,
  context
}) => {
  const hosts = await blockOutside(context);
  const path = 'video-playing.md',
    saved = `# Shared\n\n[Talk](${youtube.link} "video")\n`;
  await app.api('/p/default/api/note', {path, content: saved, revision: ''});
  await app.login(page, 'recovery');
  await page.goto(app.origin + '/?project=default&note=' + path);
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await page.locator('#mode').click();
  await page.locator('#format-visual').click();
  await expectVisual(page);
  await expect(player(page, youtube)).toBeVisible();
  await expect.poll(() => asked(hosts)).toEqual([youtube.host]);
  // The player is playing: it must stay the same element.
  await player(page, youtube).evaluate(frame => (frame.playing = true));
  await refuseLiveUpdates(page);
  await caretAtLineEnd(page, 'Shared');
  await page.keyboard.type(' refused');
  await expect(page.locator('#draft-conflict')).toBeVisible();
  await expect(page.locator('#editor .cm-lp-video-held')).toHaveCount(0);
  expect(await player(page, youtube).evaluate(frame => frame.playing)).toBe(true);
  // A video typed into the private draft is held.
  await caretAtLineEnd(page, 'Talk');
  await page.keyboard.press('Enter');
  await page.keyboard.press('Enter');
  await page.keyboard.insertText(`[Film](${vimeo.link} "video")`);
  await expect(placeholder(page, vimeo)).toBeVisible();
  await expect(player(page, vimeo)).toHaveCount(0);
  expect(await player(page, youtube).evaluate(frame => frame.playing)).toBe(true);
  expect(asked(hosts)).toEqual([youtube.host]);
  expect(await editorText(page)).toBe(
    `# Shared refused\n\n[Talk](${youtube.link} "video")\n\n[Film](${vimeo.link} "video")\n`
  );
});

test('a saved note shows its players at once', async ({page, context}) => {
  const hosts = await blockOutside(context);
  const path = 'video-saved.md';
  await app.api('/p/default/api/note', {path, content: videos, revision: ''});
  await app.login(page, 'recovery');
  await page.goto(app.origin + '/?project=default&note=' + path);
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await page.locator('#mode').click();
  await expectVisual(page);
  await expect(player(page, youtube)).toBeVisible();
  await expect(player(page, vimeo)).toBeVisible();
  await expect(page.locator('#editor .cm-lp-video-held')).toHaveCount(0);
  await expect.poll(() => asked(hosts)).toEqual([vimeo.host, youtube.host]);
});
