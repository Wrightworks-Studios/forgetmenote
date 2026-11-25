import {watchConfirmations, removeConfirmations} from './app-fixture.mjs';
import {test, expect} from '@playwright/test';
// Each worker owns an app; baseline reset makes cases independent within it.
test.describe.configure({mode: 'parallel'});
import {startApp} from './app-fixture.mjs';
import {expectEditorText, fillSource} from './editor-text.mjs';
let app, cookies;
const account = {username: 'ownership-admin', password: 'DisposableOwnership123!'};
test.beforeAll(async ({browser}) => {
  app = await startApp({name: 'fmn-ui-ownership-', account});
  await app.api('/api/projects', {id: 'other', name: 'Other notebook'});
  for (const id of ['default', 'other'])
    await app.api('/p/' + id + '/api/note', {
      path: 'same.md',
      content: '# ' + id + ' title\nUnique ' + id + ' evidence.\n',
      revision: ''
    });
  const context = await browser.newContext();
  try {
    const page = await context.newPage();
    await app.login(page, 'password');
    cookies = await context.cookies();
  } finally {
    await context.close();
  }
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
  await page.context().addCookies(cookies);
});
test.afterEach(async ({}, info) => {
  if (info.status !== info.expectedStatus)
    await info.attach('app.log', {body: app?.output || 'App did not start', contentType: 'text/plain'});
});
function deferred() {
  let resolve;
  const promise = new Promise(done => (resolve = done));
  return {promise, resolve};
}
test('historical comparison waits for the current-note check before becoming actionable', async ({page}) => {
  const old = await app.api('/p/default/api/note?path=same.md');
  await app.api('/p/default/api/note', {
    path: 'same.md',
    content: '# Current copper lantern\n',
    revision: old.revision
  });
  const gate = deferred(),
    held = deferred();
  await page.route('**/p/default/api/note?path=same.md', async route => {
    held.resolve();
    await gate.promise;
    await route.continue();
  });
  try {
    await page.goto(app.origin + '/?project=default&note=same.md&revision=' + old.revision);
    await held.promise;
    await expect(page.locator('#historical-copy')).toBeVisible();
    await expect(page.locator('#historical-compare')).toBeDisabled();
    gate.resolve();
    await expect(page.locator('#historical-compare')).toBeEnabled();
    await page.locator('#historical-compare').click();
    await expect(page.locator('#historical-comparison')).toContainText('Current copper lantern');
    expect((await app.api('/p/default/api/note?path=same.md')).content).toBe('# Current copper lantern\n');
  } finally {
    gate.resolve();
  }
});
for (const destination of ['note', 'folder'])
  test('late ' + destination + ' navigation preserves a newly opened mobile sidebar', async ({page}) => {
    await app.api('/p/default/api/note', {path: 'Folder/child.md', content: '# Child\n', revision: ''});
    await page.setViewportSize({width: 390, height: 667});
    await page.goto(app.origin + '/?project=default&folder=');
    await expect(page.locator('#folder-count')).toHaveText('1 note · 1 folder');
    const held = deferred(),
      gate = deferred();
    await page.route(
      destination === 'note' ? '**/api/note?path=same.md' : '**/api/browse?path=Folder&collection=notes',
      async route => {
        held.resolve();
        await gate.promise;
        await route.continue();
      }
    );
    try {
      await page
        .locator(destination === 'note' ? '#folder-items .browse-item-link' : '#folder-groups .browse-item-link')
        .click();
      await held.promise;
      await page.locator('#menu').click();
      await expect(page.locator('#sidebar')).toBeVisible();
      gate.resolve();
      if (destination === 'note') await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
      else await expect(page.locator('#folder-title')).toHaveText('Folder');
      await expect(page.locator('#project-select')).toBeVisible();
      await page.locator('#project-select').selectOption('other');
      await expect(page.locator('#folder-title')).toHaveText('Other notebook');
    } finally {
      gate.resolve();
    }
  });
async function checksFixture(page) {
  await page.route('**/api/jev/**', route => {
    const path = new URL(route.request().url()).pathname;
    return route.fulfill({
      json:
        path === '/api/jev/settings'
          ? {available: true, settings: {consent: true}}
          : path.endsWith('/settings')
            ? {canManage: true, settings: {autoChecks: false}}
            : {jobs: []}
    });
  });
}
async function openChecks(page) {
  await page.locator('#review-open').click();
  await page.locator('#jev-review-open').click();
  await expect(page.locator('#review-checks')).toContainText('Consistency');
}
for (const action of ['open', 'collection'])
  test('search callbacks stay in their producing notebook: ' + action, async ({page}) => {
    await page.goto(app.origin + '/?project=default&folder=');
    await expect(page.locator('#folder-count')).toHaveText('1 note · 0 folders');
    await ensureSearch(page);
    await page.locator('#search').fill('Unique default');
    await expect(page.locator('#search-results a[data-note="same.md"]')).toBeVisible();
    // Reopening valid results within this notebook remains supported.
    await page.locator('#search-results a[data-note="same.md"]').click();
    await expect(page.locator('#path')).toHaveText('default title');
    await ensureSearch(page);
    await expect(page.locator('#search-results a[data-note="same.md"]')).toBeVisible();
    if (await page.locator('#sidebar').isHidden()) await page.locator('#menu').click();
    await page.evaluate(() => {
      window.retiredResult = document.querySelector('#search-results a[data-note]');
      window.retiredAction = document.querySelector('#search-results .tree-menu');
    });
    const gate = deferred(),
      held = deferred();
    await page.route('**/p/other/api/knowledge/retrieval-settings', async route => {
      held.resolve();
      await gate.promise;
      await route.continue();
    });
    const requests = [];
    page.on('request', request => {
      const url = new URL(request.url());
      if (url.pathname === '/p/other/api/note' || url.pathname === '/p/other/api/container')
        requests.push(url.pathname);
    });
    try {
      await page.locator('#project-select').selectOption('other');
      await held.promise;
      if (action === 'open') await page.evaluate(() => window.retiredResult.click());
      else {
        await page.evaluate(() => window.retiredAction.onclick(new MouseEvent('click')));
        if (await page.locator('#file-menu').isVisible())
          await page.locator('[data-file-action="container-archive"]').click();
      }
      await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
      expect(requests).toEqual([]);
      await expect(page.locator('#search-view')).toBeHidden();
      await expect(page.locator('#search-results')).toBeEmpty();
      expect((await app.api('/p/other/api/note?path=same.md')).content).toBe('# other title\nUnique other evidence.\n');
    } finally {
      gate.resolve();
    }
  });
