import {test, expect} from '@playwright/test';
import {startApp} from './app-fixture.mjs';
import {attachmentMarkdown} from '../../internal/web/ui/media.mjs';
import {caretAtLineEnd, countLiveUpdates, editorText, expectEditorText, expectVisual} from './editor-text.mjs';

let app, cookies;
test.beforeAll(async ({browser}) => {
  app = await startApp({
    name: 'forgetmenote-links-',
    account: {username: 'link-admin', password: 'DisposableLinks123!'}
  });
  await app.api('/api/projects', {id: 'second', name: 'Second notebook'});
  await app.api('/p/second/api/note', {
    path: 'same.md',
    revision: '',
    content: '---\nfmn-id: 11111111111111111111111111111111\n---\n# Target page\n\n## Details\n\nPinned facts.\n'
  });
  const context = await browser.newContext();
  try {
    await app.login(await context.newPage(), 'password');
    cookies = await context.cookies();
  } finally {
    await context.close();
  }
});
test.afterAll(async () => {
  await app?.close();
});
test.beforeEach(async ({page}) => {
  page.on('pageerror', error => {
    throw error;
  });
  await page.context().addCookies(cookies);
});
test.afterEach(async ({}, info) => {
  if (info.status !== info.expectedStatus && app)
    await info.attach('app.log', {body: app.output, contentType: 'text/plain'});
});

test('both editors insert notebook links with a heading and navigate after rename', async ({page}) => {
  const source = '# Link source\n\nKeep this paragraph.\n';
  await app.api('/p/default/api/note', {path: 'link-source.md', revision: '', content: source});
  await page.goto(app.origin + '/?project=default&note=link-source.md');
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await page.locator('#mode').click();
  await expectVisual(page);
  await expect(page.locator('#notebook-link-insert')).toBeVisible();
  await page.locator('#format-source').click();
  await page.locator('#editor').focus();
  await page.locator('#editor').press('ControlOrMeta+End');
  await page.locator('#notebook-link-insert').click();
  const picker = page.locator('#notebook-link-dialog');
  await picker.getByRole('combobox', {name: 'Notebook', exact: true}).selectOption('second');
  await picker.getByRole('button', {name: 'Target page', exact: true}).click();
  await picker.getByRole('combobox', {name: 'Page heading'}).selectOption('details');
  await picker.getByRole('button', {name: 'Insert link', exact: true}).click();
  await expectEditorText(page, /Keep this paragraph\.[\s\S]*noteId=11111111111111111111111111111111/);
  await page.locator('#format-visual').click();
  await expect(page.locator('#editor .cm-lp-link')).toHaveText('Second notebook → Target page');
  await page.locator('#save').click();
  await expect(page.locator('#save-state')).toContainText('Saved');
  await expect(page.locator('#save')).toBeDisabled();
  const original = await app.api('/p/second/api/note?path=same.md');
  await app.api('/p/second/api/move', {from: 'same.md', to: 'renamed.md', revision: original.revision});
  await page.locator('#mode').click();
  await page.locator('#preview').getByRole('link', {name: 'Second notebook → Target page'}).click();
  await expect(page.locator('#project-select')).toHaveValue('second');
  await expect(page.locator('#preview')).toContainText('Pinned facts.');
  await expect(page).toHaveURL(/note=renamed.md/);
  await page.goBack();
  await expect(page.locator('#project-select')).toHaveValue('default');
  await expect(page.locator('#preview')).toContainText('Keep this paragraph.');
});

