import {test, expect} from '@playwright/test';
// Each worker owns an app; baseline reset makes cases independent within it.
test.describe.configure({mode: 'parallel'});
import {watchConfirmations} from './app-fixture.mjs';
import * as Y from 'yjs';
import {
  origin,
  documentEnd,
  stored,
  retainedScratch,
  retainedTexts,
  chooseHistory,
  failDraftStorage,
  openRetainedPrivate,
  api,
  login,
  edit,
  privateDraft,
  clickNoteAction,
  clickNewNote,
  navigateNote,
  shortLivePolls,
  useEditorApp
} from './editor-fixture.mjs';
import {caretAtLineEnd, editorText, expectEditorText, expectVisual, fillSource, selectSource} from './editor-text.mjs';
// Resolves when a matching request finishes or fails: a superseded navigation
// cancels its pending request instead of letting it finish.
function requestSettled(page, matches) {
  return new Promise(resolve => {
    const settle = request => {
      if (!matches(request.url())) return;
      page.off('requestfinished', settle);
      page.off('requestfailed', settle);
      resolve(request);
    };
    page.on('requestfinished', settle);
    page.on('requestfailed', settle);
  });
}
// The server refuses this tab's live updates, as it refuses an invalid one.
const refusal = 'live operation rejected: synthetic validation refusal';
const refuseLiveUpdates = page =>
  page.route('**/api/live', route =>
    route.request().method() === 'POST'
      ? route.fulfill({status: 422, json: {error: refusal, code: 'live_operation_rejected'}})
      : route.continue()
  );
useEditorApp();
test('an idle live tab cannot erase another tab private historical draft', async ({page, context}) => {
  const path = 'owned-idle.md',
    initial = '# Original checkpoint\n',
    latest = '# Current checkpoint\n',
    privateText = '# Private historical text must survive\n';
  const first = await api('/p/default/api/note', {path, content: initial, revision: ''}),
    saved = await api('/p/default/api/note', {path, content: latest, revision: first.revision});
  await login(page);
  const peer = await context.newPage();
  await shortLivePolls(peer);
  try {
    for (const p of [page, peer]) {
      await watchConfirmations(p, d => d.accept());
      await p.goto(origin + '/?project=default&note=' + path);
      await expect(p.locator('#document')).toHaveAttribute('data-live', 'connected');
    }
    await edit(page);
    await chooseHistory(page, first.revision);
    await fillSource(page, privateText);
    await page.locator('#browse-root').click();
    await expect(page.locator('#folder-browser')).toBeVisible();
    const response = await peer.waitForResponse(r => r.url().includes('/api/live?') && r.status() === 200);
    await response.finished();
    await peer.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
    expect(await retainedTexts(page, path)).toContain(privateText);
    await navigateNote(page, path);
    await edit(page);
    await expectEditorText(page, privateText);
    await page.reload();
    await edit(page);
    await expectEditorText(page, privateText);
    const committed = await api('/p/default/api/note?path=' + path);
    expect(committed.content).toBe(latest);
    expect(committed.revision).toBe(saved.revision);
    expect((await api('/p/default/api/note?path=' + path + '&revision=' + saved.revision)).content).toBe(latest);
  } finally {
    await peer.close();
  }
});
test('independent private candidates survive other tab edits Save discard and recovery choice', async ({
  page,
  context
}) => {
  const path = 'owned-independent.md',
    initial = '# First checkpoint\n',
    a = '# Private branch A\n',
    b = '# Private branch B\n',
    c = '# Independent recovered branch C\n';
  const first = await api('/p/default/api/note', {path, content: initial, revision: ''});
  await api('/p/default/api/note', {path, content: '# Current shared checkpoint\n', revision: first.revision});
  await login(page);
  const peer = await context.newPage(),
    third = await context.newPage();
  try {
    for (const p of [page, peer]) {
      await watchConfirmations(p, d => d.accept());
      await p.goto(origin + '/?project=default&note=' + path);
      await expect(p.locator('#document')).toHaveAttribute('data-live', 'connected');
      await edit(p);
    }
    await chooseHistory(page, first.revision);
    await fillSource(page, a);
    await chooseHistory(peer, first.revision);
    await fillSource(peer, b);
    for (const p of [page, peer]) {
      await p.locator('#browse-root').click();
      await expect(p.locator('#folder-browser')).toBeVisible();
    }
    expect(await retainedTexts(page, path)).toEqual(expect.arrayContaining([a, b]));
    for (const [p, content] of [
      [page, a],
      [peer, b]
    ]) {
      await p.locator('a[data-note="' + path + '"]').click();
      await edit(p);
      await expectEditorText(p, content);
      await p.reload();
      await edit(p);
      await expectEditorText(p, content);
    }
    await watchConfirmations(third, d => d.accept());
    await third.goto(origin + '/?project=default&note=' + path);
    await expect(third.locator('#draft-candidates-dialog')).toBeVisible();
    expect(
      await third.locator('#draft-candidates-list textarea').evaluateAll(inputs => inputs.map(input => input.value))
    ).toEqual(expect.arrayContaining([a, b]));
    await third.locator('#draft-candidates-list button').filter({hasText: 'Private branch A'}).click();
    await edit(third);
    await expectEditorText(third, a);
    await fillSource(third, c);
    await expect.poll(() => retainedTexts(page, path)).toEqual(expect.arrayContaining([a, b, c]));
    await page.locator('#save').click();
    await expect(page.locator('#save-state')).toContainText('Saved');
    expect((await api('/p/default/api/note?path=' + path)).content).toBe(a);
    expect(await retainedTexts(peer, path)).toEqual(expect.arrayContaining([b, c]));
    await peer.locator('#draft-compare').click();
    await expect(peer.locator('#reconcile-latest')).toHaveValue(a);
    await peer.locator('#reconcile-discard').click();
    await expect(peer.locator('#reconcile-dialog')).toBeHidden();
    expect(await retainedTexts(third, path)).toContain(c);
    await third.reload();
    await edit(third);
    await expectEditorText(third, c);
    expect((await api('/p/default/api/note?path=' + path)).content).toBe(a);
  } finally {
    await peer.close();
    await third.close();
  }
});
test('a window-open tab receives exclusive recovery ownership and independent reconciliation scratch', async ({
  page,
  context
}) => {
  const path = 'owned-cloned-session.md',
    original = '# Historical checkpoint\n',
    a = '# Private original tab\n',
    b = '# Private cloned tab\n';
  const first = await api('/p/default/api/note', {path, content: original, revision: ''});
  await api('/p/default/api/note', {path, content: '# Shared checkpoint\n', revision: first.revision});
  await login(page);
  await watchConfirmations(page, dialog => dialog.accept());
  await page.goto(origin + '/?project=default&note=' + path);
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await edit(page);
  await page.evaluate(() => sessionStorage.setItem('ownership-clone-proof', 'inherited'));
  const opened = context.waitForEvent('page');
  await page.evaluate(() => window.open(location.href, '_blank'));
  const peer = await opened;
  try {
    await watchConfirmations(peer, dialog => dialog.accept());
    await expect(peer.locator('#document')).toHaveAttribute('data-live', 'connected');
    await edit(peer);
    expect(await peer.evaluate(() => sessionStorage.getItem('ownership-clone-proof'))).toBe('inherited');
    const owner = await page.evaluate(() => sessionStorage.getItem('gitnotes:tab')),
      peerOwner = await peer.evaluate(() => sessionStorage.getItem('gitnotes:tab'));
    expect(peerOwner).not.toBe(owner);
    const user = await api('/api/me'),
      scratch = 'gitnotes:reconcile:' + user.id + '/default/' + path;
    for (const [p, text] of [
      [page, a],
      [peer, b]
    ]) {
      await chooseHistory(p, first.revision);
      await fillSource(p, text);
      await p.reload();
      await edit(p);
      await expectEditorText(p, text);
      await p.locator('#draft-compare').click();
      await p.locator('#reconcile-content').fill(text + 'Combined scratch\n');
      await p.locator('#reconcile-close').click();
    }
    expect(await stored(page, scratch)).toBe(a + 'Combined scratch\n');
    expect(await stored(peer, scratch)).toBe(b + 'Combined scratch\n');
    await page.reload();
    await edit(page);
    expect(await page.evaluate(() => sessionStorage.getItem('gitnotes:tab'))).toBe(owner);
    await expectEditorText(page, a);
    await page.locator('#draft-compare').click();
    await expect(page.locator('#reconcile-content')).toHaveValue(a + 'Combined scratch\n');
    await page.locator('#reconcile-discard').click();
    await expect(page.locator('#reconcile-dialog')).toBeHidden();
    expect(await retainedTexts(peer, path)).toContain(b);
    expect(await stored(peer, scratch)).toBe(b + 'Combined scratch\n');
    await peer.reload();
    await edit(peer);
    await expectEditorText(peer, b);
    await peer.locator('#draft-compare').click();
    await expect(peer.locator('#reconcile-content')).toHaveValue(b + 'Combined scratch\n');
    expect((await api('/p/default/api/note?path=' + path)).content).toBe('# Shared checkpoint\n');
  } finally {
    await peer.close();
  }
});
test('identical private drafts keep both reconciliation variants selectable after their tabs close', async ({
  page,
  context
}) => {
  const path = 'owned-scratch-variants.md',
    privateText = '# Identical private starting text\n',
    a = '# Reconciliation variant A\n',
    b = '# Reconciliation variant B\n',
    newer = a + 'Newer unapplied edits\n';
  const first = await api('/p/default/api/note', {path, content: '# Historical checkpoint\n', revision: ''}),
    committed = await api('/p/default/api/note', {path, content: '# Current checkpoint\n', revision: first.revision});
  await login(page);
  const peer = await context.newPage(),
    fresh = await context.newPage(),
    user = await api('/api/me'),
    scratch = 'gitnotes:reconcile:' + user.id + '/default/' + path;
  const select = async text => {
    await expect(fresh.locator('#draft-candidates-dialog')).toBeVisible();
    const choices = fresh.locator('#draft-candidates-list section'),
      values = await choices.evaluateAll(rows =>
        rows.map(row => row.querySelector('[aria-label="Retained reconciliation text"]')?.value)
      );
    expect(values).toContain(text);
    await choices.nth(values.indexOf(text)).locator('button').click();
  };
  try {
    for (const p of [page, peer]) {
      await watchConfirmations(p, dialog => dialog.accept());
      await p.goto(origin + '/?project=default&note=' + path);
      await expect(p.locator('#document')).toHaveAttribute('data-live', 'connected');
      await edit(p);
    }
    for (const [p, text] of [
      [page, a],
      [peer, b]
    ]) {
      await chooseHistory(p, first.revision);
      await fillSource(p, privateText);
      await p.reload();
      await edit(p);
      await p.locator('#draft-compare').click();
      await p.locator('#reconcile-content').fill(text);
      await p.locator('#reconcile-close').click();
    }
    await page.close();
    await peer.close();
    await watchConfirmations(fresh, dialog => dialog.accept());
    await fresh.goto(origin + '/?project=default&note=' + path);
    await expect(fresh.locator('#draft-candidates-dialog')).toBeVisible();
    await select(a);
    await edit(fresh);
    await expectEditorText(fresh, privateText);
    await fresh.locator('#draft-compare').click();
    await expect(fresh.locator('#reconcile-content')).toHaveValue(a);
    await fresh.locator('#reconcile-content').fill(newer);
    await fresh.locator('#reconcile-close').click();
    await fresh.locator('#draft-candidates-open').click();
    await expect(fresh.locator('#draft-candidates-dialog')).toBeVisible();
    await fresh.evaluate(() => {
      const set = Storage.prototype.setItem;
      window.restoreArchiveStorage = () => (Storage.prototype.setItem = set);
      Storage.prototype.setItem = function (key, value) {
        if (key.startsWith('gitnotes:draft:@' + sessionStorage.getItem('gitnotes:tab') + '.'))
          throw new DOMException('Synthetic scratch archive quota', 'QuotaExceededError');
        return set.call(this, key, value);
      };
    });
    await select(b);
    await expect(fresh.locator('#notice')).toContainText('Synthetic scratch archive quota');
    expect(await stored(fresh, scratch)).toBe(newer);
    await expectEditorText(fresh, privateText);
    await fresh.evaluate(() => window.restoreArchiveStorage());
    await fresh.locator('#draft-candidates-open').click();
    await select(b);
    await expect(fresh.locator('#save-state')).toContainText('Unsaved on this device · recovered');
    await fresh.locator('#draft-compare').click();
    await expect(fresh.locator('#reconcile-content')).toHaveValue(b);
    await fresh.locator('#reconcile-close').click();
    await fresh.reload();
    await edit(fresh);
    await fresh.locator('#draft-compare').click();
    await expect(fresh.locator('#reconcile-content')).toHaveValue(b);
    await fresh.locator('#reconcile-close').click();
    await fresh.locator('#draft-candidates-open').click();
    await select(newer);
    await fresh.reload();
    await edit(fresh);
    await fresh.locator('#draft-compare').click();
    await expect(fresh.locator('#reconcile-content')).toHaveValue(newer);
    await fresh.locator('#reconcile-close').click();
    expect(await retainedScratch(fresh, path)).toEqual(expect.arrayContaining([a, b, newer]));
    const saved = await api('/p/default/api/note?path=' + path);
    expect(saved.content).toBe(committed.content);
    expect(saved.revision).toBe(committed.revision);
    expect((await api('/p/default/api/note?path=' + path + '&revision=' + saved.revision)).content).toBe(
      committed.content
    );
  } finally {
    await peer.close().catch(() => {});
    await fresh.close();
  }
});
for (const resolutionFails of [false, true])
  test('borrowed discard is durable with session metadata unavailable ' + resolutionFails, async ({page, context}) => {
    const path = 'durable-borrowed-discard-' + resolutionFails + '.md',
      initial = '# Base\n',
      discarded = initial + '\nDiscarded borrowed operation\n';
    await api('/p/default/api/note', {path, content: initial, revision: ''});
    await login(page);
    await watchConfirmations(page, dialog => dialog.accept());
    await page.goto(origin + '/?project=default&note=' + path);
    await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
    await edit(page);
    await page.route('**/api/live', route => route.abort());
    await fillSource(page, discarded);
    const user = await api('/api/me'),
      scope = user.id + '/default/' + path,
      key = 'gitnotes:live:' + scope;
    await expect.poll(async () => JSON.parse(await stored(page, key))?.pending?.operationId).toBeTruthy();
    const sourceOwner = await page.evaluate(() => sessionStorage.getItem('gitnotes:tab')),
      sourcePacket = JSON.parse(await stored(page, key));
    await page.close();
    const base = await api('/p/default/api/live?path=' + path);
    await api('/p/default/api/live/patch', {
      path,
      base: base.liveRevision,
      operationId: 'durable-discard-peer-' + resolutionFails,
      edits: [{start: 2, end: 6, old: 'Base', text: 'Peer'}]
    });
    await api('/p/default/api/live/checkpoint', {path});
    const shared = await api('/p/default/api/note?path=' + path);
    const recovered = await context.newPage();
    await watchConfirmations(recovered, dialog => dialog.accept());
    await shortLivePolls(recovered);
    try {
      await recovered.route('**/api/live', route => route.abort());
      await recovered.goto(origin + '/?project=default&note=' + path);
      await expect(recovered.locator('#document')).toHaveAttribute('data-live', 'connected');
      await recovered.locator('#draft-compare').click();
      await expect(recovered.locator('#reconcile-dialog')).toBeVisible();
      const before = await editorText(recovered);
      await recovered.evaluate(fail => {
        window.failResolution = fail;
        const set = Storage.prototype.setItem;
        Storage.prototype.setItem = function (key, value) {
          if (this === sessionStorage && key.startsWith('gitnotes:recovery:'))
            throw new DOMException('Synthetic session quota', 'QuotaExceededError');
          if (
            window.failResolution &&
            key.startsWith('gitnotes:draft:@' + sessionStorage.getItem('gitnotes:tab') + '/')
          ) {
            let record;
            try {
              record = JSON.parse(value);
            } catch {}
            if (record?.recovery?.origin === null && Object.keys(record.recovery.ignored).length)
              throw new DOMException('Synthetic resolution quota', 'QuotaExceededError');
          }
          return set.call(this, key, value);
        };
      }, resolutionFails);
      await recovered.locator('#reconcile-discard').click();
      if (resolutionFails) {
        await expect(recovered.locator('#reconcile-error')).toContainText('Synthetic resolution quota');
        await expect(recovered.locator('#reconcile-dialog')).toBeVisible();
        await expectEditorText(recovered, before);
        expect(JSON.parse(await stored(recovered, 'gitnotes:draft:' + scope)).content).toBe(before);
        await recovered.evaluate(() => (window.failResolution = false));
        await recovered.locator('#reconcile-discard').click();
      }
      await expect(recovered.locator('#reconcile-dialog')).toBeHidden();
      await expectEditorText(recovered, shared.content);
      const sent = [];
      recovered.on('request', request => {
        if (request.method() === 'POST' && request.url().endsWith('/api/live')) sent.push(request.postDataJSON());
      });
      await recovered.unroute('**/api/live');
      await recovered.reload();
      await expect(recovered.locator('#document')).toHaveAttribute('data-live', 'connected');
      const poll = await recovered.waitForResponse(
        response => response.url().includes('/api/live?') && response.url().includes('since=') && response.ok()
      );
      await poll.finished();
      expect(sent).toEqual([]);
      await expectEditorText(recovered, shared.content);
      expect((await api('/p/default/api/live?path=' + path)).content).toBe(shared.content);
      await api('/p/default/api/live/checkpoint', {path});
      const final = await api('/p/default/api/note?path=' + path);
      expect(final.content).toBe(shared.content);
      expect((await api('/p/default/api/note?path=' + path + '&revision=' + final.revision)).content).toBe(
        shared.content
      );
      expect(
        await recovered.evaluate(
          ({scope, sourceOwner}) => JSON.parse(localStorage.getItem('gitnotes:live:@' + sourceOwner + '/' + scope)),
          {scope, sourceOwner}
        )
      ).toEqual(sourcePacket);
    } finally {
      await recovered.close();
    }
  });