for (const status of [401, 403])
  test('Notebook checks clear private evidence after ' + status, async ({page}) => {
    await checksFixture(page);
    let denied = false;
    const secret = 'PRIVATE_CHECK_EVIDENCE_' + status;
    const job = {
      id: 'private-job',
      kind: 'consistency',
      state: status === 403 ? 'running' : 'paused',
      examined: 1,
      total: 2,
      results: [
        {
          id: 'private-result',
          title: 'Private finding',
          evidence: [{path: 'same.md', revision: 'a'.repeat(40), text: secret}]
        }
      ]
    };
    await page.route('**/p/default/api/jev/jobs', route => route.fulfill({json: {jobs: [job]}}));
    await page.route('**/p/default/api/jev/jobs/private-job', route =>
      denied ? route.fulfill({status, body: 'Access denied'}) : route.fulfill({json: job})
    );
    await page.goto(app.origin + '/?project=default&folder=');
    await openChecks(page);
    await page
      .locator('#review-checks')
      .getByRole('button', {name: /consistency ·/})
      .click();
    await expect(page.locator('#review-checks')).toContainText(secret);
    await page.evaluate(
      () =>
        (window.retiredRefresh = [...document.querySelectorAll('#review-checks button')].find(
          button => button.textContent === 'Refresh'
        ))
    );
    denied = true;
    if (status === 401)
      await page.locator('#review-checks').getByRole('button', {name: 'Refresh', exact: true}).click();
    await expect(page.locator('#knowledge-dialog')).toBeHidden();
    await expect(page.locator('#review-checks')).not.toContainText(secret);
    await expect(page.locator('#review-checks button')).toHaveCount(0);
    denied = false;
    await page.evaluate(() => window.retiredRefresh.onclick());
    await expect(page.locator('#knowledge-dialog')).toBeHidden();
    await expect(page.locator('#review-checks')).not.toContainText(secret);
  });
function reviewData(proposalStatus = 'proposed') {
  return {
    findings: [
      {
        path: 'same.md',
        revision: 'a'.repeat(40),
        comment: {
          id: 'finding',
          author: account.username,
          text: 'Synthetic finding evidence',
          resolved: false,
          finding: {title: 'Synthetic finding', evidence: []}
        }
      }
    ],
    proposals: [
      {
        id: 'proposal',
        status: proposalStatus,
        author: account.username,
        provenance: {name: 'Synthetic reviewer'},
        group: {summary: 'Synthetic proposal', edits: []},
        before: {}
      }
    ],
    checkpoints: []
  };
}
for (const decision of ['Resolve', 'finding Dismiss', 'Accept group', 'proposal Dismiss', 'Archive'])
  for (const destination of ['checks', 'closed'])
    test('late ' + decision + ' does not take over ' + destination, async ({page}) => {
      await checksFixture(page);
      await page.route('**/p/default/api/knowledge/review', route =>
        route.fulfill({json: reviewData(decision === 'Archive' ? 'applied' : 'proposed')})
      );
      const gate = deferred(),
        held = deferred(),
        finished = deferred();
      await page.route('**/p/default/api/knowledge/*/decide', async route => {
        held.resolve();
        await gate.promise;
        await route.fulfill({json: {}});
        finished.resolve();
      });
      await page.goto(app.origin + '/?project=default&folder=');
      await page.locator('#review-open').click();
      const proposal = ['Accept group', 'proposal Dismiss', 'Archive'].includes(decision);
      if (proposal) await page.locator('[data-review-section=proposals]').click();
      try {
        await page
          .locator('#knowledge-body section:visible')
          .getByRole('button', {name: decision.includes('Dismiss') ? 'Dismiss' : decision, exact: true})
          .click();
        await held.promise;
        if (destination === 'checks') {
          await page.locator('#jev-review-open').click();
          await expect(page.locator('#review-checks')).toContainText('Consistency');
        } else await page.locator('#knowledge-dialog [data-close="knowledge-dialog"]').click();
        gate.resolve();
        await finished.promise;
        await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
        if (destination === 'checks') {
          await expect(page.locator('#review-checks')).toBeVisible();
          await expect(page.locator('#jev-review-open')).toHaveAttribute('aria-pressed', 'true');
        } else await expect(page.locator('#knowledge-dialog')).toBeHidden();
      } finally {
        gate.resolve();
      }
    });
