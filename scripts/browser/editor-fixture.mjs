// Shared app, helpers and hooks for the editor-*.spec.mjs suites. Each worker
// owns one app; the per-test baseline reset keeps cases independent.
import {test, expect} from '@playwright/test';
import {startApp} from './app-fixture.mjs';
import {expectEditorText} from './editor-text.mjs';
export let app, dir, origin;
export const documentEnd = process.platform === 'darwin' ? 'Meta+ArrowDown' : 'Control+End';
export async function stored(page, key) {
  return page.evaluate(key => {
    const [, kind, scope] = /^gitnotes:(draft|live|reconcile):(.*)$/.exec(key),
      owner = sessionStorage.getItem('gitnotes:tab'),
      base = '@' + owner + '/' + scope,
      raw = localStorage.getItem('gitnotes:draft:' + base);
    let record;
    try {
      record = JSON.parse(raw);
    } catch {}
    if (record?.recovery?.schema === 1) {
      if (kind === 'draft') return typeof record.content === 'string' ? raw : null;
      if (kind === 'reconcile') return record.recovery.scratch;
      return localStorage.getItem(record.recovery.liveKey);
    }
    return localStorage.getItem('gitnotes:' + kind + ':' + base) ?? localStorage.getItem(key);
  }, key);
}
export async function retainedScratch(page, path) {
  return page.evaluate(
    path =>
      Object.entries(localStorage)
        .filter(([key]) => key.endsWith('/' + path))
        .flatMap(([key, value]) => {
          if (key.startsWith('gitnotes:reconcile:')) return [value];
          if (key.startsWith('gitnotes:draft:')) {
            try {
              const r = JSON.parse(value);
              if (r.recovery?.scratch !== null && r.recovery?.scratch !== undefined) return [r.recovery.scratch];
            } catch {}
          }
          return [];
        }),
    path
  );
}
export async function retainedTexts(page, path) {
  return page.evaluate(
    path =>
      Object.entries(localStorage)
        .filter(([key]) => key.startsWith('gitnotes:draft:') && key.endsWith('/' + path))
        .map(([, raw]) => {
          try {
            return JSON.parse(raw).content;
          } catch {
            return null;
          }
        }),
    path
  );
}
export async function chooseHistory(page, revision) {
  await clickNoteAction(page, 'history-open');
  await page
    .locator('#history-list button')
    .filter({hasText: revision.slice(0, 7)})
    .click();
  await expect(page.locator('#history-dialog')).toBeHidden();
  await page.locator('#format-source').click();
}
export async function failDraftStorage(page) {
  await page.evaluate(() => {
    const set = Storage.prototype.setItem;
    window.draftWriteFailures = 0;
    window.restoreDraftStorage = () => {
      Storage.prototype.setItem = set;
    };
    Storage.prototype.setItem = function (key, value) {
      if (key.startsWith('gitnotes:draft:')) {
        window.draftWriteFailures++;
        throw new DOMException('Synthetic quota failure', 'QuotaExceededError');
      }
      return set.call(this, key, value);
    };
  });
}
export async function openRetainedPrivate(page, path) {
  const initial = '# Committed original\n',
    prior = '# Older retained private draft\n';
  const note = await api('/p/default/api/note', {path, content: initial, revision: ''}),
    user = await api('/api/me'),
    key = 'gitnotes:draft:' + user.id + '/default/' + path;
  await login(page);
  await page.goto(origin + '/login');
  await page.evaluate(
    ({key, note, prior}) =>
      localStorage.setItem(
        key,
        JSON.stringify({content: prior, revision: note.revision, version: note.version, private: true})
      ),
    {key, note, prior}
  );
  await page.goto(origin + '/?project=default&note=' + path);
  await edit(page);
  await expectEditorText(page, prior);
  return {initial, prior, note, key, user};
}
export async function api(path, body) {
  return app.api(path, body);
}
export async function login(page) {
  await app.login(page, 'recovery');
}
// An idle live poll holds for 25 seconds before answering unchanged. Cases that
// only wait for one to come back ask for a short hold; Go tests cover the full one.
export function shortLiveWait(url) {
  return url.replace('wait=25000', 'wait=500');
}
export async function shortLivePolls(page) {
  await page.route('**/api/live?*', route => route.continue({url: shortLiveWait(route.request().url())}));
}
export async function edit(page) {
  const later = page.getByRole('button', {name: 'Later', exact: true});
  if (await later.isVisible()) await later.click();
  await expect(page.locator('#save-state')).toContainText(
    /Saved|Unsaved on this device · recovered|Live changes synced/
  );
  if (await page.locator('#editing').isHidden()) await page.locator('#mode').click();
  await page.locator('#format-source').click();
}
export async function privateDraft(page, note, content, project = 'default') {
  const user = await api('/api/me');
  await page.goto(origin + '/login');
  await page.evaluate(
    ({key, note, content}) =>
      localStorage.setItem(key, JSON.stringify({content, revision: note.revision, version: note.version})),
    {key: 'gitnotes:draft:' + user.id + '/' + project + '/' + note.path, note, content}
  );
}
export async function expectCreationPending(page) {
  const controls = [
    page.locator('#new-note'),
    page.locator('#first-note'),
    page.locator('#first-source'),
    page.locator('#create-source')
  ];
  for (const control of controls) {
    await control.dispatchEvent('click');
    await expect(page.locator('#new-dialog')).toBeHidden();
    await expect(control).toBeDisabled();
  }
  await expect(page.locator('#document')).toBeHidden();
}
export async function clickNoteAction(page, id) {
  if ((await page.locator('#note-details-toggle').getAttribute('aria-expanded')) !== 'true')
    await page.locator('#note-details-toggle').click();
  await page.locator('#' + id).click();
}