test('late candidate publication failure keeps the complete prior state and exposes no hybrid', async ({
  page,
  context
}) => {
  const path = 'atomic-candidate-adoption.md',
    own = '00000000-0000-4000-8000-000000000021',
    peer = '00000000-0000-4000-8000-000000000022',
    a = '# Private A\n',
    b = '# Private B\n';
  const saved = await api('/p/default/api/note', {path, content: '# Committed\n', revision: ''}),
    user = await api('/api/me'),
    scope = user.id + '/default/' + path;
  await login(page);
  await page.goto(origin + '/login');
  await page.evaluate(
    ({scope, saved, own, peer, a, b}) => {
      sessionStorage.setItem('gitnotes:tab', own);
      for (const [owner, text, scratch] of [
        [own, a, 'Combined A'],
        [peer, b, 'Combined B']
      ]) {
        localStorage.setItem(
          'gitnotes:draft:@' + owner + '/' + scope,
          JSON.stringify({content: text, revision: saved.revision, version: saved.version, private: true})
        );
        localStorage.setItem('gitnotes:reconcile:@' + owner + '/' + scope, scratch);
      }
    },
    {scope, saved, own, peer, a, b}
  );
  await watchConfirmations(page, dialog => dialog.accept());
  await page.goto(origin + '/?project=default&note=' + path);
  await edit(page);
  await page.evaluate(scope => {
    const set = Storage.prototype.setItem,
      remove = Storage.prototype.removeItem,
      owner = sessionStorage.getItem('gitnotes:tab');
    window.archiveWritten = false;
    window.failedPublications = 0;
    window.restorePublication = () => {
      Storage.prototype.setItem = set;
      Storage.prototype.removeItem = remove;
    };
    Storage.prototype.setItem = function (key, value) {
      if (key.startsWith('gitnotes:draft:@' + owner + '.')) window.archiveWritten = true;
      let record;
      try {
        record = JSON.parse(value);
      } catch {}
      if (
        key === 'gitnotes:reconcile:@' + owner + '/' + scope ||
        (window.archiveWritten && key === 'gitnotes:draft:@' + owner + '/' + scope && record?.recovery?.schema === 1)
      ) {
        window.failedPublications++;
        throw new DOMException('Synthetic late publication quota', 'QuotaExceededError');
      }
      return set.call(this, key, value);
    };
    Storage.prototype.removeItem = function (key) {
      if (window.archiveWritten) throw new DOMException('Synthetic rollback unavailable', 'QuotaExceededError');
      return remove.call(this, key);
    };
  }, scope);
  await page.locator('#draft-candidates-open').click();
  await page.locator('#draft-candidates-list button').filter({hasText: 'Private B'}).click();
  await expect(page.locator('#notice')).toContainText('Synthetic late publication quota');
  expect(await page.evaluate(() => window.failedPublications)).toBe(1);
  await expectEditorText(page, a);
  expect(await stored(page, 'gitnotes:reconcile:' + scope)).toBe('Combined A');
  const fresh = await context.newPage();
  try {
    await fresh.goto(origin + '/?project=default&note=' + path);
    await expect(fresh.locator('#draft-candidates-dialog')).toBeVisible();
    const choices = await fresh.locator('#draft-candidates-list section').evaluateAll(rows =>
      rows.map(row => ({
        text: row.querySelector('[aria-label="Retained draft content"]').value,
        scratch: row.querySelector('[aria-label="Retained reconciliation text"]')?.value
      }))
    );
    expect(choices).toEqual(
      expect.arrayContaining([
        {text: a, scratch: 'Combined A'},
        {text: b, scratch: 'Combined B'}
      ])
    );
    expect(choices).not.toContainEqual({text: b, scratch: 'Combined A'});
  } finally {
    await fresh.close();
  }
  await page.evaluate(() => window.restorePublication());
  await page.locator('#draft-candidates-open').click();
  await page.locator('#draft-candidates-list button').filter({hasText: 'Private B'}).click();
  await expectEditorText(page, b);
  await page.locator('#draft-compare').click();
  await expect(page.locator('#reconcile-content')).toHaveValue('Combined B');
  expect((await api('/p/default/api/note?path=' + path)).revision).toBe(saved.revision);
});
test('archived borrowed replay retries the identical operation after candidate switching with live quota', async ({
  page
}) => {
  const path = 'archived-borrowed-replay.md',
    own = '00000000-0000-4000-8000-000000000031',
    source = '00000000-0000-4000-8000-000000000032',
    other = '00000000-0000-4000-8000-000000000033';
  const saved = await api('/p/default/api/note', {path, content: '# Shared base\n', revision: ''}),
    snapshot = await api('/p/default/api/live?path=' + path),
    user = await api('/api/me'),
    scope = user.id + '/default/' + path;
  const doc = new Y.Doc();
  Y.applyUpdate(doc, Buffer.from(snapshot.update, 'base64'));
  doc.getText('markdown').insert(doc.getText('markdown').length, 'Uncertain operation B\n');
  const text = doc.getText('markdown').toString(),
    update = Buffer.from(Y.encodeStateAsUpdate(doc)).toString('base64'),
    generation = snapshot.liveRevision.split(':')[0],
    pending = {path, generation, operationId: 'archived-original-operation', update},
    recovery = {generation, content: text, update, pending};
  doc.destroy();
  await login(page);
  await page.goto(origin + '/login');
  await page.evaluate(
    ({scope, saved, own, source, other, text, recovery}) => {
      sessionStorage.setItem('gitnotes:tab', own);
      localStorage.setItem(
        'gitnotes:draft:@' + source + '/' + scope,
        JSON.stringify({content: text, revision: saved.revision, version: saved.version, private: false})
      );
      localStorage.setItem('gitnotes:live:@' + source + '/' + scope, JSON.stringify(recovery));
      localStorage.setItem(
        'gitnotes:draft:@' + other + '/' + scope,
        JSON.stringify({
          content: '# Separate candidate C\n',
          revision: saved.revision,
          version: saved.version,
          private: true
        })
      );
    },
    {scope, saved, own, source, other, text, recovery}
  );
  await page.addInitScript(own => {
    const set = Storage.prototype.setItem;
    Storage.prototype.setItem = function (key, value) {
      if (key.startsWith('gitnotes:live:@' + own) && value !== 'null')
        throw new DOMException('Synthetic live-state quota', 'QuotaExceededError');
      return set.call(this, key, value);
    };
  }, own);
  const attempts = [];
  await page.route('**/api/live', async route => {
    attempts.push(route.request().postDataJSON());
    await route.abort();
  });
  await watchConfirmations(page, dialog => dialog.accept());
  await page.goto(origin + '/?project=default&note=' + path);
  await page.locator('#draft-candidates-list button').filter({hasText: 'Uncertain operation B'}).click();
  await expect.poll(() => attempts.length).toBeGreaterThan(0);
  await page.locator('#draft-candidates-open').click();
  await page.locator('#draft-candidates-list button').filter({hasText: 'Separate candidate C'}).click();
  await expect(page.locator('#save-state')).toContainText('Unsaved on this device · recovered');
  const before = attempts.length;
  await page.locator('#draft-candidates-open').click();
  await page.locator('#draft-candidates-list button').filter({hasText: 'Uncertain operation B'}).click();
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await expect.poll(() => attempts.length).toBeGreaterThan(before);
  expect(attempts.every(attempt => JSON.stringify(attempt) === JSON.stringify(pending))).toBe(true);
  expect(
    await page.evaluate(
      ({scope, source}) => JSON.parse(localStorage.getItem('gitnotes:live:@' + source + '/' + scope)),
      {scope, source}
    )
  ).toEqual(recovery);
  await page.unroute('**/api/live');
  await expect.poll(async () => (await api('/p/default/api/live?path=' + path)).content).toBe(text);
  await page.locator('#save').click();
  await expect(page.locator('#save-state')).toContainText('Saved');
  const committed = await api('/p/default/api/note?path=' + path);
  expect(committed.content).toBe(text);
  expect((await api('/p/default/api/note?path=' + path + '&revision=' + committed.revision)).content).toBe(text);
});
test('Apply stops live editing reconnected by CtrlS during Compare and preserves the combined draft', async ({
  page
}) => {
  const path = 'apply-after-shortcut.md',
    initial = '# Shared base\n',
    privateText = '# Private draft\n',
    combined = '# Combined choice must survive\n';
  const note = await api('/p/default/api/note', {path, content: initial, revision: ''}),
    user = await api('/api/me');
  await login(page);
  await page.goto(origin + '/login');
  await watchConfirmations(page, dialog => dialog.accept());
  await page.evaluate(
    ({scope, note, privateText}) =>
      localStorage.setItem(
        'gitnotes:draft:' + scope,
        JSON.stringify({content: privateText, revision: note.revision, version: note.version, private: true})
      ),
    {scope: user.id + '/default/' + path, note, privateText}
  );
  await page.goto(origin + '/?project=default&note=' + path);
  await expect(page.locator('#save-state')).toContainText('Unsaved on this device · recovered');
  await page.locator('#format-source').click();
  await expectEditorText(page, privateText);
  await page.locator('#draft-compare').click();
  await expect(page.locator('#reconcile-dialog')).toBeVisible();
  await page.locator('#reconcile-content').fill(combined);
  await page.keyboard.press('Control+s');
  await expect(page.locator('#save-state')).toContainText('Saved');
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await page.locator('#reconcile-apply').click();
  await expect(page.locator('#reconcile-error')).toContainText('shared note changed again');
  await page.locator('#reconcile-apply').click();
  await expect(page.locator('#reconcile-dialog')).toBeHidden();
  await expectEditorText(page, combined);
  const latest = await api('/p/default/api/live?path=' + path);
  await api('/p/default/api/live/patch', {
    path,
    base: latest.liveRevision,
    operationId: 'apply-after-shortcut-peer',
    edits: [{start: 2, end: 9, old: 'Private', text: 'Peer'}]
  });
  await page.waitForTimeout(1600);
  await expectEditorText(page, combined);
  await expect(page.locator('#document')).not.toHaveAttribute('data-live', 'connected');
  expect(await retainedTexts(page, path)).toContain(combined);
});
test('typed live rejection retains current newer text and reconciliation includes accepted peer text', async ({
  page
}) => {
  const path = 'typed-rejection-current.md',
    initial = '# Initial shared text\n',
    first = initial + 'First local edit\n',
    newer = first + 'Newer edit while sending\n';
  await api('/p/default/api/note', {path, content: initial, revision: ''});
  await login(page);
  let release,
    retire,
    requests = 0;
  const gate = new Promise(resolve => (release = resolve)),
    retired = new Promise(resolve => (retire = resolve));
  // The server accepts the peer patch before the rejection, but this tab must not
  // read it first: a long-poll opened by the session's first poll, 750 ms after it
  // starts, would return the patch and the editor would merge it before the 422.
  const poll = url => url.pathname.endsWith('/api/live') && url.searchParams.has('since');
  await page.route(poll, async route => {
    await retired;
    await route.continue();
  });
  try {
    await page.goto(origin + '/?project=default&note=' + path);
    await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
    await edit(page);
    await page.route('**/api/live', async route => {
      requests++;
      await gate;
      await route.fulfill({
        status: 422,
        json: {code: 'live_operation_rejected', error: 'Synthetic deterministic validation rejected the operation'}
      });
    });
    await fillSource(page, first);
    await expect.poll(() => requests).toBe(1);
    await fillSource(page, newer);
    const base = await api('/p/default/api/live?path=' + path),
      peer = await api('/p/default/api/live/patch', {
        path,
        base: base.liveRevision,
        operationId: 'typed-rejection-peer',
        edits: [{start: 2, end: 9, old: 'Initial', text: 'Accepted peer'}]
      });
    release();
    await expect(page.locator('#draft-compare')).toBeVisible();
    await expect(page.locator('#document')).not.toHaveAttribute('data-live', 'connected');
    // The retired session aborted its read; a late answer must not change the draft.
    retire();
    await expectEditorText(page, newer);
    expect(await retainedTexts(page, path)).toContain(newer);
    await page.reload();
    await expectEditorText(page, newer);
    await expect(page.locator('#draft-compare')).toBeVisible();
    expect(requests).toBe(1);
    await page.locator('#draft-compare').click();
    await expect(page.locator('#reconcile-content')).toHaveValue(newer);
    await expect(page.locator('#reconcile-latest')).toHaveValue(peer.content);
    expect((await api('/p/default/api/live?path=' + path)).content).toBe(peer.content);
  } finally {
    release();
    retire();
  }
});
for (const destination of ['note', 'folder'])
  for (const timing of ['before', 'pending input', 'pending composition'])
    test('private ' + destination + ' navigation requires current retention ' + timing, async ({page}) => {
      await watchConfirmations(page, dialog => dialog.accept());
      const suffix = destination + '-' + timing.replaceAll(' ', '-'),
        path = 'retention-navigation-' + suffix + '.md',
        other = 'retention-destination-' + suffix + '.md',
        newer = '# Newer private text after navigation started\n';
      await api('/p/default/api/note', {path: other, content: '# Destination\n', revision: ''});
      const {initial, prior, note, key} = await openRetainedPrivate(page, path),
        destinationURL = destination === 'note' ? '**/api/note?path=' + other : '**/api/browse?*';
      let release,
        requests = 0;
      const gate = new Promise(resolve => (release = resolve));
      await page.route(destinationURL, async route => {
        requests++;
        await gate;
        await route.continue();
      });
      const navigate = () =>
        destination === 'note' ? navigateNote(page, other) : page.locator('#browse-root').click();
      try {
        if (timing !== 'before') {
          await navigate();
          await expect.poll(() => requests).toBe(1);
        }
        await failDraftStorage(page);
        if (timing === 'pending composition') await page.locator('#editor').dispatchEvent('compositionstart');
        await fillSource(page, newer);
        await expect(page.locator('#save-state')).toContainText('keep this tab open');
        if (timing === 'before') await navigate();
        release();
        // The second failed write is the destination's retention boundary, not a timer.
        await expect.poll(() => page.evaluate(() => window.draftWriteFailures)).toBeGreaterThanOrEqual(2);
        await expect(page.locator('#note-path')).toHaveText(path);
        await expect(page.locator('#document')).toBeVisible();
        await expectEditorText(page, newer);
        await expect(page.locator('#notice')).toContainText('Keep this tab open');
        expect(requests).toBe(timing === 'before' ? 0 : 1);
        expect(JSON.parse(await stored(page, key))).toMatchObject({content: prior, private: true});
        const saved = await api('/p/default/api/note?path=' + path);
        expect(saved.content).toBe(initial);
        expect(saved.revision).toBe(note.revision);
        expect((await api('/p/default/api/live?path=' + path)).content).toBe(initial);
        if (timing === 'pending composition') await page.locator('#editor').dispatchEvent('compositionend');
        await page.evaluate(() => window.restoreDraftStorage());
        await navigate();
        if (destination === 'note') await expect(page.locator('#note-path')).toHaveText(other);
        else await expect(page.locator('#folder-browser')).toBeVisible();
        expect(JSON.parse(await stored(page, key))).toMatchObject({content: newer, private: true});
        await navigateNote(page, path);
        await edit(page);
        await expectEditorText(page, newer);
        expect((await api('/p/default/api/note?path=' + path)).content).toBe(initial);
      } finally {
        release();
      }
    });