for (const destination of ['findings', 'sources'])
  test('late Notebook checks error does not enter ' + destination, async ({page}) => {
    await checksFixture(page);
    const gate = deferred(),
      held = deferred(),
      finished = deferred();
    await page.route('**/p/default/api/jev/jobs', async route => {
      held.resolve();
      await gate.promise;
      await route.fulfill({status: 500, body: 'STALE_CHECK_ERROR'});
      finished.resolve();
    });
    await page.goto(app.origin + '/?project=default&note=same.md');
    await page.locator('#review-open').click();
    await page.locator('#jev-review-open').click();
    await held.promise;
    try {
      if (destination === 'findings') {
        await page.locator('[data-review-section=findings]').click();
        await expect(page.locator('#knowledge-body')).toContainText('No findings');
      } else {
        await page.locator('#knowledge-dialog [data-close="knowledge-dialog"]').click();
        await page.locator('#note-details-toggle').click();
        await page.locator('#sources-open').click();
        await expect(page.locator('#note-context-references')).toBeVisible();
      }
      gate.resolve();
      await finished.promise;
      await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
      await expect(page.locator('#knowledge-error')).toBeHidden();
      await expect(page.locator('#knowledge-dialog')).not.toContainText('STALE_CHECK_ERROR');
    } finally {
      gate.resolve();
    }
  });
test('source editor and creation import have distinct accurate labels', async ({page}) => {
  await page.goto(app.origin + '/?project=default&note=same.md');
  await page.locator('#mode').click();
  await expect(page.locator('#format-source')).toHaveText('Source');
  await page.locator('#format-source').click();
  await expect(page.locator('#editor')).toBeVisible();
  await expectEditorText(page, '# default title\nUnique default evidence.\n');
  if (await page.locator('#sidebar').isHidden()) await page.locator('#menu').click();
  await page.locator('#sidebar-import-source').click();
  await expect(page.locator('#new-dialog')).toBeVisible();
  await expect(page.locator('#new-source-panel')).toBeVisible();
});

test('late Notebook checks action failure does not enter Findings', async ({page}) => {
  await checksFixture(page);
  const gate = deferred(),
    held = deferred(),
    finished = deferred();
  await page.route('**/p/default/api/jev/jobs', async route => {
    if (route.request().method() !== 'POST') {
      await route.fallback();
      return;
    }
    held.resolve();
    await gate.promise;
    await route.fulfill({status: 500, body: 'STALE_ACTION_ERROR'});
    finished.resolve();
  });
  await page.goto(app.origin + '/?project=default&folder=');
  await openChecks(page);
  try {
    await page.locator('#review-checks').getByRole('button', {name: 'Check now', exact: true}).click();
    await held.promise;
    await page.locator('[data-review-section=findings]').click();
    await expect(page.locator('#knowledge-body')).toContainText('No findings');
    gate.resolve();
    await finished.promise;
    await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
    await expect(page.locator('#knowledge-error')).toBeHidden();
    await expect(page.locator('#knowledge-dialog')).not.toContainText('STALE_ACTION_ERROR');
  } finally {
    gate.resolve();
  }
});

test('opening Search retires an in-flight note navigation', async ({page}) => {
  await page.goto(app.origin + '/?project=default&folder=');
  await expect(page.locator('#folder-count')).toHaveText('1 note · 0 folders');
  const held = deferred(),
    gate = deferred();
  await page.route('**/p/default/api/note?path=same.md', async route => {
    held.resolve(route.request());
    await gate.promise;
    await route.continue();
  });
  try {
    await page.locator('#folder-items a[data-note="same.md"]').click();
    const request = await held.promise;
    await ensureSearch(page);
    await page.locator('#search').fill('Unique default');
    await expect(page.locator('#search-results a[data-note="same.md"]')).toBeVisible();
    const searchURL = page.url();
    gate.resolve();
    // Opening Search may cancel the superseded request instead of letting it finish.
    await (await request.response())?.finished();
    await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
    await expect(page.locator('#search-view')).toBeVisible();
    await expect(page.locator('#document')).toBeHidden();
    await expect(page).toHaveURL(searchURL);
  } finally {
    gate.resolve();
  }
});

test('search preserves ordinary sidebar navigation in its current notebook', async ({page}) => {
  await page.goto(app.origin + '/?project=default&folder=');
  await expect(page.locator('#folder-count')).toHaveText('1 note · 0 folders');
  await ensureSearch(page);
  await page.locator('#search').fill('Unique default');
  await expect(page.locator('#search-results a[data-note="same.md"]')).toBeVisible();
  await openLibraryNote(page, 'same.md');
  await expect(page.locator('#path')).toHaveText('default title');
  await expect(page.locator('#document')).toBeVisible();
});