export async function clickNewNote(page) {
  await expect(page.locator('#new-note')).toBeEnabled();
  if (await page.locator('#new-note').isHidden()) {
    if (await page.locator('#search-view').isVisible()) await page.locator('#search-close').click();
    await clickNotebookAction(page, 'sidebar-new-note');
  } else await page.locator('#new-note').click();
}

export async function ensureSearch(page) {
  if (await page.locator('#search-view').isHidden()) {
    if (await page.locator('#sidebar').isHidden()) await page.locator('#menu').click();
    await page.locator('#search-open').click();
  } else if (page.viewportSize()?.width <= 720 && (await page.locator('#sidebar').isVisible()))
    await page.locator('#menu').click();
}

export async function navigateNote(page, path) {
  const library = page.locator('#folder-items a[data-note="' + path + '"]');
  if (await library.isVisible()) {
    await library.click();
    return;
  }
  await ensureSearch(page);
  await page.locator('#search').fill('');
  await page.locator('#search-results a[data-note="' + path + '"]').click();
  await expect(page.getByRole('dialog', {name: 'Confirm action', exact: true})).toBeHidden();
  await page.locator('#search-close').evaluate(button => {
    if (!document.querySelector('#search-view').hidden) button.click();
  });
}

export async function clickNotebookAction(page, id) {
  if (await page.locator('#' + id).isHidden()) {
    if (await page.locator('#sidebar').isHidden()) await page.locator('#menu').click();
  }
  await page.locator('#' + id).click();
}
// `env` adds settings to the app's environment.
export function useEditorApp({env} = {}) {
  test.beforeAll(async () => {
    app = await startApp({
      name: 'forgetmenote-browser-',
      account: {username: 'browser-admin', password: 'DisposableBrowserPassword123!'},
      env
    });
    ({dir, origin} = app);
    await app.saveBaseline();
  });
  test.afterAll(async () => {
    await app?.close();
  });
  test.beforeEach(async ({page}) => {
    await app.reset();
    page.on('pageerror', error => {
      throw error;
    });
    page.on('response', response => {
      if (response.url().includes('/static/')) expect(response.status(), response.url()).toBeLessThan(400);
    });
  });
  test.afterEach(async ({}, info) => {
    if (info.status !== info.expectedStatus)
      await info.attach('app.log', {body: app?.output || 'App did not start', contentType: 'text/plain'});
  });
}
