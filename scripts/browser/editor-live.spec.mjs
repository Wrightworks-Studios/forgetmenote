import {test, expect} from '@playwright/test';
// Each worker owns an app; baseline reset makes cases independent within it.
test.describe.configure({mode: 'parallel'});
import {watchConfirmations} from './app-fixture.mjs';
import * as Y from 'yjs';
import {
  app,
  origin,
  stored,
  api,
  login,
  edit,
  privateDraft,
  navigateNote,
  shortLivePolls,
  shortLiveWait,
  useEditorApp
} from './editor-fixture.mjs';
import {
  caretAtLineEnd,
  editorText,
  expectEditorText,
  fillSource,
  selectSource,
  sourceSelection
} from './editor-text.mjs';
useEditorApp();
// The text a draft record holds with the changes its live record journals
// after it (internal/web/ui/draft-storage.mjs), from their stored strings.
function journaledText(draftRaw, liveRaw) {
  const record = JSON.parse(draftRaw || 'null'),
    live = JSON.parse(liveRaw || 'null');
  if (typeof record?.content !== 'string') return null;
  let text = record.content;
  if (live?.draft && live.draft === record.candidateVersion)
    for (const [from, to, insert] of live.changes) text = text.slice(0, from) + insert + text.slice(to);
  return text;
}
test('live wire rejects unsafe identifiers and preserves pinned Yjs convergence at safe boundaries', async () => {
  const path = 'collaboration/safe-integer-boundary.md';
  await api('/p/default/api/note', {path, content: '# Safe boundary\n', revision: ''});
  const base = await api('/p/default/api/live?path=' + encodeURIComponent(path)),
    generation = base.liveRevision.split(':')[0];
  const wire = (ids, clockPrefix = 0n) => {
    const bytes = [],
      uint = n => {
        n = BigInt(n);
        while (n > 127n) {
          bytes.push(Number(n & 127n) | 128);
          n >>= 7n;
        }
        bytes.push(Number(n));
      };
    const text = value => {
      const data = Buffer.from(value);
      uint(data.length);
      bytes.push(...data);
    };
    uint(ids.length);
    for (const [i, id] of ids.entries()) {
      uint(clockPrefix ? 2 : 1);
      uint(id);
      uint(0);
      if (clockPrefix) {
        bytes.push(0);
        uint(clockPrefix);
      }
      bytes.push(4);
      uint(1);
      text('markdown');
      text(clockPrefix ? '😀' : String.fromCharCode(88 + i));
    }
    uint(0);
    return Buffer.from(bytes).toString('base64');
  };
  const operationId = 'unsafe-then-safe';
  const response = await fetch(origin + '/p/default/api/live', {
    method: 'POST',
    headers: {Authorization: `Bearer ${app.token}`, 'Content-Type': 'application/json'},
    body: JSON.stringify({path, generation, operationId, update: wire([9007199254740992n, 9007199254740993n])})
  });
  if (response.ok) {
    const accepted = await response.json(),
      doc = new Y.Doc();
    Y.applyUpdate(doc, Buffer.from(accepted.update, 'base64'));
    expect(accepted.content, 'server must never acknowledge content that pinned browser Yjs decodes differently').toBe(
      doc.getText('markdown').toString()
    );
  }
  expect(response.ok, 'unsafe integer update must be rejected').toBe(false);
  expect(await api('/p/default/api/live?path=' + encodeURIComponent(path))).toEqual(base);
  for (const [index, update] of [
    wire([9007199254740991n, 9007199254740990n]),
    wire([42n], 9007199254740989n)
  ].entries()) {
    const accepted = await api('/p/default/api/live', {
      path,
      generation,
      operationId: index ? operationId + '-clock' : operationId,
      update
    });
    const doc = new Y.Doc();
    Y.applyUpdate(doc, Buffer.from(accepted.update, 'base64'));
    expect(doc.getText('markdown').toString()).toBe(accepted.content);
    expect(accepted.content).toContain(index ? '😀' : 'X');
    expect((await api('/p/default/api/live/checkpoint', {path})).content).toBe(accepted.content);
  }
});
for (const failed of [false, true])
  test(
    (failed ? 'failed' : 'delayed') + ' folder navigation keeps displayed live text synchronized before local typing',
    async ({page}) => {
      const path = 'browse-live-' + failed + '/A.md';
      await api('/p/default/api/note', {path, content: '# Folder navigation\n\nOriginal paragraph.\n', revision: ''});
      await login(page);
      await page.goto(origin + '/?project=default&note=' + encodeURIComponent(path));
      await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
      await edit(page);
      let release,
        waiting = false;
      const gate = new Promise(resolve => (release = resolve));
      await page.route('**/api/browse?*', async route => {
        waiting = true;
        await gate;
        if (failed) await route.fulfill({status: 503, json: {error: 'Synthetic folder outage'}});
        else await route.continue();
      });
      try {
        await page.locator('#browse-root').click();
        await expect.poll(() => waiting).toBe(true);
        if (failed) {
          release();
          await expect(page.locator('#notice')).toContainText('Synthetic folder outage');
        }
        await expect(page.locator('#note-path')).toHaveText(path);
        await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
        const before = await api('/p/default/api/live?path=' + encodeURIComponent(path));
        const remote = '\nAccepted remote during folder navigation\n';
        await api('/p/default/api/live/patch', {
          path,
          base: before.liveRevision,
          operationId: 'browse-live-remote-' + failed,
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
        await expectEditorText(page, before.content + remote + 'Local continuation');
      } finally {
        release();
      }
      if (!failed) await expect(page.locator('#folder-browser')).toBeVisible();
    }
  );
for (const failed of [false, true])
  test(
    'stale recovered-note preview ' + (failed ? 'failure' : 'success') + ' cannot disrupt the active live note',
    async ({page}) => {
      const folder = 'preview-navigation-' + failed,
        a = folder + '/A.md',
        b = folder + '/B.md';
      const note = await api('/p/default/api/note', {path: a, content: '# Note A\n', revision: ''});
      await api('/p/default/api/note', {path: b, content: '# Note B\n', revision: ''});
      await login(page);
      await privateDraft(page, note, '# Private draft A\n');
      let release,
        waiting = false,
        finished = false,
        liveStarts = 0;
      const gate = new Promise(resolve => (release = resolve));
      page.on('request', request => {
        const url = new URL(request.url());
        if (
          url.pathname.endsWith('/api/live') &&
          request.method() === 'GET' &&
          url.searchParams.get('path') === b &&
          !url.searchParams.has('since')
        )
          liveStarts++;
      });
      await page.route('**/api/preview', async route => {
        if (!route.request().postDataJSON().content.includes('Private draft A')) return route.continue();
        waiting = true;
        await gate;
        if (failed) await route.fulfill({status: 503, json: {error: 'Stale A preview failed'}});
        else await route.fulfill({json: {html: '<h1>Private draft A</h1>'}});
        finished = true;
      });
      try {
        await page.goto(origin + '/?project=default&note=' + encodeURIComponent(a));
        await expect.poll(() => waiting).toBe(true);
        await watchConfirmations(page, dialog => dialog.accept());
        await navigateNote(page, b);
        await expect(page.locator('#note-path')).toHaveText(b);
        await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
        await edit(page);
        await selectSource(page, 2, 6);
        release();
        await expect.poll(() => finished).toBe(true);
        const base = await api('/p/default/api/live?path=' + encodeURIComponent(b));
        await api('/p/default/api/live/patch', {
          path: b,
          base: base.liveRevision,
          operationId: 'after-stale-preview-' + failed,
          edits: [
            {start: base.content.length, end: base.content.length, old: '', text: '\nRemote B patch after navigation\n'}
          ]
        });
        await expectEditorText(page, '# Note B\n\nRemote B patch after navigation\n');
        await expect(page.locator('#save-state')).not.toContainText('Unsaved on this device · recovered');
        await expect(page.locator('#notice')).toBeHidden();
        await expect(page.locator('#note-path')).toHaveText(b);
        await expect(page).toHaveURL(new RegExp('note=' + encodeURIComponent(b)));
        expect(await sourceSelection(page)).toEqual([2, 6, 'forward']);
        expect(liveStarts).toBe(1);
      } finally {
        release();
      }
    }
  );
for (const unrelated of [false, true])
  test(
    'idle live polling clears a recovered connection notice and preserves unrelated notices ' + unrelated,
    async ({page}) => {
      const path = 'revision/connection-' + unrelated + '.md';
      await api('/p/default/api/note', {path, content: '# Connection recovery\n', revision: ''});
      await login(page);
      await shortLivePolls(page);
      await page.goto(origin + '/?project=default&note=' + encodeURIComponent(path));
      await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
      await page.route('**/api/live?*', route =>
        route.fulfill({
          status: 503,
          contentType: 'application/json',
          body: JSON.stringify({error: 'Synthetic live connection outage'})
        })
      );
      await expect(page.locator('#save-state')).toHaveText('Live connection unavailable');
      await expect(page.locator('#notice')).toContainText('Synthetic live connection outage');
      let release;
      const gate = new Promise(resolve => (release = resolve));
      let waiting = false;
      await page.unroute('**/api/live?*');
      await page.route('**/api/live?*', async route => {
        waiting = true;
        await gate;
        await route.continue({url: shortLiveWait(route.request().url())});
      });
      await expect.poll(() => waiting).toBe(true);
      if (unrelated) {
        await page.route('**/api/upload', route =>
          route.fulfill({
            status: 503,
            contentType: 'application/json',
            body: JSON.stringify({error: 'Synthetic upload unavailable'})
          })
        );
        await page.locator('#upload').setInputFiles({
          name: 'unavailable.txt',
          mimeType: 'text/plain',
          buffer: Buffer.from('Synthetic attachment')
        });
        await expect(page.locator('#notice')).toContainText('Synthetic upload unavailable');
      }
      const response = page.waitForResponse(
        response => response.url().includes('/api/live?') && response.status() === 200
      );
      release();
      expect((await (await response).json()).unchanged).toBe(true);
      await expect(page.locator('#save-state')).toContainText('Saved');
      if (unrelated) await expect(page.locator('#notice')).toContainText('Synthetic upload unavailable');
      else await expect(page.locator('#notice')).toBeHidden();
    }
  );
test('live Yjs rich and source editors converge with agent patches and preserve own undo', async ({page, browser}) => {
  const path = 'collaboration/mixed.md';
  await api('/p/default/api/note', {path, content: '# Coedit\n\nAlpha one.\n\nBeta two.\n', revision: ''});
  const secondContext = await browser.newContext(),
    second = await secondContext.newPage();
  try {
    await login(page);
    await login(second);
    await Promise.all([
      page.goto(origin + '/?project=default&note=' + encodeURIComponent(path)),
      second.goto(origin + '/?project=default&note=' + encodeURIComponent(path))
    ]);
    await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
    await expect(second.locator('#document')).toHaveAttribute('data-live', 'connected');
    await edit(page);
    const later = second.getByRole('button', {name: 'Later', exact: true});
    if (await later.isVisible()) await later.click();
    await second.getByRole('button', {name: 'Edit', exact: true}).click();
    // The other tab edits in Visual, the live preview.
    await expect(second.locator('#editing')).toHaveAttribute('data-format', 'visual');
    const rich = second.locator('#editor');
    await expect(rich).toContainText('Beta two.');
    const alpha = (await editorText(page)).indexOf('Alpha');
    await selectSource(page, alpha, alpha + 5);
    await page.locator('#editor').pressSequentially('ALPHA');
    await rich.getByText('Beta two.', {exact: true}).click();
    await second.keyboard.press('End');
    await second.keyboard.type(' BROWSER');
    await expect.poll(() => editorText(page)).toContain('BROWSER');
    await expect(rich).toContainText('ALPHA one.');
    const base = await api('/p/default/api/live?path=' + encodeURIComponent(path));
    const start = base.content.indexOf('Coedit');
    await api('/p/default/api/live/patch', {
      path,
      base: base.liveRevision,
      operationId: 'browser-agent',
      edits: [{start, end: start + 6, old: 'Coedit', text: 'Collaborative'}]
    });
    await expect(rich).toContainText('Collaborative');
    await expect.poll(() => editorText(page)).toContain('Collaborative');
    await rich.focus();
    await second.keyboard.press('Control+z');
    await expect.poll(() => editorText(page)).not.toContain('BROWSER');
    await expect(rich).toContainText('ALPHA one.');
    await expect(rich).toContainText('Collaborative');
  } finally {
    await secondContext.close();
  }
});
// The editors keep no history, so the Edit menu's and the context menu's Undo
// (beforeinput historyUndo/historyRedo) run the note's undo, as the keys do.
for (const format of ['source', 'visual'])
  test('the browser Undo command in ' + format + ' runs this user’s live undo', async ({page}) => {
    const path = 'collaboration/browser-undo-' + format + '.md',
      content = '# Undo\n\nFirst words.\n';
    await api('/p/default/api/note', {path, content, revision: ''});
    await login(page);
    await page.goto(origin + '/?project=default&note=' + encodeURIComponent(path));
    await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
    await edit(page);
    if (format === 'visual') {
      await page.locator('#format-visual').click();
      await caretAtLineEnd(page, 'First words.');
    } else await page.locator('#editor').press('Control+End');
    await page.keyboard.type(' More');
    await expect.poll(() => editorText(page)).toContain('More');
    const run = inputType =>
      page
        .locator('#editor')
        .evaluate(
          (element, inputType) =>
            element.dispatchEvent(new InputEvent('beforeinput', {inputType, bubbles: true, cancelable: true})),
          inputType
        );
    // Handled: the browser's own undo is cancelled.
    expect(await run('historyUndo')).toBe(false);
    await expectEditorText(page, content);
    expect(await run('historyRedo')).toBe(false);
    await expect.poll(() => editorText(page)).toContain('More');
  });
test('Undo changes nothing once a refused live update makes the editor read-only', async ({page}) => {
  const path = 'collaboration/refused-undo.md';
  await api('/p/default/api/note', {path, content: '# Refused\n\nKept text.\n', revision: ''});
  await login(page);
  await page.goto(origin + '/?project=default&note=' + encodeURIComponent(path));
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await edit(page);
  await page.locator('#editor').press('Control+End');
  await page.keyboard.type('Typed before.');
  await expect.poll(async () => (await api('/p/default/api/live?path=' + path)).content).toContain('Typed before.');
  await page.route('**/api/live', route =>
    route.request().method() === 'POST'
      ? route.fulfill({status: 403, json: {error: 'Editing access was removed.'}})
      : route.continue()
  );
  // One input, so all of it lands before the refusal makes the note read-only.
  await page.keyboard.insertText(' Refused.');
  await expect(page.locator('#editor')).toHaveAttribute('contenteditable', 'false');
  const kept = await editorText(page);
  expect(kept).toContain('Typed before. Refused.');
  // Neither the keys nor the browser's Undo command change a read-only note
  // (each sent to the editor, as if it had kept the focus).
  for (const event of [
    {type: 'keydown', key: 'z', ctrlKey: true},
    {type: 'keydown', key: 'y', ctrlKey: true},
    {type: 'beforeinput', inputType: 'historyUndo'}
  ]) {
    await page.locator('#editor').evaluate((element, {type, ...init}) => {
      const Event = type === 'keydown' ? KeyboardEvent : InputEvent;
      element.dispatchEvent(new Event(type, {...init, bubbles: true, cancelable: true}));
    }, event);
    expect(await editorText(page), JSON.stringify(event)).toBe(kept);
  }
});
test('JavaScript Yjs binary state round-trips through Go and survives concurrent updates', async () => {
  const path = 'collaboration/interop.md';
  await api('/p/default/api/note', {path, content: '😀 alpha\n\nbeta\n', revision: ''});
  const base = await api('/p/default/api/live?path=' + encodeURIComponent(path));
  const a = new Y.Doc(),
    b = new Y.Doc();
  for (const doc of [a, b]) Y.applyUpdate(doc, Buffer.from(base.update, 'base64'));
  a.getText('markdown').insert(3, 'A ');
  b.getText('markdown').insert(10, 'B ');
  for (const [operationId, doc] of [
    ['js-a', a],
    ['js-b', b]
  ])
    await api('/p/default/api/live', {
      path,
      generation: base.liveRevision.split(':')[0],
      operationId,
      update: Buffer.from(Y.encodeStateAsUpdate(doc)).toString('base64')
    });
  const merged = await api('/p/default/api/live?path=' + encodeURIComponent(path));
  const c = new Y.Doc();
  Y.applyUpdate(c, Buffer.from(merged.update, 'base64'));
  expect(c.getText('markdown').toString()).toBe('😀 A alpha\n\nB beta\n');
  expect(merged.content).toBe(c.getText('markdown').toString());
});
test('checkpoint readback never replaces a newer live edit received while saving', async ({page}) => {
  const path = 'collaboration/checkpoint-race.md';
  await api('/p/default/api/note', {path, content: '# Checkpoint\n\nOriginal paragraph.\n', revision: ''});
  await login(page);
  await page.goto(origin + '/?project=default&note=' + encodeURIComponent(path));
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await edit(page);
  await page.locator('#editor').press('Control+End');
  await page.keyboard.type('\nMy change.');
  let intercepted = false;
  await page.route('**/api/note?path=collaboration%2Fcheckpoint-race.md', async route => {
    const response = await route.fetch();
    const current = await api('/p/default/api/live?path=' + encodeURIComponent(path));
    const start = current.content.indexOf('Original');
    await api('/p/default/api/live/patch', {
      path,
      base: current.liveRevision,
      operationId: 'during-save',
      edits: [{start, end: start + 8, old: 'Original', text: 'REMOTE'}]
    });
    await expect.poll(() => editorText(page)).toContain('REMOTE');
    intercepted = true;
    await route.fulfill({response});
  });
  await page.locator('#save').click();
  await expect.poll(() => intercepted).toBe(true);
  await expect(page.locator('#save-state')).toContainText('Live changes synced');
  await expectEditorText(page, '# Checkpoint\n\nREMOTE paragraph.\n\nMy change.');
  await expect(page.locator('#save')).toBeEnabled();
  const user = await api('/api/me'),
    scope = user.id + '/default/' + path,
    text = '# Checkpoint\n\nREMOTE paragraph.\n\nMy change.';
  await expect
    .poll(
      async () =>
        journaledText(await stored(page, 'gitnotes:draft:' + scope), await stored(page, 'gitnotes:live:' + scope)),
      {message: 'newer pending text remains reachable after Save resolves the prior recovery record'}
    )
    .toBe(text);
  await page.unroute('**/api/note?path=collaboration%2Fcheckpoint-race.md');
  await page.reload();
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await edit(page);
  await expectEditorText(page, text);
  await expect(page.locator('#save-state')).toContainText('Live changes synced');
  await expect(page.locator('#save')).toBeEnabled();
});
test('typing renders the collection badge once typing pauses and keeps the status line still', async ({page}) => {
  const path = 'collaboration/typing-pace.md';
  await api('/p/default/api/note', {path, content: '---\nfmn-container: archive\n---\n# Archived\n', revision: ''});
  await login(page);
  await page.goto(origin + '/?project=default&note=' + encodeURIComponent(path));
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await edit(page);
  await expect(page.locator('#container-badge')).toHaveText('Archive');
  // Each render of the badge replaces it. Each change of the status line is
  // kept with its time and text.
  await page.evaluate(() => {
    window.renders = {badge: 0, since: performance.now(), states: []};
    const badge = document.getElementById('container-badge');
    new MutationObserver(records => {
      for (const record of records)
        if ([...record.addedNodes].some(node => node.id === 'container-badge')) window.renders.badge++;
    }).observe(badge.parentNode, {childList: true});
    const line = document.getElementById('save-state');
    new MutationObserver(() => window.renders.states.push({at: performance.now(), text: line.textContent})).observe(
      line,
      {
        childList: true,
        characterData: true,
        subtree: true
      }
    );
  });
  const start = (await editorText(page)).indexOf('archive');
  await selectSource(page, start, start + 'archive'.length);
  const began = Date.now();
  await page.keyboard.type('draft');
  await page.locator('#editor').press('Control+End');
  const sentence = ' Typed quickly, one key at a time.';
  await page.keyboard.type(sentence);
  const seconds = (Date.now() - began) / 1000;
  await expect(page.locator('#container-badge')).toHaveText('Archive → Shared draft · pending Save');
  const renders = await page.evaluate(() => window.renders);
  // Once a second while typing, once when it pauses, and once when the
  // note first has live changes pending: never at each of the keys.
  expect(renders.badge, 'badge renders while typing ' + seconds + ' s').toBeLessThanOrEqual(Math.ceil(seconds) + 2);
  expect(renders.badge).toBeLessThan(('draft' + sentence).length / 4);
  // The line says the changes are synced, and changes only for that, at most
  // three times. It says they are being shared only once a send has had no
  // answer for a second, during which the line was still (900 ms allows for
  // when the observer runs); a loaded host can take that second more than
  // once, and each later time adds at most two changes.
  const sharing = renders.states.filter(({text}) => text.startsWith('Sharing changes'));
  renders.states.forEach(({at, text}, i) => {
    if (text.startsWith('Sharing changes'))
      expect(at - (renders.states[i - 1]?.at ?? renders.since), 'a still line before ' + text).toBeGreaterThanOrEqual(
        900
      );
  });
  expect(renders.states.length, JSON.stringify(renders.states)).toBeLessThanOrEqual(
    3 + 2 * Math.max(0, sharing.length - 1)
  );
  await expect(page.locator('#save-state')).toContainText('Live changes synced');
  // A send that takes a second says so.
  const held = [];
  await page.route('**/api/live', route =>
    route.request().method() === 'POST' && !held.released ? held.push(route) : route.fallback()
  );
  await page.keyboard.type('!');
  await expect(page.locator('#save-state')).toContainText('Sharing changes');
  held.released = true;
  await Promise.all(held.map(route => route.continue()));
  await expect(page.locator('#save-state')).toContainText('Live changes synced');
  await expect
    .poll(async () => (await api('/p/default/api/live?path=' + encodeURIComponent(path))).content)
    .toBe('---\nfmn-container: draft\n---\n# Archived\n' + sentence + '!');
});
test('live edits survive idle disconnect and server restart until explicit Save creates one Git version', async ({
  page
}) => {
  const path = 'collaboration/explicit-save.md',
    initial = '# Explicit Save\n',
    content = initial + 'Durable live text.\n';
  const saved = await api('/p/default/api/note', {path, content: initial, revision: ''});
  // The notebook's newest revisions, newest first, from its history, and how
  // many are newer than a revision.
  const revisions = async () => (await api('/p/default/api/history')).map(commit => commit.revision),
    head = async () => (await revisions())[0],
    newer = async revision => (await revisions()).indexOf(revision);
  const before = await head();
  expect(before).toBe(saved.revision);
  await login(page);
  await page.goto(origin + '/?project=default&note=' + encodeURIComponent(path));
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await edit(page);
  await fillSource(page, content);
  await expect
    .poll(async () => (await api('/p/default/api/live?path=' + encodeURIComponent(path))).content)
    .toBe(content);
  await page.waitForTimeout(2500);
  expect(await head()).toBe(before);
  await page.goto('about:blank');
  await page.waitForTimeout(2500);
  expect(await head()).toBe(before);
  await app.restart();
  expect(await head()).toBe(before);
  const recovered = await api('/p/default/api/live?path=' + encodeURIComponent(path));
  expect(recovered.content).toBe(content);
  expect(recovered.pending).toBe(true);
  expect((await api('/p/default/api/note?path=' + encodeURIComponent(path))).content).toBe(initial);
  expect(await head()).toBe(before);
  await page.goto(origin + '/?project=default&note=' + encodeURIComponent(path));
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await edit(page);
  await expectEditorText(page, content);
  await expect(page.locator('#save-state')).toContainText('Live changes synced');
  await expect(page.locator('#save-state')).toContainText('Save to create a version');
  await page.locator('#save').click();
  await expect(page.locator('#save-state')).toContainText('Saved');
  const committed = await api('/p/default/api/note?path=' + encodeURIComponent(path));
  expect(committed.content).toBe(content);
  expect(committed.revision).not.toBe(before);
  await api('/p/default/api/live/checkpoint', {path});
  expect(await head()).toBe(committed.revision);
  expect(await newer(before)).toBe(1);
  // Returning to committed text still leaves accepted operation receipts to save.
  await fillSource(page, content + 'Temporary edit');
  await expect
    .poll(async () => (await api('/p/default/api/live?path=' + encodeURIComponent(path))).content)
    .toBe(content + 'Temporary edit');
  await fillSource(page, content);
  await expect
    .poll(async () => (await api('/p/default/api/live?path=' + encodeURIComponent(path))).content)
    .toBe(content);
  await expect(page.locator('#save-state')).toContainText('Live changes synced');
  await expect(page.locator('#save')).toBeEnabled();
  expect(await head()).toBe(committed.revision);
  await page.locator('#save').click();
  await expect(page.locator('#save-state')).toContainText('Saved');
  expect(await newer(committed.revision)).toBe(1);
});
for (const composition of [false, true])
  test('live recovery initialization preserves new ' + (composition ? 'composition' : 'typing'), async ({page}) => {
    await watchConfirmations(page, d => d.accept());
    const path = 'revision/recovery-' + composition + '.md',
      initial = '# Base\n\nRetained draft.\n';
    await api('/p/default/api/note', {path, content: '# Base\n', revision: ''});
    await login(page);
    await page.goto(origin + '/?project=default&note=' + encodeURIComponent(path));
    await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
    await edit(page);
    await page.route('**/api/live', route => route.abort());
    await fillSource(page, initial);
    await expect
      .poll(() => page.evaluate(() => Object.keys(localStorage).some(k => k.startsWith('gitnotes:live:'))))
      .toBe(true);
    let release;
    const gate = new Promise(resolve => (release = resolve));
    let started = false;
    await page.route('**/api/live?*', async route => {
      started = true;
      await gate;
      await route.continue();
    });
    await page.reload();
    await expect.poll(() => started).toBe(true);
    await expect(page.locator('#save-state')).toContainText('Unsaved on this device · recovered');
    await page.locator('#format-source').click();
    if (composition) await page.locator('#editor').dispatchEvent('compositionstart');
    await fillSource(page, initial + 'New composition');
    release();
    await expect(page.locator('#notice')).toContainText('private draft');
    if (composition) await page.locator('#editor').dispatchEvent('compositionend');
    await expectEditorText(page, initial + 'New composition');
    await page.reload();
    await expectEditorText(page, initial + 'New composition');
  });
test('discard retires unsent live payload across retries and reload', async ({page}) => {
  await watchConfirmations(page, d => d.accept());
  const path = 'revision/discard.md';
  await api('/p/default/api/note', {path, content: '# Initial\n', revision: ''});
  await login(page);
  await page.goto(origin + '/?project=default&note=' + encodeURIComponent(path));
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await edit(page);
  await page.route('**/api/live', route => route.abort());
  await fillSource(page, '# Initial\n\nDISCARD THIS\n');
  const base = await api('/p/default/api/live?path=' + encodeURIComponent(path));
  await api('/p/default/api/live/patch', {
    path,
    base: base.liveRevision,
    operationId: 'discard-peer',
    edits: [{start: 2, end: 9, old: 'Initial', text: 'Peer heading'}]
  });
  await api('/p/default/api/live/checkpoint', {path});
  await page.reload();
  await expect(page.locator('#draft-compare')).toBeVisible();
  await page.locator('#draft-compare').click();
  await page.locator('#reconcile-discard').click();
  await expect(page.locator('#reconcile-dialog')).toBeHidden();
  await page.unroute('**/api/live');
  await expectEditorText(page, '# Peer heading\n');
  await page.reload();
  await expectEditorText(page, '# Peer heading\n');
  expect((await api('/p/default/api/live?path=' + encodeURIComponent(path))).content).toBe('# Peer heading\n');
});
test('source selection remains in unchanged middle across disjoint live edits', async ({page}) => {
  const path = 'revision/selection.md',
    content = 'Top\n\nMiddle paragraph\n\nTail\n';
  await api('/p/default/api/note', {path, content, revision: ''});
  await login(page);
  await page.goto(origin + '/?project=default&note=' + encodeURIComponent(path));
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await edit(page);
  await selectSource(page, 8, 15, 'backward');
  const base = await api('/p/default/api/live?path=' + encodeURIComponent(path)),
    tail = content.indexOf('Tail');
  await api('/p/default/api/live/patch', {
    path,
    base: base.liveRevision,
    operationId: 'both-sides',
    edits: [
      {start: 0, end: 3, old: 'Top', text: 'LONG TOP'},
      {start: tail, end: tail + 4, old: 'Tail', text: 'LONG TAIL'}
    ]
  });
  await expect.poll(() => editorText(page)).toContain('LONG TAIL');
  expect(await sourceSelection(page)).toEqual([13, 20, 'backward']);
  await selectSource(page, 17);
  const next = await api('/p/default/api/live?path=' + encodeURIComponent(path)),
    last = next.content.indexOf('LONG TAIL');
  await api('/p/default/api/live/patch', {
    path,
    base: next.liveRevision,
    operationId: 'both-sides-again',
    edits: [
      {start: 0, end: 8, old: 'LONG TOP', text: 'Top'},
      {start: last, end: last + 9, old: 'LONG TAIL', text: 'Tail'}
    ]
  });
  await expectEditorText(page, content);
  expect(await sourceSelection(page)).toEqual([12, 12, 'none']);
});
test('visual selection remains in unchanged middle across disjoint live edits', async ({page}) => {
  const path = 'revision/visual-selection.md',
    content = 'Top\n\nMiddle paragraph\n\nTail\n';
  await api('/p/default/api/note', {path, content, revision: ''});
  await login(page);
  await page.goto(origin + '/?project=default&note=' + encodeURIComponent(path));
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await edit(page);
  await page.locator('#format-visual').click();
  const middle = page.locator('#editor .cm-line').filter({hasText: 'Middle paragraph'});
  await expect(middle).toBeVisible();
  await middle.evaluate(p => {
    const selection = getSelection(),
      range = document.createRange();
    range.setStart(p.firstChild, 3);
    range.setEnd(p.firstChild, 9);
    selection.removeAllRanges();
    selection.addRange(range);
    document.dispatchEvent(new Event('selectionchange'));
  });
  const base = await api('/p/default/api/live?path=' + encodeURIComponent(path)),
    tail = content.indexOf('Tail');
  await api('/p/default/api/live/patch', {
    path,
    base: base.liveRevision,
    operationId: 'visual-both-sides',
    edits: [
      {start: 0, end: 3, old: 'Top', text: 'LONG TOP'},
      {start: tail, end: tail + 4, old: 'Tail', text: 'LONG TAIL'}
    ]
  });
  await expect(page.locator('#editor')).toContainText('LONG TAIL');
  expect(
    await page.evaluate(() => {
      const s = getSelection();
      return [s.anchorNode?.textContent, s.anchorOffset, s.focusNode?.textContent, s.focusOffset];
    })
  ).toEqual(['Middle paragraph', 3, 'Middle paragraph', 9]);
});
test('discard keeps accepted changes when the update acknowledgement was lost', async ({page}) => {
  await watchConfirmations(page, d => d.accept());
  const path = 'revision/accepted-discard.md',
    content = '# Initial\n\nALREADY SHARED\n';
  await api('/p/default/api/note', {path, content: '# Initial\n', revision: ''});
  await login(page);
  await page.goto(origin + '/?project=default&note=' + encodeURIComponent(path));
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await edit(page);
  let requests = 0;
  await page.route('**/api/live', async route => {
    requests++;
    await route.fetch();
    await route.abort();
  });
  await fillSource(page, content);
  await expect
    .poll(async () => (await api('/p/default/api/live?path=' + encodeURIComponent(path))).content)
    .toBe(content);
  const base = await api('/p/default/api/live?path=' + encodeURIComponent(path));
  await api('/p/default/api/live/patch', {
    path,
    base: base.liveRevision,
    operationId: 'accepted-discard-peer',
    edits: [{start: 2, end: 9, old: 'Initial', text: 'Peer'}]
  });
  await api('/p/default/api/live/checkpoint', {path});
  await page.reload();
  await expect(page.locator('#draft-compare')).toBeVisible();
  await page.locator('#draft-compare').click();
  await expect(page.locator('#reconcile-latest')).toHaveValue('# Peer\n\nALREADY SHARED\n');
  await page.locator('#reconcile-discard').click();
  await expect(page.locator('#reconcile-dialog')).toBeHidden();
  const stopped = requests,
    head = (await api('/p/default/api/note?path=' + encodeURIComponent(path))).revision;
  await page.reload();
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await expectEditorText(page, '# Peer\n\nALREADY SHARED\n');
  expect(requests).toBe(stopped);
  expect((await api('/p/default/api/note?path=' + encodeURIComponent(path))).revision).toBe(head);
});
for (const role of ['read', 'comment'])
  test(role + ' viewers render live changes without gaining write access', async ({browser}) => {
    const path = 'revision/' + role + '.md',
      password = 'DisposableLiveReaderPassword123!',
      username = 'live-viewer-' + role;
    await api('/p/default/api/note', {path, content: '# Before\n', revision: ''});
    const user = await api('/api/local-users', {username, password});
    await api('/api/members', {project: 'default', user: user.id, role});
    const context = await browser.newContext(),
      page = await context.newPage();
    try {
      await page.request.post(origin + '/auth/password/login', {form: {username, password}, headers: {Origin: origin}});
      await page.goto(origin + '/?project=default&note=' + encodeURIComponent(path));
      await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
      const base = await api('/p/default/api/live?path=' + encodeURIComponent(path));
      await api('/p/default/api/live/patch', {
        path,
        base: base.liveRevision,
        operationId: 'viewer-' + role,
        edits: [{start: 2, end: 8, old: 'Before', text: 'Live after'}]
      });
      await expect(page.locator('#preview')).toContainText('Live after');
      await expect(page.locator('#mode')).toBeDisabled();
      const denied = await page.request.post(origin + '/p/default/api/live/patch', {
        data: {path, base: base.liveRevision, operationId: 'denied', edits: []},
        headers: {Origin: origin}
      });
      expect(denied.status()).toBe(403);
    } finally {
      await context.close();
    }
  });
for (const backToOriginal of [false, true])
  test(
    'reload retains composition newer than the saved live payload ' +
      (backToOriginal ? 'when restored to original text' : 'for reconciliation'),
    async ({page}) => {
      await watchConfirmations(page, dialog => dialog.accept());
      const path = 'revision/composition-recovery-' + backToOriginal + '.md',
        initial = '# Base\n\nRetained unsent text.\n',
        composed = backToOriginal ? '# Base\n' : initial + 'NEW COMPOSITION';
      await api('/p/default/api/note', {path, content: '# Base\n', revision: ''});
      await login(page);
      await page.goto(origin + '/?project=default&note=' + encodeURIComponent(path));
      await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
      await edit(page);
      await page.route('**/api/live', route => route.abort());
      await fillSource(page, initial);
      await expect
        .poll(() => page.evaluate(() => Object.keys(localStorage).some(key => key.startsWith('gitnotes:live:'))))
        .toBe(true);
      await page.locator('#editor').dispatchEvent('compositionstart');
      await fillSource(page, composed);
      // The draft records' texts, with what live records journal after them.
      const drafts = () =>
        page.evaluate(() =>
          Object.entries(localStorage)
            .filter(([key]) => key.startsWith('gitnotes:draft:'))
            .map(([, value]) => {
              const record = JSON.parse(value),
                live = JSON.parse(localStorage.getItem(record.recovery?.liveKey) || 'null');
              let text = record.content;
              if (live?.draft && live.draft === record.candidateVersion)
                for (const [from, to, insert] of live.changes) text = text.slice(0, from) + insert + text.slice(to);
              return text;
            })
        );
      // Typing reaches the draft record, or the live record's journal of it.
      await expect.poll(drafts).toContain(composed);
      await page.reload();
      await expect
        .poll(() =>
          page.evaluate(
            () =>
              document.getElementById('document').dataset.live === 'connected' ||
              document.getElementById('notice').textContent.includes('private changes')
          )
        )
        .toBe(true);
      await expectEditorText(page, composed);
      await expect(page.locator('#draft-compare')).toBeVisible();
      expect(await drafts()).toContain(composed);
      await page.reload();
      await expect(page.locator('#draft-compare')).toBeVisible();
      await expectEditorText(page, composed);
      await page.locator('#draft-compare').click();
      await expect(page.locator('#reconcile-content')).toHaveValue(composed);
      expect((await api('/p/default/api/live?path=' + encodeURIComponent(path))).content).toBe('# Base\n');
    }
  );
for (const operation of ['insert', 'delete'])
  test('visual selection survives preceding paragraph ' + operation, async ({page}) => {
    const path = 'revision/visual-structure-' + operation + '.md',
      prefix = 'First paragraph\n\n',
      content = prefix + 'Top\n\nMiddle paragraph\n\nTail\n';
    await api('/p/default/api/note', {path, content, revision: ''});
    await login(page);
    await page.goto(origin + '/?project=default&note=' + encodeURIComponent(path));
    await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
    await edit(page);
    await page.locator('#format-visual').click();
    const middle = page.locator('#editor .cm-line').filter({hasText: 'Middle paragraph'});
    await expect(middle).toBeVisible();
    await middle.evaluate(p => {
      const selection = getSelection(),
        range = document.createRange();
      range.setStart(p.firstChild, 3);
      range.setEnd(p.firstChild, 9);
      selection.removeAllRanges();
      selection.addRange(range);
      document.dispatchEvent(new Event('selectionchange'));
    });
    const base = await api('/p/default/api/live?path=' + encodeURIComponent(path)),
      editRange =
        operation === 'insert'
          ? {start: 0, end: 0, old: '', text: 'New first paragraph\n\n'}
          : {start: 0, end: prefix.length, old: prefix, text: ''};
    await api('/p/default/api/live/patch', {
      path,
      base: base.liveRevision,
      operationId: 'preceding-' + operation,
      edits: [editRange]
    });
    await expect
      .poll(() => editorText(page))
      .toBe(operation === 'insert' ? 'New first paragraph\n\n' + content : content.slice(prefix.length));
    expect(
      await page.evaluate(() => {
        const selection = getSelection();
        return [
          selection.anchorNode?.textContent,
          selection.anchorOffset,
          selection.focusNode?.textContent,
          selection.focusOffset
        ];
      })
    ).toEqual(['Middle paragraph', 3, 'Middle paragraph', 9]);
  });
for (const role of ['read', 'comment'])
  test(role + ' initial live hydration renders pending preview and checkpoint', async ({browser}) => {
    const path = 'revision/initial-' + role + '.md',
      password = 'DisposableInitialPreviewPassword123!',
      username = 'initial-viewer-' + role;
    await api('/p/default/api/note', {path, content: '# Committed\n', revision: ''});
    const user = await api('/api/local-users', {username, password});
    await api('/api/members', {project: 'default', user: user.id, role});
    const context = await browser.newContext(),
      page = await context.newPage();
    try {
      await page.request.post(origin + '/auth/password/login', {form: {username, password}, headers: {Origin: origin}});
      await page.route('**/api/note?path=' + encodeURIComponent(path), async route => {
        const response = await route.fetch(),
          base = await api('/p/default/api/live?path=' + encodeURIComponent(path));
        await api('/p/default/api/live/patch', {
          path,
          base: base.liveRevision,
          operationId: 'initial-preview-' + role,
          edits: [{start: 2, end: 11, old: 'Committed', text: 'Shared pending'}]
        });
        await route.fulfill({response});
      });
      await page.goto(origin + '/?project=default&note=' + encodeURIComponent(path));
      await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
      await expectEditorText(page, '# Shared pending\n');
      await expect(page.locator('#preview')).toContainText('Shared pending');
      await api('/p/default/api/live/checkpoint', {path});
      await expect(page.locator('#save-state')).toContainText('Saved');
      await expect(page.locator('#preview')).toContainText('Shared pending');
      await expect(page.locator('#mode')).toBeDisabled();
    } finally {
      await context.close();
    }
  });