test('Back to notes immediately restores an unsaved editor when list refresh fails', async ({page}) => {
  await page.goto(app.origin + '/?project=default&note=same.md');
  await expect(page.locator('#path')).toHaveText('default title');
  await page.route('**/api/live', route => route.abort());
  await page.locator('#mode').click();
  await page.locator('#format-source').click();
  const draft = 'Private unsaved text must stay exactly here.\n';
  await fillSource(page, draft);
  await ensureSearch(page);
  await page.locator('#search').fill('Unique default');
  await expect(page.locator('#search-results a[data-note="same.md"]')).toBeVisible();
  await page.route('**/api/notes**', route => route.abort());
  const state = await page.locator('#search-close').evaluate(button => {
    button.click();
    return {
      hidden: document.getElementById('search-view').hidden,
      active: document.body.classList.contains('search-active'),
      editorVisible: !!document.getElementById('editor').getClientRects().length,
      text: document.getElementById('editing').markdown
    };
  });
  expect(state).toEqual({hidden: true, active: false, editorVisible: true, text: draft});
  await expect(page.locator('#search')).toHaveValue('Unique default');
  await expect(page.locator('#editor')).toBeVisible();
  await expectEditorText(page, draft);
});
for (const entry of ['initial', 'section'])
  for (const destination of ['checks', 'closed', 'other notebook'])
    test('late ' + entry + ' Findings error cannot take over ' + destination, async ({page}) => {
      await checksFixture(page);
      await page.goto(app.origin + '/?project=default&note=same.md');
      await expect(page.locator('#path')).toHaveText('default title');
      if (entry === 'section') await openChecks(page);
      const gate = deferred(),
        held = deferred();
      await page.route('**/p/default/api/knowledge/review', async route => {
        held.resolve();
        await gate.promise;
        await route.fulfill({status: 500, body: 'RETIRED_FINDINGS_ERROR'});
      });
      try {
        if (entry === 'initial') await page.locator('#review-open').click();
        else await page.locator('[data-review-section=findings]').click();
        await held.promise;
        if (destination === 'checks') {
          await page.locator('#jev-review-open').click();
          await expect(page.locator('#review-checks')).toContainText('Consistency');
        } else {
          await page.locator('#knowledge-dialog [data-close="knowledge-dialog"]').click();
          if (destination === 'other notebook') {
            await page.locator('#project-select').selectOption('other');
            await expect(page.locator('#folder-count')).toHaveText('1 note · 0 folders');
            await openChecks(page);
          }
        }
        const response = page.waitForResponse(
          response => response.url().endsWith('/p/default/api/knowledge/review') && response.status() === 500
        );
        gate.resolve();
        await (await response).finished();
        await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
        if (destination === 'closed') await expect(page.locator('#knowledge-dialog')).toBeHidden();
        else {
          await expect(page.locator('#review-checks')).toBeVisible();
          await expect(page.locator('#jev-review-open')).toHaveAttribute('aria-pressed', 'true');
        }
        await expect(page.locator('#knowledge-error')).toBeHidden();
        await expect(page.locator('#knowledge-error')).not.toContainText('RETIRED_FINDINGS_ERROR');
        await expect(page.locator('#notice')).not.toContainText('RETIRED_FINDINGS_ERROR');
      } finally {
        gate.resolve();
      }
    });

test('current Findings load failures remain actionable in their own view', async ({page}) => {
  await page.goto(app.origin + '/?project=default&note=same.md');
  await expect(page.locator('#path')).toHaveText('default title');
  await page.route('**/p/default/api/knowledge/review', route =>
    route.fulfill({status: 500, body: 'CURRENT_FINDINGS_FAILURE'})
  );
  await page.locator('#review-open').click();
  await expect(page.locator('#knowledge-error')).toHaveText('CURRENT_FINDINGS_FAILURE');
  await expect(page.locator('#knowledge-error')).toBeVisible();
  await expect(page.locator('#knowledge-dialog')).toBeVisible();
});

async function savedInboxCapture(page, label) {
  await page.goto(app.origin + '/?project=default&area=inbox');
  await expect(page.locator('#inbox-view')).toBeVisible();
  const response = await page.request.post(app.origin + '/api/quicknotes', {
    data: {operationId: crypto.randomUUID(), content: '# ' + label + '\nPrivate navigation fixture.'},
    headers: {Origin: app.origin}
  });
  expect(response.ok()).toBe(true);
  const capture = await response.json();
  // Wait for the refresh triggered here, not an earlier Inbox load. This fixture
  // exercises real captures; its setup is not a list-latency assertion.
  const requested = page.waitForRequest(
    request =>
      request.method() === 'GET' &&
      new URL(request.url()).pathname === '/api/quicknotes' &&
      !new URL(request.url()).search
  );
  await page.locator('#inbox-view').getByRole('button', {name: 'Refresh captures', exact: true}).click();
  const refreshed = await (await requested).response();
  expect(refreshed.ok()).toBe(true);
  await refreshed.finished();
  const card = page.locator('[data-capture="' + capture.id + '"]');
  await expect(card).toBeVisible();
  return {capture, card};
}
for (const outcome of ['success', 'failure'])
  for (const newer of ['note', 'private draft'])
    test('late Inbox capture ' + outcome + ' cannot replace newer ' + newer, async ({page}) => {
      const {card} = await savedInboxCapture(page, 'Held ' + outcome + ' ' + newer),
        gate = deferred(),
        held = deferred();
      await page.route('**/api/projects', async route => {
        const response = await route.fetch(),
          catalog = await response.json();
        held.resolve();
        await gate.promise;
        if (outcome === 'failure') await route.fulfill({status: 500, body: 'RETIRED_CAPTURE_FAILURE'});
        else
          await route.fulfill({
            json: catalog.map(project => (project.id === 'default' ? {...project, name: 'RETIRED_CATALOG'} : project))
          });
      });
      const prompts = [];
      await watchConfirmations(page, async dialog => {
        prompts.push(dialog.message());
        await dialog.dismiss();
      });
      try {
        await card.getByRole('link', {name: 'Open note', exact: true}).click();
        await held.promise;
        await page.locator('#project-select').selectOption('other');
        await expect(page.locator('#folder-count')).toHaveText('1 note · 0 folders');
        if (newer === 'note') {
          await openLibraryNote(page, 'same.md');
          await expect(page.locator('#path')).toHaveText('other title');
        } else {
          await page.locator('#new-note').click();
          await page.locator('#new-name').fill('Newer private draft');
          await page.getByRole('button', {name: 'Start writing', exact: true}).click();
          await page.locator('#format-source').click();
          await fillSource(page, 'Private draft entered after the Inbox request.\n');
        }
        const newerURL = page.url(),
          newerPath = await page.locator('#note-path').textContent();
        const response = page.waitForResponse(response => new URL(response.url()).pathname === '/api/projects');
        gate.resolve();
        await (await response).finished();
        await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
        await expect(page).toHaveURL(newerURL);
        await expect(page.locator('#project-select')).toHaveValue('other');
        await expect(page.locator('#note-path')).toHaveText(newerPath);
        await expect(page.locator('#project-select')).not.toContainText('RETIRED_CATALOG');
        await expect(page.locator('#inbox-content .jev-error')).not.toContainText('RETIRED_CAPTURE_FAILURE');
        expect(prompts).toEqual([]);
        if (newer === 'private draft') await expectEditorText(page, 'Private draft entered after the Inbox request.\n');
        else await expect(page.locator('#preview')).toContainText('Unique other evidence.');
      } finally {
        gate.resolve();
      }
    });