test('visual insertion preserves the surrounding paragraph', async ({page}) => {
  const content = '# Visual link\n\nKeep **this** text.\n';
  await app.api('/p/default/api/note', {path: 'visual-link.md', revision: '', content});
  await page.goto(app.origin + '/?project=default&note=visual-link.md');
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await page.locator('#mode').click();
  await expectVisual(page);
  await caretAtLineEnd(page, 'Keep this text.');
  await page.locator('#notebook-link-insert').click();
  const picker = page.locator('#notebook-link-dialog');
  await picker.getByRole('combobox', {name: 'Notebook', exact: true}).selectOption('second');
  await picker.getByRole('button', {name: 'Target page', exact: true}).click();
  await picker.screenshot({path: 'test-results/notebook-link-picker.png'});
  await picker.getByRole('button', {name: 'Insert link', exact: true}).click();
  // The link goes in at the caret; every other byte stays.
  await expectEditorText(
    page,
    /^# Visual link\n\nKeep \*\*this\*\* text\.\[Second notebook → Target page\]\([^)\s]*noteId=1{32}[^)\s]*\)\n$/
  );
  await expect(page.locator('#editor .cm-lp-link')).toHaveText('Second notebook → Target page');
  await page.locator('#save').click();
  await expect(page.locator('#save-state')).toContainText('Saved');
  await expect(page.locator('#save')).toBeDisabled();
  const saved = await app.api('/p/default/api/note?path=visual-link.md');
  expect(saved.content).toBe(await editorText(page));
});

// Ctrl/Cmd+clicks the middle of `text` in the editor's drawn lines.
async function followText(page, text) {
  const point = await page.locator('#editor').evaluate((editor, text) => {
    const walker = document.createTreeWalker(editor, NodeFilter.SHOW_TEXT);
    for (let node = walker.nextNode(); node; node = walker.nextNode()) {
      const at = node.data.indexOf(text);
      if (at < 0) continue;
      const range = document.createRange();
      range.setStart(node, at);
      range.setEnd(node, at + text.length);
      const rect = range.getBoundingClientRect();
      return {x: rect.left + rect.width / 2, y: rect.top + rect.height / 2};
    }
    return null;
  }, text);
  expect(point).not.toBeNull();
  await page.keyboard.down('ControlOrMeta');
  await page.mouse.click(point.x, point.y);
  await page.keyboard.up('ControlOrMeta');
}

// Decision 3: a plain click edits a link, Ctrl/Cmd+click follows it by the
// reader's rules, and on a touch screen an Open button follows it.
test('a plain click edits a link in Visual and Ctrl/Cmd+click follows it as the reader does', async ({page}) => {
  await page.addInitScript(() => {
    window.opened = [];
    window.open = url => {
      window.opened.push(String(url));
      return null;
    };
  });
  await app.api('/p/default/api/note', {
    path: 'follow/target.md',
    revision: '',
    content: '# Target\n\n## Part\n\nArrived.\n'
  });
  const content =
    '# Links\n\nSee [the target](target.md#part), [the web](https://example.com/page), [a file](files/a.pdf) and [elsewhere](/?project=second&note=same.md&noteId=11111111111111111111111111111111).\n\nNever [a script](javascript:window.ran=1).\n';
  await app.api('/p/default/api/note', {path: 'follow/links.md', revision: '', content});
  await page.goto(app.origin + '/?project=default&note=follow%2Flinks.md');
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await page.locator('#mode').click();
  await expectVisual(page);
  const sent = countLiveUpdates(page);
  const link = label => page.locator('#editor .cm-lp-link').filter({hasText: label});
  await link('the target').click();
  await expect(page.locator('#editor .cm-line').filter({hasText: 'See'})).toContainText('[the target](target.md#part)');
  await expect(page).toHaveURL(/note=follow%2Flinks\.md/);
  await link('the web').click({modifiers: ['ControlOrMeta']});
  await link('a file').click({modifiers: ['ControlOrMeta']});
  await expect
    .poll(() => page.evaluate(() => window.opened))
    .toEqual(['https://example.com/page', expect.stringMatching(/^\/p\/default\/assets\/follow\/files\/a\.pdf/)]);
  // A link that leads nowhere the reader would go does nothing.
  await link('a script').click({modifiers: ['ControlOrMeta']});
  await expect(page.locator('#notice')).toContainText('This link does not lead to a note, file or web page.');
  expect(await page.evaluate(() => [window.ran, window.opened.length])).toEqual([undefined, 2]);
  // Source follows links the same way.
  await page.locator('#format-source').click();
  await followText(page, 'the web');
  await expect.poll(() => page.evaluate(() => window.opened.length)).toBe(3);
  await page.locator('#format-visual').click();
  expect(sent).toEqual([]);
  await expect(page).toHaveURL(/note=follow%2Flinks\.md/);
  // Another note opens as the reader opens it, keeping the draft.
  await caretAtLineEnd(page, 'See');
  await page.keyboard.type(' Draft.');
  await link('the target').click({modifiers: ['ControlOrMeta']});
  await expect(page.locator('[data-confirm-action]')).toContainText(
    'Keep your draft on this device and open another note?'
  );
  await page.locator('[data-confirm-action]').getByRole('button', {name: 'Continue', exact: true}).click();
  await expect(page).toHaveURL(/note=follow%2Ftarget\.md/);
  await expect(page.locator('#preview')).toContainText('Arrived.');
  await page.goBack();
  await expect(page).toHaveURL(/note=follow%2Flinks\.md/);
  await expectEditorText(page, content.replace('111).\n', '111). Draft.\n'));
  // A link to another notebook opens it.
  if ((await page.locator('#mode').textContent()) === 'Edit') await page.locator('#mode').click();
  await expect(page.locator('#editing')).toBeVisible();
  await expectVisual(page);
  await link('elsewhere').click({modifiers: ['ControlOrMeta']});
  await expect(page.locator('[data-confirm-action]')).toContainText(
    'Keep this draft on this device and switch notebooks?'
  );
  await page.locator('[data-confirm-action]').getByRole('button', {name: 'Continue', exact: true}).click();
  await expect(page.locator('#project-select')).toHaveValue('second');
  await expect(page.locator('#preview')).toContainText('Pinned facts.');
});

