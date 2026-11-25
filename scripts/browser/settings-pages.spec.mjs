import {test, expect} from '@playwright/test';
import {startApp} from './app-fixture.mjs';
import {expectEditorText, fillSource} from './editor-text.mjs';

let app, cookies;
test.beforeAll(async ({browser}) => {
  app = await startApp({
    name: 'fmn-settings-pages-',
    account: {username: 'settings-admin', password: 'DisposableSettings123!'}
  });
  await app.api('/p/default/api/note', {
    path: 'Settings.md',
    content: '# Settings context\n\nKeep this notebook note.\n',
    revision: ''
  });
  const context = await browser.newContext(),
    page = await context.newPage();
  try {
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

test('notebook settings clicked while the app is still starting open once the notebook is ready', async ({page}) => {
  let release;
  const notebooksLoaded = new Promise(resolve => {
    release = resolve;
  });
  await page.route(
    url => url.pathname === '/api/projects',
    async route => {
      await notebooksLoaded;
      await route.continue();
    }
  );
  await page.goto(app.origin + '/?project=default&note=Settings.md');
  const opening = page.locator('#settings-open').click();
  // Until a notebook is selected the button cannot open anything, so it must not take the click.
  await expect(page.locator('#settings-open')).toBeDisabled();
  release();
  await opening;
  await expect(page.locator('#settings-dialog')).toBeVisible();
  await expect(page.locator('#settings-project')).toHaveText('My notes');
});

test('notebook settings are a routable page whose category survives reload and browser history', async ({page}) => {
  await page.goto(app.origin + '/?project=default&note=Settings.md');
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await page.locator('#settings-open').click();
  await page.locator('#settings-dialog [data-settings-category=search]').click();
  await expect(page).toHaveURL(/page=notebook/);
  await expect(page).toHaveURL(/category=search/);
  expect(await page.locator('#settings-dialog').evaluate(node => node.matches(':modal'))).toBe(false);
  // Compare settled layouts rather than two frames of the entrance transform.
  await page.locator('#settings-dialog').evaluate(async panel => {
    await Promise.all(panel.getAnimations().map(animation => animation.finished));
  });
  const heading = await page.locator('#settings-dialog h2').boundingBox();
  await page.locator('#settings-dialog [data-settings-category=history]').click();
  await page.locator('#settings-dialog').evaluate(async panel => {
    await Promise.all(panel.getAnimations().map(animation => animation.finished));
  });
  expect((await page.locator('#settings-dialog h2').boundingBox()).y).toBe(heading.y);
  await page.reload();
  await expect(page.locator('#settings-dialog')).toBeVisible();
  await expect(page.locator('#backup-link')).toBeVisible();
  await page.goBack();
  await expect(page.locator('#retrieval-defaults-form')).toBeVisible();
  await expect(page).toHaveURL(/category=search/);
  await page.keyboard.press('Escape');
  await expect(page.locator('#settings-dialog')).toBeHidden();
  await expect(page.locator('#document')).toBeVisible();
  await expect(page.locator('#settings-open')).toBeFocused();
  expect(new URL(page.url()).searchParams.has('page')).toBe(false);
});

test('first visits follow the system theme and explicit choices survive system changes and reload', async ({page}) => {
  await page.emulateMedia({colorScheme: 'dark'});
  await page.goto(app.origin + '/?project=default&folder=');
  await expect(page.locator('#new-note')).toBeEnabled();
  await expect(page.locator('html')).toHaveAttribute('data-theme', 'dark');
  expect(await page.evaluate(() => getComputedStyle(document.body).backgroundColor)).toBe('rgb(29, 33, 31)');
  await page.locator('#account-actions-toggle').click();
  await page.locator('#account-open').click();
  await page.locator('#theme input[value=parchment]').check();
  await page.emulateMedia({colorScheme: 'light'});
  await page.reload();
  await expect(page.locator('html')).toHaveAttribute('data-theme', 'parchment');
  await page.locator('#theme input[value=system]').check();
  await page.emulateMedia({colorScheme: 'dark'});
  await expect(page.locator('html')).toHaveAttribute('data-theme', 'dark');
  expect(await page.evaluate(() => getComputedStyle(document.body).backgroundColor)).toBe('rgb(29, 33, 31)');
});

for (const account of ['github', 'oidc'])
  test('legacy ' + account + ' account return opens the canonical account page', async ({page}) => {
    await page.goto(app.origin + '/?project=default&account=' + account);
    await expect(page.locator('#account-dialog')).toBeVisible();
    await expect(page.locator('#account-summary')).toBeVisible();
    await expect(page).toHaveURL(/page=account/);
    expect(new URL(page.url()).searchParams.has('account')).toBe(false);
  });

test('account and Administration categories remain pages across reload', async ({page}) => {
  await page.goto(app.origin + '/?project=default&page=account&category=account');
  await expect(page.locator('#account-summary')).toBeVisible();
  await page.keyboard.press('Escape');
  await page.locator('#account-actions-toggle').click();
  await page.locator('#admin-open').click();
  await page.locator('#admin-dialog [data-settings-category=groups]').click();
  await expect(page).toHaveURL(/page=administration/);
  await expect(page).toHaveURL(/category=groups/);
  await page.reload();
  await expect(page.locator('#admin-dialog')).toBeVisible();
  await expect(page.locator('#admin-dialog [data-settings-panel=groups]')).toBeVisible();
  expect(await page.locator('#admin-dialog').evaluate(node => node.matches(':modal'))).toBe(false);
});

test('a late account page cannot replace newer notebook settings', async ({page}) => {
  await page.goto(app.origin + '/?project=default&note=Settings.md');
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  let release, arrived;
  const waiting = new Promise(resolve => (release = resolve)),
    started = new Promise(resolve => (arrived = resolve));
  await page.route('**/account', async route => {
    const response = await route.fetch();
    arrived();
    await waiting;
    await route.fulfill({response});
  });
  try {
    await page.locator('#account-actions-toggle').click();
    await page.locator('#account-open').click();
    await started;
    await page.locator('#settings-open').click();
    await expect(page.locator('#settings-dialog')).toBeVisible();
    release();
    await expect(page.locator('#account-dialog')).toBeHidden();
    await expect(page).toHaveURL(/page=notebook/);
  } finally {
    release();
  }
});

test('visiting settings preserves the current unsent private note without publishing it', async ({page}) => {
  await page.goto(app.origin + '/?project=default&note=Settings.md');
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await page.route('**/api/live/patch', route => route.abort());
  await page.locator('#mode').click();
  await page.locator('#format-source').click();
  await fillSource(page, 'PRIVATE SETTINGS DRAFT');
  await page.locator('#settings-open').click();
  await expect(page.locator('#settings-dialog')).toBeVisible();
  await page.locator('[data-close=settings-dialog]').click();
  await expectEditorText(page, 'PRIVATE SETTINGS DRAFT');
  const saved = await (await page.request.get(app.origin + '/p/default/api/note?path=Settings.md')).json();
  expect(saved.content).not.toContain('PRIVATE SETTINGS DRAFT');
});

test('settings category history retains a private draft without restarting notebook navigation', async ({page}) => {
  await page.goto(app.origin + '/?project=default&note=Settings.md');
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await page.route('**/api/live/patch', route => route.abort());
  await page.locator('#mode').click();
  await page.locator('#format-source').click();
  await fillSource(page, 'PRIVATE CATEGORY DRAFT');
  await page.locator('#settings-open').click();
  await page.locator('#settings-dialog [data-settings-category=search]').click();
  await page.locator('#settings-dialog [data-settings-category=history]').click();
  await page.goBack();
  await expect(page.locator('#retrieval-defaults-form')).toBeVisible();
  await expect(page.locator('dialog[data-confirm-action]')).toHaveCount(0);
  await page.goForward();
  await expect(page.locator('#backup-link')).toBeVisible();
  await page.keyboard.press('Escape');
  await expectEditorText(page, 'PRIVATE CATEGORY DRAFT');
});

test('settings retain a pinned repository document route across reload and return', async ({page}) => {
  const revision = 'a'.repeat(40),
    ref = {project: 'default', origin: 'repository', source: 'b'.repeat(32), revision, path: 'Guide.md'};
  await page.route('**/p/default/api/document?*', route =>
    route.fulfill({
      json: {
        ...ref,
        content: '# Pinned repository guide\n',
        html: '<h1>Pinned repository guide</h1>',
        citation: 'https://github.com/example/source/blob/' + revision + '/Guide.md',
        hash: 'captured',
        editable: false
      }
    })
  );
  await page.route('**/p/default/api/documents?*', route =>
    route.fulfill({json: {...ref, total: 1, entries: [{...ref, supported: true}]}})
  );
  await page.goto(app.origin + '/?' + new URLSearchParams({...ref, page: 'account', category: 'account'}));
  await expect(page.locator('#account-summary')).toBeVisible();
  await expect(page).toHaveURL(/revision=aaaaaaaa/);
  await page.reload();
  await expect(page.locator('#account-summary')).toBeVisible();
  await page.keyboard.press('Escape');
  await expect(page.locator('#source-document')).toBeVisible();
  await expect(page.locator('#source-document')).toContainText('Pinned repository guide');
});

test('a settings bookmark retains a historical note revision and returns to its preview', async ({page}) => {
  const note = await app.api('/p/default/api/note?path=Settings.md');
  await page.goto(
    app.origin +
      '/?' +
      new URLSearchParams({
        project: 'default',
        note: 'Settings.md',
        revision: note.revision,
        page: 'notebook',
        category: 'search'
      })
  );
  await expect(page.locator('#settings-dialog')).toBeVisible();
  await expect(page.locator('#settings-dialog')).toBeInViewport();
  await expect(page.locator('#knowledge-dialog')).toBeHidden();
  await page.keyboard.press('Escape');
  await expect(page.locator('#knowledge-dialog')).toBeVisible();
  await expect(page.locator('#knowledge-body')).toContainText('Settings context');
  expect(new URL(page.url()).searchParams.get('revision')).toBe(note.revision);
});

test('browser Back to a private draft and Forward to settings retain the same note view', async ({page}) => {
  await page.goto(app.origin + '/?project=default&note=Settings.md');
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await page.route('**/api/live/patch', route => route.abort());
  await page.locator('#mode').click();
  await page.locator('#format-source').click();
  await fillSource(page, 'PRIVATE FORWARD DRAFT');
  await page.locator('#settings-open').click();
  await expect(page.locator('#settings-dialog')).toBeVisible();
  await page.goBack();
  await expectEditorText(page, 'PRIVATE FORWARD DRAFT');
  await page.goForward();
  await expect(page.locator('#settings-dialog')).toBeVisible();
  await expect(page.locator('dialog[data-confirm-action]')).toHaveCount(0);
  await page.keyboard.press('Escape');
  await expectEditorText(page, 'PRIVATE FORWARD DRAFT');
});

test('a phone settings page closes the sidebar backdrop and returns focus to a visible control', async ({page}) => {
  await page.setViewportSize({width: 390, height: 844});
  await page.goto(app.origin + '/?project=default&note=Settings.md');
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  if (await page.locator('#sidebar').isHidden()) await page.locator('#menu').click();
  await page.locator('#settings-open').click();
  await expect(page.locator('#settings-dialog')).toBeVisible();
  await expect(page.locator('#sidebar-backdrop')).toBeHidden();
  await page.locator('#settings-dialog [data-settings-category=search]').click();
  await expect(page.locator('#retrieval-defaults-form')).toBeVisible();
  await page.keyboard.press('Escape');
  expect(await page.evaluate(() => !!document.activeElement.getClientRects().length)).toBe(true);
  await expect(page.locator('#document')).toBeVisible();
});

test('secondary account pages follow System and retain explicit Parchment', async ({page}) => {
  await page.emulateMedia({colorScheme: 'dark'});
  for (const path of ['/account/connect', '/account/assistant']) {
    await page.goto(app.origin + path);
    await expect(page.locator('html')).toHaveAttribute('data-theme', 'dark');
    expect(await page.evaluate(() => getComputedStyle(document.body).backgroundColor)).toBe('rgb(29, 33, 31)');
  }
  await page.emulateMedia({colorScheme: 'light'});
  await expect(page.locator('html')).toHaveAttribute('data-theme', 'light');
  await page.evaluate(() => localStorage.setItem('gitnotes:theme', 'parchment'));
  await page.goto(app.origin + '/account/connect');
  await expect(page.locator('html')).toHaveAttribute('data-theme', 'parchment');
});

test('a repository folder bookmark restores its settings page and returns to the folder', async ({page}) => {
  await page.route('**/p/default/api/documents?*', route =>
    route.fulfill({
      json: {
        project: 'default',
        origin: 'repository',
        source: 'b'.repeat(32),
        revision: 'a'.repeat(40),
        entries: [],
        total: 0
      }
    })
  );
  await page.goto(app.origin + '/?project=default&origin=repository&folder=docs&page=account&category=account');
  await expect(page.locator('#account-summary')).toBeVisible();
  await expect(page).toHaveURL(/page=account/);
  await page.keyboard.press('Escape');
  await expect(page.locator('#source-browser')).toBeVisible();
  expect(new URL(page.url()).searchParams.get('folder')).toBe('docs');
});

for (const endpoint of ['organization_status', 'list_data_connections', 'automation_status'])
  test('a stale nested ' + endpoint + ' response cannot overwrite newer Administration', async ({page}) => {
    await page.goto(app.origin + '/?project=default&note=Settings.md');
    await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
    let calls = 0,
      release,
      arrived,
      finished;
    const waiting = new Promise(resolve => (release = resolve)),
      started = new Promise(resolve => (arrived = resolve)),
      done = new Promise(resolve => (finished = resolve));
    const pattern =
      endpoint === 'list_data_connections'
        ? '**/integrations/actions/' + endpoint
        : '**/api/organization/action/' + endpoint;
    await page.route(pattern, async route => {
      const response = await route.fetch(),
        body = await response.json(),
        first = ++calls === 1;
      if (endpoint === 'organization_status')
        body.organization.name = first ? 'STALE ORGANIZATION' : 'LATEST ORGANIZATION';
      if (endpoint === 'list_data_connections')
        body.push({
          id: first ? 'stale' : 'latest',
          label: first ? 'STALE CONNECTION' : 'LATEST CONNECTION',
          kind: 'sheets'
        });
      if (endpoint === 'automation_status')
        body.jobs.push({
          id: first ? 'stale' : 'latest',
          kind: 'native',
          project: 'default',
          managedBy: 'file',
          spec: {name: first ? 'STALE AUTOMATION' : 'LATEST AUTOMATION', disabled: true}
        });
      if (first) {
        arrived();
        await waiting;
      }
      await route.fulfill({response, json: body});
      if (first) finished();
    });
    try {
      await page.locator('#account-actions-toggle').click();
      await page.locator('#admin-open').click();
      await started;
      if (await page.locator('#admin-open').isHidden()) await page.locator('#account-actions-toggle').click();
      await page.locator('#admin-open').click();
      await expect(page.locator('#admin-dialog')).toBeVisible();
      const selector =
        endpoint === 'organization_status'
          ? '#instance-name'
          : endpoint === 'list_data_connections'
            ? '#integration-connections'
            : '#integration-jobs';
      if (endpoint === 'organization_status') await expect(page.locator(selector)).toHaveValue('LATEST ORGANIZATION');
      else await expect(page.locator(selector)).toContainText('LATEST');
      release();
      await done;
      await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
      if (endpoint === 'organization_status') await expect(page.locator(selector)).toHaveValue('LATEST ORGANIZATION');
      else {
        await expect(page.locator(selector)).toContainText('LATEST');
        await expect(page.locator(selector)).not.toContainText('STALE');
      }
    } finally {
      release();
    }
  });

test('a completed old chat-client save preserves the reopened page JSON draft', async ({page}) => {
  const initial = {workspace: 'SYNTHETIC', disabled: true, users: {}, channels: {}, directMessages: {}};
  let reads = 0,
    release,
    arrived,
    finished;
  const waiting = new Promise(resolve => (release = resolve)),
    started = new Promise(resolve => (arrived = resolve)),
    done = new Promise(resolve => (finished = resolve));
  await page.route('**/api/organization/action/chat_clients', route => {
    reads++;
    return route.fulfill({
      json: {
        available: ['slack'],
        config: {revision: 'synthetic', clients: {slack: initial}},
        eventURL: app.origin + '/synthetic-events'
      }
    });
  });
  await page.route('**/api/organization/action/set_chat_clients', async route => {
    const payload = route.request().postDataJSON();
    arrived();
    await waiting;
    await route.fulfill({json: {config: payload.config}});
    finished();
  });
  try {
    await page.goto(app.origin + '/?project=default&page=administration&category=chat-clients');
    await expect(page.locator('.chat-client-form')).toBeVisible();
    await page.locator('.chat-client-form summary').click();
    await page.getByLabel('Chat client JSON', {exact: true}).fill(JSON.stringify({...initial, users: {OLD: 'OLD'}}));
    await page.getByRole('button', {name: 'Save chat client', exact: true}).click();
    await started;
    await page.keyboard.press('Escape');
    await page.locator('#account-actions-toggle').click();
    await page.locator('#admin-open').click();
    await expect(page.locator('.chat-client-form')).toBeVisible();
    await page.locator('.chat-client-form summary').click();
    const unsent = JSON.stringify({...initial, users: {CURRENT: 'UNSENT NEW DRAFT'}});
    await page.getByLabel('Chat client JSON', {exact: true}).fill(unsent);
    const before = reads;
    release();
    await done;
    await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
    await expect(page.getByLabel('Chat client JSON', {exact: true})).toHaveValue(unsent);
    expect(reads).toBe(before);
  } finally {
    release();
  }
});

test('a failed Administration history read retires the page and a fresh reopen has working connections', async ({
  page
}) => {
  await page.route('**/integrations/actions/list_data_connections', route =>
    route.fulfill({json: [{id: 'synthetic', label: 'Synthetic connection', kind: 'http'}]})
  );
  await page.route('**/integrations/actions/list_data_operations', route =>
    route.fulfill({
      json: [{name: 'read', description: 'Synthetic read', readOnly: true, inputSchema: {type: 'object'}}]
    })
  );
  await page.route('**/api/organization/action/data_connection_access', route =>
    route.fulfill({json: {connection: 'synthetic', revision: 0, users: {}, groups: {}}})
  );
  await page.goto(app.origin + '/?project=default&page=administration&category=connections');
  await expect(page.locator('#admin-dialog')).toBeVisible();
  await expect(page.locator('#account-actions-toggle')).toBeEnabled();
  await expect(page.locator('#integration-connections')).toContainText('Synthetic connection');
  await page.locator('#admin-dialog [data-settings-category=people]').click();
  let fail = true;
  await page.route('**/api/organization/action/organization_status', route =>
    fail
      ? route.fulfill({
          status: 503,
          json: {
            error: 'Synthetic organization read unavailable',
            code: 'unavailable',
            message: 'Synthetic organization read unavailable'
          }
        })
      : route.continue()
  );
  await page.goBack();
  await expect(page.locator('#notice')).toContainText('Synthetic organization read unavailable');
  await expect(page.locator('#admin-dialog')).toBeHidden();
  await expect(page.locator('#folder-browser')).toBeVisible();
  fail = false;
  await page.locator('#account-actions-toggle').click();
  await page.locator('#admin-open').click();
  await expect(page.locator('#admin-dialog')).toBeVisible();
  await page.locator('#admin-dialog [data-settings-category=connections]').click();
  await page.getByRole('button', {name: 'Synthetic connection · http', exact: true}).click();
  await expect(page.getByLabel('Connection account or group', {exact: true})).toBeVisible();
});

test('a completed old automation save retains the new editor without loading over its draft', async ({page}) => {
  const directory = await app.api('/api/admin'),
    steward = directory.users.find(user => user.id !== '@local-admin' && user.kind !== 'service' && !user.disabled),
    service = await app.api('/api/organization/action/create_service_account', {
      name: 'Synthetic draft test service',
      steward: steward.id
    });
  let reads = 0,
    release,
    arrived,
    finished;
  const waiting = new Promise(resolve => (release = resolve)),
    started = new Promise(resolve => (arrived = resolve)),
    done = new Promise(resolve => (finished = resolve));
  await page.route('**/api/organization/action/automation_status', route => {
    reads++;
    return route.fulfill({json: {jobs: [], runs: [], pythonConfigured: false}});
  });
  await page.route('**/api/organization/action/save_automation', async route => {
    arrived();
    await waiting;
    await route.fulfill({json: {id: 'old-draft', revision: 'synthetic'}});
    finished();
  });
  try {
    await page.goto(app.origin + '/?project=default&page=administration&category=automation');
    await expect(page.locator('#admin-dialog')).toBeVisible();
    await expect(page.locator('#account-actions-toggle')).toBeEnabled();
    const root = page.locator('#integration-jobs');
    await root.getByRole('button', {name: 'New automation', exact: true}).click();
    await root.getByLabel('ID', {exact: true}).fill('old-draft');
    await root.getByLabel('Service account', {exact: true}).selectOption(service.id);
    await root.getByLabel('Notebook ID', {exact: true}).fill('default');
    await root.getByRole('button', {name: 'Save automation', exact: true}).click();
    await started;
    await root.getByRole('button', {name: 'Back', exact: true}).click();
    await root.getByRole('button', {name: 'New automation', exact: true}).click();
    await root.getByLabel('ID', {exact: true}).fill('UNSENT CURRENT DRAFT');
    const before = reads;
    release();
    await done;
    await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
    await expect(root.getByLabel('ID', {exact: true})).toHaveValue('UNSENT CURRENT DRAFT');
    expect(reads).toBe(before);
  } finally {
    release();
  }
});

test('a failed automation view read has a usable retry instead of a retired visible editor', async ({page}) => {
  let fail = false;
  await page.route('**/api/organization/action/automation_status', route =>
    fail
      ? route.fulfill({
          status: 503,
          json: {
            error: 'Synthetic automation read unavailable',
            code: 'unavailable',
            message: 'Synthetic automation read unavailable'
          }
        })
      : route.fulfill({json: {jobs: [], runs: [], pythonConfigured: false}})
  );
  await page.goto(app.origin + '/?project=default&page=administration&category=automation');
  await expect(page.locator('#admin-dialog')).toBeVisible();
  await expect(page.locator('#account-actions-toggle')).toBeEnabled();
  const root = page.locator('#integration-jobs');
  await root.getByRole('button', {name: 'New automation', exact: true}).click();
  await root.getByLabel('ID', {exact: true}).fill('A VIEW LEFT BY CHOICE');
  fail = true;
  await root.getByRole('button', {name: 'Back', exact: true}).click();
  await expect(root).toContainText('Synthetic automation read unavailable');
  fail = false;
  await root.getByRole('button', {name: 'Retry loading automations', exact: true}).click();
  await root.getByRole('button', {name: 'New automation', exact: true}).click();
  await root.getByLabel('ID', {exact: true}).fill('USABLE AFTER RETRY');
  await expect(root.getByLabel('ID', {exact: true})).toHaveValue('USABLE AFTER RETRY');
});

test('notebook group sharing uses the notebook page lifetime before Administration is opened', async ({page}) => {
  await app.api('/api/organization/action/set_group', {
    id: 'settings-access',
    name: 'Settings access group',
    members: [],
    revision: 0
  });
  await page.goto(app.origin + '/?project=default&page=notebook&category=sharing');
  await expect(page.locator('#settings-dialog')).toBeVisible();
  const role = page.getByRole('combobox', {name: 'Role for group Settings access group', exact: true});
  await expect(role).toBeVisible();
  const save = role.locator('..').getByRole('button', {name: 'Save', exact: true});
  // Save is the primary action and is available only once the role changes.
  await expect(save).toHaveClass(/primary/);
  await expect(save).toBeDisabled();
  await role.selectOption('read');
  await expect(save).toBeEnabled();
  await save.click();
  await expect
    .poll(async () => {
      const directory = await app.api('/api/admin');
      return directory.projects.find(notebook => notebook.id === 'default').groupRoles?.['settings-access'];
    })
    .toBe('read');
});

test('an old Organization group save cannot erase a reopened group draft', async ({page}) => {
  let release, arrived, finished;
  const waiting = new Promise(resolve => (release = resolve)),
    started = new Promise(resolve => (arrived = resolve)),
    done = new Promise(resolve => (finished = resolve));
  await page.route('**/api/organization/action/set_group', async route => {
    arrived();
    await waiting;
    await route.fulfill({json: {id: 'old-group', name: 'Old group', revision: 1, members: []}});
    finished();
  });
  try {
    await page.goto(app.origin + '/?project=default&page=administration&category=groups');
    await expect(page.locator('#admin-dialog')).toBeVisible();
    await expect(page.locator('#account-actions-toggle')).toBeEnabled();
    await page.locator('#group-id').fill('old-group');
    await page.locator('#group-name').fill('Old group');
    await page.locator('#group-form').getByRole('button', {name: 'Save group', exact: true}).click();
    await started;
    await page.keyboard.press('Escape');
    await page.locator('#account-actions-toggle').click();
    await page.locator('#admin-open').click();
    await expect(page.locator('#admin-dialog')).toBeVisible();
    await page.locator('#group-id').fill('new-group');
    await page.locator('#group-name').fill('UNSENT CURRENT GROUP DRAFT');
    release();
    await done;
    await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
    await expect(page.locator('#group-name')).toHaveValue('UNSENT CURRENT GROUP DRAFT');
  } finally {
    release();
  }
});

for (const changePerson of [false, true])
  test(
    'a delayed account save preserves newer ' + (changePerson ? 'person selection' : 'edits to the same person'),
    async ({page}) => {
      if (changePerson)
        await app.api('/api/users', {id: 'other-settings-person', displayName: 'Other settings person'});
      let release, arrived, finished;
      const waiting = new Promise(resolve => (release = resolve)),
        started = new Promise(resolve => (arrived = resolve)),
        done = new Promise(resolve => (finished = resolve));
      await page.route('**/api/users', async route => {
        if (route.request().method() !== 'POST') return route.continue();
        arrived();
        await waiting;
        await route.fulfill({json: {}});
        finished();
      });
      try {
        await page.goto(app.origin + '/?project=default&page=administration&category=people');
        await expect(page.locator('#admin-dialog')).toBeVisible();
        await expect(page.locator('#account-actions-toggle')).toBeEnabled();
        await page.locator('#access-list button').filter({hasText: 'settings-admin'}).click();
        await page.locator('#user-display-name').fill('Earlier submitted name');
        await page.locator('#user-form').getByRole('button', {name: 'Save changes', exact: true}).click();
        await started;
        if (changePerson) await page.locator('#access-list button').filter({hasText: 'Other settings person'}).click();
        await page.locator('#user-display-name').fill('UNSENT NEWER ACCOUNT NAME');
        release();
        await done;
        await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
        await expect(page.locator('#user-display-name')).toHaveValue('UNSENT NEWER ACCOUNT NAME');
      } finally {
        release();
      }
    }
  );

for (const switchAccount of [false, true])
  test(
    'a late one-time receipt ' +
      (switchAccount ? 'cannot enter another account' : 'survives same-account page navigation'),
    async ({page}) => {
      const other = {username: 'receipt-reader', password: 'SyntheticReceiptReader123!'};
      let otherID;
      if (switchAccount) otherID = (await app.api('/api/local-users', other)).id;
      let release, arrived, finished;
      const waiting = new Promise(resolve => (release = resolve)),
        started = new Promise(resolve => (arrived = resolve)),
        done = new Promise(resolve => (finished = resolve)),
        url = 'https://synthetic.invalid/invitation/origin-account-only';
      await page.route('**/api/organization/action/create_invitation', async route => {
        arrived();
        await waiting;
        await route.fulfill({json: {url}});
        finished();
      });
      try {
        await page.goto(app.origin + '/?project=default&page=administration&category=people');
        await expect(page.locator('#admin-dialog')).toBeVisible();
        await expect(page.locator('#account-actions-toggle')).toBeEnabled();
        await page.locator('#invite-person summary').click();
        await page.locator('#invitation-name').fill('Synthetic held invitation');
        await page.locator('#invitation-create-form button').click();
        await started;
        await page.keyboard.press('Escape');
        await expect(page.locator('#admin-dialog')).toBeHidden();
        if (switchAccount) {
          const login = await page.request.post(app.origin + '/auth/password/login', {
            form: other,
            headers: {Origin: app.origin}
          });
          expect(login.ok()).toBe(true);
          const current = await (await page.request.get(app.origin + '/api/me')).json();
          expect(current.id).toBe(otherID);
          expect(current.admin).toBe(false);
          await page.locator('#account-actions-toggle').click();
          await page.locator('#admin-open').click();
          await expect(page.locator('#admin-open')).toBeHidden();
        }
        release();
        await done;
        await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
        await expect(page.locator('#organization-secret-value')).toHaveValue(switchAccount ? '' : url);
      } finally {
        release();
      }
    }
  );

test('a successful invitation receipt survives a failed Administration refresh', async ({page}) => {
  let fail = false;
  const url = 'https://synthetic.invalid/invitation/accepted-before-refresh';
  await page.route('**/api/organization/action/organization_status', route =>
    fail
      ? route.fulfill({
          status: 503,
          json: {
            error: 'Synthetic receipt refresh unavailable',
            code: 'unavailable',
            message: 'Synthetic receipt refresh unavailable'
          }
        })
      : route.continue()
  );
  await page.route('**/api/organization/action/create_invitation', async route => {
    fail = true;
    await route.fulfill({json: {url}});
  });
  await page.goto(app.origin + '/?project=default&page=administration&category=people');
  await expect(page.locator('#admin-dialog')).toBeVisible();
  await expect(page.locator('#account-actions-toggle')).toBeEnabled();
  await page.locator('#invite-person summary').click();
  await page.locator('#invitation-name').fill('Synthetic accepted invitation');
  await page.locator('#invitation-create-form button').click();
  await expect(page.locator('#notice')).toContainText('Synthetic receipt refresh unavailable');
  await expect(page.locator('#admin-dialog')).toBeHidden();
  await expect(page.locator('#organization-secret-value')).toHaveValue(url);
});

test('account edits made during a successful follow-up directory read remain unsaved', async ({page}) => {
  await page.goto(app.origin + '/?project=default&page=administration&category=people');
  await expect(page.locator('#admin-dialog')).toBeVisible();
  await expect(page.locator('#account-actions-toggle')).toBeEnabled();
  await page.locator('#access-list button').filter({hasText: 'settings-admin'}).click();
  let release, arrived, finished;
  const waiting = new Promise(resolve => (release = resolve)),
    started = new Promise(resolve => (arrived = resolve)),
    done = new Promise(resolve => (finished = resolve));
  await page.route('**/api/admin', async route => {
    const response = await route.fetch();
    expect(response.ok()).toBe(true);
    arrived();
    await waiting;
    await route.fulfill({response});
    finished();
  });
  try {
    await page.locator('#user-display-name').fill('Actually saved before refresh');
    await page.locator('#user-form').getByRole('button', {name: 'Save changes', exact: true}).click();
    await started;
    await page.locator('#user-display-name').fill('UNSENT DURING DIRECTORY READ');
    release();
    await done;
    await expect(page.locator('#admin-status')).toContainText('Account changes saved.');
    await expect(page.locator('#user-display-name')).toHaveValue('UNSENT DURING DIRECTORY READ');
  } finally {
    release();
  }
});

test('an already retained hidden receipt clears after the actual browser account changes', async ({page}) => {
  const other = {username: 'retained-receipt-reader', password: 'SyntheticRetainedReceipt123!'},
    otherID = (await app.api('/api/local-users', other)).id;
  let release, arrived, finished;
  const waiting = new Promise(resolve => (release = resolve)),
    started = new Promise(resolve => (arrived = resolve)),
    done = new Promise(resolve => (finished = resolve)),
    url = 'https://synthetic.invalid/invitation/retained-origin-only';
  await page.route('**/api/organization/action/create_invitation', async route => {
    arrived();
    await waiting;
    await route.fulfill({json: {url}});
    finished();
  });
  try {
    await page.goto(app.origin + '/?project=default&page=administration&category=people');
    await expect(page.locator('#admin-dialog')).toBeVisible();
    await expect(page.locator('#account-actions-toggle')).toBeEnabled();
    await page.locator('#invite-person summary').click();
    await page.locator('#invitation-name').fill('Held origin invitation');
    await page.locator('#invitation-create-form button').click();
    await started;
    await page.keyboard.press('Escape');
    release();
    await done;
    await expect(page.locator('#organization-secret-value')).toHaveValue(url);
    const login = await page.request.post(app.origin + '/auth/password/login', {
      form: other,
      headers: {Origin: app.origin}
    });
    expect(login.ok()).toBe(true);
    const current = await (await page.request.get(app.origin + '/api/me')).json();
    expect(current.id).toBe(otherID);
    expect(current.admin).toBe(false);
    await page.locator('#account-actions-toggle').click();
    await page.locator('#admin-open').click();
    await expect(page.locator('#admin-open')).toBeHidden();
    await expect(page.locator('#organization-secret-value')).toHaveValue('');
  } finally {
    release();
  }
});

test('an accepted credential keeps its one-time receipt when the following service read fails', async ({page}) => {
  const directory = await app.api('/api/admin'),
    human = directory.users.find(
      person => person.id !== '@local-admin' && person.kind !== 'service' && !person.disabled
    ),
    service = await app.api('/api/organization/action/create_service_account', {
      name: 'Receipt follow-up service',
      steward: human.id
    });
  await app.api('/api/members', {project: 'default', user: service.id, role: 'read'});
  await page.goto(app.origin + '/?project=default&page=administration&category=automation');
  await expect(page.locator('#admin-dialog')).toBeVisible();
  await expect(page.locator('#account-actions-toggle')).toBeEnabled();
  await page.locator('#service-list').getByRole('button', {name: 'Receipt follow-up service', exact: true}).click();
  const form = page.locator('#service-details form').filter({hasText: 'New credential'});
  await form.getByLabel('Name', {exact: true}).fill('Synthetic issued receipt');
  await form.getByLabel('Notebooks', {exact: true}).selectOption('default');
  let issued = false;
  await page.route('**/api/organization/action/issue_service_credential', route => {
    issued = true;
    return route.fulfill({json: {token: 'fmn_synthetic_retained_token'}});
  });
  await page.route('**/api/organization/action/offboarding_status', route =>
    issued
      ? route.fulfill({
          status: 503,
          json: {
            error: 'Synthetic service refresh unavailable',
            code: 'unavailable',
            message: 'Synthetic service refresh unavailable'
          }
        })
      : route.continue()
  );
  await form.getByRole('button', {name: 'Create credential', exact: true}).click();
  await expect(page.locator('#admin-status')).toContainText('Synthetic service refresh unavailable');
  await expect(page.locator('#organization-secret-value')).toHaveValue('fmn_synthetic_retained_token');
});

test('notebook settings opened from Search retain their route on reload and restore Search on return', async ({
  page
}) => {
  await page.goto(app.origin + '/?project=default&folder=');
  await expect(page.locator('#new-note')).toBeEnabled();
  await page.locator('#search-open').click();
  await page.locator('#search').fill('Settings context');
  await expect(page.locator('#search-results')).toContainText('Settings');
  await page.locator('#settings-open').click();
  await expect(page.locator('#settings-dialog')).toBeVisible();
  await page.locator('#settings-dialog [data-settings-category=search]').click();
  await page.reload();
  await expect(page.locator('#settings-dialog')).toBeVisible();
  await expect(page).toHaveURL(/page=notebook/);
  await expect(page).toHaveURL(/category=search/);
  await page.keyboard.press('Escape');
  await expect(page.locator('#settings-dialog')).toBeHidden();
  await expect(page.locator('#search-view')).toBeVisible();
  await expect(page.locator('#search')).toHaveValue('Settings context');
  await expect(page.locator('#search-results')).toContainText('Settings');
  await page.goBack();
  await expect(page.locator('#settings-dialog')).toBeVisible();
  await page.goForward();
  await expect(page.locator('#search-view')).toBeVisible();
  await expect(page.locator('#search')).toHaveValue('Settings context');
});