test('project switching requires retention of the current private draft', async ({page}) => {
  await watchConfirmations(page, dialog => dialog.accept());
  await api('/api/projects', {id: 'retention-other', name: 'Retention Other'});
  const path = 'retention-project.md',
    newer = '# Newer private project draft\n',
    {key} = await openRetainedPrivate(page, path);
  await failDraftStorage(page);
  await fillSource(page, newer);
  await page.locator('#project-select').selectOption('retention-other');
  await expect(page.locator('#project-select')).toHaveValue('default');
  await expect(page.locator('#note-path')).toHaveText(path);
  await expectEditorText(page, newer);
  await expect(page.locator('#notice')).toContainText('Keep this tab open');
  await page.evaluate(() => window.restoreDraftStorage());
  await page.locator('#project-select').selectOption('retention-other');
  await expect(page.locator('#folder-browser')).toBeVisible();
  expect(JSON.parse(await stored(page, key))).toMatchObject({content: newer, private: true});
});
test('same-note navigation recovers the current text retained at installation', async ({page}) => {
  await watchConfirmations(page, dialog => dialog.accept());
  const path = 'retention-same-note.md',
    newer = '# Newer private text after navigation started\n',
    {initial, key} = await openRetainedPrivate(page, path);
  let release,
    waiting = false;
  const gate = new Promise(resolve => (release = resolve));
  await page.route('**/api/note?path=' + path, async route => {
    waiting = true;
    await gate;
    await route.continue();
  });
  try {
    await navigateNote(page, path);
    await expect.poll(() => waiting).toBe(true);
    await failDraftStorage(page);
    await fillSource(page, newer);
    await expect(page.locator('#save-state')).toContainText('keep this tab open');
    await page.evaluate(() => window.restoreDraftStorage());
    release();
    await expect(page.locator('#save-state')).toContainText('Unsaved on this device · recovered');
    await expectEditorText(page, newer);
    expect(JSON.parse(await stored(page, key))).toMatchObject({content: newer, private: true});
    expect((await api('/p/default/api/note?path=' + path)).content).toBe(initial);
  } finally {
    release();
  }
});
test('pending folder navigation waits for native visual composition to finish', async ({page, context}) => {
  await watchConfirmations(page, dialog => dialog.accept());
  const path = 'retention-native-composition.md',
    initial = '# Composing\n\nKeep this paragraph.\n';
  await api('/p/default/api/note', {path, content: initial, revision: ''});
  await login(page);
  await page.goto(origin + '/?project=default&note=' + path);
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await edit(page);
  await page.locator('#format-visual').click();
  let release,
    waiting = false;
  const gate = new Promise(resolve => (release = resolve));
  await page.route('**/api/browse?*', async route => {
    waiting = true;
    await gate;
    await route.continue();
  });
  try {
    await page.locator('#browse-root').click();
    await expect.poll(() => waiting).toBe(true);
    const editor = page.locator('#editor');
    await caretAtLineEnd(page, 'Keep this paragraph.');
    const cdp = await context.newCDPSession(page);
    await cdp.send('Input.imeSetComposition', {text: '日本', selectionStart: 2, selectionEnd: 2});
    release();
    await expect(page.locator('#notice')).toContainText('Finish composing');
    await expect(page.locator('#document')).toBeVisible();
    await expect(editor).toContainText('日本');
    await expect(page.locator('#folder-browser')).toBeHidden();
    await cdp.send('Input.insertText', {text: '日本語'});
    await expect(editor).toContainText('日本語');
    await page.locator('#save').click();
    await expect(page.locator('#save-state')).toContainText('Saved');
    expect((await api('/p/default/api/note?path=' + path)).content).toContain('Keep this paragraph.日本語');
    await page.locator('#browse-root').click();
    await expect(page.locator('#folder-browser')).toBeVisible();
  } finally {
    release();
  }
});
test('save completion is shown only when navigation is available again', async ({page}) => {
  const path = 'save-navigation-ready.md';
  await api('/p/default/api/note', {path, content: '# Save navigation\n', revision: ''});
  await login(page);
  await page.goto(origin + '/?project=default&note=' + path);
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await edit(page);
  await fillSource(page, '# Save navigation\n\nA new paragraph.\n');
  let release, entered;
  const held = new Promise(resolve => {
      release = resolve;
    }),
    started = new Promise(resolve => {
      entered = resolve;
    });
  await page.route('**/api/status', async route => {
    entered();
    await held;
    await route.continue();
  });
  try {
    await page.locator('#save').click();
    await started;
    await expect(page.locator('#save-state')).not.toContainText('Saved');
    release();
    await expect(page.locator('#save-state')).toContainText('Saved');
    await page.locator('#browse-root').click();
    await expect(page.locator('#folder-browser')).toBeVisible();
    expect((await api('/p/default/api/note?path=' + path)).content).toContain('A new paragraph.');
  } finally {
    release();
  }
});
test('new draft installation requires retention of the current private draft', async ({page}) => {
  const path = 'retention-creation.md',
    newer = '# Newer private creation draft\n',
    {key} = await openRetainedPrivate(page, path);
  await failDraftStorage(page);
  await fillSource(page, newer);
  await clickNewNote(page);
  await page.locator('#new-name').fill('retention-new');
  await page.locator('#new-submit').click();
  await expect(page.locator('#new-error')).toContainText('Keep this tab open');
  await expect(page.locator('#note-path')).toHaveText(path);
  await expectEditorText(page, newer);
  await expect(page.locator('#new-dialog')).toBeVisible();
  await page.evaluate(() => window.restoreDraftStorage());
  await page.locator('#new-submit').click();
  await expect(page.locator('#new-dialog')).toBeHidden();
  await expect(page.locator('#note-path')).toHaveText('retention-new.md');
  expect(JSON.parse(await stored(page, key))).toMatchObject({content: newer, private: true});
});
test('native visual IME merges a disjoint AI patch and reaches Git', async ({page, context}) => {
  const path = 'native-ime.md',
    initial = '# Native IME\n\nAlpha paragraph.\n\nLocal paragraph.\n';
  await api('/p/default/api/note', {path, content: initial, revision: ''});
  await login(page);
  await page.goto(origin + '/?project=default&note=' + path);
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await edit(page);
  await page.locator('#format-visual').click();
  const editor = page.locator('#editor');
  await expectVisual(page);
  await caretAtLineEnd(page, 'Local paragraph.');
  await expect
    .poll(() =>
      page.evaluate(() => {
        const selection = getSelection();
        return [selection.focusNode?.textContent, selection.focusOffset];
      })
    )
    .toEqual(['Local paragraph.', 16]);
  const cdp = await context.newCDPSession(page);
  await cdp.send('Input.imeSetComposition', {text: '日本', selectionStart: 2, selectionEnd: 2});
  const before = await api('/p/default/api/live?path=' + path),
    start = before.content.indexOf('Alpha');
  await api('/p/default/api/live/patch', {
    path,
    base: before.liveRevision,
    operationId: 'native-ime-peer',
    edits: [{start, end: start + 5, old: 'Alpha', text: 'Accepted AI'}]
  });
  await expect.poll(async () => (await api('/p/default/api/live?path=' + path)).content).toContain('Accepted AI');
  await cdp.send('Input.insertText', {text: '日本語'});
  await expect(editor).toContainText('日本語');
  await expect(editor).toContainText('Accepted AI');
  await page.locator('#save').click();
  await expect(page.locator('#save-state')).toContainText('Saved');
  const committed = await api('/p/default/api/note?path=' + path);
  expect(committed.content).toContain('Accepted AI paragraph.');
  expect(committed.content).toContain('Local paragraph.日本語');
  expect((await api('/p/default/api/note?path=' + path + '&revision=' + committed.revision)).content).toBe(
    committed.content
  );
});
// The order a loaded machine produces at random, forced: the tab holds the
// AI patch, deferred by the open composition, before the composition ends.
// Chrome ends it before the editor's DOM observer reads its last text, so
// the deferred patch must not reach the editor until that text has.
for (const format of ['visual', 'source'])
  test('native ' + format + ' IME keeps its committed text when an AI patch waited for it', async ({page, context}) => {
    const path = 'native-ime-deferred-' + format + '.md',
      initial = '# Native IME\n\nAlpha paragraph.\n\nLocal paragraph.\n',
      final = '# Native IME\n\nAccepted AI paragraph.\n\nLocal paragraph.日本語\n';
    await api('/p/default/api/note', {path, content: initial, revision: ''});
    await login(page);
    await page.goto(origin + '/?project=default&note=' + path);
    await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
    await edit(page);
    await page.locator('#format-' + format).click();
    await expect(page.locator('#editing')).toHaveAttribute('data-format', format);
    const editor = page.locator('#editor');
    await caretAtLineEnd(page, 'Local paragraph.');
    // Live reads in the order the browser finished and sent them: one sent
    // after another finished means the tab has handled the first's answer.
    const reads = [],
      liveRead = request => request.method() === 'GET' && /\/api\/live\?path=[^&]+&since=/.test(request.url());
    page.on('requestfinished', request => liveRead(request) && reads.push(['finished', request]));
    page.on('request', request => liveRead(request) && reads.push(['sent', request]));
    const cdp = await context.newCDPSession(page);
    await cdp.send('Input.imeSetComposition', {text: '日本', selectionStart: 2, selectionEnd: 2});
    await expect(editor).toContainText('Local paragraph.日本');
    const before = await api('/p/default/api/live?path=' + path),
      start = before.content.indexOf('Alpha');
    const patched = await api('/p/default/api/live/patch', {
      path,
      base: before.liveRevision,
      operationId: 'native-ime-deferred-' + format,
      edits: [{start, end: start + 5, old: 'Alpha', text: 'Accepted AI'}]
    });
    await expect
      .poll(async () => {
        for (const [index, [kind, request]] of reads.entries()) {
          if (kind !== 'finished') continue;
          const answer = await request
            .response()
            .then(response => response?.json())
            .catch(() => null);
          if (answer?.liveRevision === patched.liveRevision && reads.slice(index + 1).some(([next]) => next === 'sent'))
            return true;
        }
        return false;
      })
      .toBe(true);
    // The composition keeps the patch away from the editor until it ends.
    await expect(editor).not.toContainText('Accepted AI');
    await cdp.send('Input.insertText', {text: '日本語'});
    await expect(editor).toContainText('Accepted AI');
    await expect(editor).toContainText('Local paragraph.日本語');
    await expectEditorText(page, final);
    await expect.poll(async () => (await api('/p/default/api/live?path=' + path)).content).toBe(final);
    await page.locator('#save').click();
    await expect(page.locator('#save-state')).toContainText('Saved');
    expect((await api('/p/default/api/note?path=' + path)).content).toBe(final);
  });
