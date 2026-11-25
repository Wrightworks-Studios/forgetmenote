import {test, expect} from '@playwright/test';
// Each worker owns an app; baseline reset makes cases independent within it.
test.describe.configure({mode: 'parallel'});
import {watchConfirmations} from './app-fixture.mjs';
import {origin, api, login, edit, documentEnd, useEditorApp} from './editor-fixture.mjs';
import {expectEditorText} from './editor-text.mjs';
useEditorApp();
// A live tab keeps only what the server does not hold in its recovery
// records: the draft record, written whole once, and the live record, which
// journals each change after it with the unacknowledged Yjs update. No edit,
// acknowledged or not, is lost when the tab crashes, closes or stays offline,
// or when the note starts a new live generation meanwhile.

const long = '# Recovered manuscript\n\n' + 'A paragraph of the long note. '.repeat(4000) + '\n';
const live = path => api('/p/default/api/live?path=' + encodeURIComponent(path));

// Opens path live in Source with the caret at its end.
async function openLive(page, path) {
  await page.goto(origin + '/?project=default&note=' + encodeURIComponent(path));
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await edit(page);
  await page.locator('#editor').press(documentEnd);
}

// Whether the tab's recovery records hold an update in flight, and text at
// the end of the note: the live record's journal folded into the draft
// record it names. A failed update writes the draft record whole and starts
// the journal again, so either may hold the text.
const recordsHold = (page, text) =>
  page.evaluate(text => {
    const records = Object.keys(localStorage).flatMap(key => {
      try {
        return [[key, JSON.parse(localStorage.getItem(key))]];
      } catch {
        return [];
      }
    });
    return records.some(
      ([key, live]) =>
        key.startsWith('gitnotes:live:') &&
        live?.pending?.operationId &&
        records.some(
          ([other, draft]) =>
            other.startsWith('gitnotes:draft:') &&
            draft?.candidateVersion === live.draft &&
            live.changes
              .reduce((note, [from, to, insert]) => note.slice(0, from) + insert + note.slice(to), draft.content)
              .endsWith(text)
        )
    );
  }, text);

// Types text acknowledged by the server, then text it never acknowledges:
// offline, every update fails; otherwise the update in flight never gets its
// answer, and the live record alone keeps what follows, since typing does not
// write the draft record. Waits until the live record holds that text, and
// returns the updates held in flight, which never reach the server unless
// the caller lets them.
async function typeAcknowledgedThenUnacknowledged(page, path, acknowledged, unacknowledged, offline = false) {
  await page.keyboard.type(acknowledged);
  await expect.poll(async () => (await live(path)).content).toBe(long + acknowledged);
  await page.evaluate(() => {
    const set = Storage.prototype.setItem;
    window.draftWrites = 0;
    Storage.prototype.setItem = function (key, value) {
      if (key.startsWith('gitnotes:draft:')) window.draftWrites++;
      return set.call(this, key, value);
    };
  });
  const held = [];
  await page.route('**/api/live', route => (offline ? route.abort() : held.push(route)));
  await page.keyboard.type(unacknowledged);
  await expect.poll(() => recordsHold(page, acknowledged + unacknowledged)).toBe(true);
  if (!offline) expect(await page.evaluate(() => window.draftWrites), 'typing wrote the draft record whole').toBe(0);
  return held;
}

// Ends the page as a crash would for its records: from here on it stores and
// sends nothing, and none of its pagehide, visibilitychange, beforeunload or
// unload handlers runs (capturing listeners run first at their target). The
// updates it held in flight are aborted, so they never reach the server,
// which would otherwise accept them whenever the page's requests end.
// Closing a page runs its scripts, even paused ones, while it is torn down:
// the aborted updates' handlers would otherwise store the draft record again,
// and teardown could cut them off between two writes. (A real renderer crash
// leaves a core file of about 1 GB in the working directory.)
async function crash(page, held) {
  const before = await page.evaluate(() => {
    for (const name of ['setItem', 'removeItem', 'clear']) Storage.prototype[name] = () => {};
    window.fetch = () => new Promise(() => {});
    const stop = event => event.stopImmediatePropagation();
    for (const type of ['pagehide', 'beforeunload', 'unload']) addEventListener(type, stop, true);
    document.addEventListener('visibilitychange', stop, true);
    return JSON.stringify(Object.entries(localStorage).sort());
  });
  await Promise.all(held.map(route => route.abort()));
  await page.close();
  // The page stored nothing more as it ended.
  const probe = await page.context().newPage();
  await probe.goto(origin + '/healthz');
  expect(await probe.evaluate(() => JSON.stringify(Object.entries(localStorage).sort()))).toBe(before);
  await probe.close();
}
// A signed-in tab of its own browser context, whose storage outlives its pages.
async function signedIn(browser) {
  const context = await browser.newContext();
  const page = await context.newPage();
  await login(page);
  return {context, page};
}