test('current Inbox capture catalog failure stays visible without losing the capture', async ({page}) => {
  const {card} = await savedInboxCapture(page, 'Current catalog failure');
  await page.route('**/api/projects', route => route.fulfill({status: 500, body: 'CURRENT_CAPTURE_FAILURE'}));
  await card.getByRole('link', {name: 'Open note', exact: true}).click();
  await expect(page.locator('#inbox-content .jev-error')).toHaveText('CURRENT_CAPTURE_FAILURE');
  await expect(page.locator('#inbox-view')).toBeVisible();
  await expect(card).toBeVisible();
});

async function noteAfterInboxHistory(page) {
  await page.goto(app.origin + '/?project=default&note=same.md');
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await page.locator('#inbox-open').click();
  await expect(page.locator('#inbox-view')).toBeVisible();
  // Finish the reopened note's initial read before asserting its installed
  // session. This is history setup, not a five-second network latency test.
  const connecting = page.waitForResponse(
    response =>
      response.request().method() === 'GET' &&
      new URL(response.url()).pathname === '/p/default/api/live' &&
      new URL(response.url()).search === '?path=same.md'
  );
  await openLibraryNote(page, 'same.md');
  const response = await connecting;
  expect(response.ok()).toBe(true);
  await response.finished();
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
}
for (const newer of ['notebook and note', 'Forward to note', 'same notebook folder'])
  test('delayed Inbox history restore cannot replace newer ' + newer, async ({page}) => {
    await noteAfterInboxHistory(page);
    const gate = deferred(),
      held = deferred();
    let blocked = false;
    const endpoint =
      newer === 'same notebook folder' ? '**/p/default/api/browse?*' : '**/p/default/api/knowledge/retrieval-settings';
    await page.route(endpoint, async route => {
      if (blocked) {
        await route.continue();
        return;
      }
      blocked = true;
      const response = await route.fetch();
      held.resolve(route.request());
      await gate.promise;
      await route.fulfill({response});
    });
    try {
      await page.goBack();
      const request = await held.promise;
      if (newer === 'notebook and note') {
        await page.locator('#project-select').selectOption('other');
        await expect(page.locator('#folder-count')).toHaveText('1 note · 0 folders');
        await openLibraryNote(page, 'same.md');
        await expect(page.locator('#path')).toHaveText('other title');
      } else if (newer === 'Forward to note') {
        await page.goForward();
        await expect(page.locator('#note-path')).toHaveText('same.md');
        await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
      } else {
        await page.locator('[data-collection=notes]').click();
        await expect(page.locator('#folder-browser')).toBeVisible();
        await expect(page.locator('#folder-count')).toHaveText('1 note · 0 folders');
      }
      const expectedURL = page.url(),
        expectedProject = await page.locator('#project-select').inputValue();
      gate.resolve();
      // A newer folder may cancel the delayed restore's request instead of letting it finish.
      await (await request.response())?.finished();
      await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
      await expect(page).toHaveURL(expectedURL);
      await expect(page.locator('#project-select')).toHaveValue(expectedProject);
      await expect(page.locator('#inbox-view')).toBeHidden();
      await expect(page.locator(newer === 'same notebook folder' ? '#folder-browser' : '#document')).toBeVisible();
    } finally {
      gate.resolve();
    }
  });
for (const refusal of ['cancel', 'retention'])
  test('Inbox history restoration stops after draft ' + refusal, async ({page}) => {
    await noteAfterInboxHistory(page);
    await page.route('**/api/live', route => route.abort());
    await page.locator('#mode').click();
    await page.locator('#format-source').click();
    const draft = 'Exact private draft before refused Inbox history.\n';
    await fillSource(page, draft);
    const expectedURL = page.url(),
      prompts = [];
    if (refusal === 'retention')
      await page.evaluate(() => {
        const set = Storage.prototype.setItem;
        Storage.prototype.setItem = function (key, value) {
          if (key.startsWith('gitnotes:draft:'))
            throw new DOMException('Synthetic quota failure', 'QuotaExceededError');
          return set.call(this, key, value);
        };
      });
    await watchConfirmations(page, async dialog => {
      prompts.push(dialog.message());
      if (refusal === 'cancel' && prompts.length === 1) await dialog.dismiss();
      else await dialog.accept();
    });
    await page.goBack();
    if (refusal === 'retention') await expect(page.locator('#notice')).toContainText('Keep this tab open');
    await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
    await expect(page).toHaveURL(expectedURL);
    await expect(page.locator('#inbox-view')).toBeHidden();
    await expect(page.locator('#editor')).toBeVisible();
    await expectEditorText(page, draft);
    expect(prompts).toEqual(['Keep this draft on this device and switch notebooks?']);
  });