// What the browser types reaches the editor when the editor's DOM observer
// reports it, at the end of the browser's task. A change dispatched to the
// editor before then throws the typing away (CodeMirror clears its
// observer's records in every update), so whatever reaches the editor from
// elsewhere reads the typing first. These helpers make that order certain.
//
// holdEditorInput holds back what the editor's observer reports, as if the
// browser had not reported it yet: held records are still the observer's, so
// the editor reads them whenever it asks the observer (as CodeMirror does
// before a key press or a measure, and the note editor before anything reaches
// the view). releaseEditorInput() reports what is still held. With
// typeBeforeLiveAnswer set to {text: [after, text]}, the page types text after
// `after` (typeUnread) just before it handles the next live answer that
// carries changes, with nothing between the two but promise callbacks, so no
// frame lets the editor read it first; typedBeforeLiveAnswer.read then says
// whether the editor had read it at once.
const holdEditorInput = page =>
  page.addInitScript(() => {
    const Native = window.MutationObserver,
      holding = new Set();
    window.releaseEditorInput = () => {
      window.holdEditorInput = false;
      for (const observer of holding) if (observer.held.length) observer.report(observer.held.splice(0), observer);
    };
    window.MutationObserver = class extends Native {
      constructor(callback) {
        super((records, observer) => {
          if (observer.editor && window.holdEditorInput) observer.held.push(...records);
          else callback(records, observer);
        });
        this.held = [];
        this.report = callback;
      }
      observe(target, options) {
        if (target?.classList?.contains('cm-content')) {
          this.editor = true;
          holding.add(this);
        }
        return super.observe(target, options);
      }
      takeRecords() {
        return [...this.held.splice(0), ...super.takeRecords()];
      }
      disconnect() {
        this.held.length = 0;
        return super.disconnect();
      }
    };
    // Inserts text after `after` in the editor's text nodes, as typing does,
    // away from the caret, so no selection change makes the editor read it.
    // Says whether the editor's text already has it.
    window.typeUnread = (after, text) => {
      const walker = document.createTreeWalker(document.querySelector('#editor'), NodeFilter.SHOW_TEXT);
      for (let node = walker.nextNode(); node; node = walker.nextNode()) {
        const at = node.data.indexOf(after);
        if (at < 0) continue;
        node.insertData(at + after.length, text);
        return document.getElementById('editing').markdown.includes(after + text);
      }
      throw new Error('no text node holds ' + after);
    };
    const fetchAnswer = window.fetch;
    window.fetch = async (...args) => {
      const response = await fetchAnswer(...args),
        typing = window.typeBeforeLiveAnswer;
      if (!typing || !response.ok || !/\/api\/live\?path=[^&]+&since=/.test(String(args[0]))) return response;
      const value = await response.json();
      if (!value.unchanged) {
        window.typeBeforeLiveAnswer = null;
        window.holdEditorInput = true;
        typing.read = window.typeUnread(...typing.text);
        window.typedBeforeLiveAnswer = typing;
      }
      return {ok: true, status: response.status, headers: response.headers, json: async () => value};
    };
  });