// Insert → Image or file writes [name](</path%20x.pdf>): the reader follows
// the address goldmark reads, without <…>, escapes or character references.
test('Ctrl/Cmd+click follows an inserted attachment and a note link written in <…>', async ({page}) => {
  await page.addInitScript(() => {
    window.opened = [];
    window.open = url => {
      window.opened.push(String(url));
      return null;
    };
  });
  await app.api('/p/default/api/note', {
    path: 'angle/other note.md',
    revision: '',
    content: '# Other note\n\nFound by its spaced name.\n'
  });
  const attachment = attachmentMarkdown('attachments/My file.pdf', 'My file.pdf', 'application/pdf');
  expect(attachment).toBe('[My file.pdf](</attachments/My%20file.pdf>)');
  const content =
    '# Angles\n\nAttached ' +
    attachment +
    ', spelled [by hand](</attachments/Our notes.pdf>), [escaped](a\\_b&amp;c.pdf) and [the other note](<other note.md>).\n';
  await app.api('/p/default/api/note', {path: 'angle/links.md', revision: '', content});
  await page.goto(app.origin + '/?project=default&note=angle%2Flinks.md');
  await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
  await page.locator('#mode').click();
  await expectVisual(page);
  const sent = countLiveUpdates(page);
  const link = label => page.locator('#editor .cm-lp-link').filter({hasText: label});
  await link('My file.pdf').click({modifiers: ['ControlOrMeta']});
  await link('by hand').click({modifiers: ['ControlOrMeta']});
  await link('escaped').click({modifiers: ['ControlOrMeta']});
  await expect
    .poll(() => page.evaluate(() => window.opened))
    .toEqual([
      expect.stringMatching(/^\/p\/default\/assets\/attachments\/My%20file\.pdf(\?|$)/),
      expect.stringMatching(/^\/p\/default\/assets\/attachments\/Our%20notes\.pdf(\?|$)/),
      expect.stringMatching(/^\/p\/default\/assets\/angle\/a_b%26c\.pdf(\?|$)/)
    ]);
  await expect(page.locator('#notice')).not.toContainText('does not lead');
  expect(sent).toEqual([]);
  await link('the other note').click({modifiers: ['ControlOrMeta']});
  await expect(page).toHaveURL(/note=angle%2Fother(%20|\+)note\.md/);
  await expect(page.locator('#preview')).toContainText('Found by its spaced name.');
  expect(await page.evaluate(() => window.opened.length)).toBe(3);
  expect((await app.api('/p/default/api/note?path=angle%2Flinks.md')).content).toBe(content);
});