for (const end of ['crash', 'close'])
  test('a tab that ends with unacknowledged live edits recovers and shares them on ' + end, async ({browser}) => {
    const path = 'live-record-' + end + '.md';
    await api('/p/default/api/note', {path, content: long, revision: ''});
    const {context, page} = await signedIn(browser);
    await openLive(page, path);
    const held = await typeAcknowledgedThenUnacknowledged(page, path, 'Acknowledged. ', 'Never acknowledged.');
    // A peer's change the tab never received.
    const before = await live(path);
    await api('/p/default/api/live/patch', {
      path,
      base: before.liveRevision,
      operationId: 'peer-' + end,
      edits: [{start: 2, end: 2, old: '', text: 'Peer: '}]
    });
    if (end === 'crash') await crash(page, held);
    else await page.close();
    const reopened = await context.newPage();
    await openLive(reopened, path);
    const expected = '# Peer: Recovered manuscript\n\n' + long.slice(24) + 'Acknowledged. Never acknowledged.';
    await expect.poll(async () => (await live(path)).content).toBe(expected);
    await expectEditorText(reopened, expected);
    await expect(reopened.locator('#draft-conflict')).toBeHidden();
    await expect(reopened.locator('#save-state')).toContainText('Live changes synced');
    await context.close();
  });

test('unacknowledged live edits of a crashed tab stay a private draft when the note starts again', async ({
  browser
}) => {
  const path = 'live-record-generation.md';
  await api('/p/default/api/note', {path, content: long, revision: ''});
  const {context, page} = await signedIn(browser);
  await openLive(page, path);
  const held = await typeAcknowledgedThenUnacknowledged(page, path, 'Acknowledged. ', 'Never acknowledged.');
  await crash(page, held);
  // An ordinary save of the note (after it checkpoints the accepted edit)
  // starts its live document again, in a new generation.
  const committed = await api('/p/default/api/live/checkpoint', {path});
  const saved = await api('/p/default/api/note', {
    path,
    content: committed.content + '\nSaved elsewhere.\n',
    revision: committed.revision
  });
  const reopened = await context.newPage();
  await watchConfirmations(reopened, dialog => dialog.accept());
  await reopened.goto(origin + '/?project=default&note=' + encodeURIComponent(path));
  await expect(reopened.locator('#draft-conflict')).toBeVisible();
  const retained = long + 'Acknowledged. Never acknowledged.';
  await expectEditorText(reopened, retained);
  await reopened.locator('#draft-compare').click();
  await expect(reopened.locator('#reconcile-latest')).toHaveValue(saved.content);
  await expect(reopened.locator('#reconcile-content')).toHaveValue(retained);
  // Nothing private reached the new generation.
  expect((await live(path)).content).toBe(saved.content);
  await context.close();
});

test('offline typing keeps every change through a reload and sends it once when the browser is online', async ({
  page,
  context
}) => {
  const path = 'live-record-offline.md';
  await api('/p/default/api/note', {path, content: long, revision: ''});
  await login(page);
  await openLive(page, path);
  await typeAcknowledgedThenUnacknowledged(page, path, 'Online. ', 'Offline one.', true);
  // The browser goes offline: its polls fail too.
  await context.setOffline(true);
  await page.keyboard.type(' Offline two.');
  await expect.poll(() => recordsHold(page, 'Online. Offline one. Offline two.')).toBe(true);
  // A reload while updates still fail.
  await context.setOffline(false);
  await page.reload();
  await expectEditorText(page, long + 'Online. Offline one. Offline two.');
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  expect((await live(path)).content).toBe(long + 'Online. ');
  // The server can be reached again and the browser says it is online: the
  // edits go at once, rather than when the failures' backoff (1, 2, 4, 8 s)
  // next retries, and each lands once.
  await page.unroute('**/api/live');
  await context.setOffline(true);
  await context.setOffline(false);
  await expect.poll(async () => (await live(path)).content).toBe(long + 'Online. Offline one. Offline two.');
  await expect(page.locator('#draft-conflict')).toBeHidden();
  await edit(page);
  await page.locator('#editor').press(documentEnd);
  await page.keyboard.type(' Online again.');
  await expect
    .poll(async () => (await live(path)).content)
    .toBe(long + 'Online. Offline one. Offline two. Online again.');
});