for (const format of ['visual', 'source'])
  test('a change from elsewhere never meets ' + format + ' typing the editor has not read', async ({page}) => {
    const path = 'held-input-' + format + '.md',
      initial = '# Held input\n\nAlpha paragraph.\n\nLocal paragraph.\n',
      patched = '# Held input\n\nAccepted AI paragraph.\n\nLocal paragraph. kept\n',
      final = '# Held input\n\nAccepted AI paragraph.\n\nLocal paragraph. kept again\n';
    await api('/p/default/api/note', {path, content: initial, revision: ''});
    await holdEditorInput(page);
    await login(page);
    await page.goto(origin + '/?project=default&note=' + path);
    await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
    await edit(page);
    await page.locator('#format-' + format).click();
    await expect(page.locator('#editing')).toHaveAttribute('data-format', format);
    const editor = page.locator('#editor');
    await caretAtLineEnd(page, 'Held input');
    // An agent's change arrives while the editor has not read the typing.
    await page.evaluate(() => (window.typeBeforeLiveAnswer = {text: ['Local paragraph.', ' kept']}));
    const before = await api('/p/default/api/live?path=' + path),
      start = before.content.indexOf('Alpha');
    await api('/p/default/api/live/patch', {
      path,
      base: before.liveRevision,
      operationId: 'held-input-' + format,
      edits: [{start, end: start + 5, old: 'Alpha', text: 'Accepted AI'}]
    });
    await expect(editor).toContainText('Accepted AI');
    expect(await page.evaluate(() => window.typedBeforeLiveAnswer?.read)).toBe(false);
    await page.evaluate(() => window.releaseEditorInput());
    await expectEditorText(page, patched);
    await expect.poll(async () => (await api('/p/default/api/live?path=' + path)).content).toBe(patched);
    // Undo, which reaches the editor as a change from elsewhere too, finds
    // typing the editor has not read (a step of its own, 500 ms after the
    // last), undoes it, and redo brings it back. Undo's key reaches the page
    // before the editor, as it does in the same task as the typing.
    await page.waitForTimeout(600);
    const undone = await page.evaluate(() => {
      const read = window.typeUnread(' kept', ' again');
      document
        .querySelector('#editor')
        .dispatchEvent(new KeyboardEvent('keydown', {key: 'z', ctrlKey: true, bubbles: true, cancelable: true}));
      return {read, text: document.getElementById('editing').markdown};
    });
    expect(undone).toEqual({read: false, text: patched});
    await page.keyboard.press('Control+y');
    await expectEditorText(page, final);
    await expect.poll(async () => (await api('/p/default/api/live?path=' + path)).content).toBe(final);
  });
for (const fail of [false, true])
  test(
    'reconciliation freezes accepted input during ' + (fail ? 'failed' : 'successful') + ' Apply readback',
    async ({page}) => {
      await watchConfirmations(page, dialog => dialog.accept());
      const path = 'reconcile-readback-' + fail + '.md',
        first = '# First combined draft\n';
      const {initial, note, key, user} = await openRetainedPrivate(page, path),
        scratch = 'gitnotes:reconcile:' + user.id + '/default/' + path;
      await page.locator('#draft-compare').click();
      await expect(page.locator('#reconcile-dialog')).toBeVisible();
      await page.locator('#reconcile-content').fill(first);
      let release,
        waiting = false;
      const gate = new Promise(resolve => (release = resolve));
      await page.route('**/api/locks?path=' + path, async route => {
        waiting = true;
        await gate;
        if (fail) await route.fulfill({status: 503, json: {error: 'Synthetic Apply readback failed'}});
        else await route.continue();
      });
      try {
        await page.locator('#reconcile-apply').click();
        await expect.poll(() => waiting).toBe(true);
        await page.locator('#reconcile-content').press('End');
        await page.keyboard.type('Late typing');
        await expect(page.locator('#reconcile-content')).toHaveValue(first);
        await expect(page.locator('#reconcile-content')).not.toBeEditable();
        expect(await stored(page, scratch)).toBe(first);
        release();
        if (fail) {
          await expect(page.locator('#reconcile-error')).toContainText('Synthetic Apply readback failed');
          await expect(page.locator('#reconcile-content')).toBeEditable();
          expect(await stored(page, scratch)).toBe(first);
          await page.unroute('**/api/locks?path=' + path);
          await page.locator('#reconcile-apply').click();
        }
        await expect(page.locator('#reconcile-dialog')).toBeHidden();
        await expectEditorText(page, first);
        expect(JSON.parse(await stored(page, key))).toMatchObject({content: first});
        expect(await stored(page, scratch)).toBeNull();
        const saved = await api('/p/default/api/note?path=' + path);
        expect(saved.content).toBe(initial);
        expect(saved.revision).toBe(note.revision);
        await page.locator('#save').click();
        await expect(page.locator('#save-state')).toContainText('Saved');
        expect((await api('/p/default/api/note?path=' + path)).content).toBe(first);
      } finally {
        release();
      }
    }
  );
for (const storageFails of [true, false])
  test(
    'failed private Save reports ' + (storageFails ? 'failed' : 'successful') + ' persistence of current text',
    async ({page}) => {
      const path = 'private-save-storage-' + storageFails + '.md',
        newer = '# New private text\n';
      const {initial, prior, note, key} = await openRetainedPrivate(page, path);
      if (storageFails)
        await page.evaluate(() => {
          const set = Storage.prototype.setItem;
          Storage.prototype.setItem = function (key, value) {
            if (key.startsWith('gitnotes:draft:'))
              throw new DOMException('Synthetic quota failure', 'QuotaExceededError');
            return set.call(this, key, value);
          };
        });
      await page.route('**/api/note', route =>
        route.fulfill({status: 503, json: {error: 'Synthetic failed private save'}})
      );
      await fillSource(page, newer);
      await page.locator('#save').click();
      await expect(page.locator('#save-state')).toContainText('Not saved');
      await expect(page.locator('#notice')).toContainText('Synthetic failed private save');
      if (storageFails) {
        await expect(page.locator('#save-state')).toContainText('keep this tab open');
        await expect(page.locator('#notice')).toContainText('Keep this tab open');
        await expect(page.locator('#save-state')).not.toContainText('draft kept on this device');
      } else await expect(page.locator('#save-state')).toContainText('draft kept on this device');
      expect(JSON.parse(await stored(page, key))).toMatchObject({content: storageFails ? prior : newer});
      await expectEditorText(page, newer);
      const saved = await api('/p/default/api/note?path=' + path);
      expect(saved.content).toBe(initial);
      expect(saved.revision).toBe(note.revision);
    }
  );
for (const discard of [false, true])
  test(
    'oversized historical private drafts reconcile through Git ' + (discard ? 'discard' : 'apply'),
    async ({page}) => {
      await watchConfirmations(page, dialog => dialog.accept());
      const path = 'large-history-reconcile-' + discard + '.md',
        initial = '# Large history\n\n' + 'a'.repeat((1 << 20) + 64) + '\n';
      const first = await api('/p/default/api/note', {path, content: initial, revision: ''}),
        second = await api('/p/default/api/note', {
          path,
          content: initial + 'Second revision.\n',
          revision: first.revision
        });
      await login(page);
      await page.goto(origin + '/?project=default&note=' + path);
      await edit(page);
      await clickNoteAction(page, 'history-open');
      await page
        .locator('#history-list button')
        .filter({hasText: first.revision.slice(0, 7)})
        .click();
      await expect(page.locator('#history-dialog')).toBeHidden();
      await expectEditorText(page, initial);
      const latest = await api('/p/default/api/note', {
        path,
        content: initial + 'External newer revision.\n',
        revision: second.revision
      });
      await page.locator('#save').click();
      await expect(page.locator('#save-state')).toContainText('Not saved');
      const user = await api('/api/me'),
        key = 'gitnotes:draft:' + user.id + '/default/' + path;
      await page.locator('#draft-compare').click();
      await expect(page.locator('#reconcile-dialog')).toBeVisible();
      await expect(page.locator('#reconcile-latest')).toHaveValue(latest.content);
      await expect(page.locator('#reconcile-content')).toHaveValue(initial);
      await page.route('**/api/note?path=' + path, route =>
        route.fulfill({status: 503, json: {error: 'Synthetic comparison read failed'}})
      );
      await page.locator(discard ? '#reconcile-discard' : '#reconcile-apply').click();
      await expect(page.locator('#reconcile-error')).toContainText('Synthetic comparison read failed');
      await expect(page.locator('#reconcile-content')).toBeEditable();
      expect(JSON.parse(await stored(page, key))).toMatchObject({content: initial, private: true});
      await page.unroute('**/api/note?path=' + path);
      await page.locator(discard ? '#reconcile-discard' : '#reconcile-apply').click();
      await expect(page.locator('#reconcile-dialog')).toBeHidden();
      await expectEditorText(page, discard ? latest.content : initial);
      expect((await api('/p/default/api/note?path=' + path)).content).toBe(latest.content);
      if (!discard) {
        await page.locator('#save').click();
        await expect(page.locator('#save-state')).toContainText('Saved');
        expect((await api('/p/default/api/note?path=' + path)).content).toBe(initial);
      } else expect(await stored(page, key)).toBeNull();
    }
  );
test('a failed live poll does not turn unchanged content into a recovered draft', async ({page}) => {
  const path = 'live-clean-outage.md',
    content = '# Already saved\n\nUnchanged text.\n';
  const saved = await api('/p/default/api/note', {path, content, revision: ''});
  await login(page);
  const poll = url => url.pathname.endsWith('/api/live') && url.searchParams.has('since');
  await page.route(poll, route => route.fulfill({status: 503, json: {error: 'Synthetic polling outage'}}));
  await page.goto(origin + '/?project=default&note=' + path);
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await expect(page.locator('#notice')).toContainText('Synthetic polling outage');
  expect(await retainedTexts(page, path), 'a transport error must not create an unchanged draft').toEqual([]);
  await expect(page.locator('#save-state')).toHaveText('Live connection unavailable');
  await page.reload();
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await expect(page.locator('#editing')).toBeHidden();
  await expectEditorText(page, content);
  await page.unroute(poll);
  const committed = await api('/p/default/api/note?path=' + path);
  expect(committed.content).toBe(content);
  expect(committed.revision).toBe(saved.revision);
});
for (const draftStorageFails of [true, false])
  test(
    'ordinary live outage reports ' + (draftStorageFails ? 'failed' : 'successful') + ' current draft persistence',
    async ({page}) => {
      const path = 'live-retention-' + draftStorageFails + '.md',
        initial = '# Retention\n\nOriginal text.\n',
        draft = initial + 'Local unsynced sentence.\n';
      await api('/p/default/api/note', {path, content: initial, revision: ''});
      await login(page);
      await page.goto(origin + '/?project=default&note=' + path);
      await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
      await edit(page);
      await page.route('**/api/live', route =>
        route.fulfill({status: 503, json: {error: 'Synthetic connection outage'}})
      );
      await page.evaluate(fails => {
        const set = Storage.prototype.setItem;
        Storage.prototype.setItem = function (key, value) {
          if (key.startsWith('gitnotes:live:') || (fails && key.startsWith('gitnotes:draft:')))
            throw new DOMException('Synthetic quota failure', 'QuotaExceededError');
          return set.call(this, key, value);
        };
      }, draftStorageFails);
      const user = await api('/api/me'),
        key = 'gitnotes:draft:' + user.id + '/default/' + path;
      for (const content of [draft, draft + 'Newer unsynced text.\n']) {
        const failure = page.waitForResponse(
          response => response.url().endsWith('/api/live') && response.status() === 503
        );
        await fillSource(page, content);
        await failure;
        if (draftStorageFails) {
          await expect(page.locator('#save-state')).toContainText('keep this tab open');
          await expect(page.locator('#save-state')).not.toContainText('retained on this device');
          await expect(page.locator('#notice')).toContainText('cannot preserve');
          await expect(page.locator('#notice')).toContainText('Keep this tab open');
          expect(await stored(page, key)).toBeNull();
        } else {
          await expect(page.locator('#save-state')).toContainText('draft retained on this device');
          await expect(page.locator('#notice')).toContainText('Synthetic connection outage');
          expect(JSON.parse(await stored(page, key))).toMatchObject({content});
        }
        await expectEditorText(page, content);
        expect((await api('/p/default/api/live?path=' + path)).content).toBe(initial);
      }
    }
  );
