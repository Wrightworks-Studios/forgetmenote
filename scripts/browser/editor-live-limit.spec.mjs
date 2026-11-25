// Live editing takes a note up to the server's note size limit
// (FMN_LIMIT_FILE_BYTES, 32 MiB by default). This app sets it to 1 MiB, so a
// browser can cross it: the server refuses the update, and the tab keeps its
// text as a private draft to correct and save.
import {test, expect} from '@playwright/test';
import {watchConfirmations} from './app-fixture.mjs';
import * as Y from 'yjs';
import {origin, retainedTexts, api, login, edit, useEditorApp} from './editor-fixture.mjs';
import {expectEditorText, fillSource} from './editor-text.mjs';
useEditorApp({env: {FMN_LIMIT_FILE_BYTES: String(1 << 20)}});
test('UTF8 live overflow refused by the server retains a private draft and can be corrected and saved', async ({
  page
}) => {
  // Still 6 bytes under the limit, as short words: Chromium takes 10-20 s to
  // wrap one 512K-character word in the reading view after the reload.
  const path = 'utf8-preflight.md',
    initial = '# Bytes\n\n' + 'é '.repeat(((1 << 20) - 16) / 3) + '\n',
    oversized = initial + '界'.repeat(4),
    corrected = initial + 'ok';
  const created = await api('/p/default/api/note', {path, content: initial, revision: ''});
  await login(page);
  await page.goto(origin + '/?project=default&note=' + path);
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await edit(page);
  let updates = 0;
  page.on('request', request => {
    if (request.method() === 'POST' && request.url().endsWith('/api/live')) updates++;
  });
  // Sending and refusing a 1 MiB note takes seconds at 4x or under a busy
  // suite: the server's refusal is what the page waits for.
  const refused = page.waitForResponse(
    response => response.request().method() === 'POST' && new URL(response.url()).pathname.endsWith('/api/live')
  );
  await fillSource(page, oversized);
  expect((await refused).ok()).toBe(false);
  await expect(page.locator('#draft-compare')).toBeVisible();
  await expect(page.locator('#document')).not.toHaveAttribute('data-live', 'connected');
  await expectEditorText(page, oversized);
  // The live limit is the server's: its one refused update shared nothing.
  expect(updates).toBe(1);
  expect(await retainedTexts(page, path)).toContain(oversized);
  expect((await api('/p/default/api/live?path=' + path)).content).toBe(initial);
  expect((await api('/p/default/api/note?path=' + path)).revision).toBe(created.revision);
  await fillSource(page, corrected);
  await page.locator('#save').click();
  await expect(page.locator('#save-state')).toContainText('Saved');
  expect((await api('/p/default/api/note?path=' + path)).content).toBe(corrected);
  const reloadSnapshot = page.waitForResponse(response => {
    const url = new URL(response.url());
    return (
      url.pathname === '/p/default/api/live' && url.searchParams.get('path') === path && !url.searchParams.has('since')
    );
  });
  await page.reload();
  const snapshotResponse = await reloadSnapshot;
  expect(snapshotResponse.status()).toBe(200);
  await snapshotResponse.finished();
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await expectEditorText(page, corrected);
});
test('a recovered oversized retry retires without losing a newer corrected private draft', async ({page}) => {
  await watchConfirmations(page, dialog => dialog.accept());
  const path = 'utf8-recovered-retry.md',
    initial = '# Bytes\n\n' + 'é'.repeat((1 << 19) - 8) + '\n',
    corrected = initial + 'ok';
  await api('/p/default/api/note', {path, content: initial, revision: ''});
  const snapshot = await api('/p/default/api/live?path=' + path),
    user = await api('/api/me');
  const doc = new Y.Doc();
  Y.applyUpdate(doc, Buffer.from(snapshot.update, 'base64'));
  const text = doc.getText('markdown');
  text.insert(text.length, '界'.repeat(4));
  const generation = snapshot.liveRevision.split(':')[0],
    pending = {
      path,
      generation,
      operationId: 'old-recovered-oversized-operation',
      update: Buffer.from(Y.encodeStateAsUpdate(doc)).toString('base64')
    };
  text.delete(initial.length, 4);
  text.insert(text.length, 'ok');
  const recovery = {
    generation,
    content: corrected,
    update: Buffer.from(Y.encodeStateAsUpdate(doc)).toString('base64'),
    pending
  };
  doc.destroy();
  await login(page);
  await page.goto(origin + '/login');
  await page.evaluate(
    ({path, user, snapshot, corrected, recovery}) => {
      const scope = user.id + '/default/' + path;
      localStorage.setItem(
        'gitnotes:draft:' + scope,
        JSON.stringify({content: corrected, revision: snapshot.revision, version: snapshot.version})
      );
      localStorage.setItem('gitnotes:live:' + scope, JSON.stringify(recovery));
    },
    {path, user, snapshot, corrected, recovery}
  );
  let updates = 0;
  page.on('request', request => {
    if (request.method() === 'POST' && request.url().endsWith('/api/live')) updates++;
  });
  // Opening the 1 MiB note and replaying its retry takes about 4 s at 4x
  // CPU, most of it before the replay is sent: wait for the server to refuse
  // it rather than for the page within the default timeout.
  const refused = page.waitForResponse(
    response => response.request().method() === 'POST' && new URL(response.url()).pathname.endsWith('/api/live')
  );
  await page.goto(origin + '/?project=default&note=' + path);
  expect((await refused).status()).toBe(422);
  await expect(page.locator('#draft-compare')).toBeVisible();
  await expect(page.locator('#notice')).toContainText('FMN_LIMIT_FILE_BYTES');
  await page.locator('#format-source').click();
  await expectEditorText(page, corrected);
  // The server refused the one replayed retry.
  expect(updates).toBe(1);
  expect(await retainedTexts(page, path)).toContain(corrected);
  expect(
    await page.evaluate(
      scope =>
        JSON.parse(localStorage.getItem('gitnotes:live:@' + sessionStorage.getItem('gitnotes:tab') + '/' + scope)),
      user.id + '/default/' + path
    )
  ).toBeNull();
  const before = await api('/p/default/api/live?path=' + path);
  expect(before.content).toBe(initial);
  const peer = await api('/p/default/api/live/patch', {
    path,
    base: before.liveRevision,
    operationId: 'utf8-recovery-peer',
    edits: [{start: 2, end: 7, old: 'Bytes', text: 'Peers'}]
  });
  // Reconcile against a stable Git base; ordinary Save must reject a checkpoint
  // that advances the reviewed revision between Apply and Save.
  await api('/p/default/api/live/checkpoint', {path});
  await page.reload();
  await expectEditorText(page, corrected);
  await expect(page.locator('#draft-compare')).toBeVisible();
  expect(updates).toBe(1);
  await page.locator('#draft-compare').click();
  await expect(page.locator('#reconcile-latest')).toHaveValue(peer.content);
  await expect(page.locator('#reconcile-content')).toHaveValue(corrected);
  const combined = corrected.replace('Bytes', 'Peers');
  await page.locator('#reconcile-content').fill(combined);
  await page.locator('#reconcile-apply').click();
  await expect(page.locator('#reconcile-dialog')).toBeHidden();
  expect((await api('/p/default/api/live?path=' + path)).content).toBe(peer.content);
  const saving = page.waitForResponse(
    response => response.request().method() === 'POST' && response.url().endsWith('/api/note')
  );
  await page.locator('#save').click();
  const result = await saving;
  expect(result.status(), await result.text()).toBe(200);
  await expect(page.locator('#save-state')).toContainText('Saved');
  const committed = await api('/p/default/api/note?path=' + path);
  expect(committed.content).toBe(combined);
  expect((await api('/p/default/api/note?path=' + path + '&revision=' + committed.revision)).content).toBe(combined);
});
