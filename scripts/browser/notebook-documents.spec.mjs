import {watchConfirmations, removeConfirmations} from './app-fixture.mjs';
import {test, expect} from '@playwright/test';
import {startApp} from './app-fixture.mjs';
import {expectEditorText, fillSource} from './editor-text.mjs';

let app;
const revision = 'a'.repeat(40),
  source = 'test-source';
test.beforeAll(async () => {
  app = await startApp({
    name: 'forgetmenote-documents-',
    account: {username: 'documents-admin', password: 'DisposableDocuments123!'}
  });
  await app.api('/api/projects', {id: 'source-book', name: 'Source book', kind: 'repository'});
  await app.api('/p/default/api/note', {
    path: 'Guide.md',
    content: '# Guide\n\n## Getting started\nHelpful notes.',
    revision: ''
  });
});
test.afterAll(async () => app?.close());
test('a delayed file page cannot append results to a newer repository search', async ({page}) => {
  await app.login(page, 'password');
  const ref = path => ({project: 'source-book', origin: 'repository', source, revision, path, supported: true});
  let release, held;
  await page.route('**/p/source-book/api/documents?*', async route => {
    const params = new URL(route.request().url()).searchParams;
    if (params.has('after')) {
      held = route.request();
      await new Promise(resolve => (release = resolve));
      await route.fulfill({json: {source, revision, total: 2, entries: [ref('Old-page.md')]}});
      return;
    }
    const query = params.get('query');
    await route.fulfill({
      json: {
        source,
        revision,
        total: query ? 1 : 2,
        entries: [ref(query ? 'Current-match.md' : 'README.md')],
        next: query ? '' : 'next-page'
      }
    });
  });
  await page.goto(app.origin + '/?project=source-book');
  await page.locator('#source-files-more').click();
  await expect.poll(() => typeof release).toBe('function');
  try {
    await page.locator('#source-file-query').fill('Current');
    await expect(page.locator('#source-files')).toContainText('Current-match.md');
    const response = page.waitForResponse(r => r.request() === held);
    release();
    await (await response).finished();
    await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
    await expect(page.locator('#source-files')).not.toContainText('Old-page.md');
  } finally {
    release?.();
  }
});

test('first repository download recovers and checking updates refreshes the file list', async ({page}) => {
  await app.login(page, 'password');
  let downloaded = '',
    refreshes = 0;
  const next = 'b'.repeat(40);
  await page.route('**/p/source-book/api/repository', route =>
    route.fulfill({
      json: {connected: true, downloaded, repository: {full_name: 'example/source', default_branch: 'main'}, index: {}}
    })
  );
  await page.route('**/p/source-book/api/documents?*', route =>
    downloaded
      ? route.fulfill({
          json: {
            project: 'source-book',
            origin: 'repository',
            source,
            revision: downloaded,
            total: 1,
            entries: [
              {
                project: 'source-book',
                origin: 'repository',
                source,
                revision: downloaded,
                path: downloaded === next ? 'Updated.md' : 'README.md',
                supported: true
              }
            ]
          }
        })
      : route.fulfill({status: 503, json: {error: 'Repository service unavailable'}})
  );
  await page.route('**/p/source-book/api/repository/refresh', route => {
    refreshes++;
    downloaded = next;
    return route.fulfill({json: {connected: true, downloaded}});
  });
  await page.goto(app.origin + '/?project=source-book');
  await expect(page.locator('#source-browser-status')).toContainText('Waiting for the first download');
  downloaded = revision;
  await expect(page.getByRole('link', {name: 'README.md', exact: true})).toBeVisible();
  await page.locator('#source-check').click();
  await expect(page.getByRole('link', {name: 'Updated.md', exact: true})).toBeVisible();
  expect(refreshes).toBe(1);
  await page.locator('#source-file-query').fill('Updated');
  const status = page.waitForResponse(r => r.url().endsWith('/api/repository'));
  await page.locator('#source-check').click();
  await status;
  await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
  await expect(page.locator('#source-file-query')).toHaveValue('Updated');
});