test('historical rollback equal to Git remains private until explicit reconciliation', async ({page}) => {
  await watchConfirmations(page, dialog => dialog.accept());
  const path = 'history-private-rollback.md',
    initial = '# Last checkpoint\n\nOriginal paragraph.\n';
  const note = await api('/p/default/api/note', {path, content: initial, revision: ''});
  await login(page);
  await page.goto(origin + '/?project=default&note=' + path);
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await edit(page);
  let peer, release;
  const gate = new Promise(resolve => (release = resolve));
  await page.route('**/api/live?*', async route => {
    await gate;
    await route.fulfill({json: peer});
  });
  try {
    const base = await api('/p/default/api/live?path=' + path),
      start = base.content.indexOf('Original');
    peer = await api('/p/default/api/live/patch', {
      path,
      base: base.liveRevision,
      operationId: 'historical-private-peer',
      edits: [{start, end: start + 8, old: 'Original', text: 'Accepted peer'}]
    });
    release();
    await expectEditorText(page, peer.content);
    await clickNoteAction(page, 'history-open');
    await page
      .locator('#history-list button')
      .filter({hasText: note.revision.slice(0, 7)})
      .click();
    await expect(page.locator('#history-dialog')).toBeHidden();
    await expectEditorText(page, initial);
    const user = await api('/api/me'),
      key = 'gitnotes:draft:' + user.id + '/default/' + path;
    expect(
      JSON.parse(await stored(page, key)),
      'installed historical choice must be durable private intent'
    ).toMatchObject({content: initial, private: true});
    await expect(page.locator('#save')).toBeEnabled();
    await expect(page.locator('#document')).not.toHaveAttribute('data-live', 'connected');
    await page.locator('#save').click();
    await expect(page.locator('#save-state')).toContainText('Not saved');
    expect(JSON.parse(await stored(page, key))).toMatchObject({content: initial, private: true});
    const checkpoint = await api('/p/default/api/live/checkpoint', {path});
    expect(checkpoint.content).toBe(peer.content);
    await page.unroute('**/api/live?*');
    await page.reload();
    await edit(page);
    await expectEditorText(page, initial);
    await expect(page.locator('#save')).toBeEnabled();
    expect((await api('/p/default/api/note?path=' + path)).content).toBe(peer.content);
    await page.locator('#draft-compare').click();
    await expect(page.locator('#reconcile-latest')).toHaveValue(peer.content);
    await expect(page.locator('#reconcile-content')).toHaveValue(initial);
    await page.locator('#reconcile-apply').click();
    await expect(page.locator('#reconcile-dialog')).toBeHidden();
    await page.locator('#save').click();
    await expect(page.locator('#save-state')).toContainText('Saved');
    expect((await api('/p/default/api/note?path=' + path)).content).toBe(initial);
    expect((await api('/p/default/api/note?path=' + path + '&revision=' + checkpoint.revision)).content).toBe(
      peer.content
    );
  } finally {
    release();
  }
});
for (const composition of [false, true])
  test(
    'refused private rollback equal to Git survives ' + (composition ? 'composition' : 'input') + ' and reload',
    async ({page}) => {
      await watchConfirmations(page, dialog => dialog.accept());
      const path = 'bounded-rollback-' + composition + '.md',
        initial =
          '# Private rollback\n\n' + 'a'.repeat(2500) + '\n\nIndependent 😀 paragraph.\n\n' + 'b'.repeat(2500) + '\n';
      await api('/p/default/api/note', {path, content: initial, revision: ''});
      await login(page);
      await page.goto(origin + '/?project=default&note=' + path);
      await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
      await edit(page);
      let pending, release;
      const gate = new Promise(resolve => (release = resolve));
      await page.route('**/api/live?*', async route => {
        await gate;
        await route.fulfill({json: pending});
      });
      try {
        const base = await api('/p/default/api/live?path=' + path),
          first = initial.indexOf('a'.repeat(2500)),
          last = initial.indexOf('b'.repeat(2500));
        pending = await api('/p/default/api/live/patch', {
          path,
          base: base.liveRevision,
          operationId: 'private-rollback-peer-' + composition,
          edits: [
            {start: first, end: first + 2500, old: 'a'.repeat(2500), text: 'x'.repeat(2500)},
            {start: last, end: last + 2500, old: 'b'.repeat(2500), text: 'y'.repeat(2500)}
          ]
        });
        expect(pending.pending).toBe(true);
        release();
        await expectEditorText(page, pending.content);
        await refuseLiveUpdates(page);
        if (composition) await page.locator('#editor').dispatchEvent('compositionstart');
        await fillSource(page, initial);
        if (composition) await page.locator('#editor').dispatchEvent('compositionend');
        await expect(page.locator('#notice')).toContainText(refusal);
        await expect(page.locator('#document')).not.toHaveAttribute('data-live', 'connected');
        const user = await api('/api/me'),
          key = 'gitnotes:draft:' + user.id + '/default/' + path;
        const draft = JSON.parse(await stored(page, key));
        expect(draft, 'rejected rollback must be persisted even when it equals original Git').toMatchObject({
          content: initial,
          private: true
        });
        await expect(page.locator('#draft-compare')).toBeVisible();
        await expect(page.locator('#save')).toBeEnabled();
        await page.locator('#draft-compare').click();
        await expect(page.locator('#reconcile-latest')).toHaveValue(pending.content);
        await expect(page.locator('#reconcile-content')).toHaveValue(initial);
        await page.locator('#reconcile-close').click();
        await page.locator('#save').click();
        await expect(page.locator('#save-state')).toContainText('Not saved');
        expect(JSON.parse(await stored(page, key))).toMatchObject({content: initial, private: true});
        await api('/p/default/api/live/checkpoint', {path});
        await page.unroute('**/api/live?*');
        await page.unroute('**/api/live');
        await page.reload();
        await edit(page);
        await expectEditorText(page, initial);
        await expect(page.locator('#draft-compare')).toBeVisible();
        expect((await api('/p/default/api/note?path=' + path)).content).toBe(pending.content);
      } finally {
        release();
      }
    }
  );
test('a refused update reports failed browser storage without claiming the private draft is saved', async ({page}) => {
  const path = 'bounded-storage-failure.md',
    initial = '# Storage failure\n\n' + 'a'.repeat(2500) + '\n\nKeep unchanged\n\n' + 'b'.repeat(2500),
    draft = initial.replaceAll('a'.repeat(2500), 'x'.repeat(2500)).replaceAll('b'.repeat(2500), 'y'.repeat(2500));
  await api('/p/default/api/note', {path, content: initial, revision: ''});
  await login(page);
  await page.goto(origin + '/?project=default&note=' + path);
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await edit(page);
  await page.evaluate(() => {
    const set = Storage.prototype.setItem;
    Storage.prototype.setItem = function (key, value) {
      if (key.startsWith('gitnotes:draft:')) throw new DOMException('Synthetic quota failure', 'QuotaExceededError');
      return set.call(this, key, value);
    };
  });
  await refuseLiveUpdates(page);
  await fillSource(page, draft);
  await expect(page.locator('#notice')).toContainText('cannot preserve');
  await expect(page.locator('#notice')).toContainText('Keep this tab open');
  await expect(page.locator('#save-state')).toContainText('keep this tab open');
  await expect(page.locator('#save-state')).not.toContainText('saved on this device');
  await expect(page.locator('#document')).not.toHaveAttribute('data-live', 'connected');
  await expectEditorText(page, draft);
  await expect(page.locator('#draft-compare')).toBeVisible();
  await expect(page.locator('#save')).toBeEnabled();
  expect((await api('/p/default/api/live?path=' + path)).content).toBe(initial);
});
test('a refused private draft reconciles accepted peer text and resumes save undo and redo', async ({page}) => {
  await watchConfirmations(page, dialog => dialog.accept());
  const path = 'complex-reconcile-control.md',
    initial = '# Complex\n\n' + 'a'.repeat(2500) + '\n\nIndependent paragraph.\n\n' + 'b'.repeat(2500) + '\n',
    draft = initial.replaceAll('a'.repeat(2500), 'x'.repeat(2500)).replaceAll('b'.repeat(2500), 'y'.repeat(2500));
  await api('/p/default/api/note', {path, content: initial, revision: ''});
  await login(page);
  await page.goto(origin + '/?project=default&note=' + path);
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await edit(page);
  await refuseLiveUpdates(page);
  await fillSource(page, draft);
  await expect(page.locator('#notice')).toContainText(refusal);
  await page.unroute('**/api/live');
  const base = await api('/p/default/api/live?path=' + path),
    start = base.content.indexOf('Independent');
  const peer = await api('/p/default/api/live/patch', {
    path,
    base: base.liveRevision,
    operationId: 'complex-control-peer',
    edits: [{start, end: start + 11, old: 'Independent', text: 'Accepted AI'}]
  });
  await api('/p/default/api/live/checkpoint', {path});
  await page.reload();
  await page.locator('#draft-compare').click();
  await expect(page.locator('#reconcile-latest')).toHaveValue(peer.content);
  await expect(page.locator('#reconcile-content')).toHaveValue(draft);
  const combined = draft.replace('Independent', 'Accepted AI');
  await page.locator('#reconcile-content').fill(combined);
  await page.locator('#reconcile-apply').click();
  await expect(page.locator('#reconcile-dialog')).toBeHidden();
  await page.locator('#save').click();
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  expect((await api('/p/default/api/note?path=' + path)).content).toBe(combined);
  await page.locator('#format-source').click();
  await page.locator('#editor').press('Control+End');
  await page.keyboard.type('Local 😀 continuation');
  await expect
    .poll(async () => (await api('/p/default/api/live?path=' + path)).content)
    .toBe(combined + 'Local 😀 continuation');
  await page.locator('#editor').press('Control+z');
  await expectEditorText(page, combined);
  await page.locator('#editor').press('Control+Shift+z');
  await expectEditorText(page, combined + 'Local 😀 continuation');
});
for (const composition of [false, true])
  test('refused ' + (composition ? 'composition' : 'input') + ' stays private through reload', async ({page}) => {
    const path = 'bounded-diff-' + composition + '.md',
      before = '# Bound\n\n' + 'a'.repeat(2500) + '\n\nIndependent paragraph.\n\n' + 'b'.repeat(2500) + '\n',
      after = before.replaceAll('a'.repeat(2500), 'x'.repeat(2500)).replaceAll('b'.repeat(2500), 'y'.repeat(2500));
    await api('/p/default/api/note', {path, content: before, revision: ''});
    await login(page);
    await page.goto(origin + '/?project=default&note=' + path);
    await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
    await edit(page);
    await refuseLiveUpdates(page);
    if (composition) await page.locator('#editor').dispatchEvent('compositionstart');
    await fillSource(page, after);
    if (composition) await page.locator('#editor').dispatchEvent('compositionend');
    await expect(page.locator('#notice')).toContainText(refusal);
    await expect(page.locator('#document')).not.toHaveAttribute('data-live', 'connected');
    expect((await api('/p/default/api/live?path=' + path)).content).toBe(before);
    const base = await api('/p/default/api/live?path=' + path),
      start = before.indexOf('Independent');
    const accepted = await api('/p/default/api/live/patch', {
      path,
      base: base.liveRevision,
      operationId: 'bounded-diff-peer-' + composition,
      edits: [{start, end: start + 11, old: 'Independent', text: 'Accepted peer'}]
    });
    await api('/p/default/api/live/checkpoint', {path});
    await expectEditorText(page, after);
    await page.unroute('**/api/live');
    await page.reload();
    await edit(page);
    await expectEditorText(page, after);
    await expect(page.locator('#draft-conflict')).toBeVisible();
    expect((await api('/p/default/api/note?path=' + path)).content).toBe(accepted.content);
  });