test('current Inbox search history restores normally through Back and Forward', async ({page}) => {
  await noteAfterInboxHistory(page);
  await page.goBack();
  await expect(page.locator('#search-view')).toBeVisible();
  await page.goBack();
  await expect(page.locator('#search-view')).toBeHidden();
  await expect(page.locator('#inbox-view')).toBeVisible();
  await expect(page).toHaveURL(/area=inbox/);
  await page.goForward();
  await expect(page.locator('#search-view')).toBeVisible();
  await expect(page).toHaveURL(/search=1/);
  await page.goForward();
  await expect(page.locator('#inbox-view')).toBeHidden();
  await expect(page.locator('#note-path')).toHaveText('same.md');
  await expect(page.locator('#document')).toBeVisible();
});
test('empty grants allow Inbox restoration without opening pinned or Trash notebook views', async ({browser}) => {
  const username = 'route-empty-grants',
    password = 'DisposableEmptyGrants123!';
  await app.api('/api/local-users', {username, password});
  const context = await browser.newContext(),
    page = await context.newPage();
  const invalidRequests = [];
  page.on('request', request => {
    if (new URL(request.url()).pathname.startsWith('/p/')) invalidRequests.push(request.url());
  });
  try {
    await page.request.post(app.origin + '/auth/password/login', {
      form: {username, password},
      headers: {Origin: app.origin}
    });
    for (const query of ['note=private.md&revision=' + 'a'.repeat(40), 'trash=missing']) {
      await page.goto(app.origin + '/?' + query);
      await expect(page.locator('#save-state')).toHaveText('No notebook access yet');
      await expect(page.locator('#knowledge-dialog')).toBeHidden();
      await expect(page.locator('#trash-dialog')).toBeHidden();
    }
    await page.goto(app.origin + '/?area=inbox');
    await expect(page.locator('#inbox-view')).toBeVisible();
    await expect(page.locator('#new-note')).toBeDisabled();
    expect(await (await page.request.get(app.origin + '/api/projects')).json()).toEqual([]);
    expect(invalidRequests).toEqual([]);
  } finally {
    await context.close();
  }
});

for (const entry of ['ordinary note', 'initial Inbox'])
  for (const status of ['private', 'filed'])
    test('opening ' + status + ' capture preserves Inbox history from ' + entry, async ({page}) => {
      await page.addInitScript(() => {
        window.routePushes = [];
        const push = history.pushState.bind(history);
        history.pushState = (state, title, url) => {
          window.routePushes.push(String(url));
          return push(state, title, url);
        };
      });
      await page.goto(app.origin + (entry === 'initial Inbox' ? '/?area=inbox' : '/?project=default&note=same.md'));
      if (entry === 'ordinary note') {
        await expect(page.locator('#path')).toHaveText('default title');
        await page.locator('#inbox-open').click();
      }
      await expect(page.locator('#inbox-view')).toBeVisible();
      if (entry === 'initial Inbox') expect(await page.evaluate(() => window.routePushes)).toEqual([]);
      const inboxURL = page.url(),
        initialLength = await page.evaluate(() => history.length),
        label = 'History ' + entry + ' ' + status;
      const response = await page.request.post(app.origin + '/api/quicknotes', {
        data: {operationId: crypto.randomUUID(), content: '# ' + label + '\nPrivate navigation fixture.'},
        headers: {Origin: app.origin}
      });
      expect(response.ok()).toBe(true);
      const capture = await response.json();
      await page.locator('#inbox-view').getByRole('button', {name: 'Refresh captures', exact: true}).click();
      const card = page.locator('[data-capture="' + capture.id + '"]');
      await expect(card).toBeVisible();
      if (status === 'filed') {
        await card.getByRole('button', {name: 'Choose destination', exact: true}).click();
        await page.getByRole('combobox', {name: 'Destination', exact: true}).selectOption({label: 'My notes / root'});
        // Observe the post-filing refresh before asserting its rendered outcome.
        const filed = page.waitForResponse(
          response =>
            response.request().method() === 'POST' && new URL(response.url()).pathname === '/api/quicknotes/file'
        );
        const refreshed = filed.then(response =>
          response.ok()
            ? page.waitForRequest(
                request =>
                  request.method() === 'GET' &&
                  new URL(request.url()).pathname === '/api/quicknotes' &&
                  !new URL(request.url()).search
              )
            : null
        );
        await page.getByRole('button', {name: 'File note', exact: true}).click();
        const filingResponse = await filed;
        expect(
          filingResponse.ok(),
          'POST /api/quicknotes/file: HTTP ' + filingResponse.status() + '\n' + (await filingResponse.text())
        ).toBe(true);
        const listed = await (await refreshed).response();
        expect(listed.ok()).toBe(true);
        await listed.finished();
        await expect(card.getByRole('button', {name: 'Undo filing', exact: true})).toBeVisible();
      }
      await card.getByRole('link', {name: 'Open note', exact: true}).click();
      await expect(page.locator('#path')).toHaveText(label);
      await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
      const captureURL = page.url();
      expect(await page.evaluate(() => history.length)).toBe(initialLength + 1);
      for (let step = 0; step < 2; step++) {
        await page.goBack();
        await expect(page).toHaveURL(inboxURL);
        await expect(page.locator('#inbox-view')).toBeVisible();
        await page.goForward();
        await expect(page).toHaveURL(captureURL);
        await expect(page.locator('#path')).toHaveText(label);
        await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
        expect(await page.evaluate(() => history.length)).toBe(initialLength + 1);
      }
      await page.reload();
      await expect(page.locator('#path')).toHaveText(label);
      expect(await page.evaluate(() => window.routePushes)).toEqual([]);
      expect(await page.evaluate(() => history.length)).toBe(initialLength + 1);
    });