test('on a touch screen an Open button follows the link that holds the caret', async ({browser}) => {
  const context = await browser.newContext({hasTouch: true, isMobile: true, viewport: {width: 390, height: 844}});
  await context.addCookies(cookies);
  const page = await context.newPage();
  page.on('pageerror', error => {
    throw error;
  });
  try {
    await app.api('/p/default/api/note', {
      path: 'touch-target.md',
      revision: '',
      content: '# Touch target\n\nReached.\n'
    });
    const content = '# Touch\n\nGo to [the target](touch-target.md) now.\n';
    await app.api('/p/default/api/note', {path: 'touch-links.md', revision: '', content});
    await page.goto(app.origin + '/?project=default&note=touch-links.md');
    await expect(page.locator('#document')).toHaveAttribute('data-live', 'connected');
    await page.locator('#mode').tap();
    await expectVisual(page);
    const open = page.getByRole('button', {name: 'Open link touch-target.md'});
    await expect(open).toHaveCount(0);
    await page.locator('#editor .cm-lp-link').tap();
    await expect(open).toBeVisible();
    // Moving the caret out of the link takes the button away.
    await page.locator('#editor .cm-line').filter({hasText: 'Touch'}).first().tap();
    await expect(open).toHaveCount(0);
    await page.locator('#editor .cm-lp-link').tap();
    await open.tap();
    await expect(page).toHaveURL(/note=touch-target\.md/);
    await expect(page.locator('#preview')).toContainText('Reached.');
    expect((await app.api('/p/default/api/note?path=touch-links.md')).content).toBe(content);
  } finally {
    await context.close();
  }
});

test('GitHub callback opens selection after route normalization', async ({page}) => {
  await page.route('**/api/repository', route =>
    route.fulfill({
      json: {configured: true, connected: false, installUrl: 'https://github.com/apps/fixture/installations/new'}
    })
  );
  await page.route('**/api/repository/choices', route =>
    route.fulfill({json: [{id: 42, installation: 7, full_name: 'example/source', private: true}]})
  );
  await page.goto(app.origin + '/?project=default&repository=connect');
  await expect(page.locator('#settings-dialog')).toBeVisible();
  await expect(page.locator('#repository-choice')).toHaveValue('{"repository":42,"installation":7}');
  await expect(page.locator('#repository-share')).not.toBeChecked();
  await expect(page.locator('#repository-select-form')).toBeVisible();
});

test('a delayed notebook resolver cannot displace a newer navigation', async ({page}) => {
  await app.api('/p/default/api/note', {
    path: 'slow-link.md',
    revision: '',
    content: '[Target](/?project=second&note=same.md&noteId=11111111111111111111111111111111)\n'
  });
  await app.api('/p/default/api/note', {path: 'stay-here.md', revision: '', content: '# Stay here\n'});
  await page.goto(app.origin + '/?project=default&note=slow-link.md');
  let release, entered;
  const blocked = new Promise(resolve => {
      release = resolve;
    }),
    started = new Promise(resolve => {
      entered = resolve;
    });
  await page.route('**/api/notebook-links/resolve', async route => {
    entered();
    await blocked;
    await route.continue();
  });
  await page.locator('#preview').getByRole('link', {name: 'Target', exact: true}).click();
  await started;
  await page.locator('#browse-root').click();
  await page.locator('#folder-items [data-note="stay-here.md"]').click();
  await expect(page.locator('#preview')).toContainText('Stay here');
  const response = page.waitForResponse(r => r.url().includes('/notebook-links/resolve'));
  release();
  const resolved = await response;
  expect(resolved.status(), await resolved.text()).toBe(200);
  await resolved.finished();
  // Let this response's UI continuation finish; live collaboration keeps polling.
  await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
  await expect(page).toHaveURL(/note=stay-here.md/);
  await expect(page.locator('#project-select')).toHaveValue('default');
  await expect(page.locator('#preview')).toContainText('Stay here');
});

