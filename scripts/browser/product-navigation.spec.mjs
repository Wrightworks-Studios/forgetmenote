import {test, expect} from '@playwright/test';
import {startApp} from './app-fixture.mjs';
let app, cookies;
test.beforeAll(async ({browser}) => {
  app = await startApp({
    name: 'fmn-product-navigation-',
    account: {username: 'product-reader', password: 'SyntheticProduct123!'}
  });
  await app.api('/p/default/api/note', {
    path: 'Product.md',
    content: '# Product context\n\n## Details\n\nA readable note.\n',
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
});
test.afterAll(async () => app?.close());
test.beforeEach(async ({page, context}) => {
  page.on('pageerror', error => {
    throw error;
  });
  await context.addCookies(cookies);
});

test('the notebook map belongs to the library and stays hidden in notes and Inbox', async ({page}) => {
  await page.goto(app.origin + '/?project=default&folder=');
  await expect(page.locator('#folder-browser')).toBeVisible();
  await expect(page.locator('#notebook-outline')).toBeVisible();
  await page.locator('#search-open').click();
  await expect(page.locator('#notebook-outline')).toBeHidden();
  await page.locator('#search-close').click();
  await expect(page.locator('#notebook-outline')).toBeVisible();
  await page.goto(app.origin + '/?project=default&note=Product.md');
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await expect(page.locator('#notebook-outline')).toBeHidden();
  await page.locator('#inbox-open').click();
  await expect(page.locator('#inbox-view')).toBeVisible();
  await expect(page.locator('#notebook-outline')).toBeHidden();
});

test('notebook creation is a button rather than an option in the navigation picker', async ({page}) => {
  await page.goto(app.origin + '/?project=default&folder=');
  await expect(page.locator('#new-note')).toBeEnabled();
  await expect(page.locator('#project-select option[value="@create"]')).toHaveCount(0);
  await page.getByRole('button', {name: 'Add notebook', exact: true}).click();
  await page.getByRole('button', {name: 'New notebook', exact: true}).click();
  await expect(page.locator('#workspace-create-dialog')).toBeVisible();
});

test('library Tags starts browsing tags without a previous search query', async ({page}) => {
  await page.goto(app.origin + '/?project=default&folder=');
  await expect(page.locator('#new-note')).toBeEnabled();
  await page.locator('#search-open').click();
  await page.locator('#search').fill('OLD UNRELATED QUERY');
  await page.locator('#search-close').click();
  await page.locator('#library-tags').click();
  await expect(page.locator('#tag-filter')).toBeFocused();
  await expect(page.locator('#search')).toHaveValue('');
});

test('a search route preserves its query and notebook scope through reload and browser history', async ({page}) => {
  await app.api('/api/projects', {id: 'search-route-book', name: 'Search route notebook'});
  await app.api('/p/search-route-book/api/note', {
    path: 'Route.md',
    content: '# Route\n\nUniqueRouteNeedle is visible.\n',
    revision: ''
  });
  await page.goto(app.origin + '/?project=default&folder=');
  await expect(page.locator('#new-note')).toBeEnabled();
  await page.locator('#search-open').click();
  await page.locator('#search-projects').getByRole('checkbox', {name: 'Search route notebook', exact: true}).check();
  const received = page.waitForResponse(
    response =>
      response.url().endsWith('/api/retrieval') && response.request().postDataJSON().query === 'UniqueRouteNeedle'
  );
  await page.locator('#search').fill('UniqueRouteNeedle');
  expect((await received).status()).toBe(200);
  await expect(page.locator('#search-results')).toContainText('UniqueRouteNeedle');
  expect(new URL(page.url()).searchParams.get('q')).toBe('UniqueRouteNeedle');
  await page.reload();
  await expect(page.locator('#search-view')).toBeVisible();
  await expect(page.locator('#search')).toHaveValue('UniqueRouteNeedle');
  await expect(
    page.locator('#search-projects').getByRole('checkbox', {name: 'Search route notebook', exact: true})
  ).toBeChecked();
  await expect(page.locator('#search-results')).toContainText('UniqueRouteNeedle');
  await page.locator('#search-close').click();
  await expect(page.locator('#folder-browser')).toBeVisible();
  await page.goBack();
  await expect(page.locator('#search-view')).toBeVisible();
  await expect(page.locator('#search')).toHaveValue('UniqueRouteNeedle');
});

for (const width of [390, 1440])
  test('note context has a persistent toggle and returns focus after closing at ' + width, async ({page}) => {
    await page.setViewportSize({width, height: 1000});
    await page.goto(app.origin + '/?project=default&note=Product.md');
    await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
    await expect(page.getByRole('button', {name: /Note context/})).toBeVisible();
    await page.getByRole('button', {name: /Note context/}).click();
    await expect(page.locator('#comments-panel')).toBeVisible();
    await expect(page.locator('#note-context-contents')).toContainText('Details');
    await page.locator('[data-context-tab=comments]').click();
    await page.locator('#comments-close').click();
    await expect(page.getByRole('button', {name: /Note context/})).toBeFocused();
  });

test('a newer navigation cancels the note or folder request it supersedes', async ({page}) => {
  await app.api('/p/default/api/note', {path: 'Superseded.md', content: '# Superseded\n', revision: ''});
  await app.api('/p/default/api/note', {path: 'Held/Inside.md', content: '# Inside\n', revision: ''});
  // Records each request the page cancelled before it settled.
  await page.addInitScript(() => {
    const fetchURL = window.fetch;
    window.cancelledRequests = [];
    window.fetch = (url, options = {}) => {
      let settled = false;
      options.signal?.addEventListener('abort', () => {
        if (!settled) window.cancelledRequests.push(String(url));
      });
      return fetchURL(url, options).finally(() => {
        settled = true;
      });
    };
  });
  const held = [];
  for (const [pathname, path] of [
    ['/p/default/api/note', 'Superseded.md'],
    ['/p/default/api/browse', 'Held']
  ])
    await page.route(
      url => url.pathname === pathname && url.searchParams.get('path') === path,
      route => held.push(route)
    );
  const cancelled = () => page.evaluate(() => window.cancelledRequests);
  await page.goto(app.origin + '/?project=default&folder=');
  const library = page.locator('#folder-items');
  await expect(library.locator('a[data-note="Product.md"]')).toBeVisible();
  await library.locator('a[data-note="Superseded.md"]').click();
  await expect.poll(() => held.length).toBe(1);
  expect(await cancelled()).toEqual([]);
  await page.locator('#folder-groups a[title="Held"]').click();
  await expect.poll(() => held.length).toBe(2);
  expect(await cancelled()).toEqual(['/p/default/api/note?path=Superseded.md']);
  await library.locator('a[data-note="Product.md"]').click();
  await expect(page.locator('#path')).toHaveText('Product context');
  expect(await cancelled()).toEqual([
    '/p/default/api/note?path=Superseded.md',
    '/p/default/api/browse?path=Held&collection=notes'
  ]);
});

test('saved status describes its state and keeps the revision available as provenance', async ({page}) => {
  await page.goto(app.origin + '/?project=default&note=Product.md');
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await expect(page.locator('#save-state')).toHaveText('Saved');
  await expect(page.locator('#save-state')).toHaveAttribute('title', /Saved version · [a-f0-9]{7}/);
});

test('a disabled personal assistant explains the next step and hides opt-in preferences', async ({page}) => {
  await page.goto(app.origin + '/account/assistant');
  await expect(page.locator('#assistant-message')).toContainText('administrator');
  await expect(page.locator('#assistant-preferences')).toBeHidden();
});

test('search notebook scope uses named checkboxes and preserves the explicit selected notebook IDs', async ({page}) => {
  await app.api('/api/projects', {id: 'product-second', name: 'Second product notebook'});
  await page.goto(app.origin + '/?project=default&folder=');
  await expect(page.locator('#new-note')).toBeEnabled();
  await page.locator('#search-open').click();
  await expect(page.locator('#search-projects').getByRole('checkbox', {name: 'My notes', exact: true})).toBeChecked();
  await page.locator('#search-projects').getByRole('checkbox', {name: 'Second product notebook', exact: true}).check();
  const sent = page.waitForRequest(
    request => request.url().endsWith('/api/retrieval') && request.postDataJSON().query === 'Health'
  );
  await page.locator('#search').fill('Health');
  expect((await sent).postDataJSON().projects.sort()).toEqual(['default', 'product-second']);
});

test('the visual editor toolbar exposes Insert without clipping its control at desktop width', async ({page}) => {
  await page.setViewportSize({width: 1440, height: 1000});
  await page.goto(app.origin + '/?project=default&note=Product.md');
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await page.locator('#mode').click();
  await expect(page.locator('#format-visual')).toHaveAttribute('aria-pressed', 'true');
  const insert = page.getByRole('combobox', {name: 'Insert block', exact: true});
  await expect(insert).toBeVisible();
  const control = await insert.boundingBox(),
    tools = await page.locator('#editor-tools').boundingBox();
  expect(control.x + control.width).toBeLessThanOrEqual(tools.x + tools.width + 1);
  await insert.focus();
  await expect(insert).toBeFocused();
});

test('wide tables and code keep the reading measure and scroll within it', async ({page}) => {
  await app.api('/p/default/api/note', {
    path: 'Wide.md',
    content:
      '# Wide reading\n\nA readable paragraph.\n\n| Name | Value |\n| --- | --- |\n| Example | ' +
      'longword'.repeat(30) +
      ' |\n\n```\n' +
      'long code '.repeat(30) +
      '\n```\n',
    revision: ''
  });
  await page.goto(app.origin + '/?project=default&note=Wide.md');
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  const paragraph = await page.locator('#preview > p').first().boundingBox();
  for (const selector of ['#preview .table-scroll', '#preview > pre']) {
    const block = await page.locator(selector).boundingBox();
    expect(block.width).toBeLessThanOrEqual(paragraph.width + 1);
  }
  expect(await page.locator('#preview .table-scroll').evaluate(node => node.scrollWidth > node.clientWidth)).toBe(true);
});

test('note creation belongs to the sidebar and library rather than the note inspection menu', async ({page}) => {
  await page.goto(app.origin + '/?project=default&note=Product.md');
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await page.locator('#note-details-toggle').click();
  await expect(page.locator('#note-details-actions').getByRole('button', {name: 'New note', exact: true})).toHaveCount(
    0
  );
  await expect(
    page.locator('#note-details-actions').getByRole('button', {name: 'Import source', exact: true})
  ).toHaveCount(0);
  await page.locator('#note-details-toggle').click();
  await page.locator('#sidebar').getByRole('button', {name: 'New note', exact: true}).click();
  await expect(page.locator('#new-dialog')).toBeVisible();
  await expect(page.locator('#new-note-panel')).toBeVisible();
  await page.locator('#creation-close').click();
  await page.locator('#sidebar').getByRole('button', {name: 'Import source', exact: true}).click();
  await expect(page.locator('#new-source-panel')).toBeVisible();
});

test('phone formatting expansion changes the tools shown and collapses again', async ({page}) => {
  await page.setViewportSize({width: 390, height: 844});
  await page.goto(app.origin + '/?project=default&note=Product.md');
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await page.locator('#mode').click();
  await expect(page.locator('#editor')).toHaveAttribute('aria-label', 'Note content');
  const before = await page.locator('#editor-tools').boundingBox();
  await page.getByRole('button', {name: 'More tools', exact: true}).click();
  const expanded = await page.locator('#editor-tools').boundingBox();
  expect(expanded.height).toBeGreaterThan(before.height + 1);
  await page.getByRole('button', {name: 'Fewer tools', exact: true}).click();
  expect((await page.locator('#editor-tools').boundingBox()).height).toBe(before.height);
});

test('account actions offer direct sign out without opening settings', async ({page}) => {
  await page.goto(app.origin + '/?project=default&folder=');
  await expect(page.locator('#account-actions-toggle')).toBeEnabled();
  await page.locator('#account-actions-toggle').click();
  await expect(page.locator('#account-actions').getByRole('button', {name: 'Sign out', exact: true})).toBeVisible();
  await expect(page.locator('#account-actions form')).toHaveAttribute('method', 'post');
  await expect(page.locator('#account-actions form')).toHaveAttribute('action', '/logout');
});

test('search across notebooks displays plain note snippets and retains raw passages', async ({page}) => {
  await app.api('/api/projects', {id: 'scoped-preview', name: 'Scoped preview notebook'});
  const content = '# Table preview\n\n| Field | Value |\n| --- | --- |\n| UniqueScopedNeedle | Visible final row |\n';
  await app.api('/p/scoped-preview/api/note', {path: 'Table.md', content, revision: ''});
  await page.goto(app.origin + '/?project=default&folder=');
  await expect(page.locator('#new-note')).toBeEnabled();
  await page.locator('#search-open').click();
  await page.locator('#search-projects').getByRole('checkbox', {name: 'Scoped preview notebook', exact: true}).check();
  await page.locator('#search-options-toggle').click();
  await page.locator('#advanced-search > summary').click();
  await page.locator('input[name="search-mode"][value="text"]').check();
  const received = page.waitForResponse(
    response =>
      response.url().endsWith('/api/retrieval') && response.request().postDataJSON().query === 'UniqueScopedNeedle'
  );
  await page.locator('#search').fill('UniqueScopedNeedle');
  const response = await received;
  expect(response.status()).toBe(200);
  const result = await response.json(),
    hit = result.results.find(item => item.note?.text.includes('UniqueScopedNeedle'));
  expect(hit.note.text).toContain('| UniqueScopedNeedle |');
  expect(hit.excerpt).not.toContain('|');
  await expect(page.locator('.search-excerpt').filter({hasText: 'UniqueScopedNeedle'})).not.toContainText('|');
});

test('a failed new-note discussion cannot display the previous note comments', async ({page}) => {
  const first = await app.api('/p/default/api/note', {
    path: 'Discussion-first.md',
    content: '# First note\n\n[Second note](Discussion-second.md)\n',
    revision: ''
  });
  await app.api('/p/default/api/comments', {
    path: 'Discussion-first.md',
    text: 'COMMENT ONLY ON FIRST NOTE',
    revision: first.revision
  });
  await app.api('/p/default/api/note', {
    path: 'Discussion-second.md',
    content: '# Second note\n\nNo comments.\n',
    revision: ''
  });
  await page.goto(app.origin + '/?project=default&note=Discussion-first.md');
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await page.locator('#note-context-toggle').click();
  await expect(page.locator('#comments-list')).toContainText('COMMENT ONLY ON FIRST NOTE');
  await page.locator('#comments-close').click();
  await page.route('**/api/comments?*', route =>
    route.request().method() === 'GET' &&
    new URL(route.request().url()).searchParams.get('path') === 'Discussion-second.md'
      ? route.fulfill({status: 503, json: {error: 'Synthetic new discussion unavailable'}})
      : route.continue()
  );
  await page.locator('#preview a').filter({hasText: 'Second note'}).click();
  await expect(page.locator('#note-path')).toHaveText('Discussion-second.md');
  await expect(page.locator('#notice')).toContainText('Synthetic new discussion unavailable');
  await page.locator('#note-context-toggle').click();
  await expect(page.locator('#comments-note')).toHaveText('Discussion-second.md');
  await expect(page.locator('#comments-list')).not.toContainText('COMMENT ONLY ON FIRST NOTE');
  await expect(page.locator('#comments-list')).toContainText('Synthetic new discussion unavailable');
  await expect(page.getByRole('button', {name: 'Retry loading comments', exact: true})).toBeVisible();
  await page.locator('#comments-close').click();
  await expect(page.locator('#note-context-toggle')).toBeFocused();
});

test('an unavailable bookmarked Search scope fails closed without disabling notebook navigation', async ({page}) => {
  const retrieval = [];
  page.on('request', request => {
    if (request.url().endsWith('/api/retrieval')) retrieval.push(request.postDataJSON());
  });
  await page.goto(app.origin + '/?project=default&search=1&q=PRIVATE_NEEDLE&search-notebook=revoked-book');
  await expect(page.locator('#notice')).toContainText('unavailable');
  await expect(page.locator('#search-view')).toBeHidden();
  await expect(page.locator('#settings-open')).toBeEnabled();
  await expect(page.locator('#new-note')).toBeEnabled();
  expect(retrieval).toEqual([]);
  expect(new URL(page.url()).searchParams.has('search')).toBe(false);
  await page.locator('#settings-open').click();
  await expect(page.locator('#settings-dialog')).toBeVisible();
});

test('malformed Search history cannot restore a revoked saved scope into another notebook', async ({page}) => {
  await page.goto(app.origin + '/?project=default&folder=');
  await expect(page.locator('#new-note')).toBeEnabled();
  await page.locator('#search-open').click();
  const saved = await page.evaluate(() => history.state.search);
  expect(saved).toBeTruthy();
  await page.locator('#search-close').click();
  await expect(page.locator('#folder-browser')).toBeVisible();
  const requests = [];
  page.on('request', request => {
    if (request.url().includes('/api/knowledge/hybrid-search') || request.url().endsWith('/api/retrieval'))
      requests.push(request.postDataJSON());
  });
  await page.evaluate(saved => {
    history.replaceState(
      {search: {...saved, notebooks: ['revoked-book'], values: {...saved.values, search: 'PRIVATE_HISTORICAL_NEEDLE'}}},
      '',
      '/?project=default&folder=&search=malformed'
    );
    history.pushState(null, '', '/?project=default&folder=');
  }, saved);
  await page.goBack();
  await expect(page.locator('#folder-browser')).toBeVisible();
  await expect(page.locator('#search-view')).toBeHidden();
  await expect(page.locator('#settings-open')).toBeEnabled();
  expect(
    requests.some(body => body?.text === 'PRIVATE_HISTORICAL_NEEDLE' || body?.query === 'PRIVATE_HISTORICAL_NEEDLE')
  ).toBe(false);
});