test('source folder navigation stays in the source origin of a mixed notebook', async ({page}) => {
  await app.login(page, 'password');
  const ref = path => ({project: 'default', origin: 'repository', source, revision, path});
  await page.route('**/p/default/api/document?*', route =>
    route.fulfill({
      json: {
        ...ref('src/server.py'),
        content: 'def serve():\n    return 8080\n',
        citation: 'https://github.com/example/source/blob/' + revision + '/src/server.py',
        hash: 'h',
        editable: false,
        startLine: 1,
        endLine: 3,
        lines: 3
      }
    })
  );
  await page.route('**/p/default/api/documents?*', route =>
    route.fulfill({
      json: {
        project: 'default',
        origin: 'repository',
        source,
        revision,
        total: 1,
        entries: [{...ref('src/server.py'), supported: true}]
      }
    })
  );
  await page.goto(app.origin + '/?' + new URLSearchParams({...ref('src/server.py')}));
  await expect(page.locator('#source-document')).toBeVisible();
  await page.locator('#source-back').click();
  await expect(page.locator('#source-browser')).toBeVisible();
  await expect(page.locator('#source-browser-title')).toHaveText('src');
  await expect(page).toHaveURL(/origin=repository/);
  await page.reload();
  await expect(page.locator('#source-browser')).toBeVisible();
  await expect(page.locator('#source-browser-title')).toHaveText('src');
});

test('a delayed cross-notebook citation cannot replace a newer source selection', async ({page}) => {
  await app.login(page, 'password');
  await page.route('**/api/live*', route => route.abort());
  const ref = path => ({project: 'source-book', origin: 'repository', source: 'c'.repeat(32), revision, path});
  const chat = {
    id: 'b'.repeat(32),
    version: 2,
    title: 'Source citations',
    options: {
      connection: 'local',
      model: 'synthetic',
      mode: 'ask',
      corpus: {version: 1, archives: 'exclude', drafts: 'exclude', history: false}
    },
    turns: [
      {
        id: 'answer',
        role: 'assistant',
        content: 'Read the server.',
        citations: [
          {
            id: 'source',
            kind: 'repository',
            title: 'Server source',
            ...ref('server.py'),
            url: 'https://github.com/example/source/blob/' + revision + '/server.py'
          }
        ]
      }
    ],
    runs: [],
    eventCursor: 0
  };
  await page.route('**/api/chat/connections', route =>
    route.fulfill({
      json: {
        enabled: true,
        defaultConnection: 'local',
        connections: [{id: 'local', label: 'Synthetic', models: ['synthetic'], defaultModel: 'synthetic', local: true}]
      }
    })
  );
  await page.route('**/p/default/api/chat', route => route.fulfill({json: {conversations: [chat]}}));
  await page.route('**/p/default/api/chat/' + chat.id, route => route.fulfill({json: chat}));
  await page.route('**/p/source-book/api/documents?*', route =>
    route.fulfill({
      json: {
        project: 'source-book',
        origin: 'repository',
        source,
        revision,
        total: 1,
        entries: [{...ref('README.md'), supported: true}]
      }
    })
  );
  await page.route('**/p/source-book/api/document?*', route => {
    const path = new URL(route.request().url()).searchParams.get('path');
    return route.fulfill({
      json: {
        ...ref(path),
        content: path,
        citation: 'https://github.com/example/source/blob/' + revision + '/' + path,
        hash: 'h',
        editable: false,
        startLine: 1,
        endLine: 1,
        lines: 1
      }
    });
  });
  await page.goto(app.origin + '/?project=default&note=Guide.md');
  await page.locator('#chat-open').click();
  await page.locator('.chat-sources > summary').click();
  let release, held;
  await page.route('**/p/source-book/api/status', async route => {
    if (!held) {
      held = route.request();
      await new Promise(resolve => (release = resolve));
    }
    await route.continue();
  });
  await page.getByRole('button', {name: 'Server source', exact: true}).click();
  await expect.poll(() => typeof release).toBe('function');
  await page.locator('#project-select').selectOption('source-book');
  await page.getByRole('link', {name: 'README.md', exact: true}).click();
  await expect(page.locator('#source-document-meta')).toContainText('README.md');
  const response = page.waitForResponse(r => r.request() === held);
  release();
  await response;
  await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
  await expect(page.locator('#source-document-meta')).toContainText('README.md');
  await expect(page).toHaveURL(/path=README.md/);
});