for (const format of ['visual', 'source'])
  test(format + ' disjoint local changes preserve independent AI edits and own undo', async ({page}) => {
    const path = 'disjoint-' + format + '.md',
      initial =
        '# Concurrent\n\n- item one\n- item two\n\nAlpha paragraph.\n\nRepeated 😀 paragraph.\n\nRepeated 😀 paragraph.\n\nBeta paragraph.\n';
    await api('/p/default/api/note', {path, content: initial, revision: ''});
    await login(page);
    await page.goto(origin + '/?project=default&note=' + path);
    await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
    await edit(page);
    if (format === 'visual') {
      await page.locator('#format-visual').click();
      await expectVisual(page);
    }
    const base = await api('/p/default/api/live?path=' + path);
    let release,
      waiting = false;
    const gate = new Promise(resolve => (release = resolve));
    await page.route('**/api/live', async route => {
      waiting = true;
      await gate;
      await route.continue();
    });
    await page.route('**/api/live?*', async route => {
      await gate;
      await route.continue();
    });
    try {
      if (format === 'visual') {
        await caretAtLineEnd(page, 'Beta paragraph.');
        await page.keyboard.type(' local');
      } else {
        // Two disjoint edits, typed where they belong.
        for (const {index} of initial.matchAll(/^- /gm)) {
          await selectSource(page, index, index + 1);
          await page.keyboard.type('*');
        }
        await selectSource(page, initial.indexOf('Beta paragraph.') + 'Beta paragraph.'.length);
        await page.keyboard.type(' local');
      }
      await expectEditorText(page, /Beta paragraph\. local/);
      await expect.poll(() => waiting).toBe(true);
      const start = initial.indexOf('Alpha paragraph.');
      await api('/p/default/api/live/patch', {
        path,
        base: base.liveRevision,
        operationId: 'disjoint-peer-' + format,
        edits: [{start, end: start + 16, old: 'Alpha paragraph.', text: 'Accepted AI paragraph.'}]
      });
      release();
      await expect
        .poll(async () => (await api('/p/default/api/live?path=' + path)).content)
        .toContain('Beta paragraph. local');
      const merged = (await api('/p/default/api/live?path=' + path)).content;
      expect(merged).toContain(
        '\n\nAccepted AI paragraph.\n\nRepeated 😀 paragraph.\n\nRepeated 😀 paragraph.\n\nBeta paragraph. local'
      );
      expect(merged).not.toContain('Alpha paragraph.');
      // Visual changes only what was typed; Source typed the new markers.
      expect(merged).toMatch(format === 'visual' ? /^- item one\n- item two$/m : /^\* +item one\n\* +item two$/m);
      await expectEditorText(page, merged);
      await page.locator('#editor').press('Control+z');
      const undone = initial.replace('Alpha paragraph.', 'Accepted AI paragraph.');
      await expectEditorText(page, undone);
      await expect.poll(async () => (await api('/p/default/api/live?path=' + path)).content).toBe(undone);
      const checkpoint = await api('/p/default/api/live/checkpoint', {path});
      expect(checkpoint.content).toBe(undone);
      expect((await api('/p/default/api/note?path=' + path + '&revision=' + checkpoint.revision)).content).toBe(undone);
    } finally {
      release();
    }
  });
test('newer rendered live text remains visible after a delayed Save readback', async ({page}) => {
  const path = 'save-delayed-preview.md';
  await api('/p/default/api/note', {path, content: '# Original\n\nAccepted paragraph.\n', revision: ''});
  await login(page);
  await page.goto(origin + '/?project=default&note=' + path);
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await edit(page);
  await page.locator('#editor').press('Control+End');
  await page.keyboard.type('\nLocal change.');
  let newerContent;
  await page.route('**/api/note?path=' + path, async route => {
    const response = await route.fetch(),
      base = await api('/p/default/api/live?path=' + path),
      start = base.content.indexOf('Accepted');
    newerContent = (
      await api('/p/default/api/live/patch', {
        path,
        base: base.liveRevision,
        operationId: 'newer-while-saving',
        edits: [{start, end: start + 8, old: 'Accepted', text: 'NEWER LIVE'}]
      })
    ).content;
    await expectEditorText(page, newerContent);
    await route.fulfill({response});
  });
  await page.locator('#save').click();
  await expect(page.locator('#save-state')).toContainText('Live changes synced');
  await expect(page.locator('#save')).toBeEnabled();
  await expectEditorText(page, newerContent);
  await page.locator('#mode').click();
  await expect(page.locator('#preview')).toContainText('NEWER LIVE');
  expect((await api('/p/default/api/live?path=' + path)).content).toBe(newerContent);
});
for (const cancel of ['close', 'escape', 'remote', 'supersede'])
  test('historical selection ownership ends on ' + cancel, async ({page}) => {
    const path = 'history-intent-' + cancel + '.md',
      old = '# Historical old\n',
      initial = '# Current history intent\n';
    const first = await api('/p/default/api/note', {path, content: old, revision: ''});
    await api('/p/default/api/note', {path, content: initial, revision: first.revision});
    await login(page);
    await page.goto(origin + '/?project=default&note=' + path);
    await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
    await edit(page);
    let release,
      waiting = false;
    const gate = new Promise(resolve => (release = resolve));
    const oldURL = '**/api/note?path=' + path + '&revision=' + first.revision;
    await page.route(oldURL, async route => {
      const response = await route.fetch();
      waiting = true;
      await gate;
      await route.fulfill({response});
    });
    try {
      await clickNoteAction(page, 'history-open');
      await page.locator('#history-list button').last().click();
      await expect.poll(() => waiting).toBe(true);
      let accepted = initial;
      if (cancel === 'close' || cancel === 'escape') {
        if (cancel === 'close') await page.locator('#history-dialog [data-close="history-dialog"]').click();
        else await page.keyboard.press('Escape');
        await fillSource(page, initial + 'Local before canceled restore\n');
        await expect
          .poll(async () => (await api('/p/default/api/live?path=' + path)).content)
          .toContain('Local before canceled restore');
      }
      if (cancel === 'supersede') {
        await page.locator('#history-list button').first().click();
        await expect(page.locator('#history-dialog')).toBeHidden();
        await expect(page.locator('#save-state')).toContainText('Historical version loaded');
      } else {
        const base = await api('/p/default/api/live?path=' + path);
        accepted = (
          await api('/p/default/api/live/patch', {
            path,
            base: base.liveRevision,
            operationId: 'history-intent-peer-' + cancel,
            edits: [{start: base.content.length, end: base.content.length, old: '', text: '\nAccepted AI paragraph\n'}]
          })
        ).content;
        await expectEditorText(page, accepted);
        if (cancel !== 'remote') {
          // The keyboard command checkpoints the accepted live text explicitly.
          await page.locator('#editor').press('Control+s');
          await expect(page.locator('#save-state')).toContainText('Saved');
          expect((await api('/p/default/api/note?path=' + path)).content).toBe(accepted);
        }
      }
      const response = page.waitForResponse(response =>
        response.url().endsWith('/api/note?path=' + path + '&revision=' + first.revision)
      );
      release();
      await (await response).finished();
      if (cancel === 'remote') await page.locator('#history-dialog [data-close="history-dialog"]').click();
      await page.locator('#editor').press('Control+End');
      await page.keyboard.type('Continued typing');
      await expectEditorText(page, accepted + 'Continued typing');
      if (cancel !== 'supersede') await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
      await page.locator('#save').click();
      await expect(page.locator('#save-state')).toContainText('Saved');
      expect((await api('/p/default/api/note?path=' + path)).content).toBe(accepted + 'Continued typing');
    } finally {
      release();
    }
  });