for (const width of [1280, 390])
  test('Create rows contain labels and full clickable targets at ' + width + 'px', async ({browser}) => {
    const touch = width === 390,
      context = await browser.newContext({viewport: {width, height: 844}, hasTouch: touch, isMobile: touch});
    await context.addCookies(cookies);
    const page = await context.newPage();
    try {
      await page.goto(app.origin + '/?project=default&folder=');
      await expect(page.locator('#folder-browser')).toBeVisible();
      await page.locator('#create-toggle').click();
      await page.locator('#create-actions').evaluate(async element => {
        await Promise.all(element.getAnimations({subtree: true}).map(animation => animation.finished));
      });
      const quick = await page.locator('#quicknotes-open').boundingBox(),
        note = await page.locator('#new-note').boundingBox(),
        source = await page.locator('#create-source').boundingBox();
      for (const row of [quick, note, source]) {
        expect(row.width).toBeGreaterThan(100);
        expect(row.height).toBeGreaterThanOrEqual(touch ? 44 : 34);
      }
      expect(note.y + note.height).toBeLessThanOrEqual(quick.y);
      expect(quick.y + quick.height).toBeLessThanOrEqual(source.y);
      const position = {x: note.width - 12, y: note.height / 2};
      if (touch) await page.locator('#new-note').tap({position});
      else await page.locator('#new-note').click({position});
      await expect(page.locator('#new-dialog')).toBeVisible();
    } finally {
      await context.close();
    }
  });

test('failed Inbox capture note read does not publish a new destination', async ({page}) => {
  const {capture, card} = await savedInboxCapture(page, 'Failed note read history'),
    inboxURL = page.url(),
    length = await page.evaluate(() => history.length);
  await page.route('**/api/note?path=*', route =>
    new URL(route.request().url()).searchParams.get('path') === capture.path
      ? route.fulfill({status: 500, body: 'CAPTURE_NOTE_UNAVAILABLE'})
      : route.continue()
  );
  await card.getByRole('link', {name: 'Open note', exact: true}).click();
  await expect(page.locator('#notice')).toContainText('CAPTURE_NOTE_UNAVAILABLE');
  await expect(page).toHaveURL(inboxURL);
  expect(await page.evaluate(() => history.length)).toBe(length);
});

for (const outcome of ['success', 'failure'])
  test('older Back ' + outcome + ' cannot retire a pending Forward to Inbox', async ({page}) => {
    await page.goto(app.origin + '/?project=default&note=same.md');
    await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
    const noteURL = page.url();
    await page.locator('#inbox-open').click();
    await expect(page.locator('#inbox-view')).toBeVisible();
    const inboxURL = page.url(),
      length = await page.evaluate(() => history.length),
      backGate = deferred(),
      backHeld = deferred(),
      forwardGate = deferred(),
      forwardHeld = deferred();
    let count = 0;
    const pathname = outcome === 'success' ? '/p/default/api/knowledge/retrieval-settings' : '/p/default/api/status';
    await page.route('**' + pathname, async route => {
      const order = ++count;
      if (order > 2) {
        await route.continue();
        return;
      }
      const response = await route.fetch();
      (order === 1 ? backHeld : forwardHeld).resolve();
      await (order === 1 ? backGate : forwardGate).promise;
      if (order === 1 && outcome === 'failure')
        await route.fulfill({status: 500, body: 'RETIRED_BACK_RESTORATION_ERROR'});
      else await route.fulfill({response});
    });
    try {
      await page.goBack();
      await backHeld.promise;
      await page.goForward();
      await forwardHeld.promise;
      const backResponse = page.waitForResponse(response => new URL(response.url()).pathname === pathname);
      backGate.resolve();
      await (await backResponse).finished();
      await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
      await expect(page.locator('#notice')).not.toContainText('RETIRED_BACK_RESTORATION_ERROR');
      forwardGate.resolve();
      await expect(page.locator('#inbox-view')).toBeVisible();
      await expect(page).toHaveURL(inboxURL);
      expect(await page.evaluate(() => history.length)).toBe(length);
      await page.goBack();
      await expect(page).toHaveURL(noteURL);
      await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
      await expect(page.locator('#path')).toHaveText('default title');
      await expect(page.locator('#folder-browser')).toBeHidden();
      expect(await page.evaluate(() => history.length)).toBe(length);
    } finally {
      backGate.resolve();
      forwardGate.resolve();
    }
  });