test('source settings give an actionable unconfigured state without disrupting the editor', async ({page}) => {
  await page.goto(app.origin + '/?project=default');
  await page.locator('#settings-open').click();
  await page.locator('[data-settings-category="sources"]').click();
  await expect(page.locator('#repository-status')).toHaveText('GitHub sources are not configured on this server.');
  await expect(page.locator('#repository-connect')).toBeHidden();
});

test('notebook settings remain usable while source status is loading', async ({page}) => {
  let release;
  const held = new Promise(resolve => {
    release = resolve;
  });
  await page.route('**/api/repository', async route => {
    await held;
    await route.fulfill({json: {configured: false, connected: false}});
  });
  try {
    await page.goto(app.origin + '/?project=default');
    await page.locator('#settings-open').click();
    await expect(page.locator('#settings-dialog')).toBeVisible();
    await page.locator('[data-settings-category="sources"]').click();
    await expect(page.locator('#repository-status')).toContainText('Loading');
  } finally {
    release();
  }
});

test('source status updates in place without offering documentation generation', async ({page}) => {
  let downloaded = '',
    requests = 0;
  await page.route('**/api/repository', route => {
    requests++;
    return route.fulfill({
      json: {
        configured: true,
        connected: true,
        id: 'fixture',
        repository: {full_name: 'example/source', default_branch: 'main'},
        downloaded,
        pending: !downloaded
      }
    });
  });
  await page.goto(app.origin + '/?project=default');
  await page.locator('#settings-open').click();
  await page.locator('[data-settings-category="sources"]').click();
  await expect(page.getByRole('button', {name: 'View past documentation updates'})).toHaveCount(0);
  await expect(page.getByRole('button', {name: 'Reconcile provider usage'})).toHaveCount(0);
  await expect(page.getByLabel('Publication mode', {exact: true})).toHaveCount(0);
  downloaded = 'a'.repeat(40);
  await expect(page.locator('#repository-detail')).toContainText('aaaaaaaaaaaa');
  await expect(page.locator('#repository-browse')).toBeVisible();
  await page.locator('#settings-dialog [data-close]').click();
  const stopped = requests;
  await page.waitForTimeout(2400);
  expect(requests).toBe(stopped);
});

test('source file filtering keeps citations pinned when background status advances', async ({page}) => {
  let downloaded = 'a'.repeat(40),
    opened;
  await page.route('**/api/repository', route =>
    route.fulfill({
      json: {
        configured: true,
        connected: true,
        id: 'fixture',
        repository: {full_name: 'example/source', default_branch: 'main'},
        downloaded
      }
    })
  );
  await page.route('**/api/repository/tree?*', route =>
    route.fulfill({
      json: [
        {path: 'README.md', supported: true},
        {path: 'src/server.go', supported: true},
        {path: 'images/logo.png', supported: false}
      ]
    })
  );
  await page.route('**/api/document?*', route => {
    opened = new URL(route.request().url());
    return route.fulfill({
      json: {
        project: 'default',
        origin: 'repository',
        source: 'fixture',
        path: opened.searchParams.get('path'),
        revision: opened.searchParams.get('revision'),
        content: 'package server\n',
        citation: 'https://github.com/example/source/blob/' + opened.searchParams.get('revision') + '/src/server.go'
      }
    });
  });
  await page.goto(app.origin + '/?project=default');
  await page.locator('#settings-open').click();
  await page.locator('[data-settings-category="sources"]').click();
  await page.locator('#repository-browse').click();
  const filter = page.getByRole('searchbox', {name: 'Filter source files'});
  await filter.fill('SERVER');
  await expect(page.locator('#repository-files li')).toHaveCount(1);
  await expect(page.locator('#repository-file-count')).toHaveText('1 of 3 files');
  downloaded = 'b'.repeat(40);
  await expect(page.locator('#repository-detail')).toContainText('bbbbbbbbbbbb');
  await expect(filter).toHaveValue('SERVER');
  await page.locator('#repository-files').getByRole('button', {name: 'src/server.go', exact: true}).click();
  await expect(page.locator('#source-document-meta')).toContainText('aaaaaaaaaaaa');
  expect(opened.searchParams.get('revision')).toBe('a'.repeat(40));
});