test('an intentional historical selection still restores and commits the chosen version', async ({page}) => {
  const path = 'intentional-history-restore.md',
    old = '# Chosen old version\n';
  const first = await api('/p/default/api/note', {path, content: old, revision: ''});
  await api('/p/default/api/note', {path, content: '# Current version\n', revision: first.revision});
  await login(page);
  await page.goto(origin + '/?project=default&note=' + path);
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await edit(page);
  await clickNoteAction(page, 'history-open');
  await page.locator('#history-list button').last().click();
  await expect(page.locator('#save-state')).toContainText('Historical version loaded');
  await expectEditorText(page, old);
  await page.locator('#save').click();
  await expect(page.locator('#save-state')).toContainText('Saved');
  expect((await api('/p/default/api/note?path=' + path)).content).toBe(old);
});
test('a canceled historical preview cannot overwrite newer saved text, status or live ownership', async ({page}) => {
  const path = 'history-preview-intent.md',
    old = '# Chosen history before preview\n';
  const first = await api('/p/default/api/note', {path, content: old, revision: ''});
  await api('/p/default/api/note', {path, content: '# Current before history preview\n', revision: first.revision});
  await login(page);
  await page.goto(origin + '/?project=default&note=' + path);
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  let release,
    waiting = false;
  const gate = new Promise(resolve => (release = resolve));
  // Historical private previews now load the local parser instead of posting
  // their text. Hold that first load while newer shared content is saved.
  await page.route('**/static/dist/rendered-preview-*.js', async route => {
    const response = await route.fetch();
    waiting = true;
    await gate;
    await route.fulfill({response});
  });
  try {
    await edit(page);
    await clickNoteAction(page, 'history-open');
    await page.locator('#history-list button').last().click();
    await expect.poll(() => waiting).toBe(true);
    await expectEditorText(page, old);
    await page.locator('#history-dialog [data-close="history-dialog"]').click();
    await fillSource(page, old + 'Newer local text after closing\n');
    await page.locator('#save').click();
    await expect(page.locator('#save-state')).toContainText('Saved');
    await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
    const base = await api('/p/default/api/live?path=' + path),
      remote = 'Accepted peer after newer save\n';
    await api('/p/default/api/live/patch', {
      path,
      base: base.liveRevision,
      operationId: 'history-preview-peer',
      edits: [{start: base.content.length, end: base.content.length, old: '', text: remote}]
    });
    await expectEditorText(page, base.content + remote);
    const response = page.waitForResponse(response =>
      /\/static\/dist\/rendered-preview-[^/]+\.js$/.test(response.url())
    );
    release();
    await (await response).finished();
    await page.locator('#editor').press('Control+End');
    await page.keyboard.type('Continued after preview');
    await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
    await expect(page.locator('#save-state')).not.toContainText('Historical version loaded');
    await page.locator('#save').click();
    await expect(page.locator('#save-state')).toContainText('Saved');
    expect((await api('/p/default/api/note?path=' + path)).content).toBe(
      base.content + remote + 'Continued after preview'
    );
    // Reading renders the newer text, never the canceled historical preview.
    await page.locator('#mode').click();
    await expect(page.locator('#preview')).toContainText('Accepted peer after newer save');
    await expect(page.locator('#preview')).toContainText('Continued after preview');
  } finally {
    release();
  }
});
for (const failed of [false, true])
  test(
    (failed ? 'failed' : 'pending') + ' note navigation preserves the displayed live note through editing and Save',
    async ({page}) => {
      const a = 'note-intent-a-' + failed + '.md',
        b = 'note-intent-b-' + failed + '.md';
      await api('/p/default/api/note', {path: a, content: '# Displayed A\n', revision: ''});
      await api('/p/default/api/note', {path: b, content: '# Destination B\n', revision: ''});
      await login(page);
      await page.goto(origin + '/?project=default&note=' + a);
      await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
      await edit(page);
      let release,
        waiting = false;
      const gate = new Promise(resolve => (release = resolve));
      await page.route('**/api/note?path=' + b, async route => {
        waiting = true;
        await gate;
        if (failed) await route.fulfill({status: 503, json: {error: 'Synthetic note outage'}});
        else await route.continue();
      });
      // Saving supersedes the pending navigation, which may cancel its request.
      const settled = requestSettled(page, url => url.endsWith('/api/note?path=' + b));
      try {
        await navigateNote(page, b);
        await expect.poll(() => waiting).toBe(true);
        if (failed) {
          release();
          await expect(page.locator('#notice')).toContainText('Synthetic note outage');
        }
        const base = await api('/p/default/api/live?path=' + a),
          remote = '\nAccepted remote A\n';
        await api('/p/default/api/live/patch', {
          path: a,
          base: base.liveRevision,
          operationId: 'note-intent-peer-' + failed,
          edits: [{start: base.content.length, end: base.content.length, old: '', text: remote}]
        });
        await expectEditorText(page, base.content + remote);
        await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
        await page.locator('#editor').press('Control+End');
        await page.keyboard.type('Local after navigation');
        await page.locator('#save').click();
        await expect(page.locator('#save-state')).toContainText('Saved');
        if (!failed) {
          release();
          await settled;
        }
        await expect(page.locator('#note-path')).toHaveText(a);
        await expectEditorText(page, base.content + remote + 'Local after navigation');
        expect((await api('/p/default/api/note?path=' + a)).content).toBe(
          base.content + remote + 'Local after navigation'
        );
      } finally {
        release();
      }
    }
  );
test('installing a private draft supersedes pending folder navigation', async ({page}) => {
  const path = 'private-draft-origin.md';
  await api('/p/default/api/note', {path, content: '# Origin\n', revision: ''});
  await login(page);
  await page.goto(origin + '/?project=default&note=' + path);
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  let release,
    waiting = false;
  const gate = new Promise(resolve => (release = resolve));
  await page.route('**/api/browse?*', async route => {
    waiting = true;
    await gate;
    await route.continue();
  });
  // Installing the new note supersedes the pending folder, which may cancel its request.
  const settled = requestSettled(page, url => url.includes('/api/browse?'));
  try {
    await page.locator('#browse-root').click();
    await expect.poll(() => waiting).toBe(true);
    await clickNewNote(page);
    await page.locator('#new-name').fill('superseding-private');
    await page.locator('#new-submit').click();
    await expect(page.locator('#note-path')).toHaveText('superseding-private.md');
    await page.locator('#format-source').click();
    await fillSource(page, '# New private draft\n');
    release();
    await settled;
    await page.locator('#editor').press('Control+End');
    await page.keyboard.type('Still here');
    await expect(page.locator('#document')).toBeVisible();
    await expect(page.locator('#note-path')).toHaveText('superseding-private.md');
    await expectEditorText(page, '# New private draft\nStill here');
  } finally {
    release();
  }
});
test('saving a recovered note before its preview finishes cannot invalidate its connected session', async ({page}) => {
  const path = 'collaboration/recovered-save-preview.md';
  const note = await api('/p/default/api/note', {path, content: '# Original\n', revision: ''});
  await login(page);
  await privateDraft(page, note, '# Recovered before save\n');
  let release,
    waiting = false,
    finished = false;
  const gate = new Promise(resolve => (release = resolve));
  await page.route('**/api/preview', async route => {
    if (waiting || route.request().postDataJSON().content !== '# Recovered before save\n') return route.continue();
    waiting = true;
    await gate;
    await route.fulfill({json: {html: '<h1>Recovered before save</h1>'}});
    finished = true;
  });
  try {
    await page.goto(origin + '/?project=default&note=' + encodeURIComponent(path));
    await expect.poll(() => waiting).toBe(true);
    await page.locator('#format-source').click();
    await page.locator('#save').click();
    await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
    release();
    await expect.poll(() => finished).toBe(true);
    const before = await api('/p/default/api/live?path=' + encodeURIComponent(path)),
      remote = '\nAccepted after recovered save\n';
    await api('/p/default/api/live/patch', {
      path,
      base: before.liveRevision,
      operationId: 'recovered-save-remote',
      edits: [{start: before.content.length, end: before.content.length, old: '', text: remote}]
    });
    await expect
      .configure({soft: true})
      .poll(() => editorText(page), {timeout: 3000})
      .toBe(before.content + remote);
    await page.locator('#editor').press('Control+End');
    await page.keyboard.type('Local continuation');
    await expect
      .poll(async () => (await api('/p/default/api/live?path=' + encodeURIComponent(path))).content)
      .toContain('Local continuation');
    expect((await api('/p/default/api/live?path=' + encodeURIComponent(path))).content).toBe(
      before.content + remote + 'Local continuation'
    );
  } finally {
    release();
  }
});
// Typing writes the draft record on a schedule, within 250 ms of an edit,
// rather than serializing the note on every keystroke; leaving the page
// writes what the schedule has not reached yet.
test('typing writes the draft record on a schedule and leaving the page writes it at once', async ({page}) => {
  const path = 'draft-schedule.md',
    {key, prior, user} = await openRetainedPrivate(page, path),
    scope = user.id + '/default/' + path;
  await page.evaluate(() => {
    const set = Storage.prototype.setItem;
    window.draftWrites = 0;
    Storage.prototype.setItem = function (key, value) {
      if (key.startsWith('gitnotes:draft:')) window.draftWrites++;
      return set.call(this, key, value);
    };
  });
  await page.locator('#editor').press(documentEnd);
  const typed = 'Forty characters, typed without a pause.';
  await page.keyboard.type(typed);
  await expect.poll(async () => JSON.parse(await stored(page, key)).content).toBe(prior + typed);
  const writes = await page.evaluate(() => window.draftWrites);
  expect(writes).toBeGreaterThan(0);
  expect(writes).toBeLessThan(typed.length / 4);
  await expect(page.locator('#save-state')).toHaveText('Unsaved on this device');
  // One evaluation, so no timer runs between the edit and leaving the page.
  const leaving = await page.evaluate(async scope => {
    const content = () =>
      JSON.parse(localStorage.getItem('gitnotes:draft:@' + sessionStorage.getItem('gitnotes:tab') + '/' + scope))
        .content;
    document.execCommand('insertText', false, '!');
    await Promise.resolve();
    const before = content();
    dispatchEvent(new Event('pagehide'));
    return {before, after: content(), shown: document.getElementById('editing').markdown};
  }, scope);
  expect(leaving.shown).toBe(prior + typed + '!');
  expect(leaving.before).toBe(prior + typed);
  expect(leaving.after).toBe(leaving.shown);
});
// While live editing connects, typing never writes the note whole: the draft
// record is written once, when the note first holds unsaved text, and the
// live record journals each change after it, with only the Yjs update the
// server has not acknowledged.
//
// Each keystroke reaches the editor, the journal and the session in its own
// task; the session writes the live record once the change has painted, so
// how often it is written depends on when the browser runs the page's timers.
// Between keys Playwright types without a pause, Chromium ran none of them
// while the page had no input listener (6 writes for these 17 keys, against
// 16 with one; with 30 ms between keys, two writes per key either way). The
// number of writes was only a proxy for journaling: the guarantee is that no
// write holds the note whole and that, once the tab is idle after each burst,
// the records hold everything typed so far.
test('typing in a live note journals each change instead of writing the note whole', async ({page}) => {
  const path = 'draft-journal.md',
    initial = '# Live journal\n\n' + 'A long paragraph of the manuscript. '.repeat(6000) + '\n';
  await api('/p/default/api/note', {path, content: initial, revision: ''});
  await login(page);
  await page.goto(origin + '/?project=default&note=' + path);
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await edit(page);
  const user = await api('/api/me'),
    scope = user.id + '/default/' + path;
  await page.evaluate(() => {
    const set = Storage.prototype.setItem;
    window.recordWrites = {draft: [], live: []};
    Storage.prototype.setItem = function (key, value) {
      if (key.startsWith('gitnotes:draft:')) window.recordWrites.draft.push(value.length);
      if (key.startsWith('gitnotes:live:')) window.recordWrites.live.push(value.length);
      return set.call(this, key, value);
    };
  });
  // The text the records recover: the live record's journal folded into the
  // draft record it names.
  const records = () =>
    page.evaluate(scope => {
      const record = JSON.parse(
          localStorage.getItem('gitnotes:draft:@' + sessionStorage.getItem('gitnotes:tab') + '/' + scope)
        ),
        live = record?.recovery && JSON.parse(localStorage.getItem(record.recovery.liveKey));
      if (!live) return null;
      let text = record.content;
      for (const [from, to, insert] of live.changes) text = text.slice(0, from) + insert + text.slice(to);
      return {text, named: live.draft === record.candidateVersion, pending: live.pending, update: live.update};
    }, scope);
  await page.locator('#editor').press(documentEnd);
  // The note's length as each key reaches the page, before the editor sees it.
  await page.evaluate(() => {
    window.lengthAtKey = [];
    addEventListener(
      'keydown',
      () => window.lengthAtKey.push(document.getElementById('editing').markdown.length),
      true
    );
  });
  const typed = 'Typed in bursts.';
  let expected = initial;
  for (const word of typed.split(' ')) {
    await page.keyboard.type(word + ' ');
    expected += word + ' ';
    await expect.poll(async () => (await records())?.text).toBe(expected);
  }
  await expect.poll(async () => (await api('/p/default/api/live?path=' + path)).content).toBe(expected);
  await expect(page.locator('#save-state')).toContainText('Live changes synced');
  // Every key's text was in the note before the next key.
  expect(await page.evaluate(() => window.lengthAtKey)).toEqual(
    Array.from(expected.slice(initial.length), (_, index) => initial.length + index)
  );
  const writes = await page.evaluate(() => window.recordWrites);
  expect(writes.draft.length, 'the draft record was written whole more than once').toBeLessThanOrEqual(1);
  expect(writes.live.length, 'a burst of typing left the live record unwritten').toBeGreaterThanOrEqual(3);
  for (const size of writes.live) expect(size, 'the live record held the note whole').toBeLessThan(2048);
  await expect.poll(records).toEqual({text: expected, named: true, pending: null, update: null});
  // The records rejoin live editing after a reload without reconciliation.
  await page.reload();
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await expect(page.locator('#draft-conflict')).toBeHidden();
  await expectEditorText(page, expected);
});