function capturePhasePath(capture, phase) {
  if (phase === 'catalog') return '/api/projects';
  return (
    '/p/' +
    capture.project +
    '/api/' +
    {settings: 'knowledge/retrieval-settings', list: 'notes', status: 'status', note: 'note', locks: 'locks'}[phase]
  );
}
for (const phase of ['catalog', 'settings', 'list', 'status', 'note', 'locks'])
  test('capture open ' + phase + ' failure keeps a valid visible outcome and can retry', async ({page}) => {
    const label = 'Owned capture ' + phase,
      {capture, card} = await savedInboxCapture(page, label),
      inboxURL = page.url(),
      length = await page.evaluate(() => history.length),
      pathname = capturePhasePath(capture, phase),
      pattern = '**' + pathname + '*',
      message = 'CURRENT_CAPTURE_' + phase.toUpperCase();
    await page.route(pattern, route =>
      new URL(route.request().url()).pathname === pathname
        ? route.fulfill({status: 500, body: message})
        : route.continue()
    );
    await card.getByRole('link', {name: 'Open note', exact: true}).click();
    if (['settings', 'list'].includes(phase)) {
      await expect(page.locator('#path')).toHaveText(label);
      await expect(page.locator('#document')).toBeVisible();
      await expect(page.locator('#notice')).toHaveText(message);
      await expect(page.locator('#notice')).toBeVisible();
      expect(await page.evaluate(() => history.length)).toBe(length + 1);
      await page.goBack();
      await expect(page).toHaveURL(inboxURL);
      await expect(page.locator('#inbox-view')).toBeVisible();
    } else {
      await expect(page.locator('#notice:visible, #inbox-content .jev-error:visible')).toContainText([message]);
      await expect(page.locator('#inbox-view')).toBeVisible();
      await expect(card.getByRole('link', {name: 'Open note', exact: true})).toBeVisible();
      await expect(page.locator('#empty')).toBeHidden();
      await expect(page.locator('#document')).toBeHidden();
      await expect(page.locator('#path')).toHaveText('Inbox');
      await expect(page).toHaveURL(inboxURL);
      expect(await page.evaluate(() => history.length)).toBe(length);
    }
    await page.unroute(pattern);
    await card.getByRole('link', {name: 'Open note', exact: true}).click();
    await expect(page.locator('#path')).toHaveText(label);
    await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
    await expect(page.locator('#inbox-view')).toBeHidden();
    await expect(page.locator('#notice')).not.toContainText(message);
    expect(await page.evaluate(() => history.length)).toBe(length + 1);
  });
for (const phase of ['catalog', 'settings', 'list', 'status', 'note', 'locks'])
  for (const outcome of ['success', 'failure'])
    test('retired capture ' + phase + ' ' + outcome + ' cannot replace a newer draft or notice', async ({page}) => {
      const {capture, card} = await savedInboxCapture(page, 'Retired ' + phase + ' ' + outcome),
        pathname = capturePhasePath(capture, phase),
        gate = deferred(),
        held = deferred(),
        released = deferred();
      let blocked = false;
      await page.route('**' + pathname + '*', async route => {
        if (blocked || new URL(route.request().url()).pathname !== pathname) {
          await route.continue();
          return;
        }
        blocked = true;
        const response = await route.fetch();
        held.resolve();
        await gate.promise;
        try {
          if (outcome === 'failure') await route.fulfill({status: 500, body: 'RETIRED_CAPTURE_PHASE_ERROR'});
          else await route.fulfill({response});
        } finally {
          released.resolve();
        }
      });
      try {
        await card.getByRole('link', {name: 'Open note', exact: true}).click();
        await held.promise;
        await page.route('**/p/other/api/live*', route => route.fulfill({status: 500, body: 'NEWER_LIVE_OFFLINE'}));
        await page.locator('#project-select').selectOption('other');
        await expect(page.locator('#folder-browser')).toBeVisible();
        await openLibraryNote(page, 'same.md');
        await expect(page.locator('#path')).toHaveText('other title');
        await expect(page.locator('#notice')).toHaveText('NEWER_LIVE_OFFLINE');
        await page.locator('#mode').click();
        await page.locator('#format-source').click();
        const draft = 'Newer private draft after ' + phase + ' ' + outcome + '\n';
        await fillSource(page, draft);
        await page.route('**/p/other/api/note', route =>
          route.request().method() === 'POST'
            ? route.fulfill({status: 500, body: 'NEWER_SAVE_FAILURE'})
            : route.continue()
        );
        await page.locator('#save').click();
        await expect(page.locator('#notice')).toHaveText('NEWER_SAVE_FAILURE');
        const newerURL = page.url(),
          length = await page.evaluate(() => history.length);
        gate.resolve();
        await released.promise;
        await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
        await expect(page).toHaveURL(newerURL);
        expect(await page.evaluate(() => history.length)).toBe(length);
        await expect(page.locator('#project-select')).toHaveValue('other');
        await expect(page.locator('#inbox-view')).toBeHidden();
        await expectEditorText(page, draft);
        await expect(page.locator('#notice')).toHaveText('NEWER_SAVE_FAILURE');
        await expect(page.locator('#inbox-content .jev-error')).not.toContainText('RETIRED_CAPTURE_PHASE_ERROR');
      } finally {
        gate.resolve();
      }
    });

async function openLibraryNote(page, path) {
  if (await page.locator('#folder-browser').isVisible())
    await page.locator('#folder-items a[data-note="' + path + '"]').click();
  else {
    await ensureSearch(page);
    await page.locator('#search').fill('');
    await page.locator('#search-results a[data-note="' + path + '"]').click();
  }
}

async function ensureSearch(page) {
  if (await page.locator('#search-view').isHidden()) {
    if (await page.locator('#sidebar').isHidden()) await page.locator('#menu').click();
    await page.locator('#search-open').click();
  } else if (page.viewportSize()?.width <= 720 && (await page.locator('#sidebar').isVisible()))
    await page.locator('#menu').click();
}