test('source search shares explicit notebook scope and opens exact read-only lines', async ({page}) => {
  let requested, opened;
  const revision = 'a'.repeat(40);
  await page.route('**/api/retrieval', route => {
    requested = route.request().postDataJSON();
    return route.fulfill({
      json: {
        results: [
          {
            kind: 'repository',
            project: 'second',
            source: {
              project: 'second',
              source: 'fixture',
              revision,
              path: 'src/health.go',
              symbol: 'Health',
              startLine: 21,
              endLine: 23,
              text: 'func Health() bool { return true }',
              citation: `https://github.com/example/source/blob/${revision}/src/health.go#L21-L23`
            }
          }
        ],
        sources: {coverage: {second: {indexed: revision, chunks: 10, embedded: 0, mode: 'text'}}}
      }
    });
  });
  await page.route('**/p/second/api/document?*', route => {
    opened = new URL(route.request().url());
    return route.fulfill({
      json: {
        project: 'second',
        origin: 'repository',
        source: 'fixture',
        path: 'src/health.go',
        revision,
        startLine: 21,
        endLine: 23,
        content: 'func Health() bool {\n return true\n}',
        citation: `https://github.com/example/source/blob/${revision}/src/health.go#L21-L23`
      }
    });
  });
  await page.goto(app.origin + '/?project=default');
  await page.locator('#search-open').click();
  await page.locator('#search-corpus').selectOption('repository');
  await page.locator('#search-projects input[value=default]').check();
  await page.locator('#search-projects input[value=second]').check();
  await page.locator('#search').fill('Health');
  await expect(page.locator('#search-results')).toContainText('src/health.go:21–23');
  expect(requested.projects).toEqual(['default', 'second']);
  expect(requested.corpora).toEqual(['repository']);
  await page.locator('#search-options-toggle').click();
  await page.locator('#advanced-search > summary').click();
  await page.locator('#search-scope').check();
  await page.locator('#search-corpus').selectOption('both');
  await expect
    .poll(() => ({history: requested.history, corpora: requested.corpora}))
    .toEqual({history: true, corpora: ['notes', 'repository']});
  await page.locator('#search-results a').click();
  await expect(page.locator('#source-document')).toBeVisible();
  await expect(page.locator('#source-code .cm-content')).toContainText('func Health');
  await expect(page.locator('#source-code .cm-lineNumbers')).toContainText('21');
  expect(opened.searchParams.get('revision')).toBe(revision);
  expect(opened.searchParams.get('startLine')).toBe('21');
});

test('unconfigured source search explains missing coverage and notes remain searchable', async ({page}) => {
  await page.goto(app.origin + '/?project=default');
  await page.locator('#search-open').click();
  await page.locator('#search-corpus').selectOption('repository');
  await page.locator('#search').fill('health');
  await expect(page.locator('#search-status')).toContainText('Repository integration is not configured');
  await expect(page.locator('#search-results')).toContainText('No matches in the available source coverage');
  await page.locator('#search-corpus').selectOption('notes');
  await expect(page.locator('#search-projects-label')).toBeVisible();
  await page.locator('#search').fill('Link source');
  await expect(page.locator('#search-status')).not.toContainText('Repository integration');
});