test('empty repository choices do not disable blank notebook creation', async ({page}) => {
  await app.login(page, 'password');
  await page.route('**/api/projects/repository/choices?*', route =>
    route.fulfill({
      json: {
        project: 'pending-repository',
        name: 'Pending repository',
        choices: [],
        installUrl: 'https://github.com/apps/example/installations/new'
      }
    })
  );
  await page.goto(app.origin + '/?repository=create&notebook=pending-repository');
  await expect(page.locator('#workspace-create-status')).toContainText('No repositories are available');
  await expect(page.locator('#notebook-submit')).toBeDisabled();
  await page.locator('#notebook-create-mode').click();
  await expect(page.locator('#notebook-submit')).toBeEnabled();
  await page.locator('#project-name').fill('Blank after GitHub');
  await page.locator('#project-id').fill('blank-after-github');
  await page.locator('#notebook-submit').click();
  await expect(page.locator('#workspace-create-dialog')).not.toBeVisible();
  expect(
    (await app.api('/api/projects')).some(
      p => p.id === 'blank-after-github' && (p.kind || 'notes') === 'notes' && p.capabilities.edit
    )
  ).toBe(true);
});

test('delayed repository choices cannot overwrite a different notebook form', async ({page}) => {
  await app.login(page, 'password');
  let release;
  await page.route('**/api/projects/repository/choices?*', async route => {
    await new Promise(resolve => (release = resolve));
    await route.fulfill({
      json: {
        project: 'stale-repository',
        name: 'Stale repository',
        choices: [],
        installUrl: 'https://github.com/apps/example/installations/new'
      }
    });
  });
  await page.goto(app.origin + '/?repository=create&notebook=stale-repository');
  await expect(page.locator('#workspace-create-status')).toHaveText('Loading repositories…');
  await expect.poll(() => typeof release).toBe('function');
  await page.locator('#notebook-create-mode').click();
  await page.locator('#project-name').fill('My blank notebook');
  await page.locator('#project-id').fill('my-blank-notebook');
  const response = page.waitForResponse(r => r.url().includes('/api/projects/repository/choices'));
  release();
  await response;
  await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
  await expect(page.locator('#project-name')).toHaveValue('My blank notebook');
  await expect(page.locator('#project-name')).toBeEditable();
  await expect(page.locator('#notebook-repository-selection')).toBeHidden();
  await expect(page.locator('#notebook-submit')).toBeEnabled();
});

test('repository documents use the main reader on desktop and mobile', async ({page}) => {
  page.on('pageerror', error => {
    throw error;
  });
  await app.login(page, 'password');
  const ref = path => ({project: 'source-book', origin: 'repository', source, revision, path});
  await page.route('**/p/source-book/api/documents?*', route =>
    route.fulfill({
      json: {
        project: 'source-book',
        origin: 'repository',
        source,
        revision,
        total: 2,
        entries: [
          {...ref('README.md'), supported: true},
          {...ref('server.py'), supported: true}
        ]
      }
    })
  );
  await page.route('**/p/source-book/api/document?*', route => {
    const path = new URL(route.request().url()).searchParams.get('path');
    return route.fulfill({
      json: {
        ...ref(path),
        content: path === 'README.md' ? '# Source overview\n[Server](server.py)' : 'def serve():\n    return 8080\n',
        html: path === 'README.md' ? '<h1>Source overview</h1><p><a href="server.py">Server</a></p>' : '',
        citation: 'https://github.com/example/source/blob/' + revision + '/' + path,
        hash: 'h',
        editable: false,
        startLine: 1,
        endLine: 3,
        lines: 3
      }
    });
  });
  await page.goto(app.origin + '/?project=source-book');
  await expect(page.locator('#source-browser'), await page.locator('body').innerText()).toBeVisible({timeout: 10000});
  await expect(page.locator('#count-notes')).toBeHidden();
  await page.getByRole('link', {name: 'README.md', exact: true}).click();
  await expect(page.locator('#source-document')).toBeVisible();
  await expect(page.locator('#source-document h1')).toHaveText('Source overview');
  await expect(page.locator('#mode')).toBeHidden();
  await expect(page).toHaveURL(new RegExp('revision=' + revision));
  await page.locator('#source-document').getByRole('link', {name: 'Server', exact: true}).click();
  await expect(page.locator('#source-document .cm-content')).toContainText('def serve():');
  await expect(page.locator('#source-document .cm-content')).toHaveAttribute('contenteditable', 'false');
  await expect(page.locator('#source-document .cm-scroller')).toHaveCSS('display', 'flex');
  await expect(page.locator('#source-document .cm-content')).toHaveCSS('white-space', 'pre');
  await page.screenshot({path: test.info().outputPath('source-desktop.png'), fullPage: true});
  await page.evaluate(() => (document.documentElement.dataset.theme = 'dark'));
  const contrast = await page
    .locator('#source-code .cm-line span')
    .first()
    .evaluate(span => {
      const luminance = color => {
        const rgb = color
          .match(/[\d.]+/g)
          .slice(0, 3)
          .map(v => {
            v = Number(v) / 255;
            return v <= 0.04045 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4;
          });
        return rgb[0] * 0.2126 + rgb[1] * 0.7152 + rgb[2] * 0.0722;
      };
      const foreground = luminance(getComputedStyle(span).color),
        background = luminance(getComputedStyle(document.body).backgroundColor);
      return (Math.max(foreground, background) + 0.05) / (Math.min(foreground, background) + 0.05);
    });
  expect(contrast).toBeGreaterThanOrEqual(4.5);
  await page.screenshot({path: test.info().outputPath('source-dark.png'), fullPage: true});
  await page.evaluate(() => (document.documentElement.dataset.theme = 'light'));
  await page.setViewportSize({width: 390, height: 844});
  await expect(page.locator('#mobile-navigation [data-destination="browse-root"]')).toHaveText('Files');
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(390);
  await page.reload();
  await expect(page.locator('#source-document .cm-content')).toContainText('def serve():');
  await page.screenshot({path: test.info().outputPath('source-mobile.png'), fullPage: true});
});

test('private note drafts survive a visit to repository files', async ({page}) => {
  await app.login(page, 'password');
  await page.route('**/api/live*', route => route.abort());
  await page.route('**/p/source-book/api/documents?*', route =>
    route.fulfill({json: {project: 'source-book', origin: 'repository', source, revision, total: 0, entries: []}})
  );
  await page.goto(app.origin + '/?project=default&note=Guide.md');
  await expect(page.locator('#document')).toBeVisible();
  await page.locator('#mode').click();
  await page.locator('#format-source').click();
  const draft = '# Private draft\nThis text must survive source navigation.';
  await fillSource(page, draft);
  await watchConfirmations(page, dialog => dialog.accept());
  await page.locator('#project-select').selectOption('source-book');
  await expect(page.locator('#source-browser')).toBeVisible();
  await page.locator('#project-select').selectOption('default');
  await page.locator('#folder-items a[data-note="Guide.md"]').click();
  await expectEditorText(page, draft);
});

test('managed backup controls show verified state and explicit sharing', async ({page}) => {
  await app.login(page, 'password');
  let enabled = true,
    generation = 1;
  await page.route('**/p/default/api/managed-backup', route =>
    route.fulfill({
      json: {
        configured: true,
        connected: true,
        enabled,
        generation,
        repository: {full_name: 'example/private-backup'},
        branch: 'main',
        lastRevision: revision,
        lastSuccess: '2026-09-22T00:00:00Z',
        pending: false,
        installUrl: 'https://github.com/apps/example/installations/new'
      }
    })
  );
  await page.route('**/p/default/api/managed-backup/control', route => {
    const inData = route.request().postDataJSON();
    expect(inData.generation).toBe(generation);
    enabled = inData.action !== 'pause';
    generation++;
    return route.fulfill({json: {generation, enabled}});
  });
  await page.goto(app.origin + '/?project=default');
  await page.locator('#settings-open').click();
  await page.locator('#settings-dialog [data-settings-category=history]').click();
  await expect(page.locator('#managed-backup-status')).toContainText('Up to date');
  await expect(page.locator('#managed-backup-receipt')).toContainText(revision.slice(0, 12));
  await page.locator('#managed-backup-pause').click();
  await expect(page.locator('#managed-backup-status')).toContainText('Paused');
  await expect(page.locator('#managed-backup-now')).toBeDisabled();
  await expect(page.locator('#managed-backup-pause')).toHaveText('Resume');
});
